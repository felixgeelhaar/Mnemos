# Python SDK (`mnemos-py`)

Optional thin wrapper around the HTTP API. Use it for typed return values and pip-install ergonomics; raw `httpx` against the server is fully supported alternative.

```bash
pip install mnemos-py
```

```python
from mnemos import Mnemos

with Mnemos(base_url="http://localhost:7777") as m:
    run = m.start_run(subject="chat-session-1")
    run.remember("user prefers vegetarian options", role="preference")
    run.remember("user is allergic to peanuts", role="preference")

    for memory in run.recall():
        print(memory.timestamp, memory.content)

    # Hybrid retrieval
    hits = m.search("dietary restrictions", top_k=5, min_trust=0.5)

    # Context Block — drop into a system prompt
    block = m.context(run_id=run.run_id)
```

## API surface

Server endpoint each call targets on a current (v0.85.0+) server:

| Method | Wraps |
|---|---|
| `Mnemos.start_run(subject)` | local helper — mints a UUID |
| `run.remember(content, role, metadata)` | `POST /v1/episodes` |
| `run.recall(limit=200)` | `GET /v1/episodes?run_id=…` |
| `m.append_event(run_id, content, metadata)` | `POST /v1/episodes` |
| `m.list_events(run_id?, limit=200)` | `GET /v1/episodes` |
| `m.search(query, run_id?, top_k, min_trust?, as_of?)` | `GET /v1/search` |
| `m.context(run_id, query?, max_tokens?)` | `GET /v1/context` |
| `m.health()` | `GET /health` |

!!! warning "Version pairing"
    `mnemos serve` v0.85.0 renamed `/v1/events` → `/v1/episodes` and the
    request/response wrapper key `events` → `episodes`, with no compatibility
    alias. `mnemos-py` lives in its own repository, so pin a release that
    speaks the v0.85.0 wire against a v0.85.0+ server — an older SDK gets
    `404`s. Check the SDK's own changelog for the version that made the jump.

Source + tests: [github.com/klarlabs-studio/mnemos-py](https://github.com/klarlabs-studio/mnemos-py).

## Auth

Set `MNEMOS_JWT` env or pass `token=` to the constructor. **A token is
required for every call, reads included** (`mnemos serve` has been secure by
default since v0.85.1). `m.health()` is the only exception. An operator can
re-open anonymous reads with `serve --public-reads`, but that is an explicit
opt-in and not the default.
