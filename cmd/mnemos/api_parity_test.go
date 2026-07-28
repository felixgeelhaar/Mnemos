package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestAPISurfaceParity is a fitness function that fails when one of the
// three transport surfaces (MCP / HTTP / gRPC) gains a name that isn't
// recorded in the parity map below.
//
// Why this exists. Mnemos exposes the same capabilities through three
// transports — the MCP stdio server (cmd/mnemos/mcp.go), the HTTP API
// (cmd/mnemos/serve.go), and the gRPC service (internal/server/grpc/).
// They were written independently and have drifted over time:
// `which_test_to_trust` for example exists only in MCP. Drift is fine
// **when intentional**; what's not fine is silent drift.
//
// The parityMatrix below records every (mcp_tool, http_route,
// grpc_method) triple that the project considers parity-aware. When a
// new tool/route/method ships, this test fails until it's added to
// the matrix. Adding it forces the author to think: should the other
// two surfaces also expose this? If yes, ship them now. If no, mark
// the cell `parityNA` to record the deliberate asymmetry.
//
// This is the cheapest fitness function for surface drift — no app
// layer refactor required, no IDL — and it catches the failure mode
// that motivated the task: "we forgot to add this to gRPC".
//
// WHAT IT DOES NOT PROTECT. Read this before trusting it.
//
// It compares NAMES. It cannot see that two surfaces exposing the same name
// behave differently, and that is where the real drift has happened:
//
//   - /v1/search and query_knowledge were both registered, both listed here,
//     and built query engines with different capabilities — one ranked by
//     embeddings and full text, the other by token overlap. Same name, same
//     matrix row, different product.
//   - memory_context lacked the `query` input that /v1/context accepted.
//   - AnswerOptions.Consumer was reachable from one surface only.
//
// It also scans exactly three surfaces. The EMBEDDED LIBRARY (the root
// `mnemos` package, memory.go / memory_impl.go) is not one of them — and that
// is the surface every consuming product actually uses. Two of the three
// defects above were worst there.
//
// Adding a fourth column was considered and rejected: the Memory interface has
// ~42 methods against ~123 matrix entries here, most with no transport
// analogue, so it would be mostly parityNA — a matrix nobody maintains, which
// is worse than a boundary stated plainly.
//
// Behavioural parity needs construction-site fitness functions instead, of the
// shape in internal/query/cognitive_wiring_test.go: assert at the point where
// the options/engine are BUILT, not at the point where a name is registered.
func TestAPISurfaceParity(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}

	mcpTools := mustReadMCPTools(t, filepath.Join(root, "cmd/mnemos/mcp.go"))
	httpRoutes := mustReadHTTPRoutes(t, filepath.Join(root, "cmd/mnemos/serve.go"))
	grpcMethods := mustReadGRPCMethods(t, filepath.Join(root, "internal/server/grpc"))

	tracked := map[string]bool{}
	for _, e := range parityMatrix {
		if e.MCPTool != "" && e.MCPTool != parityNA {
			tracked["mcp:"+e.MCPTool] = true
		}
		if e.HTTPRoute != "" && e.HTTPRoute != parityNA {
			tracked["http:"+e.HTTPRoute] = true
		}
		if e.GRPCMethod != "" && e.GRPCMethod != parityNA {
			tracked["grpc:"+e.GRPCMethod] = true
		}
	}

	var missing []string
	for _, t := range mcpTools {
		if !tracked["mcp:"+t] {
			missing = append(missing, "mcp:"+t)
		}
	}
	for _, r := range httpRoutes {
		if !tracked["http:"+r] {
			missing = append(missing, "http:"+r)
		}
	}
	for _, m := range grpcMethods {
		if !tracked["grpc:"+m] {
			missing = append(missing, "grpc:"+m)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("API surface drift detected — these names are not declared in parityMatrix:\n  %s\n\n"+
			"Add a row to parityMatrix in api_parity_test.go for each. If a counterpart on another transport "+
			"is intentionally missing, set that field to parityNA so the deliberate asymmetry is recorded.",
			strings.Join(missing, "\n  "))
	}
}

