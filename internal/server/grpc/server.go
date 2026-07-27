// Package grpc implements the gRPC API surface for Mnemos. It mirrors
// the HTTP REST API (serve.go) using protobuf-generated types and
// gRPC interceptors for auth, logging, and panic recovery.
package grpc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"go.klarlabs.de/bolt"
	mnemos "go.klarlabs.de/mnemos"
	"go.klarlabs.de/mnemos/internal/auth"
	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/runscope"
	"go.klarlabs.de/mnemos/internal/store"
	mnemosv1 "go.klarlabs.de/mnemos/proto/gen/mnemos/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Server implements mnemosv1.MnemosServiceServer.
type Server struct {
	mnemosv1.UnimplementedMnemosServiceServer

	conn     *store.Conn
	writer   *govwrite.Writer
	verifier *auth.Verifier
	logger   *bolt.Logger
	version  string
	// mem is the library Memory facade for the cognitive-layer RPCs (parity
	// with the HTTP surface). Nil when the facade couldn't be built — those
	// RPCs then return codes.Unavailable while storage RPCs stay up.
	mem mnemos.Memory

	// Multi-tenant mode (ADR 0007, set by serve --require-tenant). When
	// requireTenant is true, every RPC must present a token with a valid tenant;
	// openTenantConn opens a per-request tenant-scoped connection the RPC methods
	// resolve through connFor/writerFor/memFor.
	requireTenant bool
	// publicReads, when true, allows anonymous (tokenless) read RPCs in
	// single-tenant mode — an explicit operator opt-in (serve --public-reads).
	// Secure by default: when false, read RPCs require a valid token like writes.
	publicReads    bool
	openTenantConn func(ctx context.Context, tenant string) (*store.Conn, error)
	// closeTenantConn releases a conn from openTenantConn. It MUST mirror the
	// opener's ownership: when the opener returns a process-cached pool (the
	// default serve path enables a shared conn cache), a raw conn.Close() would
	// shut a pool other requests still hold. The caller supplies the cache-aware
	// closer so this matches the HTTP surface's closeConn discipline.
	closeTenantConn func(*store.Conn)
	tenantMemMu     sync.Mutex
	tenantMems      map[string]mnemos.Memory
}

// NewServer returns a gRPC server backed by the given store Conn.
// If verifier is non-nil, every RPC — reads included — requires a valid bearer
// token in the "authorization" metadata (secure by default). Call WithPublicReads
// to allow anonymous read RPCs in single-tenant mode.
//
// Every durable write the server performs routes through the governed
// govwrite.Writer (built over the borrowed conn) so the spec
// non-negotiable holds: no delivery adapter reaches a repository
// directly. The Writer borrows the conn — the caller keeps ownership and
// its existing close discipline.
func NewServer(conn *store.Conn, verifier *auth.Verifier, logger *bolt.Logger, version string) *Server {
	return NewServerWithMemory(conn, nil, verifier, logger, version)
}

// NewServerWithMemory is NewServer plus the library Memory facade used by the
// cognitive-layer RPCs. Pass nil mem to leave those RPCs returning
// codes.Unavailable (storage RPCs are unaffected).
func NewServerWithMemory(conn *store.Conn, mem mnemos.Memory, verifier *auth.Verifier, logger *bolt.Logger, version string) *Server {
	w, err := govwrite.Wrap(conn, logger)
	if err != nil {
		// Wrap fails only on a nil conn or a static plugin-registration
		// bug — both programming errors that must surface loudly rather
		// than silently degrade to an ungoverned write path.
		panic(fmt.Sprintf("grpc: build governed writer: %v", err))
	}
	return &Server{conn: conn, writer: w, verifier: verifier, logger: logger, version: version, mem: mem, tenantMems: map[string]mnemos.Memory{}}
}

// WithTenantScoping enables multi-tenant mode: every RPC requires a token with
// a valid tenant, and RPC methods run against a per-request tenant-scoped
// connection opened by openConn. Returns the server for chaining.
// closeConn is the cache-aware release for conns returned by openConn (a no-op
// for process-cached pools, a real Close for per-request conns). It MUST pair
// with openConn's ownership semantics; pass a raw func(c){ _ = c.Close() } only
// when openConn always returns a caller-owned conn.
func (s *Server) WithTenantScoping(openConn func(ctx context.Context, tenant string) (*store.Conn, error), closeConn func(*store.Conn)) *Server {
	s.requireTenant = true
	s.openTenantConn = openConn
	s.closeTenantConn = closeConn
	return s
}

// WithPublicReads opts into anonymous (tokenless) read RPCs in single-tenant
// mode. Without it, read RPCs require a valid token like writes (secure by
// default). Ignored under tenant scoping, where every RPC is authenticated.
// Returns the server for chaining.
func (s *Server) WithPublicReads() *Server {
	s.publicReads = true
	return s
}

// tenant context plumbing (per-request scoped conn/writer/memory + the
// effective tenant), stashed by the interceptor and read by the *For accessors.
type tenantConnKey struct{}
type tenantWriterKey struct{}
type tenantMemKey struct{}
type tenantKey struct{}

func withTenant(ctx context.Context, t string) context.Context {
	return context.WithValue(ctx, tenantKey{}, t)
}
func tenantFromContext(ctx context.Context) (string, bool) {
	t, ok := ctx.Value(tenantKey{}).(string)
	return t, ok && t != ""
}

// connFor returns the request's tenant-scoped conn (multi-tenant mode) or the
// shared conn.
func (s *Server) connFor(ctx context.Context) *store.Conn {
	if c, ok := ctx.Value(tenantConnKey{}).(*store.Conn); ok && c != nil {
		return c
	}
	return s.conn
}

// writerFor mirrors connFor for the governed writer.
func (s *Server) writerFor(ctx context.Context) *govwrite.Writer {
	if w, ok := ctx.Value(tenantWriterKey{}).(*govwrite.Writer); ok && w != nil {
		return w
	}
	return s.writer
}

