package brainbench

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain pins this test binary at a throwaway brain.
//
// This is the failure mode CLAUDE.md documents under "tests must not touch the
// developer's real brain": when MNEMOS_DB_URL is unset, DSN resolution falls
// back to ~/.local/share/mnemos/mnemos.db — the developer's live global brain.
// It once wrote test fixtures into a 118 MB production brain, one row per run,
// and surfaced later as a load-dependent -race failure that looked like
// flakiness.
//
// This package is unusually exposed to it. Every arm opens a store, and
// mnemos.New falls through to the global default whenever a storage option is
// missing, so one future helper that forgets WithStorage would write live data
// while every assertion still passed.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "brainbench-testbrain-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "brainbench testmain: create isolated brain dir: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("MNEMOS_DB_URL", "sqlite://"+filepath.Join(dir, "throwaway.db")); err != nil {
		fmt.Fprintf(os.Stderr, "brainbench testmain: set MNEMOS_DB_URL: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
