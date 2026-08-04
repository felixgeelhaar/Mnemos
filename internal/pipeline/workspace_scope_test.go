package pipeline_test

import (
	"context"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/pipeline"
	"go.klarlabs.de/mnemos/internal/store"
	_ "go.klarlabs.de/mnemos/internal/store/memory"
)

// wsFixture seeds two claims, each backed by one event carrying the given
// workspace tag (empty = untagged), and returns a contradiction between them.
func wsFixture(t *testing.T, wsA, wsB string) (*store.Conn, []domain.Relationship, map[string]struct{}) {
	t.Helper()
	ctx := context.Background()
	conn, err := store.Open(ctx, "memory://")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	at := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	mk := func(id, ws, text string) {
		md := map[string]string{}
		if ws != "" {
			md[pipeline.WorkspaceMetadataKey] = ws
		}
		ev := domain.Event{
			ID: "ev-" + id, RunID: "r", SchemaVersion: "1", Content: text,
			SourceInputID: "in-" + id, Timestamp: at, IngestedAt: at,
			CreatedBy: domain.SystemUser, Metadata: md,
		}
		if err := conn.Events.Append(ctx, ev); err != nil {
			t.Fatalf("append event: %v", err)
		}
		c := domain.Claim{
			ID: id, Text: text, Type: domain.ClaimTypeFact, Confidence: 0.9,
			Status: domain.ClaimStatusActive, CreatedAt: at, CreatedBy: domain.SystemUser,
		}
		if err := conn.Claims.Upsert(ctx, []domain.Claim{c}); err != nil {
			t.Fatalf("upsert claim: %v", err)
		}
		if err := conn.Claims.UpsertEvidence(ctx, []domain.ClaimEvidence{{ClaimID: id, EventID: ev.ID}}); err != nil {
			t.Fatalf("upsert evidence: %v", err)
		}
	}
	mk("c-a", wsA, "423 tests green")
	mk("c-b", wsB, "1290 tests, green, pushed")

	rels := []domain.Relationship{
		{ID: "r1", FromClaimID: "c-a", ToClaimID: "c-b", Type: domain.RelationshipTypeContradicts},
		{ID: "r2", FromClaimID: "c-a", ToClaimID: "c-b", Type: domain.RelationshipTypeSupports},
	}
	return conn, rels, map[string]struct{}{}
}

// The case this exists for: two projects reporting their own test counts.
//
// No overlap or numeric rule separates these — the claims genuinely share topic
// vocabulary and really are about the same KIND of thing, so every threshold
// that rejects them also starts rejecting true positives (#360, #361). Only
// provenance can.
func TestDropCrossWorkspace_DropsContradictionBetweenDifferentProjects(t *testing.T) {
	conn, rels, newIDs := wsFixture(t, "auth-go", "briefkasten")

	kept, dropped, err := pipeline.DropCrossWorkspaceContradictions(
		context.Background(), conn.Events, conn.Claims, rels, "auth-go", newIDs)
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	if len(kept) != 1 || kept[0].Type != domain.RelationshipTypeSupports {
		t.Errorf("kept = %+v, want only the supports edge — a cross-project "+
			"corroboration is real evidence and must survive", kept)
	}
}

// Two beliefs from the SAME project can still disagree, and must.
func TestDropCrossWorkspace_KeepsContradictionWithinOneProject(t *testing.T) {
	conn, rels, newIDs := wsFixture(t, "auth-go", "auth-go")

	kept, dropped, err := pipeline.DropCrossWorkspaceContradictions(
		context.Background(), conn.Events, conn.Claims, rels, "auth-go", newIDs)
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0 — a same-project conflict is genuine", dropped)
	}
	if len(kept) != 2 {
		t.Errorf("kept %d edges, want both", len(kept))
	}
}

// THE FAIL-SAFE. An unknown workspace on either side must never drop anything.
//
// Every belief ingested before capture began tagging has no workspace at all,
// which on a real brain today is all 68,670 of them. If an unknown tag counted
// as "different", this filter would silently delete genuine disagreement across
// the entire back catalogue the moment it shipped.
func TestDropCrossWorkspace_UnknownWorkspaceNeverDrops(t *testing.T) {
	for _, tc := range []struct{ name, a, b string }{
		{"both untagged", "", ""},
		{"incoming tagged, stored untagged", "auth-go", ""},
		{"stored tagged, incoming untagged", "", "briefkasten"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, rels, newIDs := wsFixture(t, tc.a, tc.b)
			_, dropped, err := pipeline.DropCrossWorkspaceContradictions(
				context.Background(), conn.Events, conn.Claims, rels, "auth-go", newIDs)
			if err != nil {
				t.Fatalf("drop: %v", err)
			}
			if dropped != 0 {
				t.Errorf("dropped %d edge(s) on an unknown workspace — the filter must "+
					"positively know both sides came from different projects", dropped)
			}
		})
	}
}

// An empty workspace argument disables the filter entirely, so every path that
// does not know its project is unaffected.
func TestDropCrossWorkspace_NoWorkspaceIsANoOp(t *testing.T) {
	conn, rels, newIDs := wsFixture(t, "auth-go", "briefkasten")

	kept, dropped, err := pipeline.DropCrossWorkspaceContradictions(
		context.Background(), conn.Events, conn.Claims, rels, "", newIDs)
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	if dropped != 0 || len(kept) != 2 {
		t.Errorf("dropped=%d kept=%d, want 0 and 2", dropped, len(kept))
	}
}
