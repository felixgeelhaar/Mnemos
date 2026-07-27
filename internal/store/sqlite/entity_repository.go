package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/store/sqlite/sqlcgen"
)

// EntityRepository is the v0.9 storage port for canonicalised
// entities and the claim_entities link table. The find-or-create
// flow goes through FindOrCreate so callers don't have to think
// about the UNIQUE(normalized_name, type) constraint.
type EntityRepository struct {
	db *sql.DB
	q  *sqlcgen.Queries
}

// NewEntityRepository binds a repo to the given DB handle.
func NewEntityRepository(db *sql.DB) EntityRepository {
	return EntityRepository{db: db, q: sqlcgen.New(db)}
}

// FindOrCreate returns the existing entity matching (normalized_name,
// type), or creates a new one with a fresh id. Calls are idempotent
// and safe under concurrency: the INSERT is ON CONFLICT DO NOTHING,
// and the follow-up SELECT always wins. The returned Entity.ID is
// what callers should write to claim_entities — never trust the id
// they passed in (it may not have been the winner under contention).
func (r EntityRepository) FindOrCreate(ctx context.Context, name string, etype domain.EntityType, createdBy string) (domain.Entity, error) {
	norm := domain.NormalizeEntityName(name)
	if norm == "" {
		return domain.Entity{}, fmt.Errorf("entity name cannot be empty")
	}
	if etype == "" {
		etype = domain.EntityTypeConcept
	}

	// Best-effort insert; conflicts collapse silently.
	id, err := newEntityID()
	if err != nil {
		return domain.Entity{}, fmt.Errorf("generate entity id: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := r.q.UpsertEntity(ctx, sqlcgen.UpsertEntityParams{
		ID:             id,
		Name:           name,
		NormalizedName: norm,
		Type:           string(etype),
		CreatedAt:      now,
		CreatedBy:      actorOr(createdBy),
	}); err != nil {
		return domain.Entity{}, fmt.Errorf("upsert entity %q: %w", name, err)
	}

	row, err := r.q.FindEntityByNormalizedName(ctx, sqlcgen.FindEntityByNormalizedNameParams{
		NormalizedName: norm,
		Type:           string(etype),
	})
	if err != nil {
		return domain.Entity{}, fmt.Errorf("look up entity %q: %w", name, err)
	}
	return entityFromRow(row.ID, row.Name, row.NormalizedName, row.Type, row.CreatedAt, row.CreatedBy)
}

// LinkClaim writes a claim_entities row. Idempotent under the
// UNIQUE(claim_id, entity_id, role) constraint.
func (r EntityRepository) LinkClaim(ctx context.Context, claimID, entityID, role string) error {
	if role == "" {
		role = "mention"
	}
	return r.q.UpsertClaimEntity(ctx, sqlcgen.UpsertClaimEntityParams{
		ClaimID:  claimID,
		EntityID: entityID,
		Role:     role,
	})
}

// UnlinkClaim drops every claim_entities row for the claim. Deleting
// nothing is success, so a merge or delete that is retried converges.
func (r EntityRepository) UnlinkClaim(ctx context.Context, claimID string) error {
	if err := r.q.DeleteClaimEntitiesByClaimID(ctx, claimID); err != nil {
		return fmt.Errorf("unlink entities for claim %s: %w", claimID, err)
	}
	return nil
}

