package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/ingest"
	"go.klarlabs.de/mnemos/internal/parser"
	"go.klarlabs.de/mnemos/internal/pipeline"
	"go.klarlabs.de/mnemos/internal/relate"
	"go.klarlabs.de/mnemos/internal/store"
)

// defaultWatchInterval is how often the watcher polls each registered file.
// Short enough to feel responsive; long enough not to thrash on agent edits.
const defaultWatchInterval = 5 * time.Second

// Watcher tracks a set of files and re-ingests them when content changes.
// State is in-memory only — MCP restarts wipe the watch set, and the agent
// re-registers paths as needed. Polling-based to avoid the cross-platform
// quirks of fsnotify on stdio servers; the cost is a few extra stat calls
// per interval.
//
// Every watched path is confined to a single root directory (see
// [resolveWatchRoot]). watch_file is reachable over `mcp --http`, and the
// watcher's tick runs the full ingest pipeline over whatever it is pointed at
// — so an unconfined watcher turns the brain into a remote arbitrary-file
// reader: register ~/.mnemos/jwt-secret or a .env, wait for the next write,
// and read the contents back out through query_knowledge or list_beliefs.
type Watcher struct {
	mu       sync.Mutex
	hashes   map[string]string // resolved absolute path → sha256 hex of last-seen content
	writer   *govwrite.Writer  // governed daemon-writer; all re-ingest writes route through it
	interval time.Duration
	actor    string // user id to stamp as created_by on re-ingested events/claims
	root     string // symlink-resolved directory every watched file must live under

	startOnce sync.Once
	stopCh    chan struct{}
}

// errWatchOutsideRoot is returned when a caller asks to watch a file that does
// not live under the watcher's root.
var errWatchOutsideRoot = errors.New("path is outside the watch root")

// NewWatcher returns a watcher that re-ingests changed files through the
// governed daemon-writer, confined to root. The background goroutine is not
// started until the first Add call. actor is the user id stamped on everything
// the watcher persists; pass domain.SystemUser (or empty) to attribute
// writes to the system.
//
// root is normalised once here (absolute + symlinks resolved) so the
// containment check in Add is a cheap prefix comparison against a canonical
// path. An empty or unresolvable root leaves the watcher closed: Add rejects
// every path rather than silently degrading to "watch anything".
func NewWatcher(w *govwrite.Writer, actor, root string) *Watcher {
	return &Watcher{
		hashes:   make(map[string]string),
		writer:   w,
		interval: defaultWatchInterval,
		actor:    actor,
		root:     canonicalDir(root),
		stopCh:   make(chan struct{}),
	}
}

// Root returns the directory the watcher confines registrations to.
func (w *Watcher) Root() string { return w.root }

// resolveWatchRoot picks the directory watch_file is allowed to reach into.
//
// The root is the project root when the server was launched inside a Mnemos
// project (a .mnemos/ brain was found by walking up from the working
// directory), otherwise the process working directory. Rationale: watch_file
// exists to keep a *project's* documents' claims fresh, and for the stdio
// transport Claude Code launches the server with the project as its working
// directory — so local use is unchanged. For a hosted `mcp --http` listener
// the working directory is the service's own, which by construction holds no
// user secrets, and $HOME, /etc and every sibling deployment fall outside it.
func resolveWatchRoot() string {
	if _, projectRoot, ok := findProjectDB(); ok && projectRoot != "" {
		return projectRoot
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

// canonicalDir returns dir as an absolute, symlink-resolved path, or "" when
// it cannot be resolved. Resolving matters on macOS, where /tmp and
// /var/folders are themselves symlinks: comparing an unresolved root against a
// resolved candidate would reject legitimate paths.
func canonicalDir(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return resolved
}

// resolveWatchPath turns a caller-supplied path into the canonical path the
// watcher will read, refusing anything that escapes root.
//
// Symlinks are resolved BEFORE the containment test, not after: a link inside
// the root pointing at /etc/shadow is an escape, and a check on the literal
// path would wave it through. Because the resolved path is what gets stored
// and later read, containment holds for the whole lifetime of the watch.
func resolveWatchPath(root, path string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("%w: no watch root configured", errWatchOutsideRoot)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	// EvalSymlinks requires the file to exist. It must: Add hashes the file
	// immediately, so a non-existent path is an error either way.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", fmt.Errorf("%w (%s)", errWatchOutsideRoot, root)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w (%s)", errWatchOutsideRoot, root)
	}
	return resolved, nil
}

// Add registers path for watching. The current content hash is recorded but
// no ingest happens — auto-ingest or an explicit process_text call is
// expected to have brought the file into the DB already. Returns the number
// of files currently being watched after this call.
//
// Paths outside the watcher's root — including via traversal or a symlink
// planted inside it — are refused.
func (w *Watcher) Add(path string) (int, error) {
	abs, err := resolveWatchPath(w.root, path)
	if err != nil {
		return 0, err
	}
	hash, err := hashFile(abs)
	if err != nil {
		return 0, err
	}

	w.mu.Lock()
	w.hashes[abs] = hash
	count := len(w.hashes)
	w.mu.Unlock()

	w.startOnce.Do(func() { go w.loop(context.Background()) })
	return count, nil
}

// Watching reports whether path is currently in the watch set.
func (w *Watcher) Watching(path string) bool {
	abs, err := resolveWatchPath(w.root, path)
	if err != nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.hashes[abs]
	return ok
}

// Count returns the number of paths currently being watched.
func (w *Watcher) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.hashes)
}