// requestMem is a lazily-built, request-scoped tenant Memory view, stashed by
// the interceptor and closed when the RPC returns.
//
// It exists because a Memory owns a store connection, and under the Postgres
// shared-pool mode (MNEMOS_PG_SHARED_POOL, ADR 0007 Phase 2) that connection is
// ONE *sql.Conn checked out of a single process-wide pool, whose Closer resets
// the mnemos.tenant GUC and hands it back. A process-lifetime cache of such a
// facade never closes it, so the connection never returns to the pool: one
// permanently pinned connection per tenant, until the pool is exhausted and
// every tenant stalls waiting for a checkout that can never happen. The
// cmd-side read-conn cache already declines to cache under this mode
// (enableConnCache); this is the same decision for the cognitive path.
//
// Lazy rather than eagerly built in the interceptor: building a facade opens a
// connection and boots the query/chronos wiring, and most RPCs never touch the
// cognitive layer. Only a request that actually calls memFor pays for one.
type requestMem struct {
	mu    sync.Mutex
	built bool
	mem   mnemos.Memory
}

// get returns the request's tenant view, building it on first use. A build
// failure is remembered as nil so the caller's nil-guard fails the RPC closed
// rather than retrying an open that is not going to start working mid-request.
func (r *requestMem) get(base mnemos.Memory, tenant string) mnemos.Memory {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.built {
		r.built = true
		if m, err := base.Tenant(tenant); err == nil {
			r.mem = m
		}
	}
	return r.mem
}

// close releases the view and, with it, the connection the facade holds.
func (r *requestMem) close() error {
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
// requestMem for why a cached facade there pins a pooled connection forever.
// Mirrors enableConnCache's env test — same variable, same reading.
func tenantMemCacheSafe() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MNEMOS_PG_SHARED_POOL"))) {
	case "1", "true", "yes":
		return false
	}
	return true
}

// memFor returns a per-tenant Memory view or the shared facade. When a tenant IS
// present but its view can't be opened, it returns nil (fail closed) so the
// cognitive RPCs' nil-guard yields codes.Unavailable — never the shared
// __default__ facade, which would serve the wrong partition's data.
//
// The view is normally cached per tenant for the life of the process. When the
// interceptor decided caching is unsafe it stashed a request-scoped holder
// instead (see requestMem); this reads that first, so every call within one RPC
// still gets the same instance and it is closed when the RPC returns.
func (s *Server) memFor(ctx context.Context) mnemos.Memory {
	tenant, ok := tenantFromContext(ctx)
	if !ok || s.mem == nil {
		return s.mem
	}
	if rm, ok := ctx.Value(tenantMemKey{}).(*requestMem); ok && rm != nil {
		return rm.get(s.mem, tenant)
	}

	s.tenantMemMu.Lock()
	m, cached := s.tenantMems[tenant]
	s.tenantMemMu.Unlock()
	if cached {
		return m
	}

	// Built WITHOUT the lock held. Tenant() opens a connection, so holding the
	// mutex across it made one slow dial block every other tenant's memFor —
	// and Close(), which takes the same mutex, so a shutdown during a stalled
	// open hung instead of proceeding. Same shape as openConn's cache in
	// cmd/mnemos/dsn.go (#150).
	tm, err := s.mem.Tenant(tenant)
	if err != nil {
		return nil
	}
	s.tenantMemMu.Lock()
	if existing, ok := s.tenantMems[tenant]; ok {
		// Lost the race to open the same tenant concurrently — keep the winner
		// and close our redundant view, which owns a connection of its own.
		s.tenantMemMu.Unlock()
		_ = tm.Close()
		return existing
	}
	s.tenantMems[tenant] = tm
	s.tenantMemMu.Unlock()
	return tm
}

// Close releases what the server itself allocated: the per-tenant Memory views
// memFor built and cached. Each of those owns a store connection, and nothing
// ever released them — a long-lived multi-tenant process leaked one per tenant
// it had ever served. The HTTP surface closes its equivalent map; this one had
// no way to be closed at all.
//
// The borrowed conn, the shared facade and the writer are deliberately left
// alone: NewServer borrows them and the caller keeps its close discipline.
//
// Safe to call twice, and safe to call while requests are in flight: the cache
// is detached under the same mutex memFor takes, so a second call finds it
// empty and a concurrent memFor simply rebuilds a view rather than handing back
// one that is being closed.
func (s *Server) Close() error {
	s.tenantMemMu.Lock()
	cached := s.tenantMems
	s.tenantMems = map[string]mnemos.Memory{}
	s.tenantMemMu.Unlock()

	var errs []error
	for tenant, m := range cached {
		if m == nil {
			continue
		}
		if err := m.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close memory for tenant %q: %w", tenant, err))
		}
	}
	return errors.Join(errs...)
}

// Register registers the Mnemos service on the provided gRPC server.
func (s *Server) Register(gs *grpc.Server) {
	mnemosv1.RegisterMnemosServiceServer(gs, s)
}

// ---------------------------------------------------------------------------
// Interceptors
// ---------------------------------------------------------------------------

