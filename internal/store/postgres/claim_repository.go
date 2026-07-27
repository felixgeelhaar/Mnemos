package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// ClaimRepository persists claims, claim evidence links, and
// claim_status_history. Trust scoring (RecomputeTrust /
// AverageTrust / CountClaimsBelowTrust) is implemented so this
// repository satisfies ports.TrustScorer.
type ClaimRepository struct {
	db pgQuerier
	ns string
}

// Upsert satisfies the corresponding ports method.
func (r ClaimRepository) Upsert(ctx context.Context, claims []domain.Claim) error {
	return r.upsertWithReason(ctx, claims, "", "")
}

// UpsertWithReason satisfies the corresponding ports method.
func (r ClaimRepository) UpsertWithReason(ctx context.Context, claims []domain.Claim, reason string) error {
	return r.upsertWithReason(ctx, claims, reason, "")
}

// UpsertWithReasonAs satisfies the corresponding ports method.
func (r ClaimRepository) UpsertWithReasonAs(ctx context.Context, claims []domain.Claim, reason, changedBy string) error {
	return r.upsertWithReason(ctx, claims, reason, changedBy)
}

// upsertWithReason satisfies the corresponding ports method.
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
	upsert := fmt.Sprintf(`
INSERT INTO %s (id, text, type, confidence, status, created_at, created_by, valid_from, trust_score, valid_to, lifecycle, subject_class, durability, confidence_components,
                test_id, test_requirement_ref, test_author, test_last_modified, test_last_run_at, test_pass_count, test_fail_count)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 0, NULL, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
ON CONFLICT (id) DO UPDATE SET
  text = EXCLUDED.text,
  type = EXCLUDED.type,
  confidence = EXCLUDED.confidence,
  status = EXCLUDED.status,
  valid_from = EXCLUDED.valid_from,
  lifecycle = EXCLUDED.lifecycle,
  subject_class = EXCLUDED.subject_class,
  durability = EXCLUDED.durability,
  confidence_components = EXCLUDED.confidence_components,
  test_id = EXCLUDED.test_id,
  test_requirement_ref = EXCLUDED.test_requirement_ref,
  test_author = EXCLUDED.test_author,
  test_last_modified = EXCLUDED.test_last_modified,
  test_last_run_at = EXCLUDED.test_last_run_at,
  test_pass_count = EXCLUDED.test_pass_count,
  test_fail_count = EXCLUDED.test_fail_count`, qualify(r.ns, "claims"))
	historyInsert := fmt.Sprintf(`
INSERT INTO %s (claim_id, from_status, to_status, changed_at, reason, changed_by)
VALUES ($1, $2, $3, $4, $5, $6)`, qualify(r.ns, "claim_status_history"))
	priorQuery := fmt.Sprintf(`SELECT status FROM %s WHERE id = $1`, qualify(r.ns, "claims"))

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
			validFrom.UTC(), string(claim.Lifecycle), string(claim.SubjectClass),
			string(claim.Durability.Normalized()), encodeConfidenceComponents(claim.ConfidenceComponents),
			claim.TestID, claim.TestRequirementRef, claim.TestAuthor,
			nullTime(claim.TestLastModified), nullTime(claim.TestLastRunAt),
			claim.TestPassCount, claim.TestFailCount,
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

