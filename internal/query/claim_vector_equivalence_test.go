package query

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"testing"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/ports"
)

// corpusOnlyRepo hides a repository's ClaimSimilaritySearcher so the engine is
// forced down the whole-corpus ListByEntityType path. Wrapping (rather than a
// second store) keeps both paths reading byte-identical data, which is the only
// way an equivalence assertion means anything.
type corpusOnlyRepo struct {
	inner ports.EmbeddingRepository
	scans *int
}

func (r corpusOnlyRepo) Upsert(ctx context.Context, entityID, entityType string, vector []float32, model, createdBy string) error {
	return r.inner.Upsert(ctx, entityID, entityType, vector, model, createdBy)
}
func (r corpusOnlyRepo) ListByEntityType(ctx context.Context, t string) ([]domain.EmbeddingRecord, error) {
	if r.scans != nil {
		*r.scans++
	}
	return r.inner.ListByEntityType(ctx, t)
}
func (r corpusOnlyRepo) Delete(ctx context.Context, entityID, entityType string) error {
	return r.inner.Delete(ctx, entityID, entityType)
}
func (r corpusOnlyRepo) CountAll(ctx context.Context) (int64, error) { return r.inner.CountAll(ctx) }
func (r corpusOnlyRepo) ListAll(ctx context.Context) ([]domain.EmbeddingRecord, error) {
	return r.inner.ListAll(ctx)
}
func (r corpusOnlyRepo) DeleteAll(ctx context.Context) error { return r.inner.DeleteAll(ctx) }

// equivFixture builds one SQLite brain and returns two engines over it: one
// that can use the candidate-scoped claim searcher and one that cannot.
type equivFixture struct {
	fast        Engine
	corpus      Engine
	claims      []domain.Claim
	corpusScans int
	fastScans   int
}

func newEquivFixture(tb testing.TB, n int, model string) *equivFixture {
	tb.Helper()
	c := buildBenchCorpusWithModel(tb, n, model)
	ctx := context.Background()

	all, err := c.conn.Claims.ListAll(ctx)
	if err != nil {
		tb.Fatalf("list claims: %v", err)
	}

	f := &equivFixture{claims: all}
	fastRepo := &countingEmbeddingRepo{inner: c.conn.Embeddings}
	f.fast = NewEngineWith(c.conn.Events.(eventLister), c.conn.Claims, c.conn.Relationships,
		EngineDeps{Embeddings: fastRepo, EmbedClient: benchEmbedClient{model: model}})
	f.corpus = NewEngineWith(c.conn.Events.(eventLister), c.conn.Claims, c.conn.Relationships,
		EngineDeps{
			Embeddings:  corpusOnlyRepo{inner: c.conn.Embeddings, scans: &f.corpusScans},
			EmbedClient: benchEmbedClient{model: model},
		})
	return f
}

// TestCosineClaimScores_CandidatePathMatchesCorpusScan is the equivalence gate.
// The candidate-scoped path is not an approximation — it is the same cosine over
// the same vectors with the unusable rows removed before decode — so the scores
// must match EXACTLY, not within a tolerance.
func TestCosineClaimScores_CandidatePathMatchesCorpusScan(t *testing.T) {
	const model = "equiv-model-384"
	f := newEquivFixture(t, 200, model)
	ctx := context.Background()

	questions := []string{
		"why is payments latency high?",
		"what happened to the database connection pool?",
		"deploy rollback",
		"something entirely unrelated to any stored topic, e.g. gardening",
	}
	// Candidate sets of the shapes recall actually produces: a handful, one,
	// and a large slice.
	sets := map[string][]domain.Claim{
		"five":     f.claims[:5],
		"one":      f.claims[:1],
		"eighty":   f.claims[:80],
		"disjoint": append(append([]domain.Claim{}, f.claims[3:6]...), f.claims[190:]...),
	}

	for _, q := range questions {
		for name, set := range sets {
			t.Run(fmt.Sprintf("%s/%s", name, q), func(t *testing.T) {
				want := f.corpus.cosineClaimScores(ctx, q, set)
				got := f.fast.cosineClaimScores(ctx, q, set)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("candidate path diverged\n got: %v\nwant: %v", got, want)
				}
			})
		}
	}
}

// TestRankClaimsByHybrid_OrderingUnchanged checks the thing users see: the
// ORDER claims come back in. Score equality already implies it, but ordering is
// the contract that matters and a future change to either path should break
// this test loudly.
func TestRankClaimsByHybrid_OrderingUnchanged(t *testing.T) {
	const model = "equiv-model-384"
	f := newEquivFixture(t, 200, model)
	ctx := context.Background()

	for _, q := range []string{"why is payments latency high?", "index bloat in the queue backlog"} {
		set := f.claims[:40]
		want := f.corpus.rankClaimsByHybrid(ctx, q, set, false)
		got := f.fast.rankClaimsByHybrid(ctx, q, set, false)
		if len(got) != len(want) {
			t.Fatalf("%q: length %d != %d", q, len(got), len(want))
		}
		for i := range want {
			if got[i].ID != want[i].ID {
				t.Fatalf("%q: rank %d is %s, want %s", q, i, got[i].ID, want[i].ID)
			}
		}
	}
}

