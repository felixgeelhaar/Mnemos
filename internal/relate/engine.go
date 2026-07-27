package relate

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// Engine detects relationships between claims using token-overlap heuristics.
type Engine struct {
	now    func() time.Time
	nextID func() (string, error)
}

// NewEngine returns an Engine with default clock and ID generation.
func NewEngine() Engine {
	return Engine{
		now:    time.Now,
		nextID: newRelationshipID,
	}
}

// stopWords are common English words filtered before computing token overlap
// to avoid false-positive relationships on trivial shared words.
var stopWords = map[string]struct{}{
	"the": {}, "a": {}, "an": {}, "is": {}, "are": {}, "was": {}, "were": {},
	"be": {}, "been": {}, "being": {}, "have": {}, "has": {}, "had": {},
	"do": {}, "does": {}, "did": {}, "will": {}, "would": {}, "shall": {},
	"should": {}, "may": {}, "might": {}, "must": {}, "can": {}, "could": {},
	"to": {}, "of": {}, "in": {}, "for": {}, "on": {}, "with": {}, "at": {},
	"by": {}, "from": {}, "as": {}, "into": {}, "through": {}, "during": {},
	"before": {}, "after": {}, "above": {}, "below": {}, "between": {},
	"and": {}, "but": {}, "or": {}, "nor": {}, "so": {}, "yet": {},
	"it": {}, "its": {}, "this": {}, "that": {}, "these": {}, "those": {},
	"i": {}, "we": {}, "you": {}, "he": {}, "she": {}, "they": {}, "me": {},
	"us": {}, "him": {}, "her": {}, "them": {}, "my": {}, "our": {}, "your": {},
	"his": {}, "their": {},
}

// negationWords are used for polarity detection.
var negationWords = map[string]struct{}{
	"not": {}, "no": {}, "never": {}, "without": {}, "cannot": {},
	"cant": {}, "can't": {}, "dont": {}, "don't": {}, "doesnt": {}, "doesn't": {},
	"didnt": {}, "didn't": {}, "isnt": {}, "isn't": {}, "arent": {}, "aren't": {},
	"wasnt": {}, "wasn't": {}, "werent": {}, "weren't": {}, "wont": {}, "won't": {},
	"wouldnt": {}, "wouldn't": {}, "shouldnt": {}, "shouldn't": {},
	"impossible": {}, "unlikely": {}, "disagree": {}, "rejected": {}, "declined": {},
	"failed": {},
}

var claimIDRefRE = regexp.MustCompile(`\bcl_[A-Za-z0-9_-]+\b`)

// minContentTokenOverlap is the minimum number of content (non-stop) tokens
// that must overlap for two claims to be considered related.
const minContentTokenOverlap = 2

// minOverlapRatio is the minimum fraction of the shorter claim's content tokens
// that must overlap with the longer claim for a relationship to be inferred.
const minOverlapRatio = 0.3

// minContradictionCoverage is the share of the LONGER claim's tokens that an
// opposite-polarity pair must have in common. It guards the short-claim blind
// spot in the shorter-claim ratio: genuine contradictions measured on real data
// score 0.33-0.67 here, while false pairings score 0.20-0.25.
const minContradictionCoverage = 0.3

// actionPrefixes mark a raw (un-stemmed) token as describing an
// action — something an actor *did*. Matched against the raw
// lowercase tokens of the claim so a single entry covers most
// conjugations: "deploy" matches "deploy", "deployed", "deploys",
// "deploying". The set is operations-oriented; user-action language
// ("said", "wrote") is deliberately absent.
var actionPrefixes = []string{
	"deploy", "releas", "shipp", "push", "merg",
	"restart", "rollback", "roll", "scal",
	"upgrad", "downgrad", "patch", "configur", "enabl",
	"disabl", "migrat", "switch", "cutover",
	"reboot", "restor", "trigger", "kill", "stop",
	"start", "launch", "promot", "revert",
	"hotfix", "block", "unblock", "throttl", "drain",
	"appli", "execut",
}

