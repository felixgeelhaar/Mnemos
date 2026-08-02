package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/extract"
	"go.klarlabs.de/mnemos/internal/store"
)

// halfLifeTestTime is a fixed timestamp; nothing under test reads it, but
// Claim.Validate and the store both want a real created_at.
func halfLifeTestTime() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

// volatileClaim is a text the shipped classifier is confident about; if this
// ever stops classifying as volatile the tests below become vacuous, so
// TestHalfLifeBackfill_FixtureIsActuallyVolatile pins it.
const volatileClaim = "postgres is running on port 5432"

// durableClaim carries a state verb AND a rationale, which the classifier
// vetoes — the control that an over-eager backfill would fail.
const durableClaim = "We chose Postgres because the write volume outgrew SQLite"

func TestHalfLifeBackfill_FixtureIsActuallyVolatile(t *testing.T) {
	if got := extract.HalfLifeFor("fact", volatileClaim); got != extract.VolatileHalfLifeDays {
		t.Fatalf("fixture no longer classifies as volatile: HalfLifeFor = %v", got)
	}
	if got := extract.HalfLifeFor("fact", durableClaim); got != 0 {
		t.Fatalf("control fixture classifies as volatile: HalfLifeFor = %v", got)
	}
}

// The core contract of the backfill: a stored claim that was written before the
// write path persisted the column (half_life_days = 0) and whose text the
// classifier judges volatile gets its real half-life.
func TestHalfLifeBackfill_FillsZeroValuedVolatileClaim(t *testing.T) {
	plan := planHalfLifeBackfill([]domain.Claim{
		{ID: "c1", Text: volatileClaim, Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive},
	})

	if len(plan.Changes) != 1 {
		t.Fatalf("planned %d change(s), want 1: %+v", len(plan.Changes), plan.Changes)
	}
	if got := plan.Changes[0].HalfLifeDays; got != extract.VolatileHalfLifeDays {
		t.Errorf("half_life_days = %v, want %v", got, extract.VolatileHalfLifeDays)
	}
	if plan.ByHalfLife[extract.VolatileHalfLifeDays] != 1 {
		t.Errorf("distribution = %v, want one 14-day entry", plan.ByHalfLife)
	}
}

// The regression #334's ON CONFLICT rule exists to prevent: a non-zero value is
// either a real classification or a MarkVerified override, and either way this
// pass must not touch it. Note the override here is on text the classifier WOULD
// otherwise stamp — the value being different from the classifier's answer is
// exactly what makes it an override worth protecting.
func TestHalfLifeBackfill_LeavesExistingValueAlone(t *testing.T) {
	claims := []domain.Claim{
		{ID: "override", Text: volatileClaim, Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive, HalfLifeDays: 3},
		{ID: "already", Text: volatileClaim, Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive, HalfLifeDays: extract.VolatileHalfLifeDays},
	}

	plan := planHalfLifeBackfill(claims)

	if len(plan.Changes) != 0 {
		t.Fatalf("planned %d change(s) over already-valued claims, want 0: %+v", len(plan.Changes), plan.Changes)
	}
	if plan.AlreadySet != 2 {
		t.Errorf("AlreadySet = %d, want 2", plan.AlreadySet)
	}
}

// The classifier under-catches on purpose: mis-stamping a durable claim decays
// real knowledge out of recall invisibly. The backfill must inherit that, not
// widen it.
func TestHalfLifeBackfill_SkipsDurableClaims(t *testing.T) {
	plan := planHalfLifeBackfill([]domain.Claim{
		{ID: "c1", Text: durableClaim, Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive},
		{ID: "c2", Text: "The team stand-up is at 09:30", Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive},
	})

	if len(plan.Changes) != 0 {
		t.Fatalf("planned %d change(s) over durable claims, want 0: %+v", len(plan.Changes), plan.Changes)
	}
	if plan.Live != 2 {
		t.Errorf("Live = %d, want 2", plan.Live)
	}
}

