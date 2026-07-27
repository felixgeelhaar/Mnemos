package grpc_test

import (
	"context"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/store"
	mnemosv1 "go.klarlabs.de/mnemos/proto/gen/mnemos/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// seedTwoRunsActions hangs one action off each of the two runs seedTwoRuns
// creates, plus a third with no run at all. They share a subject so the
// subject-filtered read path (which never consulted run_id) is exercised too.
func seedTwoRunsActions(t *testing.T, conn *store.Conn) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	for _, a := range []domain.Action{
		{ID: "act-mine", RunID: "run-mine", Kind: domain.ActionKindDeploy, Subject: "api", Actor: "a", At: now},
		{ID: "act-theirs", RunID: "run-theirs", Kind: domain.ActionKindDeploy, Subject: "api", Actor: "a", At: now},
		{ID: "act-unassigned", Kind: domain.ActionKindDeploy, Subject: "api", Actor: "a", At: now},
	} {
		if err := conn.Actions.Append(ctx, a); err != nil {
			t.Fatalf("seed action %s: %v", a.ID, err)
		}
	}
}

// TestGRPC_ListActionsEnforcesRunAllowlist is the ListActions half of the same
// hole PR #290 closed for ListBeliefs: run_id was an optional filter the caller
// chose, so a run-scoped bearer read every run's operational history by naming
// someone else's run, by filtering on a subject instead, or by asking for
// nothing at all. domain.Action carries its RunID, so nothing about this was
// hard to enforce — it simply was not.
func TestGRPC_ListActionsEnforcesRunAllowlist(t *testing.T) {
	client, conn, issuer := startAuthServer(t)
	seedTwoRuns(t, conn)
	seedTwoRunsActions(t, conn)

	// Wildcard scope so only the run boundary is under test.
	scoped := bearerCtx(t, mintToken(t, issuer, []string{domain.ScopeWildcard}, []string{"run-mine"}))
	unrestricted := bearerCtx(t, mintToken(t, issuer, []string{domain.ScopeWildcard}, nil))

	ids := func(resp *mnemosv1.ListActionsResponse) []string {
		out := make([]string, 0, len(resp.GetActions()))
		for _, a := range resp.GetActions() {
			out = append(out, a.GetId())
		}
		return out
	}
	wantOnly := func(t *testing.T, resp *mnemosv1.ListActionsResponse, want ...string) {
		t.Helper()
		got := ids(resp)
		if len(got) != len(want) {
			t.Fatalf("got actions %v, want exactly %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got actions %v, want exactly %v", got, want)
			}
		}
	}

	t.Run("no run id is narrowed, not unfiltered", func(t *testing.T) {
		resp, err := client.ListActions(scoped, &mnemosv1.ListActionsRequest{})
		if err != nil {
			t.Fatalf("ListActions: %v", err)
		}
		wantOnly(t, resp, "act-mine")
		if resp.GetTotal() != 1 {
			t.Errorf("total = %d, want 1: the count leaks the runs the rows no longer do", resp.GetTotal())
		}
	})

	t.Run("a subject filter does not bypass the whitelist", func(t *testing.T) {
		resp, err := client.ListActions(scoped, &mnemosv1.ListActionsRequest{Subject: "api"})
		if err != nil {
			t.Fatalf("ListActions: %v", err)
		}
		wantOnly(t, resp, "act-mine")
	})

	t.Run("another run is refused, not silently empty", func(t *testing.T) {
		_, err := client.ListActions(scoped, &mnemosv1.ListActionsRequest{RunId: "run-theirs"})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Errorf("got %v (%v), want PermissionDenied", got, err)
		}
	})

	t.Run("its own run still works", func(t *testing.T) {
		resp, err := client.ListActions(scoped, &mnemosv1.ListActionsRequest{RunId: "run-mine"})
		if err != nil {
			t.Fatalf("ListActions: %v", err)
		}
		wantOnly(t, resp, "act-mine")
	})

	// Control: the narrowing is the token's doing, not a filter that now hides
	// data from everyone.
	t.Run("an unrestricted token still sees every run", func(t *testing.T) {
		resp, err := client.ListActions(unrestricted, &mnemosv1.ListActionsRequest{})
		if err != nil {
			t.Fatalf("ListActions: %v", err)
		}
		if len(resp.GetActions()) != 3 {
			t.Errorf("got %v, want all three actions", ids(resp))
		}
	})
}

