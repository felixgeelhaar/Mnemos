package main

import (
	"context"
	"testing"

	"go.klarlabs.de/mnemos/internal/domain"
)

// seedClaim puts one belief in a throwaway store and returns its id.
func seedClaim(t *testing.T) string {
	t.Helper()
	t.Setenv("MNEMOS_DB_URL", "sqlite://"+t.TempDir()+"/parity.db")

	ctx := context.Background()
	out, err := mcpRunRemember(ctx, "tester", mcpRememberInput{
		Text:  "p95 latency stays under 300ms after the rollout",
		Kind:  string(domain.ClaimTypeHypothesis),
		RunID: "parity-test",
	})
	if err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	return out.ClaimID
}

// The gap this closes: `mnemos serve` exposes POST
// /v1/beliefs/<id>/expectation, and the MCP surface — the one Claude Code
// speaks — had no counterpart. With no way to record an expectation, nothing
// ever resolves, and `consolidate --credit` reports credited: 0 forever.
func TestRecordExpectationAndObservation_ResolveThePrediction(t *testing.T) {
	id := seedClaim(t)
	ctx := context.Background()

	if _, err := mcpRunRecordExpectation(ctx, mcpRecordExpectationInput{
		ClaimID: id, Predicted: 300, Tolerance: 25,
	}); err != nil {
		t.Fatalf("record_expectation: %v", err)
	}

	got, err := mcpRunRecordObservation(ctx, mcpRecordObservationInput{ClaimID: id, Observed: 310})
	if err != nil {
		t.Fatalf("record_observation: %v", err)
	}
	if !got.Validates {
		t.Errorf("310 is within 300±25 and must validate: %+v", got)
	}

	// Outside the band the same pair must refute.
	if _, err := mcpRunRecordObservation(ctx, mcpRecordObservationInput{ClaimID: id, Observed: 400}); err != nil {
		t.Fatalf("record_observation (refuting): %v", err)
	}
	refuted, err := mcpRunRecordObservation(ctx, mcpRecordObservationInput{ClaimID: id, Observed: 400})
	if err != nil {
		t.Fatalf("record_observation: %v", err)
	}
	if refuted.Validates {
		t.Error("400 is outside 300±25 and must not validate")
	}
}

// A negative tolerance is normalised, matching memory.Expect. Passing -25 must
// produce the same band as 25 rather than an unsatisfiable one.
func TestRecordExpectation_NormalisesNegativeTolerance(t *testing.T) {
	id := seedClaim(t)
	got, err := mcpRunRecordExpectation(context.Background(), mcpRecordExpectationInput{
		ClaimID: id, Predicted: 300, Tolerance: -25,
	})
	if err != nil {
		t.Fatalf("record_expectation: %v", err)
	}
	if got.Tolerance != 25 {
		t.Errorf("tolerance = %v, want 25 (absolute)", got.Tolerance)
	}
}

// An observation with nothing to resolve is a caller error, not an empty
// success — silently accepting it would lose the datum. Mirrors the REST 404.
func TestRecordObservation_RefusesWithoutAnExpectation(t *testing.T) {
	id := seedClaim(t)
	if _, err := mcpRunRecordObservation(context.Background(), mcpRecordObservationInput{
		ClaimID: id, Observed: 1,
	}); err == nil {
		t.Error("an observation with no expectation must fail, not silently succeed")
	}
}

// A helpful vote RESETS the negative streak rather than merely offsetting it:
// HelpfulCount is a lifetime tally, the streak measures CONSECUTIVE misses.
func TestRecordFeedback_HelpfulResetsTheNegativeStreak(t *testing.T) {
	id := seedClaim(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := mcpRunRecordFeedback(ctx, mcpRecordFeedbackInput{
			ClaimID: id, Helpful: false, Note: "not relevant",
		}); err != nil {
			t.Fatalf("record_feedback: %v", err)
		}
	}
	got, err := mcpRunRecordFeedback(ctx, mcpRecordFeedbackInput{ClaimID: id, Helpful: true})
	if err != nil {
		t.Fatalf("record_feedback: %v", err)
	}
	if got.NegativeFeedbackStreak != 0 {
		t.Errorf("a helpful vote must reset the streak, got %d", got.NegativeFeedbackStreak)
	}
	if got.HelpfulCount != 1 {
		t.Errorf("helpful_count = %d, want 1", got.HelpfulCount)
	}
}

// Every one of these writes must reject an unknown belief rather than create a
// dangling row keyed to an id that does not exist.
func TestParityTools_RejectUnknownBeliefs(t *testing.T) {
	seedClaim(t) // establishes the store
	ctx := context.Background()

	if _, err := mcpRunRecordExpectation(ctx, mcpRecordExpectationInput{ClaimID: "cl_nope", Predicted: 1}); err == nil {
		t.Error("record_expectation accepted an unknown belief")
	}
	if _, err := mcpRunRecordFeedback(ctx, mcpRecordFeedbackInput{ClaimID: "cl_nope", Helpful: true}); err == nil {
		t.Error("record_feedback accepted an unknown belief")
	}
}