// UnaryInterceptor returns a grpc.UnaryServerInterceptor chain that
// handles auth, panic recovery, and access logging.
func (s *Server) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		start := time.Now()

		// Panic recovery
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error().
					Str("method", info.FullMethod).
					Str("panic", fmt.Sprintf("%v", rec)).
					Str("stack", string(debug.Stack())).
					Msg("grpc_panic")
				resp = nil
				err = status.Errorf(codes.Internal, "internal error: panic")
			}
		}()

		// Auth
		ctx, authErr := s.authenticate(ctx, info.FullMethod)
		if authErr != nil {
			return nil, authErr
		}

		// Multi-tenant: open a per-request tenant-scoped connection (RLS) and a
		// governed writer over it, closed when the RPC returns.
		if tenant, ok := tenantFromContext(ctx); ok && s.openTenantConn != nil {
			tconn, terr := s.openTenantConn(ctx, tenant)
			if terr != nil {
				return nil, status.Errorf(codes.Internal, "tenant store unavailable")
			}
			// Release through the supplied cache-aware closer, never a raw
			// tconn.Close(): the default serve path returns a process-cached pool
			// that other in-flight requests still hold.
			defer func() {
				if s.closeTenantConn != nil {
					s.closeTenantConn(tconn)
				} else {
					_ = tconn.Close()
				}
			}()
			ctx = context.WithValue(ctx, tenantConnKey{}, tconn)
			// Wrap borrows tconn (ownConn=false); closing the writer releases
			// only the kernel evidence sink (a real fd when
			// MNEMOS_AXI_EVIDENCE_LOG is set, a no-op otherwise). tconn is
			// closed separately above. In tenant mode a Wrap failure must fail
			// the RPC closed rather than fall back to the __default__ writer.
			tw, werr := govwrite.Wrap(tconn, s.logger)
			if werr != nil {
				return nil, status.Errorf(codes.Internal, "tenant writer unavailable")
			}
			defer func() { _ = tw.Close() }()
			ctx = context.WithValue(ctx, tenantWriterKey{}, tw)
		}

		// The cognitive path's per-tenant Memory view. Cached process-wide by
		// default; under the Postgres shared-pool mode a cached facade would pin
		// its pooled connection for good, so scope it to the request instead and
		// close it here. Stashed for any tenant request (not only tenant-scoping
		// mode) so the lifetime rule holds wherever a tenant reaches memFor.
		if _, ok := tenantFromContext(ctx); ok && s.mem != nil && !tenantMemCacheSafe() {
			rm := &requestMem{}
			defer func() {
				if cerr := rm.close(); cerr != nil {
					s.logger.Warn().Str("method", info.FullMethod).Str("error", cerr.Error()).Msg("close request memory")
				}
			}()
			ctx = context.WithValue(ctx, tenantMemKey{}, rm)
		}

		resp, err = handler(ctx, req)

		actor := actorFromContext(ctx)
		s.logger.Info().
			Str("method", info.FullMethod).
			Str("code", status.Code(err).String()).
			Dur("duration", time.Since(start)).
			Str("user_id", actor).
			Msg("grpc_request")

		return resp, err
	}
}

// authenticate extracts the bearer token from gRPC metadata, validates
// it when a verifier is configured, and attaches actor/scopes/runs to
// the context. Read methods (List*, Metrics, Health) skip auth when no
// token is present.
func (s *Server) authenticate(ctx context.Context, method string) (context.Context, error) {
	if s.verifier == nil {
		return ctx, nil
	}

	// Multi-tenant mode: every method (reads included) must present a token so
	// its tenant can be resolved — no anonymous tenant.
	if s.requireTenant {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Errorf(codes.Unauthenticated, "missing authorization metadata")
		}
		vals := md.Get("authorization")
		if len(vals) == 0 {
			return nil, status.Errorf(codes.Unauthenticated, "missing bearer token")
		}
		return s.validateToken(ctx, vals[0])
	}

	// Read methods allow anonymous access ONLY when the operator opted into a
	// public read API (WithPublicReads); a token, if present, is still validated
	// for created_by/scope attribution. Secure by default: without the opt-in,
	// reads require a token exactly like writes (fall through below).
	if s.publicReads && isReadMethod(method) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return ctx, nil
		}
		vals := md.Get("authorization")
		if len(vals) == 0 {
			return ctx, nil
		}
		return s.validateToken(ctx, vals[0])
	}

	// Write methods require a token.
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.Unauthenticated, "missing authorization metadata")
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return nil, status.Errorf(codes.Unauthenticated, "missing bearer token")
	}
	return s.validateToken(ctx, vals[0])
}

func (s *Server) validateToken(ctx context.Context, raw string) (context.Context, error) {
	const prefix = "Bearer "
	if !hasPrefix(raw, prefix) {
		return nil, status.Errorf(codes.Unauthenticated, "invalid authorization format")
	}
	tokenStr := raw[len(prefix):]
	if tokenStr == "" {
		return nil, status.Errorf(codes.Unauthenticated, "missing bearer token")
	}

	claims, err := s.verifier.ParseAndValidate(ctx, tokenStr)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid or revoked token")
	}

	ctx = withActor(ctx, claims.UserID)
	ctx = withScopes(ctx, claims.Scopes)
	ctx = withAllowedRuns(ctx, claims.Runs)
	if s.requireTenant {
		// A request may select a tenant (x-mnemos-tenant metadata) within the
		// token's tnt/tnts grant (ADR 0009); else the token's single tenant.
		requested := ""
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if v := md.Get("x-mnemos-tenant"); len(v) > 0 {
				requested = v[0]
			}
		}
		eff, ok := claims.ResolveTenant(requested)
		if !ok {
			return nil, status.Errorf(codes.Unauthenticated, "not authorized for the requested tenant (needs a matching tnt/tnts grant)")
		}
		ctx = withTenant(ctx, eff)
	}
	return ctx, nil
}

func isReadMethod(method string) bool {
	switch method {
	case "/mnemos.v1.MnemosService/Health",
		"/mnemos.v1.MnemosService/ListEpisodes",
		"/mnemos.v1.MnemosService/ListBeliefs",
		"/mnemos.v1.MnemosService/ListAssociations",
		"/mnemos.v1.MnemosService/ListEmbeddings",
		"/mnemos.v1.MnemosService/Metrics":
		return true
	}
	return false
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// ---------------------------------------------------------------------------
// Auth context helpers (mirror serve_auth.go)
// ---------------------------------------------------------------------------

type actorContextKey struct{}
type scopesContextKey struct{}
type runsContextKey struct{}

func withActor(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, actorContextKey{}, userID)
}

func withScopes(ctx context.Context, scopes []string) context.Context {
	return context.WithValue(ctx, scopesContextKey{}, scopes)
}

func withAllowedRuns(ctx context.Context, runs []string) context.Context {
	return context.WithValue(ctx, runsContextKey{}, runs)
}

func actorFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(actorContextKey{}).(string); ok && v != "" {
		return v
	}
	return domain.SystemUser
}

func scopesFromContext(ctx context.Context) []string {
	if v, ok := ctx.Value(scopesContextKey{}).([]string); ok {
		return v
	}
	return nil
}

func allowedRunsFromContext(ctx context.Context) []string {
	if v, ok := ctx.Value(runsContextKey{}).([]string); ok {
		return v
	}
	return nil
}

