package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sk3y04/provenance-engine/internal/slskd"
)

var (
	flagSlskdPeer       string
	flagSlskdDir        string
	flagSlskdFiles      []string
	flagSlskdLimit      int
	flagSlskdTimeoutMS  int
	flagSlskdMinSpeed   int
	flagSlskdIgnore     []string
	flagSlskdJSON       bool
	flagSlskdStateDir   string
	flagSlskdVault      string
	flagSlskdCollection string
	flagSlskdInterval   time.Duration
	flagSlskdOnce       bool
	flagSlskdSettle     time.Duration
	flagSlskdRemove     bool
	flagSlskdAnother    bool
	flagSlskdAddr       string
)

func searchFlags(cmd *cobra.Command) *cobra.Command {
	cmd.Flags().IntVar(&flagSlskdTimeoutMS, "timeout-ms", 15000, "slskd search timeout in milliseconds (the upstream docs say seconds; the binary says ms)")
	cmd.Flags().IntVar(&flagSlskdLimit, "limit", 0, "maximum ranked folders to print (0 = all)")
	cmd.Flags().IntVar(&flagSlskdMinSpeed, "min-speed", 0, "minimum peer upload speed in bytes/second")
	cmd.Flags().StringSliceVar(&flagSlskdIgnore, "ignore", nil, "peer usernames to drop from results")
	cmd.Flags().BoolVar(&flagSlskdJSON, "json", false, "print JSON instead of a table")
	cmd.Flags().DurationVar(&flagSlskdSettle, "settle", 5*time.Second, "delay before deleting the search in slskd; 0s deletes immediately (~5s avoids a search.db concurrency bug)")
	return cmd
}

func soulseekSearchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search Soulseek and print ranked peer folders",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := slskdClient(cmd.Context())
			if err != nil {
				return err
			}
			coord := slskd.NewSearchCoordinator(client)
			res, err := coord.Search(cmd.Context(), slskd.SearchOptions{
				Query:                  strings.Join(args, " "),
				TimeoutMS:              flagSlskdTimeoutMS,
				MinimumPeerUploadSpeed: flagSlskdMinSpeed,
				IgnoredPeers:           flagSlskdIgnore,
				MaxResults:             flagSlskdLimit,
				SettleGrace:            settleGrace(),
			})
			if err != nil {
				return err
			}
			// The process ends here, so the deferred DELETE must be awaited or it
			// would never run and the search would stay in slskd forever.
			defer waitSearchCleanup(cmd, coord)
			if flagSlskdJSON {
				return printJSON(res)
			}
			fmt.Printf("search %s: %d files, %d locked, %d folders (%.1fs)\n",
				res.SearchID, res.RawFileCount, res.LockedFileCount, len(res.Groups), res.Duration.Seconds())
			if res.Stopped {
				fmt.Println("note: the search was stopped at the wall-clock limit; results are partial")
			}
			printGroups(res.Groups)
			return nil
		},
	}
	return searchFlags(cmd)
}

func printGroups(groups []slskd.ResultGroup) {
	if len(groups) == 0 {
		fmt.Println("no results")
		return
	}
	for i, g := range groups {
		fmt.Printf("%3d  %-24s  %-52s  %3d file(s)  %s  %s/s  slot=%v queue=%d\n",
			i+1, g.Username, truncate(g.Directory, 52), len(g.Files), humanBytes(g.TotalBytes),
			humanBytes(int64(g.UploadSpeed)), yesNo(g.HasFreeUploadSlot), g.QueueLength)
	}
}

func soulseekDownloadCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "download <query>",
		Short: "Search, then enqueue a peer folder as a durable download job",
		Long: `Search Soulseek, select the folder offered by --peer (optionally narrowed
with --dir or --file), and enqueue it as a durable job.

The search runs again for this command: results are never cached between
processes, so what is enqueued is always what this peer offers right now.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(flagSlskdPeer) == "" {
				return errors.New("--peer is required")
			}
			client, settings, err := slskdClient(cmd.Context())
			if err != nil {
				return err
			}
			root, err := slskdStateRoot(settings)
			if err != nil {
				return err
			}
			store, err := slskd.OpenJobStore(root)
			if err != nil {
				return err
			}
			coord := slskd.NewSearchCoordinator(client)
			res, err := coord.Search(cmd.Context(), slskd.SearchOptions{
				Query:                  strings.Join(args, " "),
				TimeoutMS:              flagSlskdTimeoutMS,
				MinimumPeerUploadSpeed: flagSlskdMinSpeed,
				IgnoredPeers:           flagSlskdIgnore,
				SettleGrace:            settleGrace(),
			})
			if err != nil {
				return err
			}
			defer waitSearchCleanup(cmd, coord)
			selected, alternates, err := selectGroups(res.Groups, flagSlskdPeer, flagSlskdDir, flagSlskdFiles)
			if err != nil {
				return err
			}
			files := make([]slskd.File, 0)
			for _, g := range selected {
				files = append(files, g.Files...)
			}
			reconciler := slskd.NewReconciler(client, store)
			out, err := reconciler.Enqueue(cmd.Context(), slskd.EnqueueRequest{
				SearchID:   res.SearchID,
				SearchText: res.Query,
				Peer:       flagSlskdPeer,
				Files:      files,
				Alternates: alternates,
			})
			if err != nil {
				return err
			}
			if out.Duplicate {
				fmt.Printf("already queued as %s (%d file(s)); nothing new enqueued\n", out.Job.ID, len(out.Job.Items))
				return nil
			}
			fmt.Printf("queued %s: %d file(s) from %s -> %s\n", out.Job.ID, len(out.Job.Items), out.Job.Peer, out.Job.DestinationSubdir)
			for _, f := range out.Failures {
				fmt.Printf("  refused %s: %s\n", f.Filename, f.Message)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&flagSlskdPeer, "peer", "", "peer username to download from (required)")
	cmd.Flags().StringVar(&flagSlskdDir, "dir", "", "narrow to one remote folder")
	cmd.Flags().StringSliceVar(&flagSlskdFiles, "file", nil, "narrow to specific remote files")
	cmd.Flags().IntVar(&flagSlskdTimeoutMS, "timeout-ms", 15000, "slskd search timeout in milliseconds")
	cmd.Flags().IntVar(&flagSlskdMinSpeed, "min-speed", 0, "minimum peer upload speed in bytes/second")
	cmd.Flags().StringSliceVar(&flagSlskdIgnore, "ignore", nil, "peer usernames to drop from results")
	cmd.Flags().DurationVar(&flagSlskdSettle, "settle", 5*time.Second, "delay before deleting the search in slskd; 0s deletes immediately (~5s avoids a search.db concurrency bug)")
	return cmd
}

// settleGrace maps the CLI's "0 means now" onto the package's "negative means no
// grace" convention; a zero value there means "use the 5s default".
func settleGrace() time.Duration {
	if flagSlskdSettle == 0 {
		return -1
	}
	return flagSlskdSettle
}

// waitSearchCleanup blocks until the deferred search deletion has run, bounded so
// a stuck slskd cannot hang a finished command.
func waitSearchCleanup(cmd *cobra.Command, coord *slskd.SearchCoordinator) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(cmd.Context()), flagSlskdSettle+10*time.Second)
	defer cancel()
	if err := coord.WaitCleanup(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "note: search cleanup did not finish: %v\n", err)
	}
}

func soulseekJobsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "List durable download jobs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, settings, err := slskdClient(cmd.Context())
			if err != nil {
				return err
			}
			root, err := slskdStateRoot(settings)
			if err != nil {
				return err
			}
			store, err := slskd.OpenJobStore(root)
			if err != nil {
				return err
			}
			jobs, err := store.List()
			if err != nil {
				return err
			}
			if flagSlskdJSON {
				return printJSON(jobs)
			}
			if len(jobs) == 0 {
				fmt.Println("no jobs")
				return nil
			}
			for _, j := range jobs {
				fmt.Println(formatJob(j))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&flagSlskdJSON, "json", false, "print JSON")
	return cmd
}

func formatJob(j slskd.Job) string {
	done, total := 0, len(j.Items)
	var bytes int64
	for _, it := range j.Items {
		if it.Sha256 != "" {
			done++
		}
		bytes += it.Size
	}
	line := fmt.Sprintf("%s  %-18s %-20s %d/%d verified  %s  %s",
		j.ID, j.Status, j.Peer, done, total, humanBytes(bytes), j.CreatedAt.Format("2006-01-02 15:04"))
	if j.FailureReason != "" {
		line += "\n    reason: " + j.FailureReason
	}
	return line
}

func soulseekIngestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ingest",
		Short: "Import completed downloads into provenance captures",
		Long: `Import every job whose bytes are complete but not yet ingested.

