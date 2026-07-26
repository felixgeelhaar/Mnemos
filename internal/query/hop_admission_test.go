package query

import (
	"context"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// Hop expansion used to append its results to the answer RAW — after every
// filter had already run over the direct hits. That made the whole filter
// chain escapable by asking for one hop:
//
//   - a claim deprecated by `forget` / `memory_deprecate` came straight back;
//   - --min-trust, --scope, --visibility, --lifecycle, --at and --as-of-recorded
//     all admitted claims they were asked to exclude;
//   - and the survivors were never credibility-rescored, so they carried their
//     raw stored trust_score into a ranking against rescored direct hits.
//
// These tests pin each of those. They all share one shape: a seed claim the
// events resolve to, and one neighbour reachable over a single supports edge
// that the filter under test must reject.

// hopAdmissionFixture builds an engine whose only directly-retrievable claim is
// cl_seed, with cl_hop one supports-edge away and reachable only via expansion.
func hopAdmissionFixture(t *testing.T, seed, hop domain.Claim, now time.Time) Engine {
	t.Helper()

	events := fakeEventRepo{events: []domain.Event{
		{ID: "ev_seed", RunID: "r", Content: "Seed event about cache eviction policy", Timestamp: now},
	}}
	repo := fakeClaimRepo{
		claims:   []domain.Claim{seed},
		evidence: []domain.ClaimEvidence{{ClaimID: seed.ID, EventID: "ev_seed"}},
	}
	wrapper := hopFakeClaimRepo{fakeClaimRepo: repo, all: []domain.Claim{seed, hop}}
	edge := domain.Relationship{
		ID:          "r1",
		Type:        domain.RelationshipTypeSupports,
		FromClaimID: seed.ID,
		ToClaimID:   hop.ID,
		CreatedAt:   now,
	}
	rels := fakeRelationshipRepo{rels: map[string][]domain.Relationship{
		seed.ID: {edge},
		hop.ID:  {edge},
	}}

	return NewEngine(events, wrapper, rels)
}

func claimIDs(claims []domain.Claim) []string {
	out := make([]string, 0, len(claims))
	for _, c := range claims {
		out = append(out, c.ID)
	}
	return out
}

func containsClaim(claims []domain.Claim, id string) bool {
	for _, c := range claims {
		if c.ID == id {
			return true
		}
	}
	return false
}

func TestHopExpansion_DeprecatedClaimStaysForgotten(t *testing.T) {
	now := time.Date(2026, 4, 18, 9, 0, 0, 0, time.UTC)

	seed := domain.Claim{
		ID: "cl_seed", Text: "Cache eviction is LRU", Type: domain.ClaimTypeFact,
		Status: domain.ClaimStatusActive, Confidence: 0.9, CreatedAt: now,
	}
	forgotten := domain.Claim{
		ID: "cl_hop", Text: "Cache eviction is FIFO", Type: domain.ClaimTypeFact,
		Status: domain.ClaimStatusDeprecated, Confidence: 0.9, CreatedAt: now,
	}
	engine := hopAdmissionFixture(t, seed, forgotten, now)

	ans, err := engine.AnswerWithOptions(context.Background(), "cache eviction policy", AnswerOptions{Hops: 2})
	if err != nil {
		t.Fatalf("AnswerWithOptions: %v", err)
	}
	if containsClaim(ans.Claims, "cl_hop") {
		t.Fatalf("deprecated claim came back through hop expansion — `forget` does not forget; got %v", claimIDs(ans.Claims))
	}
	if _, ok := ans.ClaimHopDistance["cl_hop"]; ok {
		t.Errorf("ClaimHopDistance still records the rejected claim: %v", ans.ClaimHopDistance)
	}
	if !containsClaim(ans.Claims, "cl_seed") {
		t.Errorf("direct hit lost: %v", claimIDs(ans.Claims))
	}
}

func TestHopExpansion_MinTrustIsNotEscapable(t *testing.T) {
	now := time.Date(2026, 4, 18, 9, 0, 0, 0, time.UTC)

	// A claim that has aged out of every recency/liveness signal scores far
	// below the direct hit, so a MinTrust gate that admits the seed must still
	// reject the neighbour.
	old := now.AddDate(-4, 0, 0)
	seed := domain.Claim{
		ID: "cl_seed", Text: "Cache eviction is LRU", Type: domain.ClaimTypeFact,
		Status: domain.ClaimStatusActive, Confidence: 0.9, TrustScore: 0.95,
		SourceAuthority: 0.95, CitationCount: 8, CreatedAt: now, LastVerified: now,
	}
	lowTrust := domain.Claim{
		ID: "cl_hop", Text: "Cache eviction is FIFO", Type: domain.ClaimTypeFact,
		Status: domain.ClaimStatusActive, Confidence: 0.9, TrustScore: 0.05,
		SourceAuthority: 0.05, CreatedAt: old, ValidFrom: old, LastVerified: old,
	}
	engine := hopAdmissionFixture(t, seed, lowTrust, now)

	// Sanity: with no gate, expansion reaches it — otherwise the assertion
	// below would pass vacuously.
	open, err := engine.AnswerWithOptions(context.Background(), "cache eviction policy", AnswerOptions{Hops: 2})
	if err != nil {
		t.Fatalf("AnswerWithOptions(no gate): %v", err)
	}
	if !containsClaim(open.Claims, "cl_hop") {
		t.Fatalf("fixture broken: hop neighbour unreachable even without a trust gate")
	}

	gated, err := engine.AnswerWithOptions(context.Background(), "cache eviction policy",
		AnswerOptions{Hops: 2, MinTrust: 0.5})
	if err != nil {
		t.Fatalf("AnswerWithOptions(min-trust): %v", err)
	}
	if containsClaim(gated.Claims, "cl_hop") {
		t.Fatalf("low-trust claim escaped --min-trust via a hop; got %v", claimIDs(gated.Claims))
	}
	if !containsClaim(gated.Claims, "cl_seed") {
		t.Fatalf("high-trust direct hit was dropped: %v", claimIDs(gated.Claims))
	}
}

func TestHopExpansion_RespectsScopeVisibilityLifecycleAndTime(t *testing.T) {
	now := time.Date(2026, 4, 18, 9, 0, 0, 0, time.UTC)
	base := domain.Claim{
		ID: "cl_hop", Text: "Cache eviction is FIFO", Type: domain.ClaimTypeFact,
		Status: domain.ClaimStatusActive, Confidence: 0.9, CreatedAt: now,
	}

	withField := func(mutate func(c *domain.Claim)) domain.Claim {
		c := base
		mutate(&c)
		return c
	}

	tests := []struct {
		name string
		hop  domain.Claim
		opts AnswerOptions
	}{
		{
			name: "scope",
			hop:  withField(func(c *domain.Claim) { c.Scope = domain.Scope{Service: "billing"} }),
			opts: AnswerOptions{Hops: 2, Scope: domain.Scope{Service: "payments"}},
		},
		{
			name: "visibility",
			hop:  withField(func(c *domain.Claim) { c.Visibility = domain.VisibilityOrg }),
			opts: AnswerOptions{Hops: 2, Visibility: domain.VisibilityTeam},
		},
		{
			name: "lifecycle",
			hop:  withField(func(c *domain.Claim) { c.Lifecycle = domain.ClaimLifecycleCandidate }),
			opts: AnswerOptions{Hops: 2, Lifecycle: domain.ClaimLifecyclePromoted},
		},
		{
			name: "valid-time (--at)",
			hop:  withField(func(c *domain.Claim) { c.ValidFrom = now.AddDate(0, 0, -30); c.ValidTo = now.AddDate(0, 0, -1) }),
			opts: AnswerOptions{Hops: 2},
		},
		{
			name: "recorded-time (--as-of-recorded)",
			hop:  withField(func(c *domain.Claim) { c.CreatedAt = now }),
			opts: AnswerOptions{Hops: 2, RecordedAsOf: now.AddDate(0, 0, -1)},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seed := domain.Claim{
				ID: "cl_seed", Text: "Cache eviction is LRU", Type: domain.ClaimTypeFact,
				Status: domain.ClaimStatusActive, Confidence: 0.9,
				CreatedAt: now.AddDate(0, 0, -10), ValidFrom: now.AddDate(0, 0, -10),
				Scope:     domain.Scope{Service: "payments"},
				Lifecycle: domain.ClaimLifecyclePromoted,
			}
			engine := hopAdmissionFixture(t, seed, tc.hop, now)

			ans, err := engine.AnswerWithOptions(context.Background(), "cache eviction policy", tc.opts)
			if err != nil {
				t.Fatalf("AnswerWithOptions: %v", err)
			}
			if containsClaim(ans.Claims, "cl_hop") {
				t.Fatalf("hop expansion bypassed the %s filter; got %v", tc.name, claimIDs(ans.Claims))
			}
			if !containsClaim(ans.Claims, "cl_seed") {
				t.Fatalf("direct hit lost under the %s filter: %v", tc.name, claimIDs(ans.Claims))
			}
		})
	}
}

