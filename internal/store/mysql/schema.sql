-- Mnemos schema for MySQL / MariaDB. Mirrors sql/sqlite/schema.sql
-- and sql/postgres/schema.sql, with MySQL-specific type choices:
--
--   * text columns use VARCHAR for indexed identifiers and
--     MEDIUMTEXT for free-form content. utf8mb4_bin is used on the
--     id-style columns so equality comparisons stay byte-exact.
--   * timestamps use DATETIME(6) for microsecond precision; MySQL
--     has no timezone-aware timestamp type, so callers always
--     write UTC.
--   * jsonb -> JSON.
--   * bytea -> LONGBLOB (4 GB ceiling; embeddings are typically
--     <16 KB so a smaller type would also work, but LONGBLOB
--     leaves headroom for future vector dimensions).
--   * INTEGER PK AUTOINCREMENT / BIGSERIAL -> BIGINT UNSIGNED
--     AUTO_INCREMENT.
--
-- Namespace is the database (CREATE DATABASE per namespace is the
-- ADR §3 translation for MySQL). The provider runs CREATE DATABASE
-- IF NOT EXISTS <namespace> before applying this file, so plain
-- table names here land in the right database.

CREATE TABLE IF NOT EXISTS events (
  id              VARCHAR(190) NOT NULL,
  run_id          VARCHAR(190) NOT NULL,
  schema_version  VARCHAR(64)  NOT NULL,
  content         MEDIUMTEXT   NOT NULL,
  source_input_id VARCHAR(190) NOT NULL,
  timestamp       DATETIME(6)  NOT NULL,
  metadata_json   JSON         NOT NULL,
  ingested_at     DATETIME(6)  NOT NULL,
  created_by      VARCHAR(190) NOT NULL DEFAULT '<system>',
  PRIMARY KEY (id),
  KEY idx_events_timestamp       (timestamp),
  KEY idx_events_source_input_id (source_input_id),
  KEY idx_events_run_id          (run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS claims (
  id             VARCHAR(190)     NOT NULL,
  text           MEDIUMTEXT       NOT NULL,
  type           VARCHAR(32)      NOT NULL,
  confidence     DOUBLE           NOT NULL,
  status         VARCHAR(32)      NOT NULL,
  created_at     DATETIME(6)      NOT NULL,
  created_by     VARCHAR(190)     NOT NULL DEFAULT '<system>',
  trust_score    DOUBLE           NOT NULL DEFAULT 0,
  valid_from     DATETIME(6)      NULL,
  valid_to       DATETIME(6)      NULL,
  last_verified  DATETIME(6)      NULL,
  verify_count   INT              NOT NULL DEFAULT 0,
  half_life_days DOUBLE           NOT NULL DEFAULT 0,
  scope_service  VARCHAR(190)     NOT NULL DEFAULT '',
  scope_env      VARCHAR(64)      NOT NULL DEFAULT '',
  scope_team     VARCHAR(190)     NOT NULL DEFAULT '',
  subject_class  VARCHAR(32)      NOT NULL DEFAULT '',
  durability     VARCHAR(32)      NOT NULL DEFAULT '',
  confidence_components JSON      NULL,
  test_id              VARCHAR(190) NOT NULL DEFAULT '',
  test_requirement_ref VARCHAR(190) NOT NULL DEFAULT '',
  test_author          VARCHAR(190) NOT NULL DEFAULT '',
  test_last_modified   DATETIME(6)  NULL,
  test_last_run_at     DATETIME(6)  NULL,
  test_pass_count      INT          NOT NULL DEFAULT 0,
  test_fail_count      INT          NOT NULL DEFAULT 0,
  PRIMARY KEY (id),
  KEY idx_claims_trust_score   (trust_score),
  KEY idx_claims_valid_to      (valid_to),
  KEY idx_claims_scope_service (scope_service)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Idempotent column adds for pre-existing databases upgraded from earlier
-- schema generations. MariaDB understands IF NOT EXISTS here; vanilla MySQL
-- does NOT (it is a syntax error), so the provider retries these without the
-- clause and treats "duplicate column" as success — see applySchema.
ALTER TABLE claims ADD COLUMN IF NOT EXISTS last_verified  DATETIME(6)  NULL;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS verify_count   INT          NOT NULL DEFAULT 0;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS half_life_days DOUBLE       NOT NULL DEFAULT 0;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS scope_service  VARCHAR(190) NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS scope_env      VARCHAR(64)  NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS scope_team     VARCHAR(190) NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS confidence_components JSON NULL;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS lifecycle VARCHAR(32) NOT NULL DEFAULT '';
-- ADR 0012: subject-class eligibility gate on claims. Plain column, defaulted
-- to '' (unknown). Persists domain.Claim.SubjectClass so the claim-derived
-- knowledge promotion path reads it back instead of always seeing 'unknown'.
ALTER TABLE claims ADD COLUMN IF NOT EXISTS subject_class VARCHAR(32) NOT NULL DEFAULT '';
-- durability (ADR 0023): 'durable' | 'session' | '' (unknown). Unknown is
-- treated as durable: the whole back catalogue predates the column, so absence
-- of a verdict must never demote a belief.
ALTER TABLE claims ADD COLUMN IF NOT EXISTS durability VARCHAR(32) NOT NULL DEFAULT '';
-- Test provenance (Claim.Type == 'test_result'). ListByTestRequirementRef has
-- always queried test_requirement_ref / test_last_run_at here, but the columns
-- were never declared, so the call failed outright (unknown column) rather than
-- returning no rows. Declared in the CREATE TABLE above for fresh databases and
-- here for pre-existing ones.
ALTER TABLE claims ADD COLUMN IF NOT EXISTS test_id              VARCHAR(190) NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS test_requirement_ref VARCHAR(190) NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS test_author          VARCHAR(190) NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS test_last_modified   DATETIME(6)  NULL;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS test_last_run_at     DATETIME(6)  NULL;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS test_pass_count      INT          NOT NULL DEFAULT 0;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS test_fail_count      INT          NOT NULL DEFAULT 0;
-- Epistemic provenance + the audience gate (#339). These eight existed only in
-- sql/sqlite/schema.sql, so no projection change could have reached them —
-- there was nothing to select, and a brain hosted here could not persist them
-- at all. Existing rows read as the defaults below, which are the truthful
-- values: unknown provenance IS empty/zero, and 'team' is exactly what
-- query.admission already coerces an absent audience to, so no row changes
-- meaning by acquiring the column.
--
-- source_document and provenance_rationale are VARCHAR(1024) rather than TEXT
-- because MySQL forbids a DEFAULT on a TEXT column, and a NOT NULL column with
-- no default makes every pre-existing row depend on an implicit fill. 1024 is
-- generous for a path/URL and a one-line rationale; strict mode ERRORS rather
-- than truncating, so an overflow is loud, not silent.
--
-- last_executed is NULLable with no default, deliberately: NULL is the
-- cross-backend "never executed" sentinel that trust.EvaluateLiveness reads as
-- "unknown, fall back to LastVerified/ValidFrom". A zero-instant default would
-- report every unexecuted claim as decades dead.
ALTER TABLE claims ADD COLUMN IF NOT EXISTS source_document      VARCHAR(1024) NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS source_type          VARCHAR(64)   NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS source_authority     DOUBLE        NOT NULL DEFAULT 0;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS liveness             VARCHAR(32)   NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS last_executed        DATETIME(6)   NULL;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS citation_count       INT           NOT NULL DEFAULT 0;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS provenance_rationale VARCHAR(1024) NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS visibility           VARCHAR(32)   NOT NULL DEFAULT 'team';
-- ListByTestRequirementRef filters `type = 'test_result' AND
-- test_requirement_ref = ?`. MySQL has no partial indexes, so this is a plain
-- composite; without it the lookup scans every claim row. CREATE INDEX has no
-- IF NOT EXISTS in MySQL — a re-run's "duplicate key name" (1061) is swallowed
-- by isBenignSchemaError, which is how this file stays idempotent.
CREATE INDEX idx_claims_test_requirement_ref ON claims (test_requirement_ref, type);

CREATE TABLE IF NOT EXISTS entities (
  id              VARCHAR(190) NOT NULL,
  name            VARCHAR(255) NOT NULL,
  normalized_name VARCHAR(255) NOT NULL,
  type            VARCHAR(64)  NOT NULL,
  created_at      DATETIME(6)  NOT NULL,
  created_by      VARCHAR(190) NOT NULL DEFAULT '<system>',
  PRIMARY KEY (id),
  UNIQUE KEY uniq_entities_norm_type (normalized_name, type),
  KEY idx_entities_normalized_name  (normalized_name),
  KEY idx_entities_type             (type)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS claim_entities (
  claim_id  VARCHAR(190) NOT NULL,
  entity_id VARCHAR(190) NOT NULL,
  role      VARCHAR(64)  NOT NULL DEFAULT 'mention',
  PRIMARY KEY (claim_id, entity_id, role),
  KEY idx_claim_entities_entity_id (entity_id),
  CONSTRAINT fk_claim_entities_claim  FOREIGN KEY (claim_id)  REFERENCES claims(id),
  CONSTRAINT fk_claim_entities_entity FOREIGN KEY (entity_id) REFERENCES entities(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS claim_evidence (
  claim_id VARCHAR(190) NOT NULL,
  event_id VARCHAR(190) NOT NULL,
  PRIMARY KEY (claim_id, event_id),
  KEY idx_claim_evidence_event_id (event_id),
  CONSTRAINT fk_claim_evidence_claim FOREIGN KEY (claim_id) REFERENCES claims(id),
  CONSTRAINT fk_claim_evidence_event FOREIGN KEY (event_id) REFERENCES events(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS relationships (
  id            VARCHAR(190) NOT NULL,
  type          VARCHAR(32)  NOT NULL,
  from_claim_id VARCHAR(190) NOT NULL,
  to_claim_id   VARCHAR(190) NOT NULL,
  created_at    DATETIME(6)  NOT NULL,
  created_by    VARCHAR(190) NOT NULL DEFAULT '<system>',
  strength      DOUBLE       NOT NULL DEFAULT 1,
  PRIMARY KEY (id),
  UNIQUE KEY uniq_relationships_edge (type, from_claim_id, to_claim_id),
  KEY idx_relationships_from_claim   (from_claim_id),
  KEY idx_relationships_to_claim     (to_claim_id),
  CONSTRAINT fk_relationships_from FOREIGN KEY (from_claim_id) REFERENCES claims(id),
  CONSTRAINT fk_relationships_to   FOREIGN KEY (to_claim_id)   REFERENCES claims(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- cognitive_journal (ADR 0018): append-only learning log; data is a kind-specific JSON payload.
CREATE TABLE IF NOT EXISTS cognitive_journal (
  id         VARCHAR(190) NOT NULL,
  at         DATETIME(6)  NOT NULL,
  run_id     VARCHAR(190) NOT NULL DEFAULT '',
  kind       VARCHAR(64)  NOT NULL,
  subject_id VARCHAR(190) NOT NULL DEFAULT '',
  data       JSON         NULL,
  PRIMARY KEY (id),
  KEY idx_cognitive_journal_kind_at (kind, at),
  KEY idx_cognitive_journal_subject (subject_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS compilation_jobs (
  id         VARCHAR(190) NOT NULL,
  kind       VARCHAR(64)  NOT NULL,
  status     VARCHAR(32)  NOT NULL,
  scope_json JSON         NOT NULL,
  started_at DATETIME(6)  NOT NULL,
  updated_at DATETIME(6)  NOT NULL,
  error      MEDIUMTEXT   NOT NULL,
  PRIMARY KEY (id),
  KEY idx_compilation_jobs_kind   (kind),
  KEY idx_compilation_jobs_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS claim_status_history (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  claim_id    VARCHAR(190)    NOT NULL,
  from_status VARCHAR(32)     NOT NULL,
  to_status   VARCHAR(32)     NOT NULL,
  changed_at  DATETIME(6)     NOT NULL,
  reason      MEDIUMTEXT      NOT NULL,
  changed_by  VARCHAR(190)    NOT NULL DEFAULT '<system>',
  PRIMARY KEY (id),
  KEY idx_claim_status_history_claim_id   (claim_id),
  KEY idx_claim_status_history_changed_at (changed_at),
  CONSTRAINT fk_claim_status_history_claim FOREIGN KEY (claim_id) REFERENCES claims(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS embeddings (
  entity_id   VARCHAR(190) NOT NULL,
  entity_type VARCHAR(64)  NOT NULL,
  vector      LONGBLOB     NOT NULL,
  model       VARCHAR(190) NOT NULL,
  dimensions  INT          NOT NULL,
  created_at  DATETIME(6)  NOT NULL,
  created_by  VARCHAR(190) NOT NULL DEFAULT '<system>',
  PRIMARY KEY (entity_id, entity_type),
  KEY idx_embeddings_entity_type (entity_type)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS users (
  id          VARCHAR(190) NOT NULL,
  name        VARCHAR(255) NOT NULL,
  email       VARCHAR(255) NOT NULL,
  status      VARCHAR(32)  NOT NULL DEFAULT 'active',
  scopes_json JSON         NOT NULL,
  created_at  DATETIME(6)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uniq_users_email (email),
  KEY idx_users_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS revoked_tokens (
  jti        VARCHAR(190) NOT NULL,
  revoked_at DATETIME(6)  NOT NULL,
  expires_at DATETIME(6)  NOT NULL,
  PRIMARY KEY (jti),
  KEY idx_revoked_tokens_expires_at (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS agents (
  id                VARCHAR(190) NOT NULL,
  name              VARCHAR(255) NOT NULL,
  owner_id          VARCHAR(190) NOT NULL,
  scopes_json       JSON         NOT NULL,
  allowed_runs_json JSON         NOT NULL,
  status            VARCHAR(32)  NOT NULL DEFAULT 'active',
  created_at        DATETIME(6)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_agents_owner_id (owner_id),
  KEY idx_agents_status   (status),
  CONSTRAINT fk_agents_owner FOREIGN KEY (owner_id) REFERENCES users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
;

CREATE TABLE IF NOT EXISTS actions (
  id            VARCHAR(190) NOT NULL,
  run_id        VARCHAR(190) NOT NULL DEFAULT '',
  kind          VARCHAR(64)  NOT NULL,
  subject       VARCHAR(190) NOT NULL,
  actor         VARCHAR(190) NOT NULL DEFAULT '',
  at            DATETIME(6)  NOT NULL,
  metadata_json JSON         NOT NULL,
  created_by    VARCHAR(190) NOT NULL DEFAULT '<system>',
  created_at    DATETIME(6)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_actions_run_id  (run_id),
  KEY idx_actions_subject (subject),
  KEY idx_actions_kind    (kind),
  KEY idx_actions_at      (at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS outcomes (
  id           VARCHAR(190) NOT NULL,
  action_id    VARCHAR(190) NOT NULL,
  result       VARCHAR(32)  NOT NULL,
  metrics_json JSON         NOT NULL,
  notes        TEXT         NOT NULL,
  observed_at  DATETIME(6)  NOT NULL,
  source       VARCHAR(64)  NOT NULL DEFAULT 'push',
  created_by   VARCHAR(190) NOT NULL DEFAULT '<system>',
  created_at   DATETIME(6)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_outcomes_action_id   (action_id),
  KEY idx_outcomes_result      (result),
  KEY idx_outcomes_observed_at (observed_at),
  CONSTRAINT fk_outcomes_action FOREIGN KEY (action_id) REFERENCES actions(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS lessons (
  id            VARCHAR(190) NOT NULL,
  statement     TEXT         NOT NULL,
  scope_service VARCHAR(190) NOT NULL DEFAULT '',
  scope_env     VARCHAR(64)  NOT NULL DEFAULT '',
  scope_team    VARCHAR(190) NOT NULL DEFAULT '',
  `trigger`     VARCHAR(190) NOT NULL DEFAULT '',
  kind          VARCHAR(64)  NOT NULL DEFAULT '',
  confidence    DOUBLE       NOT NULL,
  derived_at    DATETIME(6)  NOT NULL,
  last_verified DATETIME(6)  NULL,
  source        VARCHAR(32)  NOT NULL DEFAULT 'synthesize',
  created_by    VARCHAR(190) NOT NULL DEFAULT '<system>',
  subject_class VARCHAR(32)  NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  KEY idx_lessons_scope_service (scope_service),
  KEY idx_lessons_scope_env     (scope_env),
  KEY idx_lessons_scope_team    (scope_team),
  KEY idx_lessons_kind          (kind),
  KEY idx_lessons_trigger       (`trigger`),
  KEY idx_lessons_confidence    (confidence)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ADR 0012: subject-class eligibility gate. A plain column on lessons that
-- flows up from the backing beliefs at synthesis time (see AggregateSubjectClass).
-- Added for schemas created under an earlier generation of the schema.
-- MySQL 8.0.29+ supports IF NOT EXISTS on ADD COLUMN.
ALTER TABLE lessons ADD COLUMN IF NOT EXISTS subject_class VARCHAR(32) NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS lesson_evidence (
  lesson_id VARCHAR(190) NOT NULL,
  action_id VARCHAR(190) NOT NULL,
  PRIMARY KEY (lesson_id, action_id),
  KEY idx_lesson_evidence_action_id (action_id),
  CONSTRAINT fk_le_lesson FOREIGN KEY (lesson_id) REFERENCES lessons(id),
  CONSTRAINT fk_le_action FOREIGN KEY (action_id) REFERENCES actions(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS decisions (
  id                VARCHAR(190) NOT NULL,
  statement         TEXT         NOT NULL,
  plan              TEXT         NOT NULL,
  reasoning         TEXT         NOT NULL,
  risk_level        VARCHAR(32)  NOT NULL,
  alternatives_json JSON         NOT NULL,
  outcome_id        VARCHAR(190) NOT NULL DEFAULT '',
  chosen_at         DATETIME(6)  NOT NULL,
  created_by        VARCHAR(190) NOT NULL DEFAULT '<system>',
  created_at        DATETIME(6)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_decisions_chosen_at  (chosen_at),
  KEY idx_decisions_risk_level (risk_level),
  KEY idx_decisions_outcome_id (outcome_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS decision_beliefs (
  decision_id VARCHAR(190) NOT NULL,
  claim_id    VARCHAR(190) NOT NULL,
  PRIMARY KEY (decision_id, claim_id),
  KEY idx_decision_beliefs_claim_id (claim_id),
  CONSTRAINT fk_db_decision FOREIGN KEY (decision_id) REFERENCES decisions(id),
  CONSTRAINT fk_db_claim    FOREIGN KEY (claim_id)    REFERENCES claims(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS playbooks (
  id            VARCHAR(190) NOT NULL,
  `trigger`     VARCHAR(190) NOT NULL,
  statement     TEXT         NOT NULL,
  scope_service VARCHAR(190) NOT NULL DEFAULT '',
  scope_env     VARCHAR(64)  NOT NULL DEFAULT '',
  scope_team    VARCHAR(190) NOT NULL DEFAULT '',
  steps_json    JSON         NOT NULL,
  confidence    DOUBLE       NOT NULL,
  derived_at    DATETIME(6)  NOT NULL,
  last_verified DATETIME(6)  NULL,
  source        VARCHAR(32)  NOT NULL DEFAULT 'synthesize',
  created_by    VARCHAR(190) NOT NULL DEFAULT '<system>',
  PRIMARY KEY (id),
  KEY idx_playbooks_trigger       (`trigger`),
  KEY idx_playbooks_scope_service (scope_service),
  KEY idx_playbooks_confidence    (confidence)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS playbook_lessons (
  playbook_id VARCHAR(190) NOT NULL,
  lesson_id   VARCHAR(190) NOT NULL,
  PRIMARY KEY (playbook_id, lesson_id),
  KEY idx_playbook_lessons_lesson_id (lesson_id),
  CONSTRAINT fk_pl_playbook FOREIGN KEY (playbook_id) REFERENCES playbooks(id),
  CONSTRAINT fk_pl_lesson   FOREIGN KEY (lesson_id)   REFERENCES lessons(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE claims    ADD COLUMN IF NOT EXISTS scope_service VARCHAR(190) NOT NULL DEFAULT '';
ALTER TABLE claims    ADD COLUMN IF NOT EXISTS scope_env     VARCHAR(64)  NOT NULL DEFAULT '';
ALTER TABLE claims    ADD COLUMN IF NOT EXISTS scope_team    VARCHAR(190) NOT NULL DEFAULT '';
ALTER TABLE decisions ADD COLUMN IF NOT EXISTS scope_service          VARCHAR(190) NOT NULL DEFAULT '';
ALTER TABLE decisions ADD COLUMN IF NOT EXISTS scope_env              VARCHAR(64)  NOT NULL DEFAULT '';
ALTER TABLE decisions ADD COLUMN IF NOT EXISTS scope_team             VARCHAR(190) NOT NULL DEFAULT '';
ALTER TABLE decisions ADD COLUMN IF NOT EXISTS refuted_beliefs_json   TEXT         NOT NULL DEFAULT ('[]');
ALTER TABLE decisions ADD COLUMN IF NOT EXISTS failed_outcome_id      VARCHAR(190) NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS lesson_versions (
  version_id   BIGINT       NOT NULL AUTO_INCREMENT,
  lesson_id    VARCHAR(190) NOT NULL,
  payload_json JSON         NOT NULL,
  valid_from   DATETIME(6)  NOT NULL,
  valid_to     DATETIME(6)  NOT NULL,
  PRIMARY KEY (version_id),
  KEY idx_lesson_versions_lesson_id (lesson_id),
  CONSTRAINT fk_lv_lesson FOREIGN KEY (lesson_id) REFERENCES lessons(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS playbook_versions (
  version_id   BIGINT       NOT NULL AUTO_INCREMENT,
  playbook_id  VARCHAR(190) NOT NULL,
  payload_json JSON         NOT NULL,
  valid_from   DATETIME(6)  NOT NULL,
  valid_to     DATETIME(6)  NOT NULL,
  PRIMARY KEY (version_id),
  KEY idx_playbook_versions_playbook_id (playbook_id),
  CONSTRAINT fk_pv_playbook FOREIGN KEY (playbook_id) REFERENCES playbooks(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS entity_relationships (
  id         VARCHAR(190) NOT NULL,
  kind       VARCHAR(32)  NOT NULL,
  from_id    VARCHAR(190) NOT NULL,
  from_type  VARCHAR(32)  NOT NULL,
  to_id      VARCHAR(190) NOT NULL,
  to_type    VARCHAR(32)  NOT NULL,
  created_at DATETIME(6)  NOT NULL,
  created_by VARCHAR(190) NOT NULL DEFAULT '<system>',
  PRIMARY KEY (id),
  UNIQUE KEY uniq_entity_relationships_edge (kind, from_type, from_id, to_type, to_id),
  KEY idx_entity_relationships_from (from_type, from_id),
  KEY idx_entity_relationships_to   (to_type, to_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
