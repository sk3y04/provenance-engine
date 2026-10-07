package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/engine"
)

func TestNewRequiresWorkDir(t *testing.T) {
	_, err := engine.New(engine.Config{})
	if !engine.IsErrorKind(err, engine.ErrorValidation) {
		t.Fatalf("ErrorKindOf = %q, want validation (%v)", engine.ErrorKindOf(err), err)
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	eng, err := engine.New(engine.Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cfg := eng.Config()
	if cfg.Quality != engine.DefaultQuality {
		t.Errorf("Quality = %q, want %q", cfg.Quality, engine.DefaultQuality)
	}
	if cfg.Concurrency != engine.DefaultConcurrency {
		t.Errorf("Concurrency = %d, want %d", cfg.Concurrency, engine.DefaultConcurrency)
	}
	if cfg.MaxItems != engine.DefaultMaxItems {
		t.Errorf("MaxItems = %d, want %d", cfg.MaxItems, engine.DefaultMaxItems)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  engine.Config
	}{
		{"bad-quality", engine.Config{WorkDir: "x", Quality: "4k"}},
		{"negative-speed", engine.Config{WorkDir: "x", SpeedLimit: -1}},
		{"negative-timeout", engine.Config{WorkDir: "x", Timeout: -time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := engine.New(tc.cfg); !engine.IsErrorKind(err, engine.ErrorValidation) {
				t.Fatalf("ErrorKindOf = %q, want validation (%v)", engine.ErrorKindOf(err), err)
			}
		})
	}
}

func TestResolveValidatesInputs(t *testing.T) {
	eng, err := engine.New(engine.Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		req  engine.ResolveRequest
	}{
		{"empty-url", engine.ResolveRequest{}},
		{"bad-scheme", engine.ResolveRequest{URL: "ftp://example.com/x"}},
		{"no-host", engine.ResolveRequest{URL: "https:///path"}},
		{"invalid-regex", engine.ResolveRequest{
			URL:    "https://example.com/x",
			Filter: engine.FilterOptions{TitleMatch: "("},
		}},
		{"negative-size", engine.ResolveRequest{
			URL:    "https://example.com/x",
			Filter: engine.FilterOptions{MinSize: -1},
		}},
		{"min-gt-max", engine.ResolveRequest{
			URL:    "https://example.com/x",
			Filter: engine.FilterOptions{MinSize: 10, MaxSize: 5},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := eng.Resolve(context.Background(), tc.req, engine.NopSink())
			if !engine.IsErrorKind(err, engine.ErrorValidation) {
				t.Fatalf("ErrorKindOf = %q, want validation (%v)", engine.ErrorKindOf(err), err)
			}
		})
	}
}

func TestDownloadValidatesInputs(t *testing.T) {
	eng, err := engine.New(engine.Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = eng.Download(context.Background(), engine.DownloadRequest{}, engine.NopSink())
	if !engine.IsErrorKind(err, engine.ErrorValidation) {
		t.Fatalf("ErrorKindOf = %q, want validation (%v)", engine.ErrorKindOf(err), err)
	}
}

func TestCanceledContextReturnsCanceled(t *testing.T) {
	eng, err := engine.New(engine.Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := eng.Resolve(ctx, engine.ResolveRequest{URL: "https://example.com/x"}, engine.NopSink()); !engine.IsErrorKind(err, engine.ErrorCanceled) {
		t.Fatalf("Resolve ErrorKindOf = %q, want canceled (%v)", engine.ErrorKindOf(err), err)
	}
	if _, err := eng.Download(ctx, engine.DownloadRequest{URL: "https://example.com/x"}, engine.NopSink()); !engine.IsErrorKind(err, engine.ErrorCanceled) {
		t.Fatalf("Download ErrorKindOf = %q, want canceled (%v)", engine.ErrorKindOf(err), err)
	}
}

func TestTypedErrorAs(t *testing.T) {
	eng, err := engine.New(engine.Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, resolveErr := eng.Resolve(context.Background(), engine.ResolveRequest{}, engine.NopSink())
	var typed *engine.Error
	if !errors.As(resolveErr, &typed) {
		t.Fatalf("errors.As failed for %v", resolveErr)
	}
	if typed.Op != "resolve" {
		t.Fatalf("Op = %q, want resolve", typed.Op)
	}
}

func TestNopSinkAndSinkFunc(t *testing.T) {
	engine.NopSink().Emit(context.Background(), engine.Event{Kind: engine.EventProgress})

	var got []engine.EventKind
	sink := engine.SinkFunc(func(_ context.Context, e engine.Event) { got = append(got, e.Kind) })
	sink.Emit(context.Background(), engine.Event{Kind: engine.EventWarning})
	sink.Emit(context.Background(), engine.Event{Kind: engine.EventSummary})
	if len(got) != 2 || got[0] != engine.EventWarning || got[1] != engine.EventSummary {
		t.Fatalf("got %v", got)
	}
}
