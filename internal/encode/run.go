package encode

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sk3y04/provenance-engine/internal/event"
	"github.com/sk3y04/provenance-engine/internal/worker"
)

// Options holds every flag for `provenance encode`, populated directly from
// cobra flags in cmd/provenance/main.go.
//
// These live in a dedicated struct rather than on config.Config on purpose:
// config.Config is the serializable download configuration shared by the
// dispatcher/session/watch machinery (and embedded in capture manifests),
// whereas encode is a self-contained, batch-oriented feature. This mirrors how
// collection uses a dedicated SyncOptions struct and how archive carries its
// persistent flags separately from the generic download Config — the encoder
// has no download/session semantics to preserve, so it gets its own option bag.
type Options struct {
	Dir       string   // source directory (ignored when File is set)
	File      string   // single input file
	Recursive bool     // recurse into subdirectories
	Ext       []string // input extensions to match (no leading dot)
	OutDir    string   // empty -> the source directory itself (in place)
	Suffix    string   // inserted before .mkv (empty -> av1-qN)
	Overwrite bool     // re-encode even if the output already exists
	// RemoveOriginal deletes each input file after its encode succeeds.
	// Removal never happens in dry-run mode or when the encode fails.
	RemoveOriginal bool
	Encoder        string   // qsv|amf|nvenc|auto
	Devices        []string // render-node paths (QSV/AMF) or GPU indices (NVENC)
	Quality        int      // canonical QSV global_quality scale 1..51
	Preset         string   // speed/efficiency hint, mapped per backend
	GOPSeconds     float64  // keyframe interval in seconds
	Lookahead      int      // look-ahead frames; QSV max 100
	BFrames        int      // B-frames per GOP
	DryRun         bool     // print commands without encoding
	// Events, when set, receives structured progress/summary events and the
	// operation narrates to no terminal. The CLI leaves it nil today (legacy
	// narration); a TUI or worker transport supplies one.
	Events event.Sink
}

// Result summarizes a completed (or dry-run) batch.
type Result struct {
	Total     int
	Skipped   int
	Succeeded int
	Failed    int
	OutDir    string
	Encoder   string
	Devices   int
}

// OK reports whether the batch produced no failures and touched at least one
// file. A run with zero discovered files is not OK.
func (r Result) OK() bool { return r.Failed == 0 && r.Total > 0 }

