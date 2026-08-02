// Package brainbench is a controlled-experiment harness that answers one
// question the rest of the codebase cannot: do the cognitive processes
// (consolidate / forget / reinforce / credit / salience / synthesize / decay)
// actually IMPROVE what the brain knows?
//
// Mnemos can already demonstrate that those processes RAN — `mnemos
// consolidate` returns counters, the cognitive journal records passes, ADR 0019
// reports vital signs. None of that is evidence of BENEFIT. A pass that
// deprecates 400 claims and decays 900 edges has produced a large number and no
// established improvement. This package exists to make that distinction
// measurable rather than rhetorical.
//
// # Method
//
// A paired A/B experiment on byte-identical brains:
//
//  1. Seed one pristine brain from a scenario corpus (deterministic: rule-based
//     extraction, a deterministic stub embedder, no network, no LLM).
//  2. Copy the seed file three times. The copies are byte-identical, so the arms
//     cannot differ for any reason except the treatment.
//  3. CONTROL: measure, run nothing.
//  4. TREATMENT: run the process set, then measure.
//  5. NOISE: a second untreated copy, measured independently. It must produce
//     exactly the control's numbers. When it does not, measurement is not
//     deterministic and no delta from this run can be attributed to the process
//     — the report says so and stops claiming an effect.
//
// Measurement itself is a mutation (recall applies Hebbian strengthening,
// reconsolidation and competitive inhibition write-backs), which is why every
// arm is a throwaway copy and each is measured exactly once.
//
// # Honesty constraints
//
// These are the point of the package, not decoration:
//
//   - Every metric declares a DIRECTION. Metrics with no defensible normative
//     direction (claim count, database size, mean trust) are Descriptive: the
//     harness reports that they changed and refuses to call the change good.
//     Forgetting mechanically shrinks the brain and mechanically raises mean
//     trust; scoring those as wins would make the harness a rubber stamp.
//   - A metric that cannot be computed reliably is reported UNKNOWN with a
//     reason. It never gets a fabricated number, and an unknown on either arm
//     makes the comparison unknown rather than optimistic.
//   - Regressions are first-class. GoldSurvival exists specifically to catch the
//     failure mode where a process improves retrieval precision by destroying
//     knowledge.
//   - Activity and benefit are reported in separate fields and reconciled in the
//     verdict: a run with high activity and no metric movement is labelled
//     ran-no-measurable-effect, not success.
package brainbench

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Scenario is one controlled experiment: a corpus to seed both arms from, the
// probes both arms are measured with, and the process set the treatment arm
// runs. It deliberately mirrors the seeds/query/gold idiom of
// data/eval/retrieval.yaml so the two suites read alike, but it is a different
// unit of work: retrieval.yaml compares RANKERS over a fixed store, this
// compares STORES that a process has or has not been applied to.
type Scenario struct {
	ID          string `yaml:"id"`
	Description string `yaml:"description"`

	// Corpus is ingested document-by-document, in order, each as its own
	// batch — the way a real brain accumulates across sessions, and the only
	// way near-duplicates across sessions arise at all. Ingesting the whole
	// corpus as one batch would let the ingest-time deduper collapse exactly
	// the redundancy consolidation is supposed to be tested on.
	Corpus []Doc `yaml:"corpus"`

	// Warmup queries are run against the SEED, before the arms are copied, so
	// both arms inherit their effects identically.
	//
	// They exist because several consolidation stages act on state that only
	// retrieval creates. DecayAssociations pulls over-base Hebbian edge
	// strengths back toward 1.0, and DecayInhibition pulls competitive-
	// inhibition weights back toward 0 — but on a freshly-ingested brain no
	// edge is over base and nothing is inhibited, so both stages report zero
	// and the scenario measures nothing. Without a warmup the harness would
	// report "inert" for a stage that is merely untested, which is a different
	// and much less useful claim.
	Warmup []string `yaml:"warmup"`

	// Probes are the retrieval questions both arms answer.
	Probes []Probe `yaml:"probes"`

	// Process is the treatment: the exact consolidation option set under test.
	Process Process `yaml:"process"`
}

