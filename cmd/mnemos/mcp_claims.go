package main

import (
	"context"
	"strings"
	"time"

	mnemos "go.klarlabs.de/mnemos"
)

// Claim-read + advanced-recall MCP tools (parity with HTTP GET /v1/beliefs/{id},
// /v1/classify, /v1/decisions/{id}, /v1/recall and the gRPC equivalents).

func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

type mcpGetClaimInput struct {
	ClaimID string `json:"belief_id" jsonschema:"required,description=The claim id to fetch"`
}
type mcpClaimDetailOutput struct {
	ID         string  `json:"id"`
	Statement  string  `json:"statement"`
	Type       string  `json:"type"`
	Confidence float64 `json:"confidence"`
	TrustScore float64 `json:"trust_score"`
	Lifecycle  string  `json:"lifecycle,omitempty"`
	ValidFrom  string  `json:"valid_from,omitempty"`
	ValidUntil string  `json:"valid_until,omitempty"`
	RecordedAt string  `json:"recorded_at,omitempty"`
}

// mcpRunGetClaim is the get_belief handler. It resolves the brain that HOLDS
// the belief before reading it (#341) and asks memFor for a Memory bound to
// that brain — the global facade alone can only ever see the global tier, so a
// workspace belief that query_knowledge had just returned came back as an
// opaque -32603. A belief in no reachable brain surfaces as a structured
// not-found from mcpScopeToClaimBrain.
//
// memFor is injected rather than captured so the MCP server can share its
// cached facades and a test can supply throwaway ones.
func mcpRunGetClaim(ctx context.Context, memFor func(context.Context, claimBrain) (mnemos.Memory, error), in mcpGetClaimInput) (mcpClaimDetailOutput, error) {
	ctx, brain, err := mcpScopeToClaimBrain(ctx, in.ClaimID)
	if err != nil {
		return mcpClaimDetailOutput{}, err
	}
	mem, err := memFor(ctx, brain)
	if err != nil {
		return mcpClaimDetailOutput{}, err
	}
	return mcpGetClaim(ctx, mem, in)
}

func mcpGetClaim(ctx context.Context, mem mnemos.Memory, in mcpGetClaimInput) (mcpClaimDetailOutput, error) {
	c, err := mem.Get(ctx, in.ClaimID)
	if err != nil {
		return mcpClaimDetailOutput{}, err
	}
	out := mcpClaimDetailOutput{
		ID: c.ID, Statement: c.Statement, Type: c.Type, Confidence: c.Confidence,
		TrustScore: c.TrustScore, Lifecycle: string(c.Lifecycle),
		ValidFrom: rfc3339OrEmpty(c.ValidFrom), RecordedAt: rfc3339OrEmpty(c.RecordedAt),
	}
	if c.ValidUntil != nil {
		out.ValidUntil = rfc3339OrEmpty(*c.ValidUntil)
	}
	return out, nil
}

type mcpClassifyInput struct {
	Text string `json:"text" jsonschema:"required,description=Candidate statement to classify as novel or fitting"`
}
type mcpClassifyOutput struct {
	Kind       string  `json:"kind"`
	AnchorID   string  `json:"anchor_id,omitempty"`
	AnchorText string  `json:"anchor_text,omitempty"`
	Confidence float64 `json:"confidence"`
}

func mcpClassify(ctx context.Context, mem mnemos.Memory, in mcpClassifyInput) (mcpClassifyOutput, error) {
	a, err := mem.ClassifyClaim(ctx, in.Text)
	if err != nil {
		return mcpClassifyOutput{}, err
	}
	return mcpClassifyOutput{Kind: a.Kind, AnchorID: a.AnchorID, AnchorText: a.AnchorText, Confidence: a.Confidence}, nil
}

type mcpGetDecisionInput struct {
	ID string `json:"id" jsonschema:"required,description=The decision id to fetch"`
}
type mcpDecisionOutput struct {
	ID           string   `json:"id"`
	Statement    string   `json:"statement"`
	Plan         string   `json:"plan,omitempty"`
	Reasoning    string   `json:"reasoning,omitempty"`
	RiskLevel    string   `json:"risk_level,omitempty"`
	Beliefs      []string `json:"beliefs,omitempty"`
	Alternatives []string `json:"alternatives,omitempty"`
	OutcomeID    string   `json:"outcome_id,omitempty"`
	ChosenAt     string   `json:"chosen_at,omitempty"`
}

func mcpGetDecision(ctx context.Context, mem mnemos.Memory, in mcpGetDecisionInput) (mcpDecisionOutput, error) {
	d, err := mem.GetDecision(ctx, in.ID)
	if err != nil {
		return mcpDecisionOutput{}, err
	}
	return mcpDecisionOutput{
		ID: d.ID, Statement: d.Statement, Plan: d.Plan, Reasoning: d.Reasoning, RiskLevel: d.RiskLevel,
		Beliefs: d.Beliefs, Alternatives: d.Alternatives, OutcomeID: d.OutcomeID, ChosenAt: rfc3339OrEmpty(d.ChosenAt),
	}, nil
}

