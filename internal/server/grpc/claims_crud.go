package grpc

import (
	"context"
	"strings"

	mnemos "go.klarlabs.de/mnemos"
	"go.klarlabs.de/mnemos/internal/domain"
	mnemosv1 "go.klarlabs.de/mnemos/proto/gen/mnemos/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Belief CRUD RPCs (gRPC parity with the HTTP GET /v1/beliefs/{id}, lifecycle,
// classify, and decision reads). Delegate to the Memory facade; a missing id
// maps to codes.NotFound.

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

// GetBelief returns a single belief's full detail.
func (s *Server) GetBelief(ctx context.Context, req *mnemosv1.GetBeliefRequest) (*mnemosv1.BeliefDetail, error) {
	if s.memFor(ctx) == nil {
		return nil, s.brainUnavailable()
	}
	c, err := s.memFor(ctx).Get(ctx, req.GetBeliefId())
	if err != nil {
		if isNotFound(err) {
			return nil, status.Error(codes.NotFound, "belief not found")
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &mnemosv1.BeliefDetail{
		Id: c.ID, Statement: c.Statement, Type: c.Type,
		Confidence: c.Confidence, TrustScore: c.TrustScore, Lifecycle: string(c.Lifecycle),
	}
	if !c.ValidFrom.IsZero() {
		out.ValidFrom = timestamppb.New(c.ValidFrom)
	}
	if c.ValidUntil != nil && !c.ValidUntil.IsZero() {
		out.ValidUntil = timestamppb.New(*c.ValidUntil)
	}
	if !c.RecordedAt.IsZero() {
		out.RecordedAt = timestamppb.New(c.RecordedAt)
	}
	return out, nil
}

// SetBeliefLifecycle transitions a belief's lifecycle.
//
// The scope guard comes before the availability check on purpose: a caller
// without claims:write should not learn from the error code whether this
// deployment has a brain attached.
func (s *Server) SetBeliefLifecycle(ctx context.Context, req *mnemosv1.SetBeliefLifecycleRequest) (*mnemosv1.SetBeliefLifecycleResponse, error) {
	if err := s.requireScope(ctx, domain.ScopeClaimsWrite); err != nil {
		return nil, err
	}
	if s.memFor(ctx) == nil {
		return nil, s.brainUnavailable()
	}
	lc := mnemos.ClaimLifecycle(req.GetLifecycle())
	switch lc {
	case mnemos.ClaimLifecycleCandidate, mnemos.ClaimLifecyclePromoted, mnemos.ClaimLifecycleSuperseded:
	default:
		return nil, status.Error(codes.InvalidArgument, "lifecycle must be candidate, promoted, or superseded")
	}
	if err := s.memFor(ctx).SetClaimLifecycle(ctx, req.GetBeliefId(), lc); err != nil {
		if isNotFound(err) {
			return nil, status.Error(codes.NotFound, "belief not found")
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &mnemosv1.SetBeliefLifecycleResponse{BeliefId: req.GetBeliefId(), Lifecycle: req.GetLifecycle()}, nil
}

// Classify reports whether a candidate statement fits established knowledge or is novel.
func (s *Server) Classify(ctx context.Context, req *mnemosv1.ClassifyRequest) (*mnemosv1.ClassifyResponse, error) {
	if s.memFor(ctx) == nil {
		return nil, s.brainUnavailable()
	}
	if strings.TrimSpace(req.GetText()) == "" {
		return nil, status.Error(codes.InvalidArgument, "text is required")
	}
	a, err := s.memFor(ctx).ClassifyClaim(ctx, req.GetText())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &mnemosv1.ClassifyResponse{Kind: a.Kind, AnchorId: a.AnchorID, AnchorText: a.AnchorText, Confidence: a.Confidence}, nil
}

// GetDecision returns a single recorded decision by id.
func (s *Server) GetDecision(ctx context.Context, req *mnemosv1.GetDecisionRequest) (*mnemosv1.Decision, error) {
	if s.memFor(ctx) == nil {
		return nil, s.brainUnavailable()
	}
	d, err := s.memFor(ctx).GetDecision(ctx, req.GetId())
	if err != nil {
		if isNotFound(err) {
			return nil, status.Error(codes.NotFound, "decision not found")
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &mnemosv1.Decision{
		Id: d.ID, Statement: d.Statement, Plan: d.Plan, Reasoning: d.Reasoning,
		RiskLevel: d.RiskLevel, Beliefs: d.Beliefs, Alternatives: d.Alternatives, OutcomeId: d.OutcomeID,
	}
	if !d.ChosenAt.IsZero() {
		out.ChosenAt = timestamppb.New(d.ChosenAt)
	}
	return out, nil
}
