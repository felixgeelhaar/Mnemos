package main

import (
	"testing"
	"time"
)

// The write phase is what overruns the job budget, and the overrun is not
// benign: the attempt is cancelled mid-write and retried from a fresh scan, so
// a brain large enough to exceed it never finishes on the default 10m budget.
// Observed on a 1.6 GB brain (19,482 claims affected): repeated scans, each
// writing for 10 minutes before being cancelled.
func TestWritePhaseEstimateScalesWithClaimsAffected(t *testing.T) {
	tests := []struct {
		name           string
		claimsAffected int
		wantAtLeast    time.Duration
	}{
		{"small brain stays inside the default budget", 161, 0},
		{"the 1.6 GB brain that overran the default budget", 19482, 11 * time.Minute},
		{"the 4.2 GB brain", 52692, time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := writePhaseEstimate(tt.claimsAffected)
			if got < tt.wantAtLeast {
				t.Errorf("writePhaseEstimate(%d) = %s, want at least %s", tt.claimsAffected, got, tt.wantAtLeast)
			}
		})
	}
}

// The estimate is only useful if it actually trips the warning for the brains
// that motivated it — pin the comparison against the real default budget.
func TestWritePhaseEstimateTripsWarningForLargeBrains(t *testing.T) {
	defaultBudget := 10 * time.Minute

	if est := writePhaseEstimate(161); est > defaultBudget {
		t.Errorf("a 161-claim pass estimates %s, over the %s budget — it completed fine in practice", est, defaultBudget)
	}
	if est := writePhaseEstimate(19482); est <= defaultBudget {
		t.Errorf("a 19,482-claim pass estimates %s, within the %s budget — but it overran it in practice", est, defaultBudget)
	}
}

// MNEMOS_JOB_TIMEOUT is what the warning tells the user to raise, so the
// comparison must read the live value rather than a hardcoded default.
func TestJobTimeoutHonorsEnvForTheWarning(t *testing.T) {
	t.Setenv("MNEMOS_JOB_TIMEOUT", "6h")

	if got := jobTimeout(); got != 6*time.Hour {
		t.Fatalf("jobTimeout() = %s, want 6h", got)
	}
	// With the budget raised, the 4.2 GB brain must no longer trip the warning.
	if est := writePhaseEstimate(52692); est > jobTimeout() {
		t.Errorf("estimate %s still exceeds a 6h budget; the suggested fix would not help", est)
	}
}
