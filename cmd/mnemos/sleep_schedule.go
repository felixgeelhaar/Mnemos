package main

import (
	"os"
	"path/filepath"
	"time"
)

// Autonomous consolidation — the brain's sleep.
//
// A biological brain does not recover on demand; it consolidates on a cycle,
// mostly during sleep — moving the day's episodic traces into integrated
// knowledge, replaying what mattered, letting the trivial decay, and clearing
// the metabolic waste of a waking day. Mnemos has every one of those organs
// (`consolidate`: dedupe, trust refresh, forget, reinforce, plus session-noise
// clearing), but nothing ran them on a clock for a local brain — so the day's
// narration accumulated until someone ran a pass by hand. A brain that only
// consolidates when poked has insomnia; its dissonance climbs all day and never
// falls.
//
// This gives the local brain a sleep cycle. Once a day, when a session starts
// after a long enough gap (the equivalent of waking), a detached worker runs a
// full consolidation — including the LLM session-noise clearing that turns the
// day's narration-vs-narration contradictions back into quiet. Dissonance then
// follows a circadian rhythm — up through a working day, down overnight —
// instead of a one-way ratchet. Hosted brains are skipped and consolidate on
// their own server-side cron.

// sleepInterval is the minimum gap between automatic consolidations. Just under
// a day so a next-morning session reliably triggers it, and cheap-to-skip
// otherwise: the day's noise is what accrues over hours, not minutes.
const sleepInterval = 20 * time.Hour

// sleepStamp is the marker file whose mtime records the last consolidation.
func sleepStamp() string {
	return filepath.Join(captureStateDir(), "consolidate.stamp")
}

// sleepDue reports whether enough time has passed since the last consolidation.
// A missing or unusable stamp reads as due — the first run on a machine should
// consolidate, and a corrupt stamp must not put the brain into permanent
// insomnia.
func sleepDue(now time.Time) bool {
	info, err := os.Stat(sleepStamp())
	if err != nil || !info.Mode().IsRegular() {
		return true
	}
	return now.Sub(info.ModTime()) >= sleepInterval
}

// markSleep records that a consolidation was spawned. Stamped on SPAWN, not on
// completion: if the pass itself hangs, stamping on success would respawn it at
// every session start; waiting out the interval is the safer failure mode for a
// background chore.
func markSleep(now time.Time) {
	path := sleepStamp()
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

// sleepArgs is the consolidation a nightly sleep runs: the deterministic organs
// that are always safe (dedupe + trust refresh happen by default; forget only
// what an outcome refuted; reinforce what an outcome validated; tag salience),
// plus the LLM session-noise clearing. It deliberately omits aggressive
// trust-floor forgetting — that is a policy choice an operator makes explicitly,
// not something an unattended nightly pass should do.
func sleepArgs() []string {
	return []string{"consolidate", "--forget-refuted", "--reinforce-validated", "--salience", "--clear-session-noise"}
}

// maybeSleep spawns a detached consolidation when one is due, so the brain
// recovers on its own clock. Returns whether it spawned, for tests.
func maybeSleep(now time.Time) bool {
	if hostedConfigured() || !sleepDue(now) {
		return false
	}
	if !spawnWorker(sleepArgs()) {
		return false
	}
	markSleep(now)
	return true
}
