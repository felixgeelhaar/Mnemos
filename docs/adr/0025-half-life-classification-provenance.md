# ADR 0025: `half_life_days = 0` is two different facts — record classification provenance separately

- **Status:** Proposed. **Design only — nothing implemented.** No Go file, no SQL file and
  no schema is touched by this ADR.
- **Date:** 2026-08-02
- **Deciders:** Felix Geelhaar
- **Scope:** The representation of per-claim freshness half-life provenance on the
  `claims` row. Completes the work started in #331 (the half-life was computed and dropped
  at the store boundary), #334 (write path fixed) and #336 (`mnemos recompute-half-life`
  backfill). Touches the domain belief type, the four SQL backends' claim schema, the
  backfill command and metrics. Does **not** touch `internal/trust` — the scoring contract
  is unchanged by design — and does not change any recall behaviour on the day it ships.
- **Refs:** #331. Follows the marking discipline of ADR 0023 (durability) and the
  no-migration-by-map-key discipline of ADR 0013 §4 / ADR 0014 / ADR 0016 — one of which
  this ADR adopts and the other of which it deliberately declines, for reasons recorded
  below.

## Context

### One column, two facts

`extract.HalfLifeFor(claimType, text) float64` (`internal/extract/volatility.go:68`)
returns `0` in two situations that have nothing to do with each other:

1. **The claim is durable.** The classifier read the text, matched `durableRE` or matched
   nothing, and decided this belief should not decay faster than the default. This is a
   *verdict*.
2. **The classifier never ran on it.** The claim predates #334, or came in through a write
   path that does not classify. This is the *absence* of a verdict.

Both store `0`. `trust.ScoreWithHalfLife` (`internal/trust/trust.go:52-64`) treats
`halfLifeDays <= 0` as "use the 90-day `FreshnessHalfLifeDays` default", and so do
`IsStale` (`:77`) and `Staleness` (`:97`). For *scoring* the two cases are genuinely
identical, which is why this survived unnoticed: the number behaves correctly. It is
everything downstream of the number that cannot function.

### Measured, today, on the real brain

Read-only against a copy of the 465 MB production brain (2026-08-02), and independently of
the figures quoted in #336:

| | count | |
|---|---:|---|
| claims, total | 88,814 | |
| live (not deprecated) | 68,015 | what recall can reach |
| `half_life_days = 0` | **88,814** | every row in existence |
| `half_life_days > 0` | 0 | `recompute-half-life` has not been run here |
| classifier verdict *volatile* (live) | 3,064 | would store 14 |
| classifier verdict *durable* (live) | **64,951** | would store 0 — **indistinguishable from unclassified** |

So the backfill can reach 4.5% of the live set. Running it to completion leaves 64,951
rows carrying a verdict that is bit-for-bit identical to the state they were in before the
pass ran. The pass cannot report that it finished, because "finished" has no
representation.

### The three things this makes impossible

1. **The backfill can never be *done*.** `recompute-half-life`'s selection is
   `if c.HalfLifeDays != 0 { skip }` (`cmd/mnemos/recompute_half_life.go:134`). After a
   complete, successful, verified run, 95.5% of live rows still match that predicate. Every
   subsequent run redoes all of them. #336 records "no resume-from-cursor" as a known
   limitation; the deeper truth is that there is nothing a cursor could be built from.
2. **A classifier improvement cannot be targeted.** `HalfLifeFor` is deliberately
   under-catching — `volatility.go:23-26` says so, and the 4.5% match rate is the
   consequence. Widening it is expected work. But "which rows did v1 look at, so that v2
   can revisit them?" has no answer, so the only available action is to reprocess the
   entire corpus every time, forever.
