package embedding

import (
	"fmt"
	"math/rand"
	"testing"
)

// BenchmarkVectorScan measures the two halves of what a whole-corpus recall
// pays per stored vector: decoding the little-endian blob into a []float32,
// and cosining it against the question embedding.
//
// It is the evidence behind the recall optimisation's shape. Decode costs MORE
// than the cosine at every realistic width and is the only one that allocates,
// so the lever that matters is materialising fewer vectors — not making the
// arithmetic faster. (A version of this work hoisted the query-norm out of the
// cosine loop; measured min-of-3 it moved 1101ns → 1083ns, i.e. nothing, and
// was dropped.)
//
// Wall clock on this machine is noisy — several suites run concurrently — so
// compare min-of-N across runs, never a single sample.
func BenchmarkVectorScan(b *testing.B) {
	r := rand.New(rand.NewSource(11)) //nolint:gosec // deterministic fixture, not crypto
	for _, dims := range []int{384, 1536} {
		q := make([]float32, dims)
		v := make([]float32, dims)
		for i := 0; i < dims; i++ {
			q[i] = float32(r.NormFloat64())
			v[i] = float32(r.NormFloat64())
		}
		blob := EncodeVector(v)

		b.Run(fmt.Sprintf("dims=%d/decode", dims), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := DecodeVector(blob); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("dims=%d/cosine", dims), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := CosineSimilarity(q, v); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
