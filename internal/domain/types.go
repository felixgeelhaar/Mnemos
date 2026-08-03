package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// InputType represents the format classification of an ingested input.
type InputType string

// Supported InputType values.
const (
	InputTypeText       InputType = "text"
	InputTypeJSON       InputType = "json"
	InputTypeCSV        InputType = "csv"
	InputTypeMD         InputType = "md"
	InputTypeTranscript InputType = "transcript"
)

// ClaimType categorises a claim as a fact, hypothesis, or decision.
type ClaimType string

// IsBuiltinClaimType reports whether t is one of mnemos's native claim types.
// The store also accepts consumer types registered via WithClaimTypes; that
// wider check lives at the write boundary (the domain can't see the registry).
func IsBuiltinClaimType(t ClaimType) bool {
	switch t {
	case ClaimTypeFact, ClaimTypeHypothesis, ClaimTypeDecision, ClaimTypeTestResult:
		return true
	default:
		return false
	}
}

// Supported ClaimType values.
const (
	ClaimTypeFact       ClaimType = "fact"
	ClaimTypeHypothesis ClaimType = "hypothesis"
	ClaimTypeDecision   ClaimType = "decision"
	ClaimTypeTestResult ClaimType = "test_result"
)

// SourceType classifies the origin of a claim.
type SourceType string

// Supported SourceType values.
const (
	SourceTypeDocument    SourceType = "document"
	SourceTypeTranscript  SourceType = "transcript"
	SourceTypeGitCommit   SourceType = "git_commit"
	SourceTypeWebPage     SourceType = "web_page"
	SourceTypeAPIResponse SourceType = "api_response"
	SourceTypeManualEntry SourceType = "manual_entry"
)

// LivenessStatus indicates whether a source is still actively used/executed.
type LivenessStatus string

// Supported LivenessStatus values.
const (
	LivenessLive    LivenessStatus = "live"   // actively referenced/executed
	LivenessStale   LivenessStatus = "stale"  // not recently used but not dead
	LivenessZombie  LivenessStatus = "zombie" // old but still trusted
	LivenessDead    LivenessStatus = "dead"   // no longer valid or used
	LivenessUnknown LivenessStatus = "unknown"
)

// Visibility controls who can see a claim — inspired by Nomi's room model.
// It is intentionally separate from Scope (service/env/team filtering): Scope
// narrows operational context, Visibility gates access by audience.
//
//   - Personal  — visible only to the submitting user / agent; a private
//     working note not yet ready for teammates.
//   - Team      — visible to every member of the team named in Scope.Team
//     (or the default workspace team when Scope is empty). The
//     default for all new claims.
//   - Org       — organisational truth; visible to all principals in the
//     workspace. Requires explicit promotion from team.
type Visibility string

// Visibility values controlling audience access.
const (
	VisibilityPersonal Visibility = "personal"
	VisibilityTeam     Visibility = "team" // default
	VisibilityOrg      Visibility = "org"
)

// DefaultVisibility is the value applied when no Visibility is specified.
const DefaultVisibility = VisibilityTeam

// ClaimStatus represents the lifecycle state of a claim.
type ClaimStatus string

// Supported ClaimStatus values. The lifecycle reads as:
//
//	active → contested (when a contradicting claim lands)
//	contested → resolved (when an operator picks a winner)
//	contested → deprecated (when the loser of a resolution is retired)
//	any → deprecated (when a claim is manually withdrawn)
//
// Status transitions are recorded in claim_status_history — see
// ClaimRepository.ListStatusHistoryByClaimID.
const (
	ClaimStatusActive     ClaimStatus = "active"
	ClaimStatusContested  ClaimStatus = "contested"
	ClaimStatusResolved   ClaimStatus = "resolved"
	ClaimStatusDeprecated ClaimStatus = "deprecated"
)

// ClaimLifecycle is the human-curation dimension of a claim, ORTHOGONAL to
// [ClaimStatus] (which tracks contradiction/verification). It models the
// candidate -> promoted -> superseded pipeline a consumer uses to curate which
// claims are vetted, durable knowledge:
//
//	"" (empty)  – uncurated; the default for extracted/synthesized claims.
//	candidate   – proposed for promotion, not yet human-vetted.
//	promoted    – a human promoted it to vetted, durable knowledge.
//	superseded  – replaced by a newer promoted claim (history preserved).
//
// It is descriptive metadata the store round-trips and recall can filter on
// (see the Lifecycle query filter); mnemos enforces no transitions itself.
type ClaimLifecycle string

// The valid promotion states for a claim; empty means the distinction
// does not apply (an ordinary claim never routed through review).
const (
	ClaimLifecycleCandidate  ClaimLifecycle = "candidate"
	ClaimLifecyclePromoted   ClaimLifecycle = "promoted"
	ClaimLifecycleSuperseded ClaimLifecycle = "superseded"
)

// IsValidClaimLifecycle reports whether s is a recognised lifecycle value.
// The empty string (uncurated) is valid.
func IsValidClaimLifecycle(s ClaimLifecycle) bool {
	switch s {
	case "", ClaimLifecycleCandidate, ClaimLifecyclePromoted, ClaimLifecycleSuperseded:
		return true
	default:
		return false
	}
}

// RelationshipType describes how two claims are related.
type RelationshipType string