// stateChangePrefixes mark a raw (un-stemmed) token as describing an
// observed state change — something that *happened* to a system.
// Matched against raw lowercase tokens; same rationale as
// actionPrefixes.
var stateChangePrefixes = []string{
	"latenc", "throughput", "error", "response",
	"memor", "availab", "uptim", "downtim", "outag", "incident",
	"regress", "spike", "spik", "drop", "dropp", "increas", "decreas",
	"degrad", "improv", "recover", "recov",
	"crash", "broke", "broken",
	"saturat",
}

// minCausalEntityOverlap is the minimum non-noise content-token
// overlap between an action claim and a state-change claim before a
// `causes` edge is emitted. One specific token (e.g. the service or
// component name) is enough once generic infrastructure nouns
// ("service", "system") are excluded — see causalNoiseTokens.
const minCausalEntityOverlap = 1

// causalNoiseTokens are content tokens that survive stop-word removal
// but are too generic to count as a shared entity for causal
// detection. Without filtering, any two operational claims would tie
// together via "service" alone. Stems are listed because tokens
// arrive post-stemWord.
var causalNoiseTokens = map[string]struct{}{
	"service": {}, "system": {}, "systems": {}, "app": {},
	"production": {}, "prod": {}, "stage": {}, "staging": {},
	"instance": {}, "replica": {}, "cluster": {}, "server": {},
	"node": {}, "pod": {}, "container": {}, "host": {},
	"environment": {}, "env": {},
}

// causalTimeTolerance bounds how far apart two claims can be in time
// and still be linked by the causal heuristic. A deploy at 09:00
// causing latency at 09:05 is plausible; a deploy at 09:00 "causing"
// latency three weeks later is almost certainly noise. The tolerance
// is intentionally generous because evidence-event timestamps can lag
// the underlying real-world action.
const causalTimeTolerance = 48 * time.Hour

// Detect compares all claim pairs and returns inferred relationships.
func (e Engine) Detect(claims []domain.Claim) ([]domain.Relationship, error) {
	rels := make([]domain.Relationship, 0)
	now := e.now().UTC()

	// Pre-compute normalized content tokens and polarity for each claim.
	type analyzed struct {
		text         string
		tokens       map[string]struct{}
		neg          bool
		isEnumerated bool
	}
	cache := make([]analyzed, len(claims))
	for i := range claims {
		tokens, neg := contentTokensAndPolarity(claims[i].Text)
		cache[i] = analyzed{
			text:         claims[i].Text,
			tokens:       tokens,
			neg:          neg,
			isEnumerated: isEnumeratedItem(claims[i].Text),
		}
	}

	for i := 0; i < len(claims); i++ {
		if len(cache[i].tokens) == 0 {
			continue
		}
		for j := i + 1; j < len(claims); j++ {
			if len(cache[j].tokens) == 0 {
				continue
			}

			// Skip value-divergence for pairs of enumerated list items
			// (Phase 1 vs Phase 2 are parallel, not competing).
			bothEnumerated := cache[i].isEnumerated && cache[j].isEnumerated

			relType, ok := inferRelationshipWithContext(cache[i].tokens, cache[i].neg, cache[j].tokens, cache[j].neg, bothEnumerated)
			// Numeric-divergence path. Same-topic claims that share
			// most tokens but disagree on a numeric value are a
			// contradiction regardless of whether the overlap path
			// would have called them supports — "12 refunds" vs
			// "0 refunds" is a conflict, not a corroboration.
			if !bothEnumerated && detectNumericDivergence(cache[i].text, cache[j].text, cache[i].tokens, cache[j].tokens) {
				relType = domain.RelationshipTypeContradicts
				ok = true
			}
			// Entity/role-divergence path. Short copular claims that
			// share a role token but assign it to different proper
			// nouns ("The CEO is Alice" vs "The CEO is Bob") are
			// contradictions even though the overlap-based path
			// rejects them as too short for value-divergence.
			if !bothEnumerated && !ok && detectEntityRoleDivergence(cache[i].text, cache[j].text, cache[i].tokens, cache[j].tokens) {
				relType = domain.RelationshipTypeContradicts
				ok = true
			}
			// Temporal-aspect divergence path. Same-subject claims
			// whose lexical aspects exclude each other ("migration
			// completed Tuesday" vs "migration is still running")
			// are contradictions even when the overlap is too low
			// to trigger the polarity path.
			if !ok && detectTemporalDivergence(cache[i].text, cache[j].text, cache[i].tokens, cache[j].tokens) {
				relType = domain.RelationshipTypeContradicts
				ok = true
			}
			if !ok {
				continue
			}
			if suppressAsSessionNoise(relType, claims[i], claims[j]) {
				continue
			}

			id, err := e.nextID()
			if err != nil {
				return nil, err
			}

			rels = append(rels, domain.Relationship{
				ID:          id,
				Type:        relType,
				FromClaimID: claims[i].ID,
				ToClaimID:   claims[j].ID,
				CreatedAt:   now,
			})
		}
	}

	// Explicit citations: if a claim text references another claim ID,
	// add a directed cites edge to form a citation graph.
	claimByID := make(map[string]struct{}, len(claims))
	for _, c := range claims {
		claimByID[c.ID] = struct{}{}
	}
	var err error
	rels, err = e.appendCitationRelationships(rels, claims, claimByID, now)
	if err != nil {
		return nil, err
	}

	// Test-conflict detection: test_result pairs covering the same requirement.
	testRels, err := e.DetectTestConflicts(claims)
	if err != nil {
		return nil, err
	}
	rels = mergeRelationships(rels, testRels)

	return rels, nil
}

