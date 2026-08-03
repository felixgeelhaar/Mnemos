package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"go.klarlabs.de/axi"
	"go.klarlabs.de/axi/domain"
	"go.klarlabs.de/bolt"
	"go.klarlabs.de/mcp/protocol"
	"go.klarlabs.de/mnemos/internal/kernel"
)

// mcpTools returns the kernel action descriptors for every MCP-exposed
// Mnemos tool. One source of truth so the kernel registration and the
// MCP handler dispatch can't drift. The descriptors reuse the library
// [kernel.Action] shape so the MCP kernel is built by the same
// [kernel.Build] code path as the public library writes.
func mcpTools() []kernel.Action {
	return []kernel.Action{
		{Name: "query_knowledge", Effect: domain.EffectReadLocal, Idempotent: true,
			Description: "Query the Mnemos knowledge base and return evidence-backed results."},
		{Name: "process_text", Effect: domain.EffectWriteLocal, Idempotent: false,
			Description: "Ingest raw text, extract claims, detect relationships, and optionally generate embeddings."},
		{Name: "knowledge_metrics", Effect: domain.EffectReadLocal, Idempotent: true,
			Description: "Return counts and statistics about the Mnemos knowledge base."},
		{Name: "list_beliefs", Effect: domain.EffectReadLocal, Idempotent: true,
			Description: "List claims with optional type/status filtering and pagination."},
		{Name: "list_decisions", Effect: domain.EffectReadLocal, Idempotent: true,
			Description: "List claims classified as decisions."},
		{Name: "list_dissonances", Effect: domain.EffectReadLocal, Idempotent: true,
			Description: "List contradiction relationships hydrated with both claims' text."},
		{Name: "ingest_git_log", Effect: domain.EffectWriteLocal, Idempotent: true,
			Description: "Ingest recent git commits from the project repository as events."},
		{Name: "ingest_git_prs", Effect: domain.EffectWriteLocal, Idempotent: true,
			Description: "Ingest merged GitHub pull requests as events. Requires gh CLI authentication."},
		{Name: "watch_file", Effect: domain.EffectWriteLocal, Idempotent: true,
			Description: "Register a file to be re-ingested when its content changes."},
		{Name: "remember_episode", Effect: domain.EffectWriteLocal, Idempotent: false,
			Description: "Store a temporal event (deployment, incident, decision, ...) with a wall-clock timestamp."},
		{Name: "timeline_query", Effect: domain.EffectReadLocal, Idempotent: true,
			Description: "Return events filtered by time range, type, and run, sorted chronologically."},
		{Name: "recall_at_time", Effect: domain.EffectReadLocal, Idempotent: true,
			Description: "Answer a question as of a historical instant (time-travel retrieval)."},
	}
}

// axiActionName returns the axi-go-compatible action name for the given
// MCP tool name. Delegates to [kernel.ActionName] so the underscore→dash
// convention and the "mnemos-" prefix stay defined in exactly one place,
// shared with the public library writes. The MCP protocol surface is
// unchanged: agents still see snake_case names (e.g. `process_text`).
func axiActionName(mcpName string) string {
	return kernel.ActionName(mcpName)
}

// buildMCPKernel builds an axi-go kernel pre-registered with every MCP
// tool, reusing the library [kernel.Build] infrastructure (bolt logger +
// publisher, the opt-in JSONL evidence sink via MNEMOS_AXI_EVIDENCE_LOG,
// and the env-driven budget). Executors are passed in by the caller
// because each tool's implementation has different shape.
func buildMCPKernel(logger *bolt.Logger, executors map[string]domain.ActionExecutor) (*kernel.Governed, error) {
	return kernel.Build(logger, mcpTools(), executors, kernel.BudgetFromEnv(), "")
}

// remarshal converts between two arbitrary JSON-shaped types via a
// JSON round-trip. Used to bridge axi-go's untyped `any` inputs
// (map[string]any from agents) into our typed mcpXxxInput structs and
// back. Slow on the hot path? Each MCP call is bounded by network
// latency and DB writes — JSON round-trips are rounding error.
func remarshal(in any, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	return nil
}