A job becomes IMPORTED only after each file was hashed and a capture manifest
was written; a failure marks it IMPORT_FAILED and leaves the staged bytes
untouched. --cleanup deletes the staging copy only after a verified import.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, settings, err := slskdClient(cmd.Context())
			if err != nil {
				return err
			}
			root, err := slskdStateRoot(settings)
			if err != nil {
				return err
			}
			store, err := slskd.OpenJobStore(root)
			if err != nil {
				return err
			}
			ing := &slskd.Ingestor{
				SlskdDownloadsRoot: settings.SlskdDownloadsRoot,
				Mount:              settings.ProvenanceDownloadsMount,
				VaultRoot:          flagSlskdVault,
				Collection:         flagSlskdCollection,
				Cleanup:            flagSlskdCleanup,
			}
			ing.AttachStore(store)
			jobs, err := ing.IngestPending(cmd.Context(), store)
			for _, j := range jobs {
				fmt.Println(formatJob(j))
			}
			return err
		},
	}
	cmd.Flags().StringVar(&flagSlskdVault, "vault", "", "also ingest each capture into this vault root")
	cmd.Flags().StringVar(&flagSlskdCollection, "collection", "soulseek", "vault collection name")
	cmd.Flags().BoolVar(&flagSlskdCleanup, "cleanup", false, "delete the slskd staging copy after a verified import")
	return cmd
}

func soulseekWatchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Reconcile transfers and import completions until interrupted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, settings, err := slskdClient(cmd.Context())
			if err != nil {
				return err
			}
			root, err := slskdStateRoot(settings)
			if err != nil {
				return err
			}
			store, err := slskd.OpenJobStore(root)
			if err != nil {
				return err
			}
			reconciler := slskd.NewReconciler(client, store)
			ing := &slskd.Ingestor{
				SlskdDownloadsRoot: settings.SlskdDownloadsRoot,
				Mount:              settings.ProvenanceDownloadsMount,
				VaultRoot:          flagSlskdVault,
				Collection:         flagSlskdCollection,
				Cleanup:            settings.PostImportCleanup,
				Client:             client,
			}
			ing.AttachStore(store)

			interval := flagSlskdInterval
			if interval <= 0 {
				interval = 5 * time.Second
			}
			for {
				jobs, err := reconciler.Poll(cmd.Context())
				if err != nil {
					if cmd.Context().Err() != nil {
						return nil
					}
					fmt.Fprintf(os.Stderr, "poll: %v\n", err)
				}
				for _, j := range jobs {
					fmt.Println(formatJob(j))
				}
				imported, err := ing.IngestPending(cmd.Context(), store)
				for _, j := range imported {
					fmt.Println(formatJob(j))
				}
				if err != nil {
					fmt.Fprintf(os.Stderr, "ingest: %v\n", err)
				}
				if flagSlskdOnce {
					return nil
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(interval):
				}
			}
		},
	}
	cmd.Flags().DurationVar(&flagSlskdInterval, "interval", 5*time.Second, "poll interval")
	cmd.Flags().BoolVar(&flagSlskdOnce, "once", false, "run one reconcile pass and exit")
	cmd.Flags().StringVar(&flagSlskdVault, "vault", "", "also ingest each capture into this vault root")
	cmd.Flags().StringVar(&flagSlskdCollection, "collection", "soulseek", "vault collection name")
	return cmd
}

