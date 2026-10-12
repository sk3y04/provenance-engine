package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/gen2brain/beeep"
	"github.com/sk3y04/provenance-engine/internal/dispatcher"
	"github.com/sk3y04/provenance-engine/internal/event"
	"github.com/sk3y04/provenance-engine/internal/history"
	"github.com/sk3y04/provenance-engine/internal/render"
	"github.com/sk3y04/provenance-engine/internal/session"
)

// ---------------------------------------------------------------------------
// Runner: live download view
// ---------------------------------------------------------------------------

func (m *model) updateRunner(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace", "h", "q":
		if m.runner.done {
			m.view = viewMain
			m.runner = runnerState{}
			return m, m.loadSessionsCmd()
		}
	}
	return m, nil
}

// startRunner sets up the runner view and kicks off the download in a
// goroutine that streams events back to the TUI.
func (m *model) startRunner(title string, urls []string, opts dispatcher.Options, sess *session.Session) tea.Cmd {
	ctx, cancel := context.WithCancel(m.ctx)
	m.view = viewRunner
	bar := progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage())
	bar.Width = 30
	m.runner = runnerState{
		title:        title,
		urls:         append([]string(nil), urls...),
		options:      opts.Config,
		startAt:      time.Now(),
		cancel:       cancel,
		maxLines:     200,
		files:        map[string]*fileProgress{},
		bar:          bar,
		lastSampleAt: time.Now(),
		watchName:    extractWatchName(title),
	}

	prog := m.program
	rep := &teaReporter{prog: prog, sess: sess}
	opts.Reporter = rep
	opts.FileProgress = &fileReporter{prog: prog}

	// The TUI is an event consumer. Reusable paths never write to a terminal
	// when a sink is carried on the operation context, so no process-global
	// stream redirection is needed (and none is safe in a shared process).
	sink := newTeaSink(prog)
	opts.Events = sink
	ctx = event.WithSink(ctx, sink)

	go func() {
		defer cancel()
		var firstErr error
		for _, u := range urls {
			if err := dispatcher.Dispatch(ctx, u, opts); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		prog.Send(runnerDoneMsg{err: firstErr})
	}()

	return tickEvery(time.Second)
}

// sampleThroughput updates speedBps based on bytes written across all
// in-flight + recently completed files since the previous tick.
// It also computes per-file speeds stored in fileProgress.fileSpeedBps.
func (m *model) sampleThroughput() {
	var total int64
	for _, fp := range m.runner.files {
		total += fp.written
	}
	now := time.Now()
	dt := now.Sub(m.runner.lastSampleAt).Seconds()
	if dt <= 0 {
		return
	}
	delta := total - m.runner.lastSampleBytes
	if delta < 0 {
		delta = 0
	}
	// Smooth with a simple EMA so the number doesn't jitter wildly.
	instant := float64(delta) / dt
	if m.runner.speedBps == 0 {
		m.runner.speedBps = instant
	} else {
		m.runner.speedBps = 0.6*m.runner.speedBps + 0.4*instant
	}
	m.runner.lastSampleAt = now
	m.runner.lastSampleBytes = total

	// Per-file speed calculation.
	for _, fp := range m.runner.files {
		if fp == nil || fp.done {
			continue
		}
		curBytes := fp.written
		var fileDelta int64
		if fp.sampleBytes > 0 {
			fileDelta = curBytes - fp.sampleBytes
			if fileDelta < 0 {
				fileDelta = 0
			}
		}
		fp.sampleBytes = curBytes
		if fileDelta == 0 {
			// First sample for this file, or no progress since last tick.
			// Decay the speed so a stalled file doesn't keep showing a stale
			// throughput forever.
			if fp.fileSpeedBps > 0 {
				fp.fileSpeedBps *= 0.5
			}
			continue
		}
		fileInstant := float64(fileDelta) / dt
		if fp.fileSpeedBps == 0 {
			fp.fileSpeedBps = fileInstant
		} else {
			fp.fileSpeedBps = 0.6*fp.fileSpeedBps + 0.4*fileInstant
		}
	}
}

// pruneFinishedFiles drops finished file rows older than ~3s once we have
// more than `keep` rows total, so the live list stays bounded during big runs.
func (m *model) pruneFinishedFiles(keep int) {
	if len(m.runner.fileOrder) <= keep {
		return
	}
	cutoff := time.Now().Add(-3 * time.Second)
	kept := make([]string, 0, len(m.runner.fileOrder))
	for _, u := range m.runner.fileOrder {
		fp, ok := m.runner.files[u]
		if !ok {
			continue
		}
		if fp.done && !fp.doneAt.IsZero() && fp.doneAt.Before(cutoff) {
			delete(m.runner.files, u)
			continue
		}
		kept = append(kept, u)
	}
	m.runner.fileOrder = kept
}

func (m *model) appendLog(line string) {
	if line == "" {
		return
	}
	if strings.Contains(line, "PROVENANCE_YTDLP_PROGRESS:") {
		return
	}
	m.runner.logs = append(m.runner.logs, line)
	maxLines := m.runner.maxLines
	if maxLines <= 0 {
		maxLines = 200
	}
	if len(m.runner.logs) > maxLines {
		m.runner.logs = m.runner.logs[len(m.runner.logs)-maxLines:]
	}
}

