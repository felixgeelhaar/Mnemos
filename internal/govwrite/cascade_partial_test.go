package govwrite_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/ports"
	"go.klarlabs.de/mnemos/internal/store"
	_ "go.klarlabs.de/mnemos/internal/store/memory"
)

// errFinalDelete is the fault the tests inject on the LAST step of the
// cascade — the one that removes the claim row — after every dependent row
// has already been committed away by the earlier steps.
var errFinalDelete = errors.New("injected: backend refused the claim delete")

// faultyClaims decorates a real ClaimRepository and fails DeleteCascade while
// the deletes that run before it succeed. That is the exact interleaving a
// cross-repository cascade cannot rule out: relationships and the embedding are
// already gone (each repository call commits independently) when the final
// delete errors.
//
// failUntil lets a test clear the fault and re-run the delete, so the
// "idempotently resumable" half of the contract is exercised too.
type faultyClaims struct {
	ports.ClaimRepository
	failing atomic.Bool
	calls   atomic.Int32
}

func (f *faultyClaims) DeleteCascade(ctx context.Context, claimID string) error {
	f.calls.Add(1)
	if f.failing.Load() {
		return errFinalDelete
	}
	return f.ClaimRepository.DeleteCascade(ctx, claimID)
}

// newFaultyWriter opens an in-memory store, wraps its ClaimRepository in the
// fault injector, and hands back a governed Writer over the decorated conn.
func newFaultyWriter(t *testing.T) (*govwrite.Writer, *faultyClaims) {
	t.Helper()
	conn, err := store.Open(context.Background(), "memory://")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	faulty := &faultyClaims{ClaimRepository: conn.Claims}
	conn.Claims = faulty

	w, err := govwrite.Wrap(conn, nil)
	if err != nil {
		t.Fatalf("govwrite.Wrap: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, faulty
}

// seedCascadeTarget writes a claim with an evidence link, a relationship and an
// embedding, so the cascade has every kind of dependent row to strip.
func seedCascadeTarget(t *testing.T, w *govwrite.Writer, id string) {
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
		ID: id, Text: "seed " + id, Type: domain.ClaimTypeFact, Confidence: 0.9,
		Status: domain.ClaimStatusActive, CreatedAt: now, ValidFrom: now, CreatedBy: "tester",
	}}, govwrite.ClaimReason{}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if _, err := w.EvidenceLinks(ctx, []domain.ClaimEvidence{{ClaimID: id, EventID: "ev_" + id}}); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}
	if _, err := w.Relationships(ctx, []domain.Relationship{{
		ID: "rel_" + id, Type: domain.RelationshipTypeSupports,
		FromClaimID: id, ToClaimID: "cl_other", CreatedAt: now, CreatedBy: "tester",
	}}); err != nil {
		t.Fatalf("seed rel: %v", err)
	}
	if err := w.Embedding(ctx, id, "claim", []float32{0.1, 0.2}, "m", "tester"); err != nil {
		t.Fatalf("seed embedding: %v", err)
	}
}

