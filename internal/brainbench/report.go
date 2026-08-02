package brainbench

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// ReportVersion is the schema version of the machine-readable output. Bump it
// on any breaking change so a consumer diffing two reports can tell whether the
// numbers or the format moved.
const ReportVersion = 1

// Report is the harness's complete machine-readable output.
type Report struct {
	Harness     string           `json:"harness"`
	Version     int              `json:"version"`
	GeneratedAt time.Time        `json:"generated_at"`
	Config      Config           `json:"config"`
	Limitations []string         `json:"limitations"`
	Scenarios   []ScenarioResult `json:"scenarios"`
	Summary     Summary          `json:"summary"`
	// Verdicts counts scenario verdicts by kind.
	Verdicts map[RunVerdict]int `json:"verdicts"`
}

// Config records the conditions the numbers were produced under. It travels
// inside the report because a metric without its conditions is not
// reproducible, and because these particular conditions (no LLM, a stub
// embedder) bound what the numbers can be used to argue.
type Config struct {
	Extraction string `json:"extraction"`
	Embedder   string `json:"embedder"`
	LLM        string `json:"llm"`
	Backend    string `json:"backend"`
	Design     string `json:"design"`
}

// DefaultConfig describes the conditions Run operates under.
func DefaultConfig() Config {
	return Config{
		Extraction: "rule-based (internal/extract), no LLM",
		Embedder:   StubEmbedderModel + " (deterministic hashed bag-of-words, lexical only)",
		LLM:        "none",
		Backend:    "sqlite",
		Design: "paired A/B on byte-identical file copies of one seed; " +
			"control untreated, treatment consolidated, third untreated arm measures the noise floor",
	}
}

// Limitations is what this harness does NOT measure.
//
// It ships inside every report rather than living only in a README, because a
// JSON file gets pasted into an issue, a slide, or a model prompt without its
// documentation, and an unqualified "consolidation improved MRR by 0.17" is a
// materially misleading claim. The caveats have to travel with the numbers.
func Limitations() []string {
	return []string{
		"LLM path untested: extraction is rule-based and no LLM is configured. " +
			"Nothing here says anything about LLM extraction, LLM causal relation detection, " +
			"grounded generation, or the narration-clearing stage of `consolidate --clear-session-noise`.",
		"Embeddings are a deterministic hashed bag-of-words stub, not a semantic model. " +
			"It scores exact and near-lexical restatements high and true paraphrases near zero, " +
			"so reported dedupe/merge behaviour is a LOWER BOUND on production and retrieval " +
			"quality is not comparable to a real embedding provider.",
		"Scale untested: scenarios hold tens of claims. Consolidation behaviour on a " +
			"10k-claim brain (clustering cost, merge cascades, forgetting mass) is not exercised, " +
			"and the incremental-trust-scoring failure mode documented in CLAUDE.md needs volume to appear.",
		"Single cycle only: one consolidation pass is measured. Repeated nightly passes may " +
			"compound, oscillate, or converge; that is a different experiment and this is not it.",
		"Cross-tenant promotion (ADR 0011 hippocampus->neocortex, `consolidate --promote`) is not " +
			"measured at all. This harness covers in-store maintenance only.",
		"Downstream utility is not measured. Metrics are properties of the store and of retrieval; " +
			"whether an agent using this brain made a better decision is unmeasured and would need " +
			"a task benchmark, not a store diff.",
		"Gold labels are author-written, not independently adjudicated, and there are few of them. " +
			"A scenario can be unintentionally written toward the behaviour it wants to show; " +
			"treat any single scenario as an anecdote and read the noise floor before the delta.",
		"Statistical significance is not computed and would be meaningless here: the run is " +
			"deterministic, so a delta is exact for this scenario and carries no confidence interval " +
			"across scenarios. The reported noise floor is the empirical substitute.",
		"Calibration, credit and salience stages need decisions, expectations and outcomes to act on. " +
			"A scenario whose corpus contains none leaves those stages inert, and the report will " +
			"show that as zero activity rather than as a neutral result.",
	}
}