// List returns every entity ordered by name. Cheap; the entities
// table is expected to stay small (~hundreds-to-low-thousands per
// project) since canonicalisation collapses synonyms.
func (r EntityRepository) List(ctx context.Context) ([]domain.Entity, error) {
	rows, err := r.q.ListEntities(ctx)
	if err != nil {
		return nil, fmt.Errorf("list entities: %w", err)
	}
	out := make([]domain.Entity, 0, len(rows))
	for _, row := range rows {
		e, err := entityFromRow(row.ID, row.Name, row.NormalizedName, row.Type, row.CreatedAt, row.CreatedBy)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// ListByType returns entities of the given type, ordered by name.
func (r EntityRepository) ListByType(ctx context.Context, etype domain.EntityType) ([]domain.Entity, error) {
	rows, err := r.q.ListEntitiesByType(ctx, string(etype))
	if err != nil {
		return nil, fmt.Errorf("list entities by type: %w", err)
	}
	out := make([]domain.Entity, 0, len(rows))
	for _, row := range rows {
		e, err := entityFromRow(row.ID, row.Name, row.NormalizedName, row.Type, row.CreatedAt, row.CreatedBy)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// FindByName tries an exact normalized-name match across all types.
// Returns ("", false, nil) on a miss so callers can branch without
// handling sql.ErrNoRows. The first match wins when the same name
// exists across multiple types — rare in practice; the CLI
// disambiguates with --type when it matters.
func (r EntityRepository) FindByName(ctx context.Context, name string) (domain.Entity, bool, error) {
	norm := domain.NormalizeEntityName(name)
	rows, err := r.q.SearchEntitiesByNamePrefix(ctx, norm)
	if err != nil {
		return domain.Entity{}, false, fmt.Errorf("search entities: %w", err)
	}
	for _, row := range rows {
		if row.NormalizedName == norm {
			e, err := entityFromRow(row.ID, row.Name, row.NormalizedName, row.Type, row.CreatedAt, row.CreatedBy)
			return e, err == nil, err
		}
	}
	return domain.Entity{}, false, nil
}

// ListClaimsForEntity returns the claims linked to the entity in
// chronological order. Mirrors ClaimRepository.ListAll's mapping so
// downstream code can treat the slice the same way.
func (r EntityRepository) ListClaimsForEntity(ctx context.Context, entityID string) ([]domain.Claim, error) {
	rows, err := r.q.ListClaimsByEntityID(ctx, entityID)
	if err != nil {
		return nil, fmt.Errorf("list claims for entity %s: %w", entityID, err)
	}
	out := make([]domain.Claim, 0, len(rows))
	for _, row := range rows {
		c, err := mapSQLClaim(sqlcgen.Claim{
			ID:         row.ID,
			Text:       row.Text,
			Type:       row.Type,
			Confidence: row.Confidence,
			Status:     row.Status,
			CreatedAt:  row.CreatedAt,
			CreatedBy:  row.CreatedBy,
			TrustScore: row.TrustScore,
			ValidFrom:  row.ValidFrom,
			ValidTo:    row.ValidTo,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// ListEntitiesForClaim returns entities mentioned by the given
// claim, with their roles preserved. Used by the CLI's
// `entities show` and by the answer renderer when it wants to
// surface "this claim is about Felix, Acme".
func (r EntityRepository) ListEntitiesForClaim(ctx context.Context, claimID string) ([]domain.Entity, []string, error) {
	rows, err := r.q.ListEntitiesByClaimID(ctx, claimID)
	if err != nil {
		return nil, nil, fmt.Errorf("list entities for claim %s: %w", claimID, err)
	}
	ents := make([]domain.Entity, 0, len(rows))
	roles := make([]string, 0, len(rows))
	for _, row := range rows {
		e, err := entityFromRow(row.ID, row.Name, row.NormalizedName, row.Type, row.CreatedAt, row.CreatedBy)
		if err != nil {
			return nil, nil, err
		}
		ents = append(ents, e)
		roles = append(roles, row.Role)
	}
	return ents, roles, nil
}

// Merge collapses one entity into another inside a single
// transaction. All claim_entities rows pointing at loserID become
// rows pointing at winnerID (deduped via INSERT OR IGNORE), then
// the loser row is removed. Useful for canonicalising "Felix" /
// "felixgeelhaar" / "Felix Geelhaar" into one node when the
// auto-dedup didn't catch them.
//
// Returns ErrEntityMergeSelf when winner == loser to surface the
// (almost certainly programmer-error) case explicitly.
func (r EntityRepository) Merge(ctx context.Context, winnerID, loserID string) error {
	if winnerID == loserID {
		return ErrEntityMergeSelf
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin merge tx: %w", err)
	}
	defer rollbackTx(tx)
	q := r.q.WithTx(tx)
	if err := q.ReassignClaimEntitiesEntity(ctx, sqlcgen.ReassignClaimEntitiesEntityParams{
		EntityID:   winnerID,
		EntityID_2: loserID,
	}); err != nil {
		return fmt.Errorf("reassign claim_entities: %w", err)
	}
	if err := q.DeleteClaimEntitiesByEntityID(ctx, loserID); err != nil {
		return fmt.Errorf("delete losing claim_entities: %w", err)
	}
	if err := q.DeleteEntityByID(ctx, loserID); err != nil {
		return fmt.Errorf("delete losing entity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit entity merge: %w", err)
	}
	return nil
}

// Count returns the number of entity rows for the metrics command.
func (r EntityRepository) Count(ctx context.Context) (int64, error) {
	return r.q.CountEntities(ctx)
}

// ClaimIDsMissingEntityLinks returns claims that have no entries in
// claim_entities — the working set for `mnemos extract-entities`.
func (r EntityRepository) ClaimIDsMissingEntityLinks(ctx context.Context) ([]string, error) {
	return r.q.ClaimIDsMissingEntityLinks(ctx)
}

// ErrEntityMergeSelf is returned by Merge when the winner and loser
// ids are identical.
var ErrEntityMergeSelf = errors.New("entity merge: winner and loser must differ")

func entityFromRow(id, name, norm, etype, createdAt, createdBy string) (domain.Entity, error) {
	t, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return domain.Entity{}, fmt.Errorf("parse entity created_at: %w", err)
	}
	return domain.Entity{
		ID:             id,
		Name:           name,
		NormalizedName: norm,
		Type:           domain.EntityType(etype),
		CreatedAt:      t,
		CreatedBy:      createdBy,
	}, nil
}

func newEntityID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "en_" + hex.EncodeToString(buf), nil
}
