package query

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// countingEmbedClient records how many times a recall asked the embedding
// provider for a vector. On a hosted embedder each call is a serialised
// network round-trip on the pre-prompt path, so the count is the metric.
type countingEmbedClient struct {
	mu    sync.Mutex
	calls int
	vec   []float32
	model string
	err   error
}

func (c *countingEmbedClient) Embed(_ context.Context, texts []string) ([][]float32, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = c.vec
	}
	return out, nil
}
func (c *countingEmbedClient) ModelID() string { return c.model }
func (c *countingEmbedClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestAnswer_EmbedsTheQuestionOnce pins the fix: a recall that consults the
// event ranker AND the claim re-ranker must embed the question once, not once
// per consumer.
func TestAnswer_EmbedsTheQuestionOnce(t *testing.T) {
	ctx := context.Background()
	repo := newMemVectorRepo()
	repo.put("e1", "event", []float32{1, 0, 0}, "m")
	repo.put("cl1", "claim", []float32{1, 0, 0}, "m")
	repo.put("cl2", "claim", []float32{0, 1, 0}, "m")

	now := time.Now().UTC()
	target := domain.Event{ID: "e1", Content: "payments latency spike from a slow query"}
	var listAll, byIDs int
	events := spyEventRepo{
		byID:         map[string]domain.Event{"e1": target},
		all:          []domain.Event{target},
		listAllCount: &listAll,
		byIDsCount:   &byIDs,
	}
	claims := fakeClaimRepo{claims: []domain.Claim{
		{ID: "cl1", Text: "payments latency is caused by a slow query",
			TrustScore: 0.9, Confidence: 0.9, CreatedAt: now, ValidFrom: now},
		{ID: "cl2", Text: "an unrelated belief about caching",
			TrustScore: 0.9, Confidence: 0.9, CreatedAt: now, ValidFrom: now},
	}}
	embedder := &countingEmbedClient{vec: []float32{1, 0, 0}, model: "m"}

	engine := NewEngine(events, claims, fakeRelationshipRepo{rels: map[string][]domain.Relationship{}}).
		WithEmbeddings(repo, embedder)

	if _, err := engine.Answer(ctx, "why is payments slow?"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if got := embedder.count(); got != 1 {
		t.Fatalf("embedded the question %d times, want 1", got)
	}
}

// TestQuestionVector_MemoisesFailure keeps a bounded recall from spending its
// budget rediscovering a broken embedder.
func TestQuestionVector_MemoisesFailure(t *testing.T) {
	boom := errors.New("provider down")
	embedder := &countingEmbedClient{err: boom, model: "m"}
	e := Engine{embedClient: embedder}
	ctx := withQueryVectorMemo(context.Background())

	for i := 0; i < 3; i++ {
		if _, err := e.questionVector(ctx, "q"); !errors.Is(err, boom) {
			t.Fatalf("call %d: got %v, want %v", i, err, boom)
		}
	}
	if got := embedder.count(); got != 1 {
		t.Fatalf("retried a failed embed %d times, want 1 attempt", got)
	}
}

// TestQuestionVector_WorksWithoutMemo proves nothing depends on the wrapper:
// an engine method called outside an Answer entry point still embeds normally.
func TestQuestionVector_WorksWithoutMemo(t *testing.T) {
	embedder := &countingEmbedClient{vec: []float32{1, 2, 3}, model: "m"}
	e := Engine{embedClient: embedder}

	got, err := e.questionVector(context.Background(), "q")
	if err != nil {
		t.Fatalf("questionVector: %v", err)
	}
	if len(got) != 3 || got[0] != 1 {
		t.Fatalf("got %v, want [1 2 3]", got)
	}
	if embedder.count() != 1 {
		t.Fatalf("embedded %d times, want 1", embedder.count())
	}
}

// TestQuestionVector_DistinctQuestionsAreNotConflated guards the memo key: two
// different questions in one context must not share a vector.
func TestQuestionVector_DistinctQuestionsAreNotConflated(t *testing.T) {
	e := Engine{embedClient: perTextEmbedClient{}}
	ctx := withQueryVectorMemo(context.Background())

	a, err := e.questionVector(ctx, "alpha")
	if err != nil {
		t.Fatalf("alpha: %v", err)
	}
	b, err := e.questionVector(ctx, "beta")
	if err != nil {
		t.Fatalf("beta: %v", err)
	}
	if a[0] == b[0] {
		t.Fatalf("distinct questions shared a vector: %v vs %v", a, b)
	}
}

// perTextEmbedClient derives the vector from the text, so conflating two
// questions is observable.
type perTextEmbedClient struct{}

func (perTextEmbedClient) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = []float32{float32(len(t)), 1, 0}
	}
	return out, nil
}

// TestWithQueryVectorMemo_NestedReusesOuter keeps a corrective pass on the
// first pass's vector rather than re-embedding.
func TestWithQueryVectorMemo_NestedReusesOuter(t *testing.T) {
	outer := withQueryVectorMemo(context.Background())
	inner := withQueryVectorMemo(outer)
	if outer.Value(queryVectorKey{}) != inner.Value(queryVectorKey{}) {
		t.Fatal("nested memo replaced the outer one; a corrective pass would re-embed")
	}
}
