package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/store"
)

// countingQuerier wraps the repository's database handle and counts the
// statements it sends. An N+1 loop and a batched query return identical data,
// so round-trip count is the only observable that distinguishes them.
type countingQuerier struct {
	inner   pgQuerier
	queries atomic.Int64
}

func (c *countingQuerier) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return c.inner.ExecContext(ctx, q, args...)
}

func (c *countingQuerier) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	c.queries.Add(1)
	return c.inner.QueryContext(ctx, q, args...)
}

func (c *countingQuerier) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	c.queries.Add(1)
	return c.inner.QueryRowContext(ctx, q, args...)
}

func (c *countingQuerier) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return c.inner.BeginTx(ctx, opts)
}

// Listing lessons must cost a constant number of round trips. The lesson
// corpus only grows (synthesis adds rows, nothing removes them), so an O(L)
// hydration loop makes every `query_lessons` call slower than the last.
func TestPostgres_ListAllLessons_HydratesEvidenceWithoutNPlusOne(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres integration test")
	}
	ns := fmt.Sprintf("mnemos_test_%d", time.Now().UnixNano())
	full := dsn + "?namespace=" + ns
	if strings.Contains(dsn, "?") {
		full = dsn + "&namespace=" + ns
	}
	ctx := context.Background()
	conn, err := store.Open(ctx, full)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	raw, ok := conn.Raw.(*sql.DB)
	if !ok {
		t.Fatalf("postgres Conn.Raw is %T, want *sql.DB", conn.Raw)
	}
	t.Cleanup(func() {
		_, _ = raw.ExecContext(context.Background(), fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", ns))
		_ = conn.Close()
	})

	const lessons, evidencePer = 25, 2
	at := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	for i := range lessons {
		evidence := make([]string, 0, evidencePer)
		for j := range evidencePer {
			id := fmt.Sprintf("act-%d-%d", i, j)
			if err := conn.Actions.Append(ctx, domain.Action{
				ID: id, RunID: "run-1", Kind: domain.ActionKindDeploy,
				Subject: "billing", Actor: "ci", At: at,
			}); err != nil {
				t.Fatalf("append action: %v", err)
			}
			evidence = append(evidence, id)
		}
		if err := conn.Lessons.Append(ctx, domain.Lesson{
			ID: fmt.Sprintf("lesson-%02d", i), Statement: fmt.Sprintf("lesson %d holds", i),
			Scope: domain.Scope{Service: "billing"}, Trigger: "deploy-fails",
			Kind: "operational", Confidence: 0.7, DerivedAt: at, Evidence: evidence,
		}); err != nil {
			t.Fatalf("append lesson: %v", err)
		}
	}
	// A lesson row whose evidence is gone must still hydrate to an empty slice.
	if _, err := raw.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO %s (id, statement, scope_service, trigger, kind, confidence, derived_at)
VALUES ('lesson-bare', 'no evidence yet', 'billing', 'deploy-fails', 'operational', 0.5, $1)`,
		qualify(ns, "lessons")), at); err != nil {
		t.Fatalf("insert bare lesson: %v", err)
	}

	counting := &countingQuerier{inner: raw}
	repo := LessonRepository{db: counting, ns: ns}

	got, err := repo.ListAll(ctx)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(got) != lessons+1 {
		t.Fatalf("got %d lessons, want %d", len(got), lessons+1)
	}
	for _, l := range got {
		if l.Evidence == nil {
			t.Errorf("lesson %s has nil Evidence; want an empty slice", l.ID)
		}
		want := evidencePer
		if l.ID == "lesson-bare" {
			want = 0
		}
		if len(l.Evidence) != want {
			t.Errorf("lesson %s hydrated %d evidence ids, want %d", l.ID, len(l.Evidence), want)
		}
	}
	if q := counting.queries.Load(); q > 2 {
		t.Errorf("ListAll issued %d queries for %d lessons; want <= 2 (N+1 hydration is back)", q, len(got))
	}
}
