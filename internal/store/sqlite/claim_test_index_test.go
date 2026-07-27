package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// The test-vs-test contradiction lookup must be index-backed. Unindexed it
// scans every claim, so resolving one requirement's conflicting tests costs
// more with every unrelated belief the brain learns.
func TestClaims_TestRequirementRefLookup_UsesIndex(t *testing.T) {
	db := openTestDB(t)
	defer closeDB(db)

	rows, err := db.Query(
		`EXPLAIN QUERY PLAN SELECT id FROM claims WHERE type = 'test_result' AND test_requirement_ref = ?`,
		"REQ-1",
	)
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer closeRows(rows)
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}
	got := plan.String()
	if !strings.Contains(got, "idx_claims_test_requirement_ref") {
		t.Errorf("query plan does not use idx_claims_test_requirement_ref:\n%s", got)
	}
	if strings.Contains(got, "SCAN claims") && !strings.Contains(got, "USING INDEX") {
		t.Errorf("query plan still full-scans claims:\n%s", got)
	}
}

// A brain created by an earlier binary sits at user_version 23. Adding an index
// without bumping currentSchemaVersion would silently skip it forever, because
// migrate() returns early once user_version has caught up.
func TestMigrate_AddsTestRequirementIndexToExistingSchema(t *testing.T) {
	db := openTestDB(t)
	defer closeDB(db)

	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_claims_test_requirement_ref`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 23`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var name string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_claims_test_requirement_ref'`,
	).Scan(&name); err != nil {
		t.Fatalf("index missing after migrating a v23 database: %v", err)
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != currentSchemaVersion {
		t.Errorf("user_version = %d, want %d", version, currentSchemaVersion)
	}
}

// End-to-end: the indexed lookup still returns the right rows in the right
// order (freshest run first).
func TestClaims_ListByTestRequirementRef_OrdersByLastRun(t *testing.T) {
	db := openTestDB(t)
	defer closeDB(db)
	ctx := context.Background()
	repo := NewClaimRepository(db)
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	if err := repo.Upsert(ctx, []domain.Claim{
		{
			ID: "t-old", Text: "login test passed", Type: domain.ClaimTypeTestResult,
			Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: now,
			TestID: "TestLogin/old", TestRequirementRef: "REQ-42",
			TestLastRunAt: now.Add(-48 * time.Hour), TestPassCount: 3,
		},
		{
			ID: "t-new", Text: "login test failed", Type: domain.ClaimTypeTestResult,
			Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: now,
			TestID: "TestLogin/new", TestRequirementRef: "REQ-42",
			TestLastRunAt: now, TestFailCount: 2,
		},
		{
			ID: "t-other", Text: "logout test passed", Type: domain.ClaimTypeTestResult,
			Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: now,
			TestID: "TestLogout", TestRequirementRef: "REQ-99", TestLastRunAt: now,
		},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := repo.ListByTestRequirementRef(ctx, "REQ-42")
	if err != nil {
		t.Fatalf("list by test requirement ref: %v", err)
	}
	if len(got) != 2 || got[0].ID != "t-new" || got[1].ID != "t-old" {
		t.Fatalf("got %v, want [t-new t-old]", ids(got))
	}
}

func ids(claims []domain.Claim) []string {
	out := make([]string, 0, len(claims))
	for _, c := range claims {
		out = append(out, c.ID)
	}
	return out
}
