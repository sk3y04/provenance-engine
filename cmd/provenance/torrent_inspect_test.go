package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// torrentFixture writes a one-file .torrent and returns its path.
func torrentFixture(t *testing.T) (string, string) {
	t.Helper()
	content := []byte("abcd")
	info := metainfo.Info{
		Name:        "fixture",
		PieceLength: 16 * 1024,
		Files:       []metainfo.FileInfo{{Length: int64(len(content)), Path: []string{"a.bin"}}},
	}
	if err := info.GeneratePieces(func(metainfo.FileInfo) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(content)), nil
	}); err != nil {
		t.Fatalf("generate pieces: %v", err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	mi.SetDefaults()

	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatalf("write metainfo: %v", err)
	}
	path := filepath.Join(t.TempDir(), "fixture.torrent")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write torrent: %v", err)
	}
	return path, mi.HashInfoBytes().HexString()
}

// TestTorrentInspectLocalTorrent: inspecting a local .torrent reports the layout
// from a JSON document and never touches the staging root named by -o.
func TestTorrentInspectLocalTorrent(t *testing.T) {
	saveTorrentFlags(t)
	path, infohash := torrentFixture(t)
	flagTorrentOut = filepath.Join(t.TempDir(), "never-created")
	flagTorrentJSON = true

	var out bytes.Buffer
	if err := runTorrentInspect(context.Background(), path, &out); err != nil {
		t.Fatalf("inspect: %v", err)
	}

	var info struct {
		InfoHash    string `json:"info_hash"`
		Name        string `json:"name"`
		TotalLength int64  `json:"total_length"`
		Source      string `json:"source"`
		Files       []struct {
			Path   string `json:"path"`
			Length int64  `json:"length"`
			Offset int64  `json:"offset"`
		} `json:"files"`
	}
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
	}
	if info.InfoHash != infohash || info.Name != "fixture" || len(info.Files) != 1 {
		t.Fatalf("info = %+v", info)
	}
	if info.Files[0].Path != "fixture/a.bin" || info.Files[0].Length != 4 {
		t.Errorf("file = %+v", info.Files[0])
	}
	if info.Source != "magnet:?xt=urn:btih:"+infohash {
		t.Errorf("source = %q, want a sanitised magnet", info.Source)
	}
	if _, err := os.Stat(flagTorrentOut); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("inspect created the staging root %s", flagTorrentOut)
	}
}

// TestTorrentInspectHumanOutput: the human listing names the files stdout and the
// hints stderr, so a pipeline gets only the list.
func TestTorrentInspectHumanOutput(t *testing.T) {
	saveTorrentFlags(t)
	path, _ := torrentFixture(t)
	flagTorrentJSON = false

	var out bytes.Buffer
	if err := runTorrentInspect(context.Background(), path, &out); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("fixture/a.bin")) {
		t.Errorf("stdout missing the file list:\n%s", out.String())
	}
	if bytes.Contains(out.Bytes(), []byte("[provenance]")) {
		t.Errorf("diagnostics leaked into stdout:\n%s", out.String())
	}
}

// TestTorrentInspectMalformedV2Identity: metainfo that claims v2 without a file
// tree is refused by name rather than silently treated as v1. Well-formed v2
// torrents are accepted by the same command path.
func TestTorrentInspectMalformedV2Identity(t *testing.T) {
	saveTorrentFlags(t)
	path, _ := torrentFixture(t)
	mi, err := metainfo.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	info.MetaVersion = 2
	v2 := &metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	v2.SetDefaults()
	var buf bytes.Buffer
	if err := v2.Write(&buf); err != nil {
		t.Fatal(err)
	}
	v2path := filepath.Join(t.TempDir(), "v2.torrent")
	if err := os.WriteFile(v2path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err = runTorrentInspect(context.Background(), v2path, &out)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("unsupported torrent identity")) {
		t.Fatalf("inspect(v2) = %v, want an unsupported identity error", err)
	}
	if out.Len() != 0 {
		t.Errorf("a rejected inspect wrote to stdout: %q", out.String())
	}
}
