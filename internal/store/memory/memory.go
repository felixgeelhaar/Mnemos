// Package memory implements an in-process [store] provider whose
// repositories live entirely in Go maps guarded by a single
// sync.RWMutex. It is designed for two use cases:
//
//  1. Fast, hermetic tests: opening a memory:// DSN replaces the
//     temp-SQLite-file pattern that the rest of the codebase still
//     uses, with no on-disk side effects.
//  2. Embedded use from Nous (the cognitive-stack coordinator) where
//     a calling process wants Mnemos in-process without standing up
//     a SQLite file.
//
// The provider implements every port-typed repository in
// [go.klarlabs.de/mnemos/internal/ports] so a `Conn` opened
// here is a drop-in replacement for the SQLite Conn from a port-typed
// caller's perspective. Provider-specific extras (sql.DB raw handle,
// FTS5, sqlite-vss) are intentionally absent — callers that need
// those should open a sqlite:// DSN.
//
// Drop-in parity includes FIELD parity, not just method parity: the
// stored* records in records.go must carry every field of the domain
// type they persist. Dropping one is worse than a missing method,
// because the read path returns a plausible zero value instead of an
// error — see TestStoredClaim_RoundTripsEveryField, which reflects over
// domain.Claim and fails when a new field is added without a slot.
//
// See docs/adr/0001-multi-backend-storage.md for the contract.
package memory

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/store"
)

// Register the memory provider with the top-level store factory.
// Memory state is per-Open: each call to store.Open("memory://...")
// returns a fresh, empty Conn. The DSN's path/query is currently
// ignored beyond the scheme check; future work will honour
// ?namespace=foo by partitioning the maps.
func init() {
	store.Register("memory", openProvider)
}

// namespaceRE mirrors ADR 0001 §3: lowercase, alphanumeric+underscore,
// must start with a letter, max 63 bytes.
var namespaceRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

const defaultNamespace = "mnemos"

// openProvider parses a memory:// DSN and returns a Conn with all
// port-typed repositories backed by a fresh shared state struct. The
// state lives only as long as the Conn — Close releases the
// reference, after which repositories tied to the Conn must not be
// used.
func openProvider(_ context.Context, dsn string) (*store.Conn, error) {
	if !strings.HasPrefix(dsn, "memory://") {
		return nil, fmt.Errorf("memory: not a memory dsn: %q", dsn)
	}

	ns := defaultNamespace
	if u, err := url.Parse(dsn); err == nil {
		if qns := u.Query().Get("namespace"); qns != "" {
			ns = qns
		}
	}
	if !namespaceRE.MatchString(ns) {
		return nil, fmt.Errorf("memory: invalid namespace %q (want %s)", ns, namespaceRE.String())
	}

	// Namespace is validated but not stored — each memory:// open gets a
	// fresh isolated state, so isolation is by instance rather than key prefix.
	_ = ns
	st := newState()
	return &store.Conn{
		Events:        EventRepository{state: st},
		Claims:        ClaimRepository{state: st},
		Relationships: RelationshipRepository{state: st},
		Embeddings:    EmbeddingRepository{state: st},
		Users:         UserRepository{state: st},
		RevokedTokens: RevokedTokenRepository{state: st},
		Agents:        AgentRepository{state: st},
		Entities:      EntityRepository{state: st},
		Jobs:          CompilationJobRepository{state: st},
		Actions:       ActionRepository{state: st},
		Outcomes:      OutcomeRepository{state: st},
		Lessons:       LessonRepository{state: st},
		Decisions:     DecisionRepository{state: st},
		Playbooks:     PlaybookRepository{state: st},
		EntityRels:    EntityRelationshipRepository{state: st},
		Incidents:     IncidentRepository{state: st},
		Feedback:      FeedbackRepository{state: st},
		Blocks:        BlockRepository{state: st},
		Expectations:  ExpectationRepository{state: st},
		GlobalSchemas: GlobalSchemaRepository{state: st},
		Journal:       JournalRepository{state: st},
		ClaimVersions: ClaimVersionRepository{state: st},
		Raw:           st,
		Closer:        func() error { st.clear(); return nil },
	}, nil
}

// state is the shared in-memory backing store. Every repository
// returned for a single Conn shares the same state pointer so writes
// through one repo are visible through another, mirroring SQLite's
// single-database semantics. The mutex protects every field; we
// favour a single coarse lock over per-field locks because the
// memory provider is for tests and embedding, not production
// throughput.
type state struct {
	mu sync.RWMutex

	events           map[string]storedEvent
	eventOrder       []string // insertion order, for ListAll
	claims           map[string]storedClaim
	claimOrder       []string
	statusHistory    map[string][]storedTransition  // claim_id -> transitions in insertion order
	evidence         map[string]map[string]struct{} // claim_id -> set of event_ids (de-duped)
	relationships    map[string]storedRelationship
	embeddings       map[embeddingKey]storedEmbedding
	users            map[string]storedUser
	userOrder        []string
	usersByEmail     map[string]string // email -> user_id
	revokedTokens    map[string]storedRevokedToken
	agents           map[string]storedAgent
	agentOrder       []string
	entities         map[string]storedEntity
	entityOrder      []string                  // insertion order, for List
	entityByKey      map[entityKey]string      // (normalized_name, type) -> entity_id, dedup index
	claimEntities    map[claimEntityKey]string // (claim_id, entity_id, role) -> role, dedup index
	jobs             map[string]storedCompilationJob
	actions          map[string]storedAction
	actionOrder      []string
	outcomes         map[string]storedOutcome
	outcomeOrder     []string
	lessons          map[string]storedLesson
	lessonOrder      []string
	lessonEvidence   map[string]map[string]struct{} // lesson_id -> set of action_ids
	decisions        map[string]storedDecision
	decisionOrder    []string
	decisionBeliefs  map[string]map[string]struct{} // decision_id -> set of claim_ids
	playbooks        map[string]storedPlaybook
	playbookOrder    []string
	playbookLessons  map[string]map[string]struct{} // playbook_id -> set of lesson_ids
	lessonVersions   map[string][]storedEntityVersion
	playbookVersions map[string][]storedEntityVersion
	entityRels       map[string]domain.EntityRelationship
	entityRelOrder   []string
	entityRelKey     map[entityRelKey]string // (kind,from_type,from_id,to_type,to_id) → id
	incidents        map[string]domain.Incident
	incidentOrder    []string
	feedback         map[string]domain.ClaimFeedback
	claimVersions    map[string][]domain.ClaimVersion     // claim_id -> version chain, append-ordered
	blocks           map[string]domain.WorkingMemoryBlock // owner\x00label -> working-memory block
	expectations     map[string]domain.Expectation        // claim_id -> forward expectation
	globalSchemas    map[string]domain.GlobalSchema       // id -> promoted global (neocortex) schema
	journalEntries   []domain.JournalEntry                // append-only cognitive journal (ADR 0018), insertion order
}

