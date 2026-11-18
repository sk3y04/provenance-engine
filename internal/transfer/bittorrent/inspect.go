package bittorrent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// Metadata-first inspection.
//
// Inspection answers "what is in this torrent?" without joining a job: it never
// takes the staging lock, never writes to a staging root, and never opens
// payload storage. A local .torrent is parsed in memory with no client at all;
// a magnet gets a short-lived client whose data directory is a private
// temporary directory with an in-memory piece completion, so neither payload
// nor a piece database can land anywhere.
//
// The library gates that make this true (verified in the pinned v1.62.0):
//   - storage.NewFile(dir) — the default storage a client builds — creates
//     <dir> and opens <dir>/.torrent.db eagerly in at.NewClient, independently
//     of DeferStorageOpen and of OpenStorage. That is why inspection supplies
//     its own storage instead of pointing DataDir at a staging root.
//   - Torrent.setInfo opens payload storage as soon as metainfo arrives unless
//     deferStorageOpen is set, so DeferStorageOpen is what keeps a fetch from
//     creating payload directories before admission.
//   - New files start at PiecePriorityNone (t.initFiles), and
//     DisallowDataDownload/DisallowDataUpload stop piece requests and uploads
//     outright, so "no priorities yet" is not the only thing standing between a
//     metadata exchange and a payload request.

// InspectConfig configures Inspect. It deliberately has no staging directory:
// inspection does not read or write one.
type InspectConfig struct {
	// Peers is an optional list of "host:port" peers to dial for metadata.
	Peers []string

	// MetadataTimeout bounds the wait for metainfo. Zero uses two minutes.
	MetadataTimeout time.Duration

	// DisablePeerDiscovery disables DHT, PEX, trackers and webseeds and binds
	// the listener to loopback, for hermetic use.
	DisablePeerDiscovery bool

	// onClient is a test seam: it runs immediately before an inspection client
	// is constructed, so a test can prove that a local .torrent builds none.
	onClient func()
}

// Inspect returns the file layout of a torrent without downloading payload.
//
// A local .torrent file is parsed directly. A magnet is resolved through a
// temporary client with payload transfer disabled and storage deferred, and the
// client and its temporary directory are closed and removed before Inspect
// returns, including on cancellation and timeout.
func Inspect(ctx context.Context, rawSource string, cfg InspectConfig) (Info, error) {
	src, err := parseSource(rawSource)
	if err != nil {
		return Info{}, err
	}
	switch src.kind {
	case "file":
		return inspectMetainfoFile(src)
	case "magnet":
		return inspectMagnet(ctx, src, cfg)
	default:
		return Info{}, fmt.Errorf("%w: unknown source kind %q", ErrUnsupportedSource, src.kind)
	}
}

