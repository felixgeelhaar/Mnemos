package main

import (
	"strings"
	"testing"

	mnemos "go.klarlabs.de/mnemos"
)

func sampleBrainHealth() mnemos.BrainHealth {
	return mnemos.BrainHealth{
		Status: mnemos.HealthOK,
		Vitals: []mnemos.Vital{
			{Name: "free_energy", Value: 0.049, Status: mnemos.HealthOK, Detail: "prediction-error aggregate"},
			{Name: "dissonance", Value: 0.049, Status: mnemos.HealthOK, Detail: "active high-stakes contradictions per belief"},
		},
		Pathologies: []mnemos.Pathology{
			{Kind: "orphan_claims", Count: 0, Status: mnemos.HealthOK, Detail: "currently-valid beliefs with zero evidence"},
		},
	}
}

// The headline is user-facing, so it must not leak the internal ADR reference;
// instead the default view points the reader at --explain. Guards the fix for
// "why does it say ADR 0019 here".
func TestPrintBrainHealthHuman_NoADRInHeadline_HasExplainHint(t *testing.T) {
	out := captureStdout(t, func() { printBrainHealthHuman(sampleBrainHealth(), false) })

	if strings.Contains(out, "ADR 0019") {
		t.Errorf("default human output should not mention the ADR in the headline; got:\n%s", out)
	}
	if !strings.Contains(out, "Brain health: healthy") {
		t.Errorf("expected the verdict headline; got:\n%s", out)
	}
	if !strings.Contains(out, "mnemos health --explain") {
		t.Errorf("default output should hint at --explain; got:\n%s", out)
	}
	if strings.Contains(out, "what these mean") {
		t.Errorf("default output should not print the full guide; got:\n%s", out)
	}
}

// --explain prints the plain-language guide (each vital described) and is where
// the ADR pointer belongs — once, at the end, for the curious.
func TestPrintBrainHealthHuman_ExplainDescribesEachVital(t *testing.T) {
	out := captureStdout(t, func() { printBrainHealthHuman(sampleBrainHealth(), true) })

	for _, want := range []string{
		"what these mean",
		"free_energy", "calibration", "dissonance", "low_trust", "staleness",
		"orphan_claims", "dangling_edges", "stale_expectations",
		"worst-wins",
		"docs/adr/0019", // the ADR pointer survives, but only in the guide
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain output missing %q; got:\n%s", want, out)
		}
	}
	// With the guide shown, the redundant one-line hint should be suppressed.
	if strings.Contains(out, "Run 'mnemos health --explain'") {
		t.Errorf("--explain output should not also print the hint; got:\n%s", out)
	}
}

// #325 made an unmeasured vital report `unknown` in the data, but the human
// renderer's default branch mapped every non-warn status to "[ ok ]" — so
// `mnemos health --human` printed a green tick beside calibration over ZERO
// adjudicated beliefs while the JSON right next to it said "unknown". The
// status column is the whole point of the distinction; rendering it as OK
// undoes the fix for every reader who does not pipe the output through jq.
func TestPrintBrainHealthHuman_UnknownIsNotRenderedAsOK(t *testing.T) {
	h := sampleBrainHealth()
	h.Vitals = append(h.Vitals, mnemos.Vital{
		Name: "calibration", Value: 0, Status: mnemos.HealthUnknown,
		Detail: "expected calibration error over 0 adjudicated belief(s)",
	})
	out := captureStdout(t, func() { printBrainHealthHuman(h, false) })

	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "calibration") && strings.Contains(line, "[ ok ]") {
			t.Fatalf("an unmeasured vital must not render as ok; got:\n%s", out)
		}
	}
	if !strings.Contains(out, "[ ?? ]") {
		t.Errorf("expected an unknown marker in the human view; got:\n%s", out)
	}
}

// The guide describes every vital the report can print; a vital nobody
// explained is a number nobody can act on.
func TestPrintBrainHealthHuman_ExplainCoversTrustDecay(t *testing.T) {
	out := captureStdout(t, func() { printBrainHealthHuman(sampleBrainHealth(), true) })
	for _, want := range []string{"trust_decay", "skill_coverage", "unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain output missing %q; got:\n%s", want, out)
		}
	}
}
