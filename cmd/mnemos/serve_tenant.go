package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"go.klarlabs.de/mnemos"
	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/store"
)

// Per-request tenant scoping for the `serve` REST + gRPC surfaces
// (`serve --require-tenant`, ADR 0007). The handlers capture the process's
// shared single-tenant conn/writer/mem at construction; in multi-tenant mode a
// middleware opens a tenant-scoped connection per request and stashes it, and
// every handler resolves its dependencies through scopedConn/scopedWriter/
// scopedMem — which return the request-scoped values when present and the
// captured fallback otherwise (so single-tenant mode is unchanged).

type reqConnKey struct{}
type reqWriterKey struct{}

func withReqConn(ctx context.Context, c *store.Conn) context.Context {
	return context.WithValue(ctx, reqConnKey{}, c)
}
func withReqWriter(ctx context.Context, w *govwrite.Writer) context.Context {
	return context.WithValue(ctx, reqWriterKey{}, w)
}

// scopedConn returns the request's tenant-scoped conn, or the fallback (the
// shared conn) when the request carries no tenant.
func scopedConn(ctx context.Context, fallback *store.Conn) *store.Conn {
	if c, ok := ctx.Value(reqConnKey{}).(*store.Conn); ok && c != nil {
		return c
	}
	return fallback
}

// scopedWriter mirrors scopedConn for the governed writer (writes).
func scopedWriter(ctx context.Context, fallback *govwrite.Writer) *govwrite.Writer {
	if w, ok := ctx.Value(reqWriterKey{}).(*govwrite.Writer); ok && w != nil {
		return w
	}
	return fallback
}

// tenant mem cache: Memory.Tenant(id) opens its own view; cache one per tenant
// for the life of the server (closed by closeTenantMems at shutdown).
var (
	tenantMemMu   sync.Mutex
	tenantMemsSrv = map[string]mnemos.Memory{}
)

// reqMemKey carries a request-scoped tenant Memory view, used in place of the
// process-wide cache above when caching is unsafe (see reqMem).
type reqMemKey struct{}

// reqMem is a lazily-built, request-scoped tenant Memory view: opened on first
// use by scopedMem and closed by tenantScopeMiddleware when the request ends.
//
// It exists because a Memory owns a store connection, and under the Postgres
// shared-pool mode (MNEMOS_PG_SHARED_POOL, ADR 0007 Phase 2) that connection is
// ONE *sql.Conn checked out of a single process-wide pool, whose Closer resets
// the mnemos.tenant GUC and hands it back. A process-lifetime cache of such a
// facade never closes it before shutdown, so the connection never returns to
// the pool: one permanently pinned connection per tenant, until the pool is
// exhausted and every tenant stalls on a checkout that can never happen. The
// read-conn cache next door already declines to cache under this mode
// (enableConnCache in dsn.go); this is the same decision for the cognitive path.
//
// Lazy rather than opened by the middleware: building a facade opens a
// connection and boots the query/chronos wiring, and most endpoints never touch
// the cognitive layer. Only a request that actually calls scopedMem pays.
type reqMem struct {
	mu    sync.Mutex
	built bool
	mem   mnemos.Memory
}

// get returns the request's tenant view, building it on first use. A build
// failure is remembered as nil so scopedMem's callers fail closed (503) rather
// than retrying an open that is not going to start working mid-request.
func (r *reqMem) get(fallback mnemos.Memory, tenant string) mnemos.Memory {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.built {
		r.built = true
		if m, err := fallback.Tenant(tenant); err == nil {
			r.mem = m
		}
	}
	return r.mem
}

// close releases the view and, with it, the connection the facade holds.
func (r *reqMem) close() error {
	r.mu.Lock()
	m := r.mem
	r.mem = nil
	r.mu.Unlock()
	if m == nil {
		return nil
	}
	return m.Close()
}

