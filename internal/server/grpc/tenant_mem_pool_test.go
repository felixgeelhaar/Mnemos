package grpc_test

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	mnemos "go.klarlabs.de/mnemos"
	"go.klarlabs.de/mnemos/internal/auth"
	"go.klarlabs.de/mnemos/internal/domain"
	mnemosgrpc "go.klarlabs.de/mnemos/internal/server/grpc"
	"go.klarlabs.de/mnemos/internal/store"
	_ "go.klarlabs.de/mnemos/internal/store/postgres"
	mnemosv1 "go.klarlabs.de/mnemos/proto/gen/mnemos/v1"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestGRPC_SharedPool_TenantMemoryDoesNotPinConnections is the live-Postgres
// half of the guard whose backend-independent half is
// TestGRPC_TenantMemory_ReleasedPerRequestUnderSharedPool.
//
// Under MNEMOS_PG_SHARED_POOL a tenant-scoped store.Conn is one *sql.Conn
// checked out of a single process-wide pool; its Closer runs RESET
// mnemos.tenant and hands the connection back. The cognitive path cached the
// per-tenant Memory facade for the life of the process, so that Closer never
// ran: the connection was pinned, and the pool drained by one per tenant until
// nothing could check out at all.
//
// The pool here is deliberately tiny (MNEMOS_DB_MAX_CONNS=2), which is what
// makes the leak observable in a test rather than after a month in production:
//
//   - Sequential same-tenant requests must not grow the backend count. One
//     tenant only ever leaks one connection, so this phase is the weaker of the
//     two — it pins "a request does not cost a connection", sampled from
//     pg_stat_activity.
//   - Sequential requests across more tenants than the pool can hold is the
//     sharp one. Each pinned facade permanently subtracts a connection, so by
//     the third tenant nothing can check out and every RPC dies on its deadline.
//     Every call below therefore carries one: without it the leak hangs the test
//     instead of failing it.
//
// RESET is observed through its effect rather than by reading another backend's
// GUC (which Postgres does not expose): with the pool this small, a later tenant
// is certain to be handed a physical connection an earlier one just used, and it
// must still see none of that tenant's rows.
func TestGRPC_SharedPool_TenantMemoryDoesNotPinConnections(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping shared-pool connection-pinning test")
	}
	t.Setenv("MNEMOS_PG_SHARED_POOL", "1")
	t.Setenv("MNEMOS_DB_MAX_CONNS", "2")
	t.Setenv("MNEMOS_DB_MAX_IDLE_CONNS", "2")

	ctx := context.Background()
	ns := fmt.Sprintf("grpc_pool_%d", time.Now().UnixNano())
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	baseDSN := dsn + sep + "namespace=" + ns

	base, err := store.Open(ctx, baseDSN)
	if err != nil {
		t.Fatalf("open base: %v", err)
	}
	t.Cleanup(func() {
		if raw, ok := base.Raw.(interface {
			ExecContext(context.Context, string, ...any) (any, error)
		}); ok {
			_, _ = raw.ExecContext(context.Background(), fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", ns))
		}
		_ = base.Close()
	})

	// backends counts this database's server-side connections. Growth in it
	// across the request loop is the leak, made visible.
	querier, ok := base.Raw.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	if !ok {
		t.Fatal("postgres conn does not expose QueryRowContext")
	}
	backends := func() int {
		var n int
		if err := querier.QueryRowContext(ctx,
			"SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()").Scan(&n); err != nil {
			t.Fatalf("count backends: %v", err)
		}
		return n
	}

	// The connection-pinning half of this test is about pool accounting and
	// runs on any role. The cross-tenant half at the end needs RLS actually
	// enforced, which a superuser (or a rolbypassrls role) does not do — CI's
	// Postgres service runs as one, so that assertion is reported and skipped
	// there rather than failing on a leak the role itself created.
	rlsEnforced := true
	var bypass bool
	if err := querier.QueryRowContext(ctx,
		"SELECT rolbypassrls OR rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&bypass); err == nil && bypass {
		rlsEnforced = false
	}

	// A real facade over the same DSN: .Tenant(id) is what checks a connection
	// out of the shared pool.
	mem, err := mnemos.New(mnemos.WithStorage(baseDSN))
	if err != nil {
		t.Fatalf("build facade: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })

	secret, err := auth.GenerateSecret()
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	issuer := auth.NewIssuer(secret)
	verifier := auth.NewVerifier(secret, base.RevokedTokens)
	user := domain.User{ID: "usr_pool", Status: domain.UserStatusActive, CreatedAt: time.Now()}
	token := func(tenant string) context.Context {
		tok, _, err := issuer.IssueUserTokenWithTenant(user, tenant, time.Hour)
		if err != nil {
			t.Fatalf("issue %s: %v", tenant, err)
		}
		return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
	}

	impl := mnemosgrpc.NewServerWithMemory(base, mem, verifier, testLogger(), "test").
		WithTenantScoping(func(ctx context.Context, tenant string) (*store.Conn, error) {
			return store.Open(ctx, baseDSN+"&tenant="+tenant)
		}, func(c *store.Conn) { _ = c.Close() })
	t.Cleanup(func() { _ = impl.Close() })
	gs := grpclib.NewServer(grpclib.UnaryInterceptor(impl.UnaryInterceptor()))
	impl.Register(gs)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = gs.Serve(lis) }()
	// Stop, not GracefulStop: a facade's own store.Open runs on
	// context.Background(), so a handler stuck on a drained pool never returns
	// and GracefulStop would wait for it — turning a failure into a hang.
	t.Cleanup(gs.Stop)
	cc, err := grpclib.NewClient(lis.Addr().String(), grpclib.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	client := mnemosv1.NewMnemosServiceClient(cc)

	// Every RPC gets a deadline so a drained pool fails the test instead of
	// hanging it: the leak's symptom is a checkout that never completes.
	const rpcBudget = 15 * time.Second
	whoKnows := func(t *testing.T, auth context.Context, what string) error {
		t.Helper()
		rctx, cancel := context.WithTimeout(auth, rpcBudget)
		defer cancel()
		_, err := client.WhoKnows(rctx, &mnemosv1.WhoKnowsRequest{Query: what, Limit: 1})
		return err
	}

	acme := token("acme")

	// Warm up: the first request creates the shared pool and its connections,
	// so the baseline is taken after them, not before.
	if err := whoKnows(t, acme, "warm"); err != nil {
		t.Fatalf("warm-up WhoKnows: %v", err)
	}
	baseline := backends()

	const requests = 10
	for i := range requests {
		if err := whoKnows(t, acme, "q"); err != nil {
			t.Fatalf("same-tenant WhoKnows #%d: %v (pool exhausted by pinned per-tenant facades?)", i, err)
		}
	}
	if got := backends(); got > baseline+1 {
		t.Errorf("server backends grew from %d to %d over %d sequential same-tenant requests; a request must not cost a connection", baseline, got, requests)
	}

	// More tenants than the pool has connections. Each pinned facade
	// permanently subtracts one, so with the leak this cannot get past the
	// third tenant — which is precisely the production failure: every tenant
	// stalls once enough of them have been served.
	tenants := []string{"acme", "globex", "initech", "umbrella", "hooli", "soylent"}
	for _, tn := range tenants {
		auth := token(tn)
		for i := range 3 {
			if err := whoKnows(t, auth, "q"); err != nil {
				t.Fatalf("tenant %s WhoKnows #%d: %v (pool exhausted by pinned per-tenant facades?)", tn, i, err)
			}
		}
	}

	if !rlsEnforced {
		t.Log("connecting role bypasses RLS; skipping the recycled-connection isolation assertion (use a non-superuser role)")
		return
	}

	// Recycled connections must not carry the previous tenant. With a pool this
	// small every tenant above was served on a connection another had just used
	// — the RESET/SET discipline is the only thing keeping them apart.
	now := timestamppb.New(time.Now().UTC())
	appendCtx, cancelAppend := context.WithTimeout(acme, rpcBudget)
	defer cancelAppend()
	if _, err := client.AppendEpisodes(appendCtx, &mnemosv1.AppendEpisodesRequest{
		Episodes: []*mnemosv1.Episode{{Id: "ev-acme-pool", RunId: "r", SchemaVersion: "v1", Content: "acme secret", SourceInputId: "in", Timestamp: now, IngestedAt: now}},
	}); err != nil {
		t.Fatalf("acme AppendEpisodes: %v", err)
	}
	for _, tn := range tenants[1:] {
		auth := token(tn)
		if err := whoKnows(t, auth, "q"); err != nil {
			t.Fatalf("tenant %s WhoKnows after the write: %v", tn, err)
		}
		lctx, cancel := context.WithTimeout(auth, rpcBudget)
		list, err := client.ListEpisodes(lctx, &mnemosv1.ListEpisodesRequest{})
		cancel()
		if err != nil {
			t.Fatalf("tenant %s ListEpisodes: %v", tn, err)
		}
		for _, e := range list.GetEpisodes() {
			if e.GetId() == "ev-acme-pool" {
				t.Fatalf("CROSS-TENANT LEAK on a recycled connection: %s read acme's episode", tn)
			}
		}
	}
}
