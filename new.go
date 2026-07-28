package mnemos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/felixgeelhaar/chronos/embed"
	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/embedding"
	"go.klarlabs.de/mnemos/internal/kernel"
	"go.klarlabs.de/mnemos/internal/llm"
	"go.klarlabs.de/mnemos/internal/pipeline"
	"go.klarlabs.de/mnemos/internal/ports"
	"go.klarlabs.de/mnemos/internal/query"
	"go.klarlabs.de/mnemos/internal/relate"
	"go.klarlabs.de/mnemos/internal/store"
)

// New constructs a [Memory] from the supplied options. When no mode
// option is supplied, [WithPassiveMode] is assumed; when no storage
// option is supplied, the DSN is resolved from MNEMOS_DB_URL > a
// project-local .mnemos/mnemos.db (walked up from the working directory
// like .git) > the XDG default ~/.local/share/mnemos/mnemos.db.
//
// The caller is responsible for blank-importing the storage providers
// it needs. The simplest pattern:
//
//	import _ "go.klarlabs.de/mnemos/sqlite"
//
// will let the default DSN resolve. For Postgres, MySQL, libSQL, or
// in-memory storage, blank-import the corresponding sub-package.
//
// Returned [Memory] holds an open storage handle; the caller MUST call
// [Memory.Close] when finished.
//
// The returned Memory is safe for concurrent use by multiple goroutines:
// concurrent Remember / RememberClaim / RememberEvent calls each run as
// an independent governed session, and [Memory.LastWriteSession] reflects
// whichever write finished last. See the [Memory] documentation for the
// full concurrency contract.
func New(opts ...Option) (Memory, error) {
	cfg := config{
		mode:    modePassive,
		actorID: resolveActor(),
	}
	for _, opt := range opts {
		opt.applyOption(&cfg)
	}
	return newFromCfg(cfg)
}

// newFromCfg assembles a Memory from a fully-built config. Split out from [New] so
// [Memory.Tenant] can re-run the assembly against a tenant-scoped DSN, reusing the
// same client/extractor/kernel wiring.
func newFromCfg(cfg config) (Memory, error) {
	dsn := cfg.storageDSN
	if dsn == "" {
		dsn = resolveDSN()
	}
	if dsn == "" {
		return nil, errors.New("mnemos: no storage DSN; set MNEMOS_DB_URL or call WithStorage")
	}

	ctx := context.Background()
	conn, err := store.Open(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("mnemos: open storage %q: %w", dsn, err)
	}

	// Build the LLM + embedding clients per the configured mode. The
	// internal types are wrapped behind providers.TextGenerator /
	// providers.Embedder for shared mode and built from ProviderConfig
	// for enhanced mode. Passive mode leaves them nil and the engines
	// degrade gracefully.
	llmClient, embClient, err := buildClients(cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mnemos: build providers: %w", err)
	}

	// Build the extractor. Passive uses the rule-based engine; shared
	// and enhanced wrap an internal LLMEngine around the provided
	// client (bypassing pipeline.NewExtractor's env-driven path).
	extractor, err := buildExtractor(cfg.mode, llmClient)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mnemos: build extractor: %w", err)
	}

	// Query engine wires the repository ports + optional clients.
	q := query.NewEngine(conn.Events, conn.Claims, conn.Relationships)
	if embClient != nil && conn.Embeddings != nil {
		q = q.WithEmbeddings(conn.Embeddings, embClient)
	}
	// Hybrid retrieval (R1): adopt the full-text (sparse) recall leg whenever the
	// store's repositories provide one. The dense embedding leg blurs exact tokens
	// (SHAs, service names, error codes) that lexical search nails; the query
	// engine fuses the two by RRF. Auto-wired by capability, like the vector path
	// in WithEmbeddings — Postgres (tsvector) and SQLite (FTS5) implement it; other
	// backends simply don't, and recall stays dense/legacy for them.
	if es, ok := conn.Events.(ports.TextSearcher); ok {
		var cs ports.TextSearcher
		if c, ok := conn.Claims.(ports.TextSearcher); ok {
			cs = c
		}
		q = q.WithTextSearch(es, cs)
	}
	if llmClient != nil {
		q = q.WithLLM(llmClient)
	}
	if conn.Decisions != nil {
		q = q.WithDecisions(conn.Decisions)
	}
	if conn.Incidents != nil {
		q = q.WithIncidents(conn.Incidents)
	}

	// Chronos: use the supplied engine or boot a default in-memory one.
	// The default engine lets RememberEvent / Timeline work out of the
	// box; consumers wanting durable temporal patterns supply
	// WithChronos() with their own configured engine.
	chronosEngine := cfg.chronos
	chronosOwned := false
	if chronosEngine == nil {
		chronosEngine, err = embed.New() // defaults to memory storage
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("mnemos: boot embedded chronos: %w", err)
		}
		chronosOwned = true
	}

	m := &memory{
		conn:         conn,
		actorID:      cfg.actorID,
		info:         deriveInfo(cfg, dsn),
		cfg:          cfg,
		dsn:          dsn,
		tenant:       tenantFromDSN(dsn),
		extractor:    extractor,
		relator:      relate.NewEngine(),
		query:        q,
		embedder:     embClient,
		chronos:      chronosEngine,
		chronosOwned: chronosOwned,
		// logger backs the governed-write kernel's domain-event log. Nil
		// (the default) keeps axi chatter off stderr; WithLogger injects
		// a caller-supplied sink.
		logger: cfg.logger,
	}
	// Root context for background embedding work, cancelled by Close so
	// in-flight goroutines stop before the connection goes away.
	m.embedCtx, m.embedCancel = context.WithCancel(context.Background())

	// The cumulative token budget (WithTokenBudget) is enforced at the
	// Mnemos layer across writes: a pre-execution gate rejects writes once
	// it's exhausted, and the threshold-crossing write reports post-hoc.
	// See governed_writes.go. It is distinct from axi's per-write MaxTokens
	// (env-driven), which caps a single session's spend.
	if cfg.tokenBudgetSet {
		m.tokenBudget = cfg.tokenBudget
	}

	// Build the in-process axi kernel every public write routes through.
	// This is unconditional: there is no direct-write fallback, so the
	// spec non-negotiable ("every write passes through axi-go") holds by
	// construction. The per-write budget is permissive (env-derived,
	// itself defaulting to unlimited tokens) so existing flows don't
	// break.
	budget := kernel.BudgetFromEnv()
	wkernel, err := buildWriteKernel(m, budget, cfg.evidenceLog)
	if err != nil {
		_ = conn.Close()
		if chronosOwned {
			_ = chronosEngine.Close()
		}
		return nil, fmt.Errorf("mnemos: build write kernel: %w", err)
	}
	m.wkernel = wkernel

	return m, nil
}

