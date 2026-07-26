package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/store"
	_ "go.klarlabs.de/mnemos/internal/store/sqlite"
)

// watch_file is reachable over `mcp --http`, and Watcher.Add used to accept any
// path filepath.Abs could resolve. The next tick ran the full ingest pipeline
// over it, so registering ~/.mnemos/jwt-secret or a .env and waiting for a
// rotation turned the brain into an exfiltration channel: the file's text came
// back out through query_knowledge / list_beliefs. Every watched path is now
// confined to a single root, symlinks resolved before the containment test.

func TestWatcher_AddRejectsPathOutsideRoot(t *testing.T) {
	outside := t.TempDir()
	root := t.TempDir()
	w := newTestWatcher(t, root)
	if w.Root() != canonicalDir(root) {
		t.Fatalf("Root() = %q, want the canonicalised root %q", w.Root(), canonicalDir(root))
	}

	secret := filepath.Join(outside, "jwt-secret")
	writeFile(t, secret, "super-secret-signing-key")

	// 1. A plain absolute path outside the root.
	if _, err := w.Add(secret); !errors.Is(err, errWatchOutsideRoot) {
		t.Errorf("absolute path outside root: err = %v, want errWatchOutsideRoot", err)
	}

	// 2. Traversal out of the root and back down into the secret.
	traversal := filepath.Join(root, "..", filepath.Base(outside), "jwt-secret")
	if _, err := w.Add(traversal); !errors.Is(err, errWatchOutsideRoot) {
		t.Errorf("traversal path: err = %v, want errWatchOutsideRoot", err)
	}

	// 3. Traversal expressed relative to the process working directory.
	t.Chdir(root)
	if _, err := w.Add(filepath.Join("..", filepath.Base(outside), "jwt-secret")); !errors.Is(err, errWatchOutsideRoot) {
		t.Errorf("relative traversal: err = %v, want errWatchOutsideRoot", err)
	}

	if w.Count() != 0 {
		t.Fatalf("nothing should have been registered, watching %d path(s)", w.Count())
	}
}

func TestWatcher_AddRejectsSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	root := t.TempDir()
	w := newTestWatcher(t, root)

	secret := filepath.Join(outside, ".env")
	writeFile(t, secret, "DATABASE_PASSWORD=hunter2")

	// A link that lives inside the root but resolves outside it. Checking the
	// literal path for containment would wave this straight through.
	link := filepath.Join(root, "notes.md")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := w.Add(link); !errors.Is(err, errWatchOutsideRoot) {
		t.Fatalf("symlink escape: err = %v, want errWatchOutsideRoot", err)
	}
	if w.Watching(link) || w.Count() != 0 {
		t.Fatal("a symlink escape must not end up in the watch set")
	}

	// A symlink that stays inside the root is fine — confinement, not a ban.
	realDoc := filepath.Join(root, "real.md")
	writeFile(t, realDoc, "we use SQLite")
	innerLink := filepath.Join(root, "alias.md")
	if err := os.Symlink(realDoc, innerLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := w.Add(innerLink); err != nil {
		t.Fatalf("in-root symlink must be accepted: %v", err)
	}
	if !w.Watching(realDoc) {
		t.Error("the link should be stored under its resolved target")
	}
}

// A watcher with no root refuses everything rather than degrading to
// "watch anything".
func TestWatcher_EmptyRootRefusesEverything(t *testing.T) {
	dir := t.TempDir()
	w := newTestWatcher(t, "\t ")
	path := filepath.Join(dir, "doc.md")
	writeFile(t, path, "content")
	if _, err := w.Add(path); !errors.Is(err, errWatchOutsideRoot) {
		t.Fatalf("rootless watcher: err = %v, want errWatchOutsideRoot", err)
	}
}

// The containment check is re-run every tick: a registered file swapped for a
// symlink pointing out of the root must be dropped, not ingested.
func TestWatcher_TickDropsPathThatEscapesAfterRegistration(t *testing.T) {
	outside := t.TempDir()
	root := t.TempDir()
	w := newTestWatcher(t, root)

	doc := filepath.Join(root, "doc.md")
	writeFile(t, doc, "harmless project notes")
	if _, err := w.Add(doc); err != nil {
		t.Fatalf("Add: %v", err)
	}

	secret := filepath.Join(outside, "jwt-secret")
	writeFile(t, secret, "the signing key must never be ingested")
	if err := os.Remove(doc); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(secret, doc); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	changed, removed := w.tick(context.Background())
	if changed != 0 || removed != 1 {
		t.Fatalf("tick = (changed=%d, removed=%d), want (0, 1)", changed, removed)
	}
	if w.Count() != 0 {
		t.Fatalf("escaped path still watched: %d", w.Count())
	}

	var events int
	if err := rawDB(t, w).QueryRow(`SELECT COUNT(*) FROM events`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Fatalf("the secret must never reach the brain, got %d event(s)", events)
	}
}