// Run discovers inputs, resolves the encoder, and fans the batch out over the
// requested devices using the worker pool. It returns a non-nil error (so the
// CLI exits non-zero) when preflight fails or any encode fails.
func Run(ctx context.Context, opts Options, runner CommandRunner) (Result, error) {
	if runner == nil {
		runner = NewRunner()
	}
	// Carry the sink on the context so every narration helper in the batch
	// (including per-job hooks and the encoder dry-run echo) resolves it
	// without threading a parameter through every signature.
	if opts.Events != nil {
		ctx = event.WithSink(ctx, opts.Events)
	}

	exts := extSet(opts.Ext)

	// Determine the source directory (dir, or the directory of --file).
	sourceDir := opts.Dir
	if sourceDir == "" && opts.File != "" {
		sourceDir = filepath.Dir(opts.File)
	}

	quality := opts.Quality
	if quality == 0 {
		quality = 30
	}
	gopSeconds := opts.GOPSeconds
	if gopSeconds <= 0 {
		gopSeconds = 10
	}

	outDir := opts.OutDir
	if strings.TrimSpace(outDir) == "" {
		outDir = sourceDir
	}
	suffix := opts.Suffix
	if strings.TrimSpace(suffix) == "" {
		suffix = fmt.Sprintf("av1-q%d", quality)
	}
	suffix = strings.TrimSpace(suffix)

	// Look-ahead defaults to 100 (QSV's max) when unset, matching the CLI flag
	// default; <=0 disables the flag inside av1Args.
	lookahead := opts.Lookahead
	if lookahead <= 0 {
		lookahead = 100
	}

	// Preflight: ffmpeg + ffprobe must exist (EnsureInstalled has already run
	// best-effort in main's PersistentPreRunE; resolveBinaries just confirms).
	ffmpeg, ffprobe, err := resolveBinaries()
	if err != nil {
		return Result{}, err
	}

	enc, err := ResolveEncoder(ctx, opts.Encoder, ffmpeg, runner)
	if err != nil {
		return Result{}, err
	}

	// Devices default to the vendor-matched render nodes detected on this
	// machine (Intel for QSV, AMD for AMF, GPU index 0 for NVENC). An empty
	// detection result pins nothing and lets the encoder library choose.
	devices := opts.Devices
	if len(devices) == 0 {
		devices = probeDevices(enc.Name())
	}
	if len(devices) == 0 {
		devices = []string{""}
	}

	// Discover inputs.
	var inputs []string
	if opts.File != "" {
		abs, err := filepath.Abs(opts.File)
		if err != nil {
			return Result{}, fmt.Errorf("resolve --file: %w", err)
		}
		inputs = []string{abs}
	} else {
		abs, err := filepath.Abs(sourceDir)
		if err != nil {
			return Result{}, fmt.Errorf("resolve --dir: %w", err)
		}
		outDirAbs, err := filepath.Abs(outDir)
		if err != nil {
			return Result{}, fmt.Errorf("resolve --out: %w", err)
		}
		inputs, err = collectFiles(abs, opts.Recursive, exts, outDirAbs, suffix)
		if err != nil {
			return Result{}, fmt.Errorf("scan %s: %w", abs, err)
		}
	}
	if len(inputs) == 0 {
		return Result{}, fmt.Errorf("no matching video files found in %s (extensions: %s)", sourceDir, strings.Join(defaultExts(), ","))
	}

	if !opts.DryRun {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return Result{}, fmt.Errorf("create output dir: %w", err)
		}
	}

	// Build jobs, skipping outputs that already exist unless --overwrite.
	reporter := newReporter(ctx)
	reporter.total = len(inputs)

	for i, in := range inputs {
		base := strings.TrimSuffix(filepath.Base(in), filepath.Ext(in))
		outPath := filepath.Join(outDir, fmt.Sprintf("%s.%s.mkv", base, suffix))

		if !opts.Overwrite {
			if _, statErr := os.Stat(outPath); statErr == nil {
				reporter.incSkipped()
				continue
			}
		}

		fps, _ := ffmpegFPS(ctx, ffprobe, in, runner)
		job := EncodeJob{
			Input:     in,
			Output:    outPath,
			FPS:       fps,
			GOPInt:    GOPFrames(fps, gopSeconds),
			Device:    devices[i%len(devices)],
			Quality:   quality,
			Preset:    opts.Preset,
			Lookahead: clampInt(lookahead, 0, 100),
			BFrames:   opts.BFrames,
		}
		reporter.jobs = append(reporter.jobs, job)
	}

	reporter.printHeader(len(reporter.jobs), enc.Name(), quality, opts.Preset, gopSeconds, suffix, devices, opts.DryRun)

	// Fan out: one worker pool per device, each pulling its assigned jobs. This
	// yields exactly one worker per device, reusing the shared worker.Pool.
	pools := make([]*worker.Pool, len(devices))
	for i := range pools {
		pools[i] = worker.NewPool(ctx, 1)
	}

	for i, job := range reporter.jobs {
		job := job
		p := pools[i%len(devices)]
		p.SubmitWithHooks(
			func() error {
				if runErr := runOne(ctx, ffmpeg, runner, enc, job, opts.DryRun); runErr != nil {
					// Encode is not retryable: mark permanent so the pool does
					// not re-run (which could overwrite in progress output).
					return worker.Permanent(runErr)
				}
				return nil
			},
			func() {
				if opts.RemoveOriginal && !opts.DryRun {
					if rmErr := os.Remove(job.Input); rmErr != nil {
						narrate(reporter.ctx, os.Stderr, kindEncodeWarn, event.ReasonInfo, filepath.Base(job.Input), "", rmErr,
							"[provenance] warning: could not remove original %s: %v", job.Input, rmErr)
					}
				}
				reporter.incSucceeded()
				reporter.printDone(devices[i%len(devices)], job, reporter.completed())
			},
			func(runErr error) {
				reporter.fail(job, runErr)
			},
		)
	}
	for _, p := range pools {
		p.Wait()
	}

	res := Result{
		Total:     reporter.total,
		Skipped:   int(reporter.skipped),
		Succeeded: int(reporter.succeeded),
		Failed:    int(reporter.failed),
		OutDir:    outDir,
		Encoder:   enc.Name(),
		Devices:   len(devices),
	}
	reporter.printSummary(res, time.Since(reporter.start))

	if res.Failed > 0 {
		return res, fmt.Errorf("%d encode(s) failed", res.Failed)
	}
	return res, nil
}