// Supported RelationshipType values.
//
// The original pair (supports / contradicts) expresses logical agreement
// between claims. The causal+outcome family extends the graph to express
// real-world dynamics: which action produced which observed state, which
// hypothesis was validated or refuted by which outcome, and which
// synthesised lesson was derived from which raw claim. The graph is
// directional; semantics live on the From -> To direction.
const (
	RelationshipTypeSupports    RelationshipType = "supports"
	RelationshipTypeContradicts RelationshipType = "contradicts"
	// RelationshipTypeCites links a claim to another claim it explicitly
	// references, forming a directed citation graph used for convergence
	// analysis (how many independent claims point to a source claim).
	RelationshipTypeCites RelationshipType = "cites"

	// RelationshipTypeCauses asserts that From caused To (cause -> effect).
	RelationshipTypeCauses RelationshipType = "causes"
	// RelationshipTypeCausedBy is the inverse of Causes (effect -> cause)
	// stored explicitly so reverse traversal stays a single index lookup.
	RelationshipTypeCausedBy RelationshipType = "caused_by"
	// RelationshipTypeActionOf links an action claim to the outcome claim
	// it produced (action -> outcome). Used by the action+outcome layer.
	RelationshipTypeActionOf RelationshipType = "action_of"
	// RelationshipTypeOutcomeOf is the inverse of ActionOf (outcome -> action).
	RelationshipTypeOutcomeOf RelationshipType = "outcome_of"
	// RelationshipTypeValidates asserts From validates To, e.g. an outcome
	// claim validates a hypothesis claim it was meant to test.
	RelationshipTypeValidates RelationshipType = "validates"
	// RelationshipTypeRefutes asserts From refutes To, the negative
	// counterpart to Validates.
	RelationshipTypeRefutes RelationshipType = "refutes"
	// RelationshipTypeDerivedFrom links a synthesised claim (typically a
	// lesson or playbook step) back to the raw claim it was generalised
	// from, preserving provenance through the synthesis layer.
	RelationshipTypeDerivedFrom RelationshipType = "derived_from"
)

// IsValidRelationshipType reports whether t is a recognised relationship
// type. Validation paths use this rather than open-coding the switch so
// future additions only need to update the const block plus this helper.
func IsValidRelationshipType(t RelationshipType) bool {
	switch t {
	case RelationshipTypeSupports,
		RelationshipTypeContradicts,
		RelationshipTypeCites,
		RelationshipTypeCauses,
		RelationshipTypeCausedBy,
		RelationshipTypeActionOf,
		RelationshipTypeOutcomeOf,
		RelationshipTypeValidates,
		RelationshipTypeRefutes,
		RelationshipTypeDerivedFrom:
		return true
	}
	return false
}

// Input represents a raw document or data source submitted for ingestion.
type Input struct {
	ID        string
	Type      InputType
	Format    string
	Metadata  map[string]string
	CreatedAt time.Time
}

// Episode represents a single timestamped piece of knowledge extracted from an input.
type Episode struct {
	ID            string
	RunID         string
	SchemaVersion string
	Content       string
	SourceInputID string
	Timestamp     time.Time
	Metadata      map[string]string
	IngestedAt    time.Time
	CreatedBy     string // user id of the actor that created this episode; "<system>" for unattributed
}

// Event is the pre-ADR-0011 name for Episode; kept as a back-compat
// alias (remove at API v2).
type Event = Episode

