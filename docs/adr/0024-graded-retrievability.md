# ADR 0024: Graded retrievability — separating storage strength from retrieval strength

- **Status:** Proposed. **Design only — nothing implemented.** No Go file is touched by
  this ADR.
- **Date:** 2026-08-02
- **Deciders:** Felix Geelhaar
- **Scope:** The accessibility axis of recall. Completes the active-forgetting family
  started in ADR 0015 §6 and continued in ADR 0016 (competitive inhibition), and gives
  the phrase "forgetting is reduced retrievability, not erasure" — asserted in five
  places in the codebase — an implementation. Touches `internal/query` ranking, the
  domain belief type, and the consolidation pass's *automatic* forgetting sweep. Does
  **not** touch `internal/trust`, the deliberate deprecation paths (`forget`,
  `memory_deprecate`, `prune`, resolution losers), or the store schema.
- **Also records a documentation defect** found while verifying the mechanism: two
  comments describe automatic forgetting as setting `Status = deprecated` when it closes
  valid-time. Not fixed here (this change is design-only); see Context and Rollout step 0.

## Context

### The claim we make

Mnemos states, in the code and in the ADRs, that forgetting reduces *retrievability*
rather than erasing history:

- `memory_impl.go:1804-1806` — "This is forgetting as reduced retrievability, not
  erasure. Promoted (human-endorsed) claims are never forgotten…"
- `memory.go:643` — "Forgetting is reduced retrievability, NOT [erasure]"
- `cmd/mnemos/prune_session_local.go:38` — "Deprecation is reduced retrievability, never
  erasure"

Half of that is true and load-bearing. Nothing is deleted, `claim_status_history` and
`claim_versions` retain the transitions, and point-in-time recall still reaches the
retired belief. Storage is preserved. That part works.

### Two different retirement mechanisms, not one

Getting the rest of this ADR right requires being precise about *how* a belief stops
being recalled, because there are **two independent mechanisms** and they are easy to
conflate (see the defect below — the codebase conflates them in its own comments).

**Mechanism A — valid-time closure. `Status` is never touched.** This is what
*automatic* forgetting does.

- `forgetStaleClaims` (`memory_impl.go:1811-1860`) — the `consolidate
  --forget-below-trust` sweep — ends in `SetValidity(ctx, c.ID, now)`
  (`memory_impl.go:1854`). The function body contains **no reference to `Status` at
  all**; its skip condition is `if !c.ValidTo.IsZero() { continue }` with the comment
  "already invalidated (superseded or previously forgotten)" (`:1830-1831`).
- `forgetRefutedClaims` does the same (`memory_impl.go:1786`), and its own comment says
  so: "Like forgetStaleClaims it closes valid-time" (`memory_impl.go:1759`).
- `resolve --supersedes` (`cmd/mnemos/resolve.go:206`) and a `remember` carrying an
  explicit TTL (`cmd/mnemos/mcp.go:2119`) also close valid-time.

**Mechanism B — `Status → deprecated`. Valid-time is never touched.** This is what
*deliberate* retirement does — every one of these paths is a human or agent explicitly
saying "retire this":

- MCP `forget` (`cmd/mnemos/mcp.go:2151`) and `memory_deprecate`
  (`cmd/mnemos/mcp.go:1880`).
- Contradiction-resolution losers: MCP `memory_resolve_dissonance`
  (`cmd/mnemos/mcp.go:1921`) and CLI `resolve` (`cmd/mnemos/resolve.go:144`).
- `prune --narration` (`cmd/mnemos/prune_narration.go:88`) and `prune --session-local`
  (`cmd/mnemos/prune_session_local.go:104`).
- Duplicate tombstones (`internal/pipeline/semantic_dedupe.go:352`,
  `internal/govwrite/lifecycle.go:223`).

The two axes are fully orthogonal: a belief swept by consolidation keeps
`Status = active`, and a deprecated belief still has open valid-time (so the sweep can
close it later, independently).

### The gap

**There is no retrievability score.** Accessibility is a two-valued property produced by
two independent binaries, one per mechanism — and, importantly, they are **not equally
hard**:

1. **Valid time (Mechanism A).** `admitClaims` keeps a belief only if
   `c.IsValidAt(asOf)` (`internal/query/admission.go:180`; predicate at
   `internal/domain/types.go:407-415`), a boolean over `valid_from`/`valid_to`. This
   filter is **conditional** — it sits behind `if !opts.IncludeHistory`, so
   `--include-history` and `--at` bypass it. Automatic forgetting is therefore the
   *softer* retirement: recoverable at read time by a caller who asks for history.
2. **Status (Mechanism B).** `ClaimStatus` has exactly four values
   (`internal/domain/types.go:105-110`) and admission drops exactly one:
   `out := excludeDeprecated(claims)` (`internal/query/admission.go:91`, filter at
   `internal/query/contextblock.go:208-216`). This filter is **unconditional** — no
   query option bypasses it. Deliberate deprecation is the *harder* retirement.