func (e Engine) appendCitationRelationships(rels []domain.Relationship, fromClaims []domain.Claim, validClaimIDs map[string]struct{}, now time.Time) ([]domain.Relationship, error) {
	seen := make(map[string]struct{}, len(rels))
	for _, r := range rels {
		seen[string(r.Type)+"|"+r.FromClaimID+"|"+r.ToClaimID] = struct{}{}
	}

	for _, c := range fromClaims {
		refs := claimIDRefRE.FindAllString(c.Text, -1)
		if len(refs) == 0 {
			continue
		}
		localSeen := make(map[string]struct{}, len(refs))
		for _, targetID := range refs {
			if targetID == c.ID {
				continue
			}
			if _, ok := validClaimIDs[targetID]; !ok {
				continue
			}
			if _, dup := localSeen[targetID]; dup {
				continue
			}
			localSeen[targetID] = struct{}{}

			k := string(domain.RelationshipTypeCites) + "|" + c.ID + "|" + targetID
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}

			id, err := e.nextID()
			if err != nil {
				return nil, err
			}
			rels = append(rels, domain.Relationship{
				ID:          id,
				Type:        domain.RelationshipTypeCites,
				FromClaimID: c.ID,
				ToClaimID:   targetID,
				CreatedAt:   now,
			})
		}
	}

	return rels, nil
}

