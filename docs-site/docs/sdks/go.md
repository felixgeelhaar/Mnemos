# Go SDK

Mnemos ships two Go surfaces. Pick whichever matches your deployment.

## In-process library (v0.17+)

```bash
go get go.klarlabs.de/mnemos@latest
```

Lives at `go.klarlabs.de/mnemos` (the module root). Embed Mnemos
directly in a Go agent runtime — Claude Code, Codex, Hermes, Nomi,
OpenClaw, NanoClaw, or your own — without standing up the HTTP server.

```go
import (
    "go.klarlabs.de/mnemos"
    _ "go.klarlabs.de/mnemos/internal/store/sqlite"
)

mem, err := mnemos.New() // passive mode, XDG storage, bundled Chronos
if err != nil { panic(err) }
defer mem.Close()

// Text in, claims out (mnemos extracts)
_ = mem.Remember(ctx, mnemos.Item{
    Type: "decision", Content: "Adopt Postgres for the new service.",
})

// Agent-supplied claim in (skip extraction)
claimID, _ := mem.RememberClaim(ctx, mnemos.ClaimItem{
    Text: "User prefers Go for backend work.",
    Type: "fact", Confidence: 0.95,
    EventIDs: []string{"evt-source-1"}, RunID: "session-A",
})

// Temporal
_ = mem.RememberEvent(ctx, mnemos.Event{
    At: time.Now(), Type: "deployment", Content: "shipped v2.3.0",
})
events, _ := mem.Timeline(ctx, mnemos.TimelineQuery{Types: []string{"deployment"}})

// Recall
results, _ := mem.Recall(ctx, mnemos.Query{Text: "Postgres decision", Hops: 1})
```

### Three input modes

| Mode | Call | Use when |
|---|---|---|
| **Passive** | `Remember(Item)` + `WithPassiveMode()` | No LLM; rule-based extraction; zero per-call cost |
| **LLM-driven** | `Remember(Item)` + `WithSharedProvider` / `WithEnhancedMode` | Prose / docs; LLM does structured extraction |
| **Agent-supplied** | `RememberClaim(ClaimItem)` | Agent already has structured claims; skip extraction |

### Three provider modes (orthogonal to input modes)

- `mnemos.WithPassiveMode()` — no LLM; zero env vars
- `mnemos.WithSharedProvider(tg, embedder)` — agent runtime hands Mnemos its
  model client (no second API key, no duplicate config)
- `mnemos.WithEnhancedMode(cfg)` — dedicated provider for background
  enrichment, separate billing, local models

### Framework-neutral provider interfaces

```go
import "go.klarlabs.de/mnemos/providers"

type myAdapter struct{ client *anthropic.Client }

func (a *myAdapter) GenerateText(
    ctx context.Context, in providers.GenerateTextInput,
) (providers.GenerateTextOutput, error) { /* wrap your client */ }
```

### Chronos bundled

`mnemos.New()` boots an in-process [Chronos](https://github.com/felixgeelhaar/chronos)
engine (memory backend) by default so `RememberEvent` + `Timeline` work
with zero configuration. Power users supply their own:

```go
import "github.com/felixgeelhaar/chronos/embed"

eng, _ := embed.New(embed.WithStorage("sqlite:///app-chronos.db"))
defer eng.Close()

mem, _ := mnemos.New(mnemos.WithChronos(eng))
```

See [`docs/library.md`](https://github.com/klarlabs-studio/mnemos/blob/main/docs/library.md)
in the repository for the full three-mode walkthrough.

## HTTP client

Lives in-tree at `go.klarlabs.de/mnemos/client`. Same package as
the Mnemos server itself, so it tracks the wire format precisely. Use
when you want HTTP from Go (different process, registry topology, or you
just don't want the in-process dependency on the storage drivers).

```go
import "go.klarlabs.de/mnemos/client"

c := client.New("http://localhost:7777", client.WithToken(os.Getenv("MNEMOS_JWT")))

events, err := c.Events().List(ctx)
hits, err := c.Search(ctx, "dietary restrictions", client.SearchOptions{TopK: 5, MinTrust: 0.5})
block, err := c.Context(ctx, "chat-session-1", client.ContextOptions{})
```

### Surface

| Method | Wraps |
|---|---|
| `client.New(baseURL, ...Option)` | constructor |
| `WithToken / WithHTTPClient / WithTimeout / WithRetry / WithLogger` | options |
| `c.Health(ctx)` | `GET /health` |
| `c.Metrics(ctx)` | `GET /v1/metrics` |
| `c.Events()` builder | `/v1/episodes` |
| `c.Claims()` builder | `/v1/beliefs` |
| `c.Relationships()` builder | `/v1/associations` |
| `c.Embeddings()` builder | `/v1/embeddings` |
| `c.Search(ctx, query, opts)` | `GET /v1/search` |
| `c.Context(ctx, runID, opts)` | `GET /v1/context` |
| `c.Process(ctx, req)` | `POST /v1/process` |
| `c.Recall(ctx, query, opts)` | `GET /v1/recall` |
| `c.GetClaim` / `SetClaimLifecycle` / `Classify` | `/v1/beliefs/{id}`, `/v1/beliefs/{id}/lifecycle`, `/v1/classify` |
| `c.ListDecisions` / `c.GetDecision` | `/v1/decisions`, `/v1/decisions/{id}` |
| `c.WhoKnows` / `KnowledgeGaps` / `Calibration` / `Hypercorrections` / `Recombinations` | the matching `/v1/*` connected-brain reads |
| `c.Blocks` / `SetBlock` / `AppendBlock` | `/v1/blocks` |
| `c.AnalogousClaims(ctx, claimID, …)` | `GET /v1/beliefs/{id}/analogous` |
| `c.SetExpectation` / `RecordObservation` / `Expectation` | `/v1/beliefs/{id}/expectation`, `/v1/beliefs/{id}/observation` |
| `c.RecordAction` / `RecordActionOutcome` / `Synthesize` | `/v1/actions`, `/v1/actions/{id}/outcome`, `/v1/synthesize` |
| `c.Timeline` / `c.Signals` | `/v1/timeline`, `/v1/signals` |

The Go method names still say `Events` / `Claims` / `Relationships`; the paths
they hit were renamed to the brain vocabulary in v0.85.0. Pass a token with
`client.WithToken(...)` for **every** call — reads included.

Source: [go.klarlabs.de/mnemos/tree/main/client](https://github.com/klarlabs-studio/mnemos/tree/main/client).

## Picking between them

| You're building | Use |
|---|---|
| A Go agent runtime that wants Mnemos in-process | **library** (`mnemos.New`) |
| A multi-language stack with Go just one of N callers | **HTTP client** (`mnemos/client`) against `mnemos serve` |
| A CLI / script / one-shot tool | Either; library has lower latency, HTTP client requires a running server |
| A registry topology (one Mnemos serving many agents) | **HTTP client** |