Either way the transition is a step function. `forgetStaleClaims` takes a belief from
**fully recallable** to **excluded** in one write, on the strength of a single threshold
comparison (`c.TrustScore >= belowTrust`, `memory_impl.go:1836`). Valid time is an
*interval*, not a flag — but recall reads it as a predicate, so what the retrieval path
actually sees is a step: full accessibility until `valid_to`, then none.

So a belief in mnemos is either current or retired. Nothing becomes gradually *harder*
to recall while remaining recallable. The intermediate state a brain spends most of its
time in — the memory that is still there, still true, still cited, but takes a stronger
cue to reach — has no representation.

### A defect this ADR discovered (not fixed here — this is a design-only change)

Two comments describe Mechanism A as if it were Mechanism B, and they are the two
comments a reader is most likely to meet first:

- `memory.go:643-645` — `ConsolidateOptions.ForgetBelowTrust`: "Forgetting is reduced
  retrievability, NOT erasure: **the claim is marked deprecated** (excluded from recall)
  with its history preserved." It is not marked deprecated; `SetValidity` closes
  valid-time. The **adjacent** `ForgetRefuted` field (`memory.go:652-654`) documents the
  identical call shape *correctly* — "it invalidates (valid-time closed, history kept)"
  — so two neighbouring fields describe the same mechanism two different ways, and one
  is wrong.
- `memory_impl.go:1064-1067` — the `Consolidate` call site: "Reduced retrievability, not
  erasure (**marked deprecated**, history preserved)."

A likely contributing cause, worth fixing at the same time: the one comment that gets it
right — `forgetStaleClaims`'s own doc comment, "by setting their valid-to to now" — is
physically **interleaved** with the `salienceProtectFloor` const declaration
(`memory_impl.go:1794-1810`). The sentence breaks mid-parenthesis at "…the claim + its
history are preserved (a", the const and its comment intrude, and it resumes at "//
point-in-time query can still see what was once believed)". The accurate description is
split across an unrelated declaration and is effectively unreadable in place.

This is not cosmetic. The evaluation-harness lane hit it as a **real measurement bug**:
its first census counted `Status` only, so a consolidation pass reporting `forgotten: 4`
showed zero movement anywhere. Any tooling that trusts those comments will measure the
wrong column.

### Why this matters more than it sounds

This is precisely Bjork & Bjork's **New Theory of Disuse**: every memory has two
strengths, and they are not the same quantity.

- **Storage strength** — how well learned it is; monotonically non-decreasing; not
  directly observable.
- **Retrieval strength** — how accessible it is right now; decays with disuse, is
  restored by retrieval, and is what determines whether you actually get it back.

The theory's non-obvious consequences are the ones mnemos should want: retrieval
strength decays while storage strength does not, so forgetting is a loss of access, not
of content; and **retrieval practice increases storage strength most when retrieval
strength is low** — a hard, nearly-failed recall is worth far more than an easy one
(the testing and spacing effects).

Mnemos models storage strength carefully and retrieval strength not at all.
`trust.Score` / `ScoreWithHalfLife` (`internal/trust/trust.go`) is
`confidence × corroboration × freshness-of-evidence`, floored at `FreshnessFloor = 0.3`
(`internal/trust/trust.go:31`) with a 90-day half-life
(`internal/trust/trust.go:26`) — every input is about *the evidence*, i.e. how well the
belief is grounded. That is a storage-strength model wearing an accessibility hat: the
forget gate then retires beliefs on it (`memory_impl.go:1836`), which means **mnemos
currently retires beliefs for being poorly evidenced, and calls the result reduced
retrievability.**

### The blur that already exists, in the wrong direction

Worse than absence: the retrieval-strength signal *is* being collected, and it is
leaking into the storage-strength scores instead of into an accessibility axis.

`query --reconsolidate` (ADR 0015 §5) calls `MarkVerified` on the top recalled beliefs
(`internal/query/engine.go:377-389`). ADR 0015 §5 describes this as "freshness/liveness
only, **never a trust jump**". The store implementation "bumps last_verified and
increments verify_count" (`internal/store/sqlite/claim_repository.go:409-412`). Both of
those are then read back by scoring code:

- `rescoreCredibility` (`internal/query/admission.go:34`) runs on **every** admitted
  belief and feeds `LastVerified` into `trust.BuildReport`, where it reaches the recency
  signal through `EffectiveExecutionTime(LastExecuted, LastVerified, ValidFrom,
  CreatedAt)` (`internal/trust/credibility.go:112-127`) at weight `wRecency = 0.10`
  (`internal/trust/credibility.go:60`), plus the liveness signal at `wLiveness = 0.05`.
  The rescored value is the one `MinTrust` gates on. So a recall does raise a belief's
  read-time trust on every subsequent query.
- `VerifyCount` feeds the verification term of `trust.SalienceOf`
  (`internal/trust/salience.go:66,72`), and salience ≥ `salienceProtectFloor = 0.66`
  exempts a belief from forgetting (`memory_impl.go:1802,1844`). So recall already
  protects a belief from being retired — through the *stakes* channel, which is supposed
  to be intrinsic and non-decaying.

