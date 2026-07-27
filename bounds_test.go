package mnemos

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"

	_ "go.klarlabs.de/mnemos/internal/store/memory"
)

// Tests for the bounded cognitive reads. Each endpoint gets three assertions,
// which are the three ways a bound goes wrong:
//
//  1. the bound HOLDS — the work and the response are capped whatever the
//     corpus size;
//  2. the kept subset is the RANKED-BEST one, not an arbitrary prefix — every
//     corpus below is seeded so that the store's natural listing order puts the
//     WORST items first, so a cut-then-rank implementation fails;
//  3. the truncation is VISIBLE — Bounds says it was cut, why, and how much was
//     left behind.
//
// See TestRecombinations_MutationGuard for the note on removing the bound.

// boundsMem is a fresh in-memory brain in its own namespace.
func boundsMem(t *testing.T, ns string) *memory {
	t.Helper()
	for _, k := range []string{"MNEMOS_STORAGE", "MNEMOS_MODE", "MNEMOS_LLM_PROVIDER", "MNEMOS_API_KEY"} {
		t.Setenv(k, "")
	}
	mem, err := New(WithStorage("memory://?namespace="+ns), WithPassiveMode())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	return mem.(*memory)
}

// putClaims upserts claims in the given order; the memory store's ListAll
// preserves insertion order, which is what lets these tests distinguish a
// ranked cut from a prefix cut.
func putClaims(t *testing.T, m *memory, cs []domain.Claim) {
	t.Helper()
	if err := m.conn.Claims.Upsert(context.Background(), cs); err != nil {
		t.Fatalf("upsert claims: %v", err)
	}
}

// hasReason reports whether b recorded the given bound reason.
func hasReason(b Bounds, reason string) bool {
	for _, r := range b.Reasons {
		if r == reason {
			return true
		}
	}
	return false
}

// assertTruncationVisible is the shared "a truncated answer must not look
// complete" check.
func assertTruncationVisible(t *testing.T, b Bounds, wantReason string, returned int) {
	t.Helper()
	if !b.Truncated {
		t.Fatalf("truncated answer reported Truncated=false: %+v", b)
	}
	if !hasReason(b, wantReason) {
		t.Errorf("Reasons = %v, want to contain %q", b.Reasons, wantReason)
	}
	if b.Notice == "" {
		t.Error("Notice is empty; a truncated answer must say so in words")
	}
	if b.Available < returned {
		t.Errorf("Available = %d < returned %d — the caller cannot tell how much was left behind", b.Available, returned)
	}
}

// ---------------------------------------------------------------------------
// Recombinations (P6): the O(n²) pair scan
// ---------------------------------------------------------------------------

// recombineCorpus seeds low+high salience claims that are all topically similar
// (so every pair clears RecombineSimilarityFloor and the pair scan is
// saturated). The LOW-salience claims are inserted FIRST and sort first by id,
// so an implementation that cut the candidate list before ranking it would keep
// exactly the wrong ones.
func recombineCorpus(t *testing.T, m *memory, low, high int) (lowIDs, highIDs map[string]bool) {
	t.Helper()
	now := time.Now().UTC()
	lowIDs, highIDs = map[string]bool{}, map[string]bool{}
	// A shared 12-word topic vocabulary; each claim takes 8 of them, so any two
	// claims overlap well above the 0.34 floor.
	vocab := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot",
		"golf", "hotel", "india", "juliett", "kilo", "lima"}
	text := func(i int) string {
		w := make([]string, 0, 8)
		for k := 0; k < 8; k++ {
			w = append(w, vocab[(i+k)%len(vocab)])
		}
		return strings.Join(w, " ")
	}
	var batch []domain.Claim
	for i := 0; i < low; i++ {
		id := fmt.Sprintf("a-low-%04d", i) // sorts first, inserted first
		lowIDs[id] = true
		batch = append(batch, domain.Claim{
			ID: id, Text: text(i), Type: domain.ClaimTypeDecision,
			// Salience = 0.35·confidence + 0.25·typePrior(decision=1.0) = 0.5125:
			// above RecombineSalienceFloor, so these ARE candidates — but the
			// least salient ones, so the ranked cut must drop them first.
			Confidence: 0.75,
			Status:     domain.ClaimStatusActive, CreatedAt: now, ValidFrom: now,
		})
	}
	for i := 0; i < high; i++ {
		id := fmt.Sprintf("z-high-%04d", i)
		highIDs[id] = true
		batch = append(batch, domain.Claim{
			ID: id, Text: text(i), Type: domain.ClaimTypeDecision,
			Confidence: 0.99, // salience 0.5965
			Status:     domain.ClaimStatusActive, CreatedAt: now, ValidFrom: now,
		})
	}
	putClaims(t, m, batch)
	return lowIDs, highIDs
}