// UpsertEvidence inserts (claim, event) link rows. Idempotent.
func (r ClaimRepository) UpsertEvidence(ctx context.Context, links []domain.ClaimEvidence) error {
	if len(links) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin evidence tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt := fmt.Sprintf(`
INSERT INTO %s (claim_id, event_id) VALUES ($1, $2)
ON CONFLICT (claim_id, event_id) DO NOTHING`, qualify(r.ns, "claim_evidence"))
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

// ListByEventIDs returns the claims linked to any of the given event ids.
func (r ClaimRepository) ListByEventIDs(ctx context.Context, eventIDs []string) ([]domain.Claim, error) {
	if len(eventIDs) == 0 {
		return []domain.Claim{}, nil
	}
	q := fmt.Sprintf(`
SELECT DISTINCT `+claimColumns("c")+`
FROM %s c
JOIN %s ce ON ce.claim_id = c.id
WHERE ce.event_id = ANY($1)
ORDER BY c.created_at ASC`, qualify(r.ns, "claims"), qualify(r.ns, "claim_evidence"))
	rows, err := r.db.QueryContext(ctx, q, pgArray(eventIDs))
	if err != nil {
		return nil, fmt.Errorf("list claims by event ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectClaimRows(rows)
}

// ListEvidenceByClaimIDs satisfies the corresponding ports method.
func (r ClaimRepository) ListEvidenceByClaimIDs(ctx context.Context, claimIDs []string) ([]domain.ClaimEvidence, error) {
	if len(claimIDs) == 0 {
		return []domain.ClaimEvidence{}, nil
	}
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
SELECT claim_id, event_id FROM %s WHERE claim_id = ANY($1)`, qualify(r.ns, "claim_evidence")), pgArray(claimIDs))
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

// ListByIDs satisfies the corresponding ports method.
func (r ClaimRepository) ListByIDs(ctx context.Context, claimIDs []string) ([]domain.Claim, error) {
	if len(claimIDs) == 0 {
		return []domain.Claim{}, nil
	}
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
SELECT `+claimColumns("")+`
FROM %s WHERE id = ANY($1)`, qualify(r.ns, "claims")), pgArray(claimIDs))
	if err != nil {
		return nil, fmt.Errorf("list claims by ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectClaimRows(rows)
}

// RepointEvidence rewrites claim_evidence rows from one claim id
// to another inside a transaction. Duplicate (claim_id, event_id)
// pairs collapse via ON CONFLICT DO NOTHING.
func (r ClaimRepository) RepointEvidence(ctx context.Context, fromClaimID, toClaimID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin repoint evidence tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO %s (claim_id, event_id)
SELECT $1, event_id FROM %s WHERE claim_id = $2
ON CONFLICT (claim_id, event_id) DO NOTHING`,
		qualify(r.ns, "claim_evidence"), qualify(r.ns, "claim_evidence")),
		toClaimID, fromClaimID,
	); err != nil {
		return fmt.Errorf("copy evidence %s -> %s: %w", fromClaimID, toClaimID, err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE claim_id = $1`, qualify(r.ns, "claim_evidence")), fromClaimID); err != nil {
		return fmt.Errorf("delete original evidence %s: %w", fromClaimID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit repoint evidence tx: %w", err)
	}
	return nil
}

// DeleteCascade removes a claim plus every claim-keyed row it owns.
//
// The set is the canonical one from [ports.ClaimRepository], narrowed to
// the tables this backend declares: claim_evidence, claim_status_history,
// claim_expectations, then the claim row. Postgres has no claim_versions
// or claim_feedback table, so there is nothing to clear for those.
// claim_expectations was previously skipped here while SQLite cleared it,
// which is how the same delete left different residue per DSN — the
// orphaned forward prediction then survived to be re-read against a claim
// id that no longer resolves.
func (r ClaimRepository) DeleteCascade(ctx context.Context, claimID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin claim delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		fmt.Sprintf(`DELETE FROM %s WHERE claim_id = $1`, qualify(r.ns, "claim_evidence")),
		fmt.Sprintf(`DELETE FROM %s WHERE claim_id = $1`, qualify(r.ns, "claim_status_history")),
		fmt.Sprintf(`DELETE FROM %s WHERE claim_id = $1`, qualify(r.ns, "claim_expectations")),
		fmt.Sprintf(`DELETE FROM %s WHERE id = $1`, qualify(r.ns, "claims")),
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

// ListAll satisfies the corresponding ports method.
func (r ClaimRepository) ListAll(ctx context.Context) ([]domain.Claim, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
SELECT `+claimColumns("")+`
FROM %s ORDER BY created_at ASC`, qualify(r.ns, "claims")))
	if err != nil {
		return nil, fmt.Errorf("list all claims: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectClaimRows(rows)
}

// ListByTestRequirementRef returns every test_result claim sharing the given
// non-empty ref, freshest run first. Backed by idx_claims_test_requirement_ref
// (test_requirement_ref, type). Both the index and the test-provenance columns
// it covers were only declared alongside this fix: before that the query named
// columns the schema never had and failed with 42703, so test-vs-test
// contradiction resolution was not slow on Postgres, it was broken.
func (r ClaimRepository) ListByTestRequirementRef(ctx context.Context, ref string) ([]domain.Claim, error) {
	if ref == "" {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
SELECT `+claimColumns("")+`
FROM %s
WHERE type = 'test_result' AND test_requirement_ref = $1
ORDER BY test_last_run_at DESC, created_at DESC`, qualify(r.ns, "claims")), ref)
	if err != nil {
		return nil, fmt.Errorf("list claims by test_requirement_ref: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectClaimRows(rows)
}

// CountAll satisfies the corresponding ports method.
func (r ClaimRepository) CountAll(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT COUNT(*) FROM %s`, qualify(r.ns, "claims"),
	)).Scan(&n); err != nil {
		return 0, fmt.Errorf("count claims: %w", err)
	}
	return n, nil
}

// ListAllEvidence satisfies the corresponding ports method.
func (r ClaimRepository) ListAllEvidence(ctx context.Context) ([]domain.ClaimEvidence, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT claim_id, event_id FROM %s`, qualify(r.ns, "claim_evidence"),
	))
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

// DeleteAll satisfies the corresponding ports method. Wipes claims
// and the rows owned by claims (claim_evidence, claim_status_history)
// in a single transaction.
func (r ClaimRepository) DeleteAll(ctx context.Context) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin claims delete-all tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s`, qualify(r.ns, "claim_evidence"))); err != nil {
		return fmt.Errorf("delete claim_evidence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s`, qualify(r.ns, "claim_status_history"))); err != nil {
		return fmt.Errorf("delete claim_status_history: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s`, qualify(r.ns, "claims"))); err != nil {
		return fmt.Errorf("delete claims: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit claims delete-all tx: %w", err)
	}
	return nil
}

