// Package mysql implements a [store] provider backed by MySQL or
// MariaDB (the wire protocol is identical and the SQL dialect we
// rely on is the common subset).
//
// Per ADR 0001 §3, MySQL has no per-tenant schemas — the namespace
// translates to a *database*. The provider runs `CREATE DATABASE
// IF NOT EXISTS <namespace>` against the server, then opens a
// connection scoped to that database. Schema bootstrap (schema.sql)
// is applied on every Open and is idempotent.
//
// Both the canonical Mnemos URL form (`mysql://...`) and the
// MariaDB alias (`mariadb://...`) are accepted. The Go driver
// uses libmysql-style DSNs (user:pw@tcp(host)/db?…) rather than
// URLs, so this package translates between the two formats so the
// rest of the stack can stay URL-uniform.
package mysql

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"go.klarlabs.de/mnemos/internal/store"

	// go-sql-driver/mysql registers the "mysql" sql.DB driver in init().
	_ "github.com/go-sql-driver/mysql"
)

//go:embed schema.sql
var schemaSQL string

// ErrNotImplemented is the historical sentinel name. Kept exported
// for symmetry with the Postgres scaffold history; the provider
// itself no longer uses it.
var ErrNotImplemented = errors.New("mysql provider: feature not yet implemented")

// Register both `mysql://` and `mariadb://` schemes. MariaDB shares
// the wire protocol and SQL dialect we rely on, so one provider
// serves both.
func init() {
	store.Register("mysql", openProvider)
	store.Register("mariadb", openProvider)
}

// DSN holds the parsed components of a Mnemos MySQL DSN.
type DSN struct {
	// Raw is the original URL DSN the operator supplied.
	Raw string

	// DriverDSN is the libmysql-style DSN the Go driver consumes.
	// Includes parseTime=true so DATETIME columns scan into
	// time.Time.
	DriverDSN string

	// AdminDSN is the same as DriverDSN but with the database name
	// stripped — used for the `CREATE DATABASE IF NOT EXISTS` boot
	// step before we reconnect with the namespace selected.
	AdminDSN string

	// Namespace is the effective database name. Resolved from the
	// `?namespace=` query param if set, else the path component, else
	// the default "mnemos". Validated against
	// `^[a-z][a-z0-9_]{0,62}$` per ADR 0001.
	Namespace string
}

// namespaceRE mirrors ADR 0001 §3. MySQL identifier rules are more
// permissive but we deliberately stay within the conservative subset
// that works across every provider.
var namespaceRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

const defaultNamespace = "mnemos"

// ParseDSN translates a `mysql://` or `mariadb://` URL into the
// libmysql DSN the Go driver requires, plus an admin DSN for the
// CREATE DATABASE bootstrap. Honors `?namespace=` per the ADR.
func ParseDSN(dsn string) (DSN, error) {
	if !strings.HasPrefix(dsn, "mysql://") && !strings.HasPrefix(dsn, "mariadb://") {
		return DSN{}, fmt.Errorf("mysql: not a mysql/mariadb dsn: %q", dsn)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return DSN{}, fmt.Errorf("mysql: parse dsn: %w", err)
	}

	q := u.Query()
	ns := q.Get("namespace")
	if ns == "" {
		// Fallback: take the URL path as the namespace if set.
		// `mysql://host/dbname` is a common shape.
		ns = strings.TrimPrefix(u.Path, "/")
	}
	if ns == "" {
		ns = defaultNamespace
	}
	if !namespaceRE.MatchString(ns) {
		return DSN{}, fmt.Errorf("mysql: invalid namespace %q (want %s)", ns, namespaceRE.String())
	}
	q.Del("namespace")

	// parseTime=true is required for time.Time scanning.
	if q.Get("parseTime") == "" {
		q.Set("parseTime", "true")
	}

	host := u.Host
	if host == "" {
		host = "127.0.0.1:3306"
	}
	userInfo := ""
	if u.User != nil {
		if pw, ok := u.User.Password(); ok {
			userInfo = u.User.Username() + ":" + pw + "@"
		} else {
			userInfo = u.User.Username() + "@"
		}
	}
	params := ""
	if encoded := q.Encode(); encoded != "" {
		params = "?" + encoded
	}

	driver := fmt.Sprintf("%stcp(%s)/%s%s", userInfo, host, ns, params)
	admin := fmt.Sprintf("%stcp(%s)/%s", userInfo, host, params) // empty database

	return DSN{
		Raw:       dsn,
		DriverDSN: driver,
		AdminDSN:  admin,
		Namespace: ns,
	}, nil
}

