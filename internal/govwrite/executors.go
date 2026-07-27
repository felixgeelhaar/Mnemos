package govwrite

import (
	"context"
	"fmt"
	"time"

	axidomain "go.klarlabs.de/axi/domain"

	"go.klarlabs.de/mnemos/internal/autoedge"
	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/pipeline"
	"go.klarlabs.de/mnemos/internal/store"
)

// The executors below carry the persistence logic for every governed
// daemon write. They run inside the axi kernel — the only place this
// package reaches conn.<Repo>.Append/Upsert. Each returns an
// []axidomain.EvidenceRecord describing the write (ids + counts) so the
// session's evidence chain is populated and an auditor can verify what
// landed.

// writeCount is the result every batch executor returns: how many rows
// the call accepted. The public methods surface it so callers that
// reported "accepted=N" keep doing so.
type writeCount struct{ Accepted int }

// ev builds the single structured evidence record for a daemon write.
func ev(kind string, value map[string]any) []axidomain.EvidenceRecord {
	return []axidomain.EvidenceRecord{{Kind: kind, Source: Source, Value: value}}
}

// --- Events ---

type eventsInput struct{ Events []domain.Event }

type eventsExecutor struct{ conn *store.Conn }

// Execute appends the pre-built events and records an evidence row.
func (e eventsExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[eventsInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("write_events: unexpected input %T", input)
	}
	ids := make([]string, 0, len(in.Events))
	for _, evt := range in.Events {
		if err := e.conn.Events.Append(ctx, evt); err != nil {
			return axidomain.ExecutionResult{}, nil, fmt.Errorf("append event %s: %w", evt.ID, err)
		}
		ids = append(ids, evt.ID)
	}
	return axidomain.ExecutionResult{
		Data:    writeCount{Accepted: len(ids)},
		Summary: fmt.Sprintf("appended %d event(s)", len(ids)),
	}, ev("mnemos.write.events", map[string]any{"count": len(ids), "event_ids": ids}), nil
}

// Events appends pre-built events with caller-supplied ids and
// structured fields (SchemaVersion, SourceInputID, Metadata) preserved
// losslessly. Returns the number accepted.
func (w *Writer) Events(ctx context.Context, events []domain.Event) (int, error) {
	out, err := dispatch[writeCount](ctx, w, actionWriteEvents, eventsInput{Events: events})
	return out.Accepted, err
}

// --- Claims ---

// ClaimReason carries the optional audit attribution for a claim
// upsert. When Reason is set the executor routes through
// UpsertWithReason / UpsertWithReasonAs so the claim status_history
// records why and by whom — the federation and agent-edit paths rely on
// this.
type ClaimReason struct {
	Reason    string
	ChangedBy string
}

type claimsInput struct {
	Claims []domain.Claim
	Reason ClaimReason
}

type claimsExecutor struct{ conn *store.Conn }

// Execute upserts the claims (optionally with an audit reason) and records an evidence row.
func (e claimsExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[claimsInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("write_claims: unexpected input %T", input)
	}
	var err error
	switch {
	case in.Reason.ChangedBy != "":
		err = e.conn.Claims.UpsertWithReasonAs(ctx, in.Claims, in.Reason.Reason, in.Reason.ChangedBy)
	case in.Reason.Reason != "":
		err = e.conn.Claims.UpsertWithReason(ctx, in.Claims, in.Reason.Reason)
	default:
		err = e.conn.Claims.Upsert(ctx, in.Claims)
	}
	if err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("upsert claims: %w", err)
	}
	ids := make([]string, 0, len(in.Claims))
	for _, c := range in.Claims {
		ids = append(ids, c.ID)
	}
	return axidomain.ExecutionResult{
			Data:    writeCount{Accepted: len(in.Claims)},
			Summary: fmt.Sprintf("upserted %d claim(s)", len(in.Claims)),
		}, ev("mnemos.write.claims", map[string]any{
			"count": len(in.Claims), "claim_ids": ids,
			"reason": in.Reason.Reason, "changed_by": in.Reason.ChangedBy,
		}), nil
}

