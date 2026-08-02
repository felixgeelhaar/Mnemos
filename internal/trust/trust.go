// Package trust derives a single, comparable trust_score for each
// claim from three independent signals: the LLM-assigned confidence,
// the number of distinct events that corroborate the claim, and the
// freshness of the most recent corroborating evidence.
//
// Why this exists. A raw LLM "confidence" field treats a verbatim
// citation and a hallucination identically — both come back as ~0.9.
// Ranking by it produces nonsense. The trust_score below adds two
// signals the LLM cannot fake: how many independent events back the
// claim, and how recently any of them was observed. Together they
// approximate the question a human reader would actually ask: "do I
// have any reason to believe this is still true?"
package trust

import (
	"math"
	"time"
)

// FreshnessHalfLifeDays controls how quickly a claim's trust decays
// in the absence of new corroborating evidence. At one half-life the
// freshness factor is e^-1 ≈ 0.37; at two half-lives ≈ 0.13. A floor
// (FreshnessFloor) prevents very old facts from collapsing to zero —
// "Paris is the capital of France" should not score 0 just because the
// last corroborating event was a year ago.
const FreshnessHalfLifeDays = 90.0

// FreshnessFloor caps how low the freshness factor can drop. Without
// this, the multiplicative formula would let any old-but-true claim
// fall arbitrarily close to zero.
const FreshnessFloor = 0.3

// CorroborationCoefficient scales ln(evidenceCount) into the
// corroboration multiplier. With coefficient 0.2:
//   - 1 source  → 1.00 (neutral, no boost; ln(1) = 0)
//   - 5 sources → 1.32
//   - 20 sources→ 1.60
//
// Logarithmic so adding the 100th source doesn't dwarf the 10th.
//
// NB: the evidenceCount passed to Score is the INDEPENDENCE-graded count
// (see [go.klarlabs.de/mnemos/internal/domain.EffectiveEvidenceCount]) — many
// events from one author no longer inflate corroboration like independent
// confirmations. This function's math is unchanged; only its input is graded.
const CorroborationCoefficient = 0.2

// ScoreWithHalfLife is the per-claim variant of Score: it uses the
// supplied halfLifeDays for the freshness factor, falling back to
// FreshnessHalfLifeDays when halfLifeDays <= 0. Lets callers honour
// the per-claim override added in Phase 4 without forcing every
// existing call site to pass an extra argument.
func ScoreWithHalfLife(confidence float64, evidenceCount int, latestEvidence, now time.Time, halfLifeDays float64) float64 {
	c := clamp01(confidence)
	if evidenceCount < 1 {
		evidenceCount = 1
	}
	hl := halfLifeDays
	if hl <= 0 {
		hl = FreshnessHalfLifeDays
	}
	corroboration := 1 + math.Log(float64(evidenceCount))*CorroborationCoefficient
	freshness := freshnessFactorWithHalfLife(latestEvidence, now, hl)
	return clamp01(c * corroboration * freshness)
}

// IsStale reports whether a claim's freshness factor has decayed
// below the supplied threshold (default FreshnessFloor). The age
// reference is the most recent of LastVerified or latestEvidence,
// since either signal counts as "we still believe this is true".
// Zero references return false (no signal => not stale).
func IsStale(latestEvidence, lastVerified, now time.Time, halfLifeDays, threshold float64) bool {
	if threshold <= 0 {
		threshold = FreshnessFloor
	}
	hl := halfLifeDays
	if hl <= 0 {
		hl = FreshnessHalfLifeDays
	}
	ref := FreshnessRef(latestEvidence, lastVerified)
	if ref.IsZero() {
		return false
	}
	f := freshnessFactorWithHalfLife(ref, now, hl)
	return f <= threshold
}

