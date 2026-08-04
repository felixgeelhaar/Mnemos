package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"go.klarlabs.de/mnemos/client"
	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/query"
)

// Claude Code hook handlers: `mnemos hook <recall|brief|capture>`.
//
// Each reads the hook event JSON from stdin and, for recall/brief, writes a
// JSON payload with hookSpecificOutput.additionalContext to stdout so Claude
// Code injects Mnemos knowledge into the model's context. capture is a
// side-effect-only SessionEnd handler that records the session into the brain.
//
// These run inside the user's editor loop, so they are FAIL-OPEN: any error
// (no brain yet, DB locked, bad stdin) results in exit 0 with no injected
// context. A hook must never block the user or crash their session — an
// unavailable brain simply means "no memory this turn".

// hookEvent is the subset of the Claude Code hook stdin payload we read.
// Fields not sent for a given event stay zero.
type hookEvent struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	Prompt         string `json:"prompt"` // UserPromptSubmit
	Source         string `json:"source"` // SessionStart: startup|resume|clear|compact
	Reason         string `json:"reason"` // SessionEnd
}

// hookContextOutput injects additional context back into Claude Code.
type hookContextOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// handleHook dispatches `mnemos hook <name>`. It always exits 0; failures
// degrade to "no context injected" rather than surfacing to the user.
func handleHook(args []string) {
	// The brain is pinned via the global --db flag (parsed in ParseFlags and
	// exported as MNEMOS_DB_URL before dispatch), so the hook always targets
	// the store the integration was set up against, regardless of the project
	// directory Claude Code launched it from. Here we only need the sub-name.
	name := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			name = a
			break
		}
	}

	// Incremental capture re-execs itself as a detached worker, so the raw
	// payload is kept to hand to the child on stdin.
	ev, raw := readHookEventRaw(os.Stdin)
	isWorker := false
	for _, a := range args {
		if a == "--worker" {
			isWorker = true
			break
		}
	}

	switch name {
	case "recall", "prompt-submitted":
		hookRecall(ev)
	case "brief", "session-start":
		hookBrief(ev)
	case "capture", "session-end":
		hookCapture(ev)
	case "capture-incremental", "stop", "pre-compact":
		hookCaptureIncremental(ev, isWorker, raw)
	default:
		// Unknown hook name: nothing to do, but don't fail the session.
	}
	os.Exit(int(ExitSuccess))
}

// readHookEvent decodes the hook payload, tolerating an empty or malformed
// stdin (returns a zero event).
func readHookEvent(r io.Reader) hookEvent {
	ev, _ := readHookEventRaw(r)
	return ev
}

// readHookEventRaw also returns the undecoded payload, which the incremental
// capture hook forwards verbatim to its detached worker.
func readHookEventRaw(r io.Reader) (hookEvent, []byte) {
	var ev hookEvent
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil || len(data) == 0 {
		return ev, nil
	}
	_ = json.Unmarshal(data, &ev)
	return ev, data
}

