package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.klarlabs.de/mnemos"
)

// The REST surface's half of the per-tenant Memory lifetime contract; the gRPC
// half is TestGRPC_TenantMemory_* next door.
//
// A Memory owns a store connection. Under the Postgres shared-pool mode
// (MNEMOS_PG_SHARED_POOL) that connection is one *sql.Conn checked out of a
// single process-wide pool, returned only when the facade is closed. scopedMem
// cached the facade for the life of the server, so the connection was pinned
// forever — one leak per tenant, until the pool drains and every tenant stalls
// on a checkout that can never complete.
//
// Backend-independent on purpose: a fake Memory observes exactly the property
// that matters (does the server close the view it built?), so the guard still
// runs in CI where TEST_POSTGRES_DSN is not set.

type fakeSrvMem struct {
	mnemos.Memory // nil: any unexercised method panics loudly rather than lying

	mu       sync.Mutex
	tenanted int
	views    []*fakeSrvView
}

type fakeSrvView struct {
	mnemos.Memory
	id string

	mu     sync.Mutex
	closed int
}

func (m *fakeSrvMem) Tenant(id string) (mnemos.Memory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenanted++
	v := &fakeSrvView{id: id}
	m.views = append(m.views, v)
	return v, nil
}

func (m *fakeSrvMem) Close() error { return nil }

func (m *fakeSrvMem) stats() (tenanted, open int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.views {
		v.mu.Lock()
		if v.closed == 0 {
			open++
		}
		v.mu.Unlock()
	}
	return m.tenanted, open
}

func (v *fakeSrvView) Close() error {
	v.mu.Lock()
	v.closed++
	v.mu.Unlock()
	return nil
}

// serveTenantMemRequests drives n requests for the same tenant through
// tenantScopeMiddleware, calling scopedMem inside the handler exactly as the
// cognitive endpoints do, and returns the ids scopedMem handed back.
func serveTenantMemRequests(t *testing.T, fallback mnemos.Memory, tenant string, n int) []string {
	t.Helper()
	var got []string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := scopedMem(r.Context(), fallback)
		if m == nil {
			t.Error("scopedMem returned nil")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// Twice, as a handler that resolves its dependency more than once
		// would: both calls must be the same instance.
		if again := scopedMem(r.Context(), fallback); again != m {
			t.Error("scopedMem returned a different view within one request")
		}
		if v, ok := m.(*fakeSrvView); ok {
			got = append(got, v.id)
		}
		w.WriteHeader(http.StatusOK)
	})
	h := tenantScopeMiddleware(inner)

	for range n {
		req := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
		req = req.WithContext(withTenant(req.Context(), tenant))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request failed: %d %s", rec.Code, rec.Body.String())
		}
	}
	return got
}

// TestServe_TenantMemory_ReleasedPerRequestUnderSharedPool: with the shared-pool
// mode on, the per-tenant view must not outlive its request.
func TestServe_TenantMemory_ReleasedPerRequestUnderSharedPool(t *testing.T) {
	t.Setenv("MNEMOS_PG_SHARED_POOL", "1")
	// A namespace-capable backend so tenantScopeMiddleware's openConn resolves.
	t.Setenv("MNEMOS_DB_URL", "sqlite://"+t.TempDir()+"/t.db")
	t.Cleanup(closeTenantMems)

	mem := &fakeSrvMem{}
	const requests = 5
	ids := serveTenantMemRequests(t, mem, "acme", requests)
	if len(ids) != requests {
		t.Fatalf("handler saw %d views, want %d", len(ids), requests)
	}

	tenanted, open := mem.stats()
	if tenanted != requests {
		t.Errorf("built %d views over %d requests, want one per request", tenanted, requests)
	}
	if open != 0 {
		t.Errorf("%d views left open; each pins a pooled connection for the life of the process", open)
	}

	// And nothing was parked in the process-wide cache on the way past.
	tenantMemMu.Lock()
	cached := len(tenantMemsSrv)
	tenantMemMu.Unlock()
	if cached != 0 {
		t.Errorf("%d views in the process cache, want 0 under the shared pool", cached)
	}
}

// TestServe_TenantMemory_CachedWhenPoolNotShared: the default (per-tenant-pool)
// mode is unchanged — one view per tenant, reused, released by closeTenantMems.
func TestServe_TenantMemory_CachedWhenPoolNotShared(t *testing.T) {
	t.Setenv("MNEMOS_PG_SHARED_POOL", "")
	t.Setenv("MNEMOS_DB_URL", "sqlite://"+t.TempDir()+"/t.db")
	t.Cleanup(closeTenantMems)

	mem := &fakeSrvMem{}
	serveTenantMemRequests(t, mem, "acme", 5)

	if tenanted, open := mem.stats(); tenanted != 1 || open != 1 {
		t.Errorf("built %d views (%d open) for one tenant over 5 requests, want 1/1 (cache defeated)", tenanted, open)
	}

	closeTenantMems()
	if _, open := mem.stats(); open != 0 {
		t.Errorf("%d views open after closeTenantMems, want 0", open)
	}
}