// A decision's value is its reasoning, which survives the state it decided
// about — the classifier never shortens one, so neither does the backfill.
func TestHalfLifeBackfill_SkipsDecisions(t *testing.T) {
	plan := planHalfLifeBackfill([]domain.Claim{
		{ID: "d1", Text: volatileClaim, Type: domain.ClaimTypeDecision, Status: domain.ClaimStatusActive},
	})
	if len(plan.Changes) != 0 {
		t.Fatalf("planned a change over a decision: %+v", plan.Changes)
	}
}

// Deprecated claims are out of recall, so rewriting them costs a governed write
// each and changes nothing anyone reads. On an 88k-row brain that is most of the
// cost of the pass for none of the benefit.
func TestHalfLifeBackfill_SkipsDeprecated(t *testing.T) {
	plan := planHalfLifeBackfill([]domain.Claim{
		{ID: "gone", Text: volatileClaim, Type: domain.ClaimTypeFact, Status: domain.ClaimStatusDeprecated},
		{ID: "live", Text: volatileClaim, Type: domain.ClaimTypeFact, Status: domain.ClaimStatusContested},
	})

	if len(plan.Changes) != 1 || plan.Changes[0].ID != "live" {
		t.Fatalf("planned %+v, want only the contested claim", plan.Changes)
	}
	if plan.Live != 1 {
		t.Errorf("Live = %d, want 1 (deprecated is not live)", plan.Live)
	}
}

// One malformed legacy row must not abort a 88k-row pass: the store validates
// every claim in an upsert batch and rejects the whole transaction, so a claim
// that cannot round-trip is counted and left out rather than carried into the
// write.
func TestHalfLifeBackfill_SkipsUnwritableClaims(t *testing.T) {
	plan := planHalfLifeBackfill([]domain.Claim{
		// Volatile text, but no type — Claim.Validate rejects it.
		{ID: "broken", Text: volatileClaim, Status: domain.ClaimStatusActive},
		{ID: "ok", Text: volatileClaim, Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive},
	})

	if len(plan.Changes) != 1 || plan.Changes[0].ID != "ok" {
		t.Fatalf("planned %+v, want only the valid claim", plan.Changes)
	}
	if plan.Unwritable != 1 {
		t.Errorf("Unwritable = %d, want 1", plan.Unwritable)
	}
}

// The report has to be checkable after the fact, which means counts per
// resulting value, not one total.
func TestHalfLifeBackfill_ReportsDistribution(t *testing.T) {
	claims := []domain.Claim{
		{ID: "v1", Text: volatileClaim, Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive},
		{ID: "v2", Text: "the deploy is failing", Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive},
		{ID: "d1", Text: durableClaim, Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive},
	}

	plan := planHalfLifeBackfill(claims)

	if len(plan.ByHalfLife) != 1 {
		t.Fatalf("distribution = %v, want a single bucket", plan.ByHalfLife)
	}
	if got := plan.ByHalfLife[extract.VolatileHalfLifeDays]; got != len(plan.Changes) {
		t.Errorf("distribution bucket = %d but %d change(s) planned", got, len(plan.Changes))
	}
}