// emitContext writes the additionalContext injection payload. Empty context
// emits nothing (a bare exit 0), which Claude Code treats as "no output".
func emitContext(eventName, context string) {
	context = strings.TrimSpace(context)
	if context == "" {
		return
	}
	out := hookContextOutput{HookSpecificOutput: hookSpecificOutput{
		HookEventName:     eventName,
		AdditionalContext: context,
	}}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

// hostedConfigured reports whether this integration targets a remote hosted
// brain (init --url) rather than a local store. When true, the hooks call the
// REST API via the client package instead of opening a local store.
func hostedConfigured() bool {
	return strings.TrimSpace(os.Getenv(client.EnvBaseURL)) != ""
}

// hostedClient builds a REST client for the configured hosted brain with the
// given per-hook timeout. Only call when hostedConfigured() is true.
func hostedClient(timeout time.Duration) *client.Client {
	opts := []client.Option{client.WithTimeout(timeout)}
	if tok := strings.TrimSpace(os.Getenv("MNEMOS_TOKEN")); tok != "" {
		opts = append(opts, client.WithToken(tok))
	}
	return client.New(os.Getenv(client.EnvBaseURL), opts...)
}

// recallClaim is the transport-agnostic slice of a retrieved claim that the
// recall injection needs, so the local (mcpQueryOutput) and hosted
// (client.SearchResponse) paths share one renderer.
type recallClaim struct {
	Type       string
	Text       string
	TrustScore float64
	Source     string // "global" | "workspace" (blank = untagged)
	// Contested marks a claim whose own status is contested — i.e. mnemos has
	// recorded a direct conflict about it. Distinct from Conflicted below,
	// which is about global-vs-workspace disagreement.
	//
	// Contested was previously counted and then ignored at recall, so a claim
	// under active dispute was surfaced with the same weight and the same
	// trust figure as an uncontested one. "Contested" is a holding state, not
	// a resolution, and it should cost something to be in it.
	Contested bool

	// Conflicted marks a claim that a surface-dissonance read (ADR 0011 Phase C)
	// found to disagree with the other tier on the same topic. Both sides of the
	// disagreement are kept and flagged so the renderer can warn.
	Conflicted bool

	// SessionLocal marks a belief whose value was tied to the conversation that
	// produced it — a progress report, a status snapshot (ADR 0023). It is real
	// and still recalled, but it should not displace durable knowledge from the
	// handful of lines a brief actually shows.
	SessionLocal bool

	// ID and Judged drive the recall-driven fill-in: the beliefs a brief just
	// showed are exactly the ones worth a durability verdict, and Judged says
	// which of them already have one. Unjudged is not the same as
	// SessionLocal=false — the latter is also true for a belief judged durable.
	ID     string
	Judged bool
}

// repoBrain resolves the opt-in repo brain for a session working directory: the
// <repo>/.mnemos/mnemos.db of the nearest ancestor of cwd that has a .mnemos/
// (created by `mnemos init --project`), plus the repo root. Returns ("","") when
// the session isn't inside a repo that opted in — the hooks then use the global
// brain only. Hosted mode has no local repo overlay, and a repo db equal to the
// active (global) brain is not an overlay, so both yield ("",""). Returning the
// root too lets callers avoid re-walking the filesystem.
func repoBrain(cwd string) (dsn, repoRoot string) {
	if hostedConfigured() || strings.TrimSpace(cwd) == "" {
		return "", ""
	}
	// A named workspace (registry, ADR 0010) that owns cwd takes precedence over
	// the implicit .mnemos walk-up. Its matched folder is the AGENTS.md root.
	if wsDSN, _, folder := resolveWorkspaceBrain(cwd); wsDSN != "" {
		if wsDSN == strings.TrimSpace(os.Getenv("MNEMOS_DB_URL")) {
			return "", "" // workspace brain IS the pinned global brain; no overlay
		}
		return wsDSN, folder
	}
	dbPath, root, ok := findProjectDBFrom(cwd)
	if !ok {
		return "", ""
	}
	dsn = "sqlite://" + dbPath
	if dsn == strings.TrimSpace(os.Getenv("MNEMOS_DB_URL")) {
		return "", "" // the repo brain IS the pinned global brain; no overlay
	}
	return dsn, root
}

// hostedWorkspaceTenant returns the hosted tenant id (ADR 0009) for the
// workspace or repo that owns cwd, or "" if the session is in neither. Unlike
// repoBrain — which resolves a *local* overlay DB and bails in hosted mode — this
// resolves the *tenant* used to federate a remote brain: a named workspace's name
// (ADR 0010) or, failing that, the nearest .mnemos repo's portable key, each
// mapped through deriveHostedTenant. The personal (token-default) tier plus this
// tenant are the two scopes a hosted hook federates.
func hostedWorkspaceTenant(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	if _, name, _ := resolveWorkspaceBrain(cwd); name != "" {
		return deriveHostedTenant(name)
	}
	if _, root, ok := findProjectDBFrom(cwd); ok {
		if key := repoTenantKey(root); key != "" {
			return deriveHostedTenant(key)
		}
	}
	return ""
}

// withBrainDSN runs fn with MNEMOS_DB_URL temporarily set to dsn, restoring the
// previous value afterward. Hooks are one-shot, single-goroutine processes, so
// repointing the resolver at a second brain for one call is safe.
func withBrainDSN(dsn string, fn func()) {
	prev, had := os.LookupEnv("MNEMOS_DB_URL")
	_ = os.Setenv("MNEMOS_DB_URL", dsn)
	defer func() {
		if had {
			_ = os.Setenv("MNEMOS_DB_URL", prev)
		} else {
			_ = os.Unsetenv("MNEMOS_DB_URL")
		}
	}()
	fn()
}

// hookRecall (UserPromptSubmit) surfaces claims relevant to the user's prompt,
// federating the global brain with the current repo's brain (if the session is
// inside an opted-in repo). Repo claims are more specific, so they lead and win
// on duplicate text.
func hookRecall(ev hookEvent) {
	q := strings.TrimSpace(ev.Prompt)
	if q == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	// Hosted brain: federate the personal (token-default) tenant with the
	// workspace/repo tenant, mirroring the local two-tier overlay. When the
	// session isn't in a workspace, the workspace call is skipped and this is
	// personal-only.
	if hostedConfigured() {
		cli := hostedClient(12 * time.Second)
		personal, personalContra := hostedRecall(ctx, cli, q, "", "global")
		var wsClaims []recallClaim
		var wsContra int
		if tenant := hostedWorkspaceTenant(ev.Cwd); tenant != "" {
			wsClaims, wsContra = hostedRecall(ctx, cli, q, tenant, "workspace")
		}
		claims, contra := mergeRecall(wsClaims, wsContra, personal, personalContra, query.PrecedenceOrDefault())
		emitContext("UserPromptSubmit", renderRecall(claims, contra))
		return
	}

	// Global brain (the pinned MNEMOS_DB_URL).
	globalClaims, globalContra := recallLocal(ctx, q, "global")

	// Repo overlay, if the session is inside an opted-in repo.
	var repoClaims []recallClaim
	var repoContra int
	if dsn, _ := repoBrain(ev.Cwd); dsn != "" {
		withBrainDSN(dsn, func() {
			repoClaims, repoContra = recallLocal(ctx, q, "workspace")
		})
	}

	claims, contra := mergeRecall(repoClaims, repoContra, globalClaims, globalContra, query.PrecedenceOrDefault())
	emitContext("UserPromptSubmit", renderRecall(claims, contra))

	// Fill in durability verdicts for what this brief just showed. The reader's
	// own path is the only thing that predicts which beliefs surface — recall
	// ranks by relevance to the question, so no static ordering over the brain
	// can (measured: trust is blind to durability and clustered; recency barely
	// discriminates). Detached, throttled, and after the context is emitted, so
	// a brief never waits on it.
	maybeClassifyRecalled(unjudgedIDs(claims, maxRecallClassifyIDs), time.Now())
}

// recallLocal queries the currently-resolved local brain, tagging each claim
// with its source tier. Fail-open: any error yields no claims.
func recallLocal(ctx context.Context, q, source string) ([]recallClaim, int) {
	out, err := mcpRunQuery(ctx, mcpQueryInput{Question: q})
	if err != nil {
		return nil, 0
	}
	claims := make([]recallClaim, 0, len(out.Claims))
	for _, c := range out.Claims {
		claims = append(claims, recallClaim{
			Type: string(c.Type), Text: c.Text, TrustScore: c.TrustScore, Source: source,
			Contested:    c.Status == domain.ClaimStatusContested,
			SessionLocal: c.Durability.IsSessionLocal(),
			ID:           c.ID,
			Judged:       c.Durability.Normalized() != domain.DurabilityUnknown,
		})
	}
	return claims, len(out.Contradictions)
}

// hostedRecall runs one hosted Search scoped to the given tenant ("" = the
// token's default/personal tenant), tagging each claim with its source tier so
// the shared renderer can label it. Fail-open: any error yields no claims.
func hostedRecall(ctx context.Context, cli *client.Client, q, tenant, source string) ([]recallClaim, int) {
	resp, err := cli.Search(client.WithTenant(ctx, tenant), q, client.SearchOptions{TopK: 6})
	if err != nil || resp == nil {
		return nil, 0
	}
	claims := make([]recallClaim, 0, len(resp.Claims))
	for _, c := range resp.Claims {
		claims = append(claims, recallClaim{Type: c.Type, Text: c.Text, TrustScore: c.TrustScore, Source: source})
	}
	return claims, len(resp.Contradictions)
}

// mergeRecall combines the repo/tenant tier with the global tier per the
// read-time precedence policy (ADR 0011 Phase C), de-duplicating by normalized
// text:
//   - tenant-wins (default): repo leads, so its copy survives an identical-text
//     duplicate — byte-for-byte the historical behavior.
//   - global-wins: the global tier leads, so its copy wins the same duplicate.
//   - surface-dissonance: repo leads (as tenant-wins), but same-topic,
//     opposing-polarity conflicts between the tiers keep BOTH claims and are
//     flagged so renderRecall can warn instead of silently trusting one tier.
func mergeRecall(repo []recallClaim, repoContra int, global []recallClaim, globalContra int, policy query.PrecedencePolicy) ([]recallClaim, int) {
	first, second := repo, global
	if policy == query.PrecedenceGlobalWins {
		first, second = global, repo
	}
	seen := make(map[string]bool)
	out := make([]recallClaim, 0, len(repo)+len(global))
	for _, tier := range [][]recallClaim{first, second} {
		for _, c := range tier {
			key := strings.ToLower(strings.TrimSpace(c.Text))
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, c)
		}
	}
	if policy == query.PrecedenceSurfaceDissonance {
		flagRecallDissonance(out, repo, global)
	}
	// Narration sinks below everything, then contested sinks within what
	// remains. Both are stable partitions, so chaining them keeps every other
	// ordering decision the query engine made.
	return demoteSessionLocal(demoteContested(out)), repoContra + globalContra
}

