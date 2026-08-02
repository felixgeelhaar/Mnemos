package sqlite

import (
	"context"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// A partial re-write must not ERASE scope, provenance or the audience tier.
//
// #345 gave Postgres and MySQL a preserve-on-zero conflict rule for these eleven
// columns and deliberately left SQLite blind, which meant the hazard remained on
// the DEFAULT backend — the one every local brain uses (#346).
//
// Nothing on the ingest path produces any of these fields, and the API writers
// that CAN hit an existing claim id (POST /v1/beliefs, gRPC WriteBeliefs) build
// a claim from a request with no scope, citation_count, last_executed or
// provenance_rationale field at all. Under a blind `= excluded.x` one such write
// silently drops a scope set by markdown import or consolidate/promote, an
// audience set deliberately through the API, and provenance nothing can
// reconstruct.
//
// #334 is the precedent that makes this concrete rather than hypothetical: a
// blind `half_life_days = excluded.half_life_days` reset a MarkVerified override
// every time the claim came back through ingest, because re-extraction supplies
// a zero.
//
// The assertion is on what the STORE returns after the second write — a
// write-path test is exactly what passed throughout #331 and #335.
func TestSQLite_PartialRewriteKeepsScopeProvenanceAndVisibility(t *testing.T) {
	db := openTestDB(t)
	repo := NewClaimRepository(db)
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	executed := time.Date(2026, 7, 25, 8, 15, 0, 0, time.UTC)
	const claimID = "cl_partial_rewrite"

	full := domain.Claim{
		ID: claimID, Text: "the billing API retries idempotently on 5xx",
		Type: domain.ClaimTypeFact, Confidence: 0.9,
		Status: domain.ClaimStatusActive, CreatedAt: at,
		Scope:               domain.Scope{Service: "billing-api", Env: "prod", Team: "payments"},
		SourceDocument:      "docs/runbooks/billing.md",
		SourceType:          domain.SourceTypeDocument,
		SourceAuthority:     0.75,
		Liveness:            domain.LivenessLive,
		LastExecuted:        executed,
		CitationCount:       4,
		ProvenanceRationale: "3 sources agree, source is live, recent",
		Visibility:          domain.VisibilityPersonal,
	}
	if err := repo.Upsert(ctx, []domain.Claim{full}); err != nil {
		t.Fatalf("upsert full claim: %v", err)
	}

	// The partial writer: same id, revised text, every enrichment field at its
	// zero value — exactly the shape POST /v1/beliefs produces.
	partial := domain.Claim{
		ID: claimID, Text: "the billing API retries idempotently on 5xx and 429",
		Type: domain.ClaimTypeFact, Confidence: 0.95,
		Status: domain.ClaimStatusActive, CreatedAt: at,
	}
	if err := repo.Upsert(ctx, []domain.Claim{partial}); err != nil {
		t.Fatalf("upsert partial claim: %v", err)
	}

	stored, err := repo.ListByIDs(ctx, []string{claimID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("got %d claims, want 1", len(stored))
	}
	got := stored[0]

	// The fields the partial write DID carry must win — the rule preserves what
	// is absent, it does not freeze the row.
	if got.Text != partial.Text {
		t.Errorf("text = %q, want the rewritten %q", got.Text, partial.Text)
	}
	if got.Confidence != partial.Confidence {
		t.Errorf("confidence = %v, want the rewritten %v", got.Confidence, partial.Confidence)
	}

	if got.Scope != full.Scope {
		t.Errorf("scope = %+v after a partial rewrite, want %+v preserved — "+
			"a write that cannot express scope erased it", got.Scope, full.Scope)
	}
	if got.SourceDocument != full.SourceDocument {
		t.Errorf("source_document = %q, want %q preserved", got.SourceDocument, full.SourceDocument)
	}
	if got.SourceType != full.SourceType {
		t.Errorf("source_type = %q, want %q preserved", got.SourceType, full.SourceType)
	}
	if got.SourceAuthority != full.SourceAuthority {
		t.Errorf("source_authority = %v, want %v preserved", got.SourceAuthority, full.SourceAuthority)
	}
	if got.Liveness != full.Liveness {
		t.Errorf("liveness = %q, want %q preserved", got.Liveness, full.Liveness)
	}
	if !got.LastExecuted.Equal(full.LastExecuted) {
		t.Errorf("last_executed = %v, want %v preserved — a cleared execution "+
			"record makes trust.EvaluateLiveness report the claim as never run",
			got.LastExecuted, full.LastExecuted)
	}
	if got.CitationCount != full.CitationCount {
		t.Errorf("citation_count = %d, want %d preserved", got.CitationCount, full.CitationCount)
	}
	if got.ProvenanceRationale != full.ProvenanceRationale {
		t.Errorf("provenance_rationale = %q, want %q preserved", got.ProvenanceRationale, full.ProvenanceRationale)
	}
	if got.Visibility != full.Visibility {
		t.Errorf("visibility = %q, want %q preserved — a partial write widened the "+
			"audience of a belief curated as personal", got.Visibility, full.Visibility)
	}
}

// An EXPLICIT value must still override a stored one.
//
// The other half of the rule, and the reason it is "incoming wins when non-zero"
// rather than "first write wins": preserving on absence must not make a value
// uncorrectable. Without this, moving a belief from personal to team — or
// correcting a wrong scope — would be impossible through the write path.
func TestSQLite_ExplicitValueStillOverridesStored(t *testing.T) {
	db := openTestDB(t)
	repo := NewClaimRepository(db)
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	const claimID = "cl_explicit_override"

	first := domain.Claim{
		ID: claimID, Text: "checkout uses the v1 pricing table",
		Type: domain.ClaimTypeFact, Confidence: 0.8,
		Status: domain.ClaimStatusActive, CreatedAt: at,
		Scope:           domain.Scope{Service: "checkout", Env: "staging", Team: "growth"},
		SourceAuthority: 0.4,
		CitationCount:   1,
		Visibility:      domain.VisibilityPersonal,
	}
	if err := repo.Upsert(ctx, []domain.Claim{first}); err != nil {
		t.Fatalf("upsert first: %v", err)
	}

	second := first
	second.Scope = domain.Scope{Service: "checkout", Env: "prod", Team: "payments"}
	second.SourceAuthority = 0.9
	second.CitationCount = 7
	second.Visibility = domain.VisibilityOrg
	if err := repo.Upsert(ctx, []domain.Claim{second}); err != nil {
		t.Fatalf("upsert second: %v", err)
	}

	stored, err := repo.ListByIDs(ctx, []string{claimID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("got %d claims, want 1", len(stored))
	}
	got := stored[0]

	if got.Scope != second.Scope {
		t.Errorf("scope = %+v, want the corrected %+v — preserve-on-zero must not "+
			"make a value uncorrectable", got.Scope, second.Scope)
	}
	if got.SourceAuthority != second.SourceAuthority {
		t.Errorf("source_authority = %v, want %v", got.SourceAuthority, second.SourceAuthority)
	}
	if got.CitationCount != second.CitationCount {
		t.Errorf("citation_count = %d, want %d", got.CitationCount, second.CitationCount)
	}
	if got.Visibility != second.Visibility {
		t.Errorf("visibility = %q, want %q", got.Visibility, second.Visibility)
	}
}

// An unset visibility must still READ as the default.
//
// The write path now binds visibility raw so the conflict rule can distinguish
// "unset" from "explicitly team". That is only safe because reads normalise: a
// row stored with an empty audience has to present as domain.DefaultVisibility,
// or every claim written before this change would start reading as untiered.
func TestSQLite_UnsetVisibilityReadsAsTheDefault(t *testing.T) {
	db := openTestDB(t)
	repo := NewClaimRepository(db)
	ctx := context.Background()

	c := domain.Claim{
		ID: "cl_no_visibility", Text: "the nightly job runs at 03:00 UTC",
		Type: domain.ClaimTypeFact, Confidence: 0.7,
		Status: domain.ClaimStatusActive, CreatedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := repo.Upsert(ctx, []domain.Claim{c}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	stored, err := repo.ListByIDs(ctx, []string{c.ID})
	if err != nil || len(stored) != 1 {
		t.Fatalf("list: %v (%d rows)", err, len(stored))
	}
	if stored[0].Visibility != domain.DefaultVisibility {
		t.Errorf("visibility = %q, want %q — binding raw on write is only safe "+
			"while reads normalise", stored[0].Visibility, domain.DefaultVisibility)
	}
}