// ListIDsMissingEmbedding satisfies the corresponding ports method.
func (r ClaimRepository) ListIDsMissingEmbedding(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
SELECT c.id FROM %s c
LEFT JOIN %s e ON e.entity_id = c.id AND e.entity_type = 'claim'
WHERE e.entity_id IS NULL
ORDER BY c.created_at ASC`, qualify(r.ns, "claims"), qualify(r.ns, "embeddings")))
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

// ListAllStatusHistory satisfies the corresponding ports method.
func (r ClaimRepository) ListAllStatusHistory(ctx context.Context) ([]domain.ClaimStatusTransition, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT claim_id, from_status, to_status, changed_at, reason, changed_by
		 FROM %s ORDER BY id ASC`, qualify(r.ns, "claim_status_history"),
	))
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

// ListStatusHistoryByClaimID satisfies the corresponding ports method.
func (r ClaimRepository) ListStatusHistoryByClaimID(ctx context.Context, claimID string) ([]domain.ClaimStatusTransition, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
SELECT claim_id, from_status, to_status, changed_at, reason, changed_by
FROM %s WHERE claim_id = $1 ORDER BY id ASC`, qualify(r.ns, "claim_status_history")), claimID)
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

// MarkVerified bumps last_verified, increments verify_count, and
// optionally rewrites half_life_days when the caller supplies a
// non-zero override.
func (r ClaimRepository) MarkVerified(ctx context.Context, claimID string, verifiedAt time.Time, halfLifeDays float64) error {
	if verifiedAt.IsZero() {
		verifiedAt = time.Now().UTC()
	}
	stmt := fmt.Sprintf(`
UPDATE %s
SET last_verified = $1,
    verify_count = verify_count + 1,
    half_life_days = CASE WHEN $2 > 0 THEN $2 ELSE half_life_days END
WHERE id = $3`, qualify(r.ns, "claims"))
	res, err := r.db.ExecContext(ctx, stmt, verifiedAt.UTC(), halfLifeDays, claimID)
	if err != nil {
		return fmt.Errorf("mark verified %s: %w", claimID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("claim %s: %w", claimID, sql.ErrNoRows)
	}
	return nil
}

// SetValidity satisfies the corresponding ports method.
func (r ClaimRepository) SetValidity(ctx context.Context, claimID string, validTo time.Time) error {
	var args []any
	var stmt string
	if validTo.IsZero() {
		stmt = fmt.Sprintf(`UPDATE %s SET valid_to = NULL WHERE id = $1`, qualify(r.ns, "claims"))
		args = []any{claimID}
	} else {
		stmt = fmt.Sprintf(`UPDATE %s SET valid_to = $1 WHERE id = $2`, qualify(r.ns, "claims"))
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
	stmt := fmt.Sprintf(`UPDATE %s SET lifecycle = $1 WHERE id = $2`, qualify(r.ns, "claims"))
	res, err := r.db.ExecContext(ctx, stmt, string(lifecycle), claimID)
	if err != nil {
		return fmt.Errorf("set lifecycle for %s: %w", claimID, err)
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
// re-running is idempotent. This is what lets credit assignment + salience persist
// on the hosted backend (they store in confidence_components).
func (r ClaimRepository) ApplyBeliefCredit(ctx context.Context, claimID string, components map[string]float64, trustScore float64) error {
	stmt := fmt.Sprintf(`UPDATE %s SET confidence_components = $1, trust_score = $2 WHERE id = $3`, qualify(r.ns, "claims"))
	res, err := r.db.ExecContext(ctx, stmt, encodeConfidenceComponents(components), trustScore, claimID)
	if err != nil {
		return fmt.Errorf("apply belief credit for %s: %w", claimID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("apply belief credit: claim %s: %w", claimID, sql.ErrNoRows)
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

// trustInputsSQL builds the aggregate that feeds trust scoring. A non-empty
// where clause bounds it to a subset of claims. COUNT distinct evidence-event
// AUTHORS and total events separately, so corroboration can be graded by
// independence (echo-chamber guard). LEFT JOIN so claims with no evidence still
// appear.
//
// MAX(e.timestamp) is deliberately NOT coalesced to a date sentinel. A claim
// with no evidence must reach trust.Score with the ZERO time — the value
// freshnessFactor short-circuits to 1.0 — exactly as it does on sqlite and
// mysql. Coalescing to 'epoch' handed the scorer 1970-01-01, which is not
// "unknown" but "55 years stale": exp(-20000/90) clamps to the freshness floor
// and the same claim scored confidence×0.3 here and confidence×1.0 there,
// dropping it below --min-trust gates and `forget --below-trust` floors on
// Postgres only. NULL is scanned into a sql.NullTime and left zero.
func (r ClaimRepository) trustInputsSQL(where string) string {
	return fmt.Sprintf(`
SELECT c.id, c.confidence,
       COUNT(DISTINCT e.created_by), COUNT(DISTINCT ce.event_id),
       MAX(e.timestamp)
FROM %s c
LEFT JOIN %s ce ON ce.claim_id = c.id
LEFT JOIN %s e ON e.id = ce.event_id
%s
GROUP BY c.id, c.confidence`,
		qualify(r.ns, "claims"),
		qualify(r.ns, "claim_evidence"),
		qualify(r.ns, "events"),
		where,
	)
}

// listTrustInputs runs the aggregate and collects its rows. The query goes
// through the same connection as every other read, so the per-request
// mnemos.tenant GUC and its row-level security policy still apply.
func (r ClaimRepository) listTrustInputs(ctx context.Context, query string, args ...any) ([]trustInput, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var inputs []trustInput
	for rows.Next() {
		var in trustInput
		// NULL (no evidence rows) leaves in.latest at the zero time, the
		// cross-backend "unknown freshness" sentinel. See trustInputsSQL.
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
	stmt := fmt.Sprintf(`UPDATE %s SET trust_score = $1 WHERE id = $2`, qualify(r.ns, "claims"))
	for _, in := range inputs {
		evidenceCount := domain.EffectiveEvidenceCount(in.distinctSources, in.totalEvents)
		s := score(in.confidence, evidenceCount, in.latest)
		if _, err := tx.ExecContext(ctx, stmt, s, in.id); err != nil {
			return 0, fmt.Errorf("update trust for %s: %w", in.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit trust update: %w", err)
	}
	return len(inputs), nil
}

// RecomputeTrust applies the supplied scoring function to every
// claim. Returns the count touched.
func (r ClaimRepository) RecomputeTrust(ctx context.Context, score func(confidence float64, evidenceCount int, latestEvidence time.Time) float64) (int, error) {
	inputs, err := r.listTrustInputs(ctx, r.trustInputsSQL(""))
	if err != nil {
		return 0, fmt.Errorf("list trust inputs: %w", err)
	}
	return r.applyTrustInputs(ctx, inputs, score)
}

// RecomputeTrustForClaims implements [ports.ScopedTrustScorer]: the same
// recomputation bounded to claimIDs, so a write's cost tracks what it touched
// rather than the size of the store. Ids with no matching claim are skipped, so
// the returned count is the number of claims actually rescored.
//
// The id set arrives as a single text[] parameter (= ANY($1), the package's
// existing IN-list idiom), so no chunking is needed however many ids a write
// touched — there is one bind parameter regardless of slice length.
func (r ClaimRepository) RecomputeTrustForClaims(ctx context.Context, claimIDs []string, score func(confidence float64, evidenceCount int, latestEvidence time.Time) float64) (int, error) {
	if len(claimIDs) == 0 {
		return 0, nil
	}
	inputs, err := r.listTrustInputs(ctx, r.trustInputsSQL("WHERE c.id = ANY($1)"), pgArray(claimIDs))
	if err != nil {
		return 0, fmt.Errorf("list trust inputs for claims: %w", err)
	}
	return r.applyTrustInputs(ctx, inputs, score)
}

// AverageTrust satisfies the corresponding ports method.
func (r ClaimRepository) AverageTrust(ctx context.Context) (float64, error) {
	var avg sql.NullFloat64
	err := r.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT AVG(trust_score) FROM %s`, qualify(r.ns, "claims"))).Scan(&avg)
	if err != nil {
		return 0, fmt.Errorf("average trust: %w", err)
	}
	if !avg.Valid {
		return 0, nil
	}
	return avg.Float64, nil
}

