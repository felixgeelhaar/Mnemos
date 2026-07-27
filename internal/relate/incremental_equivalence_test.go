package relate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// referenceDetectIncremental is a verbatim transcription of DetectIncremental
// as it stood at 715117f, before the candidate index landed. It is the oracle
// the optimised implementation is held against.
//
// It is deliberately NOT factored to share code with the production path: the
// whole point is that it computes the answer the old way, so any behavioural
// drift shows up as a diff. If a future change to the detectors makes this
// function stale, the equivalence tests will fail loudly, which is the intended
// signal — update the oracle only after confirming the new behaviour is
// intended.
func referenceDetectIncremental(e Engine, newClaims []domain.Claim, existingClaims []domain.Claim) ([]domain.Relationship, error) {
	if len(newClaims) == 0 || len(existingClaims) == 0 {
		return nil, nil
	}

	rels := make([]domain.Relationship, 0)
	now := e.now().UTC()

	type analyzed struct {
		text   string
		tokens map[string]struct{}
		neg    bool
	}

	newCache := make([]analyzed, len(newClaims))
	for i := range newClaims {
		tokens, neg := contentTokensAndPolarity(newClaims[i].Text)
		newCache[i] = analyzed{text: newClaims[i].Text, tokens: tokens, neg: neg}
	}

	existCache := make([]analyzed, len(existingClaims))
	for i := range existingClaims {
		tokens, neg := contentTokensAndPolarity(existingClaims[i].Text)
		existCache[i] = analyzed{text: existingClaims[i].Text, tokens: tokens, neg: neg}
	}

	for i := 0; i < len(newClaims); i++ {
		if len(newCache[i].tokens) == 0 {
			continue
		}
		for j := 0; j < len(existingClaims); j++ {
			if len(existCache[j].tokens) == 0 {
				continue
			}

			relType, ok := inferRelationship(newCache[i].tokens, newCache[i].neg, existCache[j].tokens, existCache[j].neg)
			if detectNumericDivergence(newCache[i].text, existCache[j].text, newCache[i].tokens, existCache[j].tokens) {
				relType = domain.RelationshipTypeContradicts
				ok = true
			}
			if !ok && detectEntityRoleDivergence(newCache[i].text, existCache[j].text, newCache[i].tokens, existCache[j].tokens) {
				relType = domain.RelationshipTypeContradicts
				ok = true
			}
			if !ok && detectTemporalDivergence(newCache[i].text, existCache[j].text, newCache[i].tokens, existCache[j].tokens) {
				relType = domain.RelationshipTypeContradicts
				ok = true
			}
			if !ok {
				continue
			}
			if suppressAsSessionNoise(relType, newClaims[i], existingClaims[j]) {
				continue
			}

			id, err := e.nextID()
			if err != nil {
				return nil, err
			}

			rels = append(rels, domain.Relationship{
				ID:          id,
				Type:        relType,
				FromClaimID: newClaims[i].ID,
				ToClaimID:   existingClaims[j].ID,
				CreatedAt:   now,
			})
		}
	}

	claimByID := make(map[string]struct{}, len(newClaims)+len(existingClaims))
	for _, c := range existingClaims {
		claimByID[c.ID] = struct{}{}
	}
	for _, c := range newClaims {
		claimByID[c.ID] = struct{}{}
	}
	var err error
	rels, err = e.appendCitationRelationships(rels, newClaims, claimByID, now)
	if err != nil {
		return nil, err
	}

	allClaims := make([]domain.Claim, 0, len(newClaims)+len(existingClaims))
	allClaims = append(allClaims, newClaims...)
	allClaims = append(allClaims, existingClaims...)
	testRels, err := e.DetectTestConflicts(allClaims)
	if err != nil {
		return nil, err
	}
	rels = mergeRelationships(rels, testRels)

	return rels, nil
}