// Claims upserts pre-built claims. Pass a non-zero ClaimReason to record
// audit attribution (status_history reason / changed_by). Returns the
// number accepted.
func (w *Writer) Claims(ctx context.Context, claims []domain.Claim, reason ClaimReason) (int, error) {
	out, err := dispatch[writeCount](ctx, w, actionWriteClaims, claimsInput{Claims: claims, Reason: reason})
	return out.Accepted, err
}

// --- Evidence links ---

type evidenceLinksInput struct{ Links []domain.ClaimEvidence }

type evidenceLinksExecutor struct{ conn *store.Conn }

// Execute upserts the claim→event evidence links and records an evidence row.
func (e evidenceLinksExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[evidenceLinksInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("write_evidence_links: unexpected input %T", input)
	}
	if err := e.conn.Claims.UpsertEvidence(ctx, in.Links); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("upsert evidence links: %w", err)
	}
	return axidomain.ExecutionResult{
		Data:    writeCount{Accepted: len(in.Links)},
		Summary: fmt.Sprintf("linked %d evidence row(s)", len(in.Links)),
	}, ev("mnemos.write.evidence_links", map[string]any{"count": len(in.Links)}), nil
}

// EvidenceLinks upserts claim→event evidence links. Returns the number
// accepted.
func (w *Writer) EvidenceLinks(ctx context.Context, links []domain.ClaimEvidence) (int, error) {
	out, err := dispatch[writeCount](ctx, w, actionWriteEvidenceLinks, evidenceLinksInput{Links: links})
	return out.Accepted, err
}

// --- Relationships ---

type relationshipsInput struct{ Relationships []domain.Relationship }

type relationshipsExecutor struct{ conn *store.Conn }

// Execute upserts the relationships and records an evidence row.
func (e relationshipsExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[relationshipsInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("write_relationships: unexpected input %T", input)
	}
	if err := e.conn.Relationships.Upsert(ctx, in.Relationships); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("upsert relationships: %w", err)
	}
	return axidomain.ExecutionResult{
		Data:    writeCount{Accepted: len(in.Relationships)},
		Summary: fmt.Sprintf("upserted %d relationship(s)", len(in.Relationships)),
	}, ev("mnemos.write.relationships", map[string]any{"count": len(in.Relationships)}), nil
}

// Relationships upserts claim relationships (supports/contradicts).
// Returns the number accepted.
func (w *Writer) Relationships(ctx context.Context, rels []domain.Relationship) (int, error) {
	out, err := dispatch[writeCount](ctx, w, actionWriteRelationships, relationshipsInput{Relationships: rels})
	return out.Accepted, err
}

// --- Embeddings ---

type embeddingInput struct {
	EntityID   string
	EntityType string
	Vector     []float32
	Model      string
	CreatedBy  string
}

type embeddingExecutor struct{ conn *store.Conn }

// Execute upserts the embedding and records an evidence row.
func (e embeddingExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[embeddingInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("write_embedding: unexpected input %T", input)
	}
	if err := e.conn.Embeddings.Upsert(ctx, in.EntityID, in.EntityType, in.Vector, in.Model, in.CreatedBy); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("upsert embedding: %w", err)
	}
	return axidomain.ExecutionResult{
			Data:    writeCount{Accepted: 1},
			Summary: fmt.Sprintf("upserted embedding for %s/%s", in.EntityType, in.EntityID),
		}, ev("mnemos.write.embedding", map[string]any{
			"entity_id": in.EntityID, "entity_type": in.EntityType,
			"model": in.Model, "dimensions": len(in.Vector),
		}), nil
}

// Embedding upserts a single vector embedding for an entity (derived
// state). Routed through the kernel so even derived writes leave an
// evidence trail.
func (w *Writer) Embedding(ctx context.Context, entityID, entityType string, vector []float32, model, createdBy string) error {
	_, err := dispatch[writeCount](ctx, w, actionWriteEmbedding, embeddingInput{
		EntityID: entityID, EntityType: entityType, Vector: vector, Model: model, CreatedBy: createdBy,
	})
	return err
}

// --- Action ---

type actionInput struct{ Action domain.Action }

type actionExecutor struct{ conn *store.Conn }

