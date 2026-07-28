package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The regression this whole file exists for. A Go test binary handed an
// unrecognised subcommand runs the entire suite instead of erroring, so any
// detached self-exec from a test forks a copy of the suite that forks again.
// On 2026-07-27 that reached ~1000 processes and a load average near 900.
//
// Asserting underTest() here is asserting that no test run can start that.
func TestUnderTest_IsTrueInATestBinary(t *testing.T) {
	if !underTest() {
		self, _ := os.Executable()
		t.Fatalf("underTest() must be true inside `go test` — a false answer re-arms the fork bomb (executable=%q)", self)
	}
}

// Every background chore must route its spawn through the guard. A call site
// that reaches exec.Command on its own is exactly how this bug shipped.
func TestSpawnWorker_RefusesFromATestBinary(t *testing.T) {
	if spawnWorker([]string{"health", "--journal"}) {
		t.Fatal("spawnWorker must refuse to re-exec a test binary")
	}
}

func TestWorkerArgs_AppendsConfiguredDSN(t *testing.T) {
	t.Setenv("MNEMOS_DB_URL", "memory://")
	got := strings.Join(workerArgs([]string{"health", "--journal"}), " ")
	if want := "health --journal --db memory://"; got != want {
		t.Errorf("workerArgs = %q, want %q", got, want)
	}
}

func TestWorkerArgs_OmitsDSNWhenUnset(t *testing.T) {
	t.Setenv("MNEMOS_DB_URL", "")
	got := strings.Join(workerArgs([]string{"consolidate"}), " ")
	if want := "consolidate"; got != want {
		t.Errorf("workerArgs = %q, want %q", got, want)
	}
}

// workerArgs must not write through the caller's backing array — a chore that
// builds its args once and spawns twice would otherwise see the first call's
// DSN duplicated into the second.
func TestWorkerArgs_DoesNotAliasCallerSlice(t *testing.T) {
	t.Setenv("MNEMOS_DB_URL", "memory://")
	base := make([]string, 1, 8) // spare capacity: append would write in place
	base[0] = "consolidate"
	_ = workerArgs(base)
	if len(base) != 1 || base[0] != "consolidate" {
		t.Fatalf("workerArgs mutated the caller's slice: %v", base)
	}
}

// The decision logic stays testable through the seam: with spawning stubbed,
// maybeSleep must still report that it ran, stamp, and refuse a second pass
// inside the interval. This is what the old test verified by spawning a real
// process — the thing that lit the fuse.
func TestMaybeSleep_DecisionIsTestableWithoutSpawning(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("MNEMOS_DB_URL", "memory://")

	var got []string
	restore := stubSpawnWorker(t, func(args []string) bool {
		got = args
		return true
	})
	defer restore()

	now := time.Now()
	if !maybeSleep(now) {
		t.Fatal("a due sleep must run")
	}
	if len(got) == 0 || got[0] != "consolidate" {
		t.Fatalf("sleep must spawn consolidate, got %v", got)
	}
	if _, err := os.Stat(sleepStamp()); err != nil {
		t.Fatalf("spawning must stamp immediately: %v", err)
	}
	if maybeSleep(now) {
		t.Fatal("a second call within the interval must not spawn again")
	}
}

// stubSpawnWorker swaps the spawn seam for the duration of a test.
func stubSpawnWorker(t *testing.T, fn func([]string) bool) func() {
	t.Helper()
	prev := spawnWorker
	spawnWorker = fn
	return func() { spawnWorker = prev }
}
