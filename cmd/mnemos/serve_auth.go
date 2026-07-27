package main

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"go.klarlabs.de/bolt"
	"go.klarlabs.de/mnemos/internal/auth"
	"go.klarlabs.de/mnemos/internal/domain"
)

// actorContextKey tags the resolved user id on a request's context so
// downstream handlers can stamp it into created_by columns. A distinct
// unexported type keeps us from colliding with context keys from other
// packages.
type actorContextKey struct{}

// scopesContextKey tags the bearer's scope list onto the request
// context so handlers can call requireScope without re-parsing the
// token.
type scopesContextKey struct{}

// runsContextKey tags the bearer's run-id whitelist onto the request
// context. Empty list (no key set) means no run restriction.
type runsContextKey struct{}

// withActor returns a copy of ctx carrying the given user id. It also reports
// the identity back up to the access log through the actor sink (see
// actorSink) — middleware contexts only flow downward, so without the sink the
// outermost log can never see who auth resolved.
func withActor(ctx context.Context, userID string) context.Context {
	actorSinkFromContext(ctx).set(userID)
	return context.WithValue(ctx, actorContextKey{}, userID)
}

// actorSinkKey tags the mutable actor cell that the access-log middleware
// installs before auth runs.
type actorSinkKey struct{}

// actorSink is that cell: a one-slot, concurrency-safe holder the access log
// reads after the handler chain returns.
//
// Why it exists. jwtAuthMiddleware installs the token subject on a DERIVED
// request, and a derived context is only visible to handlers BELOW it — so
// boltAccessLog, which sits above auth, always read the bare request context
// and logged user_id=<system> for every call. The log's stated purpose is
// tracing writes back to the issuing identity, so it was reporting the one
// field it exists for incorrectly.
//
// Why not simply move the log below auth: auth writes its own 401/403 and
// returns WITHOUT calling downstream, so an inner log would stop recording
// rejected requests — precisely the ones a security audit trail needs. The
// sink keeps the log outermost (every request logged, denials included) while
// still observing the identity resolved deeper in the stack.
type actorSink struct {
	mu sync.Mutex
	id string
}

// set records the resolved identity. Nil-safe: contexts built outside the
// serve stack (tests, the CLI hook paths) carry no sink.
func (s *actorSink) set(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.id = id
}

// get returns the recorded identity, or "" when auth never resolved one.
func (s *actorSink) get() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// withActorSink installs a fresh sink on ctx and hands it back so the caller
// can read it once the handler chain has run.
func withActorSink(ctx context.Context) (context.Context, *actorSink) {
	s := &actorSink{}
	return context.WithValue(ctx, actorSinkKey{}, s), s
}

// actorSinkFromContext returns the installed sink, or nil.
func actorSinkFromContext(ctx context.Context) *actorSink {
	s, _ := ctx.Value(actorSinkKey{}).(*actorSink)
	return s
}

// withScopes returns a copy of ctx carrying the bearer's scope list.
func withScopes(ctx context.Context, scopes []string) context.Context {
	return context.WithValue(ctx, scopesContextKey{}, scopes)
}

// withAllowedRuns returns a copy of ctx carrying the bearer's
// run-id whitelist. Pass nil/empty to leave the request unrestricted.
func withAllowedRuns(ctx context.Context, runs []string) context.Context {
	return context.WithValue(ctx, runsContextKey{}, runs)
}

// allowedRunsFromContext returns the bearer's run whitelist, or nil
// when no token was presented or the token had no restriction.
func allowedRunsFromContext(ctx context.Context) []string {
	if v, ok := ctx.Value(runsContextKey{}).([]string); ok {
		return v
	}
	return nil
}

// runAllowedForRequest reports whether the request's bearer may touch runID,
// without writing a response. For batch pre-checks that phrase their own
// error (which element of the payload offended); single-value call sites
// should use requireRunScope instead.
//
// A bearer with no run allowlist is unrestricted. Matching delegates to
// [auth.Claims.AllowsRun], so "*" and path.Match globs (`prod-*`) behave
// identically over REST and MCP.
func runAllowedForRequest(r *http.Request, runID string) bool {
	allowed := allowedRunsFromContext(r.Context())
	if len(allowed) == 0 {
		return true
	}
	return auth.Claims{Runs: allowed}.AllowsRun(runID)
}

