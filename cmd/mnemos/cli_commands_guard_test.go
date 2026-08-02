package main

import (
	"testing"

	"go.klarlabs.de/mnemos/internal/sourceguard"
)

// Guard: `commands` in ux.go calls itself "the full set of top-level
// commands" and must actually be that.
//
// It has one job — typo correction — and a command missing from it is
// invisible to discovery: `mnemos helth` suggested nothing, because
// `health` was not in the list. Six dispatched commands had drifted out
// of it by this commit (`health`, `journal`, `predictive-error`,
// `classify-durability`, `float-back`, `global`), all six documented in
// `printUsage`, all six added here. Nothing failed, because the list has
// no correctness consequence: the dispatcher reads the switch, not this
// slice, so a missing entry only degrades the error message of a user
// who is already lost.
//
// The failure this guard produces is demonstrated rather than asserted:
// the real lists are in sync as of this commit, so
// TestCheck_ReportsCommandsMissingFromTheHandMaintainedList in
// internal/sourceguard runs the same comparison over a synthetic drifted
// list and pins the message. See that package for why the extraction is
// AST-based — `case "verify", "reconsolidate":` at main.go:293 is a
// multi-value clause, and the regex that reads only its first value
// reports `reconsolidate` as a phantom command.

// undispatchedCommands excuses an entry of `commands` that deliberately
// dispatches nowhere — a retired name kept so typo correction can still
// point at it, say. Empty today: every entry is a live case in the
// dispatch switch.
//
// An entry needs a reason; an empty one excuses nothing, and an entry
// that stops being needed fails the guard as stale, so an excuse cannot
// outlive the thing it excuses (the #340 pattern in
// internal/store/schemaguard).
var undispatchedCommands = map[string]string{}

// unlistedCommands excuses a dispatched command deliberately absent from
// `commands`. Empty today. Note what would NOT belong here: `hook`, the
// internal Claude Code hook handler, is listed and should be — a user
// who mistypes it deserves the suggestion as much as anyone, and hiding
// a command from typo correction hides nothing else.
var unlistedCommands = map[string]string{}

// dispatchedCommands are the command names main()'s dispatch switch
// actually accepts.
func dispatchedCommands(t *testing.T) []string {
	t.Helper()
	names, err := sourceguard.SwitchCaseStrings("main.go", "command")
	if err != nil {
		t.Fatalf("read the main.go dispatch switch: %v", err)
	}
	return names
}

// A dispatched command missing from `commands` is a command the CLI
// runs and cannot suggest.
func TestCLICommands_ListsEveryDispatchedCommand(t *testing.T) {
	res := sourceguard.Check(dispatchedCommands(t), commands, unlistedCommands)
	if len(res.Missing) > 0 {
		t.Errorf("commands dispatched by main.go and absent from the `commands` list: %v\n"+
			"They are invisible to suggestCommand, so a user who mistypes one gets no "+
			"suggestion. Add them to `commands` in ux.go (and to printUsage), or add an "+
			"unlistedCommands entry saying why the command should stay unsuggestable.",
			res.Missing)
	}
	if len(res.StaleAllowances) > 0 {
		t.Errorf("unlistedCommands entries that no longer excuse anything: %v\n"+
			"Delete them — a stale excuse disarms the guard for that command.",
			res.StaleAllowances)
	}
}

// The other direction: an entry that dispatches nowhere makes typo
// correction suggest a command the CLI will then reject as unknown,
// which is worse than no suggestion. It is also how a removed command
// leaves a trace behind.
func TestCLICommands_EveryListedCommandDispatches(t *testing.T) {
	res := sourceguard.Check(commands, dispatchedCommands(t), undispatchedCommands)
	if len(res.Missing) > 0 {
		t.Errorf("entries of the `commands` list that main.go does not dispatch: %v\n"+
			"suggestCommand will offer these, and running one prints `unknown command`. "+
			"Remove them from ux.go, or add an undispatchedCommands entry saying why the "+
			"name is worth suggesting anyway.", res.Missing)
	}
	if len(res.StaleAllowances) > 0 {
		t.Errorf("undispatchedCommands entries that no longer excuse anything: %v",
			res.StaleAllowances)
	}
}