// CountClaimsBelowTrust satisfies the corresponding ports method.
func (r ClaimRepository) CountClaimsBelowTrust(ctx context.Context, threshold float64) (int64, error) {
	var n int64
	err := r.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE trust_score < $1`, qualify(r.ns, "claims")), threshold).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count claims below trust: %w", err)
	}
	return n, nil
}

// nullTime maps the zero time to SQL NULL so an unset timestamp reads back as
// "unknown" rather than as year 1 / the epoch — the same sentinel discipline
// the trust scorer depends on.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// claimColumnNames is the ONE projection every claim read uses, in the exact
// order scanClaimRow scans. Hand-written per-query projections drifted out of
// step with the scanner (ListClaimsForEntity omitted `durability` and failed at
// Scan with an arity mismatch), so the list lives in one place and every reader
// goes through claimColumns.
var claimColumnNames = []string{
	"id", "text", "type", "confidence", "status", "created_at", "created_by",
	"trust_score", "valid_from", "valid_to", "lifecycle", "subject_class",
	"durability", "confidence_components",
	"test_id", "test_requirement_ref", "test_author", "test_last_modified",
	"test_last_run_at", "test_pass_count", "test_fail_count",
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
	var typ, status, lifecycle, subjectClass, durability, confidenceComponents string
	var validFrom sql.NullTime
	var validTo sql.NullTime
	var testLastModified, testLastRunAt sql.NullTime
	if err := rows.Scan(
		&c.ID, &c.Text, &typ, &c.Confidence, &status,
		&c.CreatedAt, &c.CreatedBy, &c.TrustScore, &validFrom, &validTo, &lifecycle, &subjectClass, &durability, &confidenceComponents,
		&c.TestID, &c.TestRequirementRef, &c.TestAuthor, &testLastModified, &testLastRunAt, &c.TestPassCount, &c.TestFailCount,
	); err != nil {
		return domain.Claim{}, fmt.Errorf("scan claim row: %w", err)
	}
	c.Type = domain.ClaimType(typ)
	c.Status = domain.ClaimStatus(status)
	c.Lifecycle = domain.ClaimLifecycle(lifecycle)
	c.SubjectClass = domain.SubjectClass(subjectClass)
	c.Durability = domain.Durability(durability)
	c.ConfidenceComponents = decodeConfidenceComponents(confidenceComponents)
	if validFrom.Valid {
		c.ValidFrom = validFrom.Time
	}
	if validTo.Valid {
		c.ValidTo = validTo.Time
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
