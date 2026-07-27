package relate

import (
	"unicode"

	"go.klarlabs.de/mnemos/internal/domain"
)

// A candidateIndex is an inverted index from content token to the ordinals of
// the existing claims containing it. It exists to answer one question cheaply:
// which existing claims share at least one content token with this new claim?
//
// # Why that question is the right one
//
// DetectIncremental fires an edge through exactly four paths, and EVERY ONE of
// them requires the pair to share at least one content token:
//
//   - inferRelationship's primary path requires overlap >= minContentTokenOverlap (2).
//   - detectValueDivergence, its secondary path, requires overlap >= 1 — and in
//     fact >= 2, since it also demands shorter > 2 with overlap/shorter >= 0.7,
//     or exactly one unique token on each side of claims of 3-4 tokens.
//   - detectNumericDivergence requires contentOverlap(wordTokens(a), wordTokens(b)) >= 1,
//     and wordTokens(x) is a subset of x, so it implies a shared content token.
//   - detectEntityRoleDivergence requires contentOverlap(a, b) >= 1 outright.
//   - detectTemporalDivergence requires contentOverlap(anchorTokens(a), anchorTokens(b)) >= 1,
//     and anchorTokens(x) is a subset of wordTokens(x) is a subset of x.
//
// So a pair with zero shared content tokens can never produce an edge. Skipping
// those pairs is not a heuristic and not a recall trade — it is exact. The
// candidate set the index returns is a superset of every pair the full scan
// would have kept, which is what TestScanAndIndexPathsAgree asserts directly.
//
// # The zero-token lemma is load-bearing
//
// If a future detector fires on a pair with no shared content token, this index
// will silently stop finding it. That detector must either be given its own
// candidate source or the prefilter must be disabled for it. The equivalence
// test compares the indexed path against the full scan over a large random
// corpus plus the adversarial fixtures, so such a detector should show up as a
// failing test rather than as quietly missing contradictions — but the
// invariant is stated here because a test can only sample.
type candidateIndex struct {
	claims   []domain.Claim
	postings map[string][]int32

	// Per-claim state that is cheap enough to compute during the build and is
	// needed to gate candidates before the expensive detectors run.
	//
	// arena holds every claim's content tokens back to back; offs[i]:offs[i+1]
	// is claim i's slice. Storing them flat rather than as one map per claim
	// removes N map allocations from the build — at 50k claims that was the
	// single largest allocation source in the incremental path.
	arena []string
	offs  []int32
	neg   []bool

	// nAnchor[i] is len(anchorTokens(tokens(i))): content tokens that contain a
	// letter and are not aspect markers. It is the only quantity the
	// single-token gate below needs, so counting it here keeps that gate free.
	nAnchor []int32

	// derived caches the expensive per-claim state (numeric literals, aspect
	// label, word/anchor token sets, proper nouns) for claims that actually
	// became candidates. Populated on demand; a claim that never shares a token
	// with anything in the batch never pays for it.
	derived map[int32]*claimDerived
}

// buildCandidateIndex indexes existing by content token. It does not copy the
// claims; the caller must not mutate them while the index is alive.
func buildCandidateIndex(existing []domain.Claim) *candidateIndex {
	ci := &candidateIndex{
		claims:   existing,
		postings: make(map[string][]int32),
		arena:    make([]string, 0, len(existing)*12),
		offs:     make([]int32, len(existing)+1),
		neg:      make([]bool, len(existing)),
		nAnchor:  make([]int32, len(existing)),
		derived:  make(map[int32]*claimDerived),
	}

	// One scratch set, cleared per claim, instead of one map allocation per
	// claim. contentTokensAndPolarity's own map is what we are avoiding here;
	// tokenizeContentInto is the shared core both go through, so the token sets
	// cannot drift apart.
	scratch := make(map[string]struct{}, 32)
	for i := range existing {
		clear(scratch)
		ci.neg[i] = tokenizeContentInto(scratch, existing[i].Text)
		anchor := int32(0)
		for tok := range scratch {
			ci.arena = append(ci.arena, tok)
			ci.postings[tok] = append(ci.postings[tok], int32(i))
			if isWordToken(tok) && !isAspectMarker(tok) {
				anchor++
			}
		}
		ci.nAnchor[i] = anchor
		ci.offs[i+1] = int32(len(ci.arena))
	}
	return ci
}

