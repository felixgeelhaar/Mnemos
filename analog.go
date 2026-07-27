package mnemos

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
)

// Structural-retrieval constants govern [Memory.AnalogousClaims].
const (
	// StructuralHops is the radius of the typed subgraph fingerprinted around a
	// claim — its local "causal skeleton".
	StructuralHops = 2

	// StructuralIterations is the number of Weisfeiler-Lehman label-propagation
	// rounds. 1–2 captures local relational structure without over-smoothing.
	StructuralIterations = 2

	// MinStructuralSimilarity is the cosine floor below which two subgraphs are not
	// considered analogous — filters incidental single-role overlap.
	MinStructuralSimilarity = 0.5

	// AnalogNodeBudget bounds the structural search. Fingerprinting is
	// neighbourhood-sized, so the total cost of scoring every candidate
	// neighbourhood grows with the graph: on a 10k-claim corpus the old
	// implementation walked 2,423 neighbourhoods and issued 7,271 store round
	// trips for one request (725 round trips at 1k — it grows linearly in the
	// corpus, three per neighbourhood). The graph is now loaded once and the walk
	// is in-memory, and the number of NODES visited across all fingerprints is
	// capped here. 200,000 node visits is roughly 80 ms of Weisfeiler-Lehman work
	// and covers a graph of a few tens of thousands of edges outright.
	//
	// The cut is RANKED: candidate neighbourhoods are visited in descending local
	// degree (then id), i.e. richest structure first — a claim with more typed
	// edges is what this endpoint is looking for, and a degree-1 leaf can never
	// fingerprint like a structured anchor. Exhausting the budget sets
	// [BoundReasonWorkBudget].
	AnalogNodeBudget = 200_000

	// AnalogDefaultLimit is the analogue count when the caller names none.
	AnalogDefaultLimit = 10
)

// Analogy is a claim whose surrounding typed subgraph is structurally similar to
// the query subgraph — the anchor of a past situation with the same relational
// shape (causal skeleton), even when the claims themselves differ.
type Analogy struct {
	ClaimID    string
	Text       string
	Similarity float64 // [0,1] Weisfeiler-Lehman structural similarity to the query subgraph
}

// typedEdge is an undirected epistemic edge with its relationship type.
type typedEdge struct {
	from, to, typ string
}

// hashLabel compresses a Weisfeiler-Lehman label to a short, stable token so
// labels don't grow unboundedly across iterations while staying comparable.
func hashLabel(s string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%x", h.Sum64())
}

// structuralFingerprint computes a Weisfeiler-Lehman fingerprint of a typed
// subgraph: a histogram of structural labels accumulated over
// [StructuralIterations] rounds of label propagation. Node content/identity is
// IGNORED — only node ROLES (nodeRoles, e.g. claim types) and EDGE TYPES shape the
// fingerprint. So two subgraphs with the same relational skeleton fingerprint
// alike even when their claims are entirely different. Deterministic.
func structuralFingerprint(nodeRoles map[string]string, edges []typedEdge) map[string]int {
	type neighbor struct{ typ, id string }
	adj := make(map[string][]neighbor, len(nodeRoles))
	for _, e := range edges {
		adj[e.from] = append(adj[e.from], neighbor{e.typ, e.to})
		adj[e.to] = append(adj[e.to], neighbor{e.typ, e.from})
	}
	labels := make(map[string]string, len(nodeRoles))
	for n, role := range nodeRoles {
		labels[n] = role
	}
	fp := map[string]int{}
	for _, l := range labels { // iteration 0: raw roles
		fp[l]++
	}
	for it := 0; it < StructuralIterations; it++ {
		next := make(map[string]string, len(labels))
		for n := range nodeRoles {
			tags := make([]string, 0, len(adj[n]))
			for _, a := range adj[n] {
				tags = append(tags, a.typ+"|"+labels[a.id])
			}
			sort.Strings(tags)
			next[n] = hashLabel(labels[n] + "#" + strings.Join(tags, ","))
		}
		labels = next
		for _, l := range labels {
			fp[l]++
		}
	}
	return fp
}

