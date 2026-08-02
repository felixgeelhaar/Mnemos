package brainbench

import (
	"fmt"
	"math"
)

// Direction is a metric's normative orientation: which way is an improvement.
type Direction string

const (
	// HigherBetter — an increase is an improvement.
	HigherBetter Direction = "higher_is_better"
	// LowerBetter — a decrease is an improvement.
	LowerBetter Direction = "lower_is_better"
	// Descriptive — the metric has NO defensible direction and the harness
	// refuses to score it.
	//
	// This category is load-bearing. Forgetting mechanically shrinks the claim
	// count and mechanically raises mean trust (it deletes the low-trust tail).
	// A harness that scored "fewer claims" or "higher mean trust" as wins would
	// report an improvement for a process that did nothing but delete
	// knowledge, and would report it for EVERY forgetting configuration,
	// including one that deleted everything. Those numbers are still worth
	// showing — they are how a reader notices a brain was gutted — so they are
	// reported and explicitly not scored.
	Descriptive Direction = "descriptive"
)

// Metric is one measured quantity on one arm.
//
// Known is not a formality. A metric the harness cannot compute defensibly —
// calibration with no resolved predictions, a forbidden-hit rate when no probe
// declares a forbidden string — must not be emitted as 0.0, because 0.0 reads
// as "perfect" and would silently inflate the treatment's score. Unknown
// carries the reason instead.
type Metric struct {
	Name      string    `json:"name"`
	Direction Direction `json:"direction"`
	Unit      string    `json:"unit,omitempty"`

	Known bool    `json:"known"`
	Value float64 `json:"value,omitempty"`

	// Unknown is the human-readable reason the value could not be computed.
	// Set if and only if Known is false.
	Unknown string `json:"unknown_reason,omitempty"`

	// N is the sample size behind a rate (probe count, claim count). 0 means
	// not applicable. It exists so a reader can compute the metric's
	// resolution: a rate over 6 probes cannot move by less than 1/6, so a
	// 0.17 delta is one probe flipping, not a trend.
	N int `json:"n,omitempty"`
}

// KnownMetric builds a computed metric.
func KnownMetric(name string, dir Direction, value float64, n int, unit string) Metric {
	return Metric{Name: name, Direction: dir, Unit: unit, Known: true, Value: value, N: n}
}

// UnknownMetric builds a metric the harness declined to compute, with the
// reason it declined.
func UnknownMetric(name string, dir Direction, reason string) Metric {
	return Metric{Name: name, Direction: dir, Known: false, Unknown: reason}
}

// MetricSet is one arm's full measurement, ordered for stable reporting.
type MetricSet struct {
	Metrics []Metric `json:"metrics"`
}

// Get returns the named metric.
func (ms MetricSet) Get(name string) (Metric, bool) {
	for _, m := range ms.Metrics {
		if m.Name == name {
			return m, true
		}
	}
	return Metric{}, false
}

// Names returns every metric name in order.
func (ms MetricSet) Names() []string {
	out := make([]string, 0, len(ms.Metrics))
	for _, m := range ms.Metrics {
		out = append(out, m.Name)
	}
	return out
}

// Verdict is the comparison outcome for one metric between the two arms.
type Verdict string

const (
	// VerdictBetter — moved in the improving direction.
	VerdictBetter Verdict = "better"
	// VerdictWorse — moved in the degrading direction. Reported exactly as
	// loudly as VerdictBetter.
	VerdictWorse Verdict = "worse"
	// VerdictUnchanged — no movement beyond the float-comparison epsilon.
	VerdictUnchanged Verdict = "unchanged"
	// VerdictChanged — a Descriptive metric moved. The harness reports the
	// movement and declines to call it good or bad.
	VerdictChanged Verdict = "changed"
	// VerdictUnknown — at least one arm could not be measured, so no
	// comparison is possible. Never silently treated as unchanged.
	VerdictUnknown Verdict = "unknown"
)

// deltaEpsilon is the float-comparison floor. Metrics are counts and ratios of
// small integers, so anything below this is representation noise, not movement.
const deltaEpsilon = 1e-9

