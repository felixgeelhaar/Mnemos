package pipeline

import (
	"context"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/store"
	_ "go.klarlabs.de/mnemos/internal/store/memory"
)

// seedMergeClaim writes one active claim with its own anchoring event and
// evidence link, so a merge has real evidence to union.
func seedMergeClaim(t *testing.T, conn *store.Conn, id string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := conn.Events.Append(ctx, domain.Event{
		ID: "ev_" + id, Content: "source for " + id, SourceInputID: "src_" + id,
		Timestamp: now, IngestedAt: now,
	}); err != nil {
		t.Fatalf("append event for %s: %v", id, err)
	}
	if err := conn.Claims.Upsert(ctx, []domain.Claim{{
		ID: id, Text: "claim " + id, Type: domain.ClaimTypeFact, Confidence: 0.8,
		Status: domain.ClaimStatusActive, CreatedAt: now, ValidFrom: now,
	}}); err != nil {
		t.Fatalf("upsert claim %s: %v", id, err)
	}
	if err := conn.Claims.UpsertEvidence(ctx, []domain.ClaimEvidence{{ClaimID: id, EventID: "ev_" + id}}); err != nil {
		t.Fatalf("upsert evidence for %s: %v", id, err)
	}
}

// seedLoserDependents hangs every kind of side-table row a claim can own off
// the losing claim: an entity link, a forward expectation, and feedback state.
// These are the dependents that used to survive the merge pointing at a claim
// that was no longer reachable.
func seedLoserDependents(t *testing.T, conn *store.Conn, loserID string) domain.Entity {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	ent, err := conn.Entities.FindOrCreate(ctx, "kafka", domain.EntityTypeConcept, "tester")
	if err != nil {
		t.Fatalf("FindOrCreate entity: %v", err)
	}
	if err := conn.Entities.LinkClaim(ctx, loserID, ent.ID, "mention"); err != nil {
		t.Fatalf("LinkClaim: %v", err)
	}
	if conn.Expectations != nil {
		if err := conn.Expectations.Upsert(ctx, domain.Expectation{
			ClaimID: loserID, Predicted: 42, Tolerance: 1, CreatedAt: now,
		}); err != nil {
			t.Fatalf("Expectations.Upsert: %v", err)
		}
	}
	if conn.Feedback != nil {
		if err := conn.Feedback.Upsert(ctx, domain.ClaimFeedback{
			ClaimID: loserID, HelpfulCount: 3, NegativeFeedbackStreak: 1, LastFeedbackAt: now,
		}); err != nil {
			t.Fatalf("Feedback.Upsert: %v", err)
		}
	}
	return ent
}

// evidenceEventsFor returns the event ids linked to a claim.
func evidenceEventsFor(t *testing.T, conn *store.Conn, claimID string) []string {
	t.Helper()
	links, err := conn.Claims.ListEvidenceByClaimIDs(context.Background(), []string{claimID})
	if err != nil {
		t.Fatalf("ListEvidenceByClaimIDs: %v", err)
	}
	out := make([]string, 0, len(links))
	for _, l := range links {
		out = append(out, l.EventID)
	}
	return out
}

