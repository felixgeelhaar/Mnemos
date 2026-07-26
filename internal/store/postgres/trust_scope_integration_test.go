package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/ports"
	"go.klarlabs.de/mnemos/internal/store"
)

// scopedScorer asserts the postgres claim repository carries the
// ports.ScopedTrustScorer capability. Without it PersistArtifacts silently falls
// back to the full-store RecomputeTrust and a write's cost grows with the brain
// — the failure mode this capability exists to remove.
func scopedScorer(t *testing.T, conn *store.Conn) ports.ScopedTrustScorer {
	t.Helper()
	scoped, ok := conn.Claims.(ports.ScopedTrustScorer)
	if !ok {
		t.Fatal("postgres ClaimRepository does not satisfy ports.ScopedTrustScorer")
	}
	return scoped
}

func seedClaims(t *testing.T, ctx context.Context, conn *store.Conn, ids ...string) {
	t.Helper()
	claims := make([]domain.Claim, 0, len(ids))
	for _, id := range ids {
		claims = append(claims, domain.Claim{
			ID:         id,
			Text:       "claim " + id,
			Type:       domain.ClaimTypeFact,
			Confidence: 0.8,
			Status:     domain.ClaimStatusActive,
			CreatedAt:  time.Now().UTC(),
		})
	}
	if err := conn.Claims.Upsert(ctx, claims); err != nil {
		t.Fatalf("seed claims %v: %v", ids, err)
	}
}

func trustOf(t *testing.T, ctx context.Context, conn *store.Conn, id string) float64 {
	t.Helper()
	got, err := conn.Claims.ListByIDs(ctx, []string{id})
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	if len(got) != 1 {
		t.Fatalf("claim %s missing", id)
	}
	return got[0].TrustScore
}

// The scoped recompute must rescore exactly the named claims and leave every
// other claim untouched — otherwise it is not a safe substitute for the full
// pass.
func TestPostgres_RecomputeTrustForClaims_OnlyTouchesNamedClaims(t *testing.T) {
	conn := withConn(t)
	ctx := context.Background()
	scoped := scopedScorer(t, conn)

	seedClaims(t, ctx, conn, "a", "b", "c")
	if _, err := conn.Claims.(ports.TrustScorer).RecomputeTrust(ctx, func(float64, int, time.Time) float64 { return 0.10 }); err != nil {
		t.Fatalf("baseline recompute: %v", err)
	}

	n, err := scoped.RecomputeTrustForClaims(ctx, []string{"b"}, func(float64, int, time.Time) float64 { return 0.90 })
	if err != nil {
		t.Fatalf("scoped recompute: %v", err)
	}
	if n != 1 {
		t.Errorf("rescored %d claims, want 1", n)
	}
	for id, want := range map[string]float64{"a": 0.10, "b": 0.90, "c": 0.10} {
		if got := trustOf(t, ctx, conn, id); got != want {
			t.Errorf("claim %s trust = %v, want %v", id, got, want)
		}
	}
}

// An empty id list must be a no-op, not a full-store rescore — that would
// silently reintroduce the cost this exists to remove.
func TestPostgres_RecomputeTrustForClaims_EmptyIsNoOp(t *testing.T) {
	conn := withConn(t)
	ctx := context.Background()
	scoped := scopedScorer(t, conn)

	seedClaims(t, ctx, conn, "a")
	if _, err := conn.Claims.(ports.TrustScorer).RecomputeTrust(ctx, func(float64, int, time.Time) float64 { return 0.25 }); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	n, err := scoped.RecomputeTrustForClaims(ctx, nil, func(float64, int, time.Time) float64 { return 0.99 })
	if err != nil {
		t.Fatalf("scoped recompute: %v", err)
	}
	if n != 0 {
		t.Errorf("rescored %d claims for an empty list, want 0", n)
	}
	if got := trustOf(t, ctx, conn, "a"); got != 0.25 {
		t.Errorf("empty scope rescored a claim: trust = %v, want 0.25", got)
	}
}

// A claim can be deleted between the write and the rescore, so unknown ids must
// be skipped rather than failing the write.
func TestPostgres_RecomputeTrustForClaims_UnknownIDsSkipped(t *testing.T) {
	conn := withConn(t)
	ctx := context.Background()
	scoped := scopedScorer(t, conn)

	seedClaims(t, ctx, conn, "a")
	n, err := scoped.RecomputeTrustForClaims(ctx, []string{"a", "deleted"}, func(float64, int, time.Time) float64 { return 0.5 })
	if err != nil {
		t.Fatalf("unknown id caused an error: %v", err)
	}
	if n != 1 {
		t.Errorf("rescored %d, want 1", n)
	}
}

