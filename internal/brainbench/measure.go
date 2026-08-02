package brainbench

import (
	"context"
	"fmt"
	"os"
	"strings"

	"go.klarlabs.de/mnemos"
	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/extract"
	"go.klarlabs.de/mnemos/internal/store"
)

// Metric names. Exported so tests and consumers refer to them by symbol rather
// than by a string that can drift.
const (
	// Answer quality — what a caller actually gets back.
	MetricAnswerHitRate    = "answer_hit_rate"
	MetricAnswerPrecision1 = "answer_precision_at_1"
	MetricAnswerMRR        = "answer_mrr"
	MetricForbiddenHitRate = "forbidden_hit_rate"

	// Knowledge preservation — the anti-gaming metric.
	MetricGoldSurvival = "gold_survival"

	// Graph census.
	MetricValidClaims        = "valid_claims"
	MetricInvalidatedClaims  = "invalidated_claims"
	MetricActiveClaims       = "status_active_claims"
	MetricDeprecatedClaims   = "status_deprecated_claims"
	MetricContestedClaims    = "contested_claims"
	MetricEvents             = "events"
	MetricRelationships      = "relationships"
	MetricContradictionEdges = "contradiction_edges"
	MetricEvidenceLinks      = "evidence_links"
	MetricMeanTrust          = "mean_trust"
	MetricNoiseFraction      = "noise_fraction"
	MetricDBBytes            = "db_bytes"

	// ADR 0019 vitals and pathologies, mirrored in.
	MetricVitalPrefix     = "vital_"
	MetricPathologyPrefix = "pathology_"
)

// Measure runs the full metric battery against one arm.
//
// Measurement MUTATES the brain: recall applies the Hebbian co-activation,
// reconsolidation and competitive-inhibition write-backs, so a measured brain
// is no longer the brain that was measured. Every arm is therefore a throwaway
// copy that is measured exactly once, and callers must not reuse a measured
// DSN. Run enforces this by construction.
//
// The two handles are opened sequentially, never concurrently: SQLite is
// single-writer and the library Memory holds a writer.
func Measure(ctx context.Context, dsn, dbPath string, sc Scenario) (MetricSet, error) {
	// Census FIRST, deliberately. It is the only read of the arm's unperturbed
	// state: the library probes that follow apply retrieval write-backs, so a
	// census taken afterwards would be measuring the harness's own footprint
	// mixed in with the treatment's.
	census, validTexts, err := measureCensus(ctx, dsn, dbPath)
	if err != nil {
		return MetricSet{}, err
	}
	answer, gold, health, err := measureThroughLibrary(ctx, dsn, sc, validTexts)
	if err != nil {
		return MetricSet{}, err
	}
	out := MetricSet{}
	out.Metrics = append(out.Metrics, answer...)
	out.Metrics = append(out.Metrics, gold...)
	out.Metrics = append(out.Metrics, census...)
	out.Metrics = append(out.Metrics, health...)
	return out, nil
}

// measureThroughLibrary fires the probes and reads brain health through the
// same public facade `mnemos consolidate` and `mnemos health` use, so the
// harness measures the shipped behaviour rather than a private reimplementation
// of it.
func measureThroughLibrary(ctx context.Context, dsn string, sc Scenario, validTexts []string) (answer, gold, health []Metric, err error) {
	mem, err := mnemos.New(
		mnemos.WithStorage(dsn),
		// Passive mode pins rule-based, LLM-free behaviour regardless of what
		// MNEMOS_LLM_* happens to be set to in the operator's shell. Without
		// this, whether the harness used an LLM would depend on ambient
		// environment and two runs on two machines would not be comparable.
		mnemos.WithPassiveMode(),
		mnemos.WithActor(SeedActor),
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("brainbench: open measurement handle: %w", err)
	}
	defer func() { _ = mem.Close() }()

	answer, gold, err = measureProbes(ctx, mem, sc, validTexts)
	if err != nil {
		return nil, nil, nil, err
	}
	health, err = measureHealth(ctx, mem)
	if err != nil {
		return nil, nil, nil, err
	}
	return answer, gold, health, nil
}