The testing effect is therefore already live in mnemos; it is just wired into two scores
that mean something else. Building a retrieval-strength axis is partly a matter of
giving that signal its own home so the other two stop absorbing it.

### What ADR 0016 already built, and what it does not cover

ADR 0016 shipped the only retrievability-only quantity in the system: the `inhibition`
component (`internal/domain/inhibition.go:20`), a persisted `[0,1]` magnitude read as a
bounded negative ranking term (`internal/query/inhibition.go`, applied at
`internal/query/engine.go:1774`) and decayed toward zero each sleep
(`memory_impl.go:1244-1282`). That is a genuine graded-accessibility mechanism and this
ADR is deliberately built in its shape.

But its **input** is only contradiction verdicts: `inhibitLosers`
(`internal/query/engine.go:399`) fires only when an agent-mode query resolves a
contradiction into a decisive winner and loser. A belief becomes less accessible only by
losing an adjudicated argument. Disuse — the actual content of the New Theory of Disuse
— produces no signal at all. And because inhibition decays to 0, it models *transient
suppression*, never *accumulated accessibility*. It is the negative half of one axis.
This ADR builds the positive half and the time dimension.

## Decision

Introduce **retrieval strength**: a per-belief scalar in `[0,1]` that rises with
successful retrieval, decays with disuse, is read as a small bounded ranking term, and
**never gates admission**. Storage strength (trust, evidence, status, valid time) is left
exactly as it is.

### 1. Where it lives — a reserved `confidence_components` key, not a column

`domain.RetrievalComponentKey = "retrieval"`, stored in the existing
`Belief.ConfidenceComponents` map. This is the third use of the pattern established by
credit (ADR 0014), salience (`internal/domain/salience.go:22`) and inhibition
(`internal/domain/inhibition.go:20`): an ordinary non-`credit:` key, so
`Claim.Validate()` already constrains it to `[0,1]` and **no validator change, no new
column, no sqlc regeneration, no cross-backend scanner sweep, and no new entry in the
Postgres RLS `scoped` array is required.** The ADR-0015 Batch-2 `strength` migration is
the cautionary tale this deliberately avoids.

A new `internal/domain/retrievability.go` mirrors `salience.go`/`inhibition.go` exactly:

```go
const (
    RetrievalComponentKey = "retrieval"
    NeutralRetrieval      = 0.5
)

func (c Belief) Retrieval() (float64, bool)   // stored value, clamped; (Neutral, false) when absent
func (c Belief) EffectiveRetrieval() float64  // stored, or NeutralRetrieval
```

**Neutral is 0.5, following salience, not 0 following inhibition.** The choice is
load-bearing, and it is the answer to the cold-start question. An absent component means
*"this belief has no access history"*, which is the state of every belief in an existing
brain and of every belief nobody has ever asked about. Neutral-and-absent must contribute
a zero ranking term, so an existing 80k-claim corpus ranks byte-for-byte as it does today
until the mechanism is actually exercised. This is the same inertness discipline salience
(neutral 0.5), inhibition (neutral 0) and Hebbian strength (base 1.0) each honour.

**A belief enters the system on its first recall and thereafter decays back toward
neutral — never below it.** Sub-neutral accessibility is already occupied by inhibition,
and letting disuse push a belief below neutral would produce the perverse ordering where
a belief recalled once a year ago ranks *below* one never recalled at all. The two terms
compose into a symmetric range without either duplicating the other: **disuse costs you
an earned bonus; losing an argument costs you a penalty.**

### 2. What decays it — the consolidation pass, not the wall clock

`consolidate --decay-retrievability` pulls every component-bearing belief toward
`NeutralRetrieval`:

```
R' = Neutral + (R − Neutral) · retain      // retain ≈ 0.8
```

removing the component entirely once `|R − Neutral|` falls below a floor, so the
mechanism self-clears and a brain that stops using it returns exactly to today's
behaviour. This is the identical asymptotic shape as `DecayAssociations` (toward base
1.0, `associationDecayRetain = 0.8`, `memory_impl.go:1288`) and `decayInhibition` (toward
0, `inhibitionDecayRetain = 0.5`, `memory_impl.go:1244`), implemented the same way:
read `ListAll`, merge one key, write through `ports.BeliefCreditWriter.ApplyBeliefCredit`
with **`c.TrustScore` passed through unchanged** (`internal/ports/interfaces.go:188-195`).
Passing the existing trust score through is not a convention — it makes a trust change
*structurally impossible* at this seam, which is the ADR-0011 "no silent trust change"
guardrail enforced by types rather than by review.

