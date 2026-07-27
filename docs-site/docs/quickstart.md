# Quickstart

## 1. Install

=== "Homebrew"

    ```bash
    brew tap felixgeelhaar/tap && brew install mnemos
    ```

=== "Go"

    ```bash
    go install go.klarlabs.de/mnemos/cmd/mnemos@latest
    ```

=== "Docker"

    ```bash
    docker run -d --rm -p 7777:7777 \
      -e MNEMOS_DB_URL=memory://demo \
      -e MNEMOS_JWT_SECRET=$(openssl rand -hex 32) \
      ghcr.io/klarlabs-studio/mnemos serve
    ```

## 2. Try the rule-based path (no LLM key required)

```bash
mnemos process --text "The deployment succeeded in production. \
The deployment did not succeed in production. \
Response times averaged 45ms."
```

Mnemos extracts three claims, detects the polarity contradiction between the first two, and flags them as contested.

## 3. Query with evidence

```bash
mnemos query "What happened with the deployment?"
```

Each returned claim carries its source-event id, confidence score, and any contradiction edges.

## 4. Add an LLM provider for grounded answers

```bash
export MNEMOS_LLM_PROVIDER=openai   # or anthropic / gemini / ollama / openai-compat
export MNEMOS_LLM_API_KEY=sk-...

mnemos process --llm --embed meeting-notes.md
mnemos query --llm "What decisions were made?"
```

## 5. Wrap an agent for audit + replay

The [refund-triage example](https://github.com/klarlabs-studio/mnemos/tree/main/examples/refund_triage_langgraph) shows a 4-node LangGraph agent that emits one episode per node, all keyed to one `run_id`. Replay any decision from one HTTP call — reads need a bearer token like every other `/v1/*` call:

```bash
curl -H "Authorization: Bearer $MNEMOS_JWT" \
  "http://localhost:7777/v1/episodes?run_id=<run-id>" | jq
```

[Concepts: claims and evidence →](concepts/claims.md){ .md-button }
