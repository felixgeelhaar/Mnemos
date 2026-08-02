package brainbench

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.klarlabs.de/mnemos"
)

// Activity is what the process REPORTED doing — the ConsolidateResult counters.
//
// It is kept rigorously separate from the metric comparison, because the two
// answer different questions and are routinely confused. Activity answers "did
// the process run?". The comparison answers "did anything get better?". A pass
// that decays 900 association edges has high activity and may have zero effect;
// that combination is a specific, named verdict here rather than something a
// reader has to notice.
type Activity struct {
	ClaimsScanned        int     `json:"claims_scanned"`
	DuplicateGroups      int     `json:"duplicate_groups"`
	Merged               int     `json:"merged"`
	TrustRefreshed       int     `json:"trust_refreshed"`
	Forgotten            int     `json:"forgotten"`
	Refuted              int     `json:"refuted"`
	Validated            int     `json:"validated"`
	Credited             int     `json:"credited"`
	SalienceTagged       int     `json:"salience_tagged"`
	PlaybooksReinforced  int     `json:"playbooks_reinforced"`
	Replayed             int     `json:"replayed"`
	ReplayProtected      int     `json:"replay_protected"`
	PlasticityGain       float64 `json:"plasticity_gain"`
	AssociationsDecayed  int     `json:"associations_decayed"`
	InhibitionDecayed    int     `json:"inhibition_decayed"`
	LessonsSynthesized   int     `json:"lessons_synthesized"`
	PlaybooksSynthesized int     `json:"playbooks_synthesized"`

	// Total sums every mutation counter. TrustRefreshed and ClaimsScanned are
	// EXCLUDED: trust is recomputed on every pass regardless of whether
	// anything changed, and scanning is reading. Counting them would make
	// every pass look active and destroy the inert/active distinction.
	Total int `json:"total_mutations"`
}

func activityFrom(r mnemos.ConsolidateResult) Activity {
	a := Activity{
		ClaimsScanned:        r.ClaimsScanned,
		DuplicateGroups:      r.DuplicateGroups,
		Merged:               r.Merged,
		TrustRefreshed:       r.TrustRefreshed,
		Forgotten:            r.Forgotten,
		Refuted:              r.Refuted,
		Validated:            r.Validated,
		Credited:             r.Credited,
		SalienceTagged:       r.SalienceTagged,
		PlaybooksReinforced:  r.PlaybooksReinforced,
		Replayed:             r.Replayed,
		ReplayProtected:      r.ReplayProtected,
		PlasticityGain:       r.PlasticityGain,
		AssociationsDecayed:  r.AssociationsDecayed,
		InhibitionDecayed:    r.InhibitionDecayed,
		LessonsSynthesized:   r.LessonsSynthesized,
		PlaybooksSynthesized: r.PlaybooksSynthesized,
	}
	a.Total = a.Merged + a.Forgotten + a.Refuted + a.Validated + a.Credited +
		a.SalienceTagged + a.PlaybooksReinforced + a.Replayed +
		a.AssociationsDecayed + a.InhibitionDecayed +
		a.LessonsSynthesized + a.PlaybooksSynthesized
	return a
}

// Validity records whether this run's numbers can carry any weight at all.
//
// Deterministic is measured, not assumed: a third untreated copy of the same
// seed is measured independently and must reproduce the control's numbers
// exactly. When it does not, some measured quantity varies for reasons other
// than the treatment, so no delta in this run is attributable and the verdict
// says so instead of reporting a number.
type Validity struct {
	Deterministic bool `json:"deterministic"`
	// NondeterministicMetrics names the metrics that differed between two
	// identically-untreated arms — the run's actual noise floor.
	NondeterministicMetrics []string `json:"nondeterministic_metrics,omitempty"`
	ProbeCount              int      `json:"probe_count"`
	// MinResolvableRateDelta is 1/probe_count: the smallest change any
	// probe-based rate can express. A delta at this value is one probe
	// flipping.
	MinResolvableRateDelta float64 `json:"min_resolvable_rate_delta"`
}

// RunVerdict is the single-sentence outcome for a scenario.
type RunVerdict string

