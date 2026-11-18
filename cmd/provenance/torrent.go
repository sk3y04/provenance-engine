package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	cmd.PersistentFlags().StringVarP(&flagTorrentOut, "output", "o", "./downloads", "Staging directory for fetch and status (inspect does not use it)")
	cmd.PersistentFlags().StringArrayVar(&flagTorrentPeer, "peer", nil, "Explicit peer address host:port (repeatable, for private swarms)")
	cmd.PersistentFlags().StringVar(&flagTorrentMaxBytes, "max-bytes", "", "Reject a selection larger than this, e.g. 4GB")
	cmd.PersistentFlags().DurationVar(&flagTorrentTimeout, "timeout", 0, "Overall transfer deadline, e.g. 30m (default: no limit)")
	cmd.PersistentFlags().BoolVar(&flagTorrentSeed, "seed", false, "Keep seeding the completed torrent until interrupted")

	inspect := &cobra.Command{
		Use:   "inspect SOURCE",
		Short: "List the files in a magnet or .torrent without downloading payload",
		Long: `Read a torrent's file layout without downloading payload.

A local .torrent is parsed in memory: no client is built, nothing is dialled, and
no temporary directory is created. A magnet is resolved through a short-lived
client that is bounded by --timeout (metadata only, payload transfer disabled)
and closed before the command returns.

Inspection is job-free: it does not use -o, does not take the staging lock, does
not create payload directories, a piece-completion database or a job record, and
does not persist metainfo. It can run while a fetch owns the same staging root.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTorrentInspect(cmd.Context(), args[0], os.Stdout)
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

	status := &cobra.Command{
		Use:   "status [JOB_ID]",
		Short: "Show the recorded state of torrent jobs (read-only)",
		Long: `Report what the job records in a staging root say, without touching the
network, the payload, or the writer.

With no argument every recorded job is listed in job-id order. With a job id
("bt-<infohash>" or the bare infohash) only that job is shown.

All output is recorded state: the values the last writer of that record stored,
from the last time the record changed. It is not a liveness check — a job can
say "seeding" after the process that was seeding died, and a stale lock file
means nothing here. Status reads the job record only; it does not take the
staging lock, open the piece database, start a client, verify payload hashes,
create directories, or rewrite old records, so it works while another process
owns the root.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTorrentStatus(args, os.Stdout)
		},
	}
	status.Flags().BoolVar(&flagTorrentJSON, "json", false, "Emit recorded job state as JSON")
	cmd.AddCommand(status)

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

