package mysql_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/ports"
	"go.klarlabs.de/mnemos/internal/store"
)

// scopedScorer asserts the mysql claim repository carries the
// ports.ScopedTrustScorer capability. Without it PersistArtifacts silently falls
// back to the full-store RecomputeTrust and a write's cost grows with the brain
// — the failure mode this capability exists to remove.
func scopedScorer(t *testing.T, conn *store.Conn) ports.ScopedTrustScorer {
	t.Helper()
	scoped, ok := conn.Claims.(ports.ScopedTrustScorer)
	if !ok {
		t.Fatal("mysql ClaimRepository does not satisfy ports.ScopedTrustScorer")
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
			CreatedAt:  time.Now().UTC().Truncate(time.Microsecond),
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
func TestMySQL_RecomputeTrustForClaims_OnlyTouchesNamedClaims(t *testing.T) {
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
func TestMySQL_RecomputeTrustForClaims_EmptyIsNoOp(t *testing.T) {
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
func TestMySQL_RecomputeTrustForClaims_UnknownIDsSkipped(t *testing.T) {
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
func TestMySQL_RecomputeTrustForClaims_MatchesFullRecompute(t *testing.T) {
	conn := withConn(t)
	ctx := context.Background()
	scoped := scopedScorer(t, conn)
	full := conn.Claims.(ports.TrustScorer)
	now := time.Now().UTC().Truncate(time.Microsecond)

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

// MySQL binds one placeholder per id, so an id set larger than the chunk size
// must span several reads and still rescore every match exactly once — this is
// the case that would otherwise blow the 65535-placeholder statement limit.
func TestMySQL_RecomputeTrustForClaims_ChunksLargeIDSet(t *testing.T) {
	conn := withConn(t)
	ctx := context.Background()
	scoped := scopedScorer(t, conn)

	// Real claims deliberately land in different chunks (index 0 and ~2500).
	seedClaims(t, ctx, conn, "a", "b")
	ids := []string{"a"}
	for i := 0; i < 5000; i++ {
		if i == 2500 {
			ids = append(ids, "b")
		}
		ids = append(ids, fmt.Sprintf("absent-%d", i))
	}
	n, err := scoped.RecomputeTrustForClaims(ctx, ids, func(float64, int, time.Time) float64 { return 0.42 })
	if err != nil {
		t.Fatalf("large id set: %v", err)
	}
	if n != 2 {
		t.Errorf("rescored %d, want 2", n)
	}
	for _, id := range []string{"a", "b"} {
		if got := trustOf(t, ctx, conn, id); got != 0.42 {
			t.Errorf("claim %s trust = %v, want 0.42", id, got)
		}
	}
}