// TestCosineClaimScores_PreservesModelFilter is the correctness guard that
// matters most. Vectors are keyed by embedding model; a mismatch must return
// NOTHING (silent-empty), never a score computed against a foreign model's
// vector space (silent-WRONG). The candidate path passes the model id to the
// store, so this asserts the store actually honours it end to end.
func TestCosineClaimScores_PreservesModelFilter(t *testing.T) {
	// Corpus written under one model, queried by an embedder claiming another.
	f := newEquivFixture(t, 50, "stored-model-384")
	ctx := context.Background()

	// Point both engines at a DIFFERENT query model over the same store.
	// Reusing the fixture keeps the stored data identical.
	fast := f.fast
	fast.embedClient = benchEmbedClient{model: "other-model-384"}
	corpus := f.corpus
	corpus.embedClient = benchEmbedClient{model: "other-model-384"}

	set := f.claims[:5]
	if got := fast.cosineClaimScores(ctx, "why is payments latency high?", set); len(got) != 0 {
		t.Fatalf("candidate path scored across a model boundary: %v", got)
	}
	if got := corpus.cosineClaimScores(ctx, "why is payments latency high?", set); len(got) != 0 {
		t.Fatalf("corpus path scored across a model boundary: %v", got)
	}
}

// TestCosineClaimScores_KeepsNegativeSimilarities pins why claimVectorFloor is
// -Inf. A conventional 0 floor would drop anti-correlated claims from the score
// map, and rankClaimsByHybrid treats "absent" as no-signal (-1 sentinel) rather
// than as a bad match — quietly promoting the worst matches above unscored ones.
func TestCosineClaimScores_KeepsNegativeSimilarities(t *testing.T) {
	if !math.IsInf(claimVectorFloor, -1) {
		t.Fatalf("claimVectorFloor = %v; a floor above -1 discards negative cosines", claimVectorFloor)
	}

	ctx := context.Background()
	repo := newMemVectorRepo()
	// Two opposed unit vectors: the query embeds to +x, the stored claim to -x,
	// so the true cosine is exactly -1.
	repo.put("cl-neg", "claim", []float32{-1, 0, 0}, "m")
	repo.put("cl-pos", "claim", []float32{1, 0, 0}, "m")

	e := Engine{
		embeddings:        repo,
		claimVectorSearch: repo,
		embedClient:       fixedVectorClient{vec: []float32{1, 0, 0}, model: "m"},
	}
	got := e.cosineClaimScores(ctx, "q", []domain.Claim{{ID: "cl-neg"}, {ID: "cl-pos"}})
	if len(got) != 2 {
		t.Fatalf("want both claims scored, got %v", got)
	}
	if got["cl-neg"] != -1 {
		t.Fatalf("negative similarity dropped or clamped: got %v, want -1", got["cl-neg"])
	}
}

// TestCosineClaimScores_FallsBackWhenSearcherFails proves the candidate path is
// an optimisation, not a dependency: a searcher error must yield the same
// answer via the corpus scan, never an empty one.
func TestCosineClaimScores_FallsBackWhenSearcherFails(t *testing.T) {
	ctx := context.Background()
	repo := newMemVectorRepo()
	repo.put("cl-1", "claim", []float32{1, 0, 0}, "m")
	repo.searchErr = ports.ErrVectorSearchUnavailable

	e := Engine{
		embeddings:        repo,
		claimVectorSearch: repo,
		embedClient:       fixedVectorClient{vec: []float32{1, 0, 0}, model: "m"},
	}
	got := e.cosineClaimScores(ctx, "q", []domain.Claim{{ID: "cl-1"}})
	if len(got) != 1 || got["cl-1"] != 1 {
		t.Fatalf("fallback did not run: got %v, want {cl-1: 1}", got)
	}
	if repo.listByTypeCalls == 0 {
		t.Fatal("expected the corpus scan to run after the searcher failed")
	}
}

// TestWithEmbeddings_AdoptsClaimSimilaritySearcher pins the capability
// detection: it follows the EventVectorSearcher precedent (a type assertion in
// WithEmbeddings), so wiring a store that can do candidate search is enough.
func TestWithEmbeddings_AdoptsClaimSimilaritySearcher(t *testing.T) {
	repo := newMemVectorRepo()
	e := NewEngine(nil, nil, nil).WithEmbeddings(repo, fixedVectorClient{vec: []float32{1}, model: "m"})
	if e.claimVectorSearch == nil {
		t.Fatal("WithEmbeddings did not adopt the ClaimSimilaritySearcher")
	}

	plain := NewEngine(nil, nil, nil).WithEmbeddings(plainEmbeddingRepo{}, fixedVectorClient{vec: []float32{1}, model: "m"})
	if plain.claimVectorSearch != nil {
		t.Fatal("adopted a searcher from a repository that has none")
	}
}
