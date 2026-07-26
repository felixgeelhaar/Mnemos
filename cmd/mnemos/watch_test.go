package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/store"
	_ "go.klarlabs.de/mnemos/internal/store/sqlite"
)

// newTestWatcher returns a watcher rooted at root. Pass "" to root it at a
// fresh temp dir the caller does not otherwise use.
func newTestWatcher(t *testing.T, root string) *Watcher {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "mnemos.db")
	conn, err := store.Open(context.Background(), "sqlite://"+dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	gw, err := govwrite.Wrap(conn, nil)
	if err != nil {
		t.Fatalf("wrap governed writer: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })
	if root == "" {
		root = t.TempDir()
	}
	w := NewWatcher(gw, "", root)
	t.Cleanup(w.Stop)
	return w
}

// rawDB returns the *sql.DB underlying the watcher's conn so tests
// can issue raw SQL probes against the persisted state.
func rawDB(t *testing.T, w *Watcher) *sql.DB {
	t.Helper()
	db, ok := w.writer.Conn().Raw.(*sql.DB)
	if !ok || db == nil {
		t.Fatal("watcher conn has no *sql.DB raw handle")
	}
	return db
}

func TestWatcher_AddRecordsHashAndCount(t *testing.T) {
	dir := t.TempDir()
	w := newTestWatcher(t, dir)
	path := filepath.Join(dir, "README.md")
	writeFile(t, path, "initial content")

	count, err := w.Add(path)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	if !w.Watching(path) {
		t.Fatal("expected path to be watched")
	}
}

func TestWatcher_AddNonExistentFileFails(t *testing.T) {
	dir := t.TempDir()
	w := newTestWatcher(t, dir)
	if _, err := w.Add(filepath.Join(dir, "missing.md")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestWatcher_TickSkipsWhenContentUnchanged(t *testing.T) {
	dir := t.TempDir()
	w := newTestWatcher(t, dir)
	path := filepath.Join(dir, "doc.md")
	writeFile(t, path, "stable")
	if _, err := w.Add(path); err != nil {
		t.Fatalf("Add: %v", err)
	}

	changed, removed := w.tick(context.Background())
	if changed != 0 || removed != 0 {
		t.Fatalf("tick = (changed=%d, removed=%d), want both 0", changed, removed)
	}

	var eventCount int
	if err := rawDB(t, w).QueryRow(`SELECT COUNT(*) FROM events`).Scan(&eventCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventCount != 0 {
		t.Fatalf("expected no events from no-op tick, got %d", eventCount)
	}
}

func TestWatcher_TickReingestsOnContentChange(t *testing.T) {
	dir := t.TempDir()
	w := newTestWatcher(t, dir)
	path := filepath.Join(dir, "doc.md")
	writeFile(t, path, "We use SQLite. The pipeline is event-sourced.")
	if _, err := w.Add(path); err != nil {
		t.Fatalf("Add: %v", err)
	}

	writeFile(t, path, "We migrated to PostgreSQL. The pipeline now uses a queue.")

	changed, removed := w.tick(context.Background())
	if changed != 1 || removed != 0 {
		t.Fatalf("tick = (changed=%d, removed=%d), want (1, 0)", changed, removed)
	}

	var eventCount int
	if err := rawDB(t, w).QueryRow(`SELECT COUNT(*) FROM events`).Scan(&eventCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventCount == 0 {
		t.Fatal("expected events persisted after re-ingest, got 0")
	}

	// Second tick on the same content should be a no-op.
	changed2, removed2 := w.tick(context.Background())
	if changed2 != 0 || removed2 != 0 {
		t.Fatalf("second tick = (changed=%d, removed=%d), want both 0 (hash should be updated)", changed2, removed2)
	}
}

func TestWatcher_TickDropsMissingFile(t *testing.T) {
	dir := t.TempDir()
	w := newTestWatcher(t, dir)
	path := filepath.Join(dir, "doomed.md")
	writeFile(t, path, "soon to be deleted")
	if _, err := w.Add(path); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}

	changed, removed := w.tick(context.Background())
	if changed != 0 || removed != 1 {
		t.Fatalf("tick = (changed=%d, removed=%d), want (0, 1)", changed, removed)
	}
	if w.Count() != 0 {
		t.Fatalf("expected watch set empty after drop, got %d", w.Count())
	}
}

func TestWatcher_StopIsIdempotent(t *testing.T) {
	w := newTestWatcher(t, "")
	w.Stop()
	w.Stop() // second call must not panic
}

func TestHashFile_StableAcrossReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.md")
	writeFile(t, path, "deterministic content")
	h1, err := hashFile(path)
	if err != nil {
		t.Fatalf("hash 1: %v", err)
	}
	h2, err := hashFile(path)
	if err != nil {
		t.Fatalf("hash 2: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("hash unstable: %s != %s", h1, h2)
	}
}
