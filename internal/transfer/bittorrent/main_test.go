package bittorrent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"testing"
	"time"
)

// The acceptance tests for staging ownership need real processes: the lock is
// an OS-level flock/LockFileEx, and process death is part of what must be
// proven. The test binary re-executes itself in helper mode (the pattern used
// by internal/collection and internal/filelock tests) so the child runs the
// real Session, not a stand-in.
const (
	helperEnv     = "BITTORRENT_HELPER"
	helperOp      = "BITTORRENT_HELPER_OP"
	helperStaging = "BITTORRENT_HELPER_STAGING"
	helperSource  = "BITTORRENT_HELPER_SOURCE"
	helperSelect  = "BITTORRENT_HELPER_SELECT"
	helperPeer    = "BITTORRENT_HELPER_PEER"
	helperSeed    = "BITTORRENT_HELPER_SEED"
	helperGate    = "BITTORRENT_HELPER_GATE"

	// helperExitBusy is a distinct code so the parent can tell "another
	// process owns the root" from a real failure.
	helperExitBusy = 42
	helperExitErr  = 1
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(runHelper())
	}
	code := m.Run()
	// The CLI built for the status acceptance tests lives outside the test
	// temp dirs so it survives between tests; remove it once.
	if cliBuildDir != "" {
		_ = os.RemoveAll(cliBuildDir)
	}
	os.Exit(code)
}

// runHelper executes one child-process scenario:
//
//	op=hold     take ownership, print READY, wait for stdin EOF (or a signal)
//	op=fetch    take ownership, print READY, then Fetch the selection
//	op=list     take ownership, print READY, then Inspect the source, which must
//	            not need the lock this process already holds
//	op=inspect  build no Session at all and Inspect the source
//
// It prints READY only after New() returned, so the parent's barrier means
// "this process owns the staging root".
func runHelper() int {
	cfg := Config{
		StagingDir:      os.Getenv(helperStaging),
		Discovery:       DiscoveryDirect,
		MetadataTimeout: 20 * time.Second,
		Timeout:         60 * time.Second,
	}
	if p := os.Getenv(helperPeer); p != "" {
		cfg.Peers = []string{p}
	}
	if os.Getenv(helperSeed) == "1" {
		cfg.KeepSeeding = true
	}

	source := os.Getenv(helperSource)
	var selection []string
	for _, s := range strings.Split(os.Getenv(helperSelect), ",") {
		if s = strings.TrimSpace(s); s != "" {
			selection = append(selection, s)
		}
	}

	// op=inspect models the CLI: no Session, no staging root, no lock.
	if os.Getenv(helperOp) == "inspect" {
		info, ierr := Inspect(context.Background(), source, InspectConfig{
			Peers:           cfg.Peers,
			Discovery:       DiscoveryDirect,
			MetadataTimeout: 30 * time.Second,
		})
		if ierr != nil {
			fmt.Printf("ERR %v\n", ierr)
			return helperExitErr
		}
		fmt.Printf("OK files=%d name=%s\n", len(info.Files), info.Name)
		return 0
	}

	sess, err := New(cfg)
	if err != nil {
		if errors.Is(err, ErrStagingBusy) {
			fmt.Println("BUSY")
			return helperExitBusy
		}
		fmt.Printf("ERR %v\n", err)
		return helperExitErr
	}
	fmt.Println("READY")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// The gate is what makes the busy test deterministic: the owner holds the
	// root and waits, so the competitor's attempt cannot race the release.
	gated := false
	if os.Getenv(helperGate) == "stdin" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		gated = true
	}

	switch os.Getenv(helperOp) {
	case "hold":
		// A gated hold already ended at stdin EOF; an ungated hold waits for a
		// signal (or for the parent to kill it).
		if !gated {
			<-ctx.Done()
		}
	case "list":
		// The session holds the root; inspection must not need it.
		info, lerr := Inspect(ctx, source, InspectConfig{
			Peers:           cfg.Peers,
			Discovery:       DiscoveryDirect,
			MetadataTimeout: 30 * time.Second,
		})
		if lerr != nil {
			fmt.Printf("ERR %v\n", lerr)
			_ = sess.Close()
			return helperExitErr
		}
		fmt.Printf("OK files=%d\n", len(info.Files))
	case "fetch":
		cap, ferr := sess.Fetch(ctx, source, selection)
		if ferr != nil {
			fmt.Printf("ERR %v\n", ferr)
			_ = sess.Close()
			return helperExitErr
		}
		fmt.Printf("OK manifest=%s key=%s reconciled=%t bytes_read=%d\n",
			cap.ManifestPath, cap.CaptureKey, cap.Reconciled, cap.BytesRead)
	default:
		fmt.Printf("ERR unknown op %q\n", os.Getenv(helperOp))
		_ = sess.Close()
		return helperExitErr
	}

	if err := sess.Close(); err != nil {
		fmt.Printf("ERR close: %v\n", err)
		return helperExitErr
	}
	fmt.Println("DONE")
	return 0
}
