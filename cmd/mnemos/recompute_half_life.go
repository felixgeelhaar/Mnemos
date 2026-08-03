package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/extract"
	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/workflow"
)

// `mnemos recompute-half-life [--dry-run] [--batch N]` backfills the per-claim
// freshness half-life on rows that never got one.
//
// The ingest pipeline has always classified claim volatility and stamped
// HalfLifeDays, but until #334 no backend's claim INSERT listed the column, so
// the value was computed, carried on the domain object and dropped at the store
// boundary. Every row in existence kept its DEFAULT 0 — 88,498 of 88,498 on a
// real brain — and therefore decays at the 90-day durable default
// (trust.FreshnessHalfLifeDays) instead of its own. #334 fixed the write path;
// it could not fix rows already written.
//
// This is the half-life analogue of `recompute-contested`: re-derive a stored
// derived field against today's rule and write back what it produces. It calls
// extract.HalfLifeFor — the same pure, deterministic function ingest calls, no
// LLM and no network — so the backfill can never disagree with what ingest would
// have done, and it is repeatable: running it twice is a no-op.
//
// # IT ONLY EVER FILLS A ZERO
//
// A non-zero half_life_days is either a real classification or a per-claim
// override set through MarkVerified, and clobbering an override is exactly the
// regression #334's `CASE WHEN excluded.half_life_days > 0` conflict rule was
// written to prevent. This pass matches that semantic in the selection, not just
// in the SQL: a row with any non-zero value is never a candidate, whatever the
// classifier says about its text.
//
// The classifier only ever SHORTENS a half-life, and only on a confident signal,
// because mis-classifying a durable claim as volatile decays real knowledge out
// of recall invisibly while missing a volatile one merely leaves today's
// behaviour. The backfill inherits that asymmetry unchanged — a low match rate
// is the expected result here, not a bug.
func recomputeHalfLife(dryRun bool, batch int, f Flags) {
	err := runJob("recompute-half-life", map[string]string{
		"dry_run": fmt.Sprint(dryRun), "batch": fmt.Sprint(batch),
	}, f.Verbose, func(ctx context.Context, job *workflow.Job, w *govwrite.Writer) error {
		conn := w.Conn()
		actor, actorErr := resolveActor(ctx, conn.Users, f.Actor)
		if actorErr != nil {
			return actorErr
		}
		if err := job.SetStatus("loading", ""); err != nil {
			return err
		}

		claims, err := conn.Claims.ListAll(ctx)
		if err != nil {
			return NewSystemError(err, "load claims")
		}

		plan := planHalfLifeBackfill(claims)
		printHalfLifePlan(plan)

		if dryRun {
			fmt.Println("\n(dry run — nothing written; re-run without --dry-run to apply)")
			return nil
		}
		if len(plan.Changes) == 0 {
			fmt.Println("\nnothing to backfill.")
			return nil
		}

		if err := job.SetStatus("saving", ""); err != nil {
			return err
		}
		res, err := applyHalfLifeBackfill(ctx, w, plan.Changes, batch, actor)
		if err != nil {
			return NewSystemError(err, "backfill half_life_days")
		}
		printHalfLifeResult(plan, res)
		return nil
	})
	if err != nil {
		exitWithMnemosError(f.Verbose, err)
	}
}

// defaultHalfLifeBatch caps how many claims go into one governed write.
//
// The governed writer budgets each dispatch (5 minutes by default) and the
// claim upsert issues a prior-status lookup plus an upsert per claim inside one
// transaction, so the cost of a single call grows with the batch. CLAUDE.md
// records the failure this avoids: a write whose cost grew with the size of the
// brain eventually blew that budget on an 11k-claim store. 500 keeps each
// dispatch small and bounded regardless of how large the brain is; the whole
// pass is then N/500 independent, individually-audited writes. Partial progress
// is safe because the pass is idempotent — a re-run picks up whatever a failed
// run did not reach.
const defaultHalfLifeBatch = 500

// halfLifePlan is what the pass would do, computed without touching the store so
// the selection is testable and reviewable on its own — and so a dry run reports
// exactly the set an apply would write.
type halfLifePlan struct {
	// Changes are claims with HalfLifeDays already set to the backfilled value,
	// ready to hand to the governed writer.
	Changes []domain.Claim
	// Live counts the claims eligible to be examined (see liveForHalfLife).
	Live int
	// AlreadySet counts live claims that already carry a non-zero half-life and
	// are therefore never touched.
	AlreadySet int
	// Unwritable counts live claims the classifier matched but whose stored row
	// fails domain validation, so writing them back would be rejected.
	Unwritable int
	// ByHalfLife is the distribution of values the pass would write, so the
	// result can be checked afterwards per value rather than as one total.
	ByHalfLife map[float64]int

	// Durable counts claims the classifier read and judged durable. Their
	// half_life_days stays 0 and nothing about scoring changes; what changes
	// is that the verdict becomes readable, so the pass can terminate and a
	// later classifier version can find them (ADR 0025).
	Durable int

	// AlreadyClassified counts claims carrying a verdict from THIS classifier
	// version. Before ADR 0025 these were indistinguishable from unclassified
	// and were redone on every run.
	AlreadyClassified int

	// Restamped counts claims an OLDER classifier version judged durable and
	// this one agrees with: the value is unchanged, only the stamp moves.
	Restamped int
}

