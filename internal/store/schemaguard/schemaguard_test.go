package schemaguard

import (
	"reflect"
	"testing"
)

func TestTableColumns(t *testing.T) {
	tests := []struct {
		name  string
		ddl   string
		table string
		want  []string
	}{
		{
			name:  "create table with inline comments and constraints",
			table: "claims",
			ddl: `
CREATE TABLE IF NOT EXISTS claims (
  id TEXT PRIMARY KEY,
  text TEXT NOT NULL,
  -- a comment that names a column, decoy: not_a_column TEXT
  confidence REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (id),
  KEY idx_claims_trust (trust_score),
  CONSTRAINT fk_x FOREIGN KEY (id) REFERENCES other(id)
);`,
			want: []string{"id", "text", "confidence"},
		},
		{
			// Parenthesised types must not be mistaken for the end of the
			// column list, and a DEFAULT holding a comma must not split it.
			name:  "parenthesised types and comma-bearing defaults",
			table: "claims",
			ddl: `
CREATE TABLE claims (
  id             VARCHAR(190)     NOT NULL,
  created_at     DATETIME(6)      NOT NULL,
  confidence     DOUBLE PRECISION NOT NULL DEFAULT 0,
  components     JSONB            NOT NULL DEFAULT '{}'::jsonb,
  label          TEXT             NOT NULL DEFAULT 'a,b'
);`,
			want: []string{"id", "created_at", "confidence", "components", "label"},
		},
		{
			// The ALTER-based additions are how every column since the
			// original schema generation arrives; a guard that read only
			// CREATE TABLE would miss all of them.
			name:  "alter table add column",
			table: "claims",
			ddl: `
CREATE TABLE IF NOT EXISTS claims (
  id text PRIMARY KEY
);
ALTER TABLE claims ADD COLUMN IF NOT EXISTS last_verified timestamptz;
ALTER TABLE claims ADD COLUMN verify_count integer NOT NULL DEFAULT 0;
ALTER TABLE claims    ADD COLUMN IF NOT EXISTS scope_env VARCHAR(64) NOT NULL DEFAULT '';`,
			want: []string{"id", "last_verified", "verify_count", "scope_env"},
		},
		{
			// A repeated declaration (fresh-database CREATE plus the
			// idempotent upgrade block) must not double-count.
			name:  "duplicate declarations collapse",
			table: "claims",
			ddl: `
CREATE TABLE claims (id text, half_life_days double precision);
ALTER TABLE claims ADD COLUMN IF NOT EXISTS half_life_days double precision NOT NULL DEFAULT 0;`,
			want: []string{"id", "half_life_days"},
		},
		{
			// claim_evidence must not contribute columns to claims: a
			// prefix match would silently widen the expected set.
			name:  "sibling tables sharing a name prefix are not confused",
			table: "claims",
			ddl: `
CREATE TABLE claims (id text, text text);
CREATE TABLE claim_evidence (claim_id text, event_id text);
ALTER TABLE claim_evidence ADD COLUMN IF NOT EXISTS weight double precision;`,
			want: []string{"id", "text"},
		},
		{
			name:  "quoted identifiers",
			table: "claims",
			ddl:   "CREATE TABLE `claims` (`id` VARCHAR(190) NOT NULL, \"text\" TEXT);",
			want:  []string{"id", "text"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := TableColumns(tt.ddl, tt.table)
			if err != nil {
				t.Fatalf("TableColumns: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("TableColumns = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTableColumns_UnknownTable(t *testing.T) {
	// Silently returning an empty set would make every downstream check
	// vacuously pass — the exact failure mode the guard exists to prevent.
	if _, err := TableColumns("CREATE TABLE claims (id text);", "beliefs"); err == nil {
		t.Fatal("TableColumns on an undeclared table returned no error")
	}
}

func TestInsertColumns(t *testing.T) {
	tests := []struct {
		name string
		stmt string
		want []string
	}{
		{
			name: "postgres numbered placeholders",
			stmt: `
INSERT INTO ns.claims (id, text, half_life_days)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE SET text = EXCLUDED.text`,
			want: []string{"id", "text", "half_life_days"},
		},
		{
			name: "mysql placeholders and a multiline column list",
			stmt: `
INSERT INTO claims (id, text,
                    valid_from, trust_score)
VALUES (?, ?, ?, 0)
ON DUPLICATE KEY UPDATE text = VALUES(text)`,
			want: []string{"id", "text", "valid_from", "trust_score"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := InsertColumns(tt.stmt)
			if err != nil {
				t.Fatalf("InsertColumns: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("InsertColumns = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUpsertUpdateColumns(t *testing.T) {
	tests := []struct {
		name string
		stmt string
		want []string
	}{
		{
			name: "postgres ON CONFLICT DO UPDATE",
			stmt: `
INSERT INTO ns.claims (id, text) VALUES ($1, $2)
ON CONFLICT (id) DO UPDATE SET
  text = EXCLUDED.text,
  half_life_days = CASE WHEN EXCLUDED.half_life_days > 0 THEN EXCLUDED.half_life_days ELSE claims.half_life_days END,
  lifecycle = EXCLUDED.lifecycle`,
			want: []string{"text", "half_life_days", "lifecycle"},
		},
		{
			name: "mysql ON DUPLICATE KEY UPDATE",
			stmt: `
INSERT INTO claims (id, text) VALUES (?, ?)
ON DUPLICATE KEY UPDATE
  text = VALUES(text),
  confidence = COALESCE(VALUES(confidence), confidence),
  durability = VALUES(durability)`,
			want: []string{"text", "confidence", "durability"},
		},
		{
			name: "sqlite lowercase excluded, statement terminated by a semicolon",
			stmt: `
INSERT INTO claims (id, text) VALUES (?, ?)
ON CONFLICT(id) DO UPDATE SET
  text = excluded.text,
  status = excluded.status;

-- name: SomethingElse :exec
UPDATE claims SET trust_score = ? WHERE id = ?;`,
			want: []string{"text", "status"},
		},
		{
			name: "no conflict clause yields no columns",
			stmt: `INSERT INTO claims (id, text) VALUES (?, ?)`,
			want: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := UpsertUpdateColumns(tt.stmt)
			if err != nil {
				t.Fatalf("UpsertUpdateColumns: %v", err)
			}
			if !equalOrBothEmpty(got, tt.want) {
				t.Errorf("UpsertUpdateColumns = %v, want %v", got, tt.want)
			}
		})
	}
}

// The whole point of the guard is that it FAILS on drift, so its own
// difference engine is tested for detection, not just for agreement.
func TestCheck(t *testing.T) {
	tests := []struct {
		name        string
		want        []string
		got         []string
		allow       map[string]string
		wantMissing []string
		wantStale   []string
	}{
		{
			name: "complete projection passes",
			want: []string{"id", "text"},
			got:  []string{"text", "id"},
		},
		{
			// #335 in miniature: the columns exist and the projection
			// does not name them.
			name:        "omitted columns are reported",
			want:        []string{"id", "last_verified", "verify_count"},
			got:         []string{"id"},
			wantMissing: []string{"last_verified", "verify_count"},
		},
		{
			name:  "allowlisted omissions pass",
			want:  []string{"id", "scope_env"},
			got:   []string{"id"},
			allow: map[string]string{"scope_env": "not read on this backend"},
		},
		{
			// A stale excuse is a silently weakened guard: the next
			// omission of that column would be waved through.
			name:      "allowance for a column that is present is stale",
			want:      []string{"id", "scope_env"},
			got:       []string{"id", "scope_env"},
			allow:     map[string]string{"scope_env": "not read on this backend"},
			wantStale: []string{"scope_env"},
		},
		{
			name:      "allowance for a column that no longer exists is stale",
			want:      []string{"id"},
			got:       []string{"id"},
			allow:     map[string]string{"dropped_column": "gone in v2"},
			wantStale: []string{"dropped_column"},
		},
		{
			name:  "an empty reason does not excuse anything",
			want:  []string{"id", "scope_env"},
			got:   []string{"id"},
			allow: map[string]string{"scope_env": ""},
			// An entry with no reason is not a deliberate omission, it is
			// an unexplained one — so the column still counts as missing
			// and the entry is reported as unusable.
			wantMissing: []string{"scope_env"},
			wantStale:   []string{"scope_env"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Check(tt.want, tt.got, tt.allow)
			if !equalOrBothEmpty(res.Missing, tt.wantMissing) {
				t.Errorf("Missing = %v, want %v", res.Missing, tt.wantMissing)
			}
			if !equalOrBothEmpty(res.StaleAllowances, tt.wantStale) {
				t.Errorf("StaleAllowances = %v, want %v", res.StaleAllowances, tt.wantStale)
			}
			if res.OK() != (len(tt.wantMissing) == 0 && len(tt.wantStale) == 0) {
				t.Errorf("OK() = %v with missing=%v stale=%v", res.OK(), res.Missing, res.StaleAllowances)
			}
		})
	}
}

func equalOrBothEmpty(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}