// Scoped and full recompute must agree on the claims they both cover, evidence
// aggregates included; a divergence would mean the fast path scores differently
// from the audit path.
func TestPostgres_RecomputeTrustForClaims_MatchesFullRecompute(t *testing.T) {
	conn := withConn(t)
	ctx := context.Background()
	scoped := scopedScorer(t, conn)
	full := conn.Claims.(ports.TrustScorer)
	now := time.Now().UTC()

	// "a" is corroborated by two independent authors, "b" by one author twice,
	// "c" by nothing at all — so the graded evidence count differs per claim and
	// the two paths have something to disagree about.
	for i, author := range []string{"alice", "bob", "dave", "dave"} {
		if err := conn.Events.Append(ctx, domain.Event{
			ID: fmt.Sprintf("ev%d", i), RunID: "r", SchemaVersion: "1", Content: "c",
			SourceInputID: "in", Timestamp: now, IngestedAt: now, CreatedBy: author,
		}); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}
	seedClaims(t, ctx, conn, "a", "b", "c")
	if err := conn.Claims.UpsertEvidence(ctx, []domain.ClaimEvidence{
		{ClaimID: "a", EventID: "ev0"}, {ClaimID: "a", EventID: "ev1"},
		{ClaimID: "b", EventID: "ev2"}, {ClaimID: "b", EventID: "ev3"},
	}); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}

	score := func(conf float64, n int, latest time.Time) float64 {
		return conf/2 + float64(n)/10 + float64(latest.Unix()%7)/100
	}
	if _, err := full.RecomputeTrust(ctx, score); err != nil {
		t.Fatalf("full: %v", err)
	}
	want := map[string]float64{
		"a": trustOf(t, ctx, conn, "a"),
		"b": trustOf(t, ctx, conn, "b"),
		"c": trustOf(t, ctx, conn, "c"),
	}

	// Reset, then take the scoped path over the same claims.
	if _, err := full.RecomputeTrust(ctx, func(float64, int, time.Time) float64 { return 0 }); err != nil {
		t.Fatalf("reset: %v", err)
	}
	n, err := scoped.RecomputeTrustForClaims(ctx, []string{"a", "b", "c"}, score)
	if err != nil {
		t.Fatalf("scoped: %v", err)
	}
	if n != 3 {
		t.Errorf("scoped rescored %d claims, want 3", n)
	}
	for id, w := range want {
		if got := trustOf(t, ctx, conn, id); got != w {
			t.Errorf("claim %s: scoped=%v full=%v — the two paths disagree", id, got, w)
		}
	}
}

// The scoped rescore must go through the same tenant-pinned connection as every
// other read, so ADR-0007 row-level security still bounds it: rescoring in one
// tenant must not touch an identically-named claim in another. A hand-built
// WHERE that reached across the namespace would show up here.
func TestPostgres_RecomputeTrustForClaims_RespectsTenantRLS(t *testing.T) {
	dsn := requireLiveDSN(t)
	ctx := context.Background()
	ns := fmt.Sprintf("mnemos_trustiso_%d", time.Now().UnixNano())

	open := func(t *testing.T, tenant string) *store.Conn {
		t.Helper()
		full := dsn
		sep := "?"
		if contains(full, "?") {
			sep = "&"
		}
		full += sep + "namespace=" + ns
		if tenant != "" {
			full += "&tenant=" + tenant
		}
		conn, err := store.Open(ctx, full)
		if err != nil {
			t.Fatalf("store.Open(tenant=%q): %v", tenant, err)
		}
		return conn
	}

	base := open(t, "")
	t.Cleanup(func() {
		if raw, ok := base.Raw.(interface {
			ExecContext(context.Context, string, ...any) (any, error)
		}); ok {
			_, _ = raw.ExecContext(context.Background(), fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", ns))
		}
		_ = base.Close()
	})
	if r, ok := base.Raw.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}); ok {
		var bypass bool
		if err := r.QueryRowContext(ctx, "SELECT rolbypassrls OR rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&bypass); err == nil && bypass {
			t.Skip("connecting role bypasses RLS (superuser/BYPASSRLS); isolation is unenforceable — use a non-superuser role")
		}
	}

	a := open(t, "acme")
	defer func() { _ = a.Close() }()
	b := open(t, "globex")
	defer func() { _ = b.Close() }()

	// The claims primary key is the id alone, so the two tenants hold distinct
	// ids; tenant A then deliberately NAMES tenant B's id in its scoped rescore.
	// RLS must make that row invisible to the aggregate and to the UPDATE.
	seedClaims(t, ctx, a, "claim-acme")
	seedClaims(t, ctx, b, "claim-globex")
	if _, err := b.Claims.(ports.TrustScorer).RecomputeTrust(ctx, func(float64, int, time.Time) float64 { return 0.11 }); err != nil {
		t.Fatalf("tenant B baseline: %v", err)
	}

	n, err := scopedScorer(t, a).RecomputeTrustForClaims(ctx, []string{"claim-acme", "claim-globex"}, func(float64, int, time.Time) float64 { return 0.99 })
	if err != nil {
		t.Fatalf("tenant A scoped recompute: %v", err)
	}
	if n != 1 {
		t.Errorf("tenant A rescored %d claims, want 1 (its own row only)", n)
	}
	if got := trustOf(t, ctx, b, "claim-globex"); got != 0.11 {
		t.Fatalf("CROSS-TENANT LEAK: tenant A's scoped rescore changed tenant B's claim: trust = %v, want 0.11", got)
	}
	if got := trustOf(t, ctx, a, "claim-acme"); got != 0.99 {
		t.Errorf("tenant A trust = %v, want 0.99", got)
	}
}

// The id set travels as one text[] bind parameter, so a large slice must not
// trip a parameter-count limit.
func TestPostgres_RecomputeTrustForClaims_LargeIDSet(t *testing.T) {
	conn := withConn(t)
	ctx := context.Background()
	scoped := scopedScorer(t, conn)

	seedClaims(t, ctx, conn, "a")
	ids := []string{"a"}
	for i := 0; i < 5000; i++ {
		ids = append(ids, fmt.Sprintf("absent-%d", i))
	}
	n, err := scoped.RecomputeTrustForClaims(ctx, ids, func(float64, int, time.Time) float64 { return 0.42 })
	if err != nil {
		t.Fatalf("large id set: %v", err)
	}
	if n != 1 {
		t.Errorf("rescored %d, want 1", n)
	}
	if got := trustOf(t, ctx, conn, "a"); got != 0.42 {
		t.Errorf("trust = %v, want 0.42", got)
	}
}