// Doc is one ingested document.
type Doc struct {
	ID     string `yaml:"id"`
	Source string `yaml:"source"`

	// AgeDays back-dates the document's event timestamp. This is not cosmetic:
	// trust freshness decays on a 90-day half-life, so with everything stamped
	// "now" the forgetting stage can never fire and a scenario would silently
	// measure nothing. Back-dating is the only way to reach decay-driven
	// behaviour offline and deterministically.
	AgeDays int `yaml:"age_days"`

	Text string `yaml:"text"`
}

// Probe is one retrieval question plus what a correct answer looks like.
//
// Expectations are matched on TEXT substrings, not claim ids: claim ids are
// minted per ingest and are not stable across runs, and a scenario author can
// reason about text but not about a hash.
type Probe struct {
	ID    string `yaml:"id"`
	Query string `yaml:"query"`

	// Expect is a case-insensitive substring the correct claim must contain.
	Expect string `yaml:"expect"`

	// MustNot lists substrings that should NOT appear in the returned set —
	// superseded or refuted statements the brain ought to stop surfacing.
	// Optional; when no probe in a scenario declares any, the corresponding
	// metric is reported unknown rather than a vacuous 0.
	MustNot []string `yaml:"must_not"`

	// Limit caps results for this probe. 0 uses ProbeDefaultLimit.
	Limit int `yaml:"limit"`
}

// ProbeDefaultLimit is the top-k used when a probe does not set Limit.
const ProbeDefaultLimit = 10

// Process is the consolidation option set the treatment arm runs, expressed in
// scenario YAML so each scenario declares precisely which process set it is
// making a claim about. Field-for-field this is mnemos.ConsolidateOptions; it is
// restated here rather than embedded so the YAML key names are stable even if
// the library struct is reorganised, and so a scenario cannot smuggle in
// non-deterministic options.
type Process struct {
	DedupeThreshold    float64 `yaml:"dedupe_threshold"`
	ForgetBelowTrust   float64 `yaml:"forget_below_trust"`
	ForgetRefuted      bool    `yaml:"forget_refuted"`
	ReinforceValidated bool    `yaml:"reinforce_validated"`
	ReinforcePlaybooks bool    `yaml:"reinforce_playbooks"`
	AssignCredit       bool    `yaml:"credit"`
	AssignSalience     bool    `yaml:"salience"`
	Plastic            bool    `yaml:"plastic"`
	DecayAssociations  bool    `yaml:"decay_associations"`
	DecayInhibition    bool    `yaml:"decay_inhibition"`
	Synthesize         bool    `yaml:"synthesize"`
	ReplayTopK         int     `yaml:"replay_top_k"`
}

// Enabled reports the process stages this scenario turns on, for the report.
// Named stages, not a bool blob, so a reader can tell what was under test.
func (p Process) Enabled() []string {
	var out []string
	// Dedupe + trust-refresh always run in a consolidation pass; they are not
	// optional stages, so they are listed unconditionally.
	out = append(out, "dedupe", "trust_refresh")
	if p.ForgetBelowTrust > 0 {
		out = append(out, fmt.Sprintf("forget_below_trust=%.2f", p.ForgetBelowTrust))
	}
	for _, f := range []struct {
		on   bool
		name string
	}{
		{p.ForgetRefuted, "forget_refuted"},
		{p.ReinforceValidated, "reinforce_validated"},
		{p.ReinforcePlaybooks, "reinforce_playbooks"},
		{p.AssignCredit, "credit"},
		{p.AssignSalience, "salience"},
		{p.Plastic, "plastic"},
		{p.DecayAssociations, "decay_associations"},
		{p.DecayInhibition, "decay_inhibition"},
		{p.Synthesize, "synthesize"},
	} {
		if f.on {
			out = append(out, f.name)
		}
	}
	if p.ReplayTopK > 0 {
		out = append(out, fmt.Sprintf("replay_top_k=%d", p.ReplayTopK))
	}
	return out
}

