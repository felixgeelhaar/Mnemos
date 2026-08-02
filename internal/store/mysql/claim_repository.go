package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// ClaimRepository implements ports.ClaimRepository (and
// ports.TrustScorer) against MySQL. INSERT … ON DUPLICATE KEY UPDATE
// is the MySQL analog of Postgres's ON CONFLICT … DO UPDATE.
type ClaimRepository struct {
	db *sql.DB
}

// claimUpsertSQL is the claim write statement.
//
// It is a package-level constant so the projection guard
// (TestClaimProjection_* in claim_projection_guard_test.go) can parse the
// statement the repository actually executes rather than a copy that
// would drift from it — the very failure mode the guard exists to catch.
// Every column in the ON DUPLICATE KEY UPDATE set is rewritten from
// whatever a read returned, so a column here that claimColumnNames omits
// is silently zeroed by any read-modify-write pass.
//
// The scope, provenance and visibility columns all resolve a conflict with
// "the incoming value wins ONLY when it carries one" rather than a plain
// VALUES() assignment. The reason is #334's, generalised: no ingest path
// produces any of these (extraction sets none of them), and every partial
// writer — POST /v1/beliefs and the gRPC WriteBeliefs equivalent build a claim
// field-by-field from a request whose scope, citation_count, last_executed and
// provenance_rationale fields do not even exist — therefore carries a zero. A
// blind VALUES() would let any such write silently ERASE a scope set by
// markdown import or consolidate/promote, an audience set explicitly through
// the API, or provenance that nothing can reconstruct. The reverse risk is
// bounded and recoverable: a stale value is overwritten by supplying the new
// one, since a non-empty incoming value always wins. Clearing a value back to
// empty is not expressible through the upsert — as with verify_count, that
// needs a statement that owns the column.
//
// This diverges from SQLite, which assigns these blindly and therefore still
// carries the erase hazard. Mirroring the hazard for symmetry's sake was the
// alternative, and #340 already set the precedent against it by declining to
// copy Postgres's lifecycle rule to this backend.
const claimUpsertSQL = `
INSERT INTO claims (id, text, type, confidence, status, created_at, created_by, valid_from, trust_score, valid_to, half_life_days, subject_class, durability, confidence_components,
                    test_id, test_requirement_ref, test_author, test_last_modified, test_last_run_at, test_pass_count, test_fail_count,
                    scope_service, scope_env, scope_team,
                    source_document, source_type, source_authority, liveness, last_executed, citation_count, provenance_rationale, visibility)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
        ?, ?, ?,
        ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  text = VALUES(text),
  type = VALUES(type),
  confidence = VALUES(confidence),
  status = VALUES(status),
  valid_from = VALUES(valid_from),
  half_life_days = CASE WHEN VALUES(half_life_days) > 0 THEN VALUES(half_life_days) ELSE half_life_days END,
  subject_class = VALUES(subject_class),
  durability = VALUES(durability),
  confidence_components = VALUES(confidence_components),
  test_id = VALUES(test_id),
  test_requirement_ref = VALUES(test_requirement_ref),
  test_author = VALUES(test_author),
  test_last_modified = VALUES(test_last_modified),
  test_last_run_at = VALUES(test_last_run_at),
  test_pass_count = VALUES(test_pass_count),
  test_fail_count = VALUES(test_fail_count),
  scope_service = CASE WHEN VALUES(scope_service) <> '' THEN VALUES(scope_service) ELSE scope_service END,
  scope_env = CASE WHEN VALUES(scope_env) <> '' THEN VALUES(scope_env) ELSE scope_env END,
  scope_team = CASE WHEN VALUES(scope_team) <> '' THEN VALUES(scope_team) ELSE scope_team END,
  source_document = CASE WHEN VALUES(source_document) <> '' THEN VALUES(source_document) ELSE source_document END,
  source_type = CASE WHEN VALUES(source_type) <> '' THEN VALUES(source_type) ELSE source_type END,
  source_authority = CASE WHEN VALUES(source_authority) > 0 THEN VALUES(source_authority) ELSE source_authority END,
  liveness = CASE WHEN VALUES(liveness) <> '' THEN VALUES(liveness) ELSE liveness END,
  last_executed = CASE WHEN VALUES(last_executed) IS NOT NULL THEN VALUES(last_executed) ELSE last_executed END,
  citation_count = CASE WHEN VALUES(citation_count) > 0 THEN VALUES(citation_count) ELSE citation_count END,
  provenance_rationale = CASE WHEN VALUES(provenance_rationale) <> '' THEN VALUES(provenance_rationale) ELSE provenance_rationale END,
  visibility = CASE WHEN VALUES(visibility) <> '' THEN VALUES(visibility) ELSE visibility END`