// Stop signals the polling loop to exit. Safe to call from anywhere; further
// Add calls after Stop will not restart the loop.
func (w *Watcher) Stop() {
	select {
	case <-w.stopCh:
		// already closed
	default:
		close(w.stopCh)
	}
}

func (w *Watcher) loop(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// tick re-hashes every watched file and re-ingests any whose content has
// changed since the last observation. Disappearing files are dropped from
// the set with a stderr note. Returns counts for tests.
func (w *Watcher) tick(ctx context.Context) (changed, removed int) {
	w.mu.Lock()
	snapshot := make(map[string]string, len(w.hashes))
	for k, v := range w.hashes {
		snapshot[k] = v
	}
	w.mu.Unlock()

	service := ingest.NewService()
	normalizer := parser.NewNormalizer()
	extractor, err := pipeline.NewExtractor(false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "watch: failed to build extractor: %v\n", err)
		return 0, 0
	}
	relEngine := relate.NewEngine()

	for path, oldHash := range snapshot {
		// Re-check containment every tick, not just at Add: the stored path
		// could since have been replaced by a symlink pointing out of the
		// root, which os.ReadFile would happily follow. Closing that TOCTOU
		// costs one lstat per file per interval. A vanished file is not an
		// escape — it falls through to the missing-file handling below.
		resolved, resErr := resolveWatchPath(w.root, path)
		escaped := (resErr != nil && !errors.Is(resErr, os.ErrNotExist)) ||
			(resErr == nil && resolved != path)
		if escaped {
			w.mu.Lock()
			delete(w.hashes, path)
			w.mu.Unlock()
			removed++
			fmt.Fprintf(os.Stderr, "watch: dropped %s (no longer inside the watch root)\n", path)
			continue
		}
		newHash, err := hashFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				w.mu.Lock()
				delete(w.hashes, path)
				w.mu.Unlock()
				removed++
				fmt.Fprintf(os.Stderr, "watch: dropped missing file %s\n", path)
				continue
			}
			fmt.Fprintf(os.Stderr, "watch: hash %s: %v\n", path, err)
			continue
		}
		if newHash == oldHash {
			continue
		}

		runID := fmt.Sprintf("watch-%s", time.Now().UTC().Format("20060102T150405"))
		if err := ingestSingleDoc(ctx, w.writer, service, normalizer, extractor, relEngine, runID, path, w.actor); err != nil {
			fmt.Fprintf(os.Stderr, "watch: re-ingest %s: %v\n", path, err)
			continue
		}

		w.mu.Lock()
		w.hashes[path] = newHash
		w.mu.Unlock()
		changed++
		fmt.Fprintf(os.Stderr, "watch: re-ingested %s\n", path)
	}

	return changed, removed
}

// errWatcherShutDown is returned by lazyWatcher.get once the server has begun
// tearing down; a tool call racing shutdown must not resurrect the singleton
// on a connection that is about to close.
var errWatcherShutDown = errors.New("watcher is shutting down")

// lazyWatcher owns the process-wide watch_file singleton and its long-lived
// DB connection.
//
// It replaces a `sync.Once` + bare package-local variables. sync.Once
// establishes happens-before only for goroutines that call Do, and the
// shutdown path never does: under `mcp --http`, get runs on request goroutines
// while shutdown runs on main, so the teardown read the watcher and its conn
// unsynchronised. That is a data race `-race` flags, and functionally the
// teardown could observe nil and skip Stop()/Close() — leaking the polling
// goroutine and a long-lived DB connection, the exact leak the teardown exists
// to prevent. One mutex both publishes the initialisation and orders it
// against shutdown.
type lazyWatcher struct {
	mu      sync.Mutex
	built   bool
	watcher *Watcher
	conn    *store.Conn
	err     error
}

// get builds the watcher on first call and returns the same instance
// afterwards. build returns the watcher, the connection whose lifetime the
// watcher borrows, and any construction error.
func (l *lazyWatcher) get(build func() (*Watcher, *store.Conn, error)) (*Watcher, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.built {
		l.built = true
		l.watcher, l.conn, l.err = build()
	}
	return l.watcher, l.err
}

// shutdown stops the polling goroutine and closes the borrowed connection,
// under the same lock get takes. Idempotent, and safe to call when no watcher
// was ever built.
func (l *lazyWatcher) shutdown() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.watcher != nil {
		l.watcher.Stop()
	}
	if l.conn != nil {
		_ = l.conn.Close()
	}
	l.watcher, l.conn = nil, nil
	l.built, l.err = true, errWatcherShutDown
}

// hashFile returns the hex-encoded sha256 of the file at path.
func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: watcher reads user-registered files by design
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
