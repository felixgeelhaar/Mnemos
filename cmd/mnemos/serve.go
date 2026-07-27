package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.klarlabs.de/bolt"
	mnemos "go.klarlabs.de/mnemos"
	"go.klarlabs.de/mnemos/internal/auth"
	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/embedding"
	"go.klarlabs.de/mnemos/internal/govwrite"
	markdownpkg "go.klarlabs.de/mnemos/internal/markdown"
	"go.klarlabs.de/mnemos/internal/ports"
	"go.klarlabs.de/mnemos/internal/query"
	"go.klarlabs.de/mnemos/internal/runscope"
	mnemosgrpc "go.klarlabs.de/mnemos/internal/server/grpc"
	"go.klarlabs.de/mnemos/internal/store"
	"google.golang.org/grpc"
)

// embedderResolver returns the embedding client used by the semantic-
// search branch of /v1/beliefs. Production reads env via
// embedding.ConfigFromEnv; tests override this hook to inject a stub
// without touching env state.
var embedderResolver = func() (embedding.Client, error) {
	cfg, err := embedding.ConfigFromEnv()
	if err != nil {
		return nil, err
	}
	return embedding.NewClient(cfg)
}

//go:embed web/index.html
var webIndexHTML []byte

//go:embed web/landing.html
var webLandingHTML []byte

const (
	defaultServePort   = 7777
	maxServePageLimit  = 200
	defaultServeLimit  = 50
	serveReadTimeout   = 10 * time.Second
	serveWriteTimeout  = 30 * time.Second
	serveIdleTimeout   = 60 * time.Second
	serveShutdownGrace = 10 * time.Second

	// serveReadHeaderTimeout caps header read separately from the
	// rest of the request. Slowloris-style attacks dribble a header
	// byte-at-a-time to hold connections open; a 5s header deadline
	// is generous for legitimate clients while bounding the attack.
	serveReadHeaderTimeout = 5 * time.Second

	// serveMaxHeaderBytes caps per-request header size. Default in
	// net/http is 1MB which is overkill for our REST API — we can
	// budget much tighter without rejecting legitimate traffic.
	serveMaxHeaderBytes = 64 * 1024
)

// handleServe runs the HTTP registry server. Phase 2B v1 — read-only
// endpoints over the local DB. Push, pull, namespacing, and auth ship in
// follow-up commits, since the read surface is what every later concern
// needs first anyway.
func handleServe(args []string, _ Flags) {
	port := defaultServePort
	grpcPort := 0 // 0 = disabled
	requireTenant := false
	publicReads := false   // secure by default: GET reads require a token unless opted in
	metricsPublic := false // secure by default: /internal/metrics requires a token unless opted in
	trustProxy := false    // secure by default: X-Forwarded-For is not believed
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--require-tenant":
			requireTenant = true
		case "--trust-proxy":
			trustProxy = true
		case "--public-reads":
			publicReads = true
		case "--metrics-public":
			metricsPublic = true
		case "--port":
			if i+1 >= len(args) {
				exitWithMnemosError(false, NewUserError("--port requires a value"))
				return
			}
			p, err := strconv.Atoi(args[i+1])
			if err != nil || p < 1 || p > 65535 {
				exitWithMnemosError(false, NewUserError("--port must be a number between 1 and 65535"))
				return
			}
			port = p
			i++
		case "--grpc-port":
			if i+1 >= len(args) {
				exitWithMnemosError(false, NewUserError("--grpc-port requires a value"))
				return
			}
			p, err := strconv.Atoi(args[i+1])
			if err != nil || p < 1 || p > 65535 {
				exitWithMnemosError(false, NewUserError("--grpc-port must be a number between 1 and 65535"))
				return
			}
			grpcPort = p
			i++
		default:
			exitWithMnemosError(false, NewUserError("unknown serve flag %q", args[i]))
			return
		}
	}
	if envPort := os.Getenv("MNEMOS_SERVE_PORT"); envPort != "" && port == defaultServePort {
		if p, err := strconv.Atoi(envPort); err == nil && p >= 1 && p <= 65535 {
			port = p
		}
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MNEMOS_PUBLIC_READS"))) {
	case "1", "true", "yes":
		publicReads = true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MNEMOS_METRICS_PUBLIC"))) {
	case "1", "true", "yes":
		metricsPublic = true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MNEMOS_TRUST_PROXY"))) {
	case "1", "true", "yes":
		trustProxy = true
	}
	// Only a deployment that actually sits behind a proxy overwriting
	// X-Forwarded-For may believe it; otherwise the header is attacker-supplied
	// and the per-IP limiter on the anonymous /v1/leads route is bypassable by
	// varying it. Off by default, opt in explicitly.
	trustProxyHeaders = trustProxy
	if trustProxy {
		fmt.Fprintln(os.Stderr, "serve: --trust-proxy is set — X-Forwarded-For determines the client IP for rate limiting. Only use this behind a proxy that OVERWRITES the header.")
	}
	// Secure by default: without an explicit opt-in, GET reads require a token.
	// --require-tenant already authenticates every request, so --public-reads is
	// meaningless there and ignored; warn so the operator isn't misled.
	if publicReads && requireTenant {
		fmt.Fprintln(os.Stderr, "serve: --public-reads is ignored under --require-tenant (every request is authenticated)")
		publicReads = false
	}
	if publicReads {
		fmt.Fprintln(os.Stderr, "serve: WARNING --public-reads is set — GET endpoints (including /v1/schemas) are readable WITHOUT authentication.")
	}
	if metricsPublic {
		fmt.Fprintln(os.Stderr, "serve: WARNING --metrics-public is set — /internal/metrics is scrapeable WITHOUT authentication; expose it only on a trusted internal network.")
	}

	dsn := resolveDSN()

	// Multi-tenant mode (ADR 0007): every request scopes to its token's tenant.
	// Postgres isolates by row-level security (?tenant=); sqlite/mysql/local
	// libSQL by a physically separate namespace per tenant (?namespace=<derived>).
	// Backends that can't isolate (memory, remote libSQL) are refused. Fails
	// closed if a request lacks a tenant. Enable the per-tenant connection cache
	// so per-request opens reuse a pool/handle.
	if requireTenant {
		if store.TenancyModeForDSN(dsn) == store.TenancyNone {
			exitWithMnemosError(false, NewUserError("serve --require-tenant needs a backend that isolates tenants: postgres (RLS), sqlite, mysql, or local libsql (namespace-per-tenant). This MNEMOS_DB_URL supports neither."))
			return
		}
		if dsnHasTenantParam(dsn) {
			exitWithMnemosError(false, NewUserError("serve --require-tenant: MNEMOS_DB_URL must not set ?tenant= — the per-request token supplies the tenant"))
			return
		}
		setTenantRequired()
		enableConnCache()
		defer closeConnCache()
		defer closeTenantMems()
	}

	// Base (unscoped) connection: the JWT verifier (revocation store), health,
	// and the fallback for single-tenant mode. Per-request tenant connections
	// are opened by tenantScopeMiddleware. openBaseConn never fail-closes.
	conn, err := openBaseConn(context.Background())
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "failed to open database at %q", store.RedactDSN(dsn)))
		return
	}
	defer closeConn(conn)

	// Build a Memory facade over the same store so the connected-brain
	// endpoints can delegate to the library's cognitive layer. Best-effort:
	// on failure the brain endpoints return 503 while storage stays up.
	mem, memErr := newLibraryMemory(context.Background(), "")
	if memErr != nil {
		fmt.Fprintf(os.Stderr, "serve: cognitive layer disabled (facade: %v)\n", memErr)
	} else {
		defer func() { _ = mem.Close() }()
	}

	// Product + cognitive metrics sampler (ADR 0020): a background goroutine populates
	// the Prometheus gauges on /internal/metrics so scrape frequency is decoupled from
	// the full-scan cost of computing them. Stopped when serve returns.
	samplerCtx, stopSampler := context.WithCancel(context.Background())
	defer stopSampler()
	startProductMetricsSampler(samplerCtx, conn, mem, metricsSampleInterval(), newStderrLogger())

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           newServerMuxWithMemory(conn, mem, requireTenant, publicReads, metricsPublic),
		ReadTimeout:       serveReadTimeout,
		ReadHeaderTimeout: serveReadHeaderTimeout,
		WriteTimeout:      serveWriteTimeout,
		IdleTimeout:       serveIdleTimeout,
		MaxHeaderBytes:    serveMaxHeaderBytes,
	}

	var grpcSrv *grpc.Server
	if grpcPort > 0 {
		grpcSrv = startGRPCServer(grpcPort, conn, mem, requireTenant, publicReads)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	tlsCert := os.Getenv("MNEMOS_TLS_CERT_FILE")
	tlsKey := os.Getenv("MNEMOS_TLS_KEY_FILE")
	if tlsCert != "" && tlsKey != "" {
		tlsCfg, err := buildServerTLS(tlsCert, tlsKey, os.Getenv("MNEMOS_MTLS_CLIENT_CA_FILE"))
		if err != nil {
			exitWithMnemosError(false, NewSystemError(err, "build TLS"))
		}
		srv.TLSConfig = tlsCfg
	}

	errCh := make(chan error, 1)
	go func() {
		scheme := "http"
		if srv.TLSConfig != nil {
			scheme = "https"
		}
		fmt.Printf("mnemos registry serving on %s://localhost:%d (db=%s)\n", scheme, port, store.RedactDSN(dsn))
		if grpcPort > 0 {
			fmt.Printf("mnemos gRPC serving on localhost:%d\n", grpcPort)
		}
		fmt.Println("Press Ctrl+C to stop.")
		var err error
		if srv.TLSConfig != nil {
			err = srv.ListenAndServeTLS(tlsCert, tlsKey)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		exitWithMnemosError(false, NewSystemError(err, "server error"))
	case sig := <-stop:
		fmt.Fprintf(os.Stderr, "\nreceived %s, shutting down...\n", sig)
		ctx, cancel := context.WithTimeout(context.Background(), serveShutdownGrace)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "shutdown: %v\n", err)
		}
		if grpcSrv != nil {
			grpcSrv.GracefulStop()
		}
	}
}

// startGRPCServer creates and starts a gRPC server on the given port.
// It shares the store.Conn and auth verifier with the HTTP surface.
func startGRPCServer(port int, conn *store.Conn, mem mnemos.Memory, requireTenant, publicReads bool) *grpc.Server {
	_, projectRoot, _ := findProjectDB()
	secretPath := auth.DefaultSecretPath(projectRoot)
	secret, _, err := auth.LoadOrCreateSecret(secretPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: load JWT secret for gRPC: %v\n", err)
		os.Exit(int(ExitError))
	}
	prev, err := auth.LoadPreviousSecret(secretPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: load previous JWT secret for gRPC: %v\n", err)
		os.Exit(int(ExitError))
	}
	verifier := auth.NewVerifierWithPrevious(secret, prev, conn.RevokedTokens)
	logger := bolt.New(bolt.NewJSONHandler(os.Stderr))

	mnemosSrv := mnemosgrpc.NewServerWithMemory(conn, mem, verifier, logger, version)
	if publicReads {
		// Opt-in only: allow anonymous read RPCs (secure default requires a token).
		mnemosSrv = mnemosSrv.WithPublicReads()
	}
	if requireTenant {
		// Per-request tenant-scoped connections, RLS-isolated (ADR 0007).
		mnemosSrv = mnemosSrv.WithTenantScoping(func(ctx context.Context, tenant string) (*store.Conn, error) {
			return openConn(withTenant(ctx, tenant))
		}, closeConn)
	}
	grpcOpts := []grpc.ServerOption{grpc.UnaryInterceptor(mnemosSrv.UnaryInterceptor())}
	if cert, key := os.Getenv("MNEMOS_TLS_CERT_FILE"), os.Getenv("MNEMOS_TLS_KEY_FILE"); cert != "" && key != "" {
		creds, err := buildServerCreds(cert, key, os.Getenv("MNEMOS_MTLS_CLIENT_CA_FILE"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "serve: gRPC TLS: %v\n", err)
			os.Exit(int(ExitError))
		}
		grpcOpts = append(grpcOpts, grpc.Creds(creds))
	}
	grpcSrv := grpc.NewServer(grpcOpts...)
	mnemosSrv.Register(grpcSrv)

	go func() {
		lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			fmt.Fprintf(os.Stderr, "gRPC listen: %v\n", err)
			os.Exit(int(ExitError))
		}
		if err := grpcSrv.Serve(lis); err != nil {
			fmt.Fprintf(os.Stderr, "gRPC serve: %v\n", err)
		}
	}()
	return grpcSrv
}

