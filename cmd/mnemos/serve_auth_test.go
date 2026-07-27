package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/bolt"
	"go.klarlabs.de/mnemos/internal/auth"
	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/store"
)

// runScopeFixture boots the REST surface in its PRODUCTION posture —
// publicReads=false — because that is the only configuration in which reads
// carry a token at all. newServerMux (the historical helper) sets
// publicReads=true, so an anonymous GET skips jwtAuthMiddleware entirely and
// never populates the run allowlist; a run-scope read test built on it would
// prove nothing.
type runScopeFixture struct {
	conn   *store.Conn
	srv    *httptest.Server
	secret []byte
}

func newRunScopeFixture(t *testing.T) runScopeFixture {
	t.Helper()
	secret, _ := setupJWTTestEnv(t)
	_, conn := openTestStore(t)
	// Pin the embedding provider so the semantic-search branch resolves
	// locally instead of reaching for whatever MNEMOS_EMBED_* the developer
	// happens to have exported.
	withStubEmbedder(t, &stubEmbedder{vector: []float32{1, 0, 0}})
	srv := httptest.NewServer(newServerMuxWithMemory(conn, nil, false /* requireTenant */, false /* publicReads */, false /* metricsPublic */))
	t.Cleanup(srv.Close)
	return runScopeFixture{conn: conn, srv: srv, secret: secret}
}

// runScopedToken mints an agent token confined to the given runs.
func (f runScopeFixture) runScopedToken(t *testing.T, scopes, runs []string) string {
	t.Helper()
	tok, _, err := auth.NewIssuer(f.secret).IssueAgentTokenWithScopesAndRuns("agt_scoped", scopes, runs, time.Hour)
	if err != nil {
		t.Fatalf("issue run-scoped token: %v", err)
	}
	return tok
}

// openToken mints an agent token with no run restriction (the legacy default).
func (f runScopeFixture) openToken(t *testing.T, scopes []string) string {
	t.Helper()
	tok, _, err := auth.NewIssuer(f.secret).IssueAgentTokenWithScopes("agt_open", scopes, time.Hour)
	if err != nil {
		t.Fatalf("issue open token: %v", err)
	}
	return tok
}

