package main

import (
	"context"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/extract"
)

// The prune's classification correctness is covered by internal/extract (it
// calls extract.IsJunk directly). This guards the command's flag contract: a
// bare `prune` must never guess at a destructive operation, and an unknown flag
// must be rejected rather than silently ignored.
func TestParsePruneArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{"bare prune is an error, not a default", nil, "", true},
		{"narration target", []string{"--narration"}, "narration", false},
		{"fan-out target", []string{"--fan-out"}, "fan-out", false},
		{"unknown flag rejected", []string{"--bogus"}, "", true},
		{"unknown flag rejected even with a valid one", []string{"--narration", "--bogus"}, "", true},
		{"two targets at once rejected", []string{"--narration", "--fan-out"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePruneArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("target = %q, want %q", got, tt.want)
			}
		})
	}
}

// selectNarrationClaims is the prune's only novel logic: filter to ACTIVE and
// classify via the shared extraction filter. Classification precision/recall
// is covered in internal/extract; this pins the selection contract.
func TestSelectNarrationClaims(t *testing.T) {
	claims := []domain.Claim{
		{ID: "real", Text: "We chose Postgres because write volume outgrew SQLite", Status: domain.ClaimStatusActive},
		{ID: "leadin", Text: "Now wiring the handler into core:", Status: domain.ClaimStatusActive},
		{ID: "narration", Text: "Let me check the config", Status: domain.ClaimStatusActive},
		// A junk claim that is already deprecated must NOT be re-selected.
		{ID: "already-dep", Text: "Getting the detail:", Status: domain.ClaimStatusDeprecated},
		// A junk claim that is CONTESTED is now included: it was auto-contested
		// at ingest against other junk, not human-flagged, so it is still noise.
		{ID: "contested-junk", Text: "Verifying the impact:", Status: domain.ClaimStatusContested},
		// A REAL contested claim (genuine dispute) must be left alone.
		{ID: "contested-real", Text: "The write path must never bypass the governed kernel", Status: domain.ClaimStatusContested},
	}
	got := selectNarrationClaims(claims)

	gotIDs := map[string]bool{}
	for _, c := range got {
		gotIDs[c.ID] = true
	}
	if !gotIDs["leadin"] || !gotIDs["narration"] {
		t.Errorf("missed active junk claims: got %v", gotIDs)
	}
	if gotIDs["real"] {
		t.Error("selected a real knowledge claim for deprecation")
	}
	if gotIDs["already-dep"] {
		t.Error("re-selected an already-deprecated claim, churning its history")
	}
	if !gotIDs["contested-junk"] {
		t.Error("did not select a contested JUNK claim (it is noise, not under review)")
	}
	if gotIDs["contested-real"] {
		t.Error("selected a contested REAL claim — genuine disputes must be left alone")
	}
}

// TestParsePruneArgs_SessionNoise covers the second target and the rule that
// prune never guesses: no target is an error, and two targets is an error
// rather than a silent pick.
func TestParsePruneArgs_SessionNoise(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{"session-noise", []string{"--session-noise"}, "session-noise", false},
		{"narration still works", []string{"--narration"}, "narration", false},
		{"both is an error", []string{"--narration", "--session-noise"}, "", true},
		{"neither is an error", nil, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePruneArgs(tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

// TestLiveContradictions_MatchesTheVital pins that the command counts the same
// population `mnemos health` does, so its numbers can be compared directly:
// contradiction edges only, both endpoints present and not deprecated.
func TestLiveContradictions_MatchesTheVital(t *testing.T) {
	byID := map[string]domain.Claim{
		"a": {ID: "a"},
		"b": {ID: "b"},
		"d": {ID: "d", Status: domain.ClaimStatusDeprecated},
	}
	rels := []domain.Relationship{
		{ID: "live", Type: domain.RelationshipTypeContradicts, FromClaimID: "a", ToClaimID: "b"},
		{ID: "supports", Type: domain.RelationshipTypeSupports, FromClaimID: "a", ToClaimID: "b"},
		{ID: "deprecated", Type: domain.RelationshipTypeContradicts, FromClaimID: "a", ToClaimID: "d"},
		{ID: "dangling", Type: domain.RelationshipTypeContradicts, FromClaimID: "a", ToClaimID: "gone"},
	}
	got := liveContradictions(rels, byID)
	if len(got) != 1 || got[0].ID != "live" {
		t.Fatalf("want only the live contradiction, got %+v", got)
	}
	if ids := contradictionEndpoints(got, byID); len(ids) != 2 {
		t.Fatalf("want 2 distinct endpoints, got %v", ids)
	}
}

// TestCountDurability_RateIsOverClassified pins the denominator. A pass that
// runs out of budget leaves most claims Unknown; measuring the session-local
// rate over ALL of them counts claims never looked at as though they had been
// judged durable. On a real pass that understated 60.7% as 27.9%.
func TestCountDurability_RateIsOverClassified(t *testing.T) {
	// 3 session-local, 2 durable, 5 never reached.
	verdicts := []extract.Durability{
		extract.DurabilitySessionLocal, extract.DurabilitySessionLocal, extract.DurabilitySessionLocal,
		extract.DurabilityDurable, extract.DurabilityDurable,
		extract.DurabilityUnknown, extract.DurabilityUnknown, extract.DurabilityUnknown,
		extract.DurabilityUnknown, extract.DurabilityUnknown,
	}
	sessionLocal := countDurability(verdicts, extract.DurabilitySessionLocal)
	classified := len(verdicts) - countDurability(verdicts, extract.DurabilityUnknown)
	if sessionLocal != 3 || classified != 5 {
		t.Fatalf("sessionLocal=%d classified=%d", sessionLocal, classified)
	}
	if got := pct(sessionLocal, classified); got != 60 {
		t.Fatalf("rate must be over classified claims: got %.1f%%, want 60%%", got)
	}
	if over := pct(sessionLocal, len(verdicts)); over != 30 {
		t.Fatalf("sanity: dividing by everything gives the misleading %.1f%%", over)
	}
}

// TestWithWriteReserve pins that classification cannot consume the whole job
// budget. It once did: a pass that classified 2,226 claims and identified 1,084
// prunable edges then failed on "prune relationships", because the write
// inherited the same exhausted context and every verdict was discarded.
func TestWithWriteReserve(t *testing.T) {
	t.Run("leaves room to write", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		got, done := withWriteReserve(ctx)
		defer done()

		outer, _ := ctx.Deadline()
		inner, ok := got.Deadline()
		if !ok {
			t.Fatal("classification must keep a deadline")
		}
		if !inner.Before(outer) {
			t.Fatal("classification must expire before the job does")
		}
		if gap := outer.Sub(inner); gap != pruneWriteReserve {
			t.Fatalf("reserve = %v, want %v", gap, pruneWriteReserve)
		}
	})

	t.Run("a budget too short to split is left alone", func(t *testing.T) {
		// Halving an already-tight budget would classify nothing at all.
		ctx, cancel := context.WithTimeout(context.Background(), pruneWriteReserve)
		defer cancel()
		got, done := withWriteReserve(ctx)
		defer done()
		inner, _ := got.Deadline()
		outer, _ := ctx.Deadline()
		if !inner.Equal(outer) {
			t.Fatal("a short budget must be passed through unchanged")
		}
	})

	t.Run("no deadline is passed through", func(t *testing.T) {
		got, done := withWriteReserve(context.Background())
		defer done()
		if _, ok := got.Deadline(); ok {
			t.Fatal("a job without a deadline must not gain one")
		}
	})
}
