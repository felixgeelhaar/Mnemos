package memory

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// ClaimRepository is the in-memory implementation of
// [ports.ClaimRepository]. Status transitions are recorded so the
// status-history surface (mnemos audit etc.) works across both
// backends.
type ClaimRepository struct {
	state *state
}

// Upsert records each claim, appending a status-transition row when
// the status differs from the prior stored value (or when the claim
// is new).
func (r ClaimRepository) Upsert(ctx context.Context, claims []domain.Claim) error {
	return r.upsertWithReason(ctx, claims, "", "")
}

// UpsertWithReason captures a free-form reason on every transition
// row. Empty reason still records the row — the timestamp is the
// salvageable signal.
func (r ClaimRepository) UpsertWithReason(ctx context.Context, claims []domain.Claim, reason string) error {
	return r.upsertWithReason(ctx, claims, reason, "")
}

// UpsertWithReasonAs is the actor-aware variant — the changedBy id
// is stamped on every recorded transition.
func (r ClaimRepository) UpsertWithReasonAs(ctx context.Context, claims []domain.Claim, reason, changedBy string) error {
	return r.upsertWithReason(ctx, claims, reason, changedBy)
}

func (r ClaimRepository) upsertWithReason(_ context.Context, claims []domain.Claim, reason, changedBy string) error {
	if len(claims) == 0 {
		return nil
	}
	r.state.mu.Lock()
	defer r.state.mu.Unlock()

	now := time.Now().UTC()
	for _, claim := range claims {
		if err := claim.Validate(); err != nil {
			return fmt.Errorf("invalid claim %s: %w", claim.ID, err)
		}

		var priorStatus domain.ClaimStatus
		if existing, ok := r.state.claims[claim.ID]; ok {
			priorStatus = existing.Status
		}

		stored := storedClaimFromDomain(claim)
		// Preserve any pre-existing trust score when the caller hasn't
		// set one explicitly — RecomputeTrust runs after Upsert in the
		// pipeline and we don't want to clobber its output.
		if claim.TrustScore == 0 {
			if existing, ok := r.state.claims[claim.ID]; ok {
				stored.TrustScore = existing.TrustScore
			}
		}

		if _, ok := r.state.claims[claim.ID]; !ok {
			r.state.claimOrder = append(r.state.claimOrder, claim.ID)
		}
		r.state.claims[claim.ID] = stored

		// Append a claim_versions snapshot for every write (Refs #38).
		// Done inline rather than via the ClaimVersionRepository to
		// avoid a recursive lock acquisition — this loop already holds
		// state.mu.
		versions := r.state.claimVersions[claim.ID]
		r.state.claimVersions[claim.ID] = append(versions, domain.ClaimVersion{
			ClaimID:    claim.ID,
			Version:    len(versions) + 1,
			Text:       claim.Text,
			Confidence: claim.Confidence,
			Status:     claim.Status,
			WrittenAt:  now,
			WrittenBy:  actorOr(changedBy),
		})

		if priorStatus == claim.Status {
			continue
		}
		r.state.statusHistory[claim.ID] = append(r.state.statusHistory[claim.ID], storedTransition{
			ClaimID:    claim.ID,
			FromStatus: priorStatus, // empty for first insert
			ToStatus:   claim.Status,
			ChangedAt:  now,
			Reason:     reason,
			ChangedBy:  actorOr(changedBy),
		})
	}
	return nil
}

// UpsertEvidence stores claim → event links. The (claim, event) pair
// is the dedup key so duplicate links collapse silently.
func (r ClaimRepository) UpsertEvidence(_ context.Context, links []domain.ClaimEvidence) error {
	if len(links) == 0 {
		return nil
	}
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	for _, link := range links {
		if err := link.Validate(); err != nil {
			return fmt.Errorf("invalid claim evidence: %w", err)
		}
		set, ok := r.state.evidence[link.ClaimID]
		if !ok {
			set = map[string]struct{}{}
			r.state.evidence[link.ClaimID] = set
		}
		set[link.EventID] = struct{}{}
	}
	return nil
}

// ListByEventIDs returns the claims that have evidence pointing to any
// of the given event ids. Each claim appears once, ordered by
// CreatedAt (matching the SQLite ORDER BY c.created_at ASC).
func (r ClaimRepository) ListByEventIDs(_ context.Context, eventIDs []string) ([]domain.Claim, error) {
	if len(eventIDs) == 0 {
		return []domain.Claim{}, nil
	}
	wantedEvents := make(map[string]struct{}, len(eventIDs))
	for _, id := range eventIDs {
		wantedEvents[id] = struct{}{}
	}

	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	matched := map[string]struct{}{}
	for claimID, events := range r.state.evidence {
		for evID := range events {
			if _, hit := wantedEvents[evID]; hit {
				matched[claimID] = struct{}{}
				break
			}
		}
	}

	out := make([]domain.Claim, 0, len(matched))
	for _, id := range r.state.claimOrder {
		if _, ok := matched[id]; !ok {
			continue
		}
		if c, ok := r.state.claims[id]; ok {
			out = append(out, c.toDomain())
		}
	}
	return out, nil
}