func (f runScopeFixture) do(t *testing.T, method, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (f runScopeFixture) status(t *testing.T, method, path, token string) int {
	t.Helper()
	resp := f.do(t, method, path, token)
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// seedRun writes one event plus one claim linked to it, so the run has
// something a leaking read could return.
func (f runScopeFixture) seedRun(t *testing.T, runID, eventID, claimID string) {
	t.Helper()
	now := time.Now().UTC()
	seedEventConn(t, f.conn, eventID, runID, "content of "+runID, "src-"+runID, "{}", now)
	seedClaimConn(t, f.conn, claimID, "belief in "+runID, "fact", "active", 0.9, now)
	if err := f.conn.Claims.UpsertEvidence(context.Background(), []domain.ClaimEvidence{
		{ClaimID: claimID, EventID: eventID},
	}); err != nil {
		t.Fatalf("upsert evidence: %v", err)
	}
}

// TestREST_RunScopedToken_CannotDeleteAnotherRun is the highest-severity
// guardrail: DELETE /v1/beliefs is the only destructive REST route, and it
// used to check the claims:write SCOPE only — never WHOSE runs the bearer may
// touch. A token issued for run-a could pass ?run_id=run-b and irreversibly
// cascade away another run's claims, evidence, embeddings and events.
func TestREST_RunScopedToken_CannotDeleteAnotherRun(t *testing.T) {
	f := newRunScopeFixture(t)
	f.seedRun(t, "run-a", "ev-a", "cl-a")
	f.seedRun(t, "run-b", "ev-b", "cl-b")

	tok := f.runScopedToken(t, []string{domain.ScopeClaimsWrite}, []string{"run-a"})

	if got := f.status(t, http.MethodDelete, "/v1/beliefs?run_id=run-b", tok); got != http.StatusForbidden {
		t.Errorf("DELETE another run: status = %d, want 403", got)
	}
	// The victim's data must be untouched — a 403 that still deleted would be
	// the worst possible outcome.
	victimEvents, err := f.conn.Events.ListByRunID(context.Background(), "run-b")
	if err != nil {
		t.Fatalf("list run-b events: %v", err)
	}
	if len(victimEvents) != 1 {
		t.Errorf("run-b events after refused delete = %d, want 1 (untouched)", len(victimEvents))
	}
	victimClaims, err := f.conn.Claims.ListByIDs(context.Background(), []string{"cl-b"})
	if err != nil {
		t.Fatalf("list run-b claims: %v", err)
	}
	if len(victimClaims) != 1 {
		t.Errorf("run-b claims after refused delete = %d, want 1 (untouched)", len(victimClaims))
	}

	// Deleting its OWN run still works — the fix confines the token, it does
	// not disable the endpoint.
	if got := f.status(t, http.MethodDelete, "/v1/beliefs?run_id=run-a", tok); got != http.StatusOK {
		t.Errorf("DELETE own run: status = %d, want 200", got)
	}
	ownEvents, err := f.conn.Events.ListByRunID(context.Background(), "run-a")
	if err != nil {
		t.Fatalf("list run-a events: %v", err)
	}
	if len(ownEvents) != 0 {
		t.Errorf("run-a events after own delete = %d, want 0", len(ownEvents))
	}
}

// TestREST_RunScopedToken_UnrestrictedTokenStillDeletes pins that the
// allowlist gate is a confinement, not a blanket denial: a token with no run
// claim keeps the operator/GDPR behaviour it always had.
func TestREST_RunScopedToken_UnrestrictedTokenStillDeletes(t *testing.T) {
	f := newRunScopeFixture(t)
	f.seedRun(t, "run-x", "ev-x", "cl-x")

	tok := f.openToken(t, []string{domain.ScopeClaimsWrite})
	if got := f.status(t, http.MethodDelete, "/v1/beliefs?run_id=run-x", tok); got != http.StatusOK {
		t.Fatalf("unrestricted DELETE: status = %d, want 200", got)
	}
	remaining, err := f.conn.Events.ListByRunID(context.Background(), "run-x")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("events after unrestricted delete = %d, want 0", len(remaining))
	}
}

// TestREST_RunScopedToken_ReadsAreConfined covers the write-only-allowlist
// finding: every run-carrying read on the surface either ignored the
// allowlist outright or only consulted it when the caller volunteered a
// run_id. Omitting the filter was therefore a full cross-run dump, and on the
// routes where run_id was mandatory the caller still chose its own value.
//
// Each read is probed three ways: no run id (must fail closed), a foreign run
// id (must be refused), and the bearer's own run (must pass).
func TestREST_RunScopedToken_ReadsAreConfined(t *testing.T) {
	f := newRunScopeFixture(t)
	f.seedRun(t, "run-mine", "ev-mine", "cl-mine")
	f.seedRun(t, "run-theirs", "ev-theirs", "cl-theirs")

	scoped := f.runScopedToken(t, []string{domain.ScopeClaimsWrite}, []string{"run-mine"})
	open := f.openToken(t, []string{domain.ScopeClaimsWrite})

	cases := []struct {
		name    string
		method  string
		noRun   string // path without any run id
		foreign string // path naming another bearer's run
		own     string // path naming the bearer's own run
		// ownWantOK distinguishes routes whose happy path returns 200 from
		// ones where we can only assert the request cleared the run-scope
		// gate.
		ownWantOK bool
	}{
		{
			name: "episodes", method: http.MethodGet,
			noRun:     "/v1/episodes",
			foreign:   "/v1/episodes?run_id=run-theirs",
			own:       "/v1/episodes?run_id=run-mine",
			ownWantOK: true,
		},
		{
			name: "beliefs", method: http.MethodGet,
			noRun:     "/v1/beliefs",
			foreign:   "/v1/beliefs?run_id=run-theirs",
			own:       "/v1/beliefs?run_id=run-mine",
			ownWantOK: true,
		},
		{
			name: "semantic-search", method: http.MethodGet,
			// run_id is mandatory here, so "no run id" is a 400 from the
			// route itself; the foreign case is what this route leaked.
			noRun:     "",
			foreign:   "/v1/beliefs?similar_to=hello&run_id=run-theirs",
			own:       "/v1/beliefs?similar_to=hello&run_id=run-mine",
			ownWantOK: true,
		},
		{
			name: "context", method: http.MethodGet,
			noRun:     "",
			foreign:   "/v1/context?run_id=run-theirs",
			own:       "/v1/context?run_id=run-mine",
			ownWantOK: true,
		},
		{
			name: "search", method: http.MethodGet,
			noRun:     "/v1/search?query=belief",
			foreign:   "/v1/search?query=belief&run_id=run-theirs",
			own:       "/v1/search?query=belief&run_id=run-mine",
			ownWantOK: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.noRun != "" {
				if got := f.status(t, c.method, c.noRun, scoped); got != http.StatusForbidden {
					t.Errorf("scoped read with NO run_id (%s): status = %d, want 403 (fail closed)", c.noRun, got)
				}
				// An unrestricted bearer keeps the unfiltered read.
				if got := f.status(t, c.method, c.noRun, open); got == http.StatusForbidden {
					t.Errorf("unrestricted read with no run_id (%s): status = 403, want it allowed", c.noRun)
				}
			}
			if got := f.status(t, c.method, c.foreign, scoped); got != http.StatusForbidden {
				t.Errorf("scoped read of ANOTHER run (%s): status = %d, want 403", c.foreign, got)
			}
			got := f.status(t, c.method, c.own, scoped)
			if c.ownWantOK && got != http.StatusOK {
				t.Errorf("scoped read of OWN run (%s): status = %d, want 200", c.own, got)
			}
			if !c.ownWantOK && got == http.StatusForbidden {
				t.Errorf("scoped read of OWN run (%s): status = 403, want it past the run-scope gate", c.own)
			}
		})
	}
}

