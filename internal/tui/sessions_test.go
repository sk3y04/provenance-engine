package tui

import (
	"context"
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sk3y04/provenance/internal/config"
	"github.com/sk3y04/provenance/internal/ratelimit"
	"github.com/sk3y04/provenance/internal/session"
)

// TestFKeyFromSessionsList ensures pressing "f" in the sessions list opens
// the session detail view (which surfaces the failed URLs) instead of
// staying on the list.
func TestFKeyFromSessionsList(t *testing.T) {
	t.Setenv("PROVENANCE_SESSION_DIR", t.TempDir())
	t.Setenv("PROVENANCE_WATCH_FILE", t.TempDir()+"/watch.json")
	t.Setenv("PROVENANCE_HISTORY_FILE", t.TempDir()+"/history.json")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	s, err := session.OpenOrCreate("demo", config.Config{OutputDir: "./downloads"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddURLs([]string{"https://example.com/x"}, "test"); err != nil {
		t.Fatal(err)
	}
	s.Failure("https://example.com/x", errors.New("boom"))

	m := newModel(context.Background(), ratelimit.New())
	m.view = viewSessions
	infos, err := session.List()
	if err != nil {
		t.Fatal(err)
	}
	m.sessions = infos
	m.sessCursor = 0

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	if cmd == nil {
		t.Fatalf("no cmd returned for f key")
	}
	m.Update(cmd())
	if m.view != viewSessionDetail {
		t.Fatalf("view = %d, want viewSessionDetail (%d)", m.view, viewSessionDetail)
	}
	if m.sessSelected == nil {
		t.Fatal("sessSelected is nil after f key")
	}
}
