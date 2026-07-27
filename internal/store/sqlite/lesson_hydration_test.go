package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// queryCounter counts the SQL statements a *sql.DB actually sends to the
// driver. It is the only way to assert "one round trip, not N" from the
// outside: the results of an N+1 loop and a batched query are identical, so a
// behavioural test cannot tell them apart.
type queryCounter struct{ n atomic.Int64 }

type countingDriver struct {
	inner   driver.Driver
	counter *queryCounter
}

func (d countingDriver) Open(dsn string) (driver.Conn, error) {
	c, err := d.inner.Open(dsn)
	if err != nil {
		return nil, err
	}
	return countingConn{Conn: c, counter: d.counter}, nil
}

type countingConn struct {
	driver.Conn
	counter *queryCounter
}

func (c countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		// Fall back to the prepare path; countingStmt counts there instead.
		return nil, driver.ErrSkip
	}
	c.counter.n.Add(1)
	return q.QueryContext(ctx, query, args)
}

func (c countingConn) Prepare(query string) (driver.Stmt, error) {
	s, err := c.Conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	return countingStmt{Stmt: s, counter: c.counter}, nil
}

type countingStmt struct {
	driver.Stmt
	counter *queryCounter
}

func (s countingStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.counter.n.Add(1)
	return s.Stmt.Query(args) //nolint:staticcheck // SA1019: driver fallback path
}

// openCountingDB bootstraps a schema on a temp file, then reopens the SAME file
// through a driver that counts statements, so only the repository's own queries
// are measured (schema DDL runs before instrumentation).
func openCountingDB(t *testing.T) (*sql.DB, *queryCounter) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lessons.db")
	boot, err := open(path)
	if err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	base := boot.Driver()
	closeDB(boot)

	counter := &queryCounter{}
	name := fmt.Sprintf("sqlite-counting-%d", time.Now().UnixNano())
	sql.Register(name, countingDriver{inner: base, counter: counter})
	db, err := sql.Open(name, path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open counting db: %v", err)
	}
	t.Cleanup(func() { closeDB(db) })
	return db, counter
}

func seedLessons(t *testing.T, ctx context.Context, db *sql.DB, lessons, evidencePer int) {
	t.Helper()
	actions := NewActionRepository(db)
	repo := NewLessonRepository(db)
	at := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	for i := range lessons {
		evidence := make([]string, 0, evidencePer)
		for j := range evidencePer {
			id := fmt.Sprintf("act-%d-%d", i, j)
			if err := actions.Append(ctx, domain.Action{
				ID: id, RunID: "run-1", Kind: domain.ActionKindDeploy,
				Subject: "billing", Actor: "ci", At: at,
			}); err != nil {
				t.Fatalf("record action: %v", err)
			}
			evidence = append(evidence, id)
		}
		if err := repo.Append(ctx, domain.Lesson{
			ID:         fmt.Sprintf("lesson-%02d", i),
			Statement:  fmt.Sprintf("lesson %d holds", i),
			Scope:      domain.Scope{Service: "billing"},
			Trigger:    "deploy-fails",
			Kind:       "operational",
			Confidence: 0.7,
			DerivedAt:  at,
			Evidence:   evidence,
		}); err != nil {
			t.Fatalf("append lesson: %v", err)
		}
	}
}

// Listing lessons must cost a constant number of round trips, not one per
// lesson. The lesson corpus only grows (every synthesis run adds rows, none are
// removed), so an O(L) hydration loop makes `mnemos lessons` slower every week.
func TestLessonRepository_ListAll_HydratesEvidenceWithoutNPlusOne(t *testing.T) {
	db, counter := openCountingDB(t)
	ctx := context.Background()
	const lessons, evidencePer = 25, 2
	seedLessons(t, ctx, db, lessons, evidencePer)
	repo := NewLessonRepository(db)

	counter.n.Store(0)
	got, err := repo.ListAll(ctx)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	queries := counter.n.Load()

	if len(got) != lessons {
		t.Fatalf("got %d lessons, want %d", len(got), lessons)
	}
	for _, l := range got {
		if len(l.Evidence) != evidencePer {
			t.Errorf("lesson %s hydrated %d evidence ids, want %d", l.ID, len(l.Evidence), evidencePer)
		}
		for _, aid := range l.Evidence {
			if !strings.HasPrefix(aid, "act-") {
				t.Errorf("lesson %s got unexpected evidence %q", l.ID, aid)
			}
		}
	}
	// One query for the lessons + one for the whole evidence batch.
	if queries > 2 {
		t.Errorf("ListAll issued %d queries for %d lessons; want <= 2 (N+1 hydration is back)",
			queries, lessons)
	}
}

// ListByService/ListByTrigger share the same hydration path; a lesson with no
// evidence must still come back with a non-nil empty slice.
func TestLessonRepository_ListByService_HydratesEmptyEvidence(t *testing.T) {
	db, counter := openCountingDB(t)
	ctx := context.Background()
	seedLessons(t, ctx, db, 3, 1)
	repo := NewLessonRepository(db)
	// Inserted directly: domain.Lesson.Validate rejects an evidence-free lesson,
	// but the ROW can exist (its evidence was deleted, or a lesson landed from an
	// older writer), and the read path must not return nil for it.
	if _, err := db.ExecContext(ctx, `
INSERT INTO lessons (id, statement, scope_service, trigger, kind, confidence, derived_at)
VALUES ('lesson-bare', 'no evidence yet', 'billing', 'deploy-fails', 'operational', 0.5, '2026-02-02T00:00:00Z')`); err != nil {
		t.Fatalf("insert bare lesson: %v", err)
	}

	counter.n.Store(0)
	got, err := repo.ListByService(ctx, "billing")
	if err != nil {
		t.Fatalf("list by service: %v", err)
	}
	if queries := counter.n.Load(); queries > 2 {
		t.Errorf("ListByService issued %d queries for %d lessons; want <= 2", queries, len(got))
	}
	for _, l := range got {
		if l.Evidence == nil {
			t.Errorf("lesson %s has nil Evidence; want an empty slice", l.ID)
		}
		if l.ID == "lesson-bare" && len(l.Evidence) != 0 {
			t.Errorf("bare lesson hydrated %v, want no evidence", l.Evidence)
		}
	}
}