// TestGRPC_MetricsEnforcesRunAllowlist: Metrics aggregated the whole store for
// every caller. An aggregate over runs the token cannot read is still a read of
// them — episode and belief counts leak the size and health of another run's
// graph, and differencing successive calls leaks its activity. MetricsRequest
// has no run_id, so (as with ListEpisodes) the only fail-closed reading is to
// compute the aggregates over the whitelist alone.
func TestGRPC_MetricsEnforcesRunAllowlist(t *testing.T) {
	client, conn, issuer := startAuthServer(t)
	seedTwoRuns(t, conn)

	ctx := context.Background()
	// One embedding per side, so the embedding counter is covered too.
	for _, e := range []struct{ id, kind string }{
		{"ev-mine", "event"}, {"ev-theirs", "event"},
		{"cl-mine", "claim"}, {"cl-theirs", "claim"},
	} {
		if err := conn.Embeddings.Upsert(ctx, e.id, e.kind, []float32{1, 0}, "m", "tester"); err != nil {
			t.Fatalf("seed embedding %s: %v", e.id, err)
		}
	}
	// A contradiction wholly inside run-theirs: it must not show up in the
	// scoped token's dissonance count.
	now := time.Now().UTC()
	if err := conn.Claims.Upsert(ctx, []domain.Claim{
		{ID: "cl-theirs-2", Text: "theirs too", Type: domain.ClaimTypeFact, Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: now},
	}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if err := conn.Claims.UpsertEvidence(ctx, []domain.ClaimEvidence{
		{ClaimID: "cl-theirs-2", EventID: "ev-theirs"},
	}); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}
	if err := conn.Relationships.Upsert(ctx, []domain.Relationship{
		{ID: "rel-theirs", Type: domain.RelationshipTypeContradicts, FromClaimID: "cl-theirs", ToClaimID: "cl-theirs-2", CreatedAt: now},
	}); err != nil {
		t.Fatalf("seed relationship: %v", err)
	}

	scoped := bearerCtx(t, mintToken(t, issuer, []string{domain.ScopeWildcard}, []string{"run-mine"}))
	unrestricted := bearerCtx(t, mintToken(t, issuer, []string{domain.ScopeWildcard}, nil))

	got, err := client.Metrics(scoped, &mnemosv1.MetricsRequest{})
	if err != nil {
		t.Fatalf("Metrics (scoped): %v", err)
	}
	if got.GetRuns() != 1 {
		t.Errorf("runs = %d, want 1: the run count enumerates runs the token cannot read", got.GetRuns())
	}
	if got.GetEpisodes() != 1 {
		t.Errorf("episodes = %d, want 1 (only ev-mine)", got.GetEpisodes())
	}
	if got.GetBeliefs() != 2 {
		t.Errorf("beliefs = %d, want 2 (cl-mine, cl-mine-2)", got.GetBeliefs())
	}
	if got.GetEmbeddings() != 2 {
		t.Errorf("embeddings = %d, want 2 (ev-mine, cl-mine)", got.GetEmbeddings())
	}
	if got.GetDissonances() != 0 {
		t.Errorf("dissonances = %d, want 0: the only contradiction is inside run-theirs", got.GetDissonances())
	}
	if got.GetAssociations() != 0 {
		t.Errorf("associations = %d, want 0", got.GetAssociations())
	}

	// Control: an unrestricted token is unaffected.
	all, err := client.Metrics(unrestricted, &mnemosv1.MetricsRequest{})
	if err != nil {
		t.Fatalf("Metrics (unrestricted): %v", err)
	}
	if all.GetRuns() != 2 || all.GetEpisodes() != 2 || all.GetBeliefs() != 4 || all.GetEmbeddings() != 4 || all.GetDissonances() != 1 {
		t.Errorf("unrestricted metrics narrowed too: %+v", all)
	}
}