// Staleness is IsStale's continuous companion: 1 − freshness, in [0, 1−FreshnessFloor].
// 0 means fresh (just verified / brand-new evidence); higher means the claim's
// freshest signal has decayed further, so it is a stronger candidate for
// re-verification. Like IsStale, the reference timestamp is the most recent of
// latestEvidence or lastVerified; a zero reference yields 0 (no signal ⇒ not
// stale). It reuses the same exponential-decay math as the trust freshness
// factor so callers rank on one consistent notion of decay.
func Staleness(latestEvidence, lastVerified, now time.Time, halfLifeDays float64) float64 {
	hl := halfLifeDays
	if hl <= 0 {
		hl = FreshnessHalfLifeDays
	}
	ref := FreshnessRef(latestEvidence, lastVerified)
	if ref.IsZero() {
		return 0
	}
	return 1 - freshnessFactorWithHalfLife(ref, now, hl)
}

// FreshnessRef returns the instant a belief's freshness decays from: the most
// recent of its latest evidence and its last explicit re-verification, since
// either signal means "we still had reason to believe this then". The zero time
// means the belief carries NEITHER signal and so cannot be dated — decay is
// unmeasurable for it, which is not the same as it being fresh.
//
// Exported because the decay model is also read FORWARD: a caller projecting
// what a belief's trust will be at a future instant (the brain-health
// trust_decay vital) must decay from the same reference Staleness and IsStale
// report against, rather than re-deriving it and drifting from the model.
func FreshnessRef(latestEvidence, lastVerified time.Time) time.Time {
	if !lastVerified.IsZero() && lastVerified.After(latestEvidence) {
		return lastVerified
	}
	return latestEvidence
}

func freshnessFactorWithHalfLife(latest, now time.Time, halfLifeDays float64) float64 {
	if latest.IsZero() {
		return 1.0
	}
	days := now.Sub(latest).Hours() / 24
	if days <= 0 {
		return 1.0
	}
	f := math.Exp(-days / halfLifeDays)
	if f < FreshnessFloor {
		return FreshnessFloor
	}
	return f
}

// Score returns a trust_score in [0, 1] from the three signals.
// `now` is injected so callers can test against a fixed clock; in
// production code pass time.Now().UTC().
//
// Inputs:
//
//	confidence     — LLM-assigned, expected in [0, 1]; clamped if outside
//	evidenceCount  — number of distinct events linked to this claim;
//	                 a claim with zero evidence is treated as having one
//	                 (its source event), since claims cannot exist without
//	                 at least one anchoring event in the domain model
//	latestEvidence — timestamp of the freshest evidence event; the zero
//	                 value disables the freshness factor (set to 1.0)
func Score(confidence float64, evidenceCount int, latestEvidence, now time.Time) float64 {
	c := clamp01(confidence)
	if evidenceCount < 1 {
		evidenceCount = 1
	}

	corroboration := 1 + math.Log(float64(evidenceCount))*CorroborationCoefficient
	freshness := freshnessFactor(latestEvidence, now)

	return clamp01(c * corroboration * freshness)
}

// ScoreWithAuthority is Score extended with an agentAuthority signal.
// agentAuthority is a 0.0–1.0 weight derived from the submitting
// agent's historical success rate and verification count
// (see domain.Agent.AuthorityScore). A value of 0 is treated as 1.0
// (unknown authority — no penalty) so legacy call sites that do not
// carry an agent are unaffected.
//
//	trust_score = confidence × corroboration × freshness × agentAuthority
func ScoreWithAuthority(confidence float64, evidenceCount int, latestEvidence, now time.Time, agentAuthority float64) float64 {
	base := Score(confidence, evidenceCount, latestEvidence, now)
	if agentAuthority <= 0 {
		return base
	}
	return clamp01(base * clamp01(agentAuthority))
}

// freshnessFactor returns a multiplier in [FreshnessFloor, 1].
// Exponential decay with half-life FreshnessHalfLifeDays. A zero
// timestamp short-circuits to 1.0 so callers that lack a timestamp
// (e.g., backfill before evidence is loaded) don't get penalised.
func freshnessFactor(latest, now time.Time) float64 {
	if latest.IsZero() {
		return 1.0
	}
	days := now.Sub(latest).Hours() / 24
	if days <= 0 {
		return 1.0
	}
	f := math.Exp(-days / FreshnessHalfLifeDays)
	if f < FreshnessFloor {
		return FreshnessFloor
	}
	return f
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
