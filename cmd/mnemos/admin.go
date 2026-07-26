package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.klarlabs.de/mnemos/internal/embedding"
	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/pipeline"
	"go.klarlabs.de/mnemos/internal/ports"
	"go.klarlabs.de/mnemos/internal/store"
	"go.klarlabs.de/mnemos/internal/trust"
	"go.klarlabs.de/mnemos/internal/workflow"
)

// resetCounts captures what was removed during a reset for the user-facing
// summary. Zero values are still printed so users see exactly what changed.
type resetCounts struct {
	Claims        int64
	Evidence      int64
	StatusHistory int64
	Relationships int64
	Embeddings    int64
	Events        int64
}

func handleReset(args []string, f Flags) {
	keepEvents := false
	for _, a := range args {
		switch a {
		case "--keep-events":
			keepEvents = true
		default:
			exitWithMnemosError(false, NewUserError("unknown argument %q for reset\n  mnemos reset [--keep-events] [--yes]", a))
			return
		}
	}

	if !f.Yes {
		desc := "all events, claims, relationships, and embeddings"
		if keepEvents {
			desc = "all claims, relationships, and embeddings (events kept)"
		}
		if !confirm(fmt.Sprintf("This will delete %s from %s. Continue?", desc, displayDSN())) {
			fmt.Println("aborted")
			os.Exit(int(ExitSuccess))
		}
	}

	err := runJob("reset", map[string]string{"keep_events": fmt.Sprintf("%t", keepEvents)}, f.Verbose, func(ctx context.Context, _ *workflow.Job, w *govwrite.Writer) error {
		gc, err := w.Reset(ctx, keepEvents)
		if err != nil {
			return NewSystemError(err, "reset failed")
		}
		printResetSummary(resetCounts{
			Claims:        gc.Claims,
			Evidence:      gc.Evidence,
			StatusHistory: gc.StatusHistory,
			Relationships: gc.Relationships,
			Embeddings:    gc.Embeddings,
			Events:        gc.Events,
		}, keepEvents)
		return nil
	})
	exitWithMnemosError(f.Verbose, err)
}

func handleDeleteClaim(args []string, f Flags) {
	if len(args) == 0 {
		exitWithMnemosError(false, NewUserError("delete-claim requires at least one claim id\n  mnemos delete-claim <id> [<id>...]"))
		return
	}

	confirmDestructiveOrExit(f, fmt.Sprintf("This will permanently delete %d claim(s) and their evidence/embeddings/relationships (%s) from %s. Continue?", len(args), strings.Join(args, ", "), displayDSN()))

	err := runJob("delete-claim", map[string]string{"ids": strings.Join(args, ",")}, f.Verbose, func(ctx context.Context, _ *workflow.Job, w *govwrite.Writer) error {
		var deletedClaims int64
		// Each claim's full cascade (relationships, embedding,
		// claim_evidence, claim_status_history, claim row) routes
		// through ONE governed action so the destructive op is a single
		// auditable entry on the evidence chain. Cross-claim atomicity is
		// best-effort; a partial failure leaves the store in a
		// recoverable state and surfaces via `mnemos doctor`.
		for _, id := range args {
			if err := w.DeleteClaimCascade(ctx, id); err != nil {
				return NewSystemError(err, "delete claim %s", id)
			}
			deletedClaims++
		}
		fmt.Printf("Deleted %d claim(s) and their evidence/embeddings/relationships.\n", deletedClaims)
		return nil
	})
	exitWithMnemosError(f.Verbose, err)
}

