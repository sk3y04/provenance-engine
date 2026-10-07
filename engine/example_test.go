package engine_test

import (
	"context"
	"fmt"
	"time"

	"github.com/sk3y04/provenance-engine/engine"
)

// ExampleEngine_Resolve shows external-module usage of the facade. It has no
// Output comment, so it is compiled but not executed: the test suite must never
// contact a live service.
func ExampleEngine_Resolve() {
	eng, err := engine.New(engine.Config{
		WorkDir:     "downloads",
		Quality:     "720",
		Concurrency: 2,
		MaxItems:    50,
		Timeout:     2 * time.Second,
	})
	if err != nil {
		fmt.Println("engine:", err)
		return
	}

	sink := engine.SinkFunc(func(_ context.Context, e engine.Event) {
		fmt.Println(e.Kind, e.Stage, e.URL)
	})

	src, err := eng.Resolve(context.Background(), engine.ResolveRequest{
		URL:   "https://example.com/video",
		Limit: 10,
	}, sink)
	if err != nil {
		fmt.Println("resolve:", err)
		return
	}
	fmt.Println(src.Kind, len(src.Items))
}

// ExampleEngine_Download shows the download contract, including typed errors.
func ExampleEngine_Download() {
	eng, err := engine.New(engine.Config{WorkDir: "downloads"})
	if err != nil {
		fmt.Println("engine:", err)
		return
	}

	res, err := eng.Download(context.Background(), engine.DownloadRequest{
		URL: "https://example.com/video",
	}, engine.NopSink())
	if err != nil {
		if engine.IsErrorKind(err, engine.ErrorCanceled) {
			return
		}
		fmt.Println("download:", err)
		return
	}
	for _, a := range res.Artifacts {
		fmt.Println(a.Filename, a.Size, a.SHA256)
	}
}