func equivalenceEngine() Engine {
	n := 0
	return Engine{
		now: func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
		nextID: func() (string, error) {
			n++
			return fmt.Sprintf("rl_%06d", n), nil
		},
	}
}

// assertSameRelationships compares two relationship slices element by element,
// IDs included. Because both engines mint sequential IDs, an identical slice
// also proves the emission ORDER is unchanged — which matters, since callers
// persist these in order and the store's dedup is order-sensitive.
func assertSameRelationships(t *testing.T, label string, want, got []domain.Relationship) {
	t.Helper()
	if len(want) != len(got) {
		t.Errorf("%s: relationship count = %d, want %d", label, len(got), len(want))
		describeDiff(t, label, want, got)
		return
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("%s: relationship[%d] = %+v, want %+v", label, i, got[i], want[i])
			describeDiff(t, label, want, got)
			return
		}
	}
}

// describeDiff reports the pairs present on one side only, which is the shape a
// recall regression takes: a contradiction the prefilter dropped.
func describeDiff(t *testing.T, label string, want, got []domain.Relationship) {
	t.Helper()
	key := func(r domain.Relationship) string {
		return string(r.Type) + "|" + r.FromClaimID + "|" + r.ToClaimID
	}
	inGot := map[string]struct{}{}
	for _, r := range got {
		inGot[key(r)] = struct{}{}
	}
	missing := 0
	for _, r := range want {
		if _, ok := inGot[key(r)]; !ok {
			if missing < 10 {
				t.Errorf("%s: MISSING edge %s", label, key(r))
			}
			missing++
		}
	}
	if missing > 10 {
		t.Errorf("%s: ... and %d more missing edges", label, missing-10)
	}
}