// Belief represents an assertion derived from one or more episodes,
// carrying a type, confidence score, and lifecycle status.
type Belief struct {
	ID         string
	Text       string
	Type       ClaimType
	Confidence float64
	Status     ClaimStatus
	// Lifecycle is the human-curation dimension (candidate/promoted/superseded),
	// orthogonal to Status. Empty = uncurated. See [ClaimLifecycle].
	Lifecycle  ClaimLifecycle
	CreatedAt  time.Time
	CreatedBy  string  // user id of the actor that created this claim; "<system>" for unattributed
	TrustScore float64 // derived from confidence × corroboration × freshness; computed by internal/trust

	// ValidFrom is when the claim's content first became true. Defaults
	// to the source event's timestamp at insert time; see internal/pipeline.
	// A zero value means "valid since before the system started tracking".
	ValidFrom time.Time
	// ValidTo is when the claim stopped being true (a successor claim
	// took its place). Zero value means "currently valid / still in
	// force". Set by `mnemos resolve --supersedes` or by future
	// auto-supersession detection.
	ValidTo time.Time

	// LastVerified ticks forward each time `mnemos verify` (or the
	// MCP equivalent) re-confirms the claim against fresh evidence.
	// Zero value means "never explicitly verified" — the freshness
	// factor falls back to the latest evidence event's timestamp.
	LastVerified time.Time
	// VerifyCount counts every successful re-verification. Used as a
	// secondary trust input when ranking near-tied claims.
	VerifyCount int
	// HalfLifeDays optionally overrides the global trust freshness
	// half-life on a per-claim basis. Zero falls back to the
	// internal/trust default. Useful for facts whose decay profile
	// genuinely differs from the project default — e.g. a SLA that
	// becomes stale in 7 days vs an architectural decision good for
	// a year.
	HalfLifeDays float64

	// HalfLifeClassifier records WHICH classifier assigned HalfLifeDays, and
	// is empty when none did (ADR 0025).
	//
	// It exists because HalfLifeDays = 0 is two different facts: "the
	// classifier read this text and declined to shorten it" (a verdict) and
	// "no classifier has ever looked at this claim" (the absence of one).
	// Both store 0, and for SCORING they are genuinely identical — trust's
	// `<= 0 → use the default` contract is correct for both, which is why
	// the collision went unnoticed. Everything downstream of the number is
	// what breaks: a backfill cannot report that it finished, a classifier
	// improvement cannot find the rows a superseded version looked at, and
	// coverage is unanswerable.
	//
	// Read with HalfLifeDays, the two columns give four distinct states:
	//
	//	""              + 0   unknown — nobody has looked
	//	""              + >0  human override via MarkVerified
	//	"volatility/v1" + 0   classified DURABLE — decays at the default by decision
	//	"volatility/v1" + 14  classified volatile
	//
	// Nothing in scoring, ranking, admission, staleness or consolidation
	// reads this field, deliberately: a data-model change that alters recall
	// on day one cannot be rolled back by reverting a binary.
	HalfLifeClassifier string

	// Scope optionally narrows the claim to a specific operational
	// context (service, env, team). Empty scope (the zero value)
	// means "applies everywhere". The query engine filters by scope
	// when Answer is requested with a non-empty filter; synthesis
	// already routes through claim->action->lesson scope upstream.
	Scope Scope

	// SubjectClass classifies WHAT the belief is about — a specific
	// instance (individual) versus a category (class) — for the ADR 0012
	// promotion eligibility gate. Empty (SubjectClassUnknown) means
	// unclassified. It can be inferred from the belief's subject entities
	// (see SubjectClassFromEntityTypes) or set by an extraction-time hint /
	// explicit override. Only class-level beliefs are ever eligible to feed
	// the shared global brain; individual and unknown are kept private.
	SubjectClass SubjectClass

	// Durability says whether this belief's value outlives the conversation it
	// came from (ADR 0023). Empty (DurabilityUnknown) means unclassified and is
	// treated as durable everywhere — the entire back catalogue predates the
	// field, so absence of a verdict must never demote a belief.
	Durability Durability

	// --- Epistemic Provenance Fields ---

	// SourceDocument is the original source (URL, file path, doc ID)
	// from which this claim was extracted or where it is primarily stated.
	SourceDocument string
	// SourceType classifies the kind of source (document, transcript, git_commit, etc.).
	SourceType SourceType
	// SourceAuthority is a 0.0-1.0 score of the source's authority/trustworthiness.
	// Can be user-defined or inferred from reputation systems.
	SourceAuthority float64
	// Liveness indicates whether the source is still actively used/executed.
	// A 12-year-old process doc still being executed = live (zombie).
	Liveness LivenessStatus
	// LastExecuted is the last time the source was referenced, executed, or accessed.
	// Zero value means unknown. Used to compute liveness.
	LastExecuted time.Time
	// CitationCount is the number of other claims/sources that link to this claim.
	// Derived from the citation graph; higher = more corroboration.
	CitationCount int
	// ProvenanceRationale is a human-readable explanation of why this claim
	// is trusted over alternatives (e.g. "3 sources agree, source is live, recent").
	ProvenanceRationale string

	// Test provenance fields (used when Type == test_result).
	TestID             string
	TestRequirementRef string
	TestAuthor         string
	TestLastModified   time.Time
	TestLastRunAt      time.Time
	TestPassCount      int
	TestFailCount      int

	// Visibility gates audience access independent of Scope.
	// Personal = submitting user/agent only; Team = workspace team (default);
	// Org = all principals in the workspace.
	// Zero value is treated as VisibilityTeam at read/write time.
	Visibility Visibility

	// ConfidenceComponents decomposes the scalar Confidence into named
	// contributors (e.g. "data_quality": 0.9, "corroboration": 0.5,
	// "source_authority": 0.8). The scalar Confidence stays the
	// canonical "overall" number for back-compat; components are
	// purely additional context for richer downstream narration and
	// the reaction loop in #40 (which decays "corroboration" on
	// negative feedback). Nil/empty means the producer did not
	// surface a decomposition — consumers treat absent as "no
	// decomposition available", NOT as "all components are zero".
	//
	// The map also carries the credit-assignment audit trail (ADR 0014):
	// entries whose key starts with [CreditComponentPrefix] are SIGNED trust
	// deltas attributed to the decision/prediction that drove them.
	ConfidenceComponents map[string]float64
}

// Claim is the pre-ADR-0011 name for Belief; kept as a back-compat
// alias (remove at API v2).
type Claim = Belief

// ClaimVersion is one row of a claim's full text/confidence/status
// audit trail (Refs #38). Versions accumulate on every Upsert path
// so a future read can answer "what did this claim say at version
// N?" or build a diff timeline. Lives in a side table so the claim
// row itself stays slim.
type ClaimVersion struct {
	// ClaimID is the claim this version snapshots.
	ClaimID string
	// Version is a monotonic 1-based generation counter for this
	// claim. The first insert is always v1; every subsequent Upsert
	// (text edit, confidence change, status change) bumps it.
	Version int
	// Text, Confidence, Status capture the claim's value at this
	// version.
	Text       string
	Confidence float64
	Status     ClaimStatus
	// WrittenAt is when the version row was created. Distinct from
	// the claim's CreatedAt (which marks the original insert).
	WrittenAt time.Time
	// WrittenBy stamps the actor that performed the write.
	WrittenBy string
}

// ClaimFeedback is the per-claim feedback state read off the
// claim_feedback side table. Lives separately from Claim so the
// list-claims hot paths don't pay an extra column on every read; the
// feedback endpoint and any caller that wants the streak/note loads
// it explicitly.
type ClaimFeedback struct {
	// ClaimID is the claim this feedback state attaches to.
	ClaimID string
	// NegativeFeedbackStreak counts consecutive "not helpful" votes
	// since the last "helpful" vote (which resets it to zero). When
	// the streak crosses the configured threshold the feedback
	// handler auto-transitions the claim to "contested".
	NegativeFeedbackStreak int
	// HelpfulCount is the lifetime count of "helpful" votes. Unlike
	// NegativeFeedbackStreak it never resets — it is a corroboration
	// signal the trust scorer can weight.
	HelpfulCount int
	// LastFeedbackAt stamps when the most recent feedback landed.
	// Zero means "no feedback yet".
	LastFeedbackAt time.Time
	// LastFeedbackNote is the most recent reviewer note kept verbatim
	// for the audit trail. Empty means "no note supplied" (positive
	// feedback typically omits it).
	LastFeedbackNote string
}