// demoteContested moves claims whose status is contested below uncontested
// ones, preserving relative order within each group.
//
// renderRecall shows only the top six, so ordering decides what the reader
// actually sees. A contested claim is one mnemos has recorded a direct
// conflict about; it should not displace a settled claim from that window.
//
// A stable partition rather than a re-sort on penalised trust: tier precedence
// (ADR 0011 repo-wins) is a deliberate ordering that a global sort by score
// would quietly discard. This demotes only on contested-ness and leaves every
// other ordering decision exactly as the query engine made it.
func demoteContested(claims []recallClaim) []recallClaim {
	settled := make([]recallClaim, 0, len(claims))
	contested := make([]recallClaim, 0)
	for _, c := range claims {
		if c.Contested {
			contested = append(contested, c)
			continue
		}
		settled = append(settled, c)
	}
	return append(settled, contested...)
}

// demoteSessionLocal moves beliefs marked session-local below the rest,
// preserving relative order within each group.
//
// A brief shows only a handful of lines, so ordering decides what the reader
// actually sees. Session-local beliefs are the majority of what capture takes
// in — measured at ~58% of a real brain's recent intake — and left in place
// they crowd out the knowledge a session actually needs to start from.
//
// They are demoted, not dropped: the text is still true and still recalled,
// just after anything durable. Unknown durability is NOT session-local, so the
// entire pre-ADR-0023 back catalogue keeps its existing position.
//
// A stable partition rather than a re-sort, for the same reason as
// demoteContested: tier precedence (ADR 0011 repo-wins) is a deliberate
// ordering that a global sort would quietly discard.
func demoteSessionLocal(claims []recallClaim) []recallClaim {
	durable := make([]recallClaim, 0, len(claims))
	narration := make([]recallClaim, 0)
	for _, c := range claims {
		if c.SessionLocal {
			narration = append(narration, c)
			continue
		}
		durable = append(durable, c)
	}
	return append(durable, narration...)
}

