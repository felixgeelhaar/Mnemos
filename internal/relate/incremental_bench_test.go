package relate

import (
	"fmt"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// benchEngine returns an Engine with a fixed clock and a cheap sequential ID
// generator, so benchmark numbers measure detection and not crypto/rand.
func benchEngine() Engine {
	n := 0
	return Engine{
		now: func() time.Time { return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) },
		nextID: func() (string, error) {
			n++
			return fmt.Sprintf("rl_%d", n), nil
		},
	}
}

// BenchmarkDetectIncremental measures one realistic write: a batch of 20 newly
// extracted claims landing against an existing corpus of the given size. That
// is the shape of a capture — a handful of new beliefs against the whole brain —
// and it is the operation whose cost the audit finding says grows with the
// brain.
func BenchmarkDetectIncremental(b *testing.B) {
	for _, size := range []int{1000, 10000, 50000} {
		existing := generateCorpus(defaultCorpusOptions(size, 1))
		newClaims := benchBatch()
		e := benchEngine()
		b.Run(fmt.Sprintf("existing=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := e.DetectIncremental(newClaims, existing); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestCorpusProfile is not an assertion — it prints the document-frequency
// profile of each benchmark corpus so the benchmark numbers can be read against
// the selectivity they were measured under. Run with -v.
func TestCorpusProfile(t *testing.T) {
	for _, size := range []int{1000, 10000, 50000} {
		p := profileCorpus(generateCorpus(defaultCorpusOptions(size, 1)))
		t.Logf("size=%-6d distinct=%-6d postings=%-8d avgTokens=%.1f medianDF=%-4d p99DF=%-6d maxDF=%d",
			p.Claims, p.DistinctToks, p.TotalPostings, p.AvgTokens, p.MedianDF, p.P99DF, p.MaxDF)
	}
}

// BenchmarkDetectIncrementalReference measures the pre-change implementation on
// the identical corpora, so the before/after numbers come out of the same run
// on the same machine. This machine runs several test suites concurrently and
// wall clock alone has been misleading here before; the allocation counts are
// the deterministic signal.
func BenchmarkDetectIncrementalReference(b *testing.B) {
	for _, size := range []int{1000, 10000, 50000} {
		existing := generateCorpus(defaultCorpusOptions(size, 1))
		newClaims := benchBatch()
		e := benchEngine()
		b.Run(fmt.Sprintf("existing=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := referenceDetectIncremental(e, newClaims, existing); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// benchBatch is the 20-claim write both benchmarks measure.
func benchBatch() []domain.Claim {
	newClaims := generateCorpus(corpusOptions{
		Size: 20, VocabSize: 4000, Zipf: 1.0, RankOffset: 120, Seed: 99,
		MinTokens: 5, MaxTokens: 18,
		ShortShare: 0.10, NumShare: 0.20, NegShare: 0.12,
		AspectShar: 0.15, ProperShar: 0.10,
	})
	for i := range newClaims {
		newClaims[i].ID = fmt.Sprintf("cl_new_%03d", i)
	}
	return newClaims
}

// TestIncrementalSelectivity reports, for each benchmark corpus size, how much
// of the pair space the index eliminates. Deterministic — unlike wall clock, it
// does not move when the machine is busy. Run with -v.
func TestIncrementalSelectivity(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 50k-claim corpus")
	}
	newClaims := benchBatch()
	for _, size := range []int{1000, 10000, 50000} {
		existing := generateCorpus(defaultCorpusOptions(size, 1))
		_, stats, err := benchEngine().DetectIncrementalWithStats(newClaims, existing)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("existing=%-6d path=%-5s pairs_possible=%-9d pairs_probed=%-8d pairs_evaluated=%-7d (%.3f%%) rels=%d",
			size, stats.Path, stats.PairsPossible, stats.PairsProbed, stats.PairsEvaluated,
			100*float64(stats.PairsEvaluated)/float64(stats.PairsPossible), stats.Relationships)
	}
}

// BenchmarkBuildCandidateIndex isolates the part of the incremental path that
// is still linear in corpus size: tokenizing every existing claim and building
// the postings map, from scratch, on every write.
//
// It is measured separately because it is the hand-off. Nothing inside
// internal/relate can avoid it — the caller hands DetectIncremental a
// []domain.Claim and the package has to look at all of them. Making it go away
// means a persistent or store-side index, which is a change to the storage
// ports.
func BenchmarkBuildCandidateIndex(b *testing.B) {
	for _, size := range []int{1000, 10000, 50000} {
		existing := generateCorpus(defaultCorpusOptions(size, 1))
		b.Run(fmt.Sprintf("existing=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = buildCandidateIndex(existing)
			}
		})
	}
}
