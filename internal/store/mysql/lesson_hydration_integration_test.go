package mysql_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// Lesson evidence is hydrated for the whole result set in one batched query
// (previously one query per lesson, so listing cost grew with a corpus that
// only ever grows). Batching must not change WHAT comes back: every lesson
// keeps exactly its own evidence, and an evidence-free row still hydrates to an
// empty, non-nil slice.
func TestMySQL_ListLessons_BatchedEvidenceHydration(t *testing.T) {
	conn := withConn(t)
	ctx := context.Background()
	at := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)

	const lessons, evidencePer = 12, 2
	want := map[string][]string{}
	for i := range lessons {
		evidence := make([]string, 0, evidencePer)
		for j := range evidencePer {
			id := fmt.Sprintf("act-%d-%d", i, j)
			if err := conn.Actions.Append(ctx, domain.Action{
				ID: id, RunID: "run-1", Kind: domain.ActionKindDeploy,
				Subject: "billing", Actor: "ci", At: at,
			}); err != nil {
				t.Fatalf("append action: %v", err)
			}
			evidence = append(evidence, id)
		}
		lid := fmt.Sprintf("lesson-%02d", i)
		want[lid] = evidence
		if err := conn.Lessons.Append(ctx, domain.Lesson{
			ID: lid, Statement: fmt.Sprintf("lesson %d holds", i),
			Scope: domain.Scope{Service: "billing"}, Trigger: "deploy-fails",
			Kind: "operational", Confidence: 0.7, DerivedAt: at, Evidence: evidence,
		}); err != nil {
			t.Fatalf("append lesson: %v", err)
		}
	}

	for _, tc := range []struct {
		name string
		list func() ([]domain.Lesson, error)
	}{
		{"ListAll", func() ([]domain.Lesson, error) { return conn.Lessons.ListAll(ctx) }},
		{"ListByService", func() ([]domain.Lesson, error) { return conn.Lessons.ListByService(ctx, "billing") }},
		{"ListByTrigger", func() ([]domain.Lesson, error) { return conn.Lessons.ListByTrigger(ctx, "deploy-fails") }},
	} {
		got, err := tc.list()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != lessons {
			t.Fatalf("%s: got %d lessons, want %d", tc.name, len(got), lessons)
		}
		for _, l := range got {
			if l.Evidence == nil {
				t.Errorf("%s: lesson %s has nil Evidence", tc.name, l.ID)
				continue
			}
			if len(l.Evidence) != len(want[l.ID]) {
				t.Errorf("%s: lesson %s hydrated %v, want %v", tc.name, l.ID, l.Evidence, want[l.ID])
				continue
			}
			for i, aid := range l.Evidence {
				if aid != want[l.ID][i] {
					t.Errorf("%s: lesson %s evidence[%d] = %s, want %s", tc.name, l.ID, i, aid, want[l.ID][i])
				}
			}
		}
	}
}
