package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// An expectedColumns entry must work WITHOUT a currentSchemaVersion bump.
//
// This is the regression that shipped with ADR 0025 and was caught by running
// the binary against a copy of a real brain rather than by any test. The column
// probes sat behind `userVersion >= currentSchemaVersion { return nil }`, so an
// entry added without also bumping the version constant was inert — and inert
// on exactly the databases that needed it, because a brain written by the
// previous release already sits AT the current version.
//
// The failure is total, not partial: the first read fails with
//
//	list all claims: SQL logic error: no such column: half_life_classifier (1)
//
// and the brain cannot be opened at all. Shipping that would have bricked every
// existing brain on upgrade, to fix a reporting defect that harmed nothing.
//
// It stayed invisible because a fresh test database gets every column from
// CREATE TABLE and never reaches the ALTER path, and because the entry/bump
// pairing was a convention recorded in a comment — nothing the compiler or a
// test could check.
//
// The test simulates the real shape: a database already AT currentSchemaVersion
// (as a previous release left it) that is missing a column this binary expects.
func TestMigrate_AddsAMissingColumnEvenAtTheCurrentSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	const col = "half_life_classifier"
	if has, err := columnExists(db, "claims", col); err != nil || !has {
		t.Fatalf("precondition: claims.%s should exist on a fresh DB (has=%v err=%v)", col, has, err)
	}

	// Drop it and pin user_version at the current generation — precisely the
	// state a brain from the previous release is in.
	if _, err := db.Exec("ALTER TABLE claims DROP COLUMN " + col); err != nil {
		t.Fatalf("drop %s: %v", col, err)
	}
	if _, err := db.Exec("PRAGMA user_version = " + itoa(currentSchemaVersion)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if has, _ := columnExists(db, "claims", col); has {
		t.Fatalf("precondition: %s should be gone after the drop", col)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if has, err := columnExists(db, "claims", col); err != nil || !has {
		t.Errorf("claims.%s missing after migrate (has=%v err=%v) — an expectedColumns "+
			"entry is inert at the current schema version, so every pre-existing brain "+
			"fails its first read with \"no such column\"", col, has, err)
	}
}

// Every expectedColumns entry must survive the same treatment, so this cannot
// regress for one column while passing for another.
func TestMigrate_RestoresEveryExpectedColumnAtTheCurrentVersion(t *testing.T) {
	for _, c := range expectedColumns {
		path := filepath.Join(t.TempDir(), "legacy.db")
		db, err := open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}

		// Not every expected column is droppable (SQLite refuses to drop one
		// that an index or a generated column depends on). Skip those rather
		// than assert something the engine will not allow.
		if _, err := db.Exec("ALTER TABLE " + c.table + " DROP COLUMN " + c.column); err != nil {
			_ = db.Close()
			continue
		}
		if _, err := db.Exec("PRAGMA user_version = " + itoa(currentSchemaVersion)); err != nil {
			t.Fatalf("set user_version: %v", err)
		}
		if err := migrate(db); err != nil {
			t.Fatalf("%s.%s: migrate: %v", c.table, c.column, err)
		}
		if has, err := columnExists(db, c.table, c.column); err != nil || !has {
			t.Errorf("%s.%s missing after migrate at the current schema version", c.table, c.column)
		}
		_ = db.Close()
	}
}

// itoa avoids pulling strconv in for two call sites in a test.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

var _ = sql.ErrNoRows