// newServerMux wires the routes. Exported in package for httptest in
// serve_test.go without booting a real listener.
//
// Auth model: secure by default — every request requires a valid Mnemos
// JWT (reads too), except infra endpoints and, with --public-reads, anonymous
// GET reads. The JWT is signed with the server secret. The secret is resolved from
// MNEMOS_JWT_SECRET or a per-install file (auto-created on first boot).
// Revoked JTIs are honored via the RevokedTokenRepository denylist.
// newServerMux builds the HTTP surface with only the store-backed endpoints
// (no cognitive facade). Kept for tests + callers that don't need the brain.
func newServerMux(conn *store.Conn) http.Handler {
	// Test/utility helper keeps the historical public-read behavior so existing
	// surface tests don't each need a token; production defaults to secure.
	// metricsPublic mirrors publicReads here so the metrics surface test can
	// scrape /internal/metrics anonymously.
	return newServerMuxWithMemory(conn, nil, false, true, true)
}

// newServerMuxWithMemory builds the full HTTP surface. When mem is non-nil the
// connected-brain endpoints (who-knows, knowledge-gaps, calibration,
// hypercorrections, recombinations, analogous) delegate to it; when nil they
// return 503.
func newServerMuxWithMemory(conn *store.Conn, mem mnemos.Memory, requireTenant, publicReads, metricsPublic bool) http.Handler {
	_, projectRoot, _ := findProjectDB()
	secretPath := auth.DefaultSecretPath(projectRoot)
	secret, created, err := auth.LoadOrCreateSecret(secretPath)
	if err != nil {
		// Secret resolution failing at boot is fatal — without it the
		// server can't verify any token. Fail loudly so the operator can
		// fix it (e.g. set MNEMOS_JWT_SECRET).
		fmt.Fprintf(os.Stderr, "serve: load JWT secret: %v\n", err)
		os.Exit(int(ExitError))
	}
	if created {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "===========================================================================")
		fmt.Fprintln(os.Stderr, "serve: generated a new JWT signing secret on first boot.")
		fmt.Fprintf(os.Stderr, "       location: %s\n", secretPath)
		fmt.Fprintln(os.Stderr, "       Any tokens issued under a prior secret no longer validate.")
		fmt.Fprintln(os.Stderr, "       Recommended next steps:")
		fmt.Fprintln(os.Stderr, "         1. Back the file up alongside your DB (.mnemos/) for disaster recovery.")
		fmt.Fprintln(os.Stderr, "         2. Set MNEMOS_JWT_SECRET via your secret store in production rather than")
		fmt.Fprintln(os.Stderr, "            relying on the on-disk file (the file is fine for local dev).")
		fmt.Fprintln(os.Stderr, "         3. Plan a rotation cadence (90 days recommended). Use")
		fmt.Fprintln(os.Stderr, "            MNEMOS_JWT_PREV_SECRET to bridge the rollover so live tokens keep")
		fmt.Fprintln(os.Stderr, "            validating until they expire — see internal/auth/secret.go.")
		fmt.Fprintln(os.Stderr, "===========================================================================")
		fmt.Fprintln(os.Stderr, "")
	}
	prev, err := auth.LoadPreviousSecret(secretPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: load previous JWT secret: %v\n", err)
		os.Exit(int(ExitError))
	}
	verifier := auth.NewVerifierWithPrevious(secret, prev, conn.RevokedTokens)

	// Every durable write the HTTP surface performs routes through this
	// governed daemon-writer (it borrows the long-lived conn — the
	// server keeps ownership). Building it fails only on a programming
	// error (nil conn / static plugin bug), which must surface loudly.
	gw, err := govwrite.Wrap(conn, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: build governed writer: %v\n", err)
		os.Exit(int(ExitError))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleLanding)
	mux.HandleFunc("/app", handleWebRoot)
	// /health(z) is a bare liveness 200 — no version/db/tenant data, anonymous
	// by design (probes need reachability, not data). The richer readiness probe
	// (DB write check) lives at /internal/ready behind auth.
	mux.HandleFunc("/health", makeHealthHandler())
	mux.HandleFunc("/healthz", makeHealthHandler())
	mux.HandleFunc("/internal/ready", makeReadinessHandler(conn))
	leadsLogger := bolt.New(bolt.NewJSONHandler(os.Stderr))
	mux.Handle("/v1/leads", leadsRateLimitMiddleware(makeLeadsHandler(leadsLogger)))
	mux.HandleFunc("/v1/process", makeProcessHandler())
	mux.HandleFunc("/v1/episodes", makeEventsHandler(conn, gw))
	mux.HandleFunc("/v1/beliefs", makeClaimsHandler(conn, gw))
	mux.HandleFunc("/v1/associations", makeRelationshipsHandler(conn, gw))
	mux.HandleFunc("/v1/embeddings", makeEmbeddingsHandler(conn, gw))
	mux.HandleFunc("/v1/metrics", makeMetricsHandler(conn))
	mux.HandleFunc("/v1/schemas", makeSchemasHandler(conn))
	mux.HandleFunc("/v1/context", makeContextHandler(conn))
	mux.HandleFunc("/v1/search", makeSearchHandler(conn))
	mux.HandleFunc("/v1/beliefs/", makeClaimSubresourceHandler(conn, gw, mem))
	mux.HandleFunc("/v1/incidents", makeIncidentsHandler(conn, gw))
	mux.HandleFunc("/v1/federation/export", makeFederationExportHandler(conn))
	mux.HandleFunc("/v1/incidents/", makeIncidentSubresourceHandler(conn, gw))
	// Connected-brain reads (v0.66): the library's cognitive layer over HTTP.
	// Delegate to the Memory facade; 503 when it's unavailable.
	mux.HandleFunc("/v1/who-knows", makeWhoKnowsHandler(mem))
	mux.HandleFunc("/v1/knowledge-gaps", makeKnowledgeGapsHandler(mem))
	mux.HandleFunc("/v1/calibration", makeCalibrationHandler(mem))
	mux.HandleFunc("/v1/hypercorrections", makeHypercorrectionsHandler(mem))
	mux.HandleFunc("/v1/recombinations", makeRecombinationsHandler(mem))
	// Claim CRUD parity (v0.67): novelty classification + decision browse surface.
	mux.HandleFunc("/v1/classify", makeClassifyHandler(mem))
	mux.HandleFunc("/v1/decisions", makeDecisionsHandler(mem))
	mux.HandleFunc("/v1/decisions/", makeDecisionSubresourceHandler(mem))
	// Advanced recall (v0.68): epistemic-honesty variants of retrieval.
	mux.HandleFunc("/v1/recall", makeRecallHandler(mem))
	// Working memory + skill loop + temporal (v0.69): the last of the cognitive layer.
	mux.HandleFunc("/v1/blocks", makeBlocksHandler(mem))
	mux.HandleFunc("/v1/actions", makeActionsHandler(mem))
	mux.HandleFunc("/v1/actions/", makeActionSubresourceHandler(mem))
	mux.HandleFunc("/v1/synthesize", makeSynthesizeHandler(mem))
	mux.HandleFunc("/v1/timeline", makeTimelineHandler(mem))
	mux.HandleFunc("/v1/signals", makeSignalsHandler(mem))

	mux.Handle("/internal/metrics", makeMnemosMetricsHandler())

	logger := bolt.New(bolt.NewJSONHandler(os.Stderr))
	// panicRecover is the outermost layer so a panic in any later
	// middleware (auth, access log) still produces a clean 500
	// response instead of leaving the client hanging. securityHeaders
	// sits just inside it so the hardened headers are applied to
	// recovery responses too. requestIDMiddleware decorates the
	// context so the access log and downstream handlers can include
	// the correlation id. metricsMiddleware sits inside the access
	// log so duration recorded in prometheus matches what's logged.
	// Auth stashes the tenant (multi-tenant mode); tenantScopeMiddleware then
	// opens a per-request tenant connection the handlers resolve via scopedConn.
	authed := jwtAuthMiddleware(verifier, tenantScopeMiddleware(mux), requireTenant, publicReads, metricsPublic)
	return panicRecover(logger, securityHeaders(requestIDMiddleware(boltAccessLog(logger, metricsMiddleware(mux, authed)))))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

type leadRequest struct {
	Email string `json:"email"`
}

type leadResponse struct {
	Status string `json:"status"`
}

// maxLeadEmailBytes caps email length at the RFC 5321 maximum so
// pathologically long inputs (e.g. an attacker streaming a 1 MiB body
// with no LF) never reach the structured logger.
const maxLeadEmailBytes = 254

// makeLeadsHandler returns the public POST /v1/leads handler. The
// endpoint intentionally bypasses JWT auth (handled in
// jwtAuthMiddleware) so the marketing landing form works for
// unauthenticated visitors. That means every defence — rate limit,
// strict email validation, structured logging — must live here.
func makeLeadsHandler(logger *bolt.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		var req leadRequest
		// MaxBytesReader closes the body once exceeded so a slowloris
		// stream can't keep the handler alive past 1 MiB.
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}

		email, err := validateLeadEmail(req.Email)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		// Hash the email before logging so the access log does not
		// become a plaintext PII trail. The hash is enough to detect
		// duplicate-form abuse without retaining the address.
		logger.Info().
			Str("event", "lead_captured").
			Str("email_sha256", sha256Hex(email)).
			Str("remote_ip", clientIP(r)).
			Msg("lead_captured")

		writeJSON(w, http.StatusOK, leadResponse{Status: "ok"})
	}
}

// validateLeadEmail enforces RFC 5321 length, RFC 5322 syntax (via
// net/mail.ParseAddress), and rejects any control-character payload
// that could be used for log injection. Returns the normalised
// address (lowercased local + domain).
func validateLeadEmail(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("email is required")
	}
	if len(s) > maxLeadEmailBytes {
		return "", fmt.Errorf("email exceeds %d bytes", maxLeadEmailBytes)
	}
	for _, r := range s {
		// Reject CR/LF and any other control char — these are the
		// classic log-injection vectors. Tabs are also disallowed so
		// the log line stays single-row in any tab-aware viewer.
		if r < 0x20 || r == 0x7f {
			return "", errors.New("email contains control characters")
		}
	}
	addr, err := mail.ParseAddress(s)
	if err != nil {
		return "", fmt.Errorf("invalid email: %w", err)
	}
	return strings.ToLower(addr.Address), nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// trustProxyHeaders gates whether clientIP believes X-Forwarded-For. It is
// process-wide state set once at startup by handleServe (--trust-proxy /
// MNEMOS_TRUST_PROXY); it defaults to false so every other entrypoint — tests,
// newServerMux, the embedded surfaces — gets the safe behaviour without
// opting in.
//
// Why it must default off: X-Forwarded-For is a client-supplied header. On a
// directly-exposed listener nothing overwrites it, and clientIP feeds the
// per-IP token bucket in front of POST /v1/leads — the one route
// jwtAuthMiddleware exempts from auth. Trusting the header unconditionally
// let a spammer mint a fresh rate-limit bucket per request by varying it,
// which defeats the only control that route has.
var trustProxyHeaders = false

// clientIP resolves the source address used for rate limiting and lead
// logging. The socket peer is authoritative; X-Forwarded-For is consulted
// only when the operator has declared the server sits behind a proxy that
// overwrites the header (see trustProxyHeaders).
func clientIP(r *http.Request) string {
	if trustProxyHeaders {
		// Leftmost entry is the original client, per the XFF convention.
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type healthResponse struct {
	Status  string        `json:"status"`
	Version string        `json:"version"`
	Healthy *bool         `json:"healthy,omitempty"`
	Checks  []healthCheck `json:"checks,omitempty"`
}

// makeHealthHandler returns the /health(z) handler: a bare liveness 200 with a
// minimal body ({"status":"ok"}) and nothing else. A probe needs reachability,
// not data, so this intentionally leaks no version, db path, counts, or tenant
// info — and stays anonymous. The richer readiness probe (DB write check) lives
// behind auth at /internal/ready.
func makeHealthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// makeReadinessHandler returns the authenticated /internal/ready handler: the
// full subsystem report (DB write probe + version) an orchestrator can
// readiness-gate on. It is NOT anonymous — auth applies via jwtAuthMiddleware —
// because the report carries operational detail (version, subsystem status)
// that shouldn't leak to unauthenticated callers on a hosted listener.
//
// Returns 503 when a probe fails so HTTP-aware load balancers / Kubernetes
// readiness gates can react without parsing the JSON.
func makeReadinessHandler(conn *store.Conn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn := scopedConn(r.Context(), conn)
		// The readiness probe expects a *sql.DB for the SQLite-flavoured write
		// check; non-SQLite backends would need a per-backend probe we haven't
		// built. Pull *sql.DB out of Conn.Raw when available, else degrade to the
		// shallow Healthy=true response.
		var db *sql.DB
		if raw, ok := conn.Raw.(*sql.DB); ok {
			db = raw
		}
		if db == nil {
			writeJSON(w, http.StatusOK, healthResponse{Status: "ok", Version: version})
			return
		}
		result := runHealthChecks(r.Context(), db)
		status := http.StatusOK
		if !result.Healthy {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, healthResponse{
			Status:  ternary(result.Healthy, "ok", "degraded"),
			Version: version,
			Healthy: &result.Healthy,
			Checks:  result.Checks,
		})
	}
}

func ternary(cond bool, t, f string) string {
	if cond {
		return t
	}
	return f
}

// handleWebRoot serves the embedded single-page UI at GET /app. Any other
// path returns 404 — we don't want catch-all behavior masking real route
// typos like /v1/clams. Unsupported methods get 405.
func handleWebRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/app" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if _, err := w.Write(webIndexHTML); err != nil {
		fmt.Fprintf(os.Stderr, "serve web: %v\n", err)
	}
}