// Execute appends the action and records an evidence row.
func (e actionExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[actionInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("write_action: unexpected input %T", input)
	}
	if err := e.conn.Actions.Append(ctx, in.Action); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("append action: %w", err)
	}
	return axidomain.ExecutionResult{
			Data:    in.Action.ID,
			Summary: fmt.Sprintf("appended action %s", in.Action.ID),
		}, ev("mnemos.write.action", map[string]any{
			"action_id": in.Action.ID, "kind": string(in.Action.Kind), "subject": in.Action.Subject,
		}), nil
}

// Action appends an operational action record. Returns the action id.
func (w *Writer) Action(ctx context.Context, action domain.Action) (string, error) {
	return dispatch[string](ctx, w, actionWriteAction, actionInput{Action: action})
}

// --- Outcome ---

type outcomeInput struct {
	Outcome  domain.Outcome
	AutoEdge bool
}

type outcomeExecutor struct{ conn *store.Conn }

// Execute appends the outcome (and optional auto-edges) and records an evidence row.
func (e outcomeExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[outcomeInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("write_outcome: unexpected input %T", input)
	}
	if err := e.conn.Outcomes.Append(ctx, in.Outcome); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("append outcome: %w", err)
	}
	edges := 0
	if in.AutoEdge {
		if err := autoedge.OnOutcomeAppended(ctx, e.conn.EntityRels, in.Outcome, in.Outcome.CreatedBy); err != nil {
			return axidomain.ExecutionResult{}, nil, fmt.Errorf("auto-link action_of edges: %w", err)
		}
		if in.Outcome.ActionID != "" {
			edges = 2
		}
	}
	return axidomain.ExecutionResult{
			Data:    in.Outcome.ID,
			Summary: fmt.Sprintf("appended outcome %s", in.Outcome.ID),
		}, ev("mnemos.write.outcome", map[string]any{
			"outcome_id": in.Outcome.ID, "action_id": in.Outcome.ActionID,
			"result": string(in.Outcome.Result), "auto_edges": edges,
		}), nil
}

// Outcome appends an observed outcome. When autoEdge is true the
// executor also fires the action_of/outcome_of edges via autoedge
// (matching the CLI/MCP behaviour). Returns the outcome id.
func (w *Writer) Outcome(ctx context.Context, outcome domain.Outcome, autoEdge bool) (string, error) {
	return dispatch[string](ctx, w, actionWriteOutcome, outcomeInput{Outcome: outcome, AutoEdge: autoEdge})
}

// --- Lesson ---

type lessonInput struct{ Lesson domain.Lesson }

type lessonExecutor struct{ conn *store.Conn }

// Execute upserts the lesson and records an evidence row.
func (e lessonExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[lessonInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("write_lesson: unexpected input %T", input)
	}
	if err := e.conn.Lessons.Append(ctx, in.Lesson); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("append lesson: %w", err)
	}
	return axidomain.ExecutionResult{
			Data:    in.Lesson.ID,
			Summary: fmt.Sprintf("upserted lesson %s", in.Lesson.ID),
		}, ev("mnemos.write.lesson", map[string]any{
			"lesson_id": in.Lesson.ID, "source": in.Lesson.Source,
		}), nil
}

// Lesson upserts a lesson. Returns the lesson id.
func (w *Writer) Lesson(ctx context.Context, lesson domain.Lesson) (string, error) {
	return dispatch[string](ctx, w, actionWriteLesson, lessonInput{Lesson: lesson})
}

// --- Decision ---

type decisionInput struct{ Decision domain.Decision }

type decisionExecutor struct{ conn *store.Conn }

// Execute upserts the decision and records an evidence row.
func (e decisionExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[decisionInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("write_decision: unexpected input %T", input)
	}
	if err := e.conn.Decisions.Append(ctx, in.Decision); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("append decision: %w", err)
	}
	return axidomain.ExecutionResult{
			Data:    in.Decision.ID,
			Summary: fmt.Sprintf("upserted decision %s", in.Decision.ID),
		}, ev("mnemos.write.decision", map[string]any{
			"decision_id": in.Decision.ID, "risk_level": string(in.Decision.RiskLevel),
		}), nil
}

// Decision upserts a decision record. Returns the decision id.
func (w *Writer) Decision(ctx context.Context, decision domain.Decision) (string, error) {
	return dispatch[string](ctx, w, actionWriteDecision, decisionInput{Decision: decision})
}

// --- Playbook ---