// IsValidAt reports whether the claim was in force at instant t.
// A claim is in force while ValidFrom ≤ t and (ValidTo is zero OR t < ValidTo).
// Zero ValidFrom counts as "valid from the beginning of time" so legacy
// rows that predate v0.8 still answer "yes" to current queries.
func (c Belief) IsValidAt(t time.Time) bool {
	if !c.ValidFrom.IsZero() && t.Before(c.ValidFrom) {
		return false
	}
	if !c.ValidTo.IsZero() && !t.Before(c.ValidTo) {
		return false
	}
	return true
}

// IsSuperseded reports whether the claim has been replaced by another
// (i.e., ValidTo is set). Useful for filtering history-aware queries.
func (c Belief) IsSuperseded() bool {
	return !c.ValidTo.IsZero()
}

// HalfLifeClassified reports whether a volatility classifier has assigned this
// belief's half-life (ADR 0025).
//
// This is the question HalfLifeDays alone cannot answer: a belief the
// classifier read and judged durable stores 0, and so does one nothing has
// ever looked at. Callers wanting "what work remains" must ask this, never
// `HalfLifeDays == 0` — that predicate matches 95% of a fully-classified brain
// and is why the backfill could never terminate.
func (c Belief) HalfLifeClassified() bool {
	return c.HalfLifeClassifier != ""
}

// BeliefEvidence links a Belief to the Episode that supports it.
type BeliefEvidence struct {
	ClaimID string
	EventID string
}

// ClaimEvidence is the pre-ADR-0011 name for BeliefEvidence; kept as a
// back-compat alias (remove at API v2).
type ClaimEvidence = BeliefEvidence

// Association represents a directed edge between two beliefs.
type Association struct {
	ID          string
	Type        RelationshipType
	FromClaimID string
	ToClaimID   string
	CreatedAt   time.Time
	CreatedBy   string // user id of the actor that created this association
	// Strength is the Hebbian co-activation weight of the edge (ADR 0015 §4):
	// how often the two beliefs have fired together at retrieval. It starts at the
	// base 1.0 (one association) and StrengthenAssociations increments it; spreading
	// activation weights each edge by it so well-worn associations prime more strongly.
	// A zero value reads as the base 1.0 (unset), so pre-strength edges are neutral.
	Strength float64
}

// EffectiveStrength returns the edge's Hebbian strength, treating the zero value as
// the base 1.0 so an edge that predates the strength column (or was created without
// one) is neutral rather than weightless.
func (a Association) EffectiveStrength() float64 {
	if a.Strength <= 0 {
		return 1
	}
	return a.Strength
}

// Relationship is the pre-ADR-0011 name for Association; kept as a
// back-compat alias (remove at API v2).
type Relationship = Association

// CompilationJob tracks the state of an asynchronous compilation task.
type CompilationJob struct {
	ID        string
	Kind      string
	Status    string
	Scope     map[string]string
	StartedAt time.Time
	UpdatedAt time.Time
	Error     string
}

// ClaimStatusTransition records a single status change on a claim. An
// ordered series of these forms a claim's lifecycle history: when a claim
// first appears as active, when it becomes contested, when it resolves or
// is deprecated, and why.
type ClaimStatusTransition struct {
	ClaimID    string
	FromStatus ClaimStatus // empty string means "initial state, no prior"
	ToStatus   ClaimStatus
	ChangedAt  time.Time
	Reason     string // free-form human context: "auto: contradiction detected", "resolved via mnemos resolve", etc.
	ChangedBy  string // user id of the actor that triggered the transition
}

// SystemUser is the sentinel actor recorded on rows that were written by
// internal pipelines or pre-A.2 data (no real user identity attached).
// Treated specially by the audit and narrative output paths so it reads
// as "system" rather than as an unknown user id.
const SystemUser = "<system>"

// EntityType classifies a first-class node in the knowledge graph.
// Entities exist independently of the claims that mention them so we
// can answer "what do we know about X?" without scanning every claim.
// The set is intentionally small: a longer ontology trades user
// confusion for a marginal gain in retrieval. Future versions can add
// types when there's clear demand.
type EntityType string

// Canonical EntityType values. The set is intentionally small; new
// types should be added only when a real corpus needs them.
const (
	EntityTypePerson  EntityType = "person"
	EntityTypeOrg     EntityType = "org"
	EntityTypeProject EntityType = "project"
	EntityTypeProduct EntityType = "product"
	EntityTypePlace   EntityType = "place"
	EntityTypeConcept EntityType = "concept"
)

// Entity is a canonicalised noun-phrase that appears across one or more
// claims. The (NormalizedName, Type) pair is the dedup key; Name keeps
// the human-readable casing.
type Entity struct {
	ID             string
	Name           string
	NormalizedName string // lower-cased, whitespace-collapsed; the dedup key
	Type           EntityType
	CreatedAt      time.Time
	CreatedBy      string
}

// ClaimEntity links a Claim to an Entity. The Role field describes how
// the entity participates: "subject" (the claim is *about* this entity),
// "object" (the entity is acted on or referenced), "mention" (a passing
// reference, the default). Querying by entity returns claims regardless
// of role; the field is informational for now.
type ClaimEntity struct {
	ClaimID  string
	EntityID string
	Role     string
}

// NormalizeEntityName produces the dedup key for an entity name:
// lower-case, trimmed, internal whitespace collapsed. Kept in domain
// rather than storage because both extraction and querying need to
// produce the same key from raw input.
func NormalizeEntityName(name string) string {
	out := strings.ToLower(strings.TrimSpace(name))
	// Collapse runs of whitespace to a single space so "Felix  Geelhaar"
	// and "Felix Geelhaar" hash to the same canonical form.
	parts := strings.Fields(out)
	return strings.Join(parts, " ")
}