Sleep-driven rather than clock-driven decay is a real choice with a real cost, taken for
three reasons: it needs no timestamp and therefore no schema change; it makes the
mechanism a deterministic, reproducible function of the pass sequence (journalable, and
re-runnable offline against ADR 0018's record); and mnemos already models synaptic
renormalisation as something that happens during `consolidate` (`memory_impl.go:1064-1067`
— the framing is right even though that comment's "marked deprecated" parenthetical is
the defect noted above), consistent with the sleep-homeostasis account. The cost is
documented under Risks and the wall-clock alternative under Rejected.

### 3. What renews it — the testing effect, difficulty-weighted

A fourth write-back in the existing `applyRetrievalWriteBacks` seam
(`internal/query/engine.go:284-297`), alongside Hebbian, reconsolidation and inhibition.
No new seam, and the existing invariant holds: that function runs once on the answer
actually returned, never inside `answerWithEvents` (which runs twice under corrective
retrieval), and `TestAnswerEntryPointsApplyRetrievalWriteBacks` already fails the build
if a new entry point bypasses it.

The increment is **not** flat. Bjork's asymmetry says a retrieval is worth more the
harder it was, so:

```
ΔR = δ · (R_max − R)
```

A hot belief gains almost nothing; a cold-but-still-findable one gains most of the way
back. Three properties fall out for free: the accumulation saturates without a separate
cap (unlike `inhibitionMax = 0.5`, `internal/query/inhibition.go:26`); the popularity
feedback loop saturates fast, which is the main risk this mechanism carries; and
**rare-but-load-bearing knowledge self-heals on its first use after a long silence** —
the single most important safety property here.

Bounded to the same top-N focus as the sibling write-backs (`hebbianCoactivationTopN = 8`,
`internal/query/engine.go:337`), type-asserted on `ports.BeliefCreditWriter` so backends
that cannot persist components skip rather than mutate, and best-effort: a
retrievability-write failure never fails a read.

Flag: `query --retrievability` / `MNEMOS_RETRIEVABILITY`, gating the **write** only.

### 4. How it is read — a bounded, always-applied, symmetric ranking term

`internal/query/retrievability.go`, a copy of `salience.go`'s shape:

```go
const retrievalRetrievalWeight = 0.10 // half of salience's and inhibition's 0.20

func retrievalScoreDelta(c domain.Claim) float64 {
    return retrievalRetrievalWeight * (c.EffectiveRetrieval() - domain.NeutralRetrieval)
}
```

Added to the signal-bearing branch of `rankClaimsByHybrid`
(`internal/query/engine.go:1761-1775`), next to the existing salience and inhibition
terms. Range is ±0.05 on a similarity score normalised to `[0,1]` — **half the authority
of salience and inhibition** (`salienceRetrievalWeight = 0.20`,
`internal/query/salience.go:23`; `inhibitionRetrievalWeight = 0.20`,
`internal/query/inhibition.go:20`), because access frequency is the weakest epistemic
signal of the three. Popularity is not truth, and this is the only mechanism in the
system whose input is its own output.

Always applied, like inhibition and unlike salience — a persisted accessibility state
must keep biasing later recalls whether or not those recalls opt into writing — and inert
when the component is absent. The flag gates writing, not reading.

### 5. How it changes the retire gate — Mechanism A only, and narrowing it

Today `forgetStaleClaims` closes valid-time on trust alone (`memory_impl.go:1836`). That
is the storage/retrieval conflation in its most consequential form: a belief loses
recallability for being *poorly evidenced*, which is a statement about its grounding, not
about its accessibility. Under this ADR the gate becomes a **conjunction** — valid-time is
closed only when trust is below the floor **and** retrieval strength is at the cold end:
poorly evidenced *and* unused.

Concretely, this is one more `continue` before the `SetValidity` call at
`memory_impl.go:1854`. It guards **valid-time closure**, not a status change, and it
applies **only to Mechanism A** — `forgetStaleClaims`, and by the same argument
`forgetRefutedClaims` (`memory_impl.go:1786`), both of which are automatic sweeps run by
`consolidate` against a threshold.

**It must never touch Mechanism B, and that is a deliberate guardrail, not an omission.**
Every deprecation path is a human or an agent explicitly instructing the system to retire
a belief: MCP `forget` and `memory_deprecate`, a resolved contradiction's loser, `prune
--narration`. A usage statistic has no business second-guessing an explicit instruction —
"but people still look this up" is not a valid objection to "forget this". Retrievability
softens *automatic, inferred* forgetting; it has no vote on *deliberate* forgetting. This
also keeps the mechanism away from the unconditional `excludeDeprecated` filter, where a
mistake would be unrecoverable at read time (Mechanism A's exclusion is at least
bypassable with `--include-history`).

Within Mechanism A the change makes the gate strictly **narrower**. Retrievability is
never given the power to retire anything on its own; it is only ever given the power to
spare something. A warm-but-untrusted belief is kept, joining the three protections
already in place and checked before it: promoted lifecycle (`memory_impl.go:1833`),
intrinsic salience (`memory_impl.go:1844`), and same-pass replay protection
(`memory_impl.go:1850`). The new protection is distinct from all three: salience is
intrinsic stakes and by design does not decay; replay protection lasts one pass; lifecycle
is human curation. Retrieval strength is *observed usage across passes*, which none of
them measure.

### 6. How it composes with what exists

| Mechanism | Question it answers | Decays? | This ADR's relationship |
|---|---|---|---|
| **trust** (`internal/trust`) | Is this well-grounded and still true? | Yes, on *evidence* recency | **Untouched.** Storage strength. Retrieval strength never enters `Score`, `ScoreCredibility` or `RecomputeTrust`. |
| **salience** (ADR 0013 §4) | How much does getting this wrong cost? | **No** — intrinsic by design | Orthogonal. Salience protects from retirement; retrievability biases *ordering*. Both are bounded additive ranking terms and add independently. |
| **inhibition** (ADR 0016) | Was this beaten in an argument? | Yes, toward 0 | Same axis, opposite sign, disjoint trigger (verdicts vs. access). Together they make accessibility symmetric around neutral. |
| **association strength** (ADR 0015 §4/2b) | Which beliefs go together? | Yes, toward base 1.0 | Per-**edge**; retrieval strength is per-**belief**. Hebbian raises what a hit primes; retrievability raises the hit itself. |
| **plasticity** (ADR 0015 §1–2) | How fast should trust update? | n/a | Modulates *trust* updates. Retrievability never touches trust, so the two never interact. |
| **replay** (ADR 0015 §3) | What should be rehearsed this sleep? | n/a | `rankForReplay` should read retrieval strength as an input later — a cold-but-salient belief is the ideal rehearsal candidate — but that is explicitly **out of scope** here to keep the first change one mechanism. |
| **reconsolidation** (ADR 0015 §5) | — | — | Retrievability is the axis reconsolidation's signal *should have been* writing to. See §7. |

### 7. Observability — in scope, not deferred

Three surfaces, all cheap because the substrate exists:

- **Per-belief.** `retrieval` is a `confidence_components` entry, so it round-trips on
  every claim read and appears wherever components already do. It must be rendered in
  `--why-trust` explicitly labelled **not a trust contribution** — the report's weights
  sum to 1.0 by construction (`internal/trust/credibility.go:56-68`) and a retrievability
  row inside that sum would be a lie.
- **Over time.** A new ADR-0018 journal kind `retrievability`: one pass-level row with
  the distribution (percentiles + a cold-tail count) and per-belief rows for what the
  write-back moved, joining `consolidation` / `belief_trust` / `health`
  (`internal/domain/journal.go:12-20`). This is the instrument that decides the
  constants; they are guesses until it runs.
- **Right now.** A new ADR-0019 brain-health vital `cold_beliefs` — the fraction of
  currently-valid beliefs below a cold threshold — the access-side twin of the existing
  `staleness` vital (which is an evidence-recency proxy).

And the one that is genuinely new work: **`mnemos why-not-recalled <question>
<claim-id>`**. It replays the admission chain (`internal/query/admission.go:80`) and the
ranking terms for one belief against one question and reports either the gate that
dropped it or its score decomposition and the deltas that cost it its position.

The gate report must **name the two retirement mechanisms separately**, because they have
different causes and different remedies — conflating them in the explain surface would
reproduce the exact confusion documented above:

- *"deprecated"* — Mechanism B, a deliberate act. The report should name the actor and
  reason from `claim_status_history`. There is no read-time bypass; the remedy is to
  un-deprecate.
- *"valid-time closed"* — Mechanism A, and worth distinguishing *why*: superseded by a
  newer belief (`resolve --supersedes`), swept by `--forget-below-trust`, or refuted. The
  remedy is different and much cheaper: re-run with `--include-history` or `--at`.

Plus the ordinary filters (`MinTrust`, scope, visibility, lifecycle, durability) and,
when nothing excluded it, the ranking decomposition.

This is more work than the mechanism itself, and it is **deliberately in scope for the
same change**. ADR 0016 shipped a persisted, always-applied, invisible ranking penalty
with no way to inspect it; today nobody can answer "why did that not come back?" for any
reason at all. Adding a second invisible penalty without the explain surface would
violate the ADR-0011 guardrail that mnemos borrows the brain's dynamics but keeps the
machine's citation trail. Building it here pays down 0016's debt too.

## Blast radius

**Changes.**

- `internal/domain/retrievability.go` — new file (~40 lines), mirroring `salience.go`.
  No `Validate()` change.
- `internal/query/retrievability.go` — new file (~15 lines), mirroring `salience.go`.
- `internal/query/engine.go` — one `AnswerOptions` field; one line in
  `rankClaimsByHybrid`'s signal-bearing branch (`:1761-1775`); one call in
  `applyRetrievalWriteBacks` (`:284-297`) plus its ~30-line implementation.
- `memory_impl.go` — `decayRetrievability` (a near-copy of `decayInhibition`, `:1252`);
  `ConsolidateOptions.DecayRetrievability` + a result count; the conjunction and the new
  protection in `forgetStaleClaims` (`:1836`, `:1844-1853`).
- `cmd/mnemos` — two flags + help text; an entry in `sleep_schedule.go`; the
  `why-not-recalled` command.
- `internal/config` — two leaves + `mnemos.example.yaml` + `EnvOverrides`.
- One journal kind, one health vital.

**Does not change.**

- **The store schema.** No column, no `sql/sqlite/schema.sql` edit, no `make sqlc`, no
  per-backend `rows.Scan` parity sweep, no Postgres RLS `scoped` registration, no live
  pg/mysql round-trip gate. This is the single largest risk in mnemos changes of this
  kind and it is avoided entirely.
- **`internal/trust`.** Not one line. Trust remains `f(confidence, evidence,
  freshness-of-evidence)`. The separation is enforced at the storage seam by
  `ApplyBeliefCredit(ctx, id, merged, c.TrustScore)` — the write cannot move trust.
- **`admitClaims`.** No new filter, no new gate, no new reason for a belief to be
  excluded from an answer. Retrievability is ranking-only.
- **`ClaimStatus`** and every Mechanism-B path — `forget`, `memory_deprecate`,
  contradiction-resolution losers, `prune --narration`, the dedupe tombstones — plus
  `excludeDeprecated`, `claim_status_history`, and the REST / gRPC / MCP wire. Deliberate
  retirement is untouched by design (§5).
- **`valid_from`/`valid_to` semantics.** The columns, the `IsValidAt` predicate and the
  `--include-history` / `--at` bypass all keep their current meaning; the only change is
  one additional condition guarding when consolidation *writes* a `valid_to`.
- **Salience, inhibition, Hebbian strength, plasticity, credit.** All additive and
  independent; the `retrieval` key is a non-`credit:` component so the credit pass
  preserves it and `RecomputeTrust` ignores it.

## Hard questions, answered

**Does a low-retrievability belief still surface for a direct lookup?**
Yes, and structurally rather than by tuning. (a) It is not in `admitClaims`, so no
retrieval-strength value can remove a belief from a result set. (b) `get_belief`,
`list_beliefs`, `--why-trust`, `timeline_query`, `export` and every point-in-time path
never call `rankClaimsByHybrid` at all — they are unaffected by construction. (c) For a
question that names the belief, cosine and BM25 both approach the normalisation maximum,
where a ±0.05 delta cannot displace it. The term is sized to reorder near-ties, exactly
as salience's is (`internal/query/salience.go:18-24`). This is the categorical difference
from both existing mechanisms: a `deprecated` status or a closed `valid_to` *removes* a
belief from recall; a retrieval-strength value can only *reorder* one.

**What stops useful-but-rare knowledge decaying into unreachability?**
Four independent guards, any one of which suffices. (1) Decay targets **neutral**, not
zero — the asymptote of infinite disuse is "unmarked", which is where every belief starts
and is a zero ranking term. Unreachability is not in the reachable state space. (2) The
term is bounded at ±0.05. (3) The difficulty-weighted increment `δ·(R_max − R)` means the
one recall a rare belief eventually gets restores most of its strength immediately — the
mechanism is biased toward recovery, which is the direct inverse of the failure mode.
(4) The retire gate is narrowed, not widened; promoted lifecycle and intrinsic salience
still short-circuit before retrievability is consulted at all.

**How is it observable and debuggable?**
Per-belief through `confidence_components` and `--why-trust` (labelled as non-trust);
over time through the `retrievability` journal kind, which is what the constants will
actually be tuned against; in aggregate through the `cold_beliefs` health vital. The
constants in this ADR are stated as design intent, not as tuned values — ADR 0018 exists
precisely because the previous mechanisms' constants were guesses that nobody could
check.

**Can a user tell why something was not recalled?**
Today, no — for any reason, not just this one. That is the honest state, and it is why
`why-not-recalled` is in scope for the same change rather than deferred. It will report
the dropping gate or the score decomposition. What it will **not** do is search the whole
corpus for near-misses on an unbounded question; it answers "why not *this* belief", which
is the question people actually have and is a bounded computation.

**What if a brain never sleeps?**
Then nothing decays and every recalled belief keeps its bonus. Retrieval strength
saturates near `R_max` for the working set and the term degenerates into a small, static
"has been used" bonus. That is a benign degradation but a real limitation, and it is
inherited from — not introduced by — `--decay-inhibition` and `--decay-associations`.

## Alternatives rejected

**A. A `retrieval_strength` + `last_recalled_at` column pair on `claims`, with wall-clock
decay (ACT-R base-level activation, `B = ln Σ tⱼ^−d`).**
The textbook model, and strictly better on fidelity: continuous decay, correct spacing
dynamics, no coupling to consolidation cadence. Rejected on cost/benefit. It is the
ADR-0015 Batch-2 trap in full — five backends with no shared scanner, sqlc regeneration,
the Postgres RLS `scoped` array (a table or column missed there leaks across tenants),
and live Postgres + MySQL round-trip gates — spent on a mechanism whose entire output is
a ±0.05 ranking nudge. The component route buys the behaviour at none of the parity risk.
Revisit **only** if the ADR-0018 journal shows per-pass decay is measurably too coarse;
decide on data, not on aesthetics.

**B. Derive retrieval strength from the existing `LastVerified` timestamp.**
Zero new state, and `reconsolidateRecalled` already bumps it on recall
(`internal/query/engine.go:377-389`) — the signal is sitting right there. Rejected
because it conflates the operator's deliberate `verify`/`reconsolidate` (an epistemic
act: *I checked, it is still true*) with an automatic access (*someone asked a question
that matched*). Those are different facts about a belief and the whole thesis of this ADR
is that they must be different quantities. Worse, `LastVerified` already feeds
`trust.IsStale`, `Staleness`, `EvaluateLiveness` and the credibility recency signal
(`internal/trust/credibility.go:112-127`), so deriving accessibility from it would
entangle the two axes *harder* rather than separating them. Adopting this would be
implementing the ADR by making its problem worse.

**C. Add a `dormant` / `cold` value to `ClaimStatus`.**
Rejected. It replaces one binary with a slightly longer ladder while keeping the
categorical shape that is the actual complaint. `ClaimStatus` is the
contradiction/verification axis with documented transitions and a persisted
`claim_status_history` (`internal/domain/types.go:96-110`); accessibility is orthogonal
to it, on exactly the argument `ClaimLifecycle` is documented with at
`internal/domain/types.go:112-123`. It would also force every backend, every wire format
and `excludeDeprecated` to learn a new value — a released-contract change (ADR 0011 §5)
— to express a continuous quantity that does not want to be an enum.

The two-mechanism picture makes this worse still: automatic forgetting is Mechanism A and
**never reads or writes `Status` at all** (`forgetStaleClaims` contains no reference to
it). Putting a disuse signal on `ClaimStatus` would site the graded axis on precisely the
column the disuse-driven sweep does not use, and would then need a second rule to
reconcile `dormant` with an independently-closed `valid_to`.

**D. Make the term multiplicative, or add a hard cutoff below a threshold.**
Rejected, and this is the most important rejection. Any multiplicative or gating form
gives retrieval strength the power to *remove* a belief from an answer, which reinstates
the binary this ADR exists to dissolve. It also creates an absorbing state: a belief that
falls below the cut can never be recalled, therefore can never be retrieved, therefore can
never be strengthened — irreversible forgetting delivered by a mechanism advertised as
reversible. Bounded additive keeps every state recoverable, which is the property that
makes the mechanism safe to turn on.

**E. Global disuse decay — decay every belief every pass, including never-recalled ones.**
The purest reading of the New Theory of Disuse: never accessed *means* low retrieval
strength. Rejected for now on two grounds. Behaviourally, it makes the mechanism
non-inert on an existing corpus — every belief acquires a component on the first sleep,
so the first pass reorders a brain nobody asked to change. Operationally, it is a
full-store rewrite on every consolidation pass, which is exactly the cost pattern that
forced the incremental trust-scoring change (a write whose cost grows with the brain,
which on an 11k-claim store blew the governed-write budget). And it buys nothing at
first: uniform decay across all beliefs is rank-neutral by construction. Revisit if the
journal shows the recalled population has grown large enough that "never recalled" stops
being the overwhelming default.

**F. Fold retrievability into the trust score as another multiplicative factor**
(the shape `ScoreWithAuthority` already uses).
Rejected: it is the bug, restated as a feature. Trust answers "do I have reason to
believe this is still true?"; making it move with access frequency means popular beliefs
become true — the confabulation failure mode ADR 0011 explicitly refuses to imitate. It
would also break `--why-trust`'s contribution accounting (weights sum to 1.0,
`internal/trust/credibility.go:56-68`) and reopen the "no silent trust change" guardrail.

**G. Do nothing — ADR 0016's inhibition already provides graded retrievability.**
The honest steelman, and it is half right: inhibition is a persisted, decaying, bounded,
retrievability-only ranking term, and this ADR copies its shape. Rejected because its
input is contradiction verdicts and nothing else — a belief loses accessibility only by
losing an adjudicated argument, which requires a rival, an agent-mode query and a decisive
margin. Disuse produces no signal. And decaying to 0 makes it a model of *transient
suppression*, not of *accumulated accessibility*: after a few sleeps every belief is back
to identical accessibility regardless of whether it has been used a thousand times or
never. It covers one quadrant of one axis.

## Risks

- **Rich-get-richer.** This is the only mechanism in mnemos whose input is its own output:
  recall raises `R`, `R` raises rank, rank raises the chance of recall. Mitigated by the
  halved weight (±0.05), the saturating increment (a hot belief gains almost nothing, so
  the loop damps itself within a few recalls), and decay toward neutral. **Not eliminated.**
  The `retrievability` journal kind exists to measure whether the top-N distribution
  concentrates over months; if it does, the response is a smaller weight or a per-pass
  write cap, decided on the data.
- **Coupling to consolidation cadence.** A brain that never sleeps never cools; one on a
  tight sleep cron cools fast. Inherited from `--decay-inhibition` and
  `--decay-associations`, not introduced here, but this mechanism will make it more
  visible because far more beliefs carry the component. Alternative A is the escape hatch,
  gated on measurement.
- **A second invisible ranking term.** Two persisted, always-applied, uninspectable
  ranking adjustments is one more than this project should have. The explain surface is in
  scope for exactly this reason; if it slips, the mechanism should slip with it.
- **Decay-pass cost.** `decayInhibition` already does a full `ListAll` plus one write per
  component-bearing claim. Retrievability's population will be far larger (everything ever
  recalled, rather than only contradiction losers). Bound the pass with a write cap and
  journal the truncation; the self-clearing floor keeps the population from growing without
  limit.
- **A slower-shrinking brain.** Narrowing the retire gate to a conjunction means fewer
  valid-time closures per pass. An operator relying on `--forget-below-trust` to control
  growth will see it control less. That is the intended semantics — "poorly evidenced" was
  never a good reason to make something unreachable — but it must be announced, and the
  old behaviour must remain available by simply not enabling the flag. Note this affects
  only Mechanism A; the deliberate deprecation paths retire exactly as many beliefs as
  before.
- **One more thing to get wrong about which column moves.** This ADR adds a third
  accessibility-adjacent quantity to a codebase whose own comments already disagree about
  the two it has (see the defect in Context). Any tooling, census or eval harness reading
  "was this forgotten?" must now check `Status`, `valid_to` **and** the `retrieval`
  component. The defect should be fixed before this ships, not after.
- **Constants are guesses.** `retain`, `δ`, `R_max`, the cold threshold and the weight are
  all stated as design intent. None should be treated as tuned until the journal has run
  on a real brain, exactly as ADR 0018 argued for the mechanisms that preceded this one.

## Consequences

- **Positive.** The system's own repeated claim — forgetting reduces retrievability rather
  than erasing history — becomes true of the retrieval path and not only of the storage
  path. Beliefs gain a third state between current and retired: still active, still
  trusted, still cited, but *cold* — ranked below warmer peers, fully returned on a direct
  lookup, and restored to near-full accessibility by a single successful recall. The
  storage/retrieval blur documented in Context is resolved in the right direction: the
  testing-effect signal gets its own axis instead of quietly inflating credibility and
  salience.
- **Guardrails preserved.** Retrievability-only (trust passed through unchanged at the
  storage seam), bounded (tips ties, cannot sink a better match), reversible (decays to
  neutral and self-clears), ranking-only (never an admission gate), inert when absent
  (an untouched corpus is byte-for-byte unchanged), opt-in to write, and observable
  (journal + health vital + explain surface).
- **Negative.** Two more flags and two more config leaves on an already-large cognitive
  surface. A feedback loop that must be watched rather than merely bounded. And a
  deliberate refusal of the more faithful wall-clock model in favour of the one that does
  not require a five-backend migration — a trade that should be revisited with data, not
  defended forever.

## Rollout

Sequenced so each step is independently valuable and reversible.

0. **Fix the documentation defect first** (a separate, tiny PR; **not** part of this
   design-only change). Correct `memory.go:643-645` and `memory_impl.go:1064-1067` to say
   valid-time closure rather than "marked deprecated", and un-interleave the
   `forgetStaleClaims` doc comment from the `salienceProtectFloor` const at
   `memory_impl.go:1794-1810`. It is a prerequisite, not housekeeping: the next several
   steps all reason about which column moves, and the comments currently say the wrong
   one. It has already cost the evaluation-harness lane a wrong census.
1. **Domain + read term, inert.** `retrievability.go` in `domain` and `query`, plus the
   term in `rankClaimsByHybrid`. Nothing writes the component yet, so recall is provably
   unchanged; the test asserting that is the gate.
2. **Explain surface first.** `why-not-recalled`, covering the existing admission gates
   and ranking terms *including inhibition*. Landing this before anything writes
   accessibility state is the point — it means the first cold belief is inspectable on the
   day it becomes cold.
3. **Write-back + decay.** `query --retrievability` and `consolidate
   --decay-retrievability`, both opt-in and off by default, plus the `retrievability`
   journal kind. Ship these together; a strength that rises and never falls is worse than
   none.
4. **Measure.** Run on a real brain for several weeks. Read the journal: distribution,
   concentration in the top-N, cold-tail size, recovery rate after a long silence. Tune the
   constants here, not before.
5. **Retire-gate conjunction + `cold_beliefs` vital.** Only once step 4 shows the
   distribution is sane — this is the step that changes what gets retired, so it earns its
   evidence first.
6. **Cognitive defaults.** Add to `WithCognitiveDefaults` (the production-path enabler,
   `internal/query/engine.go:275`) only after 4 and 5, following the precedent that ADR
   0016 shipped opt-in before becoming a default.

Deliberately out of scope: feeding retrieval strength into `rankForReplay` (a cold,
salient belief is the ideal rehearsal candidate — a natural follow-up, but it would make
this two mechanisms), and any wall-clock/column model (Alternative A, gated on step 4).