// buildClients converts the mode + supplied options into internal LLM
// and embedding clients. Returns (nil, nil, nil) for passive mode.
func buildClients(cfg config) (llm.Client, embedding.Client, error) {
	switch cfg.mode {
	case modePassive:
		return nil, nil, nil

	case modeShared:
		var lc llm.Client
		var ec embedding.Client
		if cfg.textGen != nil {
			lc = newTextGenAdapter(cfg.textGen)
		}
		if cfg.embedder != nil {
			ec = newEmbedderAdapter(cfg.embedder)
		}
		return lc, ec, nil

	case modeEnhanced:
		llmCfg := llm.Config{
			Provider: llm.Provider(cfg.enhancedCfg.LLMProvider),
			APIKey:   cfg.enhancedCfg.LLMAPIKey,
			Model:    cfg.enhancedCfg.LLMModel,
			BaseURL:  cfg.enhancedCfg.LLMBaseURL,
		}
		lc, err := llm.NewClient(llmCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("llm client: %w", err)
		}

		embCfg := embedding.Config{
			Provider: llm.Provider(cfg.enhancedCfg.EmbedProvider),
			APIKey:   cfg.enhancedCfg.EmbedAPIKey,
			Model:    cfg.enhancedCfg.EmbedModel,
			BaseURL:  cfg.enhancedCfg.EmbedBaseURL,
		}
		if embCfg.Provider == "" {
			embCfg.Provider = llmCfg.Provider
		}
		if embCfg.APIKey == "" {
			embCfg.APIKey = llmCfg.APIKey
		}
		if embCfg.BaseURL == "" {
			embCfg.BaseURL = llmCfg.BaseURL
		}
		ec, err := embedding.NewClient(embCfg)
		if err != nil {
			// Embedding failure is not fatal; the query engine falls
			// back to token-overlap. Surface as a soft warning rather
			// than refusing to build.
			ec = nil
		}
		return lc, ec, nil

	default:
		return nil, nil, fmt.Errorf("unknown mode %d", cfg.mode)
	}
}

// buildExtractor returns a pipeline.Extractor matched to the mode. For
// passive, it's rule-based. For shared/enhanced, it wraps an LLM engine
// around the provided client.
func buildExtractor(m mode, lc llm.Client) (*pipeline.Extractor, error) {
	if m == modePassive || lc == nil {
		// Rule-based: never fails to construct.
		ext, err := pipeline.NewExtractor(false)
		if err != nil {
			return nil, err
		}
		return ext, nil
	}
	// Use the pipeline's LLM extractor so token usage is captured into
	// LastUsage — the bridge that lets the kernel's MaxTokens budget sum
	// real spend across each governed write.
	return pipeline.NewLLMExtractor(lc, pipeline.ExtractionCacheDir()), nil
}

// resolveDSN mirrors cmd/mnemos's DSN resolution: MNEMOS_DB_URL > a
// project-local .mnemos/mnemos.db (walked up from CWD like .git) > the
// XDG default ~/.local/share/mnemos/mnemos.db.
func resolveDSN() string {
	if u := os.Getenv("MNEMOS_DB_URL"); u != "" {
		return u
	}
	return "sqlite://" + resolveDBPath()
}

func resolveDBPath() string {
	if p, _, ok := findProjectDB(); ok {
		return p
	}
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join("data", "mnemos.db")
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "mnemos", "mnemos.db")
}

// findProjectDB walks up from CWD looking for a .mnemos directory.
// Stops at filesystem root or home directory to avoid adopting a parent
// project's DB by accident.
func findProjectDB() (dbPath, projectRoot string, ok bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", false
	}
	home, _ := os.UserHomeDir()
	dir := cwd
	for {
		candidate := filepath.Join(dir, ".mnemos", "mnemos.db")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir || parent == home {
			return "", "", false
		}
		dir = parent
	}
}

// resolveActor returns the actor id every write is stamped with. Reads
// MNEMOS_USER_ID; falls back to domain.SystemUser for unattributed
// writes. Library consumers can override via [WithActor].
func resolveActor() string {
	if id := os.Getenv("MNEMOS_USER_ID"); id != "" {
		return id
	}
	return domain.SystemUser
}
