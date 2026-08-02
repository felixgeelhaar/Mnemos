package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"go.klarlabs.de/bolt"
	mnemos "go.klarlabs.de/mnemos"
	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/llm"
	"go.klarlabs.de/mnemos/internal/store"
)

// The hosted brain's sleep cycle.
//
// sleep_schedule.go gave the LOCAL brain a circadian rhythm: a session start
// after a long enough gap spawns a full consolidation. `serve` had no
// equivalent — it ran indefinitely and consolidated never. A hosted brain
// therefore only ever accumulated: narration piled up, dissonance ratcheted one
// way (the deterministic organs do not touch contradiction edges; only the LLM
// session-noise clearing lowers them), and the ADR-0019 vitals collected no
// time series because the health snapshot rides that same session hook. The
// local sleep even documents the gap in its own comment — "hosted brains are
// skipped and consolidate on their own server-side cron" — describing a cron
// that did not exist.
//
// A server is long-lived, so this is a real ticker rather than the stamp-file
// trick the one-shot local path needs: there is no process boundary to carry
// "when did we last sleep?" across, and an mtime read per tick would be state
// we already hold in memory.
//
// Four properties the cycle must hold, in order of importance:
//
//  1. A consolidation failure never takes the server down. Every pass runs
//     behind a recover() and per-target error isolation; failures are logged
//     and the cycle continues. The brain's housekeeping is not worth an outage.
//  2. Passes never overlap. A pass that outlives its interval would otherwise
//     stack up copies of a full-scan write pass against one store. A tick that
//     arrives while a pass is running is skipped, and says so.
//  3. It sleeps soon after boot, not only after a full interval. A server
//     restarted daily (rolling deploys, a crash loop) would otherwise never
//     reach its first 20-hour tick and consolidate exactly never — the defect
//     this fixes, reintroduced by the fix's own schedule.
//  4. Multi-tenant brains consolidate PER TENANT. See sleepTargets.
//
// Scope note for operators: the cycle is per process. N serve replicas against
// one store means N concurrent consolidations of the same data — correct but
// wasteful — so run it on one replica and set MNEMOS_CONSOLIDATE_INTERVAL=0 on
// the rest.

// serveSleepStartupDelay is how long after boot the first pass runs. Long
// enough that a rolling deploy's brief overlap of old and new processes has
// settled and the listener is warm; short enough that a server restarted daily
// still consolidates daily (property 3 above).
const serveSleepStartupDelay = 2 * time.Minute

// consolidateInterval reads the serve consolidation cadence:
// MNEMOS_CONSOLIDATE_INTERVAL (Go duration; 0 disables the cycle).
//
// It defaults to sleepInterval — the same gap the local brain sleeps on — so a
// hosted brain and a local one have the same rhythm rather than two numbers
// that drift apart. Like sleepInterval it is deliberately just under a day:
// what accrues is a working day's noise, not a minute's.
func consolidateInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("MNEMOS_CONSOLIDATE_INTERVAL"))
	if raw == "" {
		return sleepInterval
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		fmt.Fprintf(os.Stderr, "serve: invalid MNEMOS_CONSOLIDATE_INTERVAL=%q (want 20h, 30m, ...); using %s\n", raw, sleepInterval)
		return sleepInterval
	}
	return d // 0 → disabled
}

// parseConsolidateIntervalValue reads the `serve --consolidate-interval` value.
// 0 is legal and means "disable the cycle" (the escape hatch for extra replicas
// and for operators running consolidation from their own scheduler); a negative
// duration is not, because negative is the sentinel for "flag not given".
func parseConsolidateIntervalValue(raw string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d < 0 {
		return 0, NewUserError("--consolidate-interval must be a non-negative Go duration (e.g. 20h, 30m, 0)")
	}
	return d, nil
}

// consolidateEveryFor resolves the effective cadence. An explicit flag wins
// over MNEMOS_CONSOLIDATE_INTERVAL (and the config file behind it), which wins
// over the default — the same precedence every other serve knob follows. A
// negative flag value means it was not given.
func consolidateEveryFor(flag time.Duration) time.Duration {
	if flag >= 0 {
		return flag
	}
	return consolidateInterval()
}

// sleepConsolidateOptions derives the hosted pass from sleepArgs() — the LOCAL
// brain's sleep — instead of restating the flag list.
//
// Restating it would guarantee divergence: the moment an organ is added to one
// list, the two brains consolidate differently, and ADR 0011's whole premise is
// that a tenant brain and the global brain run the same machinery. It parses
// through the same parseConsolidateOpts the CLI uses, so `mnemos consolidate`
// and a hosted tick cannot disagree about what a flag means either.
//
// Returns the library options plus whether the command-level session-noise
// clearing was requested (it is not a ConsolidateOption — see
// splitSessionNoiseFlag).
func sleepConsolidateOptions() (mnemos.ConsolidateOptions, bool, error) {
	args, clearSessionNoise := splitSessionNoiseFlag(sleepArgs()[1:]) // [0] is the "consolidate" subcommand
	opts, err := parseConsolidateOpts(args, Flags{})
	return opts, clearSessionNoise, err
}

