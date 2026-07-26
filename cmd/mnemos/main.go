package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.klarlabs.de/mnemos/internal/config"
	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/embedding"
	"go.klarlabs.de/mnemos/internal/extract"
	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/ingest"
	"go.klarlabs.de/mnemos/internal/llm"
	"go.klarlabs.de/mnemos/internal/parser"
	"go.klarlabs.de/mnemos/internal/pipeline"
	"go.klarlabs.de/mnemos/internal/ports"
	"go.klarlabs.de/mnemos/internal/query"
	"go.klarlabs.de/mnemos/internal/relate"
	"go.klarlabs.de/mnemos/internal/workflow"

	// Register storage providers with the top-level store registry so
	// resolveDSN()/openConn() can dispatch on URL scheme. Side-effect
	// imports only. All providers below are production-ready: sqlite,
	// libsql, mysql, and postgres (schema-per-?namespace=, verified
	// against pgvector pg17).
	_ "go.klarlabs.de/mnemos/internal/store/libsql"
	_ "go.klarlabs.de/mnemos/internal/store/memory"
	_ "go.klarlabs.de/mnemos/internal/store/mysql"
	_ "go.klarlabs.de/mnemos/internal/store/postgres"
	_ "go.klarlabs.de/mnemos/internal/store/sqlite"
)

// resolveDBPath returns the SQLite file path used when MNEMOS_DB_URL
// is not set. Precedence:
//  1. Nearest .mnemos/mnemos.db walking up from the working directory
//  2. XDG-compliant global default (~/.local/share/mnemos/mnemos.db)
//
// Operators who want a non-SQLite backend (or any path not matching
// these defaults) set MNEMOS_DB_URL explicitly.
func resolveDBPath() string {
	if p, _, ok := findProjectDB(); ok {
		return p
	}
	return globalDBPath()
}

// globalDBPath returns the XDG-compliant global SQLite path
// (~/.local/share/mnemos/mnemos.db, honoring XDG_DATA_HOME) without any
// project walk-up. This is the "central brain" location shared across every
// working directory — used by `mnemos setup` and as resolveDBPath's fallback.
func globalDBPath() string {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join("data", "mnemos.db")
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "mnemos", "mnemos.db")
}

// findProjectDB walks up from the current working directory looking for a
// .mnemos directory. When found, returns the path to mnemos.db inside it,
// the project root (the directory containing .mnemos), and true. Stops at
// the filesystem root or the user's home directory (whichever is reached
// first) to avoid accidentally adopting a parent project's DB.
//
// The home directory is a hard stop that is checked BEFORE its own .mnemos:
// $HOME/.mnemos is the global fallback dir (jwt-secret, user-global config),
// not a project brain. Adopting it would shadow the XDG global brain for
// essentially every directory under $HOME — see ADR 0008. Project brains live
// strictly below $HOME; at or above home we fall through to the XDG default.
func findProjectDB() (dbPath, projectRoot string, ok bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", false
	}
	return findProjectDBFrom(cwd)
}

// findProjectDBFrom is findProjectDB rooted at an explicit directory instead of
// the process CWD. The hooks use it with the session's cwd (from the Claude Code
// hook payload) to resolve a repo brain — the hook process's own CWD is not the
// user's working directory.
func findProjectDBFrom(startDir string) (dbPath, projectRoot string, ok bool) {
	if startDir == "" {
		return "", "", false
	}
	home, _ := os.UserHomeDir()
	dir := startDir
	for {
		if home != "" && dir == home {
			return "", "", false
		}
		candidate := filepath.Join(dir, ".mnemos")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return filepath.Join(candidate, "mnemos.db"), dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false
		}
		dir = parent
	}
}

func printProgress(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// newExtractor builds the appropriate extraction engine based on the --llm flag.
func newExtractor(useLLM bool) (*pipeline.Extractor, error) {
	ext, err := pipeline.NewExtractor(useLLM)
	if err != nil {
		return nil, &MnemosError{
			Code:    ExitUsage,
			Message: fmt.Sprintf("LLM configuration error: %s", err),
			Hint:    "Set MNEMOS_LLM_PROVIDER and MNEMOS_LLM_API_KEY environment variables\n  Providers: anthropic, openai, gemini, ollama, openai-compat",
		}
	}
	return ext, nil
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(int(ExitUsage))
	}

	flags, args := ParseFlags(os.Args[1:])

	// Layer the YAML config file under the environment before anything
	// reads MNEMOS_* variables. Exported env vars always win; the file only
	// fills gaps. A missing explicit --config/MNEMOS_CONFIG file is fatal;
	// an implicit discovery miss is silently fine.
	if path, applied, err := config.LoadAndHydrate(flags.Config); err != nil {
		fmt.Fprintf(os.Stderr, "config: %s\n", err)
		os.Exit(int(ExitConfig))
	} else if path != "" && flags.Verbose {
		printProgress("config: loaded %s (%d value(s) applied)", path, len(applied))
	}

	// Episodic event typing (ADR 0023 part 2) is read inside internal/pipeline
	// now, so every entrypoint honours MNEMOS_EPISODIC_EVENTS — not just this
	// one. Assigning it here made the embedded library ignore the variable.

	// Validate the read-time precedence policy (ADR 0011 Phase C) once the
	// config is hydrated, so an unrecognized MNEMOS_PRECEDENCE fails fast with a
	// clear message instead of silently degrading at query time.
	if _, err := query.PrecedenceFromEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "config: %s\n", err)
		os.Exit(int(ExitConfig))
	}

	// An explicit --db flag is the most specific DSN source, so it wins over
	// both the config file (already hydrated above) and any inherited
	// MNEMOS_DB_URL. Exporting it here means every command — not just the ones
	// that used to parse --db themselves (hook/init/setup) — resolves the same
	// brain via resolveDSN(). init/setup read flags.DB directly for the brain
	// they configure; setting the env too is harmless (they re-pin it anyway).
	if flags.DB != "" {
		_ = os.Setenv("MNEMOS_DB_URL", flags.DB)
	}

	// Default to human-readable output in interactive terminals.
	if !flags.Human && !flags.JSON {
		if fileInfo, _ := os.Stdout.Stat(); fileInfo != nil && fileInfo.Mode()&os.ModeCharDevice != 0 {
			flags.Human = true
		}
	}

	// Auto-enable LLM and embeddings when Ollama is detected locally
	// and no explicit provider is configured.
	if !flags.LLM && os.Getenv("MNEMOS_LLM_PROVIDER") == "" && llm.OllamaAvailable() {
		flags.LLM = true
		flags.Embed = true
		printProgress("ollama detected: enabling LLM extraction and embeddings")
	}

	if flags.Help {
		printUsage()
		os.Exit(int(ExitSuccess))
	}
	if flags.Version {
		fmt.Printf("mnemos %s (commit %s, built %s)\n", version, commit, buildDate)
		os.Exit(int(ExitSuccess))
	}

	command := args[0]
	args = args[1:]

	switch command {
	case "setup":
		handleSetup(args, flags)
	case "init":
		handleInit(args, flags)
	case "ingest":
		handleIngest(args, flags)
	case "extract":
		handleExtract(args, flags)
	case "relate":
		handleRelate(args, flags)
	case "process":
		handleProcess(args, flags)
	case "query":
		handleQuery(args, flags)
	case "metrics":
		handleMetrics(args, flags)
	case "hook":
		handleHook(args)
	case "mcp":
		handleMCP(args)
	case "serve":
		handleServe(args, flags)
	case "registry":
		handleRegistry(args, flags)
	case "push":
		handlePush(args, flags)
	case "pull":
		handlePull(args, flags)
	case "audit":
		handleAudit(args, flags)
	case "resolve":
		handleResolve(args, flags)
	case "user":
		handleUser(args, flags)
	case "token":
		handleToken(args, flags)
	case "agent":
		handleAgent(args, flags)
	case "doctor":
		handleDoctor(args, flags)
	case "reset":
		handleReset(args, flags)
	case "delete-claim":
		handleDeleteClaim(args, flags)
	case "delete-event":
		handleDeleteEvent(args, flags)
	case "reembed":
		handleReembed(args, flags)
	case "recompute-trust":
		handleRecomputeTrust(args, flags)
	case "recompute-contested":
		for _, a := range args {
			exitWithMnemosError(flags.Verbose, NewUserError("recompute-contested takes no arguments (got %q)", a))
			return
		}
		recomputeContested(flags.DryRun, flags)
	case "dedup":
		handleDedupe(args, flags)
	case "prune":
		handlePrune(args, flags)
	case "classify-durability":
		handleClassifyDurability(args, flags)
	case "entities":
		handleEntities(args, flags)
	case "extract-entities":
		handleExtractEntities(args, flags)
	case "claim":
		handleClaim(args, flags)
	case "action":
		handleAction(args, flags)
	case "outcome":
		handleOutcome(args, flags)
	case "synthesize":
		handleSynthesize(args, flags)
	case "consolidate":
		handleConsolidate(args, flags)
	case "float-back":
		handleFloatBack(args, flags)
	case "global":
		handleGlobal(args, flags)
	case "sync-docs":
		handleSyncDocs(args, flags)
	case "rebuild":
		handleRebuild(args, flags)
	case "repo-tenant":
		handleRepoTenant(args, flags)
	case "workspace":
		handleWorkspace(args, flags)
	case "lessons":
		handleLessons(args, flags)
	case "verify", "reconsolidate":
		// "reconsolidate" is a brain-vocabulary alias for "verify" (ADR 0011
		// Phase A) — both re-confirm claims/beliefs against fresh evidence.
		handleVerify(args, flags)
	case "trust":
		handleTrust(args, flags)
	case "decision":
		handleDecision(args, flags)
	case "incident":
		handleIncident(args, flags)
	case "playbook":
		handlePlaybook(args, flags)
	case "export":
		handleExport(args, flags)
	case "import":
		handleImport(args, flags)
	case "history":
		handleHistory(args, flags)
	case "quality":
		handleQuality(flags)
	case "curiosity":
		handleCuriosity(args, flags)
	case "predictive-error":
		handlePredictiveError(args, flags)
	case "journal":
		handleJournal(args, flags)
	case "health":
		handleBrainHealth(args, flags)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown command %q\n", command)
		if suggestion := suggestCommand(command); suggestion != "" {
			fmt.Fprintf(os.Stderr, "  Did you mean %q?\n", suggestion)
		}
		fmt.Fprintln(os.Stderr)
		printUsage()
		os.Exit(int(ExitUsage))
	}
}

