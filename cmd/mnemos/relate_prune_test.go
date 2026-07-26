package main

import (
	"testing"

	"go.klarlabs.de/mnemos/internal/domain"
)

func claim(id, text string) domain.Claim { return domain.Claim{ID: id, Text: text} }

// Only contradiction edges are re-derived. Supports/causal edges include ones
// the LLM augmentation path contributed, which the rule-based engine cannot
// reproduce offline — re-deriving them would silently delete real knowledge.
func TestPartitionStaleContradictions_LeavesNonContradictionEdgesAlone(t *testing.T) {
	byID := map[string]domain.Claim{
		"a": claim("a", "Now I'll build"),
		"b": claim("b", "Frontend builds clean"),
	}
	rels := []domain.Relationship{
		{ID: "r1", Type: domain.RelationshipTypeSupports, FromClaimID: "a", ToClaimID: "b"},
		{ID: "r2", Type: domain.RelationshipTypeCauses, FromClaimID: "a", ToClaimID: "b"},
	}
	keep, dropped, orphaned := partitionStaleContradictions(rels, byID, nil)
	if len(dropped) != 0 || len(orphaned) != 0 {
		t.Fatalf("non-contradiction edges were touched: dropped=%d orphaned=%d", len(dropped), len(orphaned))
	}
	if len(keep) != 2 {
		t.Errorf("kept %d edges, want 2", len(keep))
	}
}

// The production false positive: two narration fragments the current detectors
// no longer relate.
func TestPartitionStaleContradictions_DropsStaleEdge(t *testing.T) {
	byID := map[string]domain.Claim{
		"a": claim("a", "Now the overlay"),
		"b": claim("b", "Clear picture now"),
	}
	rels := []domain.Relationship{
		{ID: "r1", Type: domain.RelationshipTypeContradicts, FromClaimID: "a", ToClaimID: "b"},
	}
	keep, dropped, _ := partitionStaleContradictions(rels, byID, nil)
	if len(dropped) != 1 {
		t.Fatalf("stale contradiction not dropped (kept %d)", len(keep))
	}
}

// A contradiction the detectors still produce must survive — pruning must not
// become "delete all contradictions".
func TestPartitionStaleContradictions_KeepsLiveContradiction(t *testing.T) {
	byID := map[string]domain.Claim{
		"a": claim("a", "the customer had 12 prior refunds"),
		"b": claim("b", "the customer had 0 prior refunds"),
	}
	rels := []domain.Relationship{
		{ID: "r1", Type: domain.RelationshipTypeContradicts, FromClaimID: "a", ToClaimID: "b"},
	}
	keep, dropped, _ := partitionStaleContradictions(rels, byID, nil)
	if len(dropped) != 0 {
		t.Error("a still-valid contradiction was pruned")
	}
	if len(keep) != 1 {
		t.Errorf("kept %d edges, want 1", len(keep))
	}
}

// An edge whose endpoint claim is gone cannot be re-derived and must not be
// restored — it would be a dangling reference.
func TestPartitionStaleContradictions_DropsOrphanedEdges(t *testing.T) {
	byID := map[string]domain.Claim{"a": claim("a", "something")}
	rels := []domain.Relationship{
		{ID: "r1", Type: domain.RelationshipTypeContradicts, FromClaimID: "a", ToClaimID: "gone"},
		{ID: "r2", Type: domain.RelationshipTypeSupports, FromClaimID: "missing", ToClaimID: "a"},
	}
	keep, _, orphaned := partitionStaleContradictions(rels, byID, nil)
	if len(orphaned) != 2 {
		t.Errorf("orphaned = %d, want 2", len(orphaned))
	}
	if len(keep) != 0 {
		t.Errorf("kept %d dangling edges", len(keep))
	}
}

// TestPartitionStaleContradictions_SameSessionIsStale pins that the pass agrees
// with the ingest path: a contradiction between two claims from one session is
// something the current pipeline would not produce, so it is stale by
// definition even though the detectors still fire on the pair in isolation.
func TestPartitionStaleContradictions_SameSessionIsStale(t *testing.T) {
	// A pair the detectors DO still flag, so the only thing that can drop it
	// is the session rule.
	a := domain.Claim{ID: "a", Text: "The verify engine is dead code"}
	b := domain.Claim{ID: "b", Text: "The verify engine is not dead code"}
	byID := map[string]domain.Claim{"a": a, "b": b}
	rel := domain.Relationship{ID: "r1", Type: domain.RelationshipTypeContradicts, FromClaimID: "a", ToClaimID: "b"}

	// Precondition: without session context this edge is live.
	if keep, dropped, _ := partitionStaleContradictions([]domain.Relationship{rel}, byID, nil); len(keep) != 1 || len(dropped) != 0 {
		t.Fatalf("precondition: detectors must still flag this pair, keep=%d dropped=%d", len(keep), len(dropped))
	}

	tests := []struct {
		name      string
		sessionOf map[string]string
		wantKept  bool
	}{
		{"same session is dropped", map[string]string{"a": "S1", "b": "S1"}, false},
		{"different sessions are kept", map[string]string{"a": "S1", "b": "S2"}, true},
		{"untagged claims are kept", map[string]string{}, true},
		{"one side untagged is kept", map[string]string{"a": "S1"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			keep, dropped, _ := partitionStaleContradictions([]domain.Relationship{rel}, byID, tc.sessionOf)
			if kept := len(keep) == 1; kept != tc.wantKept {
				t.Fatalf("kept=%v want %v (dropped=%d)", kept, tc.wantKept, len(dropped))
			}
		})
	}
}
