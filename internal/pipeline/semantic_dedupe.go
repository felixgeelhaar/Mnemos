package pipeline

import (
	"context"
	"errors"
	"fmt"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/embedding"
	"go.klarlabs.de/mnemos/internal/store"
)

// SemanticDedupePlan is the result of a similarity scan: a list of
// merge proposals describing which claim should absorb which others,
// and the highest similarity that justified each merge. Returned
// without writing so callers can preview (--dry-run) before applying.
type SemanticDedupePlan struct {
	// Merges keyed by canonical (winner) claim id. Each entry holds
	// the duplicate ids that should be folded into it.
	Merges []SemanticMerge
	// Threshold the plan was built against, echoed back for clarity
	// in user-facing output.
	Threshold float64
	// ClaimsScanned is the population the scan considered (claims that
	// have an embedding under the current entity_type='claim'). Claims
	// without an embedding are skipped — they cannot be compared.
	ClaimsScanned int
	// SkippedNoEmbedding is how many claims were excluded because they
	// have no embedding row. Surfaced so users know the scan was
	// partial and can run `mnemos reembed` first if they care.
	SkippedNoEmbedding int
}

// SemanticMerge describes one absorption: a winner claim id that
// should absorb the listed duplicates (and inherit their evidence
// links). MaxSimilarity is the strongest pairwise similarity inside
// the cluster, useful for sorting the dry-run output by confidence.
type SemanticMerge struct {
	WinnerID      string
	DuplicateIDs  []string
	MaxSimilarity float32
}

// PlanSemanticDedupe scans every claim with an embedding, groups
// claims whose pairwise cosine similarity is at or above threshold,
// and selects a winner per cluster — the highest trust_score, with
// the earliest CreatedAt as a deterministic tiebreaker. Returns the
// plan without modifying the database. Callers (typically
// `mnemos dedup`) can then either present it (--dry-run) or pass it
// to ApplySemanticDedupe.
//
// Memory: holds every claim embedding in memory at once — at 768
// dims × 4 bytes that's ~3 MB per 1000 claims, fine for the
// local-first scale Mnemos targets. If/when we need to scale past
// hundreds of thousands of claims this should move to a vector
// index (sqlite-vss with cgo, or a separate process).
func PlanSemanticDedupe(ctx context.Context, conn *store.Conn, threshold float64) (SemanticDedupePlan, error) {
	if threshold <= 0 || threshold > 1 {
		return SemanticDedupePlan{}, fmt.Errorf("threshold must be in (0, 1]; got %v", threshold)
	}

	allClaims, err := conn.Claims.ListAll(ctx)
	if err != nil {
		return SemanticDedupePlan{}, fmt.Errorf("list claims: %w", err)
	}
	if len(allClaims) < 2 {
		return SemanticDedupePlan{Threshold: threshold, ClaimsScanned: len(allClaims)}, nil
	}

	stored, err := conn.Embeddings.ListByEntityType(ctx, "claim")
	if err != nil {
		return SemanticDedupePlan{}, fmt.Errorf("list claim embeddings: %w", err)
	}
	vecByID := make(map[string][]float32, len(stored))
	for _, rec := range stored {
		vecByID[rec.EntityID] = rec.Vector
	}

	// Index claims that actually have an embedding. We need both the
	// vector and the rest of the claim metadata (trust + created_at
	// for tiebreaking).
	pool := make([]indexedClaim, 0, len(allClaims))
	for _, c := range allClaims {
		v, ok := vecByID[c.ID]
		if !ok || len(v) == 0 {
			continue
		}
		pool = append(pool, indexedClaim{claim: c, vec: v})
	}
	skipped := len(allClaims) - len(pool)

	// Union-Find over the pool. Each cluster collapses to one root;
	// after the pass we walk roots → members to build merges.
	parent := make([]int, len(pool))
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	// Pairwise similarity. O(n^2) but with a small constant — for the
	// 10k-claim ceiling this is ~50M cosine ops, still under a second
	// on a modern laptop.
	maxSim := make(map[[2]int]float32)
	for i := 0; i < len(pool); i++ {
		for j := i + 1; j < len(pool); j++ {
			if len(pool[i].vec) != len(pool[j].vec) {
				continue
			}
			sim, err := embedding.CosineSimilarity(pool[i].vec, pool[j].vec)
			if err != nil {
				continue
			}
			if float64(sim) >= threshold {
				union(i, j)
				maxSim[[2]int{i, j}] = sim
			}
		}
	}

	// Group members by their root.
	clusters := make(map[int][]int)
	for i := range pool {
		r := find(i)
		clusters[r] = append(clusters[r], i)
	}

	merges := make([]SemanticMerge, 0)
	for _, members := range clusters {
		if len(members) < 2 {
			continue
		}
		winnerIdx := pickWinner(pool, members)
		dupes := make([]string, 0, len(members)-1)
		for _, m := range members {
			if m == winnerIdx {
				continue
			}
			dupes = append(dupes, pool[m].claim.ID)
		}
		var clusterMax float32
		for i := 0; i < len(members); i++ {
			for j := i + 1; j < len(members); j++ {
				key := [2]int{members[i], members[j]}
				if members[i] > members[j] {
					key = [2]int{members[j], members[i]}
				}
				if s, ok := maxSim[key]; ok && s > clusterMax {
					clusterMax = s
				}
			}
		}
		merges = append(merges, SemanticMerge{
			WinnerID:      pool[winnerIdx].claim.ID,
			DuplicateIDs:  dupes,
			MaxSimilarity: clusterMax,
		})
	}

	return SemanticDedupePlan{
		Merges:             merges,
		Threshold:          threshold,
		ClaimsScanned:      len(pool),
		SkippedNoEmbedding: skipped,
	}, nil
}