// DetectTestConflicts scans all claims, isolates pairs of type
// ClaimTypeTestResult that share the same non-empty TestRequirementRef, and
// emits a relationship for each pair:
//
//   - contradicts — one test is net-passing (PassCount > FailCount) and the
//     other is net-failing, meaning two tests that cover the same requirement
//     produce conflicting evidence about whether it is met.
//   - supports — both tests agree (both net-passing or both net-failing).
//
// Pairs that share no scope signal (both Scope == "") are still compared
// because a blank scope means "global" — the conflict is real. Pairs that
// both have a non-empty Scope but whose Scopes differ are skipped: they may
// cover the same requirement in intentionally different environments (e.g.
// staging vs prod) and divergent results there are expected rather than
// conflicting.
//
// DetectTestConflicts is called inside Detect and DetectIncremental and its
// results are deduplicated against any relationships already in the slice
// passed in. Callers do not need to call it directly.
func (e Engine) DetectTestConflicts(claims []domain.Claim) ([]domain.Relationship, error) {
	now := e.now().UTC()
	rels := make([]domain.Relationship, 0)

	// Collect only test_result claims with a non-empty requirement reference.
	type testClaim struct {
		idx   int
		claim domain.Claim
	}
	var tests []testClaim
	for i, c := range claims {
		if c.Type == domain.ClaimTypeTestResult && c.TestRequirementRef != "" {
			tests = append(tests, testClaim{idx: i, claim: c})
		}
	}
	if len(tests) < 2 {
		return rels, nil
	}

	for i := 0; i < len(tests); i++ {
		for j := i + 1; j < len(tests); j++ {
			a, b := tests[i].claim, tests[j].claim

			// Same requirement reference required.
			if a.TestRequirementRef != b.TestRequirementRef {
				continue
			}

			// Scope guard: if both have an explicit scope that differs, skip.
			if !a.Scope.IsEmpty() && !b.Scope.IsEmpty() && !a.Scope.Equal(b.Scope) {
				continue
			}

			netA := testNetResult(a)
			netB := testNetResult(b)

			// Indeterminate (no pass/fail counts at all) — skip.
			if netA == 0 || netB == 0 {
				continue
			}

			var relType domain.RelationshipType
			if (netA > 0) != (netB > 0) {
				relType = domain.RelationshipTypeContradicts
			} else {
				relType = domain.RelationshipTypeSupports
			}

			id, err := e.nextID()
			if err != nil {
				return nil, err
			}
			rels = append(rels, domain.Relationship{
				ID:          id,
				Type:        relType,
				FromClaimID: a.ID,
				ToClaimID:   b.ID,
				CreatedAt:   now,
			})
		}
	}
	return rels, nil
}

// testNetResult returns +1 when the test is net-passing (PassCount > FailCount),
// -1 when net-failing (FailCount > PassCount), and 0 when indeterminate (both
// zero or equal — no signal to reason about).
func testNetResult(c domain.Claim) int {
	switch {
	case c.TestPassCount > c.TestFailCount:
		return 1
	case c.TestFailCount > c.TestPassCount:
		return -1
	default:
		return 0
	}
}

// DetectCausal scans claim pairs and emits `causes` edges when one
// claim looks like an action and another like a state change with
// shared entities and a plausible temporal ordering. Returns only
// causal edges; it does NOT replace Detect's logical edges
// (supports/contradicts) — callers compose both passes.
//
// The heuristic is intentionally conservative: a `causes` edge stored
// in the graph is a strong assertion, so false positives are worse
// than false negatives. When the heuristic is uncertain (e.g. shared
// entities but no timestamp ordering), no edge is emitted; the LLM
// extractor (DetectCausalLLM) is the path for ambiguous cases.
func (e Engine) DetectCausal(claims []domain.Claim) ([]domain.Relationship, error) {
	if len(claims) < 2 {
		return nil, nil
	}
	rels := make([]domain.Relationship, 0)
	now := e.now().UTC()

	type analyzed struct {
		tokens   map[string]struct{}
		isAction bool
		isState  bool
		ts       time.Time
	}
	cache := make([]analyzed, len(claims))
	for i, c := range claims {
		tokens, _ := contentTokensAndPolarity(c.Text)
		raw := rawContentTokens(c.Text)
		cache[i] = analyzed{
			tokens:   tokens,
			isAction: hasAnyPrefix(raw, actionPrefixes),
			isState:  hasAnyPrefix(raw, stateChangePrefixes),
			ts:       claimEffectiveTime(c),
		}
	}

	for i := 0; i < len(claims); i++ {
		for j := 0; j < len(claims); j++ {
			if i == j {
				continue
			}
			if !cache[i].isAction || !cache[j].isState {
				continue
			}
			// Don't emit if the same claim qualifies as both — usually
			// means the claim already describes cause+effect inline,
			// in which case there is no edge to draw.
			if cache[i].isState && cache[j].isAction {
				continue
			}
			if causalEntityOverlap(cache[i].tokens, cache[j].tokens) < minCausalEntityOverlap {
				continue
			}
			if !plausibleCausalOrdering(cache[i].ts, cache[j].ts) {
				continue
			}
			id, err := e.nextID()
			if err != nil {
				return nil, err
			}
			rels = append(rels, domain.Relationship{
				ID:          id,
				Type:        domain.RelationshipTypeCauses,
				FromClaimID: claims[i].ID,
				ToClaimID:   claims[j].ID,
				CreatedAt:   now,
			})
		}
	}
	return rels, nil
}

