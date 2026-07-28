package main

import (
	"os"
	"testing"
	"time"
)

func TestSleepDue(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	now := time.Now()
	if !sleepDue(now) {
		t.Fatal("a missing stamp must read as due — a new brain should get to sleep")
	}
	markSleep(now)
	if sleepDue(now.Add(time.Hour)) {
		t.Fatal("an hour later is still inside a working day, not due")
	}
	if !sleepDue(now.Add(sleepInterval + time.Minute)) {
		t.Fatal("past the interval must be due")
	}
}

// A corrupt/unusable stamp must not put the brain into permanent insomnia.
func TestSleepDue_UnusableStampFailsOpen(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if err := os.MkdirAll(sleepStamp(), 0o750); err != nil { // a dir where a file belongs
		t.Fatal(err)
	}
	if !sleepDue(time.Now()) {
		t.Fatal("an unusable stamp must read as due")
	}
}

// The stamp-on-spawn contract, verified through the spawn seam.
//
// This test used to call maybeSleep for real, which re-execed the test binary —
// and a Go test binary handed `consolidate` runs the whole suite, reaching this
// test again and spawning again. That is the fork bomb described in selfexec.go.
// It also `t.Skip`ped when the spawn failed, so the day it started misbehaving
// it reported success either way.
//
// Stubbing the seam tests the actual contract (stamp on spawn, not on success)
// without starting a process at all. See
// TestMaybeSleep_DecisionIsTestableWithoutSpawning in selfexec_test.go for the
// argument assertions.
func TestMaybeSleep_StampsOnSpawnNotSuccess(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("MNEMOS_DB_URL", "memory://")

	restore := stubSpawnWorker(t, func([]string) bool { return true })
	defer restore()

	now := time.Now()
	if !maybeSleep(now) {
		t.Fatal("a due sleep must run")
	}
	if _, err := os.Stat(sleepStamp()); err != nil {
		t.Fatalf("spawning must stamp immediately: %v", err)
	}
	if maybeSleep(now) {
		t.Fatal("a second call within the interval must not spawn again")
	}
}

// A hosted session consolidates server-side; the local worker must not run
// against whatever store this machine happens to resolve.
func TestMaybeSleep_SkipsHostedBrains(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("MNEMOS_URL", "https://brain.example.com")
	if maybeSleep(time.Now()) {
		t.Fatal("hosted brains must not run a local consolidation")
	}
}

// The nightly sleep must include the narration-clearing step (the treadmill
// cure) and must NOT include aggressive trust-floor forgetting.
func TestSleepArgs(t *testing.T) {
	args := sleepArgs()
	has := func(f string) bool {
		for _, a := range args {
			if a == f {
				return true
			}
		}
		return false
	}
	if args[0] != "consolidate" {
		t.Fatalf("sleep must run consolidate, got %v", args)
	}
	if !has("--clear-session-noise") {
		t.Error("nightly sleep must clear session noise, or the treadmill persists")
	}
	if has("--forget-below-trust") {
		t.Error("an unattended nightly pass must not do aggressive trust-floor forgetting")
	}
}