// indexedClaim pairs a claim with its embedding vector for the
// dedupe scan. Lifted out of PlanSemanticDedupe so pickWinner can
// accept the same shape without re-declaring the anonymous type.
type indexedClaim struct {
	claim domain.Claim
	vec   []float32
}

// pickWinner returns the index of the cluster member that should
// absorb the others. Highest trust_score wins; ties broken by
// earliest CreatedAt (older claim has accumulated more evidence /
// is more anchored in the knowledge base); final tiebreak is lex
// order on id for stable output.
func pickWinner(pool []indexedClaim, members []int) int {
	best := members[0]
	for _, m := range members[1:] {
		if pool[m].claim.TrustScore > pool[best].claim.TrustScore {
			best = m
			continue
		}
		if pool[m].claim.TrustScore < pool[best].claim.TrustScore {
			continue
		}
		if pool[m].claim.CreatedAt.Before(pool[best].claim.CreatedAt) {
			best = m
			continue
		}
		if pool[m].claim.CreatedAt.After(pool[best].claim.CreatedAt) {
			continue
		}
		if pool[m].claim.ID < pool[best].claim.ID {
			best = m
		}
	}
	return best
}

// DedupeTombstoneReason is the audit reason on the status_history row written
// when a losing claim is retired by a merge. It is also what an operator greps
// for when a merge could not remove the row and left a tombstone behind.
const DedupeTombstoneReason = "semantic dedupe: merged into the canonical claim"

// claimEntityUnlinker is the optional capability a backend may expose to drop a
// claim's rows in the claim↔entity link table.
//
// That table is the one claim dependent no port method can currently reach, and
// it is not inert: on FK-enforcing backends a surviving claim_entities row makes
// the final `DELETE FROM claims` fail outright, which is precisely how a merge
// used to abandon a stripped loser. When a backend grows the method this
// assertion starts succeeding and the merge completes the delete; until then the
// merge falls back to leaving an auditable tombstone (see [ApplySemanticDedupe]).
type claimEntityUnlinker interface {
	UnlinkClaim(ctx context.Context, claimID string) error
}

