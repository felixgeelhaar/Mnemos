package relate

import (
	"strings"
	"testing"

	"go.klarlabs.de/mnemos/internal/domain"
)

func TestExtractNumerics_ParsesIntsFloatsAndUnits(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []numericValue
	}{
		{"plain int", "12 prior refunds", []numericValue{{value: 12, unit: "raw:prior"}}},
		{"unitless int", "the count is 5", []numericValue{{value: 5}}},
		{"with unit", "p99 latency was 250ms", []numericValue{{value: 0.250, unit: "time"}}},
		{"percent", "error rate hit 32%", []numericValue{{value: 32, unit: "percent"}}},
		{"currency", "lifetime value $4500", []numericValue{{value: 4500, unit: "usd"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractNumerics(tc.text)
			if len(got) == 0 {
				t.Fatalf("extractNumerics(%q) returned no values", tc.text)
			}
			if got[0].value != tc.want[0].value {
				t.Errorf("value = %v, want %v", got[0].value, tc.want[0].value)
			}
			if got[0].unit != tc.want[0].unit {
				t.Errorf("unit = %q, want %q", got[0].unit, tc.want[0].unit)
			}
		})
	}
}

func TestNumericValuesAgree_HonoursUnitConversion(t *testing.T) {
	a := extractNumerics("response time is 1 second")
	b := extractNumerics("response time is 1000 ms")
	if !numericValuesAgree(a, b) {
		t.Errorf("1 second and 1000 ms should agree (unit conversion)")
	}
}

func TestNumericValuesAgree_DifferentValuesDoNotAgree(t *testing.T) {
	a := extractNumerics("user has 12 refunds")
	b := extractNumerics("user has 0 refunds")
	if numericValuesAgree(a, b) {
		t.Errorf("12 and 0 must not agree")
	}
}

func TestNumericValuesAgree_DifferentFamiliesDoNotAgree(t *testing.T) {
	// "5 minutes" (time family, normalized to 300s) vs "5 GB" (bytes family).
	// Same magnitude raw, different unit family → disagreement.
	a := extractNumerics("the limit is 5 minutes")
	b := extractNumerics("the limit is 5 GB")
	if numericValuesAgree(a, b) {
		t.Errorf("different unit families should not agree")
	}
}

func TestDetectNumericDivergence_FlagsNumericDisagreement(t *testing.T) {
	aText := "the user has 12 prior refunds"
	bText := "the user has 0 prior refunds"
	aTok, _ := contentTokensAndPolarity(aText)
	bTok, _ := contentTokensAndPolarity(bText)
	if !detectNumericDivergence(aText, bText, aTok, bTok) {
		t.Errorf("12 vs 0 prior refunds should be flagged as divergent")
	}
}

func TestDetectNumericDivergence_AgreesOnEquivalentValues(t *testing.T) {
	aText := "p99 latency hit 1 second yesterday"
	bText := "p99 latency hit 1000 ms yesterday"
	aTok, _ := contentTokensAndPolarity(aText)
	bTok, _ := contentTokensAndPolarity(bText)
	if detectNumericDivergence(aText, bText, aTok, bTok) {
		t.Errorf("1s vs 1000ms must not flag")
	}
}

func TestDetect_NumericDivergenceOverridesSupports(t *testing.T) {
	// "12 refunds" and "0 refunds" share most tokens (would normally
	// be classified as `supports`), but the numeric disagreement must
	// flip the verdict to `contradicts`.
	engine := NewEngine()
	engine.nextID = seqRelationshipIDs()
	rels, err := engine.Detect([]domain.Claim{
		{ID: "cl_1", Text: "The user has 12 prior refunds"},
		{ID: "cl_2", Text: "The user has 0 prior refunds"},
	})
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if len(rels) == 0 {
		t.Fatal("expected at least one relationship between numeric-disagreeing claims")
	}
	hasContradicts := false
	for _, r := range rels {
		if r.Type == domain.RelationshipTypeContradicts {
			hasContradicts = true
		}
	}
	if !hasContradicts {
		t.Errorf("expected contradicts (got %v)", rels)
	}
}

func TestDetectNumericDivergence_RejectsUnrelatedClaims(t *testing.T) {
	// Different topics, both contain numbers — should NOT flag.
	aText := "the deployment took 12 minutes"
	bText := "the team has 0 incidents this quarter"
	aTok, _ := contentTokensAndPolarity(aText)
	bTok, _ := contentTokensAndPolarity(bText)
	if detectNumericDivergence(aText, bText, aTok, bTok) {
		t.Errorf("unrelated claims must not be flagged on numeric difference alone")
	}
}

// TestNumericValuesAgree_SilenceIsNotDisagreement pins the rule that a claim
// which says nothing about a quantity is not contradicting one that does.
// Treating an unmatched value as a conflict meant a claim listing several
// figures contradicted a shorter one simply for being longer.
func TestNumericValuesAgree_SilenceIsNotDisagreement(t *testing.T) {
	a := extractNumerics("the image is 8721074 bytes after 12 builds")
	b := extractNumerics("the image is 8721074 bytes")
	if !numericValuesAgree(a, b) {
		t.Errorf("b is silent about builds, not in disagreement about them: %v vs %v", a, b)
	}
}

// TestNumericValuesAgree_SharedFamilyStillDisagrees proves the silence rule
// did not disarm the detector: when both claims assert a value for the SAME
// quantity and they differ, that is still a disagreement.
func TestNumericValuesAgree_SharedFamilyStillDisagrees(t *testing.T) {
	a := extractNumerics("the image is 8721074 bytes after 12 builds")
	b := extractNumerics("the image is 9000000 bytes after 12 builds")
	if numericValuesAgree(a, b) {
		t.Errorf("both assert a byte size and they differ: %v vs %v", a, b)
	}
}

