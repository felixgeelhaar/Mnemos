# API reference

The HTTP API is the source of truth — every SDK wraps the same endpoints. Full machine-readable spec: [`api/openapi.yaml`](https://github.com/klarlabs-studio/mnemos/blob/main/api/openapi.yaml).

## Vocabulary change in v0.85.0

The wire speaks the brain vocabulary (ADR 0011). `/v1/events`, `/v1/claims`
and `/v1/relationships` were **renamed in place** — they `404` on any current
server and there is no compatibility alias. The additive `/v2` layer was
retired in the same release.

| Was | Is |
|---|---|
| `/v1/events` | `/v1/episodes` |
| `/v1/claims` | `/v1/beliefs` |
| `/v1/relationships` | `/v1/associations` |
| body `{"events": […]}` | `{"episodes": […]}` |
| body `{"claims": […]}` | `{"beliefs": […]}` |
| body `{"relationships": […]}` | `{"associations": […]}` |
| edge `from_claim_id` / `to_claim_id` | `from_belief_id` / `to_belief_id` |

Request bodies are decoded with unknown fields rejected, so an old wrapper key
is a hard `400`, not a silent no-op.

## Endpoints (summary)

Auth column: **anon** = never needs a token; **JWT** = bearer token required
by default, reads included.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/health`, `/healthz` | anon | Bare liveness `200` |
| `GET` | `/`, `/app` | anon | Landing page / registry SPA shell |
| `GET` | `/internal/ready` | JWT | Readiness: version + DB write probe |
| `GET` | `/internal/metrics` | JWT | Prometheus RED metrics |
| `GET` | `/v1/metrics` | JWT | Knowledge-base counts |
| `GET` `POST` | `/v1/episodes` | JWT | List / append episodes |
| `GET` `POST` `DELETE` | `/v1/beliefs` | JWT | List / upsert / purge beliefs (with `as_of` + `recorded_as_of`) |
| `GET` `POST` | `/v1/beliefs/{id}/…` | JWT | `lifecycle`, `provenance`, `export.md`, `feedback`, `history`, `expectation`, `observation`, `analogous` |
| `GET` `POST` | `/v1/associations` | JWT | List / upsert belief-to-belief edges |
| `GET` `POST` | `/v1/embeddings` | JWT | List / upsert embeddings |
| `GET` | `/v1/schemas` | JWT | Promoted schemas (neocortex read) |
| `POST` | `/v1/process` | JWT | Run ingest→extract→relate on raw text |
| `GET` `POST` | `/v1/search` | JWT | Hybrid retrieval over the belief store |
| `GET` `POST` | `/v1/context` | JWT | Render the Context Block for a run |
| `GET` | `/v1/recall` | JWT | Advanced recall (`?mode=…`) |
| `GET` | `/v1/classify` | JWT | Novelty verdict for a candidate statement |
| `GET` | `/v1/decisions`, `/v1/decisions/{id}` | JWT | Browse recorded decisions |
| `GET` `POST` | `/v1/blocks` | JWT | Working-memory blocks |
| `POST` | `/v1/actions`, `/v1/actions/{id}/outcome` | JWT | Skill loop |
| `POST` | `/v1/synthesize` | JWT | Derive schemas + reflexes |
| `GET` | `/v1/timeline`, `/v1/signals` | JWT | Temporal timeline / detected patterns |
| `GET` | `/v1/who-knows`, `/v1/knowledge-gaps`, `/v1/calibration`, `/v1/hypercorrections`, `/v1/recombinations` | JWT | Connected-brain reads |
| `GET` `POST` | `/v1/incidents`, `/v1/incidents/{id}[/resolve\|/why-wrong]` | JWT | Incidents + post-mortem |
| `GET` | `/v1/federation/export` | JWT | Anonymized reflex export; `501` unless `MNEMOS_FEDERATION_ENABLED=true` |
| `POST` | `/v1/leads` | anon | Rate-limited lead-capture form |

## Auth

**Secure by default since v0.85.1: every data endpoint requires a JWT — reads
included.** A tokenless `GET /v1/beliefs` returns `401`, exactly like a `POST`.
Only `/health`, `/healthz`, `/`, `/app` and the rate-limited `POST /v1/leads`
are anonymous, and none of them exposes knowledge or tenant data.

Mint a token:

```bash
mnemos user create --name demo --email demo@example.com
mnemos token issue --user usr_...
```

Pass with `Authorization: Bearer <token>` on **every** `/v1/*` call.

Writes additionally check the token's `scp` claim (`events:write`,
`claims:write`, `relationships:write`, `embeddings:write`, `promote:global`,
or `*`); a missing scope is `403`.

Two opt-outs, both off by default:

| Flag | Env | Effect |
|---|---|---|
| `serve --public-reads` | `MNEMOS_PUBLIC_READS` | Anonymous `GET`/`HEAD`/`OPTIONS` on the data API. Warns at boot; ignored under `--require-tenant`. |
| `serve --metrics-public` | `MNEMOS_METRICS_PUBLIC` | Anonymous `GET /internal/metrics`. Deliberately **not** covered by `--public-reads`. |

`/internal/metrics` is authenticated by default and `/internal/ready` (version
+ DB write probe) has no public opt-out at all.

## Context Block

`GET /v1/context?run_id=<id>` (with a bearer token) returns a stable,
agent-ready string:

```
# Memory context (run <id>)
## Active claims (N)
- [cl_xxx · type · trust 0.91] text
## Contradictions (M)
- cl_xxx ⊥ cl_yyy
## Footer
Generated <ts>. claims=N contradictions=M
```

Designed to be dropped directly into an agent's system prompt. Layout is fixed so the agent can rely on it.

## MCP tools

`mnemos mcp` exposes the surface as MCP tools so Claude Code, Cursor, Cline, and other MCP-aware clients can call directly. Notable tools:

| Tool | Use |
|---|---|
| `query_knowledge` | search over beliefs |
| `process_text` | ingest raw text → episodes → beliefs → associations |
| `record_decision` | persist an agent decision with belief ids + alternatives |
| `record_action` / `record_outcome` | log operational changes + their results |
| `synthesize_schemas` / `synthesize_reflexes` | derive higher-order patterns |
| `query_schemas` / `query_reflex` | read back the derived schemas / reflexes |
| `list_beliefs` / `list_decisions` / `list_dissonances` | browse without a question |
| `memory_deprecate` | mark a belief stale (letta-style self-edit) |
| `memory_resolve_dissonance` | pick winner of a contradicting pair |
| `memory_escalate` | request human review the agent can't resolve |
| `memory_promote` | re-verify a belief against fresh evidence |
| `memory_context` | render the Context Block for a run |
| `remember` / `update` / `forget` / `search_memory` | agent self-edit primitives |

The tool names moved to the brain vocabulary in v0.85.0 alongside the REST
paths — `list_claims` → `list_beliefs`, `get_claim` → `get_belief`,
`analogous_claims` → `analogous_beliefs`, `synthesize_lessons` →
`synthesize_schemas`, `query_lessons` → `query_schemas`,
`synthesize_playbooks` → `synthesize_reflexes`, `query_playbook` →
`query_reflex`, `memory_resolve_contradiction` → `memory_resolve_dissonance`.
The old names are gone, not aliased.

The `memory_*` tools land letta-style agent-driven memory curation: the LLM decides what to deprecate / resolve / pin, Mnemos stores the audit trail.

Over `mnemos mcp --http` each tool is scope-gated exactly like the REST
surface: reads need only a valid token, write tools need `claims:write` or
`events:write`, `memory_promote` needs `promote:global`, and
`configure_environment` needs `*` (over HTTP it writes the *server's*
filesystem). Local stdio carries no token and is ungated.
