package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.klarlabs.de/bolt"
	mcp "go.klarlabs.de/mcp"
	"go.klarlabs.de/mcp/middleware"
	"go.klarlabs.de/mcp/protocol"
	"go.klarlabs.de/mnemos/internal/domain"
)

// Per-tool scope enforcement for the MCP surface.
//
// `mnemos serve` gates every REST write behind requireScope (serve_auth.go) and
// gRPC does the same in its interceptor, but the MCP transport authenticated
// requests without ever consulting the token's `scp` claim. A token minted
// read-only (`mnemos agent token issue --scope claims:read`) was refused
// POST /v1/beliefs by REST and then allowed to call process_text, remember,
// forget and every other write tool over `mcp --http --auth` — a full write
// escalation from a read-scoped credential.
//
// The guard lives in ONE place — a middleware over `tools/call` — rather than
// in each handler. Twenty hand-edited call sites is precisely how a tool ends
// up ungated, and the map below is checked against the live registration list
// by TestMCPToolScopes_CoversEveryRegisteredTool, so a new tool cannot ship
// unclassified.

// mcpToolScopes maps every registered MCP tool to the scope required to call
// it. An empty scope means the tool only reads and needs none beyond the
// authentication the transport already enforced.
//
// Scopes mirror the REST surface so the transports cannot drift: /v1/process
// requires claims:write, so process_text does too.
var mcpToolScopes = map[string]string{
	// --- reads: authentication only ---
	"query_knowledge":     "",
	"knowledge_metrics":   "",
	"list_beliefs":        "",
	"list_decisions":      "",
	"list_dissonances":    "",
	"query_schemas":       "",
	"which_test_to_trust": "",
	"query_decisions":     "",
	"query_reflex":        "",
	"memory_context":      "",
	"search_memory":       "",
	"timeline_query":      "",
	"recall_at_time":      "",
	"who_knows":           "",
	"knowledge_gaps":      "",
	"calibration":         "",
	"predictive_error":    "",
	"hypercorrections":    "",
	"recombinations":      "",
	"analogous_beliefs":   "",
	"get_belief":          "",
	"classify":            "",
	"get_decision":        "",
	"recall":              "",
	"get_blocks":          "",
	"signals":             "",

	// --- writes: ingest new events ---
	"ingest_git_log": domain.ScopeEventsWrite,
	"ingest_git_prs": domain.ScopeEventsWrite,
	"watch_file":     domain.ScopeEventsWrite,

	// --- writes: derive or mutate claims ---
	"process_text":              domain.ScopeClaimsWrite,
	"remember":                  domain.ScopeClaimsWrite,
	"remember_episode":          domain.ScopeClaimsWrite,
	"update":                    domain.ScopeClaimsWrite,
	"forget":                    domain.ScopeClaimsWrite,
	"set_block":                 domain.ScopeClaimsWrite,
	"record_action":             domain.ScopeClaimsWrite,
	"record_outcome":            domain.ScopeClaimsWrite,
	"record_expectation":        domain.ScopeClaimsWrite,
	"record_observation":        domain.ScopeClaimsWrite,
	"record_feedback":           domain.ScopeClaimsWrite,
	"record_decision":           domain.ScopeClaimsWrite,
	"synthesize_schemas":        domain.ScopeClaimsWrite,
	"synthesize_reflexes":       domain.ScopeClaimsWrite,
	"memory_deprecate":          domain.ScopeClaimsWrite,
	"memory_escalate":           domain.ScopeClaimsWrite,
	"memory_resolve_dissonance": domain.ScopeClaimsWrite,

	// memory_promote crosses the tenant→global boundary (ADR 0012), which is
	// exactly what promote:global exists to gate.
	"memory_promote": domain.ScopePromoteGlobal,

	// configure_environment writes the operator's Claude Code settings and
	// mnemos config on the SERVER's filesystem. Over HTTP that is a host-level
	// write, so it takes the broadest scope a token can carry rather than a
	// data scope.
	"configure_environment": domain.ScopeWildcard,
}

// requiredScopeForTool returns the scope a tool call needs. Unknown tools are
// treated as writes: a tool absent from the map is unclassified, and guessing
// "read" for it would reopen the exact hole this closes.
func requiredScopeForTool(name string) string {
	if scope, ok := mcpToolScopes[name]; ok {
		return scope
	}
	return domain.ScopeWildcard
}

// enforceToolScope reports an error when the request's token does not carry the
// scope its tool requires. Unauthenticated requests (stdio, or `mcp --http
// --no-auth`) carry no claims and pass through unchanged — stdio is a local
// process the user already controls, and gating it would break every existing
// local setup.
func enforceToolScope(ctx context.Context, tool string) error {
	claims, ok := claimsFromContext(ctx)
	if !ok {
		return nil
	}
	want := requiredScopeForTool(tool)
	if want == "" {
		return nil
	}
	if claims.HasScope(want) {
		return nil
	}
	return fmt.Errorf("missing required scope: %s", want)
}

// toolCallParams is the subset of a tools/call params object we need to know
// which tool is being invoked.
type toolCallParams struct {
	Name string `json:"name"`
}

// mcpMiddlewareStack builds the middleware chain both transports serve behind.
// It exists as a named function so a test can exercise the REAL stack: the
// scope guard being correct is worthless if it is never wired in, and that
// wiring is exactly the kind of thing a refactor drops silently.
func mcpMiddlewareStack(logger *bolt.Logger) []mcp.Middleware {
	return append(
		mcp.DefaultMiddlewareWithTimeout(mcpBoltLogger{logger: logger}, 30*time.Second),
		// Per-tool scope enforcement runs ahead of every handler, so an
		// authenticated-but-read-scoped token cannot reach a write tool. A
		// no-op on stdio, where there are no claims to check.
		mcpScopeMiddleware(),
	)
}

// mcpScopeMiddleware enforces per-tool scopes on every tools/call, before the
// handler runs. Other JSON-RPC methods (initialize, tools/list, …) pass
// through: they expose no data beyond the tool catalogue.
func mcpScopeMiddleware() middleware.Middleware {
	return func(next middleware.HandlerFunc) middleware.HandlerFunc {
		return func(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
			if req == nil || req.Method != protocol.MethodToolsCall {
				return next(ctx, req)
			}
			var params toolCallParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				// An unparseable tools/call cannot be authorized, so refuse it
				// rather than letting the handler decide.
				return nil, fmt.Errorf("tools/call: %w", err)
			}
			if err := enforceToolScope(ctx, params.Name); err != nil {
				return nil, fmt.Errorf("%s: %w", params.Name, err)
			}
			return next(ctx, req)
		}
	}
}