// toInputMap normalises an arbitrary typed input into the
// map[string]any shape axi-go expects on Invocation.Input. Tools that
// take a `struct{}` (no-arg) end up with an empty map; named fields
// preserve their JSON tags via the round-trip.
func toInputMap(in any) (map[string]any, error) {
	if in == nil {
		return map[string]any{}, nil
	}
	if m, ok := in.(map[string]any); ok {
		return m, nil
	}
	out := map[string]any{}
	if err := remarshal(in, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// mcpExecutorMap returns the canonical executor binding for every
// MCP tool. A single map keyed by axi-go executor ref keeps the
// "what tool dispatches where" answer in one place.
//
// Each executor closure captures the actor and (if it needs DB
// access) opens a per-call connection or reuses a shared *sql.DB.
// Today Mnemos opens fresh connections inside each handler, so the
// closures defer to the existing mcpRunXxx helpers without surfacing
// a *sql.DB here.
func mcpExecutorMap(actor string, getWatcher func() (*Watcher, error)) map[string]domain.ActionExecutor {
	return map[string]domain.ActionExecutor{
		"exec.query_knowledge": toolExecutor[mcpQueryInput, mcpQueryOutput]{run: func(ctx context.Context, in mcpQueryInput) (mcpQueryOutput, error) { return mcpRunQueryScoped(ctx, in) }, summary: querySummary},
		"exec.process_text": toolExecutor[mcpProcessTextInput, mcpProcessTextOutput]{run: func(ctx context.Context, in mcpProcessTextInput) (mcpProcessTextOutput, error) {
			return mcpRunProcessText(ctx, mcpActorFor(ctx, actor), in)
		}, summary: processTextSummary, evidence: processTextEvidence},
		"exec.knowledge_metrics": toolExecutor[struct{}, mcpMetricsOutput]{run: func(ctx context.Context, _ struct{}) (mcpMetricsOutput, error) { return mcpRunMetrics(ctx) }, summary: metricsSummary},
		// The keys below MUST equal kernel.ExecutorRef(<action name in
		// mcpTools()>). They are matched by string at dispatch time, so a rename
		// on one side and not the other builds cleanly, registers cleanly, and
		// then fails every call with "action executor not registered" — which the
		// MCP boundary sanitises into an opaque -32603 (#341). Guarded by
		// TestMCPExecutorMap_BindsEveryKernelAction.
		"exec.list_beliefs": toolExecutor[mcpListClaimsInput, mcpListClaimsOutput]{run: func(ctx context.Context, in mcpListClaimsInput) (mcpListClaimsOutput, error) {
			return mcpRunListClaims(ctx, in)
		}, summary: listClaimsSummary},
		"exec.list_decisions": toolExecutor[mcpListClaimsInput, mcpListClaimsOutput]{run: func(ctx context.Context, in mcpListClaimsInput) (mcpListClaimsOutput, error) {
			in.Type = "decision"
			return mcpRunListClaims(ctx, in)
		}, summary: listClaimsSummary},
		"exec.list_dissonances": toolExecutor[mcpListContradictionsInput, mcpListContradictionsOutput]{run: func(ctx context.Context, in mcpListContradictionsInput) (mcpListContradictionsOutput, error) {
			return mcpRunListContradictions(ctx, in)
		}, summary: listContradictionsSummary},
		"exec.ingest_git_log": toolExecutor[mcpIngestGitLogInput, mcpIngestGitLogOutput]{run: func(ctx context.Context, in mcpIngestGitLogInput) (mcpIngestGitLogOutput, error) {
			return mcpRunIngestGitLog(ctx, mcpActorFor(ctx, actor), in)
		}, summary: ingestGitLogSummary, evidence: ingestGitLogEvidence},
		"exec.ingest_git_prs": toolExecutor[mcpIngestGitPRsInput, mcpIngestGitPRsOutput]{run: func(ctx context.Context, in mcpIngestGitPRsInput) (mcpIngestGitPRsOutput, error) {
			return mcpRunIngestGitPRs(ctx, mcpActorFor(ctx, actor), in)
		}, summary: ingestGitPRsSummary, evidence: ingestGitPRsEvidence},
		"exec.watch_file": toolExecutor[mcpWatchFileInput, mcpWatchFileOutput]{run: func(_ context.Context, in mcpWatchFileInput) (mcpWatchFileOutput, error) {
			return runWatchFileTool(in, getWatcher)
		}, summary: watchFileSummary},
		"exec.remember_episode": toolExecutor[mcpRememberEventInput, mcpRememberEventOutput]{run: func(ctx context.Context, in mcpRememberEventInput) (mcpRememberEventOutput, error) {
			return mcpRunRememberEvent(ctx, mcpActorFor(ctx, actor), in)
		}, summary: rememberEventSummary},
		"exec.timeline_query": toolExecutor[mcpTimelineQueryInput, mcpTimelineQueryOutput]{run: func(ctx context.Context, in mcpTimelineQueryInput) (mcpTimelineQueryOutput, error) {
			return mcpRunTimelineQuery(ctx, in)
		}, summary: timelineQuerySummary},
		"exec.recall_at_time": toolExecutor[mcpRecallAtTimeInput, mcpQueryOutput]{run: func(ctx context.Context, in mcpRecallAtTimeInput) (mcpQueryOutput, error) {
			return mcpRunRecallAtTime(ctx, in)
		}, summary: querySummary},
	}
}

// toolExecutor is the generic adapter that lifts a `func(ctx, In)
// (Out, error)` into a domain.ActionExecutor. Eliminates per-tool
// boilerplate by handling the input/output JSON round-trip and
// summary/evidence projection in one place.
type toolExecutor[In any, Out any] struct {
	run      func(context.Context, In) (Out, error)
	summary  func(Out) string
	evidence func(Out) []domain.EvidenceRecord
}

// Execute satisfies domain.ActionExecutor.
func (e toolExecutor[In, Out]) Execute(ctx context.Context, input any, _ domain.CapabilityInvoker) (domain.ExecutionResult, []domain.EvidenceRecord, error) {
	var typed In
	if input != nil {
		if err := remarshal(input, &typed); err != nil {
			return domain.ExecutionResult{}, nil, err
		}
	}
	out, err := e.run(ctx, typed)
	if err != nil {
		return domain.ExecutionResult{}, nil, err
	}
	res := domain.ExecutionResult{Data: out}
	if e.summary != nil {
		res.Summary = e.summary(out)
	}
	var evs []domain.EvidenceRecord
	if e.evidence != nil {
		evs = e.evidence(out)
	}
	return res, evs, nil
}

// runWatchFileTool wraps the existing watch_file MCP handler shape so
// the executor closure stays one-liner-shaped like the others.
func runWatchFileTool(in mcpWatchFileInput, getWatcher func() (*Watcher, error)) (mcpWatchFileOutput, error) {
	w, err := getWatcher()
	if err != nil {
		return mcpWatchFileOutput{}, err
	}
	count, err := w.Add(in.Path)
	if err != nil {
		return mcpWatchFileOutput{}, err
	}
	return mcpWatchFileOutput{Watching: true, Path: in.Path, ActiveWatches: count}, nil
}

// --- per-tool summary + evidence projections ---
//
// Summaries are short single-line agent-readable hints. Evidence
// records carry the structured facts that should land in the
// tamper-evident chain: counts, IDs, anything an auditor would want
// to verify after the fact.

func querySummary(out mcpQueryOutput) string {
	return fmt.Sprintf("answered with %d claim(s), %d contradiction(s)", len(out.Claims), len(out.Contradictions))
}

func processTextSummary(out mcpProcessTextOutput) string {
	return fmt.Sprintf("ingested run=%s events=%d claims=%d relationships=%d", out.RunID, out.Events, out.Claims, out.Relationships)
}

func processTextEvidence(out mcpProcessTextOutput) []domain.EvidenceRecord {
	records := []domain.EvidenceRecord{
		{Kind: "mnemos.process_text", Source: "mnemos.mcp", Value: map[string]any{
			"run_id":        out.RunID,
			"events":        out.Events,
			"claims":        out.Claims,
			"relationships": out.Relationships,
			"embeddings":    out.Embeddings,
			"used_llm":      out.UsedLLM,
			"used_embed":    out.UsedEmbeddings,
		}},
	}
	// Emit a Kind="llm" evidence record so axi-go's MaxTokens budget
	// sums real spend across the session. Skipped when no tokens were
	// reported (rule-based path, cache hit, or provider that doesn't
	// surface usage).
	if out.InputTokens > 0 || out.OutputTokens > 0 {
		source := out.LLMModel
		if source == "" {
			source = "unknown"
		}
		records = append(records, domain.EvidenceRecord{
			Kind:   "llm",
			Source: source,
			Value: map[string]any{
				"input_tokens":  out.InputTokens,
				"output_tokens": out.OutputTokens,
				"model":         out.LLMModel,
			},
			// axi-go ExecutionBudget.MaxTokens sums TokensUsed across
			// every evidence record on the session. Total = input +
			// output so a token budget covers both directions.
			TokensUsed: int64(out.InputTokens + out.OutputTokens),
		})
	}
	return records
}

func metricsSummary(out mcpMetricsOutput) string {
	return fmt.Sprintf("runs=%d events=%d claims=%d contradictions=%d", out.Runs, out.Events, out.Claims, out.Contradictions)
}

func rememberEventSummary(out mcpRememberEventOutput) string {
	return fmt.Sprintf("recorded event %s", out.ID)
}

func timelineQuerySummary(out mcpTimelineQueryOutput) string {
	return fmt.Sprintf("%d event(s)", len(out.Events))
}

func listClaimsSummary(out mcpListClaimsOutput) string {
	return fmt.Sprintf("listed %d/%d claim(s)", len(out.Claims), out.Total)
}

func listContradictionsSummary(out mcpListContradictionsOutput) string {
	return fmt.Sprintf("listed %d/%d contradiction(s)", len(out.Contradictions), out.Total)
}

func ingestGitLogSummary(out mcpIngestGitLogOutput) string {
	return fmt.Sprintf("git_log: ingested=%d skipped=%d", out.Ingested, out.Skipped)
}

func ingestGitLogEvidence(out mcpIngestGitLogOutput) []domain.EvidenceRecord {
	return []domain.EvidenceRecord{
		{Kind: "mnemos.ingest.git_log", Source: "mnemos.mcp", Value: map[string]any{
			"ingested": out.Ingested, "skipped": out.Skipped,
		}},
	}
}

func ingestGitPRsSummary(out mcpIngestGitPRsOutput) string {
	return fmt.Sprintf("git_prs: ingested=%d skipped=%d", out.Ingested, out.Skipped)
}

func ingestGitPRsEvidence(out mcpIngestGitPRsOutput) []domain.EvidenceRecord {
	return []domain.EvidenceRecord{
		{Kind: "mnemos.ingest.git_prs", Source: "mnemos.mcp", Value: map[string]any{
			"ingested": out.Ingested, "skipped": out.Skipped,
		}},
	}
}

func watchFileSummary(out mcpWatchFileOutput) string {
	return fmt.Sprintf("watching %s (active=%d)", out.Path, out.ActiveWatches)
}

// dispatchAxiTool runs an MCP tool through the kernel and decodes the
// result back into the typed Out shape. The boilerplate is identical
// across every tool, so isolating it here keeps the MCP file readable.
//
// The unused db parameter is reserved: future enrichment (auditing
// invocations into a mcp_invocations table from the same DB) will
// thread it through without changing call sites.
func dispatchAxiTool[Out any](ctx context.Context, k *kernel.Governed, _ *sql.DB, mcpName string, input any) (Out, error) {
	var zero Out
	payload, err := toInputMap(input)
	if err != nil {
		return zero, fmt.Errorf("encode %s input: %w", mcpName, err)
	}
	res, err := k.Execute(ctx, axi.Invocation{
		Action: axiActionName(mcpName),
		Input:  payload,
	})
	if err != nil {
		return zero, err
	}
	if res.Failure != nil {
		// A failure the executor marked as caused by the caller's input becomes
		// -32602 with its message intact. Everything else keeps the previous
		// behaviour and is flattened by publicError into -32603, so the failure
		// direction of anything unmarked is "stays opaque" rather than "leaks"
		// (#355).
		if msg, ok := userInputMessage(res.Failure.Message); ok {
			return zero, protocol.NewInvalidParams(msg)
		}
		return zero, fmt.Errorf("%s: %s", res.Failure.Code, res.Failure.Message)
	}
	if res.Result == nil {
		return zero, fmt.Errorf("%s: kernel returned no result", mcpName)
	}
	if typed, ok := res.Result.Data.(Out); ok {
		return typed, nil
	}
	if err := remarshal(res.Result.Data, &zero); err != nil {
		return zero, fmt.Errorf("decode %s result: %w", mcpName, err)
	}
	return zero, nil
}