// End-to-end against a real store through the governed writer: the value must
// land in the STORE (not just on the object the plan built), and the neighbouring
// override must survive the same pass — the two halves of the contract, asserted
// on a read-back.
func TestHalfLifeBackfill_PersistsAndPreservesOverride(t *testing.T) {
	ctx := context.Background()
	_, conn := openTestStore(t)
	w := wrapTestWriter(t, conn)

	seed := []domain.Claim{
		{ID: "zero", Text: volatileClaim, Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive, Confidence: 0.9, CreatedAt: halfLifeTestTime(), CreatedBy: domain.SystemUser},
		{ID: "override", Text: volatileClaim, Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive, Confidence: 0.9, CreatedAt: halfLifeTestTime(), CreatedBy: domain.SystemUser, HalfLifeDays: 45},
		{ID: "durable", Text: durableClaim, Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive, Confidence: 0.9, CreatedAt: halfLifeTestTime(), CreatedBy: domain.SystemUser},
	}
	if err := conn.Claims.Upsert(ctx, seed); err != nil {
		t.Fatalf("seed claims: %v", err)
	}

	stored, err := conn.Claims.ListAll(ctx)
	if err != nil {
		t.Fatalf("list claims: %v", err)
	}
	plan := planHalfLifeBackfill(stored)
	if len(plan.Changes) != 1 || plan.Changes[0].ID != "zero" {
		t.Fatalf("planned %+v, want only the zero-valued volatile claim", plan.Changes)
	}

	res, err := applyHalfLifeBackfill(ctx, w, plan.Changes, 2, domain.SystemUser)
	if err != nil {
		t.Fatalf("apply backfill: %v", err)
	}
	if res.Written != 1 || res.Verified != 1 {
		t.Fatalf("apply reported written=%d verified=%d, want 1/1", res.Written, res.Verified)
	}

	after, err := conn.Claims.ListByIDs(ctx, []string{"zero", "override", "durable"})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := map[string]float64{}
	for _, c := range after {
		got[c.ID] = c.HalfLifeDays
	}
	if got["zero"] != extract.VolatileHalfLifeDays {
		t.Errorf("zero-valued claim: half_life_days = %v, want %v", got["zero"], extract.VolatileHalfLifeDays)
	}
	if got["override"] != 45 {
		t.Errorf("override clobbered: half_life_days = %v, want 45", got["override"])
	}
	if got["durable"] != 0 {
		t.Errorf("durable claim stamped: half_life_days = %v, want 0", got["durable"])
	}
}

// Batching is the whole point on a large brain: every batch must be written, not
// just the first.
func TestHalfLifeBackfill_WritesEveryBatch(t *testing.T) {
	ctx := context.Background()
	_, conn := openTestStore(t)
	w := wrapTestWriter(t, conn)

	const n = 7
	seed := make([]domain.Claim, 0, n)
	for i := range n {
		seed = append(seed, domain.Claim{
			ID:         string(rune('a' + i)),
			Text:       volatileClaim,
			Type:       domain.ClaimTypeFact,
			Status:     domain.ClaimStatusActive,
			Confidence: 0.9,
			CreatedAt:  halfLifeTestTime(),
			CreatedBy:  domain.SystemUser,
		})
	}
	if err := conn.Claims.Upsert(ctx, seed); err != nil {
		t.Fatalf("seed claims: %v", err)
	}

	stored, err := conn.Claims.ListAll(ctx)
	if err != nil {
		t.Fatalf("list claims: %v", err)
	}
	plan := planHalfLifeBackfill(stored)
	res, err := applyHalfLifeBackfill(ctx, w, plan.Changes, 2, domain.SystemUser) // 4 batches, last one short
	if err != nil {
		t.Fatalf("apply backfill: %v", err)
	}
	if res.Written != n || res.Verified != n {
		t.Fatalf("written=%d verified=%d, want %d/%d", res.Written, res.Verified, n, n)
	}

	after, err := conn.Claims.ListAll(ctx)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, c := range after {
		if c.HalfLifeDays != extract.VolatileHalfLifeDays {
			t.Fatalf("claim %s: half_life_days = %v, want %v", c.ID, c.HalfLifeDays, extract.VolatileHalfLifeDays)
		}
	}
}