// TestIncrementalMatchesReference is the correctness gate for this whole change.
//
// It runs the optimised DetectIncremental and the pre-change reference over
// randomised corpora at several sizes and shapes, and requires byte-identical
// relationship slices. The sizes straddle indexCorpusThreshold so both the scan
// and the index path are covered, and the shapes vary the token distribution so
// the run is not confined to one selectivity regime.
func TestIncrementalMatchesReference(t *testing.T) {
	// Corpus sizes are deliberately modest. The expensive side of this
	// comparison is the REFERENCE, which is the O(new x existing) regex-heavy
	// implementation being replaced — at 90k pairs a single subtest cost ~8s,
	// and the whole package ran for ~20s. That matters more than it looks:
	// tools/mutate runs `go test` once per mutant with a 60s timeout, so a slow
	// suite both inflates the mutation job and risks a timeout being scored as
	// a killed mutant.
	//
	// The coverage is not what shrinks. This test proves the REFACTOR (hoisting
	// per-claim derivations out of the pairwise loop) preserved semantics, and
	// that is a per-pair property — a few thousand pairs per shape samples it as
	// well as ninety thousand. The riskier property, that the PREFILTER drops no
	// pair, is proved separately by TestScanAndIndexPathsAgree, which compares
	// two fast implementations and can therefore afford an order of magnitude
	// more pairs for a fraction of the time.
	cases := []struct {
		name  string
		opts  corpusOptions
		batch int
	}{
		{"tiny-scan-path", corpusOptions{Size: 30, VocabSize: 40, Zipf: 0.8, Seed: 1, MinTokens: 3, MaxTokens: 9, ShortShare: 0.3, NumShare: 0.3, NegShare: 0.3, AspectShar: 0.3, ProperShar: 0.3}, 8},
		{"at-threshold", corpusOptions{Size: 64, VocabSize: 60, Zipf: 0.9, Seed: 2, MinTokens: 3, MaxTokens: 10, ShortShare: 0.3, NumShare: 0.3, NegShare: 0.2, AspectShar: 0.3, ProperShar: 0.2}, 10},
		{"dense-tiny-vocab", corpusOptions{Size: 150, VocabSize: 25, Zipf: 0.5, Seed: 3, MinTokens: 2, MaxTokens: 6, ShortShare: 0.5, NumShare: 0.4, NegShare: 0.3, AspectShar: 0.4, ProperShar: 0.3}, 12},
		{"zipf-head-hot", corpusOptions{Size: 180, VocabSize: 300, Zipf: 1.0, Seed: 4, MinTokens: 4, MaxTokens: 14, ShortShare: 0.15, NumShare: 0.25, NegShare: 0.15, AspectShar: 0.2, ProperShar: 0.15}, 12},
		{"realistic-tail", defaultCorpusOptions(400, 5), 12},
		{"long-claims", corpusOptions{Size: 120, VocabSize: 200, Zipf: 1.1, RankOffset: 20, Seed: 6, MinTokens: 15, MaxTokens: 40, ShortShare: 0.05, NumShare: 0.3, NegShare: 0.2, AspectShar: 0.3, ProperShar: 0.1}, 10},
		{"all-short-claims", corpusOptions{Size: 150, VocabSize: 50, Zipf: 0.7, Seed: 7, MinTokens: 2, MaxTokens: 3, ShortShare: 1.0, NumShare: 0.35, NegShare: 0.25, AspectShar: 0.35, ProperShar: 0.4}, 12},
	}

	// A corpus that produces no relationships proves nothing, and these sizes
	// are tuned for runtime — so the sweep asserts it stayed substantive rather
	// than trusting whoever tunes them next.
	totalRels := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			existing := generateCorpus(tc.opts)
			batchOpts := tc.opts
			batchOpts.Size = tc.batch
			batchOpts.Seed = tc.opts.Seed + 10000
			newClaims := generateCorpus(batchOpts)
			for i := range newClaims {
				newClaims[i].ID = fmt.Sprintf("cl_new_%04d", i)
			}

			want, err := referenceDetectIncremental(equivalenceEngine(), newClaims, existing)
			if err != nil {
				t.Fatalf("reference: %v", err)
			}
			got, stats, err := equivalenceEngine().DetectIncrementalWithStats(newClaims, existing)
			if err != nil {
				t.Fatalf("optimised: %v", err)
			}
			assertSameRelationships(t, tc.name, want, got)
			totalRels += len(want)
			t.Logf("path=%s pairs_possible=%d pairs_probed=%d pairs_evaluated=%d rels=%d (%.4f%% of pairs evaluated)",
				stats.Path, stats.PairsPossible, stats.PairsProbed, stats.PairsEvaluated, len(got),
				100*float64(stats.PairsEvaluated)/float64(max(stats.PairsPossible, 1)))
		})
	}
	if totalRels < 500 {
		t.Errorf("sweep produced only %d relationships; the corpora have been shrunk past the point of testing anything", totalRels)
	}
}

