// Package pipeline provides shared orchestration logic used by both the CLI and MCP server
// entrypoints: extraction engine setup, artifact persistence, and embedding generation.
package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.klarlabs.de/fortify/retry"
	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/embedding"
	"go.klarlabs.de/mnemos/internal/extract"
	"go.klarlabs.de/mnemos/internal/llm"
	"go.klarlabs.de/mnemos/internal/ports"
	"go.klarlabs.de/mnemos/internal/store"
	"go.klarlabs.de/mnemos/internal/trust"
)

// Extractor wraps either the rule-based or LLM-powered extraction engine,
// presenting a uniform interface to command handlers. The entity map is
// keyed by claim id and may be nil when the rule-based fallback runs
// (rule-based extraction does not tag entities). Callers should treat a
// nil map as "no entities to materialise", not as an error.
//
// After each ExtractFn call, callers may consult LastUsage for the
// token counts the underlying LLM call reported (nil when the
// rule-based engine ran or when the provider reported no usage). It
// is the canonical bridge into axi-go capability evidence so the
// kernel's MaxTokens budget can sum spend across a session.
type Extractor struct {
	ExtractFn func(context.Context, []domain.Event) ([]domain.Claim, []domain.ClaimEvidence, map[string][]extract.ExtractedEntity, error)

	lastUsage *extract.TokenUsage
}

// LastUsage returns the token usage from the most recent ExtractFn
// call, or nil when the call ran through the rule-based fallback or
// when the provider reported zero tokens. Resets on every ExtractFn
// invocation — callers should read it immediately after extracting,
// before issuing another extract call on the same Extractor.
func (e *Extractor) LastUsage() *extract.TokenUsage {
	if e == nil {
		return nil
	}
	return e.lastUsage
}

// NewExtractor builds the appropriate extraction engine based on useLLM.
// When useLLM is true, it reads provider config from environment variables
// (MNEMOS_LLM_PROVIDER, MNEMOS_LLM_API_KEY, etc.) and falls back to the
// rule-based engine on LLM failure.
func NewExtractor(useLLM bool) (*Extractor, error) {
	if !useLLM {
		engine := extract.NewEngine()
		// Rule-based extraction doesn't tag entities — return nil for
		// the entity map so the pipeline knows there's nothing to
		// materialise.
		return &Extractor{ExtractFn: func(ctx context.Context, events []domain.Event) ([]domain.Claim, []domain.ClaimEvidence, map[string][]extract.ExtractedEntity, error) {
			c, l, err := engine.Extract(ctx, events)
			return c, l, nil, err
		}}, nil
	}

	cfg, err := llm.ConfigFromEnv()
	if err != nil {
		return nil, fmt.Errorf("LLM configuration error: %s\n  Set MNEMOS_LLM_PROVIDER and MNEMOS_LLM_API_KEY environment variables\n  Providers: anthropic, openai, gemini, ollama, openai-compat", err)
	}

	// Optional per-stage model override. Lets users pair a strong model
	// for extraction with a smaller/faster model elsewhere without
	// editing MNEMOS_LLM_MODEL. Falls back silently to the base config.
	if override := os.Getenv("MNEMOS_EXTRACT_MODEL"); override != "" {
		cfg.Model = override
	}

	client, err := llm.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create LLM client: %w", err)
	}

	return NewLLMExtractor(client, ExtractionCacheDir()), nil
}

// ExtractionCacheDir resolves the extraction cache to the same per-user data
// directory as the brain, honouring XDG_DATA_HOME. It exists here so both this
// package and the public library root resolve it identically instead of each
// re-deriving a path.
func ExtractionCacheDir() string {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// No home to anchor to: fall back to the historical relative path
			// rather than disabling the cache outright.
			return filepath.Join("data", "cache", "llm-extraction")
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "mnemos", "cache", "llm-extraction")
}

