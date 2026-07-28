package main

import (
	"context"
	"fmt"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/extract"
	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/workflow"
)

// `mnemos prune --narration [--dry-run]` deprecates already-stored claims that
// the current extraction filter classifies as conversational pollution rather
// than knowledge.
//
// The filters only run at ingest, so every claim captured before a filter
// existed is still stored and still recalls. A census of a real brain found
// nearly half of active claims were narration the older filters missed:
// lead-ins ("Confirmed absent from every downstream surface:"), section
// headers, evaluative fragments. Fixing extraction forward does nothing for
// them.
//
// This re-runs extract.IsJunk — byte-for-byte the same judgement the pipeline
// now applies — over stored claims, so the cleanup can never diverge from what
// ingest would have done. Dry-run by default; the write goes through the
// governed writer so each deprecation lands a claim_status_history row and is
// inspectable, never a raw UPDATE.
//
// It only DEPRECATES, never deletes: a mis-classified claim stays queryable and
// its transition is auditable and reversible, which raw deletion would not be.
func pruneNarration(dryRun bool, f Flags) {
	err := runJob("prune-narration", map[string]string{"dry_run": fmt.Sprint(dryRun)}, f.Verbose,
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

			junk := selectNarrationClaims(claims)
			candidates := countPrunable(claims)
			var fromActive, fromContested int
			for _, c := range junk {
				switch c.Status {
				case domain.ClaimStatusActive:
					fromActive++
				case domain.ClaimStatusContested:
					fromContested++
				}
			}

			fmt.Printf("prunable claims: %d (active + contested)\n", candidates)
			fmt.Printf("narration/junk:  %d (%.1f%%) — %d active, %d contested\n",
				len(junk), pct(len(junk), candidates), fromActive, fromContested)
			for i, c := range junk {
				if i >= 8 {
					fmt.Printf("  … and %d more\n", len(junk)-8)
					break
				}
				fmt.Printf("  - %s\n", truncateClaim(c.Text))
			}

			if dryRun {
				fmt.Println("\n(dry run — nothing written; re-run without --dry-run to apply)")
				return nil
			}
			if len(junk) == 0 {
				fmt.Println("\nnothing to prune.")
				return nil
			}

			if err := job.SetStatus("saving", ""); err != nil {
				return err
			}
			// Deprecate through the governed writer in one batch so every
			// transition is attributed and audited. The reason names the tool
			// so a later reader can tell an automated cleanup from a human call.
			for i := range junk {
				junk[i].Status = domain.ClaimStatusDeprecated
			}
			if _, err := w.Claims(ctx, junk, govwrite.ClaimReason{
				Reason:    "Conversational pollution, not knowledge (prune --narration)",
				ChangedBy: actor,
			}); err != nil {
				return NewSystemError(err, "deprecate narration claims")
			}
			fmt.Printf("\ndeprecated %d narration claim(s); they remain queryable with --include-history.\n", len(junk))
			return nil
		})
	if err != nil {
		exitWithMnemosError(f.Verbose, err)
	}
}

// selectNarrationClaims returns the active-or-contested claims that the
// extraction filter classifies as conversational pollution (see
// isPrunableStatus for why contested is included).
//
// Pure so the prune's core selection is testable without the store, the
// governed writer, or the CLI plumbing around it.
func selectNarrationClaims(claims []domain.Claim) []domain.Claim {
	var junk []domain.Claim
	for _, c := range claims {
		if !isPrunableStatus(c.Status) {
			continue
		}
		if extract.IsJunk(c.Text) {
			junk = append(junk, c)
		}
	}
	return junk
}

// isPrunableStatus reports whether a claim in this status is a prune candidate.
//
// Active and contested both qualify. Contested is included deliberately: these
// claims were auto-contested at INGEST by the extraction overlap check (a
// pipeline reason), not flagged by a human — junk narration contested against
// other junk narration. A census of a real brain found 17% of the contested
// pile is junk by this same filter. "Contested" there means "looks like
// something else the model also captured", not "under genuine review", so
// leaving junk in that state just keeps noise recallable. Already-deprecated
// and resolved claims are skipped: re-deprecating churns history for no gain.
func isPrunableStatus(s domain.ClaimStatus) bool {
	return s == domain.ClaimStatusActive || s == domain.ClaimStatusContested
}

// countPrunable counts claims eligible for the narration prune (active +
// contested), for the denominator in the report.
func countPrunable(claims []domain.Claim) int {
	n := 0
	for _, c := range claims {
		if isPrunableStatus(c.Status) {
			n++
		}
	}
	return n
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(n) / float64(total)
}

// handlePrune routes `mnemos prune <--narration|--session-noise> [--dry-run]`.
// The kind is a required flag rather than a default so a bare `prune` never
// guesses at a destructive operation.
func handlePrune(args []string, f Flags) {
	target, err := parsePruneArgs(args)
	if err != nil {
		exitWithMnemosError(f.Verbose, err)
		return
	}
	switch target {
	case "narration":
		pruneNarration(f.DryRun, f)
	case "session-noise":
		pruneSessionNoise(f.DryRun, f)
	case "session-local":
		pruneSessionLocal(f.DryRun, f)
	}
}

// parsePruneArgs resolves the prune target from its flags, separated from
// execution so the flag contract is testable without the process-exiting error
// path. A bare `prune` is an error, not a default: it must never guess at a
// destructive operation.
func parsePruneArgs(args []string) (target string, err error) {
	var targets []string
	for _, a := range args {
		switch a {
		case "--narration":
			targets = append(targets, "narration")
		case "--session-noise":
			targets = append(targets, "session-noise")
		case "--session-local":
			targets = append(targets, "session-local")
		default:
			return "", NewUserError("unknown prune flag %q (want --narration|--session-noise|--session-local [--dry-run])", a)
		}
	}
	switch len(targets) {
	case 0:
		return "", NewUserError("prune requires a target: --narration (deprecate stored conversational pollution) or --session-noise (drop contradiction edges between two session-local claims)")
	case 1:
		return targets[0], nil
	default:
		// They deprecate claims and delete edges respectively; running both in
		// one pass would report two sets of numbers under one job.
		return "", NewUserError("prune takes one target at a time, got %d", len(targets))
	}
}