// handleLanding serves the marketing landing page at GET /.
func handleLanding(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if _, err := w.Write(webLandingHTML); err != nil {
		fmt.Fprintf(os.Stderr, "serve landing: %v\n", err)
	}
}

type eventsResponse struct {
	Events []eventDTO `json:"episodes"`
	Total  int        `json:"total"`
	Limit  int        `json:"limit"`
	Offset int        `json:"offset"`
}

type eventDTO struct {
	ID            string            `json:"id"`
	RunID         string            `json:"run_id"`
	SchemaVersion string            `json:"schema_version"`
	Content       string            `json:"content"`
	SourceInputID string            `json:"source_input_id"`
	Timestamp     string            `json:"timestamp"`
	Metadata      map[string]string `json:"metadata"`
	IngestedAt    string            `json:"ingested_at"`
}

// processRequestDTO is the POST /v1/process body: raw text to run through the
// ingest→extract→relate pipeline. UseLLM/UseEmbeddings are tri-state (*bool): a
// caller that omits them (e.g. a hosted-brain hook, which can't know the
// server's LLM config) lets the SERVER decide — defaulting to LLM extraction iff
// the server has a provider configured. An explicit true/false overrides.
type processRequestDTO struct {
	Text          string `json:"text"`
	UseLLM        *bool  `json:"use_llm,omitempty"`
	UseEmbeddings *bool  `json:"use_embeddings,omitempty"`
}

// processResponseDTO reports what the pipeline persisted.
type processResponseDTO struct {
	RunID          string `json:"run_id"`
	Events         int    `json:"events"`
	Claims         int    `json:"claims"`
	Relationships  int    `json:"relationships"`
	Embeddings     int    `json:"embeddings"`
	UsedLLM        bool   `json:"used_llm"`
	UsedEmbeddings bool   `json:"used_embeddings"`
}

// makeProcessHandler runs the full ingest→extract→relate pipeline from raw text —
// the REST analogue of the MCP `process_text` tool, and the server-side path a
// hosted-brain `capture` hook POSTs a transcript to. POST only; requires
// claims:write (the pipeline's meaningful output). Tenant-scoped: the shared
// mcpRunProcessText opens its writer via openWriter(ctx), which the request's
// tenant (set by jwtAuthMiddleware) scopes — so it works under serve
// --require-tenant unchanged.
func makeProcessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !requireScope(w, r, domain.ScopeClaimsWrite) {
			return
		}
		var req processRequestDTO
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if strings.TrimSpace(req.Text) == "" {
			writeError(w, http.StatusBadRequest, "text is required")
			return
		}
		// Default to LLM extraction iff this server has a provider configured, so
		// a hook that omits the flags gets the best the server can do without
		// hard-failing when no LLM is set. Explicit request flags override.
		serverHasLLM := strings.TrimSpace(os.Getenv("MNEMOS_LLM_PROVIDER")) != ""
		useLLM := serverHasLLM
		if req.UseLLM != nil {
			useLLM = *req.UseLLM
		}
		useEmbeddings := useLLM
		if req.UseEmbeddings != nil {
			useEmbeddings = *req.UseEmbeddings
		}
		out, err := mcpRunProcessText(r.Context(), actorFromContext(r.Context()), mcpProcessTextInput{
			Text:          req.Text,
			UseLLM:        useLLM,
			UseEmbeddings: useEmbeddings,
		})
		if err != nil {
			writeInternalError(w, "process text", err)
			return
		}
		writeJSON(w, http.StatusOK, processResponseDTO{
			RunID:          out.RunID,
			Events:         out.Events,
			Claims:         out.Claims,
			Relationships:  out.Relationships,
			Embeddings:     out.Embeddings,
			UsedLLM:        out.UsedLLM,
			UsedEmbeddings: out.UsedEmbeddings,
		})
	}
}

