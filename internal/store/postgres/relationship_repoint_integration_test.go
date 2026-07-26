package postgres_test

import (
	"context"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// TestPostgres_RepointEndpoint_CollisionCreatedByTheFirstUpdate is the
// regression test for the production consolidate failure:
//
//	consolidate: apply dedupe: repoint relationships cl_92f7…→cl_d515…:
//	  duplicate key value violates unique constraint "idx_relationships_unique_edge"
//	  (SQLSTATE 23505)
//
// RepointEndpoint used to pre-delete conflicting edges with two statements, one
// per endpoint, BOTH evaluated before EITHER update ran. That is unsound,
// because rewriting from_claim_id changes which rows the to_claim_id rewrite
// collides with.
//
// The minimal witness is a pair of OPPOSITE-DIRECTION edges of the same type
// between the two claims being merged — (T, new, old) and (T, old, new).
// Nothing exotic: "old supports new" and "new supports old" are both ordinary
// rows, and a prior merge produces such pairs readily.
//
// Neither pre-delete matches them. For (T, old, new) the from-side check looks
// for an existing (T, new, new); for (T, new, old) the to-side check looks for
// the same. That row does not exist yet — it is the thing the rewrite is about
// to create. So both survive, the first UPDATE turns (T, old, new) into
// (T, new, new), and the second UPDATE tries to turn (T, new, old) into
// (T, new, new) as well, colliding with the row the first UPDATE just made.
//
// Note this is unreachable by the self-loop route: domain validation rejects a
// self-referencing relationship at Upsert, so (T, old, old) cannot be stored.
// The collision has to be manufactured by the rewrite itself, which is exactly
// why evaluating both pre-deletes up front cannot see it coming.
//
// The failure was not a lost edge — it aborted the transaction, and with it the
// whole consolidation pass. Production merged nothing on every run for as long
// as one such pair existed, reporting merged:0 failed:1 and otherwise looking
// healthy.
func TestPostgres_RepointEndpoint_CollisionCreatedByTheFirstUpdate(t *testing.T) {
	conn := withConn(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := conn.Claims.Upsert(ctx, []domain.Claim{
		{ID: "old", Text: "duplicate", Type: domain.ClaimTypeFact, Confidence: 0.8, Status: domain.ClaimStatusActive, CreatedAt: now},
		{ID: "new", Text: "winner", Type: domain.ClaimTypeFact, Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: now},
	}); err != nil {
		t.Fatalf("Upsert claims: %v", err)
	}

	if err := conn.Relationships.Upsert(ctx, []domain.Relationship{
		{ID: "new-old", Type: domain.RelationshipTypeSupports, FromClaimID: "new", ToClaimID: "old", CreatedAt: now},
		{ID: "old-new", Type: domain.RelationshipTypeSupports, FromClaimID: "old", ToClaimID: "new", CreatedAt: now},
	}); err != nil {
		t.Fatalf("Upsert edges: %v", err)
	}

	// Before the fix this returned a 23505 on idx_relationships_unique_edge.
	if err := conn.Relationships.RepointEndpoint(ctx, "old", "new"); err != nil {
		t.Fatalf("RepointEndpoint must tolerate a collision the rewrite itself creates: %v", err)
	}

	rels, err := conn.Relationships.ListByClaimIDs(ctx, []string{"old", "new"})
	if err != nil {
		t.Fatalf("ListByClaimIDs: %v", err)
	}
	// Both edges collapse onto (supports, new, new) once rewritten, i.e. a
	// self-loop, and self-loops are dropped — so neither should survive, and in
	// particular nothing should still reference the merged-away claim.
	for _, r := range rels {
		if r.FromClaimID == "old" || r.ToClaimID == "old" {
			t.Errorf("edge %s still references the merged-away claim: %+v", r.ID, r)
		}
		if r.FromClaimID == r.ToClaimID {
			t.Errorf("self-loop survived the repoint: %+v", r)
		}
	}
}

// TestPostgres_RepointEndpoint_CollapsesDuplicatesLosslessly checks the ordinary
// path still behaves: edges that become duplicates of an existing edge collapse
// to exactly one, on either endpoint, and unrelated edges are rewritten intact.
// Mnemos does not distinguish duplicate edges, so collapsing loses nothing —
// but dropping BOTH copies, or leaving a dangling reference to the merged-away
// claim, would.
func TestPostgres_RepointEndpoint_CollapsesDuplicatesLosslessly(t *testing.T) {
	conn := withConn(t)
	ctx := context.Background()
	now := time.Now().UTC()

	claim := func(id string) domain.Claim {
		return domain.Claim{
			ID: id, Text: id, Type: domain.ClaimTypeFact,
			Confidence: 0.8, Status: domain.ClaimStatusActive, CreatedAt: now,
		}
	}
	if err := conn.Claims.Upsert(ctx, []domain.Claim{
		claim("old"), claim("new"), claim("z"), claim("f"), claim("solo"),
	}); err != nil {
		t.Fatalf("Upsert claims: %v", err)
	}

	if err := conn.Relationships.Upsert(ctx, []domain.Relationship{
		// from-side duplicate pair: both become (supports, new, z).
		{ID: "old-z", Type: domain.RelationshipTypeSupports, FromClaimID: "old", ToClaimID: "z", CreatedAt: now},
		{ID: "new-z", Type: domain.RelationshipTypeSupports, FromClaimID: "new", ToClaimID: "z", CreatedAt: now},
		// to-side duplicate pair: both become (supports, f, new).
		{ID: "f-old", Type: domain.RelationshipTypeSupports, FromClaimID: "f", ToClaimID: "old", CreatedAt: now},
		{ID: "f-new", Type: domain.RelationshipTypeSupports, FromClaimID: "f", ToClaimID: "new", CreatedAt: now},
		// no collision: must be rewritten, not dropped.
		{ID: "old-solo", Type: domain.RelationshipTypeContradicts, FromClaimID: "old", ToClaimID: "solo", CreatedAt: now},
	}); err != nil {
		t.Fatalf("Upsert edges: %v", err)
	}

	if err := conn.Relationships.RepointEndpoint(ctx, "old", "new"); err != nil {
		t.Fatalf("RepointEndpoint: %v", err)
	}

	rels, err := conn.Relationships.ListByClaimIDs(ctx, []string{"old", "new", "z", "f", "solo"})
	if err != nil {
		t.Fatalf("ListByClaimIDs: %v", err)
	}

	type edge struct{ typ, from, to string }
	got := map[edge]int{}
	for _, r := range rels {
		if r.FromClaimID == "old" || r.ToClaimID == "old" {
			t.Errorf("edge %s still references the merged-away claim: %+v", r.ID, r)
		}
		got[edge{string(r.Type), r.FromClaimID, r.ToClaimID}]++
	}

	for _, want := range []edge{
		{string(domain.RelationshipTypeSupports), "new", "z"},
		{string(domain.RelationshipTypeSupports), "f", "new"},
		{string(domain.RelationshipTypeContradicts), "new", "solo"},
	} {
		switch got[want] {
		case 1: // exactly what we want
		case 0:
			t.Errorf("edge %+v was lost entirely; collapsing duplicates must keep one", want)
		default:
			t.Errorf("edge %+v survived %d times; duplicates must collapse to one", want, got[want])
		}
	}
}
