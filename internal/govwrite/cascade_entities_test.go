package govwrite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/store"
	_ "go.klarlabs.de/mnemos/internal/store/sqlite"
)

// newSQLiteWriter opens a throwaway SQLite brain and wraps it in a governed
// writer.
//
// SQLite and not memory:// on purpose. The defect under test is a FOREIGN KEY
// abort, and the in-memory backend enforces no foreign keys — the same test
// there passes whether or not the fix exists, which is worse than no test:
// it reports coverage it does not have.
func newSQLiteWriter(t *testing.T) (*govwrite.Writer, *store.Conn) {
	t.Helper()

	dsn := "sqlite://" + filepath.Join(t.TempDir(), "brain.db")
	conn, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", dsn, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	w, err := govwrite.Wrap(conn, nil)
	if err != nil {
		t.Fatalf("govwrite.Wrap: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	return w, conn
}

// seedClaimWithEntity writes a claim, its source event and evidence link, then
// mentions an entity from it — the shape ingestion actually produces, since
// extraction links entities for every claim it emits.
func seedClaimWithEntity(t *testing.T, w *govwrite.Writer, conn *store.Conn, id string) domain.Entity {
	t.Helper()

	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := w.Events(ctx, []domain.Event{{
		ID: "ev_" + id, Content: "source for " + id, SourceInputID: "src_" + id,
		Timestamp: now, IngestedAt: now, CreatedBy: "tester",
	}}); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := w.Claims(ctx, []domain.Claim{{
		ID: id, Text: "checkout-api owns the payment retry budget", Type: domain.ClaimTypeFact,
		Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: now, ValidFrom: now,
		CreatedBy: "tester",
	}}, govwrite.ClaimReason{}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if _, err := w.EvidenceLinks(ctx, []domain.ClaimEvidence{{ClaimID: id, EventID: "ev_" + id}}); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}

	ent, err := conn.Entities.FindOrCreate(ctx, "checkout-api", domain.EntityTypeProject, "tester")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if err := conn.Entities.LinkClaim(ctx, id, ent.ID, "subject"); err != nil {
		t.Fatalf("seed entity link: %v", err)
	}

	return ent
}

// TestDeleteClaimCascade_UnlinksEntities is the regression for the `forget`
// path's half of the claim_entities gap.
//
// # WHY THIS EXISTS
//
// claim_entities has a foreign key to claims on every FK-enforcing backend,
// and ClaimRepository.DeleteCascade does not own that table — it is another
// aggregate's, reachable only through EntityRepository.UnlinkClaim. Semantic
// dedupe hit this and was fixed; cascadeDeleteClaim, which backs `forget` and
// belief deprecation, still walked past it.
//
// The failure is not a clean error. The cascade tombstones the claim first,
// then strips relationships and the embedding, and only then aborts on the
// foreign key — so forgetting a claim that mentions an entity left a
// deprecated, evidence-stripped row behind and returned a PartialDeleteError.
// Every claim extraction produced carries entity links, so this was the
// common case, not an edge one.
func TestDeleteClaimCascade_UnlinksEntities(t *testing.T) {
	ctx := context.Background()
	w, conn := newSQLiteWriter(t)

	const id = "cl_entity_cascade"
	seedClaimWithEntity(t, w, conn, id)

	// Guard the guard: if the seed did not actually create a link, the test
	// below passes without exercising the foreign key at all.
	if ents, _, err := conn.Entities.ListEntitiesForClaim(ctx, id); err != nil {
		t.Fatalf("read entity links: %v", err)
	} else if len(ents) != 1 {
		t.Fatalf("seed produced %d entity links, want 1 — the FK this test exists for is not engaged", len(ents))
	}

	if err := w.DeleteClaimCascade(ctx, id); err != nil {
		t.Fatalf("DeleteClaimCascade: %v\n\nThis is the defect: the claim mentions an entity, "+
			"claim_entities has an FK to claims, and the cascade never unlinked it.", err)
	}

	// The claim row is gone.
	if got, err := conn.Claims.ListByIDs(ctx, []string{id}); err != nil {
		t.Fatalf("read claim after delete: %v", err)
	} else if len(got) != 0 {
		t.Errorf("claim survived the cascade with status %q — a forgotten claim that still "+
			"exists can still be recalled", got[0].Status)
	}

	// The link row is gone with it, not orphaned at a claim that no longer exists.
	if ents, _, err := conn.Entities.ListEntitiesForClaim(ctx, id); err != nil {
		t.Fatalf("read entity links after delete: %v", err)
	} else if len(ents) != 0 {
		t.Errorf("%d claim_entities row(s) survived pointing at a deleted claim", len(ents))
	}

	// The entity itself must NOT be deleted. It is a shared aggregate; other
	// claims mention it. Cascading into it would be a far worse bug than the
	// one being fixed.
	if _, ok, err := conn.Entities.FindByName(ctx, "checkout-api"); err != nil {
		t.Fatalf("read entity after delete: %v", err)
	} else if !ok {
		t.Error("the entity was deleted along with the claim — entities are shared, " +
			"unlinking one claim must not remove them")
	}
}

// TestDeleteClaimCascade_UnlinkIsIdempotent covers the resumed-delete path: a
// cascade that already unlinked, then failed later, must converge on retry
// rather than erroring because there is nothing left to unlink.
func TestDeleteClaimCascade_UnlinkIsIdempotent(t *testing.T) {
	ctx := context.Background()
	w, conn := newSQLiteWriter(t)

	const id = "cl_unlink_twice"
	seedClaimWithEntity(t, w, conn, id)

	if err := conn.Entities.UnlinkClaim(ctx, id); err != nil {
		t.Fatalf("first unlink: %v", err)
	}
	if err := conn.Entities.UnlinkClaim(ctx, id); err != nil {
		t.Fatalf("second unlink must be a no-op, got: %v", err)
	}
	if err := w.DeleteClaimCascade(ctx, id); err != nil {
		t.Fatalf("cascade after manual unlink: %v", err)
	}
}