// ListEvidenceByClaimIDs returns the (claim_id, event_id) links for
// the given claim ids.
func (r ClaimRepository) ListEvidenceByClaimIDs(_ context.Context, claimIDs []string) ([]domain.ClaimEvidence, error) {
	if len(claimIDs) == 0 {
		return []domain.ClaimEvidence{}, nil
	}
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	out := make([]domain.ClaimEvidence, 0)
	for _, cid := range claimIDs {
		set, ok := r.state.evidence[cid]
		if !ok {
			continue
		}
		for evID := range set {
			out = append(out, domain.ClaimEvidence{ClaimID: cid, EventID: evID})
		}
	}
	return out, nil
}

// ListByIDs returns claims with the given ids. Missing ids are
// dropped silently (parity with SQLite).
func (r ClaimRepository) ListByIDs(_ context.Context, claimIDs []string) ([]domain.Claim, error) {
	if len(claimIDs) == 0 {
		return []domain.Claim{}, nil
	}
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	out := make([]domain.Claim, 0, len(claimIDs))
	for _, id := range claimIDs {
		if c, ok := r.state.claims[id]; ok {
			out = append(out, c.toDomain())
		}
	}
	return out, nil
}

// RepointEvidence rewrites every (fromClaimID, eventID) link to
// (toClaimID, eventID). Duplicates collapse via the dedup map key.
func (r ClaimRepository) RepointEvidence(_ context.Context, fromClaimID, toClaimID string) error {
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	src, ok := r.state.evidence[fromClaimID]
	if !ok || len(src) == 0 {
		return nil
	}
	dst, ok := r.state.evidence[toClaimID]
	if !ok {
		dst = map[string]struct{}{}
		r.state.evidence[toClaimID] = dst
	}
	for evID := range src {
		dst[evID] = struct{}{}
	}
	delete(r.state.evidence, fromClaimID)
	return nil
}

// DeleteCascade removes a claim plus every claim-keyed collection it
// owns: evidence, status history, the version chain, the feedback
// counters and the forward expectation — the full canonical set from
// [ports.ClaimRepository], since the in-memory backend models all five.
//
// The dependent drops run even when the claim itself is already gone.
// Returning early on a missing claim made this the one backend where a
// resumed delete could NOT finish the job: the SQL backends' DELETEs are
// unconditional, so a crash between the claim delete and a retry left
// memory holding residue nothing would ever clear.
func (r ClaimRepository) DeleteCascade(_ context.Context, claimID string) error {
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	if _, ok := r.state.claims[claimID]; ok {
		delete(r.state.claims, claimID)
		r.state.claimOrder = removeStringFromSlice(r.state.claimOrder, claimID)
	}
	delete(r.state.evidence, claimID)
	delete(r.state.statusHistory, claimID)
	delete(r.state.claimVersions, claimID)
	delete(r.state.feedback, claimID)
	delete(r.state.expectations, claimID)
	return nil
}

