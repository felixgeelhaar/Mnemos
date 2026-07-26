package sqlite

import (
	"context"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

func TestRelationshipRepositoryUpsertAndListByClaim(t *testing.T) {
	db := openTestDB(t)
	defer closeDB(db)

	claimRepo := NewClaimRepository(db)
	relRepo := NewRelationshipRepository(db)

	now := time.Date(2026, 4, 12, 16, 0, 0, 0, time.UTC)
	claims := []domain.Claim{
		{ID: "cl_a", Text: "A", Type: domain.ClaimTypeFact, Confidence: 0.8, Status: domain.ClaimStatusActive, CreatedAt: now},
		{ID: "cl_b", Text: "B", Type: domain.ClaimTypeFact, Confidence: 0.8, Status: domain.ClaimStatusActive, CreatedAt: now},
	}
	if err := claimRepo.Upsert(context.Background(), claims); err != nil {
		t.Fatalf("Upsert claims error = %v", err)
	}

	rel := domain.Relationship{
		ID:          "rl_1",
		Type:        domain.RelationshipTypeSupports,
		FromClaimID: "cl_a",
		ToClaimID:   "cl_b",
		CreatedAt:   now,
	}
	if err := relRepo.Upsert(context.Background(), []domain.Relationship{rel}); err != nil {
		t.Fatalf("Upsert relationships error = %v", err)
	}

	rels, err := relRepo.ListByClaim(context.Background(), "cl_a")
	if err != nil {
		t.Fatalf("ListByClaim error = %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("ListByClaim len = %d, want 1", len(rels))
	}
	if rels[0].Type != domain.RelationshipTypeSupports {
		t.Fatalf("ListByClaim type = %q, want supports", rels[0].Type)
	}
}

// TestRelationshipStrength_DefaultAndStrengthen covers the ADR-0015 §4 Hebbian
// strength column: new edges default to the base 1.0, ListByClaimIDs reads it back,
// and StrengthenAssociations increments only intra-set edges (either direction),
// caps at maxStrength, preserves strength across a re-Upsert, and no-ops on <2 ids.
func TestRelationshipStrength_DefaultAndStrengthen(t *testing.T) {
	db := openTestDB(t)
	defer closeDB(db)
	ctx := context.Background()
	claimRepo := NewClaimRepository(db)
	relRepo := NewRelationshipRepository(db)

	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	claims := []domain.Claim{
		{ID: "a", Text: "A", Type: domain.ClaimTypeFact, Confidence: 0.8, Status: domain.ClaimStatusActive, CreatedAt: now},
		{ID: "b", Text: "B", Type: domain.ClaimTypeFact, Confidence: 0.8, Status: domain.ClaimStatusActive, CreatedAt: now},
		{ID: "c", Text: "C", Type: domain.ClaimTypeFact, Confidence: 0.8, Status: domain.ClaimStatusActive, CreatedAt: now},
	}
	if err := claimRepo.Upsert(ctx, claims); err != nil {
		t.Fatalf("Upsert claims: %v", err)
	}
	edges := []domain.Relationship{
		{ID: "ab", Type: domain.RelationshipTypeSupports, FromClaimID: "a", ToClaimID: "b", CreatedAt: now},
		{ID: "bc", Type: domain.RelationshipTypeSupports, FromClaimID: "b", ToClaimID: "c", CreatedAt: now},
	}
	if err := relRepo.Upsert(ctx, edges); err != nil {
		t.Fatalf("Upsert edges: %v", err)
	}

	strengthOf := func(id string) float64 {
		rels, err := relRepo.ListByClaimIDs(ctx, []string{"a", "b", "c"})
		if err != nil {
			t.Fatalf("ListByClaimIDs: %v", err)
		}
		for _, r := range rels {
			if r.ID == id {
				return r.Strength
			}
		}
		t.Fatalf("edge %s not found", id)
		return 0
	}

	// Default: a fresh edge reads back at the base 1.0.
	if got := strengthOf("ab"); got != 1 {
		t.Fatalf("fresh edge strength = %v, want 1 (default)", got)
	}

	// Strengthen the {a,b} set: only edge ab qualifies (both endpoints in set); bc
	// is excluded (c not in set).
	n, err := relRepo.StrengthenAssociations(ctx, []string{"a", "b"}, 1.0, 10)
	if err != nil {
		t.Fatalf("StrengthenAssociations: %v", err)
	}
	if n != 1 {
		t.Fatalf("strengthened %d edges, want 1 (only ab)", n)
	}
	if got := strengthOf("ab"); got != 2 {
		t.Errorf("ab strength = %v, want 2", got)
	}
	if got := strengthOf("bc"); got != 1 {
		t.Errorf("bc strength = %v, want 1 (untouched)", got)
	}

	// Re-Upsert of ab must PRESERVE its accumulated strength (ON CONFLICT keeps it).
	if err := relRepo.Upsert(ctx, []domain.Relationship{edges[0]}); err != nil {
		t.Fatalf("re-Upsert ab: %v", err)
	}
	if got := strengthOf("ab"); got != 2 {
		t.Errorf("re-Upsert reset strength to %v, want preserved 2", got)
	}

	// Cap: many increments saturate at maxStrength.
	for i := 0; i < 20; i++ {
		if _, err := relRepo.StrengthenAssociations(ctx, []string{"a", "b"}, 1.0, 5); err != nil {
			t.Fatalf("strengthen loop: %v", err)
		}
	}
	if got := strengthOf("ab"); got != 5 {
		t.Errorf("capped strength = %v, want 5", got)
	}

	// No-op: fewer than two ids, or non-positive delta.
	if n, _ := relRepo.StrengthenAssociations(ctx, []string{"a"}, 1.0, 10); n != 0 {
		t.Errorf("single-id strengthen should be a no-op, got %d", n)
	}
	if n, _ := relRepo.StrengthenAssociations(ctx, []string{"a", "b"}, 0, 10); n != 0 {
		t.Errorf("zero-delta strengthen should be a no-op, got %d", n)
	}
}

// TestRelationshipStrength_Decay covers the ADR-0015 §5 consolidation decay: an
// over-base edge is pulled toward the base 1.0 by keeping the retain fraction of its
// excess, a base edge is untouched, and out-of-range retain is a no-op.
func TestRelationshipStrength_Decay(t *testing.T) {
	db := openTestDB(t)
	defer closeDB(db)
	ctx := context.Background()
	claimRepo := NewClaimRepository(db)
	relRepo := NewRelationshipRepository(db)
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)

	if err := claimRepo.Upsert(ctx, []domain.Claim{
		{ID: "a", Text: "A", Type: domain.ClaimTypeFact, Confidence: 0.8, Status: domain.ClaimStatusActive, CreatedAt: now},
		{ID: "b", Text: "B", Type: domain.ClaimTypeFact, Confidence: 0.8, Status: domain.ClaimStatusActive, CreatedAt: now},
		{ID: "c", Text: "C", Type: domain.ClaimTypeFact, Confidence: 0.8, Status: domain.ClaimStatusActive, CreatedAt: now},
	}); err != nil {
		t.Fatalf("claims: %v", err)
	}
	if err := relRepo.Upsert(ctx, []domain.Relationship{
		{ID: "ab", Type: domain.RelationshipTypeSupports, FromClaimID: "a", ToClaimID: "b", CreatedAt: now},
		{ID: "bc", Type: domain.RelationshipTypeSupports, FromClaimID: "b", ToClaimID: "c", CreatedAt: now},
	}); err != nil {
		t.Fatalf("edges: %v", err)
	}
	strengthOf := func(id string) float64 {
		rels, _ := relRepo.ListByClaimIDs(ctx, []string{"a", "b", "c"})
		for _, r := range rels {
			if r.ID == id {
				return r.Strength
			}
		}
		t.Fatalf("edge %s not found", id)
		return 0
	}
	// Raise ab to 5, leave bc at base 1.
	if _, err := relRepo.StrengthenAssociations(ctx, []string{"a", "b"}, 4, 5); err != nil {
		t.Fatalf("strengthen: %v", err)
	}
	// Decay keeping 80% of the excess: 1 + (5-1)*0.8 = 4.2.
	n, err := relRepo.DecayAssociations(ctx, 0.8)
	if err != nil {
		t.Fatalf("DecayAssociations: %v", err)
	}
	if n != 1 {
		t.Fatalf("decayed %d edges, want 1 (only the over-base ab)", n)
	}
	if got := strengthOf("ab"); got < 4.199 || got > 4.201 {
		t.Errorf("ab decayed strength = %v, want ~4.2", got)
	}
	if got := strengthOf("bc"); got != 1 {
		t.Errorf("base edge bc should be untouched, got %v", got)
	}
	// Out-of-range retain is a no-op.
	if n, _ := relRepo.DecayAssociations(ctx, 1.0); n != 0 {
		t.Errorf("retain>=1 should no-op, got %d", n)
	}
	if got := strengthOf("ab"); got < 4.199 || got > 4.201 {
		t.Errorf("no-op decay changed strength to %v", got)
	}
}

// TestRepointEndpoint_CollisionCreatedByTheFirstUpdate is the SQLite half of a
// cross-backend parity guard. The Postgres implementation had a real bug here
// (see internal/store/postgres/relationship_repoint_integration_test.go): a
// pair of opposite-direction edges between the two merging claims,
// (T, new, old) and (T, old, new), both collapse onto (T, new, new), and
// Postgres' pre-delete could not see the collision coming — it aborted the
// transaction with SQLSTATE 23505 and took the whole consolidation pass down.
//
// SQLite survives it for a different reason: UPDATE OR IGNORE tolerates the
// unique-index violation and the orphan sweep clears the leftovers. That is a
// genuine behavioural difference between the two backends, so it is worth
// pinning on this side too — otherwise a future rewrite of the SQLite path
// (e.g. dropping OR IGNORE) reintroduces the Postgres bug here silently.
func TestRepointEndpoint_CollisionCreatedByTheFirstUpdate(t *testing.T) {
	db := openTestDB(t)
	defer closeDB(db)

	ctx := context.Background()
	claimRepo := NewClaimRepository(db)
	relRepo := NewRelationshipRepository(db)
	now := time.Date(2026, 4, 12, 16, 0, 0, 0, time.UTC)

	if err := claimRepo.Upsert(ctx, []domain.Claim{
		{ID: "cl_old", Text: "duplicate", Type: domain.ClaimTypeFact, Confidence: 0.8, Status: domain.ClaimStatusActive, CreatedAt: now},
		{ID: "cl_new", Text: "winner", Type: domain.ClaimTypeFact, Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: now},
	}); err != nil {
		t.Fatalf("Upsert claims error = %v", err)
	}
	if err := relRepo.Upsert(ctx, []domain.Relationship{
		{ID: "rl_new_old", Type: domain.RelationshipTypeSupports, FromClaimID: "cl_new", ToClaimID: "cl_old", CreatedAt: now},
		{ID: "rl_old_new", Type: domain.RelationshipTypeSupports, FromClaimID: "cl_old", ToClaimID: "cl_new", CreatedAt: now},
	}); err != nil {
		t.Fatalf("Upsert relationships error = %v", err)
	}

	if err := relRepo.RepointEndpoint(ctx, "cl_old", "cl_new"); err != nil {
		t.Fatalf("RepointEndpoint must tolerate a collision the rewrite itself creates: %v", err)
	}

	rels, err := relRepo.ListByClaim(ctx, "cl_new")
	if err != nil {
		t.Fatalf("ListByClaim error = %v", err)
	}
	for _, r := range rels {
		if r.FromClaimID == "cl_old" || r.ToClaimID == "cl_old" {
			t.Errorf("edge %s still references the merged-away claim: %+v", r.ID, r)
		}
		if r.FromClaimID == r.ToClaimID {
			t.Errorf("self-loop survived the repoint: %+v", r)
		}
	}
}
