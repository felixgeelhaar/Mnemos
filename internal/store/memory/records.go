package memory

import (
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// Stored records are the in-memory analogues of the SQLite rows.
// Storing domain.X directly would let callers mutate persisted state
// through a shared map slot — every write copies into a stored*
// variant and every read copies back out so the maps own the data.

type storedEvent struct {
	ID            string
	RunID         string
	SchemaVersion string
	Content       string
	SourceInputID string
	Timestamp     time.Time
	Metadata      map[string]string
	IngestedAt    time.Time
	CreatedBy     string
}

func (e storedEvent) toDomain() domain.Event {
	return domain.Event{
		ID:            e.ID,
		RunID:         e.RunID,
		SchemaVersion: e.SchemaVersion,
		Content:       e.Content,
		SourceInputID: e.SourceInputID,
		Timestamp:     e.Timestamp,
		Metadata:      copyStringMap(e.Metadata),
		IngestedAt:    e.IngestedAt,
		CreatedBy:     e.CreatedBy,
	}
}

func storedEventFromDomain(e domain.Event) storedEvent {
	return storedEvent{
		ID:            e.ID,
		RunID:         e.RunID,
		SchemaVersion: e.SchemaVersion,
		Content:       e.Content,
		SourceInputID: e.SourceInputID,
		Timestamp:     e.Timestamp.UTC(),
		Metadata:      copyStringMap(e.Metadata),
		IngestedAt:    e.IngestedAt.UTC(),
		CreatedBy:     actorOr(e.CreatedBy),
	}
}

type storedAction struct {
	ID        string
	RunID     string
	Kind      domain.ActionKind
	Subject   string
	Actor     string
	At        time.Time
	Metadata  map[string]string
	CreatedBy string
	CreatedAt time.Time
}

func (a storedAction) toDomain() domain.Action {
	return domain.Action{
		ID:        a.ID,
		RunID:     a.RunID,
		Kind:      a.Kind,
		Subject:   a.Subject,
		Actor:     a.Actor,
		At:        a.At,
		Metadata:  copyStringMap(a.Metadata),
		CreatedBy: a.CreatedBy,
		CreatedAt: a.CreatedAt,
	}
}

func storedActionFromDomain(a domain.Action) storedAction {
	createdAt := a.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	return storedAction{
		ID:        a.ID,
		RunID:     a.RunID,
		Kind:      a.Kind,
		Subject:   a.Subject,
		Actor:     a.Actor,
		At:        a.At.UTC(),
		Metadata:  copyStringMap(a.Metadata),
		CreatedBy: actorOr(a.CreatedBy),
		CreatedAt: createdAt.UTC(),
	}
}

type storedOutcome struct {
	ID         string
	ActionID   string
	Result     domain.OutcomeResult
	Metrics    map[string]float64
	Notes      string
	ObservedAt time.Time
	Source     string
	CreatedBy  string
	CreatedAt  time.Time
}

func (o storedOutcome) toDomain() domain.Outcome {
	return domain.Outcome{
		ID:         o.ID,
		ActionID:   o.ActionID,
		Result:     o.Result,
		Metrics:    copyFloatMap(o.Metrics),
		Notes:      o.Notes,
		ObservedAt: o.ObservedAt,
		Source:     o.Source,
		CreatedBy:  o.CreatedBy,
		CreatedAt:  o.CreatedAt,
	}
}

func storedOutcomeFromDomain(o domain.Outcome) storedOutcome {
	createdAt := o.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	source := o.Source
	if source == "" {
		source = "push"
	}
	return storedOutcome{
		ID:         o.ID,
		ActionID:   o.ActionID,
		Result:     o.Result,
		Metrics:    copyFloatMap(o.Metrics),
		Notes:      o.Notes,
		ObservedAt: o.ObservedAt.UTC(),
		Source:     source,
		CreatedBy:  actorOr(o.CreatedBy),
		CreatedAt:  createdAt.UTC(),
	}
}

func copyFloatMap(in map[string]float64) map[string]float64 {
	if in == nil {
		return nil
	}
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// storedClaim mirrors the claims row. EVERY domain.Claim field must have a
// slot here: a field this struct omits is silently dropped on write and reads
// back as the zero value, which is not "missing data" but WRONG data — an
// omitted Visibility read back as "" is coerced to team downstream, so a
// personal belief leaks into team answers. TestStoredClaim_RoundTripsEveryField
// reflects over domain.Claim and fails when a new field is added without a slot.
type storedClaim struct {
	ID                   string
	Text                 string
	Type                 domain.ClaimType
	Confidence           float64
	Status               domain.ClaimStatus
	CreatedAt            time.Time
	CreatedBy            string
	TrustScore           float64
	ValidFrom            time.Time
	ValidTo              time.Time
	LastVerified         time.Time
	VerifyCount          int
	HalfLifeDays         float64
	Scope                domain.Scope
	ConfidenceComponents map[string]float64
	Lifecycle            domain.ClaimLifecycle
	SubjectClass         domain.SubjectClass
	Durability           domain.Durability

	// Epistemic provenance.
	SourceDocument      string
	SourceType          domain.SourceType
	SourceAuthority     float64
	Liveness            domain.LivenessStatus
	LastExecuted        time.Time
	CitationCount       int
	ProvenanceRationale string

	// Test provenance (Type == test_result).
	TestID             string
	TestRequirementRef string
	TestAuthor         string
	TestLastModified   time.Time
	TestLastRunAt      time.Time
	TestPassCount      int
	TestFailCount      int

	// Visibility is normalised on write (see visibilityOrDefault), matching
	// the SQLite column default, so reads never hand back an empty audience.
	Visibility domain.Visibility
}

func (c storedClaim) toDomain() domain.Claim {
	var components map[string]float64
	if len(c.ConfidenceComponents) > 0 {
		components = make(map[string]float64, len(c.ConfidenceComponents))
		for k, v := range c.ConfidenceComponents {
			components[k] = v
		}
	}
	return domain.Claim{
		ID:                   c.ID,
		Text:                 c.Text,
		Type:                 c.Type,
		Confidence:           c.Confidence,
		Status:               c.Status,
		CreatedAt:            c.CreatedAt,
		CreatedBy:            c.CreatedBy,
		TrustScore:           c.TrustScore,
		ValidFrom:            c.ValidFrom,
		ValidTo:              c.ValidTo,
		LastVerified:         c.LastVerified,
		VerifyCount:          c.VerifyCount,
		HalfLifeDays:         c.HalfLifeDays,
		Scope:                c.Scope,
		ConfidenceComponents: components,
		Lifecycle:            c.Lifecycle,
		SubjectClass:         c.SubjectClass,
		Durability:           c.Durability,

		SourceDocument:      c.SourceDocument,
		SourceType:          c.SourceType,
		SourceAuthority:     c.SourceAuthority,
		Liveness:            c.Liveness,
		LastExecuted:        c.LastExecuted,
		CitationCount:       c.CitationCount,
		ProvenanceRationale: c.ProvenanceRationale,

		TestID:             c.TestID,
		TestRequirementRef: c.TestRequirementRef,
		TestAuthor:         c.TestAuthor,
		TestLastModified:   c.TestLastModified,
		TestLastRunAt:      c.TestLastRunAt,
		TestPassCount:      c.TestPassCount,
		TestFailCount:      c.TestFailCount,

		Visibility: c.Visibility,
	}
}

// visibilityOrDefault normalises a Visibility the way the SQLite backend's
// column default does: empty and unrecognised values become
// domain.DefaultVisibility ("team") so a read never returns an empty audience.
func visibilityOrDefault(v domain.Visibility) domain.Visibility {
	switch v {
	case domain.VisibilityPersonal, domain.VisibilityTeam, domain.VisibilityOrg:
		return v
	default:
		return domain.DefaultVisibility
	}
}

func storedClaimFromDomain(c domain.Claim) storedClaim {
	validFrom := c.ValidFrom
	if validFrom.IsZero() {
		// Mirror sqlite's behaviour: when callers omit ValidFrom, the
		// claim's CreatedAt is the floor. Pipeline normally fills this
		// in earlier from the source event's timestamp.
		validFrom = c.CreatedAt
	}
	// Copy the caller's components map so a later mutation by the
	// caller cannot retroactively change what's persisted.
	var components map[string]float64
	if len(c.ConfidenceComponents) > 0 {
		components = make(map[string]float64, len(c.ConfidenceComponents))
		for k, v := range c.ConfidenceComponents {
			components[k] = v
		}
	}
	return storedClaim{
		ID:                   c.ID,
		Text:                 c.Text,
		Type:                 c.Type,
		Confidence:           c.Confidence,
		Status:               c.Status,
		CreatedAt:            c.CreatedAt.UTC(),
		CreatedBy:            actorOr(c.CreatedBy),
		TrustScore:           c.TrustScore,
		ValidFrom:            validFrom.UTC(),
		ValidTo:              c.ValidTo.UTC(),
		LastVerified:         c.LastVerified.UTC(),
		VerifyCount:          c.VerifyCount,
		HalfLifeDays:         c.HalfLifeDays,
		Scope:                c.Scope,
		ConfidenceComponents: components,
		Lifecycle:            c.Lifecycle,
		SubjectClass:         c.SubjectClass,
		Durability:           c.Durability,

		SourceDocument:      c.SourceDocument,
		SourceType:          c.SourceType,
		SourceAuthority:     c.SourceAuthority,
		Liveness:            c.Liveness,
		LastExecuted:        c.LastExecuted.UTC(),
		CitationCount:       c.CitationCount,
		ProvenanceRationale: c.ProvenanceRationale,

		TestID:             c.TestID,
		TestRequirementRef: c.TestRequirementRef,
		TestAuthor:         c.TestAuthor,
		TestLastModified:   c.TestLastModified.UTC(),
		TestLastRunAt:      c.TestLastRunAt.UTC(),
		TestPassCount:      c.TestPassCount,
		TestFailCount:      c.TestFailCount,

		Visibility: visibilityOrDefault(c.Visibility),
	}
}

type storedTransition struct {
	ClaimID    string
	FromStatus domain.ClaimStatus
	ToStatus   domain.ClaimStatus
	ChangedAt  time.Time
	Reason     string
	ChangedBy  string
}

type storedRelationship struct {
	ID          string
	Type        domain.RelationshipType
	FromClaimID string
	ToClaimID   string
	CreatedAt   time.Time
	CreatedBy   string
	Strength    float64 // Hebbian co-activation weight (ADR 0015 §4); 0 reads as base 1.0
}

type embeddingKey struct {
	EntityID   string
	EntityType string
}

type storedEmbedding struct {
	EntityID   string
	EntityType string
	Vector     []float32
	Model      string
	Dimensions int
	CreatedAt  time.Time
	CreatedBy  string
}

type storedUser struct {
	ID        string
	Name      string
	Email     string
	Status    domain.UserStatus
	Scopes    []string
	CreatedAt time.Time
}

type storedRevokedToken struct {
	JTI       string
	RevokedAt time.Time
	ExpiresAt time.Time
}

type storedAgent struct {
	ID          string
	Name        string
	OwnerID     string
	Scopes      []string
	AllowedRuns []string
	Status      domain.AgentStatus
	CreatedAt   time.Time
}

type storedEntity struct {
	ID             string
	Name           string
	NormalizedName string
	Type           domain.EntityType
	CreatedAt      time.Time
	CreatedBy      string
}

func (e storedEntity) toDomain() domain.Entity {
	return domain.Entity{
		ID:             e.ID,
		Name:           e.Name,
		NormalizedName: e.NormalizedName,
		Type:           e.Type,
		CreatedAt:      e.CreatedAt,
		CreatedBy:      e.CreatedBy,
	}
}

// entityKey is the (normalized_name, type) tuple that mirrors the
// SQLite UNIQUE(normalized_name, type) index — the dedup contract
// for FindOrCreate.
type entityKey struct {
	NormalizedName string
	Type           domain.EntityType
}

// claimEntityKey is the (claim_id, entity_id, role) tuple that
// mirrors UNIQUE(claim_id, entity_id, role) on the link table.
type claimEntityKey struct {
	ClaimID  string
	EntityID string
	Role     string
}

type storedCompilationJob struct {
	ID        string
	Kind      string
	Status    string
	Scope     map[string]string
	StartedAt time.Time
	UpdatedAt time.Time
	Error     string
}

func (j storedCompilationJob) toDomain() domain.CompilationJob {
	return domain.CompilationJob{
		ID:        j.ID,
		Kind:      j.Kind,
		Status:    j.Status,
		Scope:     copyStringMap(j.Scope),
		StartedAt: j.StartedAt,
		UpdatedAt: j.UpdatedAt,
		Error:     j.Error,
	}
}

func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyStringSlice(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func copyFloat32Slice(in []float32) []float32 {
	if in == nil {
		return nil
	}
	out := make([]float32, len(in))
	copy(out, in)
	return out
}