// NewLLMExtractor builds an Extractor backed by the supplied LLM client
// with token-usage capture wired in, so LastUsage reports the spend of
// the most recent ExtractFn call. This is the shared-/enhanced-mode
// path the public library uses; it mirrors the env-driven NewExtractor
// path but takes an already-constructed client and never reads the
// environment.
//
// Centralising the usage-sink wiring here keeps the Extractor's
// unexported lastUsage field encapsulated in this package — callers
// outside pipeline cannot set it, so they must build their LLM
// extractor through this constructor to get evidence-grade token
// accounting.
//
// cacheDir is a parameter rather than the engine's default because that default
// is RELATIVE, so it resolves against the process's working directory — the
// user's repository, for a tool invoked from one. Callers pass an absolute
// per-user path; "" disables caching.
func NewLLMExtractor(client llm.Client, cacheDir string) *Extractor {
	ext := &Extractor{}
	engine := extract.NewLLMEngine(client).WithCacheDir(cacheDir).WithUsageSink(func(u extract.TokenUsage) {
		// Copy the value so the pointer the engine stack-allocated
		// doesn't escape into our field unintentionally.
		usage := u
		ext.lastUsage = &usage
	})
	ext.ExtractFn = func(ctx context.Context, events []domain.Event) ([]domain.Claim, []domain.ClaimEvidence, map[string][]extract.ExtractedEntity, error) {
		ext.lastUsage = nil
		return engine.ExtractWithEntities(ctx, events)
	}
	return ext
}

