package mnemos

import (
	"context"
	"testing"

	_ "go.klarlabs.de/mnemos/internal/store/memory"
)

// The regression: a vital with NO DATA reported "healthy".
//
// Calibration is 0.0 expected-calibration-error over an empty set —
// arithmetically true, and rendered as a clean bill of health for a signal
// nobody had fed. That is the same failure this codebase hit with a skipped CI
// step reading as a passed one, and with `doctor` reporting a reachable LLM
// while extraction silently fell back for months. The status is what anyone
// scans; burying "over 0 beliefs" in the detail string does not undo it.
func TestBrainHealth_CalibrationWithNoSamplesIsUnknown(t *testing.T) {
	mem, err := New(WithStorage("memory://health-unknown"), WithActor("tester"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := mem.BrainHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := vitalByName(h, "calibration"); got.Status != HealthUnknown {
		t.Errorf("calibration over 0 samples = %q, want %q", got.Status, HealthUnknown)
	}
}

// An unmeasured vital must not drag the overall verdict down, or every fresh
// brain would report degraded on day one.
func TestBrainHealth_UnknownDoesNotWorsenTheVerdict(t *testing.T) {
	mem, err := New(WithStorage("memory://health-verdict"), WithActor("tester"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := mem.BrainHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Status == HealthDegraded || h.Status == HealthUnhealthy {
		t.Errorf("an empty brain must not report %q merely because vitals are unmeasured", h.Status)
	}
}

// Every other vital is computed over CLAIMS, so a brain could hold 86,505
// beliefs with zero actions, zero outcomes and zero lessons and still show five
// green vitals — which is exactly what happened, and why finding it took a
// manual row count across 18 tables instead of a glance at `mnemos health`.
func TestBrainHealth_SkillCoverageIsReported(t *testing.T) {
	mem, err := New(WithStorage("memory://health-skill"), WithActor("tester"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h, err := mem.BrainHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// No actions yet: nothing to learn from is UNKNOWN, not a fault.
	got := vitalByName(h, "skill_coverage")
	if got.Status != HealthUnknown {
		t.Errorf("with no actions recorded, skill_coverage = %q, want %q", got.Status, HealthUnknown)
	}

	// One action with an observed outcome: the loop is closing.
	id, err := mem.RecordAction(ctx, ActionItem{Kind: "deploy", Subject: "api", Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.RecordActionOutcome(ctx, OutcomeItem{ActionID: id, Result: "success"}); err != nil {
		t.Fatal(err)
	}
	h, err = mem.BrainHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got = vitalByName(h, "skill_coverage")
	if got.Value != 1 {
		t.Errorf("skill_coverage = %v, want 1 (every action observed)", got.Value)
	}
	if got.Status != HealthOK {
		t.Errorf("full coverage should be healthy, got %q", got.Status)
	}
}

// The rate, not the count, is the measure. An action recorded with no outcome
// ever observed teaches nothing — and a pile of them is the specific failure
// worth surfacing: something records what was done but never what came of it.
func TestBrainHealth_SkillCoverageDegradesOnUnobservedActions(t *testing.T) {
	mem, err := New(WithStorage("memory://health-uncovered"), WithActor("tester"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if _, err := mem.RecordAction(ctx, ActionItem{Kind: "deploy", Subject: "api", Actor: "tester"}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := mem.BrainHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := vitalByName(h, "skill_coverage")
	if got.Value != 0 {
		t.Errorf("skill_coverage = %v, want 0 (no outcome observed)", got.Value)
	}
	if got.Status == HealthOK {
		t.Error("ten actions with zero observed outcomes must not read as healthy")
	}
}

// The same regression, one row down. low_trust and staleness are FRACTIONS of
// currently-valid beliefs, so on a brain holding none they are 0/0 — and were
// rendered as a clean "0.000 healthy", indistinguishable from a brain holding
// thousands that has genuinely lost none of them. The row is where that
// difference has to show.
func TestBrainHealth_ClaimRatesWithNoBeliefsAreUnknown(t *testing.T) {
	mem, err := New(WithStorage("memory://health-no-beliefs"), WithActor("tester"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := mem.BrainHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"low_trust", "staleness"} {
		if got := vitalByName(h, name); got.Status != HealthUnknown {
			t.Errorf("%s over 0 valid beliefs = %q, want %q", name, got.Status, HealthUnknown)
		}
	}
	// And it still must not drag the verdict down.
	if h.Status != HealthOK {
		t.Errorf("empty brain verdict = %q, want %q", h.Status, HealthOK)
	}
}
