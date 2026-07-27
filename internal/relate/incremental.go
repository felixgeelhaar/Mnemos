package relate

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"

	"go.klarlabs.de/mnemos/internal/domain"
)

// indexCorpusThreshold is the number of existing claims above which the
// incremental path builds a candidate index instead of scanning every pair.
//
// Below it the index costs more than it saves: building the postings map for a
// few dozen claims is more work than the pairwise comparisons it would avoid.
// The threshold is not a correctness boundary — both paths run the identical
// pair evaluation and produce the identical relationships (see
// TestIndexedMatchesFullScan) — only a cost one.
const indexCorpusThreshold = 64

// IncrementalStats reports what one DetectIncremental call actually did. It
// exists so the prefilter is observable rather than silent: a caller (or an
// operator reading MNEMOS_RELATE_TRACE output) can see which path ran, how many
// pairs the index eliminated, and how many reached the detectors.
type IncrementalStats struct {
	// Path is "index" when the candidate index ran and "scan" when every pair
	// was compared directly.
	Path string

	NewClaims      int
	ExistingClaims int

	// PairsPossible is new*existing — what a full scan compares.
	PairsPossible int
	// PairsProbed is the number of (new, existing) pairs the index surfaced as
	// sharing at least one content token. Zero on the scan path.
	PairsProbed int
	// PairsEvaluated is the number of pairs that reached the divergence
	// detectors. On the scan path this equals PairsPossible minus pairs with an
	// empty token set on either side.
	PairsEvaluated int

	Relationships int
}

// DetectIncremental compares each new claim against all existing claims and
// returns inferred relationships. It does NOT compare existing claims against
// each other, making it suitable for incremental processing where the existing
// claims have already been compared in a prior pass.
func (e Engine) DetectIncremental(newClaims []domain.Claim, existingClaims []domain.Claim) ([]domain.Relationship, error) {
	rels, _, err := e.DetectIncrementalWithStats(newClaims, existingClaims)
	return rels, err
}

// DetectIncrementalWithStats is DetectIncremental plus a report of how the
// candidate set was narrowed. The relationships it returns are identical to
// what DetectIncremental returns for the same input.
func (e Engine) DetectIncrementalWithStats(newClaims []domain.Claim, existingClaims []domain.Claim) ([]domain.Relationship, IncrementalStats, error) {
	stats := IncrementalStats{
		Path:           "scan",
		NewClaims:      len(newClaims),
		ExistingClaims: len(existingClaims),
		PairsPossible:  len(newClaims) * len(existingClaims),
	}
	if len(newClaims) == 0 || len(existingClaims) == 0 {
		return nil, stats, nil
	}

	rels := make([]domain.Relationship, 0)
	now := e.now().UTC()

	newDerived := make([]*claimDerived, len(newClaims))
	for i := range newClaims {
		tokens, neg := contentTokensAndPolarity(newClaims[i].Text)
		newDerived[i] = newClaimDerived(newClaims[i].Text, tokens, neg)
	}

	emit := func(i int, j int32, relType domain.RelationshipType) error {
		if suppressAsSessionNoise(relType, newClaims[i], existingClaims[j]) {
			return nil
		}
		id, err := e.nextID()
		if err != nil {
			return err
		}
		rels = append(rels, domain.Relationship{
			ID:          id,
			Type:        relType,
			FromClaimID: newClaims[i].ID,
			ToClaimID:   existingClaims[j].ID,
			CreatedAt:   now,
		})
		return nil
	}

	if len(existingClaims) >= indexCorpusThreshold {
		stats.Path = "index"
		if err := e.indexedPass(newDerived, existingClaims, emit, &stats); err != nil {
			return nil, stats, err
		}
	} else if err := e.scanPass(newDerived, existingClaims, emit, &stats); err != nil {
		return nil, stats, err
	}

	// Citation edges from new claims to any known claim IDs in scope
	// (existing + new batch). This keeps citation graph tracking active
	// in incremental ingest paths.
	//
	// The id set is only built when a new claim actually cites something.
	// Building it unconditionally meant allocating a map the size of the whole
	// corpus on every write to serve the rare claim that names another claim.
	var err error
	if anyClaimCites(newClaims) {
		claimByID := make(map[string]struct{}, len(newClaims)+len(existingClaims))
		for _, c := range existingClaims {
			claimByID[c.ID] = struct{}{}
		}
		for _, c := range newClaims {
			claimByID[c.ID] = struct{}{}
		}
		rels, err = e.appendCitationRelationships(rels, newClaims, claimByID, now)
		if err != nil {
			return nil, stats, err
		}
	}

	// Test-conflict detection across new + existing test_result claims.
	testRels, err := e.detectTestConflictsAcross(newClaims, existingClaims)
	if err != nil {
		return nil, stats, err
	}
	rels = mergeRelationships(rels, testRels)

	stats.Relationships = len(rels)
	traceIncremental(stats)
	return rels, stats, nil
}

// scanPass compares every new claim against every existing claim. It is the
// reference behaviour: indexedPass must agree with it exactly.
func (e Engine) scanPass(newDerived []*claimDerived, existingClaims []domain.Claim, emit func(int, int32, domain.RelationshipType) error, stats *IncrementalStats) error {
	existDerived := make([]*claimDerived, len(existingClaims))
	for j := range existingClaims {
		tokens, neg := contentTokensAndPolarity(existingClaims[j].Text)
		existDerived[j] = newClaimDerived(existingClaims[j].Text, tokens, neg)
	}

	for i := range newDerived {
		if len(newDerived[i].tokens) == 0 {
			continue
		}
		for j := range existDerived {
			if len(existDerived[j].tokens) == 0 {
				continue
			}
			stats.PairsEvaluated++
			relType, ok := evaluatePair(newDerived[i], existDerived[j], contentOverlap(newDerived[i].tokens, existDerived[j].tokens))
			if !ok {
				continue
			}
			if err := emit(i, int32(j), relType); err != nil {
				return err
			}
		}
	}
	return nil
}