type playbookInput struct{ Playbook domain.Playbook }

type playbookExecutor struct{ conn *store.Conn }

// Execute upserts the playbook and records an evidence row.
func (e playbookExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[playbookInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("write_playbook: unexpected input %T", input)
	}
	if err := e.conn.Playbooks.Append(ctx, in.Playbook); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("append playbook: %w", err)
	}
	return axidomain.ExecutionResult{
			Data:    in.Playbook.ID,
			Summary: fmt.Sprintf("upserted playbook %s", in.Playbook.ID),
		}, ev("mnemos.write.playbook", map[string]any{
			"playbook_id": in.Playbook.ID, "source": in.Playbook.Source,
		}), nil
}

// Playbook upserts a playbook. Returns the playbook id.
func (w *Writer) Playbook(ctx context.Context, playbook domain.Playbook) (string, error) {
	return dispatch[string](ctx, w, actionWritePlaybook, playbookInput{Playbook: playbook})
}

// --- Entity relationships ---

type entityRelsInput struct{ Edges []domain.EntityRelationship }

type entityRelsExecutor struct{ conn *store.Conn }

// Execute upserts the cross-entity edges and records an evidence row.
func (e entityRelsExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[entityRelsInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("write_entity_rels: unexpected input %T", input)
	}
	if err := e.conn.EntityRels.Upsert(ctx, in.Edges); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("upsert entity_relationships: %w", err)
	}
	return axidomain.ExecutionResult{
		Data:    writeCount{Accepted: len(in.Edges)},
		Summary: fmt.Sprintf("upserted %d entity edge(s)", len(in.Edges)),
	}, ev("mnemos.write.entity_rels", map[string]any{"count": len(in.Edges)}), nil
}

// EntityRels upserts polymorphic cross-entity edges. Returns the number
// accepted.
func (w *Writer) EntityRels(ctx context.Context, edges []domain.EntityRelationship) (int, error) {
	out, err := dispatch[writeCount](ctx, w, actionWriteEntityRels, entityRelsInput{Edges: edges})
	return out.Accepted, err
}

// --- Incident ---

type incidentInput struct{ Incident domain.Incident }

type incidentExecutor struct{ conn *store.Conn }

// Execute upserts the incident and records an evidence row.
func (e incidentExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[incidentInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("write_incident: unexpected input %T", input)
	}
	if err := e.conn.Incidents.Upsert(ctx, in.Incident); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("upsert incident: %w", err)
	}
	return axidomain.ExecutionResult{
			Data:    in.Incident.ID,
			Summary: fmt.Sprintf("upserted incident %s", in.Incident.ID),
		}, ev("mnemos.write.incident", map[string]any{
			"incident_id": in.Incident.ID, "severity": string(in.Incident.Severity),
			"status": string(in.Incident.Status),
		}), nil
}

// Incident upserts an incident record. Returns the incident id.
func (w *Writer) Incident(ctx context.Context, incident domain.Incident) (string, error) {
	return dispatch[string](ctx, w, actionWriteIncident, incidentInput{Incident: incident})
}

// --- Feedback ---

type feedbackInput struct{ State domain.ClaimFeedback }

type feedbackExecutor struct{ conn *store.Conn }

// Execute upserts the per-claim feedback state and records an evidence row.
func (e feedbackExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[feedbackInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("write_feedback: unexpected input %T", input)
	}
	if err := e.conn.Feedback.Upsert(ctx, in.State); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("upsert feedback: %w", err)
	}
	return axidomain.ExecutionResult{
			Data:    in.State.ClaimID,
			Summary: fmt.Sprintf("upserted feedback for claim %s", in.State.ClaimID),
		}, ev("mnemos.write.feedback", map[string]any{
			"claim_id": in.State.ClaimID,
		}), nil
}

// Feedback upserts per-claim feedback state. Returns the claim id.
func (w *Writer) Feedback(ctx context.Context, state domain.ClaimFeedback) (string, error) {
	return dispatch[string](ctx, w, actionWriteFeedback, feedbackInput{State: state})
}

// --- Artifacts (full ingest batch) ---

type artifactsInput struct {
	Events        []domain.Event
	Claims        []domain.Claim
	Links         []domain.ClaimEvidence
	Relationships []domain.Relationship
}

