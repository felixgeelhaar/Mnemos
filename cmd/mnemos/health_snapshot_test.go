package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHealthSnapshotDue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	now := time.Now()

	// Nothing recorded yet: the first run on a machine must collect a point,
	// or the series never starts.
	if !healthSnapshotDue(now) {
		t.Fatal("a missing stamp must read as due")
	}

	markHealthSnapshot(now)
	if healthSnapshotDue(now.Add(time.Hour)) {
		t.Fatal("an hour later is not due; the vital moves over days")
	}
	if !healthSnapshotDue(now.Add(healthSnapshotInterval + time.Minute)) {
		t.Fatal("past the interval must be due")
	}
}

// A corrupt or unreadable stamp must not silence the series forever — failing
// open costs one extra scan a day, failing closed loses the data permanently.
func TestHealthSnapshotDue_UnreadableStampFailsOpen(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	stamp := healthSnapshotStamp()
	if err := os.MkdirAll(stamp, 0o750); err != nil { // a directory where a file belongs
		t.Fatal(err)
	}
	if !healthSnapshotDue(time.Now()) {
		t.Fatal("an unusable stamp must read as due")
	}
}

// The stamp is written when the snapshot is SPAWNED, not when it succeeds: if
// the scan itself hangs or dies, stamping on success would respawn it at every
// single session start.
func TestMaybeSnapshotHealth_StampsOnSpawnNotSuccess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("MNEMOS_DB_URL", "memory://")
	now := time.Now()

	// Stubbed rather than spawned for real: re-execing the test binary runs the
	// whole suite, which reaches this test and spawns again (see selfexec.go).
	// The old `t.Skip` on failure also meant this passed either way.
	restore := stubSpawnWorker(t, func([]string) bool { return true })
	defer restore()

	if !maybeSnapshotHealth(now) {
		t.Fatal("a due snapshot must run")
	}
	if _, err := os.Stat(healthSnapshotStamp()); err != nil {
		t.Fatalf("spawning must stamp immediately: %v", err)
	}
	// A second call in the same window must not spawn again.
	if maybeSnapshotHealth(now) {
		t.Fatal("rate limit did not hold within the interval")
	}
}

// A hosted session's brain is not the store this machine resolves, so scanning
// locally would journal vitals for the wrong brain entirely.
func TestMaybeSnapshotHealth_SkipsHostedBrains(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("MNEMOS_URL", "https://brain.example.com")
	if maybeSnapshotHealth(time.Now()) {
		t.Fatal("hosted brains must not snapshot the local store")
	}
	if _, err := os.Stat(filepath.Join(dir, "mnemos", "capture-state", "health-snapshot.stamp")); err == nil {
		t.Fatal("a skipped snapshot must not stamp")
	}
}
