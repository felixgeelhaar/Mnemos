# Mnemos

**A brain for your AI agents — open-source, local-first, evidence-backed memory.**

AI agents forget everything the moment a session ends. Mnemos gives them a memory that lasts: it stores facts as small, checkable **claims** with evidence back to the source, **surfaces contradictions** instead of silently overwriting, and **consolidates and forgets** on its own. A brain, not a log — a single Go binary, your data in your own storage, no vendor cloud, no per-call billing.

> **New here? Start with the intro:** [**Meet Mnemos**](https://klarlabs.de/writing/meet-mnemos) — what it does and how people use it, in plain language. Then [**Is the brain healthy?**](https://klarlabs.de/writing/is-the-brain-healthy) on how it takes the vitals of its own memory.

## Two ways to use it

### 1. Give your coding agent a memory — zero code

If you use an AI coding assistant like **Claude Code**, one command wires Mnemos in through its hooks: it recalls relevant knowledge before each task, captures what was learned at session end, and consolidates on a nightly "sleep" cycle — with nothing to call.

```bash
brew trust klarlabs-studio/tap        # first time only
brew install --cask klarlabs-studio/tap/mnemos
mnemos init
```

Homebrew refuses to load a cask from a third-party tap it has not been told
to trust, so the first install of anything from this tap needs
`brew trust klarlabs-studio/tap` once — per machine, not per tool.

That's it. Your agent now remembers your decisions, the bug you fixed last month, and why you chose Postgres over the alternative — across sessions, projects, and machines, because the brain is yours and lives where you put it.

### 2. Build it into your app — 5 lines, any language

No SDK to install. Any language with an HTTP client works. Below is Python; substitute `curl`, `fetch`, `reqwest`, etc.

```python
import httpx, os, uuid
m = "http://localhost:7777"
run = str(uuid.uuid4())
# Every /v1/* call needs a bearer token — reads included (see "Authentication").
h = {"Authorization": f"Bearer {os.environ['MNEMOS_JWT']}"}

# Remember something
httpx.post(f"{m}/v1/episodes", headers=h, json={"episodes": [{
    "id": str(uuid.uuid4()),
    "run_id": run,
    "source_input_id": "chat-session-1",
    "content": "user prefers vegetarian options",
    "timestamp": "2026-05-03T16:00:00Z",
    "metadata": {"role": "preference"},
}]})

# Recall it later (months later, same call)
episodes = httpx.get(f"{m}/v1/episodes", headers=h, params={"run_id": run}).json()
```

That's the whole API for the simple case. For richer memory — typed claims, contradiction detection, evidence-back-to-source — keep reading.

## Where Mnemos fits

| Approach | Best for | Trade-off |
|---|---|---|
| Hosted AI memory services | Fast onboarding, consumer apps | Vendor cloud, per-call billing, customer data leaves your infra |
| Vector DBs (Pinecone, Chroma, Weaviate) | Pure semantic search | No claim/contradiction structure, no evidence trace, no replay |
| Notes apps (Notion, Obsidian, Roam) | Humans organising their thinking | Not built for programmatic AI memory writes at scale |
| **Mnemos** | AI memory in stacks that can't leave your servers — regulated, on-prem, air-gapped | You run a binary |

## CLI Quickstart

### 1. Install

```bash
# macOS / Linux (Homebrew)
brew trust klarlabs-studio/tap        # first time only
brew install --cask klarlabs-studio/tap/mnemos

# Go (any platform with Go 1.26+)
go install go.klarlabs.de/mnemos/cmd/mnemos@latest

# Docker
docker run --rm ghcr.io/klarlabs-studio/mnemos --version

# From source
git clone https://github.com/klarlabs-studio/mnemos.git && cd mnemos && make install
```

### 2. Process text — extract claims and detect contradictions

```bash
mnemos process --text "The deployment succeeded in production. The deployment did not succeed in production. Response times averaged 45ms."
```

Mnemos extracts three claims, detects the contradiction between the first two, and flags them as contested.

### 3. Query with evidence

```bash
mnemos query "What happened with the deployment?"
```

The answer comes with the source claims, confidence scores, and surfaced contradictions — so you know what's true and what's contested.

### 4. Try with your own documents

```bash
mnemos process meeting-notes.md
mnemos query "What decisions were made?"
```

No API keys required — rule-based extraction and contradiction detection work out of the box.

### Recommended: Add an LLM provider for best results

For querying real documents, set up an LLM provider. This enables semantic search, better extraction, and grounded answers:

```bash
export MNEMOS_LLM_PROVIDER=openai   # or: anthropic, gemini, ollama, openai-compat
export MNEMOS_LLM_API_KEY=sk-...

# LLM extraction + embeddings + grounded query answers
mnemos process --llm --embed meeting-notes.md
mnemos query --llm "What decisions were made?"
```

Without a provider, extraction and contradiction detection still work via rule-based heuristics. Queries use BM25 keyword matching, which works well for simple questions but may miss nuance on longer documents. With `--embed` (or any embedding provider configured), queries upgrade to a **hybrid BM25 + cosine** ranking — see "Query ranking" below for the full signal breakdown.

### Optional: MCP server for AI agents

```bash
mnemos mcp   # Exposes the full tool surface over stdio
```

Beyond the basics (`query_knowledge`, `process_text`, `knowledge_metrics`,
`remember`, `recall`, `search_memory`, git/file ingestion), `mnemos mcp` exposes
the **cognitive layer** (tiers 0–4) so an out-of-process agent gets the brain, not
a bucket — at parity with the HTTP and gRPC transports:

- **Connected brain** — `who_knows`, `knowledge_gaps`, `calibration`,
  `hypercorrections`, `recombinations`, `analogous_beliefs`
- **Claims + advanced recall** — `get_belief`, `classify`, `get_decision`,
  `recall` (`mode` = sufficiency | effort | context | conflicts | iterative)
- **Working memory + skill/temporal loops** — `get_blocks`, `set_block`,
  `signals`, `record_action`, `record_outcome`, `synthesize_schemas`,
  `synthesize_reflexes`, `timeline_query`

(The brain-native tool names landed in v0.85.0: `list_claims` → `list_beliefs`,
`get_claim` → `get_belief`, `analogous_claims` → `analogous_beliefs`,
`synthesize_lessons` → `synthesize_schemas`, `query_lessons` → `query_schemas`,
`synthesize_playbooks` → `synthesize_reflexes`, `query_playbook` →
`query_reflex`, `memory_resolve_contradiction` → `memory_resolve_dissonance`.
The old names are gone, not aliased.)

### Wrap a LangGraph / CrewAI / MCP agent for audit + replay

Mnemos doubles as the audit substrate beneath any AI agent. Each
node-or-step emits one event keyed to a single `run_id`; the full
reasoning chain is one HTTP call away weeks later.

```bash
# A 4-node LangGraph refund-triage agent that wraps Mnemos for audit:
cd examples/refund_triage_langgraph
pip install -r requirements.txt
python agent.py --customer-id CUST-42 --amount 245.00

# Replay the exact decision chain
curl -s -H "Authorization: Bearer $MNEMOS_JWT" \
  "http://localhost:7777/v1/episodes?run_id=<run-id>" | jq
```

The example uses raw HTTP (no SDK), so you can see the four lines per
node that get you a defensible audit trail. Source:
[`examples/refund_triage_langgraph/`](examples/refund_triage_langgraph/).

## How It Works

```
┌─────────────┐    ┌─────────────┐    ┌─────────────┐    ┌─────────────┐
│   Ingest   │ -> │  Extract    │ -> │   Relate    │ -> │    Query    │
│  (events)  │    │  (claims)   │    │ (evidence)  │    │   (truth)   │
└─────────────┘    └─────────────┘    └─────────────┘    └─────────────┘
```

**Extract** — Turns raw text into structured claims (facts, decisions, hypotheses)
**Relate** — Detects support and contradiction relationships between claims
**Query** — Returns answers with claims, evidence, and surfaced contradictions

## Example Output

```json
{
  "answer": "Tech stack decisions show contradiction: PostgreSQL vs MySQL",
  "claims": [
    {"text": "We decided to use PostgreSQL", "type": "decision", "confidence": 0.88},
    {"text": "The team prefers MySQL", "type": "fact", "confidence": 0.75}
  ],
  "contradictions": [
    {"from": "claim-1", "to": "claim-2", "type": "contradicts"}
  ]
}
```

## Why Mnemos?

| | Traditional RAG | Mnemos |
|---|---|---|
| Claims traced to evidence | ❌ | ✅ |
| Contradictions surfaced | ❌ | ✅ |
| Local-first / private | ❌ | ✅ |
| Grounded in governed data | ❌ | ✅ |
| Evolves over time | ❌ | ✅ |

### Further reading

The thinking behind Mnemos, in plain language and in depth:

- [**Meet Mnemos**](https://klarlabs.de/writing/meet-mnemos) — the intro: what it is and how people use it.
- [**From store to brain**](https://klarlabs.de/writing/from-store-to-brain) — building the cognitive half: consolidation, forgetting, salience, self-correcting recall, with no LLM in the loop.
- [**Is the brain healthy?**](https://klarlabs.de/writing/is-the-brain-healthy) — how a brain takes its own vitals, and what running one in production taught us about counting the right thing.

## Key Features

- **Evidence-backed claims** — Every extracted claim maps to source material
- **Contradiction detection** — Automatically surface conflicting information
- **A cognitive layer** — consolidation + forgetting (a "sleep" pass), write-time salience, hybrid dense+sparse retrieval, self-correcting recall, and hypercorrection alerts — the background processes a brain runs, all deterministic and LLM-free. [Details below.](#cognitive-layer-v035v041)
- **Local-first** — Your data stays on your machine (`~/.local/share/mnemos/`)
- **Multi-provider extraction** — Anthropic, OpenAI, Gemini, Ollama, and OpenAI-compatible endpoints
- **Developer-friendly** — CLI-first, JSON output, MCP-ready, pipeline-friendly

## Commands

| Command | Description |
|---------|-------------|
| `mnemos process <path or --text>` | Ingest + extract + relate in one step |
| `mnemos process --llm --text <text>` | Use LLM-backed extraction |
| `mnemos ingest <file>` | Ingest document as events |
| `mnemos extract --run <run-id>` | Extract claims from a run's events |
| `mnemos relate` | Detect relationships between claims |
| `mnemos query <question>` | Query with evidence |
| `mnemos query --hops <N> <question>` | Expand result claims by N supports/contradicts hops (max 5) |
| `mnemos query --llm <question>` | Query with LLM-grounded answer generation |
| `mnemos metrics` | Knowledge base statistics |
| `mnemos audit [--include-embeddings]` | Export the full knowledge base as JSON for compliance/backup |
| `mnemos resolve <winner> --over <loser> [--reason "..."]` | Resolve a contradiction: winner → resolved, loser → deprecated |
| `mnemos resolve <new> --supersedes <old> [--reason "..."]` | Temporal supersession: close `old.valid_to` at `new.valid_from`. Old claim keeps its status — it remained true while it was true. |
| `mnemos query --at YYYY-MM-DD "..."` | Point-in-time query against the temporal-validity layer |
| `mnemos query --include-history "..."` | Include superseded claims in the answer set (off by default) |
| `mnemos query --entity <name\|id> "..."` | Restrict the answer to claims linked to this entity |
| `mnemos entities list [--type T]` | List canonicalised entities (people/orgs/projects/...) |
| `mnemos entities show <name\|id>` | Show one entity and the claims linked to it |
| `mnemos entities merge <winner> <loser>` | Collapse one entity into another (manual canonicalisation) |
| `mnemos extract-entities [--all]` | Backfill entity links for claims that predate the v0.9 prompt |
| `mnemos reset [--keep-events] [--yes]` | Wipe claims/relationships/embeddings (events optional) |
| `mnemos delete-claim <id>...` | Delete specific claims and their derived state |
| `mnemos delete-event <id>...` | Delete events and cascade to derived claims |
| `mnemos reembed [--force] [--dry-run]` | (Re)generate claim embeddings under the current embed config |
| `mnemos recompute-trust` | Rebuild `trust_score` for every claim (confidence × corroboration × freshness) |
| `mnemos dedup [--threshold T] [--force]` | Merge near-duplicate claims by embedding cosine similarity (dry-run by default) |
| `mnemos query --min-trust X "..."` | Only return claims whose `trust_score` ≥ X |
| `mnemos query --kind causes,validates "..."` | Restrict hop expansion to specific edge kinds (causes, caused_by, supports, contradicts, validates, refutes, action_of, outcome_of, derived_from) |
| `mnemos query --service X --env prod --team Y "..."` | Multi-tenant scope filter on the answer claims |
| `mnemos process --no-relate ...` | Skip the relate stage for fast ingest; relate later in batch |
| `mnemos verify <claim-id> [--half-life-days N]` | Bump `last_verified` and `verify_count`; optional per-claim freshness override |
| `mnemos init [--force]` | Create `.mnemos/mnemos.db` for the current project (otherwise resolves to XDG global) |
| `mnemos doctor` | Health-check the install: store reachable, schema applied, env wired, LLM/embed configured |
| `mnemos quality` | Memory-quality telemetry: avg trust, avg confidence, stale/contested/contradiction counts |
| `mnemos trust --test=<requirement-ref> [--service X --env Y --team Z]` | Rank `test_result` claims under one requirement by epistemic credibility; surface winner + rationale |
| `mnemos incident open --title "..." [--severity sev1\|sev2\|sev3\|sev4]` / `incident close <id>` | Track incident lifecycle alongside Decisions/Outcomes |
| `mnemos user create / list / rotate-token` and `mnemos agent register / list / authority` | JWT-auth admin: create operator users + non-human agents, manage authority scores |
| `mnemos metrics --workspace [--telemetry-opt-in\|--telemetry-opt-out\|--telemetry-send]` | North Star workspace view (active runs / evidence-backed claims) + opt-in anonymized payload |

### Action + Outcome (v0.13+)

Record real operational changes and their observed results so the synthesis layer can derive Lessons.

| Command | Description |
|---------|-------------|
| `mnemos action record --kind <K> --subject <name> [--actor X] [--run R]` | Record an operational action (deploy, rollback, scale, ...) |
| `mnemos action list [--subject X\|--run R]` | List recorded actions |
| `mnemos outcome record --action <id> --result <success\|failure\|partial\|unknown> [--metric k=v]...` | Attach an observed outcome to an action |
| `mnemos outcome list [--action <id>]` | List outcomes |

### Synthesis: Lessons + Playbooks (v0.13+)

| Command | Description |
|---------|-------------|
| `mnemos synthesize [--min-corroboration N] [--min-confidence X]` | Cluster action→outcome chains into Lessons |
| `mnemos lessons [--service X\|--trigger T]` | List validated lessons |
| `mnemos playbook synthesize` | Derive Playbooks from Lessons sharing a trigger |
| `mnemos playbook list [--service X]` / `mnemos playbook <trigger>` | Browse playbooks |
| `mnemos playbook show <id>` | Full playbook with steps |

### Decisions (v0.13+)

| Command | Description |
|---------|-------------|
| `mnemos decision record --statement "..." --risk <low\|medium\|high\|critical> [--belief cl_id]... [--alternative "..."]...` | Record an agent decision with its belief evidence |
| `mnemos decision list [--risk X]` / `mnemos decision show <id>` | Audit recorded decisions |
| `mnemos decision attach-outcome <decision-id> <outcome-id>` | Wire an outcome onto a previously recorded decision |

### Markdown round-trip + history (v0.13+)

| Command | Description |
|---------|-------------|
| `mnemos export --kind <lesson\|playbook> --id <id> [--out file.md]` | Export to YAML-frontmatter markdown |
| `mnemos import <file.md>` | Re-upsert a hand-edited markdown file |
| `mnemos history --kind <lesson\|playbook> --id <id>` | List prior snapshots from `*_versions` |

### Claim lifecycle

Every claim carries a status: `active`, `contested`, `resolved`, or `deprecated`. Status changes are recorded in `claim_status_history` (from, to, when, why) so the lifecycle of every claim is auditable. When a query surfaces a claim whose status changed at some point, the answer text includes an `Evolution:` line summarizing the timeline — e.g. _"Transitioned from contested to resolved on 2026-04-18 (evidence review by jane)."_
| `mnemos mcp` | Start MCP server over stdio |
| `mnemos serve [--port N]` | Start HTTP registry server (default `:7777`) |
| `mnemos registry connect <url>` | Wire this project to a remote registry |
| `mnemos push` | Send local knowledge to the registry |
| `mnemos pull` | Fetch knowledge from the registry into the local DB |

### HTTP Registry (Phase 2B)

`mnemos serve` exposes the local knowledge base as a small HTTP API so other tools, dashboards, or scripts can read and write without speaking SQLite. Cross-project federation and namespace scoping land in subsequent commits.

Since **v0.85.0** the wire speaks the brain vocabulary (ADR 0011): the
resources are `episodes`, `beliefs` and `associations`. `/v1/events`,
`/v1/claims` and `/v1/relationships` were renamed in place — they 404 now,
and there is no compatibility alias. The old additive `/v2` layer was retired
in the same release (folded into v1).

Auth column: **anon** = never needs a token; **JWT** = bearer token required
by default (see "Authentication" below).

| Endpoint | Method | Auth | Description |
|---|---|---|---|
| `/health`, `/healthz` | GET | anon | Bare liveness `200` (no version/db/tenant data) |
| `/`, `/app` | GET | anon | Marketing landing page / registry SPA shell |
| `/internal/ready` | GET | JWT | Readiness: version + DB write probe |
| `/internal/metrics` | GET | JWT | Prometheus RED metrics (`serve --metrics-public` to open) |
| `/v1/episodes` | GET | JWT | List episodes (`?run_id`, `?limit`, `?offset`) |
| `/v1/episodes` | POST | JWT `events:write` | Append a batch — body `{"episodes":[…]}` |
| `/v1/beliefs` | GET | JWT | List beliefs (`?type=fact\|hypothesis\|decision`, `?status=active\|contested\|resolved\|deprecated`, `?as_of`, `?recorded_as_of`, `?run_id`, `?similar_to`, `?limit`, `?offset`) |
| `/v1/beliefs` | POST | JWT `claims:write` | Upsert a batch — body `{"beliefs":[…],"evidence":[…]}` |
| `/v1/beliefs` | DELETE | JWT `claims:write` | Right-to-be-forgotten purge for one `?run_id` |
| `/v1/beliefs/{id}` | GET | JWT | Single belief, full detail |
| `/v1/beliefs/{id}/{sub}` | GET/POST | JWT | `lifecycle`, `provenance`, `export.md`, `feedback`, `history`, `expectation`, `observation`, `analogous` |
| `/v1/associations` | GET | JWT | List edges (`?type=supports\|contradicts\|…`, `?limit`, `?offset`) |
| `/v1/associations` | POST | JWT `relationships:write` | Upsert a batch — body `{"associations":[…]}` |
| `/v1/embeddings` | GET | JWT | List embeddings (`?entity_type=event\|claim`, `?limit`, `?offset`) |
| `/v1/embeddings` | POST | JWT `embeddings:write` | Upsert a batch (vector as JSON float array) |
| `/v1/metrics` | GET | JWT | Counts mirroring `mnemos metrics` |
| `/v1/schemas` | GET | JWT | Promoted schemas (neocortex read) |
| `/v1/process` | POST | JWT `claims:write` | Run ingest→extract→relate on raw text |
| `/v1/search` | GET POST | JWT | Hybrid retrieval over the belief store |
| `/v1/context` | GET POST | JWT | Render the Context Block for a run |
| `/v1/recall` | GET | JWT | Advanced recall (`?mode=sufficiency\|effort\|context\|conflicts\|iterative`) |
| `/v1/classify` | GET | JWT | Novelty verdict for a candidate statement (`?text`) |
| `/v1/decisions`, `/v1/decisions/{id}` | GET | JWT | Browse recorded decisions |
| `/v1/blocks` | GET POST | JWT (`claims:write` on POST) | Working-memory blocks |
| `/v1/actions` | POST | JWT `claims:write` | Record an action; `/v1/actions/{id}/outcome` records its result |
| `/v1/synthesize` | POST | JWT `claims:write` | Derive schemas + reflexes |
| `/v1/timeline`, `/v1/signals` | GET | JWT | Temporal timeline and detected patterns |
| `/v1/who-knows`, `/v1/knowledge-gaps`, `/v1/calibration`, `/v1/hypercorrections`, `/v1/recombinations` | GET | JWT | Connected-brain reads |
| `/v1/incidents`, `/v1/incidents/{id}[/resolve\|/why-wrong]` | GET POST | JWT | Incident records and post-mortem analysis |
| `/v1/federation/export` | GET | JWT | Anonymized reflex export. Returns `501` unless `MNEMOS_FEDERATION_ENABLED=true` |
| `/v1/leads` | POST | anon | Rate-limited lead-capture form on the landing page |

Defaults: `limit=50`, capped at `200`. Port also accepts `MNEMOS_SERVE_PORT`. Request bodies cap at 5 MB, 1000 records per batch. Successful appends return `201 Created` with `{"accepted":n}`.

**Web UI.** `mnemos serve` also serves a minimal single-page UI at `GET /app` (the marketing landing page is at `GET /`). It renders the metrics, paginated beliefs (with type/status filters), and the dissonance list by hitting the same `/v1/*` endpoints above. The shell HTML is anonymous; the data calls it makes are authenticated like any other `/v1/*` request. The HTML is embedded via `//go:embed` so there's no separate deploy step — one binary, one port.

**Authentication — secure by default since v0.85.1.** *Every* data endpoint requires a JWT bearer token issued by the same `mnemos serve` instance: `Authorization: Bearer <jwt>`. **Reads are not open.** A tokenless `GET /v1/beliefs` gets `401`, exactly like a `POST`.

Only these routes are anonymous, and none of them exposes knowledge: `/health` and `/healthz` (bare liveness `200`s — no version, DB or tenant data), `/` (marketing landing), `/app` (SPA shell), and the rate-limited `POST /v1/leads` form on the landing page.

Two explicit opt-outs, both off by default:

| Flag | Env | Effect |
|---|---|---|
| `serve --public-reads` | `MNEMOS_PUBLIC_READS` | Anonymous `GET`/`HEAD`/`OPTIONS` on the data API. Prints a warning at boot. Ignored under `--require-tenant` (there is no anonymous tenant). |
| `serve --metrics-public` | `MNEMOS_METRICS_PUBLIC` | Anonymous `GET /internal/metrics` for a scrape on a trusted network. Deliberately **not** covered by `--public-reads`, so a public read API can never leak the RED series. |

`/internal/metrics` and the readiness probe `/internal/ready` (version + DB write check) are authenticated by default; `/internal/ready` has no public opt-out.

**The server sleeps.** A running `mnemos serve` consolidates on a cycle — the hosted counterpart of the local brain's session-start sleep. It runs one pass a couple of minutes after boot (so a server restarted daily still consolidates daily) and then every `MNEMOS_CONSOLIDATE_INTERVAL` (`serve --consolidate-interval`, `serve.consolidate_interval`; default `20h`, `0` disables). The pass is the same one the local brain runs: dedupe, trust refresh, credit assignment, the lesson/playbook skill loop, decay, the journal, and — when an LLM is configured — the session-noise clearing that keeps dissonance from ratcheting one way. Under `--require-tenant` every tenant partition is consolidated separately under its own scope. Consolidation is failure-isolated: an error is logged and the server keeps serving. The cycle is per process, so with several replicas against one store set the interval to `0` on all but one.

Writes additionally check the token's `scp` claim (`events:write`, `claims:write`, `relationships:write`, `embeddings:write`, `promote:global`, or `*`) — see the Auth column above. The same per-tool scope gate applies to `mnemos mcp --http`.

The signing key comes from `MNEMOS_JWT_SECRET` (hex-encoded, ≥ 32 bytes) or `MNEMOS_AUTH_DIR/jwt-secret` (auto-created on first boot, 0600); boot fails loudly if neither resolves. Issue tokens with `mnemos token issue`; revoke with `mnemos token revoke`.

The client-side `MNEMOS_REGISTRY_TOKEN` (used by `mnemos push` / `mnemos pull` to talk to a *remote* registry) is unrelated to inbound HTTP auth — see "Push / Pull" below.

Full HTTP schema: [`api/openapi.yaml`](api/openapi.yaml).

### gRPC (alongside HTTP)

`mnemos serve --grpc-port 7778` exposes the same registry surface over gRPC for typed, streaming-capable clients. Schema: [`proto/mnemos/v1/mnemos.proto`](proto/mnemos/v1/mnemos.proto), service `mnemos.v1.MnemosService`. The service mirrors HTTP and covers Phase 2-7 entities: `Health`, `ListEpisodes/AppendEpisodes`, `ListBeliefs/AppendBeliefs`, `ListAssociations/AppendAssociations`, `ListEmbeddings/AppendEmbeddings`, `Metrics`, plus `List*/Append*` for `Actions`, `Outcomes`, `Schemas`, `Decisions`, `Reflexes`, `EntityAssociations`. It also carries the full **cognitive layer** at parity with HTTP and MCP: `WhoKnows`, `KnowledgeGaps`, `Calibration`, `Hypercorrections`, `Recombinations`, `AnalogousBeliefs`, `GetBelief`, `Classify`, `GetDecision`, `SetBeliefLifecycle`, `Recall` (`mode` = sufficiency/effort/context/conflicts/iterative), `GetBlocks`, `SetBlock`, `Synthesize`, `Timeline`, and `Signals`. (These names were also renamed in v0.85.0: `ListEvents` → `ListEpisodes`, `ListClaims` → `ListBeliefs`, `ListRelationships` → `ListAssociations`, `Lessons` → `Schemas`, `Playbooks` → `Reflexes`, `AnalogousClaims` → `AnalogousBeliefs`, `ClassifyClaim` → `Classify`, `SetClaimLifecycle` → `SetBeliefLifecycle`.) Auth uses the JWT verifier (`MNEMOS_JWT_SECRET` or `MNEMOS_AUTH_DIR`); send `authorization: Bearer <jwt>` metadata. Like REST, reads require a token by default; `serve --public-reads` opts into anonymous read RPCs.

Surface parity across the three transports (MCP tools ↔ HTTP routes ↔ gRPC methods) is guarded by `TestAPISurfaceParity` (`cmd/mnemos/api_parity_test.go`): the build fails if a handler is added to one transport without recording the deliberate presence/absence on the others.

### Integrating Mnemos in your app

The HTTP API at `mnemos serve` is one integration surface; for Go apps you
can also embed Mnemos in-process via the root package. Pick whichever
matches your runtime.

**Go (in-process library, v0.17+)** — `import "go.klarlabs.de/mnemos"`:

```go
import (
    "go.klarlabs.de/mnemos"
    _ "go.klarlabs.de/mnemos/internal/store/sqlite"
)

mem, err := mnemos.New() // passive mode, XDG storage, bundled Chronos
if err != nil { panic(err) }
defer mem.Close()

_ = mem.Remember(ctx, mnemos.Item{
    Type:    "decision",
    Content: "Adopted Postgres for the new service.",
})

results, _ := mem.Recall(ctx, mnemos.Query{Text: "Postgres decision"})
```

Three modes: `WithPassiveMode()` (no LLM), `WithSharedProvider(tg, emb)`
(agent runtime supplies the model), `WithEnhancedMode(cfg)` (dedicated
provider). Chronos is bundled in-process by default; supply your own with
`WithChronos(eng)`. See [`docs/library.md`](docs/library.md) for full
3-mode walkthroughs + godoc examples.

The HTTP API and MCP transport remain available for non-Go consumers; both
route through the same internals.

**Go (HTTP client)** — `import "go.klarlabs.de/mnemos/client"`:

When you do want HTTP from Go (different process, or non-Go consumer over
HTTP that needs a typed client):

```go
c := client.New("http://localhost:7777",
    client.WithToken("optional-secret"),
    client.WithLogger(logger),               // *bolt.Logger
    client.WithRetry(retry.Config{           // fortify retry; 5xx + 429 retry, 4xx fail fast
        MaxAttempts:   3,
        InitialDelay:  200 * time.Millisecond,
        MaxDelay:      time.Second,
        BackoffPolicy: retry.BackoffExponential,
        Jitter:        true,
    }),
)

// Write
c.Events().Append(ctx, []client.Event{{
    ID: "ev_1", RunID: "session-A", SchemaVersion: "v1",
    Content: "We chose Postgres for the new service",
    SourceInputID: "src_1", Timestamp: client.FormatTime(time.Now()),
}})

// Read with chained filters
list, _ := c.Claims().Type("decision").Status("active").Limit(25).List(ctx)
for _, claim := range list.Claims {
    fmt.Printf("[%s] %s\n", claim.Type, claim.Text)
}
```

Resource accessors (`Events()`, `Claims()`, `Relationships()`, `Embeddings()`) return fluent builders. Filter methods chain; terminal `List(ctx)` reads, `Append(ctx, ...)` writes. Non-2xx responses return `*client.APIError` with the server's status and message; works with `errors.As`. Built-in `bolt` request logging and `fortify` retry-with-backoff. Safe for concurrent use.

**Any other language (curl)**:

```bash
# Append an episode (id, content, source_input_id and timestamp are required)
curl -X POST http://localhost:7777/v1/episodes \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $MNEMOS_JWT" \
  -d '{"episodes":[{"id":"ev_1","run_id":"run_1","source_input_id":"cli","content":"...","timestamp":"2026-04-19T10:00:00Z"}]}'

# Browse beliefs, filtered — the token is required for reads too
curl -H "Authorization: Bearer $MNEMOS_JWT" \
  'http://localhost:7777/v1/beliefs?type=decision&limit=25'
```

**Python (stdlib)**:

```python
import json, os, urllib.request

req = urllib.request.Request(
    "http://localhost:7777/v1/episodes",
    data=json.dumps({"episodes": [{
        "id": "ev_1", "run_id": "run_1", "source_input_id": "script",
        "content": "...", "timestamp": "2026-04-19T10:00:00Z",
    }]}).encode(),
    headers={
        "Content-Type": "application/json",
        "Authorization": f"Bearer {os.environ['MNEMOS_JWT']}",
    },
)
urllib.request.urlopen(req)
```

For an AI agent: skip the HTTP API entirely and use the MCP transport (`mnemos mcp`). Agents that speak MCP get the same surface plus `query_knowledge`, `process_text`, browsing, file watching, and git ingestion already wired up.

### Push / Pull

Once a project is connected to a registry, knowledge flows like git:

```bash
mnemos registry connect https://registry.example.com --token <secret>
mnemos push                       # send local events/claims/relationships
mnemos pull                       # fetch remote knowledge into the local DB
```

`registry connect` writes `.mnemos/config.json`. Resolution precedence for `push`/`pull` is **CLI flags (`--url`, `--token`) > env vars (`MNEMOS_REGISTRY_URL`, `MNEMOS_REGISTRY_TOKEN`) > config file**, so CI can override per-job without editing the file.

Sync is idempotent — IDs are the dedup key, so running `push`/`pull` twice is safe. Vectors transfer too: embeddings ride on the same wire as JSON float arrays and round-trip bit-exact, so semantic ranking on pulled content works without re-embedding. Claim-evidence links travel with the claims so the local query engine can resolve pulled claims back to their source events.

**Federation provenance.** Pulled events get stamped with `pulled_from_registry: <url>` in their metadata. The query engine surfaces this in the answer text — claims sourced from a registry appear as `… (from https://reg.example.com)` and the summary line counts them: `Context used 5 event(s) and 8 claim(s) (3 from a connected registry).` Local claims are unmarked (the no-registry case stays uncluttered). The `claim_provenance` field on the MCP `query_knowledge` response carries the same map programmatically.

## Architecture

```
cmd/mnemos           # CLI entrypoint (CLI + mcp + serve subcommands)
proto/mnemos/v1      # gRPC schema (Phase 2-7 entities)
internal/
  domain/            # Core types: Event, Claim, ClaimEvidence, Relationship, EmbeddingRecord, Action, Outcome, Lesson, Decision, Playbook, Scope
  ports/             # Interfaces for engines and repositories
  pipeline/          # Shared orchestration (extraction, persistence, embeddings)
  ingest/            # Multi-format input ingestion
  parser/            # Input-to-event normalization
  extract/           # Claim extraction with evidence mapping
  relate/            # Relationship + causal edge detection (supports, contradicts, causes, validates, ...)
  query/             # Query assembly and ranking (BM25 + cosine hybrid)
  embedding/         # Vector embedding client abstraction
  llm/               # LLM client abstraction (multi-provider)
  synthesize/        # Cluster action→outcome chains into Lessons; Lessons → Playbooks
  markdown/          # YAML-frontmatter round-trip for Lessons + Playbooks
  adapters/outcomes/ # Pull-based Outcome sources (Prometheus instant-query)
  store/             # URL-scheme dispatched repository registry (ADR 0001)
  store/sqlite/      # SQLite + FTS5 (sqlc-generated)
  store/memory/      # In-process backend
  store/postgres/    # Postgres / Postgres-wire-compatible engines
  store/mysql/       # MySQL / MariaDB / MySQL-wire-compatible engines
  store/libsql/      # libSQL / Turso (remote + local file)
  workflow/          # Job runner with retries and structured logs
  trust/             # Trust scoring (confidence × corroboration × freshness)
  autoedge/          # Polymorphic cross-entity edges + auto-fire
  auth/              # JWT signing + bearer-token enforcement
  server/            # HTTP REST + gRPC server wiring
```

## Configuration

Every setting below can be supplied two ways, and you can mix them freely:

1. **Environment variables** — the `MNEMOS_*` variables in the table below.
2. **A YAML config file** — grouped, self-documenting, and convenient for
   self-hosting instead of exporting a dozen variables.

**Precedence follows 12-factor:** an exported environment variable always wins;
the YAML file only fills gaps. So you can keep the bulk of your config in a
committed file and override individual values (or inject secrets) via the
environment in CI/production.

### Config file

Mnemos discovers the file in this order (first match wins):

1. `--config <path>` flag
2. `MNEMOS_CONFIG` environment variable
3. the nearest `.mnemos/mnemos.yaml`, walking up from the working directory
   (the same project boundary used to locate `.mnemos/mnemos.db`)
4. `~/.config/mnemos/config.yaml` (honors `XDG_CONFIG_HOME`)

A file requested explicitly (via `--config` or `MNEMOS_CONFIG`) that is missing
or malformed is a fatal error; an implicit-discovery miss is silently fine.
Unknown keys are rejected so typos surface instead of being ignored.

```yaml
# mnemos.yaml — every key is optional; omit what you don't need.
db:
  url: postgres://mnemos@localhost:5432/mnemos
  max_conns: 25
llm:
  provider: anthropic
  api_key: sk-ant-...        # better: leave unset and export MNEMOS_LLM_API_KEY
  model: claude-opus-4-8
  timeout: 5m
embed:
  provider: openai
  model: text-embedding-3-small
serve:
  port: 8080
  tls_cert_file: /etc/mnemos/tls.crt
  tls_key_file: /etc/mnemos/tls.key
telemetry:
  optin: false
```

See [`mnemos.example.yaml`](mnemos.example.yaml) for every supported key.

## Environment Variables

| Variable | Description |
|----------|-------------|
| `MNEMOS_CONFIG` | Explicit path to the YAML config file (see [Configuration](#configuration)). Overridden by `--config`. |
| `MNEMOS_DB_URL` | Storage DSN dispatched by URL scheme: `sqlite:///var/lib/mnemos/mnemos.db`, `memory://`, `postgres://...`, `mysql://...`, `libsql://...`. When unset, Mnemos walks up from CWD looking for `.mnemos/mnemos.db`, falling back to `~/.local/share/mnemos/mnemos.db`. See [ADR 0001](docs/adr/0001-multi-backend-storage.md). |
| `MNEMOS_LLM_PROVIDER` | `anthropic`, `openai`, `gemini`, `ollama`, `openai-compat` |
| `MNEMOS_LLM_API_KEY` | API key (required for cloud providers) |
| `MNEMOS_LLM_MODEL` | Model override (optional) |
| `MNEMOS_LLM_BASE_URL` | Custom endpoint. Required for `openai-compat`. **Required for `ollama` when Mnemos is not on the same host as the Ollama daemon** — most commonly when Mnemos runs in a container and Ollama runs on the host (`http://host.docker.internal:11434` on Docker Desktop, `http://172.17.0.1:11434` on Linux). Defaults to `http://localhost:11434` for `ollama`. |
| `MNEMOS_LLM_TIMEOUT` | Per-request LLM HTTP timeout (default `120s`). Bump for slow local models or large completions: `MNEMOS_LLM_TIMEOUT=5m`. |
| `MNEMOS_EXTRACT_MODEL` | Override `MNEMOS_LLM_MODEL` just for the extract stage. Lets you pair a strong model for extraction with a smaller model elsewhere. |
| `MNEMOS_JOB_TIMEOUT` | Overall workflow-job deadline (default `10m`). Raise this if your provider is slow enough that an entire `process` run exceeds 10 minutes. |
| `MNEMOS_EMBED_PROVIDER` | Embedding provider (falls back to `LLM_PROVIDER`) |
| `MNEMOS_EMBED_API_KEY` | Embedding API key (falls back to `LLM_API_KEY`) |
| `MNEMOS_EMBED_MODEL` | Embedding model override (optional) |
| `MNEMOS_EMBED_BASE_URL` | Embedding endpoint (same container/host caveat as `MNEMOS_LLM_BASE_URL`) |
| `MNEMOS_EMBED_TIMEOUT` | Per-request embedding HTTP timeout (default `60s`) |
| `MNEMOS_AUTH_DIR` | Directory for the JWT signing secret (default: project `.mnemos/` or `$HOME/.mnemos/`). Override when running on a read-only rootfs (Docker `read_only: true`, k8s `readOnlyRootFilesystem: true`) by pointing at a writable volume. |
| `MNEMOS_JWT_SECRET` | Hex-encoded JWT signing secret (≥32 bytes). When set, takes precedence over the file path; useful in CI/Kubernetes where you'd rather inject the secret as an env var than mount a file. |
| `MNEMOS_LLM_CACHE_MAX_BYTES` | LLM extraction cache cap under `data/cache/llm-extraction/` (default `1 GiB`; `0` disables eviction). Oldest-mtime files are evicted first. |
| `MNEMOS_DB_MAX_CONNS` | Postgres/MySQL pool `MaxOpenConns` (default `25`). |
| `MNEMOS_DB_MAX_IDLE_CONNS` | Postgres/MySQL pool `MaxIdleConns` (default `5`). |
| `MNEMOS_DB_CONN_MAX_LIFETIME` | Postgres/MySQL pool `ConnMaxLifetime` (default `30m`). |
| `MNEMOS_TELEMETRY_OPTIN` | Truthy (`1`/`true`/`yes`) to opt in to anonymized usage payload. Default off. See [`docs/telemetry.md`](docs/telemetry.md). |
| `MNEMOS_TELEMETRY_ENDPOINT` | POST destination for `mnemos metrics --workspace --telemetry-send`. Unset = no destination = no requests, even with opt-in active. |

### Trust scoring (v0.7+)

Every claim carries a `trust_score ∈ [0, 1]` derived from three
signals the LLM cannot fake:

```
trust = confidence × corroboration × freshness

corroboration = 1 + ln(evidence_count) × 0.2     # 1 source: 1.0; 5: 1.32; 20: 1.60
freshness     = max(0.3, exp(-days_since_latest / 90))   # 90-day half-life, floor 0.3
```

The score is recomputed automatically after every `process` run; you
can rebuild it manually with `mnemos recompute-trust` (e.g., after
upgrading or tuning the constants in `internal/trust`).

`mnemos query --min-trust 0.5 "..."` filters out low-confidence
results before ranking. `mnemos metrics` reports `avg_trust` and
`low_trust_count` for at-a-glance corpus quality. The
trust-scoring policy lives in `internal/trust/trust.go` — change
the constants there to retune for your corpus, then run
`mnemos recompute-trust` to backfill.

### Hybrid retrieval (v0.10+)

Mnemos now ranks query results with a hybrid signal:

- **BM25** over an FTS5 keyword index (added v0.10) catches lexical
  hits — proper nouns, exact terminology, code snippets — that pure
  cosine misses.
- **Cosine similarity** over stored embeddings catches paraphrases
  and synonyms that BM25 misses.

Each signal is max-normalised into `[0, 1]` per query, then
equal-weighted into a single composite score. When only one signal
is available (no embeddings yet, or no FTS index for some reason),
that signal carries full weight without further tuning. The
in-memory token-overlap ranker survives as the ultimate fallback for
test doubles and embedding-less, FTS-less deployments.

The FTS5 indexes (`events_fts`, `claims_fts`) are auto-created and
backfilled on the v0.9 → v0.10 schema migration; no operator action
required. They're kept current by INSERT/UPDATE/DELETE triggers on
the source tables so reads don't have to think about staleness.

### Entity layer (v0.9+)

Mnemos canonicalises noun-phrases ("Felix Geelhaar", "Acme",
"PostgreSQL", "Berlin", ...) into first-class entity nodes that
exist independently of any one claim. Once entities exist you can
ask entity-scoped questions:

```bash
mnemos entities list --type person
mnemos entities show "Felix Geelhaar"
mnemos query --entity "Felix Geelhaar" "what does he need this week?"
```

How they get created. The v1.4 LLM extraction prompt tags every
claim with the named entities it mentions. After `mnemos process`
persists claims, the pipeline materialises those tags into the
`entities` and `claim_entities` tables, deduping by
(normalized_name, type) so "Felix", "felixgeelhaar", and
"Felix Geelhaar" land on different ids only if the LLM gives them
different names — manual canonicalisation closes the gap:

```bash
mnemos entities merge en_abc123 en_def456   # winner absorbs loser
```

For databases that pre-date v0.9 (claims extracted under the v1.3
prompt or earlier), `mnemos extract-entities --all` re-runs the
LLM over stored claim text to backfill entity links. It batches
through the LLM cache, so a re-run on the same content is free.

### Temporal validity (v0.8+)

Every claim carries a validity interval — `valid_from` (when the
fact became true) and `valid_to` (when it stopped being true; NULL
means "still in force"). The pipeline derives `valid_from` from the
earliest evidence event's timestamp at insert time, so backfilled
ingest gets correct timelines without operator effort.

Two new behaviors fall out:

- **Default queries hide superseded claims.** `mnemos query "..."`
  filters out claims whose `valid_to` is in the past, so "Felix is
  a junior engineer" stops surfacing once "senior engineer" closes
  its interval. Pass `--include-history` to see both.
- **Point-in-time queries.** `mnemos query --at 2026-03-01 "..."`
  returns the answer as it would have been on that date — handy for
  audit trails and "what did we believe at the time?" questions.

To close one claim's interval when a new one takes its place:

```bash
mnemos resolve cl_new --supersedes cl_old --reason "promoted 2026-04"
```

`--supersedes` is distinct from `--over` (the contradiction
resolver). `--over` says "one of these was always wrong and the
other right"; `--supersedes` says "this fact changed". The latter
preserves the old claim's status — it remained true while it was
true — and only sets `valid_to`. Auto-supersession (heuristic
detection without operator action) is on the v0.9 roadmap.

### Upgrading

Mnemos v0.6.1+ auto-migrates older databases on `sqlite.Open` — no
`mnemos reset` or DB delete needed. The migration is idempotent and
adds the `created_by` / `changed_by` columns the auth feature
introduced in v0.6.0. Schema generation is tracked via
`PRAGMA user_version`, so re-running on an already-migrated DB is a
no-op.

### Local Models (Ollama)

Tested combinations and known quirks. The extract pipeline is tolerant to
common reasoning-model output (`<think>` blocks, prose preambles, ` ```json `
fences), so most models work; the table calls out exceptions.

| Model | LLM | Embed | Notes |
|---|---|---|---|
| `llama3.2:latest` | ✅ | — | Fast, reliable, clean JSON |
| `mistral:latest` | ✅ | — | Fast; occasional prose preamble (handled) |
| `qwen3:*` | ✅ | — | Emits `<think>...</think>` blocks (stripped automatically); pair with `MNEMOS_LLM_TIMEOUT=2m+` if reasoning is long |
| `deepseek-r1:*` | ✅ | — | Same reasoning-block handling as qwen3 |
| `gpt-oss:20b` | ⚠️ | — | Strong structured output but slow on consumer hardware → set `MNEMOS_LLM_TIMEOUT=5m` and `MNEMOS_JOB_TIMEOUT=15m` |
| `gemma3:*` | ❌ | — | No tool/structured-output support in current Ollama builds |
| `nomic-embed-text` | — | ✅ | 768-dim embeddings, fast; the recommended local embed model |

Quick start for fully-local Mnemos:

```bash
ollama pull llama3.2 nomic-embed-text
export MNEMOS_LLM_PROVIDER=ollama
export MNEMOS_LLM_MODEL=llama3.2
export MNEMOS_EMBED_MODEL=nomic-embed-text
mnemos process --llm --embed --text "Your knowledge here"
```

**Container note.** When Mnemos runs in Docker/Podman and Ollama runs on
the host, set `MNEMOS_LLM_BASE_URL=http://host.docker.internal:11434`
(Docker Desktop) or `http://172.17.0.1:11434` (Linux Docker). The default
`http://localhost:11434` resolves to the container itself and will fail
with connection refused.

## Development

```bash
make check          # Format, lint, test, build (CI equivalent)
make build          # Build bin/mnemos
make test           # Run tests (includes 102 eval cases)
make sqlc           # Regenerate sqlc query code
make release-check  # Validate GoReleaser config
```

## Status

Phase 1: Developer Primitive — Available now.

- Rule-based and LLM-powered extraction with eval coverage
- Embeddings for semantic search
- CLI + MCP server + HTTP REST + gRPC entrypoints
- 102 eval cases (90 extraction + 12 relationship detection)
- Pluggable storage backends per [ADR 0001](docs/adr/0001-multi-backend-storage.md): SQLite, in-memory, Postgres, MySQL/MariaDB, libSQL/Turso

### Evidence + Causality + Outcomes (v0.13+)

Mnemos has shipped a self-learning loop on top of the evidence layer:

- **Causal edges** — `causes`, `caused_by`, `action_of`, `outcome_of`, `validates`, `refutes`, `derived_from` extend the relationship graph beyond logical agreement. `relate.DetectCausal` infers these from event-time + shared-entity signals; the optional `relate.DetectCausalLLM` augments borderline pairs via LLM disambiguation.
- **Action + Outcome recording** — `mnemos action record` / `mnemos outcome record` capture operational changes and their observed metrics. The Prometheus pull adapter (`internal/adapters/outcomes/prometheus.go`) scrapes metrics and produces Outcomes automatically.
- **Lessons synthesis** — `mnemos synthesize` clusters action→outcome chains into validated Lessons (confidence = corroboration × consistency × recency).
- **Playbooks** — `mnemos playbook synthesize` derives steps-only operational intelligence from Lesson clusters. Consumers run them through whatever execution layer they own (an agent runtime, an in-process executor, a programmatic system).
- **Decisions** — `mnemos decision record` audits agent reasoning with belief claims, alternatives, and risk level; outcomes attach later via `decision attach-outcome`.
- **Temporal hardening** — per-claim `last_verified`, `verify_count`, `half_life_days`. `mnemos verify` re-confirms a claim; `Answer.StaleClaimIDs` surfaces decay below the trust floor.
- **Multi-tenant scope** — `Scope{Service, Env, Team}` on Claims, Lessons, Decisions, Playbooks; `mnemos query --service X --env prod` filters the answer.
- **Human-editable layer** — `mnemos export` + `mnemos import` round-trip Lessons/Playbooks to Git-friendly YAML+markdown; `mnemos history` lists snapshots from system-versioned `*_versions` tables.

Marketing claim: *"evidence-based memory that learns from actions over time, with provable causality, scoped multi-tenancy, and a human-editable corrections loop."*

### Cognitive layer (v0.35–v0.60)

Most memory systems only write and read. Mnemos also runs the background processes
a brain runs — and all of it is deterministic, self-hostable, and needs **no LLM**.
See [`docs-site/docs/concepts/cognitive-layer.md`](docs-site/docs/concepts/cognitive-layer.md) and the research syntheses in [`docs/RESEARCH-brain-enhancements.md`](docs/RESEARCH-brain-enhancements.md) (arc 1) and [`docs/RESEARCH-perfect-agent-brain.md`](docs/RESEARCH-perfect-agent-brain.md) (arc 2, tiers 0–4 — all shipped).

The first arc (below, v0.35–v0.41) gave mnemos organs. The second (v0.42–v0.60) added
the nervous system — one signal, **prediction error**, tying them into loops so the
store gets *better with use*:

- **Epistemic honesty (tier 0).** Corroboration graded by source **independence**
  (`domain.EffectiveEvidenceCount`) — a single voice can't manufacture consensus — and
  **per-source calibration** (`Calibration.Sources`): each author earns its own
  accuracy/Brier, so a chronically over-confident source is visible.
- **The prediction loop (tier 1).** A claim can carry a structured numeric
  **expectation** (`Memory.Expect`); `ReconcileExpectations` closes it against the
  observed value into a validates/refutes verdict + a **surprise** scalar, which the
  sleep pass routes both ways — confirmed beliefs kept fresh (`ReinforceValidated`),
  refuted ones forgotten (`ForgetRefuted`). `KnowledgeGaps` ranks the weak spots worth
  investigating next.
- **The learning loop (tier 2).** The sleep pass rehearses the most salient memories
  (`ReplayTopK`), auto-derives skills from experience (`Synthesize`: actions → lessons →
  playbooks), reinforces playbooks by their real outcome success rate
  (`ReinforcePlaybooks`), and finds novel recombinations + schema fits
  (`Recombinations`, `ClassifyClaim`).
- **The executive (tier 3).** Working-memory **blocks** (`Memory.Blocks`, always-loaded
  core memory), a **feeling-of-knowing** abstain gate (`RecallWithSufficiency`), stakes-
  scaled **effort budgeting** (`RecallWithEffort`), and **spreading activation** from the
  current train of thought (`RecallWithContext`).
- **The connected brain (tier 4).** A transactive **who-knows-what** directory
  (`WhoKnows`), **analogical** retrieval by relational shape / Weisfeiler-Lehman
  (`AnalogousClaims`, "we've seen this shape before"), the **contested frontier**
  (`RecallWithConflicts`, a recall carrying its own counter-evidence), and an iterative
  retrieve↔reason **fixpoint** (`RecallIterative`).

The organs (arc 1):

- **The sleep pass — consolidation + forgetting** (`Memory.Consolidate`). Off the
  hot path it collapses near-duplicate claims into one canonical claim (evidence
  repointed, nothing lost), refreshes trust, and **actively forgets** claims that
  have decayed below a trust floor. Forgetting is reduced retrievability, not
  erasure — the claim and its history stay queryable point-in-time; it just stops
  surfacing. Promoted knowledge is never forgotten. (hippocampus→neocortex gist +
  synaptic renormalisation.)
- **Salience** (`trust.Salience`). A write-time importance score — from confidence,
  corroboration, claim type, authority, verification — that does **not** decay.
  The sleep pass keeps intrinsically salient claims (a decision, a corroborated
  finding) even as their trust fades, and prunes only the mundane tail. Importance
  and freshness are separate axes.
- **Hybrid retrieval, fused by RRF.** The dense embedding leg (pgvector `<=>`) is
  fused with a sparse full-text leg (Postgres `tsvector` / SQLite FTS5) by
  Reciprocal Rank Fusion, so exact tokens cosine underweights — SHAs, service
  names, error codes — are recalled even when the query is semantically distant.
  Auto-wired; no config.
- **Corrective retrieval (CRAG-style).** Recall grades its own result; on a weak
  first pass it makes one bounded corrective pass (widen the corpus, relax the soft
  trust filter) and keeps the stronger answer. Bounded to a single retry, only
  fires when weak, strictly non-regressive.
- **Hypercorrection** (`Memory.Hypercorrections`). When new evidence contradicts an
  **established** belief (promoted or high-trust), it surfaces as an alert,
  most-established-first — the front of the review queue. Resolve by retiring a
  side (`SetClaimLifecycle(id, superseded)`): accept the new evidence or dismiss
  the challenger; history is preserved and the alert clears. Named for the
  hypercorrection effect — high-confidence errors, once caught, correct strongest.

## Contributing

Contributions welcome. See [PRD.md](./PRD.md) for product direction and [TDD.md](./TDD.md) for technical design.

## Releases

Tagged releases are published with GoReleaser via `.github/workflows/release.yml`, including Homebrew formula updates and Docker images. Human-readable release history: [`CHANGELOG.md`](CHANGELOG.md).

## Security

Auth surfaces, threat model, container hardening, and secret management: [`SECURITY.md`](SECURITY.md).

## Telemetry

Default off. Two independent gates (opt-in flag + endpoint URL) must hold for any payload to leave the host. Privacy posture, payload schema, and opt-in/opt-out flows: [`docs/telemetry.md`](docs/telemetry.md).

## Reliability

SLO: 99.9% availability over 30 days, p99 read 250ms, p99 write 500ms. Error-budget burn alerts in [`SLO.md`](SLO.md). Mutation-testing gate at 70% kill rate on `internal/trust` (currently 100% / 45 mutants caught) — see [`docs/testing/mutation.md`](docs/testing/mutation.md).

## License

MIT
