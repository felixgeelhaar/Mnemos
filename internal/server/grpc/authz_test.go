package grpc_test

import (
	"context"
	"net"
	"testing"
	"time"

	mnemos "go.klarlabs.de/mnemos"
	"go.klarlabs.de/mnemos/internal/auth"
	"go.klarlabs.de/mnemos/internal/domain"
	mnemosgrpc "go.klarlabs.de/mnemos/internal/server/grpc"
	"go.klarlabs.de/mnemos/internal/store"
	_ "go.klarlabs.de/mnemos/internal/store/memory"
	mnemosv1 "go.klarlabs.de/mnemos/proto/gen/mnemos/v1"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// startAuthServer stands up the gRPC surface with a real verifier and a real
// Memory facade, and returns the client, the underlying conn (for seeding
// fixtures the RPCs then have to respect) and the issuer that mints tokens.
func startAuthServer(t *testing.T) (mnemosv1.MnemosServiceClient, *store.Conn, *auth.Issuer) {
	t.Helper()
	ctx := context.Background()

	conn, err := store.Open(ctx, "memory://")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	mem, err := mnemos.New(mnemos.WithStorage("memory://"))
	if err != nil {
		t.Fatalf("build facade: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })

	secret, err := auth.GenerateSecret()
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	issuer := auth.NewIssuer(secret)
	verifier := auth.NewVerifier(secret, conn.RevokedTokens)

	impl := mnemosgrpc.NewServerWithMemory(conn, mem, verifier, testLogger(), "test")
	t.Cleanup(func() { _ = impl.Close() })
	srv := grpclib.NewServer(grpclib.UnaryInterceptor(impl.UnaryInterceptor()))
	impl.Register(srv)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.GracefulStop)

	cc, err := grpclib.NewClient(lis.Addr().String(), grpclib.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	return mnemosv1.NewMnemosServiceClient(cc), conn, issuer
}

func bearerCtx(t *testing.T, token string) context.Context {
	t.Helper()
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
}

// mintToken issues an agent token with exactly the supplied scopes and runs.
// Note IssueUserToken* substitutes "*" for an empty scope list — the agent
// issuer does not, which is what lets this test express "authenticated but
// authorised for nothing", the shape of the token that used to be able to
// write.
func mintToken(t *testing.T, issuer *auth.Issuer, scopes, runs []string) string {
	t.Helper()
	tok, _, err := issuer.IssueAgentTokenFull("agent-1", scopes, runs, "", time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return tok
}

// writeRPC is one mutating RPC plus the scope it must demand.
type writeRPC struct {
	name  string
	scope string
	call  func(ctx context.Context, c mnemosv1.MnemosServiceClient) error
}

// writeRPCs is every mutating RPC on the service. Nine of them shipped with no
// scope check at all: a token that authenticated with `claims:read`, or with an
// empty scope list, could record actions, outcomes, schemas, decisions,
// reflexes and entity edges, flip a belief's lifecycle, rewrite working memory
// and trigger a synthesis pass. The four that were already guarded are listed
// alongside so the table is the whole write surface, not a patch note.
func writeRPCs() []writeRPC {
	return []writeRPC{
		{"AppendEpisodes", domain.ScopeEventsWrite, func(ctx context.Context, c mnemosv1.MnemosServiceClient) error {
			_, err := c.AppendEpisodes(ctx, &mnemosv1.AppendEpisodesRequest{
				Episodes: []*mnemosv1.Episode{{Id: "ev-scope", Content: "c", SourceInputId: "in"}},
			})
			return err
		}},
		{"AppendBeliefs", domain.ScopeClaimsWrite, func(ctx context.Context, c mnemosv1.MnemosServiceClient) error {
			_, err := c.AppendBeliefs(ctx, &mnemosv1.AppendBeliefsRequest{
				Beliefs: []*mnemosv1.Belief{{Id: "cl-scope", Text: "t", Type: "fact", Confidence: 0.5, Status: "active"}},
			})
			return err
		}},
		{"AppendAssociations", domain.ScopeRelationshipsWrite, func(ctx context.Context, c mnemosv1.MnemosServiceClient) error {
			_, err := c.AppendAssociations(ctx, &mnemosv1.AppendAssociationsRequest{
				Associations: []*mnemosv1.Association{{Id: "rel-scope", Type: "supports", FromBeliefId: "cl-scope", ToBeliefId: "cl-scope"}},
			})
			return err
		}},
		{"AppendEmbeddings", domain.ScopeEmbeddingsWrite, func(ctx context.Context, c mnemosv1.MnemosServiceClient) error {
			_, err := c.AppendEmbeddings(ctx, &mnemosv1.AppendEmbeddingsRequest{
				Embeddings: []*mnemosv1.Embedding{{EntityId: "ev-scope", EntityType: "event", Vector: []float32{1}, Model: "m"}},
			})
			return err
		}},
		{"AppendActions", domain.ScopeClaimsWrite, func(ctx context.Context, c mnemosv1.MnemosServiceClient) error {
			_, err := c.AppendActions(ctx, &mnemosv1.AppendActionsRequest{
				Actions: []*mnemosv1.Action{{Id: "act-scope", Kind: "deploy", Subject: "svc"}},
			})
			return err
		}},
		{"AppendOutcomes", domain.ScopeClaimsWrite, func(ctx context.Context, c mnemosv1.MnemosServiceClient) error {
			_, err := c.AppendOutcomes(ctx, &mnemosv1.AppendOutcomesRequest{
				Outcomes: []*mnemosv1.Outcome{{Id: "out-scope", ActionId: "act-scope", Result: "success"}},
			})
			return err
		}},
		{"AppendSchemas", domain.ScopeClaimsWrite, func(ctx context.Context, c mnemosv1.MnemosServiceClient) error {
			_, err := c.AppendSchemas(ctx, &mnemosv1.AppendSchemasRequest{
				Schemas: []*mnemosv1.Schema{{Id: "sch-scope", Statement: "s", Trigger: "t", Confidence: 0.5}},
			})
			return err
		}},
		{"AppendDecisions", domain.ScopeClaimsWrite, func(ctx context.Context, c mnemosv1.MnemosServiceClient) error {
			_, err := c.AppendDecisions(ctx, &mnemosv1.AppendDecisionsRequest{
				Decisions: []*mnemosv1.Decision{{Id: "dec-scope", Statement: "s", RiskLevel: "low"}},
			})
			return err
		}},
		{"AppendReflexes", domain.ScopeClaimsWrite, func(ctx context.Context, c mnemosv1.MnemosServiceClient) error {
			_, err := c.AppendReflexes(ctx, &mnemosv1.AppendReflexesRequest{
				Reflexes: []*mnemosv1.Reflex{{Id: "ref-scope", Trigger: "t", Statement: "s", Confidence: 0.5}},
			})
			return err
		}},
		{"AppendEntityAssociations", domain.ScopeRelationshipsWrite, func(ctx context.Context, c mnemosv1.MnemosServiceClient) error {
			_, err := c.AppendEntityAssociations(ctx, &mnemosv1.AppendEntityAssociationsRequest{
				Edges: []*mnemosv1.EntityAssociation{{Id: "ea-scope", Kind: "causes", FromId: "act-scope", FromType: "action", ToId: "out-scope", ToType: "outcome"}},
			})
			return err
		}},
		{"SetBeliefLifecycle", domain.ScopeClaimsWrite, func(ctx context.Context, c mnemosv1.MnemosServiceClient) error {
			_, err := c.SetBeliefLifecycle(ctx, &mnemosv1.SetBeliefLifecycleRequest{BeliefId: "cl-scope", Lifecycle: "promoted"})
			return err
		}},
		{"SetBlock", domain.ScopeClaimsWrite, func(ctx context.Context, c mnemosv1.MnemosServiceClient) error {
			_, err := c.SetBlock(ctx, &mnemosv1.SetBlockRequest{Owner: "grace", Label: "focus", Value: "v"})
			return err
		}},
		{"Synthesize", domain.ScopeClaimsWrite, func(ctx context.Context, c mnemosv1.MnemosServiceClient) error {
			_, err := c.Synthesize(ctx, &mnemosv1.SynthesizeRequest{})
			return err
		}},
	}
}

// TestGRPC_WriteRPCsRefuseScopelessToken is the guardrail: authentication is
// not authorisation. A token that parses, is unrevoked and unexpired but
// carries no scopes must not be able to mutate anything.
func TestGRPC_WriteRPCsRefuseScopelessToken(t *testing.T) {
	client, _, issuer := startAuthServer(t)
	ctx := bearerCtx(t, mintToken(t, issuer, nil, nil))

	for _, rpc := range writeRPCs() {
		t.Run(rpc.name, func(t *testing.T) {
			err := rpc.call(ctx, client)
			if got := status.Code(err); got != codes.PermissionDenied {
				t.Errorf("%s with a scope-less token: got %v (%v), want PermissionDenied", rpc.name, got, err)
			}
		})
	}
}

// TestGRPC_WriteRPCsRefuseWrongScope pins the *specific* scope each RPC
// demands. Without it, gating everything on one broad scope would pass the
// scope-less test above while still handing a claims:write token authority
// over embeddings and edges.
func TestGRPC_WriteRPCsRefuseWrongScope(t *testing.T) {
	client, _, issuer := startAuthServer(t)

	// A scope from the same vocabulary that no RPC here should accept.
	const unrelated = "claims:read"

	for _, rpc := range writeRPCs() {
		t.Run(rpc.name, func(t *testing.T) {
			wrong := bearerCtx(t, mintToken(t, issuer, []string{unrelated}, nil))
			if got := status.Code(rpc.call(wrong, client)); got != codes.PermissionDenied {
				t.Errorf("%s with %q: got %v, want PermissionDenied", rpc.name, unrelated, got)
			}

			right := bearerCtx(t, mintToken(t, issuer, []string{rpc.scope}, nil))
			if got := status.Code(rpc.call(right, client)); got == codes.PermissionDenied {
				t.Errorf("%s with its own scope %q was refused; the guard demands the wrong scope", rpc.name, rpc.scope)
			}
		})
	}
}

// seedTwoRuns writes two episodes in two runs, each with a belief hanging off
// it, so the run boundary has something to be crossed.
func seedTwoRuns(t *testing.T, conn *store.Conn) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	for _, ev := range []domain.Event{
		{ID: "ev-mine", Content: "mine", SourceInputID: "in", RunID: "run-mine", Timestamp: now, IngestedAt: now},
		{ID: "ev-theirs", Content: "theirs", SourceInputID: "in", RunID: "run-theirs", Timestamp: now, IngestedAt: now},
	} {
		if err := conn.Events.Append(ctx, ev); err != nil {
			t.Fatalf("seed event %s: %v", ev.ID, err)
		}
	}
	if err := conn.Claims.Upsert(ctx, []domain.Claim{
		{ID: "cl-mine", Text: "mine", Type: domain.ClaimTypeFact, Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: now},
		{ID: "cl-mine-2", Text: "mine too", Type: domain.ClaimTypeFact, Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: now},
		{ID: "cl-theirs", Text: "theirs", Type: domain.ClaimTypeFact, Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: now},
	}); err != nil {
		t.Fatalf("seed claims: %v", err)
	}
	if err := conn.Claims.UpsertEvidence(ctx, []domain.ClaimEvidence{
		{ClaimID: "cl-mine", EventID: "ev-mine"},
		{ClaimID: "cl-mine-2", EventID: "ev-mine"},
		{ClaimID: "cl-theirs", EventID: "ev-theirs"},
	}); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}
}

// TestGRPC_AppendRPCsEnforceRunAllowlist: a run-scoped bearer may write inside
// its own run and nowhere else. AppendBeliefs already refused cross-run
// evidence; these three wrote whatever they were handed, so the whitelist could
// be walked around by appending an episode into someone else's run, or by
// pointing an association or an embedding at their data.
func TestGRPC_AppendRPCsEnforceRunAllowlist(t *testing.T) {
	client, conn, issuer := startAuthServer(t)
	seedTwoRuns(t, conn)

	// Wildcard scope so only the run boundary is under test.
	scoped := bearerCtx(t, mintToken(t, issuer, []string{domain.ScopeWildcard}, []string{"run-mine"}))

	tests := []struct {
		name string
		call func(ctx context.Context) error
		want codes.Code
	}{
		{"AppendEpisodes into own run", func(ctx context.Context) error {
			_, err := client.AppendEpisodes(ctx, &mnemosv1.AppendEpisodesRequest{
				Episodes: []*mnemosv1.Episode{{Id: "ev-new-mine", RunId: "run-mine", Content: "c", SourceInputId: "in"}},
			})
			return err
		}, codes.OK},
		{"AppendEpisodes into another run", func(ctx context.Context) error {
			_, err := client.AppendEpisodes(ctx, &mnemosv1.AppendEpisodesRequest{
				Episodes: []*mnemosv1.Episode{{Id: "ev-new-theirs", RunId: "run-theirs", Content: "c", SourceInputId: "in"}},
			})
			return err
		}, codes.PermissionDenied},
		{"AppendEpisodes with no run at all", func(ctx context.Context) error {
			_, err := client.AppendEpisodes(ctx, &mnemosv1.AppendEpisodesRequest{
				Episodes: []*mnemosv1.Episode{{Id: "ev-new-unassigned", Content: "c", SourceInputId: "in"}},
			})
			return err
		}, codes.PermissionDenied},
		{"AppendAssociations within own run", func(ctx context.Context) error {
			_, err := client.AppendAssociations(ctx, &mnemosv1.AppendAssociationsRequest{
				Associations: []*mnemosv1.Association{{Id: "rel-mine", Type: "supports", FromBeliefId: "cl-mine", ToBeliefId: "cl-mine-2"}},
			})
			return err
		}, codes.OK},
		{"AppendAssociations touching another run", func(ctx context.Context) error {
			_, err := client.AppendAssociations(ctx, &mnemosv1.AppendAssociationsRequest{
				Associations: []*mnemosv1.Association{{Id: "rel-cross", Type: "supports", FromBeliefId: "cl-mine", ToBeliefId: "cl-theirs"}},
			})
			return err
		}, codes.PermissionDenied},
		{"AppendEmbeddings for own episode", func(ctx context.Context) error {
			_, err := client.AppendEmbeddings(ctx, &mnemosv1.AppendEmbeddingsRequest{
				Embeddings: []*mnemosv1.Embedding{{EntityId: "ev-mine", EntityType: "event", Vector: []float32{1}, Model: "m"}},
			})
			return err
		}, codes.OK},
		{"AppendEmbeddings for another run's episode", func(ctx context.Context) error {
			_, err := client.AppendEmbeddings(ctx, &mnemosv1.AppendEmbeddingsRequest{
				Embeddings: []*mnemosv1.Embedding{{EntityId: "ev-theirs", EntityType: "event", Vector: []float32{1}, Model: "m"}},
			})
			return err
		}, codes.PermissionDenied},
		{"AppendEmbeddings for another run's belief", func(ctx context.Context) error {
			_, err := client.AppendEmbeddings(ctx, &mnemosv1.AppendEmbeddingsRequest{
				Embeddings: []*mnemosv1.Embedding{{EntityId: "cl-theirs", EntityType: "claim", Vector: []float32{1}, Model: "m"}},
			})
			return err
		}, codes.PermissionDenied},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(scoped)
			if got := status.Code(err); got != tc.want {
				t.Errorf("got %v (%v), want %v", got, err, tc.want)
			}
		})
	}

	// The rejected episodes must not have landed: the check has to run before
	// the write, not alongside it.
	for _, id := range []string{"ev-new-theirs", "ev-new-unassigned"} {
		got, err := conn.Events.ListByIDs(context.Background(), []string{id})
		if err != nil {
			t.Fatalf("lookup %s: %v", id, err)
		}
		if len(got) != 0 {
			t.Errorf("%s was written despite PermissionDenied", id)
		}
	}
}

