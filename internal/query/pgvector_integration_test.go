package query

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/ports"
	"go.klarlabs.de/mnemos/internal/store"
	_ "go.klarlabs.de/mnemos/internal/store/postgres"
)

// Recall's two scale seams meet on Postgres: the event leg is served natively
// by pgvector's `<=>`, and the claim leg by the candidate-scoped searcher. The
// tests below run the real engine against a real database because that is the
// only place the interaction is observable — a fake that implements both ports
// proves the engine's plumbing, not that Postgres agrees with Go about which
// claim is nearest.
//
// Gated on TEST_POSTGRES_DSN like every other Postgres suite here. Bring one up
// with:
//
//	docker run -d --name pgv -e POSTGRES_PASSWORD=mnemos -e POSTGRES_USER=mnemos \
//	  -e POSTGRES_DB=mnemos -p 55440:5432 pgvector/pgvector:pg17
//	export TEST_POSTGRES_DSN='postgres://mnemos:mnemos@127.0.0.1:55440/mnemos?sslmode=disable'

func pgDSNOrSkip(tb testing.TB, namespace string) string {
	tb.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		tb.Skip("TEST_POSTGRES_DSN not set; skipping Postgres recall integration test")
	}
	sep := "?"
	for i := 0; i < len(dsn); i++ {
		if dsn[i] == '?' {
			sep = "&"
			break
		}
	}
	return dsn + sep + "namespace=" + namespace
}

// pgCorpus is a populated Postgres brain plus the two engines that read it:
// one allowed the candidate-scoped claim searcher, one forced to scan.
type pgCorpus struct {
	conn   *store.Conn
	fast   Engine
	corpus Engine
	counts *countingEmbeddingRepo
	claims []domain.Claim
	model  string
}

func newPGCorpus(tb testing.TB, namespace string, n int) *pgCorpus {
	tb.Helper()
	ctx := context.Background()
	conn, err := store.Open(ctx, pgDSNOrSkip(tb, namespace))
	if err != nil {
		tb.Fatalf("open postgres: %v", err)
	}
	tb.Cleanup(func() { _ = conn.Close() })

	// A namespace can survive a previous run; start from a known-empty corpus
	// so counts and rankings are reproducible.
	if err := conn.Embeddings.DeleteAll(ctx); err != nil {
		tb.Fatalf("reset embeddings: %v", err)
	}

	const model = "pg-model-384"
	now := time.Now().UTC()
	topics := []string{
		"payments latency", "auth token refresh", "database connection pool",
		"cache eviction", "deploy rollback",
	}
	for i := 0; i < n; i++ {
		topic := topics[i%len(topics)]
		eventID := fmt.Sprintf("pgev-%05d", i)
		claimID := fmt.Sprintf("pgcl-%05d", i)
		text := fmt.Sprintf("%s observation number %d in the %s subsystem", topic, i, topic)

		if err := conn.Events.Append(ctx, domain.Event{
			ID: eventID, SourceInputID: "pg-input", SchemaVersion: "v1",
			Content: text, Timestamp: now.Add(-time.Duration(i) * time.Minute),
			IngestedAt: now, RunID: "pg-run",
		}); err != nil {
			tb.Fatalf("append event %d: %v", i, err)
		}
		if err := conn.Claims.Upsert(ctx, []domain.Claim{{
			ID: claimID, Text: text, Type: domain.ClaimTypeFact,
			Status: domain.ClaimStatusActive, Confidence: 0.8, TrustScore: 0.8,
			CreatedAt: now, ValidFrom: now.Add(-time.Hour),
		}}); err != nil {
			tb.Fatalf("upsert claim %d: %v", i, err)
		}
		if err := conn.Claims.UpsertEvidence(ctx, []domain.ClaimEvidence{
			{ClaimID: claimID, EventID: eventID},
		}); err != nil {
			tb.Fatalf("upsert evidence %d: %v", i, err)
		}
		if err := conn.Embeddings.Upsert(ctx, eventID, "event", deterministicVector(eventID, benchDims), model, "pg"); err != nil {
			tb.Fatalf("upsert event embedding %d: %v", i, err)
		}
		if err := conn.Embeddings.Upsert(ctx, claimID, "claim", deterministicVector(claimID, benchDims), model, "pg"); err != nil {
			tb.Fatalf("upsert claim embedding %d: %v", i, err)
		}
	}

	all, err := conn.Claims.ListAll(ctx)
	if err != nil {
		tb.Fatalf("list claims: %v", err)
	}
	counts := &countingEmbeddingRepo{inner: conn.Embeddings}
	c := &pgCorpus{conn: conn, counts: counts, claims: all, model: model}
	c.fast = NewEngineWith(conn.Events.(eventLister), conn.Claims, conn.Relationships,
		EngineDeps{Embeddings: counts, EmbedClient: benchEmbedClient{model: model}})
	c.corpus = NewEngineWith(conn.Events.(eventLister), conn.Claims, conn.Relationships,
		EngineDeps{Embeddings: corpusOnlyRepo{inner: conn.Embeddings}, EmbedClient: benchEmbedClient{model: model}})
	return c
}

