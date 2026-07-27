package relate

import (
	"fmt"
	"testing"

	"go.klarlabs.de/mnemos/internal/domain"
)

// storedFanOut builds n stored supports edges from one source claim, plus the
// text map needed to score them.
func storedFanOut(n int) ([]domain.Relationship, map[string]string) {
	text := map[string]string{
		"cl_from": "Revenue increased in Q2 after the product launch",
	}
	rels := make([]domain.Relationship, n)
	for i := 0; i < n; i++ {
		to := fmt.Sprintf("cl_to_%03d", i)
		text[to] = "Revenue increased in Q2 after the product launch"
		rels[i] = domain.Relationship{
			ID:          fmt.Sprintf("rel_%03d", i),
			Type:        domain.RelationshipTypeSupports,
			FromClaimID: "cl_from",
			ToClaimID:   to,
		}
	}
	return rels, text
}

func TestExcessSupportsReturnsOnlyTheOverflow(t *testing.T) {
	rels, text := storedFanOut(100)

	drop := ExcessSupports(rels, text, MaxSupportsPerClaim)

	want := 100 - MaxSupportsPerClaim
	if len(drop) != want {
		t.Errorf("ExcessSupports() returned %d edges to drop, want %d", len(drop), want)
	}
	for _, r := range drop {
		if r.Type != domain.RelationshipTypeSupports {
			t.Errorf("ExcessSupports() returned a %s edge; only supports may be dropped", r.Type)
		}
	}
}

func TestExcessSupportsLeavesClaimsUnderTheCapAlone(t *testing.T) {
	rels, text := storedFanOut(MaxSupportsPerClaim)

	if drop := ExcessSupports(rels, text, MaxSupportsPerClaim); len(drop) != 0 {
		t.Errorf("ExcessSupports() wants to drop %d edges from a claim at the cap, want 0", len(drop))
	}
}

// TestExcessSupportsNeverDropsContradictions mirrors the live-detection
// guarantee: a repair pass must not quietly delete the edges the system exists
// to preserve.
func TestExcessSupportsNeverDropsContradictions(t *testing.T) {
	text := map[string]string{"cl_from": "The CEO is Bob"}
	var rels []domain.Relationship
	for i := 0; i < MaxSupportsPerClaim*3; i++ {
		to := fmt.Sprintf("cl_to_%03d", i)
		text[to] = "The CEO is Alice"
		rels = append(rels, domain.Relationship{
			ID:          fmt.Sprintf("rel_%03d", i),
			Type:        domain.RelationshipTypeContradicts,
			FromClaimID: "cl_from",
			ToClaimID:   to,
		})
	}

	if drop := ExcessSupports(rels, text, MaxSupportsPerClaim); len(drop) != 0 {
		t.Errorf("ExcessSupports() returned %d contradiction edges to drop, want 0", len(drop))
	}
}

// TestExcessSupportsSkipsUnknownEndpoints keeps a partial snapshot from
// deleting real edges: an endpoint the caller could not resolve is unscoreable,
// and unknown must never be read as excess.
func TestExcessSupportsSkipsUnknownEndpoints(t *testing.T) {
	rels, text := storedFanOut(100)
	delete(text, "cl_from") // source text unavailable

	if drop := ExcessSupports(rels, text, MaxSupportsPerClaim); len(drop) != 0 {
		t.Errorf("ExcessSupports() returned %d edges for unresolvable endpoints, want 0", len(drop))
	}
}

// TestExcessSupportsIsPerSourceClaim confirms the budget is applied per claim,
// so a brain-wide pass cannot strip one claim bare to pay for another.
func TestExcessSupportsIsPerSourceClaim(t *testing.T) {
	text := map[string]string{}
	var rels []domain.Relationship
	for _, from := range []string{"cl_a", "cl_b"} {
		text[from] = "Revenue increased in Q2 after the product launch"
		for i := 0; i < MaxSupportsPerClaim+5; i++ {
			to := fmt.Sprintf("%s_to_%03d", from, i)
			text[to] = "Revenue increased in Q2 after the product launch"
			rels = append(rels, domain.Relationship{
				ID:          fmt.Sprintf("rel_%s_%03d", from, i),
				Type:        domain.RelationshipTypeSupports,
				FromClaimID: from,
				ToClaimID:   to,
			})
		}
	}

	drop := ExcessSupports(rels, text, MaxSupportsPerClaim)

	perClaim := map[string]int{}
	for _, r := range drop {
		perClaim[r.FromClaimID]++
	}
	for _, from := range []string{"cl_a", "cl_b"} {
		if perClaim[from] != 5 {
			t.Errorf("claim %s: %d edges dropped, want 5", from, perClaim[from])
		}
	}
}

// TestExcessSupportsIsDeterministic guards replay stability: the same brain must
// yield the same deletions, independent of the order storage returned edges in.
func TestExcessSupportsIsDeterministic(t *testing.T) {
	rels, text := storedFanOut(80)

	first := ExcessSupports(rels, text, MaxSupportsPerClaim)

	// Reverse the input order — storage order must not change the outcome.
	shuffled := make([]domain.Relationship, len(rels))
	for i, r := range rels {
		shuffled[len(rels)-1-i] = r
	}
	second := ExcessSupports(shuffled, text, MaxSupportsPerClaim)

	if len(first) != len(second) {
		t.Fatalf("input order changed the drop count: %d vs %d", len(first), len(second))
	}
	firstIDs := map[string]struct{}{}
	for _, r := range first {
		firstIDs[r.ID] = struct{}{}
	}
	for _, r := range second {
		if _, ok := firstIDs[r.ID]; !ok {
			t.Fatalf("input order changed which edges are dropped (%s only in one run)", r.ID)
		}
	}
}
