package main

import (
	"context"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// fakeEmbedder records the batches it was asked to embed. `short` makes it
// return one fewer vector than requested, imitating a provider that silently
// honours only part of a batch.
type fakeEmbedder struct {
	batches [][]string
	short   bool
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.batches = append(f.batches, append([]string(nil), texts...))

	n := len(texts)
	if f.short && n > 0 {
		n--
	}
	out := make([][]float32, n)
	for i := range out {
		out[i] = []float32{1, 0, 0}
	}

	return out, nil
}

// TestReembedEntities_ChunksIntoBatches pins the batching. reembed used to send
// the entire corpus as one provider call, which is fine against OpenAI and
// fails against a self-hosted TEI/Infinity sidecar — the provider you switch to
// precisely when you need reembed to work.
func TestReembedEntities_ChunksIntoBatches(t *testing.T) {
	conn := newServerTestStore_conn(t)
	w := wrapTestWriter(t, conn)
	ctx := context.Background()

	target := reembedTarget{entityType: "claim", label: "claim"}
	target.text = map[string]string{}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		target.ids = append(target.ids, id)
		target.text[id] = "text " + id
	}

	fake := &fakeEmbedder{}
	n, err := reembedEntities(ctx, w, fake, "m", target, 2)
	if err != nil {
		t.Fatalf("reembedEntities: %v", err)
	}
	if n != 5 {
		t.Errorf("stored = %d, want 5", n)
	}

	wantSizes := []int{2, 2, 1}
	if len(fake.batches) != len(wantSizes) {
		t.Fatalf("got %d batches, want %d (%v)", len(fake.batches), len(wantSizes), fake.batches)
	}
	for i, want := range wantSizes {
		if got := len(fake.batches[i]); got != want {
			t.Errorf("batch %d size = %d, want %d", i, got, want)
		}
	}
}

// TestReembedEntities_RefusesShortBatch is the guard against the failure
// internal/pipeline calls out and reembed did not have: the old loop did
// `if i >= len(vectors) { break }` and then reported len(vectors) as its
// success count, so a provider returning a short batch left rows unembedded
// while the command printed a success line and exited 0.
//
// Silence is the whole problem — an unembedded claim is invisible to
// model-filtered recall, so it looks exactly like a claim that simply does not
// match. Erroring is the only way the operator finds out.
func TestReembedEntities_RefusesShortBatch(t *testing.T) {
	conn := newServerTestStore_conn(t)
	w := wrapTestWriter(t, conn)
	ctx := context.Background()

	target := reembedTarget{
		entityType: "claim",
		label:      "claim",
		ids:        []string{"a", "b", "c"},
		text:       map[string]string{"a": "A", "b": "B", "c": "C"},
	}

	_, err := reembedEntities(ctx, w, &fakeEmbedder{short: true}, "m", target, 3)
	if err == nil {
		t.Fatal("a short batch must be an error, not a partially-stored success")
	}
}

// TestCollectReembedTargets_IncludesEvents is the regression test for the gap
// that motivated this change: reembed hardcoded entity type "claim", and event
// vectors are written only by the ingest pipeline. Changing the embedding model
// therefore stranded every event embedding in the old space with no way to
// migrate it — and because recall is model-filtered, those rows went silently
// dark rather than erroring.
func TestCollectReembedTargets_IncludesEvents(t *testing.T) {
	conn := newServerTestStore_conn(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := conn.Events.Append(ctx, domain.Event{
		ID: "ev1", Content: "an event body", SourceInputID: "src1", Timestamp: now, IngestedAt: now,
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	if err := conn.Claims.Upsert(ctx, []domain.Claim{{
		ID: "cl1", Text: "a claim", Type: domain.ClaimTypeFact,
		Confidence: 0.8, Status: domain.ClaimStatusActive, CreatedAt: now, ValidFrom: now,
	}}); err != nil {
		t.Fatalf("upsert claim: %v", err)
	}

	// --force: everything, both types.
	targets, err := collectReembedTargets(ctx, conn, true)
	if err != nil {
		t.Fatalf("collectReembedTargets(force): %v", err)
	}
	byType := indexTargets(t, targets)
	if got := byType["claim"]; len(got.ids) != 1 || got.ids[0] != "cl1" {
		t.Errorf("force: claim ids = %v, want [cl1]", got.ids)
	}
	if got := byType["event"]; len(got.ids) != 1 || got.ids[0] != "ev1" {
		t.Errorf("force: event ids = %v, want [ev1] — events must be re-embedded too", got.ids)
	}
	if got := byType["event"].text["ev1"]; got != "an event body" {
		t.Errorf("event text = %q, want the event Content", got)
	}

	// Without --force, an already-embedded event must not be picked up again.
	if err := conn.Embeddings.Upsert(ctx, "ev1", "event", []float32{1, 0, 0}, "m", ""); err != nil {
		t.Fatalf("seed event embedding: %v", err)
	}
	targets, err = collectReembedTargets(ctx, conn, false)
	if err != nil {
		t.Fatalf("collectReembedTargets(missing-only): %v", err)
	}
	if got := indexTargets(t, targets)["event"]; len(got.ids) != 0 {
		t.Errorf("missing-only: event ids = %v, want none (ev1 is already embedded)", got.ids)
	}
}

func indexTargets(t *testing.T, targets []reembedTarget) map[string]reembedTarget {
	t.Helper()

	out := make(map[string]reembedTarget, len(targets))
	for _, tg := range targets {
		out[tg.entityType] = tg
	}
	for _, want := range []string{"claim", "event"} {
		if _, ok := out[want]; !ok {
			t.Fatalf("no %q target returned; got %+v", want, targets)
		}
	}

	return out
}