func handleIngest(args []string, f Flags) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: ingest requires a file path or --text flag")
		fmt.Fprintln(os.Stderr, "  mnemos ingest <path>")
		fmt.Fprintln(os.Stderr, "  mnemos ingest --text <content>")
		os.Exit(int(ExitUsage))
	}

	service := ingest.NewService()
	normalizer := parser.NewNormalizer()

	if args[0] == "--text" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "error: --text flag requires content after it")
			fmt.Fprintln(os.Stderr, "  mnemos ingest --text <content>")
			os.Exit(int(ExitUsage))
		}

		contentArg := strings.Join(args[1:], " ")
		err := runJob("ingest", map[string]string{"source": "raw_text"}, f.Verbose, func(ctx context.Context, job *workflow.Job, w *govwrite.Writer) error {
			conn := w.Conn()
			actor, err := resolveActor(ctx, conn.Users, f.Actor)
			if err != nil {
				return err
			}
			if err := job.SetStatus("loading", ""); err != nil {
				return err
			}
			input, content, err := service.IngestText(contentArg, nil)
			if err != nil {
				return NewSystemError(err, "failed to ingest text")
			}
			if err := job.SetStatus("extracting", ""); err != nil {
				return err
			}
			events, err := normalizer.Normalize(input, content)
			if err != nil {
				return NewSystemError(err, "failed to normalize text")
			}
			for i := range events {
				events[i].RunID = job.ID()
			}
			stampEventActor(events, actor)
			if err := job.SetStatus("saving", ""); err != nil {
				return err
			}
			if _, err := w.Events(ctx, events); err != nil {
				return NewSystemError(err, "failed to persist events")
			}
			fmt.Printf("run_id=%s input=%s type=%s format=%s bytes=%d events=%d db=%s source=%s\n", job.ID(), input.ID, input.Type, input.Format, len(content), len(events), resolveDBPath(), input.Metadata["source"])
			printIngestHint(job.ID())
			return nil
		})
		exitWithMnemosError(f.Verbose, err)
		return
	}

	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "error: ingest accepts exactly one path argument")
		fmt.Fprintln(os.Stderr, "  mnemos ingest <path>")
		os.Exit(int(ExitUsage))
	}

	path := args[0]
	err := runJob("ingest", map[string]string{"path": path}, f.Verbose, func(ctx context.Context, job *workflow.Job, w *govwrite.Writer) error {
		conn := w.Conn()
		actor, err := resolveActor(ctx, conn.Users, f.Actor)
		if err != nil {
			return err
		}
		if err := job.SetStatus("loading", ""); err != nil {
			return err
		}
		input, content, err := service.IngestFile(path)
		if err != nil {
			return NewSystemError(err, "failed to read file %q", path)
		}
		if err := job.SetStatus("extracting", ""); err != nil {
			return err
		}
		events, err := normalizer.Normalize(input, content)
		if err != nil {
			return NewSystemError(err, "failed to normalize content")
		}
		for i := range events {
			events[i].RunID = job.ID()
		}
		stampEventActor(events, actor)
		if err := job.SetStatus("saving", ""); err != nil {
			return err
		}
		if _, err := w.Events(ctx, events); err != nil {
			return NewSystemError(err, "failed to persist events")
		}
		fmt.Printf("run_id=%s input=%s type=%s format=%s bytes=%d events=%d db=%s source=%s\n", job.ID(), input.ID, input.Type, input.Format, len(content), len(events), resolveDBPath(), input.Metadata["source_path"])
		printIngestHint(job.ID())
		return nil
	})
	exitWithMnemosError(f.Verbose, err)
}

func handleQuery(args []string, f Flags) {
	qa, err := parseQueryArgs(args)
	if err != nil {
		exitWithMnemosError(f.Verbose, err)
	}
	question, runID, hops, minTrust := qa.question, qa.runID, qa.hops, qa.minTrust
	asOf, includeHistory, entity := qa.asOf, qa.includeHistory, qa.entity

	scope := map[string]string{"question": question}
	if runID != "" {
		scope["run_id"] = runID
	}
	if hops > 0 {
		scope["hops"] = strconv.Itoa(hops)
	}
	if minTrust > 0 {
		scope["min_trust"] = strconv.FormatFloat(minTrust, 'f', 2, 64)
	}
	if !asOf.IsZero() {
		scope["as_of"] = asOf.UTC().Format(time.RFC3339)
	}
	if includeHistory {
		scope["include_history"] = "true"
	}
	if qa.whyWrong {
		scope["why_wrong"] = "true"
	}

	err = runJob("query", scope, f.Verbose, func(ctx context.Context, job *workflow.Job, w *govwrite.Writer) error {
		conn := w.Conn()
		if err := job.SetStatus("loading", ""); err != nil {
			return err
		}
		eventRepo := conn.Events
		claimRepo := conn.Claims
		engine := query.NewEngine(eventRepo, claimRepo, conn.Relationships).
			WithDecisions(conn.Decisions)
		// WithTextSearch is an optional capability (FTS5 etc); engage
		// it only when both repos satisfy the TextSearcher port.
		if eventSearcher, ok := eventRepo.(ports.TextSearcher); ok {
			if claimSearcher, ok := claimRepo.(ports.TextSearcher); ok {
				engine = engine.WithTextSearch(eventSearcher, claimSearcher)
			}
		}

		if f.Embed {
			printProgress("semantic search: preparing query embeddings")
			embCfg, err := embedding.ConfigFromEnv()
			if err != nil {
				printProgress("warning: --embed requested but embedding config failed: %v (falling back to keyword matching)", err)
			} else {
				embClient, err := embedding.NewClient(embCfg)
				if err != nil {
					printProgress("warning: --embed requested but embedding client failed: %v (falling back to keyword matching)", err)
				} else {
					engine = engine.WithEmbeddings(
						conn.Embeddings,
						embClient,
					)
				}
			}
		}

		if f.LLM {
			llmCfg, err := llm.ConfigFromEnv()
			if err == nil {
				llmClient, err := llm.NewClient(llmCfg)
				if err == nil {
					engine = engine.WithLLM(llmClient)
					printProgress("grounded generation: using %s for answer synthesis", llmCfg.Provider)
				}
			}
		}

		// --why-wrong: audit-trail mode — list decisions refuted by failed outcomes.
		if qa.whyWrong {
			if statusErr := job.SetStatus("querying", ""); statusErr != nil { // "auditing" was never a machine state — see TestJobStatusesAreDeclaredInTheMachine
				return statusErr
			}
			auditOpts := query.AuditTrailOptions{
				Service: qa.scope.Service,
			}
			entries, auditErr := engine.AuditTrail(ctx, auditOpts)
			if auditErr != nil {
				return NewSystemError(auditErr, "audit trail query failed")
			}
			if f.Human {
				printAuditTrail(entries)
			} else {
				encoded, encErr := json.MarshalIndent(entries, "", "  ")
				if encErr != nil {
					return NewSystemError(encErr, "failed to encode audit trail")
				}
				fmt.Println(string(encoded))
			}
			return nil
		}

		// --why-trust <id>: provenance mode — explain how a claim's trust score was computed.
		if qa.whyTrust != "" {
			if statusErr := job.SetStatus("querying", ""); statusErr != nil { // "provenance" was never a machine state — see TestJobStatusesAreDeclaredInTheMachine
				return statusErr
			}
			report, provErr := engine.WhyTrustClaim(ctx, qa.whyTrust)
			if provErr != nil {
				return NewSystemError(provErr, "provenance query failed for claim %q", qa.whyTrust)
			}
			if f.Human {
				printProvenanceReport(report)
			} else {
				encoded, encErr := json.MarshalIndent(report, "", "  ")
				if encErr != nil {
					return NewSystemError(encErr, "failed to encode provenance report")
				}
				fmt.Println(string(encoded))
			}
			return nil
		}

		if statusErr := job.SetStatus("querying", ""); statusErr != nil {
			return statusErr
		}
		var answer domain.Answer
		var queryErr error
		opts := query.AnswerOptions{
			Hops:           hops,
			HopKinds:       qa.hopKinds,
			Scope:          qa.scope,
			MinTrust:       minTrust,
			AsOf:           asOf,
			RecordedAsOf:   qa.recordedAsOf,
			IncludeHistory: includeHistory,
			Visibility:     qa.visibility,
			// Per-query flags force a behaviour ON; the shared defaults supply
			// the rest. Five near-identical env helpers used to live here and
			// nowhere else, which is exactly why no other surface had them.
			Prime:         qa.prime,
			Salient:       qa.salient,
			Hebbian:       qa.hebbian,
			Reconsolidate: qa.reconsolidate,
			Inhibit:       qa.inhibit,
			// Inhibition suppresses a contradiction's beaten LOSER, so something
			// must have adjudicated a winner first — and verdicts are only
			// produced for an agent consumer. Without this, `--inhibit` parsed,
			// set the flag, and did nothing whatsoever.
			//
			// A person reading output otherwise wants the contradiction
			// explained rather than silently resolved: that is ConsumerUser.
			Consumer: consumerFor(qa.inhibit),
		}.WithCognitiveDefaults()
		if entity != "" {
			entRepo := conn.Entities
			ent, ok, rErr := resolveEntity(ctx, entRepo, entity)
			if rErr != nil {
				return NewSystemError(rErr, "resolve entity %q", entity)
			}
			if !ok {
				return NewUserError("no entity matching %q (try `mnemos entities list`)", entity)
			}
			opts.AllowedClaimIDs = make(map[string]struct{})
			ents, eErr := entRepo.ListClaimsForEntity(ctx, ent.ID)
			if eErr != nil {
				return NewSystemError(eErr, "load claims for entity")
			}
			for _, c := range ents {
				opts.AllowedClaimIDs[c.ID] = struct{}{}
			}
		}
		if runID != "" {
			answer, queryErr = engine.AnswerForRunWithOptions(ctx, question, runID, opts)
		} else {
			answer, queryErr = engine.AnswerWithOptions(ctx, question, opts)
		}
		if queryErr != nil {
			return NewSystemError(queryErr, "query engine failed")
		}

		if f.Human {
			// Resolve claim text for contradiction display — some
			// contradictions may reference claims outside the answer set.
			resolveContradictionClaimText(ctx, conn.Claims, &answer)
			printHumanReadableAnswer(question, answer)
		} else {
			response := map[string]any{
				"answer":      answer.AnswerText,
				"beliefs":     answer.Claims,
				"dissonances": answer.Contradictions,
				"timeline":    answer.TimelineEventIDs,
				"confidence":  answer.Confidence,
			}
			encoded, err := json.MarshalIndent(response, "", "  ")
			if err != nil {
				return NewSystemError(err, "failed to encode response")
			}
			fmt.Println(string(encoded))
		}
		return nil
	})
	exitWithMnemosError(f.Verbose, err)
}