// cosineHist is cosine similarity between two label histograms in [0,1].
func cosineHist(a, b map[string]int) float64 {
	var dot, na, nb float64
	for k, v := range a {
		na += float64(v) * float64(v)
		if w, ok := b[k]; ok {
			dot += float64(v) * float64(w)
		}
	}
	for _, v := range b {
		nb += float64(v) * float64(v)
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// epistemicGraph is the claim↔claim relationship graph loaded once per request.
//
// The structural search fingerprints many neighbourhoods, and walking each one
// straight against the store cost three round trips per neighbourhood — 7,271
// round trips to answer one AnalogousClaims call on a 10k-claim brain, growing
// linearly with the corpus. Two list calls build this instead, and every walk
// afterwards is in memory.
type epistemicGraph struct {
	// adj maps a claim id to its incident edges (both directions).
	adj map[string][]typedEdge
	// roles maps a claim id to its structural role (claim type); endpoints whose
	// claim row is missing get "unknown", as the per-walk loader did.
	roles map[string]string
	// text is the claim statement, carried so rendering the answer needs no
	// further round trip.
	text map[string]string
}

// degree is the number of incident edges of id (0 for an unknown id).
func (g *epistemicGraph) degree(id string) int { return len(g.adj[id]) }

// loadEpistemicGraph reads the whole claim↔claim graph in exactly two store
// round trips, independent of how many neighbourhoods the caller will walk.
func (m *memory) loadEpistemicGraph(ctx context.Context) (*epistemicGraph, error) {
	rels, err := m.conn.Relationships.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("list relationships: %w", err)
	}
	claims, err := m.conn.Claims.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("list claims: %w", err)
	}
	g := &epistemicGraph{
		adj:   make(map[string][]typedEdge, len(rels)),
		roles: make(map[string]string, len(claims)),
		text:  make(map[string]string, len(claims)),
	}
	for _, c := range claims {
		g.roles[c.ID] = string(c.Type)
		g.text[c.ID] = c.Text
	}
	seen := make(map[string]struct{}, len(rels))
	for _, r := range rels {
		ek := r.FromClaimID + "|" + r.ToClaimID + "|" + string(r.Type)
		if _, ok := seen[ek]; ok {
			continue
		}
		seen[ek] = struct{}{}
		e := typedEdge{r.FromClaimID, r.ToClaimID, string(r.Type)}
		g.adj[r.FromClaimID] = append(g.adj[r.FromClaimID], e)
		if r.ToClaimID != r.FromClaimID {
			g.adj[r.ToClaimID] = append(g.adj[r.ToClaimID], e)
		}
		for _, id := range []string{r.FromClaimID, r.ToClaimID} {
			if _, ok := g.roles[id]; !ok {
				g.roles[id] = "unknown" // dangling endpoint (claim missing)
			}
		}
	}
	return g, nil
}

// localSubgraph builds the typed subgraph within `hops` epistemic edges of
// seedID: node roles (claim types) and the edges among them. Pure in-memory;
// deterministic (edges keep the order loadEpistemicGraph saw them in).
func (g *epistemicGraph) localSubgraph(seedID string, hops int) (map[string]string, []typedEdge) {
	visited := map[string]struct{}{seedID: {}}
	frontier := []string{seedID}
	var edges []typedEdge
	edgeSeen := map[string]struct{}{}
	for h := 0; h < hops && len(frontier) > 0; h++ {
		var next []string
		for _, n := range frontier {
			for _, e := range g.adj[n] {
				ek := e.from + "|" + e.to + "|" + e.typ
				if _, ok := edgeSeen[ek]; !ok {
					edgeSeen[ek] = struct{}{}
					edges = append(edges, e)
				}
				for _, id := range []string{e.from, e.to} {
					if _, ok := visited[id]; !ok {
						visited[id] = struct{}{}
						next = append(next, id)
					}
				}
			}
		}
		frontier = next
	}
	roles := make(map[string]string, len(visited))
	for id := range visited {
		if role, ok := g.roles[id]; ok {
			roles[id] = role
		} else {
			roles[id] = "unknown"
		}
	}
	return roles, edges
}

// AnalogousClaims implements [Memory.AnalogousClaims]: find claims whose local
// typed subgraph is structurally similar to the one around claimID — retrieval by
// relational SHAPE (Weisfeiler-Lehman), not content. One representative per
// distinct neighbourhood, strongest similarity first.
//
// Cost: two store round trips regardless of corpus size, and at most
// [AnalogNodeBudget] node visits of in-memory fingerprinting. Use
// [memory.AnalogousClaimsBounded] to learn whether the answer was truncated.
func (m *memory) AnalogousClaims(ctx context.Context, claimID string, limit int) ([]Analogy, error) {
	rep, err := m.AnalogousClaimsBounded(ctx, claimID, limit)
	return rep.Analogous, err
}