// TestDeleteClaimCascade_FinalDeleteFails_LeavesNoStrippedActiveClaim pins the
// N2 interleaving. With the fault injected on the final delete, the cascade's
// earlier steps have already committed: the claim's relationships and embedding
// are gone. Before the tombstone-first fix, what survived was an ACTIVE claim
// with no evidence and no edges — an unsupported belief that still scored and
// that recall could still surface.
//
// The contract now: nothing active survives, the surviving row is a deprecated
// tombstone carrying the audit reason, and the caller gets a named
// PartialDeleteError instead of a bare failure.
func TestDeleteClaimCascade_FinalDeleteFails_LeavesNoStrippedActiveClaim(t *testing.T) {
	t.Parallel()
	w, faulty := newFaultyWriter(t)
	ctx := context.Background()
	seedCascadeTarget(t, w, "cl_partial")

	faulty.failing.Store(true)
	err := w.DeleteClaimCascade(ctx, "cl_partial")
	if err == nil {
		t.Fatal("DeleteClaimCascade must not report success when the claim row survives")
	}

	// The partial state is surfaced, not silent.
	if !errors.Is(err, govwrite.ErrStrippedClaimSurvives) {
		t.Errorf("error should match ErrStrippedClaimSurvives, got %v", err)
	}
	var partial *govwrite.PartialDeleteError
	if !errors.As(err, &partial) {
		t.Fatalf("error should be a *PartialDeleteError, got %T: %v", err, err)
	}
	if partial.ClaimID != "cl_partial" {
		t.Errorf("PartialDeleteError.ClaimID = %q, want cl_partial", partial.ClaimID)
	}
	if !partial.Tombstoned {
		t.Error("PartialDeleteError.Tombstoned should be true — the row was deprecated before stripping")
	}

	// The surviving row must NOT be an active, un-evidenced belief.
	got, err := w.Conn().Claims.ListByIDs(ctx, []string{"cl_partial"})
	if err != nil {
		t.Fatalf("ListByIDs: %v", err)
	}
	if len(got) == 1 && got[0].Status == domain.ClaimStatusActive {
		t.Fatalf("STRIPPED CLAIM SURVIVED ACTIVE: %+v — its evidence and edges are gone but recall can still surface it", got[0])
	}
	if len(got) == 1 && got[0].Status != domain.ClaimStatusDeprecated {
		t.Errorf("surviving row status = %q, want deprecated", got[0].Status)
	}

	// The tombstone is auditable: the status transition carries the reason.
	history, err := w.Conn().Claims.ListStatusHistoryByClaimID(ctx, "cl_partial")
	if err != nil {
		t.Fatalf("ListStatusHistoryByClaimID: %v", err)
	}
	foundReason := false
	for _, h := range history {
		if h.Reason == govwrite.TombstoneReason {
			foundReason = true
		}
	}
	if !foundReason {
		t.Errorf("tombstone reason %q missing from status history %+v", govwrite.TombstoneReason, history)
	}
}

// TestDeleteClaimCascade_ResumesAfterFaultClears is the other half of the
// contract: the cascade is idempotent, so re-running it once the backend
// recovers finishes the delete rather than needing manual repair.
func TestDeleteClaimCascade_ResumesAfterFaultClears(t *testing.T) {
	t.Parallel()
	w, faulty := newFaultyWriter(t)
	ctx := context.Background()
	seedCascadeTarget(t, w, "cl_resume")

	faulty.failing.Store(true)
	if err := w.DeleteClaimCascade(ctx, "cl_resume"); err == nil {
		t.Fatal("first delete should fail with the fault injected")
	}

	faulty.failing.Store(false)
	if err := w.DeleteClaimCascade(ctx, "cl_resume"); err != nil {
		t.Fatalf("resumed delete should succeed: %v", err)
	}
	got, err := w.Conn().Claims.ListByIDs(ctx, []string{"cl_resume"})
	if err != nil {
		t.Fatalf("ListByIDs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("claim survived the resumed cascade: %+v", got)
	}
}

// TestDeleteClaimCascade_TombstoneFailureLeavesClaimIntact covers the inverse:
// when the tombstone itself cannot be written, nothing has been stripped yet,
// so the cascade must abort with the claim whole rather than press on and
// strip a claim it could not retire.
func TestDeleteClaimCascade_TombstoneFailureLeavesClaimIntact(t *testing.T) {
	t.Parallel()
	conn, err := store.Open(context.Background(), "memory://")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	blocked := &tombstoneBlockingClaims{ClaimRepository: conn.Claims}
	conn.Claims = blocked
	w, err := govwrite.Wrap(conn, nil)
	if err != nil {
		t.Fatalf("govwrite.Wrap: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	ctx := context.Background()
	seedCascadeTarget(t, w, "cl_notomb")
	blocked.blocking.Store(true)

	if err := w.DeleteClaimCascade(ctx, "cl_notomb"); err == nil {
		t.Fatal("delete should fail when the tombstone cannot be written")
	}
	// Nothing stripped: the relationship is still there.
	rels, err := conn.Relationships.ListByClaim(ctx, "cl_notomb")
	if err != nil {
		t.Fatalf("ListByClaim: %v", err)
	}
	if len(rels) == 0 {
		t.Error("relationships were stripped even though the claim could not be tombstoned")
	}
}

// tombstoneBlockingClaims fails the deprecating upsert the cascade writes
// first, leaving the claim un-retired.
type tombstoneBlockingClaims struct {
	ports.ClaimRepository
	blocking atomic.Bool
}

func (b *tombstoneBlockingClaims) UpsertWithReasonAs(ctx context.Context, claims []domain.Claim, reason, changedBy string) error {
	if b.blocking.Load() && reason == govwrite.TombstoneReason {
		return errors.New("injected: tombstone write refused")
	}
	return b.ClaimRepository.UpsertWithReasonAs(ctx, claims, reason, changedBy)
}