func (s *Server) requireScope(ctx context.Context, want string) error {
	// When no verifier is configured, auth is disabled — allow all.
	if s.verifier == nil {
		return nil
	}
	for _, s := range scopesFromContext(ctx) {
		if s == domain.ScopeWildcard || s == want {
			return nil
		}
	}
	return status.Errorf(codes.PermissionDenied, "missing required scope: %s", want)
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

// Health is the gRPC health probe. Returns the running version and a human-readable status string.
func (s *Server) Health(ctx context.Context, req *mnemosv1.HealthRequest) (*mnemosv1.HealthResponse, error) {
	if !req.Detailed {
		return &mnemosv1.HealthResponse{Status: "ok", Version: s.version}, nil
	}
	return &mnemosv1.HealthResponse{Status: "ok", Version: s.version, Healthy: true}, nil
}

// ---------------------------------------------------------------------------
// Episodes
// ---------------------------------------------------------------------------

// ListEpisodes returns episodes ordered by timestamp ascending. Pagination via Limit/PageToken (cursor = last episode id).
func (s *Server) ListEpisodes(ctx context.Context, req *mnemosv1.ListEpisodesRequest) (*mnemosv1.ListEpisodesResponse, error) {
	limit, offset := normalizePagination(req.Pagination)

	all, err := s.connFor(ctx).Events.ListAll(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list episodes: %v", err)
	}

	// F.4.b on the read side. A run-scoped bearer could call this and get the
	// entire episode log, which made the whitelist a write-only boundary: the
	// data it was meant to protect was one unfiltered list call away.
	//
	// ListEpisodesRequest has no run_id field, so "reject a request for someone
	// else's run" is not expressible here — the only fail-closed reading is to
	// intersect the result with the whitelist. Episodes with no run at all are
	// dropped too: a run-scoped token was granted named runs, and "unassigned"
	// is not one of them.
	if allowed := allowedRunsFromContext(ctx); len(allowed) > 0 {
		kept := all[:0]
		for _, e := range all {
			if runAllowed(allowed, e.RunID) {
				kept = append(kept, e)
			}
		}
		all = kept
	}

	reversed := make([]domain.Event, len(all))
	for i, e := range all {
		reversed[len(all)-1-i] = e
	}
	total := len(reversed)
	page := paginate(reversed, limit, offset)

	episodes := make([]*mnemosv1.Episode, 0, len(page))
	for _, e := range page {
		episodes = append(episodes, episodeToProto(e))
	}
	return &mnemosv1.ListEpisodesResponse{Episodes: episodes, Total: int32(total), Limit: int32(limit), Offset: int32(offset)}, nil
}

// AppendEpisodes writes episodes idempotently. Re-appending the same id is a no-op (mirrors REST semantics).
func (s *Server) AppendEpisodes(ctx context.Context, req *mnemosv1.AppendEpisodesRequest) (*mnemosv1.AppendResponse, error) {
	if err := s.requireScope(ctx, domain.ScopeEventsWrite); err != nil {
		return nil, err
	}
	if len(req.Episodes) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "episodes array is empty")
	}
	if len(req.Episodes) > maxBatchRecords {
		return nil, status.Errorf(codes.InvalidArgument, "episodes batch size %d exceeds max %d", len(req.Episodes), maxBatchRecords)
	}

	// F.4.b: the run id here is caller-supplied and written verbatim, so a
	// run-scoped bearer could mint episodes into any run it liked — including
	// runs it is forbidden to read — and then hang evidence off them. Checked
	// against the whitelist directly (no store lookup: the run is in the
	// payload) and BEFORE the write, so a rejected batch leaves nothing behind.
	if allowed := allowedRunsFromContext(ctx); len(allowed) > 0 {
		for i, e := range req.Episodes {
			if !runAllowed(allowed, e.RunId) {
				return nil, status.Errorf(codes.PermissionDenied,
					"episodes[%d] %q (run %q) not in token whitelist", i, e.Id, e.RunId)
			}
		}
	}

	events := make([]domain.Event, 0, len(req.Episodes))
	now := time.Now().UTC()
	actor := actorFromContext(ctx)
	for i, e := range req.Episodes {
		if e.Id == "" {
			return nil, status.Errorf(codes.InvalidArgument, "episodes[%d].id is required", i)
		}
		ts := now
		if e.Timestamp != nil {
			ts = e.Timestamp.AsTime()
		}
		ingested := now
		if e.IngestedAt != nil {
			ingested = e.IngestedAt.AsTime()
		}
		events = append(events, domain.Event{
			ID:            e.Id,
			RunID:         e.RunId,
			SchemaVersion: e.SchemaVersion,
			Content:       e.Content,
			SourceInputID: e.SourceInputId,
			Timestamp:     ts,
			Metadata:      e.Metadata,
			IngestedAt:    ingested,
			CreatedBy:     actor,
		})
	}

	accepted, err := s.writerFor(ctx).Events(ctx, events)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "append episodes: %v", err)
	}
	return &mnemosv1.AppendResponse{Accepted: int32(accepted)}, nil
}

// ---------------------------------------------------------------------------
// Beliefs
// ---------------------------------------------------------------------------

