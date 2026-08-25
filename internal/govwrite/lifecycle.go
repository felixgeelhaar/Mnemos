package govwrite

import (
	"context"
	"errors"
	"fmt"
	"time"

	axidomain "go.klarlabs.de/axi/domain"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/store"
)

// The executors below carry the claim-lifecycle mutations, entity
// canonicalisation, and destructive deletes the daemon performs. Like
// every other governed write they run inside the axi kernel — the only
// place this package reaches a conn.<Repo> mutator — and each returns an
// []axidomain.EvidenceRecord so the destructive op leaves exactly one
// auditable record on the session's evidence chain. A delete with no
// audit trail is the worst case the spec guards against; routing the
// whole multi-step cascade through ONE governed action makes the
// destructive op a single atomic entry.

// --- Mark verified ---

type markVerifiedInput struct {
	ClaimID      string
	VerifiedAt   time.Time
	HalfLifeDays float64
}

type markVerifiedExecutor struct{ conn *store.Conn }

// Execute bumps the claim's verification and records an evidence row.
func (e markVerifiedExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[markVerifiedInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("mark_verified: unexpected input %T", input)
	}
	if err := e.conn.Claims.MarkVerified(ctx, in.ClaimID, in.VerifiedAt, in.HalfLifeDays); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("mark verified: %w", err)
	}
	return axidomain.ExecutionResult{
		Data:    in.ClaimID,
		Summary: fmt.Sprintf("verified claim %s", in.ClaimID),
	}, ev("mnemos.write.mark_verified", map[string]any{
		"claim_id": in.ClaimID, "half_life_days": in.HalfLifeDays,
	}), nil
}

// MarkVerified bumps a claim's last_verified and increments verify_count.
// An optional halfLifeDays > 0 also rewrites the per-claim freshness
// override.
func (w *Writer) MarkVerified(ctx context.Context, claimID string, verifiedAt time.Time, halfLifeDays float64) error {
	_, err := dispatch[string](ctx, w, actionMarkVerified, markVerifiedInput{
		ClaimID: claimID, VerifiedAt: verifiedAt, HalfLifeDays: halfLifeDays,
	})
	return err
}

// --- Set validity ---

type setValidityInput struct {
	ClaimID string
	ValidTo time.Time
}

type setValidityExecutor struct{ conn *store.Conn }

// Execute closes the claim's validity interval and records an evidence row.
func (e setValidityExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[setValidityInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("set_validity: unexpected input %T", input)
	}
	if err := e.conn.Claims.SetValidity(ctx, in.ClaimID, in.ValidTo); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("set validity: %w", err)
	}
	return axidomain.ExecutionResult{
		Data:    in.ClaimID,
		Summary: fmt.Sprintf("set valid_to on claim %s", in.ClaimID),
	}, ev("mnemos.write.set_validity", map[string]any{
		"claim_id": in.ClaimID, "valid_to": in.ValidTo.Format(time.RFC3339),
	}), nil
}

// SetValidity closes a claim's validity interval at validTo (or clears
// it when validTo is the zero value).
func (w *Writer) SetValidity(ctx context.Context, claimID string, validTo time.Time) error {
	_, err := dispatch[string](ctx, w, actionSetValidity, setValidityInput{
		ClaimID: claimID, ValidTo: validTo,
	})
	return err
}

// --- Merge entities ---

type mergeEntitiesInput struct {
	WinnerID string
	LoserID  string
}

type mergeEntitiesExecutor struct{ conn *store.Conn }

// Execute merges the loser entity into the winner and records an evidence row.
func (e mergeEntitiesExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[mergeEntitiesInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("merge_entities: unexpected input %T", input)
	}
	if err := e.conn.Entities.Merge(ctx, in.WinnerID, in.LoserID); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("merge entities: %w", err)
	}
	return axidomain.ExecutionResult{
		Data:    in.WinnerID,
		Summary: fmt.Sprintf("merged %s into %s", in.LoserID, in.WinnerID),
	}, ev("mnemos.write.merge_entities", map[string]any{
		"winner_id": in.WinnerID, "loser_id": in.LoserID,
	}), nil
}

