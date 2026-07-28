package query

import (
	"context"
	"sort"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/embedding"
	"go.klarlabs.de/mnemos/internal/ports"
)

// fixedVectorClient embeds every text to the same fixed vector and reports a
// model id, so a test can control the model gate exactly.
type fixedVectorClient struct {
	vec   []float32
	model string
	err   error
}

func (c fixedVectorClient) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if c.err != nil {
		return nil, c.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = c.vec
	}
	return out, nil
}
func (c fixedVectorClient) ModelID() string { return c.model }

// memVectorRepo is a tiny in-package EmbeddingRepository that ALSO implements
// ClaimSimilaritySearcher with the same semantics as the real backends, so the
// engine's two claim-scoring paths can be exercised without a database.
type memVectorRepo struct {
	recs      []domain.EmbeddingRecord
	searchErr error

	listByTypeCalls int
	searchCalls     int
	lastCandidates  map[string]struct{}
	lastModel       string
}

func newMemVectorRepo() *memVectorRepo { return &memVectorRepo{} }

func (r *memVectorRepo) put(entityID, entityType string, vec []float32, model string) {
	r.recs = append(r.recs, domain.EmbeddingRecord{
		EntityID: entityID, EntityType: entityType, Vector: vec,
		Model: model, Dimensions: len(vec),
	})
}

func (r *memVectorRepo) Upsert(_ context.Context, entityID, entityType string, vector []float32, model, _ string) error {
	r.put(entityID, entityType, vector, model)
	return nil
}

func (r *memVectorRepo) ListByEntityType(_ context.Context, entityType string) ([]domain.EmbeddingRecord, error) {
	r.listByTypeCalls++
	out := make([]domain.EmbeddingRecord, 0, len(r.recs))
	for _, rec := range r.recs {
		if rec.EntityType == entityType {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (r *memVectorRepo) Delete(context.Context, string, string) error { return nil }
func (r *memVectorRepo) CountAll(context.Context) (int64, error)      { return int64(len(r.recs)), nil }
func (r *memVectorRepo) ListAll(context.Context) ([]domain.EmbeddingRecord, error) {
	return r.recs, nil
}
func (r *memVectorRepo) DeleteAll(context.Context) error { r.recs = nil; return nil }

// SearchClaimsByVector mirrors the real backends: candidate allowlist first,
// then the model gate, then decode+cosine, then the minSimilarity floor, then
// an optional topK truncation (topK<=0 means keep everything).
func (r *memVectorRepo) SearchClaimsByVector(
	_ context.Context,
	queryVector []float32,
	candidateClaimIDs map[string]struct{},
	model string,
	topK int,
	minSimilarity float64,
) ([]ports.ClaimSimilarityHit, error) {
	r.searchCalls++
	r.lastCandidates = candidateClaimIDs
	r.lastModel = model
	if r.searchErr != nil {
		return nil, r.searchErr
	}
	if len(queryVector) == 0 {
		return nil, nil
	}
	if candidateClaimIDs != nil && len(candidateClaimIDs) == 0 {
		return nil, nil
	}
	hits := make([]ports.ClaimSimilarityHit, 0, len(r.recs))
	for _, rec := range r.recs {
		if rec.EntityType != "claim" {
			continue
		}
		if candidateClaimIDs != nil {
			if _, ok := candidateClaimIDs[rec.EntityID]; !ok {
				continue
			}
		}
		if model != "" && rec.Model != model {
			continue
		}
		sim, err := embedding.CosineSimilarity(queryVector, rec.Vector)
		if err != nil {
			continue
		}
		score := float64(sim)
		if score < minSimilarity {
			continue
		}
		hits = append(hits, ports.ClaimSimilarityHit{
			ClaimID: rec.EntityID, Similarity: score, Model: rec.Model,
		})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Similarity > hits[j].Similarity })
	if topK > 0 && len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}

// plainEmbeddingRepo is an EmbeddingRepository with no optional capabilities,
// standing in for older stores and test doubles.
type plainEmbeddingRepo struct{}

func (plainEmbeddingRepo) Upsert(context.Context, string, string, []float32, string, string) error {
	return nil
}
func (plainEmbeddingRepo) ListByEntityType(context.Context, string) ([]domain.EmbeddingRecord, error) {
	return nil, nil
}
func (plainEmbeddingRepo) Delete(context.Context, string, string) error { return nil }
func (plainEmbeddingRepo) CountAll(context.Context) (int64, error)      { return 0, nil }
func (plainEmbeddingRepo) ListAll(context.Context) ([]domain.EmbeddingRecord, error) {
	return nil, nil
}
func (plainEmbeddingRepo) DeleteAll(context.Context) error { return nil }