// resolveContradictionClaimText ensures all claims referenced in contradictions
// have their text available in the answer's claim set for display purposes.
func resolveContradictionClaimText(ctx context.Context, claimRepo ports.ClaimRepository, answer *domain.Answer) {
	if len(answer.Contradictions) == 0 {
		return
	}

	known := make(map[string]struct{}, len(answer.Claims))
	for _, c := range answer.Claims {
		known[c.ID] = struct{}{}
	}

	hasMissing := false
	for _, rel := range answer.Contradictions {
		if _, ok := known[rel.FromClaimID]; !ok {
			hasMissing = true
			break
		}
		if _, ok := known[rel.ToClaimID]; !ok {
			hasMissing = true
			break
		}
	}
	if !hasMissing {
		return
	}

	allClaims, err := claimRepo.ListAll(ctx)
	if err != nil {
		return
	}
	byID := make(map[string]domain.Claim, len(allClaims))
	for _, c := range allClaims {
		byID[c.ID] = c
	}
	for _, rel := range answer.Contradictions {
		if _, ok := known[rel.FromClaimID]; !ok {
			if c, found := byID[rel.FromClaimID]; found {
				answer.Claims = append(answer.Claims, c)
				known[rel.FromClaimID] = struct{}{}
			}
		}
		if _, ok := known[rel.ToClaimID]; !ok {
			if c, found := byID[rel.ToClaimID]; found {
				answer.Claims = append(answer.Claims, c)
				known[rel.ToClaimID] = struct{}{}
			}
		}
	}
}

// printAuditTrail renders a human-readable decision audit trail produced by
// the --why-wrong flag. Each entry shows the decision statement, its risk
// level, the failed outcome that refuted it, and the belief claim IDs that
// were invalidated.
func printAuditTrail(entries []query.AuditEntry) {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  Decision Audit Trail  (decisions refuted by failed outcomes)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	if len(entries) == 0 {
		fmt.Println("  No refuted decisions found.")
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		return
	}
	for i, e := range entries {
		fmt.Printf("\n  [%d] %s\n", i+1, e.Decision.Statement)
		fmt.Printf("      risk:           %s\n", e.Decision.RiskLevel)
		fmt.Printf("      decided:        %s\n", e.Decision.ChosenAt.UTC().Format("2006-01-02 15:04 UTC"))
		if e.FailedOutcomeID != "" {
			fmt.Printf("      failed outcome: %s\n", e.FailedOutcomeID)
		}
		if len(e.RefutedBeliefs) > 0 {
			fmt.Printf("      refuted beliefs (%d):\n", len(e.RefutedBeliefs))
			for _, b := range e.RefutedBeliefs {
				fmt.Printf("        - %s\n", b)
			}
		}
		if e.Decision.Scope.Service != "" || e.Decision.Scope.Env != "" || e.Decision.Scope.Team != "" {
			fmt.Printf("      scope:          service=%s env=%s team=%s\n",
				e.Decision.Scope.Service, e.Decision.Scope.Env, e.Decision.Scope.Team)
		}
	}
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("  Total refuted decisions: %d\n", len(entries))
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
}

// printProvenanceReport renders a human-readable trust provenance report
// produced by the --why-trust flag. It shows the overall trust score for
// the claim and a ranked breakdown of every signal that contributed to it.
func printProvenanceReport(r domain.ProvenanceReport) {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  Trust Provenance Report")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("  claim:       %s\n", r.ClaimID)
	fmt.Printf("  trust score: %.2f  (%s)\n", r.Score, r.Rationale)
	if len(r.Signals) == 0 {
		fmt.Println("  (no signals)")
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		return
	}
	fmt.Println("\n  signal breakdown (highest contribution first):")
	for _, s := range r.Signals {
		bar := trustBar(s.Contribution)
		fmt.Printf("    %-20s  %s  %+.3f  %s\n", s.Name, bar, s.Contribution, s.Detail)
	}
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
}

// trustBar returns a 10-char ASCII bar proportional to abs(contribution).
// Positive contributions use '█', negative use '░'.
func trustBar(v float64) string {
	const width = 10
	abs := v
	if abs < 0 {
		abs = -abs
	}
	filled := int(abs * float64(width) * 2) // scale: max contrib ~0.55 → full bar
	if filled > width {
		filled = width
	}
	ch := '█'
	if v < 0 {
		ch = '░'
	}
	bar := make([]rune, width)
	for i := range bar {
		if i < filled {
			bar[i] = ch
		} else {
			bar[i] = '·'
		}
	}
	return string(bar)
}

func printHumanReadableAnswer(question string, answer domain.Answer) {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("  Question: %s\n", question)
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("")

	if answer.AnswerText != "" && answer.AnswerText != "No ingested events yet." {
		fmt.Printf("  %s\n\n", answer.AnswerText)
	}

	if len(answer.Claims) > 0 {
		fmt.Println("  Evidence:")
		for i, claim := range answer.Claims {
			typeLabel := "Fact"
			switch claim.Type {
			case domain.ClaimTypeDecision:
				typeLabel = "Decision"
			case domain.ClaimTypeHypothesis:
				typeLabel = "Hypothesis"
			}

			status := ""
			if claim.Status == domain.ClaimStatusContested {
				status = " ⚠️  CONFLICT"
			}

			fmt.Printf("  %d. [%s] %s%s\n", i+1, typeLabel, claim.Text, status)
			// Trust line: only printed when the score has been
			// computed (>0). On older DBs the column is 0 by default
			// until the first `recompute-trust` run.
			if claim.TrustScore > 0 {
				fmt.Printf("        trust=%.2f  conf=%.2f\n", claim.TrustScore, claim.Confidence)
			}
			// Evolution line: surfaced only when temporal data is
			// non-trivial. Includes valid_from when known and
			// "(superseded ...)" when valid_to is set so users
			// browsing --include-history can see the timeline.
			if !claim.ValidFrom.IsZero() || !claim.ValidTo.IsZero() {
				fmt.Printf("        Evolution: %s\n", formatEvolution(claim))
			}
		}
		fmt.Println("")
	}

	if len(answer.Contradictions) > 0 {
		// Build claim text lookup for human-readable contradiction output.
		claimText := make(map[string]string, len(answer.Claims))
		for _, c := range answer.Claims {
			claimText[c.ID] = c.Text
		}

		fmt.Println("  Contradictions detected:")
		for i, rel := range answer.Contradictions {
			if rel.Type == domain.RelationshipTypeContradicts {
				from := claimText[rel.FromClaimID]
				if from == "" {
					from = rel.FromClaimID
				}
				to := claimText[rel.ToClaimID]
				if to == "" {
					to = rel.ToClaimID
				}
				fmt.Printf("  %d. \"%s\" contradicts \"%s\"\n", i+1, from, to)
			}
		}
		fmt.Println("")
	}

	if len(answer.Claims) == 0 && answer.AnswerText == "No ingested events yet." {
		fmt.Println("  No knowledge found yet.")
		fmt.Println("")
		fmt.Println("  Tip: Run 'mnemos process --text <your text>' to add knowledge")
	}
}