func runTorrentInspect(ctx context.Context, source string, stdout io.Writer) error {
	// Inspection is metadata-only: it takes no staging lock, writes nothing to
	// the staging root, and opens no payload storage, so -o is not used here.
	// A local .torrent is parsed in memory; a magnet gets a temporary client
	// that is closed before this returns.
	info, err := bittorrent.Inspect(ctx, source, bittorrent.InspectConfig{
		Peers:           flagTorrentPeer,
		MetadataTimeout: flagTorrentTimeout,
	})
	if err != nil {
		return err
	}
	if flagTorrentJSON {
		return writeTorrentJSON(stdout, info)
	}
	if info.InfoHash != "" {
		_, _ = fmt.Fprintf(os.Stderr, "[provenance] infohash %s\n", info.InfoHash)
	}
	if info.InfoHashV2 != "" {
		_, _ = fmt.Fprintf(os.Stderr, "[provenance] infohash (v2) %s\n", info.InfoHashV2)
	}
	_, _ = fmt.Fprintf(os.Stderr, "[provenance] %s — %d file(s), %s total\n",
		info.Name, len(info.Files), manifest.HumanSize(info.TotalLength))
	for i, f := range info.Files {
		_, _ = fmt.Fprintf(stdout, "%2d. %10s  %s\n", i+1, manifest.HumanSize(f.Length), f.Path)
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
		return writeTorrentJSON(os.Stdout, cap)
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

// writeTorrentJSON renders one value as indented JSON on w. It is the single
// JSON path for the torrent commands, so `--json` output stays consistent.
func writeTorrentJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// runTorrentStatus reports recorded job state. It is read-only: no Session, no
// staging lock, no client, no payload reads.
func runTorrentStatus(args []string, stdout io.Writer) error {
	if len(args) == 1 {
		status, err := bittorrent.ReadJobStatus(flagTorrentOut, args[0])
		if err != nil {
			return err
		}
		if flagTorrentJSON {
			return writeTorrentJSON(stdout, status)
		}
		printJobStatus(stdout, status)
		return nil
	}

	listing, err := bittorrent.ListJobs(flagTorrentOut)
	if err != nil {
		return err
	}
	if flagTorrentJSON {
		if err := writeTorrentJSON(stdout, listing); err != nil {
			return err
		}
	} else {
		printJobListing(stdout, listing)
	}
	// Diagnostics go to stderr, never into the JSON document.
	for _, bad := range listing.Unreadable {
		_, _ = fmt.Fprintf(os.Stderr, "[provenance] warning: unreadable job record %s: %s\n", bad.File, bad.Reason)
	}
	if n := len(listing.Unreadable); n > 0 {
		return fmt.Errorf("%d job record(s) in %s could not be read; the listing is incomplete", n, listing.StagingRoot)
	}
	return nil
}

// printJobListing renders one line per job plus the few fields that make a job
// identifiable, without dumping the record.
func printJobListing(w io.Writer, listing bittorrent.JobListing) {
	if len(listing.Jobs) == 0 {
		_, _ = fmt.Fprintf(w, "[provenance] no torrent jobs recorded in %s\n", listing.StagingRoot)
		return
	}
	_, _ = fmt.Fprintf(w, "[provenance] %d job(s) in %s\n", len(listing.Jobs), listing.StagingRoot)
	_, _ = fmt.Fprintf(w, "[provenance] recorded state from each job's last update; not a liveness check\n")
	for _, job := range listing.Jobs {
		_, _ = fmt.Fprintf(w, "\n%s  payload=%s capture=%s seeding=%s\n",
			job.JobID, job.RecordedState.Payload, job.RecordedState.Capture, job.RecordedState.Seeding)
		if job.InfoHash != "" {
			_, _ = fmt.Fprintf(w, "  infohash   %s\n", job.InfoHash)
		}
		if job.InfoHashV2 != "" {
			_, _ = fmt.Fprintf(w, "  infohash2  %s\n", job.InfoHashV2)
		}
		if len(job.Selection) > 0 {
			_, _ = fmt.Fprintf(w, "  files      %s\n", strings.Join(job.Selection, ", "))
		}
		_, _ = fmt.Fprintf(w, "  updated    %s (recorded)\n", formatRecordedTime(job.UpdatedAt))
		if job.Capture != nil && job.Capture.Manifest != "" {
			_, _ = fmt.Fprintf(w, "  manifest   %s\n", job.Capture.Manifest)
		}
	}
}

// printJobStatus renders one job as labelled fields.
func printJobStatus(w io.Writer, job bittorrent.JobStatus) {
	_, _ = fmt.Fprintf(w, "job            %s\n", job.JobID)
	_, _ = fmt.Fprintf(w, "backend        %s\n", job.Backend)
	if job.InfoHash != "" {
		_, _ = fmt.Fprintf(w, "infohash       %s\n", job.InfoHash)
	}
	if job.InfoHashV2 != "" {
		_, _ = fmt.Fprintf(w, "infohash v2    %s\n", job.InfoHashV2)
	}
	if job.StagingRef != "" {
		_, _ = fmt.Fprintf(w, "staging ref    %s\n", job.StagingRef)
	}
	if len(job.Selection) > 0 {
		_, _ = fmt.Fprintf(w, "selection      %s\n", strings.Join(job.Selection, ", "))
	}
	_, _ = fmt.Fprintf(w, "created        %s (recorded)\n", formatRecordedTime(job.CreatedAt))
	_, _ = fmt.Fprintf(w, "updated        %s (recorded)\n", formatRecordedTime(job.UpdatedAt))
	_, _ = fmt.Fprintf(w, "recorded state payload=%s capture=%s seeding=%s\n",
		job.RecordedState.Payload, job.RecordedState.Capture, job.RecordedState.Seeding)
	_, _ = fmt.Fprintf(w, "               last job update; this is not a liveness check\n")
	if job.Payload != nil {
		_, _ = fmt.Fprintf(w, "payload        verified %s, %d file(s) (recorded)\n",
			formatRecordedTime(job.Payload.VerifiedAt), job.Payload.Files)
	}
	if job.Capture != nil {
		_, _ = fmt.Fprintf(w, "capture        committed %s (recorded)\n", formatRecordedTime(job.Capture.CommittedAt))
		if job.Capture.Manifest != "" {
			_, _ = fmt.Fprintf(w, "manifest       %s\n", job.Capture.Manifest)
		}
		if job.Capture.CaptureKey != "" {
			_, _ = fmt.Fprintf(w, "capture key    %s\n", job.Capture.CaptureKey)
		}
		if job.Capture.Reconciled {
			_, _ = fmt.Fprintf(w, "reconciled     yes (adopted from an existing manifest)\n")
		}
	}
	_, _ = fmt.Fprintf(w, "record format  %s\n", job.RecordFormat)
}

// formatRecordedTime renders a recorded timestamp, or "unknown" when the record
// carries none. It never implies freshness.
func formatRecordedTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
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
