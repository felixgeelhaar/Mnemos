package main

import (
	"go.klarlabs.de/mnemos/internal/embedding"
	"go.klarlabs.de/mnemos/internal/llm"
	"go.klarlabs.de/mnemos/internal/query"
	"go.klarlabs.de/mnemos/internal/store"
)

// engineDepsFor collects every optional retrieval capability available for this
// connection: the embedding client and LLM from the environment, plus the
// decision and incident repositories the connection carries.
//
// A missing provider is not an error — it is simply a capability the engine
// will not have. What IS an error, and what this exists to prevent, is a
// construction site forgetting to ask.
//
// `/v1/search` called query.NewEngine and chained nothing, so the endpoint the
// hosted recall hook uses ranked by token overlap over the whole event corpus
// while the database held pgvector embeddings and a tsvector index it never
// consulted. No error, no empty result — just quietly worse answers, and
// slower. Meanwhile /v1/recall in the same binary was fully wired.
func engineDepsFor(conn *store.Conn) query.EngineDeps {
	deps := query.EngineDeps{
		Embeddings: conn.Embeddings,
		Decisions:  conn.Decisions,
		Incidents:  conn.Incidents,
	}

	if cfg, err := embedding.ConfigFromEnv(); err == nil {
		if client, err := embedding.NewClient(cfg); err == nil {
			deps.EmbedClient = client
		}
	}

	if cfg, err := llm.ConfigFromEnv(); err == nil {
		if client, err := llm.NewClient(cfg); err == nil {
			deps.LLM = client
		}
	}

	return deps
}

// newQueryEngine builds a fully-capable engine for a read path. Use it anywhere
// that ANSWERS A QUESTION — /v1/search, query_knowledge, recall_at_time.
//
// Not every query.NewEngine call needs it: BuildContextBlock lists a run's
// claims, ComputeMemoryQuality aggregates, and the export/heal paths enrich
// provenance. None of those rank by relevance, so the retrieval capabilities
// would be inert. There is deliberately no blanket lint forcing this — a guard
// that fires on sites which genuinely do not need it teaches people to silence
// guards. What matters is that the three ranking surfaces agree, and they now
// share this one constructor.
func newQueryEngine(conn *store.Conn) query.Engine {
	return query.NewEngineWith(conn.Events, conn.Claims, conn.Relationships, engineDepsFor(conn))
}