func makeEventsHandler(conn *store.Conn, gw *govwrite.Writer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn := scopedConn(r.Context(), conn)
		gw := scopedWriter(r.Context(), gw)
		switch r.Method {
		case http.MethodGet:
			listEventsHandler(conn, w, r)
		case http.MethodPost:
			appendEventsHandler(gw, w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

func listEventsHandler(conn *store.Conn, w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePaginationFromQuery(r)
	ctx := r.Context()

	// Optional run_id filter. Lets external auditors (e.g. a LangGraph
	// agent's callback) replay one run's full chain without scanning
	// the global event log. Falls back to ListAll when unset.
	runID := r.URL.Query().Get("run_id")
	// Whitelist parity with the write path — checked UNCONDITIONALLY, not
	// just when the caller supplies run_id. Gating on `runID != ""` meant a
	// run-scoped bearer only had to omit the filter to fall through to
	// ListAll and receive every run's events.
	if !requireRunScope(w, r, runID) {
		return
	}
	var all []domain.Event
	var err error
	if runID != "" {
		all, err = conn.Events.ListByRunID(ctx, runID)
	} else {
		all, err = conn.Events.ListAll(ctx)
	}
	if err != nil {
		writeInternalError(w, "list events", err)
		return
	}
	// ListAll returns ascending; the federation client expects most-
	// recent first. Reverse without sorting since timestamps are
	// already monotonic by run.
	reversed := make([]domain.Event, len(all))
	for i, e := range all {
		reversed[len(all)-1-i] = e
	}
	total := len(reversed)
	page := paginate(reversed, limit, offset)

	events := make([]eventDTO, 0, len(page))
	for _, e := range page {
		events = append(events, eventDTO{
			ID:            e.ID,
			RunID:         e.RunID,
			SchemaVersion: e.SchemaVersion,
			Content:       e.Content,
			SourceInputID: e.SourceInputID,
			Timestamp:     e.Timestamp.UTC().Format(time.RFC3339),
			Metadata:      e.Metadata,
			IngestedAt:    e.IngestedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, eventsResponse{Events: events, Total: total, Limit: limit, Offset: offset})
}

// paginate slices xs by limit/offset; safe on empty input or
// out-of-range offsets.
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

type claimsResponse struct {
	Claims   []claimDTO          `json:"beliefs"`
	Evidence []claimEvidenceItem `json:"evidence,omitempty"`
	Total    int                 `json:"total"`
	Limit    int                 `json:"limit"`
	Offset   int                 `json:"offset"`
}

type claimDTO struct {
	ID         string  `json:"id"`
	Text       string  `json:"text"`
	Type       string  `json:"type"`
	Confidence float64 `json:"confidence"`
	Status     string  `json:"status"`
	CreatedAt  string  `json:"created_at"`
	// CreatedBy attributes the claim to a specific author (a service's
	// sub-actor — a worker or, e.g., a coach). On append it OVERRIDES the
	// token actor for this claim (the mnemos ClaimItem.CreatedBy semantics);
	// absent → the request's token actor. Echoed back on read.
	CreatedBy       string  `json:"created_by,omitempty"`
	SourceDocument  string  `json:"source_document,omitempty"`
	SourceType      string  `json:"source_type,omitempty"`
	SourceAuthority float64 `json:"source_authority,omitempty"`
	Liveness        string  `json:"liveness,omitempty"`
	// Test provenance — populated when type == "test_result".
	TestID             string `json:"test_id,omitempty"`
	TestRequirementRef string `json:"test_requirement_ref,omitempty"`
	TestAuthor         string `json:"test_author,omitempty"`
	TestLastModified   string `json:"test_last_modified,omitempty"`
	TestLastRunAt      string `json:"test_last_run_at,omitempty"`
	TestPassCount      int    `json:"test_pass_count,omitempty"`
	TestFailCount      int    `json:"test_fail_count,omitempty"`
	// Visibility gates audience access: personal | team | org.
	// Defaults to "team" when omitted.
	Visibility string `json:"visibility,omitempty"`
	// Similarity is populated only by the semantic-search branch
	// (?similar_to=...); it carries the cosine similarity (1.0 =
	// identical) between the query embedding and this claim's
	// embedding. Absent from the standard list response.
	Similarity float64 `json:"similarity,omitempty"`
	// ConfidenceComponents decomposes the scalar Confidence into
	// named contributors (e.g. data_quality / corroboration /
	// source_authority / recency). Empty when the producer did not
	// surface a decomposition — consumers treat absent as "no
	// decomposition available", NOT "all zero". The scalar
	// Confidence field stays the canonical "overall" number for
	// readers that don't care about the breakdown.
	ConfidenceComponents map[string]float64 `json:"confidence_components,omitempty"`
}

func makeClaimsHandler(conn *store.Conn, gw *govwrite.Writer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn := scopedConn(r.Context(), conn)
		gw := scopedWriter(r.Context(), gw)
		switch r.Method {
		case http.MethodGet:
			listClaimsHandler(conn, w, r)
		case http.MethodPost:
			appendClaimsHandler(conn, gw, w, r)
		case http.MethodDelete:
			deleteClaimsHandler(conn, gw, w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

func listClaimsHandler(conn *store.Conn, w http.ResponseWriter, r *http.Request) {
	// Semantic-search branch — short-circuits the rest of the
	// filter pipeline. similar_to + run_id is the agent's episodic-
	// recall path; everything else (type/status/as_of) is unrelated
	// metadata filtering that doesn't compose meaningfully with
	// ranked retrieval.
	if similarTo := strings.TrimSpace(r.URL.Query().Get("similar_to")); similarTo != "" {
		semanticSearchClaimsHandler(conn, w, r, similarTo)
		return
	}

	limit, offset := parsePaginationFromQuery(r)
	typeFilter := r.URL.Query().Get("type")
	statusFilter := r.URL.Query().Get("status")
	asOfRaw := r.URL.Query().Get("as_of")                  // validity time
	recordedAsOfRaw := r.URL.Query().Get("recorded_as_of") // ingestion time
	runIDFilter := r.URL.Query().Get("run_id")             // tenant boundary

	// run_id is described as the tenant boundary but was never checked against
	// the bearer's allowlist, so the caller picked their own boundary — or
	// dropped it entirely and read every run. Gate it before any load.
	if !requireRunScope(w, r, runIDFilter) {
		return
	}

	if typeFilter != "" && !validClaimType(typeFilter) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid type %q", typeFilter))
		return
	}
	if statusFilter != "" && !validClaimStatus(statusFilter) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid status %q", statusFilter))
		return
	}

	var asOf time.Time
	if asOfRaw != "" {
		t, err := parseTimeFlexible(asOfRaw)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("as_of: %v", err))
			return
		}
		asOf = t
	}
	var recordedAsOf time.Time
	if recordedAsOfRaw != "" {
		t, err := parseTimeFlexible(recordedAsOfRaw)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("recorded_as_of: %v", err))
			return
		}
		recordedAsOf = t
	}

	ctx := r.Context()

	// Build the allowed-event set for run_id tenant scoping. Empty set
	// when run_id is specified means there are no claims for that
	// tenant — return early to avoid leaking unfiltered claims.
	var allowedEventIDs map[string]struct{}
	if runIDFilter != "" {
		events, err := conn.Events.ListByRunID(ctx, runIDFilter)
		if err != nil {
			writeInternalError(w, "list events by run id", err)
			return
		}
		allowedEventIDs = make(map[string]struct{}, len(events))
		for _, e := range events {
			allowedEventIDs[e.ID] = struct{}{}
		}
		if len(allowedEventIDs) == 0 {
			writeJSON(w, http.StatusOK, claimsResponse{
				Claims: []claimDTO{},
				Limit:  limit,
				Offset: offset,
			})
			return
		}
	}

	all, err := conn.Claims.ListAll(ctx)
	if err != nil {
		writeInternalError(w, "list claims", err)
		return
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
	// an event with the matching RunID. Performed after cheaper filters
	// so the evidence load runs only for surviving candidates.
	if allowedEventIDs != nil && len(filtered) > 0 {
		candidateIDs := make([]string, 0, len(filtered))
		for _, c := range filtered {
			candidateIDs = append(candidateIDs, c.ID)
		}
		evLinks, err := conn.Claims.ListEvidenceByClaimIDs(ctx, candidateIDs)
		if err != nil {
			writeInternalError(w, "list evidence for run_id filter", err)
			return
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
	// Reverse for created_at DESC.
	reversed := make([]domain.Claim, len(filtered))
	for i, c := range filtered {
		reversed[len(filtered)-1-i] = c
	}
	total := len(reversed)
	page := paginate(reversed, limit, offset)

	claims := make([]claimDTO, 0, len(page))
	ids := make([]string, 0, len(page))
	for _, c := range page {
		claims = append(claims, claimDTO{
			ID:                   c.ID,
			Text:                 c.Text,
			Type:                 string(c.Type),
			Confidence:           c.Confidence,
			Status:               string(c.Status),
			CreatedAt:            c.CreatedAt.UTC().Format(time.RFC3339),
			CreatedBy:            c.CreatedBy,
			Visibility:           string(c.Visibility),
			ConfidenceComponents: c.ConfidenceComponents,
		})
		ids = append(ids, c.ID)
	}

	links, evErr := conn.Claims.ListEvidenceByClaimIDs(ctx, ids)
	if evErr != nil {
		writeInternalError(w, "load evidence", evErr)
		return
	}
	var evidence []claimEvidenceItem
	for _, l := range links {
		evidence = append(evidence, claimEvidenceItem{ClaimID: l.ClaimID, EventID: l.EventID})
	}

	writeJSON(w, http.StatusOK, claimsResponse{Claims: claims, Evidence: evidence, Total: total, Limit: limit, Offset: offset})
}

// semanticSearchClaimsHandler ranks claims by cosine similarity
// against the embedding of the `similar_to` query. The tenant boundary
// is the same `run_id` allowlist the standard list path uses:
// similar_to REQUIRES run_id so a missing query param can never
// produce a cross-tenant corpus dump.
//
// Why semantic search lives behind run_id-required rather than the
// list path's optional run_id: the standard list path is auditable
// (an operator can post-filter, every row is exact-match). Ranked
// retrieval is the opposite — it surfaces "close enough" rows, so
// without a hard tenant gate a forgotten run_id silently leaks the
// nearest neighbour across tenants. Fail-closed beats audit-after.
func semanticSearchClaimsHandler(conn *store.Conn, w http.ResponseWriter, r *http.Request, similarTo string) {
	runIDFilter := strings.TrimSpace(r.URL.Query().Get("run_id"))
	if runIDFilter == "" {
		writeError(w, http.StatusBadRequest, "similar_to requires run_id to scope the search (tenant boundary)")
		return
	}
	// Requiring run_id is only half a boundary: the caller chose its value.
	// Bind it to the bearer's allowlist so the token, not the query string,
	// decides which corpus ranked retrieval may reach into.
	if !requireRunScope(w, r, runIDFilter) {
		return
	}

	topK := 10
	if raw := r.URL.Query().Get("top_k"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 100 {
			writeError(w, http.StatusBadRequest, "top_k must be a positive integer ≤ 100")
			return
		}
		topK = n
	}
	minSim := 0.0
	if raw := r.URL.Query().Get("min_similarity"); raw != "" {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil || f < 0 || f > 1 {
			writeError(w, http.StatusBadRequest, "min_similarity must be a float in [0, 1]")
			return
		}
		minSim = f
	}

	searcher, ok := conn.Embeddings.(ports.ClaimSimilaritySearcher)
	if !ok {
		writeError(w, http.StatusNotImplemented, "current storage backend does not support vector similarity search")
		return
	}
	embedder, err := embedderResolver()
	if err != nil {
		writeError(w, http.StatusNotImplemented, "embedding provider not configured (set MNEMOS_EMBED_PROVIDER)")
		return
	}

	ctx := r.Context()

	// Resolve the run_id allowlist to a candidate claim-id set. The
	// flow is run_id → events → claim_evidence → claim_ids; same
	// boundary as the standard list path so a single audit covers
	// both.
	events, err := conn.Events.ListByRunID(ctx, runIDFilter)
	if err != nil {
		writeInternalError(w, "list events by run id", err)
		return
	}
	if len(events) == 0 {
		writeJSON(w, http.StatusOK, claimsResponse{Claims: []claimDTO{}, Limit: topK, Offset: 0})
		return
	}
	allowedEventIDs := make(map[string]struct{}, len(events))
	for _, e := range events {
		allowedEventIDs[e.ID] = struct{}{}
	}
	allEvidence, err := conn.Claims.ListAllEvidence(ctx)
	if err != nil {
		writeInternalError(w, "list evidence for semantic search", err)
		return
	}
	candidateClaimIDs := make(map[string]struct{})
	for _, link := range allEvidence {
		if _, ok := allowedEventIDs[link.EventID]; ok {
			candidateClaimIDs[link.ClaimID] = struct{}{}
		}
	}
	if len(candidateClaimIDs) == 0 {
		writeJSON(w, http.StatusOK, claimsResponse{Claims: []claimDTO{}, Limit: topK, Offset: 0})
		return
	}

	vectors, err := embedder.Embed(ctx, []string{similarTo})
	if err != nil {
		writeInternalError(w, "embed query", err)
		return
	}
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		writeInternalError(w, "embed query", fmt.Errorf("provider returned empty vector"))
		return
	}

	hits, err := searcher.SearchClaimsByVector(ctx, vectors[0], candidateClaimIDs, embedding.ModelIDOf(embedder), topK, minSim)
	if err != nil {
		writeInternalError(w, "similarity search", err)
		return
	}
	if len(hits) == 0 {
		writeJSON(w, http.StatusOK, claimsResponse{Claims: []claimDTO{}, Limit: topK, Offset: 0})
		return
	}

	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ClaimID)
	}
	claims, err := conn.Claims.ListByIDs(ctx, ids)
	if err != nil {
		writeInternalError(w, "load claims by ids", err)
		return
	}
	claimByID := make(map[string]domain.Claim, len(claims))
	for _, c := range claims {
		claimByID[c.ID] = c
	}

	// Preserve the ranked order: iterate hits, not claimByID.
	out := make([]claimDTO, 0, len(hits))
	for _, h := range hits {
		c, ok := claimByID[h.ClaimID]
		if !ok {
			continue
		}
		out = append(out, claimDTO{
			ID:                   c.ID,
			Text:                 c.Text,
			Type:                 string(c.Type),
			Confidence:           c.Confidence,
			Status:               string(c.Status),
			CreatedAt:            c.CreatedAt.UTC().Format(time.RFC3339),
			Visibility:           string(c.Visibility),
			Similarity:           h.Similarity,
			ConfidenceComponents: c.ConfidenceComponents,
		})
	}
	writeJSON(w, http.StatusOK, claimsResponse{
		Claims: out,
		Total:  len(out),
		Limit:  topK,
		Offset: 0,
	})
}

// (loadEvidenceForClaims is gone — folded into listClaimsHandler via
// the conn.Claims.ListEvidenceByClaimIDs port method.)

type relationshipsResponse struct {
	Relationships []relationshipDTO `json:"associations"`
	Total         int               `json:"total"`
	Limit         int               `json:"limit"`
	Offset        int               `json:"offset"`
}

type relationshipDTO struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	FromClaimID string `json:"from_belief_id"`
	ToClaimID   string `json:"to_belief_id"`
	CreatedAt   string `json:"created_at"`
}

// verdictDTO is the JSON representation of a domain.Verdict returned in
// search responses when consumer=agent. Agents should inspect Action to
// decide whether to trust the winner, update their internal beliefs, or
// escalate to a human.
type verdictDTO struct {
	WinnerClaimID    string  `json:"winner_claim_id,omitempty"`
	LoserClaimID     string  `json:"loser_claim_id,omitempty"`
	Confidence       float64 `json:"confidence,omitempty"`
	Rationale        string  `json:"rationale,omitempty"`
	Action           string  `json:"action"`
	EscalationReason string  `json:"escalation_reason,omitempty"`
}

func makeRelationshipsHandler(conn *store.Conn, gw *govwrite.Writer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn := scopedConn(r.Context(), conn)
		gw := scopedWriter(r.Context(), gw)
		switch r.Method {
		case http.MethodGet:
			listRelationshipsHandler(conn, w, r)
		case http.MethodPost:
			appendRelationshipsHandler(conn, gw, w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

func listRelationshipsHandler(conn *store.Conn, w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePaginationFromQuery(r)
	typeFilter := r.URL.Query().Get("type")
	if typeFilter != "" && typeFilter != "supports" && typeFilter != "contradicts" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid type %q (want supports or contradicts)", typeFilter))
		return
	}

	// The relationships repository doesn't expose a "list all"
	// since the use case is hop-traversal, not export. Walk
	// every claim and union its edges; dedup by relationship id.
	// This is bounded by the federation page size (200 max), so
	// scanning every claim once per page request is acceptable for
	// the registry-server scale.
	ctx := r.Context()
	allClaims, err := conn.Claims.ListAll(ctx)
	if err != nil {
		writeInternalError(w, "list claims for relationship export: %v", err)
		return
	}
	claimIDs := make([]string, 0, len(allClaims))
	for _, c := range allClaims {
		claimIDs = append(claimIDs, c.ID)
	}
	rels, err := conn.Relationships.ListByClaimIDs(ctx, claimIDs)
	if err != nil {
		writeInternalError(w, "list relationships", err)
		return
	}
	filtered := rels[:0]
	for _, rel := range rels {
		if typeFilter != "" && string(rel.Type) != typeFilter {
			continue
		}
		filtered = append(filtered, rel)
	}
	// Reverse for created_at DESC (rels come back in storage order,
	// which approximates created_at ASC).
	reversed := make([]domain.Relationship, len(filtered))
	for i, rel := range filtered {
		reversed[len(filtered)-1-i] = rel
	}
	total := len(reversed)
	page := paginate(reversed, limit, offset)

	out := make([]relationshipDTO, 0, len(page))
	for _, rel := range page {
		out = append(out, relationshipDTO{
			ID:          rel.ID,
			Type:        string(rel.Type),
			FromClaimID: rel.FromClaimID,
			ToClaimID:   rel.ToClaimID,
			CreatedAt:   rel.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, relationshipsResponse{Relationships: out, Total: total, Limit: limit, Offset: offset})
}

// appendEventsRequest is the body for POST /v1/episodes. Single-event submits
// are common (raw streams) but a batch shape future-proofs the endpoint and
// keeps DTOs symmetric with claims/relationships.
type appendEventsRequest struct {
	Events []eventDTO `json:"episodes"`
}

type appendResponse struct {
	Accepted int `json:"accepted"`
	Skipped  int `json:"skipped"`
}

const maxRequestBytes = 5 * 1024 * 1024 // 5 MB; bigger payloads should chunk

// maxBatchRecords caps the number of items in any POST array. The
// MaxBytesReader already bounds total payload size, but a JSON
// payload of 5MB can contain tens of thousands of tiny records that
// blow up downstream memory when decoded and held for the
// per-record DB pass. 1000 is comfortable for real ingest workloads
// (push batches are 250-500 by default) and keeps a pathological
// client from forcing MB-scale intermediate maps.
const maxBatchRecords = 1000

// rejectOversizedBatch writes a 400 and returns false when n exceeds
// maxBatchRecords. Called at the top of each POST handler so the
// check runs before we enter the per-record validation loop.
func rejectOversizedBatch(w http.ResponseWriter, resource string, n int) bool {
	if n > maxBatchRecords {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%s batch size %d exceeds max %d (split into multiple requests)", resource, n, maxBatchRecords))
		return true
	}
	return false
}

func decodeJSON(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("request body is empty")
		}
		return err
	}
	if dec.More() {
		return errors.New("request body has trailing content after the JSON object")
	}
	return nil
}

func parseTimeFlexible(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time format %q (want RFC3339)", s)
}

func appendEventsHandler(gw *govwrite.Writer, w http.ResponseWriter, r *http.Request) {
	if !requireScope(w, r, domain.ScopeEventsWrite) {
		return
	}
	var req appendEventsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("decode body: %v", err))
		return
	}
	if len(req.Events) == 0 {
		writeError(w, http.StatusBadRequest, "events array is empty")
		return
	}
	if rejectOversizedBatch(w, "events", len(req.Events)) {
		return
	}

	// F.4: enforce the bearer's run whitelist before any DB write so
	// a partial batch can't sneak through. We pre-check every run_id
	// up front; an unrestricted bearer short-circuits inside the helper.
	// Membership goes through auth.Claims.AllowsRun rather than the exact-set
	// compare this used to do, so a `prod-*` token is not silently stricter
	// over REST than it is over MCP.
	for i, e := range req.Events {
		if !runAllowedForRequest(r, e.RunID) {
			writeError(w, http.StatusForbidden, fmt.Sprintf("events[%d].run_id %q not in token whitelist", i, e.RunID))
			return
		}
	}

	ctx := r.Context()
	actor := actorFromContext(ctx)
	now := time.Now().UTC()
	events := make([]domain.Event, 0, len(req.Events))
	for i, e := range req.Events {
		ts, err := parseTimeFlexible(e.Timestamp)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("events[%d].timestamp: %v", i, err))
			return
		}
		ingested, err := parseTimeFlexible(e.IngestedAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("events[%d].ingested_at: %v", i, err))
			return
		}
		if ingested.IsZero() {
			ingested = now
		}
		events = append(events, domain.Event{
			ID:            e.ID,
			RunID:         e.RunID,
			SchemaVersion: e.SchemaVersion,
			Content:       e.Content,
			SourceInputID: e.SourceInputID,
			Timestamp:     ts,
			Metadata:      e.Metadata,
			IngestedAt:    ingested,
			CreatedBy:     actor,
		})
	}
	accepted, err := gw.Events(ctx, events)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("append events: %v", err))
		return
	}
	writeJSON(w, http.StatusCreated, appendResponse{Accepted: accepted})
}

