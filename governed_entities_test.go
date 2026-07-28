package mnemos

import (
	"context"
	"testing"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/extract"

	_ "go.klarlabs.de/mnemos/internal/store/memory"
)

// The regression: the governed remember executor discarded the extractor's
// entity map into `_` and never called MaterializeEntities, while the CLI's
// extract/process commands did. That path is what MCP process_text — and
// therefore every Claude Code capture hook — goes through, so entity extraction
// ran on every captured session and persisted nothing. Measured on a real
// brain: 0 entity rows against 86,190 claims.
//
// Stubbing the extractor keeps this deterministic: it asserts the executor
// PERSISTS whatever entities it is handed, independent of whether a given model
// happens to emit any.
func TestRememberExecutor_PersistsExtractedEntities(t *testing.T) {
	mem, err := New(WithStorage("memory://entities-test"), WithActor("tester"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := mem.(*memory)
	if !ok {
		t.Fatalf("New returned %T, want *memory", mem)
	}

	m.extractor.ExtractFn = func(_ context.Context, events []domain.Event) ([]domain.Claim, []domain.ClaimEvidence, map[string][]extract.ExtractedEntity, error) {
		claim := domain.Claim{ID: "cl_test_entities", Text: "Felix deployed warden to the klarlabs tap", Type: domain.ClaimTypeFact, Confidence: 0.9, Status: domain.ClaimStatusActive}
		var ev []domain.ClaimEvidence
		if len(events) > 0 {
			ev = append(ev, domain.ClaimEvidence{ClaimID: claim.ID, EventID: events[0].ID})
		}
		return []domain.Claim{claim}, ev, map[string][]extract.ExtractedEntity{
			claim.ID: {
				{Name: "Felix", Type: "person", Role: "subject"},
				{Name: "warden", Type: "product", Role: "object"},
			},
		}, nil
	}

	ctx := context.Background()
	if err := mem.Remember(ctx, Item{Content: "Felix deployed warden to the klarlabs tap."}); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	ents, err := m.conn.Entities.List(ctx)
	if err != nil {
		t.Fatalf("list entities: %v", err)
	}
	if len(ents) == 0 {
		t.Fatal("the governed write path dropped the extractor's entities — none were persisted")
	}

	byName := map[string]bool{}
	for _, e := range ents {
		byName[e.Name] = true
	}
	for _, want := range []string{"Felix", "warden"} {
		if !byName[want] {
			t.Errorf("entity %q was not materialised (got %v)", want, byName)
		}
	}
}
