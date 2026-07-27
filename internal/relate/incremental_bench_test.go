package relate

import (
	"fmt"
	"testing"
	"time"
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
		newClaims := generateCorpus(corpusOptions{
			Size: 20, VocabSize: 4000, Zipf: 1.0, RankOffset: 120, Seed: 99,
			MinTokens: 5, MaxTokens: 18,
			ShortShare: 0.10, NumShare: 0.20, NegShare: 0.12,
			AspectShar: 0.15, ProperShar: 0.10,
		})
		for i := range newClaims {
			newClaims[i].ID = fmt.Sprintf("cl_new_%03d", i)
		}
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