// TestREST_RunScope_LeakedBodyOfForeignRunIsNotReturned proves the 403 on the
// read path is a real confinement and not a cosmetic status code: the foreign
// run's content must not appear in any response the scoped bearer can obtain.
func TestREST_RunScope_LeakedBodyOfForeignRunIsNotReturned(t *testing.T) {
	f := newRunScopeFixture(t)
	f.seedRun(t, "run-mine", "ev-mine", "cl-mine")
	f.seedRun(t, "run-theirs", "ev-theirs", "cl-theirs")

	scoped := f.runScopedToken(t, []string{domain.ScopeClaimsWrite}, []string{"run-mine"})
	// Refusal messages legitimately echo the run id the CALLER asked for, so
	// the markers here are the foreign run's stored payload — content only a
	// successful cross-run read could produce.
	for _, path := range []string{"/v1/episodes", "/v1/beliefs", "/v1/episodes?run_id=run-theirs", "/v1/beliefs?run_id=run-theirs"} {
		resp := f.do(t, http.MethodGet, path, scoped)
		body := readAllString(t, resp)
		for _, marker := range []string{"ev-theirs", "cl-theirs", "content of run-theirs", "belief in run-theirs"} {
			if strings.Contains(body, marker) {
				t.Errorf("GET %s leaked foreign-run content %q: %s", path, marker, body)
			}
		}
	}
}

// TestREST_RunScope_HonoursGlobPatterns pins the glob semantics: the inline
// REST checks used exact set membership, so a `prod-*` token — perfectly
// valid over MCP, which routes through auth.Claims.AllowsRun — was refused
// every one of its own runs. Both surfaces now share one matcher.
func TestREST_RunScope_HonoursGlobPatterns(t *testing.T) {
	f := newRunScopeFixture(t)
	f.seedRun(t, "prod-2026", "ev-prod", "cl-prod")
	f.seedRun(t, "dev-2026", "ev-dev", "cl-dev")

	tok := f.runScopedToken(t, []string{domain.ScopeEventsWrite}, []string{"prod-*"})
	if got := f.status(t, http.MethodGet, "/v1/episodes?run_id=prod-2026", tok); got != http.StatusOK {
		t.Errorf("glob-matching run: status = %d, want 200", got)
	}
	if got := f.status(t, http.MethodGet, "/v1/episodes?run_id=dev-2026", tok); got != http.StatusForbidden {
		t.Errorf("non-matching run: status = %d, want 403", got)
	}

	// Same matcher on the write path, which previously did its own exact
	// compare and would have rejected prod-2026.
	body := map[string]any{"episodes": []map[string]any{{
		"id": "ev_glob", "run_id": "prod-2026", "schema_version": "v1",
		"content": "x", "source_input_id": "in-glob",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}}}
	resp := postJSON(t, f.srv.URL+"/v1/episodes", body, map[string]string{"Authorization": "Bearer " + tok})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("POST with glob-matching run: status = %d, want 201", resp.StatusCode)
	}
}

