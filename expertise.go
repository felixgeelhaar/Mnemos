package mnemos

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

// Expert is one entry of the transactive "who-knows-what" directory: a worker
// whose memory is topically relevant to a query, with how strongly it matches
// (affinity) and how trustworthy that matching knowledge is (reliability). Routes
// a knowledge gap to the right expert-memory instead of a flat search.
type Expert struct {
	// Worker is the claim author (the worker whose memory this is).
	Worker string
	// Affinity is [0,1] — how strongly this worker's memory matches the query,
	// normalised so the strongest match is 1.0.
	Affinity float64
	// Reliability is [0,1] — the mean trust of this worker's matching claims.
	Reliability float64
	// ClaimCount is how many currently-valid, query-relevant claims this worker
	// authored.
	ClaimCount int
}

// tokenizeSet lowercases text and returns the set of word tokens ≥ 3 runes.
func tokenizeSet(s string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, field := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len([]rune(field)) >= 3 {
			set[field] = struct{}{}
		}
	}
	return set
}

// overlapFraction is the fraction of query tokens present in the claim tokens —
// a [0,1] topical-relevance proxy for one claim.
func overlapFraction(query, claim map[string]struct{}) float64 {
	if len(query) == 0 {
		return 0
	}
	hit := 0
	for t := range query {
		if _, ok := claim[t]; ok {
			hit++
		}
	}
	return float64(hit) / float64(len(query))
}

// WhoKnows implements [Memory.WhoKnows]: the transactive memory directory. It
// scores each worker's currently-valid claims for topical relevance to the query,
// aggregates per author into an affinity (how much of the topic they cover) and a
// reliability (mean trust of their matching claims), and returns the experts
// ranked by affinity × reliability. Unattributed ("<system>") claims are ignored.
//
// This ships a deterministic lexical baseline (token overlap); reusing the stored
// claim embeddings for semantic topic centroids is the production enhancement.
//
// The result count is bounded by [MaxCognitiveResults]; use
// [memory.WhoKnowsBounded] to learn whether the answer was truncated.
func (m *memory) WhoKnows(ctx context.Context, query string, limit int) ([]Expert, error) {
	rep, err := m.WhoKnowsBounded(ctx, query, limit)
	return rep.Experts, err
}

// ExpertDefaultLimit is the expert count [Memory.WhoKnows] returns when the
// caller names none.
const ExpertDefaultLimit = 10

// WhoKnowsBounded is [Memory.WhoKnows] reporting the bounds it applied (see
// [BoundedCognition]).
//
// The scoring pass is a single linear sweep of the live corpus that AGGREGATES
// into one row per author, so the answer size is bounded by the number of
// distinct authors rather than by the corpus — measured at 33 ms per 10k claims.
// It is not the resource-exhaustion vector the pairwise reads were, and the
// sweep stays complete: an author's affinity is a sum over all their claims, so
// scoring a prefix would silently misrank the experts. Only the response is
// capped.
func (m *memory) WhoKnowsBounded(ctx context.Context, query string, limit int) (ExpertReport, error) {
	var bounds Bounds
	limit = capLimit(&bounds, limit, ExpertDefaultLimit, MaxCognitiveResults)
	q := tokenizeSet(query)
	if len(q) == 0 {
		return ExpertReport{Bounds: bounds}, nil
	}
	all, err := m.conn.Claims.ListAll(ctx)
	if err != nil {
		return ExpertReport{}, fmt.Errorf("mnemos: WhoKnows: list claims: %w", err)
	}
	type agg struct {
		affinity, trustSum float64
		n                  int
	}
	byAuthor := map[string]*agg{}
	for _, c := range all {
		if !c.ValidTo.IsZero() {
			continue // only currently-valid knowledge
		}
		bounds.Scanned++
		bounds.Considered++
		author := strings.TrimSpace(c.CreatedBy)
		if author == "" || author == "<system>" {
			continue // unattributed — no owner to route to
		}
		ov := overlapFraction(q, tokenizeSet(c.Text))
		if ov <= 0 {
			continue
		}
		a := byAuthor[author]
		if a == nil {
			a = &agg{}
			byAuthor[author] = a
		}
		a.affinity += ov
		a.trustSum += c.TrustScore
		a.n++
	}
	if len(byAuthor) == 0 {
		return ExpertReport{Bounds: bounds}, nil
	}
	var maxAff float64
	for _, a := range byAuthor {
		if a.affinity > maxAff {
			maxAff = a.affinity
		}
	}
	experts := make([]Expert, 0, len(byAuthor))
	for author, a := range byAuthor {
		aff := a.affinity
		if maxAff > 0 {
			aff = a.affinity / maxAff
		}
		experts = append(experts, Expert{
			Worker:      author,
			Affinity:    aff,
			Reliability: a.trustSum / float64(a.n),
			ClaimCount:  a.n,
		})
	}
	bounds.Available = len(experts)
	// Rank by affinity × reliability, THEN cut; affinity breaks ties (it's the
	// primary signal, reliability refines it), then the worker name so equal-score
	// directories are stable across map iteration order.
	experts = topN(experts, limit, func(a, b Expert) bool {
		sa, sb := a.Affinity*a.Reliability, b.Affinity*b.Reliability
		if sa != sb {
			return sa > sb
		}
		if a.Affinity != b.Affinity {
			return a.Affinity > b.Affinity
		}
		return a.Worker < b.Worker
	})
	if bounds.Available > len(experts) {
		bounds.cut(BoundReasonResultLimit)
	}
	bounds.finish(len(experts))
	return ExpertReport{Experts: experts, Bounds: bounds}, nil
}