// measureProbes computes the answer-quality metrics and the knowledge-survival
// metric.
func measureProbes(ctx context.Context, mem mnemos.Memory, sc Scenario, validTexts []string) (answer, gold []Metric, err error) {
	n := len(sc.Probes)
	var hits, top1, survived, forbiddenHits, forbiddenProbes int
	var rrSum float64

	for _, p := range sc.Probes {
		limit := p.Limit
		if limit == 0 {
			limit = ProbeDefaultLimit
		}
		res, err := mem.Recall(ctx, mnemos.Query{Text: p.Query, Limit: limit})
		if err != nil {
			return nil, nil, fmt.Errorf("brainbench: probe %s recall: %w", p.ID, err)
		}
		rank := 0
		for i, r := range res {
			if containsFold(r.Text, p.Expect) {
				rank = i + 1
				break
			}
		}
		if rank > 0 {
			hits++
			rrSum += 1 / float64(rank)
			if rank == 1 {
				top1++
			}
		}
		if len(p.MustNot) > 0 {
			forbiddenProbes++
			for _, r := range res {
				if matchesAnyFold(r.Text, p.MustNot) {
					forbiddenHits++
					break
				}
			}
		}
		for _, text := range validTexts {
			if containsFold(text, p.Expect) {
				survived++
				break
			}
		}
	}

	answer = []Metric{
		KnownMetric(MetricAnswerHitRate, HigherBetter, ratio(hits, n), n, "ratio"),
		KnownMetric(MetricAnswerPrecision1, HigherBetter, ratio(top1, n), n, "ratio"),
		KnownMetric(MetricAnswerMRR, HigherBetter, rrSum/float64(max(n, 1)), n, "ratio"),
	}
	if forbiddenProbes > 0 {
		answer = append(answer, KnownMetric(MetricForbiddenHitRate, LowerBetter,
			ratio(forbiddenHits, forbiddenProbes), forbiddenProbes, "ratio"))
	} else {
		// Emitting 0.0 here would read as "the brain never surfaced a
		// forbidden statement", which is not what was measured — nothing was
		// measured, because no probe declared one.
		answer = append(answer, UnknownMetric(MetricForbiddenHitRate, LowerBetter,
			"no probe in this scenario declares must_not; nothing was measured"))
	}

	// gold_survival is the metric that makes the rest trustworthy. A process
	// can raise precision, cut dissonance and shrink the brain simply by
	// deleting claims; only this metric notices, because it asks whether the
	// knowledge a probe expects is still CURRENTLY VALID at all, independent of
	// ranking. It is measured against the census's valid set (valid-time open
	// and not deprecated), not against a Scan — Scan's zero window returns
	// invalidated claims too, so surviving there would mean nothing more than
	// "the row was not deleted", which forgetting never does anyway.
	gold = []Metric{
		KnownMetric(MetricGoldSurvival, HigherBetter, ratio(survived, n), n, "ratio"),
	}
	return answer, gold, nil
}

// measureHealth mirrors the ADR 0019 vitals and pathologies into metrics.
//
// ADR 0019 already models "no data" as HealthUnknown; that maps straight onto
// this package's unknown metric, so a vital with nothing behind it stays
// unknown here instead of being flattened to its zero value. free_energy,
// calibration and dissonance all report 0.0 on an empty brain, and 0.0 is the
// BEST possible value for each — flattening would score an empty brain as
// perfectly healthy.
func measureHealth(ctx context.Context, mem mnemos.Memory) ([]Metric, error) {
	h, err := mem.BrainHealth(ctx)
	if err != nil {
		return nil, fmt.Errorf("brainbench: brain health: %w", err)
	}
	out := make([]Metric, 0, len(h.Vitals)+len(h.Pathologies))
	for _, v := range h.Vitals {
		name := MetricVitalPrefix + v.Name
		if v.Status == mnemos.HealthUnknown {
			out = append(out, UnknownMetric(name, LowerBetter,
				"ADR 0019 reports this vital as unknown (no data behind it)"))
			continue
		}
		// Every ADR 0019 vital is a badness measure — prediction error,
		// calibration error, dissonance rate, low-trust fraction, staleness
		// fraction — except skill_coverage, which is a coverage fraction.
		dir := LowerBetter
		if v.Name == "skill_coverage" {
			dir = HigherBetter
		}
		out = append(out, KnownMetric(name, dir, v.Value, 0, "ratio"))
	}
	for _, p := range h.Pathologies {
		name := MetricPathologyPrefix + p.Kind
		if p.Status == mnemos.HealthUnknown {
			out = append(out, UnknownMetric(name, LowerBetter,
				"ADR 0019 reports this pathology as unknown (no data behind it)"))
			continue
		}
		out = append(out, KnownMetric(name, LowerBetter, float64(p.Count), 0, "count"))
	}
	return out, nil
}