// flagRecallDissonance marks, in place, every merged claim that participates in
// a same-topic, opposing-polarity disagreement between the repo and global
// tiers (ADR 0011 Phase C surface-dissonance policy).
func flagRecallDissonance(out, repo, global []recallClaim) {
	conflicted := make(map[string]bool)
	for _, r := range repo {
		for _, g := range global {
			if query.Conflict(r.Text, g.Text) {
				conflicted[strings.ToLower(strings.TrimSpace(r.Text))] = true
				conflicted[strings.ToLower(strings.TrimSpace(g.Text))] = true
			}
		}
	}
	for i := range out {
		if conflicted[strings.ToLower(strings.TrimSpace(out[i].Text))] {
			out[i].Conflicted = true
		}
	}
}

// renderRecall renders a compact, citation-friendly context block. It caps the
// claim list so the injection stays small. When a repo overlay contributed, each
// claim is tagged with its tier so the model knows what's repo-specific.
func renderRecall(claims []recallClaim, contradictions int) string {
	if len(claims) == 0 {
		return ""
	}
	hasRepo := false
	hasDissonance := false
	for _, c := range claims {
		if c.Source == "workspace" {
			hasRepo = true
		}
		if c.Conflicted {
			hasDissonance = true
		}
	}
	var b strings.Builder
	// A heavily contested topic is the single most useful thing in this block,
	// and it used to be a footnote under six high-trust claims. Readers took
	// the claims at face value and skimmed past the warning — which is exactly
	// backwards, because the warning is what prompts the verification that
	// catches a stale claim. Above the threshold it leads.
	if contradictions >= leadWithContradictionsAt {
		fmt.Fprintf(&b, "⚠ This topic is heavily contested — %d contradiction(s) recorded. Verify against the source before relying on anything below.\n", contradictions)
	}
	b.WriteString("Relevant knowledge from Mnemos (your long-term memory):\n")
	const maxClaims = 6
	for i, c := range claims {
		if i >= maxClaims {
			fmt.Fprintf(&b, "  …and %d more (use the query_knowledge tool for the full set)\n", len(claims)-maxClaims)
			break
		}
		tier := ""
		if hasRepo && c.Source != "" {
			tier = "{" + c.Source + "} "
		}
		// Mark the individual claims under dispute, so a contested claim is
		// identifiable rather than merely counted in an aggregate.
		mark := ""
		if c.Contested {
			mark = " ⚠contested"
		}
		fmt.Fprintf(&b, "  - %s[%s] %s (trust %.2f%s)\n", tier, c.Type, strings.TrimSpace(c.Text), c.TrustScore, mark)
	}
	if contradictions > 0 && contradictions < leadWithContradictionsAt {
		fmt.Fprintf(&b, "  ⚠ %d contradiction(s) recorded on this topic — verify before relying.\n", contradictions)
	}
	if hasDissonance {
		b.WriteString("  ⚠ global and this workspace disagree — verify before relying.\n")
	}
	if hasRepo {
		b.WriteString("{workspace} claims are specific to this workspace/repo and override {global} ones on conflict.\n")
	}
	b.WriteString("If this contradicts newer information, prefer the newer and note the conflict.")
	return b.String()
}

