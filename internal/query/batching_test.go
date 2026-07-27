package query

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// Two N+1s on the main recall path. Both called a batch API one element at a
// time inside a loop:
//
//   - collectContradictions did repo.ListByClaim(ctx, claim.ID) per claim, twice
//     per recall and AFTER hop expansion, so 100–300 serialised round trips was
//     normal;
//   - inhibitLosers did claims.ListByIDs(ctx, []string{id}) per verdict.
//
// These tests count round trips rather than asserting on output, because the
// output was already correct — the cost was the bug.

// countingRelationshipRepo wraps fakeRelationshipRepo and counts calls.
type countingRelationshipRepo struct {
	fakeRelationshipRepo
	mu           sync.Mutex
	byClaimCalls int
	batchCalls   int
}

func (r *countingRelationshipRepo) ListByClaim(ctx context.Context, claimID string) ([]domain.Relationship, error) {
	r.mu.Lock()
	r.byClaimCalls++
	r.mu.Unlock()

	return r.fakeRelationshipRepo.ListByClaim(ctx, claimID)
}

func (r *countingRelationshipRepo) ListByClaimIDs(ctx context.Context, claimIDs []string) ([]domain.Relationship, error) {
	r.mu.Lock()
	r.batchCalls++
	r.mu.Unlock()

	return r.fakeRelationshipRepo.ListByClaimIDs(ctx, claimIDs)
}

func TestCollectContradictions_BatchesInOneRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	claims := make([]domain.Claim, 0, 12)
	edges := map[string][]domain.Relationship{}
	for i := 0; i < 12; i++ {
		id := "cl_" + string(rune('a'+i))
		claims = append(claims, domain.Claim{
			ID: id, Text: id + " content", Type: domain.ClaimTypeFact,
			Status: domain.ClaimStatusActive, Confidence: 0.8, CreatedAt: now,
		})
	}
	edge("rel_contra", domain.RelationshipTypeContradicts, claims[0].ID, claims[1].ID, edges)
	edge("rel_supports", domain.RelationshipTypeSupports, claims[2].ID, claims[3].ID, edges)

	repo := &countingRelationshipRepo{fakeRelationshipRepo: fakeRelationshipRepo{rels: edges}}

	got, err := collectContradictions(context.Background(), repo, claims)
	if err != nil {
		t.Fatalf("collectContradictions: %v", err)
	}
	if len(got) != 1 || got[0].ID != "rel_contra" {
		t.Fatalf("contradictions = %+v, want exactly rel_contra", got)
	}
	if repo.byClaimCalls != 0 {
		t.Errorf("per-claim ListByClaim called %d times — the N+1 is back", repo.byClaimCalls)
	}
	if repo.batchCalls != 1 {
		t.Errorf("ListByClaimIDs called %d times, want exactly 1 for %d claims", repo.batchCalls, len(claims))
	}
}

func TestCollectContradictions_EmptyClaimSetMakesNoQuery(t *testing.T) {
	repo := &countingRelationshipRepo{fakeRelationshipRepo: fakeRelationshipRepo{rels: map[string][]domain.Relationship{}}}
	got, err := collectContradictions(context.Background(), repo, nil)
	if err != nil {
		t.Fatalf("collectContradictions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want no contradictions, got %+v", got)
	}
	if repo.batchCalls != 0 || repo.byClaimCalls != 0 {
		t.Errorf("an empty claim set must not hit the store (batch=%d, per-claim=%d)", repo.batchCalls, repo.byClaimCalls)
	}
}

// countingClaimRepo counts ListByIDs calls and records the batch sizes.
type countingClaimRepo struct {
	fakeClaimRepo
	mu         sync.Mutex
	listCalls  int
	batchSizes []int
}

func (r *countingClaimRepo) ListByIDs(ctx context.Context, ids []string) ([]domain.Claim, error) {
	r.mu.Lock()
	r.listCalls++
	r.batchSizes = append(r.batchSizes, len(ids))
	r.mu.Unlock()

	return r.fakeClaimRepo.ListByIDs(ctx, ids)
}

func TestInhibitLosers_FetchesLosersInOneBatch(t *testing.T) {
	now := time.Now().UTC()
	claims := []domain.Claim{
		{ID: "cl_loser_1", Text: "one", Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive, CreatedAt: now},
		{ID: "cl_loser_2", Text: "two", Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive, CreatedAt: now},
		{ID: "cl_loser_3", Text: "three", Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive, CreatedAt: now},
	}
	writes := map[string]map[string]float64{}
	repo := &countingClaimRepo{fakeClaimRepo: fakeClaimRepo{claims: claims, creditWrites: &writes}}
	engine := NewEngine(fakeEventRepo{}, repo, fakeRelationshipRepo{rels: map[string][]domain.Relationship{}})

	ans := domain.Answer{Verdicts: []domain.Verdict{
		{WinnerClaimID: "cl_w", LoserClaimID: "cl_loser_1"},
		{WinnerClaimID: "cl_w", LoserClaimID: "cl_loser_2"},
		{WinnerClaimID: "cl_w", LoserClaimID: "cl_loser_3"},
		{WinnerClaimID: "cl_w", LoserClaimID: ""}, // escalation: no decisive loser
	}}
	engine.inhibitLosers(context.Background(), AnswerOptions{Inhibit: true}, ans)

	if repo.listCalls != 1 {
		t.Errorf("ListByIDs called %d times for 3 losers, want 1 (batch sizes %v)", repo.listCalls, repo.batchSizes)
	}
	if len(repo.batchSizes) == 1 && repo.batchSizes[0] != 3 {
		t.Errorf("batch size = %d, want 3", repo.batchSizes[0])
	}
	for _, id := range []string{"cl_loser_1", "cl_loser_2", "cl_loser_3"} {
		if writes[id][domain.InhibitionComponentKey] <= 0 {
			t.Errorf("%s should have been suppressed; writes=%v", id, writes)
		}
	}
}

func TestInhibitLosers_NoDecisiveLoserMakesNoQuery(t *testing.T) {
	writes := map[string]map[string]float64{}
	repo := &countingClaimRepo{fakeClaimRepo: fakeClaimRepo{creditWrites: &writes}}
	engine := NewEngine(fakeEventRepo{}, repo, fakeRelationshipRepo{rels: map[string][]domain.Relationship{}})

	// Escalations only — nothing to suppress, so nothing to load.
	ans := domain.Answer{Verdicts: []domain.Verdict{{WinnerClaimID: "cl_w", LoserClaimID: ""}}}
	engine.inhibitLosers(context.Background(), AnswerOptions{Inhibit: true}, ans)

	if repo.listCalls != 0 {
		t.Errorf("escalation-only verdicts must not hit the store, got %d calls", repo.listCalls)
	}
	if len(writes) != 0 {
		t.Errorf("escalation-only verdicts must not write, got %v", writes)
	}
}
