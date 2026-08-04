package pipeline

import (
	"context"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/ports"
)

// WorkspaceMetadataKey tags an event with the workspace (project) whose session
// produced it. Capture sets it from the registered workspace that owns the
// session's cwd; every other ingestion path leaves it empty and is unaffected
// by anything in this file.
const WorkspaceMetadataKey = "workspace"

// DropCrossWorkspaceContradictions removes `contradicts` edges whose two claims
// came from DIFFERENT workspaces.
//
// The global brain federates every project a person works in, so it accumulates
// beliefs from all of them side by side. Pairwise detection has no notion of
// which project a belief is about, and the resulting conflicts are the residue
// left after #360 and #361 tightened the numeric detector:
//
//	"Icons drawn and pushed. 423 tests green"  vs  "1290 tests, green, pushed"
//	"Clean build, 158 tests pass"              vs  "148 admin tests green, build clean"
//
// Two projects reporting their own test counts. These are not fixable by any
// overlap or numeric rule, and that is the point of doing it here instead:
// the claims genuinely share topic vocabulary and really are about the same
// KIND of thing, so every threshold that rejects them also starts rejecting
// true positives. Only provenance separates them.
//
// This is the inverse of DropIntraSessionContradictions and deliberately
// mirrors it: that one drops when the two sides are the SAME session (a
// conversation moving), this one drops when they are DIFFERENT workspaces (two
// projects that were never in conversation). Both keep `supports` edges
// untouched — a cross-project corroboration is real evidence, and a project
// agreeing with another about a shared library is worth knowing.
//
// Unknown workspace on EITHER side never drops anything. Everything ingested
// before capture began tagging, and everything ingested by another path, has no
// workspace at all; the filter must positively know both sides came from two
// different projects before it suppresses a conflict. That fail-safe direction
// is what keeps this from silently hiding genuine disagreement in a brain whose
// provenance is mostly empty — which, today, it is.
func DropCrossWorkspaceContradictions(
	ctx context.Context,
	events ports.EventRepository,
	claims ports.ClaimRepository,
	rels []domain.Relationship,
	workspace string,
	newClaimIDs map[string]struct{},
) ([]domain.Relationship, int, error) {
	if workspace == "" || len(rels) == 0 {
		return rels, 0, nil
	}

	// Only claims on a contradiction edge need a lookup, keeping this
	// proportional to the conflicts found rather than to the size of the brain.
	needed := make(map[string]struct{})
	for _, r := range rels {
		if r.Type != domain.RelationshipTypeContradicts {
			continue
		}
		for _, id := range []string{r.FromClaimID, r.ToClaimID} {
			if _, isNew := newClaimIDs[id]; !isNew {
				needed[id] = struct{}{}
			}
		}
	}

	workspaceOf, err := scopeForClaims(ctx, events, claims, needed, WorkspaceMetadataKey)
	if err != nil {
		return nil, 0, err
	}
	// Everything written by this run belongs to the workspace being captured.
	for id := range newClaimIDs {
		workspaceOf[id] = workspace
	}

	kept := make([]domain.Relationship, 0, len(rels))
	dropped := 0
	for _, r := range rels {
		if r.Type == domain.RelationshipTypeContradicts {
			a, aok := workspaceOf[r.FromClaimID]
			b, bok := workspaceOf[r.ToClaimID]
			if aok && bok && a != "" && b != "" && a != b {
				dropped++
				continue
			}
		}
		kept = append(kept, r)
	}
	return kept, dropped, nil
}