// indexedPass narrows the candidate set with the inverted index before running
// the pair evaluation. See candidateIndex for why that narrowing is exact.
func (e Engine) indexedPass(newDerived []*claimDerived, existingClaims []domain.Claim, emit func(int, int32, domain.RelationshipType) error, stats *IncrementalStats) error {
	ci := buildCandidateIndex(existingClaims)

	counts := make([]int32, len(existingClaims))
	var touched []int32
	var survivors []int32

	for i := range newDerived {
		nd := newDerived[i]
		if len(nd.tokens) == 0 {
			continue
		}

		// Accumulate the exact content-token overlap against every existing
		// claim that shares at least one token. Everything else cannot produce
		// an edge, so it is never touched.
		clear(counts)
		touched = touched[:0]
		for tok := range nd.tokens {
			for _, j := range ci.postings[tok] {
				if counts[j] == 0 {
					touched = append(touched, j)
				}
				counts[j]++
			}
		}
		stats.PairsProbed += len(touched)

		nAnchor := int32(len(nd.anchor))
		survivors = survivors[:0]
		for _, j := range touched {
			if counts[j] >= minContentTokenOverlap || singleOverlapCanFire(nAnchor, ci.nAnchor[j]) {
				survivors = append(survivors, j)
			}
		}

		// The full scan emits in ascending existing-claim order and callers
		// (and tests) compare relationship slices positionally, so restore that
		// order before evaluating. Postings are ascending individually but
		// their union is not.
		slices.Sort(survivors)

		for _, j := range survivors {
			ed := ci.derivedFor(j)
			if len(ed.tokens) == 0 {
				continue
			}
			stats.PairsEvaluated++
			relType, ok := evaluatePair(nd, ed, int(counts[j]))
			if !ok {
				continue
			}
			if err := emit(i, j, relType); err != nil {
				return err
			}
		}
	}
	return nil
}

// evaluatePair runs the four detection paths against one (new, existing) pair.
// It is the single source of truth for pair classification; both the scan and
// the index path funnel through it, so the prefilter cannot change the verdict
// for a pair it lets through — only which pairs get here.
//
// overlap must be contentOverlap(a.tokens, b.tokens). The index already knows
// it, so it is passed in rather than recomputed.
func evaluatePair(a, b *claimDerived, overlap int) (domain.RelationshipType, bool) {
	relType, ok := inferRelationshipFromOverlap(overlap, a.tokens, a.neg, b.tokens, b.neg)
	// Numeric divergence runs unconditionally and OVERRIDES an earlier
	// supports verdict: two claims that agree lexically but disagree on a
	// number are in conflict, not corroborating.
	if numericDivergesPre(a.nums, a.words, b.nums, b.words) {
		relType = domain.RelationshipTypeContradicts
		ok = true
	}
	if !ok && entityRoleDivergesPre(len(a.tokens), len(b.tokens), overlap, a.properNouns, b.properNouns) {
		relType = domain.RelationshipTypeContradicts
		ok = true
	}
	if !ok && temporalDivergesPre(a.asp, a.anchor, b.asp, b.anchor) {
		relType = domain.RelationshipTypeContradicts
		ok = true
	}
	return relType, ok
}

// anyClaimCites reports whether any claim text references a claim ID.
func anyClaimCites(claims []domain.Claim) bool {
	for i := range claims {
		if claimIDRefRE.MatchString(claims[i].Text) {
			return true
		}
	}
	return false
}

// detectTestConflictsAcross is DetectTestConflicts over new ++ existing without
// materialising the concatenation.
//
// The old code built a []domain.Claim holding a copy of every claim in the
// corpus on every write purely so DetectTestConflicts could filter it down to
// the handful of test_result claims. domain.Claim is a wide struct; at corpus
// scale that copy was one of the largest single allocations in the write path,
// and it was thrown away immediately.
//
// Claim order is preserved (new first, then existing), so the relationships and
// their order are unchanged.
func (e Engine) detectTestConflictsAcross(newClaims, existingClaims []domain.Claim) ([]domain.Relationship, error) {
	tests := make([]domain.Claim, 0, 8)
	for _, group := range [][]domain.Claim{newClaims, existingClaims} {
		for _, c := range group {
			if c.Type == domain.ClaimTypeTestResult && c.TestRequirementRef != "" {
				tests = append(tests, c)
			}
		}
	}
	if len(tests) < 2 {
		return []domain.Relationship{}, nil
	}
	return e.DetectTestConflicts(tests)
}

var traceIncrementalOnce sync.Once
var traceIncrementalOn bool

// traceIncremental writes one line per call to stderr when MNEMOS_RELATE_TRACE
// is truthy. Off by default and free when off; it exists so an operator can see
// which path the detector took on a real brain without redeploying instrumented
// callers.
func traceIncremental(s IncrementalStats) {
	traceIncrementalOnce.Do(func() {
		v := os.Getenv("MNEMOS_RELATE_TRACE")
		if v == "" {
			return
		}
		if b, err := strconv.ParseBool(v); err == nil {
			traceIncrementalOn = b
			return
		}
		traceIncrementalOn = v == "yes" || v == "on"
	})
	if !traceIncrementalOn {
		return
	}
	fmt.Fprintf(os.Stderr,
		"relate.incremental path=%s new=%d existing=%d pairs_possible=%d pairs_probed=%d pairs_evaluated=%d rels=%d\n",
		s.Path, s.NewClaims, s.ExistingClaims, s.PairsPossible, s.PairsProbed, s.PairsEvaluated, s.Relationships)
}