func TestHopExpansion_RescoresCredibilityLikeDirectHits(t *testing.T) {
	now := time.Date(2026, 4, 18, 9, 0, 0, 0, time.UTC)

	// Identical provenance on both claims: whatever trust the direct hit is
	// recomputed to, the hop-expanded one must be recomputed to as well. A
	// hop claim that keeps its raw stored score is being ranked on a
	// different scale from its neighbours.
	stored := 0.2
	seed := domain.Claim{
		ID: "cl_seed", Text: "Cache eviction is LRU", Type: domain.ClaimTypeFact,
		Status: domain.ClaimStatusActive, Confidence: 0.9, TrustScore: stored,
		SourceAuthority: 0.8, CitationCount: 3, CreatedAt: now, ValidFrom: now, LastVerified: now,
	}
	hop := seed
	hop.ID = "cl_hop"
	hop.Text = "Cache eviction is FIFO"

	engine := hopAdmissionFixture(t, seed, hop, now)

	ans, err := engine.AnswerWithOptions(context.Background(), "cache eviction policy", AnswerOptions{Hops: 1})
	if err != nil {
		t.Fatalf("AnswerWithOptions: %v", err)
	}

	var seedTrust, hopTrust float64
	var hopRationale string
	for _, c := range ans.Claims {
		switch c.ID {
		case "cl_seed":
			seedTrust = c.TrustScore
		case "cl_hop":
			hopTrust = c.TrustScore
			hopRationale = c.ProvenanceRationale
		}
	}
	if hopTrust == 0 {
		t.Fatalf("hop claim missing from answer: %v", claimIDs(ans.Claims))
	}
	if hopTrust == stored {
		t.Fatalf("hop claim kept its raw stored trust score %.3f — it was never rescored", stored)
	}
	if hopTrust != seedTrust {
		t.Fatalf("hop trust %.6f != direct-hit trust %.6f for identical provenance — the two are ranked on different scales", hopTrust, seedTrust)
	}
	if hopRationale == "" {
		t.Errorf("hop claim has no provenance rationale — the rescore did not run over it")
	}
}
