package store_test

import (
	"context"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// Scope must SURVIVE a round trip on every backend (#338).
//
// Postgres and MySQL both DECLARED scope_service / scope_env / scope_team and
// then named them in neither the claim upsert's INSERT list nor
// `claimColumnNames`, so `Claim.Scope` was dropped at the store boundary on
// every write and every read handed back the zero `domain.Scope{}`.
//
// That is not a cosmetic loss. `query.admission` narrows a scoped answer with
// `c.Scope.Matches(opts.Scope)`, and Matches puts the wildcards on the FILTER
// side — an empty stored scope matches no non-empty filter. So a
// `--service`/`--env`/`--team` query against a hosted brain returned NOTHING,
// silently and without an error, while the identical query on a local SQLite
// brain returned the right subset.
//
// The assertion is on what the STORE returns rather than on the write call's
// own arguments. The write path looked correct throughout #331, #335 and #338 —
// only a read-back distinguishes a persisted value from a written one.
func TestClaimScope_ReadsBackAcrossBackends(t *testing.T) {
	backends := openBackends(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	const claimID = "c-scope-parity"
	want := domain.Scope{Service: "billing-api", Env: "prod", Team: "payments"}

	for _, b := range backends {
		claim := domain.Claim{
			ID: claimID, Text: "the billing API retries idempotently on 5xx",
			Type: domain.ClaimTypeFact, Confidence: 0.9,
			Status: domain.ClaimStatusActive, CreatedAt: at,
			Scope: want,
		}
		if err := b.conn.Claims.Upsert(ctx, []domain.Claim{claim}); err != nil {
			t.Fatalf("%s: upsert: %v", b.name, err)
		}
		stored, err := b.conn.Claims.ListByIDs(ctx, []string{claimID})
		if err != nil {
			t.Fatalf("%s: list: %v", b.name, err)
		}
		if len(stored) != 1 {
			t.Fatalf("%s: got %d claims, want 1", b.name, len(stored))
		}
		if got := stored[0].Scope; got != want {
			t.Errorf("%s: claim read back with scope %+v, want %+v — "+
				"a scoped query on this backend filters against an empty scope "+
				"and returns nothing", b.name, got, want)
		}
	}
}

// Epistemic provenance must survive a round trip on every backend (#339).
//
// These seven columns existed in sql/sqlite/schema.sql and in NEITHER hosted
// backend's DDL, so no projection change could have rescued them — there was
// nothing to select. Every one read back as its zero value on a hosted brain.
//
// Two are load-bearing beyond display:
//   - last_executed feeds trust.EffectiveExecutionTime and
//     trust.EvaluateLiveness, so recall-based recency fell back to
//     ValidFrom/CreatedAt for every claim;
//   - source_authority and citation_count feed the credibility signals in
//     internal/trust, so an authoritative, well-corroborated belief scored
//     identically to an anonymous one.
func TestClaimProvenance_ReadsBackAcrossBackends(t *testing.T) {
	backends := openBackends(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	executed := time.Date(2026, 7, 25, 8, 15, 0, 0, time.UTC)

	const claimID = "c-provenance-parity"

	for _, b := range backends {
		claim := domain.Claim{
			ID: claimID, Text: "the incident runbook is still executed weekly",
			Type: domain.ClaimTypeFact, Confidence: 0.85,
			Status: domain.ClaimStatusActive, CreatedAt: at,
			SourceDocument:      "docs/runbooks/incident.md",
			SourceType:          domain.SourceTypeDocument,
			SourceAuthority:     0.75,
			Liveness:            domain.LivenessLive,
			LastExecuted:        executed,
			CitationCount:       4,
			ProvenanceRationale: "3 sources agree, source is live, recent",
		}
		if err := b.conn.Claims.Upsert(ctx, []domain.Claim{claim}); err != nil {
			t.Fatalf("%s: upsert: %v", b.name, err)
		}
		stored, err := b.conn.Claims.ListByIDs(ctx, []string{claimID})
		if err != nil {
			t.Fatalf("%s: list: %v", b.name, err)
		}
		if len(stored) != 1 {
			t.Fatalf("%s: got %d claims, want 1", b.name, len(stored))
		}
		got := stored[0]

		if got.SourceDocument != claim.SourceDocument {
			t.Errorf("%s: source_document = %q, want %q", b.name, got.SourceDocument, claim.SourceDocument)
		}
		if got.SourceType != claim.SourceType {
			t.Errorf("%s: source_type = %q, want %q", b.name, got.SourceType, claim.SourceType)
		}
		if got.SourceAuthority != claim.SourceAuthority {
			t.Errorf("%s: source_authority = %v, want %v — the credibility "+
				"authority signal is unreachable on this backend",
				b.name, got.SourceAuthority, claim.SourceAuthority)
		}
		if got.Liveness != claim.Liveness {
			t.Errorf("%s: liveness = %q, want %q", b.name, got.Liveness, claim.Liveness)
		}
		if !got.LastExecuted.Equal(executed) {
			t.Errorf("%s: last_executed = %v, want %v — trust.EffectiveExecutionTime "+
				"falls back to ValidFrom/CreatedAt for every claim on this backend",
				b.name, got.LastExecuted, executed)
		}
		if got.CitationCount != claim.CitationCount {
			t.Errorf("%s: citation_count = %d, want %d — the corroboration signal "+
				"is pinned at zero on this backend",
				b.name, got.CitationCount, claim.CitationCount)
		}
		if got.ProvenanceRationale != claim.ProvenanceRationale {
			t.Errorf("%s: provenance_rationale = %q, want %q",
				b.name, got.ProvenanceRationale, claim.ProvenanceRationale)
		}
	}
}

// A never-executed claim must read back with the ZERO time on every backend.
//
// The counterpart to the assertion above, and the reason last_executed is NULL
// rather than an epoch default on the hosted backends: trust.EvaluateLiveness
// treats a zero LastExecuted as "unknown, fall back to LastVerified/ValidFrom",
// whereas a backend that invented 1970-01-01 would report every unexecuted
// claim as dead. Postgres already shipped exactly that bug on the evidence
// timestamp (see TestTrustScoring_NoEvidenceSentinelAgreesAcrossBackends).
func TestClaimLastExecuted_UnsetIsTheZeroTimeAcrossBackends(t *testing.T) {
	backends := openBackends(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	const claimID = "c-last-executed-unset"

	for _, b := range backends {
		claim := domain.Claim{
			ID: claimID, Text: "nobody has run the archival job in years",
			Type: domain.ClaimTypeFact, Confidence: 0.6,
			Status: domain.ClaimStatusActive, CreatedAt: at,
		}
		if err := b.conn.Claims.Upsert(ctx, []domain.Claim{claim}); err != nil {
			t.Fatalf("%s: upsert: %v", b.name, err)
		}
		stored, err := b.conn.Claims.ListByIDs(ctx, []string{claimID})
		if err != nil {
			t.Fatalf("%s: list: %v", b.name, err)
		}
		if len(stored) != 1 {
			t.Fatalf("%s: got %d claims, want 1", b.name, len(stored))
		}
		if !stored[0].LastExecuted.IsZero() {
			t.Errorf("%s: never-executed claim read back with last_executed = %v, "+
				"want the zero time", b.name, stored[0].LastExecuted)
		}
	}
}

// Visibility must survive a round trip on every backend (#339).
//
// `query.admission` gates audience access on this field and treats an empty
// value as VisibilityTeam. A backend that cannot store it therefore reports
// every claim as team-visible: a claim written `personal` is invisible to a
// personal-scoped query (which admits ONLY personal claims), and a claim
// written `org` is admitted to team-tier queries that the filter would
// otherwise exclude.
func TestClaimVisibility_ReadsBackAcrossBackends(t *testing.T) {
	backends := openBackends(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		id   string
		want domain.Visibility
	}{
		{id: "c-visibility-personal", want: domain.VisibilityPersonal},
		{id: "c-visibility-org", want: domain.VisibilityOrg},
		// Explicit team, so a backend that hardcodes the default cannot pass
		// the personal/org rows by accident and this one by coincidence.
		{id: "c-visibility-team", want: domain.VisibilityTeam},
	}

	for _, b := range backends {
		for _, tc := range cases {
			claim := domain.Claim{
				ID: tc.id, Text: "a belief tiered " + string(tc.want),
				Type: domain.ClaimTypeFact, Confidence: 0.7,
				Status: domain.ClaimStatusActive, CreatedAt: at,
				Visibility: tc.want,
			}
			if err := b.conn.Claims.Upsert(ctx, []domain.Claim{claim}); err != nil {
				t.Fatalf("%s: upsert %s: %v", b.name, tc.id, err)
			}
			stored, err := b.conn.Claims.ListByIDs(ctx, []string{tc.id})
			if err != nil {
				t.Fatalf("%s: list %s: %v", b.name, tc.id, err)
			}
			if len(stored) != 1 {
				t.Fatalf("%s: got %d claims for %s, want 1", b.name, len(stored), tc.id)
			}
			if stored[0].Visibility != tc.want {
				t.Errorf("%s: %s read back with visibility %q, want %q — "+
					"the audience gate is inert on this backend",
					b.name, tc.id, stored[0].Visibility, tc.want)
			}
		}
	}
}

// An unset Visibility must read back as the DEFAULT tier, never as empty.
//
// Every backend normalises here (SQLite via the column default, memory via
// visibilityOrDefault), and the hosted backends must too: `query.admission`
// coerces an empty value to team on the way past, so an empty read is not
// wrong today — but it makes "written team" and "never set" indistinguishable
// at the store boundary, which is precisely the ambiguity that let #331/#335
// survive.
func TestClaimVisibility_UnsetReadsBackAsDefaultAcrossBackends(t *testing.T) {
	backends := openBackends(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	const claimID = "c-visibility-unset"

	for _, b := range backends {
		claim := domain.Claim{
			ID: claimID, Text: "a belief written without an explicit audience",
			Type: domain.ClaimTypeFact, Confidence: 0.7,
			Status: domain.ClaimStatusActive, CreatedAt: at,
		}
		if err := b.conn.Claims.Upsert(ctx, []domain.Claim{claim}); err != nil {
			t.Fatalf("%s: upsert: %v", b.name, err)
		}
		stored, err := b.conn.Claims.ListByIDs(ctx, []string{claimID})
		if err != nil {
			t.Fatalf("%s: list: %v", b.name, err)
		}
		if len(stored) != 1 {
			t.Fatalf("%s: got %d claims, want 1", b.name, len(stored))
		}
		if stored[0].Visibility != domain.DefaultVisibility {
			t.Errorf("%s: unset visibility read back as %q, want %q",
				b.name, stored[0].Visibility, domain.DefaultVisibility)
		}
	}
}