// parityNA marks a deliberate asymmetry: the capability exists on at
// least one surface but is intentionally NOT exposed on this one.
const parityNA = "n/a"

// parityEntry records one capability across the three transport
// surfaces. Empty MCP/HTTP/gRPC fields mean "this capability is not
// exposed there"; parityNA means "intentionally not exposed there
// (recorded so the test doesn't flag it as drift)".
type parityEntry struct {
	Capability string // human-readable label, e.g. "list claims"
	MCPTool    string // srv.Tool("...") name in cmd/mnemos/mcp.go
	HTTPRoute  string // mux.HandleFunc("...") path in cmd/mnemos/serve.go
	GRPCMethod string // (*Server).<Method> in internal/server/grpc/
}

// parityMatrix is the ground-truth declaration of API surface coverage.
// Updates accompany every transport-surface change. Each row should be
// reviewable in isolation: if the asymmetry isn't obvious from the
// label, leave a one-line comment.
var parityMatrix = []parityEntry{
	// Health & landing — public surfaces.
	{Capability: "health probe (bare liveness)", MCPTool: parityNA, HTTPRoute: "/health", GRPCMethod: "Health"},
	{Capability: "health probe alias", MCPTool: parityNA, HTTPRoute: "/healthz", GRPCMethod: parityNA},
	{Capability: "readiness probe (authed, DB write check)", MCPTool: parityNA, HTTPRoute: "/internal/ready", GRPCMethod: parityNA},
	{Capability: "marketing landing (HTML)", MCPTool: parityNA, HTTPRoute: "/", GRPCMethod: parityNA},
	{Capability: "registry SPA (HTML)", MCPTool: parityNA, HTTPRoute: "/app", GRPCMethod: parityNA},
	{Capability: "lead capture form (public)", MCPTool: parityNA, HTTPRoute: "/v1/leads", GRPCMethod: parityNA},

	// Core knowledge surfaces.
	{Capability: "list/append events", MCPTool: parityNA, HTTPRoute: "/v1/episodes", GRPCMethod: "ListEpisodes"},
	{Capability: "append events (gRPC verb)", MCPTool: parityNA, HTTPRoute: parityNA, GRPCMethod: "AppendEpisodes"},
	{Capability: "list/append/delete claims", MCPTool: "list_beliefs", HTTPRoute: "/v1/beliefs", GRPCMethod: "ListBeliefs"},
	{Capability: "append claims (gRPC verb)", MCPTool: parityNA, HTTPRoute: parityNA, GRPCMethod: "AppendBeliefs"},
	{Capability: "claim subresources (provenance/export)", MCPTool: parityNA, HTTPRoute: "/v1/beliefs/", GRPCMethod: parityNA},
	{Capability: "list/append relationships", MCPTool: parityNA, HTTPRoute: "/v1/associations", GRPCMethod: "ListAssociations"},
	{Capability: "append relationships (gRPC verb)", MCPTool: parityNA, HTTPRoute: parityNA, GRPCMethod: "AppendAssociations"},
	{Capability: "list/append embeddings", MCPTool: parityNA, HTTPRoute: "/v1/embeddings", GRPCMethod: "ListEmbeddings"},
	{Capability: "append embeddings (gRPC verb)", MCPTool: parityNA, HTTPRoute: parityNA, GRPCMethod: "AppendEmbeddings"},

	// Metrics, search, context — read-only.
	{Capability: "knowledge metrics", MCPTool: "knowledge_metrics", HTTPRoute: "/v1/metrics", GRPCMethod: "Metrics"},
	{Capability: "promoted schemas (neocortex read)", MCPTool: parityNA, HTTPRoute: "/v1/schemas", GRPCMethod: parityNA},
	{Capability: "Prometheus RED metrics (operators)", MCPTool: parityNA, HTTPRoute: "/internal/metrics", GRPCMethod: parityNA},
	{Capability: "context block (chat-agent prompt slice)", MCPTool: parityNA, HTTPRoute: "/v1/context", GRPCMethod: parityNA},
	{Capability: "hybrid search", MCPTool: parityNA, HTTPRoute: "/v1/search", GRPCMethod: parityNA},
	{Capability: "incidents", MCPTool: parityNA, HTTPRoute: "/v1/incidents", GRPCMethod: parityNA},
	{Capability: "incident subresources", MCPTool: parityNA, HTTPRoute: "/v1/incidents/", GRPCMethod: parityNA},

	// Local host setup: detect the machine and install hooks/config. MCP-only
	// by design — it mutates the caller's Claude Code settings, which is
	// meaningless over a remote HTTP/gRPC transport.
	{Capability: "configure Claude Code integration", MCPTool: "configure_environment", HTTPRoute: parityNA, GRPCMethod: parityNA},

	// Connected-brain reads (v0.66 HTTP, gRPC parity added). MCP counterparts
	// not exposed; the library is the in-process surface.
	{Capability: "who-knows-what directory", MCPTool: "who_knows", HTTPRoute: "/v1/who-knows", GRPCMethod: "WhoKnows"},
	{Capability: "knowledge gaps", MCPTool: "knowledge_gaps", HTTPRoute: "/v1/knowledge-gaps", GRPCMethod: "KnowledgeGaps"},
	{Capability: "confidence calibration", MCPTool: "calibration", HTTPRoute: "/v1/calibration", GRPCMethod: "Calibration"},
	{Capability: "hypercorrections (contested established beliefs)", MCPTool: "hypercorrections", HTTPRoute: "/v1/hypercorrections", GRPCMethod: "Hypercorrections"},
	// Predictive-error surface (ADR 0017) is exposed MCP-only for agent active inference;
	// no REST/gRPC surface (deliberate asymmetry, like list_dissonances below).
	{Capability: "predictive error (hierarchical prediction error, ADR 0017)", MCPTool: "predictive_error", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "recombinations (candidate novel connections)", MCPTool: "recombinations", HTTPRoute: "/v1/recombinations", GRPCMethod: "Recombinations"},
	{Capability: "analogical retrieval", MCPTool: "analogous_beliefs", HTTPRoute: parityNA, GRPCMethod: "AnalogousBeliefs"},

	// Claim CRUD parity (v0.67 HTTP, gRPC parity added).
	{Capability: "classify claim novelty", MCPTool: "classify", HTTPRoute: "/v1/classify", GRPCMethod: "Classify"},
	{Capability: "get single claim", MCPTool: "get_belief", HTTPRoute: parityNA, GRPCMethod: "GetBelief"},
	{Capability: "set claim lifecycle", MCPTool: parityNA, HTTPRoute: parityNA, GRPCMethod: "SetBeliefLifecycle"},
	{Capability: "get single decision", MCPTool: "get_decision", HTTPRoute: parityNA, GRPCMethod: "GetDecision"},
	{Capability: "advanced recall (sufficiency/effort/context/conflicts/iterative)", MCPTool: "recall", HTTPRoute: "/v1/recall", GRPCMethod: "Recall"},

	// Working memory + skill loop + temporal (v0.69).
	{Capability: "working-memory blocks (read)", MCPTool: "get_blocks", HTTPRoute: "/v1/blocks", GRPCMethod: "GetBlocks"},
	{Capability: "working-memory blocks (write)", MCPTool: "set_block", HTTPRoute: parityNA, GRPCMethod: "SetBlock"},
	{Capability: "record action (skill loop)", MCPTool: parityNA, HTTPRoute: "/v1/actions", GRPCMethod: parityNA},
	{Capability: "action outcome (skill loop)", MCPTool: parityNA, HTTPRoute: "/v1/actions/", GRPCMethod: parityNA},
	{Capability: "synthesize lessons/playbooks", MCPTool: parityNA, HTTPRoute: "/v1/synthesize", GRPCMethod: "Synthesize"},
	{Capability: "temporal timeline", MCPTool: parityNA, HTTPRoute: "/v1/timeline", GRPCMethod: "Timeline"},
	{Capability: "temporal signals", MCPTool: "signals", HTTPRoute: "/v1/signals", GRPCMethod: "Signals"},
	{Capability: "decision subresources", MCPTool: parityNA, HTTPRoute: "/v1/decisions/", GRPCMethod: parityNA},

	// Browse helpers (MCP-only).
	{Capability: "list decisions (browse helper)", MCPTool: "list_decisions", HTTPRoute: "/v1/decisions", GRPCMethod: parityNA},
	{Capability: "list contradictions (browse helper)", MCPTool: "list_dissonances", HTTPRoute: parityNA, GRPCMethod: parityNA},

	// Pipeline (MCP-only — agent action surface).
	{Capability: "process text (ingest+extract+relate)", MCPTool: "process_text", HTTPRoute: "/v1/process", GRPCMethod: parityNA},
	{Capability: "query knowledge", MCPTool: "query_knowledge", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "ingest git PRs", MCPTool: "ingest_git_prs", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "ingest git log", MCPTool: "ingest_git_log", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "watch file", MCPTool: "watch_file", HTTPRoute: parityNA, GRPCMethod: parityNA},

	// Temporal (MCP-only — bundled-Chronos surface).
	{Capability: "remember temporal event", MCPTool: "remember_episode", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "query timeline (range/type/run filter)", MCPTool: "timeline_query", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "recall knowledge at a historical instant", MCPTool: "recall_at_time", HTTPRoute: parityNA, GRPCMethod: parityNA},

	// Phase 2 (action / outcome) — gRPC + MCP, no HTTP.
	{Capability: "record action", MCPTool: "record_action", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "record outcome", MCPTool: "record_outcome", HTTPRoute: parityNA, GRPCMethod: parityNA},
	// These three existed on HTTP only until the MCP counterparts were added.
	// The asymmetry was not deliberate: it is why claim_expectations and
	// claim_feedback sat empty on a brain with 86k claims, since the surface
	// that does the recalling had no way to record a prediction or say a
	// recalled belief was unhelpful.
	{Capability: "record expectation", MCPTool: "record_expectation", HTTPRoute: "POST /v1/beliefs/{id}/expectation", GRPCMethod: parityNA},
	{Capability: "record observation", MCPTool: "record_observation", HTTPRoute: "POST /v1/beliefs/{id}/observation", GRPCMethod: parityNA},
	{Capability: "record feedback", MCPTool: "record_feedback", HTTPRoute: "POST /v1/beliefs/{id}/feedback", GRPCMethod: parityNA},
	{Capability: "list actions (gRPC)", MCPTool: parityNA, HTTPRoute: parityNA, GRPCMethod: "ListActions"},
	{Capability: "append actions (gRPC)", MCPTool: parityNA, HTTPRoute: parityNA, GRPCMethod: "AppendActions"},
	{Capability: "list outcomes (gRPC)", MCPTool: parityNA, HTTPRoute: parityNA, GRPCMethod: "ListOutcomes"},
	{Capability: "append outcomes (gRPC)", MCPTool: parityNA, HTTPRoute: parityNA, GRPCMethod: "AppendOutcomes"},

	// Phase 3 — Lessons.
	{Capability: "synthesize lessons", MCPTool: "synthesize_schemas", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "query lessons", MCPTool: "query_schemas", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "list lessons (gRPC)", MCPTool: parityNA, HTTPRoute: parityNA, GRPCMethod: "ListSchemas"},
	{Capability: "append lessons (gRPC)", MCPTool: parityNA, HTTPRoute: parityNA, GRPCMethod: "AppendSchemas"},

	// Phase 5 — Decisions.
	{Capability: "record decision", MCPTool: "record_decision", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "query decisions", MCPTool: "query_decisions", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "list decisions (gRPC)", MCPTool: parityNA, HTTPRoute: parityNA, GRPCMethod: "ListDecisions"},
	{Capability: "append decisions (gRPC)", MCPTool: parityNA, HTTPRoute: parityNA, GRPCMethod: "AppendDecisions"},

	// Phase 6 — Playbooks.
	{Capability: "query playbook", MCPTool: "query_reflex", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "synthesize playbooks", MCPTool: "synthesize_reflexes", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "list playbooks (gRPC)", MCPTool: parityNA, HTTPRoute: parityNA, GRPCMethod: "ListReflexes"},
	{Capability: "append playbooks (gRPC)", MCPTool: parityNA, HTTPRoute: parityNA, GRPCMethod: "AppendReflexes"},

	// Memory governance (MCP-only — agent governance surface).
	{Capability: "memory deprecate", MCPTool: "memory_deprecate", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "memory resolve contradiction", MCPTool: "memory_resolve_dissonance", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "memory escalate", MCPTool: "memory_escalate", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "memory promote", MCPTool: "memory_promote", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "memory context (agent recall)", MCPTool: "memory_context", HTTPRoute: parityNA, GRPCMethod: parityNA},

	// Agent memory-management primitives (MCP-only — agent self-edit surface). See #41.
	{Capability: "remember (store a fact as a claim)", MCPTool: "remember", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "forget (soft-delete a claim)", MCPTool: "forget", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "update (rewrite a claim's text)", MCPTool: "update", HTTPRoute: parityNA, GRPCMethod: parityNA},
	{Capability: "search_memory (semantic claim recall)", MCPTool: "search_memory", HTTPRoute: "/v1/beliefs", GRPCMethod: parityNA},

	// Entity relationships (gRPC-only).
	{Capability: "list entity relationships (gRPC)", MCPTool: parityNA, HTTPRoute: parityNA, GRPCMethod: "ListEntityAssociations"},
	{Capability: "append entity relationships (gRPC)", MCPTool: parityNA, HTTPRoute: parityNA, GRPCMethod: "AppendEntityAssociations"},

	// Trust / epistemic provenance.
	{Capability: "which test to trust", MCPTool: "which_test_to_trust", HTTPRoute: parityNA, GRPCMethod: parityNA},

	// Federation export (HTTP-only, opt-in). See #45.
	{Capability: "federation export (anonymized playbooks)", MCPTool: parityNA, HTTPRoute: "/v1/federation/export", GRPCMethod: parityNA},
}

