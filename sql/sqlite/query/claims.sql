-- name: UpsertClaim :exec
-- ON CONFLICT preserves trust_score and valid_to (computed/managed
-- separately via UpdateClaimTrust and SetClaimValidity), but does
-- refresh valid_from: re-extracting a claim with newer evidence is
-- a legitimate "this fact is observed again from <ts>" signal.
--
-- half_life_days is written on INSERT because the ingest pipeline
-- classifies claim volatility and stamps a shorter freshness half-life
-- on assertions about mutable system state. Omitting it here dropped
-- that classification at the store boundary, so every row in existence
-- kept the DEFAULT 0 and decayed at the 90-day durable default (#331).
--
-- On CONFLICT the incoming value only wins when non-zero. A re-extracted
-- claim carries no half-life (the classifier returns 0 for anything it is
-- not confident about), so a plain excluded.half_life_days would reset a
-- human override set through MarkVerified the next time the same claim
-- came back through ingest. Same COALESCE semantics as MarkClaimVerified.
INSERT INTO claims (id, text, type, confidence, status, created_at, created_by, valid_from, half_life_days, scope_service, scope_env, scope_team, source_document, source_type, source_authority, liveness, last_executed, citation_count, provenance_rationale, test_id, test_requirement_ref, test_author, test_last_modified, test_last_run_at, test_pass_count, test_fail_count, visibility, confidence_components, lifecycle, subject_class, durability)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  text = excluded.text,
  type = excluded.type,
  confidence = excluded.confidence,
  status = excluded.status,
  created_at = excluded.created_at,
  created_by = excluded.created_by,
  valid_from = excluded.valid_from,
  half_life_days = CASE WHEN excluded.half_life_days > 0 THEN excluded.half_life_days ELSE claims.half_life_days END,
  -- Scope, provenance and visibility are PRESERVED when the incoming value is
  -- zero, generalising the half_life_days rule above (#334, #345, #346).
  --
  -- No ingest path produces any of these. The writers that can hit an existing
  -- claim id -- POST /v1/beliefs and gRPC WriteBeliefs -- build a Claim from a
  -- request with no scope, visibility, citation_count, last_executed or
  -- provenance_rationale field at all, so a blind `= excluded.x` lets a partial
  -- write erase curation that nothing can reconstruct. An explicit value still
  -- always wins, so nothing becomes uncorrectable.
  scope_service = CASE WHEN excluded.scope_service <> '' THEN excluded.scope_service ELSE claims.scope_service END,
  scope_env = CASE WHEN excluded.scope_env <> '' THEN excluded.scope_env ELSE claims.scope_env END,
  scope_team = CASE WHEN excluded.scope_team <> '' THEN excluded.scope_team ELSE claims.scope_team END,
  source_document = CASE WHEN excluded.source_document <> '' THEN excluded.source_document ELSE claims.source_document END,
  source_type = CASE WHEN excluded.source_type <> '' THEN excluded.source_type ELSE claims.source_type END,
  source_authority = CASE WHEN excluded.source_authority > 0 THEN excluded.source_authority ELSE claims.source_authority END,
  liveness = CASE WHEN excluded.liveness <> '' THEN excluded.liveness ELSE claims.liveness END,
  -- `<> ''` rather than the `IS NOT NULL` the hosted backends use: this backend
  -- stores last_executed as an RFC3339 STRING and writes the empty string for a
  -- zero time, so it is never NULL and an IS NOT NULL test would always fire.
  last_executed = CASE WHEN excluded.last_executed <> '' THEN excluded.last_executed ELSE claims.last_executed END,
  citation_count = CASE WHEN excluded.citation_count > 0 THEN excluded.citation_count ELSE claims.citation_count END,
  provenance_rationale = CASE WHEN excluded.provenance_rationale <> '' THEN excluded.provenance_rationale ELSE claims.provenance_rationale END,
  test_id = excluded.test_id,
  test_requirement_ref = excluded.test_requirement_ref,
  test_author = excluded.test_author,
  test_last_modified = excluded.test_last_modified,
  test_last_run_at = excluded.test_last_run_at,
  test_pass_count = excluded.test_pass_count,
  test_fail_count = excluded.test_fail_count,
  -- Requires the write path to bind visibility RAW rather than normalised to
  -- "team": normalising makes "unset" and "explicitly team" the same string, and
  -- this rule has to tell them apart. Reads still normalise, so an empty stored
  -- value presents as the default and no existing row changes meaning.
  visibility = CASE WHEN excluded.visibility <> '' THEN excluded.visibility ELSE claims.visibility END,
  confidence_components = excluded.confidence_components,
  lifecycle = excluded.lifecycle,
  subject_class = excluded.subject_class,
  durability = excluded.durability;

-- name: SetClaimValidity :exec
-- Atomic supersession primitive: mark a claim as no longer valid as
-- of the given timestamp. Pass NULL to clear valid_to (un-supersede
-- the claim), useful when a resolution is reverted.
UPDATE claims SET valid_to = ? WHERE id = ?;

-- name: MarkClaimVerified :exec
-- Bumps last_verified to the supplied timestamp and increments
-- verify_count by one. The half_life_days COALESCE keeps any
-- existing override when the caller passes 0 (sqlc binds it as the
-- third parameter); a non-zero value replaces the override.
UPDATE claims
SET last_verified = ?,
    verify_count = verify_count + 1,
    half_life_days = CASE WHEN ? > 0 THEN ? ELSE half_life_days END
WHERE id = ?;

-- name: UpsertClaimEvidence :exec
INSERT INTO claim_evidence (claim_id, event_id)
VALUES (?, ?)
ON CONFLICT(claim_id, event_id) DO NOTHING;

-- name: ListAllClaims :many
SELECT id, text, type, confidence, status, created_at, created_by, trust_score,
       valid_from, valid_to, last_verified, verify_count, half_life_days,
       scope_service, scope_env, scope_team,
       source_document, source_type, source_authority, liveness,
       last_executed, citation_count, provenance_rationale,
       test_id, test_requirement_ref, test_author,
       test_last_modified, test_last_run_at, test_pass_count, test_fail_count,
       visibility, confidence_components, lifecycle, subject_class, durability
FROM claims
ORDER BY created_at ASC;

-- name: ListClaimsByTestRequirementRef :many
-- Filter to test_result claims sharing a TestRequirementRef. Drives
-- `mnemos trust --test=<ref>` and the which_test_to_trust MCP tool: the
-- previous implementation called ListAllClaims and filtered in Go,
-- which scaled O(n) per invocation.
SELECT id, text, type, confidence, status, created_at, created_by, trust_score,
       valid_from, valid_to, last_verified, verify_count, half_life_days,
       scope_service, scope_env, scope_team,
       source_document, source_type, source_authority, liveness,
       last_executed, citation_count, provenance_rationale,
       test_id, test_requirement_ref, test_author,
       test_last_modified, test_last_run_at, test_pass_count, test_fail_count,
       visibility, confidence_components, lifecycle, subject_class, durability
FROM claims
WHERE type = 'test_result'
  AND test_requirement_ref = ?
ORDER BY test_last_run_at DESC, created_at DESC;

-- name: UpdateClaimTrust :exec
UPDATE claims SET trust_score = ? WHERE id = ?;

-- name: ListClaimTrustInputs :many
-- Inputs to recompute trust_score for every claim: confidence, the count of
-- DISTINCT evidence-event authors and of total events (so corroboration can be
-- graded by independence - an echo-chamber guard), and the most-recent evidence
-- timestamp. LEFT JOIN so claims with no evidence still appear; the caller treats
-- the missing aggregate as 0/empty.
SELECT
  c.id              AS claim_id,
  c.confidence      AS confidence,
  COUNT(DISTINCT e.created_by) AS distinct_sources,
  COUNT(DISTINCT ce.event_id)  AS total_events,
  CAST(COALESCE(MAX(e.timestamp), '') AS TEXT) AS latest_evidence_at
FROM claims c
LEFT JOIN claim_evidence ce ON ce.claim_id = c.id
LEFT JOIN events e          ON e.id = ce.event_id
GROUP BY c.id, c.confidence;

-- name: ListClaimTrustInputsForClaims :many
-- Same inputs as ListClaimTrustInputs, bounded to the given claims.
--
-- Trust is a function of a claim's own confidence, its evidence count and its
-- most recent evidence, so only claims a write actually touched can change.
-- Recomputing every claim on every write made ingest cost grow with the size
-- of the brain: a single capture rewrote all ~11k rows, and under -race that
-- eventually exceeded the write budget outright.
SELECT
  c.id              AS claim_id,
  c.confidence      AS confidence,
  COUNT(DISTINCT e.created_by) AS distinct_sources,
  COUNT(DISTINCT ce.event_id)  AS total_events,
  CAST(COALESCE(MAX(e.timestamp), '') AS TEXT) AS latest_evidence_at
FROM claims c
LEFT JOIN claim_evidence ce ON ce.claim_id = c.id
LEFT JOIN events e          ON e.id = ce.event_id
WHERE c.id IN (sqlc.slice('claim_ids'))
GROUP BY c.id, c.confidence;

-- name: AverageTrust :one
SELECT CAST(COALESCE(AVG(trust_score), 0) AS REAL) AS avg_trust FROM claims;

-- name: CountClaimsBelowTrust :one
SELECT COUNT(*) AS n FROM claims WHERE trust_score < ?;

-- name: DeleteClaimByID :exec
DELETE FROM claims WHERE id = ?;

-- name: DeleteAllClaims :exec
DELETE FROM claims;

-- name: DeleteClaimEvidenceByClaimID :exec
DELETE FROM claim_evidence WHERE claim_id = ?;

-- name: DeleteAllClaimEvidence :exec
DELETE FROM claim_evidence;

-- name: DeleteClaimStatusHistoryByClaimID :exec
DELETE FROM claim_status_history WHERE claim_id = ?;

-- name: DeleteAllClaimStatusHistory :exec
DELETE FROM claim_status_history;