func handleExtract(args []string, f Flags) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: extract requires event IDs or --run flag")
		fmt.Fprintln(os.Stderr, "  mnemos extract <event-id> [event-id ...]")
		fmt.Fprintln(os.Stderr, "  mnemos extract --run <run-id>")
		os.Exit(int(ExitUsage))
	}

	// Parse --run flag for run-scoped extraction.
	var runID string
	eventIDs := args
	if args[0] == "--run" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "error: --run flag requires a run ID")
			os.Exit(int(ExitUsage))
		}
		runID = args[1]
		eventIDs = args[2:]
	}

	scope := map[string]string{}
	if runID != "" {
		scope["run_id"] = runID
	} else {
		scope["event_ids"] = strings.Join(eventIDs, ",")
	}

	err := runJob("extract", scope, f.Verbose, func(ctx context.Context, job *workflow.Job, w *govwrite.Writer) error {
		conn := w.Conn()
		actor, actorErr := resolveActor(ctx, conn.Users, f.Actor)
		if actorErr != nil {
			return actorErr
		}
		if err := job.SetStatus("loading", ""); err != nil {
			return err
		}
		eventRepo := conn.Events

		var events []domain.Event
		var err error
		if runID != "" {
			events, err = eventRepo.ListByRunID(ctx, runID)
		} else {
			events, err = eventRepo.ListByIDs(ctx, eventIDs)
		}
		if err != nil {
			return NewSystemError(err, "database lookup failed")
		}
		if len(events) == 0 {
			hint := "Tip: Run 'mnemos ingest <file>' or 'mnemos process --text <text>' first"
			if runID != "" {
				return &MnemosError{Code: ExitNotFound, Message: fmt.Sprintf("no events found for run %q", runID), Hint: hint}
			}
			return &MnemosError{Code: ExitNotFound, Message: fmt.Sprintf("no events found for the provided IDs (%d given)", len(eventIDs)), Hint: hint}
		}

		if err := job.SetStatus("extracting", ""); err != nil {
			return err
		}
		if f.LLM {
			printProgress("llm extraction: sending content to %s", os.Getenv("MNEMOS_LLM_PROVIDER"))
		}
		ext, err := newExtractor(f.LLM)
		if err != nil {
			return err
		}
		claims, links, entities, err := ext.ExtractFn(ctx, events)
		if err != nil {
			return NewSystemError(err, "extraction failed")
		}
		if f.LLM {
			printProgress("llm extraction: extracted %d claims", len(claims))
		}

		if err := job.SetStatus("saving", ""); err != nil {
			return err
		}
		stampClaimActor(claims, actor)
		// Stamping CreatedBy and then discarding the reason left no
		// status_history row for CLI ingest.
		if _, err := w.Claims(ctx, claims, govwrite.ClaimReason{
			Reason:    "cli: ingest",
			ChangedBy: actor,
		}); err != nil {
			return NewSystemError(err, "failed to persist claims")
		}
		if _, err := w.EvidenceLinks(ctx, links); err != nil {
			return NewSystemError(err, "failed to persist claim evidence links")
		}

		// Materialise entities from the LLM tags. Same non-fatal
		// treatment as the process command — claims are persisted;
		// entities are an enrichment.
		if n, entErr := pipeline.MaterializeEntities(ctx, conn, entities, actor); entErr != nil {
			warn := icon("⚠️", "(!)")
			fmt.Fprintf(os.Stderr, "  %s entity materialisation failed at link %d: %v\n", warn, n, entErr)
		} else if n > 0 {
			printProgress("entities: linked %d entity reference(s)", n)
		}

		fmt.Printf("events=%d claims=%d evidence_links=%d db=%s\n", len(events), len(claims), len(links), resolveDBPath())
		return nil
	})
	exitWithMnemosError(f.Verbose, err)
}

func handleRelate(args []string, f Flags) {
	// --prune-stale re-derives existing edges against the current detectors and
	// drops what they no longer produce. Relationships are derived state, but
	// nothing re-derived them when a detector changed, so brains accumulate
	// residue from every superseded heuristic.
	for i, a := range args {
		if a == "--prune-stale" {
			args = append(args[:i], args[i+1:]...)
			if len(args) > 0 {
				exitWithMnemosError(f.Verbose, NewUserError("relate --prune-stale takes no event ids (got %q)", args[0]))
				return
			}
			pruneStaleRelationships(f.DryRun, f)
			return
		}
	}
	err := runJob("relate", map[string]string{"event_ids": strings.Join(args, ",")}, f.Verbose, func(ctx context.Context, job *workflow.Job, w *govwrite.Writer) error {
		conn := w.Conn()
		actor, actorErr := resolveActor(ctx, conn.Users, f.Actor)
		if actorErr != nil {
			return actorErr
		}
		if err := job.SetStatus("loading", ""); err != nil {
			return err
		}
		claimRepo := conn.Claims

		var claims []domain.Claim
		var err error
		if len(args) == 0 {
			claims, err = claimRepo.ListAll(ctx)
		} else {
			claims, err = claimRepo.ListByEventIDs(ctx, args)
		}
		if err != nil {
			return NewSystemError(err, "database lookup failed")
		}
		if len(claims) < 2 {
			return &MnemosError{
				Code:    ExitUsage,
				Message: fmt.Sprintf("need at least 2 claims to detect relationships (found %d)", len(claims)),
				Hint:    "Tip: Run 'mnemos ingest' followed by 'mnemos extract' to add more claims",
			}
		}

		if err := job.SetStatus("relating", ""); err != nil {
			return err
		}
		engine := relate.NewEngine()
		rels, err := engine.Detect(claims)
		if err != nil {
			return NewSystemError(err, "relationship detection failed")
		}
		if err := job.SetStatus("saving", ""); err != nil {
			return err
		}
		stampRelationshipActor(rels, actor)
		if _, err := w.Relationships(ctx, rels); err != nil {
			return NewSystemError(err, "failed to persist relationships")
		}

		fmt.Printf("claims=%d relationships=%d db=%s\n", len(claims), len(rels), resolveDBPath())
		return nil
	})
	exitWithMnemosError(f.Verbose, err)
}

func handleProcess(args []string, f Flags) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: process requires a file path or --text flag")
		fmt.Fprintln(os.Stderr, "  mnemos process <path>")
		fmt.Fprintln(os.Stderr, "  mnemos process --text <content>")
		os.Exit(int(ExitUsage))
	}

	service := ingest.NewService()
	normalizer := parser.NewNormalizer()

	scope := map[string]string{"source": "file"}
	if args[0] == "--text" {
		scope = map[string]string{"source": "raw_text"}
	}

	err := runJob("process", scope, f.Verbose, func(ctx context.Context, job *workflow.Job, w *govwrite.Writer) error {
		conn := w.Conn()
		actor, actorErr := resolveActor(ctx, conn.Users, f.Actor)
		if actorErr != nil {
			return actorErr
		}
		if err := job.SetStatus("loading", ""); err != nil {
			return err
		}

		var (
			input   domain.Input
			content string
			err     error
		)

		if args[0] == "--text" {
			if len(args) < 2 {
				return NewUserError("--text flag requires content after it")
			}
			input, content, err = service.IngestText(strings.Join(args[1:], " "), nil)
		} else {
			if len(args) != 1 {
				return NewUserError("process accepts exactly one path argument")
			}
			input, content, err = service.IngestFile(args[0])
		}
		if err != nil {
			return NewSystemError(err, "failed to read input")
		}

		if err := job.SetStatus("extracting", ""); err != nil {
			return err
		}
		events, err := normalizer.Normalize(input, content)
		if err != nil {
			return NewSystemError(err, "normalization failed")
		}
		for i := range events {
			events[i].RunID = job.ID()
		}

		ext, err := newExtractor(f.LLM)
		if err != nil {
			return err
		}
		if f.LLM {
			printProgress("llm extraction: sending content to %s", os.Getenv("MNEMOS_LLM_PROVIDER"))
		}
		claims, links, entities, err := ext.ExtractFn(ctx, events)
		if err != nil {
			return NewSystemError(err, "claim extraction failed")
		}
		if f.LLM {
			printProgress("llm extraction: extracted %d claims", len(claims))
		}

		// Load the existing knowledge once: it feeds both cross-batch
		// dedup (always) and incremental relate (unless --no-relate).
		// Failure here is non-fatal — we still persist what we have.
		var existingClaims []domain.Claim
		{
			claimRepo := conn.Claims
			loadCtx, loadCancel := context.WithTimeout(ctx, 30*time.Second)
			loaded, loadErr := claimRepo.ListAll(loadCtx)
			loadCancel()
			if loadErr != nil {
				warnRelateSkipped(loadErr, "loading existing claims")
			} else {
				existingClaims = loaded
			}
		}

		// Cross-batch dedup: collapse new claims that already exist by
		// normalized text, rewriting evidence links to point at the
		// canonical (existing) claim id. Without this, restating the
		// same fact across chunks produces near-duplicate claim rows
		// (#24).
		preDedup := len(claims)
		claims, links = pipeline.DedupeAgainstExisting(claims, links, existingClaims)
		if dropped := preDedup - len(claims); dropped > 0 {
			printProgress("dedup: collapsed %d claim(s) that match existing knowledge", dropped)
		}

		if err := job.SetStatus("relating", ""); err != nil {
			return err
		}
		relEngine := relate.NewEngine()
		rels, err := relEngine.Detect(claims)
		if err != nil {
			return NewSystemError(err, "relationship detection failed")
		}

		// Incremental relate: compare new claims against the existing
		// store. Skipped under --no-relate. Already-loaded claims feed
		// straight in, so no second DB hit.
		if !f.NoRelate && len(existingClaims) > 0 && len(claims) > 0 {
			incrementalRels, incErr := relEngine.DetectIncremental(claims, existingClaims)
			if incErr != nil {
				warnRelateSkipped(incErr, "comparing against existing claims")
			} else {
				rels = append(rels, incrementalRels...)
			}
		}

		if err := job.SetStatus("saving", ""); err != nil {
			return err
		}
		stampEventActor(events, actor)
		stampClaimActor(claims, actor)
		stampRelationshipActor(rels, actor)
		if _, err := w.Artifacts(ctx, events, claims, links, rels); err != nil {
			return err
		}

		// Materialise entities the LLM tagged on each claim. Failure
		// here is non-fatal — claims persist; a future
		// `mnemos extract-entities` can backfill any that didn't
		// land. Surfaced as a warning so the operator knows.
		if n, entErr := pipeline.MaterializeEntities(ctx, conn, entities, actor); entErr != nil {
			warn := icon("⚠️", "(!)")
			fmt.Fprintf(os.Stderr, "\n  %s Entity materialisation failed at link %d: %v\n", warn, n, entErr)
		} else if n > 0 {
			printProgress("entities: linked %d entity reference(s) across %d claim(s)", n, len(entities))
		}

		if f.Embed {
			if err := job.SetStatus("embedding", ""); err != nil {
				return err
			}
			printProgress("embedding: generating vectors with %s", os.Getenv("MNEMOS_EMBED_PROVIDER"))
			if n, err := pipeline.GenerateEmbeddings(ctx, conn, events); err != nil {
				// Embedding failure is non-fatal but should be prominent since --embed was explicit.
				warn := icon("⚠️", "(!)")
				fmt.Fprintf(os.Stderr, "\n  %s Event embedding failed: %v\n", warn, err)
				fmt.Fprintf(os.Stderr, "  Queries will fall back to keyword matching instead of semantic search.\n")
				fmt.Fprintf(os.Stderr, "  Check MNEMOS_EMBED_PROVIDER and MNEMOS_EMBED_API_KEY.\n\n")
			} else {
				printProgress("embedding: generated %d event vectors", n)
			}

			if nc, err := pipeline.GenerateClaimEmbeddings(ctx, conn, claims); err != nil {
				warn := icon("⚠️", "(!)")
				fmt.Fprintf(os.Stderr, "\n  %s Claim embedding failed: %v\n", warn, err)
			} else {
				printProgress("embedding: generated %d claim vectors", nc)
			}
		}

		fmt.Printf("Run ID: %s\n", job.ID())
		fmt.Printf("Processed %d event(s) into %d claim(s).\n", len(events), len(claims))

		printExtractionSummary(claims, rels)
		if len(claims) > 0 {
			printClaimPreview(claims, 3)
		}

		return nil
	})
	exitWithMnemosError(f.Verbose, err)
}

