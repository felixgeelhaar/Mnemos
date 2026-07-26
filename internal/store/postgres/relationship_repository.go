package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"go.klarlabs.de/mnemos/internal/domain"
)

// RelationshipRepository persists claim → claim edges. The (id) is
// the dedup key; ON CONFLICT (id) DO UPDATE matches the SQLite
// upsert semantics.
type RelationshipRepository struct {
	db pgQuerier
	ns string
}

// Upsert satisfies the corresponding ports method.
func (r RelationshipRepository) Upsert(ctx context.Context, relationships []domain.Relationship) error {
	if len(relationships) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin relationship upsert tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt := fmt.Sprintf(`
INSERT INTO %s (id, type, from_claim_id, to_claim_id, created_at, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (id) DO UPDATE SET
  type = EXCLUDED.type,
  from_claim_id = EXCLUDED.from_claim_id,
  to_claim_id = EXCLUDED.to_claim_id`, qualify(r.ns, "relationships"))
	for _, rel := range relationships {
		if err := rel.Validate(); err != nil {
			return fmt.Errorf("invalid relationship %s: %w", rel.ID, err)
		}
		if _, err := tx.ExecContext(ctx, stmt,
			rel.ID, string(rel.Type), rel.FromClaimID, rel.ToClaimID,
			rel.CreatedAt.UTC(), actorOr(rel.CreatedBy),
		); err != nil {
			return fmt.Errorf("upsert relationship %s: %w", rel.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit relationship upsert tx: %w", err)
	}
	return nil
}

// ListByClaim satisfies the corresponding ports method.
func (r RelationshipRepository) ListByClaim(ctx context.Context, claimID string) ([]domain.Relationship, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
SELECT id, type, from_claim_id, to_claim_id, created_at, created_by, strength
FROM %s WHERE from_claim_id = $1 OR to_claim_id = $1`, qualify(r.ns, "relationships")), claimID)
	if err != nil {
		return nil, fmt.Errorf("list relationships by claim: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectRelationshipRows(rows)
}

// RepointEndpoint rewrites every relationship whose endpoints equal oldID to
// point at newID. Edges that would become duplicates of an existing
// (type, from, to) collapse into one, and edges that would become self-loops
// (newID-newID) are dropped. Mnemos does not distinguish duplicate edges, so
// collapsing is lossless.
func (r RelationshipRepository) RepointEndpoint(ctx context.Context, oldID, newID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin repoint endpoint tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Postgres has no UPDATE OR IGNORE, so conflicting edges must be removed
	// before the rewrite. This used to be two pre-deletes, one per endpoint,
	// BOTH evaluated before EITHER update ran — and that is unsound: rewriting
	// from_claim_id changes which rows the to_claim_id rewrite collides with.
	//
	// Concretely, with an existing (T, new, new) edge, a row (T, old, old)
	// survives both pre-deletes (at the time they run, its from_claim_id is
	// still `old`, so it matches no conflict), becomes (T, new, old) after the
	// first UPDATE, and then collides on the second. The UPDATE raised
	// 23505 on idx_relationships_unique_edge and took the whole transaction —
	// and with it the entire consolidation pass — down with it. Observed in
	// production: memory-consolidate failed on every run, merging nothing, for
	// as long as a single such pair existed.
	//
	// Compute the collision set on the POST-rewrite identity instead, in one
	// statement, which makes it independent of the order the updates run in:
	// project each candidate row onto the (type, from', to') it WILL have, keep
	// the first row per identity, and drop the rest — plus anything that
	// becomes a self-loop. After this the updates cannot violate the index.
	//
	// Candidates must include rows touching newID, not just oldID: the edge a
	// rewritten row collides WITH is by definition already on newID.
	prune := fmt.Sprintf(`
DELETE FROM %s WHERE id IN (
  SELECT id FROM (
    SELECT id,
           CASE WHEN from_claim_id = $1 THEN $2 ELSE from_claim_id END AS nf,
           CASE WHEN to_claim_id   = $1 THEN $2 ELSE to_claim_id   END AS nt,
           ROW_NUMBER() OVER (
             PARTITION BY type,
                          CASE WHEN from_claim_id = $1 THEN $2 ELSE from_claim_id END,
                          CASE WHEN to_claim_id   = $1 THEN $2 ELSE to_claim_id   END
             ORDER BY id
           ) AS rn
      FROM %s
     WHERE from_claim_id IN ($1, $2) OR to_claim_id IN ($1, $2)
  ) s
  WHERE s.rn > 1 OR s.nf = s.nt
)`,
		qualify(r.ns, "relationships"),
		qualify(r.ns, "relationships"))
	if _, err := tx.ExecContext(ctx, prune, oldID, newID); err != nil {
		return fmt.Errorf("prune colliding edges %s -> %s: %w", oldID, newID, err)
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET from_claim_id = $1 WHERE from_claim_id = $2`, qualify(r.ns, "relationships")),
		newID, oldID,
	); err != nil {
		return fmt.Errorf("repoint from %s -> %s: %w", oldID, newID, err)
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET to_claim_id = $1 WHERE to_claim_id = $2`, qualify(r.ns, "relationships")),
		newID, oldID,
	); err != nil {
		return fmt.Errorf("repoint to %s -> %s: %w", oldID, newID, err)
	}
	// No separate self-loop sweep: the prune above already drops any row whose
	// post-rewrite endpoints are equal (nf = nt), which covers both loops the
	// rewrite would create and any that already existed on newID.
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit repoint endpoint tx: %w", err)
	}
	return nil
}

// DeleteByClaim removes every relationship touching the claim.
func (r RelationshipRepository) DeleteByClaim(ctx context.Context, claimID string) error {
	if _, err := r.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE from_claim_id = $1 OR to_claim_id = $1`, qualify(r.ns, "relationships")),
		claimID,
	); err != nil {
		return fmt.Errorf("delete relationships for %s: %w", claimID, err)
	}
	return nil
}

