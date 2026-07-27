package mnemos

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Bounded cognitive reads.
//
// The cognitive discovery surfaces — Recombinations, KnowledgeGaps, WhoKnows,
// Hypercorrections, AnalogousClaims, Scan — are all reachable from an agent over
// MCP and over REST. Each of them used to do however much work the brain happened
// to be big enough to demand: Recombinations paired every high-salience claim with
// every other (O(n²): 1.25 × 10⁹ pair evaluations on a 50k-claim brain, ~20 minutes
// of CPU for one request), AnalogousClaims issued three store round trips per
// candidate neighbourhood (7,271 round trips on a 10k-claim brain), and
// Hypercorrections and Scan had no result cap at all, so the response grew with the
// corpus. A cheap request must not buy unbounded work.
//
// Every one of them now applies an explicit, documented bound, and every bound is
// applied in the same shape:
//
//  1. RANK, then CUT — never cut, then rank. A cap that keeps an arbitrary prefix
//     turns a discovery endpoint into noise. Where a read narrows candidates before
//     scoring them (Recombinations by salience, AnalogousClaims by structural
//     degree) the narrowing is itself a ranked cut on the criterion the endpoint
//     already selects on, and the surviving results are then ranked on the
//     endpoint's own score before the result cap applies.
//  2. TRUNCATION IS VISIBLE. A truncated answer that looks complete is the exact
//     failure this codebase keeps hitting, so every bounded read reports [Bounds]
//     alongside its results — what it scanned, what it considered, how much was
//     available, and which bound bit.
//
// The stable [Memory] methods keep their signatures and apply the same bounds; the
// [BoundedCognition] optional capability is how a caller gets the [Bounds] back.

// Cognitive result caps. These bound the RESPONSE; the work caps live next to the
// read they govern (see [RecombineMaxCandidates], [AnalogNodeBudget]).
const (
	// MaxCognitiveResults is the hard ceiling on any caller-supplied limit to a
	// cognitive discovery read. These surfaces answer "what should I look at
	// next?", so a few hundred ranked items is already more than an agent can
	// act on, and an unbounded limit is just a way to make one request expensive.
	MaxCognitiveResults = 200

	// ScanMaxResults caps [Memory.Scan]. Scan is a bulk temporal read rather than
	// a top-N discovery surface, so its ceiling is looser — but Limit: 0 used to
	// mean "return the whole brain", which over MCP or HTTP is a full corpus dump
	// behind a one-line request.
	ScanMaxResults = 1000
)

// Bound reason codes. [Bounds.Reasons] carries every bound that bit, in the order
// the read applied them.
const (
	// BoundReasonCandidateCap — the read narrowed the corpus to a ranked top-N
	// candidate set before scoring, so results outside that set were never
	// considered.
	BoundReasonCandidateCap = "candidate_cap"
	// BoundReasonWorkBudget — the read exhausted its work budget (node visits,
	// pair evaluations) before exhausting its candidates.
	BoundReasonWorkBudget = "work_budget"
	// BoundReasonResultLimit — more results qualified than the effective limit
	// returns; the ones returned are the best-ranked.
	BoundReasonResultLimit = "result_limit"
	// BoundReasonLimitCapped — the caller asked for more results than
	// [MaxCognitiveResults] (or [ScanMaxResults]) allows.
	BoundReasonLimitCapped = "limit_capped"
)

// Bounds reports the limits a bounded cognitive read applied, so a caller can tell
// a complete answer from a truncated one. A zero Bounds means "nothing was cut".
type Bounds struct {
	// Truncated is true when the answer is a bounded subset of what the corpus
	// could have yielded. It is the one field a caller must not ignore.
	Truncated bool `json:"truncated"`

	// Reasons lists every bound that bit (see the BoundReason* codes), in
	// application order. Empty when Truncated is false.
	Reasons []string `json:"reasons,omitempty"`

	// Scanned is how many corpus items the read examined (claims, edges — see the
	// read's own doc). It is the denominator for "how much of the brain did this
	// answer actually see".
	Scanned int `json:"scanned"`

	// Considered is how many items survived candidate narrowing and were actually
	// scored. Considered < Scanned means a candidate cap or work budget bit.
	Considered int `json:"considered"`

	// Available is how many results qualified among the considered items, before
	// the result limit applied. Available > len(results) means the result limit
	// bit. It is NOT a corpus-wide total when Considered < Scanned.
	Available int `json:"available"`

	// Limit is the effective result limit after capping, so a caller that asked
	// for 10,000 can see it got 200.
	Limit int `json:"limit"`

	// Notice is a human-readable one-liner rendering the above. Empty when
	// Truncated is false.
	Notice string `json:"notice,omitempty"`
}

// cut records that a bound bit. Idempotent per reason.
func (b *Bounds) cut(reason string) {
	for _, r := range b.Reasons {
		if r == reason {
			return
		}
	}
	b.Truncated = true
	b.Reasons = append(b.Reasons, reason)
}