const (
	// VerdictInert — the process reported no mutations at all. It did not run
	// in any meaningful sense on this brain.
	VerdictInert RunVerdict = "inert-process-did-nothing"
	// VerdictNoEffect — the process mutated the brain but moved no scored
	// metric. Activity without demonstrated benefit. This is the verdict the
	// harness exists to be able to return.
	VerdictNoEffect RunVerdict = "ran-no-measurable-effect"
	// VerdictImproved — scored metrics moved only in the improving direction.
	VerdictImproved RunVerdict = "improved"
	// VerdictRegressed — scored metrics moved only in the degrading direction.
	VerdictRegressed RunVerdict = "regressed"
	// VerdictMixed — some scored metrics improved and others degraded.
	VerdictMixed RunVerdict = "mixed"
	// VerdictInvalid — measurement was not deterministic, so no attribution is
	// possible regardless of what the deltas say.
	VerdictInvalid RunVerdict = "invalid-nondeterministic-measurement"
)

// ScenarioResult is one scenario's full experimental record.
type ScenarioResult struct {
	ID          string   `json:"id"`
	Description string   `json:"description,omitempty"`
	Process     Process  `json:"process"`
	Stages      []string `json:"stages_under_test"`

	Seed        SeedStats    `json:"seed"`
	Activity    Activity     `json:"activity"`
	Validity    Validity     `json:"validity"`
	Comparisons []Comparison `json:"comparisons"`
	Summary     Summary      `json:"summary"`
	Verdict     RunVerdict   `json:"verdict"`
	DurationMS  int64        `json:"duration_ms"`
}

// Run executes one scenario's paired experiment under workDir.
//
// The arms are file copies of a single pristine seed, so they are byte-identical
// before the treatment and cannot differ for any reason except it. Each arm is
// measured exactly once and then discarded, because measurement writes.
func Run(ctx context.Context, sc Scenario, workDir string) (ScenarioResult, error) {
	started := time.Now()
	res := ScenarioResult{
		ID:          sc.ID,
		Description: sc.Description,
		Process:     sc.Process,
		Stages:      sc.Process.Enabled(),
	}

	dir := filepath.Join(workDir, sc.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, fmt.Errorf("brainbench: create work dir: %w", err)
	}

	seedPath := filepath.Join(dir, "seed.db")
	seedStats, err := Seed(ctx, "sqlite://"+seedPath, sc)
	if err != nil {
		return res, err
	}
	res.Seed = seedStats

	// Warm up BEFORE the copies are taken, so every arm inherits identical
	// Hebbian strengths and inhibition weights. Warming each arm separately
	// would introduce a second uncontrolled difference between them and quietly
	// destroy the paired design.
	if err := warmup(ctx, "sqlite://"+seedPath, sc); err != nil {
		return res, err
	}

	controlPath := filepath.Join(dir, "control.db")
	treatmentPath := filepath.Join(dir, "treatment.db")
	noisePath := filepath.Join(dir, "noise.db")
	for _, dst := range []string{controlPath, treatmentPath, noisePath} {
		if err := copyDB(seedPath, dst); err != nil {
			return res, err
		}
	}

	// CONTROL: untreated, measured once.
	control, err := Measure(ctx, "sqlite://"+controlPath, controlPath, sc)
	if err != nil {
		return res, fmt.Errorf("brainbench: measure control: %w", err)
	}

	// NOISE: a second untreated arm. Its only job is to establish whether two
	// identical inputs produce identical measurements. Without it, every delta
	// below the (unknown) noise floor would be reported as an effect.
	noise, err := Measure(ctx, "sqlite://"+noisePath, noisePath, sc)
	if err != nil {
		return res, fmt.Errorf("brainbench: measure noise arm: %w", err)
	}

	// TREATMENT: the process set, then measured once.
	activity, err := applyProcess(ctx, "sqlite://"+treatmentPath, sc.Process)
	if err != nil {
		return res, err
	}
	res.Activity = activity
	treatment, err := Measure(ctx, "sqlite://"+treatmentPath, treatmentPath, sc)
	if err != nil {
		return res, fmt.Errorf("brainbench: measure treatment: %w", err)
	}

	res.Validity = Validity{
		Deterministic:           true,
		ProbeCount:              len(sc.Probes),
		MinResolvableRateDelta:  1 / float64(max(len(sc.Probes), 1)),
		NondeterministicMetrics: driftingMetrics(control, noise),
	}
	res.Validity.Deterministic = len(res.Validity.NondeterministicMetrics) == 0

	res.Comparisons = CompareSets(control, treatment)
	res.Summary = Summarize(res.Comparisons)
	res.Verdict = verdictFor(res)
	res.DurationMS = time.Since(started).Milliseconds()
	return res, nil
}