// Validate rejects a scenario that cannot yield an interpretable result.
//
// It is strict on purpose. A scenario with no probes still "runs" and still
// prints a table, but every retrieval metric would be an empty average — a
// number with no content behind it. Refusing to load it is the difference
// between a harness and a number generator.
func (s Scenario) Validate() error {
	if strings.TrimSpace(s.ID) == "" {
		return fmt.Errorf("scenario: id is required")
	}
	if len(s.Corpus) == 0 {
		return fmt.Errorf("scenario %s: at least one corpus document is required", s.ID)
	}
	seenDoc := make(map[string]struct{}, len(s.Corpus))
	for i, d := range s.Corpus {
		if strings.TrimSpace(d.ID) == "" {
			return fmt.Errorf("scenario %s: corpus[%d]: id is required", s.ID, i)
		}
		if _, dup := seenDoc[d.ID]; dup {
			return fmt.Errorf("scenario %s: corpus doc id %q is duplicated", s.ID, d.ID)
		}
		seenDoc[d.ID] = struct{}{}
		if strings.TrimSpace(d.Text) == "" {
			return fmt.Errorf("scenario %s: corpus doc %s: text is required", s.ID, d.ID)
		}
		if d.AgeDays < 0 {
			return fmt.Errorf("scenario %s: corpus doc %s: age_days must not be negative", s.ID, d.ID)
		}
	}
	if len(s.Probes) == 0 {
		return fmt.Errorf("scenario %s: at least one probe is required "+
			"(a scenario with no probes cannot measure answer quality)", s.ID)
	}
	seenProbe := make(map[string]struct{}, len(s.Probes))
	for i, p := range s.Probes {
		if strings.TrimSpace(p.ID) == "" {
			return fmt.Errorf("scenario %s: probes[%d]: id is required", s.ID, i)
		}
		if _, dup := seenProbe[p.ID]; dup {
			return fmt.Errorf("scenario %s: probe id %q is duplicated", s.ID, p.ID)
		}
		seenProbe[p.ID] = struct{}{}
		if strings.TrimSpace(p.Query) == "" {
			return fmt.Errorf("scenario %s: probe %s: query is required", s.ID, p.ID)
		}
		if strings.TrimSpace(p.Expect) == "" {
			return fmt.Errorf("scenario %s: probe %s: expect is required", s.ID, p.ID)
		}
		if p.Limit < 0 {
			return fmt.Errorf("scenario %s: probe %s: limit must not be negative", s.ID, p.ID)
		}
	}
	if s.Process.DedupeThreshold < 0 || s.Process.DedupeThreshold > 1 {
		return fmt.Errorf("scenario %s: process.dedupe_threshold must be in [0, 1]", s.ID)
	}
	if s.Process.ForgetBelowTrust < 0 || s.Process.ForgetBelowTrust > 1 {
		return fmt.Errorf("scenario %s: process.forget_below_trust must be in [0, 1]", s.ID)
	}
	if s.Process.ReplayTopK < 0 {
		return fmt.Errorf("scenario %s: process.replay_top_k must not be negative", s.ID)
	}
	return nil
}

// ParseScenario decodes and validates one scenario YAML document.
//
// KnownFields is enabled: a typo'd key (`forget_bellow_trust`) would otherwise
// be silently ignored, and the scenario would quietly test a different process
// set than its author wrote down — the worst possible failure for an
// experimental harness, because the result still looks credible.
func ParseScenario(data []byte) (Scenario, error) {
	var s Scenario
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return Scenario{}, fmt.Errorf("parse scenario: %w", err)
	}
	if err := s.Validate(); err != nil {
		return Scenario{}, err
	}
	return s, nil
}