type artifactsExecutor struct{ conn *store.Conn }

// Execute persists the full ingest batch and records an evidence row.
func (e artifactsExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[artifactsInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("write_artifacts: unexpected input %T", input)
	}
	if err := pipeline.PersistArtifacts(ctx, e.conn, in.Events, in.Claims, in.Links, in.Relationships); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("persist artifacts: %w", err)
	}
	return axidomain.ExecutionResult{
			Data: writeCount{Accepted: len(in.Events)},
			Summary: fmt.Sprintf("persisted events=%d claims=%d links=%d rels=%d",
				len(in.Events), len(in.Claims), len(in.Links), len(in.Relationships)),
		}, ev("mnemos.write.artifacts", map[string]any{
			"events": len(in.Events), "claims": len(in.Claims),
			"links": len(in.Links), "relationships": len(in.Relationships),
		}), nil
}

// Artifacts persists a full ingest batch (events, claims, evidence
// links, relationships) through pipeline.PersistArtifacts. This is the
// governed replacement for direct pipeline.PersistArtifacts calls in
// the git/PR/autoingest ingestors. Returns the number of events.
func (w *Writer) Artifacts(ctx context.Context, events []domain.Event, claims []domain.Claim, links []domain.ClaimEvidence, rels []domain.Relationship) (int, error) {
	out, err := dispatch[writeCount](ctx, w, actionWriteArtifacts, artifactsInput{
		Events: events, Claims: claims, Links: links, Relationships: rels,
	})
	return out.Accepted, err
}

// --- Decision: attach outcome (link table + autoedge) ---

type attachOutcomeInput struct {
	DecisionID string
	OutcomeID  string
	Actor      string
}

type attachOutcomeExecutor struct{ conn *store.Conn }

// Execute attaches the outcome to the decision and records an evidence row.
func (e attachOutcomeExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[attachOutcomeInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("attach_outcome: unexpected input %T", input)
	}
	if err := e.conn.Decisions.AttachOutcome(ctx, in.DecisionID, in.OutcomeID); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("attach outcome: %w", err)
	}
	if err := autoedge.OnDecisionOutcomeAttached(ctx, e.conn.Decisions, e.conn.Outcomes, e.conn.EntityRels, in.DecisionID, in.OutcomeID, in.Actor); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("auto-link validates/refutes edges: %w", err)
	}
	return axidomain.ExecutionResult{
			Data:    in.DecisionID,
			Summary: fmt.Sprintf("attached outcome %s to decision %s", in.OutcomeID, in.DecisionID),
		}, ev("mnemos.write.attach_outcome", map[string]any{
			"decision_id": in.DecisionID, "outcome_id": in.OutcomeID,
		}), nil
}

// AttachOutcome attaches an outcome to a decision and fires the
// validates/refutes edges via autoedge.
func (w *Writer) AttachOutcome(ctx context.Context, decisionID, outcomeID, actor string) error {
	_, err := dispatch[string](ctx, w, actionAttachOutcome, attachOutcomeInput{
		DecisionID: decisionID, OutcomeID: outcomeID, Actor: actor,
	})
	return err
}

// --- Incident: resolve ---

type resolveIncidentInput struct {
	IncidentID string
	ResolvedAt time.Time
}

type resolveIncidentExecutor struct{ conn *store.Conn }

// Execute marks the incident resolved and records an evidence row.
func (e resolveIncidentExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[resolveIncidentInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("resolve_incident: unexpected input %T", input)
	}
	if err := e.conn.Incidents.Resolve(ctx, in.IncidentID, in.ResolvedAt); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("resolve incident: %w", err)
	}
	return axidomain.ExecutionResult{
		Data:    in.IncidentID,
		Summary: fmt.Sprintf("resolved incident %s", in.IncidentID),
	}, ev("mnemos.write.resolve_incident", map[string]any{"incident_id": in.IncidentID}), nil
}

// ResolveIncident marks an incident resolved at the given time.
func (w *Writer) ResolveIncident(ctx context.Context, incidentID string, resolvedAt time.Time) error {
	_, err := dispatch[string](ctx, w, actionResolveIncident, resolveIncidentInput{
		IncidentID: incidentID, ResolvedAt: resolvedAt,
	})
	return err
}

