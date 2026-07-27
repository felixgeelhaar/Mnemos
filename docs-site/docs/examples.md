# Examples

Two examples ship in-tree under [`examples/`](https://github.com/klarlabs-studio/mnemos/tree/main/examples).

## Quickstart chatbot (~30 LOC)

A minimal chatbot that remembers facts across turns. Two HTTP calls do the whole memory loop:

- `POST /v1/episodes` to remember
- `GET /v1/episodes?run_id=…` to recall

Both carry `Authorization: Bearer $MNEMOS_JWT` — since v0.85.1 reads need a
token too.

Source: [`examples/quickstart_chatbot/`](https://github.com/klarlabs-studio/mnemos/tree/main/examples/quickstart_chatbot)

```bash
mnemos serve &
cd examples/quickstart_chatbot
pip install -r requirements.txt
python chatbot.py
```

## Refund-triage LangGraph agent (~270 LOC)

A 4-node LangGraph agent (`fetch_history → score_risk → decide → execute`) that emits one Mnemos episode per node, all keyed to one `run_id`. Replays the full reasoning chain via `GET /v1/episodes?run_id=<run-id>`.

Source: [`examples/refund_triage_langgraph/`](https://github.com/klarlabs-studio/mnemos/tree/main/examples/refund_triage_langgraph)

```bash
mnemos serve &
cd examples/refund_triage_langgraph
pip install -r requirements.txt
python agent.py --customer-id CUST-42 --amount 245.00
# {
#   "run_id": "5bbd4777-1cd3-4ace-9711-ddb86bde278d",
#   "decision": "escalate",
#   "action": "zendesk.create_ticket",
#   "replay": "http://localhost:7777/v1/episodes?run_id=5bbd4777-..."
# }
```

The agent supports both a deterministic scripted decision (no LLM key required) and a Claude-driven one (set `ANTHROPIC_API_KEY`).

## Why these are honest

Both use **raw HTTP** — no SDK, no opinionated wrapper. The four lines per node that get you a defensible audit trail are exactly what you'd write yourself if you were inspecting the wire.