// MergeEntities folds the loser entity into the winner and deletes the
// loser. Destructive: routed through the kernel so the merge leaves an
// auditable record.
func (w *Writer) MergeEntities(ctx context.Context, winnerID, loserID string) error {
	_, err := dispatch[string](ctx, w, actionMergeEntities, mergeEntitiesInput{
		WinnerID: winnerID, LoserID: loserID,
	})
	return err
}

// --- Delete claim cascade (single governed entry, tombstone-first) ---

// TombstoneReason is the audit reason recorded on the status_history row a
// cascade delete writes before it strips a claim's dependent rows. It is the
// marker an operator (or a resumed run) can grep the audit trail for when a
// delete did not get to remove the claim row.
const TombstoneReason = "cascade delete: tombstoned before dependent rows are stripped"

// ErrStrippedClaimSurvives is the sentinel every [PartialDeleteError] wraps.
// Callers that only need "did the cascade leave residue?" can test
// errors.Is(err, ErrStrippedClaimSurvives) instead of type-asserting.
var ErrStrippedClaimSurvives = errors.New("cascade delete left a tombstoned claim row behind")

// PartialDeleteError reports the one outcome a cross-repository cascade cannot
// rule out: the dependent rows were removed but the claim row itself was not.
//
// The repositories a cascade touches (relationships, embeddings, claims) each
// commit independently — there is no cross-repository transaction in the port
// surface — so a failure on the final delete used to return an error while the
// earlier deletes had already committed. What survived was an ACTIVE claim with
// no evidence and no edges: an unsupported belief that still scored, and that
// recall could still surface. That is strictly worse than either a clean delete
// or a clean no-op.
//
// The cascade therefore tombstones first (see [TombstoneReason]): the claim is
// deprecated BEFORE anything is stripped, so the worst reachable end state is a
// deprecated, retired row rather than a live phantom belief. This error is how
// that end state is surfaced instead of left silent; the cascade is idempotent,
// so re-running it on the same id converges once the underlying fault clears.
type PartialDeleteError struct {
	// ClaimID is the claim whose row survived.
	ClaimID string
	// Tombstoned reports whether the surviving row was successfully
	// deprecated. False means the tombstone write ALSO failed — nothing was
	// stripped in that case, so the claim is untouched and still intact.
	Tombstoned bool
	// Err is the underlying failure from the final delete.
	Err error
}

func (e *PartialDeleteError) Error() string {
	if !e.Tombstoned {
		return fmt.Sprintf("tombstone claim %s before cascade delete (nothing stripped; claim is intact): %v", e.ClaimID, e.Err)
	}
	return fmt.Sprintf(
		"claim %s is TOMBSTONED (deprecated, dependent rows stripped) but its row could not be removed; re-run the delete to resume: %v",
		e.ClaimID, e.Err)
}

func (e *PartialDeleteError) Unwrap() error { return e.Err }

// Is lets errors.Is(err, ErrStrippedClaimSurvives) match a partial delete that
// actually left residue. A failed tombstone stripped nothing, so it does not
// match — the claim is intact and the caller can simply retry.
func (e *PartialDeleteError) Is(target error) bool {
	return target == ErrStrippedClaimSurvives && e.Tombstoned
}