// TestIncrementalMatchesReferenceOnFixtureCorpus runs the same equivalence check
// over real English claim text harvested from the in-tree eval fixtures, rather
// than the synthetic generator. Synthetic text exercises the thresholds; real
// text exercises the tokenizer, the numeric regex, the proper-noun heuristic and
// the aspect lexicon on the sentence shapes they were tuned for.
func TestIncrementalMatchesReferenceOnFixtureCorpus(t *testing.T) {
	texts := fixtureClaimTexts(t)
	if len(texts) < 100 {
		t.Fatalf("fixture corpus too small (%d texts) to be a meaningful check", len(texts))
	}
	// Bounded for the same reason the randomised sizes are: the reference side
	// is quadratic and the whole package has to stay inside tools/mutate's
	// per-mutant budget. Every harvested text still reaches the tokenizer
	// through TestTokenizeContentIntoMatchesContentTokens, which is linear and
	// therefore cheap enough to run on all of them.
	if len(texts) > 180 {
		texts = texts[:180]
	}
	t.Logf("fixture corpus: %d claim texts", len(texts))

	claims := make([]domain.Claim, 0, len(texts))
	for i, txt := range texts {
		claims = append(claims, domain.Claim{
			ID:        fmt.Sprintf("cl_fix_%05d", i),
			Text:      txt,
			Type:      domain.ClaimTypeFact,
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute),
		})
	}

	// The splits straddle indexCorpusThreshold in both directions: the first
	// three leave a large existing side (index path), the last leaves a small
	// one (scan path), so real English text goes through both.
	for _, split := range []int{1, 5, 25, len(claims) - 40} {
		if split <= 0 || split >= len(claims) {
			continue
		}
		newClaims := claims[:split]
		existing := claims[split:]

		want, err := referenceDetectIncremental(equivalenceEngine(), newClaims, existing)
		if err != nil {
			t.Fatalf("reference: %v", err)
		}
		got, stats, err := equivalenceEngine().DetectIncrementalWithStats(newClaims, existing)
		if err != nil {
			t.Fatalf("optimised: %v", err)
		}
		assertSameRelationships(t, fmt.Sprintf("fixture split=%d", split), want, got)
		t.Logf("split=%-4d path=%-5s new=%-4d existing=%-4d pairs_possible=%-8d pairs_evaluated=%-7d rels=%d",
			split, stats.Path, len(newClaims), len(existing), stats.PairsPossible, stats.PairsEvaluated, len(got))
	}
}

// TestIncrementalMatchesReferenceOnAdversarialPairs pins the specific pair
// shapes the prefilter's exactness argument depends on: edges that fire on a
// SINGLE shared content token, where the zero-token lemma alone is not enough
// and singleOverlapCanFire has to keep the pair.
func TestIncrementalMatchesReferenceOnAdversarialPairs(t *testing.T) {
	// Each entry is (existing text, new text) chosen to fire through a specific
	// path with minimal overlap, plus near-miss variants.
	pairs := [][2]string{
		// Entity/role divergence on one shared token.
		{"The CEO is Alice", "The CEO is Bob"},
		{"Lead is Carol", "Lead is Dave"},
		// Numeric divergence with a two-word topical surface.
		{"Timeout 30s", "Timeout 90s"},
		{"Latency 200ms", "Latency 900ms"},
		{"Budget $4500", "Budget $9900"},
		// Temporal aspect divergence anchored on one token.
		{"Migration completed", "Migration still running"},
		{"Rollout finished", "Rollout is pending"},
		// Polarity contradiction.
		{"Revenue increased in Q2", "Revenue did not increase in Q2"},
		// Value divergence.
		{"use React frontend", "use Vue frontend"},
		// Long claims sharing exactly one token — must NOT fire, and the
		// prefilter must agree with the reference about that.
		{
			"The storage layer rewrites every trust score on each governed write which blows the budget",
			"The storage adapter for libsql discards the namespace parameter entirely",
		},
		// Enumerated items.
		{"Phase 1: ship the index", "Phase 2: ship the index"},
		// Citation.
		{"Baseline claim about caching", "This supersedes cl_fix_00001 entirely"},
		// Empty / punctuation-only.
		{"", "the a an is"},
	}

	var existing, newClaims []domain.Claim
	for i, p := range pairs {
		existing = append(existing, domain.Claim{ID: fmt.Sprintf("cl_ex_%03d", i), Text: p[0], Type: domain.ClaimTypeFact})
		newClaims = append(newClaims, domain.Claim{ID: fmt.Sprintf("cl_nw_%03d", i), Text: p[1], Type: domain.ClaimTypeFact})
	}
	// Pad the existing side past indexCorpusThreshold with filler that shares
	// nothing, so the index path is the one under test.
	filler := generateCorpus(defaultCorpusOptions(200, 42))
	for i := range filler {
		filler[i].ID = fmt.Sprintf("cl_pad_%04d", i)
	}
	existing = append(existing, filler...)

	want, err := referenceDetectIncremental(equivalenceEngine(), newClaims, existing)
	if err != nil {
		t.Fatalf("reference: %v", err)
	}
	got, stats, err := equivalenceEngine().DetectIncrementalWithStats(newClaims, existing)
	if err != nil {
		t.Fatalf("optimised: %v", err)
	}
	if stats.Path != "index" {
		t.Fatalf("expected the index path, got %q", stats.Path)
	}
	assertSameRelationships(t, "adversarial", want, got)
	if len(want) == 0 {
		t.Fatal("adversarial fixture produced no relationships; it is not testing anything")
	}
	t.Logf("adversarial: %d relationships, pairs_possible=%d pairs_evaluated=%d", len(got), stats.PairsPossible, stats.PairsEvaluated)
}

