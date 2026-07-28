package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// MCP counterparts for three REST endpoints that had none.
//
// The surfaces had drifted: `mnemos serve` exposes POST on
// /v1/beliefs/<id>/expectation, /observation and /feedback, while the MCP
// server — the surface Claude Code actually speaks — exposed 45 tools and none
// of them.
//
// That gap is not cosmetic. It is why three tables sit empty on a brain with
// 86,505 claims:
//
//	claim_expectations   0   → and therefore consolidate --credit reports credited: 0
//	claim_feedback       0
//	incidents            0
//
// ADR 0014 calls credit assignment "the capstone learning loop": it propagates
// the signed prediction error of each RESOLVED expectation back to the beliefs
// that informed a decision. With no way to record an expectation, nothing ever
// resolves, and the capstone has no input. The same shape as the action/outcome
// gap — a fully-built loop with no reachable entry point.
//
// These mirror the REST handlers deliberately rather than inventing a nicer
// shape: same fields, same validation, same store calls. Two surfaces that
// disagree about semantics are worse than one surface, and the REST behaviour
// is the one already covered by tests.

type mcpRecordExpectationInput struct {
	ClaimID   string  `json:"claimId" jsonschema:"required,description=Belief this prediction attaches to"`
	Predicted float64 `json:"predicted" jsonschema:"required,description=The predicted value"`
	Tolerance float64 `json:"tolerance,omitempty" jsonschema:"description=Acceptable deviation; the prediction validates when |observed-predicted| <= tolerance"`
	Horizon   string  `json:"horizon,omitempty" jsonschema:"description=When the prediction should be judged (RFC3339 / YYYY-MM-DD / 'now')"`
}

type mcpRecordExpectationOutput struct {
	ClaimID   string  `json:"claim_id"`
	Predicted float64 `json:"predicted"`
	Tolerance float64 `json:"tolerance"`
	Horizon   string  `json:"horizon,omitempty"`
}

// mcpRunRecordExpectation attaches a forward prediction to a belief.
//
// Mirrors POST /v1/beliefs/<id>/expectation. Tolerance is normalised to its
// absolute value exactly as memory.Expect does, so a caller passing -5 gets the
// same band as one passing 5 rather than an unsatisfiable one.
func mcpRunRecordExpectation(ctx context.Context, input mcpRecordExpectationInput) (mcpRecordExpectationOutput, error) {
	claimID := strings.TrimSpace(input.ClaimID)
	if claimID == "" {
		return mcpRecordExpectationOutput{}, fmt.Errorf("claimId is required")
	}
	w, err := openWriter(ctx)
	if err != nil {
		return mcpRecordExpectationOutput{}, err
	}
	defer closeWriter(w)
	conn := w.Conn()
	if conn.Expectations == nil {
		return mcpRecordExpectationOutput{}, fmt.Errorf("expectations are not supported by this store")
	}
	found, err := conn.Claims.ListByIDs(ctx, []string{claimID})
	if err != nil {
		return mcpRecordExpectationOutput{}, fmt.Errorf("look up claim: %w", err)
	}
	if len(found) == 0 {
		return mcpRecordExpectationOutput{}, fmt.Errorf("no such belief %s", claimID)
	}

	tolerance := input.Tolerance
	if tolerance < 0 {
		tolerance = -tolerance
	}
	var horizon time.Time
	if s := strings.TrimSpace(input.Horizon); s != "" {
		t, terr := parseTimeArg(s)
		if terr != nil {
			return mcpRecordExpectationOutput{}, fmt.Errorf("horizon: %w", terr)
		}
		horizon = t
	}

	exp := domain.Expectation{
		ClaimID:   claimID,
		Predicted: input.Predicted,
		Tolerance: tolerance,
		Horizon:   horizon,
		CreatedAt: time.Now().UTC(),
	}
	if err := conn.Expectations.Upsert(ctx, exp); err != nil {
		return mcpRecordExpectationOutput{}, fmt.Errorf("upsert expectation: %w", err)
	}
	out := mcpRecordExpectationOutput{ClaimID: claimID, Predicted: exp.Predicted, Tolerance: exp.Tolerance}
	if !horizon.IsZero() {
		out.Horizon = horizon.UTC().Format(time.RFC3339)
	}
	return out, nil
}

type mcpRecordObservationInput struct {
	ClaimID  string  `json:"claimId" jsonschema:"required,description=Belief whose expectation this observation resolves"`
	Observed float64 `json:"observed" jsonschema:"required,description=The value actually observed"`
}

type mcpRecordObservationOutput struct {
	ClaimID   string  `json:"claim_id"`
	Predicted float64 `json:"predicted"`
	Observed  float64 `json:"observed"`
	Validates bool    `json:"validates"`
}