// ApplySemanticDedupe executes the plan through port-typed
// repository methods so it works against every storage backend.
//
// A merge is a move, not a delete: EVERY row hanging off the losing claim has
// to end up on the winner or be cleaned, or it survives pointing at a claim
// that is no longer reachable. The dependents of a claim, and what happens to
// each:
//
//   - claim_evidence     → repointed to the winner (Claims.RepointEvidence),
//     so the winner ends up with the UNION of both claims' evidence rather
//     than just its own. Duplicate (claim_id, event_id) pairs collapse.
//   - relationships      → repointed (Relationships.RepointEndpoint); self-loops
//     and duplicate edges created by the rewrite are dropped.
//   - claim_entities     → the winner is linked to every entity the loser
//     mentioned (Entities.LinkClaim), then the loser's links are dropped when
//     the backend exposes [claimEntityUnlinker].
//   - claim_expectations → the winner inherits the loser's forward prediction
//     when it has none of its own.
//   - claim_feedback     → helpful / negative-streak counts are folded into the
//     winner's row.
//   - embeddings         → the loser's vector is deleted; the winner keeps its
//     own (the plan chose it as canonical).
//   - claim_evidence, claim_status_history, claim_versions
//     → removed with the row by ClaimRepository.DeleteCascade.
//
// Ordering is deliberate. The loser is DEPRECATED before anything is moved off
// it: repository calls commit independently, so without a tombstone a failure
// after the evidence repoint left an ACTIVE claim with no evidence and no edges
// — an unsupported belief that still scored and that recall could still
// surface. With the tombstone the worst reachable end state is a retired row.
//
// A loser whose row cannot be deleted (a residual dependent this pass cannot
// reach) stays tombstoned and is reported in the returned error rather than
// silently abandoned; the merge continues so one stubborn claim cannot block
// the rest of the plan. Every step is idempotent, so a re-run converges.
func ApplySemanticDedupe(ctx context.Context, conn *store.Conn, plan SemanticDedupePlan) (int, error) {
	if len(plan.Merges) == 0 {
		return 0, nil
	}
	if conn == nil || conn.Claims == nil || conn.Relationships == nil || conn.Embeddings == nil {
		return 0, fmt.Errorf("apply semantic dedupe: conn missing required repositories")
	}

	merged := 0
	var tombstoned []string
	var causes []error
	for _, m := range plan.Merges {
		for _, dupID := range m.DuplicateIDs {
			cause, err := mergeClaimInto(ctx, conn, dupID, m.WinnerID)
			if err != nil {
				return merged, err
			}
			merged++
			if cause != nil {
				tombstoned = append(tombstoned, dupID)
				causes = append(causes, cause)
			}
		}
	}
	if len(tombstoned) > 0 {
		return merged, fmt.Errorf(
			"merged %d claim(s), but %d could not be removed and remain as deprecated tombstones %v (re-run to resume): %w",
			merged, len(tombstoned), tombstoned, errors.Join(causes...))
	}
	return merged, nil
}

// mergeClaimInto folds dupID into winnerID. A non-nil first return is the
// reason the losing row survived as a tombstone (the merge itself succeeded);
// a non-nil error means the merge could not even get that far.
func mergeClaimInto(ctx context.Context, conn *store.Conn, dupID, winnerID string) (tombstoneCause, err error) {
	if err := tombstoneMergedClaim(ctx, conn, dupID); err != nil {
		return nil, err
	}
	// Additive first: the winner gains everything before the loser loses it, so
	// an interrupted merge duplicates knowledge rather than destroying it.
	if err := repointEntityLinks(ctx, conn, dupID, winnerID); err != nil {
		return nil, err
	}
	if err := repointExpectation(ctx, conn, dupID, winnerID); err != nil {
		return nil, err
	}
	if err := mergeFeedback(ctx, conn, dupID, winnerID); err != nil {
		return nil, err
	}
	if err := conn.Claims.RepointEvidence(ctx, dupID, winnerID); err != nil {
		return nil, fmt.Errorf("repoint evidence %s→%s: %w", dupID, winnerID, err)
	}
	if err := conn.Relationships.RepointEndpoint(ctx, dupID, winnerID); err != nil {
		return nil, fmt.Errorf("repoint relationships %s→%s: %w", dupID, winnerID, err)
	}
	if err := conn.Embeddings.Delete(ctx, dupID, "claim"); err != nil {
		return nil, fmt.Errorf("delete embedding for %s: %w", dupID, err)
	}
	if unlinker, ok := conn.Entities.(claimEntityUnlinker); ok {
		if err := unlinker.UnlinkClaim(ctx, dupID); err != nil {
			return nil, fmt.Errorf("unlink entities for %s: %w", dupID, err)
		}
	}
	if err := conn.Claims.DeleteCascade(ctx, dupID); err != nil {
		// The loser is already retired and its knowledge is on the winner. Keep
		// the tombstone and report the cause upward rather than aborting the plan.
		return fmt.Errorf("delete merged claim %s: %w", dupID, err), nil
	}
	return nil, nil
}