// TestRecombinations_CandidateCapHoldsAndKeepsTheBest is the P6 bound: the pair
// scan is capped at C(RecombineMaxCandidates, 2) whatever the brain's size, and
// the candidates kept are the most SALIENT ones rather than the first ones the
// store listed.
//
// MUTATION GUARD: delete the `cands = topN(...)` cut in
// (*memory).RecombinationsBounded and this test fails twice over — Considered
// becomes 500 (bound gone) and low-salience claims appear in the answer.
func TestRecombinations_CandidateCapHoldsAndKeepsTheBest(t *testing.T) {
	m := boundsMem(t, "recombine_cap")
	const low, high = 100, RecombineMaxCandidates
	lowIDs, _ := recombineCorpus(t, m, low, high)

	rep, err := m.RecombinationsBounded(context.Background(), 50)
	if err != nil {
		t.Fatalf("RecombinationsBounded: %v", err)
	}

	// 1. the bound holds.
	if rep.Bounds.Considered > RecombineMaxCandidates {
		t.Fatalf("Considered = %d, want ≤ %d — the pair scan is C(Considered,2) so this IS the work bound",
			rep.Bounds.Considered, RecombineMaxCandidates)
	}
	if rep.Bounds.Scanned != low+high {
		t.Errorf("Scanned = %d, want %d (every seeded claim clears the salience floor)", rep.Bounds.Scanned, low+high)
	}
	if rep.Bounds.Considered != RecombineMaxCandidates {
		t.Errorf("Considered = %d, want exactly the cap %d", rep.Bounds.Considered, RecombineMaxCandidates)
	}

	// 2. it kept the best-ranked candidates, not a prefix. The low-salience
	// claims were inserted first and sort first by id, so a prefix cut keeps
	// them.
	if len(rep.Recombinations) == 0 {
		t.Fatal("no recombinations; the corpus is meant to saturate the pair scan")
	}
	for _, r := range rep.Recombinations {
		if lowIDs[r.ClaimA] || lowIDs[r.ClaimB] {
			t.Fatalf("low-salience claim in the answer (%s,%s): the candidate cut is a prefix, not a ranking",
				r.ClaimA, r.ClaimB)
		}
	}

	// 3. truncation is visible.
	assertTruncationVisible(t, rep.Bounds, BoundReasonCandidateCap, len(rep.Recombinations))
	if !strings.Contains(rep.Bounds.Notice, "candidates") {
		t.Errorf("Notice %q does not mention the candidate cut", rep.Bounds.Notice)
	}
}