// sleepTarget is one store a pass consolidates: a tenant partition in
// multi-tenant mode, or the whole brain in single-tenant mode.
type sleepTarget struct {
	Tenant string // "" in single-tenant mode
	DSN    string // fully scoped — safe to open directly
}

// sleepPassResult is one target's outcome. Errors are carried, not returned, so
// one broken tenant cannot stop the others from sleeping.
type sleepPassResult struct {
	Tenant string
	Result mnemos.ConsolidateResult
	Err    error
}

// sleepTargets resolves which stores this pass must consolidate.
//
// Single-tenant: the process store, unchanged.
//
// Multi-tenant (ADR 0007): every tenant partition, each under its OWN scope,
// via store.EnumerateTenants — namespace-per-tenant for sqlite/mysql/local
// libSQL, a tenant-filtered read for Postgres RLS. Consolidating the base DSN
// instead would be worse than doing nothing: under namespace isolation it
// maintains the empty default partition while every real tenant keeps
// ratcheting, and under Postgres RLS an un-scoped (superuser) connection writes
// ACROSS tenants.
//
// A backend that cannot enumerate (memory://, remote libSQL, or a provider
// whose enumerator is not linked in) is handled explicitly rather than ignored:
// it gets no pass at all and returns an error for the caller to log. Falling
// back to the base DSN here is precisely the cross-tenant write --require-tenant
// exists to prevent.
func sleepTargets(ctx context.Context, requireTenant bool) ([]sleepTarget, error) {
	dsn := resolveDSN()
	if !requireTenant {
		return []sleepTarget{{DSN: dsn}}, nil
	}
	scopes, err := store.EnumerateTenants(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("enumerate tenants of %s: %w", store.RedactDSN(dsn), err)
	}
	targets := make([]sleepTarget, 0, len(scopes))
	for _, s := range scopes {
		targets = append(targets, sleepTarget{Tenant: s.Tenant, DSN: s.DSN})
	}
	return targets, nil
}

// runSleepPass consolidates every target once. It never fails as a whole: a
// resolution failure is logged and yields no results, and a per-target failure
// is carried in that target's result.
func runSleepPass(ctx context.Context, requireTenant bool, logger *bolt.Logger) []sleepPassResult {
	targets, err := sleepTargets(ctx, requireTenant)
	if err != nil {
		if logger != nil {
			logger.Error().Err(err).Msg("mnemos: consolidation skipped — cannot resolve which stores to sleep")
		}
		return nil
	}
	return consolidateTargets(ctx, targets, logger)
}

// consolidateTargets sleeps each target in turn, isolating failures. Sequential
// on purpose: consolidation is a full-scan write pass, and running every
// tenant's at once would turn a background chore into a load spike on the one
// database they share.
func consolidateTargets(ctx context.Context, targets []sleepTarget, logger *bolt.Logger) []sleepPassResult {
	out := make([]sleepPassResult, 0, len(targets))
	for _, target := range targets {
		if ctx.Err() != nil {
			// Shutdown (or the pass budget) landed mid-sweep: stop rather than
			// start a store's pass that cannot finish. The remaining tenants
			// sleep on the next cycle.
			break
		}
		start := time.Now()
		res, err := consolidateStore(ctx, target, logger)
		out = append(out, sleepPassResult{Tenant: target.Tenant, Result: res, Err: err})
		if logger == nil {
			continue
		}
		if err != nil {
			logger.Error().Err(err).Str("tenant", target.Tenant).Msg("mnemos: consolidation failed")
			continue
		}
		logger.Info().
			Str("tenant", target.Tenant).
			Int("claims_scanned", res.ClaimsScanned).
			Int("merged", res.Merged).
			Int("trust_refreshed", res.TrustRefreshed).
			Int("lessons_synthesized", res.LessonsSynthesized).
			Float64("seconds", time.Since(start).Seconds()).
			Msg("mnemos: consolidation complete")
	}
	return out
}

