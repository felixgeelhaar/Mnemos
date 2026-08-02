package store_test

import (
	"context"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/extract"
	"go.klarlabs.de/mnemos/internal/pipeline"
)

// The INGEST path must persist the per-claim freshness half-life.
//
// pipeline.PersistArtifacts classifies every claim's volatility and stamps
// HalfLifeDays on it before writing. Every SQL backend's claim INSERT then
// dropped the column: SQLite's generated upsert listed thirty columns and not
// that one, Postgres and MySQL likewise. The value was computed, carried on the
// domain object, and thrown away at the store boundary, so all 88,498 rows of a
// real brain sat at the DEFAULT 0 and every volatile belief decayed at the
// 90-day durable default (#331).
//
// This asserts against what the STORE returns, not what the pipeline built.
// That distinction is the whole point of the test, and is exactly why the
// existing coverage missed it: MarkVerified writes half_life_days and the
// SELECT projections read it, so tests on the verification path and on
// hand-built claims passed while ingest was silently lossy. Only a read-back
// after PersistArtifacts can tell a persisted value from an in-memory one.
func TestPersistArtifacts_PersistsVolatileHalfLife(t *testing.T) {
	backends := openBackends(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	const (
		volatileID   = "c-half-life-volatile"
		volatileText = "the api gateway is running on port 8080"
		durableID    = "c-half-life-durable"
		durableText  = "we chose postgres because the write volume outgrew sqlite"
	)

	for _, b := range backends {
		event := domain.Event{
			ID: "ev-half-life", RunID: "run-half-life", SchemaVersion: "1",
			Content: volatileText, SourceInputID: "in-1",
			Timestamp: at, IngestedAt: at, CreatedBy: domain.SystemUser,
		}
		claims := []domain.Claim{
			{
				ID: volatileID, Text: volatileText, Type: domain.ClaimTypeFact,
				Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: at,
			},
			{
				ID: durableID, Text: durableText, Type: domain.ClaimTypeFact,
				Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: at,
			},
		}
		links := []domain.ClaimEvidence{
			{ClaimID: volatileID, EventID: event.ID},
			{ClaimID: durableID, EventID: event.ID},
		}

		if err := pipeline.PersistArtifacts(ctx, b.conn,
			[]domain.Event{event}, claims, links, nil); err != nil {
			t.Fatalf("%s: persist artifacts: %v", b.name, err)
		}

		stored, err := b.conn.Claims.ListByIDs(ctx, []string{volatileID, durableID})
		if err != nil {
			t.Fatalf("%s: list stored claims: %v", b.name, err)
		}
		byID := make(map[string]domain.Claim, len(stored))
		for _, c := range stored {
			byID[c.ID] = c
		}

		got, ok := byID[volatileID]
		if !ok {
			t.Fatalf("%s: volatile claim %s not stored", b.name, volatileID)
		}
		if got.HalfLifeDays != extract.VolatileHalfLifeDays {
			t.Errorf("%s: volatile claim read back with half_life_days = %v, want %v — "+
				"the ingest-path classification was dropped at the store boundary",
				b.name, got.HalfLifeDays, extract.VolatileHalfLifeDays)
		}

		// The durable claim is the control: an over-eager fix that stamped
		// every row would decay real knowledge out of recall invisibly, which
		// the classifier is deliberately built to avoid.
		durable, ok := byID[durableID]
		if !ok {
			t.Fatalf("%s: durable claim %s not stored", b.name, durableID)
		}
		if durable.HalfLifeDays != 0 {
			t.Errorf("%s: durable claim read back with half_life_days = %v, want 0 "+
				"(store default)", b.name, durable.HalfLifeDays)
		}
	}
}

// An explicit half-life already on the row must survive a re-ingest that
// carries none.
//
// The ON CONFLICT branch is a separate code path from the INSERT, and a naive
// `half_life_days = excluded.half_life_days` would be a regression rather than
// a fix: `mnemos verify` sets a per-claim override through MarkVerified, but
// the claim that comes back out of extraction on the next capture carries 0.
// Claims are re-extracted routinely from overlapping evidence, so a blind
// overwrite would wipe every human-set override within a session or two. The
// upsert therefore takes the incoming value only when it is non-zero, matching
// MarkVerified's own COALESCE semantics and the pipeline's stated rule that an
// explicit half-life always wins.
func TestPersistArtifacts_ReingestPreservesExplicitHalfLife(t *testing.T) {
	backends := openBackends(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	const (
		claimID = "c-half-life-override"
		// Durable text, so the ingest classifier stamps 0 on the re-ingest and
		// only a preserved stored value can satisfy the assertion below.
		text = "we chose postgres because the write volume outgrew sqlite"
	)
	const override = 3.0

	for _, b := range backends {
		event := domain.Event{
			ID: "ev-half-life-override", RunID: "run-half-life", SchemaVersion: "1",
			Content: text, SourceInputID: "in-1",
			Timestamp: at, IngestedAt: at, CreatedBy: domain.SystemUser,
		}
		claim := domain.Claim{
			ID: claimID, Text: text, Type: domain.ClaimTypeFact,
			Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: at,
			HalfLifeDays: override,
		}
		links := []domain.ClaimEvidence{{ClaimID: claimID, EventID: event.ID}}

		if err := pipeline.PersistArtifacts(ctx, b.conn,
			[]domain.Event{event}, []domain.Claim{claim}, links, nil); err != nil {
			t.Fatalf("%s: persist artifacts: %v", b.name, err)
		}

		// Re-ingest the same claim as extraction would hand it over: no
		// half-life at all.
		reextracted := claim
		reextracted.HalfLifeDays = 0
		if err := pipeline.PersistArtifacts(ctx, b.conn,
			[]domain.Event{event}, []domain.Claim{reextracted}, links, nil); err != nil {
			t.Fatalf("%s: re-persist artifacts: %v", b.name, err)
		}

		stored, err := b.conn.Claims.ListByIDs(ctx, []string{claimID})
		if err != nil {
			t.Fatalf("%s: list stored claims: %v", b.name, err)
		}
		if len(stored) != 1 {
			t.Fatalf("%s: got %d claims, want 1", b.name, len(stored))
		}
		if stored[0].HalfLifeDays != override {
			t.Errorf("%s: re-ingested claim read back with half_life_days = %v, want %v "+
				"(the explicit override must not be reset by a re-extraction)",
				b.name, stored[0].HalfLifeDays, override)
		}
	}
}