// hasAnyPrefix reports whether any token in tokens starts with any
// prefix in the probe list. Prefix match keeps the indicator lists
// short and keeps the action/state detection separate from the
// overlap path's stemming.
func hasAnyPrefix(tokens map[string]struct{}, prefixes []string) bool {
	for tok := range tokens {
		for _, p := range prefixes {
			if strings.HasPrefix(tok, p) {
				return true
			}
		}
	}
	return false
}

// causalEntityOverlap counts shared content tokens between two
// claims, excluding the generic infrastructure nouns in
// causalNoiseTokens. Used by DetectCausal so a shared "service" alone
// can't tie two unrelated claims together.
func causalEntityOverlap(a, b map[string]struct{}) int {
	count := 0
	for tok := range a {
		if _, noise := causalNoiseTokens[tok]; noise {
			continue
		}
		if _, ok := b[tok]; ok {
			count++
		}
	}
	return count
}

// rawContentTokens lowercases the text and returns its content tokens
// without any stemming. Used by the causal action/state heuristic
// where prefix matching against raw conjugations is more reliable
// than fighting stemWord's lossy output.
func rawContentTokens(text string) map[string]struct{} {
	words := strings.Fields(strings.ToLower(text))
	out := make(map[string]struct{}, len(words))
	for _, w := range words {
		w = strings.Trim(w, ",.;:!?()[]{}\"'")
		if w == "" {
			continue
		}
		if _, ok := negationWords[w]; ok {
			continue
		}
		if _, ok := stopWords[w]; ok {
			continue
		}
		out[w] = struct{}{}
	}
	return out
}

// claimEffectiveTime picks the timestamp the causal heuristic should
// reason about. ValidFrom is preferred (set from source-event
// timestamp during ingest); we fall back to CreatedAt only when
// ValidFrom is the zero value, which means the claim predates the
// temporal-validity layer.
func claimEffectiveTime(c domain.Claim) time.Time {
	if !c.ValidFrom.IsZero() {
		return c.ValidFrom
	}
	return c.CreatedAt
}

// plausibleCausalOrdering returns true when the cause time precedes
// the effect time within causalTimeTolerance. Zero-valued timestamps
// fall through as "no signal, allow it" — extraction often loses
// timestamps and we don't want to drop every causal pair just because
// the upstream data was thin.
func plausibleCausalOrdering(causeAt, effectAt time.Time) bool {
	if causeAt.IsZero() || effectAt.IsZero() {
		return true
	}
	if !causeAt.Before(effectAt) && !causeAt.Equal(effectAt) {
		return false
	}
	return effectAt.Sub(causeAt) <= causalTimeTolerance
}

// ContentTokens returns the normalized content tokens of text: lowercased,
// punctuation-stripped, stop-words and negation-words removed, and minimally
// stemmed. It is the shared tokenization used both here (relationship /
// contradiction detection) and by internal/consolidate (cross-tenant lesson
// clustering), so the two never drift apart. Negation is dropped from the token
// set (polarity is handled separately by the contradiction path).
func ContentTokens(text string) map[string]struct{} {
	tokens, _ := contentTokensAndPolarity(text)
	return tokens
}

