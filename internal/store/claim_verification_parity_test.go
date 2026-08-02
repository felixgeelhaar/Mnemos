package store_test

import (
	"context"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// Verification state must SURVIVE a round trip on every backend.
//
// `claimColumnNames` — by its own comment "the ONE projection every claim read
// uses" — omitted `last_verified` and `verify_count` on Postgres and MySQL
// (#335). Both columns were written correctly by MarkVerified and simply never
// selected, so every read handed the application a zero LastVerified and a zero
// VerifyCount whatever the table held.
//
// That is not a cosmetic loss. `trust.EffectiveExecutionTime` feeds the
// credibility recency signal from LastVerified, and VerifyCount feeds
// `trust.SalienceOf`, whose salienceProtectFloor is what exempts a consequential
// belief from forgetStaleClaims. With both pinned at zero, a hosted brain
// decays and FORGETS beliefs that a local SQLite brain keeps — the storage
// backend acting as a cognitive parameter.
//
// The assertion is deliberately on what the STORE returns rather than on the
// call's own arguments: the write path was correct and tested throughout, which
// is exactly why the defect survived. Only a read-back can tell a persisted
// value from a written one.
func TestMarkVerified_ReadsBackAcrossBackends(t *testing.T) {
	backends := openBackends(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	verifiedAt := time.Date(2026, 7, 20, 9, 30, 0, 0, time.UTC)

	const claimID = "c-verification-parity"

	for _, b := range backends {
		claim := domain.Claim{
			ID: claimID, Text: "the deploy pipeline runs on self-hosted runners",
			Type: domain.ClaimTypeFact, Confidence: 0.9,
			Status: domain.ClaimStatusActive, CreatedAt: at,
		}
		if err := b.conn.Claims.Upsert(ctx, []domain.Claim{claim}); err != nil {
			t.Fatalf("%s: upsert: %v", b.name, err)
		}

		// Freshly ingested and never verified: the zero time is the
		// cross-backend "never verified" sentinel, and a backend that
		// invented an instant here would score recency wrongly in the
		// other direction.
		before, err := b.conn.Claims.ListByIDs(ctx, []string{claimID})
		if err != nil {
			t.Fatalf("%s: list before verify: %v", b.name, err)
		}
		if len(before) != 1 {
			t.Fatalf("%s: got %d claims before verify, want 1", b.name, len(before))
		}
		if !before[0].LastVerified.IsZero() {
			t.Errorf("%s: unverified claim read back with last_verified = %v, want the zero time",
				b.name, before[0].LastVerified)
		}
		if before[0].VerifyCount != 0 {
			t.Errorf("%s: unverified claim read back with verify_count = %d, want 0",
				b.name, before[0].VerifyCount)
		}

		if err := b.conn.Claims.MarkVerified(ctx, claimID, verifiedAt, 0); err != nil {
			t.Fatalf("%s: mark verified: %v", b.name, err)
		}
		if err := b.conn.Claims.MarkVerified(ctx, claimID, verifiedAt, 0); err != nil {
			t.Fatalf("%s: mark verified twice: %v", b.name, err)
		}

		stored, err := b.conn.Claims.ListByIDs(ctx, []string{claimID})
		if err != nil {
			t.Fatalf("%s: list after verify: %v", b.name, err)
		}
		if len(stored) != 1 {
			t.Fatalf("%s: got %d claims after verify, want 1", b.name, len(stored))
		}
		got := stored[0]

		if !got.LastVerified.Equal(verifiedAt) {
			t.Errorf("%s: claim read back with last_verified = %v, want %v — "+
				"the verification timestamp is written but not selected, so "+
				"recall never refreshes trust on this backend",
				b.name, got.LastVerified, verifiedAt)
		}
		// Two verifications, so a projection that selected the wrong column
		// (or a scan that read a constant) cannot pass by accident.
		if got.VerifyCount != 2 {
			t.Errorf("%s: claim read back with verify_count = %d, want 2 — "+
				"the salience floor that protects a belief from being forgotten "+
				"is unreachable through the verification channel on this backend",
				b.name, got.VerifyCount)
		}
	}
}

// Curation must be visible on the backend that stores it.
//
// MySQL declared `lifecycle`, and SetLifecycle wrote it, and the projection
// never selected it — so promoting a belief there succeeded and every
// subsequent read reported it as uncurated. Found by the same projection guard
// that closes #335, and the identical shape: a correct write path with no
// reader, which no write-path test can detect.
func TestSetLifecycle_ReadsBackAcrossBackends(t *testing.T) {
	backends := openBackends(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	const claimID = "c-lifecycle-parity"

	for _, b := range backends {
		claim := domain.Claim{
			ID: claimID, Text: "retries use exponential backoff with jitter",
			Type: domain.ClaimTypeFact, Confidence: 0.9,
			Status: domain.ClaimStatusActive, CreatedAt: at,
		}
		if err := b.conn.Claims.Upsert(ctx, []domain.Claim{claim}); err != nil {
			t.Fatalf("%s: upsert: %v", b.name, err)
		}
		if err := b.conn.Claims.SetLifecycle(ctx, claimID, domain.ClaimLifecyclePromoted); err != nil {
			t.Fatalf("%s: set lifecycle: %v", b.name, err)
		}
		stored, err := b.conn.Claims.ListByIDs(ctx, []string{claimID})
		if err != nil {
			t.Fatalf("%s: list: %v", b.name, err)
		}
		if len(stored) != 1 {
			t.Fatalf("%s: got %d claims, want 1", b.name, len(stored))
		}
		if stored[0].Lifecycle != domain.ClaimLifecyclePromoted {
			t.Errorf("%s: claim read back with lifecycle = %q, want %q — "+
				"curation is written but not selected on this backend",
				b.name, stored[0].Lifecycle, domain.ClaimLifecyclePromoted)
		}
	}
}

// A per-claim half-life override set through MarkVerified must read back too.
//
// Same projection, same failure mode, and the one column of the three that
// #334 had already added — so this pins the fix rather than re-finding it, and
// keeps the three verification-path columns asserted together.
func TestMarkVerified_HalfLifeOverrideReadsBackAcrossBackends(t *testing.T) {
	backends := openBackends(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	const claimID = "c-verification-half-life"
	const override = 7.0

	for _, b := range backends {
		claim := domain.Claim{
			ID: claimID, Text: "the staging certificate rotates weekly",
			Type: domain.ClaimTypeFact, Confidence: 0.8,
			Status: domain.ClaimStatusActive, CreatedAt: at,
		}
		if err := b.conn.Claims.Upsert(ctx, []domain.Claim{claim}); err != nil {
			t.Fatalf("%s: upsert: %v", b.name, err)
		}
		if err := b.conn.Claims.MarkVerified(ctx, claimID, at, override); err != nil {
			t.Fatalf("%s: mark verified: %v", b.name, err)
		}
		stored, err := b.conn.Claims.ListByIDs(ctx, []string{claimID})
		if err != nil {
			t.Fatalf("%s: list: %v", b.name, err)
		}
		if len(stored) != 1 {
			t.Fatalf("%s: got %d claims, want 1", b.name, len(stored))
		}
		if stored[0].HalfLifeDays != override {
			t.Errorf("%s: claim read back with half_life_days = %v, want %v",
				b.name, stored[0].HalfLifeDays, override)
		}
	}
}
