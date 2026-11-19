package tui

// BT-10b integration: the view against the real local snapshot endpoint.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sk3y04/provenance-engine/internal/torrentqueue"
	"github.com/sk3y04/provenance-engine/internal/transfer/bittorrent"
)

// drainCmd runs a command and flattens a batch into its messages.
func drainCmd(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if msg == nil {
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, drainCmd(t, c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

type viewSession struct{}

func (viewSession) Serve(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
func (viewSession) Address() string                 { return "127.0.0.1:1" }
func (viewSession) UploadedBytes() int64            { return 8192 }
func (viewSession) Close() error                    { return nil }

type viewOpener struct{ torrentqueue.Adapter }

func (o viewOpener) OpenSeed(torrentqueue.Entry, torrentqueue.Request) (torrentqueue.SeedSession, error) {
	return viewSession{}, nil
}

func TestQueueViewAgainstRealEndpoint(t *testing.T) {
	root := t.TempDir()
	store, err := torrentqueue.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	entry, _, aerr := store.Add(torrentqueue.AddRequest{
		Source:    torrentqueue.Source{Kind: "magnet", Magnet: "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20)},
		RawSource: "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20),
		Discovery: "direct",
		Selection: []string{"a.bin"},
		JobID:     "bt-" + strings.Repeat("ab", 20),
		Workspace: ".provenance/torrent/jobs/bt-" + strings.Repeat("ab", 20),
	})
	if aerr != nil {
		t.Fatalf("add: %v", aerr)
	}
	if _, uerr := store.Update(entry.ID, func(e *torrentqueue.Entry) error {
		e.Phase = torrentqueue.PhaseCompleted
		e.Capture = &torrentqueue.CaptureRef{Key: "capture-fixture"}
		e.SeedDesired = torrentqueue.SeedOn
		e.SeedPhase = torrentqueue.SeedWaiting
		return nil
	}); uerr != nil {
		t.Fatalf("make completed: %v", uerr)
	}

	// A real runner publishes the real endpoint; the opener avoids a second client.
	ctx, cancel := context.WithCancel(context.Background())
	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_, _ = torrentqueue.NewRunner(store, viewOpener{Adapter: torrentqueue.Adapter{}},
			torrentqueue.Options{MaxActive: 1, MaxSeeds: 1, PollInterval: 20 * time.Millisecond, Log: os.Stderr}).Run(ctx)
	}()
	defer func() { cancel(); <-runnerDone }()

	m := &model{view: viewTorrentQueue, width: 200, height: 40}
	m.queue = queueState{root: root, liveStatus: "live status: not polled yet"}

	deadline := time.Now().Add(60 * time.Second)
	var liveSeen bool
	for time.Now().Before(deadline) {
		for _, msg := range drainCmd(t, m.refreshQueue()) {
			_, _ = m.updateQueue(msg)
		}
		if out := m.viewTorrentQueue(); strings.Contains(out, "live=seed serving") {
			liveSeen = true
			if !strings.Contains(out, entry.ID) || !strings.Contains(out, "recorded=completed") {
				t.Fatalf("live view lost the durable row:\n%s", out)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !liveSeen {
		t.Fatalf("the view never showed the live seed; last render:\n%s", m.viewTorrentQueue())
	}

	// Stop the runner: the same durable entries must remain, with no live overlay.
	cancel()
	<-runnerDone
	for _, msg := range drainCmd(t, m.refreshQueue()) {
		_, _ = m.updateQueue(msg)
	}
	out := m.viewTorrentQueue()
	if strings.Contains(out, "live=seed") || strings.Contains(out, "uploaded=") {
		t.Fatalf("a dead runner's snapshot is still shown as live:\n%s", out)
	}
	if !strings.Contains(out, entry.ID) || !strings.Contains(out, "unavailable") {
		t.Fatalf("the queue was not browsable after the runner exited:\n%s", out)
	}

	// The view must not have mutated anything.
	after, gerr := store.Get(entry.ID)
	if gerr != nil {
		t.Fatalf("entry disappeared: %v", gerr)
	}
	if after.Phase != torrentqueue.PhaseCompleted || after.SeedDesired != torrentqueue.SeedOn {
		t.Fatalf("the read-only view changed the entry: %+v", after)
	}
	_ = bittorrent.DiscoveryDirect
}
