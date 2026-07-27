package grpc

import (
	"context"
	"time"

	mnemos "go.klarlabs.de/mnemos"
	"go.klarlabs.de/mnemos/internal/domain"
	mnemosv1 "go.klarlabs.de/mnemos/proto/gen/mnemos/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Working memory + skill loop + temporal RPCs (gRPC parity with the HTTP
// /v1/blocks, /v1/synthesize, /v1/timeline, /v1/signals). Delegate to the
// Memory facade; nil facade -> codes.Unavailable.

func tsOrZero(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

func tsOrNil(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// GetBlocks returns an owner's working-memory blocks.
func (s *Server) GetBlocks(ctx context.Context, req *mnemosv1.GetBlocksRequest) (*mnemosv1.GetBlocksResponse, error) {
	if s.memFor(ctx) == nil {
		return nil, s.brainUnavailable()
	}
	if req.GetOwner() == "" {
		return nil, status.Error(codes.InvalidArgument, "owner is required")
	}
	blocks, err := s.memFor(ctx).Blocks(ctx, req.GetOwner())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &mnemosv1.GetBlocksResponse{Owner: req.GetOwner()}
	for _, b := range blocks {
		out.Blocks = append(out.Blocks, &mnemosv1.MemoryBlock{
			Owner: b.Owner, Label: b.Label, Value: b.Value, UpdatedAt: tsOrNil(b.UpdatedAt),
		})
	}
	return out, nil
}

// SetBlock sets or appends a working-memory block.
//
// Scope first, availability second: an unauthorized caller learns only that it
// is unauthorized, not whether a brain is attached.
func (s *Server) SetBlock(ctx context.Context, req *mnemosv1.SetBlockRequest) (*mnemosv1.SetBlockResponse, error) {
	if err := s.requireScope(ctx, domain.ScopeClaimsWrite); err != nil {
		return nil, err
	}
	if s.memFor(ctx) == nil {
		return nil, s.brainUnavailable()
	}
	if req.GetOwner() == "" || req.GetLabel() == "" {
		return nil, status.Error(codes.InvalidArgument, "owner and label are required")
	}
	var err error
	if req.GetAppend() {
		err = s.memFor(ctx).AppendBlock(ctx, req.GetOwner(), req.GetLabel(), req.GetValue())
	} else {
		err = s.memFor(ctx).SetBlock(ctx, req.GetOwner(), req.GetLabel(), req.GetValue())
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &mnemosv1.SetBlockResponse{Owner: req.GetOwner(), Label: req.GetLabel()}, nil
}

// Synthesize runs a synthesis pass (actions -> schemas + reflexes). It derives
// and persists schemas and reflexes, so it is a write however read-only its
// name sounds — hence claims:write, matching POST /v1/synthesize.
func (s *Server) Synthesize(ctx context.Context, _ *mnemosv1.SynthesizeRequest) (*mnemosv1.SynthesizeResponse, error) {
	if err := s.requireScope(ctx, domain.ScopeClaimsWrite); err != nil {
		return nil, err
	}
	if s.memFor(ctx) == nil {
		return nil, s.brainUnavailable()
	}
	res, err := s.memFor(ctx).Synthesize(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &mnemosv1.SynthesizeResponse{
		SchemasDerived: int32(res.LessonsDerived), ReflexesDerived: int32(res.PlaybooksDerived),
	}, nil
}

// Timeline returns episodes on the temporal timeline.
func (s *Server) Timeline(ctx context.Context, req *mnemosv1.TimelineRequest) (*mnemosv1.TimelineResponse, error) {
	if s.memFor(ctx) == nil {
		return nil, s.brainUnavailable()
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 50
	}
	events, err := s.memFor(ctx).Timeline(ctx, mnemos.TimelineQuery{
		RunID: req.GetRunId(), From: tsOrZero(req.GetFrom()), To: tsOrZero(req.GetTo()),
		Types: req.GetTypes(), Limit: limit,
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &mnemosv1.TimelineResponse{}
	for _, e := range events {
		out.Episodes = append(out.Episodes, &mnemosv1.TimelineEpisode{
			Id: e.ID, At: tsOrNil(e.At), Type: e.Type, Content: e.Content, RunId: e.RunID, Metadata: e.Metadata,
		})
	}
	return out, nil
}

// Signals returns detected temporal patterns.
func (s *Server) Signals(ctx context.Context, req *mnemosv1.SignalsRequest) (*mnemosv1.SignalsResponse, error) {
	if s.memFor(ctx) == nil {
		return nil, s.brainUnavailable()
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 50
	}
	signals, err := s.memFor(ctx).Signals(ctx, mnemos.SignalQuery{
		RunID: req.GetRunId(), MinConfidence: req.GetMinConfidence(), Limit: limit,
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &mnemosv1.SignalsResponse{}
	for _, sig := range signals {
		out.Signals = append(out.Signals, &mnemosv1.MemorySignal{
			Pattern: sig.Pattern, RunId: sig.RunID, Strength: sig.Strength, Confidence: sig.Confidence,
			SignalClass: sig.Class, DetectedAt: tsOrNil(sig.DetectedAt),
			WindowStart: tsOrNil(sig.WindowStart), WindowEnd: tsOrNil(sig.WindowEnd),
		})
	}
	return out, nil
}