// tenantMemCacheSafe reports whether a per-tenant Memory view may be cached for
// the life of the process. It is not under the Postgres shared-pool mode: see
// reqMem for why a cached facade there pins a pooled connection forever.
// Mirrors enableConnCache's env test — same variable, same reading.
func tenantMemCacheSafe() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MNEMOS_PG_SHARED_POOL"))) {
	case "1", "true", "yes":
		return false
	}
	return true
}

// scopedMem returns a per-tenant Memory view when the request carries a tenant,
// else the shared fallback. When a tenant IS present but its view can't be
// opened, it returns nil (fail closed) so the cognitive endpoints' nil-guard
// yields a 503 — never the shared __default__ facade, which would serve the
// wrong partition's data.
//
// The view is normally cached per tenant for the life of the server. When
// tenantScopeMiddleware decided caching is unsafe it stashed a request-scoped
// holder instead (see reqMem); this reads that first, so every call within one
// request still gets the same instance and it is closed when the request ends.
func scopedMem(ctx context.Context, fallback mnemos.Memory) mnemos.Memory {
	tenant, ok := tenantFromContext(ctx)
	if !ok || fallback == nil {
		return fallback
	}
	if rm, ok := ctx.Value(reqMemKey{}).(*reqMem); ok && rm != nil {
		return rm.get(fallback, tenant)
	}
	tenantMemMu.Lock()
	m, cached := tenantMemsSrv[tenant]
	tenantMemMu.Unlock()
	if cached {
		return m
	}

	// Built WITHOUT the lock held. Tenant() opens a connection, so holding the
	// mutex across it made one slow dial block every other tenant's scopedMem —
	// and closeTenantMems, which takes the same mutex, so a shutdown during a
	// stalled open hung instead of proceeding. Same shape as openConn's cache in
	// dsn.go (#150).
	tm, err := fallback.Tenant(tenant)
	if err != nil {
		return nil
	}
	tenantMemMu.Lock()
	if existing, ok := tenantMemsSrv[tenant]; ok {
		// Lost the race to open the same tenant concurrently — keep the winner
		// and close our redundant view, which owns a connection of its own.
		tenantMemMu.Unlock()
		_ = tm.Close()
		return existing
	}
	tenantMemsSrv[tenant] = tm
	tenantMemMu.Unlock()
	return tm
}

// closeTenantMems closes every cached per-tenant Memory. Call at serve shutdown.
func closeTenantMems() {
	tenantMemMu.Lock()
	defer tenantMemMu.Unlock()
	for _, m := range tenantMemsSrv {
		_ = m.Close()
	}
	tenantMemsSrv = map[string]mnemos.Memory{}
}

// tenantScopeMiddleware opens one tenant-scoped connection per request (and a
// governed writer borrowing it) and stashes both, closing the connection after
// the handler. Requests without a tenant in context pass through untouched.
func tenantScopeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := tenantFromContext(r.Context()); !ok {
			next.ServeHTTP(w, r)
			return
		}
		conn, err := openConn(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "tenant store unavailable")
			return
		}
		defer closeConn(conn)
		ctx := withReqConn(r.Context(), conn)
		// Wrap borrows the conn (ownConn=false), so the writer's Close only
		// releases the kernel's evidence sink — a no-op when
		// MNEMOS_AXI_EVIDENCE_LOG is unset, but a real file-descriptor release
		// when it is set. Close it per request either way; the conn itself is
		// closed separately above. A Wrap failure must fail the request closed
		// rather than let handlers fall back to the __default__ writer.
		gw, err := govwrite.Wrap(conn, nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "tenant store unavailable")
			return
		}
		defer func() { _ = gw.Close() }()
		ctx = withReqWriter(ctx, gw)
		// The cognitive path's per-tenant Memory view. Cached process-wide by
		// default; under the Postgres shared-pool mode a cached facade would pin
		// its pooled connection for good, so scope it to the request and close it
		// here (scopedMem builds it lazily, only if an endpoint asks).
		if !tenantMemCacheSafe() {
			rm := &reqMem{}
			defer func() {
				if cerr := rm.close(); cerr != nil {
					log.Printf("close request memory: %v", cerr)
				}
			}()
			ctx = context.WithValue(ctx, reqMemKey{}, rm)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
