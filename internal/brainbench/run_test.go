package brainbench

import (
	"context"
	"strings"
	"testing"

	"go.klarlabs.de/mnemos"

	_ "go.klarlabs.de/mnemos/internal/store/sqlite"
)

// mnemosConsolidateResult builds a ConsolidateResult with the given mutation
// count spread over one mutating counter, plus read-only counters. Used to pin
// Activity.Total's exclusion rule without depending on a live store.
func mnemosConsolidateResult(merged, scanned, trustRefreshed int) mnemos.ConsolidateResult {
	return mnemos.ConsolidateResult{
		Merged:         merged,
		ClaimsScanned:  scanned,
		TrustRefreshed: trustRefreshed,
	}
}

// experimentScenario is a small brain with genuinely superseded facts. Kept
// inline rather than reusing a shipped fixture so these tests pin harness
// behaviour and do not fail when a fixture's corpus is tuned.
func experimentScenario(process Process) Scenario {
	return Scenario{
		ID:          "unit",
		Description: "inline experiment",
		Corpus: []Doc{
			{ID: "old", AgeDays: 800, Text: "The payments API is deployed to the eu-west-1 region.\nThe rate limit is one hundred requests per minute."},
			{ID: "mid", AgeDays: 500, Text: "Deployments are performed manually by an engineer."},
			{ID: "new", AgeDays: 3, Text: "The payments API is deployed to the eu-central-1 region.\nThe rate limit is one thousand requests per minute."},
			{ID: "new2", AgeDays: 1, Text: "Deployments are performed automatically by the release pipeline."},
		},
		Probes: []Probe{
			{ID: "region", Query: "which region is the payments API deployed to", Expect: "eu-central-1", MustNot: []string{"eu-west-1"}},
			{ID: "limit", Query: "what is the rate limit", Expect: "one thousand requests"},
			{ID: "deploy", Query: "how are deployments performed", Expect: "release pipeline"},
		},
		Process: process,
	}
}

// TestRun_MeasurementIsDeterministic is the harness's own validity check,
// asserted. Two untreated arms measured independently must agree on every
// metric; if they do not, no delta the harness reports can be attributed to a
// process, and every scenario verdict becomes meaningless.
func TestRun_MeasurementIsDeterministic(t *testing.T) {
	res, err := Run(context.Background(), experimentScenario(Process{ForgetBelowTrust: 0.45}), t.TempDir())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Validity.Deterministic {
		t.Fatalf("measurement is not deterministic; drifting metrics: %v",
			res.Validity.NondeterministicMetrics)
	}
	if res.Validity.ProbeCount != 3 {
		t.Fatalf("probe count = %d, want 3", res.Validity.ProbeCount)
	}
}

// TestRun_ReportsRegressionWhenKnowledgeIsDestroyed is the single most
// important test in this package.
//
// A harness that can only report improvement is worthless, and the easiest way
// to build one by accident is to score only retrieval precision and dissonance
// — both of which a process can trivially improve by DELETING knowledge. Here
// the process forgets everything below trust 0.99, which on this corpus is
// every claim including the ones the probes need. The harness must report that
// as a regression, not as a clean sweep of improved precision.
func TestRun_ReportsRegressionWhenKnowledgeIsDestroyed(t *testing.T) {
	// 0.99 is above any trust score an ordinary corroborated claim reaches, so
	// this configuration deletes the brain.
	res, err := Run(context.Background(), experimentScenario(Process{ForgetBelowTrust: 0.99}), t.TempDir())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Validity.Deterministic {
		t.Fatalf("measurement not deterministic: %v", res.Validity.NondeterministicMetrics)
	}
	if res.Activity.Forgotten == 0 {
		t.Fatal("precondition failed: the destructive process forgot nothing")
	}

	survival, ok := comparisonNamed(res, MetricGoldSurvival)
	if !ok {
		t.Fatalf("%s must always be measured", MetricGoldSurvival)
	}
	if survival.Verdict != VerdictWorse {
		t.Fatalf("%s verdict = %q (control %.3f, treatment %.3f), want %q: "+
			"a process that deletes the knowledge the probes need MUST be reported as a regression",
			MetricGoldSurvival, survival.Verdict, survival.Control.Value, survival.Treatment.Value, VerdictWorse)
	}
	if res.Summary.Worse == 0 {
		t.Fatal("summary reports no regressions for a brain-destroying process")
	}
	if res.Verdict == VerdictImproved {
		t.Fatalf("verdict = %q for a brain-destroying process; the harness is reporting only good news", res.Verdict)
	}
}