// TestIncrementalTestResultEquivalence covers the test-conflict path separately,
// since detectTestConflictsAcross replaced a full new++existing concatenation.
func TestIncrementalTestResultEquivalence(t *testing.T) {
	mk := func(id, ref string, pass, fail int, scope domain.Scope) domain.Claim {
		return domain.Claim{
			ID: id, Text: "test " + ref + " result", Type: domain.ClaimTypeTestResult,
			TestRequirementRef: ref, TestPassCount: pass, TestFailCount: fail, Scope: scope,
		}
	}
	existing := []domain.Claim{
		mk("cl_e1", "REQ-1", 5, 0, domain.Scope{}),
		mk("cl_e2", "REQ-1", 0, 3, domain.Scope{}),
		mk("cl_e3", "REQ-2", 4, 0, domain.Scope{Env: "prod"}),
		mk("cl_e4", "", 1, 0, domain.Scope{}),
	}
	existing = append(existing, generateCorpus(defaultCorpusOptions(120, 77))...)
	newClaims := []domain.Claim{
		mk("cl_n1", "REQ-1", 0, 9, domain.Scope{}),
		mk("cl_n2", "REQ-2", 0, 2, domain.Scope{Env: "staging"}),
		mk("cl_n3", "REQ-3", 1, 1, domain.Scope{}),
	}

	want, err := referenceDetectIncremental(equivalenceEngine(), newClaims, existing)
	if err != nil {
		t.Fatalf("reference: %v", err)
	}
	got, _, err := equivalenceEngine().DetectIncrementalWithStats(newClaims, existing)
	if err != nil {
		t.Fatalf("optimised: %v", err)
	}
	assertSameRelationships(t, "test-results", want, got)
}