// measureCensus counts what is physically in the store, and returns the text of
// every currently-valid claim for the survival check.
func measureCensus(ctx context.Context, dsn, dbPath string) ([]Metric, []string, error) {
	conn, err := store.Open(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("brainbench: open census handle: %w", err)
	}
	defer func() { _ = conn.Close() }()

	claims, err := conn.Claims.ListAll(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("brainbench: list claims: %w", err)
	}
	rels, err := conn.Relationships.ListAll(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("brainbench: list relationships: %w", err)
	}
	events, err := conn.Events.ListAll(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("brainbench: list events: %w", err)
	}
	evidence, err := conn.Claims.ListAllEvidence(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("brainbench: list evidence: %w", err)
	}

	// Two distinct notions of "gone" have to be counted separately, because the
	// consolidation stages use different ones and a census that tracks only
	// status is blind to half of them. Forgetting closes VALID TIME
	// (Claims.SetValidity) and leaves status untouched; the narration prune and
	// contradiction demotion move STATUS. Counting only status made a pass that
	// reported "forgotten: 4" show zero movement in every census metric — the
	// process was working and the measurement could not see it.
	var valid, invalidated, active, deprecated, contested, junk int
	var trustSum float64
	validTexts := make([]string, 0, len(claims))
	for _, c := range claims {
		switch c.Status {
		case domain.ClaimStatusActive:
			active++
		case domain.ClaimStatusDeprecated:
			deprecated++
		case domain.ClaimStatusContested:
			contested++
		}
		// A claim counts as surviving only if BOTH gates are open: valid time
		// still open (not forgotten) and status not deprecated (not pruned or
		// demoted). Checking one alone lets the other stage's removals pass as
		// survival.
		if !c.ValidTo.IsZero() || c.Status == domain.ClaimStatusDeprecated {
			invalidated++
			continue
		}
		valid++
		validTexts = append(validTexts, c.Text)
		trustSum += c.TrustScore
		// extract.IsJunk is the SHIPPED narration filter (`mnemos prune
		// --narration` runs the same predicate over stored claims), so this is
		// mnemos's own definition of pollution rather than a metric invented
		// here to flatter a result.
		if extract.IsJunk(c.Text) {
			junk++
		}
	}
	var contradicts int
	for _, r := range rels {
		if r.Type == domain.RelationshipTypeContradicts {
			contradicts++
		}
	}

	out := []Metric{
		// Descriptive, not scored: forgetting shrinks these by design, and a
		// process that deleted the whole brain would score a perfect result on
		// any direction assigned here.
		KnownMetric(MetricValidClaims, Descriptive, float64(valid), 0, "count"),
		KnownMetric(MetricInvalidatedClaims, Descriptive, float64(invalidated), 0, "count"),
		KnownMetric(MetricActiveClaims, Descriptive, float64(active), 0, "count"),
		KnownMetric(MetricDeprecatedClaims, Descriptive, float64(deprecated), 0, "count"),
		KnownMetric(MetricEvents, Descriptive, float64(events2len(events)), 0, "count"),
		KnownMetric(MetricRelationships, Descriptive, float64(len(rels)), 0, "count"),
		KnownMetric(MetricEvidenceLinks, Descriptive, float64(len(evidence)), 0, "count"),

		// Contested claims and contradiction edges are unresolved dissonance;
		// fewer is better ONLY given gold_survival holding steady, which is
		// exactly why both are reported side by side. Resolving a
		// contradiction and deleting one side of it look identical here and
		// are told apart there.
		KnownMetric(MetricContestedClaims, LowerBetter, float64(contested), 0, "count"),
		KnownMetric(MetricContradictionEdges, LowerBetter, float64(contradicts), 0, "count"),

		// Mean trust is descriptive because forgetting raises it mechanically
		// by deleting the low-trust tail. Rising mean trust is not evidence
		// that any individual belief became better founded.
		KnownMetric(MetricMeanTrust, Descriptive, safeMean(trustSum, valid), valid, "score"),

		KnownMetric(MetricNoiseFraction, LowerBetter, ratio(junk, valid), valid, "ratio"),
	}

	if size, err := dbSize(dbPath); err == nil {
		out = append(out, KnownMetric(MetricDBBytes, Descriptive, float64(size), 0, "bytes"))
	} else {
		out = append(out, UnknownMetric(MetricDBBytes, Descriptive,
			"database file size not readable for this backend"))
	}
	return out, validTexts, nil
}

// dbSize sums the SQLite main file and its sidecars. Ignoring -wal would make
// the reported size jump around with checkpoint timing rather than with content.
func dbSize(dbPath string) (int64, error) {
	if dbPath == "" {
		return 0, fmt.Errorf("no database path")
	}
	var total int64
	var found bool
	for _, suffix := range []string{"", "-wal", "-shm"} {
		fi, err := os.Stat(dbPath + suffix)
		if err != nil {
			continue
		}
		total += fi.Size()
		found = true
	}
	if !found {
		return 0, fmt.Errorf("no database file at %s", dbPath)
	}
	return total, nil
}

func events2len[T any](s []T) int { return len(s) }

func ratio(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

func safeMean(sum float64, n int) float64 {
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(strings.TrimSpace(needle)))
}

func matchesAnyFold(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.TrimSpace(n) == "" {
			continue
		}
		if containsFold(haystack, n) {
			return true
		}
	}
	return false
}
