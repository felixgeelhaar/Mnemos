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
  scope_service TEXT NOT NULL DEFAULT '',
  scope_env TEXT NOT NULL DEFAULT '',
  scope_team TEXT NOT NULL DEFAULT '',
  source_document TEXT NOT NULL DEFAULT '',
  source_type TEXT NOT NULL DEFAULT '',
  source_authority REAL NOT NULL DEFAULT 0.0,
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
  visibility TEXT NOT NULL DEFAULT 'team',
  confidence_components TEXT NOT NULL DEFAULT '{}',
  lifecycle TEXT NOT NULL DEFAULT '',
  -- subject_class (ADR 0012): 'individual' | 'class' | '' (unknown). Only
  -- class-level claims may ever promote to the shared global brain.
  subject_class TEXT NOT NULL DEFAULT '',
  -- durability (ADR 0023): 'durable' | 'session' | '' (unknown). Unknown is
  -- treated as durable: the whole back catalogue predates the column, so
  -- absence of a verdict must never demote a belief.
  durability TEXT NOT NULL DEFAULT ''
);

-- No idx_claims_scope_service here: the runtime schema never created one, and
-- nothing would use it — no SQL statement in this repo filters
-- claims.scope_service, because scope is applied in Go after the rows come
-- back. Declaring it here described an index no SQLite brain has ever had.
CREATE INDEX IF NOT EXISTS idx_claims_lifecycle ON claims(lifecycle);

CREATE INDEX IF NOT EXISTS idx_claims_trust_score ON claims(trust_score);
CREATE INDEX IF NOT EXISTS idx_claims_valid_to ON claims(valid_to);
-- Must match the runtime definition in internal/store/sqlite/db.go
-- (postMigrateIndexes). This file is the sqlc codegen reference and is never
-- applied to a live database, so a divergence here is silent: it read as the
-- schema of record while no brain ever had that index.
--
-- Deliberately NOT partial on test_requirement_ref != '': SQLite only picks a
-- partial index when the query's WHERE provably implies the index predicate,
-- and the bound parameter could itself be '', so the partial variant is never
-- chosen for ListClaimsByTestRequirementRef.
CREATE INDEX IF NOT EXISTS idx_claims_test_requirement_ref
  ON claims(test_requirement_ref, type);

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
  strength REAL NOT NULL DEFAULT 1,
  FOREIGN KEY (from_claim_id) REFERENCES claims(id),
  FOREIGN KEY (to_claim_id) REFERENCES claims(id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_relationships_unique_edge
  ON relationships(type, from_claim_id, to_claim_id);
CREATE INDEX IF NOT EXISTS idx_relationships_from_claim ON relationships(from_claim_id);
CREATE INDEX IF NOT EXISTS idx_relationships_to_claim ON relationships(to_claim_id);

-- cognitive_journal (ADR 0018): append-only log of what the learning mechanisms did,
-- over time, for research + constant tuning. data is a kind-specific JSON payload.
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
  -- subject_class (ADR 0012): 'individual' | 'class' | '' (unknown). Only
  -- class-level schemas are eligible for promotion to the shared global brain;
  -- individual and unknown are kept private (fail closed).
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
  created_at TEXT NOT NULL,
  scope_service TEXT NOT NULL DEFAULT '',
  scope_env TEXT NOT NULL DEFAULT '',
  scope_team TEXT NOT NULL DEFAULT '',
  refuted_beliefs_json TEXT NOT NULL DEFAULT '[]',
  failed_outcome_id TEXT NOT NULL DEFAULT ''
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

-- Phase 7 follow-up: SQL:2011 system-versioned snapshot tables.
-- On every UPSERT of a lesson/playbook, the previous row is copied
-- here with a valid_to timestamp before being overwritten. This
-- gives time-travel queries (WHERE valid_from <= ? < valid_to)
-- without forcing the hot read path through versioned views.
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

-- Polymorphic cross-entity edges. The classic relationships table is
-- claim-only and stays that way for graph traversal compatibility;
-- this table holds action↔outcome, outcome↔claim, decision↔outcome
-- edges and any future cross-entity link the synthesis layer needs.
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
