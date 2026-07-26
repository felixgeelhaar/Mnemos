package query

import (
	"os"
	"strings"
)

// The cognitive retrieval behaviours (ADR 0013 §2/§4, ADR 0015 §4/§5, ADR 0016)
// are ON unless explicitly disabled.
//
// # WHY ON BY DEFAULT
//
// They are not optional enhancements; they are the memory model. Spreading
// activation primes associations, salience biases toward stakes, Hebbian
// strengthens what fires together, reconsolidation keeps recalled memories
// alive against decay, and inhibition suppresses a contradiction's beaten
// loser. A store that does none of these is a similarity search over rows, not
// a brain — and a brain that never reinforces what it retrieves is the one
// thing a memory system must not be.
//
// Three of them WRITE during a read, which is why they were originally opt-in.
// That instinct is HTTP orthodoxy applied where it does not belong: in a memory
// system retrieval IS a write — that is the testing effect, and the entire
// point of reconsolidation. The writes are bounded and best-effort by
// construction, they are tenant-scoped like every other write, and backends
// that cannot persist edge strength skip them. Nothing about "GET" makes
// forgetting correct.
//
// Opting out stays available per-variable, because a genuinely read-only
// deployment (a replica, a forensic copy, a migration dry-run) is a real thing:
//
//	MNEMOS_HEBBIAN=false        # or 0 / no / off
//
// # WHAT THIS FIXES
//
// These options were previously set in exactly ONE place — the CLI `query`
// command. MCP (`query_knowledge`, `recall`), REST (`/v1/search`) and the
// embedded store all constructed AnswerOptions without them, so every field sat
// at its zero value. No env var or config could change that: the code path
// simply never assigned the fields. The features were implemented, tested and
// documented, and unreachable from every surface anyone actually uses.
const (
	envSpreadingActivation = "MNEMOS_SPREADING_ACTIVATION"
	envSalience            = "MNEMOS_SALIENCE"
	envHebbian             = "MNEMOS_HEBBIAN"
	envReconsolidate       = "MNEMOS_RECONSOLIDATE"
	envInhibit             = "MNEMOS_INHIBIT"
)

// cognitiveEnabled reports whether a retrieval behaviour is on.
//
// Unset means ON. Only an explicit, recognised falsy value turns it off — an
// unparseable value is treated as ON rather than silently disabling a core
// behaviour on a typo, because the failure mode of "quietly stopped learning"
// has no symptom to notice.
func cognitiveEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// WithCognitiveDefaults fills the five retrieval behaviours from the
// environment, leaving every other field untouched. Call it on every
// AnswerOptions built on a production read path; TestAnswerOptionsApplyCognitiveDefaults
// fails the build if a construction site forgets.
//
// A field already set to true stays true — a caller's explicit `--hebbian`
// cannot be undone by an env var that merely fails to mention it.
func (o AnswerOptions) WithCognitiveDefaults() AnswerOptions {
	o.Prime = o.Prime || cognitiveEnabled(envSpreadingActivation)
	o.Salient = o.Salient || cognitiveEnabled(envSalience)
	o.Hebbian = o.Hebbian || cognitiveEnabled(envHebbian)
	o.Reconsolidate = o.Reconsolidate || cognitiveEnabled(envReconsolidate)
	o.Inhibit = o.Inhibit || cognitiveEnabled(envInhibit)

	return o
}
