package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mnemos "go.klarlabs.de/mnemos"
	"go.klarlabs.de/mnemos/internal/store"
)

// waitFor polls until cond holds, failing the test if it never does. Used
// instead of a fixed sleep so the timing tests assert an observable outcome
// rather than a guessed duration.
func waitFor(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

// testCycle builds a cycle with test-scale timings and a counting pass.
func testCycle(interval time.Duration, pass func(context.Context)) *sleepCycle {
	return &sleepCycle{
		interval:     interval,
		startupDelay: time.Millisecond,
		pass:         pass,
		logger:       nil,
	}
}

// A hosted brain must sleep soon after boot AND keep sleeping on the interval.
// The startup pass is the load-bearing half: a server restarted daily never
// reaches its first 20-hour tick, so an interval-only cycle would consolidate
// exactly never — the defect this fixes, reintroduced by its own schedule.
func TestSleepCycle_RunsShortlyAfterStartupThenOnInterval(t *testing.T) {
	var passes atomic.Int64
	c := testCycle(5*time.Millisecond, func(context.Context) { passes.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	c.start(ctx)

	waitFor(t, "the startup pass", func() bool { return passes.Load() >= 1 })
	waitFor(t, "at least two further ticks", func() bool { return passes.Load() >= 3 })

	cancel()
	// After cancellation the cycle must stop: sample, wait out several
	// intervals, and confirm nothing more ran.
	waitFor(t, "the in-flight pass to finish", func() bool { return !c.running.Load() })
	settled := passes.Load()
	time.Sleep(50 * time.Millisecond)
	if got := passes.Load(); got != settled {
		t.Fatalf("cycle kept running after shutdown: %d passes, want %d", got, settled)
	}
}

// A pass that outlives its interval must not stack up a second copy against the
// same store. The tick is skipped, and the skip is counted so it can be logged.
func TestSleepCycle_SkipsWhenAPassIsStillRunning(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Int64
	c := testCycle(time.Millisecond, func(context.Context) {
		started.Add(1)
		<-release
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.start(ctx)

	waitFor(t, "the first pass to start", func() bool { return started.Load() == 1 })
	waitFor(t, "a skipped tick", func() bool { return c.skipped.Load() > 0 })

	if got := started.Load(); got != 1 {
		t.Fatalf("%d passes ran concurrently; consolidation must never overlap", got)
	}
	close(release)
}

// A consolidation failure must never take the server down, in either of the two
// ways it can fail: an error return, or a panic — an unrecovered panic in ANY
// goroutine kills the whole process, listener included.
func TestSleepCycle_SurvivesAFailingPass(t *testing.T) {
	var passes atomic.Int64
	c := testCycle(time.Millisecond, func(context.Context) {
		if passes.Add(1) == 1 {
			panic("consolidation exploded")
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.start(ctx)

	// Reaching a third pass proves the panic neither killed the process nor
	// wedged the running-guard (a guard left latched would stop the cycle dead).
	waitFor(t, "the cycle to keep running after a panicking pass", func() bool {
		return passes.Load() >= 3
	})
}

// 0 disables the cycle outright — the escape hatch for an operator who runs
// consolidation from their own scheduler, or for extra serve replicas that must
// not all consolidate the same store.
func TestSleepCycle_DisabledWhenIntervalZero(t *testing.T) {
	var passes atomic.Int64
	c := testCycle(0, func(context.Context) { passes.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.start(ctx)

	time.Sleep(20 * time.Millisecond)
	if passes.Load() != 0 {
		t.Fatalf("interval 0 must disable the cycle, got %d passes", passes.Load())
	}
}

func TestConsolidateInterval(t *testing.T) {
	t.Setenv("MNEMOS_CONSOLIDATE_INTERVAL", "")
	if got := consolidateInterval(); got != sleepInterval {
		t.Errorf("default = %v, want the local brain's %v", got, sleepInterval)
	}
	t.Setenv("MNEMOS_CONSOLIDATE_INTERVAL", "45m")
	if got := consolidateInterval(); got != 45*time.Minute {
		t.Errorf("45m = %v", got)
	}
	t.Setenv("MNEMOS_CONSOLIDATE_INTERVAL", "0")
	if got := consolidateInterval(); got != 0 {
		t.Errorf("0 (disabled) = %v, want 0", got)
	}
	t.Setenv("MNEMOS_CONSOLIDATE_INTERVAL", "nonsense")
	if got := consolidateInterval(); got != sleepInterval {
		t.Errorf("garbage must fall back to the default, got %v", got)
	}
}

// The flag wins over the environment, and "not given" (negative) falls through
// to it — so `--consolidate-interval 0` really disables the cycle instead of
// being mistaken for an absent flag.
func TestConsolidateEveryFor_FlagBeatsEnv(t *testing.T) {
	t.Setenv("MNEMOS_CONSOLIDATE_INTERVAL", "6h")

	if got := consolidateEveryFor(-1); got != 6*time.Hour {
		t.Errorf("no flag = %v, want the env's 6h", got)
	}
	if got := consolidateEveryFor(90 * time.Minute); got != 90*time.Minute {
		t.Errorf("flag = %v, want 90m — the flag must win", got)
	}
	if got := consolidateEveryFor(0); got != 0 {
		t.Errorf("explicit 0 = %v, want 0 (disabled), not the env value", got)
	}
}

func TestParseConsolidateIntervalValue(t *testing.T) {
	if d, err := parseConsolidateIntervalValue("20h"); err != nil || d != 20*time.Hour {
		t.Errorf("20h = %v, %v", d, err)
	}
	if d, err := parseConsolidateIntervalValue("0"); err != nil || d != 0 {
		t.Errorf("0 must be accepted as 'disabled': %v, %v", d, err)
	}
	for _, bad := range []string{"-5m", "soon", ""} {
		if _, err := parseConsolidateIntervalValue(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

// The hosted cycle must run the SAME consolidation as the local brain's sleep.
// Derived from sleepArgs() rather than restated, so an organ added to one list
// can never be missing from the other.
func TestSleepConsolidateOptions_MatchTheLocalSleepPass(t *testing.T) {
	opts, clearSessionNoise, err := sleepConsolidateOptions()
	if err != nil {
		t.Fatalf("sleepConsolidateOptions: %v", err)
	}

	stripped, _ := splitSessionNoiseFlag(sleepArgs()[1:])
	want, err := parseConsolidateOpts(stripped, Flags{})
	if err != nil {
		t.Fatalf("parse sleepArgs: %v", err)
	}
	if opts != want {
		t.Fatalf("hosted options diverge from the local sleep:\n got %+v\nwant %+v", opts, want)
	}
	if !clearSessionNoise {
		t.Error("the hosted pass must clear session noise too — it is the only organ that LOWERS dissonance")
	}
	if !opts.Synthesize || !opts.AssignCredit || !opts.Journal {
		t.Errorf("the learning/skill/journal organs must run on a hosted brain: %+v", opts)
	}
	if opts.DryRun {
		t.Error("a scheduled server pass that writes nothing consolidates nothing")
	}
	if opts.ForgetBelowTrust != 0 {
		t.Errorf("an unattended pass must not do trust-floor forgetting, got %v", opts.ForgetBelowTrust)
	}
}

// seedSleepStore writes n claims into the store at dsn and returns it closed.
func seedSleepStore(t *testing.T, dsn string, n int) {
	t.Helper()
	mem, err := mnemos.New(mnemos.WithStorage(dsn), mnemos.WithPassiveMode())
	if err != nil {
		t.Fatalf("open seed store %s: %v", dsn, err)
	}
	defer func() { _ = mem.Close() }()
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		id := "ev" + string(rune('a'+i))
		text := "the api runs in region " + string(rune('a'+i))
		if err := mem.RememberEvent(ctx, mnemos.Event{ID: id, At: now, Type: "observation", Content: text}); err != nil {
			t.Fatalf("RememberEvent: %v", err)
		}
		if _, err := mem.RememberClaim(ctx, mnemos.ClaimItem{Text: text, EventIDs: []string{id}, ValidFrom: now}); err != nil {
			t.Fatalf("RememberClaim: %v", err)
		}
	}
}

// The pass must reach a real store and do real work, not merely be scheduled.
func TestConsolidateStore_ConsolidatesTheTargetStore(t *testing.T) {
	t.Setenv("MNEMOS_LLM_PROVIDER", "")
	t.Setenv("MNEMOS_EMBED_PROVIDER", "")
	dsn := "sqlite://" + filepath.Join(t.TempDir(), "brain.db")
	seedSleepStore(t, dsn, 3)

	res, err := consolidateStore(context.Background(), sleepTarget{DSN: dsn}, nil)
	if err != nil {
		t.Fatalf("consolidateStore: %v", err)
	}
	// TrustRefreshed is the always-on organ's row count, so it is the honest
	// "did this pass reach the store" signal in passive mode (ClaimsScanned
	// counts only claims carrying an embedding, of which a passive store has
	// none).
	if res.TrustRefreshed != 3 {
		t.Errorf("TrustRefreshed = %d, want 3 — the pass did not reach the store", res.TrustRefreshed)
	}
}

// The session-noise step is wired for real — its own governed writer over the
// target DSN — and is a clean no-op on a brain with no live contradiction
// edges, which is where it exits before ever calling the model. This is the
// path that lowers dissonance on a hosted brain, so it must not be a dead
// branch that only the CLI reaches.
func TestConsolidateStore_RunsTheSessionNoiseStep(t *testing.T) {
	// Configured but never called: with no contradiction edges the pass returns
	// before it classifies anything.
	t.Setenv("MNEMOS_LLM_PROVIDER", "ollama")
	t.Setenv("MNEMOS_EMBED_PROVIDER", "")
	dsn := "sqlite://" + filepath.Join(t.TempDir(), "brain.db")
	seedSleepStore(t, dsn, 2)

	res, err := consolidateStore(context.Background(), sleepTarget{DSN: dsn}, nil)
	if err != nil {
		t.Fatalf("consolidateStore with an LLM configured: %v", err)
	}
	if res.TrustRefreshed != 2 {
		t.Errorf("TrustRefreshed = %d, want 2", res.TrustRefreshed)
	}
}

func TestSleepTargets_SingleTenantIsTheWholeBrain(t *testing.T) {
	dsn := "sqlite://" + filepath.Join(t.TempDir(), "brain.db")
	t.Setenv("MNEMOS_DB_URL", dsn)

	targets, err := sleepTargets(context.Background(), false)
	if err != nil {
		t.Fatalf("sleepTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].DSN != dsn || targets[0].Tenant != "" {
		t.Fatalf("single-tenant targets = %+v, want one un-scoped %q", targets, dsn)
	}
}

// Multi-tenant: every tenant partition sleeps, each under its own scope. The
// base DSN must NOT be a target — under namespace isolation it is the empty
// default partition, and under Postgres RLS an un-scoped connection would
// consolidate across tenants.
func TestSleepTargets_MultiTenantEnumeratesEachPartition(t *testing.T) {
	base := "sqlite://" + filepath.Join(t.TempDir(), "brain.db")
	t.Setenv("MNEMOS_DB_URL", base)
	for _, tenant := range []string{"alpha", "bravo"} {
		seedSleepStore(t, store.SetDSNParam(base, "namespace", store.TenantNamespace(tenant)), 1)
	}
	seedSleepStore(t, base, 1) // default partition

	targets, err := sleepTargets(context.Background(), true)
	if err != nil {
		t.Fatalf("sleepTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("want 2 tenant targets, got %d: %+v", len(targets), targets)
	}
	for _, tg := range targets {
		if tg.Tenant == "" {
			t.Errorf("target %+v carries no tenant label", tg)
		}
		if tg.DSN == base || !strings.Contains(tg.DSN, "namespace=") {
			t.Errorf("target DSN %q is not tenant-scoped", tg.DSN)
		}
	}
}

// A backend that cannot enumerate its tenants must be handled explicitly: no
// pass, and an error the operator can see. Silently consolidating the base DSN
// instead would be the cross-tenant write --require-tenant exists to prevent.
func TestSleepTargets_MultiTenantRefusesUnenumerableBackend(t *testing.T) {
	t.Setenv("MNEMOS_DB_URL", "memory://")

	targets, err := sleepTargets(context.Background(), true)
	if err == nil {
		t.Fatal("a backend that cannot enumerate tenants must fail loudly, not silently")
	}
	if len(targets) != 0 {
		t.Fatalf("no target may be consolidated when enumeration fails, got %+v", targets)
	}
}

// End-to-end multi-tenant: one pass consolidates every tenant, and each result
// covers only its own partition's beliefs.
func TestRunSleepPass_ConsolidatesEachTenantSeparately(t *testing.T) {
	t.Setenv("MNEMOS_LLM_PROVIDER", "")
	t.Setenv("MNEMOS_EMBED_PROVIDER", "")
	base := "sqlite://" + filepath.Join(t.TempDir(), "brain.db")
	t.Setenv("MNEMOS_DB_URL", base)

	counts := map[string]int{"alpha": 3, "bravo": 1}
	for tenant, n := range counts {
		seedSleepStore(t, store.SetDSNParam(base, "namespace", store.TenantNamespace(tenant)), n)
	}

	results := runSleepPass(context.Background(), true, nil)
	if len(results) != 2 {
		t.Fatalf("want a result per tenant, got %d: %+v", len(results), results)
	}
	rescored := map[int]bool{}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("tenant %q failed: %v", r.Tenant, r.Err)
		}
		rescored[r.Result.TrustRefreshed] = true
	}
	if !rescored[3] || !rescored[1] {
		t.Fatalf("each tenant must be consolidated under its own scope; rescored counts %v, want {3, 1}", rescored)
	}
}

// One broken tenant must not stop the others from sleeping.
func TestRunSleepPass_OneBadTenantDoesNotStopTheRest(t *testing.T) {
	t.Setenv("MNEMOS_LLM_PROVIDER", "")
	t.Setenv("MNEMOS_EMBED_PROVIDER", "")
	base := "sqlite://" + filepath.Join(t.TempDir(), "brain.db")
	t.Setenv("MNEMOS_DB_URL", base)
	seedSleepStore(t, store.SetDSNParam(base, "namespace", store.TenantNamespace("alpha")), 2)

	// A target whose DSN cannot be opened at all, ahead of a healthy one.
	results := consolidateTargets(context.Background(), []sleepTarget{
		{Tenant: "broken", DSN: "nosuchscheme://nowhere"},
		{Tenant: "alpha", DSN: store.SetDSNParam(base, "namespace", store.TenantNamespace("alpha"))},
	}, nil)

	if len(results) != 2 {
		t.Fatalf("every target must be attempted, got %d results", len(results))
	}
	if results[0].Err == nil {
		t.Error("the broken target should have reported an error")
	}
	if results[1].Err != nil {
		t.Errorf("the healthy tenant must still sleep: %v", results[1].Err)
	}
	if results[1].Result.TrustRefreshed != 2 {
		t.Errorf("healthy tenant rescored %d beliefs, want 2", results[1].Result.TrustRefreshed)
	}
}
