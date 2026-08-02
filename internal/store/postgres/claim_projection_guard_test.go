package postgres

import (
	"os"
	"testing"

	"go.klarlabs.de/mnemos/internal/store/schemaguard"
)

// sqliteSchemaPath is the reference schema. SQLite is the only backend
// whose claim reads are GENERATED (sqlc derives the row struct from the
// query file, so an omitted column fails to compile rather than
// returning a zero), which makes it the backend that has never drifted
// and therefore the definition of what a complete claim looks like.
const sqliteSchemaPath = "../../../sql/sqlite/schema.sql"

func pgClaimColumns(t *testing.T) []string {
	t.Helper()
	cols, err := schemaguard.TableColumns(schemaSQL, "claims")
	if err != nil {
		t.Fatalf("parse postgres claims DDL: %v", err)
	}
	return cols
}

func sqliteClaimColumns(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(sqliteSchemaPath)
	if err != nil {
		t.Fatalf("read sqlite reference schema: %v", err)
	}
	cols, err := schemaguard.TableColumns(string(raw), "claims")
	if err != nil {
		t.Fatalf("parse sqlite claims DDL: %v", err)
	}
	return cols
}

// --- allowlists ---
//
// Every entry is a deliberate omission with a stated reason. An entry that
// stops being needed FAILS the guard (schemaguard reports it as stale), so an
// excuse cannot outlive the thing it excuses, and an entry with an empty reason
// excuses nothing. Adding one is the explicit alternative to silently dropping
// a column — which is how #331, #335, #338 and #339 all happened.

// unreadColumns are declared on the Postgres claims table and deliberately
// absent from claimColumnNames.
var unreadColumns = map[string]string{
	"search_tsv": "GENERATED ALWAYS AS (to_tsvector('english', text)) STORED — a derived " +
		"full-text accelerator with no domain field. Postgres rejects writes to it, and " +
		"reading it would only duplicate `text` in every row.",
}

// unwrittenColumns are read back by claimColumnNames and deliberately absent
// from the claim upsert's INSERT column list, because another statement owns
// them.
var unwrittenColumns = map[string]string{
	"last_verified": "Owned by MarkVerified. The ingest upsert must never touch it: " +
		"re-extracting a claim is not a verification, and writing it here would reset " +
		"recall-driven freshness on every capture.",
	"verify_count": "Owned by MarkVerified, which increments it. The ingest upsert " +
		"carries no count to write and would reset the tally to zero.",
}

// undeclaredColumns exist on the SQLite claims table and not in the Postgres
// schema at all. Each one is a value a brain hosted here cannot persist.
//
// Empty since #338/#339 closed: every SQLite claims column is now declared
// here. The map stays as the documented place to record a deliberate
// divergence — with a reason, which an empty string is not.
var undeclaredColumns = map[string]string{}

// Every column this backend declares must be in the read projection.
//
// This is the #335 direction: the column exists, the writer fills it, and
// the projection never selects it, so every read hands the application a
// zero that is indistinguishable from "never set". No error, no failing
// test — just a hosted brain that forgets beliefs a local one keeps.
func TestClaimProjection_ReadsEveryDeclaredColumn(t *testing.T) {
	res := schemaguard.Check(pgClaimColumns(t), claimColumnNames, unreadColumns)
	if len(res.Missing) > 0 {
		t.Errorf("postgres claims columns declared but never read: %v\n"+
			"Add them to claimColumnNames AND to scanClaimRow, or add an entry to "+
			"unreadColumns saying why the value is never needed.", res.Missing)
	}
	if len(res.StaleAllowances) > 0 {
		t.Errorf("unreadColumns entries that no longer excuse anything: %v\n"+
			"The column is read now, or no longer exists, or the reason is empty. "+
			"Delete the entry — a stale excuse disarms the guard for that column.",
			res.StaleAllowances)
	}
}