// Upsert is the no-reason variant; status_history rows lose their
// reason/changed_by attribution.
func (r ClaimRepository) Upsert(ctx context.Context, claims []domain.Claim) error {
	return r.upsertWithReason(ctx, claims, "", "")
}

// UpsertWithReason captures a free-form reason on the transition row.
func (r ClaimRepository) UpsertWithReason(ctx context.Context, claims []domain.Claim, reason string) error {
	return r.upsertWithReason(ctx, claims, reason, "")
}

// UpsertWithReasonAs is the actor-aware variant.
func (r ClaimRepository) UpsertWithReasonAs(ctx context.Context, claims []domain.Claim, reason, changedBy string) error {
	return r.upsertWithReason(ctx, claims, reason, changedBy)
}

func (r ClaimRepository) upsertWithReason(ctx context.Context, claims []domain.Claim, reason, changedBy string) error {
	if len(claims) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin claim upsert tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	upsert := claimUpsertSQL
	historyInsert := `
INSERT INTO claim_status_history (claim_id, from_status, to_status, changed_at, reason, changed_by)
VALUES (?, ?, ?, ?, ?, ?)`
	priorQuery := `SELECT status FROM claims WHERE id = ?`

	for _, claim := range claims {
		if err := claim.Validate(); err != nil {
			return fmt.Errorf("invalid claim %s: %w", claim.ID, err)
		}
		var priorStatus string
		err := tx.QueryRowContext(ctx, priorQuery, claim.ID).Scan(&priorStatus)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("look up prior status for %s: %w", claim.ID, err)
		}

		validFrom := claim.ValidFrom
		if validFrom.IsZero() {
			validFrom = claim.CreatedAt
		}
		if _, err := tx.ExecContext(ctx, upsert,
			claim.ID, claim.Text, string(claim.Type), claim.Confidence,
			string(claim.Status), claim.CreatedAt.UTC(), actorOr(claim.CreatedBy),
			validFrom.UTC(), claim.HalfLifeDays, string(claim.SubjectClass),
			string(claim.Durability.Normalized()), encodeConfidenceComponents(claim.ConfidenceComponents),
			claim.TestID, claim.TestRequirementRef, claim.TestAuthor,
			nullTime(claim.TestLastModified), nullTime(claim.TestLastRunAt),
			claim.TestPassCount, claim.TestFailCount,
			claim.Scope.Service, claim.Scope.Env, claim.Scope.Team,
			claim.SourceDocument, string(claim.SourceType), claim.SourceAuthority,
			string(claim.Liveness), nullTime(claim.LastExecuted), claim.CitationCount,
			claim.ProvenanceRationale,
			// Visibility is bound RAW, not normalised to the default the way
			// SQLite does on write. Normalising here would make "unset" and
			// "explicitly team" the same string, and the conflict rule above
			// distinguishes exactly those two: an unset audience must preserve a
			// stored 'personal', an explicit 'team' must override it. The read
			// side normalises instead (see scanClaimRow), so no caller ever sees
			// an empty audience.
			string(claim.Visibility),
		); err != nil {
			return fmt.Errorf("upsert claim %s: %w", claim.ID, err)
		}

		newStatus := string(claim.Status)
		if priorStatus == newStatus {
			continue
		}
		if _, err := tx.ExecContext(ctx, historyInsert,
			claim.ID, priorStatus, newStatus, now, reason, actorOr(changedBy),
		); err != nil {
			return fmt.Errorf("record status transition for %s: %w", claim.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit claim upsert tx: %w", err)
	}
	return nil
}

// UpsertEvidence inserts (claim, event) link rows; INSERT IGNORE
// makes it idempotent.
func (r ClaimRepository) UpsertEvidence(ctx context.Context, links []domain.ClaimEvidence) error {
	if len(links) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin evidence tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt := `INSERT IGNORE INTO claim_evidence (claim_id, event_id) VALUES (?, ?)`
	for _, link := range links {
		if err := link.Validate(); err != nil {
			return fmt.Errorf("invalid claim evidence: %w", err)
		}
		if _, err := tx.ExecContext(ctx, stmt, link.ClaimID, link.EventID); err != nil {
			return fmt.Errorf("upsert claim evidence (%s,%s): %w", link.ClaimID, link.EventID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit evidence tx: %w", err)
	}
	return nil
}

// ListByEventIDs returns claims linked to any of the given event ids.
func (r ClaimRepository) ListByEventIDs(ctx context.Context, eventIDs []string) ([]domain.Claim, error) {
	if len(eventIDs) == 0 {
		return []domain.Claim{}, nil
	}
	placeholders, args := inPlaceholders(eventIDs)
	//nolint:gosec // G202: placeholders are literal "?" tokens, not user input
	q := `
SELECT DISTINCT ` + claimColumns("c") + `
FROM claims c
JOIN claim_evidence ce ON ce.claim_id = c.id
WHERE ce.event_id IN (` + placeholders + `)
ORDER BY c.created_at ASC`
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list claims by event ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectClaimRows(rows)
}

// ListEvidenceByClaimIDs returns the (claim_id, event_id) link rows.
func (r ClaimRepository) ListEvidenceByClaimIDs(ctx context.Context, claimIDs []string) ([]domain.ClaimEvidence, error) {
	if len(claimIDs) == 0 {
		return []domain.ClaimEvidence{}, nil
	}
	placeholders, args := inPlaceholders(claimIDs)
	//nolint:gosec // G202: placeholders are literal "?" tokens, not user input
	rows, err := r.db.QueryContext(ctx, `SELECT claim_id, event_id FROM claim_evidence WHERE claim_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("list evidence by claim ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]domain.ClaimEvidence, 0)
	for rows.Next() {
		var ev domain.ClaimEvidence
		if err := rows.Scan(&ev.ClaimID, &ev.EventID); err != nil {
			return nil, fmt.Errorf("scan claim evidence row: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ListByIDs returns claims with the given ids.
func (r ClaimRepository) ListByIDs(ctx context.Context, claimIDs []string) ([]domain.Claim, error) {
	if len(claimIDs) == 0 {
		return []domain.Claim{}, nil
	}
	placeholders, args := inPlaceholders(claimIDs)
	//nolint:gosec // G202: placeholders are literal "?" tokens, not user input
	rows, err := r.db.QueryContext(ctx, `
SELECT `+claimColumns("")+`
FROM claims WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("list claims by ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectClaimRows(rows)
}

// RepointEvidence rewrites claim_evidence rows; INSERT IGNORE
// collapses duplicates on the (claim_id, event_id) primary key.
func (r ClaimRepository) RepointEvidence(ctx context.Context, fromClaimID, toClaimID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin repoint evidence tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
INSERT IGNORE INTO claim_evidence (claim_id, event_id)
SELECT ?, event_id FROM claim_evidence WHERE claim_id = ?`,
		toClaimID, fromClaimID,
	); err != nil {
		return fmt.Errorf("copy evidence %s -> %s: %w", fromClaimID, toClaimID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM claim_evidence WHERE claim_id = ?`, fromClaimID); err != nil {
		return fmt.Errorf("delete original evidence %s: %w", fromClaimID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit repoint evidence tx: %w", err)
	}
	return nil
}

// DeleteCascade drops the claim plus every claim-keyed row it owns, in
// one tx.
//
// The set is the canonical one from [ports.ClaimRepository], narrowed to
// the tables this backend declares: claim_evidence,
// claim_status_history, then the claim row. MySQL has no
// claim_versions, claim_feedback or claim_expectations table, so there
// is nothing to clear for those — if one is ever added to schema.sql it
// must be added here in the same change, or a delete on MySQL will mean
// something different from a delete on SQLite.
func (r ClaimRepository) DeleteCascade(ctx context.Context, claimID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin claim delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`DELETE FROM claim_evidence WHERE claim_id = ?`,
		`DELETE FROM claim_status_history WHERE claim_id = ?`,
		`DELETE FROM claims WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, claimID); err != nil {
			return fmt.Errorf("claim delete cascade: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit claim delete tx: %w", err)
	}
	return nil
}

// ListAll returns every claim ordered by created_at.
func (r ClaimRepository) ListAll(ctx context.Context) ([]domain.Claim, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT `+claimColumns("")+`
FROM claims ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list all claims: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectClaimRows(rows)
}

// ListByTestRequirementRef returns test_result claims sharing the given
// non-empty TestRequirementRef, freshest run first. Backed by
// idx_claims_test_requirement_ref (test_requirement_ref, type). Both the index
// and the test-provenance columns it covers were only declared alongside this
// fix: before that the query named columns the schema never had and failed with
// "unknown column", so test-vs-test contradiction resolution was not slow on
// MySQL, it was broken.
func (r ClaimRepository) ListByTestRequirementRef(ctx context.Context, ref string) ([]domain.Claim, error) {
	if ref == "" {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT `+claimColumns("")+`
FROM claims
WHERE type = 'test_result' AND test_requirement_ref = ?
ORDER BY test_last_run_at DESC, created_at DESC`, ref)
	if err != nil {
		return nil, fmt.Errorf("list claims by test_requirement_ref: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectClaimRows(rows)
}

// CountAll returns the total number of claims stored.
func (r ClaimRepository) CountAll(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM claims`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count claims: %w", err)
	}
	return n, nil
}

// ListAllEvidence returns every (claim_id, event_id) link.
func (r ClaimRepository) ListAllEvidence(ctx context.Context) ([]domain.ClaimEvidence, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT claim_id, event_id FROM claim_evidence`)
	if err != nil {
		return nil, fmt.Errorf("list all claim evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]domain.ClaimEvidence, 0)
	for rows.Next() {
		var ev domain.ClaimEvidence
		if err := rows.Scan(&ev.ClaimID, &ev.EventID); err != nil {
			return nil, fmt.Errorf("scan claim_evidence row: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// DeleteAll wipes claims plus the rows owned by claims (claim_evidence,
// claim_status_history) inside a single transaction.
func (r ClaimRepository) DeleteAll(ctx context.Context) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin claims delete-all tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM claim_evidence`); err != nil {
		return fmt.Errorf("delete claim_evidence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM claim_status_history`); err != nil {
		return fmt.Errorf("delete claim_status_history: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM claims`); err != nil {
		return fmt.Errorf("delete claims: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit claims delete-all tx: %w", err)
	}
	return nil
}

// ListIDsMissingEmbedding returns claim ids without an embedding row.
func (r ClaimRepository) ListIDsMissingEmbedding(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT c.id FROM claims c
LEFT JOIN embeddings e ON e.entity_id = c.id AND e.entity_type = 'claim'
WHERE e.entity_id IS NULL
ORDER BY c.created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list ids missing embedding: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListAllStatusHistory returns every claim_status_history row.
func (r ClaimRepository) ListAllStatusHistory(ctx context.Context) ([]domain.ClaimStatusTransition, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT claim_id, from_status, to_status, changed_at, reason, changed_by
FROM claim_status_history ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list all status history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]domain.ClaimStatusTransition, 0)
	for rows.Next() {
		var t domain.ClaimStatusTransition
		var from, to string
		if err := rows.Scan(&t.ClaimID, &from, &to, &t.ChangedAt, &t.Reason, &t.ChangedBy); err != nil {
			return nil, fmt.Errorf("scan status_history row: %w", err)
		}
		t.FromStatus = domain.ClaimStatus(from)
		t.ToStatus = domain.ClaimStatus(to)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListStatusHistoryByClaimID returns the claim's transition rows.
func (r ClaimRepository) ListStatusHistoryByClaimID(ctx context.Context, claimID string) ([]domain.ClaimStatusTransition, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT claim_id, from_status, to_status, changed_at, reason, changed_by
FROM claim_status_history WHERE claim_id = ? ORDER BY id ASC`, claimID)
	if err != nil {
		return nil, fmt.Errorf("list status history for %s: %w", claimID, err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]domain.ClaimStatusTransition, 0)
	for rows.Next() {
		var t domain.ClaimStatusTransition
		var from, to string
		if err := rows.Scan(&t.ClaimID, &from, &to, &t.ChangedAt, &t.Reason, &t.ChangedBy); err != nil {
			return nil, fmt.Errorf("scan status history row: %w", err)
		}
		t.FromStatus = domain.ClaimStatus(from)
		t.ToStatus = domain.ClaimStatus(to)
		out = append(out, t)
	}
	return out, rows.Err()
}

// MarkVerified bumps last_verified and increments verify_count.
// Optional half_life_days override applies when the caller passes a
// non-zero value.
func (r ClaimRepository) MarkVerified(ctx context.Context, claimID string, verifiedAt time.Time, halfLifeDays float64) error {
	if verifiedAt.IsZero() {
		verifiedAt = time.Now().UTC()
	}
	res, err := r.db.ExecContext(ctx, `
UPDATE claims
SET last_verified = ?,
    verify_count = verify_count + 1,
    half_life_days = CASE WHEN ? > 0 THEN ? ELSE half_life_days END
WHERE id = ?`, verifiedAt.UTC(), halfLifeDays, halfLifeDays, claimID)
	if err != nil {
		return fmt.Errorf("mark verified %s: %w", claimID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("claim %s: %w", claimID, sql.ErrNoRows)
	}
	return nil
}

// ApplyBeliefCredit overwrites the claim's confidence_components map and sets its
// trust_score together (the ports.BeliefCreditWriter capability, ADR 0014). The
// caller passes the already-merged map, so the write is a plain assignment and
// re-running is idempotent — letting credit assignment + salience persist on MySQL.
func (r ClaimRepository) ApplyBeliefCredit(ctx context.Context, claimID string, components map[string]float64, trustScore float64) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE claims SET confidence_components = ?, trust_score = ? WHERE id = ?`,
		encodeConfidenceComponents(components), trustScore, claimID)
	if err != nil {
		return fmt.Errorf("apply belief credit for %s: %w", claimID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("apply belief credit: claim %s: %w", claimID, sql.ErrNoRows)
	}
	return nil
}

// SetValidity updates the claim's valid_to.
func (r ClaimRepository) SetValidity(ctx context.Context, claimID string, validTo time.Time) error {
	var args []any
	var stmt string
	if validTo.IsZero() {
		stmt = `UPDATE claims SET valid_to = NULL WHERE id = ?`
		args = []any{claimID}
	} else {
		stmt = `UPDATE claims SET valid_to = ? WHERE id = ?`
		args = []any{validTo.UTC(), claimID}
	}
	res, err := r.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return fmt.Errorf("set validity for %s: %w", claimID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("claim %s: %w", claimID, sql.ErrNoRows)
	}
	return nil
}

// SetLifecycle transitions a claim's promotion state in place.
func (r ClaimRepository) SetLifecycle(ctx context.Context, claimID string, lifecycle domain.ClaimLifecycle) error {
	res, err := r.db.ExecContext(ctx, `UPDATE claims SET lifecycle = ? WHERE id = ?`, string(lifecycle), claimID)
	if err != nil {
		return fmt.Errorf("set lifecycle for %s: %w", claimID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("claim %s: %w", claimID, sql.ErrNoRows)
	}
	return nil
}

// trustInput is one claim's scoring inputs: its own confidence, how many
// DISTINCT evidence-event authors corroborate it, how many evidence events
// there are in total, and the most recent evidence timestamp.
type trustInput struct {
	id              string
	confidence      float64
	distinctSources int
	totalEvents     int
	latest          time.Time
}

// trustInputsSelect is the aggregate that feeds trust scoring. COUNT distinct
// evidence-event AUTHORS and total events separately, so corroboration can be
// graded by independence (echo-chamber guard). LEFT JOIN so claims with no
// evidence still appear.
const trustInputsSelect = `
SELECT c.id, c.confidence, COUNT(DISTINCT e.created_by), COUNT(DISTINCT ce.event_id), MAX(e.timestamp)
FROM claims c
LEFT JOIN claim_evidence ce ON ce.claim_id = c.id
LEFT JOIN events e ON e.id = ce.event_id
`

// trustIDChunk bounds how many claim ids go into one IN-list. MySQL caps a
// prepared statement at 65535 placeholders, and a scoped rescore is fed the ids
// of everything a single write touched, which is caller-controlled — so the read
// is chunked rather than trusting the batch to stay small.
const trustIDChunk = 1000

// listTrustInputs runs the aggregate and collects its rows.
func (r ClaimRepository) listTrustInputs(ctx context.Context, query string, args ...any) ([]trustInput, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var inputs []trustInput
	for rows.Next() {
		var in trustInput
		var latest sql.NullTime
		if err := rows.Scan(&in.id, &in.confidence, &in.distinctSources, &in.totalEvents, &latest); err != nil {
			return nil, fmt.Errorf("scan trust input: %w", err)
		}
		if latest.Valid {
			in.latest = latest.Time
		}
		inputs = append(inputs, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trust inputs: %w", err)
	}
	return inputs, nil
}

// applyTrustInputs scores each row and writes trust_score back in one
// transaction. Returns the number of claims touched.
func (r ClaimRepository) applyTrustInputs(ctx context.Context, inputs []trustInput, score func(confidence float64, evidenceCount int, latestEvidence time.Time) float64) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin trust tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, in := range inputs {
		s := score(in.confidence, domain.EffectiveEvidenceCount(in.distinctSources, in.totalEvents), in.latest)
		if _, err := tx.ExecContext(ctx, `UPDATE claims SET trust_score = ? WHERE id = ?`, s, in.id); err != nil {
			return 0, fmt.Errorf("update trust for %s: %w", in.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit trust update: %w", err)
	}
	return len(inputs), nil
}

// RecomputeTrust applies the supplied scoring function to every
// claim. Returns the count touched. Implements ports.TrustScorer.
func (r ClaimRepository) RecomputeTrust(ctx context.Context, score func(confidence float64, evidenceCount int, latestEvidence time.Time) float64) (int, error) {
	inputs, err := r.listTrustInputs(ctx, trustInputsSelect+`GROUP BY c.id, c.confidence`)
	if err != nil {
		return 0, fmt.Errorf("list trust inputs: %w", err)
	}
	return r.applyTrustInputs(ctx, inputs, score)
}

// RecomputeTrustForClaims implements [ports.ScopedTrustScorer]: the same
// recomputation bounded to claimIDs, so a write's cost tracks what it touched
// rather than the size of the store. Ids with no matching claim are skipped, so
// the returned count is the number of claims actually rescored.
func (r ClaimRepository) RecomputeTrustForClaims(ctx context.Context, claimIDs []string, score func(confidence float64, evidenceCount int, latestEvidence time.Time) float64) (int, error) {
	if len(claimIDs) == 0 {
		return 0, nil
	}
	var inputs []trustInput
	for start := 0; start < len(claimIDs); start += trustIDChunk {
		end := min(start+trustIDChunk, len(claimIDs))
		placeholders, args := inPlaceholders(claimIDs[start:end])
		//nolint:gosec // G202: placeholders are literal "?" tokens, not user input
		q := trustInputsSelect + `WHERE c.id IN (` + placeholders + `)
GROUP BY c.id, c.confidence`
		chunk, err := r.listTrustInputs(ctx, q, args...)
		if err != nil {
			return 0, fmt.Errorf("list trust inputs for claims: %w", err)
		}
		inputs = append(inputs, chunk...)
	}
	return r.applyTrustInputs(ctx, inputs, score)
}

// AverageTrust returns the mean trust_score across every claim.
func (r ClaimRepository) AverageTrust(ctx context.Context) (float64, error) {
	var avg sql.NullFloat64
	err := r.db.QueryRowContext(ctx, `SELECT AVG(trust_score) FROM claims`).Scan(&avg)
	if err != nil {
		return 0, fmt.Errorf("average trust: %w", err)
	}
	if !avg.Valid {
		return 0, nil
	}
	return avg.Float64, nil
}

// CountClaimsBelowTrust returns how many claims fall below threshold.
func (r ClaimRepository) CountClaimsBelowTrust(ctx context.Context, threshold float64) (int64, error) {
	var n int64
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM claims WHERE trust_score < ?`, threshold).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count claims below trust: %w", err)
	}
	return n, nil
}

// nullTime maps the zero time to SQL NULL so an unset timestamp reads back as
// "unknown" rather than as a real instant.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// claimColumnNames is the ONE projection every claim read uses, in the exact
// order scanClaimRow scans. Hand-written per-query projections drift out of
// step with the scanner; the list lives here and every reader goes through
// claimColumns.
//
// Being the one projection is not by itself a guard: the list is still
// hand-maintained, and it has silently omitted columns the schema declares
// twice (#331/#334, #335). claim_projection_guard_test.go now diffs it against
// the embedded DDL, the claim upsert's INSERT list and its ON DUPLICATE KEY
// UPDATE set, so an omission fails a test instead of zeroing production rows.
var claimColumnNames = []string{
	"id", "text", "type", "confidence", "status", "created_at", "created_by",
	"trust_score", "valid_from", "valid_to", "last_verified", "verify_count",
	"half_life_days", "lifecycle", "subject_class", "durability",
	"confidence_components",
	"test_id", "test_requirement_ref", "test_author", "test_last_modified",
	"test_last_run_at", "test_pass_count", "test_fail_count",
	"scope_service", "scope_env", "scope_team",
	"source_document", "source_type", "source_authority", "liveness",
	"last_executed", "citation_count", "provenance_rationale", "visibility",
}

// claimColumns renders the shared projection, optionally table-qualified
// (alias "c" yields "c.id, c.text, ...") for the JOIN queries.
func claimColumns(alias string) string {
	cols := make([]string, len(claimColumnNames))
	for i, c := range claimColumnNames {
		if alias == "" {
			cols[i] = c
			continue
		}
		cols[i] = alias + "." + c
	}
	return strings.Join(cols, ", ")
}

// visibilityOrDefault normalises a stored audience the way SQLite's column
// default and the memory backend both do: empty (a row written before the
// column existed, or by a caller that expressed no audience) and any
// unrecognised value present as domain.DefaultVisibility. A read must never
// hand back an empty audience — query.admission would coerce it anyway, and an
// empty value at the store boundary makes "team" and "never set"
// indistinguishable, which is the ambiguity that let #331 and #335 survive.
func visibilityOrDefault(v domain.Visibility) domain.Visibility {
	switch v {
	case domain.VisibilityPersonal, domain.VisibilityTeam, domain.VisibilityOrg:
		return v
	default:
		return domain.DefaultVisibility
	}
}

func collectClaimRows(rows *sql.Rows) ([]domain.Claim, error) {
	out := make([]domain.Claim, 0)
	for rows.Next() {
		c, err := scanClaimRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanClaimRow(rows *sql.Rows) (domain.Claim, error) {
	var c domain.Claim
	var typ, status, lifecycle, subjectClass, durability string
	// confidence_components is `JSON NULL` with no default on this backend —
	// unlike Postgres, where it is NOT NULL DEFAULT '{}'. Every row mnemos
	// writes carries a value, but a row that predates the ALTER holds SQL NULL,
	// and scanning that into a plain string fails the whole read with
	// "converting NULL to string is unsupported". The claim is not degraded,
	// it is UNREADABLE — so an upgraded MySQL brain lost every claim written
	// before the column existed. NullString decodes NULL as "no decomposition
	// available", which is what decodeConfidenceComponents already means by "".
	var confidenceComponents sql.NullString
	var validFrom sql.NullTime
	var validTo sql.NullTime
	// last_verified is NULL until the first MarkVerified, and NULL is the
	// cross-backend "never verified" sentinel — scanning it into a NullTime
	// keeps the zero time rather than inventing an instant.
	var lastVerified sql.NullTime
	var testLastModified, testLastRunAt sql.NullTime
	var scopeService, scopeEnv, scopeTeam string
	var sourceDocument, sourceType, liveness, provenanceRationale, visibility string
	// last_executed is NULL until something records an execution, and NULL is
	// the cross-backend "never executed" sentinel trust.EvaluateLiveness reads
	// as unknown — scanning it into a NullTime keeps the zero time rather than
	// inventing an instant that would read as decades of decay.
	var lastExecuted sql.NullTime
	if err := rows.Scan(
		&c.ID, &c.Text, &typ, &c.Confidence, &status,
		&c.CreatedAt, &c.CreatedBy, &c.TrustScore, &validFrom, &validTo, &lastVerified, &c.VerifyCount,
		&c.HalfLifeDays, &lifecycle, &subjectClass, &durability, &confidenceComponents,
		&c.TestID, &c.TestRequirementRef, &c.TestAuthor, &testLastModified, &testLastRunAt, &c.TestPassCount, &c.TestFailCount,
		&scopeService, &scopeEnv, &scopeTeam,
		&sourceDocument, &sourceType, &c.SourceAuthority, &liveness,
		&lastExecuted, &c.CitationCount, &provenanceRationale, &visibility,
	); err != nil {
		return domain.Claim{}, fmt.Errorf("scan claim row: %w", err)
	}
	c.Type = domain.ClaimType(typ)
	c.Status = domain.ClaimStatus(status)
	c.Lifecycle = domain.ClaimLifecycle(lifecycle)
	c.SubjectClass = domain.SubjectClass(subjectClass)
	c.Durability = domain.Durability(durability)
	c.ConfidenceComponents = decodeConfidenceComponents(confidenceComponents.String)
	c.Scope = domain.Scope{Service: scopeService, Env: scopeEnv, Team: scopeTeam}
	c.SourceDocument = sourceDocument
	c.SourceType = domain.SourceType(sourceType)
	c.Liveness = domain.LivenessStatus(liveness)
	c.ProvenanceRationale = provenanceRationale
	c.Visibility = visibilityOrDefault(domain.Visibility(visibility))
	if lastExecuted.Valid {
		c.LastExecuted = lastExecuted.Time
	}
	if validFrom.Valid {
		c.ValidFrom = validFrom.Time
	}
	if validTo.Valid {
		c.ValidTo = validTo.Time
	}
	if lastVerified.Valid {
		c.LastVerified = lastVerified.Time
	}
	if testLastModified.Valid {
		c.TestLastModified = testLastModified.Time
	}
	if testLastRunAt.Valid {
		c.TestLastRunAt = testLastRunAt.Time
	}
	if err := c.Validate(); err != nil {
		return domain.Claim{}, fmt.Errorf("validate persisted claim %s: %w", c.ID, err)
	}
	return c, nil
}