// runOne encodes a single job. ffmpeg stderr is discarded: no per-file log
// files are written.
func runOne(ctx context.Context, ffmpeg string, runner CommandRunner, enc Encoder, job EncodeJob, dryRun bool) error {
	if dryRun {
		// Dry-run prints the constructed command and never touches the
		// filesystem, so there is no log to open.
		return enc.Run(ctx, ffmpeg, runner, io.Discard, job, true)
	}
	if err := enc.Run(ctx, ffmpeg, runner, io.Discard, job, false); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	// ffmpeg can exit 0 without a usable output. Never report success — and
	// never let --remove-original delete the source — on a missing or empty
	// file.
	fi, err := os.Stat(job.Output)
	if err != nil {
		return fmt.Errorf("output missing after encode: %w", err)
	}
	if fi.Size() == 0 {
		return fmt.Errorf("output is empty after encode: %s", job.Output)
	}
	return nil
}

// extSet joins the configured extensions into a lookup set.
func extSet(exts []string) map[string]bool {
	if len(exts) == 0 {
		return parseExts(strings.Join(defaultExts(), ","))
	}
	return parseExts(strings.Join(exts, ","))
}

func defaultExts() []string {
	return []string{"mp4", "mkv", "mov", "avi", "ts", "webm"}
}

// reporter counts encode outcomes and narrates the [provenance] lines. It is
// safe for concurrent use because pool hooks run in worker goroutines;
// terminal writes are serialized by narrateMu in emit.go. ctx carries the
// operation's event sink (set in Run), which decides whether each line becomes
// a structured event or legacy terminal text.
type reporter struct {
	mu        sync.Mutex
	ctx       context.Context
	start     time.Time
	total     int
	jobs      []EncodeJob
	succeeded int64
	failed    int64
	skipped   int64
	firstErr  error
}

func newReporter(ctx context.Context) *reporter {
	return &reporter{ctx: ctx, start: time.Now()}
}

func (r *reporter) incSucceeded() { atomic.AddInt64(&r.succeeded, 1) }
func (r *reporter) incFailed()    { atomic.AddInt64(&r.failed, 1) }
func (r *reporter) incSkipped()   { atomic.AddInt64(&r.skipped, 1) }

func (r *reporter) completed() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return int(atomic.LoadInt64(&r.succeeded) + atomic.LoadInt64(&r.failed))
}

// fail records the first failure (guarded) and narrates a per-file error line.
func (r *reporter) fail(job EncodeJob, runErr error) {
	r.incFailed()
	r.mu.Lock()
	if r.firstErr == nil {
		r.firstErr = fmt.Errorf("encode %s: %w", filepath.Base(job.Input), runErr)
	}
	r.mu.Unlock()

	narrate(r.ctx, os.Stderr, kindEncodeItem, event.ReasonItemFailed, filepath.Base(job.Input), "", runErr,
		"[provenance] %s: FAILED %s -> %v", job.Device, filepath.Base(job.Input), runErr)
}

func (r *reporter) printHeader(count int, codec string, quality int, preset string, gopSeconds float64, suffix string, devices []string, dryRun bool) {
	narrate(r.ctx, os.Stderr, kindEncodeStart, event.ReasonStart, "", "", nil,
		"[provenance] encode: %d input(s), %d device worker(s): %s",
		count, len(devices), strings.Join(devices, ", "))
	narrate(r.ctx, os.Stderr, kindEncodeInfo, event.ReasonInfo, "", "", nil,
		"[provenance] encoder: %s | quality=%d | preset=%q | suffix=%q | gop=%gs | dry-run=%v",
		codec, quality, preset, suffix, gopSeconds, dryRun)
}

func (r *reporter) printDone(device string, job EncodeJob, done int) {
	narrate(r.ctx, os.Stderr, kindEncodeItem, event.ReasonItemOK, filepath.Base(job.Input), "", nil,
		"[provenance] %s: done %d/%d  %s (fps=%.2f gop=%d)",
		device, done, r.total, filepath.Base(job.Output), job.FPS, job.GOPInt)
}

func (r *reporter) printSummary(res Result, dur time.Duration) {
	narrateSummary(r.ctx, res.Total, res.Succeeded, res.Skipped, res.Failed, res.OutDir, dur.Round(time.Second).String())
}