// TestPostgres_ClaimCandidateSearch_MatchesCorpusScan is the equivalence gate on
// a real backend: Postgres scoring the admitted claims must produce the same
// map as Go scoring the whole table.
func TestPostgres_ClaimCandidateSearch_MatchesCorpusScan(t *testing.T) {
	c := newPGCorpus(t, "recallequiv", 120)
	ctx := context.Background()

	for _, q := range []string{
		"why is payments latency high?",
		"what happened to the database connection pool?",
		"nothing here resembles this question at all",
	} {
		for _, size := range []int{1, 5, 60} {
			t.Run(fmt.Sprintf("%d/%s", size, q), func(t *testing.T) {
				set := c.claims[:size]
				want := c.corpus.cosineClaimScores(ctx, q, set)
				got := c.fast.cosineClaimScores(ctx, q, set)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("candidate path diverged on Postgres\n got: %v\nwant: %v", got, want)
				}
			})
		}
	}
}

// TestPostgres_Recall_UsesBothScaleSeams proves the end state on the backend
// that has both: no whole-table embedding scan survives a recall. The event leg
// goes through pgvector, the claim leg through the candidate searcher, and the
// question is embedded once.
func TestPostgres_Recall_UsesBothScaleSeams(t *testing.T) {
	c := newPGCorpus(t, "recallseams", 120)
	ctx := context.Background()

	if _, ok := c.conn.Embeddings.(ports.EventVectorSearcher); !ok {
		t.Fatal("postgres embedding repository does not implement EventVectorSearcher")
	}
	if _, ok := c.conn.Embeddings.(ports.ClaimSimilaritySearcher); !ok {
		t.Fatal("postgres embedding repository does not implement ClaimSimilaritySearcher")
	}

	c.counts.reset()
	if _, err := c.fast.Answer(ctx, "why is payments latency high?"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if c.counts.listByTypeCalls != 0 {
		t.Fatalf("recall still scanned the embedding table %d time(s); "+
			"both legs should be served by a pushed-down search",
			c.counts.listByTypeCalls)
	}
	if c.counts.searchCalls == 0 {
		t.Fatal("the candidate-scoped claim search never ran")
	}
}

// TestPostgres_Recall_AnswerUnchangedByCandidatePath is the user-visible check,
// and the strongest ordering claim in this file: corpusOnlyRepo hides BOTH
// optional capabilities, so it compares "every leg pushed down into Postgres"
// against "every leg scanned in Go". The claim ids and their order in the
// returned answer must be identical.
func TestPostgres_Recall_AnswerUnchangedByCandidatePath(t *testing.T) {
	c := newPGCorpus(t, "recallanswer", 120)
	ctx := context.Background()

	for _, q := range []string{
		"why is payments latency high?",
		"cache eviction and deploy rollback",
	} {
		fast, err := c.fast.Answer(ctx, q)
		if err != nil {
			t.Fatalf("fast answer: %v", err)
		}
		slow, err := c.corpus.Answer(ctx, q)
		if err != nil {
			t.Fatalf("corpus answer: %v", err)
		}
		if len(fast.Claims) != len(slow.Claims) {
			t.Fatalf("%q: %d claims vs %d", q, len(fast.Claims), len(slow.Claims))
		}
		for i := range slow.Claims {
			if fast.Claims[i].ID != slow.Claims[i].ID {
				t.Fatalf("%q: rank %d is %s, want %s", q, i, fast.Claims[i].ID, slow.Claims[i].ID)
			}
		}
	}
}