// --- Relationship pruning ---

type pruneRelationshipsInput struct {
	Keep    []domain.Relationship
	Dropped int
}

type pruneRelationshipsExecutor struct{ conn *store.Conn }

// Execute replaces the whole relationship set with Keep.
//
// Relationships are derived state, but "derived" does not mean "unaudited":
// dropping tens of thousands of edges is exactly the kind of write the kernel
// exists to record, so this routes through it like every other mutation rather
// than reaching past it to the repository. The architectural fitness test
// (TestNoBypass_DeliveryAdaptersDoNotReachStorage) enforces that.
func (e pruneRelationshipsExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[pruneRelationshipsInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("prune_relationships: unexpected input %T", input)
	}
	if err := e.conn.Relationships.DeleteAll(ctx); err != nil {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("clear relationships: %w", err)
	}
	if len(in.Keep) > 0 {
		if err := e.conn.Relationships.Upsert(ctx, in.Keep); err != nil {
			// The set is now empty and the restore failed. Say so loudly: the
			// caller's backup is the recovery path.
			return axidomain.ExecutionResult{}, nil, fmt.Errorf(
				"restore surviving relationships (the relationship table is now EMPTY; restore from backup): %w", err)
		}
	}
	return axidomain.ExecutionResult{
			Data:    writeCount{Accepted: len(in.Keep)},
			Summary: fmt.Sprintf("pruned %d relationship(s), retained %d", in.Dropped, len(in.Keep)),
		}, ev("mnemos.write.relationships.prune", map[string]any{
			"dropped": in.Dropped, "retained": len(in.Keep),
		}), nil
}

// PruneRelationships replaces the stored relationship set with keep, dropping
// everything else. dropped is recorded on the evidence row for the audit trail.
//
// Deprecated for snapshot-based pruners: "everything not in keep" is a
// predicate evaluated against the CALLER's snapshot, so any edge a concurrent
// writer inserts between the caller's read and this write is silently deleted
// as collateral. Use [Writer.DropRelationships], which enumerates the edges to
// remove instead of the edges to retain. Kept for callers that genuinely mean
// "replace the whole set".
func (w *Writer) PruneRelationships(ctx context.Context, keep []domain.Relationship, dropped int) (int, error) {
	out, err := dispatch[writeCount](ctx, w, actionPruneRelationships, pruneRelationshipsInput{Keep: keep, Dropped: dropped})
	return out.Accepted, err
}

// --- Relationship dropping (enumerated, snapshot-safe) ---

type dropRelationshipsInput struct{ Drop []domain.Relationship }

type dropRelationshipsExecutor struct{ conn *store.Conn }

// edgeKey identifies a relationship by its CONTENT — the tuple the storage
// layer treats as unique — rather than by its row id, which differs between two
// detections of the same edge.
func edgeKey(r domain.Relationship) [3]string {
	return [3]string{string(r.Type), r.FromClaimID, r.ToClaimID}
}

