// Package govwrite is the governed daemon-write surface. Every durable
// write the Mnemos daemon performs — the CLI, the HTTP server, the gRPC
// server, the MCP tools, the federation importer, and the git/PR
// ingestors — routes through a [Writer] so the spec non-negotiable
// holds: no delivery adapter reaches a *store.Conn repository directly.
//
// The public-library writes (mnemos.Remember / RememberClaim /
// RememberEvent) already flow through an axi kernel built by
// internal/kernel. The daemon, however, performs writes the narrow
// public Memory surface can't express — pre-built events with custom
// ids (ev_git_<sha>) and structured fields (SchemaVersion,
// SourceInputID), plus whole repositories with no public method
// (Actions, Outcomes, Lessons, Decisions, Playbooks, EntityRels,
// Incidents, Feedback, Embeddings). This package extends the governed
// surface to cover exactly those writes.
//
// # Design
//
// A [Writer] wraps a *store.Conn and an internal/kernel.Governed kernel.
// One axi action + executor is registered per write kind (write-local
// effect; idempotent where the daemon dedups on a stable id). Each
// executor performs the persistence against the wrapped conn and emits
// an axidomain.EvidenceRecord (kind, source, the entity id(s), counts)
// so every governed write leaves a verifiable trail on the kernel
// session's evidence chain.
//
// The public methods on [Writer] take *domain* types only — no axi
// types leak into the signatures the daemon packages call. The rich Go
// input is boxed under a single map key and type-asserted in the
// executor (lossless), mirroring the dispatchWrite pattern in the root
// mnemos package.
package govwrite

import (
	"context"
	"fmt"

	"go.klarlabs.de/axi"
	axidomain "go.klarlabs.de/axi/domain"
	"go.klarlabs.de/bolt"

	"go.klarlabs.de/mnemos/internal/kernel"
	"go.klarlabs.de/mnemos/internal/store"
)

// Source tags every evidence record this package emits so an auditor
// can tell a governed daemon write from a library or MCP write.
const Source = "mnemos.daemon"

// Action names. These map, via kernel.ActionName, to dash-cased axi
// action identifiers (write-<kind>). The effect is always
// EffectWriteLocal (auto-approved, no human gate) but still produces
// the per-session evidence chain + budget. Idempotent actions are the
// ones the daemon dedups on a stable id (events, claims, incidents,
// embeddings, entity edges) — re-running them on the same input does
// not duplicate state because the underlying repository upserts.
const (
	actionWriteEvents        = "write_events"
	actionWriteClaims        = "write_claims"
	actionWriteEvidenceLinks = "write_evidence_links"
	actionWriteRelationships = "write_relationships"
	actionWriteEmbedding     = "write_embedding"
	actionPruneRelationships = "prune_relationships"
	actionDropRelationships  = "drop_relationships"
	actionWriteAction        = "write_action"
	actionWriteOutcome       = "write_outcome"
	actionWriteLesson        = "write_lesson"
	actionWriteDecision      = "write_decision"
	actionWritePlaybook      = "write_playbook"
	actionWriteEntityRels    = "write_entity_rels"
	actionWriteIncident      = "write_incident"
	actionWriteFeedback      = "write_feedback"
	actionWriteArtifacts     = "write_artifacts"
	actionAttachOutcome      = "attach_outcome"
	actionResolveIncident    = "resolve_incident"

	// Claim-lifecycle mutations. These bump status_history, freshness,
	// or validity on an existing claim rather than inserting a new
	// version. They are NOT idempotent the way an upsert-by-id is —
	// MarkVerified increments verify_count on every call — so they are
	// registered with Idempotent:false to keep the audit honest.
	actionMarkVerified = "mark_verified"
	actionSetValidity  = "set_validity"

	// Entity canonicalisation. Merge folds a loser entity into a winner
	// and deletes the loser; it is destructive and non-idempotent.
	actionMergeEntities = "merge_entities"

	// Destructive deletes. Each is ONE governed entry so a delete leaves
	// exactly one auditable record on the evidence chain — a delete with
	// no audit trail is the worst case.
	actionDeleteClaimCascade = "delete_claim_cascade"
	actionDeleteEventCascade = "delete_event_cascade"
	actionDeleteEvent        = "delete_event"
	actionReset              = "reset"
)

// inputPayloadKey is the single map key under which a typed write input
// is boxed onto axi.Invocation.Input. axi's Input is a map[string]any,
// so we stow the rich Go input (domain structs carrying time.Time and
// nested maps) under one key rather than flattening it through a lossy
// JSON round-trip. The executor unboxes via [payload].
const inputPayloadKey = "payload"

// Writer is the governed daemon-write surface. Construct one with [New]
// (which owns the conn lifecycle) or [Wrap] (which borrows a conn the
// caller already owns). Every method dispatches through the embedded
// kernel so the write is effect-gated, evidence-logged, and budgeted.
//
// Writer is safe for concurrent use by multiple goroutines: the kernel
// and the underlying repositories are.
type Writer struct {
	conn    *store.Conn
	kernel  *kernel.Governed
	ownConn bool
}

