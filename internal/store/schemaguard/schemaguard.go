// Package schemaguard compares a hand-written SQL backend's read
// projection against the DDL it runs on and against the statements it
// writes with, so that projection drift is a test failure instead of
// silent data loss.
//
// Why this exists: the Postgres and MySQL providers each keep ONE
// hand-maintained claim projection (`claimColumnNames`). It has drifted
// from the schema twice — #331/#334 dropped `half_life_days` on the
// write path, #335 left `last_verified` and `verify_count` out of the
// read projection on both backends — and neither was caught by a test,
// because every affected code path succeeds. A write that never
// persists a column still commits; a read that never selects one still
// scans, and hands the application a zero value that is
// indistinguishable from "never set". The defect only surfaces as
// behaviour: a hosted brain forgetting beliefs a local SQLite brain
// keeps.
//
// Why static SQL analysis rather than a live `information_schema` query:
// the two hand-written backends are exactly the two that need a
// container to test, so a live check would skip precisely where the
// drift lives — `make check` and the default CI test job run without
// TEST_POSTGRES_DSN / TEST_MYSQL_DSN. The DDL parsed here is the same
// embedded `schema.sql` the provider executes on every Open, and the
// statements parsed here are produced by the same functions the
// repository executes, so the guard reads the real artefacts and runs
// everywhere, with no infrastructure.
//
// The parser is deliberately small and dialect-tolerant rather than a
// full SQL grammar: it recognises CREATE TABLE / ALTER TABLE ADD COLUMN,
// an INSERT column list, and the assignment set of ON CONFLICT ... DO
// UPDATE (Postgres, SQLite) and ON DUPLICATE KEY UPDATE (MySQL). Every
// construct actually used by this repository's claim statements is
// covered by the package's own tests; anything it cannot parse is an
// error, never a silent empty result.
package schemaguard

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// TableColumns returns the column names declared for table in ddl, in
// declaration order, from its CREATE TABLE body plus every subsequent
// ALTER TABLE ... ADD COLUMN. Both sources matter: this repository's
// schemas declare the original generation inline and every later column
// as an idempotent ALTER, so reading only the CREATE TABLE would miss
// most of the table.
//
// An undeclared table is an error rather than an empty slice, because an
// empty "expected" set turns every caller's comparison vacuously green.
func TableColumns(ddl, table string) ([]string, error) {
	clean := stripComments(ddl)

	var cols []string
	seen := make(map[string]bool)
	add := func(c string) {
		c = unquote(c)
		if c == "" || seen[c] {
			return
		}
		seen[c] = true
		cols = append(cols, c)
	}

	body, found, err := createTableBody(clean, table)
	if err != nil {
		return nil, err
	}
	if found {
		for _, item := range splitTopLevel(body, ',') {
			item = strings.TrimSpace(item)
			if item == "" || isTableConstraint(item) {
				continue
			}
			add(firstToken(item))
		}
	}

	alter := regexp.MustCompile(`(?is)\bALTER\s+TABLE\s+` + identPattern(table) +
		`\s+ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?([` + "`" + `"]?[A-Za-z_][A-Za-z0-9_]*[` + "`" + `"]?)`)
	for _, m := range alter.FindAllStringSubmatch(clean, -1) {
		add(m[1])
	}

	if len(cols) == 0 {
		return nil, fmt.Errorf("schemaguard: no columns found for table %q — is it declared in this DDL?", table)
	}
	return cols, nil
}

