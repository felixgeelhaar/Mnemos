package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"go.klarlabs.de/mnemos/internal/domain"
)

// EntityRepository persists canonicalised entities and the
// claim_entities link table. UNIQUE(normalized_name, type) on
// entities and UNIQUE(claim_id, entity_id, role) on claim_entities
// give us idempotent upserts via ON CONFLICT.
type EntityRepository struct {
	db pgQuerier
	ns string
}

// FindOrCreate returns the entity matching (normalized_name, type),
// inserting a new one when missing. Idempotent.
func (r EntityRepository) FindOrCreate(ctx context.Context, name string, etype domain.EntityType, createdBy string) (domain.Entity, error) {
	norm := domain.NormalizeEntityName(name)
	if norm == "" {
		return domain.Entity{}, fmt.Errorf("entity name cannot be empty")
	}
	if etype == "" {
		etype = domain.EntityTypeConcept
	}

	id, err := newEntityID()
	if err != nil {
		return domain.Entity{}, fmt.Errorf("generate entity id: %w", err)
	}
	// Best-effort insert; UNIQUE conflict collapses silently.
	if _, err := r.db.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO %s (id, name, normalized_name, type, created_at, created_by)
VALUES ($1, $2, $3, $4, now(), $5)
ON CONFLICT (normalized_name, type) DO NOTHING`, qualify(r.ns, "entities")),
		id, name, norm, string(etype), actorOr(createdBy),
	); err != nil {
		return domain.Entity{}, fmt.Errorf("upsert entity %q: %w", name, err)
	}

	row := r.db.QueryRowContext(ctx, fmt.Sprintf(`
SELECT id, name, normalized_name, type, created_at, created_by
FROM %s WHERE normalized_name = $1 AND type = $2`, qualify(r.ns, "entities")), norm, string(etype))
	return scanEntityRow(row)
}

// LinkClaim satisfies the corresponding ports method.
func (r EntityRepository) LinkClaim(ctx context.Context, claimID, entityID, role string) error {
	if role == "" {
		role = "mention"
	}
	_, err := r.db.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO %s (claim_id, entity_id, role) VALUES ($1, $2, $3)
ON CONFLICT (claim_id, entity_id, role) DO NOTHING`, qualify(r.ns, "claim_entities")),
		claimID, entityID, role,
	)
	if err != nil {
		return fmt.Errorf("link claim %s -> entity %s: %w", claimID, entityID, err)
	}
	return nil
}

// List satisfies the corresponding ports method.
func (r EntityRepository) List(ctx context.Context) ([]domain.Entity, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
SELECT id, name, normalized_name, type, created_at, created_by
FROM %s ORDER BY name`, qualify(r.ns, "entities")))
	if err != nil {
		return nil, fmt.Errorf("list entities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectEntityRows(rows)
}

// ListByType satisfies the corresponding ports method.
func (r EntityRepository) ListByType(ctx context.Context, etype domain.EntityType) ([]domain.Entity, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
SELECT id, name, normalized_name, type, created_at, created_by
FROM %s WHERE type = $1 ORDER BY name`, qualify(r.ns, "entities")), string(etype))
	if err != nil {
		return nil, fmt.Errorf("list entities by type: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectEntityRows(rows)
}

// FindByName satisfies the corresponding ports method.
func (r EntityRepository) FindByName(ctx context.Context, name string) (domain.Entity, bool, error) {
	norm := domain.NormalizeEntityName(name)
	if norm == "" {
		return domain.Entity{}, false, nil
	}
	row := r.db.QueryRowContext(ctx, fmt.Sprintf(`
SELECT id, name, normalized_name, type, created_at, created_by
FROM %s WHERE normalized_name = $1 LIMIT 1`, qualify(r.ns, "entities")), norm)
	e, err := scanEntityRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Entity{}, false, nil
	}
	if err != nil {
		return domain.Entity{}, false, fmt.Errorf("find entity by name: %w", err)
	}
	return e, true, nil
}

