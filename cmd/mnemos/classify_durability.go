package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/extract"
	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/llm"
	"go.klarlabs.de/mnemos/internal/workflow"
)

// `mnemos classify-durability [--limit N] [--dry-run]` records, for beliefs that
// have no verdict yet, whether their value outlives the session that produced
// them (ADR 0023).
//
// `prune --session-noise` also classifies, but only beliefs sitting on a
// contradiction edge — that is where suppression needed a verdict and where the
// cost was justified. Recall draws from EVERY belief, so most of what a brief
// shows has no verdict and the recall demotion has nothing to act on: measured
// on a real brain, coverage was ~8% (1,732 of 22,188).
//
// This is a BULK pass, not a targeted one, and the distinction is worth being
// honest about because the first version of it claimed otherwise. Ordering by
// trust was supposed to reach "what recall shows"; measured, it does not.
// Trust is blind to durability (durable averaged 0.801, session-local 0.804)
// AND it is tightly clustered — 9,935 unclassified beliefs sat at >= 0.79, with
// 2,003 at exactly 0.83 — so "top N by trust" selects an arbitrary slice of a
// plateau. Recency discriminates only weakly too (51.9% session-local among
// today's beliefs against 38.3% two days earlier). Narration is spread roughly
// uniformly at 40-50%, so no static ordering targets it well.
//
// Newest-first is therefore chosen for a different and defensible reason: at an
// intake of several thousand beliefs a day, classifying oldest-first would fall
// permanently behind. Working from the newest keeps the field current with what
// is actually being added.
//
// Genuine targeting — classifying the beliefs recall SURFACES — cannot be done
// by any static ordering, because recall ranks by relevance to a question. It
// needs the verdicts to be filled in as beliefs are returned, which is a
// separate change to the recall path.
func classifyDurability(limit int, only []string, dryRun bool, f Flags) {
	err := runJob("classify-durability", map[string]string{
		"dry_run": fmt.Sprint(dryRun),
		"limit":   fmt.Sprint(limit),
	}, f.Verbose, func(ctx context.Context, job *workflow.Job, w *govwrite.Writer) error {
		conn := w.Conn()
		actor, actorErr := resolveActor(ctx, conn.Users, f.Actor)
		if actorErr != nil {
			return actorErr
		}
		if err := job.SetStatus("loading", ""); err != nil {
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
		var targets []domain.Claim
		if len(only) > 0 {
			targets = unclassifiedAmong(claims, only)
		} else {
			targets = unclassifiedNewestFirst(claims, limit)
		}
		if len(targets) == 0 {
			fmt.Println("every recallable belief already has a durability verdict.")
			return nil
		}

		if err := job.SetStatus("extracting", ""); err != nil {
			return err
		}
		if len(only) > 0 {
			fmt.Printf("classifying %d of %d requested belief(s) that lack a verdict\n", len(targets), len(only))
		} else {
			fmt.Printf("unclassified recallable beliefs: %d; classifying %d, newest first\n",
				countUnclassifiedRecallable(claims), len(targets))
		}

		texts := make([]string, len(targets))
		for i, c := range targets {
			texts[i] = c.Text
		}
		classifyCtx, cancel := withWriteReserve(ctx)
		defer cancel()
		verdicts, err := extract.ClassifyDurabilityCached(classifyCtx, client, texts, durabilityCacheDir())
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			return NewSystemError(err, "classify durability")
		}

		var marked []domain.Claim
		sessionLocal := 0
		for i, v := range verdicts {
			if v == extract.DurabilityUnknown {
				continue
			}
			if v == extract.DurabilitySessionLocal {
				sessionLocal++
			}
			c := targets[i]
			c.Durability = domain.Durability(v)
			marked = append(marked, c)
		}
		fmt.Printf("classified %d of %d (%.1f%% session-local)\n",
			len(marked), len(targets), pct(sessionLocal, len(marked)))

		if dryRun {
			fmt.Println("\n(dry run — nothing written; re-run without --dry-run to apply)")
			return nil
		}
		if len(marked) == 0 {
			fmt.Println("\nno verdicts to record.")
			return nil
		}
		if err := job.SetStatus("saving", ""); err != nil {
			return err
		}
		if _, err := w.Claims(ctx, marked, govwrite.ClaimReason{
			Reason:    "Durability classified (classify-durability)",
			ChangedBy: actor,
		}); err != nil {
			return NewSystemError(err, "record durability")
		}
		fmt.Printf("\nmarked %d belief(s). No belief was deprecated or deleted.\n", len(marked))
		return nil
	})
	if err != nil {
		exitWithMnemosError(f.Verbose, err)
	}
}