func handleDeleteEvent(args []string, f Flags) {
	if len(args) == 0 {
		exitWithMnemosError(false, NewUserError("delete-event requires at least one event id\n  mnemos delete-event <id> [<id>...]"))
		return
	}

	confirmDestructiveOrExit(f, fmt.Sprintf("This will permanently delete %d event(s) and cascade their dependent claims/evidence/embeddings/relationships (%s) from %s. Continue?", len(args), strings.Join(args, ", "), displayDSN()))

	err := runJob("delete-event", map[string]string{"ids": strings.Join(args, ",")}, f.Verbose, func(ctx context.Context, _ *workflow.Job, w *govwrite.Writer) error {
		var deletedEvents, cascadedClaims int64
		for _, id := range args {
			// One governed action cascades the event delete through its
			// dependent claims (relationships, embedding, claim cascade)
			// and then drops the event embedding + event row — a single
			// auditable entry per event on the evidence chain.
			n, err := w.DeleteEventCascade(ctx, id)
			if err != nil {
				return NewSystemError(err, "delete event %s", id)
			}
			cascadedClaims += int64(n)
			deletedEvents++
		}
		fmt.Printf("Deleted %d event(s); cascaded %d claim(s).\n", deletedEvents, cascadedClaims)
		return nil
	})
	exitWithMnemosError(f.Verbose, err)
}

// handleDedupe runs the semantic-dedupe pipeline against the local
// claim store. Defaults to dry-run because the operation is
// destructive (claims are merged, others deleted); --apply commits.
//
// Threshold default 0.92 is conservative on purpose. Lowering to
// 0.85 catches more paraphrases but also more legitimate distinct
// claims. Users should re-tune for their corpus.
func handleDedupe(args []string, f Flags) {
	threshold := 0.92
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--threshold":
			if i+1 >= len(args) {
				exitWithMnemosError(false, NewUserError("--threshold requires a value in (0, 1]"))
				return
			}
			t, err := strconv.ParseFloat(args[i+1], 64)
			if err != nil || t <= 0 || t > 1 {
				exitWithMnemosError(false, NewUserError("--threshold must be a float in (0, 1]"))
				return
			}
			threshold = t
			i++
		default:
			exitWithMnemosError(false, NewUserError("unknown argument %q for dedup", args[i]))
			return
		}
	}

	// --apply must be opt-in; default is dry-run. We borrow Flags.Force
	// for "yes really apply this" so users get a single mental model
	// across reembed, dedupe, etc.
	apply := f.Force
	if !apply && !f.DryRun {
		// Neither flag set → still default to dry-run, just say so.
		f.DryRun = true
	}

	err := runJob("dedup", map[string]string{
		"threshold": strconv.FormatFloat(threshold, 'f', 2, 64),
		"apply":     fmt.Sprintf("%t", apply),
	}, f.Verbose, func(ctx context.Context, _ *workflow.Job, w *govwrite.Writer) error {
		conn := w.Conn()
		plan, err := pipeline.PlanSemanticDedupe(ctx, conn, threshold)
		if err != nil {
			return NewSystemError(err, "plan semantic dedupe")
		}
		printDedupePlan(plan)
		if !apply {
			fmt.Println("\nDry run. Re-run with --force to apply.")
			return nil
		}
		merged, err := pipeline.ApplySemanticDedupe(ctx, conn, plan)
		if err != nil {
			return NewSystemError(err, "apply semantic dedupe")
		}
		fmt.Printf("\nMerged %d duplicate claim(s).\n", merged)
		// Trust ranking depends on the evidence count we just
		// changed; recompute so the next query sees fresh scores.
		now := time.Now().UTC()
		if scorer, ok := conn.Claims.(ports.TrustScorer); ok {
			if _, err := scorer.RecomputeTrust(ctx, func(confidence float64, evidenceCount int, latestEvidence time.Time) float64 {
				return trust.Score(confidence, evidenceCount, latestEvidence, now)
			}); err != nil {
				fmt.Fprintf(os.Stderr, "  warning: post-dedupe trust recompute failed: %v\n", err)
			}
		}
		return nil
	})
	exitWithMnemosError(f.Verbose, err)
}