// Execute removes exactly the enumerated edges and leaves everything else,
// including edges that did not exist when the caller took its snapshot.
//
// Two separate things make that true, and both are needed:
//
//  1. The predicate is an enumeration, not a complement. A pruner reads the
//     relationship set, spends however long it takes to re-derive each edge
//     against the current detectors, and only then writes. Expressed as "keep
//     this set", the write deletes every edge a concurrent `relate` or capture
//     inserted meanwhile: the snapshot predates them, so they fall outside keep
//     and are destroyed as "stale" without ever having been evaluated.
//     Expressed as "drop these edges", they cannot be — an edge absent from the
//     snapshot is absent from the drop list.
//
//  2. The delete is scoped to the claims the drop list actually touches, and
//     each one's surviving edges are re-read at write time. The repository has
//     no delete-this-edge primitive, only DeleteAll and DeleteByClaim, and
//     DeleteAll+restore reopens the whole race at the storage layer: everything
//     a writer commits between the clear and the restore is lost regardless of
//     how careful the predicate was. Working per affected claim shrinks the
//     exposure from "the entire table" to "edges on this one claim, during one
//     delete+restore" — and leaves edges on untouched claims, which is what a
//     concurrent capture run is writing, provably alone.
//
// Cost is one list + one delete + one upsert per affected claim rather than a
// single table sweep. A prune is a rare maintenance pass; paying for it there
// is cheaper than losing edges. An exact per-edge delete in the repository
// would remove the residual window entirely.
func (e dropRelationshipsExecutor) Execute(ctx context.Context, input any, _ axidomain.CapabilityInvoker) (axidomain.ExecutionResult, []axidomain.EvidenceRecord, error) {
	in, ok := payload[dropRelationshipsInput](input)
	if !ok {
		return axidomain.ExecutionResult{}, nil, fmt.Errorf("drop_relationships: unexpected input %T", input)
	}
	if len(in.Drop) == 0 {
		return axidomain.ExecutionResult{
			Data:    writeCount{Accepted: 0},
			Summary: "dropped 0 relationship(s)",
		}, ev("mnemos.write.relationships.drop", map[string]any{"dropped": 0, "retained": 0}), nil
	}

	doomed := make(map[[3]string]struct{}, len(in.Drop))
	affected := make([]string, 0, len(in.Drop))
	seen := make(map[string]struct{}, len(in.Drop))
	for _, r := range in.Drop {
		doomed[edgeKey(r)] = struct{}{}
		// The source endpoint alone is enough: DeleteByClaim matches an edge on
		// either endpoint, so every doomed edge is reachable from its own
		// FromClaimID. Visiting the target too would only widen the blast radius.
		if _, dup := seen[r.FromClaimID]; !dup {
			seen[r.FromClaimID] = struct{}{}
			affected = append(affected, r.FromClaimID)
		}
	}

	dropped := 0
	retainedEdges := map[[3]string]struct{}{}
	for _, claimID := range affected {
		// Read at WRITE time, per claim: the restore then carries whatever
		// landed on this claim during the pruner's scan.
		edges, err := e.conn.Relationships.ListByClaim(ctx, claimID)
		if err != nil {
			return axidomain.ExecutionResult{}, nil, fmt.Errorf("read relationships for %s: %w", claimID, err)
		}
		keep := make([]domain.Relationship, 0, len(edges))
		for _, r := range edges {
			if _, kill := doomed[edgeKey(r)]; kill {
				continue
			}
			keep = append(keep, r)
		}
		for _, r := range keep {
			retainedEdges[edgeKey(r)] = struct{}{}
		}
		if len(keep) == len(edges) {
			// Nothing doomed left on this claim — a resumed run, or another
			// pruner got there first. Do not touch it.
			continue
		}
		if err := e.conn.Relationships.DeleteByClaim(ctx, claimID); err != nil {
			return axidomain.ExecutionResult{}, nil, fmt.Errorf("delete relationships for %s: %w", claimID, err)
		}
		if len(keep) > 0 {
			if err := e.conn.Relationships.Upsert(ctx, keep); err != nil {
				// This claim's edges are gone and the restore failed. Say so
				// loudly, naming the claim, so the damage is bounded and locatable.
				return axidomain.ExecutionResult{}, nil, fmt.Errorf(
					"restore surviving relationships for claim %s (its edges are now MISSING): %w", claimID, err)
			}
		}
		dropped += len(edges) - len(keep)
	}
	retained := len(retainedEdges)

	return axidomain.ExecutionResult{
			Data:    writeCount{Accepted: retained},
			Summary: fmt.Sprintf("dropped %d relationship(s) across %d claim(s), retained %d", dropped, len(affected), retained),
		}, ev("mnemos.write.relationships.drop", map[string]any{
			"dropped": dropped, "retained": retained, "claims_touched": len(affected),
		}), nil
}

// DropRelationships removes exactly the enumerated edges, matched on content
// (type + endpoints), and retains everything else — including edges written
// after the caller built its list. Returns the number of edges retained across
// the claims it touched.
//
// This is the snapshot-safe counterpart to [Writer.PruneRelationships]: a
// pruner that decides from a stale read can delete only what it actually saw
// and judged, never a concurrent writer's new edge. Edges on claims the drop
// list does not name are never read, deleted, or rewritten.
func (w *Writer) DropRelationships(ctx context.Context, drop []domain.Relationship) (int, error) {
	out, err := dispatch[writeCount](ctx, w, actionDropRelationships, dropRelationshipsInput{Drop: drop})
	return out.Accepted, err
}
