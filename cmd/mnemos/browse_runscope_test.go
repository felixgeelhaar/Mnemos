package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/bolt"
	"go.klarlabs.de/mnemos/internal/auth"
	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/store"
)

// The browse tools (list_beliefs / list_decisions / list_dissonances) reached
// Claims.ListAll without ever calling enforceRunScope, so a token minted for a
// single run could page through the entire knowledge base. These tests pin the
// guard on both entry points and on the run filter behind it.

// seedRunClaim writes an event in runID plus a claim backed by it, so the claim
// is discoverable through the events → evidence → claims hop the run filter uses.
func seedRunClaim(t *testing.T, conn *store.Conn, runID, eventID, claimID, text, ctype string, at time.Time) {
	t.Helper()
	seedEventConn(t, conn, eventID, runID, text, "src-"+eventID, "", at)
	seedClaimConn(t, conn, claimID, text, ctype, "active", 0.9, at)
	link := domain.ClaimEvidence{ClaimID: claimID, EventID: eventID}
	if err := conn.Claims.UpsertEvidence(context.Background(), []domain.ClaimEvidence{link}); err != nil {
		t.Fatalf("upsert evidence: %v", err)
	}
}

func runScopedCtx(runs ...string) context.Context {
	return withClaims(context.Background(), &auth.Claims{Runs: runs})
}

func TestMCPRunListClaims_RunRestrictedTokenCannotBrowseEverything(t *testing.T) {
	ctx := runScopedCtx("alpha")

	// Unscoped listing from a run-restricted token spans every run — deny it.
	if _, err := mcpRunListClaims(ctx, mcpListClaimsInput{}); err == nil {
		t.Fatal("unscoped list_beliefs from a run-restricted token must be denied")
	}
	// A run outside the allowlist is denied too.
	if _, err := mcpRunListClaims(ctx, mcpListClaimsInput{RunID: "beta"}); err == nil {
		t.Fatal("list_beliefs for a run outside the allowlist must be denied")
	}
	// Whitespace must not launder an empty run past the guard.
	if _, err := mcpRunListClaims(ctx, mcpListClaimsInput{RunID: "   "}); err == nil {
		t.Fatal("a blank run id must not bypass the guard")
	}
}

func TestMCPRunListContradictions_RunRestrictedTokenCannotBrowseEverything(t *testing.T) {
	ctx := runScopedCtx("alpha")

	if _, err := mcpRunListContradictions(ctx, mcpListContradictionsInput{}); err == nil {
		t.Fatal("unscoped list_dissonances from a run-restricted token must be denied")
	}
	if _, err := mcpRunListContradictions(ctx, mcpListContradictionsInput{RunID: "beta"}); err == nil {
		t.Fatal("list_dissonances for a run outside the allowlist must be denied")
	}
}

// list_decisions is list_beliefs with type=decision pre-set, so it inherits the
// guard — but only because the guard lives in the shared handler. Pin that.
func TestMCPRunListClaims_DecisionShorthandStillGuarded(t *testing.T) {
	ctx := runScopedCtx("alpha")
	in := mcpListClaimsInput{Type: string(domain.ClaimTypeDecision)}
	if _, err := mcpRunListClaims(ctx, in); err == nil {
		t.Fatal("list_decisions must be run-guarded like list_beliefs")
	}
}

// An unauthenticated stdio caller, and an authenticated token with no run
// allowlist, must keep working unscoped.
func TestMCPRunListClaims_UnrestrictedCallersUnchanged(t *testing.T) {
	dsn := "sqlite://" + filepath.Join(t.TempDir(), "brain.db")
	t.Setenv("MNEMOS_DB_URL", dsn)
	conn, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedRunClaim(t, conn, "alpha", "ev-a", "cl-a", "the api runs in eu-west-1", "fact", time.Now())
	_ = conn.Close()

	for name, ctx := range map[string]context.Context{
		"stdio (no claims)":  context.Background(),
		"token, no run list": withClaims(context.Background(), &auth.Claims{}),
	} {
		out, err := mcpRunListClaims(ctx, mcpListClaimsInput{})
		if err != nil {
			t.Fatalf("%s: unscoped list must still work: %v", name, err)
		}
		if out.Total != 1 {
			t.Errorf("%s: total = %d, want 1", name, out.Total)
		}
	}
}

// The allowed run resolves through the whole handler, and returns only that
// run's claims — the guard must gate access, not break it.
func TestMCPRunListClaims_AllowedRunReturnsOnlyThatRun(t *testing.T) {
	dsn := "sqlite://" + filepath.Join(t.TempDir(), "brain.db")
	t.Setenv("MNEMOS_DB_URL", dsn)
	conn, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Now()
	seedRunClaim(t, conn, "alpha", "ev-a", "cl-a", "alpha knowledge", "fact", now)
	seedRunClaim(t, conn, "beta", "ev-b", "cl-b", "beta secret", "fact", now)
	_ = conn.Close()

	out, err := mcpRunListClaims(runScopedCtx("alpha"), mcpListClaimsInput{RunID: "alpha"})
	if err != nil {
		t.Fatalf("allowed run must be served: %v", err)
	}
	if out.Total != 1 || len(out.Claims) != 1 || out.Claims[0].ID != "cl-a" {
		t.Fatalf("run-scoped list leaked or lost rows: %+v", out.Claims)
	}
}