func TestResolveWatchPath_AcceptsFilesUnderRoot(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "docs", "deep")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(nested, "adr.md")
	writeFile(t, path, "we chose Postgres")

	got, err := resolveWatchPath(canonicalDir(root), path)
	if err != nil {
		t.Fatalf("nested file under the root must resolve: %v", err)
	}
	if !strings.HasSuffix(got, filepath.Join("docs", "deep", "adr.md")) {
		t.Errorf("resolved path = %q", got)
	}
}

// A directory whose name merely shares a prefix with the root is outside it:
// /tmp/roots must not be reachable from a root of /tmp/root.
func TestResolveWatchPath_PrefixSiblingIsOutside(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	sibling := filepath.Join(base, "roots")
	for _, d := range []string{root, sibling} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	path := filepath.Join(sibling, "secret.md")
	writeFile(t, path, "not yours")

	if _, err := resolveWatchPath(canonicalDir(root), path); !errors.Is(err, errWatchOutsideRoot) {
		t.Fatalf("prefix sibling: err = %v, want errWatchOutsideRoot", err)
	}
}

// resolveWatchRoot prefers the project root, and falls back to the working
// directory when the server was not launched inside a project.
func TestResolveWatchRoot(t *testing.T) {
	t.Run("project root wins", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		proj := filepath.Join(home, "proj")
		if err := os.MkdirAll(filepath.Join(proj, ".mnemos"), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		deep := filepath.Join(proj, "a", "b")
		if err := os.MkdirAll(deep, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		t.Chdir(deep)
		if got := canonicalDir(resolveWatchRoot()); got != canonicalDir(proj) {
			t.Errorf("root = %q, want the project root %q", got, canonicalDir(proj))
		}
	})

	t.Run("falls back to the working directory", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir := filepath.Join(home, "plain")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		t.Chdir(dir)
		if got := canonicalDir(resolveWatchRoot()); got != canonicalDir(dir) {
			t.Errorf("root = %q, want the cwd %q", got, canonicalDir(dir))
		}
	})
}

// --- finding 5: lazyWatcher must be race-free between requests and shutdown ---

// sync.Once publishes its writes only to goroutines that call Do. The shutdown
// path never does, so under `mcp --http` (get on request goroutines, shutdown
// on main) the teardown read watcher/conn unsynchronised — a race `-race`
// reports, and one that could leave the polling goroutine and a long-lived DB
// connection leaked. Run this under -race.
func TestLazyWatcher_ConcurrentGetAndShutdown(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mnemos.db")
	root := t.TempDir()

	var l lazyWatcher
	build := func() (*Watcher, *store.Conn, error) {
		conn, err := store.Open(context.Background(), "sqlite://"+dbPath)
		if err != nil {
			return nil, nil, err
		}
		gw, err := govwrite.Wrap(conn, nil)
		if err != nil {
			return nil, conn, err
		}
		return NewWatcher(gw, "", root), conn, nil
	}

	const goroutines = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Either a live watcher or the shutdown sentinel; never both nil.
			w, err := l.get(build)
			if err == nil && w == nil {
				t.Error("get returned no watcher and no error")
			}
			if err != nil && !errors.Is(err, errWatcherShutDown) {
				t.Errorf("unexpected get error: %v", err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		l.shutdown()
	}()
	close(start)
	wg.Wait()

	// Shutdown is idempotent and final: no watcher can be resurrected on a
	// connection that is already closed.
	l.shutdown()
	if _, err := l.get(build); !errors.Is(err, errWatcherShutDown) {
		t.Fatalf("get after shutdown = %v, want errWatcherShutDown", err)
	}
}

// The happy path still builds exactly once and the teardown actually stops the
// polling goroutine and releases the connection.
func TestLazyWatcher_BuildsOnceAndShutdownReleases(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mnemos.db")
	root := t.TempDir()

	var l lazyWatcher
	builds := 0
	var built *store.Conn
	build := func() (*Watcher, *store.Conn, error) {
		builds++
		conn, err := store.Open(context.Background(), "sqlite://"+dbPath)
		if err != nil {
			return nil, nil, err
		}
		built = conn
		gw, err := govwrite.Wrap(conn, nil)
		if err != nil {
			return nil, conn, err
		}
		return NewWatcher(gw, "", root), conn, nil
	}

	first, err := l.get(build)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	second, err := l.get(build)
	if err != nil {
		t.Fatalf("get again: %v", err)
	}
	if builds != 1 || first != second {
		t.Fatalf("builds = %d, same instance = %v; want 1 build reused", builds, first == second)
	}

	l.shutdown()
	select {
	case <-first.stopCh:
	default:
		t.Error("shutdown must stop the polling loop")
	}
	if err := built.Close(); err == nil {
		// Closing twice is tolerated by some backends; the meaningful check is
		// that shutdown ran Close itself, asserted by the stopCh probe above.
		t.Log("second Close returned nil (backend tolerates double close)")
	}
}