// AnalogousClaimsBounded is [Memory.AnalogousClaims] reporting the bounds it
// applied (see [BoundedCognition]).
func (m *memory) AnalogousClaimsBounded(ctx context.Context, claimID string, limit int) (AnalogyReport, error) {
	return m.analogousWithin(ctx, claimID, limit, AnalogNodeBudget)
}

// analogousWithin is AnalogousClaimsBounded with an explicit node budget. The
// seam exists so the budget-exhaustion path is testable: reaching the shipped
// [AnalogNodeBudget] would need a graph of a few hundred thousand claims, and an
// untested bound is a bound that quietly stops holding.
func (m *memory) analogousWithin(ctx context.Context, claimID string, limit, nodeBudget int) (AnalogyReport, error) {
	var bounds Bounds
	limit = capLimit(&bounds, limit, AnalogDefaultLimit, MaxCognitiveResults)

	claimID = strings.TrimSpace(claimID)
	if claimID == "" {
		return AnalogyReport{}, errors.New("mnemos: AnalogousClaims: claimID is required")
	}
	g, err := m.loadEpistemicGraph(ctx)
	if err != nil {
		return AnalogyReport{}, fmt.Errorf("mnemos: AnalogousClaims: %w", err)
	}
	anchorRoles, anchorEdges := g.localSubgraph(claimID, StructuralHops)
	if len(anchorEdges) == 0 {
		return AnalogyReport{Analogous: nil, Bounds: bounds}, nil // no structure around the anchor
	}
	anchorFP := structuralFingerprint(anchorRoles, anchorEdges)

	// Every node with structure, minus the anchor's own neighbourhood.
	candidates := make([]string, 0, len(g.adj))
	for id := range g.adj {
		if _, inAnchor := anchorRoles[id]; !inAnchor {
			candidates = append(candidates, id)
		}
	}
	bounds.Scanned = len(candidates)
	// Rank before cutting: richest local structure first. A high-degree node is
	// what a structural query is looking for, and it also covers more of the graph
	// per visit, so the budget buys the most coverage. Ties break on id, which
	// additionally makes the "one representative per neighbourhood" choice
	// deterministic — it used to depend on Go's randomised map iteration order.
	sort.SliceStable(candidates, func(i, j int) bool {
		di, dj := g.degree(candidates[i]), g.degree(candidates[j])
		if di != dj {
			return di > dj
		}
		return candidates[i] < candidates[j]
	})

	type scored struct {
		id  string
		sim float64
	}
	var out []scored
	done := map[string]struct{}{} // one representative per neighbourhood
	budget := nodeBudget
	for _, cand := range candidates {
		if _, seen := done[cand]; seen {
			continue
		}
		if budget <= 0 {
			bounds.cut(BoundReasonWorkBudget)
			break
		}
		roles, edges := g.localSubgraph(cand, StructuralHops)
		budget -= len(roles)
		if len(edges) == 0 {
			done[cand] = struct{}{}
			continue
		}
		for id := range roles { // this whole neighbourhood is now represented by cand
			done[id] = struct{}{}
		}
		sim := cosineHist(anchorFP, structuralFingerprint(roles, edges))
		if sim < MinStructuralSimilarity {
			continue
		}
		out = append(out, scored{cand, sim})
	}
	// Considered counts candidate nodes actually COVERED by a walked
	// neighbourhood, so it is comparable with Scanned: equal when the budget did
	// not bite, short of it when it did.
	for _, cand := range candidates {
		if _, ok := done[cand]; ok {
			bounds.Considered++
		}
	}
	bounds.Available = len(out)
	// Rank by structural similarity, THEN cut to the limit; id breaks ties.
	out = topN(out, limit, func(a, b scored) bool {
		if a.sim != b.sim {
			return a.sim > b.sim
		}
		return a.id < b.id
	})
	if bounds.Available > len(out) {
		bounds.cut(BoundReasonResultLimit)
	}
	analogies := make([]Analogy, len(out))
	for i, s := range out {
		analogies[i] = Analogy{ClaimID: s.id, Text: g.text[s.id], Similarity: s.sim}
	}
	bounds.finish(len(analogies))
	return AnalogyReport{Analogous: analogies, Bounds: bounds}, nil
}