func handleQuality(f Flags) {
	err := runJob("quality", map[string]string{}, f.Verbose, func(ctx context.Context, job *workflow.Job, w *govwrite.Writer) error {
		conn := w.Conn()
		// "querying", not "computing". The workflow machine has no `computing`
		// state, so this call failed on every single invocation — `mnemos quality`
		// has never run for anyone, exiting with
		// "invalid workflow status transition: running -> computing" rather than
		// anything that names the command or suggests a fix.
		//
		// `querying` is the right existing state, not merely an available one:
		// this job's work is a read-side aggregate over the query engine, and
		// `running -> querying` is already a declared transition. Every other
		// command uses the machine's established vocabulary (loading, extracting,
		// relating, saving, embedding); this was the one site that invented a
		// status, and inventing one is what broke it.
		if err := job.SetStatus("querying", ""); err != nil {
			return err
		}

		engine := query.NewEngine(conn.Events, conn.Claims, conn.Relationships).
			WithDecisions(conn.Decisions).
			WithIncidents(conn.Incidents)

		metrics, err := engine.ComputeMemoryQuality(ctx)
		if err != nil {
			return fmt.Errorf("compute memory quality: %w", err)
		}

		response := map[string]any{
			"total_claims":        metrics.TotalClaims,
			"avg_trust_score":     metrics.AvgTrustScore,
			"avg_confidence":      metrics.AvgConfidence,
			"stale_count":         metrics.StaleCount,
			"contested_count":     metrics.ContestedCount,
			"contradiction_count": metrics.ContradictionCount,
			"avg_citation_count":  metrics.AvgCitationCount,
		}
		encoded, err := json.MarshalIndent(response, "", "  ")
		if err != nil {
			return fmt.Errorf("encode quality metrics: %w", err)
		}
		fmt.Println(string(encoded))
		return nil
	})
	exitWithMnemosError(f.Verbose, err)
}

func handleMetrics(args []string, f Flags) {
	var workspace, optIn, optOut, send bool
	for _, a := range args {
		switch a {
		case "--workspace":
			workspace = true
		case "--telemetry-opt-in":
			optIn = true
		case "--telemetry-opt-out":
			optOut = true
		case "--telemetry-send":
			send = true
		default:
			exitWithMnemosError(false, NewUserError("unknown argument %q", a))
			return
		}
	}
	if optIn && optOut {
		exitWithMnemosError(false, NewUserError("--telemetry-opt-in and --telemetry-opt-out are mutually exclusive"))
		return
	}
	if optIn {
		if err := setTelemetryOptIn(true); err != nil {
			exitWithMnemosError(false, NewSystemError(err, "set telemetry opt-in"))
			return
		}
		fmt.Fprintln(os.Stderr, "telemetry: opted in. nothing is sent until MNEMOS_TELEMETRY_ENDPOINT is set and `mnemos metrics --workspace --telemetry-send` is run.")
	}
	if optOut {
		if err := setTelemetryOptIn(false); err != nil {
			exitWithMnemosError(false, NewSystemError(err, "remove telemetry opt-in"))
			return
		}
		fmt.Fprintln(os.Stderr, "telemetry: opted out. no payload will be sent.")
	}
	if workspace {
		handleWorkspaceMetrics(send, f)
		return
	}
	err := runJob("metrics", map[string]string{}, f.Verbose, func(ctx context.Context, job *workflow.Job, w *govwrite.Writer) error {
		conn := w.Conn()
		if err := job.SetStatus("loading", ""); err != nil {
			return err
		}

		// Trust stats: 0.5 is the floor for "low-trust" — under that
		// the claim is failing on at least one of confidence,
		// corroboration, or freshness. Tunable via the constant
		// internal/trust.LowTrustThreshold if it ever wants a knob.
		const lowTrustThreshold = 0.5
		var avgTrust float64
		var lowTrust int64
		if scorer, ok := conn.Claims.(ports.TrustScorer); ok {
			avgTrust, _ = scorer.AverageTrust(ctx)
			lowTrust, _ = scorer.CountClaimsBelowTrust(ctx, lowTrustThreshold)
		}
		entityCount, _ := conn.Entities.Count(ctx)

		// Distinct run-ids and contested counts are computed in
		// memory from ListAll instead of bespoke ports — the metrics
		// surface doesn't justify a CountDistinctRunID +
		// CountByStatus port pair.
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

		allClaims, _ := conn.Claims.ListAll(ctx)
		var contestedCount int64
		for _, c := range allClaims {
			if string(c.Status) == "contested" {
				contestedCount++
			}
		}

		metrics := map[string]any{
			"runs":                int64(len(runIDs)),
			"events":              eventsCount,
			"claims":              claimsCount,
			"contested_claims":    contestedCount,
			"relationships":       relsCount,
			"contradictions":      contradictionsCount,
			"embeddings":          embeddingsCount,
			"avg_trust":           roundTo(avgTrust, 3),
			"low_trust_count":     lowTrust,
			"low_trust_threshold": lowTrustThreshold,
			"entities":            entityCount,
			"llm_cache_entries":   cacheEntryCount(),
			"prompt_version":      extract.PromptVersion,
			// eval_cases is the static count across data/eval/*.yaml.
			// Update when suites are added/removed (last counted 2026-05).
			"eval_cases":          133,
			"llm_eval_configured": strings.TrimSpace(os.Getenv("MNEMOS_LLM_PROVIDER")) != "",
		}

		if f.Human {
			fmt.Println("Mnemos Metrics")
			fmt.Printf("Runs: %v\n", metrics["runs"])
			fmt.Printf("Events: %v\n", metrics["events"])
			fmt.Printf("Claims: %v\n", metrics["claims"])
			fmt.Printf("Contested claims: %v\n", metrics["contested_claims"])
			fmt.Printf("Relationships: %v\n", metrics["relationships"])
			fmt.Printf("Contradictions: %v\n", metrics["contradictions"])
			fmt.Printf("Embeddings: %v\n", metrics["embeddings"])
			fmt.Printf("Avg trust: %.3f\n", avgTrust)
			fmt.Printf("Low-trust claims (< %.2f): %v\n", lowTrustThreshold, lowTrust)
			fmt.Printf("Entities: %v\n", entityCount)
			fmt.Printf("LLM cache entries: %v\n", metrics["llm_cache_entries"])
			fmt.Printf("Eval cases: %v\n", metrics["eval_cases"])
			fmt.Printf("Prompt version: %v\n", metrics["prompt_version"])
			return nil
		}

		encoded, err := json.MarshalIndent(metrics, "", "  ")
		if err != nil {
			return NewSystemError(err, "failed to encode metrics")
		}
		fmt.Println(string(encoded))
		return nil
	})
	exitWithMnemosError(f.Verbose, err)
}

// roundTo trims a float to n decimal places. Used for metrics so the
// JSON output isn't a floating-point dust trail.
func roundTo(f float64, n int) float64 {
	shift := 1.0
	for i := 0; i < n; i++ {
		shift *= 10
	}
	return float64(int64(f*shift+0.5)) / shift
}

func cacheEntryCount() int {
	entries, err := os.ReadDir(filepath.Join("data", "cache", "llm-extraction"))
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	return count
}