// cascadeDeleteClaim removes a claim and every row it owns, tombstone-first.
//
// Sequence:
//
//  1. Read the claim. Absent → nothing to tombstone; the dependent deletes
//     still run (they are idempotent) so a resumed run converges.
//  2. Deprecate it, with [TombstoneReason] on the status_history row. This is
//     the invariant the whole function exists for: from here on, whatever
//     fails, no ACTIVE un-evidenced claim can survive.
//  3. Strip the dependents (relationships, the claim embedding, then the
//     entity links).
//  4. Delete the claim row (which cascades claim_evidence + status_history).
//
// Returns removed=false with a [PartialDeleteError] when step 4 fails: the
// tombstone stands, the partial state is named, and a retry resumes.
//
// The entity unlink in step 3 is not optional bookkeeping. claim_entities has
// a foreign key to claims on every FK-enforcing backend and belongs to the
// entity aggregate, so ClaimRepository.DeleteCascade does not own it and
// cannot remove it. Without the unlink, step 4 aborted on the constraint —
// and because the tombstone and the strips had already committed, `forget` on
// any claim that mentioned an entity left a deprecated, evidence-stripped row
// and returned a PartialDeleteError. Extraction links entities for every claim
// it emits, so that was the common case, not an edge one. Semantic dedupe hit
// the identical constraint from the other direction and was fixed there first.
func cascadeDeleteClaim(ctx context.Context, conn *store.Conn, claimID string) (removed bool, err error) {
	existing, err := conn.Claims.ListByIDs(ctx, []string{claimID})
	if err != nil {
		return false, fmt.Errorf("read claim %s before delete: %w", claimID, err)
	}
	if len(existing) == 1 && existing[0].Status != domain.ClaimStatusDeprecated {
		tomb := existing[0]
		tomb.Status = domain.ClaimStatusDeprecated
		if err := conn.Claims.UpsertWithReasonAs(ctx, []domain.Claim{tomb}, TombstoneReason, domain.SystemUser); err != nil {
			// Nothing has been stripped yet, so the claim is still whole.
			return false, &PartialDeleteError{ClaimID: claimID, Tombstoned: false, Err: err}
		}
	}
	if err := conn.Relationships.DeleteByClaim(ctx, claimID); err != nil {
		return false, fmt.Errorf("delete relationships for %s: %w", claimID, err)
	}
	if err := conn.Embeddings.Delete(ctx, claimID, "claim"); err != nil {
		return false, fmt.Errorf("delete embedding for %s: %w", claimID, err)
	}
	// Idempotent: unlinking nothing is success, so a resumed pass converges.
	if err := conn.Entities.UnlinkClaim(ctx, claimID); err != nil {
		return false, &PartialDeleteError{ClaimID: claimID, Tombstoned: true, Err: fmt.Errorf("unlink entities: %w", err)}
	}
	if err := conn.Claims.DeleteCascade(ctx, claimID); err != nil {
		return false, &PartialDeleteError{ClaimID: claimID, Tombstoned: true, Err: err}
	}
	return true, nil
}

type deleteClaimCascadeInput struct{ ClaimID string }

// deleteClaimCascadeResult is what the executor hands back. Removed=false is a
// SUCCESSFUL execution that left a tombstone: the kernel records the evidence
// row (including partial=true) and the public method turns it into a
// [PartialDeleteError] for the caller. Failing the action instead would drop
// the very audit record that documents the residue.
type deleteClaimCascadeResult struct {
	ClaimID string
	Removed bool
	Cause   string
}

type deleteClaimCascadeExecutor struct{ conn *store.Conn }

// Execute performs the full per-claim delete sequence — tombstone,
// relationships, embedding, then the claim cascade (claim_evidence +
// status_history + the claim row) — as ONE governed action so the destructive
// op is a single entry on the evidence chain rather than several ungoverned
// reaches into storage. Order matters under FK enforcement: edges and the
// embedding (which reference the claim) go before the claim itself.
func (e deleteClaimCascadeExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[deleteClaimCascadeInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("delete_claim_cascade: unexpected input %T", input)
	}
	removed, err := cascadeDeleteClaim(ctx, e.conn, in.ClaimID)
	var partial *PartialDeleteError
	switch {
	case err != nil && errors.As(err, &partial) && partial.Tombstoned:
		// Residue, not a failed action: report it so the evidence chain
		// records the tombstone the operator has to resolve.
		return axidomain.ExecutionResult{
			Data:    deleteClaimCascadeResult{ClaimID: in.ClaimID, Removed: false, Cause: partial.Err.Error()},
			Summary: fmt.Sprintf("tombstoned claim %s (row NOT removed: %v)", in.ClaimID, partial.Err),
		}, ev("mnemos.write.delete_claim_cascade", map[string]any{
			"claim_id": in.ClaimID, "removed": false, "partial": true,
			"cause": partial.Err.Error(),
		}), nil
	case err != nil:
		return axidomain.ExecutionResult{}, nil, err
	}
	return axidomain.ExecutionResult{
		Data:    deleteClaimCascadeResult{ClaimID: in.ClaimID, Removed: removed},
		Summary: fmt.Sprintf("deleted claim %s (relationships + embedding + cascade)", in.ClaimID),
	}, ev("mnemos.write.delete_claim_cascade", map[string]any{
		"claim_id": in.ClaimID, "removed": true, "partial": false,
	}), nil
}

