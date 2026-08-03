package sqlite

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// open creates or opens a SQLite database at the given path, ensuring the
// parent directory and schema exist.
func open(path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("database path is required")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	// Reliability PRAGMAs are passed via the DSN so they apply to
	// every connection the pool opens, not just the first one.
	// Setting them with db.Exec only affects whichever pooled conn
	// happened to handle that statement — every subsequent goroutine
	// might land on a fresh conn without WAL or busy_timeout, which
	// breaks concurrent writes (we hit this in the auth stress test).
	//
	//   foreign_keys=ON: schema-level FK constraints aren't enforced
	//     without this — SQLite's default is OFF for back-compat.
	//   journal_mode=WAL: lets readers and a single writer coexist;
	//     without it the whole file serialises and concurrent token
	//     issuance fails with SQLITE_BUSY.
	//   busy_timeout=5000: wait up to 5s for the writer lock before
	//     returning SQLITE_BUSY. Friendlier than immediate failure
	//     for short bursts of contention.
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	if err := ensureSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

// Bootstrap applies the SQLite schema and runs migrations against
// any database that speaks SQLite's SQL dialect. The libSQL provider
// reuses this on every Open since libSQL is wire-compatible with
// SQLite and the same schema works unchanged. ensureSchema already
// calls migrate, so this is a thin alias kept exported for cross-
// package callers.
func Bootstrap(db *sql.DB) error {
	return ensureSchema(db)
}

func ensureSchema(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS events (
	id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL,
	schema_version TEXT NOT NULL,
	content TEXT NOT NULL,
	source_input_id TEXT NOT NULL,
	timestamp TEXT NOT NULL,
	metadata_json TEXT NOT NULL,
	ingested_at TEXT NOT NULL,
	created_by TEXT NOT NULL DEFAULT '<system>'
);

CREATE INDEX IF NOT EXISTS idx_events_timestamp ON events(timestamp);
CREATE INDEX IF NOT EXISTS idx_events_source_input_id ON events(source_input_id);
CREATE INDEX IF NOT EXISTS idx_events_run_id ON events(run_id);

CREATE TABLE IF NOT EXISTS claims (
	id TEXT PRIMARY KEY,
	text TEXT NOT NULL,
	type TEXT NOT NULL,
	confidence REAL NOT NULL,
	status TEXT NOT NULL,
	created_at TEXT NOT NULL,
	created_by TEXT NOT NULL DEFAULT '<system>',
	trust_score REAL NOT NULL DEFAULT 0,
	valid_from TEXT NOT NULL DEFAULT '',
	valid_to TEXT,
	last_verified TEXT NOT NULL DEFAULT '',
	verify_count INTEGER NOT NULL DEFAULT 0,
	half_life_days REAL NOT NULL DEFAULT 0,
	-- half_life_classifier (ADR 0025): which classifier assigned half_life_days.
	-- '' means none did. That is a different fact from "the classifier read this
	-- and judged it durable", which also stores 0.
	half_life_classifier TEXT NOT NULL DEFAULT '',
	scope_service TEXT NOT NULL DEFAULT '',
	scope_env TEXT NOT NULL DEFAULT '',
	scope_team TEXT NOT NULL DEFAULT '',
	source_document TEXT NOT NULL DEFAULT '',
	source_type TEXT NOT NULL DEFAULT '',
	source_authority REAL NOT NULL DEFAULT 0,
	liveness TEXT NOT NULL DEFAULT '',
	last_executed TEXT NOT NULL DEFAULT '',
	citation_count INTEGER NOT NULL DEFAULT 0,
	provenance_rationale TEXT NOT NULL DEFAULT '',
	test_id TEXT NOT NULL DEFAULT '',
	test_requirement_ref TEXT NOT NULL DEFAULT '',
	test_author TEXT NOT NULL DEFAULT '',
	test_last_modified TEXT NOT NULL DEFAULT '',
	test_last_run_at TEXT NOT NULL DEFAULT '',
	test_pass_count INTEGER NOT NULL DEFAULT 0,
	test_fail_count INTEGER NOT NULL DEFAULT 0,
	subject_class TEXT NOT NULL DEFAULT '',
	-- durability (ADR 0023): 'durable' | 'session' | '' (unknown). Unknown is
	-- treated as durable: the whole back catalogue predates the column.
	durability TEXT NOT NULL DEFAULT ''
);
-- idx_claims_trust_score and idx_claims_valid_to are created by
-- migrate() after the v1→v2 / v2→v3 ALTER TABLEs add the columns on
-- legacy DBs. Defining them here would run before the column exists.

CREATE TABLE IF NOT EXISTS claim_evidence (
	claim_id TEXT NOT NULL,
	event_id TEXT NOT NULL,
	PRIMARY KEY (claim_id, event_id),
	FOREIGN KEY (claim_id) REFERENCES claims(id)
);

CREATE INDEX IF NOT EXISTS idx_claim_evidence_event_id ON claim_evidence(event_id);

CREATE TABLE IF NOT EXISTS relationships (
	id TEXT PRIMARY KEY,
	type TEXT NOT NULL,
	from_claim_id TEXT NOT NULL,
	to_claim_id TEXT NOT NULL,
	created_at TEXT NOT NULL,
	created_by TEXT NOT NULL DEFAULT '<system>',
	FOREIGN KEY (from_claim_id) REFERENCES claims(id),
	FOREIGN KEY (to_claim_id) REFERENCES claims(id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_relationships_unique_edge
	ON relationships(type, from_claim_id, to_claim_id);
CREATE INDEX IF NOT EXISTS idx_relationships_from_claim ON relationships(from_claim_id);
CREATE INDEX IF NOT EXISTS idx_relationships_to_claim ON relationships(to_claim_id);

-- cognitive_journal (ADR 0018): append-only learning log; data is a kind-specific JSON payload.
CREATE TABLE IF NOT EXISTS cognitive_journal (
	id TEXT PRIMARY KEY,
	at TEXT NOT NULL,
	run_id TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL,
	subject_id TEXT NOT NULL DEFAULT '',
	data TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_cognitive_journal_kind_at ON cognitive_journal(kind, at);
CREATE INDEX IF NOT EXISTS idx_cognitive_journal_subject ON cognitive_journal(subject_id);

CREATE TABLE IF NOT EXISTS compilation_jobs (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	status TEXT NOT NULL,
	scope_json TEXT NOT NULL,
	started_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	error TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_compilation_jobs_kind ON compilation_jobs(kind);
CREATE INDEX IF NOT EXISTS idx_compilation_jobs_status ON compilation_jobs(status);

CREATE TABLE IF NOT EXISTS claim_status_history (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	claim_id TEXT NOT NULL,
	from_status TEXT NOT NULL,
	to_status TEXT NOT NULL,
	changed_at TEXT NOT NULL,
	reason TEXT NOT NULL,
	changed_by TEXT NOT NULL DEFAULT '<system>',
	FOREIGN KEY (claim_id) REFERENCES claims(id)
);

CREATE INDEX IF NOT EXISTS idx_claim_status_history_claim_id ON claim_status_history(claim_id);
CREATE INDEX IF NOT EXISTS idx_claim_status_history_changed_at ON claim_status_history(changed_at);

-- claim_versions (Refs #38) — append-only audit trail of every text /
-- confidence / status snapshot a claim has held. Side table so the
-- claim row itself stays slim; consumers asking for the timeline JOIN
-- on claim_id.
CREATE TABLE IF NOT EXISTS claim_versions (
	claim_id TEXT NOT NULL,
	version INTEGER NOT NULL,
	text TEXT NOT NULL,
	confidence REAL NOT NULL,
	status TEXT NOT NULL,
	written_at TEXT NOT NULL,
	written_by TEXT NOT NULL DEFAULT '<system>',
	PRIMARY KEY (claim_id, version),
	FOREIGN KEY (claim_id) REFERENCES claims(id)
);

CREATE INDEX IF NOT EXISTS idx_claim_versions_claim_id ON claim_versions(claim_id);

-- claim_feedback (Refs #40) — per-claim feedback state. Side table so
-- the claim list/read hot paths don't pay a wider row for a relatively
-- cold field. Schema is small and idempotent; feedback handler
-- upserts on (claim_id) primary key.
CREATE TABLE IF NOT EXISTS claim_feedback (
	claim_id TEXT PRIMARY KEY,
	negative_feedback_streak INTEGER NOT NULL DEFAULT 0,
	helpful_count INTEGER NOT NULL DEFAULT 0,
	last_feedback_at TEXT NOT NULL DEFAULT '',
	last_feedback_note TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (claim_id) REFERENCES claims(id)
);

-- working_memory_blocks (T3.1) — an agent's "core memory": bounded, labeled,
-- mutable text keyed by (owner, label). Side table, independent of claims;
-- the caller enforces the size cap. Upserts on the (owner, label) primary key.
CREATE TABLE IF NOT EXISTS working_memory_blocks (
	owner TEXT NOT NULL,
	label TEXT NOT NULL,
	content TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (owner, label)
);

-- claim_expectations (T1.1) — one structured forward prediction per claim,
-- reconciled against an observed value into a validates/refutes verdict.
CREATE TABLE IF NOT EXISTS claim_expectations (
	claim_id TEXT PRIMARY KEY,
	predicted REAL NOT NULL DEFAULT 0,
	tolerance REAL NOT NULL DEFAULT 0,
	horizon TEXT NOT NULL DEFAULT '',
	observed REAL NOT NULL DEFAULT 0,
	has_observation INTEGER NOT NULL DEFAULT 0,
	resolved INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS embeddings (
	entity_id TEXT NOT NULL,
	entity_type TEXT NOT NULL,
	vector BLOB NOT NULL,
	model TEXT NOT NULL,
	dimensions INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	created_by TEXT NOT NULL DEFAULT '<system>',
	PRIMARY KEY (entity_id, entity_type)
);

CREATE INDEX IF NOT EXISTS idx_embeddings_entity_type ON embeddings(entity_type);

CREATE TABLE IF NOT EXISTS users (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	email TEXT NOT NULL UNIQUE,
	status TEXT NOT NULL DEFAULT 'active',
	scopes_json TEXT NOT NULL DEFAULT '["*"]',
	created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_users_status ON users(status);

CREATE TABLE IF NOT EXISTS revoked_tokens (
	jti TEXT PRIMARY KEY,
	revoked_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_revoked_tokens_expires_at ON revoked_tokens(expires_at);

CREATE TABLE IF NOT EXISTS agents (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	owner_id TEXT NOT NULL,
	scopes_json TEXT NOT NULL DEFAULT '[]',
	allowed_runs_json TEXT NOT NULL DEFAULT '[]',
	status TEXT NOT NULL DEFAULT 'active',
	created_at TEXT NOT NULL,
	FOREIGN KEY (owner_id) REFERENCES users(id)
);

CREATE INDEX IF NOT EXISTS idx_agents_owner_id ON agents(owner_id);
CREATE INDEX IF NOT EXISTS idx_agents_status ON agents(status);

CREATE TABLE IF NOT EXISTS entities (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	normalized_name TEXT NOT NULL,
	type TEXT NOT NULL,
	created_at TEXT NOT NULL,
	created_by TEXT NOT NULL DEFAULT '<system>',
	UNIQUE(normalized_name, type)
);

CREATE INDEX IF NOT EXISTS idx_entities_normalized_name ON entities(normalized_name);
CREATE INDEX IF NOT EXISTS idx_entities_type ON entities(type);

CREATE TABLE IF NOT EXISTS claim_entities (
	claim_id TEXT NOT NULL,
	entity_id TEXT NOT NULL,
	role TEXT NOT NULL DEFAULT 'mention',
	PRIMARY KEY (claim_id, entity_id, role),
	FOREIGN KEY (claim_id) REFERENCES claims(id),
	FOREIGN KEY (entity_id) REFERENCES entities(id)
);

CREATE INDEX IF NOT EXISTS idx_claim_entities_entity_id ON claim_entities(entity_id);

-- The 'porter unicode61' tokenizer chains Porter stemming on top of the
-- default unicode61 behaviour (case-fold + diacritic-fold), so inflected
-- forms collapse to a common stem: a claim stored as "issues automatic
-- refunds" is recalled by the query "issue a refund". Without stemming,
-- FTS5 matches token-for-token and paraphrases that differ only by
-- inflection miss entirely. The tokenizer is fixed at table-creation
-- time, so upgrading an existing DB rebuilds these tables (see migrate()).
CREATE VIRTUAL TABLE IF NOT EXISTS events_fts USING fts5(event_id UNINDEXED, content, tokenize = 'porter unicode61');
CREATE VIRTUAL TABLE IF NOT EXISTS claims_fts USING fts5(claim_id UNINDEXED, text, tokenize = 'porter unicode61');

CREATE TRIGGER IF NOT EXISTS events_ai_fts AFTER INSERT ON events BEGIN
	INSERT INTO events_fts(event_id, content) VALUES (new.id, new.content);
END;
CREATE TRIGGER IF NOT EXISTS events_ad_fts AFTER DELETE ON events BEGIN
	DELETE FROM events_fts WHERE event_id = old.id;
END;
CREATE TRIGGER IF NOT EXISTS events_au_fts AFTER UPDATE OF content ON events BEGIN
	UPDATE events_fts SET content = new.content WHERE event_id = old.id;
END;

CREATE TRIGGER IF NOT EXISTS claims_ai_fts AFTER INSERT ON claims BEGIN
	INSERT INTO claims_fts(claim_id, text) VALUES (new.id, new.text);
END;
CREATE TRIGGER IF NOT EXISTS claims_ad_fts AFTER DELETE ON claims BEGIN
	DELETE FROM claims_fts WHERE claim_id = old.id;
END;
CREATE TRIGGER IF NOT EXISTS claims_au_fts AFTER UPDATE OF text ON claims BEGIN
	UPDATE claims_fts SET text = new.text WHERE claim_id = old.id;
END;

-- Phase 2: actions + outcomes
CREATE TABLE IF NOT EXISTS actions (
	id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL,
	subject TEXT NOT NULL,
	actor TEXT NOT NULL DEFAULT '',
	at TEXT NOT NULL,
	metadata_json TEXT NOT NULL DEFAULT '{}',
	created_by TEXT NOT NULL DEFAULT '<system>',
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_actions_run_id ON actions(run_id);
CREATE INDEX IF NOT EXISTS idx_actions_subject ON actions(subject);
CREATE INDEX IF NOT EXISTS idx_actions_kind ON actions(kind);
CREATE INDEX IF NOT EXISTS idx_actions_at ON actions(at);

CREATE TABLE IF NOT EXISTS outcomes (
	id TEXT PRIMARY KEY,
	action_id TEXT NOT NULL,
	result TEXT NOT NULL,
	metrics_json TEXT NOT NULL DEFAULT '{}',
	notes TEXT NOT NULL DEFAULT '',
	observed_at TEXT NOT NULL,
	source TEXT NOT NULL DEFAULT 'push',
	created_by TEXT NOT NULL DEFAULT '<system>',
	created_at TEXT NOT NULL,
	FOREIGN KEY (action_id) REFERENCES actions(id)
);
CREATE INDEX IF NOT EXISTS idx_outcomes_action_id ON outcomes(action_id);
CREATE INDEX IF NOT EXISTS idx_outcomes_result ON outcomes(result);
CREATE INDEX IF NOT EXISTS idx_outcomes_observed_at ON outcomes(observed_at);

-- Phase 3: lessons + lesson_evidence
CREATE TABLE IF NOT EXISTS lessons (
	id TEXT PRIMARY KEY,
	statement TEXT NOT NULL,
	scope_service TEXT NOT NULL DEFAULT '',
	scope_env TEXT NOT NULL DEFAULT '',
	scope_team TEXT NOT NULL DEFAULT '',
	trigger TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL DEFAULT '',
	confidence REAL NOT NULL,
	derived_at TEXT NOT NULL,
	last_verified TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT 'synthesize',
	created_by TEXT NOT NULL DEFAULT '<system>',
	polarity TEXT NOT NULL DEFAULT '',
	subject_class TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_lessons_scope_service ON lessons(scope_service);
CREATE INDEX IF NOT EXISTS idx_lessons_scope_env ON lessons(scope_env);
CREATE INDEX IF NOT EXISTS idx_lessons_scope_team ON lessons(scope_team);
CREATE INDEX IF NOT EXISTS idx_lessons_kind ON lessons(kind);
CREATE INDEX IF NOT EXISTS idx_lessons_trigger ON lessons(trigger);
CREATE INDEX IF NOT EXISTS idx_lessons_confidence ON lessons(confidence);

CREATE TABLE IF NOT EXISTS lesson_evidence (
	lesson_id TEXT NOT NULL,
	action_id TEXT NOT NULL,
	PRIMARY KEY (lesson_id, action_id),
	FOREIGN KEY (lesson_id) REFERENCES lessons(id),
	FOREIGN KEY (action_id) REFERENCES actions(id)
);
CREATE INDEX IF NOT EXISTS idx_lesson_evidence_action_id ON lesson_evidence(action_id);

-- Phase 5: decisions + decision_beliefs
CREATE TABLE IF NOT EXISTS decisions (
	id TEXT PRIMARY KEY,
	statement TEXT NOT NULL,
	plan TEXT NOT NULL DEFAULT '',
	reasoning TEXT NOT NULL DEFAULT '',
	risk_level TEXT NOT NULL,
	alternatives_json TEXT NOT NULL DEFAULT '[]',
	outcome_id TEXT NOT NULL DEFAULT '',
	chosen_at TEXT NOT NULL,
	created_by TEXT NOT NULL DEFAULT '<system>',
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_decisions_chosen_at ON decisions(chosen_at);
CREATE INDEX IF NOT EXISTS idx_decisions_risk_level ON decisions(risk_level);
CREATE INDEX IF NOT EXISTS idx_decisions_outcome_id ON decisions(outcome_id);

CREATE TABLE IF NOT EXISTS decision_beliefs (
	decision_id TEXT NOT NULL,
	claim_id TEXT NOT NULL,
	PRIMARY KEY (decision_id, claim_id),
	FOREIGN KEY (decision_id) REFERENCES decisions(id),
	FOREIGN KEY (claim_id) REFERENCES claims(id)
);
CREATE INDEX IF NOT EXISTS idx_decision_beliefs_claim_id ON decision_beliefs(claim_id);

-- Phase 6: playbooks + playbook_lessons
CREATE TABLE IF NOT EXISTS playbooks (
	id TEXT PRIMARY KEY,
	trigger TEXT NOT NULL,
	statement TEXT NOT NULL,
	scope_service TEXT NOT NULL DEFAULT '',
	scope_env TEXT NOT NULL DEFAULT '',
	scope_team TEXT NOT NULL DEFAULT '',
	steps_json TEXT NOT NULL DEFAULT '[]',
	confidence REAL NOT NULL,
	derived_at TEXT NOT NULL,
	last_verified TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT 'synthesize',
	created_by TEXT NOT NULL DEFAULT '<system>'
);
CREATE INDEX IF NOT EXISTS idx_playbooks_trigger ON playbooks(trigger);
CREATE INDEX IF NOT EXISTS idx_playbooks_scope_service ON playbooks(scope_service);
CREATE INDEX IF NOT EXISTS idx_playbooks_confidence ON playbooks(confidence);

CREATE TABLE IF NOT EXISTS playbook_lessons (
	playbook_id TEXT NOT NULL,
	lesson_id TEXT NOT NULL,
	PRIMARY KEY (playbook_id, lesson_id),
	FOREIGN KEY (playbook_id) REFERENCES playbooks(id),
	FOREIGN KEY (lesson_id) REFERENCES lessons(id)
);
CREATE INDEX IF NOT EXISTS idx_playbook_lessons_lesson_id ON playbook_lessons(lesson_id);

-- Phase 7 follow-up: system-versioned snapshot tables.
CREATE TABLE IF NOT EXISTS lesson_versions (
	version_id INTEGER PRIMARY KEY AUTOINCREMENT,
	lesson_id TEXT NOT NULL,
	payload_json TEXT NOT NULL,
	valid_from TEXT NOT NULL,
	valid_to TEXT NOT NULL,
	FOREIGN KEY (lesson_id) REFERENCES lessons(id)
);
CREATE INDEX IF NOT EXISTS idx_lesson_versions_lesson_id ON lesson_versions(lesson_id);

CREATE TABLE IF NOT EXISTS playbook_versions (
	version_id INTEGER PRIMARY KEY AUTOINCREMENT,
	playbook_id TEXT NOT NULL,
	payload_json TEXT NOT NULL,
	valid_from TEXT NOT NULL,
	valid_to TEXT NOT NULL,
	FOREIGN KEY (playbook_id) REFERENCES playbooks(id)
);
CREATE INDEX IF NOT EXISTS idx_playbook_versions_playbook_id ON playbook_versions(playbook_id);

CREATE TABLE IF NOT EXISTS entity_relationships (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	from_id TEXT NOT NULL,
	from_type TEXT NOT NULL,
	to_id TEXT NOT NULL,
	to_type TEXT NOT NULL,
	created_at TEXT NOT NULL,
	created_by TEXT NOT NULL DEFAULT '<system>'
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_entity_relationships_unique_edge
	ON entity_relationships(kind, from_type, from_id, to_type, to_id);
CREATE INDEX IF NOT EXISTS idx_entity_relationships_from ON entity_relationships(from_type, from_id);
CREATE INDEX IF NOT EXISTS idx_entity_relationships_to ON entity_relationships(to_type, to_id);

-- Phase 9: incidents
CREATE TABLE IF NOT EXISTS incidents (
	id TEXT PRIMARY KEY,
	title TEXT NOT NULL,
	summary TEXT NOT NULL DEFAULT '',
	severity TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'open',
	timeline_event_ids_json TEXT NOT NULL DEFAULT '[]',
	root_cause_claim_id TEXT NOT NULL DEFAULT '',
	decision_ids_json TEXT NOT NULL DEFAULT '[]',
	outcome_ids_json TEXT NOT NULL DEFAULT '[]',
	playbook_id TEXT NOT NULL DEFAULT '',
	opened_at TEXT NOT NULL,
	resolved_at TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL DEFAULT '<system>'
);
CREATE INDEX IF NOT EXISTS idx_incidents_severity ON incidents(severity);
CREATE INDEX IF NOT EXISTS idx_incidents_status ON incidents(status);
CREATE INDEX IF NOT EXISTS idx_incidents_opened_at ON incidents(opened_at);
CREATE INDEX IF NOT EXISTS idx_incidents_root_cause_claim_id ON incidents(root_cause_claim_id);

-- ADR 0011 Phase B: global_schemas — the neocortex (shared/global) tier. A
-- promoted schema is a de-identified generalization corroborated by N distinct
-- tenants; its provenance is COUNTS (distinct_tenants, evidence_count), never
-- tenant ids or raw evidence ids. This is the INVERSE of the ADR-0007 per-tenant
-- gotcha: the table is intentionally shared and carries NO tenant column, so on
-- Postgres it is deliberately excluded from the RLS scoped array (see
-- internal/store/postgres/schema.sql).
CREATE TABLE IF NOT EXISTS global_schemas (
	id TEXT PRIMARY KEY,
	statement TEXT NOT NULL,
	scope_service TEXT NOT NULL DEFAULT '',
	scope_env TEXT NOT NULL DEFAULT '',
	scope_team TEXT NOT NULL DEFAULT '',
	polarity TEXT NOT NULL DEFAULT '',
	distinct_tenants INTEGER NOT NULL DEFAULT 0,
	evidence_count INTEGER NOT NULL DEFAULT 0,
	confidence REAL NOT NULL DEFAULT 0,
	surprise REAL NOT NULL DEFAULT 0,
	has_surprise INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL DEFAULT 'pending',
	promoted_at TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL DEFAULT '<system>'
);
CREATE INDEX IF NOT EXISTS idx_global_schemas_status ON global_schemas(status);
CREATE INDEX IF NOT EXISTS idx_global_schemas_promoted_at ON global_schemas(promoted_at);
`

	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("ensure schema: %w", err)
	}

	if err := migrate(db); err != nil {
		return fmt.Errorf("schema migration: %w", err)
	}

	return nil
}

// currentSchemaVersion is the schema generation this binary expects.
// Bump whenever a column or table is added; pair the bump with a step
// in addMissingColumns so existing DBs upgrade in place.
// v19 (ADR 0011 Phase B) adds the global_schemas table (neocortex tier). It is
// a new table, so it auto-creates via CREATE TABLE IF NOT EXISTS above and needs
// no column-add entry; the version bump keeps user_version in step.
// v20 (ADR 0012) adds claims.subject_class so the claim-derived knowledge
// promotion path can read back a claim's SubjectClass — paired with the
// column-add entry in expectedColumns below.
// v21 (ADR 0015 §4) adds relationships.strength — the Hebbian co-activation weight.
// Existing edges backfill to the base 1.0 via the DEFAULT, so pre-strength graphs are
// neutral until co-retrieval strengthens them.
// v22 (ADR 0018) adds the cognitive_journal table (append-only learning log). It is a
// new table, so it auto-creates via CREATE TABLE IF NOT EXISTS and needs no column-add
// v23 (ADR 0023) adds claims.durability so a belief can record whether its value
// outlives the session that produced it. Unknown (” from the DEFAULT) is treated
// as durable everywhere, so every existing row keeps behaving exactly as before.
// entry; the bump keeps user_version in step.
// v24 adds the index idx_claims_test_requirement_ref. No new column, but
// the bump is REQUIRED: migrate() returns early when user_version is already at
// currentSchemaVersion, so without it postMigrateIndexes never runs again and
// every existing brain would keep full-scanning claims for a test lookup.
// v25 (ADR 0025) adds claims.half_life_classifier — which classifier assigned the
// half-life, so "judged durable" stops being indistinguishable from "never examined".
// Paired with the expectedColumns entry below, and the bump is what ACTIVATES it:
// migrate() returns early once user_version has reached currentSchemaVersion, so an
// expectedColumns entry without a bump is inert on exactly the brains that need it —
// every pre-existing one — and the first read fails with "no such column". Caught by
// running the new binary against a copy of a real 88k-belief brain; a fresh test DB
// gets the column from CREATE TABLE and never exercises this path.
const currentSchemaVersion = 25

// addMissingColumn declares one defensive column-add. Each entry is
// idempotent: if the column already exists in the table we skip it,
// so re-running the migration on an up-to-date DB is a no-op.
type addMissingColumn struct {
	table  string
	column string
	def    string // full column definition appended after ADD COLUMN
}

// expectedColumns is the set of columns added after the v0.5 baseline
// across all schema generations this binary knows about. Each entry
// is added defensively: pre-existing tables don't pick up new columns
// from CREATE TABLE IF NOT EXISTS, so v0.5→v0.6+ upgrades would
// otherwise fail with the cryptic "table events has no column named
// created_by" on the next write.
//
// The order matters only when a later column references an earlier
// one (none today). Newest schema generation last is fine.
var expectedColumns = []addMissingColumn{
	// v1 — auth-era audit columns.
	{"events", "created_by", "TEXT NOT NULL DEFAULT '<system>'"},
	{"claims", "created_by", "TEXT NOT NULL DEFAULT '<system>'"},
	{"relationships", "created_by", "TEXT NOT NULL DEFAULT '<system>'"},
	{"claim_status_history", "changed_by", "TEXT NOT NULL DEFAULT '<system>'"},
	{"embeddings", "created_by", "TEXT NOT NULL DEFAULT '<system>'"},
	// v2 — derived trust score.
	{"claims", "trust_score", "REAL NOT NULL DEFAULT 0"},
	// v3 — temporal validity. valid_from gets backfilled from
	// created_at after the column is added (see migrate()), so
	// existing claims become "valid since they were created" — the
	// most defensible default for a binary that didn't track this
	// before. valid_to is nullable; NULL means "no upper bound /
	// still valid".
	{"claims", "valid_from", "TEXT NOT NULL DEFAULT ''"},
	{"claims", "valid_to", "TEXT"},
	// v6 — temporal validity hardening: per-claim last_verified
	// (RFC3339 string, empty until a `mnemos verify` lands), an
	// integer verify_count, and a per-claim half_life_days override
	// of the default in internal/trust. Zero half_life_days falls
	// back to FreshnessHalfLifeDays at scoring time.
	{"claims", "last_verified", "TEXT NOT NULL DEFAULT ''"},
	{"claims", "verify_count", "INTEGER NOT NULL DEFAULT 0"},
	{"claims", "half_life_days", "REAL NOT NULL DEFAULT 0"},
	// v7 — Phase 8 multi-tenant scope. Three lightweight TEXT
	// columns instead of a JSON blob so SQLite can index them
	// without json_extract acrobatics.
	{"claims", "scope_service", "TEXT NOT NULL DEFAULT ''"},
	{"claims", "scope_env", "TEXT NOT NULL DEFAULT ''"},
	{"claims", "scope_team", "TEXT NOT NULL DEFAULT ''"},
	// v8 — epistemic provenance fields.
	{"claims", "source_document", "TEXT NOT NULL DEFAULT ''"},
	{"claims", "source_type", "TEXT NOT NULL DEFAULT ''"},
	{"claims", "source_authority", "REAL NOT NULL DEFAULT 0"},
	{"claims", "liveness", "TEXT NOT NULL DEFAULT ''"},
	{"claims", "last_executed", "TEXT NOT NULL DEFAULT ''"},
	{"claims", "citation_count", "INTEGER NOT NULL DEFAULT 0"},
	{"claims", "provenance_rationale", "TEXT NOT NULL DEFAULT ''"},
	// v9 — test provenance fields.
	{"claims", "test_id", "TEXT NOT NULL DEFAULT ''"},
	{"claims", "test_requirement_ref", "TEXT NOT NULL DEFAULT ''"},
	{"claims", "test_author", "TEXT NOT NULL DEFAULT ''"},
	{"claims", "test_last_modified", "TEXT NOT NULL DEFAULT ''"},
	{"claims", "test_last_run_at", "TEXT NOT NULL DEFAULT ''"},
	{"claims", "test_pass_count", "INTEGER NOT NULL DEFAULT 0"},
	{"claims", "test_fail_count", "INTEGER NOT NULL DEFAULT 0"},
	{"decisions", "scope_service", "TEXT NOT NULL DEFAULT ''"},
	{"decisions", "scope_env", "TEXT NOT NULL DEFAULT ''"},
	{"decisions", "scope_team", "TEXT NOT NULL DEFAULT ''"},
	// v10 — visibility field (personal / team / org).
	{"claims", "visibility", "TEXT NOT NULL DEFAULT 'team'"},
	// v13 — confidence decomposition (Refs #39). JSON column carrying
	// named components ("data_quality", "corroboration", ...) so
	// downstream consumers can weight contributors. Empty object
	// means "no decomposition surfaced".
	{"claims", "confidence_components", "TEXT NOT NULL DEFAULT '{}'"},
	// v17: claim curation lifecycle (candidate/promoted/superseded).
	{"claims", "lifecycle", "TEXT NOT NULL DEFAULT ''"},
	// v14: claim_feedback (Refs #40) — side table; no claim-column
	// adds. The CREATE TABLE for it lives next to the other CREATE
	// statements above (auto-applies via CREATE TABLE IF NOT EXISTS),
	// so this list intentionally has no v14 row.
	// v11 — lesson polarity (positive / negative anti-lesson).
	{"lessons", "polarity", "TEXT NOT NULL DEFAULT ''"},
	// v12 — decision audit trail: refuted beliefs + failed outcome link.
	{"decisions", "refuted_beliefs_json", "TEXT NOT NULL DEFAULT '[]'"},
	{"decisions", "failed_outcome_id", "TEXT NOT NULL DEFAULT ''"},
	// v18 - schema subject class (ADR 0012): 'individual' | 'class' | ''
	// (unknown). Only class-level schemas may promote to the global brain.
	{"lessons", "subject_class", "TEXT NOT NULL DEFAULT ''"},
	// v20 - claim subject class (ADR 0012): the belief-level analogue of the
	// lessons column above. Extraction sets domain.Claim.SubjectClass; without
	// this column persist would drop it and claims read back 'unknown',
	// leaving the claim-derived knowledge promotion path inert.
	{"claims", "subject_class", "TEXT NOT NULL DEFAULT ''"},
	// v21 - Hebbian co-activation weight (ADR 0015 §4). Existing edges backfill to
	// the base 1.0, so spreading activation is unchanged until StrengthenAssociations
	// raises an edge from repeated co-retrieval.
	{"relationships", "strength", "REAL NOT NULL DEFAULT 1"},
	// v23 - claim durability (ADR 0023): 'durable' | 'session' | '' (unknown).
	// CREATE TABLE IF NOT EXISTS does not add columns to a table that already
	// exists, so without this entry every pre-existing brain would fail on the
	// first read with "no such column: durability".
	{"claims", "durability", "TEXT NOT NULL DEFAULT ''"},
	// v24 - half-life classification provenance (ADR 0025). Without this entry
	// every pre-existing brain fails its first read with "no such column", the
	// same trap durability documents above: CREATE TABLE IF NOT EXISTS does not
	// add columns to a table that already exists.
	{"claims", "half_life_classifier", "TEXT NOT NULL DEFAULT ''"},
}

// v1Columns is the legacy alias kept for any external callers (and for
// tests that assert legacy migration behavior). New work should append
// to expectedColumns instead.
var v1Columns = expectedColumns

// migrate applies every column-add this binary knows about to bring an
// older DB up to currentSchemaVersion. It is invoked after ensureSchema
// runs the CREATE TABLE statements; new tables added to the schema
// take care of themselves via CREATE TABLE IF NOT EXISTS, but added
// columns on pre-existing tables need ALTER TABLE.
//
// Strategy: for every (table, column) the binary expects, query
// PRAGMA table_info and ALTER TABLE ADD COLUMN only if missing. This
// avoids tracking a brittle linear sequence of migrations — the only
// state we need is "what columns does this DB have right now". After
// every ALTER succeeds we bump PRAGMA user_version so future binaries
// can spot a baseline and skip the column probes when possible.
func migrate(db *sql.DB) error {
	var userVersion int
	if err := db.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	// The column probes run UNCONDITIONALLY, ahead of the version gate.
	//
	// They used to sit behind `userVersion >= currentSchemaVersion { return nil }`,
	// which made an expectedColumns entry inert unless its author also remembered
	// to bump currentSchemaVersion — and inert on precisely the databases that
	// needed it, since a brain from the previous release already sits AT the
	// current version. The failure is total rather than partial: the first read
	// fails with "no such column" and the brain is unopenable.
	//
	// Two things made it invisible. A fresh test database gets every column from
	// CREATE TABLE and never exercises this path, and the pairing is a convention
	// held in a comment rather than anything the compiler or a test can check.
	// ADR 0025 shipped with the entry and without the bump; it was caught by
	// running the binary against a copy of a real 88k-belief brain, not by CI.
	//
	// This restores what the function's own strategy note above already claims:
	// the only state needed is "what columns does this DB have right now". The
	// cost is one PRAGMA table_info per entry per open — microseconds, and only
	// on the open path.
	for _, c := range expectedColumns {
		has, err := columnExists(db, c.table, c.column)
		if err != nil {
			return fmt.Errorf("inspect %s.%s: %w", c.table, c.column, err)
		}
		if has {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.def)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("add %s.%s: %w", c.table, c.column, err)
		}
	}

	// The version gate still guards everything below: the one-shot data
	// migrations must not re-run, and the index creation is version-keyed.
	if userVersion >= currentSchemaVersion {
		return nil
	}

	// One-shot data migrations that depend on the columns existing.
	// Each is idempotent (filtering on the empty-string sentinel
	// produced by ALTER TABLE ADD COLUMN ... DEFAULT '') so re-running
	// is safe. Backfills run only when the previous user_version was
	// below the version that introduced their target column.
	if userVersion < 3 {
		// valid_from defaults to the claim's created_at on legacy
		// rows. New inserts will set valid_from explicitly via the
		// pipeline; this only catches rows that predate v0.8.
		if _, err := db.Exec(`UPDATE claims SET valid_from = created_at WHERE valid_from = ''`); err != nil {
			return fmt.Errorf("backfill claims.valid_from: %w", err)
		}
	}

	if userVersion < 5 {
		// Backfill the FTS5 indexes. The triggers in the bootstrap
		// schema keep them current going forward; this catches every
		// row that existed before v0.10. The empty-target check makes
		// re-runs idempotent (re-applying schema on a v5 DB sees the
		// FTS table already populated and adds nothing).
		if _, err := db.Exec(`INSERT INTO events_fts(event_id, content)
			SELECT id, content FROM events
			WHERE id NOT IN (SELECT event_id FROM events_fts)`); err != nil {
			return fmt.Errorf("backfill events_fts: %w", err)
		}
		if _, err := db.Exec(`INSERT INTO claims_fts(claim_id, text)
			SELECT id, text FROM claims
			WHERE id NOT IN (SELECT claim_id FROM claims_fts)`); err != nil {
			return fmt.Errorf("backfill claims_fts: %w", err)
		}
	}

	if userVersion < 18 {
		// v18 — rebuild the FTS5 tables with the 'porter unicode61'
		// tokenizer so recall stems inflected forms (refund/refunds,
		// issue/issues). A tokenizer is fixed at table-creation time, so
		// the only way to change it on an existing DB is to drop and
		// recreate the virtual tables, then repopulate from the base
		// tables. The AFTER-INSERT/DELETE/UPDATE triggers bind to the
		// tables by name and keep working across the recreate. Idempotent:
		// re-running drops and rebuilds from the same source rows.
		const rebuildFTS = `
DROP TABLE IF EXISTS events_fts;
DROP TABLE IF EXISTS claims_fts;
CREATE VIRTUAL TABLE events_fts USING fts5(event_id UNINDEXED, content, tokenize = 'porter unicode61');
CREATE VIRTUAL TABLE claims_fts USING fts5(claim_id UNINDEXED, text, tokenize = 'porter unicode61');
INSERT INTO events_fts(event_id, content) SELECT id, content FROM events;
INSERT INTO claims_fts(claim_id, text) SELECT id, text FROM claims;
`
		if _, err := db.Exec(rebuildFTS); err != nil {
			return fmt.Errorf("rebuild FTS with porter tokenizer: %w", err)
		}
	}

	// Indexes that depend on migrated columns. Run after the column
	// adds above so legacy DBs don't fail with "no such column".
	const postMigrateIndexes = `
CREATE INDEX IF NOT EXISTS idx_claims_trust_score ON claims(trust_score);
CREATE INDEX IF NOT EXISTS idx_claims_valid_to ON claims(valid_to);
CREATE INDEX IF NOT EXISTS idx_claims_lifecycle ON claims(lifecycle);
-- v24: ListClaimsByTestRequirementRef filters
-- "type = 'test_result' AND test_requirement_ref = ?". Unindexed, that was a
-- full scan of every claim — on a brain that only grows, the cost of resolving
-- one test-vs-test contradiction grew with the whole corpus.
-- Deliberately NOT a partial index on test_requirement_ref != '': SQLite only
-- uses a partial index when the query's WHERE provably implies the index
-- predicate, and a bound parameter could itself be '', so the partial variant
-- is never chosen for this query (verified with EXPLAIN QUERY PLAN).
CREATE INDEX IF NOT EXISTS idx_claims_test_requirement_ref
  ON claims(test_requirement_ref, type);
`
	if _, err := db.Exec(postMigrateIndexes); err != nil {
		return fmt.Errorf("post-migrate indexes: %w", err)
	}

	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", currentSchemaVersion)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	return nil
}

// columnExists asks SQLite which columns a table currently has.
// Cheap (PRAGMA table_info is O(columns)) and the only reliable way
// to keep the migration idempotent across SQLite versions that don't
// support ALTER TABLE ... ADD COLUMN IF NOT EXISTS.
func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