// actions returns the descriptors for every governed daemon write. One
// source of truth so the plugin registration and the executor bindings
// can't drift.
func actions() []kernel.Action {
	return []kernel.Action{
		{Name: actionWriteEvents, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Append pre-built events with caller-supplied ids and structured fields."},
		{Name: actionWriteClaims, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Upsert pre-built claims, optionally with an audit reason and changed-by."},
		{Name: actionWriteEvidenceLinks, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Upsert claim→event evidence links."},
		{Name: actionWriteRelationships, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Upsert claim relationships (supports/contradicts edges)."},
		{Name: actionPruneRelationships, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Replace the stored relationship set with the given edges, dropping the rest."},
		{Name: actionDropRelationships, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Remove exactly the enumerated relationship edges, retaining everything else."},
		{Name: actionWriteEmbedding, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Upsert a vector embedding for an entity."},
		{Name: actionWriteAction, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Append an operational action record."},
		{Name: actionWriteOutcome, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Append an observed outcome and auto-link its action edges."},
		{Name: actionWriteLesson, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Upsert a synthesised or hand-authored lesson."},
		{Name: actionWriteDecision, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Upsert a decision record."},
		{Name: actionWritePlaybook, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Upsert a synthesised or hand-authored playbook."},
		{Name: actionWriteEntityRels, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Upsert polymorphic cross-entity edges."},
		{Name: actionWriteIncident, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Upsert an incident record."},
		{Name: actionWriteFeedback, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Upsert per-claim feedback state."},
		{Name: actionWriteArtifacts, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Persist a full ingest batch (events, claims, evidence links, relationships)."},
		{Name: actionAttachOutcome, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Attach an outcome to a decision and fire validates/refutes edges."},
		{Name: actionResolveIncident, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Mark an incident resolved."},
		{Name: actionMarkVerified, Effect: axidomain.EffectWriteLocal, Idempotent: false,
			Description: "Bump a claim's last_verified and increment verify_count."},
		{Name: actionSetValidity, Effect: axidomain.EffectWriteLocal, Idempotent: false,
			Description: "Close a claim's validity interval (valid_to)."},
		{Name: actionMergeEntities, Effect: axidomain.EffectWriteLocal, Idempotent: false,
			Description: "Merge a loser entity into a winner and delete the loser."},
		{Name: actionDeleteClaimCascade, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Delete a claim and its owned rows (relationships, embedding, evidence, status history) atomically."},
		{Name: actionDeleteEventCascade, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Delete an event, its dependent claims (cascaded), and embeddings."},
		{Name: actionDeleteEvent, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Delete a single event row by id."},
		{Name: actionReset, Effect: axidomain.EffectWriteLocal, Idempotent: true,
			Description: "Purge all memory state (claims, relationships, embeddings, and optionally events)."},
	}
}

// executors returns the executor binding for every governed daemon
// write, each capturing the supplied conn. Keyed by kernel.ExecutorRef.
func executors(conn *store.Conn) map[string]axidomain.ActionExecutor {
	return map[string]axidomain.ActionExecutor{
		kernel.ExecutorRef(actionWriteEvents):        eventsExecutor{conn: conn},
		kernel.ExecutorRef(actionWriteClaims):        claimsExecutor{conn: conn},
		kernel.ExecutorRef(actionWriteEvidenceLinks): evidenceLinksExecutor{conn: conn},
		kernel.ExecutorRef(actionWriteRelationships): relationshipsExecutor{conn: conn},
		kernel.ExecutorRef(actionPruneRelationships): pruneRelationshipsExecutor{conn: conn},
		kernel.ExecutorRef(actionDropRelationships):  dropRelationshipsExecutor{conn: conn},
		kernel.ExecutorRef(actionWriteEmbedding):     embeddingExecutor{conn: conn},
		kernel.ExecutorRef(actionWriteAction):        actionExecutor{conn: conn},
		kernel.ExecutorRef(actionWriteOutcome):       outcomeExecutor{conn: conn},
		kernel.ExecutorRef(actionWriteLesson):        lessonExecutor{conn: conn},
		kernel.ExecutorRef(actionWriteDecision):      decisionExecutor{conn: conn},
		kernel.ExecutorRef(actionWritePlaybook):      playbookExecutor{conn: conn},
		kernel.ExecutorRef(actionWriteEntityRels):    entityRelsExecutor{conn: conn},
		kernel.ExecutorRef(actionWriteIncident):      incidentExecutor{conn: conn},
		kernel.ExecutorRef(actionWriteFeedback):      feedbackExecutor{conn: conn},
		kernel.ExecutorRef(actionWriteArtifacts):     artifactsExecutor{conn: conn},
		kernel.ExecutorRef(actionAttachOutcome):      attachOutcomeExecutor{conn: conn},
		kernel.ExecutorRef(actionResolveIncident):    resolveIncidentExecutor{conn: conn},
		kernel.ExecutorRef(actionMarkVerified):       markVerifiedExecutor{conn: conn},
		kernel.ExecutorRef(actionSetValidity):        setValidityExecutor{conn: conn},
		kernel.ExecutorRef(actionMergeEntities):      mergeEntitiesExecutor{conn: conn},
		kernel.ExecutorRef(actionDeleteClaimCascade): deleteClaimCascadeExecutor{conn: conn},
		kernel.ExecutorRef(actionDeleteEventCascade): deleteEventCascadeExecutor{conn: conn},
		kernel.ExecutorRef(actionDeleteEvent):        deleteEventExecutor{conn: conn},
		kernel.ExecutorRef(actionReset):              resetExecutor{conn: conn},
	}
}

// build assembles the governed kernel for a conn.
func build(conn *store.Conn, logger *bolt.Logger) (*kernel.Governed, error) {
	return kernel.Build(logger, actions(), executors(conn), kernel.BudgetFromEnv(), "")
}

// New opens a store connection by DSN and wraps it in a governed
// [Writer] that OWNS the conn: [Writer.Close] closes both the kernel
// sink and the connection. Use this from short-lived CLI commands that
// previously did `conn, _ := openConn(ctx); defer closeConn(conn)`.
//
// logger may be nil; the kernel substitutes a discarding logger.
func New(ctx context.Context, dsn string, logger *bolt.Logger) (*Writer, error) {
	conn, err := store.Open(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("govwrite: open store: %w", err)
	}
	k, err := build(conn, logger)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("govwrite: build kernel: %w", err)
	}
	return &Writer{conn: conn, kernel: k, ownConn: true}, nil
}

// Wrap builds a [Writer] around a *store.Conn the caller already owns.
// The Writer does NOT close the conn; the caller's existing
// defer/close discipline stays in force. Use this from long-lived
// daemons (the HTTP and gRPC servers) that hold a shared conn.
//
// logger may be nil; the kernel substitutes a discarding logger.
func Wrap(conn *store.Conn, logger *bolt.Logger) (*Writer, error) {
	if conn == nil {
		return nil, fmt.Errorf("govwrite: nil conn")
	}
	k, err := build(conn, logger)
	if err != nil {
		return nil, fmt.Errorf("govwrite: build kernel: %w", err)
	}
	return &Writer{conn: conn, kernel: k}, nil
}

// Conn exposes the wrapped connection for the *read* paths the daemon
// still performs directly (lists, gets, counts). Reads do not bypass
// governance — governance is about writes — so handing the conn out for
// reads is intentional and keeps the call sites terse.
func (w *Writer) Conn() *store.Conn { return w.conn }

// LastSession returns the most-recently-saved kernel execution session,
// or nil if no write has run yet. Exposed so an auditor (or a test) can
// recover the evidence chain of the last governed write without holding
// onto the per-call session id. The session is the same one axi Save()s
// on every terminal path.
//
// Concurrency note: LastSession is LAST-WINS. A [Writer] built with
// [Wrap] is safe to share across request goroutines (the HTTP and gRPC
// servers do exactly that), but under concurrent writes LastSession
// returns whichever session happened to Save most recently — NOT the
// session of the caller's own write. Callers that need the evidence of
// THEIR specific write (per-request audit) must not rely on LastSession;
// it is a best-effort "last governed write on this Writer" handle, useful
// for single-threaded CLI commands and tests. Per-request evidence is
// carried on the kernel's durable evidence sink instead.
func (w *Writer) LastSession() *axidomain.ExecutionSession {
	if w == nil {
		return nil
	}
	return w.kernel.LastSession()
}

// Close releases the kernel's evidence sink and, when the Writer owns
// the conn (constructed via [New]), the underlying store connection.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	kerr := w.kernel.Close()
	if w.ownConn && w.conn != nil {
		if cerr := w.conn.Close(); cerr != nil {
			return cerr
		}
	}
	return kerr
}

// dispatch runs a governed write action through the kernel and returns
// the typed result. The executor performs the persistence; this is the
// single chokepoint every public method funnels through, so no write
// can skip the evidence chain. A pre-cancelled ctx, a business failure
// in the executor, or a budget breach all surface as an error here.
func dispatch[Out any](ctx context.Context, w *Writer, action string, input any) (Out, error) {
	var zero Out
	res, err := w.kernel.Execute(ctx, axi.Invocation{
		Action: kernel.ActionName(action),
		Input:  map[string]any{inputPayloadKey: input},
	})
	if err != nil {
		return zero, fmt.Errorf("govwrite: %s: %w", action, err)
	}
	if res.Failure != nil {
		return zero, fmt.Errorf("govwrite: %s: %s: %s", action, res.Failure.Code, res.Failure.Message)
	}
	if res.Result == nil {
		return zero, fmt.Errorf("govwrite: %s: kernel returned no result", action)
	}
	if typed, ok := res.Result.Data.(Out); ok {
		return typed, nil
	}
	return zero, fmt.Errorf("govwrite: %s: unexpected result type %T", action, res.Result.Data)
}

// payload unboxes the typed input an executor received from [dispatch].
// Returns ok=false when the input isn't the expected boxed shape.
func payload[In any](input any) (In, bool) {
	var zero In
	m, ok := input.(map[string]any)
	if !ok {
		return zero, false
	}
	typed, ok := m[inputPayloadKey].(In)
	return typed, ok
}
