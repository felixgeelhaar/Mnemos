package relate

import "strings"

// aspect classifies a claim's lexical-aspect signal — the stance it
// takes about the underlying event being completed, ongoing, planned,
// or never having happened. Two claims about the same subject whose
// aspects are mutually exclusive are a contradiction.
type aspect int

const (
	aspectUnknown aspect = iota
	aspectCompleted
	aspectOngoing
	aspectPlanned
	aspectNever
)

// completedMarkers indicate the underlying event has happened and is
// no longer in flight. Each entry is a stem; matching is done against
// the post-stem token set so "completed", "completes", and "complete"
// all match the same way the overlap path treats them.
var completedMarkers = map[string]struct{}{
	"complet":    {},
	"finish":     {},
	"done":       {},
	"succeed":    {},
	"shipp":      {},
	"deliver":    {},
	"resolv":     {},
	"end":        {},
	"clos":       {},
	"sign-off":   {},
	"signoff":    {},
	"rolledback": {},
	"deployed":   {},
}

// ongoingMarkers indicate the event is still in flight as of the
// claim's effective time. "still" is the most reliable signal in
// English; the others are common run-state verbs.
var ongoingMarkers = map[string]struct{}{
	"still":     {},
	"ongoing":   {},
	"continu":   {},
	"underway":  {},
	"runn":      {},
	"running":   {},
	"pending":   {},
	"inflight":  {},
	"inprogres": {},
	"working":   {},
	"workin":    {},
	"active":    {},
}

// plannedMarkers indicate a future / scheduled event.
//
// "forecast" is deliberately absent: in operational text it's rare as a
// planning verb ("planned"/"scheduled" dominate), while in product/marketing
// copy it's an ordinary capability verb ("forecasts next month's revenue")
// that carries no aspect claim about a shared event — reading it as an aspect
// produced spurious contradictions (see the mnemos issue on passive-mode
// contradiction over-triggering).
var plannedMarkers = map[string]struct{}{
	"plan":    {},
	"schedul": {},
	"upcom":   {},
	"will":    {},
}

// neverMarkers indicate the event explicitly did not (and will not)
// happen. Distinct from polarity negation, which the existing path
// already covers — these are dedicated lexical signals like "never".
// "abandon" is deliberately absent: its dominant real-world sense in product
// text is the e-commerce noun phrase "cart abandonment" — a tracked feature,
// not a claim that an event was cancelled — so it mis-classified ordinary
// claims as aspectNever (see the mnemos issue on passive-mode contradiction
// over-triggering). Keep the unambiguous never-signals.
var neverMarkers = map[string]struct{}{
	"never":    {},
	"cancel":   {},
	"declined": {},
}

// classifyAspect returns the dominant aspect of a claim by inspecting
// its raw lowercase tokens. Crucially this does NOT use
// rawContentTokens because that helper filters negation words like
// "never" — which carry the strongest aspect signal we want to read.
func classifyAspect(text string) aspect {
	tokens := map[string]struct{}{}
	for _, w := range strings.Fields(strings.ToLower(text)) {
		w = strings.Trim(w, ",.;:!?()[]{}\"'")
		if w == "" {
			continue
		}
		tokens[w] = struct{}{}
	}
	// Prefer stronger markers: never > planned > ongoing > completed.
	// Real claims that mix markers ("we planned the migration but it
	// completed early") are ambiguous; classifyAspect returns one
	// label, deferring to the strongest signal so the divergence
	// path stays conservative.
	for tok := range tokens {
		stem := stemWord(tok)
		if _, ok := neverMarkers[stem]; ok {
			return aspectNever
		}
		if _, ok := neverMarkers[tok]; ok {
			return aspectNever
		}
	}
	for tok := range tokens {
		stem := stemWord(tok)
		if _, ok := plannedMarkers[stem]; ok {
			return aspectPlanned
		}
		if _, ok := plannedMarkers[tok]; ok {
			return aspectPlanned
		}
	}
	for tok := range tokens {
		stem := stemWord(tok)
		if _, ok := ongoingMarkers[stem]; ok {
			return aspectOngoing
		}
		if _, ok := ongoingMarkers[tok]; ok {
			return aspectOngoing
		}
	}
	for tok := range tokens {
		stem := stemWord(tok)
		if _, ok := completedMarkers[stem]; ok {
			return aspectCompleted
		}
		if _, ok := completedMarkers[tok]; ok {
			return aspectCompleted
		}
	}
	return aspectUnknown
}

// aspectsConflict reports whether two aspects describe mutually
// exclusive states for the same event. Completed vs ongoing is the
// canonical case; planned vs completed is also a conflict (the event
// can't both be planned and already done). aspectUnknown never
// conflicts — too lossy a signal.
func aspectsConflict(a, b aspect) bool {
	if a == aspectUnknown || b == aspectUnknown || a == b {
		return false
	}
	// Treat (completed, ongoing), (completed, planned),
	// (completed, never), (ongoing, never), (planned, never),
	// (planned, ongoing) all as conflicts. Unknown → no signal.
	return true
}