3. **Coverage is unreportable.** Nothing can answer "how much of this brain has been
   volatility-classified?" — not `knowledge_metrics`, not the ADR 0019 vitals, not
   `mnemos metrics`. The `trust_decay` vital (#328) evaluates `ScoreWithHalfLife` per
   belief at its own half-life without being able to say how many of those half-lives are
   *measured* versus *defaulted*, which is precisely the distinction ADR 0019 insists on
   elsewhere ("unmeasurable means `unknown`, never 0").

### The neighbouring field does not have this problem

`Durability` (ADR 0023, `internal/domain/durability.go`) answers an almost identical
question — a text classifier's verdict about a belief — and answers it cleanly, because it
has an explicit third value:

```go
DurabilityUnknown     Durability = ""        // unclassified
DurabilityDurable     Durability = "durable"
DurabilitySessionLocal Durability = "session"
```

On the same brain, that column reports: **79,331 unknown, 2,645 durable, 6,838 session** —
10.7% classified, a number that exists because "unknown" is representable.
`cmd/mnemos/classify_durability.go` is built entirely on it: `unclassifiedNewestFirst`,
`countUnclassifiedRecallable`, `unclassifiedAmong`, and a progress line that prints
coverage. None of those functions has a half-life equivalent, and none could be written.

The difference is not that durability is more important. It is that durability's storage
type has room for the absence of a verdict and `half_life_days`'s does not: `REAL NOT NULL
DEFAULT 0` where `0` is also a legal, meaningful, load-bearing value.

### The general shape of the defect

A **quantitative** column (how fast does this decay?) was asked to also carry a
**categorical** fact (did anyone look?). Every proposal that keeps encoding the second
inside the first inherits the collision in a new place; the ones examined below all do,
and each fails on a different day.

## Decision

**Add one column to `claims` recording *which classifier assigned the half-life*, and
leave `half_life_days` semantically untouched.**

```
claims.half_life_classifier   TEXT NOT NULL DEFAULT ''
```

- `''` — no classifier has assigned this claim's half-life. The value in
  `half_life_days` is whatever it was: a default, or a human override via `MarkVerified`.
- `"volatility/v1"` — `extract.HalfLifeFor` at version 1 read this claim's text and
  assigned the value now in `half_life_days`. That value may be `14` (volatile) or `0`
  (durable — **use the store default**), and those are now distinguishable.

Domain side: `Belief.HalfLifeClassifier string`, with
`func (c Belief) HalfLifeClassified() bool { return c.HalfLifeClassifier != "" }`, and a
version constant in `internal/extract` alongside the classifier it names:

```go
// VolatilityClassifierVersion identifies the revision of HalfLifeFor that
// produced a stored verdict. Bump on any change to the lexicons, the veto or
// the normaliser, so already-classified rows can be found and revisited.
const VolatilityClassifierVersion = "volatility/v1"
```

The `<classifier>/<version>` shape mirrors `extract.PromptVersion` (`"v1.5"`), which
exists for exactly this reason one layer up: to know which revision of a text-processing
rule produced a stored artefact.

### The four states, read together

| `half_life_classifier` | `half_life_days` | meaning |
|---|---|---|
| `''` | `0` | **Unknown.** Nobody has looked. The entire pre-#334 corpus. |
| `''` | `> 0` | **Human override.** `MarkVerified` set an explicit half-life; no classifier involved. |
| `volatility/v1` | `0` | **Classified durable.** The classifier looked and declined to shorten. Decays at the 90-day default *by decision*. |
| `volatility/v1` | `14` | **Classified volatile.** |

The two columns are orthogonal and each answers only its own question: the value column
says how fast, the provenance column says who decided and when in the classifier's
history. Neither is overloaded.

### Why this is safe for the existing 88,814 rows

**The migration default is not a compromise — it is the correct answer.** Every existing
row genuinely has not been classified, so `DEFAULT ''` states the truth about all 88,814
of them without a single row being written or reinterpreted. There is no
`UPDATE … SET … WHERE …` step in the migration at all, and therefore no possibility of
the requirement's failure mode: 64,951 beliefs cannot be silently reclassified as
"durable" on ship, because shipping writes nothing.

Contrast with the nullable-column option below, whose migration would have to *rewrite*
88,814 rows to reach the same starting state, and would still be wrong for the rows that
`recompute-half-life` had already touched.

### Why nothing changes on the day it ships

`half_life_classifier` is read by the backfill command and by reporting. It is read by
**no** scoring, ranking, admission, staleness or consolidation path. `internal/trust` is
not touched and does not need to be — its `<= 0 → default` contract is still exactly
right, because "classified durable" and "unknown" really do both mean "use the default"
*for scoring purposes*. The column separates them for the purposes where they differ:
knowing what work remains, and knowing what has been measured.

That property is deliberate and worth stating as a constraint on the implementation: a
change to the belief data model that alters recall behaviour on day one is a change that
cannot be rolled back by reverting a binary. This one can.

### Back-compat on read

- An old binary against a new store ignores an unknown column; the projections are
  explicit column lists on every backend, so nothing breaks.
- A new binary against an old store gets `''` for every row via the column-add default —
  which is, again, the truthful answer, not a degraded one.
- A row written by an old binary after the column exists gets `''`, and is therefore
  correctly reported as unclassified and picked up by the next pass. **The failure
  direction of every unknown case is "unclassified"**, never a false claim of coverage.

## Migration story, per backend

The column add itself is metadata-only on all four engines — nothing rewrites 88k rows.
Measured on the real 88,814-row brain:

```
sqlite> ALTER TABLE claims ADD COLUMN half_life_classifier TEXT NOT NULL DEFAULT '';
0.023 total     (23 ms; 88,814 rows now report half_life_classifier = '')
```

| backend | change | cost |
|---|---|---|
| **sqlite** | `CREATE TABLE` in `sql/sqlite/schema.sql` + `internal/store/sqlite/db.go`, and — critically — an entry in the `expectedColumns` additive-migration list (`db.go:~680`), exactly as `durability` has. `CREATE TABLE IF NOT EXISTS` does not add columns to an existing table, so omitting that entry makes every pre-existing brain fail its first read with `no such column`. | 23 ms, measured. `ADD COLUMN` with a constant default is a schema-header write, not a table rewrite. |
| **libsql** | **none.** `libsql.go` constructs the sqlite repositories directly and reuses the sqlite schema; the sqlite change covers it. Same as #334. | — |
| **postgres** | `ALTER TABLE claims ADD COLUMN IF NOT EXISTS half_life_classifier text NOT NULL DEFAULT '';` in `internal/store/postgres/schema.sql`, plus the `CREATE TABLE`. **No ADR-0007 RLS change:** the `scoped text[]` array (`schema.sql:464`) lists *tables*, and `claims` is already in it — the CLAUDE.md gotcha applies to new tables, not new columns on a scoped table. | Metadata-only. PG 11+ stores a non-volatile default in the catalog rather than rewriting the heap. |
| **mysql** | `ALTER TABLE claims ADD COLUMN IF NOT EXISTS half_life_classifier VARCHAR(64) NOT NULL DEFAULT '';`. The provider already handles the dialect split — vanilla MySQL rejects `IF NOT EXISTS` here as a syntax error, so `applySchema` retries without the clause and treats duplicate-column as success (`internal/store/mysql/mysql.go:230-271`). | `ADD COLUMN` at the end of the table is `ALGORITHM=INSTANT` on MySQL 8.0. Honest caveat: InnoDB permits a bounded number of instant-added columns before the next add forces a rebuild, so on a very old, much-altered table this could be a rewrite. |
| **memory** | a field on `storedClaim`, copied both ways in `storedClaimFromDomain` / back. | — |

The genuinely expensive part is not the schema — it is the **value** backfill, and that is
a separate, batched, resumable pass, discussed under Consequences.

## Alternatives considered and rejected

### 1. Make `half_life_days` nullable — `NULL` = unclassified, `0` = durable

The obvious answer, and it inverts the defect cleanly in the abstract. Rejected on three
independent grounds:

- **It requires the 88k-row rewrite this ADR avoids.** Existing rows are `NOT NULL
  DEFAULT 0`; dropping the constraint does not turn them into `NULL`. Reaching the correct
  starting state means `UPDATE claims SET half_life_days = NULL WHERE half_life_days = 0`
  across 88,814 rows — on Postgres a full-table `UPDATE` is 88k dead tuples and a vacuum,
  and it is exactly the "whole-store rewrite" shape CLAUDE.md records as having blown the
  governed-write budget.
- **It redefines a value that already exists in 88,814 rows and ~15 call sites.** `0`
  currently means "no opinion" everywhere: `trust.ScoreWithHalfLife`'s `hl <= 0` fallback
  (three functions), the ingest guard `if enriched[i].HalfLifeDays == 0`
  (`pipeline.go:232`), the backfill's `if c.HalfLifeDays != 0 { skip }`, and the
  `CASE WHEN excluded.half_life_days > 0` / `CASE WHEN ? > 0` conflict rules that #334
  added on all four backends specifically to stop a re-ingest destroying a human override.
  Making `0` mean "positively classified durable" makes every one of those guards subtly
  wrong on the same day, in a codebase where the reviewer's only signal is arithmetic.
- **Blast radius in Go.** `Belief.HalfLifeDays` is `float64`; nullability means `*float64`
  or a companion bool, rippling through `internal/query`, `internal/curiosity`,
  `floatback`, the health vitals, `govwrite`, the MCP/REST/gRPC projections and every
  backend's scan function — for a fact that is not about the number.

### 2. A sentinel inside `half_life_days` (`-1` = classified durable)

Zero schema change, which is its whole appeal. Rejected:

- **It does not actually work with the existing conflict rules.** Every backend's upsert
  is `half_life_days = CASE WHEN excluded.half_life_days > 0 THEN excluded ELSE stored
  END`. A stamped `-1` is not `> 0`, so an ingest that classified a claim as durable would
  *never persist the sentinel* — the stored `0` wins. Making it work means changing the
  `> 0` semantics on sqlite, postgres and mysql, i.e. rewriting the exact rule #334
  introduced to protect overrides. It is not the migration-free option it appears to be.
- **It is a number that is not a number.** `-1` happens to be inert inside
  `internal/trust` today (all three `<= 0` guards absorb it), which is the good case and
  is luck rather than design — nothing in the type or the field name stops a future
  consumer from dividing by it. The value is already emitted as a bare number by
  `mnemos verify` (`cmd/mnemos/verify.go:54`) and carried on `domain.Belief`, so every
  future projection inherits a field whose documented range no longer describes its
  contents. `half_life_days` is not currently in the REST/gRPC/MCP belief payloads, which
  limits today's damage — but "it has not been exposed yet" is not a property worth
  designing against.
- It is the same defect one level down: a categorical fact smuggled into a quantitative
  column.

### 3. Stamp the durable default explicitly (`half_life_days = 90` for classified-durable)

The strongest of the rejected options and the runner-up. It needs **no schema change at
all**, works on all four backends today, keeps `> 0` semantics intact (`90 > 0`, so the
conflict rules persist it correctly), makes `half_life_days > 0` mean "classified", and
makes the backfill's selection predicate `WHERE half_life_days = 0` correct and
self-terminating. Rejected because:

- **It freezes a policy constant into 64,951 rows.** `trust.FreshnessHalfLifeDays = 90` is
  a live tunable. Today the durable corpus tracks it automatically; after this option, the
  corpus is pinned at 90 and retuning the default silently applies only to unclassified
  claims — the reverse of what anyone changing that constant would intend, and invisible.
- **It is unrecoverable once applied.** "Classified durable" and "a human verified this
  and chose 90 days" become the same bytes, so the distinction #334 and #336 worked to
  protect is destroyed by the fix meant to complete them.
- It answers "was this classified?" but not "by which version?", so it solves the coverage
  question and leaves the reclassification question exactly where it is.

### 4. A key in `confidence_components` (the ADR 0013/0014/0016/0024 precedent)

Genuinely tempting: salience, inhibition and credit all live there specifically to avoid a
cross-backend migration, and ADR 0024 proposes retrieval strength there for the same
reason. Rejected on four counts:

- **The map cannot hold it.** `Belief.Validate` constrains every non-credit component to
  `[0,1]` (`internal/domain/types.go:886-900`). A classifier *version* is not a number in
  `[0,1]`; the best available encoding is a bare `1.0` flag, which loses versioning — the
  half of the problem that makes a classifier improvement targetable.
- **It is the wrong map.** The field documents itself as decomposing "the scalar
  Confidence into named contributors". Salience, inhibition and credit are all genuine
  weights that modulate ranking or trust. Classifier provenance modulates nothing; putting
  it there means it surfaces in confidence narration and audit output as if it were a
  contributor to belief.
- **It is silently lossy.** Every backend's upsert is a blind
  `confidence_components = excluded.confidence_components`, so any re-upsert carrying a map
  without the marker erases it. The failure direction is safe (a claim reverts to
  "unclassified"), but it means the coverage number would drift downward for reasons
  unrelated to classification — a metric that decays on write traffic is worse than no
  metric, because it looks like a measurement.
- The precedent it invokes was about **avoiding a migration that would be expensive**. The
  measurement above is that this migration is 23 ms and rewrites nothing, so the premise
  does not hold here.

### 5. A brain-level watermark (`classifier v1 applied through <timestamp>`)

One metadata row instead of 68,015 stamped ones — by far the cheapest thing that answers
the coverage question. Rejected: it is unverifiable and it lies after a partial run. A pass
killed by `MNEMOS_JOB_TIMEOUT` either leaves no watermark (losing all progress) or leaves
one asserting coverage it did not achieve. It also cannot represent per-row exceptions,
and `recompute-half-life` deliberately skips deprecated claims, so exceptions are the norm.
And claims are mutable: a claim created before the watermark can have its text rewritten by
a later upsert, making the watermark's implication false for that row with no way to
notice. Reading a row must be enough to know its state.

### 6. Do nothing — recompute the classifier at report time

`HalfLifeFor` is pure, deterministic, offline and fast: the census at the top of this ADR
ran over all 88,814 rows in seconds. So coverage could be *derived* on demand without
storing anything. Rejected because it answers a different question. It reports what the
*current* classifier would say, not what was *applied* — so it reports full coverage for a
brain the backfill has never touched (as it would for the production brain today, where
`half_life_days > 0` is 0 rows). It cannot find rows a superseded classifier version
looked at, cannot distinguish an applied verdict from a human override, and gives the
backfill no cursor. It is a useful diagnostic, not a representation.

## Consequences

### The good

- "Classified durable" and "never classified" become distinguishable, which is the whole
  requirement.
- `recompute-half-life`'s selection becomes `WHERE half_life_classifier <> <current
  version>` — which is simultaneously idempotent, **resumable** (progress is recorded per
  row, so a killed run resumes where it stopped rather than restarting), and correctly
  self-terminating. This closes the "no resume-from-cursor" limitation #336 records.
- A classifier improvement becomes targetable: bump `VolatilityClassifierVersion`, and the
  same predicate selects exactly the rows a previous version stamped.
- Coverage becomes reportable, in `knowledge_metrics` and as an ADR 0019 vital, with
  "unclassified" as an honest `unknown` rather than an implied zero.
- Zero behaviour change on ship; revertible by reverting a binary.

### The cost, stated plainly

**The backfill gets ~22× bigger.** #336 writes 3,063 rows (only the volatile verdicts).
Recording provenance means stamping every claim the classifier *looks at* — 68,015 live
rows on this brain. At #336's measured rate (~1m40s end-to-end for 88k reads and 3k
writes) a full provenance pass is plausibly tens of minutes.

This is a real cost and it must not be waved away, given that CLAUDE.md documents a
whole-store rewrite blowing the governed-write budget. Three things make it tolerable, and
they should be verified rather than assumed:

- The existing `--batch 500` already bounds each governed dispatch, so the *per-dispatch*
  cost does not grow with the brain; only the number of dispatches does (~136 here).
- The pass is now genuinely resumable, so a run that exceeds `MNEMOS_JOB_TIMEOUT` loses
  nothing and the next run is strictly smaller. That is the property the current design
  lacks, and it is what makes a long pass acceptable rather than dangerous.
- It is one-time per classifier version. Ingest stamps the tag on write for everything new
  (a field on a row already being written — no extra cost), so the pass only ever has to
  cover the back catalogue.

If measurement shows the pass is still too heavy, the fallback is to stamp lazily rather
than eagerly — but that is a decision to take on evidence, not in advance.

### Other costs

- One more column on the hottest table, in five projections and five scan functions.
- One more thing to remember when adding a backend (mitigated by the parity test below).
- A version string is a coordination point: forgetting to bump it after changing the
  lexicons leaves stale verdicts that look current. A test can pin the constant to a hash
  of the lexicons so a change forces the bump.

## Rollout

0. **This ADR.** Design only.
1. **Domain** — `Belief.HalfLifeClassifier`, `HalfLifeClassified()`,
   `extract.VolatilityClassifierVersion`. No storage yet; pure and independently testable.
2. **Schema and repositories, all four backends in one change.** sqlite (`schema.sql`,
   `db.go` `CREATE TABLE` **and** `expectedColumns`, `sql/sqlite/query/claims.sql` insert +
   conflict set, `make sqlc`), postgres (`schema.sql`, insert, conflict set,
   `claimColumnNames`, `scanClaimRow`), mysql (the same four), memory (`storedClaim`).
   **Shipping this for fewer than all four backends recreates #331 exactly** — a value
   computed, carried on the domain object and dropped at the store boundary for the
   backends nobody ran locally — so the four move together or not at all. Note for
   scheduling: `internal/store/postgres/claim_repository.go` and `internal/store/mysql/`
   are currently owned by other lanes; this step needs that ownership resolved first, and
   is the concrete reason this ADR ships as design rather than as code.
3. **Ingest stamps it.** `pipeline.PersistArtifacts` sets
   `HalfLifeClassifier = extract.VolatilityClassifierVersion` on the claims it classifies,
   next to the existing `HalfLifeDays` stamp. From here the corpus stops growing more
   unclassified rows.
4. **Backfill reads and writes it.** `recompute-half-life` selects on the version tag,
   stamps it on every claim it evaluates (including durable verdicts), and keeps its
   existing read-back verification. Measure the full-pass wall time on a copy of the real
   brain before recommending it be run.
5. **Report it.** Coverage in `knowledge_metrics` / `mnemos metrics`, and as an ADR 0019
   vital reporting `unknown` when nothing has been classified — never `0`.

## Guardrails

- **A round-trip test per backend**, asserting on what the *store* returns after
  `PersistArtifacts` — not on the object the pipeline built. This is the shape that would
  have caught #331 and did catch it in #334; it belongs in the same
  `internal/store/claim_half_life_parity_test.go` harness, run against postgres and mysql
  containers, not just sqlite and memory.
- **A test that a durable verdict is distinguishable from an unclassified row** — the
  single assertion this entire ADR exists to make possible: two claims both with
  `half_life_days == 0`, one classified, one not, must not compare equal.
- **A test that the migration reinterprets nothing:** open a fixture store written before
  the column, assert every row reads back `HalfLifeClassified() == false` and every
  `half_life_days` is byte-identical. The requirement is that 64,951 beliefs are not
  silently reclassified as durable on ship; that should be an assertion, not a claim in a
  PR description.
- **A test that scoring is untouched:** trust, staleness and ranking outputs identical
  before and after, since the column is not read by any of them.
- **Mutation-check the backfill's selection**, as #336 did: dropping the version predicate
  must fail a named test.
