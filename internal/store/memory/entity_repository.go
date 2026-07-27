package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// EntityRepository is the in-memory implementation of
// [ports.EntityRepository]. The (NormalizedName, Type) pair is the
// dedup key, mirroring the SQLite UNIQUE(normalized_name, type)
// index. claim_entities links are deduped by (ClaimID, EntityID, Role).
type EntityRepository struct {
	state *state
}

// FindOrCreate returns the entity matching (normalized_name, type),
// creating a new one when none exists. Idempotent.
func (r EntityRepository) FindOrCreate(_ context.Context, name string, etype domain.EntityType, createdBy string) (domain.Entity, error) {
	norm := domain.NormalizeEntityName(name)
	if norm == "" {
		return domain.Entity{}, fmt.Errorf("entity name cannot be empty")
	}
	if etype == "" {
		etype = domain.EntityTypeConcept
	}

	r.state.mu.Lock()
	defer r.state.mu.Unlock()

	key := entityKey{NormalizedName: norm, Type: etype}
	if existingID, ok := r.state.entityByKey[key]; ok {
		return r.state.entities[existingID].toDomain(), nil
	}

	id, err := newEntityID()
	if err != nil {
		return domain.Entity{}, fmt.Errorf("generate entity id: %w", err)
	}
	stored := storedEntity{
		ID:             id,
		Name:           name,
		NormalizedName: norm,
		Type:           etype,
		CreatedAt:      time.Now().UTC(),
		CreatedBy:      actorOr(createdBy),
	}
	r.state.entities[id] = stored
	r.state.entityByKey[key] = id
	r.state.entityOrder = append(r.state.entityOrder, id)
	return stored.toDomain(), nil
}

// LinkClaim writes a claim_entities row. Idempotent under the
// UNIQUE(claim_id, entity_id, role) contract.
func (r EntityRepository) LinkClaim(_ context.Context, claimID, entityID, role string) error {
	if role == "" {
		role = "mention"
	}
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	r.state.claimEntities[claimEntityKey{ClaimID: claimID, EntityID: entityID, Role: role}] = role
	return nil
}

// UnlinkClaim drops every claim_entities row for the claim. Deleting
// nothing is success, so a retried merge or delete converges.
func (r EntityRepository) UnlinkClaim(_ context.Context, claimID string) error {
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	for key := range r.state.claimEntities {
		if key.ClaimID == claimID {
			delete(r.state.claimEntities, key)
		}
	}
	return nil
}

