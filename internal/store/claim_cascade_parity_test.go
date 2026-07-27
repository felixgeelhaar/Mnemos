package store_test

import (
	"context"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/pipeline"
	"go.klarlabs.de/mnemos/internal/store"
)

// claimEntityUnlinker is a byte-for-byte copy of the optional capability
// internal/pipeline/semantic_dedupe.go type-asserts for. Copying it is the
// point: the merge reaches claim_entities through a structural assertion on an
// UNEXPORTED interface, so a backend method that compiles but whose signature
// drifts (a *domain.Claim parameter, a named error return, a pointer receiver
// on a value-typed repo) silently fails the assertion and the merge quietly
// stops cleaning up. Asserting the same shape here turns that class of drift
// into a test failure instead of a behaviour change nobody notices.
type claimEntityUnlinker interface {
	UnlinkClaim(ctx context.Context, claimID string) error
}

// seedClaimWithEntityLink writes one event, one claim evidenced by it, and one
// entity linked to that claim. It returns the entity id.
func seedClaimWithEntityLink(t *testing.T, conn *store.Conn, backend, claimID, text string) string {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	eventID := "ev-" + claimID
	if err := conn.Events.Append(ctx, domain.Event{
		ID: eventID, RunID: "run-cascade", SchemaVersion: "1",
		Content: text, SourceInputID: "in-1",
		Timestamp: at, IngestedAt: at, CreatedBy: domain.SystemUser,
	}); err != nil {
		t.Fatalf("%s: append event: %v", backend, err)
	}
	if err := conn.Claims.Upsert(ctx, []domain.Claim{{
		ID: claimID, Text: text, Type: domain.ClaimTypeFact,
		Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: at,
	}}); err != nil {
		t.Fatalf("%s: upsert claim %s: %v", backend, claimID, err)
	}
	if err := conn.Claims.UpsertEvidence(ctx, []domain.ClaimEvidence{
		{ClaimID: claimID, EventID: eventID},
	}); err != nil {
		t.Fatalf("%s: upsert evidence for %s: %v", backend, claimID, err)
	}
	ent, err := conn.Entities.FindOrCreate(ctx, "Postgres", domain.EntityTypeConcept, domain.SystemUser)
	if err != nil {
		t.Fatalf("%s: find or create entity: %v", backend, err)
	}
	if err := conn.Entities.LinkClaim(ctx, claimID, ent.ID, "mention"); err != nil {
		t.Fatalf("%s: link claim %s: %v", backend, claimID, err)
	}
	return ent.ID
}

// claim_entities is the one claim dependent that used to have no port method at
// all, and it is not inert: it carries a foreign key to claims on every
// FK-enforcing backend, so a surviving link row makes DeleteCascade fail
// outright. Every backend must expose UnlinkClaim, through the PORT value (not
// the concrete type), and the row must actually be gone afterwards.
func TestEntityRepository_UnlinkClaimIsReachableThroughThePort(t *testing.T) {
	backends := openBackends(t)
	ctx := context.Background()

	for _, b := range backends {
		if _, ok := b.conn.Entities.(claimEntityUnlinker); !ok {
			t.Errorf("%s: Entities does not satisfy the shape internal/pipeline asserts for "+
				"(UnlinkClaim(context.Context, string) error) — semantic dedupe will skip the unlink", b.name)
			continue
		}

		entID := seedClaimWithEntityLink(t, b.conn, b.name, "c-unlink", "postgres enforces row level security")

		ents, _, err := b.conn.Entities.ListEntitiesForClaim(ctx, "c-unlink")
		if err != nil {
			t.Fatalf("%s: list entities for claim: %v", b.name, err)
		}
		if len(ents) != 1 || ents[0].ID != entID {
			t.Fatalf("%s: setup did not link the entity: got %d links", b.name, len(ents))
		}

		if err := b.conn.Entities.UnlinkClaim(ctx, "c-unlink"); err != nil {
			t.Fatalf("%s: UnlinkClaim: %v", b.name, err)
		}
		ents, _, err = b.conn.Entities.ListEntitiesForClaim(ctx, "c-unlink")
		if err != nil {
			t.Fatalf("%s: list entities after unlink: %v", b.name, err)
		}
		if len(ents) != 0 {
			t.Errorf("%s: %d claim_entities row(s) survived UnlinkClaim, want 0", b.name, len(ents))
		}

		// Deleting nothing is success: a resumed merge must converge.
		if err := b.conn.Entities.UnlinkClaim(ctx, "c-unlink"); err != nil {
			t.Errorf("%s: second UnlinkClaim is not idempotent: %v", b.name, err)
		}
		if err := b.conn.Entities.UnlinkClaim(ctx, "no-such-claim"); err != nil {
			t.Errorf("%s: UnlinkClaim on an unknown claim: %v", b.name, err)
		}

		// The whole reason the method exists: the claim row can now go.
		if err := b.conn.Claims.DeleteCascade(ctx, "c-unlink"); err != nil {
			t.Errorf("%s: DeleteCascade after unlink: %v", b.name, err)
		}
	}
}