// The browse tools normally dispatch through the axi kernel, so the guard is
// only real if the request context (and therefore the token's claims) survives
// that hop. Pin it for all three tools.
func TestBrowseTools_RunScopeSurvivesTheKernelDispatch(t *testing.T) {
	t.Setenv("MNEMOS_DB_URL", "sqlite://"+filepath.Join(t.TempDir(), "mnemos.db"))
	logger := bolt.New(bolt.NewJSONHandler(os.Stderr))
	k, err := buildMCPKernel(logger, mcpExecutorMap("", func() (*Watcher, error) { return nil, nil }))
	if err != nil {
		t.Fatalf("kernel: %v", err)
	}
	ctx := runScopedCtx("alpha")

	if _, err := dispatchAxiTool[mcpListClaimsOutput](ctx, k, nil, "list_beliefs", mcpListClaimsInput{}); err == nil {
		t.Error("list_beliefs through the kernel must stay run-guarded")
	}
	if _, err := dispatchAxiTool[mcpListClaimsOutput](ctx, k, nil, "list_decisions", mcpListClaimsInput{}); err == nil {
		t.Error("list_decisions through the kernel must stay run-guarded")
	}
	if _, err := dispatchAxiTool[mcpListContradictionsOutput](ctx, k, nil, "list_dissonances", mcpListContradictionsInput{}); err == nil {
		t.Error("list_dissonances through the kernel must stay run-guarded")
	}
}

func TestListClaimsFiltered_RunFilterExcludesOtherRuns(t *testing.T) {
	_, conn := openTestStore(t)
	now := time.Now()
	seedRunClaim(t, conn, "alpha", "ev-a1", "cl-a1", "alpha older", "fact", now.Add(-time.Hour))
	seedRunClaim(t, conn, "alpha", "ev-a2", "cl-a2", "alpha newer", "decision", now)
	seedRunClaim(t, conn, "beta", "ev-b1", "cl-b1", "beta only", "fact", now)
	// A claim with no evidence at all must not appear under any run.
	seedClaimConn(t, conn, "cl-orphan", "orphan", "fact", "active", 0.5, now)

	claims, total, err := listClaimsFiltered(context.Background(), conn, "", "", "alpha", 50, 0)
	if err != nil {
		t.Fatalf("listClaimsFiltered: %v", err)
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2 (alpha only): %+v", total, claims)
	}
	if claims[0].ID != "cl-a2" {
		t.Errorf("expected newest-first ordering under the run filter, got %+v", claims)
	}
	for _, c := range claims {
		if c.ID == "cl-b1" || c.ID == "cl-orphan" {
			t.Errorf("run filter leaked %s", c.ID)
		}
	}

	// The run filter composes with the type filter rather than replacing it.
	decisions, total, err := listClaimsFiltered(context.Background(), conn, "decision", "", "alpha", 50, 0)
	if err != nil {
		t.Fatalf("listClaimsFiltered (typed): %v", err)
	}
	if total != 1 || decisions[0].ID != "cl-a2" {
		t.Fatalf("type+run filter = %+v, want only cl-a2", decisions)
	}

	// An unknown run yields nothing rather than everything.
	none, total, err := listClaimsFiltered(context.Background(), conn, "", "", "does-not-exist", 50, 0)
	if err != nil {
		t.Fatalf("listClaimsFiltered (unknown run): %v", err)
	}
	if total != 0 || len(none) != 0 {
		t.Fatalf("unknown run must return nothing, got %+v", none)
	}
}

func TestListContradictionPairs_RunFilterRequiresBothEndpoints(t *testing.T) {
	_, conn := openTestStore(t)
	now := time.Now()
	seedRunClaim(t, conn, "alpha", "ev-a1", "cl-a1", "Use SQLite", "decision", now)
	seedRunClaim(t, conn, "alpha", "ev-a2", "cl-a2", "Use PostgreSQL", "decision", now)
	seedRunClaim(t, conn, "beta", "ev-b1", "cl-b1", "Use MySQL", "decision", now)
	seedRelationshipConn(t, conn, "r-in", "contradicts", "cl-a1", "cl-a2", now)
	seedRelationshipConn(t, conn, "r-cross", "contradicts", "cl-a1", "cl-b1", now)

	pairs, total, err := listContradictionPairs(context.Background(), conn, "alpha", 50, 0)
	if err != nil {
		t.Fatalf("listContradictionPairs: %v", err)
	}
	if total != 1 || len(pairs) != 1 || pairs[0].RelationshipID != "r-in" {
		t.Fatalf("want only the wholly-in-run edge, got %+v", pairs)
	}
	// The cross-run edge would have hydrated cl-b1's text into the response.
	for _, p := range pairs {
		if strings.Contains(p.FromClaimText, "MySQL") || strings.Contains(p.ToClaimText, "MySQL") {
			t.Errorf("cross-run claim text leaked: %+v", p)
		}
	}

	// Unscoped listing still sees both edges.
	all, total, err := listContradictionPairs(context.Background(), conn, "", 50, 0)
	if err != nil {
		t.Fatalf("listContradictionPairs (unscoped): %v", err)
	}
	if total != 2 || len(all) != 2 {
		t.Fatalf("unscoped listing = %d edges, want 2", total)
	}
}
