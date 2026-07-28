package main

import (
	"context"
	"testing"

	mnemos "go.klarlabs.de/mnemos"
)

// The connected-brain MCP tool handlers run against the library facade and
// return well-formed (empty) results over an empty store — this exercises the
// delegation + output shaping without a full MCP stdio round-trip.
func TestMCPCognitiveHandlers(t *testing.T) {
	mem, err := mnemos.New(mnemos.WithStorage("memory://"))
	if err != nil {
		t.Fatalf("build facade: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	ctx := context.Background()

	wk, err := mcpWhoKnows(ctx, mem, mcpWhoKnowsInput{Query: "creatinine", Limit: 5})
	if err != nil || wk.Experts == nil {
		t.Fatalf("who_knows: %v %+v", err, wk)
	}
	if kg, err := mcpKnowledgeGaps(ctx, mem, mcpKnowledgeGapsInput{}); err != nil || kg.Gaps == nil {
		t.Fatalf("knowledge_gaps: %v %+v", err, kg)
	}
	if _, err := mcpCalibration(ctx, mem, struct{}{}); err != nil {
		t.Fatalf("calibration: %v", err)
	}
	if pe, err := mcpPredictiveError(ctx, mem, struct{}{}); err != nil || len(pe.Levels) != 4 {
		t.Fatalf("predictive_error: %v %+v", err, pe)
	}
	if hc, err := mcpHypercorrections(ctx, mem, struct{}{}); err != nil || hc.Hypercorrections == nil {
		t.Fatalf("hypercorrections: %v %+v", err, hc)
	}
	if rc, err := mcpRecombinations(ctx, mem, mcpRecombinationsInput{}); err != nil || rc.Recombinations == nil {
		t.Fatalf("recombinations: %v %+v", err, rc)
	}
	an, err := mcpAnalogousClaims(ctx, mem, mcpAnalogousInput{ClaimID: "cl1", Limit: 3})
	if err != nil || an.Analogous == nil || an.ClaimID != "cl1" {
		t.Fatalf("analogous_claims: %v %+v", err, an)
	}
}

// TestMCPCognitive_TruncationReachesTheAgent is the delivery-side half of the
// bound: the library can cap all it likes, but if the MCP response does not
// carry mnemos.Bounds the agent reads a bounded answer as the whole brain.
func TestMCPCognitive_TruncationReachesTheAgent(t *testing.T) {
	mem, err := mnemos.New(mnemos.WithStorage("memory://?namespace=mcp_bounds"), mnemos.WithPassiveMode())
	if err != nil {
		t.Fatalf("build facade: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	ctx := context.Background()

	// Twelve unresolved hypotheses: knowledge_gaps has more to say than the
	// limit of 2 asked for below, so its answer is genuinely truncated.
	const seeded = 12
	for i := 0; i < seeded; i++ {
		if err := mem.Remember(ctx, mnemos.Item{
			Type:    "hypothesis",
			Content: "We suspect the checkout latency regression came from cache shard " + string(rune('a'+i)) + ".",
		}); err != nil {
			t.Fatalf("remember %d: %v", i, err)
		}
	}

	kg, err := mcpKnowledgeGaps(ctx, mem, mcpKnowledgeGapsInput{Limit: 2})
	if err != nil {
		t.Fatalf("knowledge_gaps: %v", err)
	}
	if len(kg.Gaps) != 2 {
		t.Fatalf("got %d gaps, want the limit 2", len(kg.Gaps))
	}
	if !kg.Bounds.Truncated {
		t.Fatalf("knowledge_gaps response does not report truncation: %+v", kg.Bounds)
	}
	if kg.Bounds.Available != seeded {
		t.Errorf("Bounds.Available = %d, want the true gap count %d", kg.Bounds.Available, seeded)
	}
	if kg.Bounds.Notice == "" {
		t.Error("Bounds.Notice is empty; the agent gets no words explaining the cut")
	}
	if kg.Bounds.Limit != 2 {
		t.Errorf("Bounds.Limit = %d, want the effective limit 2", kg.Bounds.Limit)
	}

	// A complete answer must NOT claim truncation — the opposite failure, and the
	// one that teaches callers to ignore the flag.
	full, err := mcpKnowledgeGaps(ctx, mem, mcpKnowledgeGapsInput{Limit: 100})
	if err != nil {
		t.Fatalf("knowledge_gaps (full): %v", err)
	}
	if full.Bounds.Truncated || full.Bounds.Notice != "" {
		t.Errorf("complete knowledge_gaps answer flagged truncated: %+v", full.Bounds)
	}

	// Every limit-taking cognitive tool reports the server's own ceiling on a
	// greedy caller limit, so an agent can tell 200-of-many from "all 200".
	greedy := func(name string, b mnemos.Bounds) {
		t.Helper()
		if b.Limit != mnemos.MaxCognitiveResults {
			t.Errorf("%s: Bounds.Limit = %d, want the capped %d", name, b.Limit, mnemos.MaxCognitiveResults)
		}
		var saw bool
		for _, r := range b.Reasons {
			if r == mnemos.BoundReasonLimitCapped {
				saw = true
			}
		}
		if !saw {
			t.Errorf("%s: Reasons = %v, want %q", name, b.Reasons, mnemos.BoundReasonLimitCapped)
		}
	}
	const huge = 1_000_000
	rc, err := mcpRecombinations(ctx, mem, mcpRecombinationsInput{Limit: huge})
	if err != nil {
		t.Fatalf("recombinations: %v", err)
	}
	greedy("recombinations", rc.Bounds)

	kg2, err := mcpKnowledgeGaps(ctx, mem, mcpKnowledgeGapsInput{Limit: huge})
	if err != nil {
		t.Fatalf("knowledge_gaps (greedy): %v", err)
	}
	greedy("knowledge_gaps", kg2.Bounds)

	wk, err := mcpWhoKnows(ctx, mem, mcpWhoKnowsInput{Query: "checkout latency", Limit: huge})
	if err != nil {
		t.Fatalf("who_knows: %v", err)
	}
	greedy("who_knows", wk.Bounds)

	an, err := mcpAnalogousClaims(ctx, mem, mcpAnalogousInput{ClaimID: "cl1", Limit: huge})
	if err != nil {
		t.Fatalf("analogous_beliefs: %v", err)
	}
	greedy("analogous_beliefs", an.Bounds)

	// hypercorrections takes no limit; its bounds must still ride along, and a
	// contradiction-free brain must not look truncated.
	hc, err := mcpHypercorrections(ctx, mem, struct{}{})
	if err != nil {
		t.Fatalf("hypercorrections: %v", err)
	}
	if hc.Bounds.Truncated {
		t.Errorf("hypercorrections on a contradiction-free brain flagged truncated: %+v", hc.Bounds)
	}
	if hc.Bounds.Limit != mnemos.HypercorrectionDefaultLimit {
		t.Errorf("hypercorrections Bounds.Limit = %d, want the default page %d",
			hc.Bounds.Limit, mnemos.HypercorrectionDefaultLimit)
	}
}
