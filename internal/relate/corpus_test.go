package relate

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// This file builds deterministic synthetic corpora for the incremental-detection
// benchmarks and the full-scan/index equivalence tests.
//
// The point of the generator is to reproduce the property that actually drives
// DetectIncremental's cost: the token-frequency distribution. Real claim text is
// Zipfian — a handful of tokens ("mnemos", "test", "claim") appear in a large
// fraction of claims while the long tail appears in one or two. A uniform-random
// vocabulary would understate the cost of a token-index prefilter (every posting
// list tiny) and a single-token vocabulary would overstate it. Both would make
// the measurement a lie, so the generator samples from a Zipf distribution and
// the benchmark reports the resulting document-frequency profile alongside the
// timings.

// benchVocabulary returns a deterministic vocabulary of pseudo-words. The words
// are not English, which does not matter: the detectors key off token identity,
// stop-word membership, and shape, and none of the generated words are stop
// words.
func benchVocabulary(n int) []string {
	words := make([]string, 0, n)
	// Three syllable tables give 20*20*20 = 8000 distinct three-syllable words
	// before the numeric suffix kicks in, which is plenty of headroom.
	a := []string{"ba", "ce", "di", "fo", "gu", "ha", "je", "ki", "lo", "mu",
		"na", "pe", "ri", "so", "tu", "va", "we", "xi", "yo", "zu"}
	b := []string{"bra", "cle", "dri", "flo", "gru", "hal", "jen", "kir", "lom", "mun",
		"nar", "pel", "ris", "sol", "tun", "val", "wen", "xir", "yol", "zun"}
	c := []string{"ax", "ex", "ix", "ox", "ux", "an", "en", "in", "on", "un",
		"ar", "er", "ir", "or", "ur", "al", "el", "il", "ol", "ul"}
	for i := 0; len(words) < n; i++ {
		w := a[i%len(a)] + b[(i/len(a))%len(b)] + c[(i/(len(a)*len(b)))%len(c)]
		if i >= len(a)*len(b)*len(c) {
			w = fmt.Sprintf("%s%d", w, i)
		}
		words = append(words, w)
	}
	return words
}

// zipfIndex maps a uniform sample u in [0,1) onto a rank in [0,n) following a
// Zipf-like law with exponent s. Implemented by inverting the normalized
// harmonic CDF numerically once and then binary-searching, which keeps corpus
// generation linear in the number of tokens drawn.
type zipfIndex struct {
	cdf []float64
}

// newZipfIndex builds the CDF over ranks [offset+1, offset+n]. The offset
// matters: a plain Zipf over rank 1..n puts ~11% of every token draw on a single
// word, which after 10 draws per claim means the head token appears in ~two
// thirds of the corpus. Real text does look like that — but the head is occupied
// by stop words, which contentTokensAndPolarity strips before any of this code
// sees them. Sampling content words from the *tail* of the Zipf curve reproduces
// the distribution the detector actually faces.
func newZipfIndex(n int, s float64, offset int) *zipfIndex {
	cdf := make([]float64, n)
	total := 0.0
	for i := 0; i < n; i++ {
		total += 1.0 / math.Pow(float64(i+1+offset), s)
		cdf[i] = total
	}
	for i := range cdf {
		cdf[i] /= total
	}
	return &zipfIndex{cdf: cdf}
}