// TestGRPC_ReadRPCsEnforceRunAllowlist: the whitelist was a write-only
// boundary. ListEpisodes had no run filter at all and ListBeliefs treated
// run_id as an optional convenience, so a run-scoped bearer read the whole
// store by simply not asking for its own run.
func TestGRPC_ReadRPCsEnforceRunAllowlist(t *testing.T) {
	client, conn, issuer := startAuthServer(t)
	seedTwoRuns(t, conn)

	scoped := bearerCtx(t, mintToken(t, issuer, []string{domain.ScopeWildcard}, []string{"run-mine"}))
	unrestricted := bearerCtx(t, mintToken(t, issuer, []string{domain.ScopeWildcard}, nil))

	t.Run("ListEpisodes is intersected with the whitelist", func(t *testing.T) {
		resp, err := client.ListEpisodes(scoped, &mnemosv1.ListEpisodesRequest{})
		if err != nil {
			t.Fatalf("ListEpisodes: %v", err)
		}
		for _, e := range resp.GetEpisodes() {
			if e.GetRunId() != "run-mine" {
				t.Errorf("leaked episode %q from run %q", e.GetId(), e.GetRunId())
			}
		}
		if len(resp.GetEpisodes()) != 1 {
			t.Errorf("got %d episodes, want only the one in run-mine", len(resp.GetEpisodes()))
		}
	})

	t.Run("ListBeliefs without a run id is narrowed, not unfiltered", func(t *testing.T) {
		resp, err := client.ListBeliefs(scoped, &mnemosv1.ListBeliefsRequest{})
		if err != nil {
			t.Fatalf("ListBeliefs: %v", err)
		}
		for _, b := range resp.GetBeliefs() {
			if b.GetId() == "cl-theirs" {
				t.Error("leaked another run's belief when no run_id was supplied")
			}
		}
		if len(resp.GetBeliefs()) != 2 {
			t.Errorf("got %d beliefs, want the two in run-mine", len(resp.GetBeliefs()))
		}
	})

	t.Run("ListBeliefs for another run is refused, not silently empty", func(t *testing.T) {
		_, err := client.ListBeliefs(scoped, &mnemosv1.ListBeliefsRequest{RunId: "run-theirs"})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Errorf("got %v (%v), want PermissionDenied", got, err)
		}
	})

	t.Run("ListBeliefs for its own run still works", func(t *testing.T) {
		resp, err := client.ListBeliefs(scoped, &mnemosv1.ListBeliefsRequest{RunId: "run-mine"})
		if err != nil {
			t.Fatalf("ListBeliefs: %v", err)
		}
		if len(resp.GetBeliefs()) != 2 {
			t.Errorf("got %+v, want the two beliefs in run-mine", resp.GetBeliefs())
		}
	})

	// Control: the narrowing is the token's doing, not a filter that now hides
	// data from everyone.
	t.Run("an unrestricted token still sees both runs", func(t *testing.T) {
		eps, err := client.ListEpisodes(unrestricted, &mnemosv1.ListEpisodesRequest{})
		if err != nil {
			t.Fatalf("ListEpisodes: %v", err)
		}
		if len(eps.GetEpisodes()) != 2 {
			t.Errorf("got %d episodes, want 2", len(eps.GetEpisodes()))
		}
		bel, err := client.ListBeliefs(unrestricted, &mnemosv1.ListBeliefsRequest{})
		if err != nil {
			t.Fatalf("ListBeliefs: %v", err)
		}
		if len(bel.GetBeliefs()) != 3 {
			t.Errorf("got %d beliefs, want all 3", len(bel.GetBeliefs()))
		}
	})
}
