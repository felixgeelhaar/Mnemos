package relate

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// numericValue is one number-with-optional-unit extracted from claim text.
// Two values match (i.e. don't contradict) when their normalized magnitude
// is equal up to a small relative tolerance.
type numericValue struct {
	raw   string  // exactly as it appeared, for debugging
	value float64 // the parsed magnitude, normalized to a base unit
	unit  string  // canonical unit name, "" when unitless
}

// numericTokenRE matches one numeric literal with optional currency
// prefix or unit suffix. Constraints:
//   - the number must not be preceded by a letter (so "p99" is skipped)
//   - the unit suffix, when present, ends at a word boundary so
//     "latency" doesn't get parsed as a 4-char unit "late". The \b is
//     load-bearing: without it the class silently truncated any longer
//     neighbouring word into a bogus unit family ("versions"→"versio",
//     "released"→"releas", "unpushed"→"unpush"), so two claims were compared
//     under a unit that does not exist. The bound is 7 to admit the longest
//     real unit tokens ("seconds", "minutes").
//   - the percent sign is its own unit family
//   - currency symbols can lead the number ($4500) or trail as ISO
//     codes (4500 USD)
//
// Submatches: 1 = leading currency symbol (or empty),
// 2 = signed-magnitude string (commas allowed as thousands separators),
// 3 = trailing unit token (or empty).
var numericTokenRE = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_])([$€£¥])?(-?\d+(?:[.,]\d+)?)(%|\s*[a-z]{1,7}\b)?`)

// unitFamily maps each recognized unit token to a (family, multiplier)
// pair. Two values from the same family compare via their normalized
// magnitudes; values from different families (or unknown units) are
// treated as different magnitudes regardless.
var unitFamily = map[string]struct {
	family string
	mult   float64
}{
	// Time
	"ms": {"time", 0.001}, "msec": {"time", 0.001},
	"s": {"time", 1}, "sec": {"time", 1}, "secs": {"time", 1}, "second": {"time", 1}, "seconds": {"time", 1},
	"m": {"time", 60}, "min": {"time", 60}, "mins": {"time", 60}, "minute": {"time", 60}, "minutes": {"time", 60},
	"h": {"time", 3600}, "hr": {"time", 3600}, "hour": {"time", 3600}, "hours": {"time", 3600},

	// Percent (its own family — a bare number is not the same as a percent)
	"%": {"percent", 1},

	// Currency: keep symbols apart (we don't FX-convert).
	"$":   {"usd", 1},
	"usd": {"usd", 1},
	"€":   {"eur", 1},
	"eur": {"eur", 1},
	"£":   {"gbp", 1},
	"gbp": {"gbp", 1},

	// Bytes
	"b":  {"bytes", 1},
	"kb": {"bytes", 1024}, "mb": {"bytes", 1024 * 1024},
	"gb": {"bytes", 1024 * 1024 * 1024}, "tb": {"bytes", 1024 * 1024 * 1024 * 1024},
}

// extractNumerics finds every numeric-with-optional-unit literal in the
// claim text. Returns at most a handful per claim — claims with dozens
// of numerics are usually data dumps where contradiction detection isn't
// useful anyway.
func extractNumerics(text string) []numericValue {
	matches := numericTokenRE.FindAllStringSubmatchIndex(text, -1)
	out := make([]numericValue, 0, len(matches))
	for _, loc := range matches {
		m := make([]string, 0, 4)
		for g := 0; g < 4; g++ {
			if loc[2*g] < 0 {
				m = append(m, "")
				continue
			}
			m = append(m, text[loc[2*g]:loc[2*g+1]])
		}
		// An identifier is not a measurement. "PR #225" and "PR #257" name two
		// different things; they do not disagree about a quantity. Without this,
		// every pair of issue/PR references on a shared topic was filed as a
		// numeric contradiction — the single largest source of false numeric
		// edges on a production brain.
		if idx := loc[0]; idx >= 0 {
			seg := text[idx:loc[1]]
			if strings.Contains(seg, "#") {
				continue
			}
			if isLocator(text, idx, m[3]) {
				continue
			}
		}
		raw := m[0]
		currency := strings.ToLower(strings.TrimSpace(m[1]))
		numStr := strings.ReplaceAll(m[2], ",", "")
		v, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			continue
		}
		suffix := strings.ToLower(strings.TrimSpace(m[3]))
		// Currency prefix beats trailing unit; otherwise use the suffix.
		unitTok := suffix
		if currency != "" {
			unitTok = currency
		}
		family := ""
		mult := 1.0
		if uf, ok := unitFamily[unitTok]; ok {
			family = uf.family
			mult = uf.mult
		} else if unitTok != "" {
			// Unknown but non-empty unit — keep as a one-off family so
			// "12 widgets" and "0 widgets" still compare via the family
			// label even though we can't normalize across families.
			family = "raw:" + unitTok
		}
		out = append(out, numericValue{
			raw:   raw,
			value: v * mult,
			unit:  family,
		})
	}
	return out
}

// numericValuesAgree reports whether the two claims are free of numeric
// disagreement.
//
// Disagreement requires BOTH claims to assert a value for the same quantity
// and for those values to differ. Silence is not disagreement: if b never
// mentions a's unit family at all, b is not contradicting a — it simply says
// nothing about it. Treating an unmatched value as a conflict made every
// number-bearing pair on a shared topic collide, because a claim listing four
// figures will nearly always name one its counterpart does not, and the
// asymmetry meant a longer claim contradicted a shorter one for being longer.
// On a production brain that path accounted for 78% of live numeric edges.
func numericValuesAgree(a, b []numericValue) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	// Single-vs-single is the unambiguous case: each claim asserts exactly one
	// number, so both are about the shared subject and a mismatch in either
	// family or magnitude is a real disagreement ("the limit is 5 minutes" vs
	// "the limit is 5 GB"). The silence rule below cannot see that, because it
	// cannot tell "b says nothing about time" from "b says the limit is in GB
	// instead" — with only one value each, there is nothing else it could be.
	if len(a) == 1 && len(b) == 1 {
		return a[0].unit == b[0].unit && floatsClose(a[0].value, b[0].value)
	}
	for _, av := range a {
		matched, comparable := false, false
		for _, bv := range b {
			if av.unit != bv.unit {
				continue
			}
			comparable = true // b asserts something in this family
			if floatsClose(av.value, bv.value) {
				matched = true
				break
			}
		}
		if comparable && !matched {
			return false
		}
	}
	return true
}

// floatsClose returns true when two magnitudes are equal up to a small
// relative tolerance. Strict equality is wrong (1.0 != 1.000001 from
// unit conversion) but generous tolerance hides real disagreements;
// 0.5% covers conversion rounding without swallowing meaningful diffs.
func floatsClose(a, b float64) bool {
	if a == b {
		return true
	}
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	mag := a
	if b > mag {
		mag = b
	}
	if mag < 0 {
		mag = -mag
	}
	if mag == 0 {
		return diff < 1e-9
	}
	return diff/mag < 0.005
}

// detectNumericDivergence returns true when two claims share substantial
// non-numeric content (so they're plausibly about the same thing) but
// disagree on at least one numeric value.
//
// Runs after the polarity / value-divergence paths so it only kicks in
// when those didn't already classify the pair.
func detectNumericDivergence(aText, bText string, aTokens, bTokens map[string]struct{}) bool {
	aNums := extractNumerics(aText)
	bNums := extractNumerics(bText)
	// Bail before building the word-token sets, exactly as the single-function
	// version did. Keeping the wrapper's evaluation ORDER faithful matters
	// beyond taste: the benchmark that establishes the "before" number calls
	// through here, and eagerly building two maps the original never built
	// would flatter the change.
	if len(aNums) == 0 || len(bNums) == 0 {
		return false
	}
	return numericDivergesPre(aNums, wordTokens(aTokens), bNums, wordTokens(bTokens))
}

// numericDivergesPre is detectNumericDivergence with the per-claim derivations
// (the numeric literals and the word-token subset) supplied by the caller.
//
// Both derivations are functions of one claim, but the pairwise loop used to
// recompute all four of them for every pair — including a regex scan of both
// full texts. Against an N-claim corpus that is 2N regex scans per new claim to
// produce at most a handful of edges. Hoisting them lets the incremental path
// derive each claim's numerics once.
func numericDivergesPre(aNums []numericValue, aWords map[string]struct{}, bNums []numericValue, bWords map[string]struct{}) bool {
	if len(aNums) == 0 || len(bNums) == 0 {
		return false
	}

	// Require non-numeric topical overlap so we don't flag every pair
	// of sentences that happen to contain different numbers.
	//
	// This has to be measured on WORD tokens only. The tokenizer keeps
	// numerals and bare symbols ("9", "269", "→") as content tokens, so
	// measuring overlap over the raw sets let the numbers themselves stand in
	// for topical agreement — the exact thing this guard exists to prevent.
	// "269 → 9" tokenizes to [9, 269, →]; against a compose-file claim
	// containing "9 → 6" it shared {9, →}, scoring 0.67 and clearing the 0.5
	// bar on nothing but a digit and an arrow.
	overlap := contentOverlap(aWords, bWords)
	if overlap < 1 {
		return false
	}
	shorter := min(len(aWords), len(bWords))
	if shorter == 0 {
		return false
	}
	// Same short-claim blind spot the polarity path had: a two-token claim
	// trivially scores 1.0 against the shorter length. Require the overlap to
	// also be a real share of the LONGER claim.
	longer := max(len(aWords), len(bWords))
	if longer == 0 || float64(overlap)/float64(longer) < minContradictionCoverage {
		return false
	}
	ratio := float64(overlap) / float64(shorter)
	// Stricter than the supports-overlap threshold: numeric
	// disagreement is a strong claim, so pairs must share most of
	// their topical surface (excluding the numbers themselves).
	if ratio < 0.5 {
		return false
	}

	return !numericValuesAgree(aNums, bNums)
}

// wordTokens returns the subset of tokens that contain at least one letter.
//
// The tokenizer treats numerals and bare symbols as content tokens, which is
// right for most purposes but wrong when the question is "are these two claims
// about the same thing?". A shared "9" or "→" says nothing about topic, so any
// same-topic guard must measure agreement over words alone.
func wordTokens(tokens map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(tokens))
	for tok := range tokens {
		for _, r := range tok {
			if unicode.IsLetter(r) {
				out[tok] = struct{}{}
				break
			}
		}
	}
	return out
}

// isLocator reports whether the number matched at idx names a PLACE rather
// than asserting a QUANTITY. `render.go:133` is a line, `imap.example:993` a
// port, `76-77` a line range: two of them differing means they point at
// different places, not that they disagree about a measurement. Same
// reasoning as the `#225`/`#257` exclusion above, extended to the other
// punctuation that binds a number to an identifier.
//
// idx is the START OF THE MATCH, which for this regex is the separator
// character itself (the pattern opens with `(?:^|[^A-Za-z0-9_])`) — not the
// first digit. Reading text[idx-1] instead looks one character too far left
// and never fires.
//
// Two guards keep it off real measurements. A recognized unit vetoes it, since
// locators never carry units, so `timeout:30s` stays a measurement. And
// the colon must sit tight against an identifier character: "timeout: 30"
// (colon, space) is a label introducing a value, which is how measurements
// are ordinarily written.
func isLocator(text string, idx int, unitSuffix string) bool {
	// Only a RECOGNIZED unit vetoes the exclusion. A guessed word does not:
	// "render.go:133 writes the header" would otherwise keep 133 as a quantity
	// of "writes".
	if _, known := unitFamily[strings.ToLower(strings.TrimSpace(unitSuffix))]; known {
		return false
	}
	if idx <= 0 || idx >= len(text) {
		return false
	}
	sep := text[idx]
	prev := text[idx-1]
	switch sep {
	case ':':
		// `file.go:133`, `host:993`.
		return prev == '.' || prev == '_' || prev == '/' || isASCIIAlnum(prev)
	case '-':
		// `76-77` — a range continuation, only when a digit leads the dash.
		return prev >= '0' && prev <= '9'
	}
	return false
}

func isASCIIAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