func printUsage() {
	fmt.Println("Mnemos CLI — local-first knowledge engine")
	fmt.Println("")
	fmt.Println("Quick Start:")
	fmt.Println("  mnemos init                          Detect your system and wire Mnemos into Claude Code (one command)")
	fmt.Println("  mnemos process --text \"Your text here\"")
	fmt.Println("  mnemos query --human \"Your question\"")
	fmt.Println("")
	fmt.Println("Setup:")
	fmt.Println("  init                                 Central brain: brain + config + MCP server + auto hooks (recall/brief/capture)")
	fmt.Println("  init --dry-run                       Preview everything init would write; change nothing")
	fmt.Println("  init --yes                           Apply without the confirmation prompt")
	fmt.Println("  init --project                       Scope the brain + hooks to this project instead of global")
	fmt.Println("  init --no-hooks                      Register the MCP server + config without installing hooks")
	fmt.Println("  init --no-skills                     Skip the on-demand /mnemos-brief and /mnemos-capture skills")
	fmt.Println("  init --hooks recall,capture          Install only the named hooks (of recall,brief,capture)")
	fmt.Println("  init --db <dsn>                      Use a specific backend (sqlite/postgres/mysql/libsql)")
	fmt.Println("  init --url <url> [--token <jwt>]     Connect Claude Code to a hosted mnemos (HTTP MCP endpoint)")
	fmt.Println("  init --service [--out <dir>]         Scaffold a hosted `mnemos serve` deployment (compose+config)")
	fmt.Println("  setup                                Minimal: register only the MCP server (no hooks/config)")
	fmt.Println("  doctor [--json]                      Diagnose brain/providers/config; exits non-zero if a check fails")
	fmt.Println("")
	fmt.Println("Serving:")
	fmt.Println("  mcp                                  MCP server over stdio (default, for local Claude Code)")
	fmt.Println("  mcp --http <addr> [--auth]           MCP server over HTTP for remote clients (JWT auth by default)")
	fmt.Println("  mcp --http <addr> --require-tenant   Multi-tenant: every request needs a token with a tenant (postgres, RLS-isolated)")
	fmt.Println("  serve [--port N] [--grpc-port N]     REST + gRPC API service (default :7777)")
	fmt.Println("  serve --require-tenant               Multi-tenant REST + gRPC: every request needs a token with a tenant (postgres, RLS)")
	fmt.Println("  serve --public-reads                 Insecure/local: anonymous GET reads (default: reads require a token). Not for hosted/production.")
	fmt.Println("  serve --metrics-public               Trusted-network scrape: anonymous /internal/metrics (default: authenticated). /health stays a bare-liveness 200.")
	fmt.Println("  repo-tenant [--json]                 Print this repo's derived hosted tenant id (ADR 0009), for `token issue --tenant`")
	fmt.Println("")
	fmt.Println("Pipeline Commands:")
	fmt.Println("  ingest <path>                        Ingest a file as events")
	fmt.Println("  ingest --text <content>              Ingest raw text as events")
	fmt.Println("  extract <event-id> [event-id ...]    Extract claims from events")
	fmt.Println("  extract --run <run-id>               Extract claims from all events in a run")
	fmt.Println("  relate [event-id ...]                Detect relationships between claims\n  relate --prune-stale [--dry-run]     Drop stored edges the current detectors no longer produce")
	fmt.Println("")
	fmt.Println("All-in-One:")
	fmt.Println("  process <path>                       Ingest + extract + relate in one step")
	fmt.Println("  process --text <content>             Same, from raw text")
	fmt.Println("  process --llm <path>                 Use LLM-powered extraction")
	fmt.Println("  process --llm --embed <path>         LLM extraction + embeddings")
	fmt.Println("  claim record --text <t> [--type T]   Record a single claim directly (fact/decision/hypothesis)")
	fmt.Println("  claim record --global --text <t>     Record a claim tagged to always float up to the central brain")
	fmt.Println("")
	fmt.Println("Query & Reporting:")
	fmt.Println("  query [--run <run-id>] <question>    Query with evidence")
	fmt.Println("  query --hops <N> <question>          Expand result claims via N hops of supports/contradicts")
	fmt.Println("  query --hops <N> --kind <list>       Restrict hop expansion to comma-separated edge kinds")
	fmt.Println("  query --prime <question>             Prime associated beliefs via spreading activation (ADR 0013 §2)")
	fmt.Println("  query --salient <question>           Bias recall toward high-stakes beliefs (ADR 0013 §4)")
	fmt.Println("  query --hebbian <question>           Strengthen associations among co-retrieved beliefs (ADR 0015 §4)")
	fmt.Println("  query --reconsolidate <question>     Refresh liveness of recalled beliefs — the testing effect (ADR 0015 §5)")
	fmt.Println("  query --inhibit <question>           Suppress retrievability of beaten contradiction losers (ADR 0016)")
	fmt.Println("                                          (e.g. causes,validates,refutes)")
	fmt.Println("  query --llm <question>               Query with LLM-grounded answer")
	fmt.Println("  query --min-trust X <question>       Only return claims with trust_score >= X (X in [0, 1])")
	fmt.Println("  query --at YYYY-MM-DD <question>     Point-in-time query against the temporal-validity layer")
	fmt.Println("  query --recorded-at YYYY-MM-DD <q>   Point-in-time query against the ingestion-time layer")
	fmt.Println("  query --include-history <question>   Include superseded claims (off by default)")
	fmt.Println("  query --entity <name|id> <question>  Restrict the answer to claims linked to this entity")
	fmt.Println("  entities list [--type T]             List canonicalised entities (people/orgs/projects/...)")
	fmt.Println("  entities show <name|id>              Show one entity and the claims linked to it")
	fmt.Println("  entities merge <winner> <loser>      Collapse one entity into another (manual canonicalisation)")
	fmt.Println("  extract-entities [--all]             Backfill entity links over claims that lack them")
	fmt.Println("  metrics [--human]                    Knowledge base statistics")
	fmt.Println("  quality                              Memory-quality metrics (trust, staleness, contested, contradictions)")
	fmt.Println("  curiosity [--limit N] [--human]      What to learn/verify next: gap-driven acquisition queue (ADR 0013)")
	fmt.Println("  predictive-error [--human]           Hierarchical prediction error — where the model is most wrong (ADR 0017)")
	fmt.Println("  journal [--belief <id>] [--human]    Read the cognitive journal: what learning did over time (ADR 0018)")
	fmt.Println("  health [--human] [--explain] [--journal]  Brain health: vitals + integrity checks, one verdict (--explain describes each)")
	fmt.Println("  audit [--include-embeddings]         Export the full knowledge base as JSON")
	fmt.Println("")
	fmt.Println("Decisions, Actions & Outcomes:")
	fmt.Println("  decision record --statement <s> ...  Record an agent decision (plan/reasoning/risk/beliefs)")
	fmt.Println("  decision list|show|attach-outcome    List, show, or attach an outcome to a decision")
	fmt.Println("  action record --kind <k> [...]       Record an operational action")
	fmt.Println("  action list [--subject S] [--run R]  List recorded actions")
	fmt.Println("  outcome record --action <id> [...]   Record the observed outcome of an action")
	fmt.Println("  outcome list [--action <id>]         List recorded outcomes")
	fmt.Println("  incident open|close|show|list        Track incidents linked to root-cause claims")
	fmt.Println("")
	fmt.Println("Synthesis (lessons & playbooks):")
	fmt.Println("  synthesize [--min-corroboration N]   Cluster action->outcome chains into lessons")
	fmt.Println("  lessons list [--service S|--trigger T] List synthesised lessons")
	fmt.Println("  playbook synthesize                  Cluster lessons by trigger into agent-ready playbooks")
	fmt.Println("  playbook list | show <id>            List playbooks, or show one")
	fmt.Println("  playbook <trigger>                   Look up the best playbook for a trigger")
	fmt.Println("  export --kind lesson|playbook        Export a lesson/playbook to YAML-frontmatter markdown")
	fmt.Println("  import <file.md>                     Import a lesson/playbook from markdown")
	fmt.Println("  history --kind lesson|playbook       Show the version history of a lesson/playbook")
	fmt.Println("")
	fmt.Println("Truth Maintenance:")
	fmt.Println("  resolve <winner> --over <loser>      Resolve a contradiction: winner -> resolved, loser -> deprecated")
	fmt.Println("  resolve <new> --supersedes <old>     Temporal supersession: close old.valid_to at new.valid_from")
	fmt.Println("  trust --test <requirement-ref>       Rank test_result claims for a requirement and pick a winner")
	fmt.Println("  verify <claim-id> [--half-life-days N] Mark a claim re-verified (refreshes freshness/trust)")
	fmt.Println("")
	fmt.Println("Identity:")
	fmt.Println("  user create --name <n> --email <e>   Create a user identity")
	fmt.Println("  user list                            List users")
	fmt.Println("  user revoke <id>                     Revoke a user (soft delete)")
	fmt.Println("  token issue --user <id> [--ttl <d>]  Mint a JWT for a user (default ttl 90 days; --tenant T, repeat")
	fmt.Println("                                       or --tenants A,B for a tenant allowlist — ADR 0009)")
	fmt.Println("  token revoke <jti>                   Add a JWT's jti to the denylist")
	fmt.Println("  agent create --name <n>              Create an agent identity")
	fmt.Println("  agent list | revoke <id>             List agents, or revoke one")
	fmt.Println("  agent token issue --agent <id>       Mint a JWT for an agent (--tenant T to scope)")
	fmt.Println("  agent heal [--json]                  Diagnose + repair an agent's belief set")
	fmt.Println("")
	fmt.Println("Registry Sync:")
	fmt.Println("  registry connect <url> [--token T]   Wire this project to a remote registry")
	fmt.Println("  push [--url U] [--token T]           Send local knowledge to the registry")
	fmt.Println("  pull [--url U] [--token T]           Fetch knowledge from the registry")
	fmt.Println("")
	fmt.Println("Maintenance:")
	fmt.Println("  reset [--keep-events] [--yes]        Wipe claims/relationships/embeddings (events optional)")
	fmt.Println("  delete-claim <id> [<id>...] [--yes]  Delete specific claims and their derived state")
	fmt.Println("  delete-event <id> [<id>...] [--yes]  Delete events and cascade to derived claims")
	fmt.Println("  reembed [--force] [--dry-run]        (Re)generate claim + event embeddings under the current embed config")
	fmt.Println("  recompute-trust [--all]              Rebuild trust_score for every claim under the current policy")
	fmt.Println("  recompute-contested [--dry-run]      Clear contested status the current heuristic no longer assigns")
	fmt.Println("  dedup [--threshold T] [--force]      Merge near-duplicate claims by embedding similarity (dry-run by default)")
	fmt.Println("  prune --narration [--dry-run]        Deprecate stored conversational pollution the extraction filter now catches")
	fmt.Println("  prune --session-noise [--dry-run]    Drop contradiction edges where an LLM judges both claims session-local (edges only)")
	fmt.Println("  classify-durability [--limit N]      Record whether a belief outlives its session; targets the highest-trust unclassified ones")
	fmt.Println("  consolidate [--dry-run] [--forget-below-trust T]  The cognitive \"sleep\" pass: dedupe + refresh trust,")
	fmt.Println("    [--forget-refuted] [--synthesize]  and optionally forget/reinforce/synthesize/replay. Deterministic.")
	fmt.Println("    [--clear-session-noise]            also classify durability + drop narration contradictions (needs an LLM).")
	fmt.Println("  sync-docs [--claude] [--file <name>] Write this repo's learnings into AGENTS.md (or CLAUDE.md) so")
	fmt.Println("                                       agents follow them natively (mnemos-managed block)")
	fmt.Println("  rebuild [--claude] [--file <name>]   Rebuild the gitignored repo brain from a committed AGENTS.md")
	fmt.Println("                                       (run once after cloning a repo that has an AGENTS.md block)")
	fmt.Println("  workspace create <name> --folder <d> Define a named workspace over 1+ folders (Cowork-style — ADR 0010);")
	fmt.Println("  workspace list | remove <name>       sessions in those folders federate global ∪ this workspace")
	fmt.Println("  workspace use <name> | use --none    Pin a workspace regardless of cwd (or clear the pin)")
	fmt.Println("  workspace export <name> [--out <f>]  Emit a shareable workspace definition; import recreates it")
	fmt.Println("  workspace import <file> [--folder <d>] on another machine (--folder/--db adjust local paths)")
	fmt.Println("  float-back [--from <path|dsn>]        Float important repo/workspace learnings up into the central brain")
	fmt.Println("    [--to <dsn>] [--min-trust X] [--apply]  (dry-run by default; --global-tagged claims float unconditionally)")
	fmt.Println("                                       (set MNEMOS_FLOATBACK_ON_CAPTURE=true to also float up at session-end capture)")
	fmt.Println("  global author --statement <text>     Author curated reference knowledge straight into the global brain")
	fmt.Println("    [--scope-service S --scope-env E --scope-team T] [--polarity positive|negative] [--status active|pending]")
	fmt.Println("    [--token <jwt>] [--global-dsn <dsn>] [--apply]  (born-global, ADR 0012; needs a promote:global curator token)")
	fmt.Println("")
	fmt.Println("Flags:")
	fmt.Println("  -h, --help     show this help message")
	fmt.Println("  --version      print version and exit")
	fmt.Println("  -v, --verbose  show detailed error output")
	fmt.Println("  --human        human-readable output (default: JSON)")
	fmt.Println("  --json         force JSON output (default for non-query commands)")
	fmt.Println("  --llm          use LLM-powered extraction (requires MNEMOS_LLM_PROVIDER)")
	fmt.Println("  --embed        generate embeddings for semantic search")
	fmt.Println("  --no-relate    skip the relate stage in 'process' (faster ingest, no cross-claim edges)")
	fmt.Println("  --force        with reembed/dedup: actually apply (default is dry-run)")
	fmt.Println("  --dry-run      report what would change without writing")
	fmt.Println("  --min-trust X  query: only return claims with trust_score ≥ X (X in [0, 1])")
	fmt.Println("  -y, --yes      skip the confirmation prompt (reset, delete-claim, delete-event)")
	fmt.Println("  --config PATH  YAML config file (else MNEMOS_CONFIG, then .mnemos/mnemos.yaml, then ~/.config/mnemos/config.yaml)")
	fmt.Println("  --db DSN       storage DSN for this command (overrides MNEMOS_DB_URL / config; any registered backend)")
	fmt.Println("")
	fmt.Println("Configuration:")
	fmt.Println("  Settings come from the environment and/or a YAML config file.")
	fmt.Println("  An exported MNEMOS_* variable always overrides the file. See")
	fmt.Println("  mnemos.example.yaml for every supported key.")
	fmt.Println("")
	fmt.Println("Environment Variables:")
	fmt.Println("  MNEMOS_DB_URL          full storage DSN (any registered backend)")
	fmt.Println("                         examples: sqlite:///var/lib/mnemos/mnemos.db   memory://")
	fmt.Println("                         postgres://...   mysql://...   libsql://...")
	fmt.Println("                         when unset: ./.mnemos/mnemos.db (walked up) → ~/.local/share/mnemos/mnemos.db")
	fmt.Println("  MNEMOS_LLM_PROVIDER    anthropic, openai, gemini, ollama, openai-compat")
	fmt.Println("  MNEMOS_LLM_API_KEY     API key (required for cloud providers)")
	fmt.Println("  MNEMOS_LLM_MODEL       model override (optional)")
	fmt.Println("  MNEMOS_LLM_BASE_URL    custom endpoint")
	fmt.Println("                         - required for openai-compat")
	fmt.Println("                         - required for ollama when not on the same host as Mnemos")
	fmt.Println("                           (e.g. Mnemos in a container, Ollama on the host:")
	fmt.Println("                            set http://host.docker.internal:11434 on Docker Desktop)")
	fmt.Println("                         - defaults to http://localhost:11434 for ollama")
	fmt.Println("  MNEMOS_LLM_TIMEOUT     per-request LLM HTTP timeout (default 120s; e.g. 60s, 5m)")
	fmt.Println("  MNEMOS_EXTRACT_MODEL   override MNEMOS_LLM_MODEL just for the extract stage")
	fmt.Println("  MNEMOS_JOB_TIMEOUT     overall job deadline (default 10m; raise for slow local LLMs)")
	fmt.Println("  MNEMOS_EMBED_PROVIDER  embedding provider (falls back to LLM provider)")
	fmt.Println("  MNEMOS_EMBED_API_KEY   embedding API key (falls back to LLM key)")
	fmt.Println("  MNEMOS_EMBED_MODEL     embedding model override (optional)")
	fmt.Println("  MNEMOS_EMBED_BASE_URL  embedding endpoint (optional; same container note as MNEMOS_LLM_BASE_URL)")
	fmt.Println("  MNEMOS_EMBED_TIMEOUT   per-request embedding HTTP timeout (default 60s)")
}

