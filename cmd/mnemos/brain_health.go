package main

import (
	"context"
	"fmt"

	mnemos "go.klarlabs.de/mnemos"
)

// handleBrainHealth runs the read-only brain-health verdict (ADR 0019): a single
// healthy/degraded/unhealthy rollup of the cognitive vitals (free-energy, calibration,
// dissonance, low-trust, staleness) and structural-integrity checks (orphan beliefs,
// dangling edges, stale-expectation backlog). `--journal` also records the snapshot to
// the cognitive journal so vital signs become a time series (read with `journal --kind health`).
//
//	mnemos health [--human] [--journal] [--explain]
func handleBrainHealth(args []string, f Flags) {
	journal, explain := false, false
	for _, a := range args {
		switch a {
		case "--journal":
			journal = true
		case "--explain":
			explain = true
		default:
			exitWithMnemosError(false, NewUserError("unknown argument %q", a))
			return
		}
	}
	// --explain is a human-readable guide; it implies the human view.
	if explain {
		f.Human = true
	}
	ctx := context.Background()
	mem, err := newLibraryMemory(ctx, "")
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "open store"))
		return
	}
	defer func() { _ = mem.Close() }()

	var health mnemos.BrainHealth
	if journal {
		health, err = mem.SnapshotHealth(ctx)
	} else {
		health, err = mem.BrainHealth(ctx)
	}
	if err != nil {
		exitWithMnemosError(f.Verbose, err)
		return
	}

	if f.Human {
		printBrainHealthHuman(health, explain)
		return
	}
	emitJSON(health)
}

func printBrainHealthHuman(h mnemos.BrainHealth, explain bool) {
	fmt.Printf("Brain health: %s\n\n", h.Status)
	fmt.Println("  vitals")
	for _, v := range h.Vitals {
		fmt.Printf("    %-8s %-12s %6.3f   %s\n", healthMark(v.Status), v.Name, v.Value, v.Detail)
	}
	fmt.Println("  integrity")
	for _, p := range h.Pathologies {
		fmt.Printf("    %-8s %-12s %6d   %s\n", healthMark(p.Status), p.Kind, p.Count, p.Detail)
	}
	if explain {
		printBrainHealthExplain()
		return
	}
	fmt.Println("\n  Run 'mnemos health --explain' for what each line means.")
}

// printBrainHealthExplain describes the vitals and integrity checks in plain
// language, so the report is self-documenting instead of pointing a reader at an
// internal design doc. The ADR is mentioned once, at the end, for the curious.
func printBrainHealthExplain() {
	fmt.Print(`
  what these mean
    A brain is healthy when its model of the world keeps prediction error low: it
    is not surprised by what it later observes, its confidence matches its accuracy,
    and it does not hold beliefs that flatly contradict each other. The verdict is
    worst-wins — the single worst line sets the overall status, so one sick vital is
    never hidden by healthy neighbours.

    vitals — each is 0..1, higher is worse, with its own warn and critical line
      free_energy   aggregate prediction-error signal, and a pointer to the layer
                    most wrong. The headline number; the rest explain it.
      calibration   when the brain says it is 80% sure, is it right about 80% of the
                    time? Confidently wrong is worse than unsure.
      dissonance    active, high-stakes contradictions per belief — conflicts between
                    beliefs established enough that a disagreement means something.
      low_trust     share of currently-valid beliefs whose trust has decayed below a
                    floor as their evidence ages without re-verification.
      staleness     share of valid beliefs not seen or re-verified within the horizon
                    — "still load-bearing, or just still present?"
      trust_decay   share of still-trusted beliefs that fall below the same floor
                    within a month if nobody re-verifies them, each decaying at its
                    own half-life. low_trust is what has already gone; this is what
                    is going. A month of re-verification work, named in advance.
      skill_coverage share of recorded actions whose outcome was ever observed — an
                    action with no outcome teaches the brain nothing.

    A vital marked [ ?? ] is unknown: nothing has been measured for it yet. That
    is not a pass. It never worsens the overall verdict either — absence of
    evidence of a problem is not evidence of one.

    integrity — structural smell tests, not cognitive ones
      orphan_claims       beliefs with zero evidence; every belief should trace to a
                          source, so these should not exist.
      dangling_edges      relationships pointing at a belief that is gone.
      stale_expectations  predictions the brain made and never reconciled.

    Thresholds ship as conservative defaults and are being calibrated against real
    snapshots — run 'mnemos health --journal' to add to that series. Full design
    rationale lives in the repo (docs/adr/0019).
`)
}

// healthMark renders a status. Unknown gets its own marker rather than falling
// into the ok branch: "we did not measure this" read as "[ ok ]" is the exact
// failure the unknown status was introduced to end, and the human view is where
// most people read the report.
func healthMark(s mnemos.HealthStatus) string {
	switch s {
	case mnemos.HealthUnhealthy:
		return "[CRIT]"
	case mnemos.HealthDegraded:
		return "[warn]"
	case mnemos.HealthUnknown:
		return "[ ?? ]"
	default:
		return "[ ok ]"
	}
}
