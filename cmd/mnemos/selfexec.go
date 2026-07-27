package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Self-execution guard for mnemos's detached background workers.
//
// Several chores re-exec this binary and return immediately so the caller (a
// session hook, a recall) never blocks: the nightly consolidation, the health
// snapshot, durability classification, and incremental capture. Each does
// `os.Executable()` + `exec.Command(self, ...)` + `Process.Release()`.
//
// Under `go test` that pattern is a fork bomb. `os.Executable()` resolves to the
// compiled test binary, and a Go test binary handed a subcommand it does not
// recognise does NOT error — it ignores the unknown arguments and runs the whole
// suite. So a detached self-exec from a test starts a second copy of the suite,
// which reaches the same call site and starts a third, and so on. Every child is
// detached and silent, so the test run itself looks completely normal.
//
// Observed on 2026-07-27: ~1000 stray `mnemos.test` processes and a load average
// near 900, which starved unrelated work on the machine for hours and only
// stopped at a reboot. `health --journal` and `consolidate --forget-refuted`
// were the two spawn sites reached most often.
//
// The fix is to make "am I a test binary?" a precondition of self-exec rather
// than something each call site has to remember.

// underTest reports whether this process is a binary built by `go test`.
//
// Two independent signals, because each alone has a gap: the testing package
// registers its -test.* flags only in a test binary, and `.test` is what the Go
// toolchain names that binary. Checking both means an unusual build or an
// unexpected argv still trips one of them.
//
// The asymmetry is deliberate. A false positive costs one skipped background
// chore in a test run, where nothing depends on it. A false negative costs the
// fork bomb described above. When in doubt, refuse to spawn.
func underTest() bool {
	if flag.Lookup("test.v") != nil {
		return true
	}
	self, err := os.Executable()
	return err == nil && strings.HasSuffix(filepath.Base(self), ".test")
}

// spawnWorker re-execs this binary with args as a detached background worker and
// returns whether it started one. It appends the configured store DSN so the
// worker addresses the same brain as its parent.
//
// It is a package var so tests can assert which worker a code path *decides* to
// run without paying for a real process — the behaviour worth testing is the
// decision and its arguments, not os/exec.
var spawnWorker = func(args []string) bool {
	if underTest() {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	cmd := exec.Command(self, workerArgs(args)...) //nolint:gosec // self-exec with fixed args
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = detachSysProcAttr()
	if err := cmd.Start(); err != nil {
		return false
	}
	// Release rather than Wait: the worker outlives us by design, and init
	// reaps it. Nothing here is waiting for its result.
	_ = cmd.Process.Release()
	return true
}

// workerArgs appends the store DSN when one is configured, so a detached worker
// writes to the same brain the parent read.
func workerArgs(args []string) []string {
	if dsn := strings.TrimSpace(os.Getenv("MNEMOS_DB_URL")); dsn != "" {
		return append(append([]string{}, args...), "--db", dsn)
	}
	return append([]string{}, args...)
}