type appendClaimsRequest struct {
	Claims   []claimDTO          `json:"beliefs"`
	Evidence []claimEvidenceItem `json:"evidence,omitempty"`
}

type claimEvidenceItem struct {
	ClaimID string `json:"belief_id"`
	EventID string `json:"episode_id"`
}

// deleteClaimsResponse carries the per-table delete counts plus the
// request id. The request id lands in the structured log so a later
// compliance audit can correlate "we deleted X" with "rows actually
// disappeared".
type deleteClaimsResponse struct {
	RequestID            string `json:"request_id"`
	RunID                string `json:"run_id"`
	ClaimsDeleted        int    `json:"beliefs_deleted"`
	EventsDeleted        int    `json:"episodes_deleted"`
	EmbeddingsDeleted    int    `json:"embeddings_deleted"`
	RelationshipsDeleted int    `json:"associations_deleted"`
}

// deleteClaimsHandler implements DELETE /v1/beliefs?run_id=<prefix>.
// It is the GDPR Art.17 right-to-be-forgotten endpoint: in one call,
// every claim tied to events under the given run_id is removed along
// with its dependent rows (evidence links, embeddings, status
// history, claim-only relationships). Events for that run_id are
// removed last.
//
// Idempotency: calling DELETE twice for the same run_id is safe —
// the second call returns zero counts and 200, matching the
// HTTP-semantics expectation that DELETE is idempotent.
//
// This handler is not transactional across the four repositories
// (each repo manages its own tx). Failure halts the cascade and
// surfaces the error so the operator can retry; the design
// trade-off is that a half-failed run can be re-deleted safely
// (idempotent) rather than risking a giant cross-repo transaction.
func deleteClaimsHandler(conn *store.Conn, gw *govwrite.Writer, w http.ResponseWriter, r *http.Request) {
	if !requireScope(w, r, domain.ScopeClaimsWrite) {
		return
	}
	runID := strings.TrimSpace(r.URL.Query().Get("run_id"))
	if runID == "" {
		writeError(w, http.StatusBadRequest, "run_id query parameter is required")
		return
	}
	// The scope check above only says "may write claims"; it says nothing
	// about WHOSE. Without this, a token scoped to run-a could pass
	// ?run_id=run-b and irreversibly cascade-delete another run's claims,
	// evidence, embeddings and events — the destructive counterpart to every
	// other run-carrying write path, which has checked the allowlist since F.4.
	if !requireRunScope(w, r, runID) {
		return
	}
	ctx := r.Context()
	requestID := requestIDFromContext(ctx)

	events, err := conn.Events.ListByRunID(ctx, runID)
	if err != nil {
		writeInternalError(w, "list events for delete", err)
		return
	}
	if len(events) == 0 {
		// Nothing to do — idempotent zero-count response.
		writeJSON(w, http.StatusOK, deleteClaimsResponse{RequestID: requestID, RunID: runID})
		return
	}

	// Resolve target claims via the evidence join. A claim only
	// "belongs" to this run_id if at least one evidence row links it
	// to an event in scope. Claims linked to multiple runs survive
	// — only the in-scope evidence rows are dropped.
	eventIDSet := make(map[string]struct{}, len(events))
	for _, e := range events {
		eventIDSet[e.ID] = struct{}{}
	}
	allEvidence, err := conn.Claims.ListAllEvidence(ctx)
	if err != nil {
		writeInternalError(w, "list evidence for delete", err)
		return
	}
	claimToOtherRunEvidence := map[string]bool{}
	targetClaims := map[string]struct{}{}
	for _, link := range allEvidence {
		if _, ok := eventIDSet[link.EventID]; ok {
			targetClaims[link.ClaimID] = struct{}{}
		}
	}
	// Second pass: detect claims that ALSO have evidence outside the
	// run — those must NOT be cascade-deleted; the run-scoped evidence
	// rows go but the claim itself survives.
	for _, link := range allEvidence {
		if _, isTarget := targetClaims[link.ClaimID]; !isTarget {
			continue
		}
		if _, inRun := eventIDSet[link.EventID]; !inRun {
			claimToOtherRunEvidence[link.ClaimID] = true
		}
	}

	resp := deleteClaimsResponse{RequestID: requestID, RunID: runID}
	for cid := range targetClaims {
		if claimToOtherRunEvidence[cid] {
			// Skip cascade — claim is shared with another run. The
			// shared-evidence cleanup happens implicitly when we
			// delete the events below.
			continue
		}
		// One governed action performs the whole per-claim cascade
		// (relationships, embedding, claim_evidence, claim_status_history,
		// claim row), so the destructive op is a single auditable entry
		// on the evidence chain rather than three ungoverned reaches into
		// storage.
		if err := gw.DeleteClaimCascade(ctx, cid); err != nil {
			writeInternalError(w, "delete claim "+cid, err)
			return
		}
		resp.ClaimsDeleted++
		resp.EmbeddingsDeleted++
		resp.RelationshipsDeleted++
	}

	// Events go last so a partial failure above leaves the events
	// referenceable (the operator can re-run the DELETE idempotently
	// to finish). Each event delete is a governed, audited action.
	for _, e := range events {
		if err := gw.DeleteEvent(ctx, e.ID); err != nil {
			writeInternalError(w, "delete event "+e.ID, err)
			return
		}
		resp.EventsDeleted++
	}

	writeJSON(w, http.StatusOK, resp)
}

func appendClaimsHandler(conn *store.Conn, gw *govwrite.Writer, w http.ResponseWriter, r *http.Request) {
	if !requireScope(w, r, domain.ScopeClaimsWrite) {
		return
	}
	var req appendClaimsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("decode body: %v", err))
		return
	}
	if len(req.Claims) == 0 {
		writeError(w, http.StatusBadRequest, "claims array is empty")
		return
	}
	if rejectOversizedBatch(w, "claims", len(req.Claims)) {
		return
	}
	if rejectOversizedBatch(w, "evidence", len(req.Evidence)) {
		return
	}

	claims := make([]domain.Claim, 0, len(req.Claims))
	now := time.Now().UTC()
	actor := actorFromContext(r.Context())
	for i, c := range req.Claims {
		if c.Type != "" && !validClaimType(c.Type) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("claims[%d].type %q invalid", i, c.Type))
			return
		}
		if c.Status != "" && !validClaimStatus(c.Status) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("claims[%d].status %q invalid", i, c.Status))
			return
		}
		if c.Visibility != "" && !validClaimVisibility(c.Visibility) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("claims[%d].visibility %q invalid; must be personal, team, or org", i, c.Visibility))
			return
		}
		created, err := parseTimeFlexible(c.CreatedAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("claims[%d].created_at: %v", i, err))
			return
		}
		if created.IsZero() {
			created = now
		}
		testLastModified, err := parseTimeFlexible(c.TestLastModified)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("claims[%d].test_last_modified: %v", i, err))
			return
		}
		testLastRunAt, err := parseTimeFlexible(c.TestLastRunAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("claims[%d].test_last_run_at: %v", i, err))
			return
		}
		// Per-claim author override (mnemos ClaimItem.CreatedBy): a trusted
		// service attributes on behalf of its sub-actor (worker/coach). Absent
		// → the request's token actor.
		claimActor := actor
		if c.CreatedBy != "" {
			claimActor = c.CreatedBy
		}
		claim := domain.Claim{
			ID:                   c.ID,
			Text:                 c.Text,
			Type:                 domain.ClaimType(c.Type),
			Confidence:           c.Confidence,
			Status:               domain.ClaimStatus(c.Status),
			CreatedAt:            created,
			CreatedBy:            claimActor,
			SourceDocument:       c.SourceDocument,
			SourceType:           domain.SourceType(c.SourceType),
			SourceAuthority:      c.SourceAuthority,
			Liveness:             domain.LivenessStatus(c.Liveness),
			TestID:               c.TestID,
			TestRequirementRef:   c.TestRequirementRef,
			TestAuthor:           c.TestAuthor,
			TestLastModified:     testLastModified,
			TestLastRunAt:        testLastRunAt,
			TestPassCount:        c.TestPassCount,
			TestFailCount:        c.TestFailCount,
			Visibility:           domain.Visibility(c.Visibility),
			ConfidenceComponents: c.ConfidenceComponents,
		}
		claims = append(claims, claim)
	}

	ctx := r.Context()

	// F.4.b: if the bearer is run-scoped, every event referenced by
	// the request's evidence links must belong to an allowed run.
	// We pre-check before any DB write so failures don't leave
	// orphan claims behind.
	if allowed := allowedRunsFromContext(ctx); len(allowed) > 0 && len(req.Evidence) > 0 {
		eventIDs := make([]string, 0, len(req.Evidence))
		for _, e := range req.Evidence {
			eventIDs = append(eventIDs, e.EventID)
		}
		bad, badRun, err := runscope.CheckEventRunsAllowed(ctx, conn, eventIDs, allowed)
		if err != nil {
			writeInternalError(w, "run-scope check", err)
			return
		}
		if bad != "" {
			writeError(w, http.StatusForbidden, fmt.Sprintf("evidence event %q (run %q) not in token whitelist", bad, badRun))
			return
		}
	}

	// changed_by is who PERFORMED the write (the authenticated request actor);
	// per-claim CreatedBy authorship is already stamped on each claim above.
	// Passing the zero value meant no status_history row at all, so anything
	// arriving over REST had no provenance in `mnemos audit who`.
	if _, err := gw.Claims(ctx, claims, govwrite.ClaimReason{
		Reason:    "api: POST /v1/beliefs",
		ChangedBy: actor,
	}); err != nil {
		writeInternalError(w, "upsert claims", err)
		return
	}

	if len(req.Evidence) > 0 {
		links := make([]domain.ClaimEvidence, 0, len(req.Evidence))
		for _, e := range req.Evidence {
			links = append(links, domain.ClaimEvidence{ClaimID: e.ClaimID, EventID: e.EventID})
		}
		if _, err := gw.EvidenceLinks(ctx, links); err != nil {
			writeInternalError(w, "upsert evidence", err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, appendResponse{Accepted: len(claims)})
}

type appendRelationshipsRequest struct {
	Relationships []relationshipDTO `json:"associations"`
}

func appendRelationshipsHandler(conn *store.Conn, gw *govwrite.Writer, w http.ResponseWriter, r *http.Request) {
	if !requireScope(w, r, domain.ScopeRelationshipsWrite) {
		return
	}
	var req appendRelationshipsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("decode body: %v", err))
		return
	}
	if len(req.Relationships) == 0 {
		writeError(w, http.StatusBadRequest, "relationships array is empty")
		return
	}
	if rejectOversizedBatch(w, "relationships", len(req.Relationships)) {
		return
	}

	rels := make([]domain.Relationship, 0, len(req.Relationships))
	now := time.Now().UTC()
	actor := actorFromContext(r.Context())
	for i, rel := range req.Relationships {
		if rel.Type != "supports" && rel.Type != "contradicts" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("relationships[%d].type %q invalid (want supports or contradicts)", i, rel.Type))
			return
		}
		created, err := parseTimeFlexible(rel.CreatedAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("relationships[%d].created_at: %v", i, err))
			return
		}
		if created.IsZero() {
			created = now
		}
		rels = append(rels, domain.Relationship{
			ID:          rel.ID,
			Type:        domain.RelationshipType(rel.Type),
			FromClaimID: rel.FromClaimID,
			ToClaimID:   rel.ToClaimID,
			CreatedAt:   created,
			CreatedBy:   actor,
		})
	}

	// F.4.b: relationships span claims; both endpoint claims' evidence
	// events must lie in the bearer's allowed runs.
	if allowed := allowedRunsFromContext(r.Context()); len(allowed) > 0 {
		claimIDs := make([]string, 0, len(rels)*2)
		seen := map[string]struct{}{}
		for _, rel := range rels {
			for _, id := range []string{rel.FromClaimID, rel.ToClaimID} {
				if _, dup := seen[id]; dup {
					continue
				}
				seen[id] = struct{}{}
				claimIDs = append(claimIDs, id)
			}
		}
		evIDs, err := runscope.ClaimEventIDs(r.Context(), conn, claimIDs)
		if err != nil {
			writeInternalError(w, "run-scope lookup", err)
			return
		}
		bad, badRun, err := runscope.CheckEventRunsAllowed(r.Context(), conn, evIDs, allowed)
		if err != nil {
			writeInternalError(w, "run-scope check", err)
			return
		}
		if bad != "" {
			writeError(w, http.StatusForbidden, fmt.Sprintf("relationship references event %q (run %q) not in token whitelist", bad, badRun))
			return
		}
	}

	if _, err := gw.Relationships(r.Context(), rels); err != nil {
		writeInternalError(w, "upsert relationships", err)
		return
	}
	writeJSON(w, http.StatusCreated, appendResponse{Accepted: len(rels)})
}

