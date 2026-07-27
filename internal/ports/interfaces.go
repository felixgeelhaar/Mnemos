package ports

import (
	"context"
	"errors"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// ErrVectorSearchUnavailable is returned by an EventVectorSearcher
// implementation whose backend cannot serve a native vector search — the
// pgvector `vector` type is not installed, or the accelerator column has
// not been provisioned. Callers treat it as a soft signal to fall back to
// the in-Go cosine / token-overlap ranking path, NOT as a hard failure.
var ErrVectorSearchUnavailable = errors.New("mnemos: native vector search unavailable")

// EventRepository persists and retrieves domain events.
type EventRepository interface {
	Append(ctx context.Context, event domain.Event) error
	GetByID(ctx context.Context, id string) (domain.Event, error)
	ListByIDs(ctx context.Context, ids []string) ([]domain.Event, error)
	ListAll(ctx context.Context) ([]domain.Event, error)
	ListByRunID(ctx context.Context, runID string) ([]domain.Event, error)

	// CountAll returns the total number of events stored. Used by the
	// federation push/pull path to compute "newly inserted" deltas
	// without a per-row RowsAffected probe.
	CountAll(ctx context.Context) (int64, error)

	// DeleteByID removes the event with the given id. Idempotent —
	// deleting a non-existent event is a no-op. The caller is
	// responsible for cleaning up dependent rows (claim_evidence
	// links, claim cascade, embeddings) — DeleteByID does not
	// reach across repository boundaries.
	DeleteByID(ctx context.Context, id string) error

	// DeleteAll wipes every event row. Used by `mnemos reset`.
	// Foreign-key-dependent rows (claim_evidence) must be cleaned
	// up by the caller first; on backends that enforce FKs this
	// will error otherwise.
	DeleteAll(ctx context.Context) error
}

// ClaimRepository persists and retrieves extracted claims.
//
// The interface is the union of methods callers across cmd/mnemos and
// internal/pipeline reach for. Optional capabilities (full-text search,
// trust scoring) are factored into separate interfaces below so a
// backend that lacks them is still a valid ClaimRepository.
type ClaimRepository interface {
	Upsert(ctx context.Context, claims []domain.Claim) error
	UpsertWithReason(ctx context.Context, claims []domain.Claim, reason string) error
	UpsertWithReasonAs(ctx context.Context, claims []domain.Claim, reason, changedBy string) error
	UpsertEvidence(ctx context.Context, links []domain.ClaimEvidence) error
	ListByEventIDs(ctx context.Context, eventIDs []string) ([]domain.Claim, error)
	ListEvidenceByClaimIDs(ctx context.Context, claimIDs []string) ([]domain.ClaimEvidence, error)
	ListByIDs(ctx context.Context, claimIDs []string) ([]domain.Claim, error)
	ListAll(ctx context.Context) ([]domain.Claim, error)

	// ListByTestRequirementRef returns every test_result claim sharing the
	// given TestRequirementRef, ordered by TestLastRunAt DESC then
	// CreatedAt DESC. Drives `mnemos trust --test=<ref>` and the
	// which_test_to_trust MCP tool — replaces the previous
	// ListAll-then-filter-in-Go path so the query stays O(matched rows)
	// rather than O(total claims).
	ListByTestRequirementRef(ctx context.Context, ref string) ([]domain.Claim, error)
	ListStatusHistoryByClaimID(ctx context.Context, claimID string) ([]domain.ClaimStatusTransition, error)
	SetValidity(ctx context.Context, claimID string, validTo time.Time) error

	// SetLifecycle transitions an existing claim's promotion state
	// (candidate/promoted/superseded, or empty). It is the in-place
	// counterpart to writing Lifecycle on Upsert: a candidate claim a
	// human promotes, or a promoted claim a successor supersedes. Returns
	// sql.ErrNoRows-wrapped when the claim does not exist. mnemos enforces
	// no transition ordering — any value the domain recognises is allowed.
	SetLifecycle(ctx context.Context, claimID string, lifecycle domain.ClaimLifecycle) error

	// MarkVerified bumps the claim's last_verified to verifiedAt and
	// increments verify_count by one. Optional halfLifeDays > 0 also
	// rewrites the claim's per-claim freshness override; pass 0 to
	// leave whatever override was already on the row in place. Used by
	// `mnemos verify` and the MCP equivalent so re-confirming a claim
	// against fresh evidence is a single repository call.
	MarkVerified(ctx context.Context, claimID string, verifiedAt time.Time, halfLifeDays float64) error

	// RepointEvidence rewrites every claim_evidence row pointing at
	// fromClaimID to point at toClaimID instead, then deletes the
	// original rows. Idempotent on the (claim_id, event_id) dedup
	// key — duplicate evidence collapses silently. Used by
	// pipeline.ApplySemanticDedupe.
	RepointEvidence(ctx context.Context, fromClaimID, toClaimID string) error

	// DeleteCascade removes a claim and every row keyed on claim_id
	// that the claim alone owns. The canonical cascade set, which
	// every backend must clear in full for the tables it actually
	// has, is:
	//
	//	claim_evidence
	//	claim_status_history
	//	claim_versions
	//	claim_feedback
	//	claim_expectations
	//	claims (the row itself)
	//
	// "For the tables it actually has" is the whole contract: not
	// every backend declares every side table (MySQL has none of
	// versions/feedback/expectations; Postgres has expectations
	// only), but a backend that HAS one and skips it leaves a row
	// pointing at a claim that no longer exists — which is exactly
	// how the same delete came to mean different things depending on
	// the DSN. A new claim-keyed side table must be added here and to
	// every backend that declares it.
	//
	// Rows owned by ANOTHER aggregate are deliberately out of scope
	// and must be cleaned by the caller through the owning
	// repository: relationships (RelationshipRepository),
	// embeddings (EmbeddingRepository), claim_entities
	// (EntityRepository.UnlinkClaim), decision_beliefs
	// (DecisionRepository). DeleteCascade does not reach across
	// repository boundaries.
	DeleteCascade(ctx context.Context, claimID string) error

	// CountAll returns the total number of claims stored. Used by
	// federation pull (delta tracking) and metrics surfaces.
	CountAll(ctx context.Context) (int64, error)

	// ListAllEvidence returns every (claim_id, event_id) link in the
	// store. Used by federation push to dump the link table without
	// walking claims one at a time.
	ListAllEvidence(ctx context.Context) ([]domain.ClaimEvidence, error)

	// ListAllStatusHistory returns every claim_status_history row.
	// Used by `mnemos audit who` to attribute transitions to a
	// principal — filtering happens in the caller.
	ListAllStatusHistory(ctx context.Context) ([]domain.ClaimStatusTransition, error)

	// DeleteAll wipes claims plus their dependent rows (claim_evidence,
	// claim_status_history). The caller is responsible for clearing
	// rows owned by other repositories (relationships pointing at
	// claims, embeddings keyed on claim id) — DeleteAll does not
	// reach across repository boundaries.
	DeleteAll(ctx context.Context) error

	// ListIDsMissingEmbedding returns the claim ids that have no
	// row in the embeddings table for entity_type='claim'. Used by
	// `mnemos reembed` to scope the work to claims that need it.
	ListIDsMissingEmbedding(ctx context.Context) ([]string, error)
}

// TrustScorer is the optional capability to recompute and aggregate
// trust scores. Backends that don't track trust (e.g. a thin in-memory
// fixture) are still valid ClaimRepositories — callers type-assert
// before invoking these methods.
type TrustScorer interface {
	RecomputeTrust(ctx context.Context, score func(confidence float64, evidenceCount int, latestEvidence time.Time) float64) (int, error)
	AverageTrust(ctx context.Context) (float64, error)
	CountClaimsBelowTrust(ctx context.Context, threshold float64) (int64, error)
}

// ScopedTrustScorer is the optional capability to recompute trust for a
// bounded set of claims rather than the whole store.
//
// Trust is a function of a claim's own confidence, its evidence count and its
// most recent evidence, so a write can only change the trust of claims it
// touched: the claims it wrote, plus any claim it attached evidence to.
// Recomputing everything on every write made the cost of a write grow with the
// size of the brain — on an 11k-claim store every capture rewrote every row,
// and under -race that eventually blew the governed-write budget outright.
//
// Optional so a backend that cannot scope the query keeps working: callers
// fall back to the full [TrustScorer.RecomputeTrust].
type ScopedTrustScorer interface {
	RecomputeTrustForClaims(ctx context.Context, claimIDs []string, score func(confidence float64, evidenceCount int, latestEvidence time.Time) float64) (int, error)
}

// BeliefCreditWriter is the optional capability to persist an attributed
// belief-credit update produced by credit assignment (ADR 0014): a claim's
// confidence_components audit map plus the resulting trust_score, written
// together and without the claim-version churn a full Upsert would incur.
//
// Only backends that fully round-trip confidence_components implement it (SQLite,
// and therefore local libSQL, plus the in-memory store). The credit pass
// type-asserts for it, exactly like [TrustScorer]: a backend that does not
// persist the credit audit map simply skips credit assignment rather than
// mutating trust it could not attribute — the ADR-0011 "no silent trust change"
// guardrail applied at the storage seam.
type BeliefCreditWriter interface {
	// ApplyBeliefCredit overwrites the claim's confidence_components with
	// components (nil clears the map to '{}') and sets its trust_score. Callers
	// pass the already-merged component map (non-credit entries preserved, credit
	// entries replaced) so the write is a plain assignment and re-running is
	// idempotent.
	ApplyBeliefCredit(ctx context.Context, claimID string, components map[string]float64, trustScore float64) error
}

// RelationshipRepository persists and retrieves relationships between claims.
type RelationshipRepository interface {
	Upsert(ctx context.Context, relationships []domain.Relationship) error
	ListByClaim(ctx context.Context, claimID string) ([]domain.Relationship, error)
	ListByClaimIDs(ctx context.Context, claimIDs []string) ([]domain.Relationship, error)

	// RepointEndpoint rewrites every relationship whose from_claim_id
	// or to_claim_id equals oldID so that endpoint becomes newID.
	// Self-loops created by the rewrite (newID = newID) are dropped,
	// and unique-edge conflicts collapse silently — Mnemos doesn't
	// distinguish duplicate edges. Used by ApplySemanticDedupe to
	// fold a duplicate claim's edges onto its winner.
	RepointEndpoint(ctx context.Context, oldID, newID string) error

	// DeleteByClaim removes every relationship that touches the
	// given claim (as source OR target). Used to clean up a claim's
	// edges before the claim itself is deleted.
	DeleteByClaim(ctx context.Context, claimID string) error

	// CountAll returns the total number of relationships stored.
	CountAll(ctx context.Context) (int64, error)

	// CountByType returns the total number of relationships with the
	// given type. Used by metrics + browse contradiction listing.
	CountByType(ctx context.Context, relType string) (int64, error)

	// ListAll returns every relationship stored, ordered by
	// created_at ascending.
	ListAll(ctx context.Context) ([]domain.Relationship, error)

	// DeleteAll wipes every relationship row.
	DeleteAll(ctx context.Context) error
}

// RelationshipStrengthener is an optional capability (ADR 0015 §4, Hebbian
// co-activation): raise the stored strength of EXISTING association edges among a
// set of co-retrieved beliefs — "fire together, wire together". It is type-asserted
// exactly like [BeliefCreditWriter]: a backend that cannot persist a strength column
// simply skips strengthening rather than inventing edges or resetting weights. It
// never creates edges (co-retrieval is not itself a typed relationship) — only edges
// that already connect two members of the set are strengthened.
type RelationshipStrengthener interface {
	// StrengthenAssociations increments (by delta, capped at maxStrength) the strength
	// of every stored edge whose BOTH endpoints are in claimIDs, in either direction.
	// Returns the number of edges strengthened. delta<=0 or fewer than two ids is a
	// no-op.
	StrengthenAssociations(ctx context.Context, claimIDs []string, delta, maxStrength float64) (int, error)

	// DecayAssociations pulls every over-base edge's strength back TOWARD the base 1.0
	// by keeping the given fraction of its excess: strength = 1 + (strength-1)*retain.
	// So an edge that was co-activated but has since gone unused drifts back to neutral
	// (recency-weighted co-activation), while an edge at base 1.0 is untouched. It never
	// deletes edges and never falls below the base — mnemos edges are meaningful typed
	// relationships, not prunable co-occurrence noise. retain must be in [0,1); returns
	// the number of edges moved. The consolidation complement to StrengthenAssociations.
	DecayAssociations(ctx context.Context, retain float64) (int, error)
}

// ExtractionEngine extracts structured claims from domain events.
type ExtractionEngine interface {
	ExtractClaims(context.Context, []domain.Event) ([]domain.Claim, error)
}

// QueryEngine answers natural-language queries against the knowledge base.
type QueryEngine interface {
	Answer(ctx context.Context, query string) (domain.Answer, error)
}

// EmbeddingRepository persists and retrieves vector embeddings.
type EmbeddingRepository interface {
	// Upsert stores or replaces a vector for (entityID, entityType).
	// createdBy stamps the row's actor; pass "" to fall back to
	// domain.SystemUser at the storage boundary.
	Upsert(ctx context.Context, entityID, entityType string, vector []float32, model, createdBy string) error
	ListByEntityType(ctx context.Context, entityType string) ([]domain.EmbeddingRecord, error)

	// Delete removes the embedding row for the given entity. Idempotent
	// — deleting a non-existent embedding is a no-op. Used by
	// pipeline.ApplySemanticDedupe to drop a duplicate claim's
	// vector before deleting the claim itself.
	Delete(ctx context.Context, entityID, entityType string) error

	// CountAll returns the total number of embedding rows.
	CountAll(ctx context.Context) (int64, error)

	// ListAll returns every embedding row, ordered by created_at
	// ascending. Used by the federation push path to dump the
	// embedding table without per-type query plumbing.
	ListAll(ctx context.Context) ([]domain.EmbeddingRecord, error)

	// DeleteAll wipes every embedding row.
	DeleteAll(ctx context.Context) error
}

// ClaimVersionRepository persists the append-only version chain for
// a claim (Refs #38). Every Upsert path appends one row so a later
// read can answer "what did this claim say at version N?" without
// reconstructing the timeline from status_history + the current row.
type ClaimVersionRepository interface {
	// Append writes one new row. Version numbers are 1-based and
	// monotonic per claim id; the implementation chooses the next
	// number (typically max(existing)+1 or 1 if none).
	Append(ctx context.Context, v domain.ClaimVersion) error
	// ListByClaim returns every recorded version for a claim,
	// ordered version DESC (newest first) so consumers can build a
	// diff timeline without extra sorting.
	ListByClaim(ctx context.Context, claimID string) ([]domain.ClaimVersion, error)
}

// FeedbackRepository persists per-claim feedback state (helpful /
// not-helpful streaks + last note). Lives separately from the claim
// row itself so the claim hot paths don't pay extra columns for what
// is a relatively cold signal. Implementations are upsert-by-claim_id;
// a fresh claim with no recorded feedback yet returns the zero value
// (and ok=false) from Get.
type FeedbackRepository interface {
	// Get returns the feedback state for claimID, or ok=false if no
	// row exists yet. Implementations must not return an error for
	// the "not found" case — feedback is sparse by design.
	Get(ctx context.Context, claimID string) (domain.ClaimFeedback, bool, error)
	// Upsert writes the supplied state atomically. The handler is
	// responsible for read-modify-write; the repository is dumb on
	// purpose so a future caller (background reaction worker) can
	// share the same write path.
	Upsert(ctx context.Context, state domain.ClaimFeedback) error
}

// ExpectationRepository persists structured forward expectations attached to
// claims (one per claim). Like the other side tables it is a dumb store; the
// reconciliation logic lives in the caller. nil on backends without an
// implementation; callers type-check before use.
type ExpectationRepository interface {
	// Upsert writes the expectation under its ClaimID.
	Upsert(ctx context.Context, exp domain.Expectation) error
	// Get returns the expectation for claimID, or ok=false when none exists.
	Get(ctx context.Context, claimID string) (domain.Expectation, bool, error)
	// ListOpen returns every unresolved expectation (awaiting reconciliation).
	ListOpen(ctx context.Context) ([]domain.Expectation, error)
}

// BlockRepository persists an agent's working-memory blocks — bounded, labeled,
// mutable text keyed by (owner, label). Like [FeedbackRepository] it is a dumb
// side-table store: bound-enforcement and read-modify-write live in the caller
// (the public API), so the repository stays a plain persistence seam. nil on
// backends without an implementation; callers type-check before use.
type BlockRepository interface {
	// Get returns the block for (owner, label), or ok=false when none exists.
	// "Not found" is not an error — blocks are sparse by design.
	Get(ctx context.Context, owner, label string) (domain.WorkingMemoryBlock, bool, error)
	// List returns every block for an owner, label-ordered.
	List(ctx context.Context, owner string) ([]domain.WorkingMemoryBlock, error)
	// Upsert writes the block atomically under (owner, label).
	Upsert(ctx context.Context, block domain.WorkingMemoryBlock) error
	// Delete removes the block for (owner, label). Deleting a missing block is
	// a no-op, not an error.
	Delete(ctx context.Context, owner, label string) error
}

// ClaimSimilarityHit is one row of a vector-similarity search over
// claim embeddings: the claim id, the cosine similarity (1.0 =
// identical, 0.0 = orthogonal), and the model the stored vector was
// generated with. Callers compare Model against their query embedding
// model so a quietly-different stored model can't masquerade as a
// match.
type ClaimSimilarityHit struct {
	ClaimID    string
	Similarity float64
	Model      string
}

// ClaimSimilaritySearcher is the optional capability to rank claims by
// cosine similarity against a query vector. Implementations iterate
// embeddings with entity_type='claim', restrict to candidateClaimIDs
// when the set is non-nil (server-side tenant boundary applied
// upstream), and return up to topK hits with similarity >=
// minSimilarity ordered by similarity desc.
//
// The candidate-id set is the tenant-isolation seam: the HTTP handler
// computes "claims this caller is allowed to see" — typically by
// resolving run_id → events → claim_evidence — and hands the result
// down so the searcher never has to know about RunIDs. A nil set means
// "search the entire corpus" (admin / unscoped queries).
type ClaimSimilaritySearcher interface {
	SearchClaimsByVector(
		ctx context.Context,
		queryVector []float32,
		candidateClaimIDs map[string]struct{},
		model string,
		topK int,
		minSimilarity float64,
	) ([]ClaimSimilarityHit, error)
}

// EventSimilarityHit is one row of a vector-similarity search over event
// embeddings: the event id, the cosine similarity (1.0 = identical, 0.0 =
// orthogonal), and the model the stored vector was generated with. Callers
// compare Model against their query embedding model so a quietly-different
// stored model can't masquerade as a match.
type EventSimilarityHit struct {
	EventID    string
	Similarity float64
	Model      string
}

// EventVectorSearcher is the optional capability to rank events by cosine
// similarity against a query vector using a native, index-friendly vector
// operator (pgvector `<=>`) instead of decoding every stored embedding and
// cosining in Go. It is the scale seam for recall: the query engine
// type-asserts on this and, when present, retrieves the top-K candidate
// event ids by vector rather than loading the whole corpus with ListAll.
//
// Implementations MUST scope results to the caller's tenant (row-level
// security handles this transparently on Postgres) and MUST return
// ErrVectorSearchUnavailable — never a partial or wrong result — when the
// backend has no native vector path, so the engine falls back cleanly to
// the in-Go cosine / token-overlap ranker.
type EventVectorSearcher interface {
	SearchEventsByVector(
		ctx context.Context,
		queryVector []float32,
		model string,
		topK int,
		minSimilarity float64,
	) ([]EventSimilarityHit, error)
}

// TextHit is one row of a keyword search: the matched row's id and a
// positive relevance score (higher is better). Returned by
// TextSearcher implementations so the query engine can rank without
// caring whether the underlying index is FTS5, Lucene, or anything
// else.
type TextHit struct {
	ID    string
	Score float64
}

// TextSearcher exposes a keyword (BM25-style) search index over a
// table of text rows. Optional capability: the query engine type-
// asserts on this and falls back to cosine + token-overlap when the
// repository doesn't implement it (older test doubles, in-memory
// fakes, etc.).
type TextSearcher interface {
	SearchByText(ctx context.Context, query string, limit int) ([]TextHit, error)
}

// UserRepository persists and retrieves user identities.
type UserRepository interface {
	Create(ctx context.Context, user domain.User) error
	GetByID(ctx context.Context, id string) (domain.User, error)
	GetByEmail(ctx context.Context, email string) (domain.User, error)
	List(ctx context.Context) ([]domain.User, error)
	UpdateStatus(ctx context.Context, id string, status domain.UserStatus) error
	UpdateScopes(ctx context.Context, id string, scopes []string) error
}

// RevokedTokenRepository persists and queries the JWT denylist.
type RevokedTokenRepository interface {
	Add(ctx context.Context, token domain.RevokedToken) error
	IsRevoked(ctx context.Context, jti string) (bool, error)
	PurgeExpired(ctx context.Context, before time.Time) (int, error)
}

// AgentRepository persists and retrieves non-human principals.
type AgentRepository interface {
	Create(ctx context.Context, agent domain.Agent) error
	GetByID(ctx context.Context, id string) (domain.Agent, error)
	List(ctx context.Context) ([]domain.Agent, error)
	UpdateStatus(ctx context.Context, id string, status domain.AgentStatus) error
	UpdateScopes(ctx context.Context, id string, scopes []string) error
	UpdateAllowedRuns(ctx context.Context, id string, runs []string) error

	// Upsert is the federation entry point: callers (HTTP /v1/agents
	// POST handlers, push/pull workers) hand a batch of agents and
	// the repository preserves identity, status, scopes, and
	// allowed-runs. Implementations dedupe by ID.
	Upsert(ctx context.Context, agents []domain.Agent) error
}

// CompilationJobRepository persists workflow job state. The runner
// in internal/workflow drives the lifecycle; this interface is just
// the storage seam.
type CompilationJobRepository interface {
	Upsert(ctx context.Context, job domain.CompilationJob) error
	GetByID(ctx context.Context, id string) (domain.CompilationJob, error)
}

// ActionRepository persists and retrieves operational actions —
// recorded changes that may have produced observable outcomes.
// Actions are append-only: an action's facts are immutable once
// written; status drift is captured by emitting a follow-up Action
// plus its Outcome rather than mutating the original row.
type ActionRepository interface {
	Append(ctx context.Context, action domain.Action) error
	GetByID(ctx context.Context, id string) (domain.Action, error)
	ListByRunID(ctx context.Context, runID string) ([]domain.Action, error)
	ListBySubject(ctx context.Context, subject string) ([]domain.Action, error)
	ListAll(ctx context.Context) ([]domain.Action, error)
	CountAll(ctx context.Context) (int64, error)
	DeleteAll(ctx context.Context) error
}

// OutcomeRepository persists and retrieves the observed results of
// actions. Outcomes carry an ActionID back-reference plus a numeric
// metric map. Outcomes are append-only for the same reasons as
// Actions; corrections are expressed via a fresh Outcome row.
type OutcomeRepository interface {
	Append(ctx context.Context, outcome domain.Outcome) error
	GetByID(ctx context.Context, id string) (domain.Outcome, error)
	ListByActionID(ctx context.Context, actionID string) ([]domain.Outcome, error)
	ListAll(ctx context.Context) ([]domain.Outcome, error)
	CountAll(ctx context.Context) (int64, error)
	DeleteAll(ctx context.Context) error
}

// JournalRepository is the append-only cognitive journal (ADR 0018): a durable,
// timestamped log of what the brain's learning mechanisms did, so trajectories can be
// studied and constants tuned against real data. Append-only — rows are never updated or
// deleted on the write path. nil on providers without an implementation; callers
// type-check before use.
type JournalRepository interface {
	// Append persists journal entries. Idempotent on id.
	Append(ctx context.Context, entries []domain.JournalEntry) error
	// List returns the most recent entries of the given kind (empty kind = any),
	// newest first, capped at limit (limit <= 0 means a sane default).
	List(ctx context.Context, kind string, limit int) ([]domain.JournalEntry, error)
	// ListBySubject returns the most recent entries for one subject id (e.g. a claim's
	// trust trajectory), newest first, capped at limit.
	ListBySubject(ctx context.Context, subjectID string, limit int) ([]domain.JournalEntry, error)
}

// EntityRelationshipRepository persists polymorphic edges between
// arbitrary entities (action↔outcome, outcome↔claim, decision
// ↔outcome). The classic claim-only relationships graph stays
// unaffected; cross-entity edges live exclusively here.
type EntityRelationshipRepository interface {
	Upsert(ctx context.Context, edges []domain.EntityRelationship) error
	ListByEntity(ctx context.Context, entityID, entityType string) ([]domain.EntityRelationship, error)
	ListByKind(ctx context.Context, kind string) ([]domain.EntityRelationship, error)
	ListAll(ctx context.Context) ([]domain.EntityRelationship, error)
	CountAll(ctx context.Context) (int64, error)
	DeleteAll(ctx context.Context) error
}

// LessonRepository persists synthesised lessons and the link table
// back to the actions that corroborated them. Lessons are upsert-by-id
// so re-running synthesis with fresh evidence ratchets confidence and
// last_verified forward without churning identity. Evidence rows are
// idempotent on (lesson_id, action_id).
type LessonRepository interface {
	Append(ctx context.Context, lesson domain.Lesson) error
	GetByID(ctx context.Context, id string) (domain.Lesson, error)
	ListByService(ctx context.Context, service string) ([]domain.Lesson, error)
	ListByTrigger(ctx context.Context, trigger string) ([]domain.Lesson, error)
	ListAll(ctx context.Context) ([]domain.Lesson, error)
	CountAll(ctx context.Context) (int64, error)
	DeleteAll(ctx context.Context) error
	AppendEvidence(ctx context.Context, lessonID string, actionIDs []string) error
	ListEvidence(ctx context.Context, lessonID string) ([]string, error)
	// ListVersions returns the lesson's full snapshot history newest
	// first. Each entry is the JSON-encoded payload that was current
	// at the time of that snapshot, plus the [valid_from, valid_to)
	// window the snapshot covered.
	ListVersions(ctx context.Context, lessonID string) ([]EntityVersion, error)
}

// DecisionRepository persists records of agent (or human) decisions:
// the belief claims that justified a chosen plan, the alternatives
// that were considered, and — once observed — the outcome that
// validated or refuted the bet. Decisions are upsert-by-id so
// retrying with stronger evidence rewrites the row without churning
// the original chosen_at moment.
type DecisionRepository interface {
	Append(ctx context.Context, decision domain.Decision) error
	GetByID(ctx context.Context, id string) (domain.Decision, error)
	ListAll(ctx context.Context) ([]domain.Decision, error)
	ListByRiskLevel(ctx context.Context, level string) ([]domain.Decision, error)
	AttachOutcome(ctx context.Context, decisionID, outcomeID string) error
	CountAll(ctx context.Context) (int64, error)
	DeleteAll(ctx context.Context) error
	AppendBeliefs(ctx context.Context, decisionID string, claimIDs []string) error
	ListBeliefs(ctx context.Context, decisionID string) ([]string, error)
}

// PlaybookRepository persists synthesised playbooks and their link
// table back to the lessons that justified them. Playbooks are
// upsert-by-id so re-synthesis ratchets confidence and last_verified
// without churning identity.
type PlaybookRepository interface {
	Append(ctx context.Context, playbook domain.Playbook) error
	GetByID(ctx context.Context, id string) (domain.Playbook, error)
	ListByTrigger(ctx context.Context, trigger string) ([]domain.Playbook, error)
	ListByService(ctx context.Context, service string) ([]domain.Playbook, error)
	ListAll(ctx context.Context) ([]domain.Playbook, error)
	CountAll(ctx context.Context) (int64, error)
	DeleteAll(ctx context.Context) error
	AppendLessons(ctx context.Context, playbookID string, lessonIDs []string) error
	ListLessons(ctx context.Context, playbookID string) ([]string, error)
	// ListVersions mirrors LessonRepository.ListVersions for playbooks.
	ListVersions(ctx context.Context, playbookID string) ([]EntityVersion, error)
}

// EntityVersion is one row from a system-versioned snapshot table.
// PayloadJSON carries the full prior shape of the entity (lesson or
// playbook) so callers can render it without a per-version schema.
// ValidFrom and ValidTo bound the [valid_from, valid_to) window the
// snapshot covered; for the most recent version (still in force)
// ValidTo is the zero value.
type EntityVersion struct {
	VersionID   int64
	PayloadJSON string
	ValidFrom   time.Time
	ValidTo     time.Time
}

// GlobalSchemaRepository persists promoted, de-identified schemas in the
// shared global ("neocortex") tier — the write side of the ADR 0011
// consolidation pass. Records are upsert-by-id so re-running promotion with
// fresh corroboration ratchets a schema's counts/confidence forward without
// churning identity.
//
// Unlike every per-tenant repository, this store is deliberately NOT
// tenant-scoped: promoted schemas are the shared tier and must be readable
// across tenants (the inverse of the ADR-0007 isolation gotcha). nil on
// providers that have not implemented it; callers type-check before use.
type GlobalSchemaRepository interface {
	// Upsert writes (or replaces) the schema on its id.
	Upsert(ctx context.Context, schema domain.GlobalSchema) error
	// GetByID returns the schema with the given id, or ok=false when none
	// exists. "Not found" is not an error.
	GetByID(ctx context.Context, id string) (domain.GlobalSchema, bool, error)
	// ListAll returns every promoted schema, ordered by promoted_at descending.
	ListAll(ctx context.Context) ([]domain.GlobalSchema, error)
	// ListByStatus returns promoted schemas matching the given lifecycle
	// status, ordered by promoted_at descending.
	ListByStatus(ctx context.Context, status domain.GlobalSchemaStatus) ([]domain.GlobalSchema, error)
	// Approve activates a pending schema (pending → active). It is the
	// operator-gate release step. Returns sql.ErrNoRows-wrapped when the id
	// does not exist; approving an already-active schema is a no-op.
	Approve(ctx context.Context, id string) error
	// CountAll returns the total number of promoted schema rows.
	CountAll(ctx context.Context) (int64, error)
	// DeleteAll wipes every promoted schema row. Used by mnemos reset.
	DeleteAll(ctx context.Context) error
}

// EntityRepository persists canonicalised entities and the
// claim_entities link table. The interface mirrors the SQLite
// implementation's public surface so cmd/mnemos and internal/pipeline
// can drop their named SQLite import.
//
// Implementations must enforce the UNIQUE(normalized_name, type)
// dedup contract: FindOrCreate is the only sanctioned write path
// for new entities and is expected to be idempotent under contention.
type EntityRepository interface {
	FindOrCreate(ctx context.Context, name string, etype domain.EntityType, createdBy string) (domain.Entity, error)
	LinkClaim(ctx context.Context, claimID, entityID, role string) error
	// UnlinkClaim drops every claim_entities row for claimID. It is
	// the ONLY way a caller can reach that table by claim, and it is
	// not a convenience: claim_entities has a foreign key to claims
	// on the FK-enforcing backends, so a surviving link makes
	// ClaimRepository.DeleteCascade fail outright. Semantic dedupe
	// hit exactly that — the loser's evidence had already moved to
	// the winner when the final DELETE aborted, leaving an active
	// claim with zero evidence. Deleting nothing is success:
	// the call is idempotent so a resumed pass converges.
	UnlinkClaim(ctx context.Context, claimID string) error
	List(ctx context.Context) ([]domain.Entity, error)
	ListByType(ctx context.Context, etype domain.EntityType) ([]domain.Entity, error)
	FindByName(ctx context.Context, name string) (domain.Entity, bool, error)
	ListClaimsForEntity(ctx context.Context, entityID string) ([]domain.Claim, error)
	ListEntitiesForClaim(ctx context.Context, claimID string) ([]domain.Entity, []string, error)
	Merge(ctx context.Context, winnerID, loserID string) error
	Count(ctx context.Context) (int64, error)
	ClaimIDsMissingEntityLinks(ctx context.Context) ([]string, error)
}

// IncidentRepository persists and retrieves incidents — structured
// records of failure events used for "Why Were We Wrong?" analysis.
// Incidents are upsert-by-id so re-opening with updated evidence
// rewrites the row without churning the opened_at timestamp.
type IncidentRepository interface {
	// Upsert creates or replaces an incident row by ID. The full
	// Incident struct (including all JSON array fields) is written
	// atomically.
	Upsert(ctx context.Context, incident domain.Incident) error
	// GetByID returns the incident with the given ID, or
	// (Incident{}, false, nil) when no matching row exists.
	GetByID(ctx context.Context, id string) (domain.Incident, bool, error)
	// ListAll returns every incident ordered by opened_at descending.
	ListAll(ctx context.Context) ([]domain.Incident, error)
	// ListBySeverity returns incidents matching the given severity,
	// ordered by opened_at descending.
	ListBySeverity(ctx context.Context, severity domain.IncidentSeverity) ([]domain.Incident, error)
	// ListByStatus returns incidents matching the given lifecycle
	// status, ordered by opened_at descending.
	ListByStatus(ctx context.Context, status domain.IncidentStatus) ([]domain.Incident, error)
	// Resolve sets the incident's status to "resolved" and stamps
	// resolved_at. Idempotent on already-resolved incidents.
	Resolve(ctx context.Context, id string, resolvedAt time.Time) error
	// AttachDecision appends decisionID to the incident's
	// decision_ids list if not already present.
	AttachDecision(ctx context.Context, incidentID, decisionID string) error
	// AttachOutcome appends outcomeID to the incident's outcome_ids
	// list if not already present.
	AttachOutcome(ctx context.Context, incidentID, outcomeID string) error
	// SetPlaybook records the synthesised playbook that came out of
	// this incident's lessons.
	SetPlaybook(ctx context.Context, incidentID, playbookID string) error
	// CountAll returns the total number of incident rows.
	CountAll(ctx context.Context) (int64, error)
	// DeleteAll wipes every incident row. Used by mnemos reset.
	DeleteAll(ctx context.Context) error
}