// leadWithContradictionsAt is the contradiction count at or above which the
// warning moves above the claims instead of below them.
//
// Chosen from observed recall blocks: real sessions carried counts of 107, 157
// and 485 on contested topics, while an ordinary topic carries a handful. 25
// sits well clear of normal disagreement without waiting for the extremes.
const leadWithContradictionsAt = 25

// hookBrief (SessionStart) injects a short brain summary at session start.
func hookBrief(ev hookEvent) {
	// Skip on /clear and compaction resumes to avoid re-injecting every time.
	if ev.Source == "clear" || ev.Source == "compact" {
		return
	}
	// Collect a health data point at most once a day, detached so this
	// latency-sensitive path never waits on a full scan. Without this the
	// ADR-0019 vitals have no time series and their thresholds can never be
	// calibrated (see health_snapshot.go).
	maybeSnapshotHealth(time.Now())
	// Let the brain sleep: a daily, detached consolidation (including
	// session-noise clearing) so dissonance follows a circadian rhythm instead
	// of ratcheting up. Off the critical path; see sleep_schedule.go.
	maybeSleep(time.Now())
	if hostedConfigured() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cli := hostedClient(10 * time.Second)
		m, err := cli.Metrics(ctx)
		if err != nil || m == nil {
			return
		}
		// Workspace/repo tenant overlay: count its claims for the scoped note.
		var wsClaims int64
		if tenant := hostedWorkspaceTenant(ev.Cwd); tenant != "" {
			if wm, e := cli.Metrics(client.WithTenant(ctx, tenant)); e == nil && wm != nil {
				wsClaims = wm.Claims
			}
		}
		if m.Claims == 0 && wsClaims == 0 {
			return
		}
		emitContext("SessionStart", renderBrief(m.Claims, m.Runs, m.Contradictions, wsClaims, query.PrecedenceOrDefault()))
		return
	}

	gm, err := mcpRunMetrics(context.Background())
	if err != nil {
		return
	}
	// Repo overlay, if the session is inside an opted-in repo.
	var repoClaims int64
	if dsn, repoRoot := repoBrain(ev.Cwd); dsn != "" {
		// Sync-back: fold any human edits of the repo's AGENTS.md managed block
		// back into the brain so the loop closes. This is the latency-sensitive
		// SessionStart path, so it runs RULE-BASED (no LLM call) under a short
		// timeout — session start must not hang on a slow/unreachable model. The
		// thorough LLM pass happens at session end (capture) and on `sync-docs`.
		if repoRoot != "" {
			sbCtx, sbCancel := context.WithTimeout(context.Background(), 8*time.Second)
			syncBackFromDocs(sbCtx, repoRoot, "AGENTS.md", dsn, false /* useLLM */)
			sbCancel()
		}
		withBrainDSN(dsn, func() {
			if rm, e := mcpRunMetrics(context.Background()); e == nil {
				repoClaims = rm.Claims
			}
		})
	}
	if gm.Claims == 0 && repoClaims == 0 {
		return // both tiers empty: nothing worth announcing
	}
	emitContext("SessionStart", renderBrief(gm.Claims, gm.Runs, gm.Contradictions, repoClaims, query.PrecedenceOrDefault()))
}

