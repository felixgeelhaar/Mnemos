package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/extract"
	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/llm"
	"go.klarlabs.de/mnemos/internal/workflow"
)

// `mnemos prune --session-noise [--dry-run]` drops contradiction edges where an
// LLM judges BOTH claims to be session-local rather than durable knowledge.
//
// It exists for the backlog that session tagging cannot reach. Capture began
// stamping events with their session in v0.108.0, and the ingest path drops
// intra-session contradictions from that point on — but nothing ingested
// earlier carries a session, and a session cannot be inferred after the fact.
// That legacy pile is dominated by conversational progress reports arguing with
// each other across sessions ("both PRs are open" against "PR #23 merged"),
// which no rule-based detector distinguishes from real disagreement.
//
// It removes EDGES ONLY, never claims. That is the whole reason an LLM is
// allowed near this decision. ADR 0023 rejected LLM ROUTING of observations
// because dropping a belief on a false positive is an invisible loss of real
// knowledge; here a false positive costs one contradiction alert, which is
// visible, cheap, and re-derivable by `relate` at any time. The classifier's
// measured error profile fits that bet: on a hand-labelled sample it never
// called a durable claim session-local (18 verdicts, 0 errors) while
// over-calling durable, so it under-suppresses rather than over-suppresses.
//
// Both endpoints must be session-local. One session-local claim contradicting
// a durable one is exactly the case worth keeping — that is a conversation
// bumping into something the brain actually believes.
func pruneSessionNoise(dryRun bool, f Flags) {
	err := runJob("prune-session-noise", map[string]string{"dry_run": fmt.Sprint(dryRun)}, f.Verbose,
		func(ctx context.Context, job *workflow.Job, w *govwrite.Writer) error {
			actor, actorErr := resolveActor(ctx, w.Conn().Users, f.Actor)
			if actorErr != nil {
				return actorErr
			}
			_, err := runSessionNoisePass(ctx, w, actor, dryRun, os.Stdout, func(s string) error {
				return job.SetStatus(s, "")
			})
			return err
		})
	if err != nil {
		exitWithMnemosError(f.Verbose, err)
	}
}

// sessionNoiseStats reports what one pass did. The CLI prints its progress as
// it goes; a non-interactive caller (the hosted sleep cycle in serve_sleep.go)
// discards that narration and logs these numbers instead.
type sessionNoiseStats struct {
	LiveEdges    int
	Classified   int
	SessionLocal int
	Marked       int
	Pruned       int
}