// TestScanAndIndexPathsAgree isolates the prefilter from the refactor: it runs
// the same corpus through both internal paths, so a disagreement can only come
// from the candidate narrowing.
//
// This is the load-bearing correctness test of the change, and it can afford to
// be the big one. Both sides are the fast implementation — the reference is not
// involved — so a pair costs about 2us here against about 90us in
// TestIncrementalMatchesReference. That buys an order of magnitude more pairs,
// across more corpus shapes, for a fraction of the runtime.
func TestScanAndIndexPathsAgree(t *testing.T) {
	shapes := []struct {
		name string
		opts corpusOptions
	}{
		// Dense: many candidates survive the prefilter, so the ov>=2 branch of
		// the gate is exercised constantly.
		{"dense", corpusOptions{VocabSize: 150, Zipf: 1.0, RankOffset: 5, MinTokens: 5, MaxTokens: 18, ShortShare: 0.10, NumShare: 0.20, NegShare: 0.12, AspectShar: 0.15, ProperShar: 0.10}},
		// Sparse: almost every candidate has overlap exactly 1, which is the
		// branch singleOverlapCanFire decides. If that gate were too strict this
		// is where edges would go missing.
		{"sparse", corpusOptions{VocabSize: 4000, Zipf: 1.0, RankOffset: 120, MinTokens: 5, MaxTokens: 18, ShortShare: 0.10, NumShare: 0.20, NegShare: 0.12, AspectShar: 0.15, ProperShar: 0.10}},
		// Short claims: the only shape where an overlap of 1 may legitimately
		// fire, through the entity / numeric / temporal divergence detectors.
		{"short", corpusOptions{VocabSize: 60, Zipf: 0.7, MinTokens: 2, MaxTokens: 3, ShortShare: 1.0, NumShare: 0.35, NegShare: 0.25, AspectShar: 0.35, ProperShar: 0.4}},
		// Long claims: large anchor sets, so singleOverlapCanFire must reject.
		{"long", corpusOptions{VocabSize: 250, Zipf: 1.1, RankOffset: 20, MinTokens: 15, MaxTokens: 40, ShortShare: 0.0, NumShare: 0.3, NegShare: 0.2, AspectShar: 0.3, ProperShar: 0.1}},
	}

	totalPairs, totalRels := 0, 0
	for _, shape := range shapes {
		for _, seed := range []int64{11, 12} {
			opts := shape.opts
			opts.Size = 600
			opts.Seed = seed
			existing := generateCorpus(opts)
			batch := opts
			batch.Size = 20
			batch.Seed = seed + 500
			newClaims := generateCorpus(batch)
			for i := range newClaims {
				newClaims[i].ID = fmt.Sprintf("cl_new_%04d", i)
			}

			scanRels, scanStats, err := runPass(newClaims, existing, (Engine).scanPass)
			if err != nil {
				t.Fatalf("%s seed=%d scanPass: %v", shape.name, seed, err)
			}
			indexRels, indexStats, err := runPass(newClaims, existing, (Engine).indexedPass)
			if err != nil {
				t.Fatalf("%s seed=%d indexedPass: %v", shape.name, seed, err)
			}
			assertSameRelationships(t, fmt.Sprintf("%s seed=%d", shape.name, seed), scanRels, indexRels)

			totalPairs += len(newClaims) * len(existing)
			totalRels += len(scanRels)
			t.Logf("%-7s seed=%d rels=%-5d scan_evaluated=%-6d index_evaluated=%-6d (%.2f%%)",
				shape.name, seed, len(scanRels), scanStats.PairsEvaluated, indexStats.PairsEvaluated,
				100*float64(indexStats.PairsEvaluated)/float64(max(scanStats.PairsEvaluated, 1)))
		}
	}
	t.Logf("total: %d pairs compared, %d relationships, all identical", totalPairs, totalRels)
	if totalRels == 0 {
		t.Fatal("no relationships produced; the comparison is vacuous")
	}
}

// runPass drives one of the two candidate-enumeration passes and collects what
// it emitted, so the two can be compared without going through the citation and
// test-conflict stages they share.
func runPass(newClaims, existing []domain.Claim, pass func(Engine, []*claimDerived, []domain.Claim, func(int, int32, domain.RelationshipType) error, *IncrementalStats) error) ([]domain.Relationship, IncrementalStats, error) {
	e := equivalenceEngine()
	derived := make([]*claimDerived, len(newClaims))
	for i := range newClaims {
		tok, neg := contentTokensAndPolarity(newClaims[i].Text)
		derived[i] = newClaimDerived(newClaims[i].Text, tok, neg)
	}
	var out []domain.Relationship
	stats := IncrementalStats{}
	emit := func(i int, j int32, rt domain.RelationshipType) error {
		id, err := e.nextID()
		if err != nil {
			return err
		}
		out = append(out, domain.Relationship{ID: id, Type: rt, FromClaimID: newClaims[i].ID, ToClaimID: existing[j].ID})
		return nil
	}
	err := pass(e, derived, existing, emit, &stats)
	return out, stats, err
}