// TestRun_ForgettingDoesNotFlatterViaDescriptiveMetrics checks the other half
// of the same guard: when knowledge is destroyed, the metrics that move
// FAVOURABLY as a side effect (a smaller brain, a higher mean trust) must be
// reported as descriptive changes and must not count toward "better".
func TestRun_ForgettingDoesNotFlatterViaDescriptiveMetrics(t *testing.T) {
	res, err := Run(context.Background(), experimentScenario(Process{ForgetBelowTrust: 0.99}), t.TempDir())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, name := range []string{MetricValidClaims, MetricMeanTrust} {
		c, ok := comparisonNamed(res, name)
		if !ok {
			t.Fatalf("%s not measured", name)
		}
		if c.Direction != Descriptive {
			t.Errorf("%s direction = %q, want %q: scoring it would reward deleting knowledge",
				name, c.Direction, Descriptive)
		}
		if c.Verdict == VerdictBetter {
			t.Errorf("%s reported as an improvement; a descriptive metric must never score", name)
		}
	}
}

// TestRun_InertProcessIsNotReportedAsNeutral pins the activity/benefit split: a
// process set that mutates nothing must be called out as inert rather than
// quietly summarised as "everything unchanged", which reads as a clean bill of
// health.
func TestRun_InertProcessIsNotReportedAsNeutral(t *testing.T) {
	// No forgetting, no decay, and a dedupe threshold of 1.0 that only exact
	// vector matches can clear.
	res, err := Run(context.Background(), experimentScenario(Process{DedupeThreshold: 1.0}), t.TempDir())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Activity.Total != 0 {
		t.Skipf("process mutated %d item(s); this scenario is not inert here", res.Activity.Total)
	}
	if res.Verdict != VerdictInert {
		t.Fatalf("verdict = %q, want %q for a process that mutated nothing", res.Verdict, VerdictInert)
	}
}

// TestRun_UnknownMetricsAreNotZeroed checks that a metric with nothing behind
// it stays unknown end to end. Flattening it to 0.0 would be actively
// misleading: 0.0 is the BEST possible value for free energy, calibration
// error and dissonance, so an empty brain would score as perfectly healthy.
func TestRun_UnknownMetricsAreNotZeroed(t *testing.T) {
	sc := experimentScenario(Process{})
	// No probe declares must_not, so the forbidden-hit rate has no sample.
	for i := range sc.Probes {
		sc.Probes[i].MustNot = nil
	}
	res, err := Run(context.Background(), sc, t.TempDir())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	c, ok := comparisonNamed(res, MetricForbiddenHitRate)
	if !ok {
		t.Fatalf("%s must appear in the report even when unmeasurable", MetricForbiddenHitRate)
	}
	if c.Verdict != VerdictUnknown {
		t.Fatalf("%s verdict = %q, want %q", MetricForbiddenHitRate, c.Verdict, VerdictUnknown)
	}
	if c.Control.Known || c.Treatment.Known {
		t.Fatal("an unmeasurable metric must not carry a value on either arm")
	}
	if !strings.Contains(c.Control.Unknown, "must_not") {
		t.Errorf("unknown reason should explain what was missing, got %q", c.Control.Unknown)
	}
}

// TestRun_ArmsShareAnIdenticalBaseline verifies the paired design at its
// foundation: the census of both untreated arms must match exactly, because
// they are byte-identical copies of one seed.
func TestRun_ArmsShareAnIdenticalBaseline(t *testing.T) {
	sc := experimentScenario(Process{})
	res, err := Run(context.Background(), sc, t.TempDir())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Validity.NondeterministicMetrics) != 0 {
		t.Fatalf("untreated arms disagreed on %v; the paired design is broken",
			res.Validity.NondeterministicMetrics)
	}
	if res.Seed.Claims == 0 {
		t.Fatal("seed produced no claims; the scenario cannot measure anything")
	}
}

func comparisonNamed(res ScenarioResult, name string) (Comparison, bool) {
	for _, c := range res.Comparisons {
		if c.Name == name {
			return c, true
		}
	}
	return Comparison{}, false
}
