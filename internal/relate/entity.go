package relate

import (
	"strings"
	"unicode"
)

// detectEntityRoleDivergence flags two claims as contradicting when
// they share a role/subject token but assign it to different
// proper-noun-shaped values.
//
// Example: "The CEO is Alice" vs "The CEO is Bob" share `ceo` but
// disagree on the entity (Alice vs Bob). The token-overlap path
// rejects these as too short for value-divergence; this path catches
// them.
//
// Heuristic:
//   - Both claims must, after stop-word removal, fit in ≤4 content
//     tokens (the heuristic targets short copular statements; longer
//     claims are handled by detectValueDivergence and the polarity
//     path).
//   - They must share ≥1 content token (the role/subject anchor).
//   - Each must have ≥1 token that the other doesn't, and at least
//     one of those differing tokens must be a proper-noun-shaped
//     literal in the original text (capitalized, alphabetic).
//
// Out of scope: cross-language proper-noun resolution, alias
// reconciliation ("Alice Smith" vs "Alice"), pronoun resolution.
func detectEntityRoleDivergence(aText, bText string, aTokens, bTokens map[string]struct{}) bool {
	if !entityShapeAllows(len(aTokens), len(bTokens), contentOverlap(aTokens, bTokens)) {
		return false
	}
	return entityRoleDivergesPre(properNounTokens(aText), properNounTokens(bText))
}

// entityShapeAllows is the cheap half of detectEntityRoleDivergence: the shape
// guards that decide whether extracting proper nouns is worth it at all.
//
// It is split out rather than folded into entityRoleDivergesPre so that neither
// caller has to build a closure to keep the extraction lazy. A bound method
// value or a capturing func literal is a heap allocation, and in a loop that
// runs once per candidate pair that alone cost more than the work it deferred.
//
// The incremental path already knows the overlap — it comes out of the
// candidate index — so it is passed in rather than recomputed.
func entityShapeAllows(la, lb, overlap int) bool {
	if la == 0 || lb == 0 {
		return false
	}
	// Restrict the shape to very short copular claims (e.g. "X is Y").
	// Anything longer is covered by detectValueDivergence and the
	// polarity path; relaxing this caused false positives like
	// "Python is used for data science" vs "Go is used for backend
	// services" being flagged as conflicts because both contained
	// proper-noun-shaped tokens.
	if la > 3 || lb > 3 {
		return false
	}
	if overlap < 1 {
		return false
	}
	return la-overlap >= 1 && lb-overlap >= 1
}

// entityRoleDivergesPre is the expensive half: it decides whether the two
// claims' proper nouns actually disagree. Only call it when entityShapeAllows
// returned true.
func entityRoleDivergesPre(aProperNouns, bProperNouns map[string]struct{}) bool {
	if len(aProperNouns) == 0 && len(bProperNouns) == 0 {
		// No capitalized non-leading tokens on either side — likely
		// not a named-entity claim. Fall back to other detectors.
		return false
	}

	// At least one of the differing-tokens-on-each-side must be a
	// proper noun (capitalized in the source) — otherwise we'd flag
	// "the cat is small" vs "the cat is fast" as a contradiction,
	// which is a value-divergence call, not an entity one.
	return hasProperNounDivergence(aProperNouns, bProperNouns)
}

// properNounTokens returns the lowercased + stemmed forms of tokens
// that look like proper nouns in the source text. Heuristic:
//   - capitalized first letter, alphabetic only
//   - not a stop-word once lowercased
//   - sentence-initial words count ONLY when followed by a copula. The first
//     word of a sentence is capitalized by orthography, so on its own that
//     says nothing about whether it is a name — but "Alice is …" makes it the
//     subject of a copular claim, which is exactly the shape this detector
//     targets.
//
// That distinction is what the stop-word list alone could not draw, despite
// this function's test name ("SkipsSentenceInitialAndStopWords"): any
// capitalized non-stop-word opening a sentence was read as a proper noun. In
// production that made ordinary fragments look like competing named entities —
// "Now the overlay" vs "Clear picture now" was recorded as a contradiction
// because "Now" and "Clear" were taken for names diverging over the shared
// token "now". Neither is followed by a copula; "Alice is the CEO" is.
func properNounTokens(text string) map[string]struct{} {
	out := map[string]struct{}{}
	words := strings.Fields(text)
	sentenceStart := true
	for i, w := range words {
		trimmed := strings.Trim(w, ",.;:!?()[]{}\"'")
		// Whether the NEXT word opens a sentence is decided by this word's
		// original trailing punctuation, so compute it before continuing.
		initial := sentenceStart
		sentenceStart = strings.HasSuffix(w, ".") || strings.HasSuffix(w, "!") || strings.HasSuffix(w, "?")

		if trimmed == "" {
			continue
		}
		runes := []rune(trimmed)
		if !unicode.IsUpper(runes[0]) {
			continue
		}
		// Sentence-initial capitals are orthography unless the word is the
		// subject of a copular claim ("Alice is the CEO").
		if initial && !followedByCopula(words, i) {
			continue
		}
		// All remaining runes must be letters; mixed alpha-numeric
		// like "API2" is more likely a code identifier than a
		// proper noun.
		alpha := true
		for _, r := range runes[1:] {
			if !unicode.IsLetter(r) {
				alpha = false
				break
			}
		}
		if !alpha {
			continue
		}
		lower := strings.ToLower(trimmed)
		if _, isStop := stopWords[lower]; isStop {
			continue
		}
		out[stemWord(lower)] = struct{}{}
	}
	return out
}

// hasProperNounDivergence reports whether the two proper-noun token
// sets disagree — i.e. each contains at least one token the other
// doesn't.
func hasProperNounDivergence(a, b map[string]struct{}) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	uniqueA := false
	for tok := range a {
		if _, ok := b[tok]; !ok {
			uniqueA = true
			break
		}
	}
	uniqueB := false
	for tok := range b {
		if _, ok := a[tok]; !ok {
			uniqueB = true
			break
		}
	}
	return uniqueA && uniqueB
}

// copulas are the linking verbs that mark a copular claim ("X is Y"). A
// sentence-initial capitalized word followed by one of these is the subject of
// such a claim, and therefore a genuine name candidate rather than orthography.
var copulas = map[string]struct{}{
	"is": {}, "are": {}, "was": {}, "were": {}, "'s": {},
}

// followedByCopula reports whether the word after index i is a copula.
func followedByCopula(words []string, i int) bool {
	if i+1 >= len(words) {
		return false
	}
	next := strings.ToLower(strings.Trim(words[i+1], ",.;:!?()[]{}\"'"))
	_, ok := copulas[next]
	return ok
}
