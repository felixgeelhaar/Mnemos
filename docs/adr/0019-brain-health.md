# ADR 0019: Brain health — vital signs, integrity checks, and health-over-time

- **Status:** Accepted; implemented (v0.97.0).
- **Date:** 2026-07-13
- **Deciders:** Felix Geelhaar
- **Scope:** Diagnostics / observability. Answers "is the brain *healthy* right now, and
  is its health trending the right way?" — which nothing before this could.

## Context

Mnemos has liveness checks (`/health`, `/internal/ready`, `doctor` — DB/LLM/embedding
reachability + version) and it has many **cognitive vital signs**, but they are
**scattered**: calibration, predictive-error/free-energy, hypercorrections (dissonance),
knowledge gaps, and trust counts each live behind a *separate* command or endpoint, with
**no rollup, no thresholds, and no single verdict**. Three things are missing entirely:

1. **A unified health verdict.** Nothing aggregates the vitals into "healthy / degraded /
   unhealthy" with thresholds, so an operator (or the hosted products) cannot ask one
   question and get one answer.
2. **Structural-integrity / pathology detection.** There is no check for orphan claims
   (beliefs with zero evidence), dangling relationships (edges to missing claims), or a
   backlog of overdue unresolved predictions. A brain can be *corrupt* and nothing notices.
3. **Health over time.** Vitals are strictly point-in-time; there is no log of the brain's
   vital signs on a health cadence (free-energy is only incidentally captured inside
   `--journal` consolidation passes).

(Also: `quality`'s `stale_count` is a dead value — never incremented — so the unused
`CheckSLOs` divides by a structural zero. Fixed here in passing.)

## Decision

Add a single read-only **brain-health** surface that rolls the scattered vitals plus new
integrity checks into one verdict, and that can snapshot itself to the cognitive journal
so health becomes a **queryable time series**.

### `Memory.BrainHealth` — one verdict

`BrainHealth{ Status, Vitals[], Pathologies[], At }` where `Status` is
`healthy | degraded | unhealthy` (the worst of all component statuses). Strictly
read-only.

**Vitals** (each a value + its own OK/WARN/CRIT threshold; the impl knows each one's
direction). v1 set, reusing existing producers:

- **free_energy** — `PredictiveError().Total` (the overall prediction-error aggregate).
- **calibration** — `Calibration().ECE`.
- **dissonance** — active hypercorrections per belief (`Hypercorrections`).
- **low_trust** — fraction of currently-valid beliefs below a trust floor.
- **staleness** — fraction of currently-valid beliefs not verified within a staleness
  horizon (a documented recency proxy for freshness decay).

**Pathologies** (integrity checks that did not exist before — counts, escalating with
severity):

- **orphan_claims** — currently-valid beliefs with **zero evidence** (a claim requires
  evidence; an orphan is a data-integrity smell).
- **dangling_edges** — relationships whose endpoint claim **does not exist** (referential
  corruption — CRIT).
- **stale_expectations** — **open** expectations whose horizon is **past** (a backlog of
  predictions the brain never reconciled — its prediction loop is stalling).

### `mnemos health` — the surface

`mnemos health [--human] [--journal]`. Default: the current verdict (JSON, or a readable
table with `--human`). `--journal`: also append a **`health`** entry (a new journal kind,
reusing the ADR-0018 `cognitive_journal`) — so running it on a cadence (cron, or after
each consolidation) builds a **health-over-time series** with no new table. Read the
series with `mnemos journal --kind health`.

## Consequences

- **Positive.** One question, one answer: is the brain healthy? Adds the pathology
  detection that was entirely absent (a corrupt brain is now visible). Reuses the journal
  so health-over-time is free and plots alongside free-energy. The hosted products get a
  real cognitive health check, not just a liveness ping.
- **Cost / guardrails.** Read-only; **no new table** (health snapshots reuse the ADR-0018
  journal). A full-scan diagnostic (lists claims/evidence/relationships once) — meant to
  be run on demand or on a modest cadence, not per request. Thresholds are conservative
  defaults, documented; tuning them against real data is exactly what the journal (ADR
  0018) now enables.
