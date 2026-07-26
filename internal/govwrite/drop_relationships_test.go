package govwrite_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/govwrite"
)

// rel builds a relationship edge with a deterministic id.
func rel(id, typ, from, to string) domain.Relationship {
	return domain.Relationship{
		ID: id, Type: domain.RelationshipType(typ), FromClaimID: from, ToClaimID: to,
		CreatedAt: time.Now().UTC(), CreatedBy: "tester",
	}
}

// edgeSet returns the stored edges keyed by content (type|from|to).
func edgeSet(t *testing.T, w *govwrite.Writer) map[string]bool {
	t.Helper()
	all, err := w.Conn().Relationships.ListAll(context.Background())
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	out := make(map[string]bool, len(all))
	for _, r := range all {
		out[string(r.Type)+"|"+r.FromClaimID+"|"+r.ToClaimID] = true
	}
	return out
}

// TestDropRelationships_KeepsEdgesWrittenAfterTheSnapshot is the N4 regression.
//
// It reproduces the interleaving, not just the happy path: a pruner reads the
// relationship set, and while it is re-deriving each edge a concurrent writer
// inserts a brand-new one. The pruner then commits its decision.
//
// The old, keep-based write said "the stored set is exactly what I decided to
// retain", which deletes the concurrent edge as collateral — it was absent from
// a snapshot taken before it existed and was never evaluated. The drop-based
// write enumerates only the edges the pruner actually judged stale, so the new
// edge survives by construction.
func TestDropRelationships_KeepsEdgesWrittenAfterTheSnapshot(t *testing.T) {
	t.Parallel()
	w := newWriter(t)
	ctx := context.Background()

	// The pre-existing set: one edge the pruner will judge stale, one it keeps.
	if _, err := w.Relationships(ctx, []domain.Relationship{
		rel("r_stale", "contradicts", "c1", "c2"),
		rel("r_live", "supports", "c3", "c4"),
	}); err != nil {
		t.Fatalf("seed relationships: %v", err)
	}

	// --- The pruner's snapshot. Everything it decides is derived from this. ---
	snapshot, err := w.Conn().Relationships.ListAll(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var stale []domain.Relationship
	for _, r := range snapshot {
		if r.Type == domain.RelationshipTypeContradicts {
			stale = append(stale, r)
		}
	}
	if len(stale) != 1 {
		t.Fatalf("fixture: expected 1 stale edge in the snapshot, got %d", len(stale))
	}

	// --- A concurrent writer lands a new edge AFTER the snapshot. ---
	if _, err := w.Relationships(ctx, []domain.Relationship{
		rel("r_concurrent", "supports", "c5", "c6"),
	}); err != nil {
		t.Fatalf("concurrent insert: %v", err)
	}

	// --- The pruner commits its decision. ---
	if _, err := w.DropRelationships(ctx, stale); err != nil {
		t.Fatalf("DropRelationships: %v", err)
	}

	got := edgeSet(t, w)
	if got["contradicts|c1|c2"] {
		t.Error("the stale edge the pruner judged should have been dropped")
	}
	if !got["supports|c3|c4"] {
		t.Error("an edge the pruner judged live was deleted")
	}
	if !got["supports|c5|c6"] {
		t.Fatal("the concurrently-inserted edge was deleted as stale — the pruner outran its own snapshot")
	}
}

// TestDropRelationships_ConcurrentWriterDuringPrune runs the same race with a
// real second goroutine hammering inserts while the drop executes, under -race.
// Every edge the writer reports as committed must still be stored afterwards:
// none of them was ever in the pruner's drop list, so none may be collateral.
func TestDropRelationships_ConcurrentWriterDuringPrune(t *testing.T) {
	t.Parallel()
	w := newWriter(t)
	ctx := context.Background()

	// A sizeable pre-existing set so the drop has real work to do.
	seed := make([]domain.Relationship, 0, 200)
	for i := range 200 {
		seed = append(seed, rel(fmt.Sprintf("r_seed_%d", i), "contradicts",
			fmt.Sprintf("s%d", i), fmt.Sprintf("t%d", i)))
	}
	if _, err := w.Relationships(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The pruner's snapshot: it decides to drop half of them.
	snapshot, err := w.Conn().Relationships.ListAll(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	drop := make([]domain.Relationship, 0, len(snapshot)/2)
	for i, r := range snapshot {
		if i%2 == 0 {
			drop = append(drop, r)
		}
	}

	var (
		mu        sync.Mutex
		committed []domain.Relationship
		wg        sync.WaitGroup
	)
	start := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := range 50 {
			r := rel(fmt.Sprintf("r_new_%d", i), "supports",
				fmt.Sprintf("n%d", i), fmt.Sprintf("m%d", i))
			if _, err := w.Relationships(ctx, []domain.Relationship{r}); err != nil {
				return
			}
			mu.Lock()
			committed = append(committed, r)
			mu.Unlock()
		}
	}()

	close(start)
	if _, err := w.DropRelationships(ctx, drop); err != nil {
		t.Fatalf("DropRelationships: %v", err)
	}
	wg.Wait()

	got := edgeSet(t, w)
	mu.Lock()
	defer mu.Unlock()
	for _, r := range committed {
		key := string(r.Type) + "|" + r.FromClaimID + "|" + r.ToClaimID
		if !got[key] {
			t.Errorf("concurrently-committed edge %s was deleted by the prune", key)
		}
	}
	for _, r := range drop {
		key := string(r.Type) + "|" + r.FromClaimID + "|" + r.ToClaimID
		if got[key] {
			t.Errorf("edge %s was in the drop list but survived", key)
		}
	}
}

// TestDropRelationships_IsIdempotent pins that re-running a drop whose edges are
// already gone leaves the table alone rather than rewriting it.
func TestDropRelationships_IsIdempotent(t *testing.T) {
	t.Parallel()
	w := newWriter(t)
	ctx := context.Background()
	if _, err := w.Relationships(ctx, []domain.Relationship{
		rel("r_a", "contradicts", "a1", "a2"),
		rel("r_b", "supports", "b1", "b2"),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	drop := []domain.Relationship{rel("r_a", "contradicts", "a1", "a2")}

	if _, err := w.DropRelationships(ctx, drop); err != nil {
		t.Fatalf("first drop: %v", err)
	}
	if _, err := w.DropRelationships(ctx, drop); err != nil {
		t.Fatalf("second drop: %v", err)
	}
	got := edgeSet(t, w)
	if got["contradicts|a1|a2"] {
		t.Error("dropped edge came back")
	}
	if !got["supports|b1|b2"] {
		t.Error("re-running the drop removed an unrelated edge")
	}
}