// BuildReport assembles the full report from scenario results.
func BuildReport(results []ScenarioResult) Report {
	r := Report{
		Harness:     "brainbench",
		Version:     ReportVersion,
		GeneratedAt: time.Now().UTC(),
		Config:      DefaultConfig(),
		Limitations: Limitations(),
		Scenarios:   results,
		Verdicts:    map[RunVerdict]int{},
	}
	for _, s := range results {
		r.Summary.Better += s.Summary.Better
		r.Summary.Worse += s.Summary.Worse
		r.Summary.Unchanged += s.Summary.Unchanged
		r.Summary.Changed += s.Summary.Changed
		r.Summary.Unknown += s.Summary.Unknown
		r.Verdicts[s.Verdict]++
	}
	return r
}

// HasRegression reports whether any scenario degraded a scored metric. Callers
// that want the harness to gate CI use this; it is not the default, because the
// harness's job is to report honestly, not to block on a number nobody has
// baselined yet.
func (r Report) HasRegression() bool {
	return r.Summary.Worse > 0
}

// WriteJSON emits the machine-readable report.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteHuman renders a readable report.
//
// It leads with activity and the noise floor, and only then shows deltas —
// the same order the reader needs to judge them in. Unchanged metrics are
// listed rather than hidden: "we measured this and it did not move" is a
// finding, and suppressing it would leave a reader assuming the harness simply
// did not look.
func (r Report) WriteHuman(w io.Writer) error {
	// Write errors are latched rather than checked per call: the report is ~40
	// Fprintf calls and threading an error through each would drown the layout.
	// A broken stdout surfaces once, at the end.
	var werr error
	p := func(format string, args ...any) {
		if werr != nil {
			return
		}
		_, werr = fmt.Fprintf(w, format, args...)
	}
	p("brainbench — do the cognitive processes improve what the brain knows?\n")
	p("generated %s\n\n", r.GeneratedAt.Format(time.RFC3339))
	p("conditions: %s | embedder %s | backend %s\n", r.Config.Extraction, r.Config.Embedder, r.Config.Backend)
	p("design:     %s\n\n", r.Config.Design)

	for _, s := range r.Scenarios {
		p("──────────────────────────────────────────────────────────────────────\n")
		p("SCENARIO %s — %s\n", s.ID, s.Description)
		p("  stages under test: %s\n", strings.Join(s.Stages, ", "))
		p("  seed: %d events, %d claims (%d collapsed by ingest dedupe), %d edges, %d vectors\n",
			s.Seed.Events, s.Seed.Claims, s.Seed.DedupedAtIngest, s.Seed.Relationships, s.Seed.Embeddings)

		p("\n  ACTIVITY (what the process reported doing — NOT evidence of benefit)\n")
		p("    scanned %d | duplicate groups %d | merged %d | forgotten %d | refuted %d\n",
			s.Activity.ClaimsScanned, s.Activity.DuplicateGroups, s.Activity.Merged,
			s.Activity.Forgotten, s.Activity.Refuted)
		p("    validated %d | credited %d | salience %d | replayed %d (protected %d)\n",
			s.Activity.Validated, s.Activity.Credited, s.Activity.SalienceTagged,
			s.Activity.Replayed, s.Activity.ReplayProtected)
		p("    associations decayed %d | inhibition decayed %d | lessons %d | playbooks %d\n",
			s.Activity.AssociationsDecayed, s.Activity.InhibitionDecayed,
			s.Activity.LessonsSynthesized, s.Activity.PlaybooksSynthesized)
		p("    total mutations: %d\n", s.Activity.Total)

		p("\n  VALIDITY\n")
		if s.Validity.Deterministic {
			p("    measurement deterministic: yes (two untreated arms agreed on every metric)\n")
		} else {
			p("    measurement deterministic: NO — %s\n", strings.Join(s.Validity.NondeterministicMetrics, ", "))
			p("    deltas below this noise floor cannot be attributed to the process\n")
		}
		p("    probes: %d — smallest expressible rate change is %.4f\n",
			s.Validity.ProbeCount, s.Validity.MinResolvableRateDelta)

		p("\n  EFFECT (treatment minus control)\n")
		p("    %-34s %10s %10s %10s  %s\n", "metric", "control", "treatment", "delta", "verdict")
		for _, c := range s.Comparisons {
			ctrl := fmtMetric(c.Control)
			trt := fmtMetric(c.Treatment)
			delta := "—"
			if c.Verdict != VerdictUnknown {
				delta = fmt.Sprintf("%+.4f", c.Delta)
			}
			p("    %-34s %10s %10s %10s  %s%s\n",
				c.Name, ctrl, trt, delta, verdictMark(c.Verdict), verdictLabel(c))
			if c.Note != "" {
				p("    %-34s %s\n", "", "^ "+c.Note)
			}
		}
		p("\n  SUMMARY: %d better, %d worse, %d unchanged, %d changed-but-unscored, %d unknown\n",
			s.Summary.Better, s.Summary.Worse, s.Summary.Unchanged, s.Summary.Changed, s.Summary.Unknown)
		p("  VERDICT: %s — %s\n\n", s.Verdict, verdictExplanation(s.Verdict))
	}

	p("──────────────────────────────────────────────────────────────────────\n")
	p("TOTAL: %d better, %d worse, %d unchanged, %d changed-but-unscored, %d unknown\n",
		r.Summary.Better, r.Summary.Worse, r.Summary.Unchanged, r.Summary.Changed, r.Summary.Unknown)
	verdicts := make([]string, 0, len(r.Verdicts))
	for v, n := range r.Verdicts {
		verdicts = append(verdicts, fmt.Sprintf("%s=%d", v, n))
	}
	sort.Strings(verdicts)
	p("VERDICTS: %s\n\n", strings.Join(verdicts, " "))

	p("WHAT THIS HARNESS DOES NOT MEASURE\n")
	for _, l := range r.Limitations {
		p("  - %s\n", wrapIndent(l, 76, "    "))
	}
	return werr
}