// TestExtractNumerics_UnitEndsAtWordBoundary pins the invariant the doc
// comment always claimed but the pattern did not enforce: a longer
// neighbouring word must not be truncated into a bogus unit family.
func TestExtractNumerics_UnitEndsAtWordBoundary(t *testing.T) {
	for _, tc := range []struct{ text, bogus string }{
		{"2 versions were cut", "raw:versio"},
		{"4 unpushed commits", "raw:unpush"},
		{"396 frontend routes", "raw:fronte"},
	} {
		for _, v := range extractNumerics(tc.text) {
			if v.unit == tc.bogus {
				t.Errorf("%q: truncated %q into a unit family", tc.text, tc.bogus)
			}
		}
	}
}

// TestExtractNumerics_LocatorsAreNotQuantities pins that numbers bound to an
// identifier by punctuation name a place, not a measurement.
func TestExtractNumerics_LocatorsAreNotQuantities(t *testing.T) {
	for _, text := range []string{"render.go:133 writes the header", "listening on imap.example.org:993"} {
		if got := extractNumerics(text); len(got) != 0 {
			t.Errorf("%q: locators are not quantities, got %v", text, got)
		}
	}
	// A unit suffix vetoes the exclusion — `timeout:30s` is a measurement.
	if got := extractNumerics("timeout:30s"); len(got) != 1 || got[0].unit != "time" {
		t.Errorf("timeout:30s must stay a measurement, got %v", got)
	}
}

// Real false positives from a production brain, all of which cleared the
// pre-tuning anchors (0.50 of the shorter claim, 0.30 of the longer, no
// absolute floor). Together they were 47.5% of every live contradiction edge.
func TestDetectNumericDivergence_RejectsSharedGenericVocabulary(t *testing.T) {
	cases := []struct{ name, a, b string }{
		{
			"different test suites in different projects",
			"245 tests green, including the existing state-file guards",
			"Full suite green (1950 tests)",
		},
		{
			"shared word is incidental",
			"1838 migrated, 0 failures",
			"lockout after repeated failures → 429",
		},
		{
			// Two word tokens, so ANY shared pair scores 1.0 against the
			// shorter claim. The ratio bars alone cannot reject this; the
			// absolute token floor is what does.
			"claim too short to identify a subject",
			"25 tests green",
			"phpunit at 13.2.4 (latest), 878 tests green",
		},
		{
			"different measurements of different things",
			"Dry run matches the database exactly: 1018 events + 816 claims + 4 decisions = 1838, zero drift",
			"Exactly as predicted — 206 events, 412 claims cascaded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			aTok, _ := contentTokensAndPolarity(tc.a)
			bTok, _ := contentTokensAndPolarity(tc.b)
			if detectNumericDivergence(tc.a, tc.b, aTok, bTok) {
				t.Errorf("flagged as a numeric contradiction:\n  %q\n  %q", tc.a, tc.b)
			}
		})
	}
}

// The canonical true positive must survive the tightened anchors. Its shared
// subject is exactly three word tokens ({user, prior, refunds}), which is what
// pins minNumericSubjectTokens at 3 rather than 4.
func TestDetectNumericDivergence_KeepsTheCanonicalTruePositive(t *testing.T) {
	a, b := "the user has 12 prior refunds", "the user has 0 prior refunds"
	aTok, _ := contentTokensAndPolarity(a)
	bTok, _ := contentTokensAndPolarity(b)
	if !detectNumericDivergence(a, b, aTok, bTok) {
		t.Error("the canonical numeric contradiction was lost to the tightened anchors")
	}
}

// A bound and a value that satisfies it do not disagree.
//
// `require: {"php": ">=8.1"}` next to "PHP 8.3" is the single most common shape
// among the numeric edges that survived the #360 anchor tightening — 67 of 75.
// The two claims AGREE; comparing a constraint as though it were a measurement
// is what made them contradict.
func TestDetectNumericDivergence_BoundIsNotAMeasurement(t *testing.T) {
	cases := []struct{ name, a, b string }{
		{
			"dependency constraint vs installed version",
			`Zero runtime dependencies (` + "`" + `require: {"php": ">=8.1"}` + "`" + ` only); DDD layering; TDD`,
			"PHP 8.3, zero runtime dependencies (composer requires only `php`), DDD layering",
		},
		{
			"caret range vs resolved version",
			"the parser package is pinned at ^4.2 in the manifest",
			"the parser package resolved to 4.9 in the manifest",
		},
		{
			"word-form bound vs value",
			"the retention window must be at least 30 days for audit",
			"the retention window is 90 days for audit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			aTok, _ := contentTokensAndPolarity(tc.a)
			bTok, _ := contentTokensAndPolarity(tc.b)
			if detectNumericDivergence(tc.a, tc.b, aTok, bTok) {
				t.Errorf("a bound was compared as a value:\n  %q\n  %q", tc.a, tc.b)
			}
		})
	}
}

// The exclusion must not swallow ordinary measurements that merely sit near a
// symbol, or it would silence the detector wholesale.
func TestIsBounded_LeavesPlainMeasurementsAlone(t *testing.T) {
	plain := "the user has 12 prior refunds"
	if isBounded(plain, strings.Index(plain, "12")-1) {
		t.Error("a plain measurement was treated as a bound")
	}
	a, b := "the user has 12 prior refunds", "the user has 0 prior refunds"
	aTok, _ := contentTokensAndPolarity(a)
	bTok, _ := contentTokensAndPolarity(b)
	if !detectNumericDivergence(a, b, aTok, bTok) {
		t.Error("the canonical true positive was lost to the bound exclusion")
	}
}
