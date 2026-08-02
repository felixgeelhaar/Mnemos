package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.klarlabs.de/bolt"
	mcp "go.klarlabs.de/mcp"
	mnemos "go.klarlabs.de/mnemos"
	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/embedding"
	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/ingest"
	"go.klarlabs.de/mnemos/internal/llm"
	"go.klarlabs.de/mnemos/internal/parser"
	"go.klarlabs.de/mnemos/internal/pipeline"
	"go.klarlabs.de/mnemos/internal/ports"
	"go.klarlabs.de/mnemos/internal/query"
	"go.klarlabs.de/mnemos/internal/relate"
	"go.klarlabs.de/mnemos/internal/store"
	"go.klarlabs.de/mnemos/internal/synthesize"
	"go.klarlabs.de/mnemos/internal/trust"
	"go.klarlabs.de/mnemos/internal/workflow"
)

type mcpBoltLogger struct {
	logger *bolt.Logger
}

func (l mcpBoltLogger) Info(msg string, fields ...mcp.LogField) {
	l.log(l.logger.Info(), msg, fields...)
}
func (l mcpBoltLogger) Error(msg string, fields ...mcp.LogField) {
	l.log(l.logger.Error(), msg, fields...)
}
func (l mcpBoltLogger) Debug(msg string, fields ...mcp.LogField) {
	l.log(l.logger.Debug(), msg, fields...)
}
func (l mcpBoltLogger) Warn(msg string, fields ...mcp.LogField) {
	l.log(l.logger.Warn(), msg, fields...)
}

func (l mcpBoltLogger) log(event *bolt.Event, msg string, fields ...mcp.LogField) {
	for _, field := range fields {
		event = event.Any(field.Key, field.Value)
	}
	event.Msg(msg)
}

type mcpQueryInput struct {
	Question string `json:"question" jsonschema:"required,description=Natural language question to ask Mnemos"`
	RunID    string `json:"runId,omitempty" jsonschema:"description=Optional run ID to scope the query"`
	Hops     int    `json:"hops,omitempty" jsonschema:"description=BFS hop expansion depth through supports/contradicts edges (0-5, default 0)"`
	Scope    string `json:"context,omitempty" jsonschema:"description=Which brain(s) to search: 'both' (default; global + this repo's brain if one exists), 'global', or 'repo'. Repo claims win on conflict and are tagged claim_provenance=repo."`
}

type mcpQueryOutput struct {
	Answer         string                `json:"answer"`
	Claims         []domain.Claim        `json:"beliefs"`
	Contradictions []domain.Relationship `json:"dissonances"`
	Timeline       []string              `json:"timeline"`
	// ClaimProvenance maps claim ID to "local" or a registry URL so the
	// agent can show which claims came from a federated registry.
	ClaimProvenance map[string]string `json:"belief_provenance,omitempty"`
	// ClaimHopDistance maps claim ID to the BFS hop count from the
	// directly-retrieved set (0 = direct, N>0 = expanded via N hops of
	// supports/contradicts). Empty when hops=0.
	ClaimHopDistance map[string]int `json:"belief_hop_distance,omitempty"`
}

type mcpProcessTextInput struct {
	Text          string `json:"text" jsonschema:"required,description=Raw text to ingest and process"`
	UseLLM        bool   `json:"useLlm,omitempty" jsonschema:"description=Use configured LLM extraction provider"`
	UseEmbeddings bool   `json:"useEmbeddings,omitempty" jsonschema:"description=Generate embeddings after processing"`
	// SessionID tags the resulting events with the agent session that produced
	// them, so contradiction detection can tell a conversation arguing with
	// itself from two sessions genuinely disagreeing. Optional; empty means
	// the ingestion is not session-scoped and nothing is suppressed.
	SessionID string `json:"sessionId,omitempty" jsonschema:"description=Agent session id; suppresses contradictions between claims from the same session"`
}

type mcpProcessTextOutput struct {
	RunID          string `json:"runId"`
	Events         int    `json:"episodes"`
	Claims         int    `json:"beliefs"`
	Relationships  int    `json:"associations"`
	Embeddings     int    `json:"embeddings"`
	UsedLLM        bool   `json:"usedLlm"`
	UsedEmbeddings bool   `json:"usedEmbeddings"`
	// LLM token usage from the underlying extraction call. Zero when
	// the rule-based engine ran or when the provider reported no
	// usage. Sums to TokensUsed on the axi-go capability-evidence
	// record so the kernel's MaxTokens budget can enforce.
	InputTokens  int    `json:"inputTokens,omitempty"`
	OutputTokens int    `json:"outputTokens,omitempty"`
	LLMModel     string `json:"llmModel,omitempty"`
}

type mcpMetricsOutput struct {
	Runs            int64 `json:"runs"`
	Events          int64 `json:"episodes"`
	Claims          int64 `json:"beliefs"`
	ContestedClaims int64 `json:"contested_beliefs"`
	Relationships   int64 `json:"associations"`
	Contradictions  int64 `json:"dissonances"`
	Embeddings      int64 `json:"embeddings"`
}

type mcpIngestGitLogInput struct {
	Limit int    `json:"limit,omitempty" jsonschema:"description=Max number of commits to ingest (default 50, cap 1000)"`
	Since string `json:"since,omitempty" jsonschema:"description=Optional date string passed to git --since (e.g. '2026-01-01' or '2 weeks ago')"`
}

type mcpIngestGitLogOutput struct {
	Ingested int `json:"ingested"`
	Skipped  int `json:"skipped"`
}

type mcpIngestGitPRsInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"description=Max number of merged PRs to ingest (default 20, cap 200)"`
}

type mcpIngestGitPRsOutput struct {
	Ingested int `json:"ingested"`
	Skipped  int `json:"skipped"`
}

type mcpWatchFileInput struct {
	Path string `json:"path" jsonschema:"required,description=Absolute or relative path to the file to watch for changes"`
}

type mcpWatchFileOutput struct {
	Watching      bool   `json:"watching"`
	Path          string `json:"path"`
	ActiveWatches int    `json:"activeWatches"`
}

type mcpRecordActionInput struct {
	Kind     string            `json:"kind" jsonschema:"required,description=Action kind (deploy, rollback, restart, scale, configure, migrate, feature_flag, hotfix, custom)"`
	Subject  string            `json:"subject" jsonschema:"required,description=Service or component the action targets"`
	Actor    string            `json:"actor,omitempty" jsonschema:"description=User or agent id that performed the action"`
	RunID    string            `json:"runId,omitempty" jsonschema:"description=Optional run id for scoped storage"`
	At       string            `json:"at,omitempty" jsonschema:"description=When the action happened (RFC3339 / YYYY-MM-DD / 'now', defaults to now)"`
	Metadata map[string]string `json:"metadata,omitempty" jsonschema:"description=Free-form string metadata for the action"`
}

type mcpRecordActionOutput struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
}

type mcpRecordOutcomeInput struct {
	ActionID   string             `json:"actionId" jsonschema:"required,description=Id of the action this outcome reports on"`
	Result     string             `json:"result" jsonschema:"required,description=success | failure | partial | unknown"`
	Metrics    map[string]float64 `json:"metrics,omitempty" jsonschema:"description=Numeric metric observations (e.g. latency_after_ms, error_rate)"`
	Notes      string             `json:"notes,omitempty" jsonschema:"description=Free-form human notes about the outcome"`
	ObservedAt string             `json:"observedAt,omitempty" jsonschema:"description=When the outcome was observed (RFC3339, defaults to now)"`
	Source     string             `json:"source,omitempty" jsonschema:"description=Source of the report (default 'push'; pull adapters use 'pull:<name>')"`
}

type mcpRecordOutcomeOutput struct {
	ID       string `json:"id"`
	ActionID string `json:"action_id"`
	Result   string `json:"result"`
}

type mcpQueryLessonsInput struct {
	Service string `json:"service,omitempty" jsonschema:"description=Filter to lessons scoped to this service"`
	Trigger string `json:"trigger,omitempty" jsonschema:"description=Filter to lessons matching this trigger label"`
}

type mcpWhichTestToTrustInput struct {
	RequirementRef string `json:"requirement_ref" jsonschema:"required,description=Test requirement reference; matches Claim.TestRequirementRef on test_result claims"`
	// Optional scope filter — narrows the candidate set to claims
	// whose Scope matches the supplied filter. An empty filter (the
	// zero value) returns every test_result for the requirement,
	// matching the previous behaviour. Use this from a low-privilege
	// caller to avoid enumerating tests across services / environments
	// / teams that are not in scope.
	Service string `json:"service,omitempty" jsonschema:"description=Restrict to claims whose Scope.Service matches"`
	Env     string `json:"env,omitempty" jsonschema:"description=Restrict to claims whose Scope.Env matches"`
	Team    string `json:"team,omitempty" jsonschema:"description=Restrict to claims whose Scope.Team matches"`
}

type mcpWhichTestToTrustCandidate struct {
	ClaimID        string  `json:"belief_id"`
	Text           string  `json:"text"`
	TestID         string  `json:"test_id,omitempty"`
	TestAuthor     string  `json:"test_author,omitempty"`
	TestLastRunAt  string  `json:"test_last_run_at,omitempty"`
	TestPassCount  int     `json:"test_pass_count"`
	TestFailCount  int     `json:"test_fail_count"`
	Score          float64 `json:"score"`
	Rationale      string  `json:"rationale"`       // compact metric breakdown
	ProseRationale string  `json:"prose_rationale"` // operator-readable
}

type mcpWhichTestToTrustOutput struct {
	RequirementRef string                         `json:"requirement_ref"`
	Verdict        string                         `json:"verdict"`
	WinnerClaimID  string                         `json:"winner_belief_id,omitempty"`
	WinnerScore    float64                        `json:"winner_score,omitempty"`
	Candidates     []mcpWhichTestToTrustCandidate `json:"candidates"`
}

type mcpQueryLessonsOutput struct {
	Lessons []domain.Lesson `json:"schemas"`
}

type mcpSynthesizeInput struct {
	MinCorroboration int     `json:"minCorroboration,omitempty" jsonschema:"description=Override the minimum cluster size before a lesson is emitted"`
	MinConfidence    float64 `json:"minConfidence,omitempty" jsonschema:"description=Override the minimum confidence floor in [0, 1]"`
}

type mcpSynthesizeOutput struct {
	Clusters       int      `json:"clusters"`
	LessonsEmitted int      `json:"schemas_emitted"`
	Skipped        int      `json:"skipped"`
	LessonIDs      []string `json:"schema_ids"`
}

type mcpRecordDecisionInput struct {
	Statement    string   `json:"statement" jsonschema:"required,description=Short statement of the decision being recorded"`
	Plan         string   `json:"plan,omitempty" jsonschema:"description=Plan or chosen course of action"`
	Reasoning    string   `json:"reasoning,omitempty" jsonschema:"description=Free-form rationale for choosing this plan"`
	RiskLevel    string   `json:"riskLevel" jsonschema:"required,description=low | medium | high | critical"`
	Beliefs      []string `json:"beliefs,omitempty" jsonschema:"description=Claim ids that justified this decision"`
	Alternatives []string `json:"alternatives,omitempty" jsonschema:"description=Plans considered but not chosen"`
	OutcomeID    string   `json:"outcomeId,omitempty" jsonschema:"description=Optional Outcome id when the result is already known"`
	ChosenAt     string   `json:"chosenAt,omitempty" jsonschema:"description=When the decision was made (RFC3339 / 'now', defaults to now)"`
}

type mcpRecordDecisionOutput struct {
	ID        string `json:"id"`
	Statement string `json:"statement"`
	RiskLevel string `json:"risk_level"`
}

type mcpQueryDecisionsInput struct {
	RiskLevel string `json:"riskLevel,omitempty" jsonschema:"description=Filter to decisions matching this risk level"`
}

type mcpQueryDecisionsOutput struct {
	Decisions []domain.Decision `json:"decisions"`
}

type mcpQueryPlaybookInput struct {
	Trigger string `json:"trigger,omitempty" jsonschema:"description=Trigger label to look up"`
	Service string `json:"service,omitempty" jsonschema:"description=Service scope to filter by"`
}

type mcpQueryPlaybookOutput struct {
	Playbooks []domain.Playbook `json:"reflexes"`
}

type mcpSynthesizePlaybooksInput struct {
	MinLessons    int     `json:"minLessons,omitempty" jsonschema:"description=Minimum corroborating lessons per cluster (default 1)"`
	MinConfidence float64 `json:"minConfidence,omitempty" jsonschema:"description=Minimum confidence floor in [0, 1] (default 0.55)"`
}

type mcpSynthesizePlaybooksOutput struct {
	TriggerClusters  int      `json:"trigger_clusters"`
	PlaybooksEmitted int      `json:"reflexes_emitted"`
	Skipped          int      `json:"skipped"`
	PlaybookIDs      []string `json:"reflex_ids"`
}

// handleMCP starts the MCP server. By default it serves over stdio (for a
// local Claude Code); with `--http <addr>` it serves the same tool set over
// Streamable HTTP so a hosted Mnemos can be reached by remote MCP clients. It
// is a long-lived process that blocks until the connection is closed or the
// process is signalled.
func handleMCP(args []string) {
	serveCfg, err := parseMCPArgs(args)
	if err != nil {
		exitWithMnemosError(false, err)
		return
	}

	logger := bolt.New(bolt.NewJSONHandler(os.Stderr))

	// Reuse one read pool per (tenant-scoped) DSN across requests instead of
	// opening a fresh one each call. Isolation is unchanged — the cache key is
	// the resolved DSN, which includes ?tenant=. Closed by the shutdown defer.
	enableConnCache()

	// Resolve the actor once at startup from MNEMOS_USER_ID; every
	// persistence path below stamps it as created_by / changed_by. We
	// only validate against the DB when the env var is non-empty — an
	// unset env reliably means "attribute to <system>" with no lookup.
	mcpActor := resolveMCPActor()

	// When launched inside a project (.mnemos/ exists), bulk-ingest the
	// standard project documents so the agent has context immediately. New
	// or unchanged source paths are skipped, so this is safe to run on
	// every startup.
	if _, projectRoot, ok := findProjectDB(); ok {
		runAutoIngest(projectRoot, mcpActor)
		if repoIsGit(projectRoot) {
			runGitContextIngest(projectRoot, mcpActor)
			runPRContextIngest(projectRoot, mcpActor)
		}
	}

	srv := mcp.NewServer(mcp.ServerInfo{
		Name:    "mnemos",
		Version: version,
		Capabilities: mcp.Capabilities{
			Tools: true,
		},
	},
		mcp.WithTitle("Mnemos MCP Server"),
		mcp.WithDescription("Query and update evidence-backed local knowledge with Mnemos."),
		mcp.WithWebsiteURL("https://github.com/klarlabs-studio/mnemos"),
		mcp.WithBuildInfo(commit, buildDate),
		mcp.WithInstructions("Use query_knowledge to read the knowledge base, process_text to ingest raw text, and watch_file to keep a specific file's claims fresh as it changes. Prefer process_text before querying when no knowledge exists yet."),
	)

	// watch_file uses a long-lived DB connection separate from the
	// per-call connections in the other handlers. Opened lazily so
	// startup doesn't fail just because the watcher isn't needed.
	// lazyWatcher also remembers the DB handle so the shutdown defer can close
	// it after stopping the polling goroutine — without this the
	// watcher leaks a connection on every MCP exit — and guards both with a
	// mutex so the request goroutines that build it and the main goroutine
	// that tears it down are properly ordered.
	//
	// The watch root is resolved once here, before any request can be served,
	// so a later chdir cannot widen what watch_file may reach.
	watchRoot := resolveWatchRoot()
	var watcherState lazyWatcher
	getWatcher := func() (*Watcher, error) {
		return watcherState.get(func() (*Watcher, *store.Conn, error) {
			// Background context is fine here: the open is a one-shot
			// per process and the long-lived Conn lifecycle is governed
			// by watcherState.shutdown below, not by request-scoped
			// cancellation.
			conn, err := openConn(context.Background())
			if err != nil {
				return nil, nil, err
			}
			// Wrap the long-lived conn in a governed writer so the
			// watcher's re-ingest writes route through the kernel. The
			// writer borrows the conn; watcherState still owns it.
			gw, gwErr := govwrite.Wrap(conn, logger)
			if gwErr != nil {
				return nil, conn, gwErr
			}
			return NewWatcher(gw, mcpActor, watchRoot), conn, nil
		})
	}

	// Lazily build the library Memory facade for the cognitive tools (who-knows,
	// knowledge-gaps, calibration, …). Built once, reused across tool calls, and
	// closed by the shutdown defer below.
	//
	// memMu guards the whole set — the base facade included. It used to be a
	// sync.Once, which publishes memFacade only to goroutines that call Do; the
	// shutdown defer below reads it without calling Do, so under `mcp --http`
	// (build on a request goroutine, close on main) that was the same
	// unsynchronised read as the watcher's.
	var (
		memMu      sync.Mutex
		memBuilt   bool
		memFacade  mnemos.Memory
		memErr     error
		tenantMems = map[string]mnemos.Memory{}
		// brainMems caches one facade per NON-default brain (the repo/workspace
		// overlay), so a by-id read against it costs one construction for the
		// life of the server rather than one per call.
		brainMems = map[string]mnemos.Memory{}
	)
	// getMem returns the cognitive-layer Memory for a request. In multi-tenant
	// mode it returns a per-tenant view (memFacade.Tenant), cached so each
	// tenant reuses one pool for the life of the server.
	getMem := func(ctx context.Context) (mnemos.Memory, error) {
		memMu.Lock()
		defer memMu.Unlock()
		// Base (unscoped) facade uses the plain DSN — never fail-closed — since
		// in multi-tenant mode it is only a handle to call .Tenant(id) on.
		if !memBuilt {
			memBuilt = true
			memFacade, memErr = newLibraryMemoryForDSN(resolveDSN(), mcpActor)
		}
		if memErr != nil {
			return nil, memErr
		}
		tenant, ok := tenantFromContext(ctx)
		if !ok {
			// Fail closed: never hand a cognitive tool the unscoped __default__
			// facade in multi-tenant mode.
			if tenantRequired {
				return nil, errors.New("multi-tenant mode: request has no tenant")
			}
			return memFacade, nil
		}
		if m, cached := tenantMems[tenant]; cached {
			return m, nil
		}
		tm, err := memFacade.Tenant(tenant)
		if err != nil {
			return nil, err
		}
		tenantMems[tenant] = tm
		return tm, nil
	}

	// getMemForBrain returns the Memory for a brain a belief id resolved to
	// (#341). The global tier keeps its existing facade — including the
	// multi-tenant scoping in getMem — while the repo/workspace overlay gets its
	// own, built through the SAME constructor so the two tiers cannot report a
	// belief differently.
	getMemForBrain := func(ctx context.Context, brain claimBrain) (mnemos.Memory, error) {
		if brain.DSN == "" {
			return getMem(ctx)
		}
		memMu.Lock()
		defer memMu.Unlock()
		if m, cached := brainMems[brain.DSN]; cached {
			return m, nil
		}
		m, err := newLibraryMemoryForDSN(brain.DSN, mcpActor)
		if err != nil {
			return nil, err
		}
		brainMems[brain.DSN] = m
		return m, nil
	}

	// Build the axi-go kernel that wraps every MCP tool with effect
	// gating, an evidence chain, and an execution budget. If the
	// kernel fails to build the MCP server still starts — we fall
	// back to direct dispatch so a kernel bug never blocks the agent.
	kernel, kernelErr := buildMCPKernel(logger, mcpExecutorMap(mcpActor, getWatcher))
	if kernelErr != nil {
		fmt.Fprintf(os.Stderr, "mcp: axi-go kernel disabled: %v\n", kernelErr)
	}

	srv.Tool("query_knowledge").
		Description("Query the Mnemos knowledge base and return evidence-backed results.").
		OutputSchema(mcpQueryOutput{}).
		Handler(func(ctx context.Context, input mcpQueryInput) (mcpQueryOutput, error) {
			if kernel != nil {
				return dispatchAxiTool[mcpQueryOutput](ctx, kernel, nil, "query_knowledge", input)
			}
			return mcpRunQueryScoped(ctx, input)
		})

	srv.Tool("process_text").
		Description("Ingest raw text, extract claims, detect relationships, and optionally generate embeddings.").
		OutputSchema(mcpProcessTextOutput{}).
		Handler(func(ctx context.Context, input mcpProcessTextInput) (mcpProcessTextOutput, error) {
			if kernel != nil {
				return dispatchAxiTool[mcpProcessTextOutput](ctx, kernel, nil, "process_text", input)
			}
			return mcpRunProcessText(ctx, mcpActorFor(ctx, mcpActor), input)
		})

	srv.Tool("knowledge_metrics").
		Description("Return counts and statistics about the Mnemos knowledge base.").
		OutputSchema(mcpMetricsOutput{}).
		Handler(func(ctx context.Context, _ struct{}) (mcpMetricsOutput, error) {
			if kernel != nil {
				return dispatchAxiTool[mcpMetricsOutput](ctx, kernel, nil, "knowledge_metrics", struct{}{})
			}
			return mcpRunMetrics(ctx)
		})

	srv.Tool("configure_environment").
		Description("Finish wiring Mnemos into Claude Code: detect the host and install the recall/brief/capture hooks (and a starter config) so memory is automatic. The MCP server itself is already registered. Ask the user before calling if they didn't request setup.").
		OutputSchema(mcpConfigureOutput{}).
		Handler(func(_ context.Context, input mcpConfigureInput) (mcpConfigureOutput, error) {
			return mcpRunConfigure(input)
		})

	srv.Tool("list_beliefs").
		Description("List claims with optional type/status filtering and pagination. Useful for browsing the knowledge base without a specific question.").
		OutputSchema(mcpListClaimsOutput{}).
		Handler(func(ctx context.Context, input mcpListClaimsInput) (mcpListClaimsOutput, error) {
			if kernel != nil {
				return dispatchAxiTool[mcpListClaimsOutput](ctx, kernel, nil, "list_beliefs", input)
			}
			return mcpRunListClaims(ctx, input)
		})

	srv.Tool("list_decisions").
		Description("List claims classified as decisions (shorthand for list_claims with type=decision).").
		OutputSchema(mcpListClaimsOutput{}).
		Handler(func(ctx context.Context, input mcpListClaimsInput) (mcpListClaimsOutput, error) {
			input.Type = string(domain.ClaimTypeDecision)
			if kernel != nil {
				return dispatchAxiTool[mcpListClaimsOutput](ctx, kernel, nil, "list_decisions", input)
			}
			return mcpRunListClaims(ctx, input)
		})

	srv.Tool("list_dissonances").
		Description("List contradiction relationships hydrated with both claims' text. Pagination supported.").
		OutputSchema(mcpListContradictionsOutput{}).
		Handler(func(ctx context.Context, input mcpListContradictionsInput) (mcpListContradictionsOutput, error) {
			if kernel != nil {
				return dispatchAxiTool[mcpListContradictionsOutput](ctx, kernel, nil, "list_dissonances", input)
			}
			return mcpRunListContradictions(ctx, input)
		})

	srv.Tool("ingest_git_prs").
		Description("Ingest merged GitHub pull requests from the project as events. Requires gh CLI authenticated for the repo's remote. Idempotent — already-ingested PR numbers are skipped.").
		OutputSchema(mcpIngestGitPRsOutput{}).
		Handler(func(ctx context.Context, input mcpIngestGitPRsInput) (mcpIngestGitPRsOutput, error) {
			if kernel != nil {
				return dispatchAxiTool[mcpIngestGitPRsOutput](ctx, kernel, nil, "ingest_git_prs", input)
			}
			return mcpRunIngestGitPRs(ctx, mcpActorFor(ctx, mcpActor), input)
		})

	srv.Tool("ingest_git_log").
		Description("Ingest recent git commits from the project repository as events so they appear in queries. Idempotent — already-ingested commits are skipped by SHA.").
		OutputSchema(mcpIngestGitLogOutput{}).
		Handler(func(ctx context.Context, input mcpIngestGitLogInput) (mcpIngestGitLogOutput, error) {
			if kernel != nil {
				return dispatchAxiTool[mcpIngestGitLogOutput](ctx, kernel, nil, "ingest_git_log", input)
			}
			return mcpRunIngestGitLog(ctx, mcpActorFor(ctx, mcpActor), input)
		})

	srv.Tool("record_action").
		Description("Record an operational action (deploy, rollback, scale, etc.) so it can be paired with later Outcomes for the synthesis layer. Idempotent on id.").
		OutputSchema(mcpRecordActionOutput{}).
		Handler(func(ctx context.Context, input mcpRecordActionInput) (mcpRecordActionOutput, error) {
			return mcpRunRecordAction(ctx, mcpActorFor(ctx, mcpActor), input)
		})

	srv.Tool("record_expectation").
		Description("Attach a forward numeric prediction to a belief (predicted value + tolerance + horizon). Resolving it later with record_observation is what feeds credit assignment (ADR 0014). Mirrors POST /v1/beliefs/<id>/expectation.").
		OutputSchema(mcpRecordExpectationOutput{}).
		Handler(func(ctx context.Context, input mcpRecordExpectationInput) (mcpRecordExpectationOutput, error) {
			return mcpRunRecordExpectation(ctx, input)
		})

	srv.Tool("record_observation").
		Description("Record what was actually observed for a belief that carries an expectation, resolving it into a signed prediction error. Returns whether the prediction validated. Mirrors POST /v1/beliefs/<id>/observation.").
		OutputSchema(mcpRecordObservationOutput{}).
		Handler(func(ctx context.Context, input mcpRecordObservationInput) (mcpRecordObservationOutput, error) {
			return mcpRunRecordObservation(ctx, input)
		})

	srv.Tool("record_feedback").
		Description("Record whether a recalled belief was actually useful. A helpful vote resets the negative streak; an unhelpful one extends it. Mirrors POST /v1/beliefs/<id>/feedback.").
		OutputSchema(mcpRecordFeedbackOutput{}).
		Handler(func(ctx context.Context, input mcpRecordFeedbackInput) (mcpRecordFeedbackOutput, error) {
			return mcpRunRecordFeedback(ctx, input)
		})

	srv.Tool("record_outcome").
		Description("Record the observed outcome of a previously recorded Action, including numeric metrics. Idempotent on id.").
		OutputSchema(mcpRecordOutcomeOutput{}).
		Handler(func(ctx context.Context, input mcpRecordOutcomeInput) (mcpRecordOutcomeOutput, error) {
			return mcpRunRecordOutcome(ctx, mcpActorFor(ctx, mcpActor), input)
		})

	srv.Tool("synthesize_schemas").
		Description("Run one full synthesis pass over actions+outcomes and emit derived Lessons. Idempotent: re-running on the same data refreshes confidence without churning ids.").
		OutputSchema(mcpSynthesizeOutput{}).
		Handler(func(ctx context.Context, input mcpSynthesizeInput) (mcpSynthesizeOutput, error) {
			return mcpRunSynthesize(ctx, input)
		})

	srv.Tool("query_schemas").
		Description("Return validated lessons (synthesised operational knowledge) optionally filtered by service or trigger. Lessons are evidence-backed: each carries the action ids that corroborated it and a confidence in [0, 1].").
		OutputSchema(mcpQueryLessonsOutput{}).
		Handler(func(ctx context.Context, input mcpQueryLessonsInput) (mcpQueryLessonsOutput, error) {
			return mcpRunQueryLessons(ctx, input)
		})

	srv.Tool("which_test_to_trust").
		Description("Rank every test_result claim sharing a TestRequirementRef by epistemic credibility (recency, pass-ratio, authority, citations) and return the winner with rationale. Use when CI shows divergent results for the same requirement and the agent must decide which test to believe.").
		OutputSchema(mcpWhichTestToTrustOutput{}).
		Handler(func(ctx context.Context, input mcpWhichTestToTrustInput) (mcpWhichTestToTrustOutput, error) {
			return mcpRunWhichTestToTrust(ctx, input)
		})

	srv.Tool("record_decision").
		Description("Record a decision: the belief claims that justified it, the plan chosen, the alternatives considered, and the risk level. Optional outcomeId attaches an already-observed Outcome.").
		OutputSchema(mcpRecordDecisionOutput{}).
		Handler(func(ctx context.Context, input mcpRecordDecisionInput) (mcpRecordDecisionOutput, error) {
			return mcpRunRecordDecision(ctx, mcpActorFor(ctx, mcpActor), input)
		})

	srv.Tool("query_decisions").
		Description("Return recorded decisions newest-first, optionally filtered by risk level. Each decision carries its belief claim ids, alternatives, and (when attached) the linked Outcome.").
		OutputSchema(mcpQueryDecisionsOutput{}).
		Handler(func(ctx context.Context, input mcpQueryDecisionsInput) (mcpQueryDecisionsOutput, error) {
			return mcpRunQueryDecisions(ctx, input)
		})

	srv.Tool("query_reflex").
		Description("Return playbooks (steps-only operational intelligence) by trigger or service scope. Mnemos returns steps; execution is the caller's responsibility — Praxis consumes this contract.").
		OutputSchema(mcpQueryPlaybookOutput{}).
		Handler(func(ctx context.Context, input mcpQueryPlaybookInput) (mcpQueryPlaybookOutput, error) {
			return mcpRunQueryPlaybook(ctx, input)
		})

	srv.Tool("synthesize_reflexes").
		Description("Run one full playbook-synthesis pass over the lessons store and emit derived Playbooks. Idempotent: re-running on the same lessons refreshes confidence without churning ids.").
		OutputSchema(mcpSynthesizePlaybooksOutput{}).
		Handler(func(ctx context.Context, input mcpSynthesizePlaybooksInput) (mcpSynthesizePlaybooksOutput, error) {
			return mcpRunSynthesizePlaybooks(ctx, input)
		})

	srv.Tool("watch_file").
		Description("Register a file to be re-ingested when its content changes. Polls every few seconds; in-memory only — restart drops all watches. The path must live inside the server's project root (or working directory); paths outside it, including via symlink, are refused.").
		OutputSchema(mcpWatchFileOutput{}).
		Handler(func(ctx context.Context, input mcpWatchFileInput) (mcpWatchFileOutput, error) {
			// watch_file re-ingests a server-local file through a process
			// singleton bound to the default partition — meaningless and
			// cross-tenant-leaky on a multi-tenant server. Refuse it there.
			if tenantRequired {
				return mcpWatchFileOutput{}, errors.New("watch_file is not available on a multi-tenant server")
			}
			if kernel != nil {
				return dispatchAxiTool[mcpWatchFileOutput](ctx, kernel, nil, "watch_file", input)
			}
			out, err := runWatchFileTool(input, getWatcher)
			if err != nil {
				return mcpWatchFileOutput{}, err
			}
			return out, nil
		})

	// --- Self-editing memory tools (letta-style) ---
	srv.Tool("memory_deprecate").
		Description("Mark a claim as deprecated when the agent finds it stale or wrong. Records the rationale on the status transition; existing evidence + history stay queryable.").
		OutputSchema(mcpMemoryDeprecateOutput{}).
		Handler(func(ctx context.Context, input mcpMemoryDeprecateInput) (mcpMemoryDeprecateOutput, error) {
			return mcpRunMemoryDeprecate(ctx, mcpActorFor(ctx, mcpActor), input)
		})

	srv.Tool("memory_resolve_dissonance").
		Description("Pick the winner of two contradicting claims. Winner moves to status=resolved; loser to status=deprecated. Both transitions carry the rationale.").
		OutputSchema(mcpMemoryResolveOutput{}).
		Handler(func(ctx context.Context, input mcpMemoryResolveInput) (mcpMemoryResolveOutput, error) {
			return mcpRunMemoryResolve(ctx, mcpActorFor(ctx, mcpActor), input)
		})

	srv.Tool("memory_escalate").
		Description("Signal that the agent cannot resolve a claim autonomously and requests human review. Records an escalation Verdict on the claim with the agent-provided reason so the audit trail captures who escalated and why.").
		OutputSchema(mcpMemoryEscalateOutput{}).
		Handler(func(ctx context.Context, input mcpMemoryEscalateInput) (mcpMemoryEscalateOutput, error) {
			return mcpRunMemoryEscalate(ctx, mcpActorFor(ctx, mcpActor), input)
		})

	srv.Tool("memory_promote").
		Description("Re-verify a claim against fresh evidence. Bumps last_verified, increments verify_count — the trust score follows.").
		OutputSchema(mcpMemoryPromoteOutput{}).
		Handler(func(ctx context.Context, input mcpMemoryPromoteInput) (mcpMemoryPromoteOutput, error) {
			return mcpRunMemoryPromote(ctx, mcpActorFor(ctx, mcpActor), input)
		})

	srv.Tool("memory_context").
		Description("Render the system-prompt-ready Context Block for a run: top claims by trust, surfaced contradictions, footer with counts. Drop directly into the agent's prompt at the start of each turn.").
		OutputSchema(mcpMemoryContextOutput{}).
		Handler(func(ctx context.Context, input mcpMemoryContextInput) (mcpMemoryContextOutput, error) {
			return mcpRunMemoryContext(ctx, input)
		})

	// --- Agent memory-management primitives (Refs #41) ---
	srv.Tool("remember").
		Description("Store a single fact as a claim, scoped to a run_id, with an event + evidence link so it is auditable. Use this when the agent decides to commit something to long-term memory.").
		OutputSchema(mcpRememberOutput{}).
		Handler(func(ctx context.Context, input mcpRememberInput) (mcpRememberOutput, error) {
			return mcpRunRemember(ctx, mcpActorFor(ctx, mcpActor), input)
		})

	srv.Tool("forget").
		Description("Soft-delete a claim by flipping its status to deprecated. The claim and its evidence stay queryable for audit; future recall paths exclude it from active context.").
		OutputSchema(mcpForgetOutput{}).
		Handler(func(ctx context.Context, input mcpForgetInput) (mcpForgetOutput, error) {
			return mcpRunForget(ctx, mcpActorFor(ctx, mcpActor), input)
		})

	srv.Tool("update").
		Description("Rewrite a claim's text (and optionally its confidence) when the agent's understanding refines. The reason is recorded in the status history.").
		OutputSchema(mcpUpdateOutput{}).
		Handler(func(ctx context.Context, input mcpUpdateInput) (mcpUpdateOutput, error) {
			return mcpRunUpdate(ctx, mcpActorFor(ctx, mcpActor), input)
		})

	srv.Tool("search_memory").
		Description("Semantic search over the agent's memory: embeds the query, ranks claims by cosine similarity, scoped to a run_id (tenant boundary).").
		OutputSchema(mcpSearchMemoryOutput{}).
		Handler(func(ctx context.Context, input mcpSearchMemoryInput) (mcpSearchMemoryOutput, error) {
			return mcpRunSearchMemory(ctx, input)
		})

	srv.Tool("remember_episode").
		Description("Store a temporal event (deployment, incident, decision, ...) with a wall-clock timestamp.").
		OutputSchema(mcpRememberEventOutput{}).
		Handler(func(ctx context.Context, input mcpRememberEventInput) (mcpRememberEventOutput, error) {
			if kernel != nil {
				return dispatchAxiTool[mcpRememberEventOutput](ctx, kernel, nil, "remember_episode", input)
			}
			return mcpRunRememberEvent(ctx, mcpActorFor(ctx, mcpActor), input)
		})

	srv.Tool("timeline_query").
		Description("Return events filtered by time range, type, and run, sorted chronologically.").
		OutputSchema(mcpTimelineQueryOutput{}).
		Handler(func(ctx context.Context, input mcpTimelineQueryInput) (mcpTimelineQueryOutput, error) {
			if kernel != nil {
				return dispatchAxiTool[mcpTimelineQueryOutput](ctx, kernel, nil, "timeline_query", input)
			}
			return mcpRunTimelineQuery(ctx, input)
		})

	srv.Tool("recall_at_time").
		Description("Answer a question against the state of the knowledge base at a historical instant.").
		OutputSchema(mcpQueryOutput{}).
		Handler(func(ctx context.Context, input mcpRecallAtTimeInput) (mcpQueryOutput, error) {
			if kernel != nil {
				return dispatchAxiTool[mcpQueryOutput](ctx, kernel, nil, "recall_at_time", input)
			}
			return mcpRunRecallAtTime(ctx, input)
		})

	// --- Connected-brain cognitive tools (parity with HTTP/gRPC) ---
	srv.Tool("who_knows").
		Description("Rank the workers whose memory best matches a topic (who-knows-what directory).").
		OutputSchema(mcpWhoKnowsOutput{}).
		Handler(func(ctx context.Context, input mcpWhoKnowsInput) (mcpWhoKnowsOutput, error) {
			mem, err := getMem(ctx)
			if err != nil {
				return mcpWhoKnowsOutput{}, err
			}
			return mcpWhoKnows(ctx, mem, input)
		})

	srv.Tool("knowledge_gaps").
		Description("List the highest-value open questions (unresolved hypotheses / contested claims).").
		OutputSchema(mcpKnowledgeGapsOutput{}).
		Handler(func(ctx context.Context, input mcpKnowledgeGapsInput) (mcpKnowledgeGapsOutput, error) {
			mem, err := getMem(ctx)
			if err != nil {
				return mcpKnowledgeGapsOutput{}, err
			}
			return mcpKnowledgeGaps(ctx, mem, input)
		})

	srv.Tool("calibration").
		Description("Report how well stated confidence tracks reality (ECE/Brier, per source).").
		OutputSchema(mcpCalibrationOutput{}).
		Handler(func(ctx context.Context, _ struct{}) (mcpCalibrationOutput, error) {
			mem, err := getMem(ctx)
			if err != nil {
				return mcpCalibrationOutput{}, err
			}
			return mcpCalibration(ctx, mem, struct{}{})
		})

	srv.Tool("predictive_error").
		Description("Report hierarchical prediction error (outcome/schema/dissonance/calibration) + total and hotspot — where the model is most wrong (ADR 0017).").
		OutputSchema(mcpPredictiveErrorOutput{}).
		Handler(func(ctx context.Context, _ struct{}) (mcpPredictiveErrorOutput, error) {
			mem, err := getMem(ctx)
			if err != nil {
				return mcpPredictiveErrorOutput{}, err
			}
			return mcpPredictiveError(ctx, mem, struct{}{})
		})

	srv.Tool("hypercorrections").
		Description("List established beliefs a newer claim now contradicts (worth re-examining).").
		OutputSchema(mcpHypercorrectionsOutput{}).
		Handler(func(ctx context.Context, _ struct{}) (mcpHypercorrectionsOutput, error) {
			mem, err := getMem(ctx)
			if err != nil {
				return mcpHypercorrectionsOutput{}, err
			}
			return mcpHypercorrections(ctx, mem, struct{}{})
		})

	srv.Tool("recombinations").
		Description("List topically-similar-but-unlinked claim pairs (candidate novel connections).").
		OutputSchema(mcpRecombinationsOutput{}).
		Handler(func(ctx context.Context, input mcpRecombinationsInput) (mcpRecombinationsOutput, error) {
			mem, err := getMem(ctx)
			if err != nil {
				return mcpRecombinationsOutput{}, err
			}
			return mcpRecombinations(ctx, mem, input)
		})

	srv.Tool("analogous_beliefs").
		Description("Return the claims most structurally analogous to a given one.").
		OutputSchema(mcpAnalogousOutput{}).
		Handler(func(ctx context.Context, input mcpAnalogousInput) (mcpAnalogousOutput, error) {
			mem, err := getMem(ctx)
			if err != nil {
				return mcpAnalogousOutput{}, err
			}
			return mcpAnalogousClaims(ctx, mem, input)
		})

	// --- Claim reads + advanced recall (parity with HTTP/gRPC) ---
	srv.Tool("get_belief").
		Description("Fetch a single claim's full detail (statement, trust, lifecycle, validity).").
		OutputSchema(mcpClaimDetailOutput{}).
		Handler(func(ctx context.Context, input mcpGetClaimInput) (mcpClaimDetailOutput, error) {
			return mcpRunGetClaim(ctx, getMemForBrain, input)
		})

	srv.Tool("classify").
		Description("Report whether a candidate statement fits established knowledge or is novel.").
		OutputSchema(mcpClassifyOutput{}).
		Handler(func(ctx context.Context, input mcpClassifyInput) (mcpClassifyOutput, error) {
			mem, err := getMem(ctx)
			if err != nil {
				return mcpClassifyOutput{}, err
			}
			return mcpClassify(ctx, mem, input)
		})

	srv.Tool("get_decision").
		Description("Fetch a single recorded decision by id.").
		OutputSchema(mcpDecisionOutput{}).
		Handler(func(ctx context.Context, input mcpGetDecisionInput) (mcpDecisionOutput, error) {
			mem, err := getMem(ctx)
			if err != nil {
				return mcpDecisionOutput{}, err
			}
			return mcpGetDecision(ctx, mem, input)
		})

	srv.Tool("recall").
		Description("Advanced retrieval; mode selects the epistemic-honesty variant (sufficiency|effort|context|conflicts|iterative).").
		OutputSchema(mcpRecallOutput{}).
		Handler(func(ctx context.Context, input mcpRecallInput) (mcpRecallOutput, error) {
			mem, err := getMem(ctx)
			if err != nil {
				return mcpRecallOutput{}, err
			}
			return mcpRecall(ctx, mem, input)
		})

	srv.Tool("get_blocks").
		Description("Read an agent's working-memory blocks — its bounded, always-injected 'core memory' (persona, open_threads, ...).").
		OutputSchema(mcpGetBlocksOutput{}).
		Handler(func(ctx context.Context, input mcpGetBlocksInput) (mcpGetBlocksOutput, error) {
			mem, err := getMem(ctx)
			if err != nil {
				return mcpGetBlocksOutput{}, err
			}
			return mcpGetBlocks(ctx, mem, input)
		})

	srv.Tool("set_block").
		Description("Set or append an agent's working-memory block. append=true evicts oldest lines to stay within the attention budget; empty value replaces (clears) the block.").
		OutputSchema(mcpSetBlockOutput{}).
		Handler(func(ctx context.Context, input mcpSetBlockInput) (mcpSetBlockOutput, error) {
			mem, err := getMem(ctx)
			if err != nil {
				return mcpSetBlockOutput{}, err
			}
			return mcpSetBlock(ctx, mem, input)
		})

	srv.Tool("signals").
		Description("Detected temporal patterns (trend|spike|drop|stall|anomaly|...) over a run's event series — the 'what is changing?' reading that complements recall.").
		OutputSchema(mcpSignalsOutput{}).
		Handler(func(ctx context.Context, input mcpSignalsInput) (mcpSignalsOutput, error) {
			mem, err := getMem(ctx)
			if err != nil {
				return mcpSignalsOutput{}, err
			}
			return mcpSignals(ctx, mem, input)
		})

	// Wire signal handling so a SIGINT/SIGTERM cancels the parent
	// context: ServeStdio observes the cancellation and returns,
	// then we tear the watcher down. Without this, Ctrl+C would
	// leave the polling goroutine alive and the DB unflushed.
	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopSignals := make(chan os.Signal, 1)
	signal.Notify(stopSignals, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig, ok := <-stopSignals
		if !ok {
			return
		}
		fmt.Fprintf(os.Stderr, "mcp: received %s, shutting down...\n", sig)
		cancel()
	}()

	// Defer watcher shutdown so the polling goroutine exits and the
	// DB connection it holds gets released. Cheap if no watcher was
	// ever started.
	defer func() {
		watcherState.shutdown()
		memMu.Lock()
		if memFacade != nil {
			_ = memFacade.Close()
		}
		for _, tm := range tenantMems {
			_ = tm.Close()
		}
		for _, bm := range brainMems {
			_ = bm.Close()
		}
		memMu.Unlock()
		closeConnCache()
	}()

	mw := mcp.WithMiddleware(mcpMiddlewareStack(logger)...)
	if serveCfg.httpAddr != "" {
		// Multi-tenant scoping isolates per token tenant: Postgres by RLS,
		// sqlite/mysql/local libSQL by a separate namespace per tenant. Refuse to
		// start on a backend that can't isolate (memory, remote libSQL) rather
		// than silently share data.
		if serveCfg.requireTenant {
			base := resolveDSN()
			if store.TenancyModeForDSN(base) == store.TenancyNone {
				exitWithMnemosError(false, NewUserError("--require-tenant needs a backend that isolates tenants: postgres (RLS), sqlite, mysql, or local libsql (namespace-per-tenant). This MNEMOS_DB_URL supports neither."))
				return
			}
			// A base DSN with its own ?tenant= would override every request's
			// tenant (ParseDSN reads the first value) and collapse all tenants.
			if dsnHasTenantParam(base) {
				exitWithMnemosError(false, NewUserError("--require-tenant: MNEMOS_DB_URL must not set ?tenant= — the per-request token supplies the tenant"))
				return
			}
			setTenantRequired()
		}
		if err := serveMCPHTTP(rootCtx, srv, serveCfg.httpAddr, serveCfg.requireAuth, serveCfg.requireTenant, mw); err != nil && !errors.Is(err, context.Canceled) {
			log.Fatal(err)
		}
		return
	}
	if err := mcp.ServeStdio(rootCtx, srv, mw); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func runGitContextIngest(projectRoot, actor string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w, err := openWriter(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git: failed to open DB: %v\n", err)
		return
	}
	defer closeWriter(w)

	ingested, skipped, err := ingestGitLog(ctx, w, projectRoot, defaultGitLogLimit, "", actor)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git: %v\n", err)
		return
	}
	if ingested > 0 || skipped > 0 {
		fmt.Fprintf(os.Stderr, "git-context: ingested=%d skipped=%d root=%s\n", ingested, skipped, projectRoot)
	}
}

func runPRContextIngest(projectRoot, actor string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Avoid shelling out to gh if it isn't available or isn't authed —
	// ghAvailable is the cheap probe.
	if !ghAvailable(ctx) {
		return
	}

	w, err := openWriter(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-prs: failed to open DB: %v\n", err)
		return
	}
	defer closeWriter(w)

	ingested, skipped, err := ingestGhPRs(ctx, w, projectRoot, defaultGitPRLimit, actor)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-prs: %v\n", err)
		return
	}
	if ingested > 0 || skipped > 0 {
		fmt.Fprintf(os.Stderr, "git-prs: ingested=%d skipped=%d root=%s\n", ingested, skipped, projectRoot)
	}
}

func mcpRunIngestGitPRs(ctx context.Context, actor string, input mcpIngestGitPRsInput) (mcpIngestGitPRsOutput, error) {
	_, projectRoot, ok := findProjectDB()
	if !ok {
		return mcpIngestGitPRsOutput{}, fmt.Errorf("no project (.mnemos/) found — run 'mnemos init' first")
	}
	if !ghAvailable(ctx) {
		return mcpIngestGitPRsOutput{}, fmt.Errorf("gh CLI not installed or not authenticated for github.com")
	}
	w, err := openWriter(ctx)
	if err != nil {
		return mcpIngestGitPRsOutput{}, err
	}
	defer closeWriter(w)

	ingested, skipped, err := ingestGhPRs(ctx, w, projectRoot, input.Limit, actor)
	if err != nil {
		return mcpIngestGitPRsOutput{}, err
	}
	return mcpIngestGitPRsOutput{Ingested: ingested, Skipped: skipped}, nil
}

func mcpRunIngestGitLog(ctx context.Context, actor string, input mcpIngestGitLogInput) (mcpIngestGitLogOutput, error) {
	_, projectRoot, ok := findProjectDB()
	if !ok {
		return mcpIngestGitLogOutput{}, fmt.Errorf("no project (.mnemos/) found — run 'mnemos init' first")
	}
	if !repoIsGit(projectRoot) {
		return mcpIngestGitLogOutput{}, fmt.Errorf("project root %s is not a git repository", projectRoot)
	}
	w, err := openWriter(ctx)
	if err != nil {
		return mcpIngestGitLogOutput{}, err
	}
	defer closeWriter(w)

	ingested, skipped, err := ingestGitLog(ctx, w, projectRoot, input.Limit, input.Since, actor)
	if err != nil {
		return mcpIngestGitLogOutput{}, err
	}
	return mcpIngestGitLogOutput{Ingested: ingested, Skipped: skipped}, nil
}

func runAutoIngest(projectRoot, actor string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w, err := openWriter(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "auto-ingest: failed to open DB: %v\n", err)
		return
	}
	defer closeWriter(w)

	report := autoIngestProjectDocs(ctx, w, projectRoot, actor)
	if report.Ingested > 0 || report.Skipped > 0 || report.HasFailures() {
		fmt.Fprintf(os.Stderr, "auto-ingest: ingested=%d skipped=%d failed=%d root=%s\n",
			report.Ingested, report.Skipped, len(report.PerFileErrors), projectRoot)
	}
	if report.DedupeFailed {
		fmt.Fprintln(os.Stderr, "auto-ingest: warning — dedupe lookup failed; nothing was ingested to avoid duplicate runs")
	}
	if report.ExtractorError != nil {
		fmt.Fprintf(os.Stderr, "auto-ingest: warning — extractor build failed (%v); nothing was attempted\n", report.ExtractorError)
	}
}

// resolveMCPActor reads MNEMOS_USER_ID at MCP startup. Empty env ->
// SystemUser. Non-empty env -> validated against the local DB so typos
// surface immediately instead of silently stamping a nonexistent user.
// A lookup failure here logs a warning and falls back to SystemUser,
// since the MCP process shouldn't refuse to start over an auth config
// mistake — downstream writes will just carry the fallback attribution.
func resolveMCPActor() string {
	candidate := strings.TrimSpace(os.Getenv("MNEMOS_USER_ID"))
	if candidate == "" {
		return domain.SystemUser
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := openConn(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp: MNEMOS_USER_ID=%s but couldn't open DB to validate: %v — using <system>\n", candidate, err)
		return domain.SystemUser
	}
	defer closeConn(conn)

	actor, err := resolveActor(ctx, conn.Users, candidate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp: MNEMOS_USER_ID=%s rejected: %v — using <system>\n", candidate, err)
		return domain.SystemUser
	}
	return actor
}

// mcpRepoBrainDSN resolves the repo overlay brain from the MCP server's working
// directory (the project Claude Code spawned it in). Empty when the server isn't
// inside an opted-in repo, or in hosted mode.
func mcpRepoBrainDSN() string {
	// The repo overlay resolves from the SERVER's working directory, so on a
	// multi-tenant server it is the same un-tenanted SQLite file for every
	// tenant — federating it into a query would hand each tenant data from
	// outside its isolation boundary. repoBrain only bails on
	// hostedConfigured(), which tests MNEMOS_URL: a *client* setting that is
	// never present on the server. Refuse here instead, as watch_file does.
	if tenantRequired {
		return ""
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	dsn, _ := repoBrain(cwd)
	return dsn
}

// mcpRunQueryScoped federates query_knowledge across the global brain and this
// repo's brain per input.Scope (default "both"). Each side runs against an
// explicit per-request DSN (context override) — never mutating MNEMOS_DB_URL —
// so it is safe under the concurrent MCP transport.
func mcpRunQueryScoped(ctx context.Context, input mcpQueryInput) (mcpQueryOutput, error) {
	scope := strings.ToLower(strings.TrimSpace(input.Scope))
	repoDSN := mcpRepoBrainDSN()

	if repoDSN == "" || scope == "global" {
		return mcpRunQuery(ctx, input) // no overlay, or explicitly global
	}
	if scope == "repo" {
		return mcpRunQuery(withDSNOverride(ctx, repoDSN), input)
	}
	// "both" (default): federate the repo and global tiers. Which tier wins a
	// conflict — or whether the conflict is surfaced — follows the read-time
	// precedence policy (ADR 0011 Phase C); the default preserves repo-wins.
	repoOut, rerr := mcpRunQuery(withDSNOverride(ctx, repoDSN), input)
	globalOut, gerr := mcpRunQuery(ctx, input)
	switch {
	case rerr != nil && gerr != nil:
		return mcpQueryOutput{}, gerr
	case rerr != nil:
		return globalOut, nil
	case gerr != nil:
		return repoOut, nil
	}
	return mergeScopedQuery(repoOut, globalOut, query.PrecedenceOrDefault()), nil
}

// mergeScopedQuery merges a repo-tier and global-tier result under the read-time
// precedence policy (ADR 0011 Phase C). claim_provenance tags each tier;
// contradictions, timeline, and hop distances are unioned. The policy decides
// the winner on conflict:
//   - tenant-wins (default): repo claims lead and win a duplicate; repo's
//     grounded answer and hop distances are preferred — byte-for-byte the
//     historical behavior.
//   - global-wins: the global tier leads and wins instead.
//   - surface-dissonance: repo leads (as tenant-wins), but same-topic,
//     opposing-polarity conflicts between the tiers surface as synthetic
//     `contradicts` relationships on Contradictions so neither silently wins.
func mergeScopedQuery(repo, global mcpQueryOutput, policy query.PrecedencePolicy) mcpQueryOutput {
	globalLeads := policy == query.PrecedenceGlobalWins
	out := mcpQueryOutput{ClaimProvenance: map[string]string{}}
	seen := map[string]bool{}
	add := func(claims []domain.Claim, tier string) {
		for _, c := range claims {
			key := strings.ToLower(strings.TrimSpace(c.Text))
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out.Claims = append(out.Claims, c)
			out.ClaimProvenance[c.ID] = tier
		}
	}
	if globalLeads {
		add(global.Claims, "global")
		add(repo.Claims, "repo")
	} else {
		add(repo.Claims, "repo")
		add(global.Claims, "global")
	}

	out.Contradictions = append(append([]domain.Relationship{}, repo.Contradictions...), global.Contradictions...)
	out.Timeline = append(append([]string{}, repo.Timeline...), global.Timeline...)

	if len(repo.ClaimHopDistance)+len(global.ClaimHopDistance) > 0 {
		out.ClaimHopDistance = map[string]int{}
		lead, follow := repo.ClaimHopDistance, global.ClaimHopDistance
		if globalLeads {
			lead, follow = global.ClaimHopDistance, repo.ClaimHopDistance
		}
		for k, v := range follow {
			out.ClaimHopDistance[k] = v
		}
		for k, v := range lead {
			out.ClaimHopDistance[k] = v // leading tier wins
		}
	}

	leadAnswer, leadClaims, followAnswer := repo.Answer, repo.Claims, global.Answer
	if globalLeads {
		leadAnswer, leadClaims, followAnswer = global.Answer, global.Claims, repo.Answer
	}
	if len(leadClaims) > 0 && strings.TrimSpace(leadAnswer) != "" {
		out.Answer = leadAnswer
	} else {
		out.Answer = followAnswer
	}

	if policy == query.PrecedenceSurfaceDissonance {
		out.Contradictions = append(out.Contradictions, scopedDissonance(repo.Claims, global.Claims)...)
	}
	return out
}

// scopedDissonance returns synthetic `contradicts` relationships for each
// same-topic, opposing-polarity disagreement between a repo claim and a global
// claim. Surfacing them on the existing Contradictions field lets the agent
// reconcile the tenant-vs-global conflict without any new wire identifier.
func scopedDissonance(repo, global []domain.Claim) []domain.Relationship {
	var rels []domain.Relationship
	for _, r := range repo {
		for _, g := range global {
			if query.Conflict(r.Text, g.Text) {
				rels = append(rels, domain.Relationship{
					Type:        domain.RelationshipTypeContradicts,
					FromClaimID: r.ID,
					ToClaimID:   g.ID,
				})
			}
		}
	}
	return rels
}

func mcpRunQuery(ctx context.Context, input mcpQueryInput) (mcpQueryOutput, error) {
	// Enforce the token's run allowlist: deny a disallowed run, and deny an
	// unscoped query from a run-restricted token (which would span every run).
	if err := enforceRunScope(ctx, input.RunID); err != nil {
		return mcpQueryOutput{}, err
	}
	conn, err := openConn(ctx)
	if err != nil {
		return mcpQueryOutput{}, err
	}
	defer closeConn(conn)

	// Shared builder: embeddings AND the full-text leg AND decisions. The
	// hand-rolled chain here wired embeddings and the LLM but never
	// WithTextSearch, so MCP recall lost the sparse (BM25/FTS) leg that nails
	// exact tokens — SHAs, service names, error codes — which dense retrieval
	// blurs. Capability detection lives in one place now.
	engine := newQueryEngine(conn)

	hops := input.Hops
	if hops < 0 {
		hops = 0
	}
	if hops > 5 {
		hops = 5
	}
	// Cognitive retrieval is ON here, not just in the CLI. Before this, MCP
	// built AnswerOptions without the five behaviours, so every recall through
	// Claude Code got none of them and no env var could change it.
	// Consumer=agent, because the caller IS one. This gates more than it looks:
	// verdicts are only produced for an agent consumer, and inhibition
	// (ADR 0016) returns early without verdicts — so leaving it unset made
	// Inhibit a no-op here no matter what the env said.
	opts := query.AnswerOptions{Hops: hops, Consumer: domain.ConsumerAgent}.WithCognitiveDefaults()

	var answer domain.Answer
	if strings.TrimSpace(input.RunID) != "" {
		answer, err = engine.AnswerForRunWithOptions(ctx, strings.TrimSpace(input.Question), strings.TrimSpace(input.RunID), opts)
	} else {
		answer, err = engine.AnswerWithOptions(ctx, strings.TrimSpace(input.Question), opts)
	}
	if err != nil {
		return mcpQueryOutput{}, err
	}

	return mcpQueryOutput{
		Answer:           answer.AnswerText,
		Claims:           answer.Claims,
		Contradictions:   answer.Contradictions,
		Timeline:         answer.TimelineEventIDs,
		ClaimProvenance:  answer.ClaimProvenance,
		ClaimHopDistance: answer.ClaimHopDistance,
	}, nil
}

func mcpRunProcessText(ctx context.Context, actor string, input mcpProcessTextInput) (mcpProcessTextOutput, error) {
	service := ingest.NewService()
	normalizer := parser.NewNormalizer()
	progress := mcp.ProgressFromContext(ctx)

	w, err := openWriter(ctx)
	if err != nil {
		return mcpProcessTextOutput{}, err
	}
	defer closeWriter(w)
	conn := w.Conn()

	runner := workflow.NewRunner(conn.Jobs)
	// 30s fits rule-based extraction, but LLM extraction alone budgets 3x the
	// per-request timeout, so a flat 30s would cancel every LLM attempt and the
	// job would persist nothing. Give the LLM path room for its own budget plus
	// the relate/embed/persist work that follows. This is a ceiling, not a
	// reservation: the caller's context (the capture hook's budget) still
	// caps it, and a fast run returns as soon as it is done.
	runner.Timeout = 30 * time.Second
	if input.UseLLM {
		runner.Timeout = 3*llm.Timeout() + 60*time.Second
	}
	runner.MaxRetries = 1

	var result mcpProcessTextOutput
	err = runner.Run(ctx, "process", map[string]string{"source": "raw_text", "mcp": "true"}, func(ctx context.Context, job *workflow.Job) error {
		total := 5.0
		_ = progress.Report(0, &total)

		if err := job.SetStatus("loading", ""); err != nil {
			return err
		}
		raw := strings.TrimSpace(input.Text)
		in, content, err := service.IngestText(raw, nil)
		if err != nil {
			return err
		}
		_ = progress.Report(1, &total)

		if err := job.SetStatus("extracting", ""); err != nil {
			return err
		}
		events, err := normalizer.Normalize(in, content)
		if err != nil {
			return err
		}
		for i := range events {
			events[i].RunID = job.ID()
			if input.SessionID != "" {
				if events[i].Metadata == nil {
					events[i].Metadata = map[string]string{}
				}
				events[i].Metadata[pipeline.SessionMetadataKey] = input.SessionID
			}
		}

		ext, err := pipeline.NewExtractor(input.UseLLM)
		if err != nil {
			return err
		}
		claims, links, mcpEntities, err := ext.ExtractFn(ctx, events)
		if err != nil {
			return err
		}
		_ = progress.Report(2, &total)
		// MCP path defers entity materialisation to the
		// post-PersistArtifacts step at the end of this handler so
		// the audit/persist transaction stays focused on artifacts.
		_ = mcpEntities

		if err := job.SetStatus("relating", ""); err != nil {
			return err
		}
		relEngine := relate.NewEngine()
		rels, err := relEngine.Detect(claims)
		if err != nil {
			return err
		}

		existingClaims, err := conn.Claims.ListAll(ctx)
		if err != nil {
			return err
		}
		if len(existingClaims) > 0 {
			incrementalRels, err := relEngine.DetectIncremental(claims, existingClaims)
			if err != nil {
				return err
			}
			rels = append(rels, incrementalRels...)
		}
		if input.SessionID != "" {
			newIDs := make(map[string]struct{}, len(claims))
			for i := range claims {
				newIDs[claims[i].ID] = struct{}{}
			}
			filtered, _, ferr := pipeline.DropIntraSessionContradictions(
				ctx, conn.Events, conn.Claims, rels, input.SessionID, newIDs)
			if ferr != nil {
				return ferr
			}
			rels = filtered
		}
		_ = progress.Report(3, &total)

		if err := job.SetStatus("saving", ""); err != nil {
			return err
		}
		stampEventActor(events, actor)
		stampClaimActor(claims, actor)
		stampRelationshipActor(rels, actor)
		if _, err := w.Artifacts(ctx, events, claims, links, rels); err != nil {
			return err
		}
		// Best-effort entity materialisation; failures are logged
		// but don't abort the MCP response. The agent caller cares
		// about the answer; entity tagging is enrichment that can
		// be backfilled via `mnemos extract-entities`.
		if _, entErr := pipeline.MaterializeEntities(ctx, conn, mcpEntities, actor); entErr != nil {
			fmt.Fprintf(os.Stderr, "  entity materialisation (mcp) failed: %v\n", entErr)
		}

		embeddingCount := 0
		if input.UseEmbeddings {
			if err := job.SetStatus("embedding", ""); err != nil {
				return err
			}
			embeddingCount, err = pipeline.GenerateEmbeddings(ctx, conn, events)
			if err != nil {
				return err
			}
			claimEmbCount, claimErr := pipeline.GenerateClaimEmbeddings(ctx, conn, claims)
			if claimErr != nil {
				return claimErr
			}
			embeddingCount += claimEmbCount
		}
		_ = progress.Report(5, &total)

		result = mcpProcessTextOutput{
			RunID:          job.ID(),
			Events:         len(events),
			Claims:         len(claims),
			Relationships:  len(rels),
			Embeddings:     embeddingCount,
			UsedLLM:        input.UseLLM,
			UsedEmbeddings: input.UseEmbeddings,
		}
		if usage := ext.LastUsage(); usage != nil {
			result.InputTokens = usage.InputTokens
			result.OutputTokens = usage.OutputTokens
			result.LLMModel = usage.Model
		}
		return nil
	})
	if err != nil {
		return mcpProcessTextOutput{}, err
	}

	return result, nil
}

func mcpRunMetrics(ctx context.Context) (mcpMetricsOutput, error) {
	conn, err := openConn(ctx)
	if err != nil {
		return mcpMetricsOutput{}, err
	}
	defer closeConn(conn)

	// Each port-typed CountAll/CountByType replaces a hand-written
	// COUNT query. Distinct-run-id is computed in-memory from
	// ListAll because adding a CountDistinctRunID port method just
	// for the metrics surface isn't worth the surface bloat.
	allEvents, _ := conn.Events.ListAll(ctx)
	runIDs := map[string]struct{}{}
	for _, e := range allEvents {
		if e.RunID != "" {
			runIDs[e.RunID] = struct{}{}
		}
	}

	eventsCount, _ := conn.Events.CountAll(ctx)
	claimsCount, _ := conn.Claims.CountAll(ctx)
	relsCount, _ := conn.Relationships.CountAll(ctx)
	contradictionsCount, _ := conn.Relationships.CountByType(ctx, "contradicts")
	embeddingsCount, _ := conn.Embeddings.CountAll(ctx)

	// Contested claims: count from the in-memory listing — same
	// reasoning as runs, the surface payoff doesn't justify a
	// CountByStatus port method.
	allClaims, _ := conn.Claims.ListAll(ctx)
	var contested int64
	for _, c := range allClaims {
		if string(c.Status) == "contested" {
			contested++
		}
	}

	return mcpMetricsOutput{
		Runs:            int64(len(runIDs)),
		Events:          eventsCount,
		Claims:          claimsCount,
		ContestedClaims: contested,
		Relationships:   relsCount,
		Contradictions:  contradictionsCount,
		Embeddings:      embeddingsCount,
	}, nil
}

func mcpRunRecordAction(ctx context.Context, actor string, input mcpRecordActionInput) (mcpRecordActionOutput, error) {
	// record_action persists input.RunID verbatim on the Action, so it is a
	// run-carrying write and gets the same guard as every other one: a run
	// outside the token's allowlist is denied, and a run-restricted token may
	// not write an unscoped (empty-run) Action either.
	if err := enforceRunScope(ctx, input.RunID); err != nil {
		return mcpRecordActionOutput{}, err
	}
	at := time.Now().UTC()
	if strings.TrimSpace(input.At) != "" {
		t, err := parseTimeArg(input.At)
		if err != nil {
			return mcpRecordActionOutput{}, fmt.Errorf("at: %w", err)
		}
		at = t
	}
	id, err := newID("ac_")
	if err != nil {
		return mcpRecordActionOutput{}, fmt.Errorf("generate action id: %w", err)
	}
	resolvedActor := input.Actor
	if resolvedActor == "" {
		resolvedActor = actor
	}
	action := domain.Action{
		ID:        id,
		RunID:     input.RunID,
		Kind:      domain.ActionKind(input.Kind),
		Subject:   input.Subject,
		Actor:     resolvedActor,
		At:        at,
		Metadata:  input.Metadata,
		CreatedBy: actor,
	}
	w, err := openWriter(ctx)
	if err != nil {
		return mcpRecordActionOutput{}, err
	}
	defer closeWriter(w)
	if _, err := w.Action(ctx, action); err != nil {
		return mcpRecordActionOutput{}, err
	}
	return mcpRecordActionOutput{ID: action.ID, Kind: string(action.Kind), Subject: action.Subject}, nil
}

func mcpRunRecordDecision(ctx context.Context, actor string, input mcpRecordDecisionInput) (mcpRecordDecisionOutput, error) {
	chosenAt := time.Now().UTC()
	if strings.TrimSpace(input.ChosenAt) != "" {
		t, err := parseTimeArg(input.ChosenAt)
		if err != nil {
			return mcpRecordDecisionOutput{}, fmt.Errorf("chosenAt: %w", err)
		}
		chosenAt = t
	}
	id, err := newID("dc_")
	if err != nil {
		return mcpRecordDecisionOutput{}, fmt.Errorf("generate decision id: %w", err)
	}
	d := domain.Decision{
		ID:           id,
		Statement:    input.Statement,
		Plan:         input.Plan,
		Reasoning:    input.Reasoning,
		RiskLevel:    domain.RiskLevel(input.RiskLevel),
		Beliefs:      input.Beliefs,
		Alternatives: input.Alternatives,
		OutcomeID:    input.OutcomeID,
		ChosenAt:     chosenAt,
		CreatedBy:    actor,
	}
	w, err := openWriter(ctx)
	if err != nil {
		return mcpRecordDecisionOutput{}, err
	}
	defer closeWriter(w)
	if _, err := w.Decision(ctx, d); err != nil {
		return mcpRecordDecisionOutput{}, err
	}
	return mcpRecordDecisionOutput{ID: d.ID, Statement: d.Statement, RiskLevel: string(d.RiskLevel)}, nil
}

func mcpRunQueryPlaybook(ctx context.Context, input mcpQueryPlaybookInput) (mcpQueryPlaybookOutput, error) {
	conn, err := openConn(ctx)
	if err != nil {
		return mcpQueryPlaybookOutput{}, err
	}
	defer closeConn(conn)
	var ps []domain.Playbook
	switch {
	case input.Trigger != "":
		ps, err = conn.Playbooks.ListByTrigger(ctx, input.Trigger)
	case input.Service != "":
		ps, err = conn.Playbooks.ListByService(ctx, input.Service)
	default:
		ps, err = conn.Playbooks.ListAll(ctx)
	}
	if err != nil {
		return mcpQueryPlaybookOutput{}, err
	}
	return mcpQueryPlaybookOutput{Playbooks: ps}, nil
}

func mcpRunSynthesizePlaybooks(ctx context.Context, input mcpSynthesizePlaybooksInput) (mcpSynthesizePlaybooksOutput, error) {
	conn, err := openConn(ctx)
	if err != nil {
		return mcpSynthesizePlaybooksOutput{}, err
	}
	defer closeConn(conn)
	res, err := synthesize.Playbooks(ctx, conn.Lessons, conn.Playbooks, synthesize.PlaybookOptions{
		MinLessons:    input.MinLessons,
		MinConfidence: input.MinConfidence,
	})
	if err != nil {
		return mcpSynthesizePlaybooksOutput{}, err
	}
	return mcpSynthesizePlaybooksOutput{
		TriggerClusters:  res.TriggerClusters,
		PlaybooksEmitted: res.PlaybooksEmitted,
		Skipped:          res.Skipped,
		PlaybookIDs:      res.PlaybookIDs,
	}, nil
}

func mcpRunQueryDecisions(ctx context.Context, input mcpQueryDecisionsInput) (mcpQueryDecisionsOutput, error) {
	conn, err := openConn(ctx)
	if err != nil {
		return mcpQueryDecisionsOutput{}, err
	}
	defer closeConn(conn)
	var ds []domain.Decision
	if input.RiskLevel != "" {
		ds, err = conn.Decisions.ListByRiskLevel(ctx, input.RiskLevel)
	} else {
		ds, err = conn.Decisions.ListAll(ctx)
	}
	if err != nil {
		return mcpQueryDecisionsOutput{}, err
	}
	return mcpQueryDecisionsOutput{Decisions: ds}, nil
}

func mcpRunQueryLessons(ctx context.Context, input mcpQueryLessonsInput) (mcpQueryLessonsOutput, error) {
	conn, err := openConn(ctx)
	if err != nil {
		return mcpQueryLessonsOutput{}, err
	}
	defer closeConn(conn)
	var ls []domain.Lesson
	switch {
	case input.Service != "":
		ls, err = conn.Lessons.ListByService(ctx, input.Service)
	case input.Trigger != "":
		ls, err = conn.Lessons.ListByTrigger(ctx, input.Trigger)
	default:
		ls, err = conn.Lessons.ListAll(ctx)
	}
	if err != nil {
		return mcpQueryLessonsOutput{}, err
	}
	return mcpQueryLessonsOutput{Lessons: ls}, nil
}

func mcpRunWhichTestToTrust(ctx context.Context, input mcpWhichTestToTrustInput) (mcpWhichTestToTrustOutput, error) {
	if input.RequirementRef == "" {
		return mcpWhichTestToTrustOutput{}, fmt.Errorf("requirement_ref is required")
	}
	conn, err := openConn(ctx)
	if err != nil {
		return mcpWhichTestToTrustOutput{}, err
	}
	defer closeConn(conn)

	matches, err := conn.Claims.ListByTestRequirementRef(ctx, input.RequirementRef)
	if err != nil {
		return mcpWhichTestToTrustOutput{}, err
	}

	scopeFilter := domain.Scope{Service: input.Service, Env: input.Env, Team: input.Team}

	now := time.Now().UTC()
	cands := make([]mcpWhichTestToTrustCandidate, 0)
	for _, c := range matches {
		if !scopeFilter.IsEmpty() && !c.Scope.Matches(scopeFilter) {
			continue
		}
		score, _, rationale, prose := trust.BuildReport(trust.CredibilityInputs{
			CurrentTrust:    c.TrustScore,
			SourceAuthority: c.SourceAuthority,
			Liveness:        c.Liveness,
			CitationCount:   c.CitationCount,
			LastExecuted:    c.LastExecuted,
			LastVerified:    c.LastVerified,
			ValidFrom:       c.ValidFrom,
			CreatedAt:       c.CreatedAt,
			Now:             now,
			IsTest:          true,
			TestLastRunAt:   c.TestLastRunAt,
			TestPassCount:   c.TestPassCount,
			TestFailCount:   c.TestFailCount,
		})
		lastRun := ""
		if !c.TestLastRunAt.IsZero() {
			lastRun = c.TestLastRunAt.UTC().Format(time.RFC3339)
		}
		cands = append(cands, mcpWhichTestToTrustCandidate{
			ClaimID:        c.ID,
			Text:           c.Text,
			TestID:         c.TestID,
			TestAuthor:     c.TestAuthor,
			TestLastRunAt:  lastRun,
			TestPassCount:  c.TestPassCount,
			TestFailCount:  c.TestFailCount,
			Score:          score,
			Rationale:      rationale,
			ProseRationale: prose,
		})
	}

	if len(cands) == 0 {
		return mcpWhichTestToTrustOutput{
			RequirementRef: input.RequirementRef,
			Verdict:        "no_candidates",
			Candidates:     []mcpWhichTestToTrustCandidate{},
		}, nil
	}

	sort.Slice(cands, func(i, j int) bool { return cands[i].Score > cands[j].Score })

	verdict := "winner"
	if len(cands) > 1 && cands[0].Score-cands[1].Score < 0.05 {
		verdict = "ambiguous"
	}
	return mcpWhichTestToTrustOutput{
		RequirementRef: input.RequirementRef,
		Verdict:        verdict,
		WinnerClaimID:  cands[0].ClaimID,
		WinnerScore:    cands[0].Score,
		Candidates:     cands,
	}, nil
}

func mcpRunSynthesize(ctx context.Context, input mcpSynthesizeInput) (mcpSynthesizeOutput, error) {
	conn, err := openConn(ctx)
	if err != nil {
		return mcpSynthesizeOutput{}, err
	}
	defer closeConn(conn)
	res, err := synthesize.Synthesize(ctx, conn.Actions, conn.Outcomes, conn.Lessons, synthesize.Options{
		MinCorroboration: input.MinCorroboration,
		MinConfidence:    input.MinConfidence,
	})
	if err != nil {
		return mcpSynthesizeOutput{}, err
	}
	return mcpSynthesizeOutput{
		Clusters:       res.Clusters,
		LessonsEmitted: res.LessonsEmitted,
		Skipped:        res.Skipped,
		LessonIDs:      res.LessonIDs,
	}, nil
}

func mcpRunRecordOutcome(ctx context.Context, actor string, input mcpRecordOutcomeInput) (mcpRecordOutcomeOutput, error) {
	observed := time.Now().UTC()
	if strings.TrimSpace(input.ObservedAt) != "" {
		t, err := parseTimeArg(input.ObservedAt)
		if err != nil {
			return mcpRecordOutcomeOutput{}, fmt.Errorf("observedAt: %w", err)
		}
		observed = t
	}
	source := input.Source
	if source == "" {
		source = "push"
	}
	id, err := newID("oc_")
	if err != nil {
		return mcpRecordOutcomeOutput{}, fmt.Errorf("generate outcome id: %w", err)
	}
	outcome := domain.Outcome{
		ID:         id,
		ActionID:   input.ActionID,
		Result:     domain.OutcomeResult(input.Result),
		Metrics:    input.Metrics,
		Notes:      input.Notes,
		ObservedAt: observed,
		Source:     source,
		CreatedBy:  actor,
	}
	w, err := openWriter(ctx)
	if err != nil {
		return mcpRecordOutcomeOutput{}, err
	}
	defer closeWriter(w)
	// autoEdge=true: the governed executor appends the outcome and fires
	// the action_of/outcome_of edges, replacing the inline
	// autoedge.OnOutcomeAppended call.
	if _, err := w.Outcome(ctx, outcome, true); err != nil {
		return mcpRecordOutcomeOutput{}, err
	}
	return mcpRecordOutcomeOutput{ID: outcome.ID, ActionID: outcome.ActionID, Result: string(outcome.Result)}, nil
}

// --- Self-editing memory tools (letta-style) ---------------------------------
//
// The four tools below let an LLM agent curate Mnemos's memory while it
// runs: deprecate a claim it discovers is stale, resolve a contradiction
// by picking a winner, and pull a system-prompt-ready Context Block at
// the start of each turn. The shape mirrors letta's core/recall/archival
// edit tools, mapped onto Mnemos's claim-status lifecycle.
//
// All four are stateless: each call resolves the claim by id, applies
// the change through the existing repository contracts, returns a small
// JSON receipt. No agent-internal state is kept.

type mcpMemoryDeprecateInput struct {
	ClaimID string `json:"belief_id" jsonschema:"required,description=Claim id to deprecate"`
	Reason  string `json:"reason,omitempty" jsonschema:"description=Free-form rationale stored on the status transition"`
}

type mcpMemoryDeprecateOutput struct {
	ClaimID   string `json:"belief_id"`
	OldStatus string `json:"old_status"`
	NewStatus string `json:"new_status"`
}

type mcpMemoryResolveInput struct {
	WinnerID string `json:"winner_id" jsonschema:"required,description=Claim id that should remain active"`
	LoserID  string `json:"loser_id" jsonschema:"required,description=Claim id that should be marked deprecated"`
	Reason   string `json:"reason,omitempty" jsonschema:"description=Free-form rationale stored on both status transitions"`
}

type mcpMemoryResolveOutput struct {
	WinnerID string `json:"winner_id"`
	LoserID  string `json:"loser_id"`
}

type mcpMemoryPromoteInput struct {
	ClaimID string `json:"belief_id" jsonschema:"required,description=Claim id to promote (re-verify against fresh evidence)"`
}

type mcpMemoryPromoteOutput struct {
	ClaimID    string `json:"belief_id"`
	VerifiedAt string `json:"verified_at"`
}

type mcpMemoryEscalateInput struct {
	ClaimID string `json:"belief_id" jsonschema:"required,description=ID of the claim the agent cannot resolve autonomously"`
	Reason  string `json:"reason,omitempty" jsonschema:"description=Why the agent is escalating this claim (shown verbatim in the audit trail)"`
}

type mcpMemoryEscalateOutput struct {
	ClaimID          string `json:"belief_id"`
	Action           string `json:"action"`
	EscalationReason string `json:"escalation_reason"`
	Rationale        string `json:"rationale"`
}

type mcpMemoryContextInput struct {
	RunID     string `json:"run_id" jsonschema:"required,description=Run id whose context block to render"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"description=Approximate token budget (chars/4); 0 disables truncation"`
}

type mcpMemoryContextOutput struct {
	RunID   string `json:"run_id"`
	Context string `json:"context"`
}

func mcpRunMemoryEscalate(ctx context.Context, _ string, input mcpMemoryEscalateInput) (mcpMemoryEscalateOutput, error) {
	// Escalate the belief in the brain that HOLDS it (#341); a bare
	// openConn would only ever see the global tier.
	ctx, _, err := mcpScopeToClaimBrain(ctx, input.ClaimID)
	if err != nil {
		return mcpMemoryEscalateOutput{}, err
	}
	conn, err := openConn(ctx)
	if err != nil {
		return mcpMemoryEscalateOutput{}, err
	}
	defer closeConn(conn)
	eng := query.NewEngine(conn.Events, conn.Claims, conn.Relationships)
	verdict, err := eng.EscalateClaimForAgent(ctx, input.ClaimID, input.Reason)
	if err != nil {
		return mcpMemoryEscalateOutput{}, err
	}
	return mcpMemoryEscalateOutput{
		ClaimID:          input.ClaimID,
		Action:           string(verdict.Action),
		EscalationReason: verdict.EscalationReason,
		Rationale:        verdict.Rationale,
	}, nil
}

func mcpRunMemoryDeprecate(ctx context.Context, actor string, input mcpMemoryDeprecateInput) (mcpMemoryDeprecateOutput, error) {
	// Pin the write to the brain that HOLDS the belief BEFORE opening the
	// writer (#341). Deprecating a workspace belief against the global brain
	// does nothing, or worse writes a phantom row.
	ctx, _, err := mcpScopeToClaimBrain(ctx, input.ClaimID)
	if err != nil {
		return mcpMemoryDeprecateOutput{}, err
	}
	gw, err := openWriter(ctx)
	if err != nil {
		return mcpMemoryDeprecateOutput{}, err
	}
	defer closeWriter(gw)
	existing, err := gw.Conn().Claims.ListByIDs(ctx, []string{input.ClaimID})
	if err != nil || len(existing) == 0 {
		return mcpMemoryDeprecateOutput{}, fmt.Errorf("claim %s not found", input.ClaimID)
	}
	old := existing[0].Status
	updated := existing[0]
	updated.Status = domain.ClaimStatusDeprecated
	reason := input.Reason
	if reason == "" {
		reason = "agent-deprecated"
	}
	if _, err := gw.Claims(ctx, []domain.Claim{updated}, govwrite.ClaimReason{Reason: reason, ChangedBy: actor}); err != nil {
		return mcpMemoryDeprecateOutput{}, err
	}
	return mcpMemoryDeprecateOutput{
		ClaimID:   input.ClaimID,
		OldStatus: string(old),
		NewStatus: string(domain.ClaimStatusDeprecated),
	}, nil
}

func mcpRunMemoryResolve(ctx context.Context, actor string, input mcpMemoryResolveInput) (mcpMemoryResolveOutput, error) {
	if input.WinnerID == "" || input.LoserID == "" {
		return mcpMemoryResolveOutput{}, fmt.Errorf("winner_id and loser_id are required")
	}
	if input.WinnerID == input.LoserID {
		return mcpMemoryResolveOutput{}, fmt.Errorf("winner_id and loser_id must differ")
	}
	// Both beliefs must live in ONE brain: the resolution writes both status
	// transitions and this API cannot span two stores atomically (#341). The
	// resolver refuses a split pair explicitly rather than picking a brain and
	// silently dropping the other side.
	ctx, _, err := mcpScopeToClaimBrain(ctx, input.WinnerID, input.LoserID)
	if err != nil {
		return mcpMemoryResolveOutput{}, err
	}
	gw, err := openWriter(ctx)
	if err != nil {
		return mcpMemoryResolveOutput{}, err
	}
	defer closeWriter(gw)
	existing, err := gw.Conn().Claims.ListByIDs(ctx, []string{input.WinnerID, input.LoserID})
	if err != nil || len(existing) < 2 {
		return mcpMemoryResolveOutput{}, fmt.Errorf("both claims must exist")
	}
	reason := input.Reason
	if reason == "" {
		reason = "agent-resolved-contradiction"
	}
	updates := make([]domain.Claim, 0, 2)
	for _, c := range existing {
		switch c.ID {
		case input.WinnerID:
			c.Status = domain.ClaimStatusResolved
		case input.LoserID:
			c.Status = domain.ClaimStatusDeprecated
		}
		updates = append(updates, c)
	}
	if _, err := gw.Claims(ctx, updates, govwrite.ClaimReason{Reason: reason, ChangedBy: actor}); err != nil {
		return mcpMemoryResolveOutput{}, err
	}
	return mcpMemoryResolveOutput{WinnerID: input.WinnerID, LoserID: input.LoserID}, nil
}

func mcpRunMemoryPromote(ctx context.Context, actor string, input mcpMemoryPromoteInput) (mcpMemoryPromoteOutput, error) {
	_ = actor // attribution lives in the verify path's own log
	// MarkVerified is a blind update: without resolving the owning brain first
	// (#341) a workspace belief would be "re-verified" against the global brain,
	// silently touching nothing.
	ctx, _, err := mcpScopeToClaimBrain(ctx, input.ClaimID)
	if err != nil {
		return mcpMemoryPromoteOutput{}, err
	}
	gw, err := openWriter(ctx)
	if err != nil {
		return mcpMemoryPromoteOutput{}, err
	}
	defer closeWriter(gw)
	now := time.Now().UTC()
	if err := gw.MarkVerified(ctx, input.ClaimID, now, 0); err != nil {
		return mcpMemoryPromoteOutput{}, err
	}
	return mcpMemoryPromoteOutput{
		ClaimID:    input.ClaimID,
		VerifiedAt: now.Format(time.RFC3339),
	}, nil
}

func mcpRunMemoryContext(ctx context.Context, input mcpMemoryContextInput) (mcpMemoryContextOutput, error) {
	if input.RunID == "" {
		return mcpMemoryContextOutput{}, fmt.Errorf("run_id is required")
	}
	if err := enforceRunScope(ctx, input.RunID); err != nil {
		return mcpMemoryContextOutput{}, err
	}
	conn, err := openConn(ctx)
	if err != nil {
		return mcpMemoryContextOutput{}, err
	}
	defer closeConn(conn)
	engine := query.NewEngine(conn.Events, conn.Claims, conn.Relationships)
	block, err := engine.BuildContextBlock(ctx, query.ContextBlockOptions{
		RunID:     input.RunID,
		MaxTokens: input.MaxTokens,
	})
	if err != nil {
		return mcpMemoryContextOutput{}, err
	}
	return mcpMemoryContextOutput{RunID: input.RunID, Context: block}, nil
}

// --- Agent memory-management tools (Refs #41) ---
//
// remember / forget / update / search_memory are the four primitives an
// LLM agent needs to keep its own working memory inside Mnemos. They
// wrap the existing claim / event / evidence + similarity-search
// primitives so an agent doesn't have to compose three lower-level
// MCP tools to register a single fact.

type mcpRememberInput struct {
	Text       string  `json:"text" jsonschema:"required,description=The fact to remember"`
	Kind       string  `json:"kind,omitempty" jsonschema:"description=Claim kind: fact (default), hypothesis, decision"`
	RunID      string  `json:"run_id" jsonschema:"required,description=Tenant scope; events are stamped with this for run_id-filtered recall"`
	ValidUntil string  `json:"valid_until,omitempty" jsonschema:"description=RFC3339 timestamp at which this claim should automatically be considered no longer in force"`
	Confidence float64 `json:"confidence,omitempty" jsonschema:"description=Confidence in [0,1]; defaults to 0.9 when omitted"`
}

type mcpRememberOutput struct {
	ClaimID string `json:"belief_id"`
	EventID string `json:"episode_id"`
	RunID   string `json:"run_id"`
	Status  string `json:"status"`
}

type mcpForgetInput struct {
	ClaimID string `json:"belief_id" jsonschema:"required,description=ID of the claim to forget (status flips to deprecated; audit history preserved)"`
	Reason  string `json:"reason,omitempty" jsonschema:"description=Why the agent is forgetting this claim (recorded in the audit trail)"`
}

type mcpForgetOutput struct {
	ClaimID   string `json:"belief_id"`
	OldStatus string `json:"old_status"`
	NewStatus string `json:"new_status"`
}

type mcpUpdateInput struct {
	ClaimID    string  `json:"belief_id" jsonschema:"required,description=ID of the claim to update"`
	NewText    string  `json:"new_text" jsonschema:"required,description=Replacement text for the claim"`
	Confidence float64 `json:"confidence,omitempty" jsonschema:"description=New confidence in [0,1]; omit to keep the current value"`
	Reason     string  `json:"reason,omitempty" jsonschema:"description=Why the agent is rewriting this claim (recorded in the audit trail)"`
}

type mcpUpdateOutput struct {
	ClaimID string `json:"belief_id"`
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

type mcpSearchMemoryInput struct {
	Query         string  `json:"query" jsonschema:"required,description=Free-text query; embedded by the configured provider and matched by cosine similarity"`
	RunID         string  `json:"run_id" jsonschema:"required,description=Tenant scope; required to prevent cross-tenant semantic leakage"`
	TopK          int     `json:"top_k,omitempty" jsonschema:"description=Maximum hits to return (default 10, max 100)"`
	MinSimilarity float64 `json:"min_similarity,omitempty" jsonschema:"description=Drop hits below this cosine similarity threshold (default 0)"`
}

type mcpSearchMemoryHit struct {
	ClaimID    string  `json:"belief_id"`
	Text       string  `json:"text"`
	Similarity float64 `json:"similarity"`
	Status     string  `json:"status"`
}

type mcpSearchMemoryOutput struct {
	Hits []mcpSearchMemoryHit `json:"hits"`
}

func mcpRunRemember(ctx context.Context, actor string, input mcpRememberInput) (mcpRememberOutput, error) {
	text := strings.TrimSpace(input.Text)
	runID := strings.TrimSpace(input.RunID)
	if text == "" {
		return mcpRememberOutput{}, fmt.Errorf("text is required")
	}
	if runID == "" {
		return mcpRememberOutput{}, fmt.Errorf("run_id is required")
	}
	if err := enforceRunScope(ctx, runID); err != nil {
		return mcpRememberOutput{}, err
	}
	kind := domain.ClaimType(strings.TrimSpace(input.Kind))
	if kind == "" {
		kind = domain.ClaimTypeFact
	}
	switch kind {
	case domain.ClaimTypeFact, domain.ClaimTypeHypothesis, domain.ClaimTypeDecision:
	default:
		return mcpRememberOutput{}, fmt.Errorf("kind must be one of: fact, hypothesis, decision (got %q)", input.Kind)
	}
	conf := input.Confidence
	if conf == 0 {
		conf = 0.9
	}
	if conf < 0 || conf > 1 {
		return mcpRememberOutput{}, fmt.Errorf("confidence must be in [0, 1]")
	}
	var validTo time.Time
	if raw := strings.TrimSpace(input.ValidUntil); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return mcpRememberOutput{}, fmt.Errorf("valid_until must be RFC3339: %v", err)
		}
		validTo = t.UTC()
	}

	w, err := openWriter(ctx)
	if err != nil {
		return mcpRememberOutput{}, err
	}
	defer closeWriter(w)

	now := time.Now().UTC()
	eventID := fmt.Sprintf("ev_remember_%d", now.UnixNano())
	claimID := fmt.Sprintf("cl_remember_%d", now.UnixNano())

	event := domain.Event{
		ID:            eventID,
		RunID:         runID,
		SchemaVersion: "v1",
		Content:       text,
		SourceInputID: "agent_remember",
		Timestamp:     now,
		IngestedAt:    now,
		CreatedBy:     actor,
	}
	if _, err := w.Events(ctx, []domain.Event{event}); err != nil {
		return mcpRememberOutput{}, fmt.Errorf("append event: %w", err)
	}

	claim := domain.Claim{
		ID:         claimID,
		Text:       text,
		Type:       kind,
		Confidence: conf,
		Status:     domain.ClaimStatusActive,
		CreatedAt:  now,
		CreatedBy:  actor,
		ValidFrom:  now,
		ValidTo:    validTo,
	}
	if _, err := w.Claims(ctx, []domain.Claim{claim}, govwrite.ClaimReason{Reason: "agent-remember", ChangedBy: actor}); err != nil {
		return mcpRememberOutput{}, fmt.Errorf("append claim: %w", err)
	}
	// ValidTo lives on a separate write path (SetValidity) because
	// it's mutated by lifecycle events (resolve --supersedes) rather
	// than on every upsert. Apply it post-insert when the caller set
	// an explicit TTL.
	if !validTo.IsZero() {
		if err := w.SetValidity(ctx, claimID, validTo); err != nil {
			return mcpRememberOutput{}, fmt.Errorf("set valid_to: %w", err)
		}
	}
	if _, err := w.EvidenceLinks(ctx, []domain.ClaimEvidence{
		{ClaimID: claimID, EventID: eventID},
	}); err != nil {
		return mcpRememberOutput{}, fmt.Errorf("link evidence: %w", err)
	}
	return mcpRememberOutput{
		ClaimID: claimID,
		EventID: eventID,
		RunID:   runID,
		Status:  string(domain.ClaimStatusActive),
	}, nil
}

func mcpRunForget(ctx context.Context, actor string, input mcpForgetInput) (mcpForgetOutput, error) {
	// Forget the belief in the brain that HOLDS it (#341): the agent's id may
	// well have come from the workspace tier of a federated recall.
	ctx, _, err := mcpScopeToClaimBrain(ctx, input.ClaimID)
	if err != nil {
		return mcpForgetOutput{}, err
	}
	gw, err := openWriter(ctx)
	if err != nil {
		return mcpForgetOutput{}, err
	}
	defer closeWriter(gw)
	existing, err := gw.Conn().Claims.ListByIDs(ctx, []string{input.ClaimID})
	if err != nil || len(existing) == 0 {
		return mcpForgetOutput{}, fmt.Errorf("claim %s not found", input.ClaimID)
	}
	old := existing[0].Status
	updated := existing[0]
	updated.Status = domain.ClaimStatusDeprecated
	reason := input.Reason
	if reason == "" {
		reason = "agent-forget"
	}
	if _, err := gw.Claims(ctx, []domain.Claim{updated}, govwrite.ClaimReason{Reason: reason, ChangedBy: actor}); err != nil {
		return mcpForgetOutput{}, err
	}
	return mcpForgetOutput{
		ClaimID:   input.ClaimID,
		OldStatus: string(old),
		NewStatus: string(domain.ClaimStatusDeprecated),
	}, nil
}

func mcpRunUpdate(ctx context.Context, actor string, input mcpUpdateInput) (mcpUpdateOutput, error) {
	claimID := strings.TrimSpace(input.ClaimID)
	newText := strings.TrimSpace(input.NewText)
	if claimID == "" {
		return mcpUpdateOutput{}, fmt.Errorf("claim_id is required")
	}
	if newText == "" {
		return mcpUpdateOutput{}, fmt.Errorf("new_text is required")
	}
	if input.Confidence < 0 || input.Confidence > 1 {
		return mcpUpdateOutput{}, fmt.Errorf("confidence must be in [0, 1]")
	}
	// Rewrite the belief in the brain that HOLDS it (#341) — correcting a stale
	// workspace belief in place is the whole point of this tool.
	ctx, _, err := mcpScopeToClaimBrain(ctx, claimID)
	if err != nil {
		return mcpUpdateOutput{}, err
	}
	gw, err := openWriter(ctx)
	if err != nil {
		return mcpUpdateOutput{}, err
	}
	defer closeWriter(gw)
	existing, err := gw.Conn().Claims.ListByIDs(ctx, []string{claimID})
	if err != nil || len(existing) == 0 {
		return mcpUpdateOutput{}, fmt.Errorf("claim %s not found", claimID)
	}
	oldText := existing[0].Text
	updated := existing[0]
	updated.Text = newText
	if input.Confidence > 0 {
		updated.Confidence = input.Confidence
	}
	reason := input.Reason
	if reason == "" {
		reason = "agent-update"
	}
	if _, err := gw.Claims(ctx, []domain.Claim{updated}, govwrite.ClaimReason{Reason: reason, ChangedBy: actor}); err != nil {
		return mcpUpdateOutput{}, err
	}
	return mcpUpdateOutput{
		ClaimID: claimID,
		OldText: oldText,
		NewText: newText,
	}, nil
}

func mcpRunSearchMemory(ctx context.Context, input mcpSearchMemoryInput) (mcpSearchMemoryOutput, error) {
	q := strings.TrimSpace(input.Query)
	runID := strings.TrimSpace(input.RunID)
	if q == "" {
		return mcpSearchMemoryOutput{}, fmt.Errorf("query is required")
	}
	if runID == "" {
		return mcpSearchMemoryOutput{}, fmt.Errorf("run_id is required (tenant boundary)")
	}
	if err := enforceRunScope(ctx, runID); err != nil {
		return mcpSearchMemoryOutput{}, err
	}
	topK := input.TopK
	if topK == 0 {
		topK = 10
	}
	if topK < 0 || topK > 100 {
		return mcpSearchMemoryOutput{}, fmt.Errorf("top_k must be in [1, 100]")
	}
	if input.MinSimilarity < 0 || input.MinSimilarity > 1 {
		return mcpSearchMemoryOutput{}, fmt.Errorf("min_similarity must be in [0, 1]")
	}

	conn, err := openConn(ctx)
	if err != nil {
		return mcpSearchMemoryOutput{}, err
	}
	defer closeConn(conn)

	searcher, ok := conn.Embeddings.(ports.ClaimSimilaritySearcher)
	if !ok {
		return mcpSearchMemoryOutput{}, fmt.Errorf("current storage backend does not support vector similarity search")
	}
	embedder, err := embedderResolver()
	if err != nil {
		return mcpSearchMemoryOutput{}, fmt.Errorf("embedding provider not configured: %w", err)
	}

	events, err := conn.Events.ListByRunID(ctx, runID)
	if err != nil {
		return mcpSearchMemoryOutput{}, fmt.Errorf("list events for run: %w", err)
	}
	if len(events) == 0 {
		return mcpSearchMemoryOutput{Hits: []mcpSearchMemoryHit{}}, nil
	}
	allowedEventIDs := make(map[string]struct{}, len(events))
	for _, e := range events {
		allowedEventIDs[e.ID] = struct{}{}
	}
	allEvidence, err := conn.Claims.ListAllEvidence(ctx)
	if err != nil {
		return mcpSearchMemoryOutput{}, fmt.Errorf("list evidence: %w", err)
	}
	candidateClaimIDs := make(map[string]struct{})
	for _, link := range allEvidence {
		if _, ok := allowedEventIDs[link.EventID]; ok {
			candidateClaimIDs[link.ClaimID] = struct{}{}
		}
	}
	if len(candidateClaimIDs) == 0 {
		return mcpSearchMemoryOutput{Hits: []mcpSearchMemoryHit{}}, nil
	}

	vectors, err := embedder.Embed(ctx, []string{q})
	if err != nil {
		return mcpSearchMemoryOutput{}, fmt.Errorf("embed query: %w", err)
	}
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		return mcpSearchMemoryOutput{}, fmt.Errorf("embed query: provider returned empty vector")
	}

	hits, err := searcher.SearchClaimsByVector(ctx, vectors[0], candidateClaimIDs, embedding.ModelIDOf(embedder), topK, input.MinSimilarity)
	if err != nil {
		return mcpSearchMemoryOutput{}, fmt.Errorf("similarity search: %w", err)
	}
	if len(hits) == 0 {
		return mcpSearchMemoryOutput{Hits: []mcpSearchMemoryHit{}}, nil
	}

	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ClaimID)
	}
	claims, err := conn.Claims.ListByIDs(ctx, ids)
	if err != nil {
		return mcpSearchMemoryOutput{}, fmt.Errorf("load claims: %w", err)
	}
	claimByID := make(map[string]domain.Claim, len(claims))
	for _, c := range claims {
		claimByID[c.ID] = c
	}
	out := make([]mcpSearchMemoryHit, 0, len(hits))
	for _, h := range hits {
		c, ok := claimByID[h.ClaimID]
		if !ok {
			continue
		}
		out = append(out, mcpSearchMemoryHit{
			ClaimID:    c.ID,
			Text:       c.Text,
			Similarity: h.Similarity,
			Status:     string(c.Status),
		})
	}
	return mcpSearchMemoryOutput{Hits: out}, nil
}