// storedEntityVersion is the in-memory analogue of a row in
// {lesson,playbook}_versions: a JSON-encoded snapshot of the prior
// entity state plus the [valid_from, valid_to) window it covered.
type storedEntityVersion struct {
	PayloadJSON string
	ValidFrom   time.Time
	ValidTo     time.Time
}

func newState() *state {
	return &state{
		events:           map[string]storedEvent{},
		claims:           map[string]storedClaim{},
		statusHistory:    map[string][]storedTransition{},
		evidence:         map[string]map[string]struct{}{},
		relationships:    map[string]storedRelationship{},
		embeddings:       map[embeddingKey]storedEmbedding{},
		users:            map[string]storedUser{},
		usersByEmail:     map[string]string{},
		revokedTokens:    map[string]storedRevokedToken{},
		agents:           map[string]storedAgent{},
		entities:         map[string]storedEntity{},
		entityByKey:      map[entityKey]string{},
		claimEntities:    map[claimEntityKey]string{},
		jobs:             map[string]storedCompilationJob{},
		actions:          map[string]storedAction{},
		outcomes:         map[string]storedOutcome{},
		lessons:          map[string]storedLesson{},
		lessonEvidence:   map[string]map[string]struct{}{},
		decisions:        map[string]storedDecision{},
		decisionBeliefs:  map[string]map[string]struct{}{},
		playbooks:        map[string]storedPlaybook{},
		playbookLessons:  map[string]map[string]struct{}{},
		lessonVersions:   map[string][]storedEntityVersion{},
		playbookVersions: map[string][]storedEntityVersion{},
		entityRels:       map[string]domain.EntityRelationship{},
		entityRelKey:     map[entityRelKey]string{},
		incidents:        map[string]domain.Incident{},
		feedback:         map[string]domain.ClaimFeedback{},
		claimVersions:    map[string][]domain.ClaimVersion{},
		blocks:           map[string]domain.WorkingMemoryBlock{},
		expectations:     map[string]domain.Expectation{},
		globalSchemas:    map[string]domain.GlobalSchema{},
	}
}

// clear drops every collection. Called from Conn.Close so a closed
// Conn that's still reachable via a stale repo reference returns
// empty results rather than stale ones.
func (s *state) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = map[string]storedEvent{}
	s.eventOrder = nil
	s.claims = map[string]storedClaim{}
	s.claimOrder = nil
	s.statusHistory = map[string][]storedTransition{}
	s.evidence = map[string]map[string]struct{}{}
	s.relationships = map[string]storedRelationship{}
	s.embeddings = map[embeddingKey]storedEmbedding{}
	s.users = map[string]storedUser{}
	s.userOrder = nil
	s.usersByEmail = map[string]string{}
	s.revokedTokens = map[string]storedRevokedToken{}
	s.agents = map[string]storedAgent{}
	s.agentOrder = nil
	s.entities = map[string]storedEntity{}
	s.entityOrder = nil
	s.entityByKey = map[entityKey]string{}
	s.claimEntities = map[claimEntityKey]string{}
	s.jobs = map[string]storedCompilationJob{}
	s.actions = map[string]storedAction{}
	s.actionOrder = nil
	s.outcomes = map[string]storedOutcome{}
	s.outcomeOrder = nil
	s.lessons = map[string]storedLesson{}
	s.lessonOrder = nil
	s.lessonEvidence = map[string]map[string]struct{}{}
	s.decisions = map[string]storedDecision{}
	s.decisionOrder = nil
	s.decisionBeliefs = map[string]map[string]struct{}{}
	s.playbooks = map[string]storedPlaybook{}
	s.playbookOrder = nil
	s.playbookLessons = map[string]map[string]struct{}{}
	s.lessonVersions = map[string][]storedEntityVersion{}
	s.playbookVersions = map[string][]storedEntityVersion{}
	s.entityRels = map[string]domain.EntityRelationship{}
	s.entityRelOrder = nil
	s.entityRelKey = map[entityRelKey]string{}
	s.incidents = map[string]domain.Incident{}
	s.feedback = map[string]domain.ClaimFeedback{}
	s.claimVersions = map[string][]domain.ClaimVersion{}
	s.globalSchemas = map[string]domain.GlobalSchema{}
	s.incidentOrder = nil
}

// actorOr mirrors sqlite.actorOr: an empty actor falls back to the
// SystemUser sentinel so internal write paths don't have to remember
// to set CreatedBy explicitly.
func actorOr(s string) string {
	if s == "" {
		return domain.SystemUser
	}
	return s
}