// TestRecombinations_ResultLimitReturnsTheStrongestPairs checks the second cut:
// among the considered candidates, the pairs returned are the highest-similarity
// ones and the caller is told how many qualified.
func TestRecombinations_ResultLimitReturnsTheStrongestPairs(t *testing.T) {
	m := boundsMem(t, "recombine_limit")
	now := time.Now().UTC()
	// Two identical-text claims (similarity 1.0) plus a cluster of merely-similar
	// ones. The identical pair is inserted LAST, so a prefix cut misses it.
	var batch []domain.Claim
	for i := 0; i < 20; i++ {
		batch = append(batch, domain.Claim{
			ID: fmt.Sprintf("a-mid-%02d", i), Type: domain.ClaimTypeDecision, Confidence: 0.9,
			Text:   strings.Join([]string{"alpha", "bravo", "charlie", "delta", fmt.Sprintf("tag%02d", i), fmt.Sprintf("tag%02d", i+1)}, " "),
			Status: domain.ClaimStatusActive, CreatedAt: now, ValidFrom: now,
		})
	}
	for _, id := range []string{"z-twin-1", "z-twin-2"} {
		batch = append(batch, domain.Claim{
			ID: id, Type: domain.ClaimTypeDecision, Confidence: 0.9,
			Text:   "identical statement about the payments rollback runbook",
			Status: domain.ClaimStatusActive, CreatedAt: now, ValidFrom: now,
		})
	}
	putClaims(t, m, batch)

	rep, err := m.RecombinationsBounded(context.Background(), 1)
	if err != nil {
		t.Fatalf("RecombinationsBounded: %v", err)
	}
	if len(rep.Recombinations) != 1 {
		t.Fatalf("got %d results, want exactly the limit 1", len(rep.Recombinations))
	}
	got := rep.Recombinations[0]
	if got.ClaimA != "z-twin-1" || got.ClaimB != "z-twin-2" {
		t.Errorf("limit-1 answer = (%s,%s) sim=%.3f, want the strongest pair (z-twin-1,z-twin-2) sim=1.0 — the cut is not ranked",
			got.ClaimA, got.ClaimB, got.Similarity)
	}
	assertTruncationVisible(t, rep.Bounds, BoundReasonResultLimit, 1)
	if rep.Bounds.Available <= 1 {
		t.Errorf("Available = %d, want the full qualifying-pair count", rep.Bounds.Available)
	}
}

// TestRecombinations_CallerLimitIsCapped: a caller cannot buy a bigger response
// by asking for one.
func TestRecombinations_CallerLimitIsCapped(t *testing.T) {
	m := boundsMem(t, "recombine_limitcap")
	recombineCorpus(t, m, 0, 60) // C(60,2)=1770 qualifying pairs

	rep, err := m.RecombinationsBounded(context.Background(), 1_000_000)
	if err != nil {
		t.Fatalf("RecombinationsBounded: %v", err)
	}
	if rep.Bounds.Limit != MaxCognitiveResults {
		t.Errorf("effective Limit = %d, want %d", rep.Bounds.Limit, MaxCognitiveResults)
	}
	if len(rep.Recombinations) > MaxCognitiveResults {
		t.Fatalf("got %d results, want ≤ %d", len(rep.Recombinations), MaxCognitiveResults)
	}
	assertTruncationVisible(t, rep.Bounds, BoundReasonLimitCapped, len(rep.Recombinations))
}

// TestRecombinations_UnboundedCorpusStaysCheap is the shape of the P6 finding:
// the work must stop growing with the brain. Doubling a saturated corpus must
// not change the number of pairs considered.
func TestRecombinations_UnboundedCorpusStaysCheap(t *testing.T) {
	considered := func(ns string, n int) int {
		m := boundsMem(t, ns)
		recombineCorpus(t, m, 0, n)
		rep, err := m.RecombinationsBounded(context.Background(), 10)
		if err != nil {
			t.Fatalf("RecombinationsBounded: %v", err)
		}
		return rep.Bounds.Considered
	}
	small := considered("recombine_growth_small", RecombineMaxCandidates+50)
	big := considered("recombine_growth_big", RecombineMaxCandidates*3)
	if small != big || small != RecombineMaxCandidates {
		t.Fatalf("considered candidates grew with the corpus: %d then %d, want %d both times",
			small, big, RecombineMaxCandidates)
	}
}

