# Bi-temporal queries

Mnemos tracks two independent time axes per claim:

| Axis | Field | Meaning |
|---|---|---|
| **Validity time** | `valid_from` / `valid_to` | When the claim's *content* was true |
| **Ingestion time** | `created_at` | When Mnemos *recorded* the claim |

Both axes are queryable independently.

## Why both?

Trace tools log when something was written. Mnemos additionally tracks when the underlying event was true. Common questions only one of the axes can answer:

- *"What was true on April 15?"* → validity time
- *"What did we know on April 15?"* → ingestion time
- *"What was true on April 15, knowing only what we'd recorded by then?"* → both

That last one is the regulator-grade question: reproduce the agent's view at the moment a decision was made.

## How to ask

=== "HTTP"

    ```bash
    # Reads need a bearer token too — see the API reference.
    AUTH="Authorization: Bearer $MNEMOS_JWT"

    # Validity-time
    curl -H "$AUTH" 'http://localhost:7777/v1/beliefs?as_of=2026-04-15T00:00:00Z'

    # Ingestion-time
    curl -H "$AUTH" 'http://localhost:7777/v1/beliefs?recorded_as_of=2026-04-15T00:00:00Z'

    # Both
    curl -H "$AUTH" 'http://localhost:7777/v1/beliefs?as_of=2026-04-15T00:00:00Z&recorded_as_of=2026-04-15T00:00:00Z'
    ```

=== "CLI"

    ```bash
    mnemos query --at 2026-04-15 "what did we believe?"           # validity
    mnemos query --recorded-at 2026-04-15 "what was recorded?"    # ingestion
    ```

=== "Python"

    ```python
    from mnemos import Mnemos
    m = Mnemos()
    hits = m.search(
        "what did we believe?",
        as_of="2026-04-15T00:00:00Z",
        recorded_as_of="2026-04-15T00:00:00Z",
    )
    ```

## What this isn't

Mnemos doesn't track "expired_at" on the ingestion axis (i.e. when a row was *removed* from the store). Claims aren't soft-deleted; they get marked `deprecated` and stay queryable forever via the validity supersession path. If you need full auditable deletion semantics, file an issue.
