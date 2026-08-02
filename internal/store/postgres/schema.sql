-- Mnemos schema for Postgres backends. Mirrors sql/sqlite/schema.sql
-- with the SQLite-specific bits (FTS5 virtual tables, json_extract,
-- BLOB embeddings) replaced by Postgres equivalents:
--
--   * timestamps:  TEXT in SQLite → timestamptz in Postgres
--   * BLOB         → bytea
--   * INTEGER PK AUTOINCREMENT → BIGSERIAL
--   * json_extract → JSON operators on jsonb columns
--   * FTS5         → tsvector + GIN index (added once the
--                    Postgres provider implements ports.TextSearcher)
--   * vss          → pgvector (optional capability, build-tagged)
--
-- The schema is namespaced via Postgres SCHEMA: the provider runs
-- `CREATE SCHEMA IF NOT EXISTS <namespace>` and `SET search_path TO
-- <namespace>` before applying this file, so unqualified table names
-- here land inside the configured namespace.
--
-- This file is the contract for migrations 000_init.sql onward;
-- destructive changes must ship as numbered up/down migrations once
-- the provider is in production.

CREATE TABLE IF NOT EXISTS events (
  id              text        PRIMARY KEY,
  run_id          text        NOT NULL,
  schema_version  text        NOT NULL,
  content         text        NOT NULL,
  source_input_id text        NOT NULL,
  timestamp       timestamptz NOT NULL,
  metadata_json   jsonb       NOT NULL,
  ingested_at     timestamptz NOT NULL,
  created_by      text        NOT NULL DEFAULT '<system>'
);

CREATE INDEX IF NOT EXISTS idx_events_timestamp       ON events(timestamp);
CREATE INDEX IF NOT EXISTS idx_events_source_input_id ON events(source_input_id);
CREATE INDEX IF NOT EXISTS idx_events_run_id          ON events(run_id);

CREATE TABLE IF NOT EXISTS claims (
  id             text             PRIMARY KEY,
  text           text             NOT NULL,
  type           text             NOT NULL,
  confidence     double precision NOT NULL,
  status         text             NOT NULL,
  created_at     timestamptz      NOT NULL,
  created_by     text             NOT NULL DEFAULT '<system>',
  trust_score    double precision NOT NULL DEFAULT 0,
  valid_from     timestamptz,
  valid_to       timestamptz,
  last_verified  timestamptz,
  verify_count   integer          NOT NULL DEFAULT 0,
  half_life_days double precision NOT NULL DEFAULT 0,
  scope_service  text             NOT NULL DEFAULT '',
  scope_env      text             NOT NULL DEFAULT '',
  scope_team     text             NOT NULL DEFAULT ''
);