// contentTokensAndPolarity splits text into content tokens (stop words removed)
// and detects whether the text contains negation.
func contentTokensAndPolarity(text string) (map[string]struct{}, bool) {
	tokens := make(map[string]struct{}, 12)
	neg := tokenizeContentInto(tokens, text)
	return tokens, neg
}

// tokenizeContentInto is the shared core of content tokenization: it adds text's
// content tokens to dst and reports whether the text is negated.
//
// The incremental path indexes tens of thousands of claims per write and cannot
// afford a fresh map per claim, so it calls this with one reusable scratch set.
// Keeping a single implementation is deliberate: two tokenizers that disagree
// by one stop word would make the candidate index drop real pairs, and the
// failure would be silent.
func tokenizeContentInto(dst map[string]struct{}, text string) bool {
	words := strings.Fields(strings.ToLower(text))
	neg := false
	for i, w := range words {
		w = strings.Trim(w, ",.;:!?()[]{}\"'")
		if w == "" {
			continue
		}
		if _, ok := negationWords[w]; ok {
			// A contrastive negation does not make the claim negative:
			// "it is a bug in an existing pattern, not merely a missing one"
			// ASSERTS the bug. Flipping polarity for it made such claims
			// collide with every claim that asserted something similar.
			if !isContrastiveNegation(words, i) {
				neg = true
			}
			continue
		}
		if _, ok := stopWords[w]; ok {
			continue
		}
		dst[stemWord(w)] = struct{}{}
	}
	return neg
}

// contrastiveHedges are the words that, following a negation, mark it as
// contrastive rather than a denial: "not merely X", "not just X".
var contrastiveHedges = map[string]struct{}{
	"merely": {}, "just": {}, "only": {}, "simply": {}, "solely": {},
}

// isContrastiveNegation reports whether the negation word at index i is
// contrasting two alternatives rather than denying the claim.
//
// English marks this two ways, and both are cheap to read positionally:
// a preceding clause break ("real work, not already-done" — the assertion is
// "real work"), or a following hedge ("not merely a missing one"). In both
// shapes the sentence asserts something POSITIVE and names the rejected
// alternative for contrast.
//
// A sentence-initial or mid-clause negation is left alone, so genuine denials
// ("no admin bypass needed", "it is not dead code") keep negative polarity —
// those are exactly the pairs the contradiction detector should still catch.
func isContrastiveNegation(words []string, i int) bool {
	if i > 0 {
		// A clause break immediately before the negation.
		prev := words[i-1]
		if strings.HasSuffix(prev, ",") || strings.HasSuffix(prev, ";") ||
			strings.HasSuffix(prev, "—") || strings.HasSuffix(prev, "–") {
			return true
		}
		if prev == "-" || prev == "—" || prev == "–" {
			return true
		}
	}
	if i+1 < len(words) {
		next := strings.Trim(words[i+1], ",.;:!?()[]{}\"'")
		if _, ok := contrastiveHedges[next]; ok {
			return true
		}
	}
	return false
}

// stemWord applies minimal suffix stripping to reduce inflection noise.
func stemWord(word string) string {
	if len(word) > 5 && strings.HasSuffix(word, "ed") {
		return strings.TrimSuffix(word, "ed")
	}
	if len(word) > 5 && strings.HasSuffix(word, "es") {
		return strings.TrimSuffix(word, "es")
	}
	if len(word) > 4 && strings.HasSuffix(word, "s") {
		return strings.TrimSuffix(word, "s")
	}
	return word
}

func inferRelationship(aTokens map[string]struct{}, aNeg bool, bTokens map[string]struct{}, bNeg bool) (domain.RelationshipType, bool) {
	return inferRelationshipWithContext(aTokens, aNeg, bTokens, bNeg, false)
}