// Comparison is one metric compared across the control and treatment arms.
type Comparison struct {
	Name      string    `json:"name"`
	Direction Direction `json:"direction"`
	Unit      string    `json:"unit,omitempty"`
	N         int       `json:"n,omitempty"`

	Control   Metric  `json:"control"`
	Treatment Metric  `json:"treatment"`
	Delta     float64 `json:"delta,omitempty"`
	Verdict   Verdict `json:"verdict"`

	// Note carries a caveat that must travel with the number, such as the
	// resolution floor of a small-n rate.
	Note string `json:"note,omitempty"`
}

// Compare diffs one metric across arms.
//
// The unknown rule is deliberately pessimistic: if EITHER arm is unknown the
// comparison is unknown. The tempting alternative — treat a missing control as
// 0 and score the treatment against it — manufactures improvements out of
// measurement gaps.
func Compare(control, treatment Metric) Comparison {
	c := Comparison{
		Name:      control.Name,
		Direction: control.Direction,
		Unit:      control.Unit,
		N:         control.N,
		Control:   control,
		Treatment: treatment,
	}
	if c.Name == "" {
		c.Name = treatment.Name
	}
	if c.Direction == "" {
		c.Direction = treatment.Direction
	}
	if c.N == 0 {
		c.N = treatment.N
	}
	if !control.Known || !treatment.Known {
		c.Verdict = VerdictUnknown
		return c
	}

	c.Delta = treatment.Value - control.Value
	switch {
	case math.Abs(c.Delta) <= deltaEpsilon:
		c.Delta = 0
		c.Verdict = VerdictUnchanged
	case c.Direction == Descriptive:
		c.Verdict = VerdictChanged
	case c.Direction == HigherBetter:
		if c.Delta > 0 {
			c.Verdict = VerdictBetter
		} else {
			c.Verdict = VerdictWorse
		}
	case c.Direction == LowerBetter:
		if c.Delta < 0 {
			c.Verdict = VerdictBetter
		} else {
			c.Verdict = VerdictWorse
		}
	default:
		c.Verdict = VerdictUnknown
	}

	// A rate over n items cannot express a change of fewer than 1/n items. A
	// delta at or below that is one item moving — or, for a mean-of-reciprocals
	// like MRR, a re-ranking deeper in the list that flipped no probe from miss
	// to hit at all. Either way it is not a trend, and on a suite this small the
	// distinction is the difference between a finding and a rounding artefact.
	if c.Verdict != VerdictUnchanged && c.N > 0 && c.Unit == "ratio" {
		res := 1 / float64(c.N)
		if math.Abs(c.Delta) <= res+deltaEpsilon {
			c.Note = fmt.Sprintf(
				"delta (%+.4f) is within this metric's 1/n resolution (%.4f over n=%d): at most one item moved, not a trend",
				c.Delta, res, c.N)
		}
	}
	return c
}

// CompareSets diffs two arms metric-by-metric, preserving the control's order.
// A metric present in only one arm is compared against an explicit unknown
// rather than dropped, so an asymmetry is visible instead of invisible.
func CompareSets(control, treatment MetricSet) []Comparison {
	out := make([]Comparison, 0, len(control.Metrics))
	seen := make(map[string]struct{}, len(control.Metrics))
	for _, cm := range control.Metrics {
		seen[cm.Name] = struct{}{}
		tm, ok := treatment.Get(cm.Name)
		if !ok {
			tm = UnknownMetric(cm.Name, cm.Direction, "not measured on the treatment arm")
		}
		out = append(out, Compare(cm, tm))
	}
	for _, tm := range treatment.Metrics {
		if _, ok := seen[tm.Name]; ok {
			continue
		}
		cm := UnknownMetric(tm.Name, tm.Direction, "not measured on the control arm")
		out = append(out, Compare(cm, tm))
	}
	return out
}

// Summary counts comparison outcomes.
type Summary struct {
	Better    int `json:"better"`
	Worse     int `json:"worse"`
	Unchanged int `json:"unchanged"`
	// Changed counts Descriptive metrics that moved — movement the harness
	// reports but refuses to score.
	Changed int `json:"changed_descriptive"`
	Unknown int `json:"unknown"`
}

// Summarize tallies a comparison list.
func Summarize(cs []Comparison) Summary {
	var s Summary
	for _, c := range cs {
		switch c.Verdict {
		case VerdictBetter:
			s.Better++
		case VerdictWorse:
			s.Worse++
		case VerdictUnchanged:
			s.Unchanged++
		case VerdictChanged:
			s.Changed++
		default:
			s.Unknown++
		}
	}
	return s
}
