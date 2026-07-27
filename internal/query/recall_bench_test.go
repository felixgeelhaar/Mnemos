package query

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/ports"
	"go.klarlabs.de/mnemos/internal/store"
	_ "go.klarlabs.de/mnemos/internal/store/sqlite"
)

// benchDims is the vector width used by the recall benchmarks. 384 is the
// all-MiniLM / bge-small family width — the realistic local-embedding case,
// and small enough that a 50k-row corpus fits in memory without dominating
// the measurement with GC noise.
const benchDims = 384

// benchEmbedClient returns a deterministic pseudo-random unit-ish vector per
// text so cosine scores vary across the corpus (a constant vector would make
// every similarity identical and hide ordering regressions).
type benchEmbedClient struct{ model string }

func (c benchEmbedClient) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = deterministicVector(t, benchDims)
	}
	return out, nil
}

func (c benchEmbedClient) ModelID() string { return c.model }

// deterministicVector derives a stable vector from a seed string, so the same
// text always embeds to the same point and benchmarks/tests are reproducible.
func deterministicVector(seed string, dims int) []float32 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(seed); i++ {
		h ^= uint64(seed[i])
		h *= 1099511628211
	}
	r := rand.New(rand.NewSource(int64(h))) //nolint:gosec // deterministic test fixture, not crypto
	v := make([]float32, dims)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return v
}

// countingEmbeddingRepo wraps a real EmbeddingRepository and records the
// deterministic cost signals recall pays: how many whole-table scans it
// triggered and how many stored vectors those scans materialised in Go.
//
// Wall clock on this machine is unreliable (several suites run concurrently);
// vectorsMaterialised is exact and is the quantity the optimisation targets.
type countingEmbeddingRepo struct {
	inner ports.EmbeddingRepository

	listByTypeCalls     int
	vectorsMaterialised int
	searchCalls         int
	searchHits          int
}

func (r *countingEmbeddingRepo) Upsert(ctx context.Context, entityID, entityType string, vector []float32, model, createdBy string) error {
	return r.inner.Upsert(ctx, entityID, entityType, vector, model, createdBy)
}

func (r *countingEmbeddingRepo) ListByEntityType(ctx context.Context, entityType string) ([]domain.EmbeddingRecord, error) {
	recs, err := r.inner.ListByEntityType(ctx, entityType)
	r.listByTypeCalls++
	r.vectorsMaterialised += len(recs)
	return recs, err
}

func (r *countingEmbeddingRepo) Delete(ctx context.Context, entityID, entityType string) error {
	return r.inner.Delete(ctx, entityID, entityType)
}
func (r *countingEmbeddingRepo) CountAll(ctx context.Context) (int64, error) {
	return r.inner.CountAll(ctx)
}
func (r *countingEmbeddingRepo) ListAll(ctx context.Context) ([]domain.EmbeddingRecord, error) {
	return r.inner.ListAll(ctx)
}
func (r *countingEmbeddingRepo) DeleteAll(ctx context.Context) error { return r.inner.DeleteAll(ctx) }

// SearchClaimsByVector forwards to the wrapped repository's candidate-scoped
// searcher when it has one, counting the call. Absent on the inner repo, the
// capability is absent here too — reported via the searcherAvailable helper.
func (r *countingEmbeddingRepo) SearchClaimsByVector(
	ctx context.Context,
	queryVector []float32,
	candidateClaimIDs map[string]struct{},
	model string,
	topK int,
	minSimilarity float64,
) ([]ports.ClaimSimilarityHit, error) {
	s, ok := r.inner.(ports.ClaimSimilaritySearcher)
	if !ok {
		return nil, ports.ErrVectorSearchUnavailable
	}
	hits, err := s.SearchClaimsByVector(ctx, queryVector, candidateClaimIDs, model, topK, minSimilarity)
	r.searchCalls++
	r.searchHits += len(hits)
	return hits, err
}

func (r *countingEmbeddingRepo) reset() {
	r.listByTypeCalls = 0
	r.vectorsMaterialised = 0
	r.searchCalls = 0
	r.searchHits = 0
}

// benchCorpus is a populated SQLite brain plus the engine that reads it.
type benchCorpus struct {
	conn   *store.Conn
	embeds *countingEmbeddingRepo
	engine Engine
}

