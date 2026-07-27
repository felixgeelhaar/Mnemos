package grpc_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	mnemos "go.klarlabs.de/mnemos"
	"go.klarlabs.de/mnemos/internal/auth"
	"go.klarlabs.de/mnemos/internal/domain"
	mnemosgrpc "go.klarlabs.de/mnemos/internal/server/grpc"
	"go.klarlabs.de/mnemos/internal/store"
	_ "go.klarlabs.de/mnemos/internal/store/memory"
	mnemosv1 "go.klarlabs.de/mnemos/proto/gen/mnemos/v1"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// The lifetime contract for a per-tenant Memory view, checked without a
// database.
//
// A Memory owns a store connection. Under the Postgres shared-pool mode that
// connection is one *sql.Conn checked out of a single process-wide pool, and it
// only goes back when the facade is closed. Caching the facade for the life of
// the process therefore pins one connection per tenant forever — the pool
// drains as tenants arrive and eventually every tenant blocks on a checkout
// that can never complete. Whether that happens is decided entirely by whether
// the server closes the view it built, which is a property of the server, not
// of Postgres: a fake Memory can observe it exactly, and does so in CI where
// TEST_POSTGRES_DSN is not set.

// countingMem is a Memory that only knows how to be scoped and closed. The
// embedded nil interface makes any other method a loud panic rather than a
// silent zero value — nothing in these tests should reach one.
type countingMem struct {
	mnemos.Memory

	mu       sync.Mutex
	tenanted int // Tenant() calls
	views    []*tenantView
}

type tenantView struct {
	mnemos.Memory
	id string

	mu     sync.Mutex
	closed int
}

func (m *countingMem) Tenant(id string) (mnemos.Memory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenanted++
	v := &tenantView{id: id}
	m.views = append(m.views, v)
	return v, nil
}

func (m *countingMem) Close() error { return nil }

// WhoKnows is the one real method: it is what the RPC under test calls, and it
// must be served by a view that is still open.
func (m *countingMem) WhoKnows(context.Context, string, int) ([]mnemos.Expert, error) {
	return nil, nil
}

func (m *countingMem) stats() (tenanted, open int, views []*tenantView) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.views {
		v.mu.Lock()
		if v.closed == 0 {
			open++
		}
		v.mu.Unlock()
	}
	return m.tenanted, open, append([]*tenantView(nil), m.views...)
}

func (v *tenantView) Close() error {
	v.mu.Lock()
	v.closed++
	v.mu.Unlock()
	return nil
}

func (v *tenantView) WhoKnows(context.Context, string, int) ([]mnemos.Expert, error) {
	v.mu.Lock()
	closed := v.closed
	v.mu.Unlock()
	if closed > 0 {
		return nil, context.Canceled // surfaces as a failed RPC below
	}
	return nil, nil
}

// startTenantMemServer stands up the gRPC surface in tenant-scoping mode over
// an in-memory store, with mem as the cognitive facade.
func startTenantMemServer(t *testing.T, mem mnemos.Memory) (mnemosv1.MnemosServiceClient, *auth.Issuer, *mnemosgrpc.Server) {
	t.Helper()
	ctx := context.Background()

	conn, err := store.Open(ctx, "memory://")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	secret, err := auth.GenerateSecret()
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	issuer := auth.NewIssuer(secret)
	verifier := auth.NewVerifier(secret, conn.RevokedTokens)

	impl := mnemosgrpc.NewServerWithMemory(conn, mem, verifier, testLogger(), "test").
		// The storage side is irrelevant here: hand every tenant the same
		// in-memory conn (borrowed, so the closer is a no-op) and let the test
		// watch only the cognitive facade.
		WithTenantScoping(
			func(context.Context, string) (*store.Conn, error) { return conn, nil },
			func(*store.Conn) {},
		)
	t.Cleanup(func() { _ = impl.Close() })

	gs := grpclib.NewServer(grpclib.UnaryInterceptor(impl.UnaryInterceptor()))
	impl.Register(gs)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.GracefulStop)

	cc, err := grpclib.NewClient(lis.Addr().String(), grpclib.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	return mnemosv1.NewMnemosServiceClient(cc), issuer, impl
}

func tenantBearer(t *testing.T, issuer *auth.Issuer, tenant string) context.Context {
	t.Helper()
	user := domain.User{ID: "usr-lifecycle", Status: domain.UserStatusActive, CreatedAt: time.Now()}
	tok, _, err := issuer.IssueUserTokenWithTenant(user, tenant, time.Hour)
	if err != nil {
		t.Fatalf("issue token for %q: %v", tenant, err)
	}
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+tok)
}

