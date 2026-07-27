package query

import (
	"context"
	"errors"
	"sync"
)

// errNoQueryVector reports an embedder that returned successfully but produced
// no vector for the question. Every caller treats it exactly like an embed
// failure: no dense signal, fall back.
var errNoQueryVector = errors.New("query: embedder returned no vector for the question")

// One recall needs the question's embedding in up to three places: the native
// vector fast-path (eventsByHybrid), the whole-corpus event ranker
// (cosineEventScores) and the claim re-ranker (cosineClaimScores). Each used to
// call Embed for itself, so a single prompt cost two or three embedding
// round-trips — and with the corrective-retrieval gate, up to six. Against a
// local model that is wasted CPU; against a hosted embedder it is wasted
// NETWORK, serialised, on the path that runs before every Claude Code prompt.
//
// The memo below computes it once per recall. It hangs off the context rather
// than the Engine because Engine is a value copied by every With* option and is
// shared across concurrent queries, while a context is exactly one call.
//
// Absent the memo (an engine method invoked outside an Answer entry point)
// questionVector simply embeds, so nothing depends on the wrapper being there.

type queryVectorKey struct{}

type queryVectorMemo struct {
	mu   sync.Mutex
	vecs map[string][]float32
	errs map[string]error
}

// withQueryVectorMemo installs a per-recall embedding memo. Nested calls reuse
// the outer memo, so a corrective pass shares the first pass's vector.
func withQueryVectorMemo(ctx context.Context) context.Context {
	if _, ok := ctx.Value(queryVectorKey{}).(*queryVectorMemo); ok {
		return ctx
	}
	return context.WithValue(ctx, queryVectorKey{}, &queryVectorMemo{
		vecs: map[string][]float32{},
		errs: map[string]error{},
	})
}

// questionVector returns the embedding of question, computing it at most once
// per recall. The returned slice is shared by every caller within the recall
// and must be treated as read-only.
//
// A failure is memoised too: an embedder that just failed will fail again, and
// a bounded recall should not spend the rest of its budget rediscovering that.
func (e Engine) questionVector(ctx context.Context, question string) ([]float32, error) {
	memo, ok := ctx.Value(queryVectorKey{}).(*queryVectorMemo)
	if !ok {
		return e.embedQuestion(ctx, question)
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if vec, hit := memo.vecs[question]; hit {
		return vec, nil
	}
	if err, hit := memo.errs[question]; hit {
		return nil, err
	}
	vec, err := e.embedQuestion(ctx, question)
	if err != nil {
		memo.errs[question] = err
		return nil, err
	}
	memo.vecs[question] = vec
	return vec, nil
}

// embedQuestion is the uncached call. It normalises "provider returned no
// vectors" into an error so callers have one failure shape to handle.
func (e Engine) embedQuestion(ctx context.Context, question string) ([]float32, error) {
	vectors, err := e.embedClient.Embed(ctx, []string{question})
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		return nil, errNoQueryVector
	}
	return vectors[0], nil
}