// The decisive test for the unlink: drive the REAL merge and require that it
// completes. This exercises the type assertion in
// pipeline.mergeClaimInto rather than re-declaring it — with the method
// absent (or mis-signed) the assertion fails, the loser keeps its
// claim_entities row, and the final DELETE FROM claims trips the foreign key,
// so ApplySemanticDedupe returns the "remain as deprecated tombstones" error
// with the loser's evidence already moved to the winner.
//
// The memory backend has no foreign keys, so the delete would SUCCEED there
// while leaving an orphan link — which is why this also asserts the link row
// itself is gone, not just that the merge returned nil.
func TestSemanticDedupe_MergeRemovesTheLosersEntityLink(t *testing.T) {
	backends := openBackends(t)
	ctx := context.Background()

	for _, b := range backends {
		entID := seedClaimWithEntityLink(t, b.conn, b.name, "c-winner", "postgres enforces row level security")
		seedClaimWithEntityLink(t, b.conn, b.name, "c-loser", "row level security is enforced by postgres")

		merged, err := pipeline.ApplySemanticDedupe(ctx, b.conn, pipeline.SemanticDedupePlan{
			Merges: []pipeline.SemanticMerge{{
				WinnerID: "c-winner", DuplicateIDs: []string{"c-loser"}, MaxSimilarity: 0.99,
			}},
			Threshold: 0.9,
		})
		if err != nil {
			t.Errorf("%s: ApplySemanticDedupe left residue: %v", b.name, err)
		}
		if merged != 1 {
			t.Errorf("%s: merged %d claims, want 1", b.name, merged)
		}

		rows, err := b.conn.Claims.ListByIDs(ctx, []string{"c-loser"})
		if err != nil {
			t.Fatalf("%s: read back loser: %v", b.name, err)
		}
		if len(rows) != 0 {
			t.Errorf("%s: the losing claim survived the merge as a %s tombstone; "+
				"the delete could not complete", b.name, rows[0].Status)
		}

		ents, _, err := b.conn.Entities.ListEntitiesForClaim(ctx, "c-loser")
		if err != nil {
			t.Fatalf("%s: list entities for loser: %v", b.name, err)
		}
		if len(ents) != 0 {
			t.Errorf("%s: %d claim_entities row(s) still point at the deleted loser", b.name, len(ents))
		}

		// The merge is a MOVE: the winner must have inherited the mention.
		winnerEnts, _, err := b.conn.Entities.ListEntitiesForClaim(ctx, "c-winner")
		if err != nil {
			t.Fatalf("%s: list entities for winner: %v", b.name, err)
		}
		if len(winnerEnts) != 1 || winnerEnts[0].ID != entID {
			t.Errorf("%s: winner has %d entity links, want the loser's mention folded in", b.name, len(winnerEnts))
		}
	}
}