func printDedupePlan(plan pipeline.SemanticDedupePlan) {
	fmt.Printf("Semantic dedupe plan (threshold=%.2f)\n", plan.Threshold)
	fmt.Printf("  scanned:   %d claim(s) with embeddings\n", plan.ClaimsScanned)
	if plan.SkippedNoEmbedding > 0 {
		fmt.Printf("  skipped:   %d claim(s) without embeddings (run 'mnemos reembed' to include them)\n", plan.SkippedNoEmbedding)
	}
	if len(plan.Merges) == 0 {
		fmt.Println("  no near-duplicates found.")
		return
	}
	fmt.Printf("  proposing: %d merge(s)\n", len(plan.Merges))
	for i, m := range plan.Merges {
		fmt.Printf("    %d. winner=%s sim=%.3f absorbs %d duplicate(s): %s\n",
			i+1, m.WinnerID, m.MaxSimilarity, len(m.DuplicateIDs), strings.Join(m.DuplicateIDs, ", "))
	}
}

// handleRecomputeTrust rebuilds trust_score for every claim under the
// current scoring policy. Useful after upgrading (the v1→v2 migration
// adds the column with default 0; this command actually populates
// it), after tuning the trust constants in internal/trust, or as a
// nightly cron via `mnemos schedule`.
func handleRecomputeTrust(args []string, f Flags) {
	for _, a := range args {
		if a != "--all" {
			exitWithMnemosError(false, NewUserError("unknown argument %q for recompute-trust\n  mnemos recompute-trust [--all]", a))
			return
		}
	}

	err := runJob("recompute-trust", map[string]string{}, f.Verbose, func(ctx context.Context, _ *workflow.Job, w *govwrite.Writer) error {
		conn := w.Conn()
		scorer, ok := conn.Claims.(ports.TrustScorer)
		if !ok {
			return NewSystemError(fmt.Errorf("backend %T does not support trust scoring", conn.Claims), "recompute trust")
		}
		now := time.Now().UTC()
		n, err := scorer.RecomputeTrust(ctx, func(confidence float64, evidenceCount int, latestEvidence time.Time) float64 {
			return trust.Score(confidence, evidenceCount, latestEvidence, now)
		})
		if err != nil {
			return NewSystemError(err, "recompute trust")
		}
		fmt.Printf("Recomputed trust for %d claim(s).\n", n)
		return nil
	})
	exitWithMnemosError(f.Verbose, err)
}

// defaultEmbedBatch caps how many texts go into one provider request.
//
// reembed used to send the ENTIRE corpus as a single call. That survives
// against OpenAI, which accepts very large batches, and falls over against a
// self-hosted TEI/Infinity sidecar, whose max-batch and max-token budgets are
// far smaller — which is precisely the provider you switch to when you move off
// a US API, i.e. exactly when you need this command to work. Override with
// MNEMOS_EMBED_BATCH.
const defaultEmbedBatch = 64

