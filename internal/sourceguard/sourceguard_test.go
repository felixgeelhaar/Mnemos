package sourceguard_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/mnemos/internal/sourceguard"
)

// writePkg writes files into a fresh directory and returns its path.
func writePkg(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The multi-value case clause is the reason this package parses instead
// of matching text. `case "verify", "reconsolidate":` is a real clause in
// cmd/mnemos/main.go, and a `case "([a-z-]+)"` regex sees only "verify" —
// which makes "reconsolidate" look like an entry in the `commands` list
// that dispatches nowhere. That is a guard reporting drift it invented,
// and the cost is not a wasted hour: it is that the guard gets deleted.
func TestSwitchCaseStrings_ReadsEveryValueOfAMultiValueCase(t *testing.T) {
	dir := writePkg(t, map[string]string{"main.go": `package main

func main() {
	command := "x"
	switch command {
	case "ingest":
	case "verify", "reconsolidate":
	case "health":
	default:
	}
}
`})

	got, err := sourceguard.SwitchCaseStrings(filepath.Join(dir, "main.go"), "command")
	if err != nil {
		t.Fatalf("SwitchCaseStrings: %v", err)
	}
	want := []string{"health", "ingest", "reconsolidate", "verify"}
	if !equal(got, want) {
		t.Errorf("SwitchCaseStrings = %v, want %v (the second value of the multi-value "+
			"clause is the one a regex drops)", got, want)
	}
}

// The CLI guard, exercised against a synthetic drift. The real
// cmd/mnemos list is (as of this commit) in sync, so this is where the
// guard's failure behaviour is demonstrated rather than merely asserted:
// a dispatched command absent from the list must be reported, and it
// must be reported by name — a guard that says "the lists differ" leaves
// the reader to diff 58 strings by eye.
func TestCheck_ReportsCommandsMissingFromTheHandMaintainedList(t *testing.T) {
	dir := writePkg(t, map[string]string{"main.go": `package main

func main() {
	command := "x"
	switch command {
	case "ingest":
	case "verify", "reconsolidate":
	case "health":
	case "journal":
	}
}
`})

	dispatched, err := sourceguard.SwitchCaseStrings(filepath.Join(dir, "main.go"), "command")
	if err != nil {
		t.Fatalf("SwitchCaseStrings: %v", err)
	}
	// The drifted list: `health` and `journal` shipped without anyone
	// updating it — exactly the state cmd/mnemos/ux.go was in before this
	// commit, reproduced small enough to assert on.
	drifted := []string{"ingest", "verify", "reconsolidate"}

	res := sourceguard.Check(dispatched, drifted, nil)
	if !equal(res.Missing, []string{"health", "journal"}) {
		t.Errorf("Check().Missing = %v, want [health journal]", res.Missing)
	}
}

// The MCP executor guard, exercised against a synthetic half-finished
// rename — the shape of #341, where the tools were renamed and their
// `exec.` executor keys were not. Both directions must report, and the
// pair is the diagnosis: three dispatched-with-no-executor AND three
// registered-with-no-dispatch is a rename, where the first list alone
// would be an omission. Pinned here because the real tree is in sync
// since #343, so this is where the failure stays demonstrable.
func TestCheck_ReportsAHalfFinishedRenameInBothDirections(t *testing.T) {
	dir := writePkg(t, map[string]string{"mcp.go": `package main

func register() {
	dispatch[Out](ctx, k, nil, "query_knowledge", in)
	dispatch[Out](ctx, k, nil, "list_beliefs", in)
	dispatch[Out](ctx, k, nil, "remember_episode", in)
}
`})

	dispatched, err := sourceguard.CallStringArgs(dir, "dispatch", 3)
	if err != nil {
		t.Fatalf("CallStringArgs: %v", err)
	}
	// The executor keys the rename left behind, minus their `exec.`
	// prefix.
	registered := []string{"query_knowledge", "list_claims", "remember_event"}

	res := sourceguard.Check(dispatched, registered, nil)
	if !equal(res.Missing, []string{"list_beliefs", "remember_episode"}) {
		t.Errorf("dispatched-with-no-executor = %v, want [list_beliefs remember_episode] "+
			"(every call to these fails with `action executor not registered`)", res.Missing)
	}

	res = sourceguard.Check(registered, dispatched, nil)
	if !equal(res.Missing, []string{"list_claims", "remember_event"}) {
		t.Errorf("registered-with-no-dispatch = %v, want [list_claims remember_event] "+
			"(the orphans are the other half of the rename)", res.Missing)
	}
}

// A reason-bearing allowlist entry excuses a name; the same entry once
// the name stops needing it is itself a failure. Asserted here because
// this package delegates to schemaguard: if that delegation is ever
// replaced by a local copy, the stale-allowance semantics must not
// quietly weaken with it.
func TestCheck_AllowlistExcusesAndGoesStale(t *testing.T) {
	res := sourceguard.Check([]string{"a", "b"}, []string{"a"}, map[string]string{
		"b": "deliberately absent, for a stated reason",
	})
	if len(res.Missing) != 0 || len(res.StaleAllowances) != 0 {
		t.Errorf("a reasoned allowance should excuse: Missing=%v Stale=%v",
			res.Missing, res.StaleAllowances)
	}

	// Same allowlist, but "b" is present now: the excuse has outlived
	// what it excused.
	res = sourceguard.Check([]string{"a", "b"}, []string{"a", "b"}, map[string]string{
		"b": "deliberately absent, for a stated reason",
	})
	if !equal(res.StaleAllowances, []string{"b"}) {
		t.Errorf("StaleAllowances = %v, want [b]", res.StaleAllowances)
	}

	// An entry with no reason excuses nothing.
	res = sourceguard.Check([]string{"a", "b"}, []string{"a"}, map[string]string{"b": "  "})
	if !equal(res.Missing, []string{"b"}) {
		t.Errorf("an empty reason must not excuse: Missing = %v, want [b]", res.Missing)
	}
}

func TestCallStringArgs_FindsGenericAndPlainCalls(t *testing.T) {
	dir := writePkg(t, map[string]string{
		"a.go": `package main

func main() {
	dispatch[Out](ctx, k, nil, "query_knowledge", in)
	dispatch[In, Out](ctx, k, nil, "process_text", in)
	dispatch(ctx, k, nil, "list_beliefs", in)
	other(ctx, k, nil, "not_a_tool", in)
}
`,
		// Excluded: a name only a test dispatches is not a name a user
		// can reach, so counting it would excuse real drift.
		"a_test.go": `package main

func TestX() {
	dispatch[Out](ctx, k, nil, "only_in_tests", in)
}
`,
	})

	got, err := sourceguard.CallStringArgs(dir, "dispatch", 3)
	if err != nil {
		t.Fatalf("CallStringArgs: %v", err)
	}
	want := []string{"list_beliefs", "process_text", "query_knowledge"}
	if !equal(got, want) {
		t.Errorf("CallStringArgs = %v, want %v", got, want)
	}
}

// Every way of finding nothing is an error, never an empty slice. An
// empty "what the code does" set makes the caller's comparison vacuously
// green — the guard reports success precisely when it has stopped
// working.
func TestExtractors_FailLoudlyRatherThanVacuously(t *testing.T) {
	t.Run("no call sites", func(t *testing.T) {
		dir := writePkg(t, map[string]string{"a.go": "package main\n\nfunc main() {}\n"})
		if _, err := sourceguard.CallStringArgs(dir, "dispatch", 3); err == nil {
			t.Fatal("want an error when the function is never called")
		}
	})

	t.Run("computed tool name", func(t *testing.T) {
		dir := writePkg(t, map[string]string{"a.go": `package main

func main() { dispatch(ctx, k, nil, toolName, in) }
`})
		_, err := sourceguard.CallStringArgs(dir, "dispatch", 3)
		if err == nil || !strings.Contains(err.Error(), "not a string literal") {
			t.Fatalf("want a not-a-string-literal error, got %v", err)
		}
	})

	t.Run("no such switch", func(t *testing.T) {
		dir := writePkg(t, map[string]string{"main.go": "package main\n\nfunc main() {}\n"})
		if _, err := sourceguard.SwitchCaseStrings(filepath.Join(dir, "main.go"), "command"); err == nil {
			t.Fatal("want an error when no switch has that tag")
		}
	})

	t.Run("ambiguous switch", func(t *testing.T) {
		dir := writePkg(t, map[string]string{"main.go": `package main

func main() {
	command := "x"
	switch command {
	case "a":
	}
	switch command {
	case "b":
	}
}
`})
		_, err := sourceguard.SwitchCaseStrings(filepath.Join(dir, "main.go"), "command")
		if err == nil || !strings.Contains(err.Error(), "cannot tell which one") {
			t.Fatalf("want an ambiguity error, got %v", err)
		}
	})

	t.Run("non-literal case", func(t *testing.T) {
		dir := writePkg(t, map[string]string{"main.go": `package main

const cmdIngest = "ingest"

func main() {
	command := "x"
	switch command {
	case cmdIngest:
	}
}
`})
		_, err := sourceguard.SwitchCaseStrings(filepath.Join(dir, "main.go"), "command")
		if err == nil || !strings.Contains(err.Error(), "not a string literal") {
			t.Fatalf("want a not-a-string-literal error, got %v", err)
		}
	})
}

// A nested switch inside a case body belongs to that body, not to the
// dispatcher, and its cases are not commands. Reading only the
// dispatcher's own CaseClauses is what keeps flag values like "--run"
// out of the command set.
func TestSwitchCaseStrings_IgnoresNestedSwitches(t *testing.T) {
	dir := writePkg(t, map[string]string{"main.go": `package main

func main() {
	command := "x"
	arg := "y"
	switch command {
	case "query":
		switch arg {
		case "--run":
		case "--hops":
		}
	case "health":
	}
}
`})

	got, err := sourceguard.SwitchCaseStrings(filepath.Join(dir, "main.go"), "command")
	if err != nil {
		t.Fatalf("SwitchCaseStrings: %v", err)
	}
	if !equal(got, []string{"health", "query"}) {
		t.Errorf("SwitchCaseStrings = %v, want [health query]", got)
	}
}