-- Idempotent column adds for pre-existing namespaces upgraded from
-- earlier schema generations.
ALTER TABLE claims ADD COLUMN IF NOT EXISTS last_verified  timestamptz;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS verify_count   integer          NOT NULL DEFAULT 0;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS half_life_days double precision NOT NULL DEFAULT 0;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS scope_service  text             NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS scope_env      text             NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS scope_team     text             NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS confidence_components jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS lifecycle text NOT NULL DEFAULT '';
-- ADR 0012: subject-class eligibility gate on claims. Plain column, defaulted
-- to '' (unknown). Persists domain.Claim.SubjectClass so the claim-derived
-- knowledge promotion path reads it back instead of always seeing 'unknown'.
ALTER TABLE claims ADD COLUMN IF NOT EXISTS subject_class text NOT NULL DEFAULT '';
-- durability (ADR 0023): 'durable' | 'session' | '' (unknown). Unknown is
-- treated as durable: the whole back catalogue predates the column, so absence
-- of a verdict must never demote a belief.
ALTER TABLE claims ADD COLUMN IF NOT EXISTS durability text NOT NULL DEFAULT '';
-- Test provenance (Claim.Type == 'test_result'). ListByTestRequirementRef has
-- always queried test_requirement_ref / test_last_run_at here, but the columns
-- were never declared, so the call failed outright with 42703 (undefined
-- column) — not "no results", an error — and any test_result claim read back
-- without test_id failed domain validation.
ALTER TABLE claims ADD COLUMN IF NOT EXISTS test_id               text        NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS test_requirement_ref  text        NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS test_author           text        NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS test_last_modified    timestamptz;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS test_last_run_at      timestamptz;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS test_pass_count       integer     NOT NULL DEFAULT 0;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS test_fail_count       integer     NOT NULL DEFAULT 0;
-- Epistemic provenance + the audience gate (#339). These eight existed only in
-- sql/sqlite/schema.sql, so no projection change could have reached them —
-- there was nothing to select, and a brain hosted here could not persist them
-- at all. Existing rows read as the defaults below, which are the truthful
-- values: unknown provenance IS empty/zero, and 'team' is exactly what
-- query.admission already coerces an absent audience to, so no row changes
-- meaning by acquiring the column.
--
-- last_executed is NULLable with no default, deliberately: NULL is the
-- cross-backend "never executed" sentinel that trust.EvaluateLiveness reads as
-- "unknown, fall back to LastVerified/ValidFrom". A zero-instant default would
-- report every unexecuted claim as decades dead — the same mistake Postgres
-- once made COALESCE-ing the evidence timestamp to 'epoch'.
ALTER TABLE claims ADD COLUMN IF NOT EXISTS source_document       text             NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS source_type           text             NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS source_authority      double precision NOT NULL DEFAULT 0;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS liveness              text             NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS last_executed         timestamptz;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS citation_count        integer          NOT NULL DEFAULT 0;
ALTER TABLE claims ADD COLUMN IF NOT EXISTS provenance_rationale  text             NOT NULL DEFAULT '';
ALTER TABLE claims ADD COLUMN IF NOT EXISTS visibility            text             NOT NULL DEFAULT 'team';
CREATE INDEX IF NOT EXISTS idx_claims_scope_service ON claims(scope_service);
CREATE INDEX IF NOT EXISTS idx_claims_lifecycle ON claims(lifecycle);
-- ListByTestRequirementRef filters `type = 'test_result' AND
-- test_requirement_ref = $1`; without an index that is a seq scan of every
-- claim on a store that only grows. Deliberately NOT partial on
-- test_requirement_ref <> '': the planner only uses a partial index when the
-- query's quals imply its predicate, which a bound parameter does not do under
-- a generic plan — the index would then be silently skipped exactly when it
-- matters. Same shape as the sqlite and mysql indexes.
CREATE INDEX IF NOT EXISTS idx_claims_test_requirement_ref
  ON claims(test_requirement_ref, type);

CREATE INDEX IF NOT EXISTS idx_claims_trust_score ON claims(trust_score);
CREATE INDEX IF NOT EXISTS idx_claims_valid_to    ON claims(valid_to);

CREATE TABLE IF NOT EXISTS entities (
  id              text        PRIMARY KEY,
  name            text        NOT NULL,
  normalized_name text        NOT NULL,
  type            text        NOT NULL,
  created_at      timestamptz NOT NULL,
  created_by      text        NOT NULL DEFAULT '<system>',
  UNIQUE(normalized_name, type)
);

CREATE INDEX IF NOT EXISTS idx_entities_normalized_name ON entities(normalized_name);
CREATE INDEX IF NOT EXISTS idx_entities_type            ON entities(type);

CREATE TABLE IF NOT EXISTS claim_entities (
  claim_id  text NOT NULL REFERENCES claims(id),
  entity_id text NOT NULL REFERENCES entities(id),
  role      text NOT NULL DEFAULT 'mention',
  PRIMARY KEY (claim_id, entity_id, role)
);

CREATE INDEX IF NOT EXISTS idx_claim_entities_entity_id ON claim_entities(entity_id);

CREATE TABLE IF NOT EXISTS claim_evidence (
  claim_id text NOT NULL REFERENCES claims(id),
  event_id text NOT NULL REFERENCES events(id),
  PRIMARY KEY (claim_id, event_id)
);

CREATE INDEX IF NOT EXISTS idx_claim_evidence_event_id ON claim_evidence(event_id);

CREATE TABLE IF NOT EXISTS relationships (
  id            text             PRIMARY KEY,
  type          text             NOT NULL,
  from_claim_id text             NOT NULL REFERENCES claims(id),
  to_claim_id   text             NOT NULL REFERENCES claims(id),
  created_at    timestamptz      NOT NULL,
  created_by    text             NOT NULL DEFAULT '<system>',
  strength      double precision NOT NULL DEFAULT 1
);
-- ADR 0015 §4: Hebbian co-activation weight for existing DBs (fresh ones get it
-- inline above). Backfills to the base 1.0 so spreading activation is unchanged.
ALTER TABLE relationships ADD COLUMN IF NOT EXISTS strength double precision NOT NULL DEFAULT 1;

CREATE UNIQUE INDEX IF NOT EXISTS idx_relationships_unique_edge
  ON relationships(type, from_claim_id, to_claim_id);
CREATE INDEX IF NOT EXISTS idx_relationships_from_claim ON relationships(from_claim_id);
CREATE INDEX IF NOT EXISTS idx_relationships_to_claim   ON relationships(to_claim_id);

-- cognitive_journal (ADR 0018): append-only learning log. Registered in the RLS `scoped`
-- array below so it is per-tenant isolated (each tenant's brain journals separately).
CREATE TABLE IF NOT EXISTS cognitive_journal (
  id         text        PRIMARY KEY,
  at         timestamptz NOT NULL,
  run_id     text        NOT NULL DEFAULT '',
  kind       text        NOT NULL,
  subject_id text        NOT NULL DEFAULT '',
  data       jsonb       NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS idx_cognitive_journal_kind_at  ON cognitive_journal (kind, at);
CREATE INDEX IF NOT EXISTS idx_cognitive_journal_subject  ON cognitive_journal (subject_id);

CREATE TABLE IF NOT EXISTS compilation_jobs (
  id         text        PRIMARY KEY,
  kind       text        NOT NULL,
  status     text        NOT NULL,
  scope_json jsonb       NOT NULL,
  started_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  error      text        NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_compilation_jobs_kind   ON compilation_jobs(kind);
CREATE INDEX IF NOT EXISTS idx_compilation_jobs_status ON compilation_jobs(status);

CREATE TABLE IF NOT EXISTS claim_status_history (
  id          bigserial   PRIMARY KEY,
  claim_id    text        NOT NULL REFERENCES claims(id),
  from_status text        NOT NULL,
  to_status   text        NOT NULL,
  changed_at  timestamptz NOT NULL,
  reason      text        NOT NULL,
  changed_by  text        NOT NULL DEFAULT '<system>'
);

CREATE INDEX IF NOT EXISTS idx_claim_status_history_claim_id   ON claim_status_history(claim_id);
CREATE INDEX IF NOT EXISTS idx_claim_status_history_changed_at ON claim_status_history(changed_at);

-- Embeddings: bytea matches the SQLite BLOB shape and stays the portable
-- source of truth. When pgvector is installed, a nullable `embedding
-- public.vector` accelerator column is added below (see the $mnemos_pgvector$
-- block) and populated on write; the bytea column is never dropped.
CREATE TABLE IF NOT EXISTS embeddings (
  entity_id   text             NOT NULL,
  entity_type text             NOT NULL,
  vector      bytea            NOT NULL,
  model       text             NOT NULL,
  dimensions  integer          NOT NULL,
  created_at  timestamptz      NOT NULL,
  created_by  text             NOT NULL DEFAULT '<system>',
  PRIMARY KEY (entity_id, entity_type)
);

CREATE INDEX IF NOT EXISTS idx_embeddings_entity_type ON embeddings(entity_type);

CREATE TABLE IF NOT EXISTS users (
  id          text        PRIMARY KEY,
  name        text        NOT NULL,
  email       text        NOT NULL UNIQUE,
  status      text        NOT NULL DEFAULT 'active',
  scopes_json jsonb       NOT NULL DEFAULT '["*"]'::jsonb,
  created_at  timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_users_status ON users(status);

CREATE TABLE IF NOT EXISTS revoked_tokens (
  jti        text        PRIMARY KEY,
  revoked_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_revoked_tokens_expires_at ON revoked_tokens(expires_at);

CREATE TABLE IF NOT EXISTS agents (
  id                 text        PRIMARY KEY,
  name               text        NOT NULL,
  owner_id           text        NOT NULL REFERENCES users(id),
  scopes_json        jsonb       NOT NULL DEFAULT '[]'::jsonb,
  allowed_runs_json  jsonb       NOT NULL DEFAULT '[]'::jsonb,
  status             text        NOT NULL DEFAULT 'active',
  created_at         timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_agents_owner_id ON agents(owner_id);
CREATE INDEX IF NOT EXISTS idx_agents_status   ON agents(status);

CREATE TABLE IF NOT EXISTS actions (
  id            text        PRIMARY KEY,
  run_id        text        NOT NULL DEFAULT '',
  kind          text        NOT NULL,
  subject       text        NOT NULL,
  actor         text        NOT NULL DEFAULT '',
  at            timestamptz NOT NULL,
  metadata_json jsonb       NOT NULL DEFAULT '{}'::jsonb,
  created_by    text        NOT NULL DEFAULT '<system>',
  created_at    timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_actions_run_id  ON actions(run_id);
CREATE INDEX IF NOT EXISTS idx_actions_subject ON actions(subject);
CREATE INDEX IF NOT EXISTS idx_actions_kind    ON actions(kind);
CREATE INDEX IF NOT EXISTS idx_actions_at      ON actions(at);

CREATE TABLE IF NOT EXISTS outcomes (
  id           text        PRIMARY KEY,
  action_id    text        NOT NULL REFERENCES actions(id),
  result       text        NOT NULL,
  metrics_json jsonb       NOT NULL DEFAULT '{}'::jsonb,
  notes        text        NOT NULL DEFAULT '',
  observed_at  timestamptz NOT NULL,
  source       text        NOT NULL DEFAULT 'push',
  created_by   text        NOT NULL DEFAULT '<system>',
  created_at   timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_outcomes_action_id   ON outcomes(action_id);
CREATE INDEX IF NOT EXISTS idx_outcomes_result      ON outcomes(result);
CREATE INDEX IF NOT EXISTS idx_outcomes_observed_at ON outcomes(observed_at);

CREATE TABLE IF NOT EXISTS lessons (
  id            text        PRIMARY KEY,
  statement     text        NOT NULL,
  scope_service text        NOT NULL DEFAULT '',
  scope_env     text        NOT NULL DEFAULT '',
  scope_team    text        NOT NULL DEFAULT '',
  trigger       text        NOT NULL DEFAULT '',
  kind          text        NOT NULL DEFAULT '',
  confidence    double precision NOT NULL,
  derived_at    timestamptz NOT NULL,
  last_verified timestamptz,
  source        text        NOT NULL DEFAULT 'synthesize',
  created_by    text        NOT NULL DEFAULT '<system>',
  subject_class text        NOT NULL DEFAULT ''
);

-- ADR 0012: subject-class eligibility gate. A plain column on lessons that
-- flows up from the backing beliefs at synthesis time (see AggregateSubjectClass).
-- Added for schemas created under an earlier generation of the schema.
ALTER TABLE lessons ADD COLUMN IF NOT EXISTS subject_class text NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_lessons_scope_service ON lessons(scope_service);
CREATE INDEX IF NOT EXISTS idx_lessons_scope_env     ON lessons(scope_env);
CREATE INDEX IF NOT EXISTS idx_lessons_scope_team    ON lessons(scope_team);
CREATE INDEX IF NOT EXISTS idx_lessons_kind          ON lessons(kind);
CREATE INDEX IF NOT EXISTS idx_lessons_trigger       ON lessons(trigger);
CREATE INDEX IF NOT EXISTS idx_lessons_confidence    ON lessons(confidence);

CREATE TABLE IF NOT EXISTS lesson_evidence (
  lesson_id text NOT NULL REFERENCES lessons(id),
  action_id text NOT NULL REFERENCES actions(id),
  PRIMARY KEY (lesson_id, action_id)
);

CREATE INDEX IF NOT EXISTS idx_lesson_evidence_action_id ON lesson_evidence(action_id);

CREATE TABLE IF NOT EXISTS decisions (
  id                text        PRIMARY KEY,
  statement         text        NOT NULL,
  plan              text        NOT NULL DEFAULT '',
  reasoning         text        NOT NULL DEFAULT '',
  risk_level        text        NOT NULL,
  alternatives_json jsonb       NOT NULL DEFAULT '[]'::jsonb,
  outcome_id        text        NOT NULL DEFAULT '',
  chosen_at         timestamptz NOT NULL,
  created_by        text        NOT NULL DEFAULT '<system>',
  created_at        timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_decisions_chosen_at  ON decisions(chosen_at);
CREATE INDEX IF NOT EXISTS idx_decisions_risk_level ON decisions(risk_level);
CREATE INDEX IF NOT EXISTS idx_decisions_outcome_id ON decisions(outcome_id);

CREATE TABLE IF NOT EXISTS decision_beliefs (
  decision_id text NOT NULL REFERENCES decisions(id),
  claim_id    text NOT NULL REFERENCES claims(id),
  PRIMARY KEY (decision_id, claim_id)
);

CREATE INDEX IF NOT EXISTS idx_decision_beliefs_claim_id ON decision_beliefs(claim_id);

CREATE TABLE IF NOT EXISTS playbooks (
  id            text             PRIMARY KEY,
  trigger       text             NOT NULL,
  statement     text             NOT NULL,
  scope_service text             NOT NULL DEFAULT '',
  scope_env     text             NOT NULL DEFAULT '',
  scope_team    text             NOT NULL DEFAULT '',
  steps_json    jsonb            NOT NULL DEFAULT '[]'::jsonb,
  confidence    double precision NOT NULL,
  derived_at    timestamptz      NOT NULL,
  last_verified timestamptz,
  source        text             NOT NULL DEFAULT 'synthesize',
  created_by    text             NOT NULL DEFAULT '<system>'
);

CREATE INDEX IF NOT EXISTS idx_playbooks_trigger       ON playbooks(trigger);
CREATE INDEX IF NOT EXISTS idx_playbooks_scope_service ON playbooks(scope_service);
CREATE INDEX IF NOT EXISTS idx_playbooks_confidence    ON playbooks(confidence);

CREATE TABLE IF NOT EXISTS playbook_lessons (
  playbook_id text NOT NULL REFERENCES playbooks(id),
  lesson_id   text NOT NULL REFERENCES lessons(id),
  PRIMARY KEY (playbook_id, lesson_id)
);

CREATE INDEX IF NOT EXISTS idx_playbook_lessons_lesson_id ON playbook_lessons(lesson_id);

 ALTER TABLE decisions ADD COLUMN IF NOT EXISTS scope_service          text NOT NULL DEFAULT '';
 ALTER TABLE decisions ADD COLUMN IF NOT EXISTS scope_env              text NOT NULL DEFAULT '';
 ALTER TABLE decisions ADD COLUMN IF NOT EXISTS scope_team             text NOT NULL DEFAULT '';
 ALTER TABLE decisions ADD COLUMN IF NOT EXISTS refuted_beliefs_json   text NOT NULL DEFAULT '[]';
 ALTER TABLE decisions ADD COLUMN IF NOT EXISTS failed_outcome_id      text NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS lesson_versions (
  version_id   bigserial   PRIMARY KEY,
  lesson_id    text        NOT NULL REFERENCES lessons(id),
  payload_json jsonb       NOT NULL,
  valid_from   timestamptz NOT NULL,
  valid_to     timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_lesson_versions_lesson_id ON lesson_versions(lesson_id);

CREATE TABLE IF NOT EXISTS playbook_versions (
  version_id   bigserial   PRIMARY KEY,
  playbook_id  text        NOT NULL REFERENCES playbooks(id),
  payload_json jsonb       NOT NULL,
  valid_from   timestamptz NOT NULL,
  valid_to     timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_playbook_versions_playbook_id ON playbook_versions(playbook_id);

CREATE TABLE IF NOT EXISTS entity_relationships (
  id         text        PRIMARY KEY,
  kind       text        NOT NULL,
  from_id    text        NOT NULL,
  from_type  text        NOT NULL,
  to_id      text        NOT NULL,
  to_type    text        NOT NULL,
  created_at timestamptz NOT NULL,
  created_by text        NOT NULL DEFAULT '<system>'
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_entity_relationships_unique_edge
  ON entity_relationships(kind, from_type, from_id, to_type, to_id);
CREATE INDEX IF NOT EXISTS idx_entity_relationships_from ON entity_relationships(from_type, from_id);
CREATE INDEX IF NOT EXISTS idx_entity_relationships_to   ON entity_relationships(to_type, to_id);

-- working_memory_blocks (T3.1) — an agent's "core memory": bounded, labeled,
-- mutable text keyed by (owner, label). Gets the ADR 0007 tenant column + RLS
-- via the DO block below (owner is a globally-unique agent id, so the natural
-- key never collides across tenants). The caller enforces the size cap.
CREATE TABLE IF NOT EXISTS working_memory_blocks (
    owner text NOT NULL,
    label text NOT NULL,
    content text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (owner, label)
);

-- claim_expectations (T1.1) — one structured forward prediction per claim; gets
-- the ADR 0007 tenant column + RLS via the DO block below.
CREATE TABLE IF NOT EXISTS claim_expectations (
    claim_id text PRIMARY KEY,
    predicted double precision NOT NULL DEFAULT 0,
    tolerance double precision NOT NULL DEFAULT 0,
    horizon timestamptz,
    observed double precision NOT NULL DEFAULT 0,
    has_observation boolean NOT NULL DEFAULT false,
    resolved boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- global_schemas (ADR 0011 Phase B) — the neocortex (shared/global) tier: a
-- promoted, de-identified generalization corroborated by N distinct tenants. Its
-- provenance is COUNTS (distinct_tenants, evidence_count), never tenant ids or
-- raw evidence ids.
--
-- INVERSE of the ADR-0007 RLS gotcha: this is the SHARED tier and MUST be
-- readable across tenants, so it is DELIBERATELY NOT added to the `scoped` array
-- below — it gets no `tenant` column and no tenant_isolation policy on purpose.
-- Adding it to `scoped` would break the cross-tenant read the neocortex exists
-- to provide. (Like users/revoked_tokens, but for the opposite reason: those are
-- auth infra; this is the global brain.)
CREATE TABLE IF NOT EXISTS global_schemas (
    id text PRIMARY KEY,
    statement text NOT NULL,
    scope_service text NOT NULL DEFAULT '',
    scope_env text NOT NULL DEFAULT '',
    scope_team text NOT NULL DEFAULT '',
    polarity text NOT NULL DEFAULT '',
    distinct_tenants integer NOT NULL DEFAULT 0,
    evidence_count integer NOT NULL DEFAULT 0,
    confidence double precision NOT NULL DEFAULT 0,
    surprise double precision NOT NULL DEFAULT 0,
    has_surprise boolean NOT NULL DEFAULT false,
    status text NOT NULL DEFAULT 'pending',
    promoted_at timestamptz,
    created_by text NOT NULL DEFAULT '<system>'
);
CREATE INDEX IF NOT EXISTS idx_global_schemas_status ON global_schemas(status);
CREATE INDEX IF NOT EXISTS idx_global_schemas_promoted_at ON global_schemas(promoted_at);

-- ADR 0007: per-tenant isolation WITHIN a namespace. Each data table gets a
-- `tenant` column defaulted from the `mnemos.tenant` GUC (pinned per connection by
-- the provider), plus row-level security that filters every read/write to the
-- current tenant. FORCE applies RLS to the owning role too (our connection), so
-- there is no owner bypass. A connection without the GUC fails CLOSED: RLS denies
-- reads and the NOT NULL default rejects writes — never a cross-tenant leak.
-- Idempotent: ADD COLUMN IF NOT EXISTS + DROP/CREATE POLICY make re-runs no-ops.
-- (users/revoked_tokens are auth infrastructure, deliberately NOT tenant-scoped.)
DO $mnemos_rls$
DECLARE
  t text;
  scoped text[] := ARRAY[
    'events','claims','entities','claim_entities','claim_evidence','relationships',
    'compilation_jobs','claim_status_history','embeddings','agents','actions',
    'outcomes','lessons','lesson_evidence','decisions','decision_beliefs',
    'playbooks','playbook_lessons','lesson_versions','playbook_versions',
    'entity_relationships','working_memory_blocks','claim_expectations',
    'cognitive_journal'
  ];
BEGIN
  FOREACH t IN ARRAY scoped LOOP
    IF to_regclass(t) IS NULL THEN
      CONTINUE; -- table absent in this build
    END IF;
    EXECUTE format('ALTER TABLE %I ADD COLUMN IF NOT EXISTS tenant text NOT NULL DEFAULT %L', t, '__default__');
    EXECUTE format('ALTER TABLE %I ALTER COLUMN tenant SET DEFAULT current_setting(%L, true)', t, 'mnemos.tenant');
    EXECUTE format('CREATE INDEX IF NOT EXISTS %I ON %I (tenant)', 'idx_' || t || '_tenant', t);
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', t);
    EXECUTE format('CREATE POLICY tenant_isolation ON %I USING (tenant = current_setting(%L, true)) WITH CHECK (tenant = current_setting(%L, true))', t, 'mnemos.tenant', 'mnemos.tenant');
  END LOOP;
END
$mnemos_rls$;

-- pgvector scale path (optional, detect-only). When the pgvector `vector`
-- type is installed, mirror each embedding into a native `vector` column so
-- recall can rank with the C-native cosine operator (`<=>`) instead of
-- decoding every bytea blob and cosining in Go over the whole corpus.
--
-- DETECT ONLY — never `CREATE EXTENSION`. Mnemos connects as a non-superuser
-- role (ADR 0007 RLS requires it), and installing an extension is a DBA/
-- superuser concern. If the operator has installed pgvector, this adopts it;
-- if not, the bytea path stays authoritative and nothing changes.
--
-- The `vector` type lives in `public`, so it is fully qualified here
-- (`public.vector`, `to_regtype('public.vector')`) — the bootstrap tx pins
-- search_path to the namespace only, which would otherwise hide it.
--
-- The bytea `vector` column remains the portable source of truth; `embedding`
-- is a derived accelerator, (re)written on Upsert. Rows written before the
-- extension was present (or before a re-embed) simply have a NULL `embedding`
-- and are invisible to the native path until rewritten — recall stays correct,
-- only narrower, exactly like the embedding-model migration story. The column
-- is intentionally unbounded (`vector`, no dimension typmod) so a single
-- namespace can hold multiple embedding models; an ivfflat/hnsw index (which
-- needs a fixed dimension) is a per-deployment follow-up, not required for the
-- C-native sequential `<=>` scan that already beats Go-side cosine.
DO $mnemos_pgvector$
BEGIN
  IF to_regtype('public.vector') IS NOT NULL THEN
    EXECUTE 'ALTER TABLE embeddings ADD COLUMN IF NOT EXISTS embedding public.vector';
  END IF;
END
$mnemos_pgvector$;

-- Full-text search (hybrid retrieval, R1). A generated `tsvector` column plus a
-- GIN index over each table's text gives the sparse (lexical) recall leg that the
-- dense embedding leg underweights: exact tokens — SHAs, service names, error
-- codes, flag names — that cosine similarity blurs. The query engine fuses this
-- leg with the pgvector `<=>` leg by Reciprocal Rank Fusion.
--
-- STORED generated columns backfill existing rows the moment they're added and
-- stay in sync on every write, so there is no trigger to maintain and no separate
-- indexing pass. The TWO-ARG `to_tsvector('english', ...)` is IMMUTABLE (required
-- for a generated-column expression); the one-arg form reads a GUC and is not.
-- tsvector + GIN are core Postgres — no extension, so (unlike pgvector) this needs
-- no detect guard; it always applies. `search_tsv` is a derived accelerator — the
-- source text column stays authoritative.
ALTER TABLE events ADD COLUMN IF NOT EXISTS search_tsv tsvector
  GENERATED ALWAYS AS (to_tsvector('english', content)) STORED;
CREATE INDEX IF NOT EXISTS idx_events_search_tsv ON events USING GIN (search_tsv);

ALTER TABLE claims ADD COLUMN IF NOT EXISTS search_tsv tsvector
  GENERATED ALWAYS AS (to_tsvector('english', text)) STORED;
CREATE INDEX IF NOT EXISTS idx_claims_search_tsv ON claims USING GIN (search_tsv);
