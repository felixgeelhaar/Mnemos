package main

import (
	"os"
	"path/filepath"
	"time"
)

// Automatic brain-health snapshots.
//
// ADR 0019 shipped the five vitals with thresholds it openly called
// "conservative defaults", and said tuning them against real data "is exactly
// what the journal (ADR 0018) now enables". That tuning never became possible:
// recording a snapshot requires `mnemos health --journal`, nothing runs it on a
// cadence, and so the journal stays empty. Measured on a real brain after
// months of use it held zero rows of any kind — the time series the thresholds
// were meant to be calibrated from had never collected a single point, so
// "degraded" and "unhealthy" have never been grounded verdicts for anyone.
//
// Session start is the natural cadence: once per session, already a moment the
// hooks run. But a health snapshot is a full scan — 7.5s on a 19,640-claim
// brain, and it grows with the brain — and SessionStart is latency-sensitive,
// so it is spawned detached and rate-limited rather than run inline.
//
// It is best-effort in every direction. A failure to snapshot must never cost
// the user their brief.

// healthSnapshotInterval is the minimum gap between automatic snapshots. Daily
// is enough to calibrate a threshold and cheap enough to be invisible; the
// vital moves over days of use, not minutes.
const healthSnapshotInterval = 24 * time.Hour

// healthSnapshotStamp is the marker file whose mtime records the last snapshot.
// An mtime carries the whole state, so there is no format to parse or corrupt.
func healthSnapshotStamp() string {
	return filepath.Join(captureStateDir(), "health-snapshot.stamp")
}

// healthSnapshotDue reports whether enough time has passed since the last
// automatic snapshot. An unusable or missing stamp means due: the first run on
// a new machine should collect a point, and a corrupt stamp should not silence
// the series forever.
func healthSnapshotDue(now time.Time) bool {
	info, err := os.Stat(healthSnapshotStamp())
	if err != nil || !info.Mode().IsRegular() {
		// Anything that is not a readable regular file — missing, a directory,
		// a device — carries no usable timestamp. Fail open: an extra scan a
		// day costs little, while failing closed would silence the series
		// permanently on a path we could never write.
		return true
	}
	return now.Sub(info.ModTime()) >= healthSnapshotInterval
}

// markHealthSnapshot records that a snapshot was spawned. It is stamped BEFORE
// the child finishes — deliberately. If the snapshot itself is what fails or
// hangs, stamping on success would respawn it at every session start forever;
// waiting a day to retry is the safer failure mode for a diagnostic.
func markHealthSnapshot(now time.Time) {
	path := healthSnapshotStamp()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_ = f.Close()
	_ = os.Chtimes(path, now, now)
}

// maybeSnapshotHealth spawns a detached `mnemos health --journal` when one is
// due, so the vitals accumulate as a time series without the user opting in per
// run. Returns whether it spawned, for tests.
//
// Hosted brains are skipped: the snapshot would scan whichever store this
// machine resolves, which is not the brain the session is actually using.
func maybeSnapshotHealth(now time.Time) bool {
	if hostedConfigured() || !healthSnapshotDue(now) {
		return false
	}
	if !spawnWorker([]string{"health", "--journal"}) {
		return false
	}
	markHealthSnapshot(now)
	return true
}
