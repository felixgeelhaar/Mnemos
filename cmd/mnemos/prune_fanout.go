package main

import (
	"context"
	"fmt"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/relate"
	"go.klarlabs.de/mnemos/internal/workflow"
)

// `mnemos prune --fan-out [--dry-run]` drops the `supports` edges that exceed
// relate.MaxSupportsPerClaim for their source claim.
//
// The cap in `relate` bounds edges created from now on, which does nothing for
// a brain that grew before it existed — and those are exactly the brains that
// need it. Measured on a production brain: 76,099 claims carrying 12,448,304
// relationships, 99.8% of them `supports`, for 3.4 GB of table and indexes.
// Every capture then exceeded its budget, so the transcript offset never
// advanced and the same span was re-extracted every turn until the SessionEnd
// hook was killed by the host timeout. `relate --prune-stale` cannot reach this
// pile: it re-derives contradictions and keeps every other edge by design.
//
// It removes EDGES ONLY, never claims, and only the surplus above the cap — the
// strongest corroborations for each claim are kept, chosen by the same rule
// detection now applies, so the result matches what a fresh `relate` pass would
// produce rather than approximating it.
func pruneFanOut(dryRun bool, f Flags) {
	err := runJob("prune-fan-out", map[string]string{"dry_run": fmt.Sprint(dryRun)}, f.Verbose,
		func(ctx context.Context, job *workflow.Job, w *govwrite.Writer) error {
			conn := w.Conn()
			if err := job.SetStatus("loading", ""); err != nil {
				return err
			}

			claims, err := conn.Claims.ListAll(ctx)
			if err != nil {
				return NewSystemError(err, "load claims")
			}
			rels, err := conn.Relationships.ListAll(ctx)
			if err != nil {
				return NewSystemError(err, "load relationships")
			}

			textByID := make(map[string]string, len(claims))
			for _, c := range claims {
				textByID[c.ID] = c.Text
			}

			supports := 0
			for _, r := range rels {
				if r.Type == domain.RelationshipTypeSupports {
					supports++
				}
			}

			drop := relate.ExcessSupports(rels, textByID, relate.MaxSupportsPerClaim)
			fmt.Printf("claims:          %d\n", len(claims))
			fmt.Printf("relationships:   %d (%d supports)\n", len(rels), supports)
			fmt.Printf("over the cap of %d per claim: %d edge(s) to drop (%.1f%% of supports)\n",
				relate.MaxSupportsPerClaim, len(drop), pct(len(drop), supports))

			if len(drop) == 0 {
				fmt.Println("nothing over the cap; nothing to do.")
				return nil
			}

			// Name the worst offenders: a bare count says nothing about whether
			// the pass is about to touch a few claims or the whole brain.
			worst := map[string]int{}
			for _, r := range drop {
				worst[r.FromClaimID]++
			}
			shown := 0
			for _, c := range claims {
				n, ok := worst[c.ID]
				if !ok || n < relate.MaxSupportsPerClaim {
					continue
				}
				if shown >= 5 {
					break
				}
				fmt.Printf("  - %s (%d dropped)\n", truncateClaim(c.Text), n)
				shown++
			}
			fmt.Printf("claims affected: %d\n", len(worst))
			// The governed writer rewrites each affected claim's edge set
			// individually so the drop stays auditable and restorable. That is
			// the right trade for a one-time repair, but on a large brain it is
			// minutes, not seconds — say so rather than letting it look hung.
			fmt.Printf("this rewrites each affected claim through the write audit — expect roughly %d-%d minute(s)\n",
				1+len(worst)/1200, 1+len(worst)/400)

			if dryRun {
				fmt.Println("\n(dry run — nothing written; re-run without --dry-run to apply)")
				return nil
			}
			if err := job.SetStatus("saving", ""); err != nil {
				return err
			}
			// Through the governed writer, like every other prune: edge deletion
			// is auditable and must not bypass the write audit.
			if _, err := w.DropRelationships(ctx, drop); err != nil {
				return NewSystemError(err, "prune relationships")
			}
			fmt.Printf("\npruned %d excess supports edge(s); %d retained.\n", len(drop), len(rels)-len(drop))
			fmt.Println("run `mnemos rebuild --vacuum` or `sqlite3 <brain> 'VACUUM;'` to reclaim the disk space.")
			return nil
		})
	if err != nil {
		exitWithMnemosError(f.Verbose, err)
	}
}