// TestApplySemanticDedupe_StrandsNoOrphanedDependents is the N3 regression.
//
// The merge used to move the loser's evidence to the winner and then attempt
// the delete WITHOUT having dealt with the claim↔entity links. On an
// FK-enforcing backend that delete fails, so the merge aborted having already
// stripped the loser: an ACTIVE claim with zero evidence, still reachable by
// recall, plus an entity link, an expectation and feedback state all pointing
// at a claim the merge considered gone.
//
// The contract now: the winner ends up with the UNION of the evidence and every
// reachable dependent, and no active un-evidenced claim survives — a loser whose
// row cannot be removed is a deprecated tombstone that the caller is told about.
func TestApplySemanticDedupe_StrandsNoOrphanedDependents(t *testing.T) {
	ctx := context.Background()
	_, conn := openDedupeDB(t)

	seedMergeClaim(t, conn, "cl_win")
	seedMergeClaim(t, conn, "cl_lose")
	ent := seedLoserDependents(t, conn, "cl_lose")

	merged, err := ApplySemanticDedupe(ctx, conn, SemanticDedupePlan{
		Merges: []SemanticMerge{{WinnerID: "cl_win", DuplicateIDs: []string{"cl_lose"}}},
	})
	if merged != 1 {
		t.Errorf("merged = %d, want 1", merged)
	}

	// 1. No stranded ACTIVE claim. Either the row is gone, or it is a
	//    deprecated tombstone the caller was told about.
	survivors, lerr := conn.Claims.ListByIDs(ctx, []string{"cl_lose"})
	if lerr != nil {
		t.Fatalf("ListByIDs: %v", lerr)
	}
	if len(survivors) == 1 {
		if survivors[0].Status == domain.ClaimStatusActive {
			t.Fatalf("ORPHANED ACTIVE CLAIM SURVIVED the merge: %+v", survivors[0])
		}
		if survivors[0].Status != domain.ClaimStatusDeprecated {
			t.Errorf("surviving loser status = %q, want deprecated", survivors[0].Status)
		}
		if err == nil {
			t.Error("a merge that could not remove the losing row must surface it, not return nil")
		} else if !contains(err.Error(), "cl_lose") {
			t.Errorf("error should name the tombstoned claim, got: %v", err)
		}
	} else if err != nil {
		t.Fatalf("merge removed the loser but still errored: %v", err)
	}

	// 2. The winner retains the UNION of evidence, not just its own.
	events := evidenceEventsFor(t, conn, "cl_win")
	if !containsAll(events, "ev_cl_win", "ev_cl_lose") {
		t.Errorf("winner evidence = %v, want the union {ev_cl_win, ev_cl_lose}", events)
	}

	// 3. The loser's entity link moved to the winner instead of dangling.
	winnerEnts, _, err := conn.Entities.ListEntitiesForClaim(ctx, "cl_win")
	if err != nil {
		t.Fatalf("ListEntitiesForClaim: %v", err)
	}
	found := false
	for _, e := range winnerEnts {
		if e.ID == ent.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("winner did not inherit the loser's entity link %s; links=%+v", ent.ID, winnerEnts)
	}

	// 4. The loser's forward expectation moved to the winner.
	if conn.Expectations != nil {
		exp, ok, err := conn.Expectations.Get(ctx, "cl_win")
		if err != nil {
			t.Fatalf("Expectations.Get: %v", err)
		}
		if !ok || exp.Predicted != 42 {
			t.Errorf("winner did not inherit the loser's expectation: ok=%v exp=%+v", ok, exp)
		}
	}

	// 5. The loser's feedback counters folded into the winner.
	if conn.Feedback != nil {
		fb, ok, err := conn.Feedback.Get(ctx, "cl_win")
		if err != nil {
			t.Fatalf("Feedback.Get: %v", err)
		}
		if !ok || fb.HelpfulCount != 3 {
			t.Errorf("winner did not inherit the loser's feedback: ok=%v fb=%+v", ok, fb)
		}
	}
}

// TestApplySemanticDedupe_RemovesLoserWhenBackendAllows covers the clean path:
// on a backend with no residual dependent blocking the delete, the losing row is
// actually gone, the winner holds the union of evidence, and no error is
// returned.
func TestApplySemanticDedupe_RemovesLoserWhenBackendAllows(t *testing.T) {
	ctx := context.Background()
	conn, err := store.Open(ctx, "memory://")
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	seedMergeClaim(t, conn, "cl_win")
	seedMergeClaim(t, conn, "cl_lose")

	merged, err := ApplySemanticDedupe(ctx, conn, SemanticDedupePlan{
		Merges: []SemanticMerge{{WinnerID: "cl_win", DuplicateIDs: []string{"cl_lose"}}},
	})
	if err != nil {
		t.Fatalf("ApplySemanticDedupe: %v", err)
	}
	if merged != 1 {
		t.Errorf("merged = %d, want 1", merged)
	}
	survivors, err := conn.Claims.ListByIDs(ctx, []string{"cl_lose"})
	if err != nil {
		t.Fatalf("ListByIDs: %v", err)
	}
	if len(survivors) != 0 {
		t.Errorf("loser survived a clean merge: %+v", survivors)
	}
	if events := evidenceEventsFor(t, conn, "cl_win"); !containsAll(events, "ev_cl_win", "ev_cl_lose") {
		t.Errorf("winner evidence = %v, want the union", events)
	}
}

func containsAll(got []string, want ...string) bool {
	set := make(map[string]struct{}, len(got))
	for _, g := range got {
		set[g] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[w]; !ok {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
