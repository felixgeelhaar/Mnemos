# Quickstart chatbot — Mnemos memory in 30 lines

A minimal chatbot that remembers facts across turns by posting episodes
to Mnemos and pulling them back at query time. No SDK, no LLM required
to demo the memory loop — swap the `reply()` stub for your model when
you're ready.

## What it shows

- **Two HTTP calls** are the whole memory API for the simple case:
  `POST /v1/episodes` to remember, `GET /v1/episodes?run_id=…` to recall.
  (Those resources were named `events` before v0.85.0; the old paths 404.)
- **One `run_id` per chat session** ties the conversation together.
  Replay it months later from the same store.
- **No SDK to install.** The only dependency is `httpx` (or your
  language's standard HTTP client).

## Run it

```bash
# 1. Start Mnemos somewhere reachable
mnemos serve

# 2. Mint a token — every /v1/* call needs one, reads included.
#    `token issue` prints the JWT on its own line; copy it into MNEMOS_JWT.
mnemos user create --name demo --email demo@example.com
mnemos token issue --user usr_... --ttl 24h
export MNEMOS_JWT=<the token it printed>

# 3. Install the demo's only dependency
cd examples/quickstart_chatbot
pip install -r requirements.txt

# 4. Chat
python chatbot.py
```

Type messages mentioning "vegetarian" or "allergy" — the stub `reply()`
function will surface the memory back. Replace `reply()` with an
Anthropic, OpenAI, or local Llama call to see your own model use the
recalled events as context.

## What you get for those 30 lines

- Run-id-keyed history that survives restarts.
- Replay from a single `GET` weeks later.
- The same audit shape as the production refund-triage demo at
  [`examples/refund_triage_langgraph/`](../refund_triage_langgraph/) —
  add structured claims and contradiction detection on top whenever
  you need them.

## Auth

`MNEMOS_JWT` is **required**, not optional. Since v0.85.1 `mnemos serve` is
secure by default: every `/v1/*` request needs `Authorization: Bearer <jwt>`,
reads included — a tokenless `GET /v1/episodes` returns `401`. Mint one with
`mnemos token issue --user <id>`.

The only way to run this without a token is to start the server with
`mnemos serve --public-reads`, and even then the `POST` still needs one.
