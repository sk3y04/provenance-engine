package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sk3y04/provenance-engine/internal/manifest"
	"github.com/sk3y04/provenance-engine/internal/transfer/bittorrent"
)

var (
	flagTorrentOut      string
	flagTorrentSelect   []string
	flagTorrentPeer     []string
	flagTorrentMaxBytes string
	flagTorrentTimeout  time.Duration
	flagTorrentJSON     bool
	flagTorrentSeed     bool
)

func torrentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "torrent",
		Short: "Selective BitTorrent fetch into a staging directory",
		Long: `Fetch BitTorrent content without downloading a whole torrent.

A source is a magnet link (v1, urn:btih) or a local .torrent file. Use
"torrent inspect" to list the files and their exact selectable paths, then
"torrent fetch --select PATH" to download only those files. Selection is
required and exact; there is no "download everything" flag.

Downloads land in <out>/<torrent name>/ and every selected file is verified
against the torrent's piece hashes before a capture manifest is written to
<out>/.provenance/. That manifest is the same format as collect --record, so
"provenance manifest verify <out>" and "provenance archive import <out>"
work on the result unchanged.

Peers are found with trackers and DHT, or passed explicitly with --peer for a
private swarm. The client does not seed after completion unless --seed is set,
and it is never exposed to a remote endpoint. Only grab content you are
authorized to download.`,
	}
	cmd.PersistentFlags().StringVarP(&flagTorrentOut, "output", "o", "./downloads", "Staging directory for this job")
	cmd.PersistentFlags().StringArrayVar(&flagTorrentPeer, "peer", nil, "Explicit peer address host:port (repeatable, for private swarms)")
	cmd.PersistentFlags().StringVar(&flagTorrentMaxBytes, "max-bytes", "", "Reject a selection larger than this, e.g. 4GB")
	cmd.PersistentFlags().DurationVar(&flagTorrentTimeout, "timeout", 0, "Overall transfer deadline, e.g. 30m (default: no limit)")
	cmd.PersistentFlags().BoolVar(&flagTorrentSeed, "seed", false, "Keep seeding the completed torrent until interrupted")

	inspect := &cobra.Command{
		Use:   "inspect SOURCE",
		Short: "List the files in a magnet or .torrent without downloading payload",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTorrentInspect(cmd.Context(), args[0])
		},
	}
	inspect.Flags().BoolVar(&flagTorrentJSON, "json", false, "Emit the file list as JSON")
	cmd.AddCommand(inspect)

	fetch := &cobra.Command{
		Use:   "fetch SOURCE",
		Short: "Download only the selected files and record a capture manifest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTorrentFetch(cmd.Context(), args[0])
		},
	}
	fetch.Flags().StringArrayVar(&flagTorrentSelect, "select", nil, "Exact file path to download (repeatable; see inspect)")
	fetch.Flags().BoolVar(&flagTorrentJSON, "json", false, "Emit the capture result as JSON")
	cmd.AddCommand(fetch)

	return cmd
}

// torrentConfig maps the CLI flags onto the adapter configuration.
func torrentConfig(maxBytes int64, onUpdate func(bittorrent.Update)) bittorrent.Config {
	return bittorrent.Config{
		StagingDir:       flagTorrentOut,
		Peers:            flagTorrentPeer,
		MaxSelectedBytes: maxBytes,
		MaxPeers:         40,
		Timeout:          flagTorrentTimeout,
		KeepSeeding:      flagTorrentSeed,
		OnUpdate:         onUpdate,
	}
}

func parseMaxBytes() (int64, error) {
	n, err := manifest.ParseSize(flagTorrentMaxBytes)
	if err != nil {
		return 0, fmt.Errorf("--max-bytes: %w", err)
	}
	return n, nil
}

