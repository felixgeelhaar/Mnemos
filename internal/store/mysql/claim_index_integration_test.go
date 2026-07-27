package mysql_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/store"
)

// The test-requirement lookup must be index-backed; unindexed it scans every
// claim row. Asserting on the catalogue rather than EXPLAIN keeps the test
// honest on a near-empty table, where the optimiser legitimately prefers a scan
// no matter what indexes exist.
func TestMySQL_ClaimTestRequirementIndexExists(t *testing.T) {
	conn := withConn(t)
	raw, ok := conn.Raw.(*sql.DB)
	if !ok {
		t.Fatalf("mysql Conn.Raw is %T, want *sql.DB", conn.Raw)
	}

	rows, err := raw.QueryContext(context.Background(), `
SELECT column_name FROM information_schema.statistics
WHERE table_schema = DATABASE() AND table_name = 'claims'
  AND index_name = 'idx_claims_test_requirement_ref'
ORDER BY seq_in_index`)
	if err != nil {
		t.Fatalf("read index catalogue: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(cols) != 2 || cols[0] != "test_requirement_ref" || cols[1] != "type" {
		t.Errorf("idx_claims_test_requirement_ref covers %v, want [test_requirement_ref type]", cols)
	}
}

// A database created by an older binary has neither the test-provenance columns
// nor the index. MySQL rejects ALTER TABLE ... ADD COLUMN IF NOT EXISTS
// outright, so for a long time those ALTERs were silently skipped and no
// pre-existing database ever gained a new column. Re-applying the schema over a
// legacy-shaped table must upgrade it in place.
func TestMySQL_ApplySchema_UpgradesLegacyClaimsTable(t *testing.T) {
	dsn := requireLiveDSN(t)
	ns := fmt.Sprintf("mnemos_test_%d", time.Now().UnixNano())
	full := dsn + "?namespace=" + ns
	if strings.Contains(dsn, "?") {
		full = dsn + "&namespace=" + ns
	}
	ctx := context.Background()
	conn, err := store.Open(ctx, full)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	raw, ok := conn.Raw.(*sql.DB)
	if !ok {
		t.Fatalf("mysql Conn.Raw is %T, want *sql.DB", conn.Raw)
	}
	t.Cleanup(func() {
		_, _ = raw.ExecContext(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", ns))
		_ = conn.Close()
	})

	// Rewind to a pre-test-provenance shape.
	if _, err := raw.ExecContext(ctx, `DROP INDEX idx_claims_test_requirement_ref ON claims`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	for _, col := range []string{
		"test_id", "test_requirement_ref", "test_author",
		"test_last_modified", "test_last_run_at", "test_pass_count", "test_fail_count",
	} {
		if _, err := raw.ExecContext(ctx, "ALTER TABLE claims DROP COLUMN "+col); err != nil {
			t.Fatalf("drop column %s: %v", col, err)
		}
	}

	// Re-opening the same namespace re-applies schema.sql over the legacy table.
	again, err := store.Open(ctx, full)
	if err != nil {
		t.Fatalf("re-open after rewinding the schema: %v", err)
	}
	defer func() { _ = again.Close() }()
	if _, err := again.Claims.ListByTestRequirementRef(ctx, "REQ-1"); err != nil {
		t.Fatalf("lookup after re-applying schema over a legacy table: %v", err)
	}

	var n int
	if err := raw.QueryRowContext(ctx, `
SELECT COUNT(*) FROM information_schema.statistics
WHERE table_schema = DATABASE() AND table_name = 'claims'
  AND index_name = 'idx_claims_test_requirement_ref'`).Scan(&n); err != nil {
		t.Fatalf("read index catalogue: %v", err)
	}
	if n == 0 {
		t.Error("idx_claims_test_requirement_ref was not recreated on the upgraded table")
	}
}