// UserStatus represents the lifecycle state of a user account.
type UserStatus string

// Supported UserStatus values.
const (
	UserStatusActive  UserStatus = "active"
	UserStatusRevoked UserStatus = "revoked"
)

// User represents a human or service identity that can authenticate
// against the Mnemos registry. Tokens are issued to users; every
// audit-bearing action records the issuing user as created_by.
//
// Scopes is the authorisation list embedded into tokens issued for
// this user. Empty scopes is treated as the legacy default (full
// access via "*") so pre-F.3 users keep working — F.3 added the
// column with a default of '["*"]', and unmarshalled empty slices
// are interpreted the same way at issuance time.
type User struct {
	ID        string
	Name      string
	Email     string
	Status    UserStatus
	Scopes    []string
	CreatedAt time.Time
}

// Validate checks that a User has the minimum required fields. Email
// uniqueness is enforced at the storage layer.
func (u User) Validate() error {
	if strings.TrimSpace(u.ID) == "" {
		return errors.New("user id is required")
	}
	if strings.TrimSpace(u.Name) == "" {
		return errors.New("user name is required")
	}
	if strings.TrimSpace(u.Email) == "" {
		return errors.New("user email is required")
	}
	if u.Status == "" {
		return errors.New("user status is required")
	}
	switch u.Status {
	case UserStatusActive, UserStatusRevoked:
	default:
		return fmt.Errorf("invalid user status %q", u.Status)
	}
	for _, s := range u.Scopes {
		if strings.TrimSpace(s) == "" {
			return errors.New("user scope entries must be non-empty")
		}
	}
	return nil
}

// RevokedToken records that a particular JWT (identified by its jti
// claim) is no longer valid before its natural expiry. Auth middleware
// consults this denylist on every request. Rows older than expires_at
// can be purged because the token would have expired anyway.
type RevokedToken struct {
	JTI       string
	RevokedAt time.Time
	ExpiresAt time.Time
}

// AgentStatus mirrors UserStatus for non-human principals.
type AgentStatus string

// Supported AgentStatus values.
const (
	AgentStatusActive  AgentStatus = "active"
	AgentStatusRevoked AgentStatus = "revoked"
)

// Scope strings recognised by the auth middleware. "*" matches every
// scope. Resource-level scopes follow `<resource>:<verb>` so future
// additions stay grep-friendly.
const (
	ScopeWildcard           = "*"
	ScopeEventsWrite        = "events:write"
	ScopeClaimsWrite        = "claims:write"
	ScopeRelationshipsWrite = "relationships:write"
	ScopeEmbeddingsWrite    = "embeddings:write"
	// ScopePromoteGlobal is the curator capability (ADR 0012): a token
	// bearing it may take the CURATED single-source promotion path, pushing a
	// novel class-level fact into the shared global brain from one source
	// (bypassing cross-tenant corroboration). Without it, only the emergent
	// (corroborated) promotion path is available. Modelled as a distinct scope
	// so "contribute to global" is a granted vet/operator capability rather
	// than something every tenant user holds.
	ScopePromoteGlobal = "promote:global"
)

// Agent represents a non-human principal — a coding assistant, CI job,
// or other automated identity. Agents always have an owning user (so
// every action traces back to a human accountable party) and an
// explicit scope list. There is no "implicit *" for agents: tokens
// issued for an agent carry exactly the scopes recorded on the agent,
// nothing more.
//
// AllowedRuns optionally restricts the agent to a whitelist of run
// ids. Empty list means "every run is allowed". Entries support
// shell-glob patterns (matched via [path.Match]) so a single agent
// can scope to a class of runs without listing every concrete ID:
// `prod-*`, `nightly-?-2026`, `release/[0-9]*`. The whitelist gates
// write paths that carry a run_id (today: events); claim /
// relationship / embedding writes indirectly inherit because the
// agent must be able to seed the underlying event first.
//
// Quota optionally caps how much an agent can write per rolling
// window. Zero values mean "no limit". Counters live on the agent
// row and are incremented after every successful write.
type Agent struct {
	ID          string
	Name        string
	OwnerID     string // user_id of the human accountable for this agent
	Scopes      []string
	AllowedRuns []string
	Quota       AgentQuota
	Status      AgentStatus
	CreatedAt   time.Time

	// AuthorityScore is a 0.0–1.0 signal of how much trust claims
	// submitted by this agent should receive. It is derived from
	// SuccessRate and VerificationCount and updated asynchronously;
	// it is not the caller's job to compute it on write.
	AuthorityScore float64

	// SuccessRate is the fraction of this agent's claims that were
	// subsequently corroborated by independent evidence (verified /
	// total). Zero when no data has been collected yet.
	SuccessRate float64

	// VerificationCount is the total number of claims from this
	// agent that have been independently verified. Used to weight
	// SuccessRate: a 100% rate on 3 claims is much weaker than 80%
	// on 500.
	VerificationCount int
}

// AgentQuota caps an agent's write volume. WindowSeconds is the
// rolling window (e.g. 86400 for "per day"); MaxWrites caps the
// number of write RPCs in that window; MaxTokens caps the cumulative
// LLM token spend reported by the axi-go capability evidence chain
// (zero means uncapped).
type AgentQuota struct {
	WindowSeconds int64 `json:"window_seconds,omitempty"`
	MaxWrites     int64 `json:"max_writes,omitempty"`
	MaxTokens     int64 `json:"max_tokens,omitempty"`
}

// IsZero reports whether the quota imposes any limit.
func (q AgentQuota) IsZero() bool {
	return q.WindowSeconds == 0 && q.MaxWrites == 0 && q.MaxTokens == 0
}