// ListClaimsForEntity satisfies the corresponding ports method.
func (r EntityRepository) ListClaimsForEntity(ctx context.Context, entityID string) ([]domain.Claim, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
SELECT `+claimColumns("c")+`
FROM %s c
JOIN %s ce ON ce.claim_id = c.id
WHERE ce.entity_id = $1
ORDER BY c.created_at ASC`, qualify(r.ns, "claims"), qualify(r.ns, "claim_entities")), entityID)
	if err != nil {
		return nil, fmt.Errorf("list claims for entity %s: %w", entityID, err)
	}
	defer func() { _ = rows.Close() }()
	return collectClaimRows(rows)
}

// ListEntitiesForClaim satisfies the corresponding ports method.
func (r EntityRepository) ListEntitiesForClaim(ctx context.Context, claimID string) ([]domain.Entity, []string, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
SELECT e.id, e.name, e.normalized_name, e.type, e.created_at, e.created_by, ce.role
FROM %s e
JOIN %s ce ON ce.entity_id = e.id
WHERE ce.claim_id = $1
ORDER BY e.name`, qualify(r.ns, "entities"), qualify(r.ns, "claim_entities")), claimID)
	if err != nil {
		return nil, nil, fmt.Errorf("list entities for claim %s: %w", claimID, err)
	}
	defer func() { _ = rows.Close() }()
	ents := make([]domain.Entity, 0)
	roles := make([]string, 0)
	for rows.Next() {
		var e domain.Entity
		var typ, role string
		if err := rows.Scan(&e.ID, &e.Name, &e.NormalizedName, &typ, &e.CreatedAt, &e.CreatedBy, &role); err != nil {
			return nil, nil, fmt.Errorf("scan entity row: %w", err)
		}
		e.Type = domain.EntityType(typ)
		ents = append(ents, e)
		roles = append(roles, role)
	}
	return ents, roles, rows.Err()
}

// Merge satisfies the corresponding ports method.
func (r EntityRepository) Merge(ctx context.Context, winnerID, loserID string) error {
	if winnerID == loserID {
		return ErrEntityMergeSelf
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin merge tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Re-point any claim_entities rows from loser to winner; the
	// (claim, winner, role) combo may already exist, so use
	// ON CONFLICT to drop the redundant row instead.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO %s (claim_id, entity_id, role)
SELECT claim_id, $1, role FROM %s WHERE entity_id = $2
ON CONFLICT (claim_id, entity_id, role) DO NOTHING`,
		qualify(r.ns, "claim_entities"), qualify(r.ns, "claim_entities")),
		winnerID, loserID,
	); err != nil {
		return fmt.Errorf("rewrite claim_entities: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE entity_id = $1`, qualify(r.ns, "claim_entities")), loserID); err != nil {
		return fmt.Errorf("delete losing claim_entities: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = $1`, qualify(r.ns, "entities")), loserID); err != nil {
		return fmt.Errorf("delete losing entity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit entity merge: %w", err)
	}
	return nil
}

// Count satisfies the corresponding ports method.
func (r EntityRepository) Count(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, qualify(r.ns, "entities"))).Scan(&n)
	return n, err
}

// ClaimIDsMissingEntityLinks satisfies the corresponding ports method.
func (r EntityRepository) ClaimIDsMissingEntityLinks(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
SELECT c.id FROM %s c
LEFT JOIN %s ce ON ce.claim_id = c.id
WHERE ce.claim_id IS NULL
ORDER BY c.created_at ASC`, qualify(r.ns, "claims"), qualify(r.ns, "claim_entities")))
	if err != nil {
		return nil, fmt.Errorf("claim ids missing entity links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ErrEntityMergeSelf mirrors the SQLite implementation's sentinel.
var ErrEntityMergeSelf = errors.New("entity merge: winner and loser must differ")

func scanEntityRow(row *sql.Row) (domain.Entity, error) {
	var e domain.Entity
	var typ string
	if err := row.Scan(&e.ID, &e.Name, &e.NormalizedName, &typ, &e.CreatedAt, &e.CreatedBy); err != nil {
		return domain.Entity{}, err
	}
	e.Type = domain.EntityType(typ)
	return e, nil
}

func collectEntityRows(rows *sql.Rows) ([]domain.Entity, error) {
	out := make([]domain.Entity, 0)
	for rows.Next() {
		var e domain.Entity
		var typ string
		if err := rows.Scan(&e.ID, &e.Name, &e.NormalizedName, &typ, &e.CreatedAt, &e.CreatedBy); err != nil {
			return nil, fmt.Errorf("scan entity row: %w", err)
		}
		e.Type = domain.EntityType(typ)
		out = append(out, e)
	}
	return out, rows.Err()
}

func newEntityID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "en_" + hex.EncodeToString(buf), nil
}
