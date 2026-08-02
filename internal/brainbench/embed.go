package brainbench

import (
	"hash/fnv"
	"math"
	"strings"
)

// StubEmbedderModel is the model id stamped on every vector this package
// produces. It is deliberately explicit about being a stub so the id shows up
// in stored rows and in the report, and nobody mistakes these vectors for a
// real model's.
const StubEmbedderModel = "brainbench-stub-bow-256"

// stubEmbedDims is the vector width. Wide enough that FNV collisions between
// distinct tokens are rare in a scenario-sized vocabulary, narrow enough to keep
// the seeded database small.
const stubEmbedDims = 256

// EmbedText produces a deterministic vector for text: an L2-normalised
// hashed bag-of-words.
//
// WHY A STUB AND NOT A REAL MODEL. The semantic-dedupe stage of consolidation
// only considers claims that have a stored embedding — with no embeddings it
// scans zero claims and the whole merge stage is a silent no-op. So a harness
// with no embedder cannot measure consolidation's largest stage at all. A real
// provider would fix that but would also make the harness require network, an
// API key, and money, and would make results non-reproducible across model
// versions — which for an experiment whose entire value is the diff is
// disqualifying.
//
// WHAT THIS COSTS, STATED PLAINLY. Bag-of-words cosine captures LEXICAL
// overlap. It scores an exact restatement at 1.0 and a heavy reword high, but
// it scores a true paraphrase with disjoint vocabulary near 0 — where a real
// embedding model would score it high. The dedupe numbers this harness reports
// are therefore a LOWER BOUND on what dedupe would merge in production. The
// harness says so in its limitations block; do not read a small merge count
// here as evidence that dedupe is weak in production.
func EmbedText(text string) []float32 {
	v := make([]float32, stubEmbedDims)
	for _, tok := range strings.Fields(strings.ToLower(text)) {
		tok = strings.Trim(tok, ".,;:!?\"'`()[]{}")
		if tok == "" {
			continue
		}
		h := fnv.New32a()
		// FNV's Write never returns an error.
		_, _ = h.Write([]byte(tok))
		v[h.Sum32()%stubEmbedDims]++
	}
	var sumSq float64
	for _, x := range v {
		sumSq += float64(x) * float64(x)
	}
	if sumSq == 0 {
		// An empty or all-punctuation text has no direction. Returning the zero
		// vector is correct: cosine against it is undefined, and the dedupe
		// planner skips zero-length vectors rather than treating them as
		// similar to everything.
		return v
	}
	norm := math.Sqrt(sumSq)
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}