// ListBeliefs returns beliefs with optional type/status/run_id filters and
// cursor-based pagination. The run_id filter is the load-bearing tenant
// boundary for integrators: beliefs whose evidence cannot be traced to an
// episode with the matching RunID are dropped (fail-closed).
func (s *Server) ListBeliefs(ctx context.Context, req *mnemosv1.ListBeliefsRequest) (*mnemosv1.ListBeliefsResponse, error) {
	limit, offset := normalizePagination(req.Pagination)
	typeFilter := req.TypeFilter
	statusFilter := req.StatusFilter
	runIDFilter := req.RunId

	if typeFilter != "" && !validClaimType(typeFilter) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid type %q", typeFilter)
	}
	if statusFilter != "" && !validClaimStatus(statusFilter) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid status %q", statusFilter)
	}

	// F.4.b on the read side. run_id was an OPTIONAL filter the caller chose,
	// never a boundary the token imposed: a run-scoped bearer that simply
	// omitted it read every belief in the store. The whitelist now decides
	// which runs are legible, and run_id only narrows within it.
	//
	// Naming a run outside the whitelist is refused rather than silently
	// answered with an empty page — a caller that cannot tell "no beliefs" from
	// "not yours" will read the first as fact.
	allowedRuns := allowedRunsFromContext(ctx)
	var runFilters []string
	switch {
	case runIDFilter != "":
		if !runAllowed(allowedRuns, runIDFilter) {
			return nil, status.Errorf(codes.PermissionDenied, "run_id %q not in token whitelist", runIDFilter)
		}
		runFilters = []string{runIDFilter}
	case len(allowedRuns) > 0:
		runFilters = allowedRuns
	}

	// Build the allowed-event set from the effective runs. Empty set →
	// no claims belong to them; return early to avoid leaking
	// unfiltered claims (ADR-002 / Thor's safety audit).
	var allowedEventIDs map[string]struct{}
	if len(runFilters) > 0 {
		allowedEventIDs = map[string]struct{}{}
		for _, run := range runFilters {
			events, err := s.connFor(ctx).Events.ListByRunID(ctx, run)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "list episodes by run id: %v", err)
			}
			for _, e := range events {
				allowedEventIDs[e.ID] = struct{}{}
			}
		}
		if len(allowedEventIDs) == 0 {
			return &mnemosv1.ListBeliefsResponse{Limit: int32(limit), Offset: int32(offset)}, nil
		}
	}

	all, err := s.connFor(ctx).Claims.ListAll(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list beliefs: %v", err)
	}
	var asOf, recordedAsOf time.Time
	if req.AsOf != nil {
		asOf = req.AsOf.AsTime()
	}
	if req.RecordedAsOf != nil {
		recordedAsOf = req.RecordedAsOf.AsTime()
	}
	filtered := all[:0]
	for _, c := range all {
		if typeFilter != "" && string(c.Type) != typeFilter {
			continue
		}
		if statusFilter != "" && string(c.Status) != statusFilter {
			continue
		}
		// Validity-time filter: claim must have been valid at as_of.
		// IsValidAt treats zero ValidFrom as "valid since forever".
		if !asOf.IsZero() && !c.IsValidAt(asOf) {
			continue
		}
		// Ingestion-time filter: drop rows recorded after the query
		// timestamp so the response is reproducible from the snapshot
		// of the store as it stood then.
		if !recordedAsOf.IsZero() && c.CreatedAt.After(recordedAsOf) {
			continue
		}
		filtered = append(filtered, c)
	}

	// run_id post-filter: drop claims whose evidence does not link to
	// an allowed event. Done after the cheaper type/status filters so
	// evidence is loaded only for surviving candidates.
	if allowedEventIDs != nil && len(filtered) > 0 {
		candidateIDs := make([]string, 0, len(filtered))
		for _, c := range filtered {
			candidateIDs = append(candidateIDs, c.ID)
		}
		evLinks, err := s.connFor(ctx).Claims.ListEvidenceByClaimIDs(ctx, candidateIDs)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "load evidence for run_id filter: %v", err)
		}
		eventsByClaim := make(map[string][]string, len(evLinks))
		for _, link := range evLinks {
			eventsByClaim[link.ClaimID] = append(eventsByClaim[link.ClaimID], link.EventID)
		}
		kept := filtered[:0]
		for _, c := range filtered {
			matched := false
			for _, eid := range eventsByClaim[c.ID] {
				if _, ok := allowedEventIDs[eid]; ok {
					matched = true
					break
				}
			}
			if matched {
				kept = append(kept, c)
			}
		}
		filtered = kept
	}
	reversed := make([]domain.Claim, len(filtered))
	for i, c := range filtered {
		reversed[len(filtered)-1-i] = c
	}
	total := len(reversed)
	page := paginate(reversed, limit, offset)

	beliefs := make([]*mnemosv1.Belief, 0, len(page))
	ids := make([]string, 0, len(page))
	for _, c := range page {
		beliefs = append(beliefs, beliefToProto(c))
		ids = append(ids, c.ID)
	}

	var evidence []*mnemosv1.BeliefEvidence
	if req.IncludeEvidence && len(ids) > 0 {
		links, err := s.connFor(ctx).Claims.ListEvidenceByClaimIDs(ctx, ids)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "load evidence: %v", err)
		}
		for _, l := range links {
			evidence = append(evidence, &mnemosv1.BeliefEvidence{BeliefId: l.ClaimID, EpisodeId: l.EventID})
		}
	}

	return &mnemosv1.ListBeliefsResponse{Beliefs: beliefs, Evidence: evidence, Total: int32(total), Limit: int32(limit), Offset: int32(offset)}, nil
}

