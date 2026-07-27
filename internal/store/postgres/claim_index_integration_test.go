package postgres_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// The test-requirement lookup must be index-backed. Unindexed it seq-scans
// every claim, so resolving one requirement's conflicting tests costs more with
// every unrelated belief the brain learns. Asserting on the catalogue rather
// than on EXPLAIN keeps the test honest on an empty table, where the planner
// legitimately prefers a seq scan regardless of what indexes exist.
func TestPostgres_ClaimTestRequirementIndexExists(t *testing.T) {
	conn := withConn(t)
	raw, ok := conn.Raw.(*sql.DB)
	if !ok {
		t.Fatalf("postgres Conn.Raw is %T, want *sql.DB", conn.Raw)
	}

	var def string
	err := raw.QueryRowContext(context.Background(),
		`SELECT indexdef FROM pg_indexes WHERE tablename = 'claims' AND indexname = 'idx_claims_test_requirement_ref'`,
	).Scan(&def)
	if err != nil {
		t.Fatalf("idx_claims_test_requirement_ref missing from the claims table: %v", err)
	}
	for _, col := range []string{"test_requirement_ref", "type"} {
		if !strings.Contains(def, col) {
			t.Errorf("index does not cover %s: %s", col, def)
		}
	}
	// A partial index on test_requirement_ref <> '' would be skipped under a
	// generic plan (a bound parameter does not imply the predicate), which is
	// the one case that matters.
	if strings.Contains(strings.ToUpper(def), " WHERE ") {
		t.Errorf("index is partial and will not be used for a parameterised lookup: %s", def)
	}
}
