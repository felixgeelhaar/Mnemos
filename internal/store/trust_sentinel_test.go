package store_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/ports"
	"go.klarlabs.de/mnemos/internal/store"
	"go.klarlabs.de/mnemos/internal/trust"

	_ "go.klarlabs.de/mnemos/internal/store/mysql"
)

// backendConn is one named backend under test. Backends whose DSN env var is
// unset are skipped, but the ones that are configured must AGREE — the point of
// the suite is cross-backend equality, not per-backend self-consistency.
type backendConn struct {
	name string
	conn *store.Conn
}

// namespacedDSN appends a per-test namespace to a live-server DSN so parallel
// runs and leftovers from a failed run cannot collide.
func namespacedDSN(dsn, ns string) string {
	if strings.Contains(dsn, "?") {
		return dsn + "&namespace=" + ns
	}
	return dsn + "?namespace=" + ns
}

func openBackends(t *testing.T) []backendConn {
	t.Helper()
	ctx := context.Background()
	ns := fmt.Sprintf("mnemos_test_%d", time.Now().UnixNano())

	dsns := []struct{ name, dsn, drop string }{
		{name: "memory", dsn: "memory://trust-sentinel"},
		{name: "sqlite", dsn: "sqlite://" + filepath.Join(t.TempDir(), "trust.db")},
	}
	if pg := os.Getenv("TEST_POSTGRES_DSN"); pg != "" {
		dsns = append(dsns, struct{ name, dsn, drop string }{
			name: "postgres", dsn: namespacedDSN(pg, ns),
			drop: fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", ns),
		})
	}
	if my := os.Getenv("TEST_MYSQL_DSN"); my != "" {
		dsns = append(dsns, struct{ name, dsn, drop string }{
			name: "mysql", dsn: namespacedDSN(my, ns),
			drop: fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", ns),
		})
	}

	out := make([]backendConn, 0, len(dsns))
	for _, d := range dsns {
		conn, err := store.Open(ctx, d.dsn)
		if err != nil {
			t.Fatalf("open %s (%s): %v", d.name, d.dsn, err)
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
		out = append(out, backendConn{name: d.name, conn: conn})
	}
	return out
}

// Every backend must hand trust.Score the SAME "no evidence" sentinel — the
// ZERO time, which freshnessFactor short-circuits to 1.0. Postgres used to
// COALESCE MAX(evidence.timestamp) to 'epoch', so an evidence-free claim
// arrived as 1970-01-01: 55 years of decay, clamped to the freshness floor.
// The same claim then scored confidence×0.3 on Postgres and confidence×1.0
// everywhere else (0.24 vs 0.80 at confidence 0.8) and fell below --min-trust
// gates and `forget --below-trust` floors on one backend only.
func TestTrustScoring_NoEvidenceSentinelAgreesAcrossBackends(t *testing.T) {
	backends := openBackends(t)
	ctx := context.Background()
	const confidence = 0.8

	type result struct {
		latest time.Time
		score  float64
	}
	got := make(map[string]result, len(backends))

	for _, b := range backends {
		claim := domain.Claim{
			ID:         "no-evidence",
			Text:       "a claim nobody has produced evidence for yet",
			Type:       domain.ClaimTypeFact,
			Confidence: confidence,
			Status:     domain.ClaimStatusActive,
			CreatedAt:  time.Now().UTC(),
		}
		if err := b.conn.Claims.Upsert(ctx, []domain.Claim{claim}); err != nil {
			t.Fatalf("%s: upsert: %v", b.name, err)
		}

		scorer, ok := b.conn.Claims.(ports.TrustScorer)
		if !ok {
			t.Fatalf("%s: claim repository is not a ports.TrustScorer", b.name)
		}
		var seen time.Time
		now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
		if _, err := scorer.RecomputeTrust(ctx, func(c float64, evidenceCount int, latestEvidence time.Time) float64 {
			seen = latestEvidence
			return trust.Score(c, evidenceCount, latestEvidence, now)
		}); err != nil {
			t.Fatalf("%s: recompute trust: %v", b.name, err)
		}

		rows, err := b.conn.Claims.ListByIDs(ctx, []string{"no-evidence"})
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: read back claim: %v (rows=%d)", b.name, err, len(rows))
		}
		got[b.name] = result{latest: seen, score: rows[0].TrustScore}

		if !seen.IsZero() {
			t.Errorf("%s: an evidence-free claim reached the scorer with latestEvidence=%s, want the zero time",
				b.name, seen)
		}
	}

	want := trust.Score(confidence, 0, time.Time{}, time.Now().UTC())
	for name, r := range got {
		if r.score != want {
			t.Errorf("%s: trust score %v, want %v (every backend must score an evidence-free claim identically)",
				name, r.score, want)
		}
	}
	t.Logf("backends exercised: %d %v", len(got), keysOf(got))
}

// With evidence present the backends must still agree — this pins that the
// NULL-handling fix did not change the WITH-evidence path.
func TestTrustScoring_WithEvidenceAgreesAcrossBackends(t *testing.T) {
	backends := openBackends(t)
	ctx := context.Background()
	evidenceAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	scores := map[string]float64{}
	for _, b := range backends {
		ev := domain.Event{
			ID: "ev-1", RunID: "run-A", SchemaVersion: "1",
			Content: "deploy succeeded", SourceInputID: "in-1",
			Timestamp: evidenceAt, IngestedAt: evidenceAt, CreatedBy: domain.SystemUser,
		}
		if err := b.conn.Events.Append(ctx, ev); err != nil {
			t.Fatalf("%s: append event: %v", b.name, err)
		}
		claim := domain.Claim{
			ID: "with-evidence", Text: "the deploy succeeded",
			Type: domain.ClaimTypeFact, Confidence: 0.8,
			Status: domain.ClaimStatusActive, CreatedAt: evidenceAt,
		}
		if err := b.conn.Claims.Upsert(ctx, []domain.Claim{claim}); err != nil {
			t.Fatalf("%s: upsert claim: %v", b.name, err)
		}
		if err := b.conn.Claims.UpsertEvidence(ctx, []domain.ClaimEvidence{
			{ClaimID: "with-evidence", EventID: "ev-1"},
		}); err != nil {
			t.Fatalf("%s: upsert evidence: %v", b.name, err)
		}

		scorer := b.conn.Claims.(ports.TrustScorer) //nolint:errcheck // asserted in the sibling test
		if _, err := scorer.RecomputeTrust(ctx, func(c float64, n int, latest time.Time) float64 {
			if latest.IsZero() {
				t.Errorf("%s: claim WITH evidence reached the scorer with a zero timestamp", b.name)
			}
			return trust.Score(c, n, latest, now)
		}); err != nil {
			t.Fatalf("%s: recompute trust: %v", b.name, err)
		}
		rows, err := b.conn.Claims.ListByIDs(ctx, []string{"with-evidence"})
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: read back claim: %v (rows=%d)", b.name, err, len(rows))
		}
		scores[b.name] = rows[0].TrustScore
	}

	want := trust.Score(0.8, 1, evidenceAt, now)
	for name, s := range scores {
		if s != want {
			t.Errorf("%s: trust score %v, want %v", name, s, want)
		}
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