// planHalfLifeBackfill selects the claims whose half-life the current classifier
// would set and that currently have none.
func planHalfLifeBackfill(claims []domain.Claim) halfLifePlan {
	plan := halfLifePlan{ByHalfLife: map[float64]int{}}
	for _, c := range claims {
		if !liveForHalfLife(c.Status) {
			continue
		}
		plan.Live++
		if c.HalfLifeDays != 0 {
			// A real classification or a MarkVerified override — either way, not
			// ours to overwrite. See the command doc.
			plan.AlreadySet++
			continue
		}
		// Already carries a verdict from THIS classifier version, and that
		// verdict was "durable" (the value is 0). Before ADR 0025 this state was
		// indistinguishable from "never classified", so every run redid all of
		// them and the pass could never report itself finished — on a real brain
		// that was 95.5% of the live set, every time, forever.
		//
		// A verdict from an OLDER version is deliberately NOT skipped: finding
		// those is the reason the version is stored rather than a bare flag.
		if c.HalfLifeClassifier == extract.VolatilityClassifierVersion {
			plan.AlreadyClassified++
			continue
		}
		hl := extract.HalfLifeFor(string(c.Type), c.Text)
		if hl == 0 && c.HalfLifeClassified() {
			// Reclassified by a newer version and still durable: the value does
			// not change, but the stamp must, or the row stays queued forever.
			c.HalfLifeClassifier = extract.VolatilityClassifierVersion
			if err := c.Validate(); err != nil {
				plan.Unwritable++
				continue
			}
			plan.Changes = append(plan.Changes, c)
			plan.Restamped++
			continue
		}
		if hl == 0 {
			// Classifier read it and declined to shorten. Record the VERDICT —
			// the value stays 0 and scoring is unchanged, but "judged durable"
			// stops looking like "never examined". This is the whole of ADR 0025.
			c.HalfLifeClassifier = extract.VolatilityClassifierVersion
			if err := c.Validate(); err != nil {
				plan.Unwritable++
				continue
			}
			plan.Changes = append(plan.Changes, c)
			plan.Durable++
			continue
		}
		// A malformed legacy row would fail Claim.Validate inside the store's
		// upsert transaction and take the whole batch down with it. Count it and
		// leave it out: one bad row must not abort an 88k-row pass.
		if err := c.Validate(); err != nil {
			plan.Unwritable++
			continue
		}
		c.HalfLifeDays = hl
		c.HalfLifeClassifier = extract.VolatilityClassifierVersion
		plan.Changes = append(plan.Changes, c)
		plan.ByHalfLife[hl]++
	}
	return plan
}

// liveForHalfLife reports whether a claim in this status is worth backfilling.
//
// Deprecated claims are excluded from recall, so their half-life is read by
// nothing; on a brain where retired rows are a large share of the table, writing
// them would be most of the cost of the pass for none of the benefit. Every other
// status — active, contested, resolved — still decays and still recalls. Nothing
// is lost permanently by the exclusion: the pass is deterministic and repeatable,
// so a claim later returned to active is picked up by a later run.
func liveForHalfLife(s domain.ClaimStatus) bool {
	return s != domain.ClaimStatusDeprecated
}

// halfLifeApplyResult reports what an apply actually did, read back from the
// store rather than counted from the writes issued.
type halfLifeApplyResult struct {
	// Written is how many claims were handed to the governed writer.
	Written int
	// Verified is how many of those read back from the store carrying the
	// intended value. A gap means a backend accepted the write and did not
	// persist the column — the exact class of defect this backfill exists to
	// repair (#331), so the pass checks its own work rather than trusting it.
	Verified int
	// Batches is how many governed writes the pass issued.
	Batches int
}

// applyHalfLifeBackfill writes the planned changes through the governed writer in
// bounded batches, verifying each batch by reading it back.
func applyHalfLifeBackfill(ctx context.Context, w *govwrite.Writer, changes []domain.Claim, batch int, actor string) (halfLifeApplyResult, error) {
	var res halfLifeApplyResult
	if batch <= 0 {
		batch = defaultHalfLifeBatch
	}
	for start := 0; start < len(changes); start += batch {
		end := min(start+batch, len(changes))
		chunk := changes[start:end]

		if _, err := w.Claims(ctx, chunk, govwrite.ClaimReason{
			// The reason names the tool so a later reader can tell an automated
			// backfill from a human edit in claim_status_history.
			Reason:    "Backfill per-claim freshness half-life from the volatility classifier (recompute-half-life)",
			ChangedBy: actor,
		}); err != nil {
			return res, fmt.Errorf("write batch %d (claims %d-%d): %w", res.Batches+1, start+1, end, err)
		}
		res.Written += len(chunk)
		res.Batches++

		verified, err := verifyHalfLifeBatch(ctx, w, chunk)
		if err != nil {
			return res, err
		}
		res.Verified += verified
	}
	return res, nil
}

