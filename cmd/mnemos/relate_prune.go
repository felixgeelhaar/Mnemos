package main

import (
	"context"
	"fmt"
	"os"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/pipeline"
	"go.klarlabs.de/mnemos/internal/relate"
	"go.klarlabs.de/mnemos/internal/workflow"
)

// `mnemos relate --prune-stale` re-derives existing relationship edges against
// the CURRENT detectors and drops the ones they would no longer produce.
//
// Relationships are derived state — events are the immutable source of truth —
// but nothing re-derived them when a detector changed, so every brain kept
// every edge any past version had ever inferred. After the contradiction-
// detector fixes, a real brain held 54,598 `contradicts` edges of which 36,892
// (67.6%) were no longer producible: pure residue from superseded heuristics.
// That residue is not inert. The recall hook surfaces a contradiction count on
// every prompt, so a brain full of stale edges trains the reader to ignore the
// warning entirely.
//
// Only contradiction edges are re-evaluated. Supports/causal edges are left
// alone: they are cheap to be wrong about, and re-deriving them would drop
// edges the LLM augmentation path (DetectCausalLLM) contributed, which the
// rule-based engine cannot reproduce offline.
func pruneStaleRelationships(dryRun bool, f Flags) {
	err := runJob("relate-prune", map[string]string{"dry_run": fmt.Sprint(dryRun)}, f.Verbose,
		func(ctx context.Context, job *workflow.Job, w *govwrite.Writer) error {
			conn := w.Conn()
			if err := job.SetStatus("loading", ""); err != nil {
				return err
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
			// Same-session edges are part of what the current pipeline would
			// NOT produce, so the pass has to know about them too — otherwise
			// it retains edges ingest would never create today.
			links, err := conn.Claims.ListAllEvidence(ctx)
			if err != nil {
				return NewSystemError(err, "load claim evidence")
			}
			events, err := conn.Events.ListAll(ctx)
			if err != nil {
				return NewSystemError(err, "load events")
			}
			sessionOf := pipeline.SessionOfClaims(links, events)

			if err := job.SetStatus("relating", ""); err != nil {
				return err
			}
			keep, dropped, orphanEdges := partitionStaleContradictions(rels, byID, sessionOf)
			orphaned := len(orphanEdges)

			fmt.Printf("relationships:      %d total\n", len(rels))
			fmt.Printf("stale contradicts:  %d (no longer produced by the current detectors)\n", len(dropped))
			if orphaned > 0 {
				fmt.Printf("orphaned endpoints: %d (claim deleted; edge dropped)\n", orphaned)
			}
			fmt.Printf("keeping:            %d\n", len(keep))
			for i, r := range dropped {
				if i >= 5 {
					fmt.Printf("  … and %d more\n", len(dropped)-5)
					break
				}
				fmt.Printf("  - %s ⇎ %s\n", truncateClaim(byID[r.FromClaimID].Text), truncateClaim(byID[r.ToClaimID].Text))
			}

			if dryRun {
				fmt.Println("\n(dry run — nothing written; re-run without --dry-run to apply)")
				return nil
			}
			if len(dropped) == 0 && orphaned == 0 {
				fmt.Println("\nnothing to prune.")
				return nil
			}

			if err := job.SetStatus("saving", ""); err != nil {
				return err
			}
			// Delete the edges this pass JUDGED, never "everything except my
			// snapshot". The two are the same only if nothing wrote in between,
			// and something usually does: re-deriving every contradiction edge
			// against the detectors takes long enough that a `relate` run or a
			// capture hook routinely lands edges mid-scan. Asking to retain
			// `keep` would delete those as stale without ever evaluating them,
			// because the pruner's snapshot predates them. Asking to drop the
			// enumerated stale edges cannot: an edge absent from the snapshot is
			// absent from the drop list, and the surviving set is re-read inside
			// the governed write.
			//
			// Through the governed writer, not the repository directly: dropping
			// tens of thousands of edges belongs in the evidence chain like any
			// other mutation.
			drop := make([]domain.Relationship, 0, len(dropped)+orphaned)
			drop = append(drop, dropped...)
			drop = append(drop, orphanEdges...)
			if _, err := w.DropRelationships(ctx, drop); err != nil {
				return NewSystemError(err, "prune relationships")
			}
			fmt.Printf("\npruned %d stale edge(s); %d retained.\n", len(drop), len(keep))
			return nil
		})
	if err != nil {
		exitWithMnemosError(f.Verbose, err)
	}
}

// partitionStaleContradictions splits relationships into those to keep and the
// contradiction edges the current detectors no longer produce. Edges whose
// endpoints no longer exist are returned separately as orphaned.
//
// Both outgoing sets are explicit edge lists, not counts: the write path
// deletes what this function names, so an edge it never saw can never be
// deleted by it.
//
// Re-evaluation runs each pair back through relate.Engine.Detect — the same
// entry point production uses — so this can never drift from the real
// detection logic the way a reimplementation would.
func partitionStaleContradictions(rels []domain.Relationship, byID map[string]domain.Claim, sessionOf map[string]string) (keep, dropped, orphaned []domain.Relationship) {
	engine := relate.NewEngine()
	keep = make([]domain.Relationship, 0, len(rels))

	for _, r := range rels {
		from, okFrom := byID[r.FromClaimID]
		to, okTo := byID[r.ToClaimID]
		if !okFrom || !okTo {
			orphaned = append(orphaned, r)
			continue
		}
		if r.Type != domain.RelationshipTypeContradicts {
			keep = append(keep, r)
			continue
		}
		// A conversation arguing with itself is not a contradiction, and the
		// ingest path stopped recording those. Claims with no session — every
		// ingestion predating session tagging — are untouched: absence of a
		// tag is not evidence they shared one.
		if sa, sb := sessionOf[r.FromClaimID], sessionOf[r.ToClaimID]; sa != "" && sa == sb {
			dropped = append(dropped, r)
			continue
		}
		if stillContradicts(engine, from, to) {
			keep = append(keep, r)
			continue
		}
		dropped = append(dropped, r)
	}
	return keep, dropped, orphaned
}

// stillContradicts reports whether the engine, given just this pair, infers a
// contradiction between them.
func stillContradicts(engine relate.Engine, a, b domain.Claim) bool {
	out, err := engine.Detect([]domain.Claim{a, b})
	if err != nil {
		// Fail closed: an evaluation error must never be read as "stale", or a
		// transient fault would delete real edges.
		fmt.Fprintf(os.Stderr, "relate-prune: evaluating %s/%s: %v (keeping)\n", a.ID, b.ID, err)
		return true
	}
	for _, r := range out {
		if r.Type == domain.RelationshipTypeContradicts {
			return true
		}
	}
	return false
}

func truncateClaim(s string) string {
	const max = 48
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
