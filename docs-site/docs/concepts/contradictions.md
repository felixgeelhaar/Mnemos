# Contradictions

The headline differentiator. When two claims disagree, Mnemos surfaces the conflict instead of letting your agent quote both confidently.

## What it catches

| Kind | Example |
|---|---|
| **Polarity** | "deployment succeeded" ⊥ "deployment did not succeed" |
| **Numeric** | "user has 12 prior refunds" ⊥ "user has 0 prior refunds" |
| **Entity-role** | "the CEO is Alice" ⊥ "the CEO is Bob" |
| **Temporal aspect** | "migration completed Tuesday" ⊥ "migration is still running" |

All four ship with rule-based detection that runs without any LLM. Contradictions land as `contradicts` edges in the association graph.

## Benchmark numbers

Against the seed [contradiction-detection suite](../benchmarks.md):

| Case | F1 |
|---|---|
| direct polarity conflict | 1.00 |
| three-way partial conflict (entity) | 1.00 |
| no contradictions clean facts (true negative) | 1.00 |
| numeric disagreement | 1.00 |
| implicit temporal conflict | 1.00 |
| **Suite F1** | **1.00** |

## Querying

```bash
# All active contradicts edges (reads need a bearer token)
curl -H "Authorization: Bearer $MNEMOS_JWT" \
  'http://localhost:7777/v1/associations?type=contradicts'

# Just the contradictions involving a specific claim
mnemos query "what contradicts cl_a?"
```

## Resolving

Once an operator (or an agent via the [`memory_resolve_dissonance`](../api.md#mcp-tools) MCP tool) picks a winner, both sides get a status transition:

```bash
mnemos resolve cl_a --over cl_b --reason "rolled back at 14:02"
# cl_a → status=resolved
# cl_b → status=deprecated
```

The audit chain records who made the call and why — that's the part competitors don't ship.