// tokensOf materialises claim i's content-token set from the arena.
func (ci *candidateIndex) tokensOf(i int32) map[string]struct{} {
	slice := ci.arena[ci.offs[i]:ci.offs[i+1]]
	out := make(map[string]struct{}, len(slice))
	for _, tok := range slice {
		out[tok] = struct{}{}
	}
	return out
}

// derivedFor returns the cached expensive per-claim state for claim i.
func (ci *candidateIndex) derivedFor(i int32) *claimDerived {
	if d, ok := ci.derived[i]; ok {
		return d
	}
	d := newClaimDerived(ci.claims[i].Text, ci.tokensOf(i), ci.neg[i])
	ci.derived[i] = d
	return d
}

// claimDerived is everything the pairwise detectors need about ONE claim.
// Deriving it costs a regex pass (numerics), two text scans (aspect, proper
// nouns) and three map builds, which is why the pairwise loop must not do it
// per pair.
type claimDerived struct {
	text   string
	tokens map[string]struct{}
	neg    bool

	words  map[string]struct{}
	anchor map[string]struct{}
	nums   []numericValue
	asp    aspect

	// proper nouns stay lazy even here: detectEntityRoleDivergence only reaches
	// them for pairs of very short claims, which is a small minority.
	proper     map[string]struct{}
	properDone bool
}

func newClaimDerived(text string, tokens map[string]struct{}, neg bool) *claimDerived {
	words := wordTokens(tokens)
	anchor := make(map[string]struct{}, len(words))
	for tok := range words {
		if !isAspectMarker(tok) {
			anchor[tok] = struct{}{}
		}
	}
	return &claimDerived{
		text:   text,
		tokens: tokens,
		neg:    neg,
		words:  words,
		anchor: anchor,
		nums:   extractNumerics(text),
		asp:    classifyAspect(text),
	}
}

func (d *claimDerived) properNouns() map[string]struct{} {
	if !d.properDone {
		d.proper = properNounTokens(d.text)
		d.properDone = true
	}
	return d.proper
}

// isWordToken reports whether a content token contains at least one letter.
// It is the predicate wordTokens applies, factored out so the index can count
// word tokens without building the set.
func isWordToken(tok string) bool {
	for _, r := range tok {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// singleOverlapCanFire reports whether a pair sharing EXACTLY ONE content token
// can still produce an edge.
//
// With overlap == 1 the two overlap-ratio paths are already out:
// inferRelationship's primary path needs overlap >= 2, and detectValueDivergence
// needs shorter > 2 with overlap/shorter >= 0.7 (so overlap >= 3) or exactly one
// unique token per side of a 3-4 token pair (so overlap >= 2).
//
// That leaves the three divergence detectors, and each of them caps the LONGER
// claim at 3 tokens once the overlap is 1, because each requires its own
// overlap to be at least minContradictionCoverage (0.3) of the longer side and
// 1/4 = 0.25 < 0.3:
//
//   - numeric: overlap over wordTokens, so 1/max(|wordsA|,|wordsB|) >= 0.3.
//   - temporal: overlap over anchorTokens, so 1/max(|anchorA|,|anchorB|) >= 0.3.
//   - entity: requires len(tokensA) <= 3 and len(tokensB) <= 3 outright.
//
// anchorTokens is a subset of wordTokens is a subset of tokens, so
// |anchor| <= |words| <= |tokens| and all three necessary conditions imply
// max(|anchorA|, |anchorB|) <= 3. Testing the anchor sizes alone is therefore
// the weakest (most permissive) exact gate — it can only ever keep a pair the
// detectors would reject, never drop one they would accept.
func singleOverlapCanFire(aAnchor, bAnchor int32) bool {
	return aAnchor <= 3 && bAnchor <= 3
}