// tombstoneMergedClaim deprecates the losing claim before any of its rows move,
// so a failure part-way through can never leave an active un-evidenced belief.
func tombstoneMergedClaim(ctx context.Context, conn *store.Conn, dupID string) error {
	existing, err := conn.Claims.ListByIDs(ctx, []string{dupID})
	if err != nil {
		return fmt.Errorf("read claim %s before merge: %w", dupID, err)
	}
	if len(existing) != 1 || existing[0].Status == domain.ClaimStatusDeprecated {
		return nil
	}
	tomb := existing[0]
	tomb.Status = domain.ClaimStatusDeprecated
	if err := conn.Claims.UpsertWithReasonAs(ctx, []domain.Claim{tomb}, DedupeTombstoneReason, domain.SystemUser); err != nil {
		return fmt.Errorf("tombstone claim %s before merge: %w", dupID, err)
	}
	return nil
}

// repointEntityLinks gives the winner every entity the loser mentioned. Linking
// is idempotent on (claim_id, entity_id, role), so re-running is safe.
func repointEntityLinks(ctx context.Context, conn *store.Conn, dupID, winnerID string) error {
	if conn.Entities == nil {
		return nil
	}
	entities, roles, err := conn.Entities.ListEntitiesForClaim(ctx, dupID)
	if err != nil {
		return fmt.Errorf("list entity links for %s: %w", dupID, err)
	}
	for i, e := range entities {
		role := "mention"
		if i < len(roles) && roles[i] != "" {
			role = roles[i]
		}
		if err := conn.Entities.LinkClaim(ctx, winnerID, e.ID, role); err != nil {
			return fmt.Errorf("link entity %s to winner %s: %w", e.ID, winnerID, err)
		}
	}
	return nil
}

// repointExpectation moves the loser's forward prediction onto the winner when
// the winner has none. A winner that already carries an expectation keeps it —
// it is the canonical claim, and expectations are one-per-claim.
func repointExpectation(ctx context.Context, conn *store.Conn, dupID, winnerID string) error {
	if conn.Expectations == nil {
		return nil
	}
	exp, ok, err := conn.Expectations.Get(ctx, dupID)
	if err != nil {
		return fmt.Errorf("read expectation for %s: %w", dupID, err)
	}
	if !ok {
		return nil
	}
	if _, winnerHas, err := conn.Expectations.Get(ctx, winnerID); err != nil {
		return fmt.Errorf("read expectation for %s: %w", winnerID, err)
	} else if winnerHas {
		return nil
	}
	exp.ClaimID = winnerID
	if err := conn.Expectations.Upsert(ctx, exp); err != nil {
		return fmt.Errorf("move expectation %s→%s: %w", dupID, winnerID, err)
	}
	return nil
}

// mergeFeedback folds the loser's feedback counters into the winner's row so a
// merge never discards recorded human signal.
func mergeFeedback(ctx context.Context, conn *store.Conn, dupID, winnerID string) error {
	if conn.Feedback == nil {
		return nil
	}
	loser, ok, err := conn.Feedback.Get(ctx, dupID)
	if err != nil {
		return fmt.Errorf("read feedback for %s: %w", dupID, err)
	}
	if !ok {
		return nil
	}
	winner, _, err := conn.Feedback.Get(ctx, winnerID)
	if err != nil {
		return fmt.Errorf("read feedback for %s: %w", winnerID, err)
	}
	winner.ClaimID = winnerID
	winner.HelpfulCount += loser.HelpfulCount
	if loser.NegativeFeedbackStreak > winner.NegativeFeedbackStreak {
		winner.NegativeFeedbackStreak = loser.NegativeFeedbackStreak
	}
	if loser.LastFeedbackAt.After(winner.LastFeedbackAt) {
		winner.LastFeedbackAt = loser.LastFeedbackAt
		winner.LastFeedbackNote = loser.LastFeedbackNote
	}
	if err := conn.Feedback.Upsert(ctx, winner); err != nil {
		return fmt.Errorf("merge feedback %s→%s: %w", dupID, winnerID, err)
	}
	return nil
}
