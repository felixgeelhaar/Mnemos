package query

import (
	"go.klarlabs.de/mnemos/internal/embedding"
	"go.klarlabs.de/mnemos/internal/llm"
	"go.klarlabs.de/mnemos/internal/ports"
)

// EngineDeps carries the optional capabilities a full-strength engine needs.
// Every field may be nil; each is wired only when present.
type EngineDeps struct {
	Embeddings  ports.EmbeddingRepository
	EmbedClient embedding.Client
	LLM         llm.Client
	Decisions   ports.DecisionRepository
	Incidents   ports.IncidentRepository
}

// NewEngineWith builds an Engine with every capability the caller can supply,
// auto-detecting the full-text leg from the repositories themselves.
//
// # WHY THIS EXISTS
//
// NewEngine returns a bare engine and each capability is chained on separately,
// so a construction site gets exactly the retrieval it remembers to ask for.
// That is a silent failure by design: an engine with no embeddings and no
// text search does not error, it falls back to token overlap over ListAll and
// returns plausible, worse, slower results.
//
// It had already happened. `/v1/search` — the endpoint the Claude Code recall
// hook uses on every HOSTED deployment — called NewEngine and chained nothing,
// so the highest-traffic read path in the product ranked by token overlap while
// the database held pgvector embeddings and a tsvector index it never
// consulted. The handler's own comment described hybrid BM25 + cosine
// retrieval. Meanwhile /v1/recall in the same binary was fully wired, so two
// REST endpoints on one server ranked differently.
//
// Capability detection belongs in one place. Pass what you have; this decides
// what that enables.
func NewEngineWith(events eventLister, claims ports.ClaimRepository, relationships ports.RelationshipRepository, d EngineDeps) Engine {
	e := NewEngine(events, claims, relationships)

	if d.EmbedClient != nil && d.Embeddings != nil {
		e = e.WithEmbeddings(d.Embeddings, d.EmbedClient)
	}

	// Hybrid retrieval: the sparse (full-text) leg is adopted whenever the
	// store provides one — Postgres via tsvector, SQLite via FTS5. The dense
	// leg blurs exact tokens (SHAs, service names, error codes) that lexical
	// search nails; the engine fuses both by RRF. Backends without it simply
	// stay dense, which is a real difference in quality but not a silent one:
	// it is a property of the store, not of which constructor was used.
	if es, ok := events.(ports.TextSearcher); ok {
		var cs ports.TextSearcher
		if c, ok := claims.(ports.TextSearcher); ok {
			cs = c
		}

		e = e.WithTextSearch(es, cs)
	}

	if d.LLM != nil {
		e = e.WithLLM(d.LLM)
	}

	if d.Decisions != nil {
		e = e.WithDecisions(d.Decisions)
	}

	if d.Incidents != nil {
		e = e.WithIncidents(d.Incidents)
	}

	return e
}