// PersistArtifacts writes events, claims, evidence links, and
// relationships through the port-typed repositories on conn.
//
// Backend-agnostic. The previous SQLite-specific implementation
// wrapped every write in a single cross-table transaction; that
// guarantee is replaced by per-repository idempotent upserts so
// memory:// and future Postgres backends share the same code path.
// A retry of a partially-applied batch converges because every
// underlying write is idempotent on its identity key (events are
// INSERT … ON CONFLICT DO NOTHING; claims, evidence, relationships
// are UPSERTs).
//
// claim status_history attribution is preserved by grouping claims
// by CreatedBy so each transition row carries the right changed_by.
//
// Trust scoring runs only when the backend's ClaimRepository also
// implements ports.TrustScorer (SQLite does; in-memory test fakes
// and minimal embeds may not). Failure here remains non-fatal in
// spirit but is surfaced to the caller for visibility.
func PersistArtifacts(ctx context.Context, conn *store.Conn, events []domain.Event, claims []domain.Claim, links []domain.ClaimEvidence, relationships []domain.Relationship) error {
	if conn == nil {
		return fmt.Errorf("persist artifacts: nil conn")
	}

	// Episodic event typing (ADR 0023 part 2, additive form). When enabled,
	// tag the source events of operational-event claims (deploys, releases,
	// merges, incidents) with an event_type, so timeline_query can surface and
	// filter them as typed episodes. Purely additive: the belief claims are
	// still persisted unchanged, so a mis-classification adds a spurious tag to
	// a timeline entry — never drops knowledge. This is why it does NOT route
	// (drop) the belief: a rule-based classifier is ~78% precise, which is fine
	// for a low-cost tag but unsafe for a destructive route.
	if episodicTaggingEnabled() {
		tagEventEpisodes(events, claims, links)
	}

	for _, event := range events {
		if err := conn.Events.Append(ctx, event); err != nil {
			return fmt.Errorf("append event %s: %w", event.ID, err)
		}
	}

	// Pre-compute earliest evidence-event timestamp per claim. The
	// claim's valid_from reflects when the *fact was first observed*
	// in the source — the earliest evidence event — not when we
	// happened to extract it. For backfill / out-of-order ingest this
	// is the only defensible default.
	eventTS := make(map[string]time.Time, len(events))
	for _, ev := range events {
		eventTS[ev.ID] = ev.Timestamp
	}
	earliestEvidence := make(map[string]time.Time, len(claims))
	for _, link := range links {
		ts, ok := eventTS[link.EventID]
		if !ok {
			continue
		}
		cur, seen := earliestEvidence[link.ClaimID]
		if !seen || ts.Before(cur) {
			earliestEvidence[link.ClaimID] = ts
		}
	}

	// Apply ValidFrom inference to claim copies so we don't mutate
	// caller-owned slice elements.
	enriched := make([]domain.Claim, len(claims))
	for i, c := range claims {
		if c.ValidFrom.IsZero() {
			if ts, ok := earliestEvidence[c.ID]; ok {
				c.ValidFrom = ts
			} else {
				c.ValidFrom = c.CreatedAt
			}
		}
		enriched[i] = c
	}

	// Group by CreatedBy so each batch's status_history rows carry
	// the right changed_by — the original implementation attributed
	// per-claim, and most pipelines do produce homogeneous batches
	// (one user, one agent), so this is usually a single group.
	// Stamp a freshness half-life on claims that assert mutable system state.
	//
	// HalfLifeDays is honoured by trust scoring, staleness, curiosity and
	// query, but nothing set it at ingest — so every captured claim inherited
	// the 90-day default, and an assertion about what is installed or running
	// recalled as confidently a month later as the day it was written. That is
	// the mechanism behind stale beliefs like "we're not using warden" or
	// "mnemos doesn't work": each was true when written and none expired.
	//
	// Only set when unset, so an explicit half-life (human verification via
	// MarkVerified) always wins.
	for i := range enriched {
		if enriched[i].HalfLifeDays == 0 {
			enriched[i].HalfLifeDays = extract.HalfLifeFor(string(enriched[i].Type), enriched[i].Text)
		}
	}

	groups := groupClaimsByCreatedBy(enriched)
	for actor, group := range groups {
		if err := conn.Claims.UpsertWithReasonAs(ctx, group, "pipeline", actor); err != nil {
			return fmt.Errorf("upsert claims (created_by=%s): %w", actor, err)
		}
	}

	for _, link := range links {
		if err := link.Validate(); err != nil {
			return fmt.Errorf("invalid claim evidence: %w", err)
		}
	}
	if err := conn.Claims.UpsertEvidence(ctx, links); err != nil {
		return fmt.Errorf("upsert claim evidence: %w", err)
	}

	if err := conn.Relationships.Upsert(ctx, relationships); err != nil {
		return fmt.Errorf("upsert relationships: %w", err)
	}

	// Trust scoring is optional on a ClaimRepository — backends that
	// don't track trust skip silently. SQLite implements TrustScorer;
	// memory:// also does (added in Phase 2a) so this engages there too.
	//
	// Prefer the scoped recompute. Trust is a function of a claim's own
	// confidence, its evidence count and its most recent evidence, so only the
	// claims this write touched can have changed. Rescoring the whole store
	// made a write cost grow with the brain: on an 11k-claim store every
	// capture rewrote every row, and under -race that eventually exceeded the
	// governed-write budget and failed the write outright.
	if scoped, ok := conn.Claims.(ports.ScopedTrustScorer); ok {
		if _, err := scoped.RecomputeTrustForClaims(ctx, trustAffectedClaimIDs(enriched, links), defaultTrustScorer()); err != nil {
			return fmt.Errorf("recompute trust: %w", err)
		}
		return nil
	}
	if scorer, ok := conn.Claims.(ports.TrustScorer); ok {
		if _, err := scorer.RecomputeTrust(ctx, defaultTrustScorer()); err != nil {
			return fmt.Errorf("recompute trust: %w", err)
		}
	}
	return nil
}

