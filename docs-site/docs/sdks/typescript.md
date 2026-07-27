# TypeScript SDK (`mnemos-ts`)

Optional thin wrapper. Bring your own `fetch` (defaults to global) so it runs in Node 18+, browsers, Deno, Bun, and edge runtimes.

```bash
npm install mnemos-ts
```

```ts
import { Mnemos } from "mnemos-ts";

const m = new Mnemos({ baseUrl: "http://localhost:7777" });

const run = m.startRun("chat-session-1");
await run.remember("user prefers vegetarian options", { role: "preference" });
await run.remember("user is allergic to peanuts", { role: "preference" });

for (const memory of await run.recall()) {
  console.log(memory.timestamp, memory.content);
}

// Hybrid retrieval
const hits = await m.search("dietary restrictions", { topK: 5, minTrust: 0.5 });

// Context Block
const block = await m.context(run.runId);
```

## API surface

Server endpoint each call targets on a current (v0.85.0+) server:

| Method | Wraps |
|---|---|
| `m.startRun(subject)` | local helper — mints a UUID |
| `run.remember(content, opts)` | `POST /v1/episodes` |
| `run.recall(limit?)` | `GET /v1/episodes?run_id=…` |
| `m.appendEvent(runId, content, metadata?, sourceInputId?)` | `POST /v1/episodes` |
| `m.listEvents(runId?, limit?)` | `GET /v1/episodes` |
| `m.search(query, opts)` | `GET /v1/search` |
| `m.context(runId, opts)` | `GET /v1/context` |
| `m.health()` | `GET /health` |

!!! warning "Version pairing"
    `mnemos serve` v0.85.0 renamed `/v1/events` → `/v1/episodes` and the
    request/response wrapper key `events` → `episodes`, with no compatibility
    alias. `mnemos-ts` lives in its own repository, so pin a release that
    speaks the v0.85.0 wire against a v0.85.0+ server — an older SDK gets
    `404`s. Check the SDK's own changelog for the version that made the jump.

## Auth

Pass a token to the constructor. **Every call needs one, reads included**
(`mnemos serve` has been secure by default since v0.85.1); `m.health()` is the
only exception.

Source + tests: [github.com/klarlabs-studio/mnemos-ts](https://github.com/klarlabs-studio/mnemos-ts).