// "Delete this claim" must mean the same thing on every backend. It did not:
// Postgres cleared claim_evidence + claim_status_history only, while SQLite
// also cleared claim_versions + claim_feedback and NEITHER cleared
// claim_expectations — so the residue left behind depended on the DSN.
//
// The canonical set is claim_evidence, claim_status_history, claim_versions,
// claim_feedback, claim_expectations. A backend is checked against every
// member it actually declares (nil repo == table absent in that build), which
// is the same rule DeleteCascade implements.
func TestDeleteCascade_ClearsEveryClaimOwnedSideTableItDeclares(t *testing.T) {
	backends := openBackends(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	for _, b := range backends {
		const claimID = "c-cascade"
		seedClaimWithEntityLink(t, b.conn, b.name, claimID, "the cache eviction policy is lru")

		// claim_status_history: a status change writes an audit row.
		if err := b.conn.Claims.UpsertWithReasonAs(ctx, []domain.Claim{{
			ID: claimID, Text: "the cache eviction policy is lru", Type: domain.ClaimTypeFact,
			Confidence: 0.9, Status: domain.ClaimStatusContested, CreatedAt: at,
		}}, "cascade parity test", domain.SystemUser); err != nil {
			t.Fatalf("%s: transition claim: %v", b.name, err)
		}

		if b.conn.ClaimVersions != nil {
			if err := b.conn.ClaimVersions.Append(ctx, domain.ClaimVersion{
				ClaimID: claimID, Version: 1, Text: "the cache eviction policy is lru",
				Confidence: 0.9, Status: domain.ClaimStatusActive,
				WrittenAt: at, WrittenBy: domain.SystemUser,
			}); err != nil {
				t.Fatalf("%s: append claim version: %v", b.name, err)
			}
		}
		if b.conn.Feedback != nil {
			if err := b.conn.Feedback.Upsert(ctx, domain.ClaimFeedback{
				ClaimID: claimID, HelpfulCount: 2, LastFeedbackAt: at,
			}); err != nil {
				t.Fatalf("%s: upsert feedback: %v", b.name, err)
			}
		}
		if b.conn.Expectations != nil {
			if err := b.conn.Expectations.Upsert(ctx, domain.Expectation{
				ClaimID: claimID, Predicted: 1, Tolerance: 0.1,
				Horizon: at.Add(24 * time.Hour), CreatedAt: at,
			}); err != nil {
				t.Fatalf("%s: upsert expectation: %v", b.name, err)
			}
		}

		// claim_entities is owned by EntityRepository, not by the cascade —
		// clear it the way every caller must, so the FK cannot mask the check.
		if err := b.conn.Entities.UnlinkClaim(ctx, claimID); err != nil {
			t.Fatalf("%s: unlink entities: %v", b.name, err)
		}
		if err := b.conn.Claims.DeleteCascade(ctx, claimID); err != nil {
			t.Fatalf("%s: DeleteCascade: %v", b.name, err)
		}

		if rows, err := b.conn.Claims.ListByIDs(ctx, []string{claimID}); err != nil || len(rows) != 0 {
			t.Errorf("%s: claim row survived DeleteCascade (err=%v, rows=%d)", b.name, err, len(rows))
		}
		ev, err := b.conn.Claims.ListEvidenceByClaimIDs(ctx, []string{claimID})
		if err != nil {
			t.Fatalf("%s: list evidence: %v", b.name, err)
		}
		if len(ev) != 0 {
			t.Errorf("%s: %d claim_evidence row(s) survived DeleteCascade", b.name, len(ev))
		}
		hist, err := b.conn.Claims.ListStatusHistoryByClaimID(ctx, claimID)
		if err != nil {
			t.Fatalf("%s: list status history: %v", b.name, err)
		}
		if len(hist) != 0 {
			t.Errorf("%s: %d claim_status_history row(s) survived DeleteCascade", b.name, len(hist))
		}
		if b.conn.ClaimVersions != nil {
			vs, err := b.conn.ClaimVersions.ListByClaim(ctx, claimID)
			if err != nil {
				t.Fatalf("%s: list claim versions: %v", b.name, err)
			}
			if len(vs) != 0 {
				t.Errorf("%s: %d claim_versions row(s) survived DeleteCascade", b.name, len(vs))
			}
		}
		if b.conn.Feedback != nil {
			if _, ok, err := b.conn.Feedback.Get(ctx, claimID); err != nil {
				t.Fatalf("%s: get feedback: %v", b.name, err)
			} else if ok {
				t.Errorf("%s: the claim_feedback row survived DeleteCascade", b.name)
			}
		}
		if b.conn.Expectations != nil {
			if _, ok, err := b.conn.Expectations.Get(ctx, claimID); err != nil {
				t.Fatalf("%s: get expectation: %v", b.name, err)
			} else if ok {
				t.Errorf("%s: the claim_expectations row survived DeleteCascade", b.name)
			}
		}
		t.Logf("%s: cascade checked against side tables declared by this backend "+
			"(versions=%t feedback=%t expectations=%t)",
			b.name, b.conn.ClaimVersions != nil, b.conn.Feedback != nil, b.conn.Expectations != nil)
	}
}

// A resumed delete must converge. Every SQL backend issues its dependent
// DELETEs unconditionally, so re-running after the claim row is already gone
// still clears residue; the memory backend used to return early on a missing
// claim, making it the one backend where a crash mid-delete left orphans that
// nothing would ever collect.
func TestDeleteCascade_ClearsDependentsEvenWhenTheClaimRowIsAlreadyGone(t *testing.T) {
	backends := openBackends(t)
	ctx := context.Background()

	for _, b := range backends {
		if b.conn.Expectations == nil {
			continue
		}
		const claimID = "c-orphan"
		seedClaimWithEntityLink(t, b.conn, b.name, claimID, "the retry budget is three attempts")
		if err := b.conn.Expectations.Upsert(ctx, domain.Expectation{
			ClaimID: claimID, Predicted: 3, Tolerance: 0,
			CreatedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatalf("%s: upsert expectation: %v", b.name, err)
		}
		if err := b.conn.Entities.UnlinkClaim(ctx, claimID); err != nil {
			t.Fatalf("%s: unlink entities: %v", b.name, err)
		}
		// Simulate the interrupted state: the claim row is gone, the
		// expectation is not.
		if err := b.conn.Claims.DeleteCascade(ctx, claimID); err != nil {
			t.Fatalf("%s: first DeleteCascade: %v", b.name, err)
		}
		if err := b.conn.Expectations.Upsert(ctx, domain.Expectation{
			ClaimID: claimID, Predicted: 3,
			CreatedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatalf("%s: re-seed orphan expectation: %v", b.name, err)
		}

		if err := b.conn.Claims.DeleteCascade(ctx, claimID); err != nil {
			t.Fatalf("%s: resumed DeleteCascade: %v", b.name, err)
		}
		if _, ok, err := b.conn.Expectations.Get(ctx, claimID); err != nil {
			t.Fatalf("%s: get expectation: %v", b.name, err)
		} else if ok {
			t.Errorf("%s: a resumed DeleteCascade left the orphaned claim_expectations row behind", b.name)
		}
	}
}