// trustAffectedClaimIDs returns the claims whose trust inputs this write could
// have changed: the claims written (their confidence may be new) plus every
// claim the batch attached evidence to (its evidence count and recency moved).
//
// The evidence side matters and is easy to miss — a batch often links new
// events to claims that already existed and are not in `claims`. Omitting them
// would leave their trust stale at the value it had before the corroboration
// arrived.
func trustAffectedClaimIDs(claims []domain.Claim, links []domain.ClaimEvidence) []string {
	seen := make(map[string]struct{}, len(claims)+len(links))
	ids := make([]string, 0, len(claims)+len(links))
	add := func(id string) {
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, c := range claims {
		add(c.ID)
	}
	for _, l := range links {
		add(l.ClaimID)
	}
	return ids
}

// groupClaimsByCreatedBy buckets claims by their CreatedBy actor so
// UpsertWithReasonAs can stamp each transition row with the correct
// changed_by. Empty CreatedBy folds into domain.SystemUser to match
// the actorOr fallback used at the storage layer.
func groupClaimsByCreatedBy(claims []domain.Claim) map[string][]domain.Claim {
	groups := map[string][]domain.Claim{}
	for _, c := range claims {
		actor := c.CreatedBy
		if actor == "" {
			actor = domain.SystemUser
		}
		groups[actor] = append(groups[actor], c)
	}
	return groups
}

// defaultTrustScorer wraps internal/trust.Score with a real wall
// clock. Defined here (rather than inlined) so tests can swap in a
// fixed clock if/when we add an integration test for the persist
// → trust pipeline.
func defaultTrustScorer() func(confidence float64, evidenceCount int, latestEvidence time.Time) float64 {
	return func(confidence float64, evidenceCount int, latestEvidence time.Time) float64 {
		return trust.Score(confidence, evidenceCount, latestEvidence, time.Now().UTC())
	}
}

// MaterializeEntities walks the per-claim entity tags produced by the
// extractor and writes them through the port-typed EntityRepository.
// Idempotent: re-running over the same input is a no-op via
// FindOrCreate (entities) and the (claim, entity, role) dedup
// contract on LinkClaim.
//
// Resolution at ingest: before creating a new entity row, the helper
// tries to fold the incoming surface form onto an existing entity of
// the same type via case-insensitive substring containment. This
// catches the common "Alice" vs "Alice Smith" case — the first
// surface form wins as canonical, subsequent surface forms link to
// the same row. Out of scope: cross-language resolution, alias
// schema, embedding similarity (separate roadmap items).
//
// Runs after PersistArtifacts so the linked claim_id rows already
// exist. Failure here is reported to the caller; current cmd/mnemos
// callers treat it as a warning rather than aborting the whole job
// — the claims are persisted and a future `mnemos extract-entities`
// can backfill what didn't land.
func MaterializeEntities(ctx context.Context, conn *store.Conn, entities map[string][]extract.ExtractedEntity, createdBy string) (int, error) {
	if len(entities) == 0 {
		return 0, nil
	}
	if conn == nil || conn.Entities == nil {
		return 0, fmt.Errorf("materialize entities: conn missing Entities repository")
	}
	// Per-type cache of existing entities, populated lazily so a
	// claim batch with no entities of a type doesn't pay the lookup.
	existingByType := map[domain.EntityType][]domain.Entity{}
	linked := 0
	for claimID, ents := range entities {
		for _, ent := range ents {
			etype := domain.EntityType(ent.Type)
			canonical, err := resolveOrCreateEntity(ctx, conn, existingByType, ent.Name, etype, createdBy)
			if err != nil {
				return linked, err
			}
			if err := conn.Entities.LinkClaim(ctx, claimID, canonical.ID, ent.Role); err != nil {
				return linked, fmt.Errorf("link claim %s -> entity %s: %w", claimID, canonical.ID, err)
			}
			linked++
		}
	}
	return linked, nil
}

// resolveOrCreateEntity returns the canonical entity for the given
// surface form, creating a new row only when no existing entity of
// the same type contains-or-is-contained-by the incoming name.
//
// Mutates existingByType so subsequent calls within one batch see
// freshly-created rows and don't double-create.
func resolveOrCreateEntity(
	ctx context.Context,
	conn *store.Conn,
	cache map[domain.EntityType][]domain.Entity,
	name string,
	etype domain.EntityType,
	createdBy string,
) (domain.Entity, error) {
	pool, ok := cache[etype]
	if !ok {
		list, err := conn.Entities.ListByType(ctx, etype)
		if err != nil {
			return domain.Entity{}, fmt.Errorf("list entities of type %s: %w", etype, err)
		}
		pool = list
		cache[etype] = pool
	}
	if match, ok := matchByContainment(pool, name); ok {
		return match, nil
	}
	created, err := conn.Entities.FindOrCreate(ctx, name, etype, createdBy)
	if err != nil {
		return domain.Entity{}, fmt.Errorf("find-or-create entity %q: %w", name, err)
	}
	cache[etype] = append(cache[etype], created)
	return created, nil
}

// matchByContainment scans an entity pool for one whose normalized
// name contains the incoming form (or vice versa) and returns it if
// found. The check is symmetric so that ingesting "Alice" first then
// "Alice Smith" — or "Alice Smith" first then "Alice" — both fold
// onto the first-seen row.
//
// Containment is the lightest viable resolution heuristic. False
// positives are bounded by the (normalized_name, type) UNIQUE index
// — entities of different types never collide. False negatives
// (e.g. "Alice" vs "A. Smith") are accepted; the operator can use
// `mnemos entities merge` to fold them by hand.
func matchByContainment(pool []domain.Entity, name string) (domain.Entity, bool) {
	target := normalizeEntityName(name)
	if target == "" {
		return domain.Entity{}, false
	}
	for _, e := range pool {
		existing := e.NormalizedName
		if existing == "" {
			existing = normalizeEntityName(e.Name)
		}
		if existing == "" {
			continue
		}
		if existing == target ||
			strings.Contains(existing, target) ||
			strings.Contains(target, existing) {
			return e, true
		}
	}
	return domain.Entity{}, false
}

// normalizeEntityName mirrors the (assumed) repository-side
// normalization: lowercase + collapsed whitespace. Idempotent.
func normalizeEntityName(name string) string {
	return strings.Join(strings.Fields(strings.ToLower(name)), " ")
}

// GenerateEmbeddings creates vector embeddings for the given events
// and stores them through conn.Embeddings. Returns the number of
// embeddings created. Failure of the embedding provider is fatal to
// the call; storage failures abort partway through (so callers see
// the count of successfully written rows on a partial-failure path).
func GenerateEmbeddings(ctx context.Context, conn *store.Conn, events []domain.Event) (int, error) {
	if conn == nil || conn.Embeddings == nil {
		return 0, fmt.Errorf("generate embeddings: conn missing Embeddings repository")
	}
	cfg, err := embedding.ConfigFromEnv()
	if err != nil {
		return 0, fmt.Errorf("embedding config: %w", err)
	}

	client, err := embedding.NewClient(cfg)
	if err != nil {
		return 0, fmt.Errorf("create embedding client: %w", err)
	}

	texts := make([]string, 0, len(events))
	for _, ev := range events {
		texts = append(texts, ev.Content)
	}

	retrier := retry.New[[][]float32](retry.Config{
		MaxAttempts:   3,
		InitialDelay:  200 * time.Millisecond,
		MaxDelay:      time.Second,
		BackoffPolicy: retry.BackoffExponential,
		Jitter:        true,
		Logger:        slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	})

	vectors, err := retrier.Execute(ctx, func(ctx context.Context) ([][]float32, error) {
		return client.Embed(ctx, texts)
	})
	if err != nil {
		return 0, fmt.Errorf("embed events: %w", err)
	}

	// The provider contract is one vector per text (enforced in the embedding
	// clients). Silently storing a short prefix and returning its length as a
	// success count is how events end up unembedded while the CLI prints
	// "N embeddings created" and exits 0.
	if len(vectors) != len(events) {
		return 0, fmt.Errorf("embed events: got %d vectors for %d events", len(vectors), len(events))
	}

	model := cfg.Model
	for i, ev := range events {
		if err := conn.Embeddings.Upsert(ctx, ev.ID, "event", vectors[i], model, ""); err != nil {
			return 0, fmt.Errorf("store embedding for event %s: %w", ev.ID, err)
		}
	}

	return len(vectors), nil
}

// GenerateClaimEmbeddings creates vector embeddings for the given
// claims and stores them through conn.Embeddings with
// entity_type="claim". Returns the number of embeddings created.
func GenerateClaimEmbeddings(ctx context.Context, conn *store.Conn, claims []domain.Claim) (int, error) {
	if len(claims) == 0 {
		return 0, nil
	}
	if conn == nil || conn.Embeddings == nil {
		return 0, fmt.Errorf("generate claim embeddings: conn missing Embeddings repository")
	}

	cfg, err := embedding.ConfigFromEnv()
	if err != nil {
		return 0, fmt.Errorf("embedding config: %w", err)
	}

	client, err := embedding.NewClient(cfg)
	if err != nil {
		return 0, fmt.Errorf("create embedding client: %w", err)
	}

	texts := make([]string, 0, len(claims))
	for _, cl := range claims {
		texts = append(texts, cl.Text)
	}

	retrier := retry.New[[][]float32](retry.Config{
		MaxAttempts:   3,
		InitialDelay:  200 * time.Millisecond,
		MaxDelay:      time.Second,
		BackoffPolicy: retry.BackoffExponential,
		Jitter:        true,
		Logger:        slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	})

	vectors, err := retrier.Execute(ctx, func(ctx context.Context) ([][]float32, error) {
		return client.Embed(ctx, texts)
	})
	if err != nil {
		return 0, fmt.Errorf("embed claims: %w", err)
	}

	if len(vectors) != len(claims) {
		return 0, fmt.Errorf("embed claims: got %d vectors for %d claims", len(vectors), len(claims))
	}

	model := cfg.Model
	for i, cl := range claims {
		if err := conn.Embeddings.Upsert(ctx, cl.ID, "claim", vectors[i], model, ""); err != nil {
			return 0, fmt.Errorf("store embedding for claim %s: %w", cl.ID, err)
		}
	}

	return len(vectors), nil
}

// EnableEpisodicTagging forces additive operational-event typing on
// (ADR 0023 part 2), regardless of the environment. Prefer the
// MNEMOS_EPISODIC_EVENTS variable; this exists for callers that configure
// programmatically rather than through the environment.
//
// Off by default: the classifier is rule-based (~78% precision), which is safe
// for an additive tag but not something to impose on every install.
var EnableEpisodicTagging bool

// episodicTaggingEnabled reports whether to tag episodes on this write.
//
// The environment is read HERE rather than assigned into the package var by an
// entrypoint. It used to be set in exactly one place — cmd/mnemos/main.go — so
// the embedded library ignored MNEMOS_EPISODIC_EVENTS entirely: senat-os and
// pet-medical could export it and get nothing, with no error and no symptom
// beyond timeline_query returning fewer typed episodes than it should.
//
// A package-level var that only one entrypoint assigns is a feature reachable
// from one entrypoint.
func episodicTaggingEnabled() bool {
	if EnableEpisodicTagging {
		return true
	}

	switch strings.ToLower(strings.TrimSpace(os.Getenv("MNEMOS_EPISODIC_EVENTS"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// tagEventEpisodes sets event_type on the source events of claims classified as
// operational events, so timeline_query surfaces them as typed episodes. It
// mutates event Metadata in place and never touches claims — additive only.
//
// An event grounding several claims takes the kind of the first event-claim
// linked to it; a chunk that records both a release and an incident is rare and
// either label is a reasonable timeline entry.
func tagEventEpisodes(events []domain.Event, claims []domain.Claim, links []domain.ClaimEvidence) {
	kindByClaim := make(map[string]extract.EventKind, len(claims))
	for _, c := range claims {
		if k := extract.ClassifyEventObservation(string(c.Type), c.Text); k != extract.EventNone {
			kindByClaim[c.ID] = k
		}
	}
	if len(kindByClaim) == 0 {
		return
	}
	kindByEvent := make(map[string]extract.EventKind, len(links))
	for _, l := range links {
		if k, ok := kindByClaim[l.ClaimID]; ok {
			if _, already := kindByEvent[l.EventID]; !already {
				kindByEvent[l.EventID] = k
			}
		}
	}
	for i := range events {
		k, ok := kindByEvent[events[i].ID]
		if !ok {
			continue
		}
		if events[i].Metadata == nil {
			events[i].Metadata = map[string]string{}
		}
		// Never overwrite an event_type an ingester set deliberately.
		if events[i].Metadata["event_type"] == "" {
			events[i].Metadata["event_type"] = string(k)
		}
	}
}