// AgentUsage carries the rolling counters paired with the quota
// configured on Agent. Persistence implementations refresh the
// window when WindowStart + WindowSeconds is in the past.
type AgentUsage struct {
	AgentID     string
	WindowStart time.Time
	Writes      int64
	Tokens      int64
}

// Validate enforces the minimum invariants for a persistable Agent.
// Scope strings are not validated against the constant list — agents
// may legitimately carry forward-compatible scopes the current binary
// doesn't yet recognise.
func (a Agent) Validate() error {
	if strings.TrimSpace(a.ID) == "" {
		return errors.New("agent id is required")
	}
	if strings.TrimSpace(a.Name) == "" {
		return errors.New("agent name is required")
	}
	if strings.TrimSpace(a.OwnerID) == "" {
		return errors.New("agent owner_id is required")
	}
	if a.Status == "" {
		return errors.New("agent status is required")
	}
	switch a.Status {
	case AgentStatusActive, AgentStatusRevoked:
	default:
		return fmt.Errorf("invalid agent status %q", a.Status)
	}
	for _, s := range a.Scopes {
		if strings.TrimSpace(s) == "" {
			return errors.New("agent scope entries must be non-empty")
		}
	}
	for _, r := range a.AllowedRuns {
		if strings.TrimSpace(r) == "" {
			return errors.New("agent allowed_runs entries must be non-empty")
		}
	}
	return nil
}

// EmbeddingRecord holds a stored vector embedding with its metadata.
type EmbeddingRecord struct {
	EntityID   string
	EntityType string
	Vector     []float32
	Model      string
	Dimensions int
	CreatedAt  time.Time // when this vector was last (re)written; used by audit-who principal scans
	CreatedBy  string    // user id of the actor that generated this embedding
}

// VerdictAction describes what the engine recommends an agent do after
// resolving a contradiction.
type VerdictAction string

// Supported VerdictAction values.
const (
	// VerdictActionTrust indicates the winning claim should be trusted and
	// acted on; the loser has been demoted.
	VerdictActionTrust VerdictAction = "trust"
	// VerdictActionUpdate indicates the agent should update its beliefs to
	// reflect the winning claim before acting.
	VerdictActionUpdate VerdictAction = "update"
	// VerdictActionEscalate indicates the engine could not resolve the
	// contradiction automatically and a human must decide.
	VerdictActionEscalate VerdictAction = "escalate"
)

// Verdict is the structured resolution output for agent consumers. One Verdict
// is produced per contradicting claim pair. When Action is VerdictActionEscalate
// the WinnerClaimID/LoserClaimID fields are empty and EscalationReason explains why.
type Verdict struct {
	// WinnerClaimID is the ID of the claim the engine selected as authoritative.
	// Empty when Action is VerdictActionEscalate.
	WinnerClaimID string `json:"winner_claim_id"`
	// LoserClaimID is the ID of the demoted claim.
	// Empty when Action is VerdictActionEscalate.
	LoserClaimID string `json:"loser_claim_id"`
	// Confidence is the structural confidence of the winning claim (0–1).
	Confidence float64 `json:"confidence"`
	// Rationale explains which provenance signals tipped the scale.
	// E.g. "recency: winner 3d vs loser 47d; authority: 0.92 vs 0.71".
	Rationale string `json:"rationale"`
	// Action is the recommended agent action.
	Action VerdictAction `json:"action"`
	// EscalationReason is non-empty when Action is VerdictActionEscalate and
	// explains why automatic resolution was not possible.
	EscalationReason string `json:"escalation_reason,omitempty"`
}

// Consumer identifies who is consuming a query answer. It controls how
// contradictions are handled: agents receive an automatic resolution verdict
// so they can act without ambiguity; human users receive a full explanation
// of the contradiction so they can reason about it themselves.
type Consumer string

const (
	// ConsumerAgent requests automatic contradiction resolution using the
	// trust scoring engine. The winning claim is surfaced; the losing claim
	// is demoted. AutoResolved is set to true in the Answer.
	ConsumerAgent Consumer = "agent"
	// ConsumerUser requests that contradictions be surfaced with a human-
	// readable explanation. AutoResolved is false; ContradictionExplanation
	// is populated.
	ConsumerUser Consumer = "user"
)

// Answer holds the result of a query, including supporting claims and contradictions.
type Answer struct {
	AnswerText       string
	Claims           []Belief
	Contradictions   []Association
	TimelineEventIDs []string
	// ClaimProvenance maps claim ID to a human-readable origin: "local"
	// for claims sourced from this project's events, or "<registry-url>"
	// for claims that reached the local DB via `mnemos pull`. Empty means
	// unknown — the engine fills this in when it can.
	ClaimProvenance map[string]string
	// ClaimHopDistance maps claim ID to the BFS hop distance from the
	// directly-retrieved claims. 0 means the claim came from the top-ranked
	// events; 1 means it was reached by following one supports/contradicts
	// edge from a hop-0 claim, etc. Empty when hop expansion was not
	// requested.
	ClaimHopDistance map[string]int
	// StaleClaimIDs lists claim ids whose freshness factor has
	// decayed below the trust floor — i.e. the most recent evidence
	// or last_verified signal is old enough that the claim should
	// not be acted on without re-verification. Empty when no claims
	// in the answer are stale; nil when the engine could not compute
	// staleness (e.g. timestamps absent).
	StaleClaimIDs []string
	// AutoResolved is true when the engine automatically resolved one or
	// more contradictions on behalf of an agent consumer. The Claims slice
	// contains only the winning claims; demoted claims are omitted.
	AutoResolved bool
	// ContradictionExplanation is a human-readable explanation of any
	// unresolved contradictions surfaced for a user consumer. Empty when
	// there are no contradictions or when the consumer is ConsumerAgent.
	ContradictionExplanation string
	// Verdicts contains one structured resolution entry per contradicting
	// claim pair. Only populated when the consumer is ConsumerAgent.
	// Agents should inspect Action on each Verdict to decide whether to
	// trust, update beliefs, or escalate to a human.
	Verdicts []Verdict
	// Confidence is a 0–1 score indicating how confident the system is
	// in the answer, based on evidence quality (trust scores, citation
	// density, contradiction presence, and recency). A value ≥ 0.9
	// means the answer is "never wrong on recall" grade.
	Confidence float64
}

