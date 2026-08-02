package mysql_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// A partial re-write must not ERASE scope, provenance or the audience tier.
//
// This is #334's lesson applied to the eleven columns #338/#339 added to this
// backend's write path. Nothing on the ingest path produces any of them —
// extraction sets none — and the API writers that CAN hit an existing claim id
// build a claim field-by-field from a request that has no scope, no
// citation_count, no last_executed and no provenance_rationale field at all.
// Under a blind `= VALUES(x)`, one such write would silently drop a scope set
// by markdown import or consolidate/promote, an audience set deliberately
// through the API, and provenance that nothing can reconstruct.
//
// #334 is the precedent that makes this concrete rather than hypothetical: a
// blind `half_life_days = excluded.half_life_days` reset a MarkVerified
// override every time the claim came back through ingest, because re-extraction
// supplies a zero.
//
// The assertion is on what the STORE returns after the second write.
func TestMySQL_PartialRewriteKeepsScopeProvenanceAndVisibility(t *testing.T) {
	conn := withConn(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	executed := time.Date(2026, 7, 25, 8, 15, 0, 0, time.UTC)
	const claimID = "c-partial-rewrite"

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
	if err := conn.Claims.Upsert(ctx, []domain.Claim{full}); err != nil {
		t.Fatalf("upsert full claim: %v", err)
	}

	// The partial writer: same id, revised text, and every enrichment field at
	// its zero value — exactly the shape POST /v1/beliefs produces.
	partial := domain.Claim{
		ID: claimID, Text: "the billing API retries idempotently on 5xx and 429",
		Type: domain.ClaimTypeFact, Confidence: 0.95,
		Status: domain.ClaimStatusActive, CreatedAt: at,
	}
	if err := conn.Claims.Upsert(ctx, []domain.Claim{partial}); err != nil {
		t.Fatalf("upsert partial claim: %v", err)
	}

	stored, err := conn.Claims.ListByIDs(ctx, []string{claimID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("got %d claims, want 1", len(stored))
	}
	got := stored[0]

	// The fields the partial write DID carry must win — the conflict rule
	// preserves what is absent, it does not freeze the row.
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
	if !got.LastExecuted.Equal(executed) {
		t.Errorf("last_executed = %v, want %v preserved — the recency signal "+
			"trust.EffectiveExecutionTime reads was erased", got.LastExecuted, executed)
	}
	if got.CitationCount != full.CitationCount {
		t.Errorf("citation_count = %d, want %d preserved", got.CitationCount, full.CitationCount)
	}
	if got.ProvenanceRationale != full.ProvenanceRationale {
		t.Errorf("provenance_rationale = %q, want %q preserved", got.ProvenanceRationale, full.ProvenanceRationale)
	}
	if got.Visibility != domain.VisibilityPersonal {
		t.Errorf("visibility = %q, want %q preserved — a re-ingest silently "+
			"re-tiered a personal belief", got.Visibility, domain.VisibilityPersonal)
	}
}

// An EXPLICIT value must still override a stored one.
//
// The other half of the conflict rule, and the reason it is "incoming wins when
// non-zero" rather than "first write wins": preserving on absence must not make
// a value uncorrectable. Without this, moving a belief from personal to team —
// or correcting a wrong scope — would be impossible through the only write path
// that exists.
func TestMySQL_ExplicitRewriteOverridesScopeAndVisibility(t *testing.T) {
	conn := withConn(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	const claimID = "c-explicit-rewrite"

	first := domain.Claim{
		ID: claimID, Text: "the checkout service owns the payment retry policy",
		Type: domain.ClaimTypeFact, Confidence: 0.8,
		Status: domain.ClaimStatusActive, CreatedAt: at,
		Scope:      domain.Scope{Service: "checkout", Env: "staging", Team: "payments"},
		Visibility: domain.VisibilityPersonal,
	}
	if err := conn.Claims.Upsert(ctx, []domain.Claim{first}); err != nil {
		t.Fatalf("upsert first: %v", err)
	}

	second := first
	second.Scope = domain.Scope{Service: "checkout", Env: "prod", Team: "platform"}
	second.Visibility = domain.VisibilityTeam
	if err := conn.Claims.Upsert(ctx, []domain.Claim{second}); err != nil {
		t.Fatalf("upsert second: %v", err)
	}

	stored, err := conn.Claims.ListByIDs(ctx, []string{claimID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("got %d claims, want 1", len(stored))
	}
	if stored[0].Scope != second.Scope {
		t.Errorf("scope = %+v, want the corrected %+v — the conflict rule froze "+
			"a value it should only preserve when absent", stored[0].Scope, second.Scope)
	}
	if stored[0].Visibility != domain.VisibilityTeam {
		t.Errorf("visibility = %q, want the explicit %q — a belief cannot be "+
			"moved out of the personal tier", stored[0].Visibility, domain.VisibilityTeam)
	}
}

// A row written before these columns existed must read back truthfully.
//
// The columns arrive by ALTER TABLE ... ADD COLUMN with a default. The question
// this test settles is not migration cost but MEANING: what does a pre-existing
// row now claim about itself? Empty provenance and a zero authority/citation
// count are the truthful readings of "unknown", a NULL last_executed is the
// never-executed sentinel, and 'team' is exactly what query.admission already
// coerced an absent audience to — so no historical row changes meaning by
// acquiring the column.
//
// The INSERT below names only the pre-#338 columns, which is precisely what an
// upgraded row looks like.
//
// Writing this test found a separate, worse defect on the same path, which it
// now pins: `confidence_components` is `JSON NULL` with no default here (it is
// NOT NULL DEFAULT '{}' on Postgres), so a row predating that ALTER holds SQL
// NULL — and scanClaimRow scanned it into a plain string, failing the entire
// read with "converting NULL to string is unsupported". Not a zero value: an
// error. An upgraded MySQL brain could not read any claim written before the
// column existed.
func TestMySQL_RowsPredatingTheColumnsReadAsUnknown(t *testing.T) {
	conn := withConn(t)
	ctx := context.Background()
	raw, ok := conn.Raw.(*sql.DB)
	if !ok {
		t.Fatalf("mysql Conn.Raw is %T, want *sql.DB", conn.Raw)
	}
	const claimID = "c-legacy-row"

	if _, err := raw.ExecContext(ctx, `
INSERT INTO claims (id, text, type, confidence, status, created_at, created_by)
VALUES (?, ?, 'fact', 0.7, 'active', UTC_TIMESTAMP(6), '<system>')`,
		claimID, "a belief written before the provenance columns existed"); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	stored, err := conn.Claims.ListByIDs(ctx, []string{claimID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("got %d claims, want 1", len(stored))
	}
	got := stored[0]

	if got.Scope != (domain.Scope{}) {
		t.Errorf("legacy row scope = %+v, want the empty scope", got.Scope)
	}
	if got.SourceDocument != "" || got.SourceType != "" || got.ProvenanceRationale != "" || got.Liveness != "" {
		t.Errorf("legacy row provenance = %+v, want empty strings", got)
	}
	if got.SourceAuthority != 0 || got.CitationCount != 0 {
		t.Errorf("legacy row authority/citations = %v/%d, want 0/0",
			got.SourceAuthority, got.CitationCount)
	}
	if !got.LastExecuted.IsZero() {
		t.Errorf("legacy row last_executed = %v, want the zero time — a defaulted "+
			"instant would read as decades of decay", got.LastExecuted)
	}
	if got.Visibility != domain.DefaultVisibility {
		t.Errorf("legacy row visibility = %q, want %q — the column default must "+
			"agree with what query.admission already assumed",
			got.Visibility, domain.DefaultVisibility)
	}
	// A NULL confidence_components means "the producer surfaced no
	// decomposition", NOT "every component is zero" — and above all it must not
	// make the row unreadable.
	if got.ConfidenceComponents != nil {
		t.Errorf("legacy row confidence_components = %v, want nil", got.ConfidenceComponents)
	}
}
