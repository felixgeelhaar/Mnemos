package main

import (
	"testing"

	"go.klarlabs.de/mnemos/internal/domain"
)

func TestSelectSessionLocalClaims_OnlyExplicitVerdicts(t *testing.T) {
	claims := []domain.Claim{
		{ID: "narration", Durability: domain.DurabilitySessionLocal, Status: domain.ClaimStatusActive},
		{ID: "contested-narration", Durability: domain.DurabilitySessionLocal, Status: domain.ClaimStatusContested},
		{ID: "durable", Durability: domain.DurabilityDurable, Status: domain.ClaimStatusActive},
		{ID: "unclassified", Status: domain.ClaimStatusActive},
	}

	got := selectSessionLocalClaims(claims)

	if len(got) != 2 {
		t.Fatalf("expected the two session-local claims, got %d: %+v", len(got), got)
	}
	for _, c := range got {
		if !c.Durability.IsSessionLocal() {
			t.Errorf("selected a claim that is not session-local: %+v", c)
		}
	}
}

// "Not yet judged" is not "narration". 80,703 claims on a real brain carried no
// verdict; retiring those would empty it.
func TestSelectSessionLocalClaims_NeverTouchesUnclassified(t *testing.T) {
	claims := []domain.Claim{
		{ID: "unset", Status: domain.ClaimStatusActive},
		{ID: "unknown", Durability: domain.DurabilityUnknown, Status: domain.ClaimStatusActive},
	}
	if got := selectSessionLocalClaims(claims); len(got) != 0 {
		t.Errorf("unclassified claims must never be retired, got %+v", got)
	}
}

// Idempotent: an already-deprecated claim is not re-selected, so a second run
// does not rewrite it and append another status-history row.
func TestSelectSessionLocalClaims_SkipsAlreadyRetired(t *testing.T) {
	claims := []domain.Claim{
		{ID: "done", Durability: domain.DurabilitySessionLocal, Status: domain.ClaimStatusDeprecated},
	}
	if got := selectSessionLocalClaims(claims); len(got) != 0 {
		t.Errorf("an already-deprecated claim must not be re-selected, got %+v", got)
	}
}

func TestParsePruneArgs_AcceptsSessionLocal(t *testing.T) {
	got, err := parsePruneArgs([]string{"--session-local"})
	if err != nil {
		t.Fatalf("parsePruneArgs: %v", err)
	}
	if got != "session-local" {
		t.Errorf("target = %q, want %q", got, "session-local")
	}
}

// A bare `prune` must stay an error: it must never guess at a destructive
// operation, and adding a third target does not change that.
func TestParsePruneArgs_StillRefusesABareprune(t *testing.T) {
	if _, err := parsePruneArgs(nil); err == nil {
		t.Error("a bare `prune` must remain an error")
	}
}