// runSessionNoisePass is the pass itself, extracted from the CLI command so
// `serve`'s sleep cycle can run the identical work. It writes its progress to
// out (os.Stdout for the CLI, io.Discard for a server) and reports job progress
// through status, which may be nil.
//
// It takes an already-opened governed writer rather than opening one, because
// the two callers scope it differently: the CLI gets the process store from
// runJob, while the sleep cycle opens one per TENANT partition.
func runSessionNoisePass(
	ctx context.Context,
	w *govwrite.Writer,
	actor string,
	dryRun bool,
	out io.Writer,
	status func(string) error,
) (sessionNoiseStats, error) {
	setStatus := func(s string) error {
		if status == nil {
			return nil
		}
		return status(s)
	}
	// Progress narration. The write error is deliberately dropped: out is a
	// terminal or io.Discard, and a broken pipe is no reason to abandon a
	// consolidation that is otherwise succeeding.
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format, args...) }
	sayln := func(s string) { _, _ = fmt.Fprintln(out, s) }
	var stats sessionNoiseStats
	err := func() error {
		conn := w.Conn()
		if err := setStatus("loading"); err != nil {
			return err
		}

		cfg, cfgErr := llm.ConfigFromEnv()
		if cfgErr != nil {
			return NewUserError("this pass needs an LLM; set MNEMOS_LLM_PROVIDER (and its key/model): %v", cfgErr)
		}
		client, clientErr := llm.NewClient(cfg)
		if clientErr != nil {
			return NewUserError("build LLM client: %v", clientErr)
		}

		claims, err := conn.Claims.ListAll(ctx)
		if err != nil {
			return NewSystemError(err, "load claims")
		}
		byID := make(map[string]domain.Claim, len(claims))
		for _, c := range claims {
			byID[c.ID] = c
		}
		rels, err := conn.Relationships.ListAll(ctx)
		if err != nil {
			return NewSystemError(err, "load relationships")
		}

		// Only claims sitting on a live contradiction edge are worth an LLM
		// call — classifying the whole brain would cost orders of magnitude
		// more for no additional decision.
		live := liveContradictions(rels, byID)
		subjects := contradictionEndpoints(live, byID)
		stats.LiveEdges = len(live)
		if len(subjects) == 0 {
			sayln("no live contradiction edges; nothing to classify.")
			return nil
		}

		// "extracting" in the job state machine's vocabulary: this is an LLM
		// pass over claim text, and the machine has no classification state.
		if err := setStatus("extracting"); err != nil {
			return err
		}
		say("live contradiction edges: %d over %d claim(s)\n", len(live), len(subjects))
		texts := make([]string, len(subjects))
		for i, id := range subjects {
			texts[i] = byID[id].Text
		}
		// A brain of any size will outlast a fixed job budget on a local
		// model, so running out of time is a normal outcome, not a failure:
		// keep the verdicts already earned and say how far the pass got.
		// Unclassified claims stay Unknown, which suppresses nothing, so a
		// partial run is safe. Verdicts are cached, so a re-run skips what
		// this pass already paid for and genuinely continues rather than
		// re-classifying the same prefix.
		// Classification must not spend the ENTIRE job budget, or the write
		// below inherits an already-expired context and the pass discards
		// everything it just paid for — which is precisely what happened:
		// a run that classified 2,226 claims and identified 1,084 edges
		// died on "prune relationships" with nothing written. Hold back a
		// slice of the budget so persisting the result is always possible.
		classifyCtx, cancelClassify := withWriteReserve(ctx)
		defer cancelClassify()
		verdicts, err := extract.ClassifyDurabilityCached(classifyCtx, client, texts, durabilityCacheDir())
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			return NewSystemError(err, "classify durability")
		}
		classified := len(verdicts) - countDurability(verdicts, extract.DurabilityUnknown)
		stats.Classified = classified
		if classified < len(texts) {
			say("classified %d of %d claim(s) before the budget ran out; re-run to continue (raise MNEMOS_JOB_TIMEOUT to cover more per pass)\n",
				classified, len(texts))
		}
		durability := make(map[string]extract.Durability, len(subjects))
		for i, id := range subjects {
			durability[id] = verdicts[i]
		}
		sessionLocal := countDurability(verdicts, extract.DurabilitySessionLocal)
		stats.SessionLocal = sessionLocal

		var drop []domain.Relationship
		for _, r := range live {
			if durability[r.FromClaimID] == extract.DurabilitySessionLocal &&
				durability[r.ToClaimID] == extract.DurabilitySessionLocal {
				drop = append(drop, r)
			}
		}

		// Rate over what was CLASSIFIED, not over every claim on an edge.
		// Dividing by the full set silently counts claims the budget never
		// reached as "not session-local": a pass covering 1,125 of 2,448
		// reported 27.9% where the measured rate among classified claims was
		// 60.7%, understating it by more than half.
		say("session-local claims:    %d of %d classified (%.1f%%)\n",
			sessionLocal, classified, pct(sessionLocal, classified))
		say("edges with BOTH sides session-local: %d (%.1f%%)\n", len(drop), pct(len(drop), len(live)))
		for i, r := range drop {
			if i >= 5 {
				say("  … and %d more\n", len(drop)-5)
				break
			}
			say("  - %s ⇎ %s\n",
				truncateClaim(byID[r.FromClaimID].Text), truncateClaim(byID[r.ToClaimID].Text))
		}

		if dryRun {
			sayln("\n(dry run — nothing written; re-run without --dry-run to apply)")
			return nil
		}
		if err := setStatus("saving"); err != nil {
			return err
		}
		// Persist the verdicts onto the beliefs themselves, not just the
		// on-disk classifier cache. Stored durability is what lets `relate`
		// stop CREATING these edges (ADR 0023); without it every future
		// session regenerates exactly what this pass just removed, and the
		// cleanup has to be re-run forever.
		//
		// Only beliefs whose verdict actually changed are rewritten: a
		// claim write triggers a trust rescore, so rewriting all of them
		// would make this pass cost more the larger the brain gets.
		var marked []domain.Claim
		for _, id := range subjects {
			v := durability[id]
			if v == extract.DurabilityUnknown {
				continue
			}
			c, ok := byID[id]
			if !ok {
				continue
			}
			want := domain.Durability(v)
			if c.Durability == want {
				continue
			}
			c.Durability = want
			marked = append(marked, c)
		}
		if len(marked) > 0 {
			if _, err := w.Claims(ctx, marked, govwrite.ClaimReason{
				Reason:    "Durability classified (prune --session-noise)",
				ChangedBy: actor,
			}); err != nil {
				return NewSystemError(err, "record durability")
			}
			stats.Marked = len(marked)
			say("marked %d belief(s) with a durability verdict.\n", len(marked))
		}
		// Through the governed writer, like `relate --prune-stale`: the
		// writer takes the surviving set, so build it by difference.
		if len(drop) == 0 {
			sayln("\nno edges to prune.")
			return nil
		}
		dropIDs := make(map[string]struct{}, len(drop))
		for _, r := range drop {
			dropIDs[r.ID] = struct{}{}
		}
		keep := make([]domain.Relationship, 0, len(rels)-len(drop))
		for _, r := range rels {
			if _, gone := dropIDs[r.ID]; !gone {
				keep = append(keep, r)
			}
		}
		if _, err := w.PruneRelationships(ctx, keep, len(drop)); err != nil {
			return NewSystemError(err, "prune relationships")
		}
		stats.Pruned = len(drop)
		// Precise about what was and was not changed: durability verdicts
		// ARE written to beliefs now, so "claims are untouched" would be
		// false. What holds is that no belief is deprecated or deleted.
		say("\npruned %d edge(s). No belief was deprecated or deleted; `mnemos relate` can re-derive edges.\n", len(drop))
		return nil
	}()
	return stats, err
}