// requireRunScope confines a run-carrying request to the bearer's run
// allowlist, writing a 403 and returning false when it must not proceed.
// A bearer with no allowlist (the unrestricted default) passes anything,
// including an empty runID.
//
// It FAILS CLOSED on an absent run id, and that is the point. run_id is an
// OPTIONAL filter on the read surface (/v1/episodes, /v1/beliefs,
// /v1/search) — so a run-scoped token that simply omitted the parameter used
// to receive the whole corpus, turning "confined to run X" into "may read
// every run". A token that names runs must name one.
//
// Destructive and write paths call it with a run id that is already required
// by the route, so the empty-id branch never fires there.
func requireRunScope(w http.ResponseWriter, r *http.Request, runID string) bool {
	if len(allowedRunsFromContext(r.Context())) == 0 {
		return true
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		writeError(w, http.StatusForbidden, "token is run-scoped: run_id is required")
		return false
	}
	if !runAllowedForRequest(r, runID) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("run_id %q not in token whitelist", runID))
		return false
	}
	return true
}

// actorFromContext returns the user id previously installed via withActor.
// When the request is unauthenticated (reads), falls back to SystemUser so
// the caller always has a non-empty string to stamp.
func actorFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(actorContextKey{}).(string); ok && v != "" {
		return v
	}
	return domain.SystemUser
}

// scopesFromContext returns the bearer's scope list, or an empty slice
// when no token was presented (read-only requests).
func scopesFromContext(ctx context.Context) []string {
	if v, ok := ctx.Value(scopesContextKey{}).([]string); ok {
		return v
	}
	return nil
}

// requireScope returns true and writes nothing when the request's
// scope list grants want; otherwise it writes a 403 and returns false.
// Handlers should `if !requireScope(w, r, "events:write") { return }`
// at the very top of their POST path.
func requireScope(w http.ResponseWriter, r *http.Request, want string) bool {
	for _, s := range scopesFromContext(r.Context()) {
		if s == domain.ScopeWildcard || s == want {
			return true
		}
	}
	writeError(w, http.StatusForbidden, "missing required scope: "+want)
	return false
}

