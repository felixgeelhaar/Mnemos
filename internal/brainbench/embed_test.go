package brainbench

import (
	"math"
	"testing"
)

func cosine(a, b []float32) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

// TestEmbedText_Deterministic is a precondition of the whole harness: if the
// embedder varied run to run, the two untreated arms would disagree and every
// run would be reported invalid.
func TestEmbedText_Deterministic(t *testing.T) {
	const text = "The billing service runs on PostgreSQL 16"
	a, b := EmbedText(text), EmbedText(text)
	if len(a) != len(b) {
		t.Fatalf("dimension mismatch: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("embedding is not deterministic at index %d: %v vs %v", i, a[i], b[i])
		}
	}
}

// TestEmbedText_Normalised checks the unit-length invariant the store's cosine
// ranking assumes.
func TestEmbedText_Normalised(t *testing.T) {
	v := EmbedText("some ordinary sentence with several words")
	if math.Abs(cosine(v, v)-1) > 1e-5 {
		t.Fatalf("vector is not unit length: self-cosine = %v", cosine(v, v))
	}
}

// TestEmbedText_ConstantDimension pins the constant-width contract; a varying
// dimension would make cosine comparisons silently wrong.
func TestEmbedText_ConstantDimension(t *testing.T) {
	for _, s := range []string{"", "one", "a much longer sentence with many more distinct tokens in it"} {
		if got := len(EmbedText(s)); got != stubEmbedDims {
			t.Errorf("EmbedText(%q) width = %d, want %d", s, got, stubEmbedDims)
		}
	}
}

// TestEmbedText_EmptyIsZeroVector documents that a text with no usable tokens
// produces the zero vector rather than an arbitrary direction. The dedupe
// planner skips zero-length vectors; a normalised garbage direction would
// instead look similar to something.
func TestEmbedText_EmptyIsZeroVector(t *testing.T) {
	for _, s := range []string{"", "   ", "..."} {
		for _, x := range EmbedText(s) {
			if x != 0 {
				t.Fatalf("EmbedText(%q) should be the zero vector", s)
			}
		}
	}
}

// TestEmbedText_SimilarityBand documents, as an executable claim, exactly what
// the stub can and cannot see. This is the harness's central caveat, so it is
// pinned rather than left in prose: an identical restatement is 1.0, a
// one-token addition clears the 0.92 merge threshold, and a semantically
// equivalent REWORD does not. That last row is why reported dedupe counts are
// a lower bound on production behaviour.
func TestEmbedText_SimilarityBand(t *testing.T) {
	base := "The billing service runs on PostgreSQL 16"

	identical := cosine(EmbedText(base), EmbedText(base))
	if math.Abs(identical-1) > 1e-5 {
		t.Errorf("identical texts: cosine = %.4f, want 1.0", identical)
	}

	oneTokenAdded := cosine(EmbedText(base), EmbedText(base+" today"))
	if oneTokenAdded < 0.92 {
		t.Errorf("one added token: cosine = %.4f, want >= 0.92 (the dedupe threshold)", oneTokenAdded)
	}

	// A true reword shares few tokens, so a lexical embedder scores it low
	// where a semantic model would score it high. Asserted so the limitation
	// cannot quietly change without this test failing.
	reworded := cosine(EmbedText(base), EmbedText("Billing is backed by Postgres in production"))
	if reworded >= 0.92 {
		t.Errorf("reworded text: cosine = %.4f, unexpectedly >= 0.92 — "+
			"the documented lower-bound caveat may no longer hold", reworded)
	}
}
