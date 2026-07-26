# Refund-triage agent — Mnemos audit demo

A small LangGraph agent that decides whether to **approve**,
**approve-with-check**, or **escalate** a refund — and writes its full
reasoning chain into Mnemos as it goes. Every node emits one Episode
keyed to a single `run_id`, so the entire decision is replayable from a
single HTTP query.

This is the proof behind Olymp's "wrap your existing agent for audit +
replay" claim. It deliberately uses raw HTTP (no SDK package), so you
can see the four lines of code that get you there.

## What it shows

- **One `run_id` per decision** — ties the four-step chain together.
- **Each node = one Episode** with structured metadata (`node`, `node_role`,
  rationale, confidence, action result).
- **Replay** the whole chain with a single
  `GET /v1/episodes?run_id=<run-id>`. (The resource was `events` before
  v0.85.0; the old path 404s.)
- **Works with or without an LLM.** Set `ANTHROPIC_API_KEY` for a real
  Claude-driven decision; without it, the agent falls back to a scripted
  decision (clearly marked in the audit chain).

## Run it

```bash
# 1. Start Mnemos somewhere it can reach you. The XDG default works fine:
mnemos serve

# 2. Mint a token — every /v1/* call needs one, reads included.
#    `token issue` prints the JWT on its own line; copy it into MNEMOS_JWT.
mnemos user create --name demo --email demo@example.com
mnemos token issue --user usr_... --ttl 24h
export MNEMOS_JWT=<the token it printed>

# 3. Install the demo's dependencies
cd examples/refund_triage_langgraph
pip install -r requirements.txt

# 4. Trigger a triage decision
python agent.py --customer-id CUST-42 --amount 245.00
```

Sample output:

```json
{
  "run_id": "5bbd4777-1cd3-4ace-9711-ddb86bde278d",
  "decision": "escalate",
  "action": "zendesk.create_ticket",
  "replay": "http://localhost:7777/v1/episodes?run_id=5bbd4777-..."
}
```

## Replay the chain

```bash
curl -s -H "Authorization: Bearer $MNEMOS_JWT" \
  'http://localhost:7777/v1/episodes?run_id=5bbd4777-...' \
  | jq '.episodes[] | {node: .metadata.node, role: .metadata.node_role, content}'
```

Returns the four ordered episodes that made up the decision — observation
→ observation → decision → action — exactly what a regulator, auditor,
or you-at-2am needs to reconstruct what the agent did and why.

## Try the three personas

```bash
python agent.py --customer-id CUST-1  --amount 50.00     # likely approve
python agent.py --customer-id CUST-99 --amount 500.00    # mid risk
python agent.py --customer-id CUST-42 --amount 245.00    # likely escalate
```

The agent's risk model is intentionally simple (prior refund count +
tenure + amount-to-LTV ratio) — swap it for whatever your org actually
uses. Mnemos doesn't care; it just records what the agent decided and
why.

## What's next

This script wires Mnemos directly. If it lands and external users want a
proper Python package + LangGraph callback, we'll extract one. Until
then, four HTTP calls are honest enough.
