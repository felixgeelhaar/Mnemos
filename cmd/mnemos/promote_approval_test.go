package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/store"
)

// mustTime parses an RFC3339 timestamp for a fixture, panicking on a bad
// literal (the input is a constant in the test, so a failure is a typo).
func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// TestHandlePromote_RePromotionPreservesOperatorApproval is the N8 regression.
//
// GlobalSchemaID is content-addressed, so a recurring schema upserts the SAME
// global row on every pass, and the repository's upsert overwrites every column
// — status included. An operator who had approved the schema (pending → active)
// therefore had that decision silently reverted by the next
// `--promote --gate operator --apply`, even though nothing in the run
// re-examined it.
//
// The contract: a second pass refreshes the corroboration figures it actually
// recomputed and leaves the approval, and the audit fields recording it, alone.
func TestHandlePromote_RePromotionPreservesOperatorApproval(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	stmt := "rolling back a failed deploy restores service availability"

	var tenantArgs []string
	for _, name := range []string{"t1", "t2", "t3"} {
		dsn := "sqlite://" + filepath.Join(dir, name+".db")
		seedTenantLesson(t, dsn, name+"_l", stmt)
		tenantArgs = append(tenantArgs, "--tenant-dsn", dsn)
	}
	globalDSN := "sqlite://" + filepath.Join(dir, "global.db")
	promoteArgs := append([]string{"--promote", "--gate", "operator", "--apply", "--global-dsn", globalDSN}, tenantArgs...)

	// Pass 1: lands Pending under the operator gate.
	_ = captureStdout(t, func() { handlePromote(promoteArgs, Flags{}) })

	gconn, err := store.Open(ctx, globalDSN)
	if err != nil {
		t.Fatalf("open global: %v", err)
	}
	defer func() { _ = gconn.Close() }()

	pending, err := gconn.GlobalSchemas.ListByStatus(ctx, domain.GlobalSchemaStatusPending)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("want 1 pending global schema after the first pass, got %d", len(pending))
	}
	id := pending[0].ID

	// A human approves it.
	_ = captureStdout(t, func() {
		handlePromote([]string{"--promote", "approve", id, "--global-dsn", globalDSN}, Flags{})
	})
	approved, ok, err := gconn.GlobalSchemas.GetByID(ctx, id)
	if err != nil || !ok {
		t.Fatalf("get after approve: ok=%v err=%v", ok, err)
	}
	if approved.Status != domain.GlobalSchemaStatusActive {
		t.Fatalf("fixture: approve did not activate, status=%s", approved.Status)
	}

	// A fourth tenant corroborates, so the next pass has a genuine update to
	// write — the corroboration count really should move.
	fourth := "sqlite://" + filepath.Join(dir, "t4.db")
	seedTenantLesson(t, fourth, "t4_l", stmt)
	promoteArgs = append(promoteArgs, "--tenant-dsn", fourth)

	// Pass 2, same operator gate. This is the run that used to un-approve.
	_ = captureStdout(t, func() { handlePromote(promoteArgs, Flags{}) })

	after, ok, err := gconn.GlobalSchemas.GetByID(ctx, id)
	if err != nil || !ok {
		t.Fatalf("get after re-promotion: ok=%v err=%v", ok, err)
	}
	if after.Status != domain.GlobalSchemaStatusActive {
		t.Fatalf("re-promotion REVERTED the operator's approval: status=%s, want active", after.Status)
	}
	if !after.PromotedAt.Equal(approved.PromotedAt) {
		t.Errorf("re-promotion rewrote promoted_at on an approved row: %v → %v", approved.PromotedAt, after.PromotedAt)
	}
	if after.CreatedBy != approved.CreatedBy {
		t.Errorf("re-promotion rewrote created_by on an approved row: %q → %q", approved.CreatedBy, after.CreatedBy)
	}

	// The fields the pass genuinely recomputed DO move — preserving approval
	// must not freeze the record.
	if after.DistinctTenants <= approved.DistinctTenants {
		t.Errorf("corroboration count did not advance: %d → %d (want > %d)",
			approved.DistinctTenants, after.DistinctTenants, approved.DistinctTenants)
	}

	// And nothing was left dangling in pending.
	stillPending, err := gconn.GlobalSchemas.ListByStatus(ctx, domain.GlobalSchemaStatusPending)
	if err != nil {
		t.Fatalf("list pending after: %v", err)
	}
	for _, p := range stillPending {
		if p.ID == id {
			t.Error("the approved schema reappeared in the pending list")
		}
	}
}

// TestPreserveApproval covers the helper's directionality directly: an active
// prior row pins status and its audit fields; a pending prior row does not, so
// the auto gate can still activate a schema that used to need review.
func TestPreserveApproval(t *testing.T) {
	prior := domain.GlobalSchema{
		ID: "gsch_x", Status: domain.GlobalSchemaStatusActive,
		PromotedAt: mustTime("2026-01-02T03:04:05Z"), CreatedBy: "operator@example.com",
		DistinctTenants: 3, Confidence: 0.5,
	}
	next := domain.GlobalSchema{
		ID: "gsch_x", Status: domain.GlobalSchemaStatusPending,
		PromotedAt: mustTime("2026-06-07T08:09:10Z"), CreatedBy: domain.SystemUser,
		DistinctTenants: 7, Confidence: 0.9,
	}

	got := preserveApproval(next, prior)
	if got.Status != domain.GlobalSchemaStatusActive {
		t.Errorf("status = %s, want the prior approval to hold (active)", got.Status)
	}
	if !got.PromotedAt.Equal(prior.PromotedAt) {
		t.Errorf("promoted_at = %v, want the approved row's %v", got.PromotedAt, prior.PromotedAt)
	}
	if got.CreatedBy != prior.CreatedBy {
		t.Errorf("created_by = %q, want the approved row's %q", got.CreatedBy, prior.CreatedBy)
	}
	// Recomputed evidence must still come from the new record.
	if got.DistinctTenants != 7 || got.Confidence != 0.9 {
		t.Errorf("recomputed fields were frozen: tenants=%d confidence=%v", got.DistinctTenants, got.Confidence)
	}

	// A pending prior imposes nothing: the auto gate may activate it.
	priorPending := prior
	priorPending.Status = domain.GlobalSchemaStatusPending
	nextActive := next
	nextActive.Status = domain.GlobalSchemaStatusActive
	if got := preserveApproval(nextActive, priorPending); got.Status != domain.GlobalSchemaStatusActive {
		t.Errorf("a pending prior must not block activation, got %s", got.Status)
	}
	if got := preserveApproval(next, priorPending); got.Status != domain.GlobalSchemaStatusPending {
		t.Errorf("a pending prior must leave the new status alone, got %s", got.Status)
	}
}