func runTorrentInspect(ctx context.Context, source string) error {
	maxBytes, err := parseMaxBytes()
	if err != nil {
		return err
	}
	sess, err := bittorrent.New(torrentConfig(maxBytes, nil))
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()

	info, err := sess.List(ctx, source)
	if err != nil {
		return err
	}
	if flagTorrentJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}
	_, _ = fmt.Fprintf(os.Stderr, "[provenance] infohash %s\n", info.InfoHash)
	_, _ = fmt.Fprintf(os.Stderr, "[provenance] %s — %d file(s), %s total\n",
		info.Name, len(info.Files), manifest.HumanSize(info.TotalLength))
	for i, f := range info.Files {
		_, _ = fmt.Fprintf(os.Stdout, "%2d. %10s  %s\n", i+1, manifest.HumanSize(f.Length), f.Path)
	}
	_, _ = fmt.Fprintln(os.Stderr, "\n[provenance] pass one of these paths to: provenance torrent fetch SOURCE --select PATH")
	return nil
}

func runTorrentFetch(ctx context.Context, source string) error {
	selection := normalizeCLISelection(flagTorrentSelect)
	if len(selection) == 0 {
		return fmt.Errorf("--select is required: run 'provenance torrent inspect %s' to list paths", source)
	}
	maxBytes, err := parseMaxBytes()
	if err != nil {
		return err
	}
	progress := &torrentProgress{}
	sess, err := bittorrent.New(torrentConfig(maxBytes, progress.report))
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()

	_, _ = fmt.Fprintf(os.Stderr, "[provenance] fetching %s\n", redactSource(source))
	cap, err := sess.Fetch(ctx, source, selection)
	if err != nil {
		return err
	}
	if flagTorrentJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(cap)
	}
	for _, w := range cap.Warnings {
		_, _ = fmt.Fprintf(os.Stderr, "[provenance] warning: %s\n", w)
	}
	if cap.Reconciled {
		_, _ = fmt.Fprintf(os.Stderr, "[provenance] existing capture reconciled for job %s (no duplicate recorded)\n", cap.JobID)
	} else {
		for _, a := range cap.Assets {
			_, _ = fmt.Fprintf(os.Stderr, "[provenance] captured %s (%s, sha256 %s)\n",
				a.Path, manifest.HumanSize(a.Length), a.SHA256)
		}
	}
	_, _ = fmt.Fprintf(os.Stderr, "[provenance] job %s: payload=%s capture=%s seeding=%s\n",
		cap.JobID, cap.State.Payload, cap.State.Capture, cap.State.Seeding)
	_, _ = fmt.Fprintf(os.Stderr, "[provenance] capture manifest: %s\n", cap.ManifestPath)
	_, _ = fmt.Fprintf(os.Stderr, "[provenance] verify with: provenance manifest verify %s\n", cap.StagingDir)
	_, _ = fmt.Fprintf(os.Stderr, "[provenance] archive with: provenance archive import %s --collection NAME\n", cap.StagingDir)
	return nil
}

// normalizeCLISelection splits comma-separated values as well as repeated
// flags, so both --select a --select b and --select a,b work.
func normalizeCLISelection(raw []string) []string {
	var out []string
	for _, v := range raw {
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// redactSource shortens a source for terminal output: a magnet keeps only its
// infohash (never tracker URLs, which can contain passkeys), a path keeps only
// its base name.
func redactSource(source string) string {
	s := strings.TrimSpace(source)
	if strings.HasPrefix(strings.ToLower(s), "magnet:") {
		if i := strings.Index(s, "urn:btih:"); i >= 0 {
			rest := s[i+len("urn:btih:"):]
			if j := strings.IndexAny(rest, "&"); j >= 0 {
				rest = rest[:j]
			}
			return "magnet (" + rest + ")"
		}
		return "magnet"
	}
	return filepathBase(s)
}

func filepathBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 && i+1 < len(p) {
		return p[i+1:]
	}
	return p
}

// torrentProgress prints at most one line per 10% of the selection, so piped
// output stays readable and a slow transfer still reports progress.
type torrentProgress struct {
	lastStep int
	lastAt   time.Time
}

func (p *torrentProgress) report(u bittorrent.Update) {
	if u.Total <= 0 {
		return
	}
	step := int(u.Written * 100 / u.Total / 10 * 10)
	if step <= p.lastStep && time.Since(p.lastAt) < 2*time.Second {
		return
	}
	p.lastStep = step
	p.lastAt = time.Now()
	_, _ = fmt.Fprintf(os.Stderr, "[provenance] %s / %s (%d%%)\n",
		manifest.HumanSize(u.Written), manifest.HumanSize(u.Total), step)
}
