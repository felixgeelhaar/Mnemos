package relate

import (
	"fmt"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// fanOutEngine builds an engine with a fixed clock and sequential IDs, matching
// the rest of the relate tests.
func fanOutEngine() Engine {
	return Engine{
		now: func() time.Time {
			return time.Date(2026, 4, 12, 15, 0, 0, 0, time.UTC)
		},
		nextID: seqRelationshipIDs(),
	}
}

// corroborating builds n existing claims that all overlap the same topic, so
// every one of them is a `supports` candidate for the new claim below. This is
// the shape that produced 12.4M edges on a production brain: one new claim
// against a brain full of claims about the same subject.
func corroborating(n int) []domain.Claim {
	claims := make([]domain.Claim, n)
	for i := range claims {
		claims[i] = domain.Claim{
			ID:   fmt.Sprintf("cl_old_%03d", i),
			Text: "Revenue increased in Q2 after the product launch",
		}
	}
	return claims
}

func countByType(rels []domain.Relationship, t domain.RelationshipType) int {
	n := 0
	for _, r := range rels {
		if r.Type == t {
			n++
		}
	}
	return n
}

// TestDetectIncrementalCapsSupportsFanOut is the regression test for the edge
// explosion: without a cap, one new claim against 200 corroborating claims
// emits 200 `supports` edges, and the brain grows quadratically with claim
// count until every capture exceeds its budget.
func TestDetectIncrementalCapsSupportsFanOut(t *testing.T) {
	engine := fanOutEngine()

	existing := corroborating(200)
	newClaims := []domain.Claim{
		{ID: "cl_new_1", Text: "Revenue increased in Q2 after the product launch"},
	}

	rels, err := engine.DetectIncremental(newClaims, existing)
	if err != nil {
		t.Fatalf("DetectIncremental() error = %v", err)
	}

	supports := countByType(rels, domain.RelationshipTypeSupports)
	if supports == 0 {
		t.Fatal("DetectIncremental() emitted no supports edges; the fixture no longer corroborates")
	}
	if supports > MaxSupportsPerClaim {
		t.Errorf("DetectIncremental() emitted %d supports edges for one claim, want at most %d",
			supports, MaxSupportsPerClaim)
	}
}

// TestDetectIncrementalCapIsPerNewClaim ensures the cap bounds each new claim
// independently rather than the batch as a whole — otherwise a batch of claims
// would starve, with later claims getting no edges at all.
func TestDetectIncrementalCapIsPerNewClaim(t *testing.T) {
	engine := fanOutEngine()

	existing := corroborating(100)
	newClaims := []domain.Claim{
		{ID: "cl_new_1", Text: "Revenue increased in Q2 after the product launch"},
		{ID: "cl_new_2", Text: "Revenue increased in Q2 after the product launch"},
	}

	rels, err := engine.DetectIncremental(newClaims, existing)
	if err != nil {
		t.Fatalf("DetectIncremental() error = %v", err)
	}

	perClaim := map[string]int{}
	for _, r := range rels {
		if r.Type == domain.RelationshipTypeSupports {
			perClaim[r.FromClaimID]++
		}
	}

	for _, id := range []string{"cl_new_1", "cl_new_2"} {
		if perClaim[id] == 0 {
			t.Errorf("claim %s got no supports edges; the cap starved it", id)
		}
		if perClaim[id] > MaxSupportsPerClaim {
			t.Errorf("claim %s got %d supports edges, want at most %d", id, perClaim[id], MaxSupportsPerClaim)
		}
	}
}

// TestDetectIncrementalKeepsStrongestSupports verifies the cap keeps the
// highest-overlap corroborations rather than whichever happened to be scanned
// first. A cap that truncated by scan order would silently discard the best
// evidence in favour of the oldest.
func TestDetectIncrementalKeepsStrongestSupports(t *testing.T) {
	engine := fanOutEngine()

	// Weak matches come first in scan order, the strong one last: a scan-order
	// truncation would drop exactly the edge that matters.
	existing := make([]domain.Claim, 0, MaxSupportsPerClaim+1)
	for i := 0; i < MaxSupportsPerClaim; i++ {
		existing = append(existing, domain.Claim{
			ID:   fmt.Sprintf("cl_weak_%03d", i),
			Text: "Revenue increased slightly",
		})
	}
	existing = append(existing, domain.Claim{
		ID:   "cl_strong",
		Text: "Revenue increased in Q2 after the product launch worldwide",
	})

	newClaims := []domain.Claim{
		{ID: "cl_new_1", Text: "Revenue increased in Q2 after the product launch"},
	}

	rels, err := engine.DetectIncremental(newClaims, existing)
	if err != nil {
		t.Fatalf("DetectIncremental() error = %v", err)
	}

	for _, r := range rels {
		if r.ToClaimID == "cl_strong" {
			return // strongest corroboration survived the cap
		}
	}
	t.Error("cap dropped the strongest corroboration (cl_strong); it truncated by scan order")
}

// TestDetectIncrementalDoesNotCapContradictions guards the asymmetry that makes
// the cap safe: contradictions are first-class in this codebase and rare in
// practice (29k against 12.4M supports on the brain that motivated this), so
// they must never be dropped to satisfy a fan-out budget.
func TestDetectIncrementalDoesNotCapContradictions(t *testing.T) {
	engine := fanOutEngine()

	n := MaxSupportsPerClaim * 3
	existing := make([]domain.Claim, n)
	for i := range existing {
		existing[i] = domain.Claim{
			ID:   fmt.Sprintf("cl_old_%03d", i),
			Text: "The CEO is Alice",
		}
	}

	newClaims := []domain.Claim{
		{ID: "cl_new_1", Text: "The CEO is Bob"},
	}

	rels, err := engine.DetectIncremental(newClaims, existing)
	if err != nil {
		t.Fatalf("DetectIncremental() error = %v", err)
	}

	contradicts := countByType(rels, domain.RelationshipTypeContradicts)
	if contradicts != n {
		t.Errorf("DetectIncremental() kept %d contradiction edges, want all %d — contradictions must not be capped",
			contradicts, n)
	}
}

// TestDetectIncrementalFanOutIsDeterministic protects the project-wide
// guarantee that the same inputs produce the same edges: a cap backed by map
// iteration would silently break replay and fingerprint stability.
func TestDetectIncrementalFanOutIsDeterministic(t *testing.T) {
	existing := corroborating(120)
	newClaims := []domain.Claim{
		{ID: "cl_new_1", Text: "Revenue increased in Q2 after the product launch"},
	}

	first, err := fanOutEngine().DetectIncremental(newClaims, existing)
	if err != nil {
		t.Fatalf("DetectIncremental() error = %v", err)
	}

	for attempt := 0; attempt < 5; attempt++ {
		again, err := fanOutEngine().DetectIncremental(newClaims, existing)
		if err != nil {
			t.Fatalf("DetectIncremental() error = %v", err)
		}
		if len(again) != len(first) {
			t.Fatalf("run %d produced %d edges, first run produced %d", attempt, len(again), len(first))
		}
		for i := range again {
			if again[i].ToClaimID != first[i].ToClaimID || again[i].Type != first[i].Type {
				t.Fatalf("run %d edge %d = (%s,%s), want (%s,%s)",
					attempt, i, again[i].ToClaimID, again[i].Type, first[i].ToClaimID, first[i].Type)
			}
		}
	}
}