func fmtMetric(m Metric) string {
	if !m.Known {
		return "unknown"
	}
	if m.Unit == "count" || m.Unit == "bytes" {
		return fmt.Sprintf("%.0f", m.Value)
	}
	return fmt.Sprintf("%.4f", m.Value)
}

func verdictMark(v Verdict) string {
	switch v {
	case VerdictBetter:
		return "+ "
	case VerdictWorse:
		return "- "
	case VerdictChanged:
		return "~ "
	case VerdictUnknown:
		return "? "
	default:
		return "  "
	}
}

func verdictLabel(c Comparison) string {
	if c.Verdict == VerdictUnknown {
		reason := c.Control.Unknown
		if reason == "" {
			reason = c.Treatment.Unknown
		}
		if reason != "" {
			return string(c.Verdict) + " (" + reason + ")"
		}
	}
	if c.Verdict == VerdictChanged {
		return "changed (descriptive — no direction claimed)"
	}
	return string(c.Verdict)
}

func verdictExplanation(v RunVerdict) string {
	switch v {
	case VerdictInert:
		return "the process reported zero mutations; it did not act on this brain"
	case VerdictNoEffect:
		return "the process mutated the brain but moved no scored metric — activity without demonstrated benefit"
	case VerdictImproved:
		return "scored metrics moved only in the improving direction"
	case VerdictRegressed:
		return "scored metrics moved only in the degrading direction"
	case VerdictMixed:
		return "some scored metrics improved and others degraded; read the rows, not the headline"
	case VerdictInvalid:
		return "two identically-untreated arms disagreed, so no delta in this run is attributable"
	default:
		return ""
	}
}

// wrapIndent soft-wraps long limitation text so the human report stays readable
// in a terminal.
func wrapIndent(s string, width int, indent string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	lineLen := 0
	for i, w := range words {
		if i > 0 {
			if lineLen+1+len(w) > width {
				b.WriteString("\n" + indent)
				lineLen = 0
			} else {
				b.WriteString(" ")
				lineLen++
			}
		}
		b.WriteString(w)
		lineLen += len(w)
	}
	return b.String()
}
