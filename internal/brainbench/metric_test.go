package brainbench

import "testing"

func TestCompare_Directions(t *testing.T) {
	cases := []struct {
		name             string
		dir              Direction
		control, treated float64
		want             Verdict
	}{
		{"higher-better up is better", HigherBetter, 0.5, 0.8, VerdictBetter},
		{"higher-better down is worse", HigherBetter, 0.8, 0.5, VerdictWorse},
		{"lower-better down is better", LowerBetter, 0.8, 0.5, VerdictBetter},
		{"lower-better up is worse", LowerBetter, 0.5, 0.8, VerdictWorse},
		{"no movement is unchanged", HigherBetter, 0.5, 0.5, VerdictUnchanged},
		{"descriptive movement is unscored", Descriptive, 10, 4, VerdictChanged},
		{"descriptive movement up is unscored", Descriptive, 4, 10, VerdictChanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Compare(
				KnownMetric("m", tc.dir, tc.control, 0, "ratio"),
				KnownMetric("m", tc.dir, tc.treated, 0, "ratio"),
			)
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q (delta %+.3f)", got.Verdict, tc.want, got.Delta)
			}
		})
	}
}

// TestCompare_UnknownIsNeverOptimistic pins the rule that makes unknown
// metrics safe: a missing control must NOT be treated as zero and scored
// against, which would manufacture an improvement out of a measurement gap.
func TestCompare_UnknownIsNeverOptimistic(t *testing.T) {
	unknownControl := Compare(
		UnknownMetric("m", HigherBetter, "no data"),
		KnownMetric("m", HigherBetter, 0.9, 0, "ratio"),
	)
	if unknownControl.Verdict != VerdictUnknown {
		t.Fatalf("unknown control: verdict = %q, want %q", unknownControl.Verdict, VerdictUnknown)
	}
	if unknownControl.Delta != 0 {
		t.Fatalf("unknown control: delta = %v, want 0 (no comparison is possible)", unknownControl.Delta)
	}

	unknownTreatment := Compare(
		KnownMetric("m", HigherBetter, 0.9, 0, "ratio"),
		UnknownMetric("m", HigherBetter, "no data"),
	)
	if unknownTreatment.Verdict != VerdictUnknown {
		t.Fatalf("unknown treatment: verdict = %q, want %q", unknownTreatment.Verdict, VerdictUnknown)
	}
}

// TestCompare_ResolutionNote checks that a delta inside a small suite's 1/n
// resolution carries the caveat that stops it being read as a trend.
func TestCompare_ResolutionNote(t *testing.T) {
	withinFloor := Compare(
		KnownMetric("m", HigherBetter, 0.5, 6, "ratio"),
		KnownMetric("m", HigherBetter, 0.6, 6, "ratio"),
	)
	if withinFloor.Note == "" {
		t.Fatal("delta of 0.1 over n=6 (resolution 0.167) should carry a resolution caveat")
	}

	aboveFloor := Compare(
		KnownMetric("m", HigherBetter, 0.1, 6, "ratio"),
		KnownMetric("m", HigherBetter, 0.9, 6, "ratio"),
	)
	if aboveFloor.Note != "" {
		t.Fatalf("delta of 0.8 over n=6 is well above resolution; unexpected note %q", aboveFloor.Note)
	}
}

// TestCompareSets_AsymmetryIsVisible checks that a metric measured on only one
// arm surfaces as unknown rather than being silently dropped from the report.
func TestCompareSets_AsymmetryIsVisible(t *testing.T) {
	control := MetricSet{Metrics: []Metric{
		KnownMetric("shared", HigherBetter, 1, 0, "ratio"),
		KnownMetric("control_only", HigherBetter, 1, 0, "ratio"),
	}}
	treatment := MetricSet{Metrics: []Metric{
		KnownMetric("shared", HigherBetter, 1, 0, "ratio"),
		KnownMetric("treatment_only", HigherBetter, 1, 0, "ratio"),
	}}

	cs := CompareSets(control, treatment)
	if len(cs) != 3 {
		t.Fatalf("got %d comparisons, want 3 (shared + one per arm-only metric)", len(cs))
	}
	byName := make(map[string]Comparison, len(cs))
	for _, c := range cs {
		byName[c.Name] = c
	}
	for _, name := range []string{"control_only", "treatment_only"} {
		if byName[name].Verdict != VerdictUnknown {
			t.Errorf("%s: verdict = %q, want %q", name, byName[name].Verdict, VerdictUnknown)
		}
	}
}

func TestSummarize(t *testing.T) {
	cs := []Comparison{
		{Verdict: VerdictBetter},
		{Verdict: VerdictBetter},
		{Verdict: VerdictWorse},
		{Verdict: VerdictUnchanged},
		{Verdict: VerdictChanged},
		{Verdict: VerdictUnknown},
	}
	got := Summarize(cs)
	want := Summary{Better: 2, Worse: 1, Unchanged: 1, Changed: 1, Unknown: 1}
	if got != want {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
}

// TestVerdictFor covers the run-level reduction, especially the two verdicts
// that exist to stop activity being mistaken for benefit.
func TestVerdictFor(t *testing.T) {
	cases := []struct {
		name string
		res  ScenarioResult
		want RunVerdict
	}{
		{
			name: "nondeterministic measurement invalidates everything",
			res: ScenarioResult{
				Validity: Validity{Deterministic: false},
				Activity: Activity{Total: 50},
				Summary:  Summary{Better: 10},
			},
			want: VerdictInvalid,
		},
		{
			name: "no mutations is inert, not neutral",
			res: ScenarioResult{
				Validity: Validity{Deterministic: true},
				Activity: Activity{Total: 0},
			},
			want: VerdictInert,
		},
		{
			name: "mutations with no metric movement is activity without benefit",
			res: ScenarioResult{
				Validity: Validity{Deterministic: true},
				Activity: Activity{Total: 900},
				Summary:  Summary{Unchanged: 20, Changed: 3},
			},
			want: VerdictNoEffect,
		},
		{
			name: "improvement only",
			res: ScenarioResult{
				Validity: Validity{Deterministic: true},
				Activity: Activity{Total: 4},
				Summary:  Summary{Better: 3, Unchanged: 5},
			},
			want: VerdictImproved,
		},
		{
			name: "regression only",
			res: ScenarioResult{
				Validity: Validity{Deterministic: true},
				Activity: Activity{Total: 4},
				Summary:  Summary{Worse: 2, Unchanged: 5},
			},
			want: VerdictRegressed,
		},
		{
			name: "both directions is mixed",
			res: ScenarioResult{
				Validity: Validity{Deterministic: true},
				Activity: Activity{Total: 4},
				Summary:  Summary{Better: 2, Worse: 1},
			},
			want: VerdictMixed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := verdictFor(tc.res); got != tc.want {
				t.Fatalf("verdict = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestActivityTotal_ExcludesReadOnlyCounters pins the rule that keeps the
// inert/active distinction meaningful: trust is recomputed and claims are
// scanned on EVERY pass regardless of whether anything changed, so counting
// them would make every pass look active and no pass could ever be reported
// inert.
func TestActivityTotal_ExcludesReadOnlyCounters(t *testing.T) {
	a := activityFrom(mnemosConsolidateResult(0, 500, 300))
	if a.Total != 0 {
		t.Fatalf("total = %d, want 0: scanning and trust refresh are not mutations", a.Total)
	}
	b := activityFrom(mnemosConsolidateResult(3, 500, 300))
	if b.Total != 3 {
		t.Fatalf("total = %d, want 3", b.Total)
	}
}
