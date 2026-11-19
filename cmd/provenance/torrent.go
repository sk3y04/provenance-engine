package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sk3y04/provenance-engine/internal/manifest"
	"github.com/sk3y04/provenance-engine/internal/torrentqueue"
	"github.com/sk3y04/provenance-engine/internal/transfer/bittorrent"
)

var (
	flagTorrentOut       string
	flagTorrentSelect    []string
	flagTorrentPeer      []string
	flagTorrentMaxBytes  string
	flagTorrentTimeout   time.Duration
	flagTorrentJSON      bool
	flagTorrentSeed      bool
	flagQueueMaxActive   int
	flagQueueMaxSeeds    int
	flagQueueDownRate    string
	flagQueueUpRate      string
	flagQueuePeers       int
	flagQueueHalfOpen    int
	flagTorrentDiscovery string
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
private swarm. --discovery direct uses ONLY the peers given with --peer: no
trackers, no DHT, no PEX, no webseeds, no local discovery, no port mapping and
no inbound connections from outside this host. An unknown profile is an error,
never a fallback to public. The client does not seed after completion unless
--seed is set, and it is never exposed to a remote endpoint. Only grab content
you are authorized to download.`,
	}
	cmd.PersistentFlags().StringVarP(&flagTorrentOut, "output", "o", "./downloads", "Staging directory for fetch and status (inspect does not use it)")
	cmd.PersistentFlags().StringArrayVar(&flagTorrentPeer, "peer", nil, "Explicit peer address host:port (repeatable, for private swarms)")
	cmd.PersistentFlags().StringVar(&flagTorrentDiscovery, "discovery", "public", "Peer discovery profile: public (trackers, DHT, PEX), tracker (trackers only), or direct (explicit --peer only)")
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

	recheck := &cobra.Command{
		Use: "recheck JOB_ID",
		// A mismatch is a result, not a usage error: printing the flag table after
		// "1 of 1 files no longer match" would bury the finding.
		SilenceUsage: true,
		Short:        "Verify a job's selected files against their recorded digests, offline",
		Long: "Recheck hashes each selected, finalized file and compares it with the digest\n" +
			"recorded when the job's capture was published. It starts no torrent client, contacts\n" +
			"no peer, tracker or web seed, opens no completion database and never repairs: a\n" +
			"mismatch exits nonzero so repair stays an explicit fetch.\n\n" +
			"The job root is --output: a legacy staging root or a directly supplied isolated\n" +
			"workspace. Exit status is 0 only when every selected file matches, so a job with\n" +
			"unchecked files is never reported as verified.\n\n" +
			"A match verifies the working copy against the recorded digest. It is not a\n" +
			"piece-completion scan and says nothing about the vault blob.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTorrentRecheck(cmd.Context(), flagTorrentOut, args[0], os.Stdout)
		},
	}
	recheck.Flags().BoolVar(&flagTorrentJSON, "json", false, "Emit the recheck report as JSON on stdout")
	cmd.AddCommand(recheck)

	queue := &cobra.Command{
		Use:   "queue",
		Short: "Durable local job queue: add paused, start, pause, retry, run",
		Long: "A durable queue over isolated workspaces. Entries are added paused; only an explicit\n" +
			"start makes one runnable. `queue run` drains the queue in the foreground with one worker\n" +
			"and then exits, so nothing keeps running in the background. No daemon, no RPC, no TUI.\n\n" +
			"Interrupting the runner is not a pause: the recorded intent is preserved, so a restart\n" +
			"picks the work back up. Nothing in this command group deletes payload or data.",
	}
	queue.AddCommand(queueAddCmd(), queueListCmd(), queueStartCmd(), queuePauseCmd(), queueRetryCmd(),
		queueSeedCmd(), queueUnseedCmd(), queuePriorityCmd(), queueRemoveCmd(),
		queueLiveCmd(), queueRunCmd())
	cmd.AddCommand(queue)

	return cmd
}

// queueStore opens the queue under --output, which is the job root.
func queueStore() (*torrentqueue.Store, error) {
	return torrentqueue.OpenStore(flagTorrentOut)
}

func queueAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add SOURCE",
		Short: "Add a magnet or .torrent to the queue, paused",
		Long: "Validates the source without contacting the network and records the entry as paused.\n" +
			"A magnet whose identity needs a metadata exchange is accepted as unresolved and keeps\n" +
			"its discovery policy for that exchange; a source this build cannot resume is refused\n" +
			"here rather than accepted as a job that would never start.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(normalizeCLISelection(flagTorrentSelect)) == 0 {
				return errors.New("a queue entry needs --select (repeatable, comma lists accepted)")
			}
			mode, err := bittorrent.ParseDiscovery(flagTorrentDiscovery)
			if err != nil {
				return fmt.Errorf("--discovery: %w", err)
			}
			prepared, err := bittorrent.PrepareSource(flagTorrentOut, args[0], mode)
			if err != nil {
				return err
			}
			store, err := queueStore()
			if err != nil {
				return err
			}
			source := torrentqueue.Source{Kind: prepared.Kind}
			if prepared.Kind == "file" {
				if abs, aerr := filepath.Abs(args[0]); aerr == nil {
					source.Path = abs
				} else {
					source.Path = args[0]
				}
			} else {
				source.Magnet = prepared.Sanitized
			}
			entry, duplicate, err := store.Add(torrentqueue.AddRequest{
				Source:     source,
				RawSource:  args[0],
				Peers:      flagTorrentPeer,
				Discovery:  string(mode),
				Selection:  normalizeCLISelection(flagTorrentSelect),
				JobID:      prepared.JobID,
				InfoHash:   prepared.InfoHash,
				InfoHashV2: prepared.InfoHashV2,
				Workspace:  prepared.Workspace,
				Unresolved: prepared.Unresolved,
			})
			if err != nil {
				return err
			}
			var b strings.Builder
			switch {
			case duplicate:
				fmt.Fprintf(&b, "%s already queued for this selection (%s)\n", entry.ID, torrentqueue.EntryLabel(entry))
			case prepared.Unresolved:
				fmt.Fprintf(&b, "%s added paused; identity unresolved until metadata is fetched\n", entry.ID)
			case entry.JobID != "":
				fmt.Fprintf(&b, "%s added paused for %s (%s)\n", entry.ID, entry.JobID, entry.Workspace)
			default:
				fmt.Fprintf(&b, "%s added paused\n", entry.ID)
			}
			fmt.Fprintf(&b, "start it with: provenance torrent queue start %s\n", entry.ID)
			_, _ = io.WriteString(os.Stdout, b.String())
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&flagTorrentSelect, "select", nil, "Exact file path within the torrent to fetch (repeatable; comma lists accepted)")
	return cmd
}

func queueListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List queued entries from their records (read-only)",
		Long: "Reads the queue records and prints them. Nothing here is live telemetry: phases are\n" +
			"what the runner last recorded, and the only liveness signal shown is whether the runner\n" +
			"lock is currently held.",
		Args: cobra.NoArgs,
		// A failed entry is an outcome, not a usage error: do not bury it under the
		// flag table.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := queueStore()
			if err != nil {
				return err
			}
			entries, err := store.List()
			if err != nil {
				return err
			}
			if flagTorrentJSON {
				payload := struct {
					RunnerHeld bool                 `json:"runner_held"`
					Entries    []torrentqueue.Entry `json:"entries"`
				}{RunnerHeld: torrentqueue.RunnerHeld(store.Root()), Entries: entries}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(payload)
			}
			var b strings.Builder
			fmt.Fprintf(&b, "runner: %s\n", runnerState(store.Root()))
			if len(entries) == 0 {
				fmt.Fprintln(&b, "no entries")
				_, _ = io.WriteString(os.Stdout, b.String())
				return nil
			}
			for _, e := range entries {
				source := e.Source.Magnet
				if source == "" {
					source = "local .torrent"
				}
				target := e.JobID
				if target == "" {
					target = "(unresolved)"
				}
				fmt.Fprintf(&b, "%s  %-7s %-18s %s\n", e.ID, e.Desired, torrentqueue.EntryLabel(e), target)
				fmt.Fprintf(&b, "    source: %s\n", source)
				if e.Capture != nil {
					fmt.Fprintf(&b, "    capture: %s (%s)\n", e.Capture.Key, e.Capture.Manifest)
				}
				if e.LastError != "" {
					fmt.Fprintf(&b, "    error: %s\n", e.LastError)
				}
				if e.Note != "" {
					fmt.Fprintf(&b, "    note: %s\n", e.Note)
				}
			}
			_, _ = io.WriteString(os.Stdout, b.String())
			return nil
		},
	}
	cmd.Flags().BoolVar(&flagTorrentJSON, "json", false, "Emit entries as JSON on stdout")
	return cmd
}

func runnerState(root string) string {
	if torrentqueue.RunnerHeld(root) {
		return "active (runner lock held)"
	}
	return "idle (no runner lock; recorded phases may predate this command)"
}

func queueStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start ID...",
		Short: "Make paused entries runnable",
		Long: "Records desired=run. This is the only way a new entry becomes runnable, and it is\n" +
			"durable: a restart keeps it. A failed entry is refused here - use `queue retry`, so a\n" +
			"failure is never silently retried by a bulk start.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := queueStore()
			if err != nil {
				return err
			}
			var b strings.Builder
			defer func() { _, _ = io.WriteString(os.Stdout, b.String()) }()
			for _, id := range args {
				entry, uerr := store.Update(id, func(e *torrentqueue.Entry) error {
					if e.Phase == torrentqueue.PhaseFailed {
						return fmt.Errorf("entry %s is failed; use `torrent queue retry %s` to make the retry explicit", e.ID, e.ID)
					}
					e.Desired = torrentqueue.DesiredRun
					if e.Phase == torrentqueue.PhasePaused || e.Phase == torrentqueue.PhaseStopping {
						e.Phase = torrentqueue.PhaseQueued
					}
					return nil
				})
				if uerr != nil {
					return uerr
				}
				fmt.Fprintf(&b, "%s  desired=%s phase=%s\n", entry.ID, entry.Desired, torrentqueue.EntryLabel(entry))
			}
			return nil
		},
	}
	return cmd
}

func queuePauseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pause ID...",
		Short: "Record pause intent for entries",
		Long: "Writes desired=paused and acknowledges only after it is durable. A running job is not\n" +
			"killed by this command: the runner observes the intent, reports the entry as stopping\n" +
			"until the client has closed and the workspace is released, and then records paused.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := queueStore()
			if err != nil {
				return err
			}
			var b strings.Builder
			defer func() { _, _ = io.WriteString(os.Stdout, b.String()) }()
			for _, id := range args {
				entry, uerr := store.Update(id, func(e *torrentqueue.Entry) error {
					e.Desired = torrentqueue.DesiredPaused
					if e.Phase == torrentqueue.PhaseQueued {
						e.Phase = torrentqueue.PhasePaused
					}
					return nil
				})
				if uerr != nil {
					return uerr
				}
				switch entry.Phase {
				case torrentqueue.PhaseRunning, torrentqueue.PhaseStopping:
					fmt.Fprintf(&b, "%s  pause recorded; the runner will stop it and report stopping until the client closes\n", entry.ID)
				case torrentqueue.PhaseCompleted:
					fmt.Fprintf(&b, "%s  pause recorded (already completed; the capture is untouched)\n", entry.ID)
				default:
					fmt.Fprintf(&b, "%s  paused\n", entry.ID)
				}
			}
			return nil
		},
	}
	return cmd
}

func queueRetryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retry ID...",
		Short: "Clear a failure or a backoff and make the entry runnable",
		Long: "An explicit retry, separate from start and from recheck: a failed capture retries from\n" +
			"verified local bytes through the normal resume path, and a failed recheck never repairs\n" +
			"anything on its own.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := queueStore()
			if err != nil {
				return err
			}
			var b strings.Builder
			defer func() { _, _ = io.WriteString(os.Stdout, b.String()) }()
			for _, id := range args {
				entry, uerr := store.Update(id, func(e *torrentqueue.Entry) error {
					if e.Phase == torrentqueue.PhaseCompleted {
						return fmt.Errorf("entry %s already completed a capture; nothing to retry", e.ID)
					}
					e.Desired = torrentqueue.DesiredRun
					e.Phase = torrentqueue.PhaseQueued
					e.LastError = ""
					e.Note = ""
					e.NotBefore = time.Time{}
					return nil
				})
				if uerr != nil {
					return uerr
				}
				fmt.Fprintf(&b, "%s  retryable: desired=%s phase=%s\n", entry.ID, entry.Desired, torrentqueue.EntryLabel(entry))
			}
			return nil
		},
	}
	return cmd
}

func queueSeedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "seed ID...",
		Short: "Record seeding intent for completed entries",
		Long: "Writes seed_desired=on and acknowledges only after it is durable. Only a completed\n" +
			"entry with a published capture can be seeded, and download intent is not changed.\n" +
			"Intent does nothing on its own: serving requires `torrent queue run --max-seeds N`.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := queueStore()
			if err != nil {
				return err
			}
			var b strings.Builder
			defer func() { _, _ = io.WriteString(os.Stdout, b.String()) }()
			for _, id := range args {
				entry, uerr := store.SetSeedIntent(id, true)
				if uerr != nil {
					return uerr
				}
				fmt.Fprintf(&b, "%s  seed intent recorded: seed_desired=%s seed_phase=%s\n",
					entry.ID, entry.SeedDesired, entry.SeedPhase)
			}
			return nil
		},
	}
	return cmd
}

func queueUnseedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unseed ID...",
		Short: "Stop seeding an entry and keep it stopped",
		Long: "Writes seed_desired=off durably. A serving seed reports stopping until its client has\n" +
			"closed and the workspace is released; a later run does not resume it. The capture,\n" +
			"payload and workspace are untouched.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := queueStore()
			if err != nil {
				return err
			}
			var b strings.Builder
			defer func() { _, _ = io.WriteString(os.Stdout, b.String()) }()
			for _, id := range args {
				entry, uerr := store.SetSeedIntent(id, false)
				if uerr != nil {
					return uerr
				}
				fmt.Fprintf(&b, "%s  seed intent cleared: seed_desired=%s seed_phase=%s\n",
					entry.ID, entry.SeedDesired, entry.SeedPhase)
			}
			return nil
		},
	}
	return cmd
}

func queueLiveCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "live",
		Short: "Show what the foreground runner is doing right now",
		Long: "Asks the running foreground runner for one read-only snapshot over its local Unix\n" +
			"socket. `queue list` remains the durable view: a recorded phase is never reported as\n" +
			"live, and if no runner answers this command says so instead of guessing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := queueStore()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			snap, lerr := torrentqueue.LiveSnapshotFor(ctx, store.Root())
			if lerr != nil {
				return lerr
			}
			if asJSON {
				out, merr := json.MarshalIndent(snap, "", "  ")
				if merr != nil {
					return merr
				}
				_, _ = io.WriteString(os.Stdout, string(out)+"\n")
				return nil
			}
			var b strings.Builder
			fmt.Fprintf(&b, "runner pid %d on %s since %s\n", snap.PID, snap.Root, snap.StartedAt.Format(time.RFC3339))
			fmt.Fprintf(&b, "download slots %d/%d   seed slots %d/%d\n",
				snap.Downloads.Used, snap.Downloads.Max, snap.Seeds.Used, snap.Seeds.Max)
			if len(snap.Attempts) == 0 {
				fmt.Fprintf(&b, "no active attempts\n")
			}
			for _, a := range snap.Attempts {
				extra := ""
				if a.Stopping {
					extra = "  (slot held until the client closes)"
				}
				if a.UploadedBytes != nil {
					extra += fmt.Sprintf("  uploaded=%d bytes", *a.UploadedBytes)
				}
				fmt.Fprintf(&b, "%s  %s %s%s\n", a.EntryID, a.Kind, a.Phase, extra)
			}
			_, _ = io.WriteString(os.Stdout, b.String())
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print the snapshot as JSON")
	return cmd
}

func queuePriorityCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "priority ENTRY_ID FILE normal|high",
		Short: "Favor an already-selected file when fetching",
		Long: "Records a download preference for one file that is already in the entry's immutable\n" +
			"selection. `high` raises that file's piece priority; `normal` clears the override. It\n" +
			"is a preference, not a guarantee about which file finishes first, and every selected\n" +
			"file is still verified before the capture is published. The edit cannot add or remove\n" +
			"a file, change the workspace or capture, or make a completed entry runnable again.",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := queueStore()
			if err != nil {
				return err
			}
			entry, perr := store.SetPriority(args[0], args[1], args[2])
			if perr != nil {
				return perr
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%s  %s -> %s (selection unchanged: %d files; desired=%s phase=%s)\n",
				entry.ID, args[1], strings.ToLower(args[2]), len(entry.Selection), entry.Desired, entry.Phase)
			for p := range entry.Priorities {
				fmt.Fprintf(&b, "  high: %s\n", p)
			}
			if len(entry.Priorities) > 0 {
				fmt.Fprintf(&b, "  (seed_desired=%s seed_phase=%s)\n", entry.SeedDesired, entry.SeedPhase)
			}
			_, _ = io.WriteString(os.Stdout, b.String())
			return nil
		},
	}
	return cmd
}

func queueRemoveCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "remove ID...",
		Short: "Forget queue entries without deleting any downloaded data",
		Long: "Removes the queue entry and its own restricted source record. The workspace, payload,\n" +
			"resume state, published capture and archive artifacts are left exactly as they are, so\n" +
			"the job can still be inspected, rechecked or re-added. Removal is refused while a\n" +
			"foreground runner owns the queue: stop it and wait for its clients to close first.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := queueStore()
			if err != nil {
				return err
			}
			var b strings.Builder
			results := make([]torrentqueue.Removal, 0, len(args))
			for _, id := range args {
				res, rerr := store.Remove(id)
				if rerr != nil {
					return rerr
				}
				results = append(results, res)
				switch {
				case res.CleanupError != "":
					fmt.Fprintf(&b, "%s  removed; %s\n", res.ID, res.CleanupError)
				default:
					fmt.Fprintf(&b, "%s  removed (payload, workspace and capture kept)\n", res.ID)
				}
			}
			out, merr := json.MarshalIndent(struct {
				Removed []torrentqueue.Removal `json:"removed"`
			}{Removed: results}, "", "  ")
			if merr != nil {
				return merr
			}
			if asJSON {
				_, _ = io.WriteString(os.Stdout, string(out)+"\n")
				return nil
			}
			_, _ = io.WriteString(os.Stdout, b.String())
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print the removal result as JSON")
	return cmd
}

func queueRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Drain the queue in the foreground",
		Long: "Claims runnable entries in queue order and runs up to --max-active of them, then exits.\n" +
			"This is not a daemon: when nothing is running and nothing is eligible it returns. A pause\n" +
			"frees its slot only when the job's client has closed, not when the request is recorded:\n" +
			"when there is nothing left to do it returns. SIGINT stops the active job, records it as\n" +
			"an interruption (not a pause) and leaves every entry's intent intact.",
		Args: cobra.NoArgs,
		// A failed entry is an outcome, not a usage error: do not bury it under the
		// flag table.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Reject an explicit zero: the runner's zero value means "default", so the
			// flag has to refuse 0 itself instead of silently meaning 1.
			if flagQueueMaxActive < 1 {
				return fmt.Errorf("--max-active must be at least 1, got %d", flagQueueMaxActive)
			}
			downRate, err := parseQueueRate("--download-rate", flagQueueDownRate)
			if err != nil {
				return err
			}
			upRate, err := parseQueueRate("--upload-rate", flagQueueUpRate)
			if err != nil {
				return err
			}
			if flagQueueMaxSeeds < 0 || flagQueueMaxSeeds > torrentqueue.MaxSeedLimit {
				return fmt.Errorf("--max-seeds must be between 1 and %d, got %d (omit it to leave managed seeding off)",
					torrentqueue.MaxSeedLimit, flagQueueMaxSeeds)
			}
			store, err := queueStore()
			if err != nil {
				return err
			}
			// --timeout bounds each attempt; the queue has no global deadline so one
			// slow entry cannot consume the whole run.
			runner := torrentqueue.NewRunner(store, torrentqueue.Adapter{Timeout: flagTorrentTimeout},
				torrentqueue.Options{
					MaxActive:      flagQueueMaxActive,
					DownloadLimit:  downRate,
					UploadLimit:    upRate,
					PeersPerJob:    flagQueuePeers,
					HalfOpenPerJob: flagQueueHalfOpen,
					MaxSeeds:       flagQueueMaxSeeds,
					Log:            os.Stdout,
				})
			sum, rerr := runner.Run(cmd.Context())
			if rerr != nil {
				return rerr
			}
			_, _ = fmt.Fprintf(os.Stdout, "drained: %d completed, %d failed, %d blocked, %d paused, %d claimed\n",
				sum.Completed, sum.Failed, sum.Blocked, sum.Paused, len(sum.Ran))
			if sum.Failed > 0 {
				return fmt.Errorf("queue run: %d entr%s failed; inspect with `torrent queue list`", sum.Failed, plural(sum.Failed))
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&flagQueueMaxActive, "max-active", 1, "Concurrent attempts to run at once (1-32)")
	cmd.Flags().IntVar(&flagQueueMaxSeeds, "max-seeds", 0,
		"Seed slots for completed entries with seed intent (1-32); 0 leaves managed seeding off")
	cmd.Flags().StringVar(&flagQueueDownRate, "download-rate", "", "Aggregate download budget shared by every attempt, e.g. 2MB (bytes/second; empty means unlimited)")
	cmd.Flags().StringVar(&flagQueueUpRate, "upload-rate", "", "Aggregate upload budget shared by every attempt, e.g. 512KB (bytes/second; empty means unlimited)")
	cmd.Flags().IntVar(&flagQueuePeers, "peers-per-job", 0, "Established peers per torrent per attempt (0 keeps the default of 40; max 200)")
	cmd.Flags().IntVar(&flagQueueHalfOpen, "half-open-per-job", 0, "Outbound handshakes in progress per torrent per attempt (0 derives it from the peer limit)")
	return cmd
}

// parseQueueRate turns a --download-rate/--upload-rate value into bytes per second.
// The suffix convention is the one --max-bytes already uses; empty means unlimited.
func parseQueueRate(flag, raw string) (int64, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	n, err := manifest.ParseSize(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", flag, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be a positive size in bytes per second, got %q (omit the flag for unlimited)", flag, raw)
	}
	return n, nil
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// runTorrentRecheck prints the report and turns a non-verified outcome into a
// nonzero exit. JSON always lands on stdout, including on failure, so a caller can
// parse it; the one-line reason goes to stderr through the command error.
func runTorrentRecheck(ctx context.Context, root, jobID string, stdout io.Writer) error {
	report, err := bittorrent.Recheck(ctx, root, jobID)
	if flagTorrentJSON {
		if err != nil {
			report.Error = err.Error()
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if merr := enc.Encode(report); merr != nil {
			return merr
		}
		if err != nil {
			return err
		}
	} else {
		if err != nil {
			return err
		}
		printRecheck(stdout, report)
	}
	if !report.Verified {
		return errors.New("recheck: " + report.UnverifiedProblem() + "; nothing was repaired")
	}
	return nil
}

// printRecheck renders the human report. It keeps the historical capture and the
// current outcome in separate blocks on purpose.
func printRecheck(w io.Writer, r bittorrent.RecheckReport) {
	var b strings.Builder
	fmt.Fprintf(&b, "job %s\n", r.JobID)
	if r.InfoHash != "" {
		fmt.Fprintf(&b, "  btih %s\n", r.InfoHash)
	}
	if r.InfoHashV2 != "" {
		fmt.Fprintf(&b, "  btmh %s\n", r.InfoHashV2)
	}
	if r.Capture != nil {
		state := "manifest missing"
		if r.Capture.Present {
			state = "manifest present"
		}
		reconciled := ""
		if r.Capture.Reconciled {
			reconciled = ", reconciled"
		}
		fmt.Fprintf(&b, "capture (historical): %s  key %s  committed %s%s  %s\n",
			r.Capture.Manifest, r.Capture.Key, r.Capture.CommittedAt.UTC().Format(time.RFC3339), reconciled, state)
	} else {
		fmt.Fprintln(&b, "capture (historical): none published")
	}
	fmt.Fprintf(&b, "selected: %d file(s)\n", r.Summary.Selected)
	for _, f := range r.Files {
		fmt.Fprintf(&b, "  %-12s %s", strings.ToUpper(string(f.Status)), f.Path)
		if f.SHA256 != "" {
			fmt.Fprintf(&b, "  %d bytes  sha256 %s", f.Bytes, f.SHA256)
		}
		if f.AttestedSHA256 != "" && f.SHA256 != "" && f.SHA256 != f.AttestedSHA256 {
			fmt.Fprintf(&b, "  attested %s", f.AttestedSHA256)
		}
		if f.Detail != "" {
			fmt.Fprintf(&b, "  (%s)", f.Detail)
		}
		fmt.Fprintln(&b)
	}
	s := r.Summary
	fmt.Fprintf(&b, "recheck (now): %d match, %d mismatch, %d missing, %d partial, %d unverifiable, %d error\n",
		s.Match, s.Mismatch, s.Missing, s.Partial, s.Unverifiable, s.Error)
	if r.Detail != "" {
		fmt.Fprintf(&b, "note: %s\n", r.Detail)
	}
	if r.Verified {
		fmt.Fprintln(&b, "verified: yes (every selected file matches its recorded digest)")
	} else {
		fmt.Fprintln(&b, "verified: no")
		fmt.Fprintln(&b, "repair: rerun the fetch that created this job with the same source and selection;")
		fmt.Fprintln(&b, "        recheck itself never repairs, re-downloads or rewrites a capture")
	}
	_, _ = io.WriteString(w, b.String())
}

// torrentConfig maps the CLI flags onto the adapter configuration. An unknown
// --discovery value is returned as an error and never replaced by the default.
func torrentConfig(maxBytes int64, onUpdate func(bittorrent.Update)) (bittorrent.Config, error) {
	mode, err := bittorrent.ParseDiscovery(flagTorrentDiscovery)
	if err != nil {
		return bittorrent.Config{}, fmt.Errorf("--discovery: %w", err)
	}
	return bittorrent.Config{
		StagingDir:       flagTorrentOut,
		Peers:            flagTorrentPeer,
		Discovery:        mode,
		MaxSelectedBytes: maxBytes,
		MaxPeers:         40,
		Timeout:          flagTorrentTimeout,
		KeepSeeding:      flagTorrentSeed,
		OnUpdate:         onUpdate,
	}, nil
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
	mode, err := bittorrent.ParseDiscovery(flagTorrentDiscovery)
	if err != nil {
		return fmt.Errorf("--discovery: %w", err)
	}
	info, err := bittorrent.Inspect(ctx, source, bittorrent.InspectConfig{
		Peers:           flagTorrentPeer,
		Discovery:       mode,
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
	if info.Private {
		_, _ = fmt.Fprintf(os.Stderr, "[provenance] private=1 (BEP 27): fetch with --discovery tracker, or direct with --peer\n")
	}
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
	cfg, err := torrentConfig(maxBytes, progress.report)
	if err != nil {
		return err
	}
	sess, err := bittorrent.New(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()

	_, _ = fmt.Fprintf(os.Stderr, "[provenance] fetching %s (discovery %s)\n", redactSource(source), cfg.Discovery)
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