// The read-modify-write zeroing hazard, in the other direction.
//
// `recompute-half-life`, `prune --narration` and `recompute-contested`
// all read a claim, change one field, and upsert the whole object back.
// Every column in the ON CONFLICT set is therefore rewritten from what
// the read returned — so a column that is written on conflict but not
// read is silently zeroed across every row such a pass touches.
//
// There is no legitimate reason to be in this set and not in the
// projection, which is why this check has no allowlist.
func TestClaimProjection_ReadsEveryUpsertedColumn(t *testing.T) {
	upserted, err := schemaguard.UpsertUpdateColumns(claimUpsertSQL(""))
	if err != nil {
		t.Fatalf("parse claim upsert: %v", err)
	}
	if len(upserted) == 0 {
		t.Fatal("no ON CONFLICT update columns parsed — the guard would be vacuous")
	}
	res := schemaguard.Check(upserted, claimColumnNames, nil)
	if len(res.Missing) > 0 {
		t.Errorf("postgres claim upsert rewrites columns the read projection omits: %v\n"+
			"A read-modify-write pass will zero these across every row it touches. "+
			"Add them to claimColumnNames and scanClaimRow, or drop them from the "+
			"ON CONFLICT set.", res.Missing)
	}
}

// A column that is read but never written keeps its default forever.
//
// This is the #331 direction: half_life_days was in the schema and in the
// projection, and absent from the INSERT column list, so the pipeline's
// classification was computed, carried on the domain object, and dropped
// at the store boundary for the life of the product.
func TestClaimProjection_WritesEveryReadColumn(t *testing.T) {
	inserted, err := schemaguard.InsertColumns(claimUpsertSQL(""))
	if err != nil {
		t.Fatalf("parse claim insert: %v", err)
	}
	res := schemaguard.Check(claimColumnNames, inserted, unwrittenColumns)
	if len(res.Missing) > 0 {
		t.Errorf("postgres claim columns read but never written by the claim upsert: %v\n"+
			"They will read back as the store default forever. Add them to the INSERT "+
			"column list, or add an entry to unwrittenColumns naming the statement "+
			"that owns them.", res.Missing)
	}
	if len(res.StaleAllowances) > 0 {
		t.Errorf("unwrittenColumns entries that no longer excuse anything: %v", res.StaleAllowances)
	}
}

// A projection naming a column the schema does not declare is the one
// variant of this drift that fails loudly — SQLSTATE 42703 at query time
// — but only on the code path that runs it. ListByTestRequirementRef
// shipped in exactly that state until the test_* columns were declared.
func TestClaimProjection_OnlyNamesDeclaredColumns(t *testing.T) {
	res := schemaguard.Check(claimColumnNames, pgClaimColumns(t), nil)
	if len(res.Missing) > 0 {
		t.Errorf("claimColumnNames selects columns the postgres schema does not declare: %v\n"+
			"Every read using this projection fails with 42703 (undefined column).", res.Missing)
	}
}

// The cross-backend dimension: a column SQLite stores and Postgres does
// not declare at all cannot be read, written or fixed by any projection.
// It is the same defect one layer down, and it has the same symptom —
// the same brain behaving differently depending on where it is hosted.
func TestClaimSchema_DeclaresEverySQLiteColumn(t *testing.T) {
	res := schemaguard.Check(sqliteClaimColumns(t), pgClaimColumns(t), undeclaredColumns)
	if len(res.Missing) > 0 {
		t.Errorf("columns in the SQLite claims table that Postgres does not declare: %v\n"+
			"A brain hosted on Postgres cannot persist these at all. Declare them (and "+
			"add them to the projection and the upsert), or add an entry to "+
			"undeclaredColumns saying why this backend does not need them.", res.Missing)
	}
	if len(res.StaleAllowances) > 0 {
		t.Errorf("undeclaredColumns entries that no longer excuse anything: %v", res.StaleAllowances)
	}
}
