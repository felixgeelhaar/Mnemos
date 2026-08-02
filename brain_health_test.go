package mnemos

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

func vitalByName(h BrainHealth, name string) Vital {
	for _, v := range h.Vitals {
		if v.Name == name {
			return v
		}
	}
	return Vital{Name: "MISSING"}
}

func pathologyByKind(h BrainHealth, kind string) Pathology {
	for _, p := range h.Pathologies {
		if p.Kind == kind {
			return p
		}
	}
	return Pathology{Kind: "MISSING"}
}

// TestBrainHealth_EmptyIsHealthy verifies the ADR-0019 verdict: an empty brain is
// healthy with all vitals + integrity checks present and clean.
func TestBrainHealth_EmptyIsHealthy(t *testing.T) {
	m := calibMem(t)
	h, err := m.BrainHealth(context.Background())
	if err != nil {
		t.Fatalf("BrainHealth: %v", err)
	}
	if h.Status != HealthOK {
		t.Errorf("empty brain status = %q, want healthy", h.Status)
	}
	// Seven: the five claim-derived vitals of the ADR-0019 v1 set, plus
	// skill_coverage (is the action -> outcome -> lesson layer being built at
	// all?) and trust_decay (is trust falling faster than it is replaced?). The
	// count is pinned deliberately — a vital silently disappearing is the
	// failure this assertion exists to catch.
	if len(h.Vitals) != 7 {
		t.Errorf("want 7 vitals, got %d", len(h.Vitals))
	}
	if len(h.Pathologies) != 3 {
		t.Errorf("want 3 pathologies, got %d", len(h.Pathologies))
	}
	if fe := vitalByName(h, "free_energy"); fe.Status != HealthOK {
		t.Errorf("free_energy vital on empty brain = %+v, want ok", fe)
	}
}

// TestBrainHealth_DetectsPathologies verifies the new integrity checks: an orphan belief
// (no evidence) and a dangling edge (endpoint missing) are found, and a dangling edge
// drives the overall verdict to unhealthy (referential corruption).
func TestBrainHealth_DetectsPathologies(t *testing.T) {
	m := calibMem(t)
	ctx := context.Background()
	seedClaim(t, m, "a", 0.8) // seedClaim adds no evidence → orphan
	if err := m.conn.Relationships.Upsert(ctx, []domain.Relationship{
		{ID: "r1", Type: domain.RelationshipTypeSupports, FromClaimID: "a", ToClaimID: "ghost", CreatedAt: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("seed dangling edge: %v", err)
	}
	h, err := m.BrainHealth(ctx)
	if err != nil {
		t.Fatalf("BrainHealth: %v", err)
	}
	if orphan := pathologyByKind(h, "orphan_claims"); orphan.Count < 1 || orphan.Status == HealthOK {
		t.Errorf("orphan_claims = %+v, want count>=1 and non-ok", orphan)
	}
	dangling := pathologyByKind(h, "dangling_edges")
	if dangling.Count < 1 || dangling.Status != HealthUnhealthy {
		t.Errorf("dangling_edges = %+v, want count>=1 and unhealthy", dangling)
	}
	if h.Status != HealthUnhealthy {
		t.Errorf("a dangling edge should make the brain unhealthy overall, got %q", h.Status)
	}
}

// TestSnapshotHealth_RecordsToJournal verifies ADR-0019 health-over-time: SnapshotHealth
// appends a `health` entry that round-trips into a BrainHealth verdict.
func TestSnapshotHealth_RecordsToJournal(t *testing.T) {
	m := calibMem(t)
	ctx := context.Background()
	got, err := m.SnapshotHealth(ctx)
	if err != nil {
		t.Fatalf("SnapshotHealth: %v", err)
	}
	entries, err := m.conn.Journal.List(ctx, domain.JournalKindHealth, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("health entries = %d err=%v, want 1", len(entries), err)
	}
	var recorded BrainHealth
	if err := json.Unmarshal([]byte(entries[0].Data), &recorded); err != nil {
		t.Fatalf("unmarshal health snapshot: %v", err)
	}
	// Compare against what BrainHealth actually returned rather than a literal
	// count: this test is about the JOURNAL faithfully recording the snapshot,
	// and pinning a number here made adding a vital fail in a place that has
	// nothing to do with the change.
	if recorded.Status != got.Status || len(recorded.Vitals) != len(got.Vitals) {
		t.Errorf("recorded snapshot = %+v, want status %q with %d vitals", recorded, got.Status, len(got.Vitals))
	}
}

// TestBrainHealth_DeprecatedClaimsAreNotCurrentlyValid pins that a deprecated
// belief is excluded from the vitals — validCount, low-trust, staleness, and
// especially the orphan check. Deprecating closes STATUS, not valid-time, so
// the loop (which keyed only on valid-time) counted a retired belief as
// currently-valid: an ungrounded deprecated claim kept raising an orphan
// warning that deprecating it was meant to clear. Same class of bug as the
// dissonance vital counting contradictions into deprecated beliefs.
func TestBrainHealth_DeprecatedClaimsAreNotCurrentlyValid(t *testing.T) {
	m := calibMem(t)
	ctx := context.Background()

	// An ACTIVE ungrounded claim is an orphan (no evidence).
	seedClaim(t, m, "orphan-active", 0.8)
	if h, err := m.BrainHealth(ctx); err != nil {
		t.Fatal(err)
	} else if o := pathologyByKind(h, "orphan_claims"); o.Count != 1 {
		t.Fatalf("precondition: active ungrounded claim must be an orphan, got %d", o.Count)
	}

	// Deprecate it (status only; valid-time stays open, exactly like the real
	// deprecation tools).
	now := time.Now().UTC()
	if err := m.conn.Claims.Upsert(ctx, []domain.Claim{{
		ID: "orphan-active", Text: "claim orphan-active", Type: domain.ClaimTypeFact,
		Confidence: 0.8, Status: domain.ClaimStatusDeprecated, CreatedAt: now, ValidFrom: now,
	}}); err != nil {
		t.Fatalf("deprecate: %v", err)
	}

	h, err := m.BrainHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if o := pathologyByKind(h, "orphan_claims"); o.Count != 0 {
		t.Fatalf("a deprecated belief must not count as a currently-valid orphan, got %d", o.Count)
	}
}