// TestGRPC_TenantMemory_ReleasedPerRequestUnderSharedPool: with the Postgres
// shared-pool mode on, a per-tenant Memory view must not outlive the request
// that built it. It used to be cached for the life of the process, so the
// pooled connection it holds was never handed back — one leaked connection per
// tenant, and a pool that drains until every tenant stalls.
//
// Backend-independent on purpose: this is the guard that still runs in CI, where
// the live-Postgres cross-tenant tests skip.
func TestGRPC_TenantMemory_ReleasedPerRequestUnderSharedPool(t *testing.T) {
	t.Setenv("MNEMOS_PG_SHARED_POOL", "1")

	mem := &countingMem{}
	client, issuer, _ := startTenantMemServer(t, mem)
	acme := tenantBearer(t, issuer, "acme")

	const requests = 5
	for i := range requests {
		if _, err := client.WhoKnows(acme, &mnemosv1.WhoKnowsRequest{Query: "q", Limit: 1}); err != nil {
			t.Fatalf("WhoKnows #%d: %v", i, err)
		}
		// Nothing may still be open between requests: a view held past the end
		// of its RPC is exactly the pinned connection.
		if _, open, _ := mem.stats(); open != 0 {
			t.Fatalf("after request #%d: %d tenant views still open, want 0", i, open)
		}
	}

	tenanted, open, views := mem.stats()
	if tenanted != requests {
		t.Errorf("built %d tenant views over %d requests, want one per request", tenanted, requests)
	}
	if open != 0 {
		t.Errorf("%d tenant views left open; each pins a pooled connection forever", open)
	}
	for _, v := range views {
		v.mu.Lock()
		closed := v.closed
		v.mu.Unlock()
		if closed != 1 {
			t.Errorf("view for tenant %q closed %d times, want exactly 1", v.id, closed)
		}
	}

	// A second tenant must not accumulate either: the count of live views is
	// what exhausts the pool, and it must stay flat in the number of tenants.
	globex := tenantBearer(t, issuer, "globex")
	if _, err := client.WhoKnows(globex, &mnemosv1.WhoKnowsRequest{Query: "q", Limit: 1}); err != nil {
		t.Fatalf("WhoKnows (globex): %v", err)
	}
	if _, open, _ := mem.stats(); open != 0 {
		t.Errorf("%d views open after serving a second tenant, want 0", open)
	}
}

// TestGRPC_TenantMemory_CachedWhenPoolNotShared: the default (per-tenant-pool)
// mode is unchanged — one view per tenant, reused across that tenant's
// requests, released by Server.Close. Without this the fix above could be
// "always rebuild", which would pay a fresh pool per request on the common path.
func TestGRPC_TenantMemory_CachedWhenPoolNotShared(t *testing.T) {
	t.Setenv("MNEMOS_PG_SHARED_POOL", "")

	mem := &countingMem{}
	client, issuer, impl := startTenantMemServer(t, mem)
	acme := tenantBearer(t, issuer, "acme")

	for i := range 5 {
		if _, err := client.WhoKnows(acme, &mnemosv1.WhoKnowsRequest{Query: "q", Limit: 1}); err != nil {
			t.Fatalf("WhoKnows #%d: %v", i, err)
		}
	}
	tenanted, open, _ := mem.stats()
	if tenanted != 1 {
		t.Errorf("built %d tenant views for one tenant over 5 requests, want 1 (cache defeated)", tenanted)
	}
	if open != 1 {
		t.Errorf("%d views open, want the single cached one", open)
	}

	// Server.Close still releases the cache.
	if err := impl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, open, _ := mem.stats(); open != 0 {
		t.Errorf("%d views open after Server.Close, want 0", open)
	}
}

// TestGRPC_TenantMemory_ConcurrentRequestsDoNotLeak: the per-request holder is
// reached from the interceptor's defer and from memFor on the handler
// goroutine; under -race, concurrent same-tenant traffic must neither leak a
// view nor trip the detector.
func TestGRPC_TenantMemory_ConcurrentRequestsDoNotLeak(t *testing.T) {
	t.Setenv("MNEMOS_PG_SHARED_POOL", "1")

	mem := &countingMem{}
	client, issuer, _ := startTenantMemServer(t, mem)
	acme := tenantBearer(t, issuer, "acme")

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = client.WhoKnows(acme, &mnemosv1.WhoKnowsRequest{Query: "q", Limit: 1})
		}()
	}
	wg.Wait()

	if _, open, _ := mem.stats(); open != 0 {
		t.Errorf("%d tenant views open after 16 concurrent requests, want 0", open)
	}
}