// detectTemporalDivergence flags two claims as contradicting when
// they share a subject token but their aspects are mutually
// exclusive — e.g. "The migration completed on Tuesday" vs "The
// migration is still running".
//
// Acceptance threshold for the benchmark is 0.70 (lower than entity
// or numeric) because aspect signals are easier to spoof in real
// production text — claims often mix tenses ("we planned to ship and
// shipped"). The lexicon-based heuristic targets the clearest cases.
func detectTemporalDivergence(aText, bText string, aTokens, bTokens map[string]struct{}) bool {
	aAspect := classifyAspect(aText)
	bAspect := classifyAspect(bText)
	// Bail before building the anchor sets, as the single-function version did.
	// See the note on detectNumericDivergence: the wrapper is what the
	// before/after benchmark measures, so its evaluation order has to stay
	// faithful.
	if !aspectsConflict(aAspect, bAspect) {
		return false
	}
	return temporalDivergesPre(aAspect, anchorTokens(aTokens), bAspect, anchorTokens(bTokens))
}

// temporalDivergesPre is detectTemporalDivergence with the per-claim
// derivations (aspect label and anchor-token set) supplied by the caller.
// Both are functions of one claim; the pairwise loop recomputed all four per
// pair, which meant two full text scans and two map allocations for every
// candidate.
func temporalDivergesPre(aAspect aspect, aAnchor map[string]struct{}, bAspect aspect, bAnchor map[string]struct{}) bool {
	if !aspectsConflict(aAspect, bAspect) {
		return false
	}

	// Require a shared subject anchor after stop-word removal. Without
	// this guard, "deploy completed" and "rollback still running" would
	// flag despite being about different operations.
	//
	// The anchor is measured as a PROPORTION of the shorter claim's topical
	// surface, not an absolute token count, and it excludes the aspect markers
	// themselves.
	//
	// An absolute count cannot work here. Requiring "at least two shared
	// tokens" is trivially met by any two long claims drawn from the same
	// corpus — generic words like "all", "three", "still" recur everywhere —
	// so a single claim containing "done" was flagged against dozens of
	// unrelated ones about Docker startup, UI screens and CI failures. That one
	// path produced 31,411 of 52,254 contradiction edges in a production brain.
	// Counting the aspect markers toward the anchor made it circular on top of
	// that: the very tokens creating the aspect conflict were also being taken
	// as proof the claims shared a subject.
	//
	// The canonical short case still passes: "migration completed Tuesday" vs
	// "migration is still running" anchors on {migration} out of {migration,
	// tuesday} — a ratio of 0.5 — once "completed"/"still"/"running" are
	// removed.
	overlap := contentOverlap(aAnchor, bAnchor)
	if overlap < 1 {
		return false
	}
	shorter := min(len(aAnchor), len(bAnchor))
	if shorter == 0 {
		return false
	}
	// The shorter-claim ratio alone has a blind spot: a two-token claim
	// ("Storage layer done") trivially scores 1.0 against a long unrelated one
	// that happens to share those tokens. Require the overlap to also be a real
	// share of the LONGER claim, the same guard the polarity and numeric paths
	// use. Measured on a production brain, temporal pairings were ~100% false
	// without it — "Both agents finished" against "Both keys present".
	longer := max(len(aAnchor), len(bAnchor))
	if longer == 0 || float64(overlap)/float64(longer) < minContradictionCoverage {
		return false
	}
	return float64(overlap)/float64(shorter) >= temporalAnchorRatio
}

// temporalAnchorRatio is the share of the shorter claim's topical tokens that
// must be common to both before an aspect mismatch counts as a contradiction.
// Matches the numeric path's threshold: both are asking the same question —
// "are these two claims about the same thing?".
const temporalAnchorRatio = 0.5

// anchorTokens reduces a token set to the words that can serve as a shared
// subject: real words (not numerals or symbols), minus the aspect markers that
// the conflict is being read from.
func anchorTokens(tokens map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(tokens))
	for tok := range wordTokens(tokens) {
		if isAspectMarker(tok) {
			continue
		}
		out[tok] = struct{}{}
	}
	return out
}

// isAspectMarker reports whether a token is one of the lexical aspect signals
// classifyAspect reads. Such a token is evidence of an aspect, never evidence
// that two claims share a subject.
func isAspectMarker(tok string) bool {
	for _, set := range []map[string]struct{}{
		completedMarkers, ongoingMarkers, plannedMarkers, neverMarkers,
	} {
		if _, ok := set[tok]; ok {
			return true
		}
	}
	return false
}