func inferRelationshipWithContext(aTokens map[string]struct{}, aNeg bool, bTokens map[string]struct{}, bNeg bool, skipValueDivergence bool) (domain.RelationshipType, bool) {
	return inferRelationshipFromOverlapWithContext(contentOverlap(aTokens, bTokens), aTokens, aNeg, bTokens, bNeg, skipValueDivergence)
}

// inferRelationshipFromOverlap is inferRelationship with the content overlap
// supplied by the caller. The candidate index computes the overlap as a side
// effect of finding the candidate at all, so recomputing it here would be pure
// duplicated work.
func inferRelationshipFromOverlap(overlap int, aTokens map[string]struct{}, aNeg bool, bTokens map[string]struct{}, bNeg bool) (domain.RelationshipType, bool) {
	return inferRelationshipFromOverlapWithContext(overlap, aTokens, aNeg, bTokens, bNeg, false)
}

func inferRelationshipFromOverlapWithContext(overlap int, aTokens map[string]struct{}, aNeg bool, bTokens map[string]struct{}, bNeg bool, skipValueDivergence bool) (domain.RelationshipType, bool) {
	// Primary path: sufficient token overlap.
	if overlap >= minContentTokenOverlap {
		shorter := len(aTokens)
		if len(bTokens) < shorter {
			shorter = len(bTokens)
		}
		if shorter > 0 {
			ratio := float64(overlap) / float64(shorter)
			if ratio >= minOverlapRatio {
				if aNeg != bNeg {
					// Contradictions require stricter overlap: the claims must
					// be about the same topic, not just share a few tokens.
					// Prevents "without X" structural phrases from flagging
					// as contradictions with unrelated claims.
					//
					// `ratio` alone is measured against the SHORTER claim, so a
					// very short claim trivially scores 1.0 by sharing both its
					// tokens: "Found the gap" hit ratio 1.0 against a ten-token
					// claim that merely mentioned finding a gap, and was filed
					// as a contradiction of it. Require the overlap to also be
					// a real share of the LONGER claim, so both sides have to
					// be substantially about the same thing.
					//
					// Measured on a production brain, the polarity path
					// produced 70% of all contradiction edges and was ~92% false
					// positive even between non-junk claims — pairs that were
					// unrelated, or that actually agreed with each other.
					longer := len(aTokens)
					if len(bTokens) > longer {
						longer = len(bTokens)
					}
					coverage := float64(overlap) / float64(longer)
					if ratio >= 0.5 && coverage >= minContradictionCoverage {
						return domain.RelationshipTypeContradicts, true
					}
					return "", false
				}
				return domain.RelationshipTypeSupports, true
			}
		}
	}

	// Secondary path: value-divergence detection.
	// Claims that share most tokens but differ on 1 key token are competing
	// alternatives (e.g., "use PostgreSQL" vs "prefers MySQL").
	// Skipped for pairs of enumerated list items (Phase 1 vs Phase 2).
	if !skipValueDivergence && valueDivergesFromOverlap(len(aTokens), len(bTokens), overlap) {
		return domain.RelationshipTypeContradicts, true
	}

	return "", false
}

// enumeratedListRE matches claims that look like enumerated list items
// (e.g., "Phase 1: ...", "Outcome 2: ...", "Use Case 3: ..."). These are
// parallel items, not competing alternatives, so value-divergence detection
// should not flag them as contradictions.
var enumeratedListRE = regexp.MustCompile(`(?i)^(?:- )?(?:phase|outcome|step|milestone|use\s*case|task|objective|deliverable|feature|requirement)\s*\d+`)

// isEnumeratedItem returns true if the text looks like a parallel enumerated
// list item that shouldn't be compared for value-divergence.
func isEnumeratedItem(text string) bool {
	return enumeratedListRE.MatchString(strings.TrimSpace(text))
}