// DeleteClaimCascade deletes a claim and every row it owns
// (relationships touching it, its claim embedding, claim_evidence,
// claim_status_history, the claim row) as one governed, audited action.
//
// The claim is deprecated before any dependent row is stripped, so a failure
// part-way through can never leave an active, un-evidenced belief for recall to
// surface. When the claim row itself survives, the returned error is a
// [PartialDeleteError] (matching errors.Is(err, [ErrStrippedClaimSurvives]))
// naming the tombstoned id; the action is idempotent, so re-running it once the
// fault clears completes the delete.
func (w *Writer) DeleteClaimCascade(ctx context.Context, claimID string) error {
	res, err := dispatch[deleteClaimCascadeResult](ctx, w, actionDeleteClaimCascade, deleteClaimCascadeInput{ClaimID: claimID})
	if err != nil {
		return err
	}
	if !res.Removed {
		return &PartialDeleteError{ClaimID: claimID, Tombstoned: true, Err: errors.New(res.Cause)}
	}
	return nil
}

// --- Delete event cascade ---

type deleteEventCascadeInput struct{ EventID string }

type deleteEventCascadeExecutor struct{ conn *store.Conn }

// Execute cascades an event delete: every claim whose evidence points at
// the event is deleted (tombstone + relationships + embedding + claim
// cascade), then the event's own embedding and the event row go. One governed
// entry for the whole destructive sequence.
//
// Per-claim deletes reuse [cascadeDeleteClaim], so the same tombstone-first
// guarantee holds here: a failure part-way through leaves deprecated rows, not
// active un-evidenced beliefs, and the event row is left in place so a re-run
// can find its remaining claims and resume.
func (e deleteEventCascadeExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[deleteEventCascadeInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("delete_event_cascade: unexpected input %T", input)
	}
	dependent, err := e.conn.Claims.ListByEventIDs(ctx, []string{in.EventID})
	if err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("list dependent claims for %s: %w", in.EventID, err)
	}
	cascaded := 0
	for _, c := range dependent {
		removed, err := cascadeDeleteClaim(ctx, e.conn, c.ID)
		if err != nil {
			return axidomain.ExecutionResult{}, nil, fmt.Errorf("cascade event %s: %w", in.EventID, err)
		}
		if !removed {
			return axidomain.ExecutionResult{}, nil, fmt.Errorf("cascade event %s: claim %s tombstoned but not removed", in.EventID, c.ID)
		}
		cascaded++
	}
	if err := e.conn.Embeddings.Delete(ctx, in.EventID, "event"); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("delete event embedding %s: %w", in.EventID, err)
	}
	if err := e.conn.Events.DeleteByID(ctx, in.EventID); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("delete event %s: %w", in.EventID, err)
	}
	return axidomain.ExecutionResult{
		Data:    cascaded,
		Summary: fmt.Sprintf("deleted event %s; cascaded %d claim(s)", in.EventID, cascaded),
	}, ev("mnemos.write.delete_event_cascade", map[string]any{
		"event_id": in.EventID, "cascaded_claims": cascaded,
	}), nil
}

// DeleteEventCascade deletes an event, its dependent claims (each fully
// cascaded), and the related embeddings as one governed, audited action.
// Returns the number of claims cascaded.
func (w *Writer) DeleteEventCascade(ctx context.Context, eventID string) (int, error) {
	return dispatch[int](ctx, w, actionDeleteEventCascade, deleteEventCascadeInput{EventID: eventID})
}

// --- Delete single event ---

type deleteEventInput struct{ EventID string }