// consolidateStore runs ONE store's sleep: the deterministic organs through the
// library facade, then the LLM narration clearing.
func consolidateStore(ctx context.Context, target sleepTarget, logger *bolt.Logger) (mnemos.ConsolidateResult, error) {
	opts, clearSessionNoise, err := sleepConsolidateOptions()
	if err != nil {
		return mnemos.ConsolidateResult{}, err
	}
	mem, err := newLibraryMemoryForDSN(target.DSN, "")
	if err != nil {
		return mnemos.ConsolidateResult{}, fmt.Errorf("open store: %w", err)
	}
	res, err := mem.Consolidate(ctx, opts)
	// Release the facade before the session-noise pass opens its own governed
	// writer: SQLite is single-writer, so a still-open handle would deadlock the
	// second pass (the same reason handleConsolidate closes first).
	_ = mem.Close()
	if err != nil {
		return res, fmt.Errorf("consolidate: %w", err)
	}
	if clearSessionNoise {
		if err := clearSessionNoiseFor(ctx, target, logger); err != nil {
			return res, fmt.Errorf("clear session noise: %w", err)
		}
	}
	return res, nil
}

// clearSessionNoiseFor runs the narration-clearing half of the sleep against
// one target. It is the only organ that LOWERS dissonance, which is why an
// unattended pass runs it at all.
//
// Skipped cleanly when no LLM is configured, so a server without a provider
// still gets the deterministic sleep instead of a failed pass — the same
// degradation handleConsolidate does.
func clearSessionNoiseFor(ctx context.Context, target sleepTarget, logger *bolt.Logger) error {
	if _, err := llm.ConfigFromEnv(); err != nil {
		return nil
	}
	// Bounded like the CLI's, which gets its budget from runJob: this is an LLM
	// call per claim on a live contradiction edge, so its cost grows with the
	// brain and a hosted pass must not run until the next tick catches it.
	ctx, cancel := context.WithTimeout(ctx, jobTimeout())
	defer cancel()

	w, err := govwrite.New(ctx, target.DSN, nil)
	if err != nil {
		return fmt.Errorf("open governed writer: %w", err)
	}
	defer closeWriter(w)

	// Attributed to the system actor: a scheduled server pass is the system
	// acting, not any user — and MNEMOS_USER_ID on a server names whoever
	// happened to export it, which would be a false audit trail.
	stats, err := runSessionNoisePass(ctx, w, domain.SystemUser, false, io.Discard, nil)
	if err != nil {
		return err
	}
	if logger != nil {
		logger.Info().
			Str("tenant", target.Tenant).
			Int("live_edges", stats.LiveEdges).
			Int("classified", stats.Classified).
			Int("session_local", stats.SessionLocal).
			Int("pruned", stats.Pruned).
			Msg("mnemos: session-noise clearing complete")
	}
	return nil
}

// sleepCycle is the scheduler: one goroutine that fires passes, plus the
// running-guard that keeps them from overlapping. pass is a seam so the
// schedule can be tested without a store.
type sleepCycle struct {
	interval     time.Duration
	startupDelay time.Duration
	pass         func(context.Context)
	logger       *bolt.Logger

	running   atomic.Bool
	completed atomic.Int64
	skipped   atomic.Int64
}

// start launches the cycle until ctx is cancelled. interval <= 0 disables it.
func (c *sleepCycle) start(ctx context.Context) {
	if c.interval <= 0 || c.pass == nil {
		return
	}
	go func() {
		// One pass shortly after boot (property 3), then on the interval.
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.startupDelay):
		}
		c.tick(ctx)

		t := time.NewTicker(c.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.tick(ctx)
			}
		}
	}()
}

// tick starts a pass unless one is still running.
func (c *sleepCycle) tick(ctx context.Context) {
	if !c.running.CompareAndSwap(false, true) {
		c.skipped.Add(1)
		if c.logger != nil {
			c.logger.Warn().
				Int64("skipped_total", c.skipped.Load()).
				Msg("mnemos: consolidation still running; skipping this cycle")
		}
		return
	}
	// A pass must not outlive its own cycle: bounding it by the interval means a
	// wedged store costs at most one skipped cycle instead of silencing every
	// future one.
	passCtx, cancel := context.WithTimeout(ctx, c.interval)
	go func() {
		defer cancel()
		defer c.running.Store(false)
		defer func() {
			// An unrecovered panic in ANY goroutine kills the process, listener
			// included. A background chore must never be able to do that.
			if r := recover(); r != nil && c.logger != nil {
				c.logger.Error().Str("panic", fmt.Sprint(r)).Msg("mnemos: consolidation panicked; server continues")
			}
		}()
		c.pass(passCtx)
		c.completed.Add(1)
	}()
}

// startConsolidationCycle wires the hosted sleep into a running server.
func startConsolidationCycle(ctx context.Context, interval time.Duration, requireTenant bool, logger *bolt.Logger) *sleepCycle {
	c := &sleepCycle{
		interval:     interval,
		startupDelay: serveSleepStartupDelay,
		logger:       logger,
		pass: func(ctx context.Context) {
			runSleepPass(ctx, requireTenant, logger)
		},
	}
	c.start(ctx)
	return c
}