// finish renders [Bounds.Notice] from whatever was recorded. Call once, last.
func (b *Bounds) finish(returned int) {
	if !b.Truncated {
		b.Notice = ""
		return
	}
	parts := make([]string, 0, 3)
	if b.Considered < b.Scanned {
		parts = append(parts, fmt.Sprintf("scored the top %d of %d candidates", b.Considered, b.Scanned))
	}
	if b.Available > returned {
		parts = append(parts, fmt.Sprintf("returned the %d best-ranked of %d results", returned, b.Available))
	}
	if len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("returned %d results", returned))
	}
	b.Notice = "bounded answer: " + strings.Join(parts, "; ") +
		". This is not the whole brain — narrow the query or raise the limit."
}

// capLimit resolves a caller-supplied limit against a default and a hard ceiling,
// recording [BoundReasonLimitCapped] when the caller asked for more than allowed.
// A limit ≤ 0 means "use the default" — it no longer means "unbounded".
func capLimit(b *Bounds, limit, def, max int) int {
	if limit <= 0 {
		limit = def
	}
	if limit > max {
		limit = max
		b.cut(BoundReasonLimitCapped)
	}
	b.Limit = limit
	return limit
}

// ---------------------------------------------------------------------------
// reports
// ---------------------------------------------------------------------------

// RecombinationReport is [Memory.Recombinations] plus the bounds it applied.
type RecombinationReport struct {
	Recombinations []Recombination `json:"recombinations"`
	Bounds         Bounds          `json:"bounds"`
}

// GapReport is [Memory.KnowledgeGaps] plus the bounds it applied.
type GapReport struct {
	Gaps   []Gap  `json:"gaps"`
	Bounds Bounds `json:"bounds"`
}

// ExpertReport is [Memory.WhoKnows] plus the bounds it applied.
type ExpertReport struct {
	Experts []Expert `json:"experts"`
	Bounds  Bounds   `json:"bounds"`
}

// HypercorrectionReport is [Memory.Hypercorrections] plus the bounds it applied.
type HypercorrectionReport struct {
	Hypercorrections []Hypercorrection `json:"hypercorrections"`
	Bounds           Bounds            `json:"bounds"`
}

// AnalogyReport is [Memory.AnalogousClaims] plus the bounds it applied.
type AnalogyReport struct {
	Analogous []Analogy `json:"analogous"`
	Bounds    Bounds    `json:"bounds"`
}

// ScanReport is [Memory.Scan] plus the bounds it applied.
type ScanReport struct {
	Claims []Claim `json:"claims"`
	Bounds Bounds  `json:"bounds"`
}

// BoundedCognition is the optional capability a [Memory] implementation exposes
// when its cognitive reads can report the bounds they applied. It mirrors the
// same-named [Memory] methods, which keep their existing signatures and apply
// identical bounds — the difference is only that these hand back [Bounds] so a
// delivery adapter can tell its caller the answer was cut.
//
// Callers type-assert rather than depend on it (the pattern
// ports.ScopedTrustScorer already uses in this codebase), so adding a read here
// does not break an external [Memory] implementation:
//
//	if bc, ok := mem.(mnemos.BoundedCognition); ok { … }
type BoundedCognition interface {
	// RecombinationsBounded is [Memory.Recombinations] with its bounds.
	RecombinationsBounded(ctx context.Context, limit int) (RecombinationReport, error)
	// KnowledgeGapsBounded is [Memory.KnowledgeGaps] with its bounds.
	KnowledgeGapsBounded(ctx context.Context, limit int) (GapReport, error)
	// WhoKnowsBounded is [Memory.WhoKnows] with its bounds.
	WhoKnowsBounded(ctx context.Context, query string, limit int) (ExpertReport, error)
	// HypercorrectionsBounded is [Memory.Hypercorrections] with its bounds, plus
	// the result limit the unbounded-by-construction original never had.
	HypercorrectionsBounded(ctx context.Context, limit int) (HypercorrectionReport, error)
	// AnalogousClaimsBounded is [Memory.AnalogousClaims] with its bounds.
	AnalogousClaimsBounded(ctx context.Context, claimID string, limit int) (AnalogyReport, error)
	// ScanBounded is [Memory.Scan] with its bounds.
	ScanBounded(ctx context.Context, q ScanQuery) (ScanReport, error)
}

// topN keeps the n highest-ranked items of xs according to less (which reports
// "i outranks j"), preserving that ranking in the returned slice. It exists so a
// bound is always a RANKED cut: sorting first and slicing second, never slicing
// first. Returns xs itself when n ≤ 0 or n ≥ len(xs).
func topN[T any](xs []T, n int, less func(a, b T) bool) []T {
	sort.SliceStable(xs, func(i, j int) bool { return less(xs[i], xs[j]) })
	if n > 0 && len(xs) > n {
		return xs[:n]
	}
	return xs
}

// *memory implements the optional bounded-cognition capability. The assertion is
// here rather than next to the reads so adding one cannot silently miss it.
var _ BoundedCognition = (*memory)(nil)