// TestRecombinations_CompleteAnswerIsNotFlaggedTruncated guards the opposite
// error: crying truncation on a complete answer is just as useless.
func TestRecombinations_CompleteAnswerIsNotFlaggedTruncated(t *testing.T) {
	m := boundsMem(t, "recombine_complete")
	recombineCorpus(t, m, 0, 3) // C(3,2)=3 pairs, all returned

	rep, err := m.RecombinationsBounded(context.Background(), 100)
	if err != nil {
		t.Fatalf("RecombinationsBounded: %v", err)
	}
	if rep.Bounds.Truncated {
		t.Errorf("complete answer flagged truncated: %+v", rep.Bounds)
	}
	if rep.Bounds.Notice != "" {
		t.Errorf("complete answer carries a notice: %q", rep.Bounds.Notice)
	}
	if len(rep.Recombinations) != rep.Bounds.Available {
		t.Errorf("returned %d of %d available on a complete answer", len(rep.Recombinations), rep.Bounds.Available)
	}
}

// ---------------------------------------------------------------------------
// AnalogousClaims (P5): N sequential round trips
// ---------------------------------------------------------------------------

// analogCorpus seeds `chains` disjoint 5-claim supports chains, so there are
// many distinct neighbourhoods to fingerprint.
func analogCorpus(t *testing.T, m *memory, chains int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	types := []domain.ClaimType{domain.ClaimTypeDecision, domain.ClaimTypeFact, domain.ClaimTypeHypothesis}
	var cs []domain.Claim
	var rs []domain.Relationship
	for c := 0; c < chains; c++ {
		for i := 0; i < 5; i++ {
			cs = append(cs, domain.Claim{
				ID: fmt.Sprintf("n%04d-%d", c, i), Text: fmt.Sprintf("chain %d node %d", c, i),
				Type: types[i%len(types)], Confidence: 0.8,
				Status: domain.ClaimStatusActive, CreatedAt: now, ValidFrom: now,
			})
			if i > 0 {
				rs = append(rs, domain.Relationship{
					ID: fmt.Sprintf("e%04d-%d", c, i), Type: domain.RelationshipTypeSupports,
					FromClaimID: fmt.Sprintf("n%04d-%d", c, i-1), ToClaimID: fmt.Sprintf("n%04d-%d", c, i),
					CreatedAt: now,
				})
			}
		}
	}
	if err := m.conn.Claims.Upsert(ctx, cs); err != nil {
		t.Fatalf("upsert claims: %v", err)
	}
	if err := m.conn.Relationships.Upsert(ctx, rs); err != nil {
		t.Fatalf("upsert relationships: %v", err)
	}
}

// TestAnalogousClaims_RoundTripsDoNotGrowWithTheCorpus is the P5 bound. The old
// implementation issued three store calls per candidate neighbourhood — 725 on
// a 1k-claim brain, 7,271 on a 10k one. It must now be a constant.
//
// MUTATION GUARD: put the per-candidate m.localSubgraph(ctx, …) walk back and
// this test fails immediately; the count becomes ~3 per neighbourhood.
func TestAnalogousClaims_RoundTripsDoNotGrowWithTheCorpus(t *testing.T) {
	roundTrips := func(ns string, chains int) int64 {
		m := boundsMem(t, ns)
		analogCorpus(t, m, chains)
		counts := instrument(m)
		if _, err := m.AnalogousClaims(context.Background(), "n0000-2", 10); err != nil {
			t.Fatalf("AnalogousClaims: %v", err)
		}
		return counts.total()
	}
	small := roundTrips("analog_rt_small", 10) // 50 claims
	big := roundTrips("analog_rt_big", 400)    // 2,000 claims, 40x the neighbourhoods
	const wantRoundTrips = 2                   // rels.ListAll + claims.ListAll
	if small != wantRoundTrips || big != wantRoundTrips {
		t.Fatalf("store round trips = %d (50 claims) and %d (2,000 claims), want %d both times — "+
			"the walk is back to querying per neighbourhood", small, big, wantRoundTrips)
	}
}