// renderBrief formats the session-start brain summary. repoClaims > 0 adds a
// note that this repo has its own scoped overlay. Under the surface-dissonance
// precedence policy (ADR 0011 Phase C) it also warns that global/workspace
// disagreements will be flagged at recall for the agent to reconcile.
func renderBrief(claims, runs, contradictions, repoClaims int64, policy query.PrecedencePolicy) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Mnemos brain connected: %d claims across %d runs", claims, runs)
	if contradictions > 0 {
		fmt.Fprintf(&b, ", %d open contradiction(s)", contradictions)
	}
	if repoClaims > 0 {
		fmt.Fprintf(&b, "; +%d claim(s) scoped to this workspace", repoClaims)
	}
	b.WriteString(".\n")
	if repoClaims > 0 && policy == query.PrecedenceSurfaceDissonance {
		b.WriteString("⚠ Precedence is surface-dissonance: global and this workspace may disagree — such conflicts are flagged at recall; verify before acting.\n")
	}
	b.WriteString("Use query_knowledge before answering questions about past decisions, and record new decisions/facts with process_text or record_decision.")
	return b.String()
}

// defaultCaptureBudget bounds the whole SessionEnd capture when nothing
// overrides it. It has to fit a real worst case, not an optimistic one: a full
// 20KiB transcript extracted by a local model (qwen2.5:14b via ollama)
// measured ~165s end to end. The old 90s was never enforced — extraction and
// the job runner both detached from the caller's context — so it went
// unnoticed until those leaks were closed. Too small a budget is not a
// graceful degradation here: the pipeline persists at the end, so a mid-flight
// cancel drops the entire session's knowledge.
//
// This is a ceiling, not a reservation: a fast provider returns as soon as it
// is done. Hosted brains and cloud LLMs can safely set this much lower.
const defaultCaptureBudget = 240 * time.Second

// captureBudget returns the SessionEnd capture budget, honoring
// MNEMOS_CAPTURE_TIMEOUT (`capture.timeout` in mnemos.yaml). Invalid or
// non-positive values fall back to the default with a warning, matching
// jobTimeout's behavior.
//
// Raising this past captureHookTimeout (the Claude Code hook timeout in
// hooks_install.go) means Claude Code kills the hook before this budget
// applies, so re-run `mnemos init` after raising it to widen the hook timeout
// to match.
func captureBudget() time.Duration {
	raw := strings.TrimSpace(os.Getenv("MNEMOS_CAPTURE_TIMEOUT"))
	if raw == "" {
		return defaultCaptureBudget
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "warning: invalid MNEMOS_CAPTURE_TIMEOUT=%q (want 60s, 4m, etc.); using %s\n", raw, defaultCaptureBudget)
		return defaultCaptureBudget
	}
	return d
}

// hookCapture (SessionEnd) distills the session transcript into the brain.
// SessionEnd ignores hook stdout, so this is pure side effect. It reads the
// JSONL transcript, extracts the conversational text, caps the size, and runs
// it through the standard ingest→extract→relate pipeline tagged with the
// session id so the run is traceable and de-duplicated on replay.
func hookCapture(ev hookEvent) {
	if strings.TrimSpace(ev.TranscriptPath) == "" {
		return
	}
	// Sweep whatever the incremental Stop/PreCompact captures did not already
	// take. When they kept up there is nothing left and this is a no-op, which
	// is what keeps a session's end fast; when they failed or never ran, this
	// captures the whole transcript exactly as it always did.
	//
	// This is the LAST chance to capture the session — nothing runs after it —
	// so it drains in a loop rather than taking a single chunk. A long session
	// carries far more than one chunk of backlog, and a single pass would leave
	// the remainder permanently uncaptured.
	captureDrain(ev, sessionEndChunkBytes)
}

// sessionEndChunkBytes caps one chunk of the SessionEnd sweep. The sweep loops,
// so this bounds a single extraction request (what a slow local model can chew
// through at once), not the total the session may capture.
const sessionEndChunkBytes = 20 * 1024

// captureDrain repeatedly captures chunks until the transcript is drained, the
// shared budget is spent, or a chunk fails. Every chunk shares ONE context, so
// the whole drain is bounded by captureBudget() exactly as a single capture was
// — the Claude Code hook timeout math in hooks_install.go is unchanged.
func captureDrain(ev hookEvent, maxBytes int) {
	ctx, cancel := context.WithTimeout(context.Background(), captureBudget())
	defer cancel()

	for chunks := 0; ; chunks++ {
		if ctx.Err() != nil {
			// Out of budget with backlog left. The offset still points at the
			// first uncaptured byte, so nothing is falsely marked done — but
			// this session ends without it, which is worth saying out loud.
			fmt.Fprintf(os.Stderr,
				"mnemos: capture budget (%s) spent after %d chunk(s); transcript remainder not captured\n",
				captureBudget(), chunks)
			return
		}
		if !captureRangeCtx(ctx, ev, maxBytes) {
			return // drained, or the chunk failed and left the offset for a retry
		}
	}
}