// repoRoot walks up from the test's working directory looking for go.mod.
// The cmd/mnemos test cwd is cmd/mnemos, so repoRoot returns the
// project root regardless of where the test is launched.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

var (
	mcpToolPattern    = regexp.MustCompile(`(?m)srv\.Tool\("([a-z_]+)"\)`)
	httpRoutePattern  = regexp.MustCompile(`(?m)mux\.Handle(?:Func)?\("(/[^"]*)"`)
	grpcMethodPattern = regexp.MustCompile(`(?m)^func \(s \*Server\) ([A-Z][A-Za-z]+)\(ctx context\.Context`)
)

func mustReadMCPTools(t *testing.T, path string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read mcp.go: %v", err)
	}
	out := uniqueMatches(mcpToolPattern.FindAllSubmatch(src, -1))
	sort.Strings(out)
	return out
}

func mustReadHTTPRoutes(t *testing.T, path string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read serve.go: %v", err)
	}
	out := uniqueMatches(httpRoutePattern.FindAllSubmatch(src, -1))
	sort.Strings(out)
	return out
}

func mustReadGRPCMethods(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read grpc dir: %v", err)
	}
	seen := map[string]struct{}{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range grpcMethodPattern.FindAllSubmatch(src, -1) {
			seen[string(m[1])] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func uniqueMatches(matches [][][]byte) []string {
	seen := map[string]struct{}{}
	for _, m := range matches {
		seen[string(m[1])] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}