- **Negative / scope limits (v1).** The staleness vital is a recency proxy, not the full
  liveness model; the vital set is curated, not exhaustive (schema-health, embedding
  coverage deferred); thresholds are fixed constants (not yet config-driven).

## Rollout

Shipped in v0.97.0: `Memory.BrainHealth`, `mnemos health` (+`--human`, `--journal`), the
five vitals, the three integrity checks, the new `health` journal kind + `journal --kind`
filter, and the `stale_count` fix — with tests over a healthy brain, each pathology, and
the health-snapshot round-trip. Threshold tuning rides on ADR 0018's journal.

## Amendments

### `unknown` — a vital with no data behind it (v0.122.0)

A sixth status, `unknown`, distinguishes "measured and fine" from "never measured".
Calibration reported an ECE of 0.0000 over **zero** adjudicated beliefs and rendered
green. `unknown` does **not** roll up into the overall verdict — absence of evidence of
a problem is not evidence of one — so a fresh brain is not degraded on day one. Also
added `skill_coverage`: every other vital is computed over *claims*, so a brain with
86,505 beliefs and zero actions/outcomes/lessons showed five green vitals.

### `trust_decay` — the rate, not the level (this change)

The v1 set reported trust as two point-in-time **levels**: `low_trust` (already below the
floor) and `staleness` (missed a fixed 90-day horizon). Neither answers *is trust
falling, and how fast* — and neither reads the decay model the brain actually carries.
`domain.Claim.HalfLifeDays` is stamped at ingest by the volatility classifier and honoured
by query staleness, curiosity and float-back, but it was **monitored nowhere**: a brain
could be shedding its volatile beliefs on a fortnightly half-life with every vital green.

- **What it measures.** The share of **currently-trusted** beliefs whose trust falls
  through the same floor `low_trust` uses (0.30) within **30 days** if nothing
  re-verifies them. The leading indicator to `low_trust`'s lagging one; directly
  actionable, since it names a month of re-verification work in advance.
- **How.** The real decay model (`trust.ScoreWithHalfLife`) evaluated at two instants
  over real stored data — each belief at **its own** half-life. No sampling, no estimate.
  Deliberately conservative in two places, both under-reporting decay: the freshness
  reference is the belief's `ValidFrom`/`LastVerified` (never newer than its freshest
  evidence, which the scan does not load), and the evidence count is ungraded for
  independence (which can only overstate corroboration).
- **Why a crossing rate, not aggregate trust lost per unit time.** The aggregate is
  perverse: exponential decay is steepest right *after* evidence arrives, so a brain
  being actively fed would score worst, and one whose every belief had already bottomed
  out on the freshness floor would score a clean zero.
- **`unknown` when no belief is above the floor** — an empty brain and a fully-decayed
  one both have no trust left to lose, and a green 0 for the second is exactly the
  failure the `unknown` status exists to prevent. `low_trust` owns that state.
- **Thresholds are derived, not chosen.** `warn = low_trust_warn / 3`,
  `crit = low_trust_crit / 3`: the outflow that, sustained over a quarter (three
  horizons), carries a fully-trusted brain to `low_trust`'s own warn/crit line
  unaided. Re-tuning the floor's thresholds re-tunes the rate that fills it, rather than
  leaving two constants to drift apart. (Approximate — it ignores the compounding as the
  trusted base shrinks, which errs toward warning early.)
- **Known limit.** The *persisted* `trust_score` is still recomputed with the **global**
  half-life (`pipeline.defaultTrustScorer` and `Consolidate` both call `trust.Score`), so
  it cannot see a volatile belief. The vital therefore reads the model rather than the
  stored column; both its endpoints come from the same evaluation, so the comparison is
  internally consistent. Making the persisted score honour `HalfLifeDays` is a separate,
  larger change (it moves everything that ranks on trust).

Same change: `low_trust` and `staleness` now grade with their sample count — 0/0 valid
beliefs is `unknown`, not healthy — and `--human` renders `unknown` as `[ ?? ]` instead
of falling through to `[ ok ]`, which had been undoing the v0.122.0 fix for every reader
who did not pipe the output through `jq`.