// AppendBeliefs upserts beliefs and their evidence links in a single batched call.
func (s *Server) AppendBeliefs(ctx context.Context, req *mnemosv1.AppendBeliefsRequest) (*mnemosv1.AppendResponse, error) {
	if err := s.requireScope(ctx, domain.ScopeClaimsWrite); err != nil {
		return nil, err
	}
	if len(req.Beliefs) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "beliefs array is empty")
	}
	if len(req.Beliefs) > maxBatchRecords {
		return nil, status.Errorf(codes.InvalidArgument, "beliefs batch size %d exceeds max %d", len(req.Beliefs), maxBatchRecords)
	}
	if len(req.Evidence) > maxBatchRecords {
		return nil, status.Errorf(codes.InvalidArgument, "evidence batch size %d exceeds max %d", len(req.Evidence), maxBatchRecords)
	}

	claims := make([]domain.Claim, 0, len(req.Beliefs))
	now := time.Now().UTC()
	actor := actorFromContext(ctx)
	for i, c := range req.Beliefs {
		if c.Type != "" && !validClaimType(c.Type) {
			return nil, status.Errorf(codes.InvalidArgument, "beliefs[%d].type %q invalid", i, c.Type)
		}
		if c.Status != "" && !validClaimStatus(c.Status) {
			return nil, status.Errorf(codes.InvalidArgument, "beliefs[%d].status %q invalid", i, c.Status)
		}
		if c.Visibility != "" && !validClaimVisibility(c.Visibility) {
			return nil, status.Errorf(codes.InvalidArgument, "beliefs[%d].visibility %q invalid; must be personal, team, or org", i, c.Visibility)
		}
		created := now
		if c.CreatedAt != nil {
			created = c.CreatedAt.AsTime()
		}
		claims = append(claims, domain.Claim{
			ID:         c.Id,
			Text:       c.Text,
			Type:       domain.ClaimType(c.Type),
			Confidence: c.Confidence,
			Status:     domain.ClaimStatus(c.Status),
			CreatedAt:  created,
			CreatedBy:  actor,
			Visibility: domain.Visibility(c.Visibility),
		})
	}

	// F.4.b: a run-scoped bearer may only attach evidence from runs it is
	// allowed to see. Checked BEFORE any write so a rejection cannot leave
	// orphan claims behind.
	//
	// This returned Unimplemented — fail-closed and honest, but it meant REST
	// enforced a boundary gRPC could not, so the same token behaved differently
	// depending on the transport. The check now lives in internal/runscope and
	// both call the one implementation; a security check with two copies
	// eventually has two behaviours.
	if allowed := allowedRunsFromContext(ctx); len(allowed) > 0 && len(req.Evidence) > 0 {
		eventIDs := make([]string, 0, len(req.Evidence))
		for _, e := range req.Evidence {
			eventIDs = append(eventIDs, e.EpisodeId)
		}

		bad, badRun, err := runscope.CheckEventRunsAllowed(ctx, s.connFor(ctx), eventIDs, allowed)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "run-scope check: %v", err)
		}

		if bad != "" {
			return nil, status.Errorf(codes.PermissionDenied,
				"evidence episode %q (run %q) not in token whitelist", bad, badRun)
		}
	}

	// Same attribution as the REST and CLI write paths; the zero value left
	// gRPC-written claims with no status_history row.
	if _, err := s.writerFor(ctx).Claims(ctx, claims, govwrite.ClaimReason{
		Reason:    "grpc: AppendBeliefs",
		ChangedBy: actor,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "upsert beliefs: %v", err)
	}

	if len(req.Evidence) > 0 {
		links := make([]domain.ClaimEvidence, 0, len(req.Evidence))
		for _, e := range req.Evidence {
			links = append(links, domain.ClaimEvidence{ClaimID: e.BeliefId, EventID: e.EpisodeId})
		}
		if _, err := s.writerFor(ctx).EvidenceLinks(ctx, links); err != nil {
			return nil, status.Errorf(codes.Internal, "upsert evidence: %v", err)
		}
	}
	return &mnemosv1.AppendResponse{Accepted: int32(len(claims))}, nil
}

// ---------------------------------------------------------------------------
// Associations
// ---------------------------------------------------------------------------

// ListAssociations returns associations filtered by type/from/to with cursor pagination.
func (s *Server) ListAssociations(ctx context.Context, req *mnemosv1.ListAssociationsRequest) (*mnemosv1.ListAssociationsResponse, error) {
	limit, offset := normalizePagination(req.Pagination)
	typeFilter := req.TypeFilter
	if typeFilter != "" && typeFilter != "supports" && typeFilter != "contradicts" {
		return nil, status.Errorf(codes.InvalidArgument, "invalid type %q (want supports or contradicts)", typeFilter)
	}

	allClaims, err := s.connFor(ctx).Claims.ListAll(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list beliefs for associations: %v", err)
	}
	claimIDs := make([]string, 0, len(allClaims))
	for _, c := range allClaims {
		claimIDs = append(claimIDs, c.ID)
	}
	rels, err := s.connFor(ctx).Relationships.ListByClaimIDs(ctx, claimIDs)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list associations: %v", err)
	}
	filtered := rels[:0]
	for _, rel := range rels {
		if typeFilter != "" && string(rel.Type) != typeFilter {
			continue
		}
		filtered = append(filtered, rel)
	}
	reversed := make([]domain.Relationship, len(filtered))
	for i, rel := range filtered {
		reversed[len(filtered)-1-i] = rel
	}
	total := len(reversed)
	page := paginate(reversed, limit, offset)

	out := make([]*mnemosv1.Association, 0, len(page))
	for _, rel := range page {
		out = append(out, associationToProto(rel))
	}
	return &mnemosv1.ListAssociationsResponse{Associations: out, Total: int32(total), Limit: int32(limit), Offset: int32(offset)}, nil
}

// AppendAssociations upserts association rows. Idempotent on the (type, from, to) unique edge.
func (s *Server) AppendAssociations(ctx context.Context, req *mnemosv1.AppendAssociationsRequest) (*mnemosv1.AppendResponse, error) {
	if err := s.requireScope(ctx, domain.ScopeRelationshipsWrite); err != nil {
		return nil, err
	}
	if len(req.Associations) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "associations array is empty")
	}
	if len(req.Associations) > maxBatchRecords {
		return nil, status.Errorf(codes.InvalidArgument, "associations batch size %d exceeds max %d", len(req.Associations), maxBatchRecords)
	}

	rels := make([]domain.Relationship, 0, len(req.Associations))
	now := time.Now().UTC()
	actor := actorFromContext(ctx)
	for i, rel := range req.Associations {
		if rel.Type != "supports" && rel.Type != "contradicts" {
			return nil, status.Errorf(codes.InvalidArgument, "associations[%d].type %q invalid", i, rel.Type)
		}
		created := now
		if rel.CreatedAt != nil {
			created = rel.CreatedAt.AsTime()
		}
		rels = append(rels, domain.Relationship{
			ID:          rel.Id,
			Type:        domain.RelationshipType(rel.Type),
			FromClaimID: rel.FromBeliefId,
			ToClaimID:   rel.ToBeliefId,
			CreatedAt:   created,
			CreatedBy:   actor,
		})
	}

	// F.4.b: an association is a claim about two beliefs, so writing one into
	// another run's graph is a write into that run. Both endpoints' evidence
	// must resolve to episodes inside the whitelist — checked before the write,
	// mirroring the REST relationship handler.
	if allowed := allowedRunsFromContext(ctx); len(allowed) > 0 {
		claimIDs := make([]string, 0, len(rels)*2)
		for _, rel := range rels {
			claimIDs = append(claimIDs, rel.FromClaimID, rel.ToClaimID)
		}
		evIDs, err := claimEventIDs(ctx, s.connFor(ctx), claimIDs)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "run-scope lookup: %v", err)
		}
		bad, badRun, err := checkEventRunsAllowed(ctx, s.connFor(ctx), evIDs, allowed)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "run-scope check: %v", err)
		}
		if bad != "" {
			return nil, status.Errorf(codes.PermissionDenied,
				"association references episode %q (run %q) not in token whitelist", bad, badRun)
		}
	}

	if _, err := s.writerFor(ctx).Relationships(ctx, rels); err != nil {
		return nil, status.Errorf(codes.Internal, "upsert associations: %v", err)
	}
	return &mnemosv1.AppendResponse{Accepted: int32(len(rels))}, nil
}

