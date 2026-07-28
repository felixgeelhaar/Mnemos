package main

import (
	"context"
	"fmt"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/workflow"
)

// `mnemos prune --session-local [--dry-run]` retires beliefs an LLM already
// judged SESSION-LOCAL — statements tied to the moment they were written rather
// than knowledge that outlives it.
//
// It is the sibling of `prune --narration`. That one re-runs the rule-based junk
// filter; this one acts on a verdict the durability classifier has already
// recorded on the claim, so it needs no model at all and is purely a write.
//
// # WHY DEPRECATION IS THE RIGHT VERB
//
// `prune --session-noise` drops contradiction EDGES between two session-local
// claims. That leaves the claims themselves live, and measurement showed why
// that is not enough: on a real brain only 209 of 25,074 live contradictions had
// session-local on BOTH sides, while 10,354 had it on at least one. Edge pruning
// cannot reach the asymmetric majority, because a narration fragment arguing
// with an unclassified one is not a pair of session-local claims — it is one
// piece of narration that should not have been a belief.
//
// Retiring the claim reaches all of them at once, and the health vital is built
// to reward exactly that: `hypercorrectionList` skips any contradiction with a
// deprecated endpoint, and its own comment records that this was a bug fix
// precisely so "the brain-health vital could be improved by retiring bad
// beliefs".
//
// # WHY IT IS SAFE
//
// Deprecation is reduced retrievability, never erasure: the claim keeps its
// evidence, its history, and its queryability under --include-history, and the
// transition lands a claim_status_history row through the governed writer. It is
// reversible; deletion would not be.
//
// The verdict it acts on comes from a classifier with a measured error profile
// that OVER-calls durable — it under-suppresses rather than over-suppresses — so
// the population it retires is conservative by construction. And these claims
// are already excluded from recall, so this makes the store agree with what the
// reader has been seeing rather than changing what anyone can retrieve.
//
// Unknown/unclassified durability is never touched: "not yet judged" is not
// "narration", and treating it as such would retire the brain.
func pruneSessionLocal(dryRun bool, f Flags) {
	err := runJob("prune-session-local", map[string]string{"dry_run": fmt.Sprint(dryRun)}, f.Verbose,
		func(ctx context.Context, job *workflow.Job, w *govwrite.Writer) error {
			conn := w.Conn()
			actor, actorErr := resolveActor(ctx, conn.Users, f.Actor)
			if actorErr != nil {
				return actorErr
			}
			if err := job.SetStatus("loading", ""); err != nil {
				return err
			}

			claims, err := conn.Claims.ListAll(ctx)
			if err != nil {
				return NewSystemError(err, "load claims")
			}

			local := selectSessionLocalClaims(claims)
			candidates := countPrunable(claims)
			var fromActive, fromContested int
			for _, c := range local {
				switch c.Status {
				case domain.ClaimStatusActive:
					fromActive++
				case domain.ClaimStatusContested:
					fromContested++
				}
			}

			fmt.Printf("prunable claims:    %d (active + contested)\n", candidates)
			fmt.Printf("session-local:      %d (%.1f%%) — %d active, %d contested\n",
				len(local), pct(len(local), candidates), fromActive, fromContested)
			for i, c := range local {
				if i >= 8 {
					fmt.Printf("  … and %d more\n", len(local)-8)
					break
				}
				fmt.Printf("  - %s\n", truncateClaim(c.Text))
			}

			if dryRun {
				fmt.Println("\n(dry run — nothing written; re-run without --dry-run to apply)")
				return nil
			}
			if len(local) == 0 {
				fmt.Println("\nnothing to prune.")
				return nil
			}

			if err := job.SetStatus("saving", ""); err != nil {
				return err
			}
			for i := range local {
				local[i].Status = domain.ClaimStatusDeprecated
			}
			if _, err := w.Claims(ctx, local, govwrite.ClaimReason{
				Reason:    "Session-local, not durable knowledge (prune --session-local)",
				ChangedBy: actor,
			}); err != nil {
				return NewSystemError(err, "deprecate session-local claims")
			}
			fmt.Printf("\ndeprecated %d session-local claim(s); they remain queryable with --include-history.\n", len(local))
			return nil
		})
	if err != nil {
		exitWithMnemosError(f.Verbose, err)
	}
}

// selectSessionLocalClaims returns the live claims carrying a session-local
// durability verdict.
//
// Deliberately narrow on both axes. Only an explicit session-local verdict
// qualifies — Unknown is not narration — and only claims that are still live
// are returned, so a re-run is idempotent rather than rewriting the same
// deprecations and appending a second history row each time.
func selectSessionLocalClaims(claims []domain.Claim) []domain.Claim {
	var out []domain.Claim
	for _, c := range claims {
		if !c.Durability.IsSessionLocal() {
			continue
		}
		if c.Status != domain.ClaimStatusActive && c.Status != domain.ClaimStatusContested {
			continue
		}
		out = append(out, c)
	}
	return out
}
