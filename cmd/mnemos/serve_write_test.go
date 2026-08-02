package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/auth"
	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/store"
	"go.klarlabs.de/mnemos/internal/store/sqlite"
)

// setupJWTTestEnv wires a per-test JWT secret so newServerMux boots a
// verifier the test can also use to mint tokens. Each test gets its own
// tmpdir-scoped secret and a fresh project root, keeping tests isolated.
func setupJWTTestEnv(t *testing.T) (secret []byte, cleanupDir string) {
	t.Helper()
	secret = make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	t.Setenv("MNEMOS_JWT_SECRET", hex.EncodeToString(secret))
	t.Setenv("HOME", t.TempDir()) // contain DefaultSecretPath if it's consulted
	return secret, t.TempDir()
}

// issueTestToken mints a JWT for a known user id using the secret that
// setupJWTTestEnv injected. Returns the Authorization header value.
func issueTestToken(t *testing.T, secret []byte, userID string) string {
	t.Helper()
	user := domain.User{
		ID:        userID,
		Name:      userID,
		Email:     userID + "@test.local",
		Status:    domain.UserStatusActive,
		CreatedAt: time.Now().UTC(),
	}
	tok, _, err := auth.NewIssuer(secret).IssueUserToken(user, time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return "Bearer " + tok
}

// seedUser persists a user so tests can verify user_id → created_by.
func seedUser(t *testing.T, conn *store.Conn, id string) {
	t.Helper()
	err := conn.Users.Create(context.Background(), domain.User{
		ID:        id,
		Name:      id,
		Email:     id + "@test.local",
		Status:    domain.UserStatusActive,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

// serveJWTBase holds the shared bits a test needs: a fresh DB with a
// per-test JWT secret wired in, and a ready Authorization header.
type serveJWTBase struct {
	DB    *sql.DB
	Conn  *store.Conn
	Srv   *httptest.Server
	Auth  map[string]string
	Actor string
}

func newServeJWTTest(t *testing.T) serveJWTBase {
	t.Helper()
	secret, _ := setupJWTTestEnv(t)
	db, conn := openTestStore(t)
	seedUser(t, conn, "usr_writer")
	srv := httptest.NewServer(newServerMux(conn))
	t.Cleanup(srv.Close)
	return serveJWTBase{
		DB:    db,
		Conn:  conn,
		Srv:   srv,
		Auth:  map[string]string{"Authorization": issueTestToken(t, secret, "usr_writer")},
		Actor: "usr_writer",
	}
}

func postJSON(t *testing.T, url string, body any, headers map[string]string) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func getJSON(t *testing.T, url string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func TestServe_AppendEvents_PersistsAndCanBeListed(t *testing.T) {
	st := newServeJWTTest(t)

	ts := time.Now().UTC().Format(time.RFC3339)
	body := map[string]any{
		"episodes": []map[string]any{
			{
				"id":              "ev_post_1",
				"run_id":          "r-post",
				"schema_version":  "v1",
				"content":         "We adopted gRPC for service-to-service calls.",
				"source_input_id": "in_post_1",
				"timestamp":       ts,
				"metadata":        map[string]string{"source": "raw_text"},
			},
		},
	}

	resp := postJSON(t, st.Srv.URL+"/v1/episodes", body, st.Auth)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		var msg errorResponse
		_ = json.NewDecoder(resp.Body).Decode(&msg)
		t.Fatalf("status = %d, want 201 (%v)", resp.StatusCode, msg.Error)
	}
	var got appendResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Accepted != 1 {
		t.Fatalf("accepted = %d, want 1", got.Accepted)
	}

	// Verify by listing.
	getResp, err := http.Get(st.Srv.URL + "/v1/episodes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = getResp.Body.Close() }()
	var list eventsResponse
	if err := json.NewDecoder(getResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Total != 1 || len(list.Events) != 1 || list.Events[0].ID != "ev_post_1" {
		t.Fatalf("listed events = %+v", list)
	}

	// And verify the JWT subject landed on created_by.
	var createdBy string
	if err := st.DB.QueryRowContext(context.Background(), `SELECT created_by FROM events WHERE id = ?`, "ev_post_1").Scan(&createdBy); err != nil {
		t.Fatalf("read created_by: %v", err)
	}
	if createdBy != st.Actor {
		t.Errorf("created_by = %q, want %q", createdBy, st.Actor)
	}
}

func TestServe_AppendEvents_RejectsEmptyArray(t *testing.T) {
	st := newServeJWTTest(t)
	resp := postJSON(t, st.Srv.URL+"/v1/episodes", map[string]any{"episodes": []any{}}, st.Auth)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestServe_AppendEvents_RejectsOversizedBatch covers the F-Slice L
// batch cap: a single POST can't include more than maxBatchRecords
// entries. The MaxBytesReader bounds total body size but doesn't
// protect against 5MB of tiny rows that balloon on decode.
func TestServe_AppendEvents_RejectsOversizedBatch(t *testing.T) {
	st := newServeJWTTest(t)
	ts := time.Now().UTC().Format(time.RFC3339)
	events := make([]map[string]any, 1001) // one over maxBatchRecords
	for i := range events {
		events[i] = map[string]any{
			"id":              "ev_batch_" + string(rune('a'+(i%26))),
			"run_id":          "r",
			"schema_version":  "v1",
			"content":         "x",
			"source_input_id": "in",
			"timestamp":       ts,
		}
	}
	resp := postJSON(t, st.Srv.URL+"/v1/episodes", map[string]any{"episodes": events}, st.Auth)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (batch cap)", resp.StatusCode)
	}
}

func TestServe_AppendEvents_RejectsBadTimestamp(t *testing.T) {
	st := newServeJWTTest(t)
	body := map[string]any{
		"episodes": []map[string]any{{"id": "ev_x", "content": "x", "timestamp": "yesterday"}},
	}
	resp := postJSON(t, st.Srv.URL+"/v1/episodes", body, st.Auth)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestServe_AppendClaims_PersistsAndCanBeListed(t *testing.T) {
	st := newServeJWTTest(t)

	now := time.Now().UTC()
	// Need an event for the evidence link FK to resolve.
	seedEventConn(t, st.Conn, "ev_for_claim", "r1", "context", "in_e", `{}`, now)

	body := map[string]any{
		"beliefs": []map[string]any{
			{
				"id":         "cl_post_1",
				"text":       "Authentication uses OAuth2.",
				"type":       "fact",
				"confidence": 0.9,
				"status":     "active",
				"created_at": now.Format(time.RFC3339),
			},
		},
		"evidence": []map[string]string{
			{"belief_id": "cl_post_1", "episode_id": "ev_for_claim"},
		},
	}
	resp := postJSON(t, st.Srv.URL+"/v1/beliefs", body, st.Auth)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		var msg errorResponse
		_ = json.NewDecoder(resp.Body).Decode(&msg)
		t.Fatalf("status = %d, want 201 (%v)", resp.StatusCode, msg.Error)
	}

	// Verify the evidence link landed.
	var n int
	if err := st.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM claim_evidence WHERE claim_id = ? AND event_id = ?`, "cl_post_1", "ev_for_claim").Scan(&n); err != nil {
		t.Fatalf("count evidence: %v", err)
	}
	if n != 1 {
		t.Fatalf("evidence rows = %d, want 1", n)
	}

	// The JWT subject should have been stamped on the claim.
	var createdBy string
	if err := st.DB.QueryRowContext(context.Background(), `SELECT created_by FROM claims WHERE id = ?`, "cl_post_1").Scan(&createdBy); err != nil {
		t.Fatalf("read created_by: %v", err)
	}
	if createdBy != st.Actor {
		t.Errorf("created_by = %q, want %q", createdBy, st.Actor)
	}
}

func TestServe_AppendClaims_RejectsInvalidType(t *testing.T) {
	st := newServeJWTTest(t)
	body := map[string]any{
		"beliefs": []map[string]any{
			{"id": "cl_x", "text": "x", "type": "guess", "confidence": 0.5, "status": "active"},
		},
	}
	resp := postJSON(t, st.Srv.URL+"/v1/beliefs", body, st.Auth)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestServe_AppendRelationships_PersistsCorrectly(t *testing.T) {
	st := newServeJWTTest(t)

	now := time.Now().UTC()
	seedClaimConn(t, st.Conn, "c-from", "a", "fact", "active", 0.8, now)
	seedClaimConn(t, st.Conn, "c-to", "b", "fact", "active", 0.8, now)

	body := map[string]any{
		"associations": []map[string]any{
			{
				"id":             "r_post_1",
				"type":           "supports",
				"from_belief_id": "c-from",
				"to_belief_id":   "c-to",
			},
		},
	}
	resp := postJSON(t, st.Srv.URL+"/v1/associations", body, st.Auth)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		var msg errorResponse
		_ = json.NewDecoder(resp.Body).Decode(&msg)
		t.Fatalf("status = %d, want 201 (%v)", resp.StatusCode, msg.Error)
	}
}

func TestServe_AppendRelationships_RejectsBadType(t *testing.T) {
	st := newServeJWTTest(t)
	body := map[string]any{
		"associations": []map[string]any{
			{"id": "r-x", "type": "neutralizes", "from_belief_id": "c1", "to_belief_id": "c2"},
		},
	}
	resp := postJSON(t, st.Srv.URL+"/v1/associations", body, st.Auth)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestServe_Auth_RejectsMissingHeader(t *testing.T) {
	st := newServeJWTTest(t)
	resp := postJSON(t, st.Srv.URL+"/v1/episodes",
		map[string]any{"episodes": []map[string]any{{"id": "e1", "content": "x"}}}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestServe_Auth_RejectsBadSignature(t *testing.T) {
	st := newServeJWTTest(t)
	// Mint a token against a different secret; verifier should reject.
	other := make([]byte, 32)
	for i := range other {
		other[i] = 0xAB
	}
	user := domain.User{ID: "usr_x", Name: "x", Email: "x@test.local", Status: domain.UserStatusActive, CreatedAt: time.Now().UTC()}
	bad, _, err := auth.NewIssuer(other).IssueUserToken(user, time.Hour)
	if err != nil {
		t.Fatalf("issue bad token: %v", err)
	}

	resp := postJSON(t, st.Srv.URL+"/v1/episodes",
		map[string]any{"episodes": []map[string]any{{"id": "e1", "content": "x"}}},
		map[string]string{"Authorization": "Bearer " + bad})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestServe_Auth_RejectsRevokedJTI(t *testing.T) {
	secret, _ := setupJWTTestEnv(t)
	_, conn := openTestStore(t)
	seedUser(t, conn, "usr_revoked")

	user := domain.User{ID: "usr_revoked", Name: "r", Email: "r@test.local", Status: domain.UserStatusActive, CreatedAt: time.Now().UTC()}
	tok, jti, err := auth.NewIssuer(secret).IssueUserToken(user, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Drop the JTI on the denylist before the mux consults it.
	if err := conn.RevokedTokens.Add(context.Background(), domain.RevokedToken{
		JTI: jti, RevokedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	srv := httptest.NewServer(newServerMux(conn))
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/v1/episodes",
		map[string]any{"episodes": []map[string]any{{"id": "e1", "content": "x"}}},
		map[string]string{"Authorization": "Bearer " + tok})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (token revoked)", resp.StatusCode)
	}
}

func TestServe_Auth_ReadsAlwaysAllowed(t *testing.T) {
	st := newServeJWTTest(t)
	// No Authorization header — reads still return 200.
	resp, err := http.Get(st.Srv.URL + "/v1/episodes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (reads open)", resp.StatusCode)
	}
}

// TestServe_Auth_AgentScopeEnforcement verifies that agents only get
// access to the resources their token's scopes name. An agent with
// just events:write can append events but is forbidden from claims,
// relationships, or embeddings — that's the whole point of governance.
func TestServe_Auth_AgentScopeEnforcement(t *testing.T) {
	secret, _ := setupJWTTestEnv(t)
	_, conn := openTestStore(t)
	srv := httptest.NewServer(newServerMux(conn))
	defer srv.Close()

	// Mint a narrow agent token (events:write only).
	tok, _, err := auth.NewIssuer(secret).IssueAgentTokenWithScopes("agt_narrow", []string{"events:write"}, time.Hour)
	if err != nil {
		t.Fatalf("issue agent token: %v", err)
	}
	hdrs := map[string]string{"Authorization": "Bearer " + tok}

	// Allowed: events.
	ts := time.Now().UTC().Format(time.RFC3339)
	body := map[string]any{
		"episodes": []map[string]any{{
			"id": "ev_agent", "run_id": "r", "schema_version": "v1",
			"content": "x", "source_input_id": "in", "timestamp": ts,
		}},
	}
	resp := postJSON(t, srv.URL+"/v1/episodes", body, hdrs)
	if resp.StatusCode != http.StatusCreated {
		var msg errorResponse
		_ = json.NewDecoder(resp.Body).Decode(&msg)
		_ = resp.Body.Close()
		t.Fatalf("events status = %d, want 201 (%v)", resp.StatusCode, msg.Error)
	}
	_ = resp.Body.Close()

	// Forbidden: claims, relationships, embeddings.
	for _, c := range []struct {
		path string
		body map[string]any
	}{
		{"/v1/beliefs", map[string]any{"beliefs": []map[string]any{{"id": "c1", "text": "x", "type": "fact", "confidence": 0.5, "status": "active"}}}},
		{"/v1/associations", map[string]any{"associations": []map[string]any{{"id": "r1", "type": "supports", "from_belief_id": "a", "to_belief_id": "b"}}}},
		{"/v1/embeddings", map[string]any{"embeddings": []map[string]any{{"entity_id": "e1", "entity_type": "event", "vector": []float32{1, 2}, "model": "m"}}}},
	} {
		r := postJSON(t, srv.URL+c.path, c.body, hdrs)
		if r.StatusCode != http.StatusForbidden {
			t.Errorf("%s status = %d, want 403", c.path, r.StatusCode)
		}
		_ = r.Body.Close()
	}
}

// TestServe_Auth_AgentRunWhitelist exercises the F.4 contract: an
// agent token whose Runs claim names "run-allowed" can POST events
// tagged with that run_id; the same agent posting run_id
// "run-forbidden" gets a 403 before any DB write happens. An agent
// with empty Runs (the legacy default) writes every run.
func TestServe_Auth_AgentRunWhitelist(t *testing.T) {
	secret, _ := setupJWTTestEnv(t)
	_, conn := openTestStore(t)
	srv := httptest.NewServer(newServerMux(conn))
	defer srv.Close()

	// Agent restricted to one run.
	scopedTok, _, err := auth.NewIssuer(secret).IssueAgentTokenWithScopesAndRuns(
		"agt_scoped", []string{"events:write"}, []string{"run-allowed"}, time.Hour,
	)
	if err != nil {
		t.Fatalf("issue scoped: %v", err)
	}
	scopedHdrs := map[string]string{"Authorization": "Bearer " + scopedTok}

	ts := time.Now().UTC().Format(time.RFC3339)
	allowedBody := map[string]any{"episodes": []map[string]any{{
		"id": "ev_ok", "run_id": "run-allowed", "schema_version": "v1",
		"content": "x", "source_input_id": "in1", "timestamp": ts,
	}}}
	r := postJSON(t, srv.URL+"/v1/episodes", allowedBody, scopedHdrs)
	if r.StatusCode != http.StatusCreated {
		var msg errorResponse
		_ = json.NewDecoder(r.Body).Decode(&msg)
		_ = r.Body.Close()
		t.Fatalf("allowed run status = %d, want 201 (%v)", r.StatusCode, msg.Error)
	}
	_ = r.Body.Close()

	forbiddenBody := map[string]any{"episodes": []map[string]any{{
		"id": "ev_no", "run_id": "run-forbidden", "schema_version": "v1",
		"content": "x", "source_input_id": "in2", "timestamp": ts,
	}}}
	r = postJSON(t, srv.URL+"/v1/episodes", forbiddenBody, scopedHdrs)
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("forbidden run status = %d, want 403", r.StatusCode)
	}
	_ = r.Body.Close()

	// Agent with empty Runs whitelist (legacy default) can write any run.
	openTok, _, err := auth.NewIssuer(secret).IssueAgentTokenWithScopes("agt_open", []string{"events:write"}, time.Hour)
	if err != nil {
		t.Fatalf("issue open: %v", err)
	}
	openHdrs := map[string]string{"Authorization": "Bearer " + openTok}

	openBody := map[string]any{"episodes": []map[string]any{{
		"id": "ev_open", "run_id": "run-anything", "schema_version": "v1",
		"content": "x", "source_input_id": "in3", "timestamp": ts,
	}}}
	r = postJSON(t, srv.URL+"/v1/episodes", openBody, openHdrs)
	if r.StatusCode != http.StatusCreated {
		var msg errorResponse
		_ = json.NewDecoder(r.Body).Decode(&msg)
		_ = r.Body.Close()
		t.Errorf("open agent status = %d, want 201 (%v)", r.StatusCode, msg.Error)
	}
	_ = r.Body.Close()
}

// TestServe_Auth_AgentRunWhitelist_BatchAllOrNothing verifies the
// pre-check semantics: a batch where one event is forbidden makes
// the whole batch fail before any row lands in the DB.
func TestServe_Auth_AgentRunWhitelist_BatchAllOrNothing(t *testing.T) {
	secret, _ := setupJWTTestEnv(t)
	db, conn := openTestStore(t)
	srv := httptest.NewServer(newServerMux(conn))
	defer srv.Close()

	tok, _, err := auth.NewIssuer(secret).IssueAgentTokenWithScopesAndRuns(
		"agt_batch", []string{"events:write"}, []string{"run-yes"}, time.Hour,
	)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	hdrs := map[string]string{"Authorization": "Bearer " + tok}

	ts := time.Now().UTC().Format(time.RFC3339)
	body := map[string]any{"episodes": []map[string]any{
		{"id": "ev_a", "run_id": "run-yes", "schema_version": "v1", "content": "x", "source_input_id": "i1", "timestamp": ts},
		{"id": "ev_b", "run_id": "run-no", "schema_version": "v1", "content": "x", "source_input_id": "i2", "timestamp": ts},
	}}
	r := postJSON(t, srv.URL+"/v1/episodes", body, hdrs)
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("mixed batch status = %d, want 403", r.StatusCode)
	}
	_ = r.Body.Close()

	// And the allowed event should NOT have landed — pre-check happens
	// before the DB write loop.
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM events WHERE id = ?`, "ev_a").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("partial write leaked through: ev_a count = %d, want 0", n)
	}
}

// TestServe_Auth_AgentRunWhitelist_BlocksClaimsWithCrossRunEvidence
// proves the F.4.b extension: a scoped agent can't sneak claims in
// by linking evidence to events from runs they don't own. The
// pre-check happens before any DB write.
func TestServe_Auth_AgentRunWhitelist_BlocksClaimsWithCrossRunEvidence(t *testing.T) {
	secret, _ := setupJWTTestEnv(t)
	db, conn := openTestStore(t)
	srv := httptest.NewServer(newServerMux(conn))
	defer srv.Close()

	// Seed an event in a run the agent shouldn't be able to touch.
	now := time.Now().UTC()
	seedEventConn(t, conn, "ev_other", "run-other", "x", "in", `{}`, now)

	tok, _, err := auth.NewIssuer(secret).IssueAgentTokenWithScopesAndRuns(
		"agt_scoped",
		[]string{"claims:write"},
		[]string{"run-mine"},
		time.Hour,
	)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	hdrs := map[string]string{"Authorization": "Bearer " + tok}

	body := map[string]any{
		"beliefs": []map[string]any{{
			"id": "cl_sneaky", "text": "x", "type": "fact",
			"confidence": 0.5, "status": "active",
			"created_at": now.Format(time.RFC3339),
		}},
		"evidence": []map[string]string{{"belief_id": "cl_sneaky", "episode_id": "ev_other"}},
	}
	r := postJSON(t, srv.URL+"/v1/beliefs", body, hdrs)
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", r.StatusCode)
	}
	_ = r.Body.Close()

	// The claim must NOT have landed.
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM claims WHERE id = ?`, "cl_sneaky").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("blocked claim leaked through: count = %d", n)
	}
}

// TestServe_Auth_AgentRunWhitelist_BlocksRelationshipWithCrossRunClaim
// covers the relationship gate: both endpoint claims must derive
// from events in allowed runs.
func TestServe_Auth_AgentRunWhitelist_BlocksRelationshipWithCrossRunClaim(t *testing.T) {
	secret, _ := setupJWTTestEnv(t)
	db, conn := openTestStore(t)
	srv := httptest.NewServer(newServerMux(conn))
	defer srv.Close()

	now := time.Now().UTC()
	// Two events, two runs.
	seedEventConn(t, conn, "ev_mine", "run-mine", "x", "i1", `{}`, now)
	seedEventConn(t, conn, "ev_other", "run-other", "x", "i2", `{}`, now)
	seedClaimConn(t, conn, "cl_mine", "x", "fact", "active", 0.8, now)
	seedClaimConn(t, conn, "cl_other", "x", "fact", "active", 0.8, now)
	if _, err := db.Exec(`INSERT INTO claim_evidence VALUES ('cl_mine','ev_mine'), ('cl_other','ev_other')`); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}

	tok, _, err := auth.NewIssuer(secret).IssueAgentTokenWithScopesAndRuns(
		"agt_scoped", []string{"relationships:write"}, []string{"run-mine"}, time.Hour,
	)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	hdrs := map[string]string{"Authorization": "Bearer " + tok}

	body := map[string]any{
		"associations": []map[string]any{{
			"id": "r_cross", "type": "supports",
			"from_belief_id": "cl_mine", "to_belief_id": "cl_other",
		}},
	}
	r := postJSON(t, srv.URL+"/v1/associations", body, hdrs)
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", r.StatusCode)
	}
	_ = r.Body.Close()
}

// TestServe_Auth_AgentRunWhitelist_BlocksEmbeddingForCrossRunEvent
// covers embeddings: an event-typed embedding for an out-of-scope
// event is forbidden.
func TestServe_Auth_AgentRunWhitelist_BlocksEmbeddingForCrossRunEvent(t *testing.T) {
	secret, _ := setupJWTTestEnv(t)
	_, conn := openTestStore(t)
	srv := httptest.NewServer(newServerMux(conn))
	defer srv.Close()

	now := time.Now().UTC()
	seedEventConn(t, conn, "ev_other", "run-other", "x", "in", `{}`, now)

	tok, _, err := auth.NewIssuer(secret).IssueAgentTokenWithScopesAndRuns(
		"agt_scoped", []string{"embeddings:write"}, []string{"run-mine"}, time.Hour,
	)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	hdrs := map[string]string{"Authorization": "Bearer " + tok}

	body := map[string]any{
		"embeddings": []map[string]any{{
			"entity_id": "ev_other", "entity_type": "event",
			"vector": []float32{0.1, 0.2}, "model": "m",
		}},
	}
	r := postJSON(t, srv.URL+"/v1/embeddings", body, hdrs)
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", r.StatusCode)
	}
	_ = r.Body.Close()
}

// TestServe_Auth_AgentRunWhitelist_AllowsClaimsInOwnRun confirms
// the gate doesn't over-block: same-run evidence still works.
func TestServe_Auth_AgentRunWhitelist_AllowsClaimsInOwnRun(t *testing.T) {
	secret, _ := setupJWTTestEnv(t)
	_, conn := openTestStore(t)
	srv := httptest.NewServer(newServerMux(conn))
	defer srv.Close()

	now := time.Now().UTC()
	seedEventConn(t, conn, "ev_mine", "run-mine", "x", "in", `{}`, now)

	tok, _, err := auth.NewIssuer(secret).IssueAgentTokenWithScopesAndRuns(
		"agt_scoped", []string{"claims:write"}, []string{"run-mine"}, time.Hour,
	)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	hdrs := map[string]string{"Authorization": "Bearer " + tok}

	body := map[string]any{
		"beliefs": []map[string]any{{
			"id": "cl_ok", "text": "x", "type": "fact",
			"confidence": 0.5, "status": "active",
			"created_at": now.Format(time.RFC3339),
		}},
		"evidence": []map[string]string{{"belief_id": "cl_ok", "episode_id": "ev_mine"}},
	}
	r := postJSON(t, srv.URL+"/v1/beliefs", body, hdrs)
	if r.StatusCode != http.StatusCreated {
		var msg errorResponse
		_ = json.NewDecoder(r.Body).Decode(&msg)
		_ = r.Body.Close()
		t.Errorf("status = %d, want 201 (%v)", r.StatusCode, msg.Error)
		return
	}
	_ = r.Body.Close()
}

// TestServe_Auth_NarrowUserScopeRejectsOtherEndpoints proves the
// F.3 contract end-to-end: a user with `events:write` only is
// allowed at /v1/episodes but forbidden everywhere else.
func TestServe_Auth_NarrowUserScopeRejectsOtherEndpoints(t *testing.T) {
	secret, _ := setupJWTTestEnv(t)
	_, conn := openTestStore(t)

	user := domain.User{
		ID: "usr_narrow", Name: "n", Email: "n@test.local",
		Status:    domain.UserStatusActive,
		Scopes:    []string{"events:write"},
		CreatedAt: time.Now().UTC(),
	}
	if err := conn.Users.Create(context.Background(), user); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	tok, _, err := auth.NewIssuer(secret).IssueUserToken(user, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	hdrs := map[string]string{"Authorization": "Bearer " + tok}

	srv := httptest.NewServer(newServerMux(conn))
	defer srv.Close()

	// Allowed: events.
	resp := postJSON(t, srv.URL+"/v1/episodes",
		map[string]any{"episodes": []map[string]any{{
			"id": "ev_n", "run_id": "r", "schema_version": "v1",
			"content": "x", "source_input_id": "in",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		}}}, hdrs)
	if resp.StatusCode != http.StatusCreated {
		var msg errorResponse
		_ = json.NewDecoder(resp.Body).Decode(&msg)
		_ = resp.Body.Close()
		t.Fatalf("events status = %d, want 201 (%v)", resp.StatusCode, msg.Error)
	}
	_ = resp.Body.Close()

	// Forbidden: other resources.
	for _, c := range []struct {
		path string
		body map[string]any
	}{
		{"/v1/beliefs", map[string]any{"beliefs": []map[string]any{{"id": "c1", "text": "x", "type": "fact", "confidence": 0.5, "status": "active"}}}},
		{"/v1/associations", map[string]any{"associations": []map[string]any{{"id": "r1", "type": "supports", "from_belief_id": "a", "to_belief_id": "b"}}}},
		{"/v1/embeddings", map[string]any{"embeddings": []map[string]any{{"entity_id": "e1", "entity_type": "event", "vector": []float32{1, 2}, "model": "m"}}}},
	} {
		r := postJSON(t, srv.URL+c.path, c.body, hdrs)
		if r.StatusCode != http.StatusForbidden {
			t.Errorf("%s status = %d, want 403", c.path, r.StatusCode)
		}
		_ = r.Body.Close()
	}
}

// TestServe_Auth_UserTokenGetsAllScopes confirms the implicit-wildcard
// behaviour: a user JWT can hit every POST endpoint without an
// explicit scope list.
func TestServe_Auth_UserTokenGetsAllScopes(t *testing.T) {
	secret, _ := setupJWTTestEnv(t)
	_, conn := openTestStore(t)
	seedUser(t, conn, "usr_full")
	srv := httptest.NewServer(newServerMux(conn))
	defer srv.Close()

	hdrs := map[string]string{"Authorization": issueTestToken(t, secret, "usr_full")}
	now := time.Now().UTC()
	seedEventConn(t, conn, "ev_for_full", "r", "x", "in", `{}`, now)
	seedClaimConn(t, conn, "cl_a", "a", "fact", "active", 0.8, now)
	seedClaimConn(t, conn, "cl_b", "b", "fact", "active", 0.8, now)

	for _, c := range []struct {
		name string
		path string
		body map[string]any
	}{
		{"episodes", "/v1/episodes", map[string]any{"episodes": []map[string]any{{"id": "ev_full_1", "content": "x", "source_input_id": "in_full_1", "timestamp": now.Format(time.RFC3339)}}}},
		{"beliefs", "/v1/beliefs", map[string]any{"beliefs": []map[string]any{{"id": "cl_full_1", "text": "x", "type": "fact", "confidence": 0.7, "status": "active", "created_at": now.Format(time.RFC3339)}}}},
		{"associations", "/v1/associations", map[string]any{"associations": []map[string]any{{"id": "r_full_1", "type": "supports", "from_belief_id": "cl_a", "to_belief_id": "cl_b"}}}},
		{"embeddings", "/v1/embeddings", map[string]any{"embeddings": []map[string]any{{"entity_id": "ev_for_full", "entity_type": "event", "vector": []float32{0.1, 0.2}, "model": "m"}}}},
	} {
		r := postJSON(t, srv.URL+c.path, c.body, hdrs)
		if r.StatusCode != http.StatusCreated {
			var msg errorResponse
			_ = json.NewDecoder(r.Body).Decode(&msg)
			_ = r.Body.Close()
			t.Errorf("%s status = %d, want 201 (%v)", c.name, r.StatusCode, msg.Error)
			continue
		}
		_ = r.Body.Close()
	}
}

// TestServe_AppendClaims_VisibilityRoundTrip verifies that an explicit
// visibility value is persisted and returned by GET /v1/beliefs.
func TestServe_AppendClaims_VisibilityRoundTrip(t *testing.T) {
	for _, vis := range []string{"personal", "team", "org"} {
		t.Run(vis, func(t *testing.T) {
			st := newServeJWTTest(t)
			now := time.Now().UTC()

			body := map[string]any{
				"beliefs": []map[string]any{
					{
						"id":         "cl_vis_" + vis,
						"text":       "visibility test " + vis,
						"type":       "fact",
						"confidence": 0.8,
						"status":     "active",
						"created_at": now.Format(time.RFC3339),
						"visibility": vis,
					},
				},
			}
			resp := postJSON(t, st.Srv.URL+"/v1/beliefs", body, st.Auth)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusCreated {
				var msg errorResponse
				_ = json.NewDecoder(resp.Body).Decode(&msg)
				t.Fatalf("POST status = %d, want 201 (%v)", resp.StatusCode, msg.Error)
			}

			// Read back and confirm visibility is present.
			getResp := getJSON(t, st.Srv.URL+"/v1/beliefs", st.Auth)
			defer func() { _ = getResp.Body.Close() }()
			if getResp.StatusCode != http.StatusOK {
				t.Fatalf("GET status = %d, want 200", getResp.StatusCode)
			}
			var result struct {
				Claims []struct {
					ID         string `json:"id"`
					Visibility string `json:"visibility"`
				} `json:"beliefs"`
			}
			if err := json.NewDecoder(getResp.Body).Decode(&result); err != nil {
				t.Fatalf("decode GET response: %v", err)
			}
			if len(result.Claims) != 1 {
				t.Fatalf("claims count = %d, want 1", len(result.Claims))
			}
			if result.Claims[0].Visibility != vis {
				t.Errorf("visibility = %q, want %q", result.Claims[0].Visibility, vis)
			}
		})
	}
}

// TestServe_AppendClaims_RejectsInvalidVisibility verifies that an
// unrecognised visibility value is rejected with 400.
func TestServe_AppendClaims_RejectsInvalidVisibility(t *testing.T) {
	st := newServeJWTTest(t)
	body := map[string]any{
		"beliefs": []map[string]any{
			{"id": "cl_vis_bad", "text": "x", "type": "fact", "confidence": 0.5, "status": "active", "visibility": "public"},
		},
	}
	resp := postJSON(t, st.Srv.URL+"/v1/beliefs", body, st.Auth)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestServe_AppendClaims_VisibilityDefaultsToTeam verifies that omitting
// visibility results in the claim PRESENTING as "team".
//
// The assertion is on what the store RETURNS, not on the raw column. Since #346
// the write path binds visibility raw and the read path normalises, so an
// omitted audience is stored as "" and presents as domain.DefaultVisibility.
// The behaviour every consumer sees is unchanged; only where the defaulting
// happens moved.
//
// That indirection is load-bearing rather than incidental: normalising on WRITE
// makes "the caller said nothing" and "the caller said team" the same string,
// and the upsert's preserve-on-zero rule has to tell them apart or a partial
// re-write silently resets a belief curated as `personal` back to `team`.
//
// Asserting through the repository is also the rule CLAUDE.md states outright —
// a test reading the column directly is what #331 and #335 both passed under.
func TestServe_AppendClaims_VisibilityDefaultsToTeam(t *testing.T) {
	st := newServeJWTTest(t)
	now := time.Now().UTC()

	body := map[string]any{
		"beliefs": []map[string]any{
			{"id": "cl_vis_nofield", "text": "no vis field", "type": "fact", "confidence": 0.7, "status": "active", "created_at": now.Format(time.RFC3339)},
		},
	}
	resp := postJSON(t, st.Srv.URL+"/v1/beliefs", body, st.Auth)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201", resp.StatusCode)
	}

	stored, err := sqlite.NewClaimRepository(st.DB).ListByIDs(context.Background(), []string{"cl_vis_nofield"})
	if err != nil {
		t.Fatalf("read claim: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("got %d claims, want 1", len(stored))
	}
	if stored[0].Visibility != domain.DefaultVisibility {
		t.Errorf("visibility = %q, want %q", stored[0].Visibility, domain.DefaultVisibility)
	}
}
