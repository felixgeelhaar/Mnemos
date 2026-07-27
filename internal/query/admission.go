package query

import (
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/trust"
)

// Claim admission: the single gate every claim must pass before it can appear
// in an Answer, whichever way it was retrieved.
//
// # WHY THIS IS ONE FUNCTION
//
// The chain used to live inline in answerWithEvents and applied only to the
// directly-retrieved claims. Hop expansion (--hops / AnswerOptions.Hops)
// appended its results RAW, so every filter was escapable by asking for one
// hop: a claim deprecated by `forget` / `memory_deprecate` came back through an
// edge, and --scope, --visibility, --lifecycle, --min-trust and --at were all
// bypassable the same way. Hop results also skipped the credibility rescore, so
// they carried their raw stored trust_score while direct hits carried a
// recomputed one — and then the two were ranked against each other on different
// scales.
//
// Both are the same bug: a second entry into the answer set that did not go
// through the gate. Keeping the gate in one function is the fix; duplicating
// the chain would just recreate the drift.

// rescoreCredibility fills each claim's derived provenance fields (execution
// time, liveness) and recomputes its trust score from the current provenance
// signals, in place. Every claim that reaches an Answer must be rescored, or
// claims from different retrieval paths get ranked and trust-filtered on
// different scales.
func rescoreCredibility(claims []domain.Claim, now time.Time) {
	for i := range claims {
		if claims[i].LastExecuted.IsZero() {
			claims[i].LastExecuted = trust.EffectiveExecutionTime(
				claims[i].LastExecuted,
				claims[i].LastVerified,
				claims[i].ValidFrom,
				claims[i].CreatedAt,
			)
		}
		if claims[i].Liveness == "" || claims[i].Liveness == domain.LivenessUnknown {
			claims[i].Liveness = trust.EvaluateLiveness(
				claims[i].LastExecuted,
				claims[i].LastVerified,
				claims[i].ValidFrom,
				claims[i].CreatedAt,
				now,
				claims[i].TrustScore,
			)
		}
		score, rationale := trust.ScoreCredibility(trust.CredibilityInputs{
			CurrentTrust:    claims[i].TrustScore,
			SourceAuthority: claims[i].SourceAuthority,
			Liveness:        claims[i].Liveness,
			CitationCount:   claims[i].CitationCount,
			LastExecuted:    claims[i].LastExecuted,
			LastVerified:    claims[i].LastVerified,
			ValidFrom:       claims[i].ValidFrom,
			CreatedAt:       claims[i].CreatedAt,
			Now:             now,
			IsTest:          claims[i].Type == domain.ClaimTypeTestResult,
			TestLastRunAt:   claims[i].TestLastRunAt,
			TestPassCount:   claims[i].TestPassCount,
			TestFailCount:   claims[i].TestFailCount,
		})
		claims[i].TrustScore = score
		claims[i].ProvenanceRationale = rationale
	}
}

// admitClaims runs the full admission chain over a candidate claim set and
// returns the survivors, credibility-rescored. The input slice is never
// mutated: the first filter copies.
//
// Order matters in exactly one place — the rescore has to run before MinTrust,
// because MinTrust gates on the recomputed score, not the stored one.
func admitClaims(claims []domain.Claim, opts AnswerOptions, now time.Time) []domain.Claim {
	// Drop deprecated claims before anything ranks them. `forget` and
	// `memory_deprecate` promise that a forgotten claim stops being recalled,
	// and BuildContextBlock honored that, but this path — the one the recall
	// hook uses to inject context every turn — did not filter by status at all,
	// so forgetting changed the record without changing what came back.
	//
	// Only deprecated is excluded. Contested is how a live disagreement is
	// represented and hiding it would silently pick a side; resolved is the
	// winner of one. The evidence and history of a deprecated claim stay
	// queryable — this governs recall, not retention.
	out := excludeDeprecated(claims)

	rescoreCredibility(out, now)

	// Entity scope: if the caller restricted the answer to claims
	// linked to a specific entity (--entity in the CLI), drop
	// everything else before ranking. The map is small (one entity's
	// worth of claim ids); the filter is O(claims).
	if opts.AllowedClaimIDs != nil {
		out = keepClaims(out, func(c domain.Claim) bool {
			_, ok := opts.AllowedClaimIDs[c.ID]
			return ok
		})
	}

	// Scope filter: narrow the candidate set to claims whose Scope
	// matches the caller's filter before any ranking. Empty filter
	// is a no-op so single-tenant deployments see no change.
	if !opts.Scope.IsEmpty() {
		out = keepClaims(out, func(c domain.Claim) bool { return c.Scope.Matches(opts.Scope) })
	}

	// Visibility filter: enforce workspace isolation. The zero value is
	// treated as VisibilityTeam. Resolution is additive — each tier
	// includes claims visible to narrower tiers:
	//   personal → only VisibilityPersonal claims
	//   team     → VisibilityPersonal + VisibilityTeam claims (default)
	//   org      → all claims (no filter needed)
	vis := opts.Visibility
	if vis == "" {
		vis = domain.VisibilityTeam
	}
	if vis != domain.VisibilityOrg {
		allowed := visibilityAllowed(vis)
		out = keepClaims(out, func(c domain.Claim) bool {
			cv := c.Visibility
			if cv == "" {
				cv = domain.VisibilityTeam
			}
			return allowed[cv]
		})
	}

	// Lifecycle filter: narrow to a promotion state when requested. Empty
	// is a no-op, so ordinary recall (including claims that were never
	// routed through a candidate→promoted review) is unchanged.
	if opts.Lifecycle != "" {
		out = keepClaims(out, func(c domain.Claim) bool { return c.Lifecycle == opts.Lifecycle })
	}

	// Filter out low-trust claims before ranking — saves work on the
	// cosine pass and prevents low-trust noise from displacing
	// high-trust answers in the top-N.
	if opts.MinTrust > 0 {
		out = keepClaims(out, func(c domain.Claim) bool { return c.TrustScore >= opts.MinTrust })
	}

	// Temporal filter: by default, exclude claims that have been
	// superseded (valid_to in the past). Callers asking for history
	// (--include-history) opt out; --at <date> queries swap the
	// cutoff for a point-in-time check.
	if !opts.IncludeHistory {
		asOf := opts.AsOf
		if asOf.IsZero() {
			asOf = now
		}
		out = keepClaims(out, func(c domain.Claim) bool { return c.IsValidAt(asOf) })
	}

	// Ingestion-time filter (the second axis of the bi-temporal
	// model). Drop rows recorded after RecordedAsOf so the response
	// reproduces what the store knew at that timestamp. Independent
	// of the validity filter — a claim that was valid yesterday but
	// recorded today returns under (AsOf=yesterday, RecordedAsOf=now)
	// and disappears under (AsOf=yesterday, RecordedAsOf=yesterday).
	if !opts.RecordedAsOf.IsZero() {
		out = keepClaims(out, func(c domain.Claim) bool { return !c.CreatedAt.After(opts.RecordedAsOf) })
	}

	return out
}

// keepClaims returns the claims satisfying pred, preserving order.
func keepClaims(claims []domain.Claim, pred func(domain.Claim) bool) []domain.Claim {
	out := make([]domain.Claim, 0, len(claims))
	for _, c := range claims {
		if pred(c) {
			out = append(out, c)
		}
	}
	return out
}
