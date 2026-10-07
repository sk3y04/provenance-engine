// Package externaltest is a compile-time consumer of the public engine facade.
//
// It exists to prove that an external Go module can import
// github.com/sk3y04/provenance-engine/engine using only exported identifiers.
// It is a library (not a command) so `go build ./...` leaves no binary behind,
// and it is only built, never executed by the test suite.
package externaltest

import (
	"context"

	"github.com/sk3y04/provenance-engine/engine"
)

// Resolve calls the facade through its exported API. The compiler verifies the
// external import; nothing here contacts a network.
func Resolve(ctx context.Context, eng *engine.Engine, rawURL string) (engine.Source, error) {
	return eng.Resolve(ctx, engine.ResolveRequest{
		URL:   rawURL,
		Limit: 10,
	}, engine.SinkFunc(func(context.Context, engine.Event) {}))
}

// Download calls the facade through its exported API.
func Download(ctx context.Context, eng *engine.Engine, rawURL string) (engine.Result, error) {
	return eng.Download(ctx, engine.DownloadRequest{URL: rawURL}, engine.NopSink())
}

// Kind reports the category of an engine error using only exported helpers.
func Kind(err error) engine.ErrorKind { return engine.ErrorKindOf(err) }