// The backfill has to work on every backend the brain can live on, and #331 was
// specifically a defect that only some backends had. Postgres and MySQL hand-write
// their SQL, and #335 documents that their shared read projection is still missing
// columns — so "the pass verified its own work" is only meaningful if half_life_days
// is one of the columns those backends actually read back. This asserts exactly
// that, per backend, through the same plan → governed write → read-back path the
// command runs.
//
// memory, sqlite and local libSQL always run. Postgres and MySQL run when
// TEST_POSTGRES_DSN / TEST_MYSQL_DSN are set, mirroring the internal/store harness.
func TestHalfLifeBackfill_AcrossBackends(t *testing.T) {
	ctx := context.Background()
	ns := fmt.Sprintf("mnemos_test_%d", time.Now().UnixNano())

	dsns := []struct{ name, dsn, drop string }{
		{name: "memory", dsn: "memory://half-life-backfill"},
		{name: "sqlite", dsn: "sqlite://" + filepath.Join(t.TempDir(), "half-life.db")},
		{name: "libsql", dsn: "libsql://" + filepath.Join(t.TempDir(), "half-life-libsql.db")},
	}
	if pg := os.Getenv("TEST_POSTGRES_DSN"); pg != "" {
		dsns = append(dsns, struct{ name, dsn, drop string }{
			name: "postgres", dsn: withNamespace(pg, ns),
			drop: fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", ns),
		})
	}
	if my := os.Getenv("TEST_MYSQL_DSN"); my != "" {
		dsns = append(dsns, struct{ name, dsn, drop string }{
			name: "mysql", dsn: withNamespace(my, ns),
			drop: fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", ns),
		})
	}

	for _, d := range dsns {
		t.Run(d.name, func(t *testing.T) {
			conn, err := store.Open(ctx, d.dsn)
			if err != nil {
				t.Fatalf("open %s: %v", d.dsn, err)
			}
			t.Cleanup(func() {
				if d.drop != "" {
					if raw, ok := conn.Raw.(interface {
						ExecContext(context.Context, string, ...any) (any, error)
					}); ok {
						_, _ = raw.ExecContext(context.Background(), d.drop)
					}
				}
				_ = conn.Close()
			})
			w := wrapTestWriter(t, conn)

			seed := []domain.Claim{
				{ID: "zero", Text: volatileClaim, Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive, Confidence: 0.9, CreatedAt: halfLifeTestTime(), CreatedBy: domain.SystemUser},
				{ID: "override", Text: volatileClaim, Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive, Confidence: 0.9, CreatedAt: halfLifeTestTime(), CreatedBy: domain.SystemUser, HalfLifeDays: 45},
			}
			if err := conn.Claims.Upsert(ctx, seed); err != nil {
				t.Fatalf("seed claims: %v", err)
			}

			stored, err := conn.Claims.ListAll(ctx)
			if err != nil {
				t.Fatalf("list claims: %v", err)
			}
			plan := planHalfLifeBackfill(stored)
			if len(plan.Changes) != 1 || plan.Changes[0].ID != "zero" {
				t.Fatalf("planned %+v, want only the zero-valued claim (is the override readable on this backend?)", plan.Changes)
			}

			res, err := applyHalfLifeBackfill(ctx, w, plan.Changes, defaultHalfLifeBatch, domain.SystemUser)
			if err != nil {
				t.Fatalf("apply backfill: %v", err)
			}
			// Verified is computed from a read-back, so a backend that accepts the
			// write without persisting the column fails here rather than reporting
			// success — the failure mode that produced #331 in the first place.
			if res.Verified != 1 {
				t.Fatalf("verified=%d of written=%d — half_life_days did not read back", res.Verified, res.Written)
			}

			after, err := conn.Claims.ListByIDs(ctx, []string{"zero", "override"})
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			got := map[string]float64{}
			for _, c := range after {
				got[c.ID] = c.HalfLifeDays
			}
			if got["zero"] != extract.VolatileHalfLifeDays {
				t.Errorf("zero: half_life_days = %v, want %v", got["zero"], extract.VolatileHalfLifeDays)
			}
			if got["override"] != 45 {
				t.Errorf("override clobbered: half_life_days = %v, want 45", got["override"])
			}
		})
	}
}

// withNamespace scopes an integration DSN to a throwaway schema/database.
func withNamespace(dsn, ns string) string {
	if strings.Contains(dsn, "?") {
		return dsn + "&namespace=" + ns
	}
	return dsn + "?namespace=" + ns
}

func TestParseRecomputeHalfLifeArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    int
		wantErr bool
	}{
		{name: "default", args: nil, want: defaultHalfLifeBatch},
		{name: "space form", args: []string{"--batch", "50"}, want: 50},
		{name: "equals form", args: []string{"--batch=50"}, want: 50},
		{name: "missing value", args: []string{"--batch"}, wantErr: true},
		{name: "non-numeric", args: []string{"--batch", "lots"}, wantErr: true},
		{name: "zero", args: []string{"--batch", "0"}, wantErr: true},
		{name: "unknown flag", args: []string{"--all"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRecomputeHalfLifeArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parse(%v) = %d, want error", tt.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse(%v): %v", tt.args, err)
			}
			if got != tt.want {
				t.Errorf("parse(%v) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}
