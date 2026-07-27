package store_test

import (
	"context"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// Test-vs-test contradiction resolution depends on ListByTestRequirementRef
// returning the test_result claims that share a requirement ref. That was dead
// on three of four backends for different reasons: memory never persisted
// TestRequirementRef (so the non-empty filter could never match), and
// postgres/mysql queried test_requirement_ref / test_last_run_at columns their
// schemas never declared (so the call failed with "undefined column" rather
// than returning rows). Every backend must now agree.
func TestClaims_TestProvenanceRoundTripsOnEveryBackend(t *testing.T) {
	backends := openBackends(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	for _, b := range backends {
		claims := []domain.Claim{
			{
				ID: "t-old", Text: "login test passed", Type: domain.ClaimTypeTestResult,
				Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: now,
				TestID: "TestLogin/old", TestRequirementRef: "REQ-42", TestAuthor: "ada",
				TestLastRunAt: now.Add(-48 * time.Hour), TestLastModified: now.Add(-96 * time.Hour),
				TestPassCount: 7,
			},
			{
				ID: "t-new", Text: "login test failed", Type: domain.ClaimTypeTestResult,
				Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: now,
				TestID: "TestLogin/new", TestRequirementRef: "REQ-42", TestAuthor: "grace",
				TestLastRunAt: now, TestFailCount: 3,
			},
			{
				ID: "t-other", Text: "logout test passed", Type: domain.ClaimTypeTestResult,
				Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: now,
				TestID: "TestLogout", TestRequirementRef: "REQ-99", TestLastRunAt: now,
			},
		}
		if err := b.conn.Claims.Upsert(ctx, claims); err != nil {
			t.Fatalf("%s: upsert: %v", b.name, err)
		}

		got, err := b.conn.Claims.ListByTestRequirementRef(ctx, "REQ-42")
		if err != nil {
			t.Fatalf("%s: list by test requirement ref: %v", b.name, err)
		}
		ids := make([]string, 0, len(got))
		for _, c := range got {
			ids = append(ids, c.ID)
		}
		if len(ids) != 2 || ids[0] != "t-new" || ids[1] != "t-old" {
			t.Fatalf("%s: got %v, want [t-new t-old] (freshest run first)", b.name, ids)
		}

		// Field-level round trip: the scorer and the "which test to trust"
		// surface read these, so an unpersisted counter silently changes verdicts.
		newest := got[0]
		if newest.TestID != "TestLogin/new" || newest.TestAuthor != "grace" {
			t.Errorf("%s: test identity lost: id=%q author=%q", b.name, newest.TestID, newest.TestAuthor)
		}
		if newest.TestFailCount != 3 {
			t.Errorf("%s: TestFailCount = %d, want 3", b.name, newest.TestFailCount)
		}
		if !newest.TestLastRunAt.Equal(now) {
			t.Errorf("%s: TestLastRunAt = %s, want %s", b.name, newest.TestLastRunAt, now)
		}
		oldest := got[1]
		if oldest.TestPassCount != 7 {
			t.Errorf("%s: TestPassCount = %d, want 7", b.name, oldest.TestPassCount)
		}
		if !oldest.TestLastModified.Equal(now.Add(-96 * time.Hour)) {
			t.Errorf("%s: TestLastModified = %s, want %s", b.name, oldest.TestLastModified, now.Add(-96*time.Hour))
		}

		// An empty ref matches nothing on every backend (short-circuited).
		empty, err := b.conn.Claims.ListByTestRequirementRef(ctx, "")
		if err != nil {
			t.Fatalf("%s: empty ref: %v", b.name, err)
		}
		if len(empty) != 0 {
			t.Errorf("%s: empty ref returned %d claims, want 0", b.name, len(empty))
		}
	}
}