func embedBatchSize() int {
	if v := os.Getenv("MNEMOS_EMBED_BATCH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}

	return defaultEmbedBatch
}

// reembedTarget is one entity type's worth of work.
type reembedTarget struct {
	entityType string // "claim" | "event" — the embeddings.entity_type value
	label      string // for user-facing output
	ids        []string
	text       map[string]string
}

// reembedEntities embeds one target in bounded batches and stores each vector
// under its entity type. Returns how many were stored.
//
// It requires one vector per input, per batch. The previous implementation did
// `if i >= len(vectors) { break }` and then reported len(vectors) as the
// success count — so a provider returning a short batch left rows unembedded
// while the command printed a success line and exited 0. internal/pipeline
// guards against exactly this ("silently storing a short prefix and returning
// its length as a success count"); reembed simply never got the guard. With
// batching added it matters more, not less: a partially-honoured batch is the
// most likely way a self-hosted sidecar degrades under load.
func reembedEntities(ctx context.Context, w *govwrite.Writer, client embedding.Client, model string, t reembedTarget, batch int) (int, error) {
	texts := make([]string, 0, len(t.ids))
	keep := make([]string, 0, len(t.ids))

	for _, id := range t.ids {
		txt, ok := t.text[id]
		if !ok || strings.TrimSpace(txt) == "" {
			continue // deleted between listing and now, or nothing to embed
		}
		texts = append(texts, txt)
		keep = append(keep, id)
	}

	stored := 0
	for start := 0; start < len(texts); start += batch {
		end := min(start+batch, len(texts))

		vectors, err := client.Embed(ctx, texts[start:end])
		if err != nil {
			return stored, NewSystemError(err, "embed %ss (batch %d-%d of %d)", t.entityType, start, end, len(texts))
		}
		if len(vectors) != end-start {
			return stored, NewSystemError(
				fmt.Errorf("provider returned %d vectors for %d inputs", len(vectors), end-start),
				"embed %ss (batch %d-%d): short batch, refusing to store a partial result", t.entityType, start, end)
		}

		for i, vec := range vectors {
			if err := w.Embedding(ctx, keep[start+i], t.entityType, vec, model, ""); err != nil {
				return stored, NewSystemError(err, "store embedding for %s %s", t.entityType, keep[start+i])
			}
			stored++
		}
	}

	return stored, nil
}

func handleReembed(args []string, f Flags) {
	for _, a := range args {
		switch a {
		default:
			exitWithMnemosError(false, NewUserError("unknown argument %q for reembed\n  mnemos reembed [--force] [--dry-run]", a))
			return
		}
	}

	err := runJob("reembed", map[string]string{"force": fmt.Sprintf("%t", f.Force), "dry_run": fmt.Sprintf("%t", f.DryRun)}, f.Verbose, func(ctx context.Context, _ *workflow.Job, w *govwrite.Writer) error {
		conn := w.Conn()

		targets, err := collectReembedTargets(ctx, conn, f.Force)
		if err != nil {
			return err
		}

		total := 0
		for _, t := range targets {
			total += len(t.ids)
		}
		if total == 0 {
			fmt.Println("Nothing needs embeddings. Nothing to do.")
			return nil
		}

		if f.DryRun {
			for _, t := range targets {
				fmt.Printf("Would (re)embed %d %s(s).\n", len(t.ids), t.label)
			}
			fmt.Println("Run without --dry-run to apply.")

			return nil
		}

		cfg, err := embedding.ConfigFromEnv()
		if err != nil {
			return NewSystemError(err, "embedding config")
		}
		client, err := embedding.NewClient(cfg)
		if err != nil {
			return NewSystemError(err, "embedding client")
		}

		batch := embedBatchSize()
		for _, t := range targets {
			n, err := reembedEntities(ctx, w, client, cfg.Model, t, batch)
			if err != nil {
				// Report progress before bailing: the run is resumable, and
				// knowing how far it got is the difference between "re-run it"
				// and "work out what state the corpus is in".
				fmt.Printf("Embedded %d %s(s) before failing.\n", n, t.label)
				return err
			}
			fmt.Printf("Embedded %d %s(s) with %s/%s.\n", n, t.label, cfg.Provider, cfg.Model)
		}

		return nil
	})
	exitWithMnemosError(f.Verbose, err)
}

// collectReembedTargets works out what needs embedding, for BOTH claims and
// events.
//
// Events were previously ignored entirely — reembed hardcoded entity type
// "claim", and event vectors are written only by the ingest pipeline. Changing
// the embedding model therefore stranded every event embedding in the old
// space with no way to migrate it: recall is model-filtered, so those rows went
// silently dark and stayed dark short of re-ingesting the corpus.
func collectReembedTargets(ctx context.Context, conn *store.Conn, force bool) ([]reembedTarget, error) {
	claims, err := conn.Claims.ListAll(ctx)
	if err != nil {
		return nil, NewSystemError(err, "list claims")
	}
	claimText := make(map[string]string, len(claims))
	for _, c := range claims {
		claimText[c.ID] = c.Text
	}

	events, err := conn.Events.ListAll(ctx)
	if err != nil {
		return nil, NewSystemError(err, "list events")
	}
	eventText := make(map[string]string, len(events))
	for _, e := range events {
		eventText[e.ID] = e.Content
	}

	var claimIDs, eventIDs []string
	if force {
		claimIDs = make([]string, 0, len(claims))
		for _, c := range claims {
			claimIDs = append(claimIDs, c.ID)
		}
		eventIDs = make([]string, 0, len(events))
		for _, e := range events {
			eventIDs = append(eventIDs, e.ID)
		}
	} else {
		if claimIDs, err = conn.Claims.ListIDsMissingEmbedding(ctx); err != nil {
			return nil, NewSystemError(err, "list missing claim embeddings")
		}
		// No anti-join port method exists for events, so diff in Go against the
		// stored event embeddings rather than add a method to every backend.
		existing, err := conn.Embeddings.ListByEntityType(ctx, "event")
		if err != nil {
			return nil, NewSystemError(err, "list event embeddings")
		}
		embedded := make(map[string]struct{}, len(existing))
		for _, rec := range existing {
			embedded[rec.EntityID] = struct{}{}
		}
		for _, e := range events {
			if _, ok := embedded[e.ID]; !ok {
				eventIDs = append(eventIDs, e.ID)
			}
		}
	}

	return []reembedTarget{
		{entityType: "claim", label: "claim", ids: claimIDs, text: claimText},
		{entityType: "event", label: "event", ids: eventIDs, text: eventText},
	}, nil
}

func printResetSummary(c resetCounts, keepEvents bool) {
	fmt.Printf("Reset complete (db=%s)\n", displayDSN())
	fmt.Printf("  claims:        %-8d (deleted)\n", c.Claims)
	fmt.Printf("  evidence:      %-8d (deleted)\n", c.Evidence)
	fmt.Printf("  status hist:   %-8d (deleted)\n", c.StatusHistory)
	fmt.Printf("  relationships: %-8d (deleted)\n", c.Relationships)
	fmt.Printf("  embeddings:    %-8d (deleted)\n", c.Embeddings)
	if keepEvents {
		fmt.Printf("  events:        kept\n")
	} else {
		fmt.Printf("  events:        %-8d (deleted)\n", c.Events)
	}
}

// stdinReader is shared by every interactive prompt in the CLI.
//
// It must be a single reader: bufio reads ahead in chunks, so a prompt that
// makes its own reader can pull the NEXT prompt's answer into a buffer that is
// then discarded. Two prompts in one command (init asks about capture, then
// confirms) would leave the second reading EOF and silently taking the "no"
// branch — the command aborts having consumed the user's answer.
var (
	stdinReaderOnce sync.Once
	stdinReaderVal  *bufio.Reader
)

func stdinReader() *bufio.Reader {
	stdinReaderOnce.Do(func() { stdinReaderVal = bufio.NewReader(os.Stdin) })
	return stdinReaderVal
}

// promptLine writes a prompt and reads one answer, reporting whether a line
// was actually read (false on EOF, so callers can distinguish "declined" from
// "nobody there").
func promptLine(prompt string) (string, bool) {
	fmt.Print(prompt)
	line, err := stdinReader().ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", false
	}
	return strings.TrimSpace(line), true
}

func confirm(prompt string) bool {
	line, ok := promptLine(fmt.Sprintf("%s [y/N]: ", prompt))
	if !ok {
		return false
	}
	line = strings.ToLower(line)
	return line == "y" || line == "yes"
}

// stdinIsInteractive reports whether stdin is a terminal (so a y/N prompt can
// actually be answered).
func stdinIsInteractive() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// confirmDestructiveOrExit gates a destructive command. With --yes it returns.
// On an interactive terminal it prompts and exits 0 if the user declines. In a
// NON-interactive context without --yes it exits NON-ZERO with a usage error —
// so a script/CI run fails loudly instead of silently no-opping while reporting
// success (the prompt would otherwise read EOF, "decline", and exit 0 without
// deleting anything).
func confirmDestructiveOrExit(f Flags, prompt string) {
	if f.Yes {
		return
	}
	if !stdinIsInteractive() {
		exitWithMnemosError(f.Verbose, NewUserError("refusing to proceed without confirmation in a non-interactive context — pass --yes to confirm"))
		return
	}
	if !confirm(prompt) {
		fmt.Println("aborted")
		os.Exit(int(ExitSuccess))
	}
}