// warmup runs the scenario's warmup queries against the seed so retrieval-created
// state (Hebbian association strength, competitive inhibition) exists for the
// decay stages to act on.
//
// AgentConsumer is set because inhibition only fires when a consumer adjudicates
// contradictions — without it the inhibition weights the DecayInhibition stage
// is supposed to relax are never written in the first place.
func warmup(ctx context.Context, dsn string, sc Scenario) error {
	if len(sc.Warmup) == 0 {
		return nil
	}
	mem, err := mnemos.New(
		mnemos.WithStorage(dsn),
		mnemos.WithPassiveMode(),
		mnemos.WithActor(SeedActor),
	)
	if err != nil {
		return fmt.Errorf("brainbench: open warmup handle: %w", err)
	}
	defer func() { _ = mem.Close() }()

	for _, q := range sc.Warmup {
		if _, err := mem.Recall(ctx, mnemos.Query{Text: q, Limit: ProbeDefaultLimit, AgentConsumer: true}); err != nil {
			return fmt.Errorf("brainbench: warmup query %q: %w", q, err)
		}
	}
	return nil
}

// applyProcess runs the process set on the treatment arm through the same
// public facade `mnemos consolidate` calls, so the harness tests the shipped
// code path and not a copy of it.
func applyProcess(ctx context.Context, dsn string, p Process) (Activity, error) {
	mem, err := mnemos.New(
		mnemos.WithStorage(dsn),
		mnemos.WithPassiveMode(),
		mnemos.WithActor(SeedActor),
	)
	if err != nil {
		return Activity{}, fmt.Errorf("brainbench: open treatment handle: %w", err)
	}
	defer func() { _ = mem.Close() }()

	out, err := mem.Consolidate(ctx, mnemos.ConsolidateOptions{
		DedupeThreshold:    p.DedupeThreshold,
		ForgetBelowTrust:   p.ForgetBelowTrust,
		ForgetRefuted:      p.ForgetRefuted,
		Synthesize:         p.Synthesize,
		ReinforceValidated: p.ReinforceValidated,
		ReinforcePlaybooks: p.ReinforcePlaybooks,
		AssignCredit:       p.AssignCredit,
		AssignSalience:     p.AssignSalience,
		ReplayTopK:         p.ReplayTopK,
		Plastic:            p.Plastic,
		DecayAssociations:  p.DecayAssociations,
		DecayInhibition:    p.DecayInhibition,
	})
	if err != nil {
		return Activity{}, fmt.Errorf("brainbench: consolidate: %w", err)
	}
	return activityFrom(out), nil
}

// driftingMetrics names every metric that differs between two arms that
// received identical treatment (i.e. none). Any name here is measurement noise,
// and any delta of that size elsewhere in the run is uninterpretable.
func driftingMetrics(a, b MetricSet) []string {
	var out []string
	for _, am := range a.Metrics {
		bm, ok := b.Get(am.Name)
		if !ok {
			out = append(out, am.Name)
			continue
		}
		if am.Known != bm.Known {
			out = append(out, am.Name)
			continue
		}
		if am.Known && am.Value != bm.Value {
			out = append(out, am.Name)
		}
	}
	sort.Strings(out)
	return out
}

// verdictFor reduces a scenario to one honest sentence.
//
// The ordering is deliberate. Validity is checked FIRST: a non-deterministic
// run cannot support any claim, however good its deltas look. Inertness is
// checked before effect, so "the process did nothing" is never dressed up as
// "the process was neutral".
func verdictFor(r ScenarioResult) RunVerdict {
	if !r.Validity.Deterministic {
		return VerdictInvalid
	}
	if r.Activity.Total == 0 {
		return VerdictInert
	}
	switch {
	case r.Summary.Better > 0 && r.Summary.Worse > 0:
		return VerdictMixed
	case r.Summary.Better > 0:
		return VerdictImproved
	case r.Summary.Worse > 0:
		return VerdictRegressed
	default:
		return VerdictNoEffect
	}
}

// copyDB copies a SQLite database and its sidecars.
//
// The -wal file is copied too: it holds committed pages that have not been
// checkpointed into the main file yet, so copying only the .db can silently
// drop the tail of the seed and give the arms different content — which would
// invalidate the entire paired design without any visible error.
func copyDB(src, dst string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := copyFileIfExists(src+suffix, dst+suffix); err != nil {
			return fmt.Errorf("brainbench: copy %s: %w", src+suffix, err)
		}
	}
	if _, err := os.Stat(dst); err != nil {
		return fmt.Errorf("brainbench: copy %s -> %s produced no database: %w", src, dst, err)
	}
	return nil
}

func copyFileIfExists(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // harness-controlled temp paths
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst) //nolint:gosec // harness-controlled temp paths
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