// TestAnalogousClaims_ResultLimitReturnsTheClosestShapes checks the ranked cut
// and the truncation report on the structural search.
func TestAnalogousClaims_ResultLimitReturnsTheClosestShapes(t *testing.T) {
	m := boundsMem(t, "analog_limit")
	analogCorpus(t, m, 40) // 40 identically-shaped chains

	rep, err := m.AnalogousClaimsBounded(context.Background(), "n0000-2", 5)
	if err != nil {
		t.Fatalf("AnalogousClaimsBounded: %v", err)
	}
	if len(rep.Analogous) != 5 {
		t.Fatalf("got %d analogues, want the limit 5", len(rep.Analogous))
	}
	// Descending similarity — the cut happened after the ranking.
	for i := 1; i < len(rep.Analogous); i++ {
		if rep.Analogous[i-1].Similarity < rep.Analogous[i].Similarity {
			t.Fatalf("analogues are not ranked best-first: %v", rep.Analogous)
		}
	}
	if rep.Analogous[0].Text == "" {
		t.Error("analogue text is empty; the answer must not need another lookup to be readable")
	}
	assertTruncationVisible(t, rep.Bounds, BoundReasonResultLimit, len(rep.Analogous))
}

// TestAnalogousClaims_RepresentativeIsDeterministic pins the fix to a latent
// non-determinism: the "one representative per neighbourhood" choice used to
// follow Go's randomised map iteration order, so the same brain could answer
// differently twice in a row.
func TestAnalogousClaims_RepresentativeIsDeterministic(t *testing.T) {
	m := boundsMem(t, "analog_determinism")
	analogCorpus(t, m, 30)

	var first []Analogy
	for i := 0; i < 8; i++ {
		got, err := m.AnalogousClaims(context.Background(), "n0000-2", 10)
		if err != nil {
			t.Fatalf("AnalogousClaims: %v", err)
		}
		if i == 0 {
			first = got
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("run %d returned %d analogues, run 0 returned %d", i, len(got), len(first))
		}
		for k := range got {
			if got[k].ClaimID != first[k].ClaimID {
				t.Fatalf("run %d differs at %d: %s vs %s", i, k, got[k].ClaimID, first[k].ClaimID)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// KnowledgeGaps
// ---------------------------------------------------------------------------

// gapCorpus seeds n unresolved hypotheses. Salience rises with the index while
// the id falls, so the highest-scoring gaps are last in listing order.
func gapCorpus(t *testing.T, m *memory, n int) {
	t.Helper()
	now := time.Now().UTC()
	var cs []domain.Claim
	for i := 0; i < n; i++ {
		cs = append(cs, domain.Claim{
			ID: fmt.Sprintf("g%04d", i), Text: fmt.Sprintf("hypothesis %d", i),
			Type: domain.ClaimTypeHypothesis, Confidence: float64(i+1) / float64(n+1),
			TrustScore: 0.1, Status: domain.ClaimStatusActive, CreatedAt: now, ValidFrom: now,
		})
	}
	putClaims(t, m, cs)
}

func TestKnowledgeGaps_LimitIsCappedAndKeepsTheBiggestGaps(t *testing.T) {
	m := boundsMem(t, "gaps_cap")
	const n = 400
	gapCorpus(t, m, n)

	rep, err := m.KnowledgeGapsBounded(context.Background(), 1_000_000)
	if err != nil {
		t.Fatalf("KnowledgeGapsBounded: %v", err)
	}
	if len(rep.Gaps) > MaxCognitiveResults {
		t.Fatalf("got %d gaps, want ≤ %d", len(rep.Gaps), MaxCognitiveResults)
	}
	if rep.Bounds.Limit != MaxCognitiveResults {
		t.Errorf("effective Limit = %d, want %d", rep.Bounds.Limit, MaxCognitiveResults)
	}
	if rep.Bounds.Available != n {
		t.Errorf("Available = %d, want the true gap count %d", rep.Bounds.Available, n)
	}
	// Ranked-best: scores descending, and the top gap is the most salient one
	// (highest confidence, seeded LAST) rather than the first listed.
	for i := 1; i < len(rep.Gaps); i++ {
		if rep.Gaps[i-1].Score < rep.Gaps[i].Score {
			t.Fatalf("gaps are not ranked best-first at %d", i)
		}
	}
	if want := fmt.Sprintf("g%04d", n-1); rep.Gaps[0].ClaimID != want {
		t.Errorf("top gap = %s, want %s — the cut is a prefix, not a ranking", rep.Gaps[0].ClaimID, want)
	}
	assertTruncationVisible(t, rep.Bounds, BoundReasonLimitCapped, len(rep.Gaps))
	if !hasReason(rep.Bounds, BoundReasonResultLimit) {
		t.Errorf("Reasons = %v, want to also record %q", rep.Bounds.Reasons, BoundReasonResultLimit)
	}
}

// ---------------------------------------------------------------------------
// WhoKnows
// ---------------------------------------------------------------------------

func TestWhoKnows_LimitIsCappedAndKeepsTheStrongestExperts(t *testing.T) {
	m := boundsMem(t, "whoknows_cap")
	now := time.Now().UTC()
	// 300 authors; the later ones (which list last) own more matching claims, so
	// they must rank first.
	var cs []domain.Claim
	const authors = 300
	for a := 0; a < authors; a++ {
		for k := 0; k <= a%5; k++ {
			cs = append(cs, domain.Claim{
				ID: fmt.Sprintf("w%04d-%d", a, k), Text: "rollback the payments deploy",
				Type: domain.ClaimTypeFact, Confidence: 0.8, TrustScore: 0.5 + 0.4*float64(a)/float64(authors),
				Status: domain.ClaimStatusActive, CreatedBy: fmt.Sprintf("worker-%04d", a),
				CreatedAt: now, ValidFrom: now,
			})
		}
	}
	putClaims(t, m, cs)

	rep, err := m.WhoKnowsBounded(context.Background(), "rollback payments deploy", 1_000_000)
	if err != nil {
		t.Fatalf("WhoKnowsBounded: %v", err)
	}
	if len(rep.Experts) > MaxCognitiveResults {
		t.Fatalf("got %d experts, want ≤ %d", len(rep.Experts), MaxCognitiveResults)
	}
	if rep.Bounds.Available != authors {
		t.Errorf("Available = %d, want the true expert count %d", rep.Bounds.Available, authors)
	}
	for i := 1; i < len(rep.Experts); i++ {
		si := rep.Experts[i-1].Affinity * rep.Experts[i-1].Reliability
		sj := rep.Experts[i].Affinity * rep.Experts[i].Reliability
		if si < sj {
			t.Fatalf("experts are not ranked best-first at %d", i)
		}
	}
	if rep.Experts[0].ClaimCount != 5 {
		t.Errorf("top expert owns %d matching claims, want the maximum 5 — the cut is a prefix, not a ranking",
			rep.Experts[0].ClaimCount)
	}
	assertTruncationVisible(t, rep.Bounds, BoundReasonLimitCapped, len(rep.Experts))
}

// ---------------------------------------------------------------------------
// Hypercorrections
// ---------------------------------------------------------------------------

// hyperCorpus seeds n contradiction pairs whose contradicted side is
// established. Trust RISES with the index while the id falls, so the most
// alarming alerts are last in listing order.
func hyperCorpus(t *testing.T, m *memory, n int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	var cs []domain.Claim
	var rs []domain.Relationship
	for i := 0; i < n; i++ {
		cs = append(cs,
			domain.Claim{
				ID: fmt.Sprintf("h%04d-established", i), Text: fmt.Sprintf("established belief %d", i),
				Type: domain.ClaimTypeFact, Confidence: 0.9,
				TrustScore: 0.7 + 0.3*float64(i)/float64(n),
				Status:     domain.ClaimStatusActive, CreatedAt: now, ValidFrom: now,
			},
			domain.Claim{
				ID: fmt.Sprintf("h%04d-challenger", i), Text: fmt.Sprintf("challenging belief %d", i),
				Type: domain.ClaimTypeFact, Confidence: 0.5, TrustScore: 0.2,
				Status: domain.ClaimStatusActive, CreatedAt: now, ValidFrom: now,
			})
		rs = append(rs, domain.Relationship{
			ID: fmt.Sprintf("hc%04d", i), Type: domain.RelationshipTypeContradicts,
			FromClaimID: fmt.Sprintf("h%04d-established", i), ToClaimID: fmt.Sprintf("h%04d-challenger", i),
			CreatedAt: now,
		})
	}
	if err := m.conn.Claims.Upsert(ctx, cs); err != nil {
		t.Fatalf("upsert claims: %v", err)
	}
	if err := m.conn.Relationships.Upsert(ctx, rs); err != nil {
		t.Fatalf("upsert relationships: %v", err)
	}
}

// TestHypercorrections_ResponseIsBoundedAndMostEstablishedFirst: the surface had
// no limit at all, so its response grew with the corpus.
func TestHypercorrections_ResponseIsBoundedAndMostEstablishedFirst(t *testing.T) {
	m := boundsMem(t, "hyper_cap")
	const n = 300
	hyperCorpus(t, m, n)

	got, err := m.Hypercorrections(context.Background())
	if err != nil {
		t.Fatalf("Hypercorrections: %v", err)
	}
	if len(got) != HypercorrectionDefaultLimit {
		t.Fatalf("got %d alerts from a %d-alert brain, want the default page %d",
			len(got), n, HypercorrectionDefaultLimit)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].ContradictedTrust < got[i].ContradictedTrust {
			t.Fatalf("alerts are not most-established-first at %d", i)
		}
	}
	if want := fmt.Sprintf("h%04d-established", n-1); got[0].ContradictedClaimID != want {
		t.Errorf("top alert = %s, want %s — the cut is a prefix, not a ranking", got[0].ContradictedClaimID, want)
	}

	rep, err := m.HypercorrectionsBounded(context.Background(), 0)
	if err != nil {
		t.Fatalf("HypercorrectionsBounded: %v", err)
	}
	if rep.Bounds.Available != n {
		t.Errorf("Available = %d, want the true alert count %d", rep.Bounds.Available, n)
	}
	assertTruncationVisible(t, rep.Bounds, BoundReasonResultLimit, len(rep.Hypercorrections))
}

// TestPredictiveError_DissonanceUsesTheCompleteAlertCount guards the regression
// the Hypercorrections cap could have introduced: the dissonance level is a
// RATE, so reading the capped page would have floored it once a brain grew past
// HypercorrectionDefaultLimit contradictions.
func TestPredictiveError_DissonanceUsesTheCompleteAlertCount(t *testing.T) {
	m := boundsMem(t, "hyper_rate")
	const n = 120 // > HypercorrectionDefaultLimit
	hyperCorpus(t, m, n)

	pe, err := m.PredictiveError(context.Background())
	if err != nil {
		t.Fatalf("PredictiveError: %v", err)
	}
	var basis string
	for _, l := range pe.Levels {
		if l.Level == "dissonance" {
			basis = l.Basis
		}
	}
	if !strings.Contains(basis, fmt.Sprintf("%d active hypercorrection", n)) {
		t.Errorf("dissonance basis = %q, want the complete count %d — the rate is reading the capped page", basis, n)
	}
}

// ---------------------------------------------------------------------------
// Scan
// ---------------------------------------------------------------------------

// TestScan_ZeroLimitNoLongerDumpsTheWholeBrain: Limit 0 used to mean
// "everything", which over MCP or HTTP is a full corpus dump behind one line.
func TestScan_ZeroLimitNoLongerDumpsTheWholeBrain(t *testing.T) {
	m := boundsMem(t, "scan_cap")
	now := time.Now().UTC()
	n := ScanMaxResults + 250
	cs := make([]domain.Claim, 0, n)
	for i := 0; i < n; i++ {
		cs = append(cs, domain.Claim{
			ID: fmt.Sprintf("s%05d", i), Text: fmt.Sprintf("claim %d", i),
			Type: domain.ClaimTypeFact, Confidence: 0.5, Status: domain.ClaimStatusActive,
			CreatedAt: now, ValidFrom: now.Add(-time.Duration(n-i) * time.Minute),
		})
	}
	putClaims(t, m, cs)

	rep, err := m.ScanBounded(context.Background(), ScanQuery{})
	if err != nil {
		t.Fatalf("ScanBounded: %v", err)
	}
	if len(rep.Claims) != ScanMaxResults {
		t.Fatalf("Limit 0 returned %d claims from a %d-claim brain, want the cap %d", len(rep.Claims), n, ScanMaxResults)
	}
	if rep.Bounds.Available != n {
		t.Errorf("Available = %d, want the true match count %d", rep.Bounds.Available, n)
	}
	// Ranked-best: earliest ValidFrom first — s00000 is the oldest.
	if rep.Claims[0].ID != "s00000" {
		t.Errorf("first claim = %s, want s00000 (earliest ValidFrom) — the cut is not ordered", rep.Claims[0].ID)
	}
	for i := 1; i < len(rep.Claims); i++ {
		if rep.Claims[i].ValidFrom.Before(rep.Claims[i-1].ValidFrom) {
			t.Fatalf("scan is not ordered by ValidFrom at %d", i)
		}
	}
	assertTruncationVisible(t, rep.Bounds, BoundReasonResultLimit, len(rep.Claims))
}

// TestScan_ExplicitLimitIsCapped: naming a huge Limit does not buy a dump either.
func TestScan_ExplicitLimitIsCapped(t *testing.T) {
	m := boundsMem(t, "scan_explicit")
	now := time.Now().UTC()
	cs := make([]domain.Claim, 0, ScanMaxResults+10)
	for i := 0; i < ScanMaxResults+10; i++ {
		cs = append(cs, domain.Claim{
			ID: fmt.Sprintf("s%05d", i), Text: "x", Type: domain.ClaimTypeFact, Confidence: 0.5,
			Status: domain.ClaimStatusActive, CreatedAt: now, ValidFrom: now,
		})
	}
	putClaims(t, m, cs)

	rep, err := m.ScanBounded(context.Background(), ScanQuery{Limit: 500_000})
	if err != nil {
		t.Fatalf("ScanBounded: %v", err)
	}
	if len(rep.Claims) != ScanMaxResults {
		t.Fatalf("got %d claims, want the cap %d", len(rep.Claims), ScanMaxResults)
	}
	assertTruncationVisible(t, rep.Bounds, BoundReasonLimitCapped, len(rep.Claims))
}

// ---------------------------------------------------------------------------
// capability wiring
// ---------------------------------------------------------------------------

// TestBoundedCognition_EveryReadReportsBounds makes an unbounded new cognitive
// read a compile-or-test failure rather than a silently unbounded surface.
func TestBoundedCognition_EveryReadReportsBounds(t *testing.T) {
	m := boundsMem(t, "bounded_capability")
	var mem Memory = m
	bc, ok := mem.(BoundedCognition)
	if !ok {
		t.Fatal("*memory no longer implements BoundedCognition; the delivery adapters lose truncation visibility")
	}
	ctx := context.Background()
	if _, err := bc.RecombinationsBounded(ctx, 5); err != nil {
		t.Errorf("RecombinationsBounded: %v", err)
	}
	if _, err := bc.KnowledgeGapsBounded(ctx, 5); err != nil {
		t.Errorf("KnowledgeGapsBounded: %v", err)
	}
	if _, err := bc.WhoKnowsBounded(ctx, "anything", 5); err != nil {
		t.Errorf("WhoKnowsBounded: %v", err)
	}
	if _, err := bc.HypercorrectionsBounded(ctx, 5); err != nil {
		t.Errorf("HypercorrectionsBounded: %v", err)
	}
	if _, err := bc.ScanBounded(ctx, ScanQuery{}); err != nil {
		t.Errorf("ScanBounded: %v", err)
	}
	if _, err := bc.AnalogousClaimsBounded(ctx, "nope", 5); err != nil {
		t.Errorf("AnalogousClaimsBounded: %v", err)
	}
}
