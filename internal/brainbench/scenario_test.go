package brainbench

import (
	"strings"
	"testing"
)

const minimalScenario = `
id: t
description: d
corpus:
  - id: c1
    text: The service runs on PostgreSQL 16.
probes:
  - id: p1
    query: which database
    expect: PostgreSQL
`

func TestParseScenario_Minimal(t *testing.T) {
	sc, err := ParseScenario([]byte(minimalScenario))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sc.ID != "t" || len(sc.Corpus) != 1 || len(sc.Probes) != 1 {
		t.Fatalf("unexpected scenario: %+v", sc)
	}
}

// TestParseScenario_RejectsUnknownKeys is the highest-value parser test here.
// A typo'd process key would otherwise be silently ignored and the scenario
// would test a DIFFERENT process set than its author wrote, while still
// producing a credible-looking report. That is the worst failure mode an
// experimental harness can have.
func TestParseScenario_RejectsUnknownKeys(t *testing.T) {
	typo := minimalScenario + `
process:
  forget_bellow_trust: 0.5
`
	_, err := ParseScenario([]byte(typo))
	if err == nil {
		t.Fatal("a misspelled process key must be rejected, not silently ignored")
	}
	if !strings.Contains(err.Error(), "forget_bellow_trust") {
		t.Fatalf("error should name the offending key, got: %v", err)
	}
}

func TestScenario_Validate(t *testing.T) {
	valid := func() Scenario {
		return Scenario{
			ID:     "s",
			Corpus: []Doc{{ID: "c", Text: "text"}},
			Probes: []Probe{{ID: "p", Query: "q", Expect: "e"}},
		}
	}
	cases := []struct {
		name   string
		mutate func(*Scenario)
		want   string
	}{
		{"missing id", func(s *Scenario) { s.ID = "" }, "id is required"},
		{"no corpus", func(s *Scenario) { s.Corpus = nil }, "corpus document"},
		{"no probes", func(s *Scenario) { s.Probes = nil }, "probe is required"},
		{"probe without expect", func(s *Scenario) { s.Probes[0].Expect = "" }, "expect is required"},
		{"duplicate doc id", func(s *Scenario) {
			s.Corpus = append(s.Corpus, Doc{ID: "c", Text: "other"})
		}, "duplicated"},
		{"duplicate probe id", func(s *Scenario) {
			s.Probes = append(s.Probes, Probe{ID: "p", Query: "q2", Expect: "e2"})
		}, "duplicated"},
		{"negative age", func(s *Scenario) { s.Corpus[0].AgeDays = -1 }, "age_days"},
		{"out of range forget", func(s *Scenario) { s.Process.ForgetBelowTrust = 1.5 }, "forget_below_trust"},
		{"out of range dedupe", func(s *Scenario) { s.Process.DedupeThreshold = -0.1 }, "dedupe_threshold"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := valid()
			tc.mutate(&s)
			err := s.Validate()
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}

	if err := valid().Validate(); err != nil {
		t.Fatalf("baseline scenario should be valid, got: %v", err)
	}
}

func TestProcess_Enabled(t *testing.T) {
	p := Process{ForgetBelowTrust: 0.4, AssignCredit: true, ReplayTopK: 5}
	got := strings.Join(p.Enabled(), ",")
	for _, want := range []string{"dedupe", "trust_refresh", "forget_below_trust=0.40", "credit", "replay_top_k=5"} {
		if !strings.Contains(got, want) {
			t.Errorf("stages %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "salience") {
		t.Errorf("stages %q lists a stage that was not enabled", got)
	}
}

// TestLoadScenarios_ShippedFixturesAreValid guards the fixtures themselves: an
// invalid scenario would otherwise only be discovered when someone ran the
// harness, and LoadScenarios treats it as a hard error by design.
func TestLoadScenarios_ShippedFixturesAreValid(t *testing.T) {
	scenarios, err := LoadScenarios("../../data/eval/brainbench")
	if err != nil {
		t.Fatalf("shipped scenarios must load: %v", err)
	}
	if len(scenarios) == 0 {
		t.Fatal("no shipped scenarios found")
	}
	for _, sc := range scenarios {
		if err := sc.Validate(); err != nil {
			t.Errorf("scenario %s: %v", sc.ID, err)
		}
	}
}