func (m *model) saveHistoryRun() error {
	if m.runner.title == "" || m.runner.startAt.IsZero() {
		return nil
	}
	files := make([]history.File, 0, len(m.runner.fileOrder))
	seen := map[string]struct{}{}
	for _, u := range m.runner.fileOrder {
		fp := m.runner.files[u]
		if fp == nil || fp.dest == "" {
			continue
		}
		key := fp.dest + "\x00" + fp.url
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		entry := history.File{
			URL:     fp.url,
			Path:    fp.dest,
			Size:    fp.written,
			Success: fp.err == nil,
		}
		if fp.err != nil {
			entry.Error = fp.err.Error()
		}
		files = append(files, entry)
	}
	var errText string
	if m.runner.err != nil {
		errText = m.runner.err.Error()
	}
	_, err := history.Add(history.Run{
		Title:       m.runner.title,
		URLs:        append([]string(nil), m.runner.urls...),
		Options:     m.runner.options,
		StartedAt:   m.runner.startAt,
		CompletedAt: time.Now(),
		Duration:    time.Since(m.runner.startAt),
		Succeeded:   m.runner.ok,
		Failed:      m.runner.failed,
		Skipped:     m.runner.skipped,
		TotalBytes:  historyBytes(files),
		Files:       files,
		Error:       errText,
	})
	return err
}

func historyBytes(files []history.File) int64 {
	var total int64
	for _, f := range files {
		if f.Success && f.Size > 0 {
			total += f.Size
		}
	}
	return total
}

// notifyComplete fires a desktop toast when a run finishes. Best-effort.
func notifyComplete(r runnerState) {
	title := "provenance - done"
	if r.err != nil {
		title = "provenance - failed"
	}
	body := fmt.Sprintf("%s\n%d ok, %d failed, %d skipped",
		r.title, r.ok, r.failed, r.skipped)
	_ = beeep.Notify(title, body, "")
}

// teaReporter implements dispatcher.Reporter. It forwards events to the TUI
// and, optionally, to a session for persistence.
type teaReporter struct {
	prog *tea.Program
	sess *session.Session
}

func (r *teaReporter) Queue(url, source string) {
	if r.sess != nil {
		r.sess.Queue(url, source)
	}
	r.prog.Send(runnerEventMsg{kind: "queue", url: url, note: source})
}
func (r *teaReporter) Start(url string) {
	if r.sess != nil {
		r.sess.Start(url)
	}
	r.prog.Send(runnerEventMsg{kind: "start", url: url})
}
func (r *teaReporter) Success(url string) {
	if r.sess != nil {
		r.sess.Success(url)
	}
	r.prog.Send(runnerEventMsg{kind: "ok", url: url})
}
func (r *teaReporter) Failure(url string, err error) {
	if r.sess != nil {
		r.sess.Failure(url, err)
	}
	note := ""
	if err != nil {
		note = err.Error()
	}
	r.prog.Send(runnerEventMsg{kind: "fail", url: url, note: note})
}
func (r *teaReporter) Skip(url, reason string) {
	if r.sess != nil {
		r.sess.Skip(url, reason)
	}
	r.prog.Send(runnerEventMsg{kind: "skip", url: url, note: reason})
}

// fileReporter implements downloader.ProgressReporter and forwards per-file
// byte-level updates to the TUI program.
type fileReporter struct {
	prog *tea.Program
}

func (r *fileReporter) OnStart(url, dest string, total int64) {
	r.prog.Send(fileStartMsg{url: url, dest: dest, total: total})
}
func (r *fileReporter) OnProgress(url string, written, total int64) {
	r.prog.Send(fileProgressMsg{url: url, written: written, total: total})
}
func (r *fileReporter) OnDone(url string, err error) {
	r.prog.Send(fileDoneMsg{url: url, err: err})
}

// teaSink implements event.Sink for the runner view. Rendered event lines are
// forwarded to the TUI as runnerLogMsg.
type teaSink struct {
	prog *tea.Program
	r    *render.Sink
}

func newTeaSink(prog *tea.Program) *teaSink {
	return &teaSink{prog: prog, r: render.NewSink(lineWriter{prog: prog})}
}

func (s *teaSink) Emit(ctx context.Context, e event.Event) { s.r.Emit(ctx, e) }

// lineWriter forwards each written line to the TUI as a runner log line.
type lineWriter struct{ prog *tea.Program }

func (w lineWriter) Write(p []byte) (int, error) {
	if w.prog == nil {
		return len(p), nil
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(p), "\n"), "\n") {
		line = stripANSI(line)
		if line == "" {
			continue
		}
		w.prog.Send(runnerLogMsg{line: line})
	}
	return len(p), nil
}

// extractWatchName parses the watch subscription name from a runner title.
// Titles from watches have the format: watch "name" -> URL
func extractWatchName(title string) string {
	const prefix = `watch "`
	if !strings.HasPrefix(title, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(title, prefix)
	if idx := strings.Index(rest, `"`); idx > 0 {
		return rest[:idx]
	}
	return ""
}

// stripANSI removes most ANSI escape codes so progress-bar output doesn't
// scramble the runner log. It's intentionally minimal.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) {
				if s[j] >= 0x40 && s[j] <= 0x7E {
					break
				}
				j++
			}
			i = j
			continue
		}
		if c == '\r' {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