// buildBenchCorpus writes n events, n claims (one per event, linked by
// evidence) and 2n embeddings into a throwaway SQLite file, then wires the
// engine exactly as NewEngineWith does in production.
func buildBenchCorpus(tb testing.TB, n int) *benchCorpus {
	tb.Helper()
	ctx := context.Background()

	dir, err := os.MkdirTemp("", "mnemos-bench-*")
	if err != nil {
		tb.Fatalf("temp dir: %v", err)
	}
	tb.Cleanup(func() { _ = os.RemoveAll(dir) })

	conn, err := store.Open(ctx, "sqlite://"+filepath.Join(dir, "bench.db"))
	if err != nil {
		tb.Fatalf("open store: %v", err)
	}
	tb.Cleanup(func() { _ = conn.Close() })

	const model = "bench-model-384"
	now := time.Now().UTC()
	topics := []string{
		"payments latency", "auth token refresh", "database connection pool",
		"cache eviction", "deploy rollback", "index bloat", "queue backlog",
		"tls handshake", "memory leak", "rate limiting",
	}
	for i := 0; i < n; i++ {
		topic := topics[i%len(topics)]
		eventID := fmt.Sprintf("ev-%06d", i)
		claimID := fmt.Sprintf("cl-%06d", i)
		text := fmt.Sprintf("%s observation number %d in the %s subsystem", topic, i, topic)

		ev := domain.Event{
			ID:            eventID,
			SourceInputID: "bench-input",
			SchemaVersion: "v1",
			Content:       text,
			Timestamp:     now.Add(-time.Duration(i) * time.Minute),
			IngestedAt:    now,
			RunID:         "bench-run",
		}
		if err := conn.Events.Append(ctx, ev); err != nil {
			tb.Fatalf("append event %d: %v", i, err)
		}
		cl := domain.Claim{
			ID:         claimID,
			Text:       text,
			Type:       domain.ClaimTypeFact,
			Status:     domain.ClaimStatusActive,
			Confidence: 0.8,
			TrustScore: 0.8,
			CreatedAt:  now,
			ValidFrom:  now.Add(-time.Hour),
		}
		if err := conn.Claims.Upsert(ctx, []domain.Claim{cl}); err != nil {
			tb.Fatalf("upsert claim %d: %v", i, err)
		}
		if err := conn.Claims.UpsertEvidence(ctx, []domain.ClaimEvidence{{ClaimID: claimID, EventID: eventID}}); err != nil {
			tb.Fatalf("upsert evidence %d: %v", i, err)
		}
		if err := conn.Embeddings.Upsert(ctx, eventID, "event", deterministicVector(eventID, benchDims), model, "bench"); err != nil {
			tb.Fatalf("upsert event embedding %d: %v", i, err)
		}
		if err := conn.Embeddings.Upsert(ctx, claimID, "claim", deterministicVector(claimID, benchDims), model, "bench"); err != nil {
			tb.Fatalf("upsert claim embedding %d: %v", i, err)
		}
	}

	counting := &countingEmbeddingRepo{inner: conn.Embeddings}
	engine := NewEngineWith(
		conn.Events.(eventLister),
		conn.Claims,
		conn.Relationships,
		EngineDeps{Embeddings: counting, EmbedClient: benchEmbedClient{model: model}},
	)
	return &benchCorpus{conn: conn, embeds: counting, engine: engine}
}

// BenchmarkRecall_SQLite measures one full recall (the Claude Code hook's hot
// path) against corpora of 1k / 10k / 50k embedded events+claims.
//
// Wall clock (ns/op) is reported by the framework but is NOISY on a machine
// running several suites at once — read `vectors/op`, the number of stored
// vectors recall materialises and cosines in Go. That number is exact, is
// what grows linearly with the brain, and is what this work reduces.
//
// Run with:
//
//	MNEMOS_DB_URL='memory://throwaway' go test -run '^$' -bench BenchmarkRecall_SQLite \
//	  -benchtime 10x ./internal/query/
func BenchmarkRecall_SQLite(b *testing.B) {
	for _, n := range []int{1000, 10000, 50000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			c := buildBenchCorpus(b, n)
			ctx := context.Background()
			// Warm the page cache so the first iteration doesn't skew.
			if _, err := c.engine.Answer(ctx, "why is payments latency high?"); err != nil {
				b.Fatalf("warmup: %v", err)
			}
			c.embeds.reset()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := c.engine.Answer(ctx, "why is payments latency high?"); err != nil {
					b.Fatalf("answer: %v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(c.embeds.vectorsMaterialised)/float64(b.N), "vectors/op")
			b.ReportMetric(float64(c.embeds.listByTypeCalls)/float64(b.N), "tablescans/op")
			b.ReportMetric(float64(c.embeds.searchCalls)/float64(b.N), "vsearch/op")
		})
	}
}

// BenchmarkRecallLegs_SQLite attributes the cost of one recall to its parts so
// the bottleneck is evidence, not assumption:
//
//	events-listall  — loading every event row (the corpus-wide path)
//	event-cosine    — materialising every event vector and cosining in Go
//	claim-cosine    — materialising every CLAIM vector to score a handful of
//	                  admitted candidates (this leg runs on EVERY recall,
//	                  including the pgvector fast path)
//
// Read `vectors/op`; ns/op is min-of-N-noisy on a shared machine.
func BenchmarkRecallLegs_SQLite(b *testing.B) {
	const n = 10000
	c := buildBenchCorpus(b, n)
	ctx := context.Background()
	const q = "why is payments latency high?"

	allEvents, err := c.conn.Events.(eventLister).ListAll(ctx)
	if err != nil {
		b.Fatalf("list events: %v", err)
	}
	// The claim set a real recall actually scores: claims attached to the
	// top-5 ranked events. A handful of rows, against a 10k-vector table.
	admitted, err := c.conn.Claims.ListByEventIDs(ctx, []string{
		"ev-000000", "ev-000010", "ev-000020", "ev-000030", "ev-000040",
	})
	if err != nil {
		b.Fatalf("list claims: %v", err)
	}

	b.Run("events-listall", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := c.conn.Events.(eventLister).ListAll(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("event-cosine", func(b *testing.B) {
		c.embeds.reset()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.engine.cosineEventScores(ctx, q, allEvents)
		}
		b.StopTimer()
		b.ReportMetric(float64(c.embeds.vectorsMaterialised)/float64(b.N), "vectors/op")
	})
	b.Run("claim-cosine", func(b *testing.B) {
		c.embeds.reset()
		b.ReportAllocs()
		b.ReportMetric(float64(len(admitted)), "candidates")
		for i := 0; i < b.N; i++ {
			c.engine.cosineClaimScores(ctx, q, admitted)
		}
		b.StopTimer()
		b.ReportMetric(float64(c.embeds.vectorsMaterialised)/float64(b.N), "vectors/op")
	})
}