// TestREST_Incidents_RequireScopeAndIgnoreBodyAuthor covers the two incident
// findings together: POST /v1/incidents and POST /v1/incidents/<id>/resolve
// were the only REST POSTs with no scope gate (any valid token, including a
// read-only one, could file and close incidents), and openIncident stamped
// CreatedBy straight from the request body — forgeable attribution on the one
// record type whose whole purpose is post-hoc accountability.
func TestREST_Incidents_RequireScopeAndIgnoreBodyAuthor(t *testing.T) {
	f := newRunScopeFixture(t)

	// A token with no write scope at all.
	readOnly := f.openToken(t, nil)
	writer, _, err := auth.NewIssuer(f.secret).IssueUserToken(domain.User{
		ID:     "usr_oncall",
		Scopes: []string{domain.ScopeClaimsWrite},
		Status: domain.UserStatusActive,
	}, time.Hour)
	if err != nil {
		t.Fatalf("issue writer token: %v", err)
	}

	body := map[string]any{
		"id":         "inc_authz",
		"title":      "db saturated",
		"severity":   "high",
		"created_by": "usr_someone_else", // the forgery attempt
	}

	resp := postJSON(t, f.srv.URL+"/v1/incidents", body, map[string]string{"Authorization": "Bearer " + readOnly})
	got := resp.StatusCode
	_ = resp.Body.Close()
	if got != http.StatusForbidden {
		t.Errorf("open incident without claims:write: status = %d, want 403", got)
	}

	resp = postJSON(t, f.srv.URL+"/v1/incidents", body, map[string]string{"Authorization": "Bearer " + writer})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open incident with claims:write: status = %d, want 201", resp.StatusCode)
	}
	var inc domain.Incident
	if err := json.NewDecoder(resp.Body).Decode(&inc); err != nil {
		t.Fatalf("decode incident: %v", err)
	}
	if inc.CreatedBy != "usr_oncall" {
		t.Errorf("incident CreatedBy = %q, want the token subject %q (body-supplied author must be ignored)", inc.CreatedBy, "usr_oncall")
	}

	// resolve is a governed state change and needs the same gate.
	resolveBody := map[string]any{}
	resp = postJSON(t, f.srv.URL+"/v1/incidents/inc_authz/resolve", resolveBody, map[string]string{"Authorization": "Bearer " + readOnly})
	got = resp.StatusCode
	_ = resp.Body.Close()
	if got != http.StatusForbidden {
		t.Errorf("resolve incident without claims:write: status = %d, want 403", got)
	}
	resp = postJSON(t, f.srv.URL+"/v1/incidents/inc_authz/resolve", resolveBody, map[string]string{"Authorization": "Bearer " + writer})
	got = resp.StatusCode
	_ = resp.Body.Close()
	if got != http.StatusOK {
		t.Errorf("resolve incident with claims:write: status = %d, want 200", got)
	}
}

// TestBoltAccessLog_RecordsAuthenticatedActor pins the audit-trail fix. The
// log sits ABOVE jwtAuthMiddleware, which installs the actor on a derived
// request that only flows downward — so every line, including authenticated
// writes, read user_id=<system> and the log could not do the one job its
// doc comment claims (tracing a write back to the issuing identity).
func TestBoltAccessLog_RecordsAuthenticatedActor(t *testing.T) {
	tok, verifier := readAuthFixture(t)

	var buf bytes.Buffer
	logger := bolt.New(bolt.NewJSONHandler(&buf))
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The handler itself must still see the actor on its own context.
		if got := actorFromContext(r.Context()); got != "u" {
			t.Errorf("handler actorFromContext = %q, want %q", got, "u")
		}
		w.WriteHeader(http.StatusOK)
	})
	h := boltAccessLog(logger, jwtAuthMiddleware(verifier, inner, false, false, false))

	req := httptest.NewRequest(http.MethodGet, "/v1/beliefs", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(httptest.NewRecorder(), req)

	line := buf.String()
	if !strings.Contains(line, `"user_id":"u"`) {
		t.Errorf("access log did not carry the authenticated actor: %s", line)
	}
	if strings.Contains(line, `"user_id":"`+domain.SystemUser+`"`) {
		t.Errorf("access log still attributes an authenticated request to %s: %s", domain.SystemUser, line)
	}

	// An unauthenticated rejection is still logged — moving the log below auth
	// would have dropped exactly the requests an audit trail needs most.
	buf.Reset()
	anon := httptest.NewRequest(http.MethodGet, "/v1/beliefs", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, anon)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("anon status = %d, want 401", rr.Code)
	}
	if !strings.Contains(buf.String(), `"status":401`) {
		t.Errorf("rejected request was not access-logged: %s", buf.String())
	}
}

func readAllString(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}