// CountAll satisfies the corresponding ports method.
func (r RelationshipRepository) CountAll(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT COUNT(*) FROM %s`, qualify(r.ns, "relationships"),
	)).Scan(&n); err != nil {
		return 0, fmt.Errorf("count relationships: %w", err)
	}
	return n, nil
}

// CountByType satisfies the corresponding ports method.
func (r RelationshipRepository) CountByType(ctx context.Context, relType string) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT COUNT(*) FROM %s WHERE type = $1`, qualify(r.ns, "relationships"),
	), relType).Scan(&n); err != nil {
		return 0, fmt.Errorf("count relationships by type: %w", err)
	}
	return n, nil
}

// DeleteAll satisfies the corresponding ports method.
func (r RelationshipRepository) DeleteAll(ctx context.Context) error {
	if _, err := r.db.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM %s`, qualify(r.ns, "relationships"),
	)); err != nil {
		return fmt.Errorf("delete all relationships: %w", err)
	}
	return nil
}

// ListAll satisfies the corresponding ports method.
func (r RelationshipRepository) ListAll(ctx context.Context) ([]domain.Relationship, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
SELECT id, type, from_claim_id, to_claim_id, created_at, created_by, strength
FROM %s ORDER BY created_at ASC`, qualify(r.ns, "relationships")))
	if err != nil {
		return nil, fmt.Errorf("list all relationships: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectRelationshipRows(rows)
}

// ListByClaimIDs satisfies the corresponding ports method.
func (r RelationshipRepository) ListByClaimIDs(ctx context.Context, claimIDs []string) ([]domain.Relationship, error) {
	if len(claimIDs) == 0 {
		return []domain.Relationship{}, nil
	}
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
SELECT id, type, from_claim_id, to_claim_id, created_at, created_by, strength
FROM %s WHERE from_claim_id = ANY($1) OR to_claim_id = ANY($1)`, qualify(r.ns, "relationships")), pgArray(claimIDs))
	if err != nil {
		return nil, fmt.Errorf("list relationships by claim ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectRelationshipRows(rows)
}

func collectRelationshipRows(rows *sql.Rows) ([]domain.Relationship, error) {
	out := make([]domain.Relationship, 0)
	for rows.Next() {
		var rel domain.Relationship
		var typ string
		if err := rows.Scan(&rel.ID, &typ, &rel.FromClaimID, &rel.ToClaimID, &rel.CreatedAt, &rel.CreatedBy, &rel.Strength); err != nil {
			return nil, fmt.Errorf("scan relationship row: %w", err)
		}
		rel.Type = domain.RelationshipType(typ)
		out = append(out, rel)
	}
	return out, rows.Err()
}

// StrengthenAssociations implements [ports.RelationshipStrengthener] (ADR 0015 §4):
// it raises the strength of every edge whose BOTH endpoints are in claimIDs — a
// single `from = ANY AND to = ANY` predicate catches intra-set edges in either
// direction — by delta, capped at maxStrength. Only existing edges are touched; no
// edge is created. Returns the number of edges matched.
func (r RelationshipRepository) StrengthenAssociations(ctx context.Context, claimIDs []string, delta, maxStrength float64) (int, error) {
	if delta <= 0 || len(claimIDs) < 2 {
		return 0, nil
	}
	res, err := r.db.ExecContext(ctx, fmt.Sprintf(`
UPDATE %s SET strength = LEAST(strength + $1, $2)
WHERE from_claim_id = ANY($3) AND to_claim_id = ANY($3)`, qualify(r.ns, "relationships")),
		delta, maxStrength, pgArray(claimIDs))
	if err != nil {
		return 0, fmt.Errorf("strengthen associations: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DecayAssociations implements [ports.RelationshipStrengthener] (ADR 0015 §5): it pulls
// every over-base edge's strength toward the base 1.0, keeping the given fraction of
// its excess. Only edges above the base are touched, so base edges stay neutral and
// none is deleted.
func (r RelationshipRepository) DecayAssociations(ctx context.Context, retain float64) (int, error) {
	if retain < 0 || retain >= 1 {
		return 0, nil
	}
	res, err := r.db.ExecContext(ctx, fmt.Sprintf(
		`UPDATE %s SET strength = 1 + (strength - 1) * $1 WHERE strength > 1`, qualify(r.ns, "relationships")), retain)
	if err != nil {
		return 0, fmt.Errorf("decay associations: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