// selectGroups picks the folders to enqueue for one peer and returns the rest as
// ranked alternates for "try another source".
func selectGroups(groups []slskd.ResultGroup, peer, dir string, files []string) ([]slskd.ResultGroup, []slskd.Candidate, error) {
	var selected []slskd.ResultGroup
	var others []slskd.Candidate
	wantFiles := map[string]bool{}
	for _, f := range files {
		wantFiles[strings.ToLower(strings.ReplaceAll(f, "/", `\`))] = true
	}
	for _, g := range groups {
		if !strings.EqualFold(g.Username, peer) {
			others = append(others, slskd.Candidate{
				Username: g.Username, Directory: g.Directory, Files: g.Files,
				UploadSpeed: g.UploadSpeed, HasFreeUploadSlot: g.HasFreeUploadSlot,
				QueueLength: g.QueueLength, Score: g.Score,
			})
			continue
		}
		if dir != "" && !strings.EqualFold(strings.ReplaceAll(g.Directory, "\\", "/"), strings.ReplaceAll(dir, "\\", "/")) {
			continue
		}
		if len(wantFiles) > 0 {
			filtered := g.Files[:0:0]
			for _, f := range g.Files {
				key := strings.ToLower(strings.ReplaceAll(f.Filename, "/", `\`))
				if wantFiles[key] || wantFiles[strings.ToLower(filepath.Base(strings.ReplaceAll(f.Filename, "\\", "/")))] {
					filtered = append(filtered, f)
				}
			}
			if len(filtered) == 0 {
				continue
			}
			g.Files = filtered
		}
		selected = append(selected, g)
	}
	if len(selected) == 0 {
		var dirs []string
		for _, g := range groups {
			if strings.EqualFold(g.Username, peer) {
				dirs = append(dirs, g.Directory)
			}
		}
		if len(dirs) == 0 {
			return nil, nil, fmt.Errorf("peer %q offered nothing matching this search", peer)
		}
		sort.Strings(dirs)
		return nil, nil, fmt.Errorf("peer %q offers several folders; pick one with --dir:\n  %s", peer, strings.Join(dirs, "\n  "))
	}
	return selected, others, nil
}

// slskdStateRoot resolves where durable job records live: the mapped download
// mount, so job state travels with the bytes it describes.
func slskdStateRoot(s slskd.Settings) (string, error) {
	if strings.TrimSpace(flagSlskdStateDir) != "" {
		return flagSlskdStateDir, nil
	}
	if strings.TrimSpace(s.ProvenanceDownloadsMount) == "" {
		return "", errors.New("no download mount is configured; run `provenance soulseek settings set --downloads-mount <path>`")
	}
	return s.ProvenanceDownloadsMount, nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return "..." + s[len(s)-(n-3):]
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func soulseekCancelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cancel <jobID>",
		Short: "Cancel a job's transfers and mark it cancelled",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, settings, err := slskdClient(cmd.Context())
			if err != nil {
				return err
			}
			root, err := slskdStateRoot(settings)
			if err != nil {
				return err
			}
			store, err := slskd.OpenJobStore(root)
			if err != nil {
				return err
			}
			job, err := slskd.NewReconciler(client, store).Cancel(cmd.Context(), args[0], flagSlskdRemove)
			if err != nil {
				return err
			}
			fmt.Println(formatJob(job))
			return nil
		},
	}
	cmd.Flags().BoolVar(&flagSlskdRemove, "remove", false, "also remove the transfer record in slskd")
	return cmd
}

func soulseekRetryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retry <jobID>",
		Short: "Re-enqueue a failed, cancelled or interrupted job as a new job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, settings, err := slskdClient(cmd.Context())
			if err != nil {
				return err
			}
			root, err := slskdStateRoot(settings)
			if err != nil {
				return err
			}
			store, err := slskd.OpenJobStore(root)
			if err != nil {
				return err
			}
			out, err := slskd.NewReconciler(client, store).Retry(cmd.Context(), args[0], flagSlskdAnother)
			if err != nil {
				return err
			}
			if out.Duplicate {
				fmt.Printf("already queued as %s\n", out.Job.ID)
				return nil
			}
			fmt.Printf("queued %s: %d file(s) from %s\n", out.Job.ID, len(out.Job.Items), out.Job.Peer)
			return nil
		},
	}
	cmd.Flags().BoolVar(&flagSlskdAnother, "another-source", false, "use the next ranked alternate peer instead of retrying the same one")
	return cmd
}

func soulseekPositionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "position <jobID>",
		Short: "Ask each peer for this job's place in its queue (rate-limited)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, settings, err := slskdClient(cmd.Context())
			if err != nil {
				return err
			}
			root, err := slskdStateRoot(settings)
			if err != nil {
				return err
			}
			store, err := slskd.OpenJobStore(root)
			if err != nil {
				return err
			}
			pos, err := slskd.NewReconciler(client, store).QueuePosition(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if flagSlskdJSON {
				return printJSON(pos)
			}
			for name, place := range pos {
				fmt.Printf("%-60s position %d\n", name, place)
			}
			return nil
		},
	}
}

func soulseekClearCompletedCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clear-completed",
		Short: "Remove completed download records from slskd (job history is kept)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, settings, err := slskdClient(cmd.Context())
			if err != nil {
				return err
			}
			root, err := slskdStateRoot(settings)
			if err != nil {
				return err
			}
			store, err := slskd.OpenJobStore(root)
			if err != nil {
				return err
			}
			if err := slskd.NewReconciler(client, store).ClearCompleted(cmd.Context()); err != nil {
				return err
			}
			fmt.Println("cleared completed transfers in slskd; provenance job records were not touched")
			return nil
		},
	}
}

func soulseekRecoverCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "recover",
		Short: "Reconcile durable jobs against slskd and the filesystem",
		Long: `Cross-check every pending job against slskd's transfers and the bytes on
disk. A job whose transfer records expired but whose files arrived becomes
ready to ingest; a job that exists in neither place becomes INTERRUPTED and
can be re-enqueued with ` + "`provenance soulseek retry`" + `.

Nothing is changed when slskd cannot be reached: a network failure is not
evidence that a download is gone.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, settings, err := slskdClient(cmd.Context())
			if err != nil {
				return err
			}
			root, err := slskdStateRoot(settings)
			if err != nil {
				return err
			}
			store, err := slskd.OpenJobStore(root)
			if err != nil {
				return err
			}
			rep, err := (&slskd.Recovery{Client: client, Store: store, Mount: settings.ProvenanceDownloadsMount}).Run(cmd.Context())
			for _, line := range rep.Summary() {
				fmt.Println(line)
			}
			return err
		},
	}
}

func soulseekWebhookListenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "webhook-listen",
		Short: "Run the optional webhook receiver that wakes the reconciler early",
		Long: `Serve the slskd webhook endpoint and reconcile on delivery.

Webhooks are a latency optimisation only: polling stays authoritative, because
slskd emits no search, progress or failure webhooks and drops them entirely on a
volatile restart. The endpoint refuses every delivery unless a shared secret is
configured.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, settings, err := slskdClient(cmd.Context())
			if err != nil {
				return err
			}
			secret := settings.WebhookSecret
			if strings.TrimSpace(secret) == "" {
				return errors.New("no webhook secret is configured; run `provenance soulseek settings set --webhook-secret <value>`")
			}
			root, err := slskdStateRoot(settings)
			if err != nil {
				return err
			}
			store, err := slskd.OpenJobStore(root)
			if err != nil {
				return err
			}
			reconciler := slskd.NewReconciler(client, store)
			ing := &slskd.Ingestor{
				SlskdDownloadsRoot: settings.SlskdDownloadsRoot,
				Mount:              settings.ProvenanceDownloadsMount,
				VaultRoot:          flagSlskdVault,
				Collection:         flagSlskdCollection,
				Cleanup:            settings.PostImportCleanup,
				Client:             client,
			}
			ing.AttachStore(store)

			wake := make(chan string, 1)
			handler := &slskd.WebhookHandler{
				Secret: secret,
				Wake: func(reason string) {
					select {
					case wake <- reason:
					default:
					}
				},
			}
			mux := http.NewServeMux()
			mux.Handle("/api/integrations/slskd/webhook", handler)

			ctx := cmd.Context()
			go func() {
				// Reconcile immediately at start, then whenever woken, plus a slow
				// safety tick so a lost webhook cannot stall a download forever.
				runPass := func() {
					if _, err := reconciler.Poll(ctx); err != nil && ctx.Err() == nil {
						fmt.Fprintf(os.Stderr, "poll: %v\n", err)
					}
					if _, err := ing.IngestPending(ctx, store); err != nil && ctx.Err() == nil {
						fmt.Fprintf(os.Stderr, "ingest: %v\n", err)
					}
				}
				runPass()
				tick := time.NewTicker(60 * time.Second)
				defer tick.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-wake:
						runPass()
					case <-tick.C:
						runPass()
					}
				}
			}()

			srv := &http.Server{
				Addr:              flagSlskdAddr,
				Handler:           mux,
				ReadHeaderTimeout: 5 * time.Second,
				ReadTimeout:       10 * time.Second,
				WriteTimeout:      10 * time.Second,
			}
			fmt.Printf("webhook receiver listening on %s\n", flagSlskdAddr)
			fmt.Println("paste this into slskd.yml (substitute the secret from your settings file):")
			fmt.Print(slskd.WebhookConfigSnippet("http://<provenance-host>"+flagSlskdAddr+"/api/integrations/slskd/webhook", "<webhook-secret>"))
			errCh := make(chan error, 1)
			go func() { errCh <- srv.ListenAndServe() }()
			select {
			case <-ctx.Done():
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				return srv.Shutdown(shutdownCtx)
			case err := <-errCh:
				if errors.Is(err, http.ErrServerClosed) {
					return nil
				}
				return err
			}
		},
	}
	cmd.Flags().StringVar(&flagSlskdAddr, "addr", ":8791", "listen address for the webhook receiver")
	cmd.Flags().StringVar(&flagSlskdVault, "vault", "", "also ingest each capture into this vault root")
	cmd.Flags().StringVar(&flagSlskdCollection, "collection", "soulseek", "vault collection name")
	return cmd
}