// verifyHalfLifeBatch re-reads a written batch and counts the rows that came back
// with the value the pass intended.
func verifyHalfLifeBatch(ctx context.Context, w *govwrite.Writer, chunk []domain.Claim) (int, error) {
	want := make(map[string]float64, len(chunk))
	ids := make([]string, 0, len(chunk))
	for _, c := range chunk {
		want[c.ID] = c.HalfLifeDays
		ids = append(ids, c.ID)
	}
	stored, err := w.Conn().Claims.ListByIDs(ctx, ids)
	if err != nil {
		return 0, fmt.Errorf("verify batch: %w", err)
	}
	n := 0
	for _, c := range stored {
		if want[c.ID] == c.HalfLifeDays {
			n++
		}
	}
	return n, nil
}

func printHalfLifePlan(plan halfLifePlan) {
	fmt.Printf("live claims:     %d (everything but deprecated)\n", plan.Live)
	fmt.Printf("half-life set:   %d (%.1f%%) — left untouched\n", plan.AlreadySet, pct(plan.AlreadySet, plan.Live))
	// Coverage is the number ADR 0025 exists to make answerable. Before the
	// classifier column, "classified durable" and "never classified" were the
	// same bytes, so this line could not be written at all — and the pass could
	// never report itself finished.
	classified := plan.AlreadySet + plan.AlreadyClassified
	fmt.Printf("classified:      %d (%.1f%%) — a verdict has been recorded\n", classified, pct(classified, plan.Live))
	fmt.Printf("would backfill:  %d (%.1f%%)\n", len(plan.Changes), pct(len(plan.Changes), plan.Live))
	if plan.Durable > 0 {
		fmt.Printf("  of which %d judged DURABLE — half-life unchanged at the store default,\n"+
			"  recording only that the classifier looked. Scoring does not change.\n", plan.Durable)
	}
	if plan.Restamped > 0 {
		fmt.Printf("  of which %d re-stamped from an older classifier version (value unchanged)\n", plan.Restamped)
	}
	printHalfLifeDistribution(plan.ByHalfLife, "  ")
	if plan.Unwritable > 0 {
		fmt.Printf("skipped:         %d claim(s) the classifier matched but that fail validation (unwritable)\n", plan.Unwritable)
	}
	for i, c := range plan.Changes {
		if i >= 6 {
			fmt.Printf("  … and %d more\n", len(plan.Changes)-6)
			break
		}
		fmt.Printf("  - %.0fd  %s\n", c.HalfLifeDays, truncateClaim(c.Text))
	}
}

func printHalfLifeResult(plan halfLifePlan, res halfLifeApplyResult) {
	fmt.Printf("\nrecorded a classifier verdict on %d claim(s) in %d batch(es):\n", res.Written, res.Batches)
	printHalfLifeDistribution(plan.ByHalfLife, "  ")
	if res.Verified != res.Written {
		// Silence here would reproduce the original defect: a write path that
		// reports success while the column never lands.
		fmt.Printf("  WARNING: %d of %d claim(s) did not read back with the intended value — this backend may not persist half_life_days.\n",
			res.Written-res.Verified, res.Written)
		return
	}
	fmt.Printf("  verified: all %d read back with the intended value.\n", res.Verified)
}

// printHalfLifeDistribution prints counts per resulting half-life, sorted so two
// runs are diffable.
func printHalfLifeDistribution(byHalfLife map[float64]int, indent string) {
	values := make([]float64, 0, len(byHalfLife))
	for v := range byHalfLife {
		values = append(values, v)
	}
	sort.Float64s(values)
	for _, v := range values {
		fmt.Printf("%s%.0f-day half-life: %d\n", indent, v, byHalfLife[v])
	}
}

// handleRecomputeHalfLife routes `mnemos recompute-half-life [--dry-run] [--batch N]`.
func handleRecomputeHalfLife(args []string, f Flags) {
	batch, err := parseRecomputeHalfLifeArgs(args)
	if err != nil {
		exitWithMnemosError(f.Verbose, err)
		return
	}
	recomputeHalfLife(f.DryRun, batch, f)
}

// parseRecomputeHalfLifeArgs resolves the batch size, separated from execution so
// the flag contract is testable without the process-exiting error path.
func parseRecomputeHalfLifeArgs(args []string) (int, error) {
	batch := defaultHalfLifeBatch
	usage := "\n  mnemos recompute-half-life [--dry-run] [--batch N]"
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value, hasValue := strings.CutPrefix(arg, "--batch=")
		if !hasValue {
			if arg != "--batch" {
				return 0, NewUserError("unknown argument %q for recompute-half-life%s", arg, usage)
			}
			if i+1 >= len(args) {
				return 0, NewUserError("--batch needs a size%s", usage)
			}
			value = args[i+1]
			i++
		}
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return 0, NewUserError("--batch wants a positive integer, got %q%s", value, usage)
		}
		batch = n
	}
	return batch, nil
}