// liveContradictions returns the contradiction edges whose endpoints both still
// exist and are not deprecated — the same population the dissonance vital
// counts, so the command's numbers match what `mnemos health` reports.
func liveContradictions(rels []domain.Relationship, byID map[string]domain.Claim) []domain.Relationship {
	var out []domain.Relationship
	for _, r := range rels {
		if r.Type != domain.RelationshipTypeContradicts {
			continue
		}
		from, okFrom := byID[r.FromClaimID]
		to, okTo := byID[r.ToClaimID]
		if !okFrom || !okTo {
			continue
		}
		if from.Status == domain.ClaimStatusDeprecated || to.Status == domain.ClaimStatusDeprecated {
			continue
		}
		out = append(out, r)
	}
	return out
}

// contradictionEndpoints lists the distinct claims appearing on the given
// edges, in a stable order so a dry run and the apply that follows classify the
// same batches and reach the same verdicts.
func contradictionEndpoints(rels []domain.Relationship, byID map[string]domain.Claim) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, r := range rels {
		for _, id := range []string{r.FromClaimID, r.ToClaimID} {
			if _, dup := seen[id]; dup {
				continue
			}
			if _, ok := byID[id]; !ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

// countDurability counts verdicts of one kind.
//
// Split out because the two counts it feeds are easy to divide by the wrong
// thing: the session-local RATE must be measured over classified claims, never
// over every claim on an edge, or a budget-limited pass reports claims it never
// looked at as though they had been judged durable.
func countDurability(verdicts []extract.Durability, want extract.Durability) int {
	n := 0
	for _, v := range verdicts {
		if v == want {
			n++
		}
	}
	return n
}

// pruneWriteReserve is how much of the job budget is held back for persisting
// the result. The write replaces the surviving relationship set, so it scales
// with the size of the graph rather than with what was pruned.
const pruneWriteReserve = 90 * time.Second

// withWriteReserve returns a context that expires before the job's own
// deadline, leaving time to write. A job with no deadline, or one too short to
// split sensibly, is returned unchanged: halving an already-tight budget would
// mean classifying nothing at all.
func withWriteReserve(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return ctx, func() {}
	}
	remaining := time.Until(deadline)
	if remaining <= 2*pruneWriteReserve {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, deadline.Add(-pruneWriteReserve))
}