// TestTokenizeContentIntoMatchesContentTokens guards the one place the two
// tokenization entry points could drift. The index builds token sets through
// tokenizeContentInto with a reusable scratch map; if that ever produced a
// different set from contentTokensAndPolarity, candidates would go missing and
// nothing else in the system would notice.
func TestTokenizeContentIntoMatchesContentTokens(t *testing.T) {
	corpus := generateCorpus(defaultCorpusOptions(2000, 31))
	corpus = append(corpus, domain.Claim{Text: ""}, domain.Claim{Text: "   "}, domain.Claim{Text: "the a an is of"})
	for _, txt := range fixtureClaimTexts(t) {
		corpus = append(corpus, domain.Claim{Text: txt})
	}

	scratch := make(map[string]struct{}, 32)
	for i := range corpus {
		want, wantNeg := contentTokensAndPolarity(corpus[i].Text)
		clear(scratch)
		gotNeg := tokenizeContentInto(scratch, corpus[i].Text)
		if gotNeg != wantNeg {
			t.Fatalf("polarity mismatch for %q: got %v want %v", corpus[i].Text, gotNeg, wantNeg)
		}
		if len(scratch) != len(want) {
			t.Fatalf("token count mismatch for %q: got %d want %d", corpus[i].Text, len(scratch), len(want))
		}
		for tok := range want {
			if _, ok := scratch[tok]; !ok {
				t.Fatalf("token %q missing for %q", tok, corpus[i].Text)
			}
		}
	}
}

// TestIsWordTokenMatchesWordTokens guards the other derived quantity the index
// computes without building the set.
func TestIsWordTokenMatchesWordTokens(t *testing.T) {
	corpus := generateCorpus(defaultCorpusOptions(1500, 32))
	for i := range corpus {
		tokens, _ := contentTokensAndPolarity(corpus[i].Text)
		want := wordTokens(tokens)
		got := 0
		for tok := range tokens {
			if isWordToken(tok) {
				got++
			}
		}
		if got != len(want) {
			t.Fatalf("word-token count mismatch for %q: got %d want %d", corpus[i].Text, got, len(want))
		}
		wantAnchor := anchorTokens(tokens)
		gotAnchor := 0
		for tok := range tokens {
			if isWordToken(tok) && !isAspectMarker(tok) {
				gotAnchor++
			}
		}
		if gotAnchor != len(wantAnchor) {
			t.Fatalf("anchor-token count mismatch for %q: got %d want %d", corpus[i].Text, gotAnchor, len(wantAnchor))
		}
	}
}

var fixtureTextRE = regexp.MustCompile(`(?m)^\s*(?:-\s*)?(?:text|input):\s*"(.+)"\s*$`)

// fixtureClaimTexts harvests claim-shaped English sentences from the in-tree
// eval fixtures. Falls back to skipping if the fixtures move, rather than
// silently testing nothing.
func fixtureClaimTexts(t *testing.T) []string {
	t.Helper()
	root := findRepoRoot(t)
	dir := filepath.Join(root, "data", "eval")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("eval fixtures unavailable: %v", err)
	}
	seen := map[string]struct{}{}
	var out []string
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".yaml") {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(dir, ent.Name()))
		if readErr != nil {
			t.Fatalf("read %s: %v", ent.Name(), readErr)
		}
		for _, m := range fixtureTextRE.FindAllStringSubmatch(string(raw), -1) {
			txt := strings.TrimSpace(strings.ReplaceAll(m[1], `\"`, `"`))
			if txt == "" {
				continue
			}
			if _, dup := seen[txt]; dup {
				continue
			}
			seen[txt] = struct{}{}
			out = append(out, txt)
			// Multi-sentence inputs are also useful as individual claims.
			for _, sent := range strings.Split(txt, ". ") {
				sent = strings.TrimSpace(sent)
				if len(sent) < 8 {
					continue
				}
				if _, dup := seen[sent]; dup {
					continue
				}
				seen[sent] = struct{}{}
				out = append(out, sent)
			}
		}
	}
	return out
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("repo root not found")
	return ""
}
