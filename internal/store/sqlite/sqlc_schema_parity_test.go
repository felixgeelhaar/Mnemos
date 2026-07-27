package sqlite

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// referenceSchemaPath is the sqlc codegen reference. It is NEVER executed
// against a live brain — the runtime schema is the CREATE statements in db.go
// — which is exactly why it can drift without anything failing.
const referenceSchemaPath = "../../../sql/sqlite/schema.sql"

// applyReferenceSchema runs sql/sqlite/schema.sql verbatim against a fresh
// database and returns the handle, so SQLite itself (not a regexp over the
// file) tells us what the reference schema declares.
func applyReferenceSchema(t *testing.T) *sql.DB {
	t.Helper()
	raw, err := os.ReadFile(referenceSchemaPath)
	if err != nil {
		t.Fatalf("read %s: %v", referenceSchemaPath, err)
	}
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "reference.db"))
	if err != nil {
		t.Fatalf("open reference db: %v", err)
	}
	t.Cleanup(func() { closeDB(db) })
	if _, err := db.Exec(string(raw)); err != nil {
		t.Fatalf("apply reference schema: %v", err)
	}
	return db
}

// indexDefs returns name → normalised CREATE INDEX text for every index on the
// given table that carries its own SQL (auto-indexes from UNIQUE constraints
// have a NULL sql and are skipped — they are declared by the table, not here).
func indexDefs(t *testing.T, db *sql.DB, table string) map[string]string {
	t.Helper()
	rows, err := db.Query(
		`SELECT name, sql FROM sqlite_master WHERE type='index' AND tbl_name = ? AND sql IS NOT NULL`,
		table,
	)
	if err != nil {
		t.Fatalf("read sqlite_master indexes for %s: %v", table, err)
	}
	defer closeRows(rows)

	out := map[string]string{}
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			t.Fatalf("scan index row: %v", err)
		}
		out[name] = normaliseDDL(ddl)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate index rows: %v", err)
	}
	return out
}

// normaliseDDL collapses whitespace and the IF NOT EXISTS noise so two
// definitions that differ only in formatting compare equal.
func normaliseDDL(ddl string) string {
	s := strings.Join(strings.Fields(ddl), " ")
	s = strings.ReplaceAll(s, "CREATE INDEX IF NOT EXISTS ", "CREATE INDEX ")
	return s
}

// sql/sqlite/schema.sql is the sqlc reference and never runs against a real
// database, so a divergence from db.go is silent by construction: the file
// reads as the schema of record while describing an index no brain has.
//
// It said idx_claims_test_requirement_ref was PARTIAL — ON
// claims(test_requirement_ref) WHERE test_requirement_ref != ” — long after
// db.go had settled on the non-partial (test_requirement_ref, type) form,
// deliberately, because SQLite will not choose a partial index for a query
// whose bound parameter could itself be ”. Anyone reading the reference to
// reason about the test-provenance lookup would have concluded the opposite of
// what runs.
//
// Every index the reference declares on claims must therefore exist at runtime
// with an identical definition. Runtime-only indexes are allowed (the
// reference lags on tables sqlc has no queries for); a reference index that
// LIES about runtime is not.
func TestReferenceSchema_ClaimsIndexesMatchRuntime(t *testing.T) {
	reference := applyReferenceSchema(t)
	runtime := openTestDB(t)
	defer closeDB(runtime)

	want := indexDefs(t, reference, "claims")
	got := indexDefs(t, runtime, "claims")
	if len(want) == 0 {
		t.Fatalf("no indexes on claims found in %s — the parity check would pass vacuously", referenceSchemaPath)
	}

	for name, wantDDL := range want {
		gotDDL, ok := got[name]
		if !ok {
			t.Errorf("%s declares index %s on claims, but the runtime schema in db.go never creates it",
				referenceSchemaPath, name)
			continue
		}
		if gotDDL != wantDDL {
			t.Errorf("index %s disagrees between the sqlc reference and the runtime schema:\n  reference: %s\n  runtime:   %s",
				name, wantDDL, gotDDL)
		}
	}
}

// Pin the shape the reference now claims, so a future edit back to the partial
// form fails here as well as in the parity test above.
func TestReferenceSchema_TestRequirementRefIndexIsNotPartial(t *testing.T) {
	reference := applyReferenceSchema(t)
	ddl, ok := indexDefs(t, reference, "claims")["idx_claims_test_requirement_ref"]
	if !ok {
		t.Fatalf("idx_claims_test_requirement_ref missing from %s", referenceSchemaPath)
	}
	if strings.Contains(strings.ToUpper(ddl), " WHERE ") {
		t.Errorf("idx_claims_test_requirement_ref is partial in the reference schema (%s); "+
			"SQLite never picks a partial index for ListClaimsByTestRequirementRef, whose bound "+
			"parameter could itself be ''", ddl)
	}
	if !strings.Contains(ddl, "test_requirement_ref, type") {
		t.Errorf("idx_claims_test_requirement_ref does not cover (test_requirement_ref, type): %s", ddl)
	}
}