// recallable reports whether a belief can surface at recall at all. Anything
// deprecated or valid-time closed is already excluded there, so classifying it
// would spend the budget on text no reader will see.
func recallable(c domain.Claim) bool {
	return c.Status != domain.ClaimStatusDeprecated && c.ValidTo.IsZero()
}

func countUnclassifiedRecallable(claims []domain.Claim) int {
	n := 0
	for _, c := range claims {
		if recallable(c) && c.Durability.Normalized() == domain.DurabilityUnknown {
			n++
		}
	}
	return n
}

// unclassifiedNewestFirst returns up to limit recallable beliefs that have no
// verdict, newest first.
//
// Newest-first keeps pace with intake rather than falling behind it; see the
// command doc for why neither trust nor recency actually targets what recall
// surfaces.
//
// Ties break on ID so a dry run and the apply that follows pick the same
// beliefs and produce the same batches — otherwise the preview would describe
// work the apply does not do. Ties are common: thousands of beliefs share a
// capture timestamp to the second.
func unclassifiedNewestFirst(claims []domain.Claim, limit int) []domain.Claim {
	var out []domain.Claim
	for _, c := range claims {
		if recallable(c) && c.Durability.Normalized() == domain.DurabilityUnknown {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// handleClassifyDurability parses `classify-durability [--limit N] [--dry-run]`.
//
// The limit defaults rather than being required: full coverage is hours of
// local-model time, so a bare invocation should do a useful, bounded amount of
// work instead of appearing to hang.
func handleClassifyDurability(args []string, f Flags) {
	limit := defaultDurabilityLimit
	var only []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--ids":
			if i+1 >= len(args) {
				exitWithMnemosError(f.Verbose, NewUserError("--ids requires a comma-separated list"))
				return
			}
			for _, id := range strings.Split(args[i+1], ",") {
				if id = strings.TrimSpace(id); id != "" {
					only = append(only, id)
				}
			}
			i++
		case "--limit":
			if i+1 >= len(args) {
				exitWithMnemosError(f.Verbose, NewUserError("--limit requires a value"))
				return
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n <= 0 {
				exitWithMnemosError(f.Verbose, NewUserError("--limit must be a positive integer, got %q", args[i+1]))
				return
			}
			limit = n
			i++
		case "--dry-run":
			// parsed globally into Flags.DryRun; accepted here so it is not
			// rejected as unknown.
		default:
			exitWithMnemosError(f.Verbose, NewUserError("unknown argument %q (want --limit N | --ids a,b [--dry-run])", args[i]))
			return
		}
	}
	classifyDurability(limit, only, f.DryRun, f)
}

// defaultDurabilityLimit is a bounded default: roughly what one job budget
// covers on a local model, so a bare `classify-durability` finishes rather than
// appearing to hang against a brain with thousands of unclassified beliefs.
const defaultDurabilityLimit = 300

// unclassifiedAmong returns the recallable, still-unclassified beliefs from an
// explicit id list, preserving the caller's order.
//
// This is the recall-driven path: the ids are the beliefs a brief just showed
// the reader, which is the only way to reach "what recall surfaces" — no static
// ordering can, because recall ranks by relevance to a question. Ids that are
// already classified, deprecated, or unknown to this brain are simply dropped,
// so a stale id list costs nothing.
func unclassifiedAmong(claims []domain.Claim, ids []string) []domain.Claim {
	byID := make(map[string]domain.Claim, len(claims))
	for _, c := range claims {
		byID[c.ID] = c
	}
	seen := make(map[string]struct{}, len(ids))
	var out []domain.Claim
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		c, ok := byID[id]
		if !ok || !recallable(c) || c.Durability.Normalized() != domain.DurabilityUnknown {
			continue
		}
		out = append(out, c)
	}
	return out
}