// inspectMetainfoFile reads a local .torrent in memory. No client, listener,
// socket or temporary directory is involved.
func inspectMetainfoFile(src source) (Info, error) {
	base := filepath.Base(src.path)
	mi, err := metainfo.LoadFromFile(src.path)
	if err != nil {
		return Info{}, fmt.Errorf("%w: parse %s: %v", ErrUnsupportedSource, base, err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return Info{}, fmt.Errorf("%w: parse info of %s: %v", ErrUnsupportedSource, base, err)
	}
	// The identity comes from the bytes just parsed, so a v2-only or hybrid
	// torrent is described by its own full hashes rather than a truncated or
	// synthesised one.
	id, err := requireIdentity(mi, &info)
	if err != nil {
		return Info{}, fmt.Errorf("%s: %w", base, err)
	}
	if !id.SameTorrent(src.id) {
		return Info{}, fmt.Errorf("%w: %s changed while it was read", ErrUnsupportedSource, base)
	}
	rels, err := validateInfoPaths(&info)
	if err != nil {
		return Info{}, err
	}
	return infoFromMetainfo(src, &info, rels), nil
}

// inspectMagnet resolves metadata from peers without touching any staging root.
func inspectMagnet(ctx context.Context, src source, cfg InspectConfig) (Info, error) {
	if cfg.MetadataTimeout <= 0 {
		cfg.MetadataTimeout = defaultMetadataTimeout
	}
	// A private data directory, removed on every exit path. Nothing here is
	// keyed to a job, so a caller inspecting a magnet cannot disturb a running
	// fetch or leave a partial layout behind.
	tmp, err := os.MkdirTemp("", "provenance-inspect-")
	if err != nil {
		return Info{}, fmt.Errorf("inspect: create metadata directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	cc := at.NewDefaultClientConfig()
	cc.DataDir = tmp
	cc.Seed = false
	cc.ListenPort = 0
	cc.NoDefaultPortForwarding = true
	cc.DisableWebtorrent = true
	cc.DisableWebseeds = true
	// An in-memory piece completion replaces <dir>/.torrent.db: inspection must
	// not create a completion database anywhere, not even in its temp dir.
	cc.DefaultStorage = storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   tmp,
		PieceCompletion: storage.NewMapPieceCompletion(),
	})
	if cfg.DisablePeerDiscovery {
		cc.NoDHT = true
		cc.DisableTrackers = true
		cc.DisablePEX = true
		cc.DisableUTP = true
		cc.DisableIPv6 = true
		cc.ListenHost = func(string) string { return "127.0.0.1" }
	}
	peers, err := parsePeers(cfg.Peers)
	if err != nil {
		return Info{}, err
	}
	// Torrent construction, not just the client, decides whether a deferred
	// storage open applies; ClientConfig has no such switch.
	spec, err := at.TorrentSpecFromMagnetUri(src.magnet)
	if err != nil {
		return Info{}, fmt.Errorf("inspect: %s", sanitizeSourceText(err.Error(), src.magnet))
	}
	spec.DeferStorageOpen = true
	spec.DisallowDataDownload = true
	spec.DisallowDataUpload = true

	if cfg.onClient != nil {
		cfg.onClient()
	}
	client, err := at.NewClient(cc)
	if err != nil {
		return Info{}, fmt.Errorf("inspect: start client: %w", err)
	}
	defer func() { _ = client.Close() }()

	t, _, err := client.AddTorrentSpec(spec)
	if err != nil {
		return Info{}, fmt.Errorf("inspect: %s", sanitizeSourceText(err.Error(), src.magnet))
	}
	// Metadata exchange only: no piece data in either direction, even if a peer
	// offers payload before the layout has been looked at.
	t.DisallowDataDownload()
	t.DisallowDataUpload()
	if len(peers) > 0 {
		t.AddPeers(peers)
	}

	metaCtx, cancel := context.WithTimeout(ctx, cfg.MetadataTimeout)
	defer cancel()
	select {
	case <-t.GotInfo():
	case <-metaCtx.Done():
		return Info{}, fmt.Errorf("%w after %s: %w", ErrMetadataTimeout, cfg.MetadataTimeout, metaCtx.Err())
	}
	info := t.Info()
	if info == nil {
		return Info{}, errors.New("torrent reported no info after metadata")
	}
	// Torrent.InfoHash is a 20-byte short hash for v2 torrents, so identity is
	// checked against the metadata bytes instead of that accessor.
	got, err := IdentityFromInfo(info, t.Metainfo().InfoBytes)
	if err != nil {
		return Info{}, err
	}
	if !got.SameTorrent(src.id) {
		return Info{}, fmt.Errorf("metadata identity %s does not match source %s", got, src.id)
	}
	rels, err := validateInfoPaths(info)
	if err != nil {
		return Info{}, err
	}
	return infoFromMetainfo(src, info, rels), nil
}

// requireIdentity refuses a torrent this adapter cannot represent and returns
// the identity it does have. It runs on every path, before any layout is
// reported and before a fetch opens storage: v1, v2-only and hybrid metainfo are
// accepted, a dictionary with neither identity is not.
func requireIdentity(mi *metainfo.MetaInfo, info *metainfo.Info) (Identity, error) {
	if info == nil {
		return Identity{}, fmt.Errorf("%w: no info dictionary", ErrUnsupportedIdentity)
	}
	if info.PieceLength <= 0 {
		return Identity{}, fmt.Errorf("%w: metainfo has no piece length", ErrUnsupportedIdentity)
	}
	if info.HasV2() && len(info.FileTree.Dir) == 0 {
		return Identity{}, fmt.Errorf("%w: v2 info dictionary has no file tree", ErrUnsupportedIdentity)
	}
	return IdentityFromInfo(info, mi.InfoBytes)
}

// infoFromMetainfo projects a validated info dictionary. Offsets mirror the
// library's own file layout (initFiles), so a caller can reason about pieces.
func infoFromMetainfo(src source, info *metainfo.Info, rels []string) Info {
	list := Info{
		InfoHash:    src.id.V1Hex(),
		InfoHashV2:  src.id.V2Hex(),
		Name:        info.BestName(),
		TotalLength: info.TotalLength(),
		Files:       make([]File, 0, len(rels)),
		Source:      src.canonical(),
	}
	var offset int64
	for i, fi := range info.UpvertedFiles() {
		if i >= len(rels) {
			break
		}
		list.Files = append(list.Files, File{Path: rels[i], Length: fi.Length, Offset: offset})
		offset += fi.Length
		if info.FilesArePieceAligned() && info.PieceLength > 0 {
			offset = (offset + info.PieceLength - 1) / info.PieceLength * info.PieceLength
		}
	}
	return list
}

// sanitizeSourceText removes tracker and credential material from text that is
// about to become an error. A raw magnet or .torrent path may carry a passkey,
// so it is replaced rather than echoed, and any surviving magnet URI is cut at
// its query string.
func sanitizeSourceText(text, raw string) string {
	if raw != "" && strings.Contains(text, raw) {
		text = strings.ReplaceAll(text, raw, "<source>")
	}
	if i := strings.Index(text, "magnet:?"); i >= 0 {
		text = text[:i] + "magnet:?<redacted>"
	}
	return text
}