// embeddingDTO carries a vector as a JSON array of float32. Larger on the
// wire than a binary blob (typically 5–8× the raw byte size for 768-dim
// vectors), but debuggable, language-agnostic, and bit-exact through the
// encode/decode cycle since float32 has well-defined JSON behavior.
type embeddingDTO struct {
	EntityID   string    `json:"entity_id"`
	EntityType string    `json:"entity_type"`
	Vector     []float32 `json:"vector"`
	Model      string    `json:"model"`
	Dimensions int       `json:"dimensions"`
}

type embeddingsResponse struct {
	Embeddings []embeddingDTO `json:"embeddings"`
	Total      int            `json:"total"`
	Limit      int            `json:"limit"`
	Offset     int            `json:"offset"`
}

type appendEmbeddingsRequest struct {
	Embeddings []embeddingDTO `json:"embeddings"`
}

func makeEmbeddingsHandler(conn *store.Conn, gw *govwrite.Writer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn := scopedConn(r.Context(), conn)
		gw := scopedWriter(r.Context(), gw)
		switch r.Method {
		case http.MethodGet:
			listEmbeddingsHandler(conn, w, r)
		case http.MethodPost:
			appendEmbeddingsHandler(conn, gw, w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

func listEmbeddingsHandler(conn *store.Conn, w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePaginationFromQuery(r)
	typeFilter := r.URL.Query().Get("entity_type")
	ctx := r.Context()
	if typeFilter != "" && typeFilter != "event" && typeFilter != "claim" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid entity_type %q (want event or claim)", typeFilter))
		return
	}

	// Two-pass: when no filter is set we union both entity types so
	// the federation pull can mirror the registry.
	var records []domain.EmbeddingRecord
	wantedTypes := []string{typeFilter}
	if typeFilter == "" {
		wantedTypes = []string{"event", "claim"}
	}
	for _, t := range wantedTypes {
		recs, err := conn.Embeddings.ListByEntityType(ctx, t)
		if err != nil {
			writeInternalError(w, "list embeddings", err)
			return
		}
		records = append(records, recs...)
	}
	total := len(records)
	page := paginate(records, limit, offset)

	out := make([]embeddingDTO, 0, len(page))
	for _, rec := range page {
		out = append(out, embeddingDTO{
			EntityID:   rec.EntityID,
			EntityType: rec.EntityType,
			Vector:     rec.Vector,
			Model:      rec.Model,
			Dimensions: rec.Dimensions,
		})
	}
	writeJSON(w, http.StatusOK, embeddingsResponse{Embeddings: out, Total: total, Limit: limit, Offset: offset})
}

func appendEmbeddingsHandler(conn *store.Conn, gw *govwrite.Writer, w http.ResponseWriter, r *http.Request) {
	if !requireScope(w, r, domain.ScopeEmbeddingsWrite) {
		return
	}
	var req appendEmbeddingsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("decode body: %v", err))
		return
	}
	if len(req.Embeddings) == 0 {
		writeError(w, http.StatusBadRequest, "embeddings array is empty")
		return
	}
	if rejectOversizedBatch(w, "embeddings", len(req.Embeddings)) {
		return
	}

	ctx := r.Context()
	actor := actorFromContext(ctx)

	// F.4.b: validate every embedding's entity belongs to an
	// allowed run before writing any. Event entities are checked
	// directly; claim entities derive their runs through evidence.
	if allowed := allowedRunsFromContext(ctx); len(allowed) > 0 {
		var eventIDs, claimIDs []string
		for _, e := range req.Embeddings {
			switch e.EntityType {
			case "event":
				eventIDs = append(eventIDs, e.EntityID)
			case "claim":
				claimIDs = append(claimIDs, e.EntityID)
			}
		}
		extraEvents, err := runscope.ClaimEventIDs(ctx, conn, claimIDs)
		if err != nil {
			writeInternalError(w, "run-scope lookup", err)
			return
		}
		eventIDs = append(eventIDs, extraEvents...)
		bad, badRun, err := runscope.CheckEventRunsAllowed(ctx, conn, eventIDs, allowed)
		if err != nil {
			writeInternalError(w, "run-scope check", err)
			return
		}
		if bad != "" {
			writeError(w, http.StatusForbidden, fmt.Sprintf("embedding entity references event %q (run %q) not in token whitelist", bad, badRun))
			return
		}
	}

	accepted := 0
	for i, e := range req.Embeddings {
		if e.EntityID == "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("embeddings[%d].entity_id is required", i))
			return
		}
		if e.EntityType != "event" && e.EntityType != "claim" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("embeddings[%d].entity_type %q invalid (want event or claim)", i, e.EntityType))
			return
		}
		if len(e.Vector) == 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("embeddings[%d].vector is empty", i))
			return
		}
		if e.Dimensions != 0 && e.Dimensions != len(e.Vector) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("embeddings[%d]: dimensions=%d but vector length=%d", i, e.Dimensions, len(e.Vector)))
			return
		}
		if err := gw.Embedding(ctx, e.EntityID, e.EntityType, e.Vector, e.Model, actor); err != nil {
			writeInternalError(w, "upsert embedding", err)
			return
		}
		accepted++
	}
	writeJSON(w, http.StatusCreated, appendResponse{Accepted: accepted})
}

type metricsResponse struct {
	Runs            int64 `json:"runs"`
	Events          int64 `json:"episodes"`
	Claims          int64 `json:"beliefs"`
	ContestedClaims int64 `json:"contested_beliefs"`
	Relationships   int64 `json:"associations"`
	Contradictions  int64 `json:"dissonances"`
	Embeddings      int64 `json:"embeddings"`
}

func makeMetricsHandler(conn *store.Conn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn := scopedConn(r.Context(), conn)
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		ctx := r.Context()
		events, _ := conn.Events.ListAll(ctx)
		claims, _ := conn.Claims.ListAll(ctx)
		// Same union pattern as listEmbeddingsHandler — no port-level
		// "list every embedding type" since we deliberately segregate
		// event vs claim vectors at the storage layer.
		eventEmbs, _ := conn.Embeddings.ListByEntityType(ctx, "event")
		claimEmbs, _ := conn.Embeddings.ListByEntityType(ctx, "claim")

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
		// Relationships need the union of every claim's edges.
		ids := make([]string, 0, len(claims))
		for _, c := range claims {
			ids = append(ids, c.ID)
		}
		rels, _ := conn.Relationships.ListByClaimIDs(ctx, ids)
		var contradictions int64
		for _, rel := range rels {
			if rel.Type == domain.RelationshipTypeContradicts {
				contradictions++
			}
		}

		writeJSON(w, http.StatusOK, metricsResponse{
			Runs:            int64(len(runs)),
			Events:          int64(len(events)),
			Claims:          int64(len(claims)),
			ContestedClaims: contestedClaims,
			Relationships:   int64(len(rels)),
			Contradictions:  contradictions,
			Embeddings:      int64(len(eventEmbs) + len(claimEmbs)),
		})
	}
}

// parsePaginationFromQuery reads ?limit and ?offset query params with the
// same defaults/caps as the MCP browse handlers. Invalid values are
// silently coerced rather than rejected — query strings are best-effort.
func parsePaginationFromQuery(r *http.Request) (int, int) {
	limit := defaultServeLimit
	offset := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxServePageLimit {
		limit = maxServePageLimit
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Body already partially written — log to stderr but can't change the status.
		fmt.Fprintf(os.Stderr, "writeJSON: %v\n", err)
	}
}

type errorResponse struct {
	Error string `json:"error"`
}

// writeError emits a typed error response. 4xx messages are shown to
// the client verbatim because they describe a problem with the
// client's request. 5xx messages leak internal state — raw SQL
// errors, file paths, driver details — if passed through directly,
// so callers hitting the 500 path should prefer writeInternalError
// below.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// writeInternalError is the sanitizing variant for the 500 code path.
// The full detail is logged to stderr (so operators can debug) but
// the HTTP response body only carries a generic "internal error"
// plus the context label that helps bucket issues in dashboards
// without leaking anything about the DB schema or filesystem.
func writeInternalError(w http.ResponseWriter, label string, cause error) {
	fmt.Fprintf(os.Stderr, "serve 500 [%s]: %v\n", label, cause)
	writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error: " + label})
}