// jwtAuthMiddleware enforces JWT auth. Secure by default: every request —
// reads included — requires a valid token, EXCEPT bare-liveness/static infra
// endpoints (health probes, marketing landing, SPA shell) and, only when the
// operator explicitly opts in with publicReads (serve --public-reads /
// MNEMOS_PUBLIC_READS), anonymous GET/HEAD/OPTIONS reads. Multi-tenant mode
// (requireTenant) always authenticates every request so a tenant can be
// resolved, and ignores publicReads.
//
// Prometheus metrics (/internal/metrics) are AUTHENTICATED by default — the RED
// series expose per-route traffic shape and latency, which must not be
// world-readable on a hosted listener. An operator scraping over a trusted
// internal network opts out with metricsPublic (serve --metrics-public /
// MNEMOS_METRICS_PUBLIC); it is deliberately never covered by the --public-reads
// anonymous-GET bypass, so a public read API can't inadvertently expose metrics.
//
// On authenticated methods:
//   - Missing or malformed Authorization header → 401
//   - Invalid signature / expired / revoked token → 401
//   - Valid token → user id from the `sub` claim lands on the request
//     context for created_by stamping.
func jwtAuthMiddleware(verifier *auth.Verifier, h http.Handler, requireTenant, publicReads, metricsPublic bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/leads" {
			h.ServeHTTP(w, r)
			return
		}
		// Bare-liveness and static infra endpoints never require a token — even
		// in multi-tenant mode. They expose no knowledge/tenant data: /health(z)
		// is a bare 200, / and /app are static HTML, and the SPA fetches its data
		// from the now-authenticated /v1/* API. (/internal/metrics is handled
		// separately below — it is NOT anonymous by default.)
		switch r.URL.Path {
		case "/health", "/healthz", "/", "/app":
			h.ServeHTTP(w, r)
			return
		}

		// Prometheus metrics: anonymous only when the operator explicitly opts in
		// with --metrics-public / MNEMOS_METRICS_PUBLIC. Otherwise it falls
		// through to the bearer check below — and is deliberately excluded from
		// the public-reads anonymous-read bypass so a public read API never also
		// exposes metrics.
		if r.URL.Path == "/internal/metrics" {
			if metricsPublic {
				h.ServeHTTP(w, r)
				return
			}
		} else if publicReads && !requireTenant && (r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions) {
			// Secure by default: reads require a token too. Only when the operator
			// explicitly opts into a public read API (--public-reads /
			// MNEMOS_PUBLIC_READS) do anonymous GET/HEAD/OPTIONS reads pass. In
			// multi-tenant mode EVERY request must present a token so its tenant
			// can be resolved — there is no anonymous tenant, so public-reads is
			// ignored.
			h.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodOptions {
			// CORS preflight carries no body/tenant; let it through even in
			// multi-tenant mode.
			h.ServeHTTP(w, r)
			return
		}

		raw := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(raw, prefix) {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		tokenStr := strings.TrimPrefix(raw, prefix)
		if tokenStr == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}

		claims, err := verifier.ParseAndValidate(r.Context(), tokenStr)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or revoked token")
			return
		}

		ctx := withActor(r.Context(), claims.UserID)
		ctx = withScopes(ctx, claims.Scopes)
		ctx = withAllowedRuns(ctx, claims.Runs)
		if requireTenant {
			// A request may select a tenant (X-Mnemos-Tenant) within the token's
			// grant (tnt or the tnts allowlist, ADR 0009); otherwise the token's
			// single tenant is used. Fail closed on an unauthorized/malformed one.
			eff, ok := claims.ResolveTenant(r.Header.Get("X-Mnemos-Tenant"))
			if !ok {
				writeError(w, http.StatusUnauthorized, "not authorized for the requested tenant (needs a matching tnt/tnts grant)")
				return
			}
			ctx = withTenant(ctx, eff)
		}
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// panicRecover is the outermost middleware: it catches any panic
// raised by a handler, logs the full stack to stderr (so operators
// can debug), and returns a sanitised 500 to the client. Go's
// net/http already recovers panics and closes the connection, but
// the default behaviour swallows the stack and writes nothing —
// operators end up debugging blind. This middleware fixes that.
//
// Must wrap every other middleware so a panic in auth or the access
// log itself still produces a clean response.
func panicRecover(logger *bolt.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			logger.Error().
				Str("method", r.Method).
				Str("path", r.URL.RequestURI()).
				Str("panic", fmt.Sprintf("%v", rec)).
				Str("stack", string(debug.Stack())).
				Msg("http_panic")
			// The response may already be partially written. Best
			// effort — http.Error will silently no-op in that case.
			writeError(w, http.StatusInternalServerError, "internal error: panic")
		}()
		h.ServeHTTP(w, r)
	})
}

// boltAccessLog returns a middleware that emits one structured access
// log per request. Uses bolt so field names match the rest of the
// codebase; `user_id` is included when authentication resolved an actor
// so we can trace writes back to the issuing identity.
//
// It installs an actorSink before delegating: auth runs BELOW this
// middleware and can only publish the token subject onto a derived context
// this frame never sees, so the sink is how the identity travels back up.
// Requests auth rejects (401/403) carry no subject and log <system> — which
// is accurate, since no identity was established.
func boltAccessLog(logger *bolt.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx, sink := withActorSink(r.Context())
		r = r.WithContext(ctx)
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rw, r)

		actor := sink.get()
		if actor == "" {
			actor = actorFromContext(r.Context())
		}
		logger.Info().
			Str("request_id", requestIDFromContext(r.Context())).
			Str("method", r.Method).
			Str("path", r.URL.RequestURI()).
			Int("status", rw.status).
			Dur("duration", time.Since(start)).
			Str("user_id", actor).
			Msg("http_request")
	})
}
