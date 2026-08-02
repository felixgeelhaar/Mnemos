# brainbench — do the cognitive processes improve what the brain knows?

Mnemos runs continuous cognitive processes: consolidate, forget, reinforce,
credit, salience, synthesize, decay. Before this harness, nothing measured
whether any of them **improves** anything. `mnemos consolidate` prints counters
— `forgotten: 412`, `associations_decayed: 900` — which establish that the pass
**ran**, not that the brain got better. That gap made every claim about the
brain improving unfalsifiable.

This harness closes it, and is built so that it can return bad news.

```bash
make brain-eval          # human table + brainbench.json
make brain-eval-strict   # same, exit 1 if any scored metric degraded
go run ./tools/brainbench -human=false -json -   # JSON to stdout
```

## Method

A paired A/B experiment on byte-identical brains.

1. **Seed** one pristine brain from the scenario corpus, document by document,
   reproducing the production ingest path (`mnemos process --embed`) offline:
   rule-based extraction, ingest-time exact-text dedupe, within-batch and
   incremental relationship detection, then event and claim embeddings.
2. **Warm up** (optional per scenario) by running recall against the seed, so
   retrieval-created state — Hebbian association strength, competitive
   inhibition — exists for the decay stages to act on.
3. **Copy** the seed file three times. The arms are byte-identical, so they
   cannot differ for any reason except the treatment.
4. **Control** — measured, untreated.
5. **Treatment** — the process set runs, then it is measured.
6. **Noise arm** — a second untreated copy, measured independently. It must
   reproduce the control's numbers exactly. When it does not, the run is
   reported `invalid-nondeterministic-measurement` and no delta is attributed.

Measurement is itself a mutation — recall applies the Hebbian, reconsolidation
and inhibition write-backs — which is why every arm is a throwaway copy measured
exactly once, and why the census is read before the probes fire.

## What it measures

| Metric | Direction | What it tells you |
|---|---|---|
| `answer_hit_rate`, `answer_precision_at_1`, `answer_mrr` | higher better | Does the right claim come back, and how high? |
| `forbidden_hit_rate` | lower better | Does a superseded statement still surface? Unknown when no probe declares `must_not`. |
| `gold_survival` | higher better | **Is the knowledge still there at all?** |
| `contested_claims`, `contradiction_edges` | lower better | Unresolved dissonance. |
| `noise_fraction` | lower better | Share of valid claims that `extract.IsJunk` — mnemos's own shipped narration filter — rejects. |
| `vital_*` | lower better (`skill_coverage` higher) | The ADR 0019 vitals, mirrored in. `HealthUnknown` maps to unknown, never to 0. |
| `pathology_*` | lower better | The ADR 0019 integrity checks. |
| `valid_claims`, `invalidated_claims`, `status_*`, `events`, `relationships`, `evidence_links`, `mean_trust`, `db_bytes` | **descriptive** | Reported, never scored. |

`gold_survival` is the metric that makes the rest trustworthy. A process can
raise precision, cut dissonance and shrink the brain simply by deleting claims.
Only this metric notices, and `TestRun_ReportsRegressionWhenKnowledgeIsDestroyed`
asserts that a brain-destroying configuration is reported as a regression.

**Why the descriptive category exists.** Forgetting mechanically shrinks the
claim count and mechanically raises mean trust, because it deletes the low-trust
tail. Scoring "fewer claims" or "higher mean trust" as improvements would make
the harness report success for a process that did nothing but destroy
knowledge — for *every* forgetting configuration, including one that deleted the
whole brain. Those numbers are still shown, because they are how a reader
notices a brain was gutted.

## What it does **not** measure

The full list ships inside every report, because a JSON file gets pasted into an
issue or a prompt without its documentation. In short:

- **The LLM path.** Extraction is rule-based, no LLM is configured. Says nothing
  about LLM extraction, LLM causal detection, grounded generation, or
  `consolidate --clear-session-noise`.
- **Real embeddings.** The embedder is a deterministic hashed bag-of-words stub.
  It sees exact and near-lexical restatements and misses true paraphrases, so
  reported dedupe behaviour is a **lower bound** on production.
- **Scale.** Scenarios hold tens of claims, not thousands.
- **Repetition.** One consolidation pass, not a series of nightly ones.
- **Cross-tenant promotion.** `consolidate --promote` (ADR 0011) is out of scope.
- **Downstream utility.** These are properties of the store and of retrieval.
  Whether an agent using this brain made a better decision needs a task
  benchmark, not a store diff.
- **Statistical significance**, which would be meaningless: the run is
  deterministic, so a delta is exact for its scenario and carries no confidence
  interval across scenarios. The noise arm is the empirical substitute.

## Writing a scenario

Scenarios live here as `*.yaml`. The format deliberately echoes
`../retrieval.yaml`'s seeds/query/gold idiom, but the unit of work differs:
`retrieval.yaml` compares **rankers** over one store, this compares **stores**
that a process has or has not been applied to.

```yaml
id: my_scenario
description: One sentence on the question this scenario settles.

corpus:
  - id: s1
    source: session-label        # optional; defaults to the doc id
    age_days: 400                # back-dates the event; see below
    text: |
      The service runs on PostgreSQL 16.

warmup:                          # optional; creates state for the decay stages
  - which database does the service use

probes:
  - id: p1
    query: which database does the service use
    expect: PostgreSQL 16        # case-insensitive substring
    must_not:                    # optional
      - MySQL

process:                         # the ConsolidateOptions under test
  dedupe_threshold: 0.92
  forget_below_trust: 0.45
  decay_associations: true
```

Three things to get right:

- **`age_days` is not cosmetic.** Trust freshness decays on a 90-day half-life.
  With everything stamped "now", the forgetting stage can never fire and the
  scenario measures nothing while still printing a table.
- **`warmup` is what makes the decay stages measurable.** On a freshly-ingested
  brain no association edge is above base and nothing is inhibited, so
  `decay_associations` and `decay_inhibition` report zero — "untested", which
  reads identically to "inert" and is a much weaker claim.
- **Expectations match text, not claim ids.** Claim ids are minted per ingest
  and are not stable across runs.

Unknown YAML keys are rejected. A misspelled process key would otherwise be
silently ignored and the scenario would test a different process set than its
author wrote, while still producing a credible-looking report.

## Reading a result

Read in this order, which is how the human report is laid out:

1. **Validity.** If measurement was not deterministic, stop; nothing below is
   attributable.
2. **Activity.** Did the process mutate anything? Zero means `inert` — the stage
   was not exercised, which is not the same as "the stage is neutral".
3. **The rows.** Deltas at or below the `1/n` resolution floor are annotated;
   on a six-probe suite that is one probe moving, not a trend.
4. **`gold_survival`.** Any improvement bought alongside a survival regression
   was bought by deleting knowledge.
5. **The verdict**, last. `mixed` means read the rows.