// contextRequest is the body for POST /v1/context. run_id is required;
// query and max_tokens are optional.
type contextRequest struct {
	RunID     string `json:"run_id"`
	Query     string `json:"query,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
}

// contextResponse wraps the rendered Context Block string in JSON so
// callers can inline it (the canonical "drop into your system prompt"
// path) without splitting on a content-type guess.
type contextResponse struct {
	RunID   string `json:"run_id"`
	Context string `json:"context"`
}

// makeContextHandler returns the /v1/context handler. POST returns a
// rendered Context Block for the given run; GET ?run_id=… is a thin
// alias so curl users don't have to send a body.
func makeContextHandler(conn *store.Conn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn := scopedConn(r.Context(), conn)
		var req contextRequest
		switch r.Method {
		case http.MethodGet:
			req.RunID = r.URL.Query().Get("run_id")
			req.Query = r.URL.Query().Get("query")
			if v := r.URL.Query().Get("max_tokens"); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 0 {
					writeError(w, http.StatusBadRequest, "max_tokens must be a non-negative integer")
					return
				}
				req.MaxTokens = n
			}
		case http.MethodPost:
			// /v1/context is read-only; the JWT middleware skips reads
			// (see jwtAuthMiddleware). Treat POST the same way — its
			// only purpose here is to accept a JSON body for clients
			// that prefer it to query strings.
			if err := decodeJSON(r, &req); err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("decode body: %v", err))
				return
			}
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if req.RunID == "" {
			writeError(w, http.StatusBadRequest, "run_id is required")
			return
		}
		// A Context Block is a rendered digest of a run's whole knowledge
		// state — the single most concentrated read on the surface. run_id was
		// required but unbound, so any bearer could render any run's block.
		if !requireRunScope(w, r, req.RunID) {
			return
		}

		engine := query.NewEngine(conn.Events, conn.Claims, conn.Relationships)
		block, err := engine.BuildContextBlock(r.Context(), query.ContextBlockOptions{
			RunID:     req.RunID,
			Query:     req.Query,
			MaxTokens: req.MaxTokens,
		})
		if err != nil {
			writeInternalError(w, "build context block", err)
			return
		}
		writeJSON(w, http.StatusOK, contextResponse{RunID: req.RunID, Context: block})
	}
}

// searchRequest is the body for POST /v1/search. The query is the
// only required field; everything else is an optional filter.
type searchRequest struct {
	Query        string         `json:"query"`
	RunID        string         `json:"run_id,omitempty"`
	TopK         int            `json:"top_k,omitempty"`
	MinTrust     float64        `json:"min_trust,omitempty"`
	AsOf         string         `json:"as_of,omitempty"`          // validity time
	RecordedAsOf string         `json:"recorded_as_of,omitempty"` // ingestion time
	Scope        *searchScope   `json:"scope,omitempty"`
	Filters      *searchFilters `json:"filters,omitempty"`
	// Consumer controls contradiction handling: "agent" triggers automatic
	// trust-based resolution; "user" (default) surfaces contradictions with
	// a human-readable explanation.
	Consumer string `json:"consumer,omitempty"`
	// Visibility controls workspace isolation: "personal", "team" (default), or "org".
	// personal – only personal claims; team – personal+team; org – all claims.
	Visibility string `json:"visibility,omitempty"`
}

type searchScope struct {
	Service string `json:"service,omitempty"`
	Env     string `json:"env,omitempty"`
	Team    string `json:"team,omitempty"`
}

type searchFilters struct {
	ClaimType string `json:"type,omitempty"`
	Status    string `json:"status,omitempty"`
}

// searchClaimDTO is a slim subset of domain.Claim for /v1/search
// responses — agents asking for retrieval don't need every field on
// the underlying record.
type searchClaimDTO struct {
	ID         string  `json:"id"`
	Text       string  `json:"text"`
	Type       string  `json:"type"`
	Status     string  `json:"status"`
	Confidence float64 `json:"confidence"`
	TrustScore float64 `json:"trust_score"`
}

type searchResponse struct {
	Query          string                 `json:"query"`
	Claims         []searchClaimDTO       `json:"beliefs"`
	Contradictions []relationshipDTO      `json:"dissonances"`
	Total          int                    `json:"total"`
	TopK           int                    `json:"top_k"`
	AppliedFilters map[string]interface{} `json:"applied_filters,omitempty"`
	// AutoResolved is true when the engine automatically resolved one or more
	// contradictions on behalf of an agent consumer.
	AutoResolved bool `json:"auto_resolved,omitempty"`
	// ContradictionExplanation is a human-readable explanation of
	// unresolved contradictions, populated when consumer=user.
	ContradictionExplanation string `json:"contradiction_explanation,omitempty"`
	// Verdicts contains one structured resolution entry per contradicting
	// claim pair. Only populated when consumer=agent.
	Verdicts []verdictDTO `json:"verdicts,omitempty"`
}

// makeSearchHandler returns the /v1/search handler. Hybrid retrieval:
// internal/query.AnswerWithOptions already combines BM25 with cosine
// similarity (when embeddings are configured) and hop-expansion over
// the relationship graph; this endpoint exposes that surface with a
// filter DSL on top.
func makeSearchHandler(conn *store.Conn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn := scopedConn(r.Context(), conn)
		var req searchRequest
		switch r.Method {
		case http.MethodGet:
			req.Query = r.URL.Query().Get("query")
			req.RunID = r.URL.Query().Get("run_id")
			if v := r.URL.Query().Get("top_k"); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 0 {
					writeError(w, http.StatusBadRequest, "top_k must be a non-negative integer")
					return
				}
				req.TopK = n
			}
			if v := r.URL.Query().Get("min_trust"); v != "" {
				f, err := strconv.ParseFloat(v, 64)
				if err != nil || f < 0 || f > 1 {
					writeError(w, http.StatusBadRequest, "min_trust must be a float in [0,1]")
					return
				}
				req.MinTrust = f
			}
			req.AsOf = r.URL.Query().Get("as_of")
			req.RecordedAsOf = r.URL.Query().Get("recorded_as_of")
			req.Consumer = r.URL.Query().Get("consumer")
		case http.MethodPost:
			if err := decodeJSON(r, &req); err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("decode body: %v", err))
				return
			}
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if req.Query == "" {
			writeError(w, http.StatusBadRequest, "query is required")
			return
		}
		// run_id is an optional filter here, which for a run-scoped bearer is
		// the same hole as on /v1/episodes: omitting it selected
		// AnswerWithOptions over AnswerForRunWithOptions and searched the
		// whole brain. Fail closed instead.
		if !requireRunScope(w, r, req.RunID) {
			return
		}
		if req.TopK <= 0 {
			req.TopK = 10
		}

		// Same defaults as MCP and the CLI: a hosted brain is still a brain.
		opts := query.AnswerOptions{MinTrust: req.MinTrust}.WithCognitiveDefaults()
		if req.AsOf != "" {
			t, err := parseTimeFlexible(req.AsOf)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("as_of: %v", err))
				return
			}
			opts.AsOf = t
		}
		if req.RecordedAsOf != "" {
			t, err := parseTimeFlexible(req.RecordedAsOf)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("recorded_as_of: %v", err))
				return
			}
			opts.RecordedAsOf = t
		}
		if req.Scope != nil {
			opts.Scope = domain.Scope{
				Service: req.Scope.Service,
				Env:     req.Scope.Env,
				Team:    req.Scope.Team,
			}
		}
		if req.Consumer == "agent" {
			opts.Consumer = domain.ConsumerAgent
		} else {
			opts.Consumer = domain.ConsumerUser
		}
		switch domain.Visibility(req.Visibility) {
		case domain.VisibilityPersonal, domain.VisibilityTeam, domain.VisibilityOrg:
			opts.Visibility = domain.Visibility(req.Visibility)
		default:
			// zero / unknown → engine default (team)
		}

		// Fully wired: embeddings, full-text, LLM, decisions. This built a bare
		// engine and ranked by token overlap — on the path the hosted recall hook
		// uses for every prompt.
		engine := newQueryEngine(conn)
		var answer domain.Answer
		var err error
		if req.RunID != "" {
			answer, err = engine.AnswerForRunWithOptions(r.Context(), req.Query, req.RunID, opts)
		} else {
			answer, err = engine.AnswerWithOptions(r.Context(), req.Query, opts)
		}
		if err != nil {
			writeInternalError(w, "search", err)
			return
		}

		// Post-filter on type/status — these aren't AnswerOptions
		// fields. Cheap to do at the HTTP layer; cleaner than
		// reaching into the engine.
		filtered := answer.Claims
		if req.Filters != nil {
			if req.Filters.ClaimType != "" || req.Filters.Status != "" {
				kept := make([]domain.Claim, 0, len(filtered))
				for _, c := range filtered {
					if req.Filters.ClaimType != "" && string(c.Type) != req.Filters.ClaimType {
						continue
					}
					if req.Filters.Status != "" && string(c.Status) != req.Filters.Status {
						continue
					}
					kept = append(kept, c)
				}
				filtered = kept
			}
		}

		// Truncate to top_k after filter so the cap reflects what the
		// caller asked to see, not what the engine returned upstream.
		total := len(filtered)
		if total > req.TopK {
			filtered = filtered[:req.TopK]
		}

		dto := make([]searchClaimDTO, 0, len(filtered))
		for _, c := range filtered {
			dto = append(dto, searchClaimDTO{
				ID:         c.ID,
				Text:       c.Text,
				Type:       string(c.Type),
				Status:     string(c.Status),
				Confidence: c.Confidence,
				TrustScore: c.TrustScore,
			})
		}

		contradictions := make([]relationshipDTO, 0, len(answer.Contradictions))
		for _, rel := range answer.Contradictions {
			contradictions = append(contradictions, relationshipDTO{
				ID:          rel.ID,
				Type:        string(rel.Type),
				FromClaimID: rel.FromClaimID,
				ToClaimID:   rel.ToClaimID,
				CreatedAt:   rel.CreatedAt.UTC().Format(time.RFC3339),
			})
		}

		applied := map[string]interface{}{}
		if req.RunID != "" {
			applied["run_id"] = req.RunID
		}
		if req.MinTrust > 0 {
			applied["min_trust"] = req.MinTrust
		}
		if !opts.AsOf.IsZero() {
			applied["as_of"] = opts.AsOf.UTC().Format(time.RFC3339)
		}
		if !opts.RecordedAsOf.IsZero() {
			applied["recorded_as_of"] = opts.RecordedAsOf.UTC().Format(time.RFC3339)
		}
		if req.Filters != nil {
			if req.Filters.ClaimType != "" {
				applied["type"] = req.Filters.ClaimType
			}
			if req.Filters.Status != "" {
				applied["status"] = req.Filters.Status
			}
		}

		verdictDTOs := make([]verdictDTO, 0, len(answer.Verdicts))
		for _, v := range answer.Verdicts {
			verdictDTOs = append(verdictDTOs, verdictDTO{
				WinnerClaimID:    v.WinnerClaimID,
				LoserClaimID:     v.LoserClaimID,
				Confidence:       v.Confidence,
				Rationale:        v.Rationale,
				Action:           string(v.Action),
				EscalationReason: v.EscalationReason,
			})
		}

		writeJSON(w, http.StatusOK, searchResponse{
			Query:                    req.Query,
			Claims:                   dto,
			Contradictions:           contradictions,
			Total:                    total,
			TopK:                     req.TopK,
			AppliedFilters:           applied,
			AutoResolved:             answer.AutoResolved,
			ContradictionExplanation: answer.ContradictionExplanation,
			Verdicts:                 verdictDTOs,
		})
	}
}

// makeClaimSubresourceHandler routes requests under /v1/beliefs/<id>/<subresource>.
// Currently supports:
//
//	GET /v1/beliefs/<id>/provenance  → trust provenance report for the belief
//	GET /v1/beliefs/<id>/export.md   → human-readable markdown export with provenance annotations
func makeClaimSubresourceHandler(conn *store.Conn, gw *govwrite.Writer, mem mnemos.Memory) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn := scopedConn(r.Context(), conn)
		gw := scopedWriter(r.Context(), gw)
		mem := scopedMem(r.Context(), mem)
		// path: /v1/beliefs/<id>/<subresource>
		// Strip the prefix so we can parse id + subresource.
		tail := strings.TrimPrefix(r.URL.Path, "/v1/beliefs/")
		parts := strings.SplitN(tail, "/", 2)
		// Bare /v1/beliefs/<id> (no subresource) → single-belief Get (v0.67).
		if len(parts) == 1 && parts[0] != "" {
			makeGetClaimHandler(mem, parts[0])(w, r)
			return
		}
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		claimID, subresource := parts[0], parts[1]

		switch subresource {
		case "lifecycle":
			makeLifecycleHandler(mem, claimID)(w, r)
		case "provenance":
			makeProvenanceHandler(conn, claimID)(w, r)
		case "export.md":
			makeClaimMarkdownExportHandler(conn, claimID)(w, r)
		case "feedback":
			makeFeedbackHandler(conn, gw, claimID)(w, r)
		case "history":
			makeClaimHistoryHandler(conn, claimID)(w, r)
		case "expectation":
			makeExpectationHandler(conn, claimID)(w, r)
		case "observation":
			makeObservationHandler(conn, claimID)(w, r)
		case "analogous":
			makeAnalogousHandler(mem, claimID)(w, r)
		default:
			writeError(w, http.StatusNotFound, fmt.Sprintf("unknown subresource %q", subresource))
		}
	}
}

// claimVersionDTO is the wire shape for one row of the version chain.
type claimVersionDTO struct {
	ClaimID    string  `json:"claim_id"`
	Version    int     `json:"version"`
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
	Status     string  `json:"status"`
	WrittenAt  string  `json:"written_at"`
	WrittenBy  string  `json:"written_by"`
}

type claimHistoryResponse struct {
	ClaimID  string            `json:"claim_id"`
	Versions []claimVersionDTO `json:"versions"`
}

// makeClaimHistoryHandler handles GET /v1/beliefs/<id>/history. Returns
// the full version chain newest-first, so a caller diffing can read
// versions[0].text vs versions[1].text without re-sorting client-side.
func makeClaimHistoryHandler(conn *store.Conn, claimID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn := scopedConn(r.Context(), conn)
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if conn.ClaimVersions == nil {
			writeError(w, http.StatusNotImplemented, "version history not available on this backend")
			return
		}
		versions, err := conn.ClaimVersions.ListByClaim(r.Context(), claimID)
		if err != nil {
			writeInternalError(w, "list claim versions", err)
			return
		}
		out := claimHistoryResponse{ClaimID: claimID, Versions: make([]claimVersionDTO, 0, len(versions))}
		for _, v := range versions {
			out.Versions = append(out.Versions, claimVersionDTO{
				ClaimID:    v.ClaimID,
				Version:    v.Version,
				Text:       v.Text,
				Confidence: v.Confidence,
				Status:     string(v.Status),
				WrittenAt:  v.WrittenAt.UTC().Format(time.RFC3339Nano),
				WrittenBy:  v.WrittenBy,
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// feedbackRequest is the inbound wire shape for
// POST /v1/beliefs/<id>/feedback.
type feedbackRequest struct {
	// Helpful is the binary signal: true = the claim was useful,
	// false = the consumer pushed back.
	Helpful bool `json:"helpful"`
	// Note is an optional reviewer note kept verbatim on the claim's
	// feedback row. Useful for the human-review path; omit for
	// programmatic signals.
	Note string `json:"note,omitempty"`
}

// feedbackResponse echoes the post-application state of the claim
// plus the feedback counters, so a caller can react to the streak
// transition without a second GET.
type feedbackResponse struct {
	ClaimID                string  `json:"claim_id"`
	Helpful                bool    `json:"helpful"`
	NewStatus              string  `json:"new_status"`
	NewConfidence          float64 `json:"new_confidence"`
	NegativeFeedbackStreak int     `json:"negative_feedback_streak"`
	HelpfulCount           int     `json:"helpful_count"`
	AutoContested          bool    `json:"auto_contested"`
}

// feedbackDecayFactor is the per-negative-vote multiplier applied to
// the scalar Confidence. 0.9 ≈ "each push-back trims 10% of
// confidence" — keeps the decay shallow enough that a single bad
// vote can't tank a well-corroborated claim while still being
// observable. Env-tunable in case operators want a sharper or
// softer slope.
func feedbackDecayFactor() float64 {
	return envFloat("MNEMOS_FEEDBACK_DECAY", 0.9)
}

// feedbackAutoContestThreshold is the consecutive-negative count
// above which the claim's status auto-transitions to "contested".
// 3 by default — enough to suppress noise from a single reviewer
// having a bad day, low enough to surface real drift.
func feedbackAutoContestThreshold() int {
	return envInt("MNEMOS_FEEDBACK_CONTEST_THRESHOLD", 3)
}

// makeFeedbackHandler returns the POST /v1/beliefs/<id>/feedback
// handler. Reads the current claim + feedback row, applies the
// helpful/not-helpful signal (confidence decay on negative, streak
// reset on positive), auto-transitions to "contested" when the
// negative streak crosses the threshold, persists both rows.
func makeFeedbackHandler(conn *store.Conn, gw *govwrite.Writer, claimID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn := scopedConn(r.Context(), conn)
		gw := scopedWriter(r.Context(), gw)
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !requireScope(w, r, domain.ScopeClaimsWrite) {
			return
		}
		if conn.Feedback == nil {
			writeError(w, http.StatusNotImplemented, "feedback storage not available on this backend")
			return
		}
		var req feedbackRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("decode body: %v", err))
			return
		}
		ctx := r.Context()
		claims, err := conn.Claims.ListByIDs(ctx, []string{claimID})
		if err != nil || len(claims) == 0 {
			writeError(w, http.StatusNotFound, fmt.Sprintf("claim %q not found", claimID))
			return
		}
		claim := claims[0]

		state, _, err := conn.Feedback.Get(ctx, claimID)
		if err != nil {
			writeInternalError(w, "load feedback state", err)
			return
		}
		state.ClaimID = claimID
		now := time.Now().UTC()
		state.LastFeedbackAt = now
		state.LastFeedbackNote = req.Note

		autoContested := false
		if req.Helpful {
			state.NegativeFeedbackStreak = 0
			state.HelpfulCount++
		} else {
			state.NegativeFeedbackStreak++
			// Decay the scalar confidence. The "corroboration"
			// component (if surfaced) shrinks in lockstep so the
			// decomposition stays internally consistent.
			claim.Confidence *= feedbackDecayFactor()
			if claim.Confidence < 0 {
				claim.Confidence = 0
			}
			if claim.ConfidenceComponents != nil {
				if v, ok := claim.ConfidenceComponents["corroboration"]; ok {
					claim.ConfidenceComponents["corroboration"] = v * feedbackDecayFactor()
				}
			}
			if state.NegativeFeedbackStreak >= feedbackAutoContestThreshold() && claim.Status == domain.ClaimStatusActive {
				claim.Status = domain.ClaimStatusContested
				autoContested = true
			}
		}

		actor := actorFromContext(ctx)
		reason := "agent-feedback"
		if !req.Helpful {
			reason = "agent-feedback-negative"
		}
		if _, err := gw.Claims(ctx, []domain.Claim{claim}, govwrite.ClaimReason{Reason: reason, ChangedBy: actor}); err != nil {
			writeInternalError(w, "update claim", err)
			return
		}
		if _, err := gw.Feedback(ctx, state); err != nil {
			writeInternalError(w, "store feedback", err)
			return
		}
		writeJSON(w, http.StatusOK, feedbackResponse{
			ClaimID:                claimID,
			Helpful:                req.Helpful,
			NewStatus:              string(claim.Status),
			NewConfidence:          claim.Confidence,
			NegativeFeedbackStreak: state.NegativeFeedbackStreak,
			HelpfulCount:           state.HelpfulCount,
			AutoContested:          autoContested,
		})
	}
}

// envFloat reads a float64 env var with a default.
func envFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}

// envInt reads an int env var with a default.
func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// makeProvenanceHandler handles GET /v1/beliefs/<id>/provenance.
// It builds a query.Engine and delegates to WhyTrustClaim to produce a
// structured domain.ProvenanceReport explaining how the claim's trust
// score was computed from its provenance signals.
func makeProvenanceHandler(conn *store.Conn, claimID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn := scopedConn(r.Context(), conn)
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		ctx := r.Context()
		eng := query.NewEngine(conn.Events, conn.Claims, conn.Relationships)
		report, err := eng.WhyTrustClaim(ctx, claimID)
		if err != nil {
			writeError(w, http.StatusNotFound, fmt.Sprintf("provenance query failed: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, report)
	}
}

// makeClaimMarkdownExportHandler handles GET /v1/beliefs/<id>/export.md.
// It fetches the claim, runs a provenance report (unless ?provenance=false
// is passed), and returns a Git-friendly markdown document with YAML
// frontmatter carrying trust score, confidence rationale, and source links.
func makeClaimMarkdownExportHandler(conn *store.Conn, claimID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn := scopedConn(r.Context(), conn)
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		ctx := r.Context()
		claims, err := conn.Claims.ListByIDs(ctx, []string{claimID})
		if err != nil || len(claims) == 0 {
			writeError(w, http.StatusNotFound, fmt.Sprintf("claim %q not found", claimID))
			return
		}
		c := claims[0]
		// Provenance enrichment is opt-out: pass ?provenance=false to skip.
		var report *domain.ProvenanceReport
		if r.URL.Query().Get("provenance") != "false" {
			eng := query.NewEngine(conn.Events, conn.Claims, conn.Relationships)
			rpt, pErr := eng.WhyTrustClaim(ctx, claimID)
			if pErr == nil {
				report = &rpt
			}
			// Non-fatal: if provenance fails we still return the claim markdown.
		}
		rendered, err := markdownpkg.ExportClaim(c, report)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("render markdown: %v", err))
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.md"`, claimID))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(rendered)) //nolint:gosec // G705: rendered is produced by our own markdown.ExportClaim, not from user input
	}
}