type deleteEventExecutor struct{ conn *store.Conn }

// Execute deletes a single event row and records an evidence row.
func (e deleteEventExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[deleteEventInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("delete_event: unexpected input %T", input)
	}
	if err := e.conn.Events.DeleteByID(ctx, in.EventID); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("delete event %s: %w", in.EventID, err)
	}
	return axidomain.ExecutionResult{
		Data:    in.EventID,
		Summary: fmt.Sprintf("deleted event %s", in.EventID),
	}, ev("mnemos.write.delete_event", map[string]any{
		"event_id": in.EventID,
	}), nil
}

// DeleteEvent removes a single event row by id. Used by the HTTP
// delete-by-run path, which deletes events last (after their claims) so
// a partial failure leaves them referenceable for an idempotent re-run.
func (w *Writer) DeleteEvent(ctx context.Context, eventID string) error {
	_, err := dispatch[string](ctx, w, actionDeleteEvent, deleteEventInput{EventID: eventID})
	return err
}

// --- Reset (purge) ---

// ResetCounts reports what a [Writer.Reset] removed so the operator-facing
// summary reflects what was actually purged.
type ResetCounts struct {
	Claims        int64
	Evidence      int64
	StatusHistory int64
	Relationships int64
	Embeddings    int64
	Events        int64
}

type resetInput struct{ KeepEvents bool }

type resetExecutor struct{ conn *store.Conn }

// Execute reads the pre-purge counts, then deletes all derived memory
// state through the port-typed DeleteAll methods. Order matters under FK
// enforcement: relationships and embeddings (which reference claims /
// events) go first, claims (including claim_evidence) next, events last.
func (e resetExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[resetInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("reset: unexpected input %T", input)
	}
	conn := e.conn
	var counts ResetCounts
	var err error
	if counts.Claims, err = conn.Claims.CountAll(ctx); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("count claims: %w", err)
	}
	evidence, err := conn.Claims.ListAllEvidence(ctx)
	if err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("list claim evidence: %w", err)
	}
	counts.Evidence = int64(len(evidence))
	history, err := conn.Claims.ListAllStatusHistory(ctx)
	if err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("list status history: %w", err)
	}
	counts.StatusHistory = int64(len(history))
	if counts.Relationships, err = conn.Relationships.CountAll(ctx); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("count relationships: %w", err)
	}
	if counts.Embeddings, err = conn.Embeddings.CountAll(ctx); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("count embeddings: %w", err)
	}
	if !in.KeepEvents {
		if counts.Events, err = conn.Events.CountAll(ctx); err != nil {
			return axidomain.ExecutionResult{}, nil, fmt.Errorf("count events: %w", err)
		}
	}

	if err := conn.Relationships.DeleteAll(ctx); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("delete relationships: %w", err)
	}
	if err := conn.Embeddings.DeleteAll(ctx); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("delete embeddings: %w", err)
	}
	if err := conn.Claims.DeleteAll(ctx); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("delete claims: %w", err)
	}
	if !in.KeepEvents {
		if err := conn.Events.DeleteAll(ctx); err != nil {
			return axidomain.ExecutionResult{}, nil, fmt.Errorf("delete events: %w", err)
		}
	}
	return axidomain.ExecutionResult{
		Data:    counts,
		Summary: fmt.Sprintf("reset: purged %d claim(s), %d relationship(s), %d embedding(s)", counts.Claims, counts.Relationships, counts.Embeddings),
	}, ev("mnemos.write.reset", map[string]any{
		"claims": counts.Claims, "relationships": counts.Relationships,
		"embeddings": counts.Embeddings, "events": counts.Events,
		"keep_events": in.KeepEvents,
	}), nil
}

// Reset purges all derived memory state (claims, relationships,
// embeddings, and — unless keepEvents — events) as one governed, audited
// action. Returns the counts removed for the operator-facing summary.
func (w *Writer) Reset(ctx context.Context, keepEvents bool) (ResetCounts, error) {
	return dispatch[ResetCounts](ctx, w, actionReset, resetInput{KeepEvents: keepEvents})
}
