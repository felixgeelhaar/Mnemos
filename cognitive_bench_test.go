package mnemos

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/ports"

	_ "go.klarlabs.de/mnemos/internal/store/memory"
)

// This file is the measurement harness for the bounded cognitive reads
// (Recombinations, KnowledgeGaps, WhoKnows, Hypercorrections, Scan,
// AnalogousClaims). It exists so the bounds those endpoints apply are argued
// from numbers rather than from intuition.
//
// It reports DETERMINISTIC metrics first — store round trips and pair/candidate
// counts — because this repository has twice been misled by wall clock read off
// a machine running several test suites at once. Wall clock is reported too, but
// only as min-of-N (see benchMinOf).
//
// Nothing here runs under a plain `go test`: benchmarks need -bench, and the
// census tests are behind -short guards / explicit -run selectors.

// ---------------------------------------------------------------------------
// counting repositories
// ---------------------------------------------------------------------------

// storeCounts tallies store round trips by method. Every cognitive read in this
// package reaches the store only through these two repositories.
type storeCounts struct {
	claimsListAll      atomic.Int64
	claimsListByIDs    atomic.Int64
	claimsListEvidence atomic.Int64
	relsListAll        atomic.Int64
	relsListByClaimIDs atomic.Int64
	entityRelsByKind   atomic.Int64
}

func (c *storeCounts) total() int64 {
	return c.claimsListAll.Load() + c.claimsListByIDs.Load() + c.claimsListEvidence.Load() +
		c.relsListAll.Load() + c.relsListByClaimIDs.Load() + c.entityRelsByKind.Load()
}

func (c *storeCounts) String() string {
	return fmt.Sprintf("total=%d claims.ListAll=%d claims.ListByIDs=%d claims.ListAllEvidence=%d rels.ListAll=%d rels.ListByClaimIDs=%d entityRels.ListByKind=%d",
		c.total(), c.claimsListAll.Load(), c.claimsListByIDs.Load(), c.claimsListEvidence.Load(),
		c.relsListAll.Load(), c.relsListByClaimIDs.Load(), c.entityRelsByKind.Load())
}

type countingClaims struct {
	ports.ClaimRepository
	c *storeCounts
}

func (r countingClaims) ListAll(ctx context.Context) ([]domain.Claim, error) {
	r.c.claimsListAll.Add(1)
	return r.ClaimRepository.ListAll(ctx)
}

func (r countingClaims) ListByIDs(ctx context.Context, ids []string) ([]domain.Claim, error) {
	r.c.claimsListByIDs.Add(1)
	return r.ClaimRepository.ListByIDs(ctx, ids)
}

func (r countingClaims) ListAllEvidence(ctx context.Context) ([]domain.ClaimEvidence, error) {
	r.c.claimsListEvidence.Add(1)
	return r.ClaimRepository.ListAllEvidence(ctx)
}

type countingRels struct {
	ports.RelationshipRepository
	c *storeCounts
}

func (r countingRels) ListAll(ctx context.Context) ([]domain.Relationship, error) {
	r.c.relsListAll.Add(1)
	return r.RelationshipRepository.ListAll(ctx)
}

func (r countingRels) ListByClaimIDs(ctx context.Context, ids []string) ([]domain.Relationship, error) {
	r.c.relsListByClaimIDs.Add(1)
	return r.RelationshipRepository.ListByClaimIDs(ctx, ids)
}

type countingEntityRels struct {
	ports.EntityRelationshipRepository
	c *storeCounts
}

func (r countingEntityRels) ListByKind(ctx context.Context, kind string) ([]domain.EntityRelationship, error) {
	r.c.entityRelsByKind.Add(1)
	return r.EntityRelationshipRepository.ListByKind(ctx, kind)
}

// instrument wraps m's repositories so store round trips are counted. Returns
// the tally, which is live for the rest of m's life.
func instrument(m *memory) *storeCounts {
	c := &storeCounts{}
	m.conn.Claims = countingClaims{m.conn.Claims, c}
	m.conn.Relationships = countingRels{m.conn.Relationships, c}
	if m.conn.EntityRels != nil {
		m.conn.EntityRels = countingEntityRels{m.conn.EntityRels, c}
	}
	return c
}