// detectValueDivergence returns true when two claims share a structural
// pattern but differ on a key value. This catches "use React frontend" vs
// "use Vue frontend" where claims share most tokens but differ on one.
//
// Uses strict ratio-based rules to avoid false positives on enumerated list
// items (e.g., "Phase 1: ..." vs "Phase 2: ...") which share tokens but are
// parallel items, not competing alternatives.
func detectValueDivergence(a, b map[string]struct{}) bool {
	return valueDivergesFromOverlap(len(a), len(b), contentOverlap(a, b))
}

// valueDivergesFromOverlap is detectValueDivergence expressed over the token
// counts and the overlap alone — it never needed the sets themselves. Taking
// the overlap as an argument lets the incremental path reuse the count the
// candidate index already produced.
func valueDivergesFromOverlap(la, lb, overlap int) bool {
	if la < 2 || lb < 2 {
		return false
	}

	onlyA := la - overlap
	onlyB := lb - overlap

	// Must share at least 1 token AND each has at least 1 unique token.
	if overlap < 1 || onlyA < 1 || onlyB < 1 {
		return false
	}

	// For short claims (both <= 4 tokens), require exactly 1 unique token on each side.
	// Catches "use React frontend" vs "use Vue frontend" style.
	shorter := la
	if lb < shorter {
		shorter = lb
	}
	longer := la
	if lb > longer {
		longer = lb
	}

	// Very short claims (both <= 2 tokens) are too ambiguous for value-divergence.
	if shorter <= 2 {
		return false
	}

	if longer <= 4 {
		return onlyA == 1 && onlyB == 1
	}

	// For longer claims, use an overlap ratio of the shorter claim's tokens.
	// Claims are competing alternatives only if they share MOST (>= 70%) of the
	// shorter claim's content and differ on a small fraction.
	overlapRatio := float64(overlap) / float64(shorter)
	divergenceRatio := float64(onlyA+onlyB) / float64(la+lb)

	return overlapRatio >= 0.7 && divergenceRatio <= 0.25
}

func contentOverlap(a, b map[string]struct{}) int {
	count := 0
	for token := range a {
		if _, ok := b[token]; ok {
			count++
		}
	}
	return count
}

func newRelationshipID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "rl_" + hex.EncodeToString(buf), nil
}

// mergeRelationships appends additions to base, skipping any pair that already
// has a relationship of the same type between the same two claims.
func mergeRelationships(base, additions []domain.Relationship) []domain.Relationship {
	seen := make(map[string]struct{}, len(base))
	for _, r := range base {
		seen[string(r.Type)+"|"+r.FromClaimID+"|"+r.ToClaimID] = struct{}{}
	}
	for _, r := range additions {
		k := string(r.Type) + "|" + r.FromClaimID + "|" + r.ToClaimID
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		base = append(base, r)
	}
	return base
}

// suppressAsSessionNoise reports whether a detected relationship between two
// beliefs should not be recorded because both are session-local narration
// (ADR 0023).
//
// Two progress reports from different sessions — "both PRs are open" against
// "PR #23 squash-merged" — are lexically a contradiction and semantically
// nothing at all. Suppressing them here means they are never created, rather
// than pruned back out afterwards by `prune --session-noise`.
//
// Only contradictions are suppressed. A conversation corroborating itself is
// real evidence, so `supports` between two session-local beliefs still stands,
// exactly as in the session-scope filter.
//
// BOTH sides must be session-local. A session-local belief contradicting a
// durable one is the case most worth surfacing: a conversation bumping into
// something the brain actually believes.
//
// Unknown durability is not session-local (domain.Durability.IsSessionLocal),
// so every belief captured before the field existed keeps behaving exactly as
// it does today. This can only ever suppress edges between beliefs positively
// identified as narration.
func suppressAsSessionNoise(relType domain.RelationshipType, a, b domain.Claim) bool {
	if relType != domain.RelationshipTypeContradicts {
		return false
	}
	return a.Durability.IsSessionLocal() && b.Durability.IsSessionLocal()
}