// Validate checks that the Belief has a non-empty ID and text, a confidence
// between 0 and 1, and a valid type and status.
func (c Belief) Validate() error {
	if strings.TrimSpace(c.ID) == "" {
		return errors.New("claim id is required")
	}
	if strings.TrimSpace(c.Text) == "" {
		return errors.New("claim text is required")
	}
	if c.Confidence < 0 || c.Confidence > 1 {
		return errors.New("claim confidence must be between 0 and 1")
	}
	// A type is required, but the KNOWN-type check (built-in ∪ consumer-registered
	// via WithClaimTypes) lives at the store write boundary where the configured
	// vocabulary is available — the domain can't see the registry. Internal
	// writers only ever produce built-ins, so a non-empty check suffices here.
	if strings.TrimSpace(string(c.Type)) == "" {
		return errors.New("claim type is required")
	}
	if c.Type == ClaimTypeTestResult && strings.TrimSpace(c.TestID) == "" {
		return errors.New("claim test_id is required for test_result type")
	}
	switch c.Status {
	case ClaimStatusActive, ClaimStatusContested, ClaimStatusResolved, ClaimStatusDeprecated:
	default:
		return errors.New("claim status is invalid")
	}
	for k, v := range c.ConfidenceComponents {
		if strings.TrimSpace(k) == "" {
			return errors.New("confidence_components key must be non-empty")
		}
		// Credit-assignment entries (ADR 0014) are SIGNED trust deltas — a refuted
		// prediction blames its beliefs with a negative contribution — so they may
		// range over [-1, 1]. All other components are confidence contributors in
		// [0, 1].
		if strings.HasPrefix(k, CreditComponentPrefix) {
			if v < -1 || v > 1 {
				return fmt.Errorf("confidence_components[%q] must be between -1 and 1", k)
			}
			continue
		}
		if v < 0 || v > 1 {
			return fmt.Errorf("confidence_components[%q] must be between 0 and 1", k)
		}
	}
	return nil
}

// IncidentSeverity classifies the impact of an incident.
type IncidentSeverity string

const (
	// IncidentSeverityCritical — service is down or data is lost.
	IncidentSeverityCritical IncidentSeverity = "critical"
	// IncidentSeverityHigh — major functionality degraded.
	IncidentSeverityHigh IncidentSeverity = "high"
	// IncidentSeverityMedium — partial degradation, workaround exists.
	IncidentSeverityMedium IncidentSeverity = "medium"
	// IncidentSeverityLow — minor impact, monitored.
	IncidentSeverityLow IncidentSeverity = "low"
)

// IncidentStatus represents the lifecycle state of an incident.
type IncidentStatus string

const (
	// IncidentStatusOpen — incident is active and being investigated.
	IncidentStatusOpen IncidentStatus = "open"
	// IncidentStatusResolved — incident has been mitigated and verified.
	IncidentStatusResolved IncidentStatus = "resolved"
	// IncidentStatusPostmortem — incident closed; postmortem complete.
	IncidentStatusPostmortem IncidentStatus = "postmortem"
)

// Incident records a failure event with a structured timeline, root-cause
// claim, linked decisions, and associated outcomes. It is the primary
// entry point for "Why Were We Wrong?" analysis: an agent or operator
// opens an Incident, attaches the claim that turned out to be wrong
// (RootCauseClaimID), the decisions made on that belief (DecisionIDs),
// and the outcomes that proved the belief false (OutcomeIDs). The
// synthesise engine can then generate anti-lessons automatically.
type Incident struct {
	// ID is a caller-supplied stable identifier (UUID or human-readable slug).
	ID string `json:"id"`
	// Title is a short human-readable label for the incident.
	Title string `json:"title"`
	// Summary is a prose description of what happened and what was affected.
	Summary string `json:"summary"`
	// Severity classifies the blast radius / impact.
	Severity IncidentSeverity `json:"severity"`
	// Status tracks the incident lifecycle.
	Status IncidentStatus `json:"status"`
	// TimelineEventIDs is an ordered list of event IDs that constitute the
	// evidence timeline for this incident (detection, mitigation, resolution).
	TimelineEventIDs []string `json:"timeline_event_ids,omitempty"`
	// RootCauseClaimID points at the claim that was believed to be true
	// but turned out to be wrong — the epistemic root cause.
	RootCauseClaimID string `json:"root_cause_claim_id,omitempty"`
	// DecisionIDs links the decisions that were made on the basis of the
	// now-refuted belief.
	DecisionIDs []string `json:"decision_ids,omitempty"`
	// OutcomeIDs links the observed outcomes that proved the belief wrong.
	OutcomeIDs []string `json:"outcome_ids,omitempty"`
	// PlaybookID is set once a playbook has been synthesised from this
	// incident's lessons.
	PlaybookID string `json:"playbook_id,omitempty"`
	// OpenedAt is when the incident was first recorded.
	OpenedAt time.Time `json:"opened_at"`
	// ResolvedAt is when the incident was closed; zero if still open.
	ResolvedAt time.Time `json:"resolved_at,omitempty"`
	// CreatedBy is the principal (user or agent ID) that opened the incident.
	CreatedBy string `json:"created_by,omitempty"`
}