// List returns every entity ordered by name (matching the SQLite
// ORDER BY name COLLATE NOCASE behaviour close enough for tests —
// case-insensitive lexical sort on name).
func (r EntityRepository) List(_ context.Context) ([]domain.Entity, error) {
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	out := make([]domain.Entity, 0, len(r.state.entities))
	for _, e := range r.state.entities {
		out = append(out, e.toDomain())
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// ListByType returns entities of the given type, ordered by name.
func (r EntityRepository) ListByType(_ context.Context, etype domain.EntityType) ([]domain.Entity, error) {
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	out := make([]domain.Entity, 0)
	for _, e := range r.state.entities {
		if e.Type == etype {
			out = append(out, e.toDomain())
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// FindByName returns the first entity whose normalized name matches
// the input. Returns (zero, false, nil) on miss so callers can branch
// without handling sql.ErrNoRows.
func (r EntityRepository) FindByName(_ context.Context, name string) (domain.Entity, bool, error) {
	norm := domain.NormalizeEntityName(name)
	if norm == "" {
		return domain.Entity{}, false, nil
	}
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	// Iterate insertion order to match the SQLite "first match wins"
	// behaviour deterministically.
	for _, id := range r.state.entityOrder {
		e, ok := r.state.entities[id]
		if !ok {
			continue
		}
		if e.NormalizedName == norm {
			return e.toDomain(), true, nil
		}
	}
	return domain.Entity{}, false, nil
}

// ListClaimsForEntity returns the claims linked to the given entity,
// in chronological order by claim CreatedAt.
func (r EntityRepository) ListClaimsForEntity(_ context.Context, entityID string) ([]domain.Claim, error) {
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	seen := map[string]struct{}{}
	out := make([]domain.Claim, 0)
	for key := range r.state.claimEntities {
		if key.EntityID != entityID {
			continue
		}
		if _, dup := seen[key.ClaimID]; dup {
			continue
		}
		c, ok := r.state.claims[key.ClaimID]
		if !ok {
			continue
		}
		seen[key.ClaimID] = struct{}{}
		out = append(out, c.toDomain())
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// ListEntitiesForClaim returns the entities linked to the claim
// along with their roles. The two slices are aligned by index.
func (r EntityRepository) ListEntitiesForClaim(_ context.Context, claimID string) ([]domain.Entity, []string, error) {
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	type pair struct {
		entity domain.Entity
		role   string
	}
	pairs := make([]pair, 0)
	for key := range r.state.claimEntities {
		if key.ClaimID != claimID {
			continue
		}
		e, ok := r.state.entities[key.EntityID]
		if !ok {
			continue
		}
		pairs = append(pairs, pair{entity: e.toDomain(), role: key.Role})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		return strings.ToLower(pairs[i].entity.Name) < strings.ToLower(pairs[j].entity.Name)
	})
	ents := make([]domain.Entity, 0, len(pairs))
	roles := make([]string, 0, len(pairs))
	for _, p := range pairs {
		ents = append(ents, p.entity)
		roles = append(roles, p.role)
	}
	return ents, roles, nil
}

// Merge collapses one entity into another. Every claim_entities row
// pointing at loserID is rewritten to point at winnerID (deduped by
// the unique key), then the loser entity row is removed. Mirrors the
// SQLite implementation's transactional intent — under a single
// mutex, the operation is observably atomic to other readers.
func (r EntityRepository) Merge(_ context.Context, winnerID, loserID string) error {
	if winnerID == loserID {
		return ErrEntityMergeSelf
	}
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	loser, ok := r.state.entities[loserID]
	if !ok {
		// Match SQLite: loser missing is a no-op, the FK constraint
		// would have caught it earlier in a normal flow.
		return nil
	}
	for key := range r.state.claimEntities {
		if key.EntityID != loserID {
			continue
		}
		// Rewrite to winner; dedup via the unique-key map semantics.
		newKey := claimEntityKey{ClaimID: key.ClaimID, EntityID: winnerID, Role: key.Role}
		role := r.state.claimEntities[key]
		delete(r.state.claimEntities, key)
		// INSERT OR IGNORE: don't clobber an already-existing winner row.
		if _, exists := r.state.claimEntities[newKey]; !exists {
			r.state.claimEntities[newKey] = role
		}
	}
	delete(r.state.entityByKey, entityKey{NormalizedName: loser.NormalizedName, Type: loser.Type})
	delete(r.state.entities, loserID)
	r.state.entityOrder = removeString(r.state.entityOrder, loserID)
	return nil
}

// Count returns the number of entities currently stored.
func (r EntityRepository) Count(_ context.Context) (int64, error) {
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	return int64(len(r.state.entities)), nil
}

// ClaimIDsMissingEntityLinks returns claim ids that have no row in
// the link table — the working set for `mnemos extract-entities`.
func (r EntityRepository) ClaimIDsMissingEntityLinks(_ context.Context) ([]string, error) {
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	linked := map[string]struct{}{}
	for key := range r.state.claimEntities {
		linked[key.ClaimID] = struct{}{}
	}
	out := make([]string, 0)
	for _, id := range r.state.claimOrder {
		if _, hit := linked[id]; hit {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// ErrEntityMergeSelf is returned by Merge when winner == loser.
// Mirrors the SQLite implementation's sentinel so callers can use
// the same errors.Is target across backends.
var ErrEntityMergeSelf = errors.New("entity merge: winner and loser must differ")

func newEntityID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "en_" + hex.EncodeToString(buf), nil
}

func removeString(s []string, v string) []string {
	for i, x := range s {
		if x == v {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}
