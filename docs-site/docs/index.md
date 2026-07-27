# Mnemos

**Self-hosted memory for AI apps.** No vendor cloud, no per-call billing, no SDK to install. Just a Go binary, an HTTP API, and your data.

```python
import httpx, os, uuid

m = "http://localhost:7777"
run = str(uuid.uuid4())
# Every /v1/* call needs a bearer token — reads included.
h = {"Authorization": f"Bearer {os.environ['MNEMOS_JWT']}"}

# Remember
httpx.post(f"{m}/v1/episodes", headers=h, json={"episodes": [{
    "id": str(uuid.uuid4()),
    "run_id": run,
    "source_input_id": "chat-session-1",
    "content": "user prefers vegetarian options",
    "timestamp": "2026-05-04T16:00:00Z",
    "metadata": {"role": "preference"},
}]})

# Recall
episodes = httpx.get(f"{m}/v1/episodes", headers=h, params={"run_id": run}).json()
```

## What you get

- **Typed claims** with confidence + evidence + status (fact / hypothesis / decision; active / contested / resolved / deprecated)
- **Contradiction detection** — rule-based across polarity / numeric / entity / temporal axes
- **A [cognitive layer](concepts/cognitive-layer.md)** — consolidation + forgetting (a "sleep" pass), write-time salience, hybrid dense+sparse retrieval, self-correcting recall, and hypercorrection alerts. No LLM required.
- **Replay-by-run-id** — group events by chat session or agent run, replay months later from one HTTP call
- **Bi-temporal queries** — separate validity-time and ingestion-time axes
- **Multi-backend SQL** — SQLite default, Postgres / MySQL / libSQL also supported
- **MCP server** for direct agent integration (`mnemos mcp`)
- **First-party SDKs** for [Python](sdks/python.md), [TypeScript](sdks/typescript.md), [Go](sdks/go.md)

## Where Mnemos fits

| Approach | Best for | Trade-off |
|---|---|---|
| Hosted AI memory services | Fast onboarding, consumer apps | Vendor cloud, per-call billing, your data leaves your infra |
| Vector DBs (Pinecone, Chroma, Weaviate) | Pure semantic search | No claim/contradiction structure, no evidence trace, no replay |
| Notes apps (Notion, Obsidian, Roam) | Humans organising thinking | Not built for programmatic AI memory writes at scale |
| **Mnemos** | AI memory in stacks that can't leave your servers — regulated, on-prem, air-gapped | You run a binary |

[Read the full comparison →](comparison.md){ .md-button }
[Quickstart →](quickstart.md){ .md-button .md-button--primary }
