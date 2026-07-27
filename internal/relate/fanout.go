package relate

import (
	"sort"

	"go.klarlabs.de/mnemos/internal/domain"
)

// MaxSupportsPerClaim bounds how many `supports` edges a single claim may
// contribute when compared against the whole brain.
//
// Without a bound, DetectIncremental is O(new × existing) in emitted edges, and
// corroboration is exactly the case where nearly every pair matches: a brain
// that has discussed one subject for months answers every new claim about it
// with thousands of near-identical agreements. Measured on a production brain,
// 76,099 claims had produced 12,448,304 relationships — 99.8% of them
// `supports`, averaging 164 per claim and peaking at 4,752 for a single claim.
// The relationships table and its indexes reached 3.4 GB, which pushed every
// capture past its budget: the offset never advanced, so the same transcript
// span was re-extracted every turn and the SessionEnd hook was killed by the
// host's timeout.
//
// The nth corroboration of an already well-supported claim carries almost no
// information, so the cheapest correct fix is to keep the strongest few and
// drop the rest. This bounds growth at claims × MaxSupportsPerClaim instead of
// claims², which is what keeps the brain (and therefore capture) fast.
//
// Contradictions are deliberately NOT capped — see selectFanOut.
const MaxSupportsPerClaim = 16

// fanOutCandidate is one edge DetectIncremental could emit for the new claim
// under consideration. index refers into the existing-claims slice; score is
// the content-token overlap, and is meaningful only for `supports`.
type fanOutCandidate struct {
	index   int
	relType domain.RelationshipType
	score   int
}

// selectFanOut applies the per-claim `supports` budget and returns the edges to
// emit, in ascending candidate order so output ordering is unchanged from the
// uncapped implementation.
//
// The asymmetry is the point. `supports` is capped because corroboration is
// abundant and redundant — see MaxSupportsPerClaim. Contradictions are kept
// unconditionally: they are first-class in this codebase, they are what
// truth-maintenance and the dissonance surfaces read, and they are rare enough
// in practice not to threaten the storage bound (29,084 against 12,448,304
// supports on the brain that motivated the cap). Dropping a contradiction to
// save space would lose the signal the system exists to preserve.
//
// Selection is by descending score, breaking ties on ascending index, so the
// result depends only on the inputs — the determinism the pipeline guarantees.
func selectFanOut(candidates []fanOutCandidate, maxSupports int) []fanOutCandidate {
	supports := 0
	for _, c := range candidates {
		if c.relType == domain.RelationshipTypeSupports {
			supports++
		}
	}
	if supports <= maxSupports {
		return candidates
	}

	ranked := make([]fanOutCandidate, 0, supports)
	for _, c := range candidates {
		if c.relType == domain.RelationshipTypeSupports {
			ranked = append(ranked, c)
		}
	}
	sort.SliceStable(ranked, func(a, b int) bool {
		if ranked[a].score != ranked[b].score {
			return ranked[a].score > ranked[b].score
		}
		return ranked[a].index < ranked[b].index
	})

	keep := make(map[int]struct{}, maxSupports)
	for _, c := range ranked[:maxSupports] {
		keep[c.index] = struct{}{}
	}

	out := make([]fanOutCandidate, 0, len(candidates)-supports+maxSupports)
	for _, c := range candidates {
		if c.relType == domain.RelationshipTypeSupports {
			if _, ok := keep[c.index]; !ok {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// ExcessSupports returns the stored `supports` edges that exceed maxPerClaim
// for their source claim — the ones a capped DetectIncremental would not have
// created.
//
// The cap only bounds NEW edges, so a brain that grew before it existed stays
// slow forever without a way to apply the same rule retroactively. This is that
// way, and it deliberately reuses selectFanOut so the surviving set matches
// what detection would produce today rather than approximating it.
//
// textByID supplies claim text for scoring. An edge whose endpoints are missing
// from it cannot be scored and is never returned for deletion: unknown is not
// evidence of excess, and a caller working from a partial snapshot must not
// delete on that basis.
//
// Contradictions are never returned, for the reasons in selectFanOut.
func ExcessSupports(rels []domain.Relationship, textByID map[string]string, maxPerClaim int) []domain.Relationship {
	// Group by source claim: the budget is per claim, exactly as in detection.
	byFrom := make(map[string][]domain.Relationship)
	for _, r := range rels {
		if r.Type != domain.RelationshipTypeSupports {
			continue
		}
		if _, ok := textByID[r.FromClaimID]; !ok {
			continue
		}
		if _, ok := textByID[r.ToClaimID]; !ok {
			continue
		}
		byFrom[r.FromClaimID] = append(byFrom[r.FromClaimID], r)
	}

	// Stable claim order so the result is deterministic regardless of map
	// iteration, matching the guarantee detection makes.
	froms := make([]string, 0, len(byFrom))
	for id := range byFrom {
		froms = append(froms, id)
	}
	sort.Strings(froms)

	var drop []domain.Relationship
	for _, from := range froms {
		group := byFrom[from]
		if len(group) <= maxPerClaim {
			continue
		}
		// Sort by edge ID so the candidate order — and therefore the tie-break
		// inside selectFanOut — does not depend on how storage returned them.
		sort.SliceStable(group, func(a, b int) bool { return group[a].ID < group[b].ID })

		fromTokens, _ := contentTokensAndPolarity(textByID[from])
		candidates := make([]fanOutCandidate, len(group))
		for i, r := range group {
			toTokens, _ := contentTokensAndPolarity(textByID[r.ToClaimID])
			candidates[i] = fanOutCandidate{
				index:   i,
				relType: domain.RelationshipTypeSupports,
				score:   contentOverlap(fromTokens, toTokens),
			}
		}

		kept := make(map[int]struct{}, maxPerClaim)
		for _, c := range selectFanOut(candidates, maxPerClaim) {
			kept[c.index] = struct{}{}
		}
		for i, r := range group {
			if _, ok := kept[i]; !ok {
				drop = append(drop, r)
			}
		}
	}
	return drop
}