// InsertColumns returns the column list of the INSERT in stmt: the
// parenthesised names between the target table and VALUES. This is the
// write-path counterpart of the read projection — #331 was a column
// present in the schema and in the projection but absent from this list,
// so the value was computed, carried, and dropped at the store boundary.
func InsertColumns(stmt string) ([]string, error) {
	clean := stripComments(stmt)
	loc := regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+[^\s(]+\s*\(`).FindStringIndex(clean)
	if loc == nil {
		return nil, fmt.Errorf("schemaguard: no INSERT INTO <table> (...) found")
	}
	body, err := balancedParen(clean, loc[1]-1)
	if err != nil {
		return nil, fmt.Errorf("schemaguard: insert column list: %w", err)
	}
	var cols []string
	for _, c := range splitTopLevel(body, ',') {
		c = unquote(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		cols = append(cols, c)
	}
	return cols, nil
}

var conflictClause = regexp.MustCompile(`(?is)\bON\s+CONFLICT\b.*?\bDO\s+UPDATE\s+SET\b|\bON\s+DUPLICATE\s+KEY\s+UPDATE\b`)

// UpsertUpdateColumns returns the columns assigned by the statement's
// conflict-resolution UPDATE set — ON CONFLICT ... DO UPDATE SET
// (Postgres, SQLite) or ON DUPLICATE KEY UPDATE (MySQL).
//
// These are the columns a read-modify-write rewrites from whatever the
// read returned. A column in this set that the read projection omits is
// the zeroing hazard: `recompute-half-life`, `prune --narration` and
// `recompute-contested` all read a claim, change one field, and upsert
// it back, so an unread column is written back as its zero value across
// every row the pass touches.
//
// A statement with no conflict clause yields no columns and no error —
// it simply cannot zero anything.
func UpsertUpdateColumns(stmt string) ([]string, error) {
	clean := stripComments(stmt)
	loc := conflictClause.FindStringIndex(clean)
	if loc == nil {
		return []string{}, nil
	}
	set := clean[loc[1]:]
	// The set runs to the end of the statement. In a multi-statement file
	// (the sqlc query files) that is the next top-level semicolon.
	if parts := splitTopLevel(set, ';'); len(parts) > 0 {
		set = parts[0]
	}

	cols := []string{}
	for _, assign := range splitTopLevel(set, ',') {
		assign = strings.TrimSpace(assign)
		if assign == "" {
			continue
		}
		eq := strings.Index(assign, "=")
		if eq < 0 {
			return nil, fmt.Errorf("schemaguard: update-set item %q has no assignment", assign)
		}
		col := unquote(strings.TrimSpace(assign[:eq]))
		if !isIdentifier(col) {
			return nil, fmt.Errorf("schemaguard: update-set item %q does not start with a column name", assign)
		}
		cols = append(cols, col)
	}
	return cols, nil
}

// Result is the outcome of one comparison.
type Result struct {
	// Missing are the wanted columns the projection does not contain and
	// no allowlist entry excuses.
	Missing []string
	// StaleAllowances are allowlist entries that no longer excuse
	// anything: the column is present after all, or it no longer exists,
	// or the entry carries no reason. They are reported as failures too —
	// an unnecessary excuse silently disarms the guard for that column
	// the next time it goes missing.
	StaleAllowances []string
}

// OK reports whether the comparison found nothing to fix.
func (r Result) OK() bool { return len(r.Missing) == 0 && len(r.StaleAllowances) == 0 }

// Check reports which of want is absent from got, excusing the columns
// named in allow (column -> reason). An entry with an empty reason
// excuses nothing: a deliberate omission has to say why, or it is
// indistinguishable from the drift this package exists to catch.
func Check(want, got []string, allow map[string]string) Result {
	have := make(map[string]bool, len(got))
	for _, c := range got {
		have[c] = true
	}
	wanted := make(map[string]bool, len(want))
	for _, c := range want {
		wanted[c] = true
	}

	var res Result
	excused := make(map[string]bool)
	for _, c := range want {
		if have[c] {
			continue
		}
		if reason, ok := allow[c]; ok && strings.TrimSpace(reason) != "" {
			excused[c] = true
			continue
		}
		res.Missing = append(res.Missing, c)
	}
	for c := range allow {
		if !excused[c] {
			res.StaleAllowances = append(res.StaleAllowances, c)
		}
	}
	sort.Strings(res.StaleAllowances)
	return res
}

// --- parsing helpers ---

// stripComments removes `-- ...` line comments so a column name written
// in prose (or a commented-out column) cannot be mistaken for a
// declaration.
func stripComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\'':
			inString = !inString
			b.WriteByte(s[i])
		case !inString && s[i] == '-' && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			if i < len(s) {
				b.WriteByte('\n')
			}
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// identPattern matches a table name optionally quoted and optionally
// schema-qualified, so `claims`, "claims" and ns.claims all resolve —
// and `claim_evidence` does not.
func identPattern(name string) string {
	q := regexp.QuoteMeta(name)
	return `(?:[A-Za-z_][A-Za-z0-9_]*\.)?[` + "`" + `"]?` + q + `[` + "`" + `"]?`
}

func createTableBody(ddl, table string) (body string, found bool, err error) {
	re := regexp.MustCompile(`(?is)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?` + identPattern(table) + `\s*\(`)
	loc := re.FindStringIndex(ddl)
	if loc == nil {
		return "", false, nil
	}
	body, err = balancedParen(ddl, loc[1]-1)
	if err != nil {
		return "", false, fmt.Errorf("schemaguard: create table %s: %w", table, err)
	}
	return body, true, nil
}

// balancedParen returns the contents of the parenthesised group that
// opens at s[open].
func balancedParen(s string, open int) (string, error) {
	if open >= len(s) || s[open] != '(' {
		return "", fmt.Errorf("expected '(' at offset %d", open)
	}
	depth := 0
	inString := false
	for i := open; i < len(s); i++ {
		switch {
		case s[i] == '\'':
			inString = !inString
		case inString:
		case s[i] == '(':
			depth++
		case s[i] == ')':
			depth--
			if depth == 0 {
				return s[open+1 : i], nil
			}
		}
	}
	return "", fmt.Errorf("unbalanced parentheses")
}

// splitTopLevel splits on sep, ignoring separators nested in parentheses
// or inside a single-quoted literal. A type like VARCHAR(190) or a
// DEFAULT of 'a,b' therefore cannot split a column definition.
func splitTopLevel(s string, sep byte) []string {
	var out []string
	depth := 0
	inString := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\'':
			inString = !inString
		case inString:
		case s[i] == '(':
			depth++
		case s[i] == ')':
			depth--
		case s[i] == sep && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

var tableConstraintKeywords = map[string]bool{
	"PRIMARY": true, "UNIQUE": true, "KEY": true, "CONSTRAINT": true,
	"FOREIGN": true, "CHECK": true, "INDEX": true, "FULLTEXT": true,
	"SPATIAL": true, "EXCLUDE": true, "LIKE": true,
}

// isTableConstraint reports whether a CREATE TABLE body item declares a
// constraint or index rather than a column.
func isTableConstraint(item string) bool {
	return tableConstraintKeywords[strings.ToUpper(firstToken(item))]
}

func firstToken(s string) string {
	s = strings.TrimSpace(s)
	i := strings.IndexAny(s, " \t\n\r(")
	if i < 0 {
		return s
	}
	return s[:i]
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`\"")
	// Drop a schema/table qualifier: `c.id` in a projection is column id.
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	return s
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func isIdentifier(s string) bool { return identRe.MatchString(s) }