// openProvider runs the bootstrap (CREATE DATABASE IF NOT EXISTS,
// then schema.sql) and returns a Conn populated with port-typed
// repositories.
func openProvider(ctx context.Context, dsn string) (*store.Conn, error) {
	parsed, err := ParseDSN(dsn)
	if err != nil {
		return nil, err
	}

	// Step 1: connect without a database to issue CREATE DATABASE.
	// We avoid taking a long-lived handle on the admin connection;
	// it's only used to ensure the namespace exists.
	if err := ensureDatabase(ctx, parsed); err != nil {
		return nil, err
	}

	// Step 2: open the main connection scoped to the namespace.
	db, err := sql.Open("mysql", parsed.DriverDSN)
	if err != nil {
		return nil, fmt.Errorf("mysql: sql.Open: %w", err)
	}
	tuneConnPool(db)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mysql: ping: %w (dsn=%s)", err, parsed.DriverDSN)
	}

	if err := applySchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mysql: apply schema in %s: %w", parsed.Namespace, err)
	}

	return &store.Conn{
		Events:        EventRepository{db: db},
		Claims:        ClaimRepository{db: db},
		Relationships: RelationshipRepository{db: db},
		Embeddings:    EmbeddingRepository{db: db},
		Users:         UserRepository{db: db},
		RevokedTokens: RevokedTokenRepository{db: db},
		Agents:        AgentRepository{db: db},
		Entities:      EntityRepository{db: db},
		Jobs:          CompilationJobRepository{db: db},
		Actions:       ActionRepository{db: db},
		Outcomes:      OutcomeRepository{db: db},
		Journal:       JournalRepository{db: db},
		Lessons:       LessonRepository{db: db},
		Decisions:     DecisionRepository{db: db},
		Playbooks:     PlaybookRepository{db: db},
		EntityRels:    EntityRelationshipRepository{db: db},
		Raw:           db,
		Closer:        db.Close,
	}, nil
}

// ensureDatabase issues CREATE DATABASE IF NOT EXISTS against the
// admin DSN. It opens its own short-lived *sql.DB so failures here
// don't leave a half-initialised pool around.
func ensureDatabase(ctx context.Context, parsed DSN) error {
	if !namespaceRE.MatchString(parsed.Namespace) {
		return fmt.Errorf("invalid namespace %q", parsed.Namespace)
	}
	admin, err := sql.Open("mysql", parsed.AdminDSN)
	if err != nil {
		return fmt.Errorf("mysql: open admin: %w", err)
	}
	defer func() { _ = admin.Close() }()
	if err := admin.PingContext(ctx); err != nil {
		return fmt.Errorf("mysql: ping admin: %w", err)
	}
	// Identifier validation above is the only safety against
	// injection — the namespace is interpolated, not parameter-bound,
	// because identifiers can't be parameter-bound in SQL.
	stmt := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", parsed.Namespace)
	if _, err := admin.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("mysql: create database %s: %w", parsed.Namespace, err)
	}
	return nil
}

// applySchema runs every statement in schema.sql against the open
// connection (which is already USE <namespace>'d courtesy of the
// driver). Each statement is split on `;` because the Go driver
// rejects multi-statement queries by default; we don't enable
// MultiStatements because a one-time per-statement Exec costs
// nothing on a small DDL file.
func applySchema(ctx context.Context, db *sql.DB) error {
	for _, stmt := range splitStatements(schemaSQL) {
		_, err := db.ExecContext(ctx, stmt)
		if err == nil {
			continue
		}
		// Vanilla MySQL rejects ALTER TABLE ... ADD COLUMN IF NOT EXISTS as a
		// SYNTAX error (1064) — it has never supported the clause, MariaDB
		// aside. Swallowing that error left every pre-existing database stuck
		// on the schema generation it was created with: new columns landed only
		// on fresh databases, and the first read touching one failed with
		// "unknown column". Retry without the clause and let 1060 (duplicate
		// column) mean "already there".
		if retry, ok := alterWithoutIfNotExists(stmt, err); ok {
			if _, rerr := db.ExecContext(ctx, retry); rerr != nil {
				if isBenignSchemaError(retry, rerr) {
					continue
				}
				return fmt.Errorf("apply schema stmt %q: %w", firstLine(retry), rerr)
			}
			continue
		}
		if isBenignSchemaError(stmt, err) {
			continue
		}
		return fmt.Errorf("apply schema stmt %q: %w", firstLine(stmt), err)
	}
	return nil
}

