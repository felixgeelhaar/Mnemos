"""Refund-triage agent — LangGraph + Mnemos audit demo.

A 4-node LangGraph agent decides whether to auto-approve, escalate, or
deny a refund request. Each node emits a Mnemos episode keyed to a single
run_id so the full reasoning chain is replayable from
GET /v1/episodes?run_id=<run-id>.

Run:
    mnemos token issue --user <id>        # every /v1/* call needs one
    export MNEMOS_JWT=<token>
    pip install -r requirements.txt
    python agent.py --customer-id CUST-42 --amount 245.00

The script exits with the run_id printed; pass it back into Mnemos to
replay:
    curl -s -H "Authorization: Bearer $MNEMOS_JWT" \
      'http://localhost:7777/v1/episodes?run_id=<run-id>' | jq

Set ANTHROPIC_API_KEY for real LLM-driven decisions; without it the
agent falls back to a deterministic scripted decision (clearly marked
in the audit chain so reviewers know what they're looking at).
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, TypedDict

import httpx
from langgraph.graph import END, StateGraph

MNEMOS_URL = os.environ.get("MNEMOS_URL", "http://localhost:7777")
# `mnemos serve` is secure by default since v0.85.1: every /v1/* call needs a
# bearer token, READS INCLUDED. Mint one with `mnemos token issue --user <id>`.
MNEMOS_TOKEN = os.environ.get("MNEMOS_JWT")


class State(TypedDict, total=False):
    run_id: str
    customer_id: str
    amount: float
    history: dict[str, Any]
    risk: dict[str, Any]
    decision: dict[str, Any]
    action: dict[str, Any]


@dataclass
class MnemosClient:
    """Minimal Mnemos HTTP client. Two methods is the whole surface."""

    base_url: str = MNEMOS_URL
    token: str | None = MNEMOS_TOKEN
    _client: httpx.Client = field(default_factory=httpx.Client)

    def _headers(self) -> dict[str, str]:
        h = {"Content-Type": "application/json"}
        if self.token:
            h["Authorization"] = f"Bearer {self.token}"
        return h

    def append_event(
        self,
        run_id: str,
        content: str,
        metadata: dict[str, Any],
    ) -> str:
        # Mnemos's episode metadata accepts map[string]string, so JSON-
        # stringify any non-string value. Replay tooling unmarshals on
        # read.
        flat: dict[str, str] = {}
        for k, v in metadata.items():
            flat[k] = v if isinstance(v, str) else json.dumps(v, default=str)

        event_id = str(uuid.uuid4())
        # The resource is `episodes` (renamed from `events` in v0.85.0), and
        # the server rejects unknown JSON fields — the wrapper key must match.
        body = {
            "episodes": [
                {
                    "id": event_id,
                    "run_id": run_id,
                    "source_input_id": f"refund-triage-langgraph::{run_id}",
                    "content": content,
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "metadata": flat,
                }
            ]
        }
        r = self._client.post(
            f"{self.base_url}/v1/episodes", headers=self._headers(), json=body
        )
        r.raise_for_status()
        return event_id

    def list_events(self, run_id: str) -> list[dict[str, Any]]:
        r = self._client.get(
            f"{self.base_url}/v1/episodes",
            headers=self._headers(),
            params={"run_id": run_id, "limit": 200},
        )
        r.raise_for_status()
        return r.json().get("episodes", [])


# ---------- Mock data layer (replace with real lookups in production) ----------

_FAKE_HISTORY = {
    "CUST-1": {"prior_refunds": 0, "lifetime_value": 1240.00, "tenure_days": 423},
    "CUST-42": {"prior_refunds": 12, "lifetime_value": 89.00, "tenure_days": 18},
    "CUST-99": {"prior_refunds": 1, "lifetime_value": 4_500.00, "tenure_days": 1_200},
}


def _fetch_customer_history(customer_id: str) -> dict[str, Any]:
    return _FAKE_HISTORY.get(
        customer_id,
        {"prior_refunds": 0, "lifetime_value": 0.0, "tenure_days": 0},
    )


# ---------- Nodes ----------


def node_fetch_history(state: State) -> State:
    """Pull prior refund + lifetime-value history for the customer."""
    history = _fetch_customer_history(state["customer_id"])
    mnemos.append_event(
        run_id=state["run_id"],
        content=(
            f"fetched history for {state['customer_id']}: "
            f"{history['prior_refunds']} prior refunds, "
            f"${history['lifetime_value']:.2f} LTV, "
            f"{history['tenure_days']}-day tenure"
        ),
        metadata={
            "node": "fetch_history",
            "node_role": "observation",
            "customer_id": state["customer_id"],
            "history": history,
        },
    )
    return {**state, "history": history}


def node_score_risk(state: State) -> State:
    """Compute a refund-fraud risk score from history + amount."""
    h = state["history"]
    amount = state["amount"]
    # Simple weighted heuristic; real impl would call a model.
    score = 0.0
    factors: list[str] = []
    if h["prior_refunds"] >= 5:
        score += 0.5
        factors.append(f"prior_refunds={h['prior_refunds']} (>=5)")
    elif h["prior_refunds"] >= 2:
        score += 0.2
        factors.append(f"prior_refunds={h['prior_refunds']} (>=2)")
    if h["tenure_days"] < 30:
        score += 0.25
        factors.append(f"tenure_days={h['tenure_days']} (<30)")
    if amount > h["lifetime_value"]:
        score += 0.25
        factors.append(f"amount=${amount:.2f} > LTV ${h['lifetime_value']:.2f}")
    score = min(1.0, score)

    mnemos.append_event(
        run_id=state["run_id"],
        content=f"risk score = {score:.2f} (factors: {', '.join(factors) or 'none'})",
        metadata={
            "node": "score_risk",
            "node_role": "observation",
            "score": score,
            "factors": factors,
        },
    )
    return {**state, "risk": {"score": score, "factors": factors}}


def _scripted_decide(state: State) -> dict[str, Any]:
    """Deterministic fallback when no LLM is configured."""
    score = state["risk"]["score"]
    if score >= 0.5:
        return {
            "outcome": "escalate",
            "rationale": "High risk score — human review required.",
            "confidence": 0.9,
            "model": "scripted",
        }
    if score >= 0.25:
        return {
            "outcome": "approve_with_check",
            "rationale": "Mid risk score — auto-approve with verification email.",
            "confidence": 0.75,
            "model": "scripted",
        }
    return {
        "outcome": "approve",
        "rationale": "Low risk score — auto-approve.",
        "confidence": 0.95,
        "model": "scripted",
    }


def _llm_decide(state: State) -> dict[str, Any]:
    """Anthropic-driven decision when ANTHROPIC_API_KEY is set."""
    try:
        import anthropic  # type: ignore
    except ImportError:
        return _scripted_decide(state)

    client = anthropic.Anthropic()
    prompt = (
        f"You are a refund-triage agent. Decide one of: approve / "
        f"approve_with_check / escalate.\n\n"
        f"Customer history: {json.dumps(state['history'])}\n"
        f"Risk score: {state['risk']['score']:.2f}\n"
        f"Risk factors: {state['risk']['factors']}\n"
        f"Refund amount: ${state['amount']:.2f}\n\n"
        f"Reply ONLY with JSON of shape: "
        f'{{"outcome":"...","rationale":"...","confidence":0.0}}'
    )
    msg = client.messages.create(
        model="claude-haiku-4-5-20251001",
        max_tokens=300,
        messages=[{"role": "user", "content": prompt}],
    )
    text = msg.content[0].text.strip()
    try:
        parsed = json.loads(text)
    except json.JSONDecodeError:
        return _scripted_decide(state)
    parsed["model"] = "claude-haiku-4-5"
    return parsed


def node_decide(state: State) -> State:
    """Decide the refund outcome — LLM or scripted fallback."""
    decision: dict[str, Any]
    if os.environ.get("ANTHROPIC_API_KEY"):
        try:
            decision = _llm_decide(state)
        except Exception as exc:  # noqa: BLE001
            print(f"LLM decision failed ({exc}); falling back to scripted.", file=sys.stderr)
            decision = _scripted_decide(state)
    else:
        decision = _scripted_decide(state)

    mnemos.append_event(
        run_id=state["run_id"],
        content=(
            f"decision: {decision['outcome']} "
            f"(confidence {decision['confidence']:.2f}, model {decision['model']}) "
            f"— {decision['rationale']}"
        ),
        metadata={
            "node": "decide",
            "node_role": "decision",
            "decision": decision,
        },
    )
    return {**state, "decision": decision}


def node_execute(state: State) -> State:
    """Carry out the decision (mocked: log what would happen)."""
    decision = state["decision"]
    outcome = decision["outcome"]
    if outcome == "approve":
        action = {
            "kind": "stripe.refund",
            "params": {"amount": state["amount"], "customer": state["customer_id"]},
            "status": "succeeded",
        }
    elif outcome == "approve_with_check":
        action = {
            "kind": "stripe.refund_pending_email_verify",
            "params": {"amount": state["amount"], "customer": state["customer_id"]},
            "status": "pending",
        }
    else:  # escalate
        action = {
            "kind": "zendesk.create_ticket",
            "params": {
                "subject": f"Refund review: {state['customer_id']}",
                "priority": "normal",
            },
            "status": "succeeded",
        }

    mnemos.append_event(
        run_id=state["run_id"],
        content=f"executed {action['kind']} → {action['status']}",
        metadata={
            "node": "execute",
            "node_role": "action",
            "action": action,
        },
    )
    return {**state, "action": action}


# ---------- Wire up ----------

mnemos = MnemosClient()


def build_graph():
    g = StateGraph(State)
    g.add_node("fetch_history", node_fetch_history)
    g.add_node("score_risk", node_score_risk)
    g.add_node("decide", node_decide)
    g.add_node("execute", node_execute)
    g.set_entry_point("fetch_history")
    g.add_edge("fetch_history", "score_risk")
    g.add_edge("score_risk", "decide")
    g.add_edge("decide", "execute")
    g.add_edge("execute", END)
    return g.compile()


def main():
    p = argparse.ArgumentParser(description="Refund-triage agent (Mnemos audit demo)")
    p.add_argument("--customer-id", required=True)
    p.add_argument("--amount", required=True, type=float)
    p.add_argument("--mnemos-url", default=MNEMOS_URL)
    args = p.parse_args()

    mnemos.base_url = args.mnemos_url

    run_id = str(uuid.uuid4())
    initial: State = {
        "run_id": run_id,
        "customer_id": args.customer_id,
        "amount": args.amount,
    }

    if not MNEMOS_TOKEN:
        print(
            "warning: MNEMOS_JWT is unset. `mnemos serve` requires a bearer "
            "token on every /v1/* call (reads included) unless it was started "
            "with --public-reads; expect 401s.",
            file=sys.stderr,
        )

    print(f"run_id: {run_id}", file=sys.stderr)
    graph = build_graph()
    final = graph.invoke(initial)

    print(json.dumps({
        "run_id": run_id,
        "decision": final["decision"]["outcome"],
        "action": final["action"]["kind"],
        "replay": f"{args.mnemos_url}/v1/episodes?run_id={run_id}",
    }, indent=2))


if __name__ == "__main__":
    main()