type mcpRecallInput struct {
	Query     string  `json:"query" jsonschema:"required,description=What to retrieve"`
	Mode      string  `json:"mode,omitempty" jsonschema:"description=sufficiency (default) | effort | context | conflicts | iterative"`
	RunID     string  `json:"run_id,omitempty"`
	Limit     int     `json:"limit,omitempty"`
	Hops      int     `json:"hops,omitempty"`
	Stakes    float64 `json:"stakes,omitempty" jsonschema:"description=effort mode: scales the retrieval budget"`
	Context   string  `json:"context,omitempty" jsonschema:"description=context mode: bias string"`
	MaxRounds int     `json:"max_rounds,omitempty" jsonschema:"description=iterative mode: cap on rounds"`
}
type mcpRecallResult struct {
	ClaimID     string  `json:"belief_id"`
	Text        string  `json:"text"`
	Type        string  `json:"type"`
	Confidence  float64 `json:"confidence"`
	TrustScore  float64 `json:"trust_score"`
	HopDistance int     `json:"hop_distance"`
	Provenance  string  `json:"provenance,omitempty"`
}
type mcpRecallSufficiency struct {
	Confidence float64 `json:"confidence"`
	ClaimCount int     `json:"belief_count"`
	Sufficient bool    `json:"sufficient"`
	Floor      float64 `json:"floor"`
}
type mcpRecallEffort struct {
	Budget  float64 `json:"budget"`
	Passes  int     `json:"passes"`
	Hops    int     `json:"hops"`
	Breadth int     `json:"breadth"`
}
type mcpRecallConflict struct {
	ClaimID            string  `json:"belief_id"`
	ClaimText          string  `json:"belief_text"`
	ContradictingID    string  `json:"contradicting_id"`
	ContradictingText  string  `json:"contradicting_text"`
	ContradictingTrust float64 `json:"contradicting_trust"`
}
type mcpRecallOutput struct {
	Mode        string                `json:"mode"`
	Results     []mcpRecallResult     `json:"results"`
	Sufficiency *mcpRecallSufficiency `json:"sufficiency,omitempty"`
	Effort      *mcpRecallEffort      `json:"effort,omitempty"`
	Conflicts   []mcpRecallConflict   `json:"conflicts,omitempty"`
	Rounds      int                   `json:"rounds,omitempty"`
}

func mcpRecallResults(rs []mnemos.Result) []mcpRecallResult {
	out := make([]mcpRecallResult, 0, len(rs))
	for _, r := range rs {
		out = append(out, mcpRecallResult{r.ClaimID, r.Text, r.Type, r.Confidence, r.TrustScore, r.HopDistance, r.Provenance})
	}
	return out
}
func mcpSufficiency(s mnemos.Sufficiency) *mcpRecallSufficiency {
	return &mcpRecallSufficiency{s.Confidence, s.ClaimCount, s.Sufficient, s.Floor}
}

func mcpRecall(ctx context.Context, mem mnemos.Memory, in mcpRecallInput) (mcpRecallOutput, error) {
	q := mnemos.Query{Text: in.Query, RunID: in.RunID, Limit: in.Limit, Hops: in.Hops}
	mode := in.Mode
	if mode == "" {
		mode = "sufficiency"
	}
	out := mcpRecallOutput{Mode: mode, Results: []mcpRecallResult{}}
	switch mode {
	case "sufficiency":
		res, suf, err := mem.RecallWithSufficiency(ctx, q)
		if err != nil {
			return mcpRecallOutput{}, err
		}
		out.Results, out.Sufficiency = mcpRecallResults(res), mcpSufficiency(suf)
	case "effort":
		res, suf, eff, err := mem.RecallWithEffort(ctx, q, in.Stakes)
		if err != nil {
			return mcpRecallOutput{}, err
		}
		out.Results, out.Sufficiency = mcpRecallResults(res), mcpSufficiency(suf)
		out.Effort = &mcpRecallEffort{eff.Budget, eff.Passes, eff.Hops, eff.Breadth}
	case "context":
		res, err := mem.RecallWithContext(ctx, q, in.Context)
		if err != nil {
			return mcpRecallOutput{}, err
		}
		out.Results = mcpRecallResults(res)
	case "conflicts":
		res, conflicts, err := mem.RecallWithConflicts(ctx, q)
		if err != nil {
			return mcpRecallOutput{}, err
		}
		out.Results = mcpRecallResults(res)
		for _, c := range conflicts {
			out.Conflicts = append(out.Conflicts, mcpRecallConflict{c.ClaimID, c.ClaimText, c.ContradictingID, c.ContradictingText, c.ContradictingTrust})
		}
	case "iterative":
		maxRounds := in.MaxRounds
		if maxRounds <= 0 {
			maxRounds = 3
		}
		res, rounds, err := mem.RecallIterative(ctx, q, maxRounds)
		if err != nil {
			return mcpRecallOutput{}, err
		}
		out.Results, out.Rounds = mcpRecallResults(res), rounds
	default:
		return mcpRecallOutput{}, &mnemosModeError{mode: mode}
	}
	return out, nil
}

type mnemosModeError struct{ mode string }

func (e *mnemosModeError) Error() string {
	return "unknown recall mode " + strings.TrimSpace(e.mode) + " (want sufficiency, effort, context, conflicts, or iterative)"
}