// captureTextCtx runs one chunk of session text through the standard
// ingest→extract→relate pipeline. Reports whether it was persisted, so the
// incremental caller only advances its offset on success.
//
// It takes the caller's context so a multi-chunk drain can hold ONE budget
// across every chunk instead of granting each chunk a fresh one (which would
// let a drain run N × the budget and blow past the Claude Code hook timeout).
func captureTextCtx(ctx context.Context, ev hookEvent, text string) bool {
	if hostedConfigured() {
		// Repo-first routing, hosted edition: a session inside a workspace/repo
		// captures to that tenant (keeping its knowledge scoped), otherwise the
		// personal (token-default) tenant. Let the server decide LLM/embeddings
		// from ITS config (the hook machine can't know the hosted providers): omit
		// the flags (nil).
		pctx := client.WithTenant(ctx, hostedWorkspaceTenant(ev.Cwd))
		_, err := hostedClient(90*time.Second).Process(pctx, client.ProcessRequest{Text: text})
		return err == nil
	}

	// Local brain: auto-enable LLM/embeddings when a provider is configured on
	// this machine; the pipeline degrades to rule-based extraction otherwise.
	//
	// Ask the resolver rather than reading MNEMOS_LLM_PROVIDER directly.
	// ConfigFromEnv also auto-detects a locally running Ollama — the documented
	// zero-config path — so testing the variable alone reports "no LLM" on
	// exactly the setup the project tells people to use, and does so silently.
	//
	// The consequence was not a missing nicety. Every captured session fell back
	// to rule-based extraction, which on conversational text yields fragments of
	// narration as "beliefs" ("Now the doctor output:", "Let me clean those up
	// first"), and UseEmbeddings rode the same flag so those claims got no
	// vectors either. Entity extraction never ran at all: 0 rows, against 86,190
	// claims. `mnemos doctor` reported provider=ollama throughout, because doctor
	// asks the resolver and this did not.
	useLLM := llmConfigured()
	ok := false
	write := func() {
		_, err := mcpRunProcessText(ctx, "claude-code", mcpProcessTextInput{
			Text:          text,
			UseLLM:        useLLM,
			UseEmbeddings: useLLM,
			SessionID:     ev.SessionID,
			// The workspace that owns this session's cwd, so a belief captured
			// in one project never contradicts one captured in another. Empty
			// outside a registered workspace, which suppresses nothing.
			Workspace: captureWorkspace(ev.Cwd),
		})
		ok = err == nil
	}
	// Repo-first routing: a session inside an opted-in repo captures to the repo
	// brain (keeping repo-specific knowledge local); otherwise the global brain.
	if dsn, repoRoot := repoBrain(ev.Cwd); dsn != "" {
		withBrainDSN(dsn, write)
		// Absorb any human edits of the managed block into the brain (thorough,
		// LLM when configured). We deliberately do NOT regenerate AGENTS.md here:
		// regenerating from the brain would wipe a human note that didn't extract
		// into a claim. Refreshing the committed doc is the explicit, non-
		// destructive `mnemos sync-docs` (which absorbs then regenerates). Best-
		// effort: never fail the hook over docs.
		if repoRoot != "" {
			syncBackFromDocs(ctx, repoRoot, "AGENTS.md", dsn, useLLM)
		}
		// Opt-in float-back (MNEMOS_FLOATBACK_ON_CAPTURE, default OFF): after the
		// repo/workspace capture completes, promote important learnings up into the
		// central brain. Skipped entirely when the flag is off, so the default
		// capture path is byte-identical. Best-effort and content-addressed
		// (idempotent), on its own short-timeout context so it never hangs the hook.
		if floatbackOnCaptureEnabled() {
			fbCtx, fbCancel := context.WithTimeout(context.Background(), 20*time.Second)
			floatBackOnCapture(fbCtx, dsn)
			fbCancel()
		}
		return ok
	}
	write()
	return ok
}

// transcriptLine is the lenient shape we read from each JSONL transcript row.
// Claude Code's transcript schema varies by version, so we decode defensively:
// role/type plus a content field that may be a string or an array of blocks.
type transcriptLine struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Message json.RawMessage `json:"message"`
	Content json.RawMessage `json:"content"`
}