// alterWithoutIfNotExists rewrites an `ALTER TABLE ... ADD COLUMN IF NOT
// EXISTS` that the server rejected as a syntax error into the plain form, so
// the column add can be retried on engines lacking the clause. Reports false
// for any other statement or error.
func alterWithoutIfNotExists(stmt string, err error) (string, bool) {
	upper := strings.ToUpper(strings.TrimSpace(stmt))
	if !strings.HasPrefix(upper, "ALTER TABLE") || !strings.Contains(upper, "ADD COLUMN IF NOT EXISTS") {
		return "", false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "1064") && !strings.Contains(msg, "syntax") {
		return "", false
	}
	idx := strings.Index(upper, "IF NOT EXISTS ")
	if idx < 0 {
		return "", false
	}
	return stmt[:idx] + stmt[idx+len("IF NOT EXISTS "):], true
}

// isBenignSchemaError swallows three MySQL idempotency gaps:
//
//   - Vanilla MySQL (unlike MariaDB) rejects ALTER TABLE ADD COLUMN
//     IF NOT EXISTS as a syntax error. applySchema retries those
//     without the clause (see alterWithoutIfNotExists); the
//     duplicate-column error 1060 then signals the column already
//     exists, which is the success case for an upgrade migration.
//   - On a fresh database the CREATE TABLE statements just above
//     have already created the columns, so the ALTERs are no-ops by
//     design. Treat any 1060 from an ALTER ADD COLUMN as success.
//   - CREATE INDEX has no IF NOT EXISTS in MySQL at all, so an index
//     added to an existing table can only be idempotent by tolerating
//     1061 (duplicate key name) on re-run.
func isBenignSchemaError(stmt string, err error) bool {
	upper := strings.ToUpper(strings.TrimSpace(stmt))
	msgLower := strings.ToLower(err.Error())
	if strings.HasPrefix(upper, "CREATE INDEX") {
		// Error 1061: Duplicate key name — the index is already installed.
		return strings.Contains(msgLower, "1061") || strings.Contains(msgLower, "duplicate key name")
	}
	if !strings.HasPrefix(upper, "ALTER TABLE") {
		return false
	}
	if !strings.Contains(upper, "ADD COLUMN") {
		return false
	}
	msg := strings.ToLower(err.Error())
	// Error 1060: Duplicate column name. Means the column is
	// already present and the ALTER would have been redundant.
	if strings.Contains(msg, "1060") || strings.Contains(msg, "duplicate column") {
		return true
	}
	// Older MySQL parses "IF NOT EXISTS" as a syntax error. Strip
	// it and retry once via a side-effecting recursion is heavy;
	// instead skip the statement and rely on the explicit CREATE
	// TABLE above having installed the column. This is safe on a
	// fresh DB; pre-existing databases that need the upgrade
	// must be migrated manually until MySQL adds the syntax.
	if strings.Contains(msg, "if not exists") || strings.Contains(msg, "1064") {
		return true
	}
	return false
}

// splitStatements splits a SQL file into individual statements on
// semicolons. Strips `--` line comments first so a `;` inside a
// comment doesn't accidentally split a statement. The schema we
// ship has no string literals containing `;`, so this two-step
// approach is safe; a real SQL parser would be overkill for a
// single trusted DDL file.
func splitStatements(src string) []string {
	stripped := stripLineComments(src)
	parts := strings.Split(stripped, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// stripLineComments removes everything from `--` to end-of-line on
// every line. We don't try to handle `--` inside string literals
// because the schema doesn't have any; the parser is deliberately
// minimal.
func stripLineComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// firstLine returns the first non-empty line of stmt, used in
// error messages so operators can locate the failing DDL.
func firstLine(stmt string) string {
	for _, line := range strings.Split(stmt, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return stmt
}

// inPlaceholders builds a `(?, ?, ?)` clause matching the slice
// length, returning the placeholder string and a []any of values
// pre-typed for sql.DB.ExecContext / QueryContext. Used by every
// `WHERE id IN (...)` predicate since MySQL has no array param.
func inPlaceholders[T any](xs []T) (string, []any) {
	if len(xs) == 0 {
		return "", nil
	}
	parts := make([]string, len(xs))
	args := make([]any, len(xs))
	for i, x := range xs {
		parts[i] = "?"
		args[i] = x
	}
	return strings.Join(parts, ","), args
}
