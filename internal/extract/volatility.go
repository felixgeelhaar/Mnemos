package extract

import (
	"regexp"
	"strings"
)

// Claim volatility: how fast a claim's truth decays with age.
//
// Every claim mnemos captures gets the same 90-day freshness half-life
// (trust.FreshnessHalfLifeDays), because HalfLifeDays is read everywhere —
// trust scoring, staleness, curiosity, query — and set nowhere except explicit
// human verification. So a claim about what is currently installed recalls at
// the same confidence on day 30 as a claim about why a design decision was
// made.
//
// That is the mechanism behind a whole class of stale-belief incidents: "we're
// not using warden", "briefkasten is an HTTP MCP server, not stdio", "mnemos
// doesn't work". Each was TRUE when written. None was wrong at write time —
// they were assertions about mutable system state that kept recalling at full
// confidence long after the state moved.
//
// The classifier below only shortens the half-life, and only on a confident
// signal. That direction is deliberate. Mis-classifying a durable claim as
// volatile decays real knowledge out of recall — an invisible loss. Missing a
// volatile claim just leaves today's behaviour. When unsure, do nothing.

// VolatileHalfLifeDays is the freshness half-life for claims about mutable
// system state. Roughly a fortnight: long enough that a claim stays useful
// across a working sprint, short enough that a month-old assertion about what
// is installed or running no longer recalls as settled fact.
const VolatileHalfLifeDays = 14.0

// VolatilityClassifierVersion identifies the revision of HalfLifeFor that
// produced a stored verdict, and is written to Belief.HalfLifeClassifier
// whenever this classifier assigns a half-life (ADR 0025).
//
// Bump it on any change to the lexicons, the veto or the normaliser. That is
// the whole point: a stored verdict records which revision decided, so rows an
// older version looked at can be found and revisited. Without it, widening the
// classifier — which volatility.go says outright is expected, since it
// deliberately under-catches — has no way to target the rows that need
// reclassifying, and the only available action is to reprocess the entire
// corpus forever.
//
// The `<classifier>/<version>` shape mirrors extract.PromptVersion, which
// exists for the same reason one layer up: to know which revision of a
// text-processing rule produced a stored artefact.
const VolatilityClassifierVersion = "volatility/v1"

// systemStateRE matches assertions about mutable system state — what exists,
// what is installed or wired, what is running, what version is deployed.
//
// Each alternative requires a state verb, not merely a topic word, so
// "we chose Postgres because the write volume outgrew SQLite" (a decision, and
// durable) does not match while "postgres is running on port 5432" does.
var systemStateRE = regexp.MustCompile(`(?i)\b(?:` +
	// presence / absence in a codebase or system
	`(?:is|are|isn'?t|aren'?t|not)\s+(?:currently\s+)?(?:installed|present|available|enabled|disabled|configured|wired|set\s+up|deployed|running|up|down|live|active|passing|failing|broken|green|red)` +
	`|no\s+\w+\s+(?:anywhere|in\s+the\s+repo|installed|configured)` +
	`|(?:doesn'?t|does\s+not|don'?t)\s+(?:exist|work|run|build|compile|pass)` +
	`|(?:currently|now)\s+(?:on|at|using|running)\s+v?\d` +
	// version / port / endpoint state
	`|\bv\d+\.\d+\.\d+\b` +
	`|\bport\s+\d{2,5}\b` +
	`)`)

// durableRE matches claims whose value does not decay with system state:
// decisions, rationale, constraints and policy. A match here vetoes the
// volatile classification even when a state verb is also present, because
// "we chose X because Y is faster" is a decision that happens to describe
// state.
var durableRE = regexp.MustCompile(`(?i)\b(?:` +
	`decided|decision|chose|chosen|because|rationale|in\s+order\s+to|so\s+that|` +
	`must\s+never|must\s+always|policy|convention|by\s+design|deliberate(?:ly)?|` +
	`the\s+reason|trade-?off|we\s+prefer|prefers?\b` +
	`)`)

// HalfLifeFor returns the freshness half-life to stamp on a claim of the given
// type and text, or 0 to leave the store's default in place.
//
// Decisions are never treated as volatile regardless of wording: a decision's
// value is its reasoning, which stays true even after the thing it decided
// about changes.
func HalfLifeFor(claimType, text string) float64 {
	if strings.EqualFold(claimType, "decision") {
		return 0
	}
	t := strings.TrimSpace(text)
	if t == "" {
		return 0
	}
	t = normalizeForMatch(t)
	if durableRE.MatchString(t) {
		return 0
	}
	if systemStateRE.MatchString(t) {
		return VolatileHalfLifeDays
	}
	return 0
}

// normalizeForMatch folds away typography that otherwise defeats the lexicons.
//
// The classifiers key on words, but claim text carries the punctuation of real
// prose and code — curly apostrophes from editors, backticks around
// identifiers. Measured against a real brain, this fragility was most of the
// classifier's misses: "Mnemos doesn't work" (curly apostrophe) and
// "No `warden` anywhere" (backticks) both went unclassified, then recalled at
// 90-day freshness while being false. Same meaning, different bytes.
//
// This is deliberately ONLY typography. It does not widen what counts as a
// state claim: phrasings like "not using X" or "is an HTTP MCP server" are left
// unmatched on purpose, because "using" and "is a/an X" appear just as often in
// durable claims ("we're using Postgres", "mnemos is a local-first evidence
// layer"), and mis-classifying a durable claim as volatile decays real
// knowledge out of recall invisibly. Under-catching keeps the store default;
// over-catching loses knowledge silently.
func normalizeForMatch(text string) string {
	return matchNormalizer.Replace(text)
}

// matchNormalizer maps typographic variants to the ASCII the lexicons expect.
// Backticks become spaces so `warden` reads as a bare word rather than one
// fused to its quotes.
var matchNormalizer = strings.NewReplacer(
	"\u2019", "'", // right single quote
	"\u2018", "'", // left single quote
	"\u02bc", "'", // modifier letter apostrophe
	"`", " ",
)