// Validate checks that an Incident has the minimum required fields.
func (i Incident) Validate() error {
	if strings.TrimSpace(i.ID) == "" {
		return errors.New("incident id is required")
	}
	if strings.TrimSpace(i.Title) == "" {
		return errors.New("incident title is required")
	}
	switch i.Severity {
	case IncidentSeverityCritical, IncidentSeverityHigh, IncidentSeverityMedium, IncidentSeverityLow:
	default:
		return fmt.Errorf("incident severity %q is invalid", i.Severity)
	}
	switch i.Status {
	case IncidentStatusOpen, IncidentStatusResolved, IncidentStatusPostmortem:
	default:
		return fmt.Errorf("incident status %q is invalid", i.Status)
	}
	if i.OpenedAt.IsZero() {
		return errors.New("incident opened_at is required")
	}
	return nil
}

// ProvenanceSignal is a named component of a credibility score breakdown.
// Each signal carries its raw value and a human-readable label so callers
// can surface exactly which factors drove the trust decision.
type ProvenanceSignal struct {
	// Name is the signal identifier (e.g. "authority", "recency", "citations").
	Name string `json:"name"`
	// Value is the raw signal value before weighting (0.0–1.0 unless otherwise noted).
	Value float64 `json:"value"`
	// Weight is the fractional contribution of this signal to the final score.
	Weight float64 `json:"weight"`
	// Contribution is Value × Weight — the signal's additive share of the score.
	Contribution float64 `json:"contribution"`
	// Detail is a short prose note explaining the value (e.g. "3 citations", "47 days old").
	Detail string `json:"detail,omitempty"`
}

// ProvenanceReport is the structured output of Engine.WhyTrustClaim. It
// explains the credibility decision for a single claim in machine-readable
// form (Signals, Score) and human-readable form (Rationale).
type ProvenanceReport struct {
	// ClaimID is the claim this report describes.
	ClaimID string `json:"claim_id"`
	// ClaimText is the verbatim claim text for display without a second lookup.
	ClaimText string `json:"claim_text"`
	// Score is the final credibility score (0.0–1.0) after all signals are combined.
	Score float64 `json:"score"`
	// Signals is the per-component breakdown in contribution-descending order.
	Signals []ProvenanceSignal `json:"signals"`
	// Rationale is the compact, machine-friendly rationale string returned
	// by trust.ScoreCredibility — engineer shorthand suitable for tooling
	// and dashboards. Pair with ProseRationale when surfacing to humans.
	Rationale string `json:"rationale"`
	// ProseRationale is a plain-English explanation of the trust decision
	// suitable for non-technical operators ("Last ran 12 days ago.
	// Passed 8 of 10 runs. Live test."). Always populated when Score is.
	ProseRationale string `json:"prose_rationale,omitempty"`
	// SourceDocument is the primary source of the claim, for attribution.
	SourceDocument string `json:"source_document,omitempty"`
	// Liveness is the evaluated liveness status of the source at query time.
	Liveness LivenessStatus `json:"liveness,omitempty"`
}

// Validate checks that both ClaimID and EventID are non-empty.
func (e BeliefEvidence) Validate() error {
	if strings.TrimSpace(e.ClaimID) == "" {
		return errors.New("claim evidence claim_id is required")
	}
	if strings.TrimSpace(e.EventID) == "" {
		return errors.New("claim evidence event_id is required")
	}
	return nil
}

// MaxEventContentBytes caps a single event's Content field. Events
// are meant to be small paragraph-sized fragments of knowledge, not
// entire documents — the ingest pipeline normalises documents into
// sentence-level events. Anything much larger almost always means
// the caller didn't chunk properly, and an unbounded field wastes
// DB/index space plus makes extraction latency pathological. Keep
// this comfortably larger than a typical paragraph (2KB) so
// legitimate edge cases still fit.
const MaxEventContentBytes = 64 * 1024

// Validate checks that an Episode has the minimum required fields and
// that Content stays within MaxEventContentBytes.
func (e Episode) Validate() error {
	if strings.TrimSpace(e.ID) == "" {
		return errors.New("event id is required")
	}
	if strings.TrimSpace(e.Content) == "" {
		return errors.New("event content is required")
	}
	if len(e.Content) > MaxEventContentBytes {
		return fmt.Errorf("event content is %d bytes, max is %d (chunk longer documents into multiple events)", len(e.Content), MaxEventContentBytes)
	}
	if strings.TrimSpace(e.SourceInputID) == "" {
		return errors.New("event source_input_id is required")
	}
	if e.Timestamp.IsZero() {
		return errors.New("event timestamp is required")
	}
	return nil
}

// Validate checks that an Association has the required fields and a
// valid type, and that it doesn't self-reference (a belief can't
// support or contradict itself — that's either a no-op or a bug).
func (r Association) Validate() error {
	if strings.TrimSpace(r.ID) == "" {
		return errors.New("relationship id is required")
	}
	if strings.TrimSpace(r.FromClaimID) == "" {
		return errors.New("relationship from_claim_id is required")
	}
	if strings.TrimSpace(r.ToClaimID) == "" {
		return errors.New("relationship to_claim_id is required")
	}
	if r.FromClaimID == r.ToClaimID {
		return fmt.Errorf("relationship %s self-references claim %s", r.ID, r.FromClaimID)
	}
	if !IsValidRelationshipType(r.Type) {
		return fmt.Errorf("relationship type %q invalid", r.Type)
	}
	if r.Strength < 0 {
		return fmt.Errorf("relationship %s strength %v is negative", r.ID, r.Strength)
	}
	return nil
}