// ---------------------------------------------------------------------------
// synthetic corpora
// ---------------------------------------------------------------------------

// corpusOpts describes a synthetic brain.
type corpusOpts struct {
	// Claims is the number of claims to seed.
	Claims int
	// TopicSize is how many claims share a topic vocabulary. Real brains cluster
	// topically; a uniformly-random corpus would understate pair-similarity work
	// because almost no pair would clear RecombineSimilarityFloor.
	TopicSize int
	// EdgesPerTopic chains this many supports edges inside each topic, giving the
	// structural reads (AnalogousClaims) something to walk.
	EdgesPerTopic int
	// Contradictions is how many contradicts edges to sprinkle in (drives
	// Hypercorrections and the contested half of KnowledgeGaps).
	Contradictions int
}

func defaultCorpus(n int) corpusOpts {
	return corpusOpts{Claims: n, TopicSize: 50, EdgesPerTopic: 40, Contradictions: n / 20}
}

var corpusVocab = func() []string {
	// A fixed, deterministic vocabulary. Per-topic slices of it give claims in a
	// topic high mutual Jaccard and claims across topics ~none.
	words := make([]string, 0, 2400)
	stems := []string{
		"deploy", "rollback", "latency", "throughput", "cache", "index", "shard", "replica",
		"timeout", "retry", "circuit", "quota", "token", "tenant", "schema", "migration",
		"cluster", "queue", "consumer", "producer", "checkpoint", "snapshot", "backup", "restore",
	}
	for i := 0; i < 100; i++ {
		for _, s := range stems {
			words = append(words, s+strconv.Itoa(i))
		}
	}
	return words
}()

// seedCorpus writes a synthetic brain into m. Deterministic for a given opts.
func seedCorpus(tb testing.TB, m *memory, opts corpusOpts) {
	tb.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Add(-30 * 24 * time.Hour)
	rng := rand.New(rand.NewSource(1))

	const topicVocab = 12 // words available to a topic
	const claimWords = 8  // words used by one claim

	batch := make([]domain.Claim, 0, 1000)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := m.conn.Claims.Upsert(ctx, batch); err != nil {
			tb.Fatalf("seed claims: %v", err)
		}
		batch = batch[:0]
	}
	for i := 0; i < opts.Claims; i++ {
		topic := i / opts.TopicSize
		base := (topic * topicVocab) % (len(corpusVocab) - topicVocab)
		words := make([]string, 0, claimWords)
		perm := rng.Perm(topicVocab)[:claimWords]
		for _, p := range perm {
			words = append(words, corpusVocab[base+p])
		}
		text := ""
		for j, w := range words {
			if j > 0 {
				text += " "
			}
			text += w
		}
		typ := domain.ClaimTypeDecision // salience 0.565 at conf 0.9 — clears RecombineSalienceFloor
		if i%7 == 0 {
			typ = domain.ClaimTypeHypothesis // unresolved-hypothesis gaps
		}
		batch = append(batch, domain.Claim{
			ID: fmt.Sprintf("c%06d", i), Text: text, Type: typ,
			Confidence: 0.9, TrustScore: 0.4 + 0.5*float64(i%5)/4,
			Status: domain.ClaimStatusActive, CreatedBy: fmt.Sprintf("worker-%d", i%25),
			CreatedAt: now, ValidFrom: now,
		})
		if len(batch) == cap(batch) {
			flush()
		}
	}
	flush()

	rels := make([]domain.Relationship, 0, 1000)
	flushRels := func() {
		if len(rels) == 0 {
			return
		}
		if err := m.conn.Relationships.Upsert(ctx, rels); err != nil {
			tb.Fatalf("seed relationships: %v", err)
		}
		rels = rels[:0]
	}
	topics := opts.Claims / opts.TopicSize
	for t := 0; t < topics; t++ {
		for e := 0; e < opts.EdgesPerTopic && e+1 < opts.TopicSize; e++ {
			from := t*opts.TopicSize + e
			to := t*opts.TopicSize + e + 1
			rels = append(rels, domain.Relationship{
				ID: fmt.Sprintf("r%06d-%03d", t, e), Type: domain.RelationshipTypeSupports,
				FromClaimID: fmt.Sprintf("c%06d", from), ToClaimID: fmt.Sprintf("c%06d", to),
				CreatedAt: now,
			})
			if len(rels) == cap(rels) {
				flushRels()
			}
		}
	}
	for i := 0; i < opts.Contradictions; i++ {
		a := (i * 13) % opts.Claims
		b := (a + 1) % opts.Claims
		rels = append(rels, domain.Relationship{
			ID: fmt.Sprintf("x%06d", i), Type: domain.RelationshipTypeContradicts,
			FromClaimID: fmt.Sprintf("c%06d", a), ToClaimID: fmt.Sprintf("c%06d", b),
			CreatedAt: now,
		})
		if len(rels) == cap(rels) {
			flushRels()
		}
	}
	flushRels()
}