func removeStringFromSlice(s []string, v string) []string {
	for i, x := range s {
		if x == v {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}

// ListAll returns every claim in insertion order.
func (r ClaimRepository) ListAll(_ context.Context) ([]domain.Claim, error) {
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	out := make([]domain.Claim, 0, len(r.state.claimOrder))
	for _, id := range r.state.claimOrder {
		if c, ok := r.state.claims[id]; ok {
			out = append(out, c.toDomain())
		}
	}
	return out, nil
}

// ListByTestRequirementRef returns test_result claims sharing the given
// non-empty TestRequirementRef. Sorted TestLastRunAt DESC then CreatedAt
// DESC to mirror the SQL backends.
func (r ClaimRepository) ListByTestRequirementRef(_ context.Context, ref string) ([]domain.Claim, error) {
	if ref == "" {
		return nil, nil
	}
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	out := make([]domain.Claim, 0)
	for _, id := range r.state.claimOrder {
		c, ok := r.state.claims[id]
		if !ok {
			continue
		}
		dc := c.toDomain()
		if dc.Type != domain.ClaimTypeTestResult || dc.TestRequirementRef != ref {
			continue
		}
		out = append(out, dc)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].TestLastRunAt.Equal(out[j].TestLastRunAt) {
			return out[i].TestLastRunAt.After(out[j].TestLastRunAt)
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// CountAll returns the total number of claims stored.
func (r ClaimRepository) CountAll(_ context.Context) (int64, error) {
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	return int64(len(r.state.claims)), nil
}

// ListAllEvidence returns every (claim_id, event_id) link.
func (r ClaimRepository) ListAllEvidence(_ context.Context) ([]domain.ClaimEvidence, error) {
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	out := make([]domain.ClaimEvidence, 0)
	for cid, set := range r.state.evidence {
		for evID := range set {
			out = append(out, domain.ClaimEvidence{ClaimID: cid, EventID: evID})
		}
	}
	return out, nil
}

// DeleteAll wipes claims plus their owned dependent rows.
func (r ClaimRepository) DeleteAll(_ context.Context) error {
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	r.state.claims = map[string]storedClaim{}
	r.state.claimOrder = nil
	r.state.evidence = map[string]map[string]struct{}{}
	r.state.statusHistory = map[string][]storedTransition{}
	return nil
}

// ListIDsMissingEmbedding returns claim ids without an embedding row.
func (r ClaimRepository) ListIDsMissingEmbedding(_ context.Context) ([]string, error) {
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	out := make([]string, 0)
	for _, id := range r.state.claimOrder {
		key := embeddingKey{EntityID: id, EntityType: "claim"}
		if _, ok := r.state.embeddings[key]; !ok {
			out = append(out, id)
		}
	}
	return out, nil
}

// ListAllStatusHistory returns every status transition across all
// claims. Order: per-claim insertion order, claims interleaved by
// transition timestamp.
func (r ClaimRepository) ListAllStatusHistory(_ context.Context) ([]domain.ClaimStatusTransition, error) {
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	out := make([]domain.ClaimStatusTransition, 0)
	for _, transitions := range r.state.statusHistory {
		for _, t := range transitions {
			out = append(out, domain.ClaimStatusTransition{
				ClaimID:    t.ClaimID,
				FromStatus: t.FromStatus,
				ToStatus:   t.ToStatus,
				ChangedAt:  t.ChangedAt,
				Reason:     t.Reason,
				ChangedBy:  t.ChangedBy,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ChangedAt.Before(out[j].ChangedAt)
	})
	return out, nil
}

// ListStatusHistoryByClaimID returns the claim's status transitions
// in insertion order (oldest first).
func (r ClaimRepository) ListStatusHistoryByClaimID(_ context.Context, claimID string) ([]domain.ClaimStatusTransition, error) {
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	src := r.state.statusHistory[claimID]
	out := make([]domain.ClaimStatusTransition, 0, len(src))
	for _, t := range src {
		out = append(out, domain.ClaimStatusTransition{
			ClaimID:    t.ClaimID,
			FromStatus: t.FromStatus,
			ToStatus:   t.ToStatus,
			ChangedAt:  t.ChangedAt,
			Reason:     t.Reason,
			ChangedBy:  t.ChangedBy,
		})
	}
	return out, nil
}

// MarkVerified bumps last_verified, increments verify_count, and
// optionally writes a per-claim half-life override. Returns an error
// when the claim does not exist (the SQLite UPDATE is silent on a
// missing row; the in-memory variant is stricter to make tests fail
// loudly when an id typo escapes).
func (r ClaimRepository) MarkVerified(_ context.Context, claimID string, verifiedAt time.Time, halfLifeDays float64) error {
	if verifiedAt.IsZero() {
		verifiedAt = time.Now().UTC()
	}
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	c, ok := r.state.claims[claimID]
	if !ok {
		return fmt.Errorf("claim %s: not found", claimID)
	}
	c.LastVerified = verifiedAt.UTC()
	c.VerifyCount++
	if halfLifeDays > 0 {
		c.HalfLifeDays = halfLifeDays
	}
	r.state.claims[claimID] = c
	return nil
}

// SetValidity sets (or, with a zero validTo, clears) the claim's
// upper validity bound. Returns an error if the claim does not exist.
func (r ClaimRepository) SetValidity(_ context.Context, claimID string, validTo time.Time) error {
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	c, ok := r.state.claims[claimID]
	if !ok {
		return fmt.Errorf("claim %s: not found", claimID)
	}
	if validTo.IsZero() {
		c.ValidTo = time.Time{}
	} else {
		c.ValidTo = validTo.UTC()
	}
	r.state.claims[claimID] = c
	return nil
}

// SetLifecycle transitions a claim's promotion state in place.
func (r ClaimRepository) SetLifecycle(_ context.Context, claimID string, lifecycle domain.ClaimLifecycle) error {
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	c, ok := r.state.claims[claimID]
	if !ok {
		// Wrap sql.ErrNoRows so callers can errors.Is across every backend
		// (the SQL backends return the same), per the SetLifecycle contract.
		return fmt.Errorf("claim %s: %w", claimID, sql.ErrNoRows)
	}
	c.Lifecycle = lifecycle
	r.state.claims[claimID] = c
	return nil
}

// RecomputeTrust applies the supplied scoring function to every
// stored claim and writes the result back. Returns the number of
// claims touched.
func (r ClaimRepository) RecomputeTrust(_ context.Context, score func(confidence float64, evidenceCount int, latestEvidence time.Time) float64) (int, error) {
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	return r.recomputeTrustLocked(r.state.claimOrder, score), nil
}

// RecomputeTrustForClaims implements [ports.ScopedTrustScorer]: the same
// recomputation bounded to claimIDs, so a write's cost tracks what it touched
// rather than the size of the store.
func (r ClaimRepository) RecomputeTrustForClaims(_ context.Context, claimIDs []string, score func(confidence float64, evidenceCount int, latestEvidence time.Time) float64) (int, error) {
	if len(claimIDs) == 0 {
		return 0, nil
	}
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	return r.recomputeTrustLocked(claimIDs, score), nil
}

// recomputeTrustLocked rescores the given claim ids. Callers hold state.mu.
// Unknown ids are skipped, so a caller may pass ids for claims that were
// deleted between the write and the rescore.
func (r ClaimRepository) recomputeTrustLocked(ids []string, score func(confidence float64, evidenceCount int, latestEvidence time.Time) float64) int {
	count := 0
	for _, id := range ids {
		c, ok := r.state.claims[id]
		if !ok {
			continue
		}
		// Corroboration is graded by INDEPENDENCE, not raw volume: count distinct
		// evidence-event authors, with same-source repeats discounted (echo-chamber
		// guard) — many events from one voice don't corroborate like many voices.
		distinct := make(map[string]struct{})
		total := 0
		for evID := range r.state.evidence[id] {
			if ev, ok := r.state.events[evID]; ok {
				distinct[ev.CreatedBy] = struct{}{}
				total++
			}
		}
		evidenceCount := domain.EffectiveEvidenceCount(len(distinct), total)
		latest := latestEvidenceTimestamp(r.state, id)
		c.TrustScore = score(c.Confidence, evidenceCount, latest)
		r.state.claims[id] = c
		count++
	}
	return count
}

// ApplyBeliefCredit implements [ports.BeliefCreditWriter]: it overwrites a
// claim's confidence_components (the credit-assignment audit map) and its
// trust_score together. Callers pass the already-merged component map, so the
// write is a plain assignment and re-running is idempotent.
func (r ClaimRepository) ApplyBeliefCredit(_ context.Context, claimID string, components map[string]float64, trustScore float64) error {
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	c, ok := r.state.claims[claimID]
	if !ok {
		return fmt.Errorf("apply belief credit: claim %s not found", claimID)
	}
	if len(components) == 0 {
		c.ConfidenceComponents = nil
	} else {
		cp := make(map[string]float64, len(components))
		for k, v := range components {
			cp[k] = v
		}
		c.ConfidenceComponents = cp
	}
	c.TrustScore = trustScore
	r.state.claims[claimID] = c
	return nil
}

// AverageTrust returns the mean trust score across every stored
// claim, or 0 when none exist.
func (r ClaimRepository) AverageTrust(_ context.Context) (float64, error) {
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	if len(r.state.claims) == 0 {
		return 0, nil
	}
	var sum float64
	for _, c := range r.state.claims {
		sum += c.TrustScore
	}
	return sum / float64(len(r.state.claims)), nil
}

// CountClaimsBelowTrust returns the count of claims whose trust score
// is strictly less than the threshold.
func (r ClaimRepository) CountClaimsBelowTrust(_ context.Context, threshold float64) (int64, error) {
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	var n int64
	for _, c := range r.state.claims {
		if c.TrustScore < threshold {
			n++
		}
	}
	return n, nil
}

// latestEvidenceTimestamp returns the timestamp of the most recent
// event linked to the claim. The state mutex must already be held by
// the caller. Zero time when no evidence is present.
func latestEvidenceTimestamp(s *state, claimID string) time.Time {
	var latest time.Time
	for evID := range s.evidence[claimID] {
		ev, ok := s.events[evID]
		if !ok {
			continue
		}
		if ev.Timestamp.After(latest) {
			latest = ev.Timestamp
		}
	}
	return latest
}