// queryArgs bundles the parsed --flag values for `mnemos query`.
// Returned as a struct so adding the next flag doesn't churn every
// caller's signature.
type queryArgs struct {
	question       string
	runID          string
	hops           int
	minTrust       float64
	asOf           time.Time
	recordedAsOf   time.Time
	includeHistory bool
	entity         string // filter answer to claims linked to this entity (id or name)
	hopKinds       []domain.RelationshipType
	scope          domain.Scope
	// whyWrong, when true, switches the query to audit-trail mode: instead
	// of answering a question the engine returns decisions that were refuted
	// by a failed outcome. Use --service to scope to one service.
	whyWrong bool
	// whyTrust, when non-empty, switches the query to provenance mode: the
	// engine returns a structured ProvenanceReport for the given claim ID
	// explaining how its trust score was computed.
	whyTrust string
	// visibility controls workspace isolation. personal/team/org.
	// Zero value treated as "team" (see AnswerOptions.Visibility).
	visibility domain.Visibility
	// prime enables spreading-activation priming at retrieval (ADR 0013 §2):
	// after the initial ranked set is assembled, activation is spread over
	// relationship edges from the top hits and blended into the ranking so
	// strongly-associated beliefs are primed up. Off by default; set by
	// --prime or a truthy MNEMOS_SPREADING_ACTIVATION.
	prime bool
	// hebbian enables the Hebbian co-activation write-back (ADR 0015 §4): after the
	// answer resolves, existing edges among the co-retrieved beliefs are strengthened,
	// so worn associations prime more strongly later. Off by default (it WRITES on a
	// read); set by --hebbian or a truthy MNEMOS_HEBBIAN.
	hebbian bool
	// reconsolidate enables the retrieval freshness touch (ADR 0015 §5): recalled
	// beliefs are re-marked verified-now so retrieval keeps a memory alive against
	// decay. Off by default (it WRITES on a read); set by --reconsolidate or a truthy
	// MNEMOS_RECONSOLIDATE.
	reconsolidate bool
	// inhibit enables competitive inhibition (ADR 0016): a decisively-beaten
	// contradiction loser has its retrievability bounded-suppressed (never its trust).
	// Off by default (it WRITES on a read); set by --inhibit or a truthy MNEMOS_INHIBIT.
	inhibit bool
	// salient enables salience-biased retrieval (ADR 0013 §4): a bounded stakes
	// term is blended into the similarity score so a high-stakes belief outranks a
	// trivial one of equal similarity. Off by default; set by --salient or a truthy
	// MNEMOS_SALIENCE.
	salient bool
}