// mcpRunRecordObservation records what actually happened, resolving a belief's
// expectation into the signed prediction error credit assignment consumes.
//
// Mirrors POST /v1/beliefs/<id>/observation, including its 404 for a claim with
// no expectation: an observation with nothing to resolve is a caller error, not
// an empty success, and silently accepting it would lose the datum.
//
// `validates` is reported back because the whole point of an observation is the
// verdict, and making the caller re-derive it from predicted/observed/tolerance
// invites two implementations of one rule.
func mcpRunRecordObservation(ctx context.Context, input mcpRecordObservationInput) (mcpRecordObservationOutput, error) {
	claimID := strings.TrimSpace(input.ClaimID)
	if claimID == "" {
		return mcpRecordObservationOutput{}, fmt.Errorf("claimId is required")
	}
	w, err := openWriter(ctx)
	if err != nil {
		return mcpRecordObservationOutput{}, err
	}
	defer closeWriter(w)
	conn := w.Conn()
	if conn.Expectations == nil {
		return mcpRecordObservationOutput{}, fmt.Errorf("expectations are not supported by this store")
	}

	exp, ok, err := conn.Expectations.Get(ctx, claimID)
	if err != nil {
		return mcpRecordObservationOutput{}, fmt.Errorf("get expectation: %w", err)
	}
	if !ok {
		return mcpRecordObservationOutput{}, fmt.Errorf("no expectation for claim %s; record one first", claimID)
	}
	exp.Observed = input.Observed
	exp.HasObservation = true
	if err := conn.Expectations.Upsert(ctx, exp); err != nil {
		return mcpRecordObservationOutput{}, fmt.Errorf("upsert observation: %w", err)
	}
	return mcpRecordObservationOutput{
		ClaimID:   claimID,
		Predicted: exp.Predicted,
		Observed:  exp.Observed,
		Validates: expectationValidates(exp),
	}, nil
}

// expectationValidates reports whether an observation fell inside the predicted
// band. A zero tolerance demands an exact match, which is the honest reading of
// "predicted X with no stated slack".
func expectationValidates(e domain.Expectation) bool {
	d := e.Observed - e.Predicted
	if d < 0 {
		d = -d
	}
	return d <= e.Tolerance
}

type mcpRecordFeedbackInput struct {
	ClaimID string `json:"claimId" jsonschema:"required,description=Belief the feedback is about"`
	Helpful bool   `json:"helpful" jsonschema:"required,description=true if the recalled belief was useful; false records an unhelpful vote"`
	Note    string `json:"note,omitempty" jsonschema:"description=Why it was or was not helpful; kept verbatim for the audit trail"`
}

type mcpRecordFeedbackOutput struct {
	ClaimID                string `json:"claim_id"`
	HelpfulCount           int    `json:"helpful_count"`
	NegativeFeedbackStreak int    `json:"negative_feedback_streak"`
}

// mcpRunRecordFeedback records whether a recalled belief was actually useful.
//
// Mirrors POST /v1/beliefs/<id>/feedback. The consumer of recall is the only
// party that can judge this, and until now the only consumer that could SAY so
// was an HTTP client — not the MCP client that does the recalling.
//
// A helpful vote resets the negative streak; an unhelpful one extends it. That
// asymmetry is the REST handler's, kept deliberately: HelpfulCount is a lifetime
// corroboration tally, while the streak measures CONSECUTIVE misses, so one good
// recall should clear a run of bad ones rather than merely offset it.
func mcpRunRecordFeedback(ctx context.Context, input mcpRecordFeedbackInput) (mcpRecordFeedbackOutput, error) {
	claimID := strings.TrimSpace(input.ClaimID)
	if claimID == "" {
		return mcpRecordFeedbackOutput{}, fmt.Errorf("claimId is required")
	}
	w, err := openWriter(ctx)
	if err != nil {
		return mcpRecordFeedbackOutput{}, err
	}
	defer closeWriter(w)
	conn := w.Conn()
	if conn.Feedback == nil {
		return mcpRecordFeedbackOutput{}, fmt.Errorf("feedback is not supported by this store")
	}
	found, err := conn.Claims.ListByIDs(ctx, []string{claimID})
	if err != nil {
		return mcpRecordFeedbackOutput{}, fmt.Errorf("look up claim: %w", err)
	}
	if len(found) == 0 {
		return mcpRecordFeedbackOutput{}, fmt.Errorf("no such belief %s", claimID)
	}

	state, _, err := conn.Feedback.Get(ctx, claimID)
	if err != nil {
		return mcpRecordFeedbackOutput{}, fmt.Errorf("get feedback: %w", err)
	}
	state.ClaimID = claimID
	state.LastFeedbackAt = time.Now().UTC()
	state.LastFeedbackNote = strings.TrimSpace(input.Note)
	if input.Helpful {
		state.HelpfulCount++
		state.NegativeFeedbackStreak = 0
	} else {
		state.NegativeFeedbackStreak++
	}
	// Through the governed writer, never conn.Feedback.Upsert directly: the
	// no-bypass fitness function enforces that every delivery adapter writes via
	// the kernel, so each change lands an evidence row and is attributable. The
	// REST handler does the same.
	if _, err := w.Feedback(ctx, state); err != nil {
		return mcpRecordFeedbackOutput{}, fmt.Errorf("record feedback: %w", err)
	}
	return mcpRecordFeedbackOutput{
		ClaimID:                claimID,
		HelpfulCount:           state.HelpfulCount,
		NegativeFeedbackStreak: state.NegativeFeedbackStreak,
	}, nil
}