// ---------------------------------------------------------------------------
// Embeddings
// ---------------------------------------------------------------------------

// ListEmbeddings returns embedding rows filtered by entity_type with cursor pagination.
func (s *Server) ListEmbeddings(ctx context.Context, req *mnemosv1.ListEmbeddingsRequest) (*mnemosv1.ListEmbeddingsResponse, error) {
	limit, offset := normalizePagination(req.Pagination)
	typeFilter := req.EntityType
	if typeFilter != "" && typeFilter != "event" && typeFilter != "claim" {
		return nil, status.Errorf(codes.InvalidArgument, "invalid entity_type %q (want event or claim)", typeFilter)
	}

	var records []domain.EmbeddingRecord
	wantedTypes := []string{typeFilter}
	if typeFilter == "" {
		wantedTypes = []string{"event", "claim"}
	}
	for _, t := range wantedTypes {
		recs, err := s.connFor(ctx).Embeddings.ListByEntityType(ctx, t)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list embeddings: %v", err)
		}
		records = append(records, recs...)
	}
	total := len(records)
	page := paginate(records, limit, offset)

	out := make([]*mnemosv1.Embedding, 0, len(page))
	for _, rec := range page {
		out = append(out, embeddingToProto(rec))
	}
	return &mnemosv1.ListEmbeddingsResponse{Embeddings: out, Total: int32(total), Limit: int32(limit), Offset: int32(offset)}, nil
}

// AppendEmbeddings upserts vector rows. Re-writing the same (entity_id, entity_type) replaces the vector.
func (s *Server) AppendEmbeddings(ctx context.Context, req *mnemosv1.AppendEmbeddingsRequest) (*mnemosv1.AppendResponse, error) {
	if err := s.requireScope(ctx, domain.ScopeEmbeddingsWrite); err != nil {
		return nil, err
	}
	if len(req.Embeddings) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "embeddings array is empty")
	}
	if len(req.Embeddings) > maxBatchRecords {
		return nil, status.Errorf(codes.InvalidArgument, "embeddings batch size %d exceeds max %d", len(req.Embeddings), maxBatchRecords)
	}

	// F.4.b: an embedding is a searchable index entry for its entity, so
	// writing one for another run's episode or belief plants a row in that
	// run's retrieval surface. Event entities resolve to a run directly; claim
	// entities resolve through their evidence. Checked before the loop below,
	// which writes each row as it goes — a mid-loop rejection would leave a
	// partial batch behind.
	if allowed := allowedRunsFromContext(ctx); len(allowed) > 0 {
		var eventIDs, claimIDs []string
		for _, e := range req.Embeddings {
			switch e.EntityType {
			case "event":
				eventIDs = append(eventIDs, e.EntityId)
			case "claim":
				claimIDs = append(claimIDs, e.EntityId)
			}
		}
		extraEvents, err := claimEventIDs(ctx, s.connFor(ctx), claimIDs)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "run-scope lookup: %v", err)
		}
		eventIDs = append(eventIDs, extraEvents...)
		bad, badRun, err := checkEventRunsAllowed(ctx, s.connFor(ctx), eventIDs, allowed)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "run-scope check: %v", err)
		}
		if bad != "" {
			return nil, status.Errorf(codes.PermissionDenied,
				"embedding entity references episode %q (run %q) not in token whitelist", bad, badRun)
		}
	}

	actor := actorFromContext(ctx)
	accepted := 0
	for i, e := range req.Embeddings {
		if e.EntityId == "" {
			return nil, status.Errorf(codes.InvalidArgument, "embeddings[%d].entity_id is required", i)
		}
		if e.EntityType != "event" && e.EntityType != "claim" {
			return nil, status.Errorf(codes.InvalidArgument, "embeddings[%d].entity_type %q invalid", i, e.EntityType)
		}
		if len(e.Vector) == 0 {
			return nil, status.Errorf(codes.InvalidArgument, "embeddings[%d].vector is empty", i)
		}
		if e.Dimensions != 0 && int(e.Dimensions) != len(e.Vector) {
			return nil, status.Errorf(codes.InvalidArgument, "embeddings[%d]: dimensions=%d but vector length=%d", i, e.Dimensions, len(e.Vector))
		}
		if err := s.writerFor(ctx).Embedding(ctx, e.EntityId, e.EntityType, e.Vector, e.Model, actor); err != nil {
			return nil, status.Errorf(codes.Internal, "upsert embedding: %v", err)
		}
		accepted++
	}
	return &mnemosv1.AppendResponse{Accepted: int32(accepted)}, nil
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

