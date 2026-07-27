package mysql

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"go.klarlabs.de/mnemos/internal/domain"
)

// EntityRepository implements ports.EntityRepository.
type EntityRepository struct {
	db *sql.DB
}

// FindOrCreate returns the entity matching (normalized_name, type),
// inserting one when missing. Idempotent via INSERT IGNORE.
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
	if _, err := r.db.ExecContext(ctx, `
INSERT IGNORE INTO entities (id, name, normalized_name, type, created_at, created_by)
VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP(6), ?)`,
		id, name, norm, string(etype), actorOr(createdBy),
	); err != nil {
		return domain.Entity{}, fmt.Errorf("upsert entity %q: %w", name, err)
	}
	row := r.db.QueryRowContext(ctx, `
SELECT id, name, normalized_name, type, created_at, created_by
FROM entities WHERE normalized_name = ? AND type = ?`, norm, string(etype))
	return scanEntityRow(row)
}

// LinkClaim writes a claim_entities row idempotently.
func (r EntityRepository) LinkClaim(ctx context.Context, claimID, entityID, role string) error {
	if role == "" {
		role = "mention"
	}
	_, err := r.db.ExecContext(ctx, `
INSERT IGNORE INTO claim_entities (claim_id, entity_id, role) VALUES (?, ?, ?)`,
		claimID, entityID, role,
	)
	if err != nil {
		return fmt.Errorf("link claim %s -> entity %s: %w", claimID, entityID, err)
	}
	return nil
}

// List returns all entities ordered by name.
func (r EntityRepository) List(ctx context.Context) ([]domain.Entity, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT id, name, normalized_name, type, created_at, created_by FROM entities ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list entities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectEntityRows(rows)
}

// ListByType returns entities of a given type.
func (r EntityRepository) ListByType(ctx context.Context, etype domain.EntityType) ([]domain.Entity, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT id, name, normalized_name, type, created_at, created_by
FROM entities WHERE type = ? ORDER BY name`, string(etype))
	if err != nil {
		return nil, fmt.Errorf("list entities by type: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectEntityRows(rows)
}

// FindByName returns the first entity whose normalized name matches.
func (r EntityRepository) FindByName(ctx context.Context, name string) (domain.Entity, bool, error) {
	norm := domain.NormalizeEntityName(name)
	if norm == "" {
		return domain.Entity{}, false, nil
	}
	row := r.db.QueryRowContext(ctx, `
SELECT id, name, normalized_name, type, created_at, created_by
FROM entities WHERE normalized_name = ? LIMIT 1`, norm)
	e, err := scanEntityRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Entity{}, false, nil
	}
	if err != nil {
		return domain.Entity{}, false, fmt.Errorf("find entity by name: %w", err)
	}
	return e, true, nil
}

// ListClaimsForEntity returns the claims linked to the entity.
func (r EntityRepository) ListClaimsForEntity(ctx context.Context, entityID string) ([]domain.Claim, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT `+claimColumns("c")+`
FROM claims c
JOIN claim_entities ce ON ce.claim_id = c.id
WHERE ce.entity_id = ?
ORDER BY c.created_at ASC`, entityID)
	if err != nil {
		return nil, fmt.Errorf("list claims for entity %s: %w", entityID, err)
	}
	defer func() { _ = rows.Close() }()
	return collectClaimRows(rows)
}

// ListEntitiesForClaim returns entities mentioned by the claim.
func (r EntityRepository) ListEntitiesForClaim(ctx context.Context, claimID string) ([]domain.Entity, []string, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT e.id, e.name, e.normalized_name, e.type, e.created_at, e.created_by, ce.role
FROM entities e
JOIN claim_entities ce ON ce.entity_id = e.id
WHERE ce.claim_id = ?
ORDER BY e.name`, claimID)
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

// Merge collapses one entity into another.
func (r EntityRepository) Merge(ctx context.Context, winnerID, loserID string) error {
	if winnerID == loserID {
		return ErrEntityMergeSelf
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin merge tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Re-point claim_entities rows from loser to winner; INSERT IGNORE
	// drops conflicts where (claim, winner, role) already exists.
	if _, err := tx.ExecContext(ctx, `
INSERT IGNORE INTO claim_entities (claim_id, entity_id, role)
SELECT claim_id, ?, role FROM claim_entities WHERE entity_id = ?`,
		winnerID, loserID,
	); err != nil {
		return fmt.Errorf("rewrite claim_entities: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM claim_entities WHERE entity_id = ?`, loserID); err != nil {
		return fmt.Errorf("delete losing claim_entities: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM entities WHERE id = ?`, loserID); err != nil {
		return fmt.Errorf("delete losing entity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit entity merge: %w", err)
	}
	return nil
}

// Count returns the number of entities currently stored.
func (r EntityRepository) Count(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entities`).Scan(&n)
	return n, err
}

// ClaimIDsMissingEntityLinks returns claim ids that have no
// claim_entities link rows.
func (r EntityRepository) ClaimIDsMissingEntityLinks(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT c.id FROM claims c
LEFT JOIN claim_entities ce ON ce.claim_id = c.id
WHERE ce.claim_id IS NULL
ORDER BY c.created_at ASC`)
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

// ErrEntityMergeSelf mirrors the SQLite/Postgres sentinel.
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