// incidentRequest is the body for POST /v1/incidents.
type incidentRequest struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	Summary          string `json:"summary"`
	Severity         string `json:"severity"`
	RootCauseClaimID string `json:"root_cause_claim_id"`
	// CreatedBy is accepted and IGNORED. Authorship comes from the
	// authenticated token (actorFromContext), never from the body — a caller
	// that could name its own author could file an incident under someone
	// else's identity. The field stays on the DTO only so existing clients
	// that still send it don't trip the strict decoder with a 400.
	CreatedBy string `json:"created_by"`
}

// makeIncidentsHandler handles:
//
//	POST /v1/incidents  → open a new incident
//	GET  /v1/incidents  → list all incidents (supports ?severity=<s> and ?status=<s>)
func makeIncidentsHandler(conn *store.Conn, gw *govwrite.Writer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn := scopedConn(r.Context(), conn)
		gw := scopedWriter(r.Context(), gw)
		switch r.Method {
		case http.MethodPost:
			openIncidentHandler(gw, w, r)
		case http.MethodGet:
			listIncidentsHandler(conn, w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

func openIncidentHandler(gw *govwrite.Writer, w http.ResponseWriter, r *http.Request) {
	// Opening an incident is a durable, governed write; it was the only REST
	// POST that took no scope at all, so a read-only token could file
	// incidents. Incidents are claim-adjacent records, hence claims:write —
	// the same gate the rest of the knowledge write surface uses.
	if !requireScope(w, r, domain.ScopeClaimsWrite) {
		return
	}
	var req incidentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid body: %v", err))
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	id := req.ID
	if id == "" {
		gen, err := newID("inc_")
		if err != nil {
			writeInternalError(w, "generate incident id", err)
			return
		}
		id = gen
	}
	severity := domain.IncidentSeverity(req.Severity)
	if severity == "" {
		severity = domain.IncidentSeverityMedium
	}
	ctx := r.Context()
	inc := domain.Incident{
		ID:               id,
		Title:            req.Title,
		Summary:          req.Summary,
		Severity:         severity,
		Status:           domain.IncidentStatusOpen,
		RootCauseClaimID: req.RootCauseClaimID,
		OpenedAt:         time.Now().UTC(),
		// Attribution follows the token, not req.CreatedBy — every other write
		// path on this surface stamps the context actor, and a body-supplied
		// author is forgeable.
		CreatedBy: actorFromContext(ctx),
	}
	if _, err := gw.Incident(ctx, inc); err != nil {
		writeInternalError(w, "upsert incident", err)
		return
	}
	writeJSON(w, http.StatusCreated, inc)
}

func listIncidentsHandler(conn *store.Conn, w http.ResponseWriter, r *http.Request) {
	if conn.Incidents == nil {
		writeError(w, http.StatusNotImplemented, "incident storage not available on this backend")
		return
	}
	ctx := r.Context()
	severityFilter := r.URL.Query().Get("severity")
	statusFilter := r.URL.Query().Get("status")
	var (
		incidents []domain.Incident
		err       error
	)
	switch {
	case severityFilter != "":
		incidents, err = conn.Incidents.ListBySeverity(ctx, domain.IncidentSeverity(severityFilter))
	case statusFilter != "":
		incidents, err = conn.Incidents.ListByStatus(ctx, domain.IncidentStatus(statusFilter))
	default:
		incidents, err = conn.Incidents.ListAll(ctx)
	}
	if err != nil {
		writeInternalError(w, "list incidents", err)
		return
	}
	writeJSON(w, http.StatusOK, incidents)
}

// makeIncidentSubresourceHandler handles:
//
//	GET /v1/incidents/<id>              → fetch incident by ID
//	POST /v1/incidents/<id>/resolve     → mark incident resolved
//	GET  /v1/incidents/<id>/why-wrong   → WhyWereWeWrong analysis
func makeIncidentSubresourceHandler(conn *store.Conn, gw *govwrite.Writer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn := scopedConn(r.Context(), conn)
		gw := scopedWriter(r.Context(), gw)
		if conn.Incidents == nil {
			writeError(w, http.StatusNotImplemented, "incident storage not available on this backend")
			return
		}
		// Strip "/v1/incidents/" prefix and split remainder.
		prefix := "/v1/incidents/"
		tail := strings.TrimPrefix(r.URL.Path, prefix)
		parts := strings.SplitN(tail, "/", 2)
		id := parts[0]
		if id == "" {
			writeError(w, http.StatusBadRequest, "incident id required")
			return
		}
		sub := ""
		if len(parts) == 2 {
			sub = parts[1]
		}

		ctx := r.Context()
		switch {
		case sub == "" && r.Method == http.MethodGet:
			inc, found, err := conn.Incidents.GetByID(ctx, id)
			if err != nil {
				writeInternalError(w, "get incident", err)
				return
			}
			if !found {
				writeError(w, http.StatusNotFound, fmt.Sprintf("incident %q not found", id))
				return
			}
			writeJSON(w, http.StatusOK, inc)

		case sub == "resolve" && r.Method == http.MethodPost:
			// Closing an incident is a governed state change and needs the
			// same gate as opening one; it was reachable with any valid token.
			if !requireScope(w, r, domain.ScopeClaimsWrite) {
				return
			}
			if err := gw.ResolveIncident(ctx, id, time.Now().UTC()); err != nil {
				writeInternalError(w, "resolve incident", err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": string(domain.IncidentStatusResolved)})

		case sub == "why-wrong" && r.Method == http.MethodGet:
			eng := query.NewEngine(conn.Events, conn.Claims, conn.Relationships).
				WithDecisions(conn.Decisions).
				WithIncidents(conn.Incidents)
			report, err := eng.WhyWereWeWrong(ctx, id)
			if err != nil {
				writeError(w, http.StatusNotFound, fmt.Sprintf("why-wrong analysis failed: %v", err))
				return
			}
			writeJSON(w, http.StatusOK, report)

		default:
			writeError(w, http.StatusNotFound, fmt.Sprintf("unknown incident sub-resource %q", sub))
		}
	}
}