// Metrics returns aggregate counts (events, claims, contradictions, embeddings) and the running version.
//
// F.4.b on the read side. These are aggregates, but an aggregate over runs the
// token cannot read is still a read of them: "how many episodes exist" and "how
// many beliefs are contested" leak the size and health of every other run's
// graph, and differencing successive calls leaks its activity. MetricsRequest
// carries no run_id, so — exactly as in ListEpisodes — "reject a request for
// someone else's run" is not expressible and the only fail-closed reading is to
// compute the aggregates over the whitelist alone. Episodes with no run at all
// are excluded: a run-scoped token was granted named runs, and "unassigned" is
// not one of them.
func (s *Server) Metrics(ctx context.Context, _ *mnemosv1.MetricsRequest) (*mnemosv1.MetricsResponse, error) {
	events, _ := s.connFor(ctx).Events.ListAll(ctx)
	claims, _ := s.connFor(ctx).Claims.ListAll(ctx)
	eventEmbs, _ := s.connFor(ctx).Embeddings.ListByEntityType(ctx, "event")
	claimEmbs, _ := s.connFor(ctx).Embeddings.ListByEntityType(ctx, "claim")

	if allowed := allowedRunsFromContext(ctx); len(allowed) > 0 {
		keptEvents := events[:0]
		allowedEventIDs := map[string]struct{}{}
		for _, e := range events {
			if runAllowed(allowed, e.RunID) {
				keptEvents = append(keptEvents, e)
				allowedEventIDs[e.ID] = struct{}{}
			}
		}
		events = keptEvents

		// A belief's run is only reachable through its evidence links — the same
		// route ListBeliefs takes for its run_id filter.
		claimIDs := make([]string, 0, len(claims))
		for _, c := range claims {
			claimIDs = append(claimIDs, c.ID)
		}
		visibleClaims := map[string]struct{}{}
		if len(claimIDs) > 0 && len(allowedEventIDs) > 0 {
			links, err := s.connFor(ctx).Claims.ListEvidenceByClaimIDs(ctx, claimIDs)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "load evidence for run scope: %v", err)
			}
			for _, l := range links {
				if _, ok := allowedEventIDs[l.EventID]; ok {
					visibleClaims[l.ClaimID] = struct{}{}
				}
			}
		}
		keptClaims := claims[:0]
		for _, c := range claims {
			if _, ok := visibleClaims[c.ID]; ok {
				keptClaims = append(keptClaims, c)
			}
		}
		claims = keptClaims

		// An embedding is an index entry for its entity, so it is legible
		// exactly when its entity is.
		keptEventEmbs := eventEmbs[:0]
		for _, e := range eventEmbs {
			if _, ok := allowedEventIDs[e.EntityID]; ok {
				keptEventEmbs = append(keptEventEmbs, e)
			}
		}
		eventEmbs = keptEventEmbs
		keptClaimEmbs := claimEmbs[:0]
		for _, e := range claimEmbs {
			if _, ok := visibleClaims[e.EntityID]; ok {
				keptClaimEmbs = append(keptClaimEmbs, e)
			}
		}
		claimEmbs = keptClaimEmbs
	}

	runs := map[string]struct{}{}
	for _, e := range events {
		if e.RunID != "" {
			runs[e.RunID] = struct{}{}
		}
	}
	var contestedClaims int64
	for _, c := range claims {
		if c.Status == domain.ClaimStatusContested {
			contestedClaims++
		}
	}
	// Associations and dissonances are counted from the surviving beliefs, so
	// the run narrowing above carries into them without a second filter.
	ids := make([]string, 0, len(claims))
	for _, c := range claims {
		ids = append(ids, c.ID)
	}
	rels, _ := s.connFor(ctx).Relationships.ListByClaimIDs(ctx, ids)
	var contradictions int64
	for _, rel := range rels {
		if rel.Type == domain.RelationshipTypeContradicts {
			contradictions++
		}
	}

	return &mnemosv1.MetricsResponse{
		Runs:             int64(len(runs)),
		Episodes:         int64(len(events)),
		Beliefs:          int64(len(claims)),
		ContestedBeliefs: contestedClaims,
		Associations:     int64(len(rels)),
		Dissonances:      contradictions,
		Embeddings:       int64(len(eventEmbs) + len(claimEmbs)),
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

const (
	maxBatchRecords   = 1000
	defaultServeLimit = 50
	maxServePageLimit = 200
)

func normalizePagination(p *mnemosv1.Pagination) (int, int) {
	if p == nil {
		return defaultServeLimit, 0
	}
	limit := int(p.Limit)
	if limit <= 0 {
		limit = defaultServeLimit
	}
	if limit > maxServePageLimit {
		limit = maxServePageLimit
	}
	offset := int(p.Offset)
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func paginate[T any](xs []T, limit, offset int) []T {
	if offset >= len(xs) {
		return nil
	}
	end := offset + limit
	if end > len(xs) {
		end = len(xs)
	}
	return xs[offset:end]
}

func validClaimType(t string) bool {
	return t == string(domain.ClaimTypeFact) || t == string(domain.ClaimTypeHypothesis) || t == string(domain.ClaimTypeDecision) || t == string(domain.ClaimTypeTestResult)
}

func validClaimStatus(s string) bool {
	return s == string(domain.ClaimStatusActive) || s == string(domain.ClaimStatusContested) || s == string(domain.ClaimStatusResolved) || s == string(domain.ClaimStatusDeprecated)
}

func validClaimVisibility(s string) bool {
	return s == string(domain.VisibilityPersonal) || s == string(domain.VisibilityTeam) || s == string(domain.VisibilityOrg)
}

func episodeToProto(e domain.Event) *mnemosv1.Episode {
	return &mnemosv1.Episode{
		Id:            e.ID,
		RunId:         e.RunID,
		SchemaVersion: e.SchemaVersion,
		Content:       e.Content,
		SourceInputId: e.SourceInputID,
		Timestamp:     timestamppb.New(e.Timestamp.UTC()),
		Metadata:      e.Metadata,
		IngestedAt:    timestamppb.New(e.IngestedAt.UTC()),
	}
}

func beliefToProto(c domain.Claim) *mnemosv1.Belief {
	return &mnemosv1.Belief{
		Id:         c.ID,
		Text:       c.Text,
		Type:       string(c.Type),
		Confidence: c.Confidence,
		Status:     string(c.Status),
		CreatedAt:  timestamppb.New(c.CreatedAt.UTC()),
		Visibility: string(c.Visibility),
	}
}

func associationToProto(r domain.Relationship) *mnemosv1.Association {
	return &mnemosv1.Association{
		Id:           r.ID,
		Type:         string(r.Type),
		FromBeliefId: r.FromClaimID,
		ToBeliefId:   r.ToClaimID,
		CreatedAt:    timestamppb.New(r.CreatedAt.UTC()),
	}
}

func embeddingToProto(e domain.EmbeddingRecord) *mnemosv1.Embedding {
	return &mnemosv1.Embedding{
		EntityId:   e.EntityID,
		EntityType: e.EntityType,
		Vector:     e.Vector,
		Model:      e.Model,
		Dimensions: int32(e.Dimensions),
	}
}