// extractTranscriptText pulls user prompts and assistant text out of a JSONL
// transcript, oldest first, stopping once maxBytes is reached.
func extractTranscriptText(path string, maxBytes int) string {
	f, err := os.Open(path) //nolint:gosec // path supplied by Claude Code hook payload
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		return ""
	}
	text, _ := transcriptTextFromLines(string(data), maxBytes)
	return text
}

// transcriptTextFromLines pulls user prompts and assistant text out of raw
// JSONL transcript lines, oldest first, stopping once maxBytes is reached.
// Shared by the whole-file and incremental (offset-based) readers so both see
// identical content rules.
//
// It returns the extracted text AND the number of bytes of data it actually
// consumed to produce that text. The second value is what makes incremental
// capture correct: the reader's window (4 MiB) is far larger than maxBytes
// (8–20 KiB), so stopping at the cap leaves most of the window unread. An
// offset advanced by the window rather than by what was consumed silently
// skips everything past the cap — the whole point of the incremental strategy
// is that the *next* run picks up exactly where this one stopped.
func transcriptTextFromLines(data string, maxBytes int) (text string, consumed int) {
	var b strings.Builder
	pos := 0
	for pos < len(data) {
		// Slice one line without allocating, tracking where it ends so the
		// caller can resume from exactly there.
		raw := data[pos:]
		next := len(data)
		if idx := strings.IndexByte(raw, '\n'); idx >= 0 {
			raw = raw[:idx]
			next = pos + idx + 1
		}
		pos = next

		if line, ok := parseTranscriptLine(raw); ok {
			b.WriteString(line)
			b.WriteByte('\n')
			// Break *after* advancing pos, so the line that filled the budget
			// counts as consumed and is not re-ingested next run.
			if b.Len() >= maxBytes {
				break
			}
		}
	}
	return b.String(), pos
}

// parseTranscriptLine extracts the conversational text from one raw JSONL
// line, reporting whether the line carried any. Non-JSON, non-conversational,
// and empty-text lines are skipped — a transcript legitimately contains all
// three, so they are not errors.
func parseTranscriptLine(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	var line transcriptLine
	if err := json.Unmarshal([]byte(raw), &line); err != nil {
		return "", false
	}
	role := line.Role
	if role == "" {
		role = line.Type
	}
	if role != "user" && role != "assistant" {
		return "", false
	}
	body := line.Message
	if len(body) == 0 {
		body = line.Content
	}
	txt := extractMessageText(body, line.Content)
	return txt, txt != ""
}

// extractMessageText coaxes plain text out of a message payload that may be a
// string, an object with a content field (itself a string or block array), or
// a bare array of {type,text} blocks.
func extractMessageText(message, content json.RawMessage) string {
	for _, raw := range []json.RawMessage{message, content} {
		if t := textFromAny(raw); t != "" {
			// Strip harness-injected content (system reminders, task
			// notifications, loaded skill files, resume preambles). It arrives
			// in user-role messages, so without this capture reads it as
			// conversation and extraction turns it into claims — 98 of 525
			// junk claims removed from one production brain came from exactly
			// this. Returns "" when the message was nothing else, and the
			// caller then skips the line.
			return stripHarnessText(t)
		}
	}
	return ""
}

// textFromAny recursively resolves text from a raw JSON value that may be a
// string, a block array, or an object with text/content fields.
func textFromAny(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	if t := extractBlocks(raw); t != "" {
		return t
	}
	var obj struct {
		Content json.RawMessage `json:"content"`
		Text    string          `json:"text"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if strings.TrimSpace(obj.Text) != "" {
			return obj.Text
		}
		if t := textFromAny(obj.Content); t != "" {
			return t
		}
	}
	return ""
}

// extractBlocks joins the text of an Anthropic-style content-block array,
// skipping non-text blocks (tool_use, tool_result, images).
func extractBlocks(raw json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, blk := range blocks {
		if blk.Type == "text" && strings.TrimSpace(blk.Text) != "" {
			parts = append(parts, blk.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// captureWorkspace names the project a captured session belongs to, or "" when
// the session is not inside a registered workspace.
//
// The registry name is used rather than the path: it is stable across clones
// and checkouts, and it is what `mnemos workspace list` shows, so a stored
// value means something to a human reading it back. A path would tie every
// belief to one machine's directory layout.
//
// Returning "" is the safe answer and the common one — a session outside any
// registered workspace suppresses nothing, which is the same fail-open rule the
// session filter uses.
func captureWorkspace(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return ""
	}
	_, name, _ := resolveWorkspaceBrain(cwd)
	return name
}