// benchMem builds an isolated in-memory brain. Namespaces are per-call so two
// corpora never share state.
func benchMem(tb testing.TB, ns string) *memory {
	tb.Helper()
	for _, k := range []string{"MNEMOS_STORAGE", "MNEMOS_MODE", "MNEMOS_LLM_PROVIDER", "MNEMOS_API_KEY"} {
		tb.Setenv(k, "")
	}
	mem, err := New(WithStorage("memory://?namespace="+ns), WithPassiveMode())
	if err != nil {
		tb.Fatalf("New: %v", err)
	}
	tb.Cleanup(func() { _ = mem.Close() })
	return mem.(*memory)
}

// benchMinOf runs fn n times and returns the fastest run. Min-of-N is the only
// wall-clock statistic reported here: this machine runs concurrent test suites,
// so the mean and the max measure contention, not the code.
func benchMinOf(n int, fn func()) time.Duration {
	best := time.Duration(1<<63 - 1)
	for i := 0; i < n; i++ {
		start := time.Now()
		fn()
		if d := time.Since(start); d < best {
			best = d
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// census: the numbers reported in the PR body
// ---------------------------------------------------------------------------

var censusSizes = []int{1000, 10000, 50000}

func censusEnabled() bool { return os.Getenv("MNEMOS_CENSUS") == "1" }

// TestCognitiveCensus prints the per-endpoint cost table. It is skipped in
// -short mode and under a normal `go test` run (it needs -run and CENSUS=1)
// because the 50k Recombinations pass is deliberately expensive.
func TestCognitiveCensus(t *testing.T) {
	if testing.Short() {
		t.Skip("census is a measurement harness, not an assertion")
	}
	if v := censusEnabled(); !v {
		t.Skip("set MNEMOS_CENSUS=1 to run the cost census")
	}
	for _, n := range censusSizes {
		n := n
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			ctx := context.Background()
			m := benchMem(t, "census"+strconv.Itoa(n))
			seedCorpus(t, m, defaultCorpus(n))
			counts := instrument(m)

			type probe struct {
				name string
				run  func() int
			}
			probes := []probe{
				{"Recombinations", func() int { r, err := m.Recombinations(ctx, 20); mustNoErr(t, err); return len(r) }},
				{"KnowledgeGaps", func() int { r, err := m.KnowledgeGaps(ctx, 20); mustNoErr(t, err); return len(r) }},
				{"WhoKnows", func() int {
					r, err := m.WhoKnows(ctx, "deploy0 rollback0 latency0", 10)
					mustNoErr(t, err)
					return len(r)
				}},
				{"Hypercorrections", func() int { r, err := m.Hypercorrections(ctx); mustNoErr(t, err); return len(r) }},
				{"Scan", func() int { r, err := m.Scan(ctx, ScanQuery{}); mustNoErr(t, err); return len(r) }},
				{"AnalogousClaims", func() int {
					r, err := m.AnalogousClaims(ctx, "c000001", 10)
					mustNoErr(t, err)
					return len(r)
				}},
			}
			for _, p := range probes {
				start := counts.total()
				var got int
				d := benchMinOf(1, func() { got = p.run() })
				t.Logf("n=%-6d %-16s results=%-6d roundtrips=%-7d wall(min-of-1)=%s",
					n, p.name, got, counts.total()-start, d.Round(time.Millisecond))
			}
		})
	}
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
}
