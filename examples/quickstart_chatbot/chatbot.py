"""Quickstart chatbot — Mnemos memory in 30 lines, no SDK.

A minimal chatbot that remembers facts about the user across turns by
posting episodes to Mnemos and pulling them back at query time. The point
is to show that "memory for an AI app" is just two HTTP calls; the
fancier capabilities (beliefs, contradictions, replay) are a layer on
top, not a prerequisite.

Run:
    mnemos token issue --user <id>        # every /v1/* call needs one
    export MNEMOS_JWT=<token>
    pip install -r requirements.txt
    python chatbot.py
"""

from __future__ import annotations

import os
import sys
import uuid
from datetime import datetime, timezone

import httpx

MNEMOS = os.environ.get("MNEMOS_URL", "http://localhost:7777")
# `mnemos serve` is secure by default since v0.85.1: every /v1/* call needs a
# bearer token, READS INCLUDED. Mint one with `mnemos token issue --user <id>`.
TOKEN = os.environ.get("MNEMOS_JWT")
SESSION = str(uuid.uuid4())            # one run per chat session


def _headers() -> dict[str, str]:
    h = {"Content-Type": "application/json"}
    if TOKEN:
        h["Authorization"] = f"Bearer {TOKEN}"
    return h


def remember(text: str, role: str) -> None:
    """Append one episode to Mnemos for this session."""
    # The resource is `episodes` (renamed from `events` in v0.85.0), and the
    # server rejects unknown JSON fields — so the wrapper key must match.
    httpx.post(
        f"{MNEMOS}/v1/episodes",
        headers=_headers(),
        json={"episodes": [{
            "id": str(uuid.uuid4()),
            "run_id": SESSION,
            "source_input_id": f"chatbot::{SESSION}",
            "content": text,
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "metadata": {"role": role},
        }]},
    ).raise_for_status()


def recall() -> list[dict]:
    """Get every episode for this session, oldest first."""
    r = httpx.get(
        f"{MNEMOS}/v1/episodes",
        headers=_headers(),
        params={"run_id": SESSION, "limit": 200},
    )
    r.raise_for_status()
    return list(reversed(r.json().get("episodes", [])))


def reply(user: str, history: list[dict]) -> str:
    """Stand-in for an LLM. Replace with your model of choice."""
    text = " ".join(e["content"].lower() for e in history)
    if "vegetarian" in text:
        return "I remember you're vegetarian. Want me to skip meat in suggestions?"
    if "allergy" in text or "allergic" in text:
        return "Got it — I'll keep your allergy in mind."
    return f"You said: {user}. Tell me more."


def main() -> None:
    print(f"Mnemos quickstart chatbot — session {SESSION}")
    print(f"Mnemos at {MNEMOS}. Ctrl-D to exit.")
    if not TOKEN:
        print(
            "warning: MNEMOS_JWT is unset. `mnemos serve` requires a bearer "
            "token on every /v1/* call (reads included) unless it was started "
            "with --public-reads; expect 401s.",
            file=sys.stderr,
        )
    print()
    while True:
        try:
            user = input("> ").strip()
        except EOFError:
            print()
            break
        if not user:
            continue
        remember(user, role="user")
        history = recall()
        bot = reply(user, history)
        remember(bot, role="assistant")
        print(bot)
    print(f"\nReplay this session: GET {MNEMOS}/v1/episodes?run_id={SESSION}")


if __name__ == "__main__":
    sys.exit(main())
