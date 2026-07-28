package query

import (
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// The regression this closes: rule-based extraction over transcripts stored
// conversational narration as beliefs, the durability classifier correctly
// judged 4,064 of them session-local on one real brain — and recall surfaced
// every one anyway, because nothing between the store and the answer looked at
// the verdict. Fragments like "Now the doctor output:" were injected as context
// at trust ~0.67.
func TestAdmitClaims_DropsSessionLocalClaims(t *testing.T) {
	claims := []domain.Claim{
		{ID: "narration", Text: "Now the doctor output:", Status: domain.ClaimStatusActive, Durability: domain.DurabilitySessionLocal},
		{ID: "knowledge", Text: "warden pins actions/checkout at v7.0.1", Status: domain.ClaimStatusActive, Durability: domain.DurabilityDurable},
	}

	got := admitClaims(claims, AnswerOptions{}, time.Now())

	for _, c := range got {
		if c.ID == "narration" {
			t.Error("a session-local claim reached recall — narration is not knowledge")
		}
	}
	if len(got) != 1 || got[0].ID != "knowledge" {
		t.Fatalf("the durable claim must survive; got %d claims: %+v", len(got), got)
	}
}

// Unclassified is not narration. 80,703 claims on that same brain carry no
// verdict yet, so treating "not yet judged" as session-local would empty recall
// — a far worse failure than leaving noise in it.
func TestAdmitClaims_KeepsUnclassifiedClaims(t *testing.T) {
	claims := []domain.Claim{
		{ID: "unset", Text: "nox runs at 1.24.0 in CI", Status: domain.ClaimStatusActive},
	}

	if got := admitClaims(claims, AnswerOptions{}, time.Now()); len(got) != 1 {
		t.Fatalf("an unclassified claim must still be recalled, got %d", len(got))
	}
}

// Suppression is a recall filter, not retention: the claim is not deprecated
// and not deleted, so audit and history paths still see it.
func TestAdmitClaims_SessionLocalIsNotDeprecation(t *testing.T) {
	c := domain.Claim{ID: "n", Status: domain.ClaimStatusActive, Durability: domain.DurabilitySessionLocal}
	if c.Status == domain.ClaimStatusDeprecated {
		t.Fatal("filtering must not require deprecating the claim")
	}
	if got := admitClaims([]domain.Claim{c}, AnswerOptions{}, time.Now()); len(got) != 0 {
		t.Errorf("expected the session-local claim to be filtered from recall, got %+v", got)
	}
}