func (z *zipfIndex) pick(u float64) int {
	lo, hi := 0, len(z.cdf)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if z.cdf[mid] < u {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// corpusOptions controls the shape of a generated corpus.
type corpusOptions struct {
	Size       int     // number of claims
	VocabSize  int     // distinct content words available
	Zipf       float64 // Zipf exponent; ~1.0 is English-like
	RankOffset int     // ranks the stop words would have occupied (see newZipfIndex)
	Seed       int64
	MinTokens  int
	MaxTokens  int
	ShortShare float64 // fraction of claims that are very short (<= 3 tokens)
	NumShare   float64 // fraction of claims carrying a numeric literal
	NegShare   float64 // fraction of claims carrying a negation
	AspectShar float64 // fraction of claims carrying a temporal aspect marker
	ProperShar float64 // fraction of claims carrying a proper-noun-shaped token
}

func defaultCorpusOptions(size int, seed int64) corpusOptions {
	return corpusOptions{
		Size:       size,
		VocabSize:  4000,
		Zipf:       1.0,
		RankOffset: 120,
		Seed:       seed,
		MinTokens:  5,
		MaxTokens:  18,
		ShortShare: 0.10,
		NumShare:   0.20,
		NegShare:   0.12,
		AspectShar: 0.15,
		ProperShar: 0.10,
	}
}

var (
	benchAspectWords = []string{"completed", "still running", "planned", "never", "finished", "pending"}
	benchProperNames = []string{"Alice", "Bob", "Carol", "Dave", "Erin", "Frank"}
	benchNegWords    = []string{"not", "no", "never"}
	benchFiller      = []string{"the", "is", "of", "in", "for", "with", "and", "a"}
)

// generateCorpus builds a deterministic slice of claims.
func generateCorpus(opts corpusOptions) []domain.Claim {
	rng := rand.New(rand.NewSource(opts.Seed)) //nolint:gosec // deterministic test fixture, not security
	vocab := benchVocabulary(opts.VocabSize)
	z := newZipfIndex(opts.VocabSize, opts.Zipf, opts.RankOffset)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	claims := make([]domain.Claim, 0, opts.Size)
	for i := 0; i < opts.Size; i++ {
		n := opts.MinTokens + rng.Intn(opts.MaxTokens-opts.MinTokens+1)
		if rng.Float64() < opts.ShortShare {
			n = 2 + rng.Intn(2)
		}
		parts := make([]string, 0, n*2)
		if rng.Float64() < opts.ProperShar {
			parts = append(parts, benchProperNames[rng.Intn(len(benchProperNames))], "is")
		}
		for k := 0; k < n; k++ {
			if k > 0 && rng.Float64() < 0.35 {
				parts = append(parts, benchFiller[rng.Intn(len(benchFiller))])
			}
			parts = append(parts, vocab[z.pick(rng.Float64())])
		}
		if rng.Float64() < opts.NegShare {
			parts = append(parts, benchNegWords[rng.Intn(len(benchNegWords))], vocab[z.pick(rng.Float64())])
		}
		if rng.Float64() < opts.NumShare {
			parts = append(parts, fmt.Sprintf("%d", rng.Intn(500)), []string{"ms", "%", "MB", ""}[rng.Intn(4)])
		}
		if rng.Float64() < opts.AspectShar {
			parts = append(parts, benchAspectWords[rng.Intn(len(benchAspectWords))])
		}
		text := strings.TrimSpace(strings.Join(parts, " "))
		claims = append(claims, domain.Claim{
			ID:        fmt.Sprintf("cl_%07d", i),
			Text:      text,
			Type:      domain.ClaimTypeFact,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
			ValidFrom: base.Add(time.Duration(i) * time.Minute),
		})
	}
	return claims
}

// corpusTokenProfile reports the document-frequency profile of a corpus, so a
// benchmark can state the selectivity its numbers were measured under instead of
// asserting it.
type corpusTokenProfile struct {
	Claims        int
	DistinctToks  int
	TotalPostings int
	MaxDF         int
	P99DF         int
	MedianDF      int
	AvgTokens     float64
}

func profileCorpus(claims []domain.Claim) corpusTokenProfile {
	df := map[string]int{}
	total := 0
	for i := range claims {
		toks, _ := contentTokensAndPolarity(claims[i].Text)
		total += len(toks)
		for t := range toks {
			df[t]++
		}
	}
	counts := make([]int, 0, len(df))
	for _, c := range df {
		counts = append(counts, c)
	}
	sort.Ints(counts)
	p := corpusTokenProfile{
		Claims:        len(claims),
		DistinctToks:  len(df),
		TotalPostings: total,
	}
	if len(counts) > 0 {
		p.MaxDF = counts[len(counts)-1]
		p.P99DF = counts[int(float64(len(counts))*0.99)]
		p.MedianDF = counts[len(counts)/2]
	}
	if len(claims) > 0 {
		p.AvgTokens = float64(total) / float64(len(claims))
	}
	return p
}
