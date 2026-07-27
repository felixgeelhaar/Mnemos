package mnemos

import (
	"context"
	"fmt"

	"go.klarlabs.de/mnemos/internal/trust"
)

// Recombination constants govern [Memory.Recombinations].
const (
	// RecombineSalienceFloor is the minimum salience for a claim to be a
	// recombination candidate — REM recombines what matters, not the mundane.
	RecombineSalienceFloor = 0.5
	// RecombineSimilarityFloor is the minimum topical similarity for a pair to be
	// a candidate — close enough to be worth connecting.
	RecombineSimilarityFloor = 0.34

	// RecombineMaxCandidates bounds the pair scan. Recombination is inherently
	// pairwise, so its cost is C(candidates, 2); the salience floor alone does not
	// bound that, because on a real brain most live claims clear it. Measured on a
	// topically-clustered synthetic corpus, unbounded pair scanning cost 1.3 s at
	// 1k claims, 50.6 s at 10k and ~1.25 × 10⁹ pair evaluations (tens of minutes,
	// hundreds of MB of intermediate results) at 50k — behind a single MCP or REST
	// request with no authentication cost. Capping the candidate set at 400 caps
	// the scan at C(400, 2) = 79,800 pair evaluations, ~80 ms, whatever the brain's
	// size.
	//
	// The cut is RANKED, not arbitrary: candidates are ordered by salience
	// (descending, id-tiebroken) before the cap applies, which is the criterion the
	// endpoint already selects on — "REM recombines what matters". A brain with
	// more than 400 salient live claims therefore recombines its 400 most salient
	// ones, and says so via [Bounds].
	RecombineMaxCandidates = 400

	// RecombineDefaultLimit is the result count when the caller names none.
	RecombineDefaultLimit = 20
)

// Recombination is a proposed novel connection: two high-salience claims that are
// topically related but that the waking graph never linked — the REM-stage
// juxtaposition a human (or an LLM) can turn into a schema or hypothesis.
type Recombination struct {
	ClaimA, TextA string
	ClaimB, TextB string
	Similarity    float64 // [0,1] topical (Jaccard) similarity of the pair
}

// jaccard is the symmetric token overlap of two claims in [0,1].
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for t := range a {
		if _, ok := b[t]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// Recombinations implements [Memory.Recombinations]: the REM-like recombination
// detector. It finds pairs of high-salience, currently-valid claims that are
// topically similar yet NOT directly connected in the epistemic graph — novel
// juxtapositions the waking graph never made — ranked by similarity. Deterministic
// detection; naming the emergent schema/hypothesis is left to a human or an LLM
// (these are proposals, never auto-promoted).
//
// The scan is bounded: see [RecombineMaxCandidates] for the pair-count ceiling
// and the reason it is a ranked cut. Use [memory.RecombinationsBounded] to learn
// whether the answer was truncated.
func (m *memory) Recombinations(ctx context.Context, limit int) ([]Recombination, error) {
	rep, err := m.RecombinationsBounded(ctx, limit)
	return rep.Recombinations, err
}

// RecombinationsBounded is [Memory.Recombinations] reporting the bounds it
// applied (see [BoundedCognition]).
//
// Cost: C(min(salient live claims, [RecombineMaxCandidates]), 2) pair
// evaluations — at most 79,800, independent of the size of the brain.
func (m *memory) RecombinationsBounded(ctx context.Context, limit int) (RecombinationReport, error) {
	var bounds Bounds
	limit = capLimit(&bounds, limit, RecombineDefaultLimit, MaxCognitiveResults)

	all, err := m.conn.Claims.ListAll(ctx)
	if err != nil {
		return RecombinationReport{}, fmt.Errorf("mnemos: Recombinations: list claims: %w", err)
	}
	evidence, err := m.conn.Claims.ListAllEvidence(ctx)
	if err != nil {
		return RecombinationReport{}, fmt.Errorf("mnemos: Recombinations: list evidence: %w", err)
	}
	evidenceCount := make(map[string]int, len(all))
	for _, e := range evidence {
		evidenceCount[e.ClaimID]++
	}
	// High-salience candidates. Salience is carried so the cap below can be a
	// RANKED cut rather than "whatever ListAll happened to return first".
	type cand struct {
		id, text string
		salience float64
		tokens   map[string]struct{}
	}
	var cands []cand
	for _, c := range all {
		if !c.ValidTo.IsZero() {
			continue
		}
		sal := trust.SalienceOf(c, evidenceCount[c.ID])
		if sal < RecombineSalienceFloor {
			continue
		}
		cands = append(cands, cand{id: c.ID, text: c.Text, salience: sal})
	}
	bounds.Scanned = len(cands)

	// Rank by salience, THEN cut: the 400 that matter most, not the first 400 the
	// store listed. Ties break on id so the bounded set is deterministic.
	cands = topN(cands, RecombineMaxCandidates, func(a, b cand) bool {
		if a.salience != b.salience {
			return a.salience > b.salience
		}
		return a.id < b.id
	})
	bounds.Considered = len(cands)
	if bounds.Considered < bounds.Scanned {
		bounds.cut(BoundReasonCandidateCap)
	}
	// Tokenize only what survived the cap — on a large brain this alone drops
	// tens of thousands of map allocations per request.
	for i := range cands {
		cands[i].tokens = tokenizeSet(cands[i].text)
	}

	// Already-connected pairs (either direction).
	rels, err := m.conn.Relationships.ListAll(ctx)
	if err != nil {
		return RecombinationReport{}, fmt.Errorf("mnemos: Recombinations: list relationships: %w", err)
	}
	connected := map[string]struct{}{}
	for _, r := range rels {
		connected[pairKey(r.FromClaimID, r.ToClaimID)] = struct{}{}
	}

	// Intermediate hits carry candidate INDICES, not the claim texts: at the cap
	// every pair can qualify, and 79,800 fully-materialised Recombinations is
	// ~16 MB of duplicated claim text per in-flight request. Text is attached
	// after the result cut, so the peak is ~2 MB.
	type hit struct {
		i, j int
		sim  float64
	}
	var hits []hit
	for i := 0; i < len(cands); i++ {
		for j := i + 1; j < len(cands); j++ {
			if _, ok := connected[pairKey(cands[i].id, cands[j].id)]; ok {
				continue // the waking graph already juxtaposed these
			}
			sim := jaccard(cands[i].tokens, cands[j].tokens)
			if sim < RecombineSimilarityFloor {
				continue
			}
			hits = append(hits, hit{i, j, sim})
		}
	}
	bounds.Available = len(hits)
	// Rank the pairs on the endpoint's own score, THEN cut to the limit. Ties
	// break on the pair's ids so equal-similarity answers are stable.
	hits = topN(hits, limit, func(a, b hit) bool {
		if a.sim != b.sim {
			return a.sim > b.sim
		}
		if cands[a.i].id != cands[b.i].id {
			return cands[a.i].id < cands[b.i].id
		}
		return cands[a.j].id < cands[b.j].id
	})
	if bounds.Available > len(hits) {
		bounds.cut(BoundReasonResultLimit)
	}
	out := make([]Recombination, len(hits))
	for k, h := range hits {
		out[k] = Recombination{
			ClaimA: cands[h.i].id, TextA: cands[h.i].text,
			ClaimB: cands[h.j].id, TextB: cands[h.j].text,
			Similarity: h.sim,
		}
	}
	bounds.finish(len(out))
	return RecombinationReport{Recombinations: out, Bounds: bounds}, nil
}

func pairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "\x00" + b
}
