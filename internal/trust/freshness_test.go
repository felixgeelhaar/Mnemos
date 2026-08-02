package trust

import (
	"testing"
	"time"
)

// FreshnessRef is the timestamp Staleness and IsStale have always derived
// privately. It is exported so a caller that wants to evaluate the SAME decay
// model at a future instant — projecting how much trust a belief still has to
// lose — picks the same reference the model already uses, instead of
// re-deriving "most recent of evidence and verification" and drifting from it.
func TestFreshnessRef_PicksTheMostRecentSignal(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	if got := FreshnessRef(older, newer); !got.Equal(newer) {
		t.Errorf("re-verification is the fresher signal: got %v, want %v", got, newer)
	}
	if got := FreshnessRef(newer, older); !got.Equal(newer) {
		t.Errorf("evidence newer than the last verification must win: got %v, want %v", got, newer)
	}
	if got := FreshnessRef(newer, time.Time{}); !got.Equal(newer) {
		t.Errorf("never verified falls back to evidence: got %v, want %v", got, newer)
	}
}

// A belief with neither signal cannot be dated, and the model refuses to
// penalise what it cannot date. Callers use the zero return to tell "fresh"
// apart from "unmeasurable".
func TestFreshnessRef_UndateableBeliefIsZero(t *testing.T) {
	if got := FreshnessRef(time.Time{}, time.Time{}); !got.IsZero() {
		t.Errorf("no evidence and never verified = %v, want the zero time", got)
	}
}

// The public decay functions must agree with the reference they document,
// otherwise a projection built on FreshnessRef would decay from a different
// instant than Staleness reports.
func TestFreshnessRef_AgreesWithStaleness(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	evidence := now.Add(-300 * 24 * time.Hour)
	verified := now.Add(-10 * 24 * time.Hour)

	ref := FreshnessRef(evidence, verified)
	viaRef := Staleness(ref, time.Time{}, now, 30)
	direct := Staleness(evidence, verified, now, 30)
	if viaRef != direct {
		t.Errorf("Staleness via FreshnessRef = %v, direct = %v; the two must be the same model", viaRef, direct)
	}
}