func parseQueryArgs(args []string) (queryArgs, error) {
	if len(args) == 0 {
		return queryArgs{}, NewUserError("query requires a question")
	}

	out := queryArgs{}
	questionArgs := args
	for len(questionArgs) > 0 {
		switch questionArgs[0] {
		case "--run":
			if len(questionArgs) < 3 {
				return queryArgs{}, NewUserError("--run flag requires <run-id> followed by a question")
			}
			out.runID = strings.TrimSpace(questionArgs[1])
			if out.runID == "" {
				return queryArgs{}, NewUserError("--run flag requires a non-empty run-id")
			}
			questionArgs = questionArgs[2:]
		case "--hops":
			if len(questionArgs) < 2 {
				return queryArgs{}, NewUserError("--hops flag requires a value")
			}
			n, err := strconv.Atoi(questionArgs[1])
			if err != nil || n < 0 || n > 5 {
				return queryArgs{}, NewUserError("--hops must be an integer in [0, 5]")
			}
			out.hops = n
			questionArgs = questionArgs[2:]
		case "--min-trust":
			if len(questionArgs) < 2 {
				return queryArgs{}, NewUserError("--min-trust flag requires a value in [0, 1]")
			}
			v, err := strconv.ParseFloat(questionArgs[1], 64)
			if err != nil || v < 0 || v > 1 {
				return queryArgs{}, NewUserError("--min-trust must be a float in [0, 1]")
			}
			out.minTrust = v
			questionArgs = questionArgs[2:]
		case "--at":
			if len(questionArgs) < 2 {
				return queryArgs{}, NewUserError("--at flag requires a date (YYYY-MM-DD) or RFC3339 timestamp")
			}
			t, err := parseAsOf(questionArgs[1])
			if err != nil {
				return queryArgs{}, NewUserError("--at: %v", err)
			}
			out.asOf = t
			questionArgs = questionArgs[2:]
		case "--recorded-at":
			if len(questionArgs) < 2 {
				return queryArgs{}, NewUserError("--recorded-at flag requires a date (YYYY-MM-DD) or RFC3339 timestamp")
			}
			t, err := parseAsOf(questionArgs[1])
			if err != nil {
				return queryArgs{}, NewUserError("--recorded-at: %v", err)
			}
			out.recordedAsOf = t
			questionArgs = questionArgs[2:]
		case "--include-history":
			out.includeHistory = true
			questionArgs = questionArgs[1:]
		case "--prime":
			out.prime = true
			questionArgs = questionArgs[1:]
		case "--salient":
			out.salient = true
			questionArgs = questionArgs[1:]
		case "--hebbian":
			out.hebbian = true
			questionArgs = questionArgs[1:]
		case "--reconsolidate":
			out.reconsolidate = true
			questionArgs = questionArgs[1:]
		case "--inhibit":
			out.inhibit = true
			questionArgs = questionArgs[1:]
		case "--entity":
			if len(questionArgs) < 2 {
				return queryArgs{}, NewUserError("--entity requires a name or id")
			}
			out.entity = strings.TrimSpace(questionArgs[1])
			questionArgs = questionArgs[2:]
		case "--kind":
			if len(questionArgs) < 2 {
				return queryArgs{}, NewUserError("--kind requires a comma-separated list (e.g. causes,supports)")
			}
			kinds, err := parseHopKinds(questionArgs[1])
			if err != nil {
				return queryArgs{}, NewUserError("--kind: %v", err)
			}
			out.hopKinds = kinds
			questionArgs = questionArgs[2:]
		case "--service":
			if len(questionArgs) < 2 {
				return queryArgs{}, NewUserError("--service requires a value")
			}
			out.scope.Service = strings.TrimSpace(questionArgs[1])
			questionArgs = questionArgs[2:]
		case "--env":
			if len(questionArgs) < 2 {
				return queryArgs{}, NewUserError("--env requires a value")
			}
			out.scope.Env = strings.TrimSpace(questionArgs[1])
			questionArgs = questionArgs[2:]
		case "--team":
			if len(questionArgs) < 2 {
				return queryArgs{}, NewUserError("--team requires a value")
			}
			out.scope.Team = strings.TrimSpace(questionArgs[1])
			questionArgs = questionArgs[2:]
		case "--why-wrong":
			out.whyWrong = true
			questionArgs = questionArgs[1:]
		case "--why-trust":
			if len(questionArgs) < 2 {
				return queryArgs{}, NewUserError("--why-trust flag requires a claim ID")
			}
			out.whyTrust = strings.TrimSpace(questionArgs[1])
			if out.whyTrust == "" {
				return queryArgs{}, NewUserError("--why-trust flag requires a non-empty claim ID")
			}
			questionArgs = questionArgs[2:]
		case "--visibility":
			if len(questionArgs) < 2 {
				return queryArgs{}, NewUserError("--visibility requires a value: personal, team, or org")
			}
			v := domain.Visibility(strings.TrimSpace(questionArgs[1]))
			switch v {
			case domain.VisibilityPersonal, domain.VisibilityTeam, domain.VisibilityOrg:
				out.visibility = v
			default:
				return queryArgs{}, NewUserError("--visibility must be one of: personal, team, org")
			}
			questionArgs = questionArgs[2:]
		default:
			goto done
		}
	}
done:

	out.question = strings.TrimSpace(strings.Join(questionArgs, " "))
	if out.question == "" && !out.whyWrong && out.whyTrust == "" {
		return queryArgs{}, NewUserError("query requires a question")
	}

	return out, nil
}

// formatEvolution renders a one-line summary of a claim's temporal
// validity for the human-readable answer output. Examples:
//
//	"valid since 2026-04-01"
//	"valid 2026-04-01 → 2026-05-15 (superseded)"
//	"valid until 2026-05-15 (superseded)"
//
// Only invoked when at least one of valid_from / valid_to is non-zero,
// so callers don't have to gate.
func formatEvolution(c domain.Claim) string {
	const dateFmt = "2006-01-02"
	switch {
	case !c.ValidFrom.IsZero() && !c.ValidTo.IsZero():
		return fmt.Sprintf("valid %s → %s (superseded)",
			c.ValidFrom.UTC().Format(dateFmt),
			c.ValidTo.UTC().Format(dateFmt))
	case !c.ValidTo.IsZero():
		return fmt.Sprintf("valid until %s (superseded)", c.ValidTo.UTC().Format(dateFmt))
	case !c.ValidFrom.IsZero():
		return fmt.Sprintf("valid since %s", c.ValidFrom.UTC().Format(dateFmt))
	default:
		return ""
	}
}

// parseHopKinds parses a comma-separated list of relationship kinds
// for the `query --kind` flag. Each entry is validated against the
// recognised RelationshipType set so a typo fails fast rather than
// silently filtering out every edge.
func parseHopKinds(spec string) ([]domain.RelationshipType, error) {
	parts := strings.Split(spec, ",")
	out := make([]domain.RelationshipType, 0, len(parts))
	seen := make(map[domain.RelationshipType]struct{}, len(parts))
	for _, p := range parts {
		k := domain.RelationshipType(strings.TrimSpace(p))
		if k == "" {
			continue
		}
		if !domain.IsValidRelationshipType(k) {
			return nil, fmt.Errorf("unknown relationship kind %q", k)
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one kind is required")
	}
	return out, nil
}

// parseAsOf accepts a YYYY-MM-DD date or a full RFC3339(Nano)
// timestamp. Date-only inputs anchor to 00:00:00 UTC, which means
// `--at 2026-04-01` returns claims that were valid at the start of
// April 1st (the most intuitive reading for "as of that day").
func parseAsOf(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp %q (want YYYY-MM-DD or RFC3339)", s)
}

// warnRelateSkipped surfaces an incremental-relate failure as a warning
// rather than a fatal error. Distinguishes a deadline-exceeded cause —
// usually upstream budget exhaustion, not a real DB problem — from other
// failures, and points users at the right knobs.
func warnRelateSkipped(err error, stage string) {
	warn := icon("⚠️", "(!)")
	if errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintf(os.Stderr, "\n  %s Skipped incremental relate (%s): job deadline exceeded.\n", warn, stage)
		fmt.Fprintf(os.Stderr, "    Extracted claims have been persisted; cross-run edges will be picked up next time.\n")
		fmt.Fprintf(os.Stderr, "    Tune MNEMOS_JOB_TIMEOUT (default 10m) or MNEMOS_LLM_TIMEOUT (default 120s) for slower providers,\n")
		fmt.Fprintf(os.Stderr, "    or pass --no-relate to skip this stage entirely.\n\n")
		return
	}
	fmt.Fprintf(os.Stderr, "\n  %s Skipped incremental relate (%s): %v\n", warn, stage, err)
	fmt.Fprintf(os.Stderr, "    Extracted claims have been persisted; cross-run edges will be picked up next time.\n\n")
}

// jobTimeout returns the per-job workflow timeout, honoring MNEMOS_JOB_TIMEOUT.
// Defaults to 10 minutes — generous enough for local-LLM extraction over
// many events. The previous 20s default forced the downstream relate-stage
// DB read onto an exhausted ctx, surfacing as the misleading "failed to
// load existing claims: list all claims: context deadline exceeded".
func jobTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("MNEMOS_JOB_TIMEOUT"))
	if raw == "" {
		return 10 * time.Minute
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "warning: invalid MNEMOS_JOB_TIMEOUT=%q (want 60s, 5m, etc.); using 10m\n", raw)
		return 10 * time.Minute
	}
	return d
}

func runJob(kind string, scope map[string]string, verbose bool, fn func(context.Context, *workflow.Job, *govwrite.Writer) error) error {
	// First-run detection still uses the resolved file path (a
	// SQLite-only convenience — checking whether the DB file is
	// newly created on disk). With non-SQLite DSNs the path is
	// just a fallback default and isFirstRun is harmless.
	dbPath := resolveDBPath()
	if isFirstRun(dbPath) && kind != "ingest" && kind != "process" {
		printWelcome()
		fmt.Println("  First run detected. Use 'process' or 'ingest' to add knowledge.")
		printFirstRunHints()
	}

	// Every job runs against a governed daemon-writer so any durable
	// write inside the job's closure routes through the axi kernel.
	// The writer owns the store connection (opened here, closed below).
	w, err := openWriter(context.Background())
	if err != nil {
		return NewSystemError(err, "failed to open database at %q", displayDSN())
	}
	defer closeWriter(w)

	runner := workflow.NewRunner(w.Conn().Jobs)
	runner.Timeout = jobTimeout()
	runner.MaxRetries = 1
	runner.Verbose = verbose

	// CLI invocations have no caller deadline to inherit; jobTimeout() (10m by
	// default, MNEMOS_JOB_TIMEOUT) is the bound.
	jobErr := runner.Run(context.Background(), kind, scope, func(ctx context.Context, job *workflow.Job) error {
		return fn(ctx, job, w)
	})
	return jobErr
}

// consumerFor picks the contradiction-handling mode for a CLI query.
//
// A person reading output wants contradictions surfaced and explained, so the
// default is ConsumerUser. Asking for inhibition is asking for the opposite —
// it acts on a decided loser, and only an agent query adjudicates one. Rather
// than let `--inhibit` be silently inert, requesting it selects the mode that
// makes it mean something.
func consumerFor(inhibit bool) domain.Consumer {
	if inhibit {
		return domain.ConsumerAgent
	}

	return domain.ConsumerUser
}
