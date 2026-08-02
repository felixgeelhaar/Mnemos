package main

import (
	"sort"
	"strings"
	"testing"

	"go.klarlabs.de/mnemos/internal/kernel"
	"go.klarlabs.de/mnemos/internal/sourceguard"
)

// Guard: the three hand-maintained lists that wire an MCP tool to the
// code that runs it must name the same tools.
//
// A tool needs three separate mentions to work: an entry in `mcpTools()`
// (the kernel action), a `dispatchAxiTool(..., "<tool>", ...)` call site
// (the MCP handler), and an `exec.<tool>` key in `mcpExecutorMap()` (the
// implementation). Nothing connects them but the string, and nothing
// checked that the strings agree.
//
// What that cost: the v0.85.0 brain-native rename (2026-07-12) renamed
// the tools and the actions and NOT the executor keys. `list_beliefs`,
// `list_dissonances` and `remember_episode` — one of them a write —
// dispatched to executors that were never registered, failed with
// `action executor not registered`, and returned a sanitised `-32603` to
// the agent. They were dead in every release for three weeks (#341).
// Every existing test stayed green: the tools were registered actions,
// they listed in `tools/list`, they carried the right scopes, and the
// only way to discover the defect was to call one against a real brain.
//
// This guard was written against the drifted tree (86b5813, before
// #343) and failed there exactly as intended:
//
//	MCP tools dispatched with no registered executor:
//	  [list_beliefs list_dissonances remember_episode]
//	registered executors no MCP tool dispatches to:
//	  [list_claims list_contradictions remember_event]
//
// The pair of directions is what identifies the cause: three missing
// AND three orphaned is a rename that stopped half way, where three
// missing and none orphaned would be an omission. #343 renamed the
// three keys, so it is green here — the guard's job now is that the
// next such rename fails on the commit that makes it, rather than three
// weeks later against a real brain. Its failure behaviour stays pinned
// in internal/sourceguard, which runs the same comparison over a
// synthetic half-finished rename.

// executorAllowlist excuses an `exec.<name>` key that is deliberately
// registered without any MCP tool dispatching to it. It is empty: every
// executor this binary registers today is reachable, and the three that
// currently are not are the #341 drift, not a decision.
//
// An entry needs a reason; an empty one excuses nothing. An entry that
// stops being needed fails the guard as stale, so an excuse cannot
// outlive the thing it excuses — the pattern #340 set in
// internal/store/schemaguard.
var executorAllowlist = map[string]string{}

// dispatchAllowlist excuses a dispatched tool name that deliberately has
// no `exec.` executor. Empty for the same reason: a dispatch without an
// executor is a tool that cannot run.
var dispatchAllowlist = map[string]string{}

// dispatchedToolNames are the tool names the MCP handlers actually send
// through the kernel, read from the `dispatchAxiTool` call sites in this
// package's non-test sources.
func dispatchedToolNames(t *testing.T) []string {
	t.Helper()
	names, err := sourceguard.CallStringArgs(".", "dispatchAxiTool", 3)
	if err != nil {
		t.Fatalf("read dispatchAxiTool call sites: %v", err)
	}
	return names
}

// registeredExecutorNames are the bare tool names of the executor map,
// with the `exec.` prefix that [kernel.ExecutorRef] adds stripped back
// off. Read at runtime from the real map rather than parsed, so it is
// the binding the kernel is built with. The nil watcher factory is never
// called: the map only closes over it.
func registeredExecutorNames(t *testing.T) []string {
	t.Helper()
	execs := mcpExecutorMap("guard", nil)
	if len(execs) == 0 {
		t.Fatal("mcpExecutorMap is empty — the guard would compare against an empty set")
	}
	names := make([]string, 0, len(execs))
	for ref := range execs {
		bare, ok := strings.CutPrefix(ref, "exec.")
		if !ok {
			t.Errorf("executor key %q does not use the exec. prefix — kernel.ExecutorRef "+
				"builds the binding as %q, so this executor can never be found", ref,
				kernel.ExecutorRef("<name>"))
			continue
		}
		names = append(names, bare)
	}
	sort.Strings(names)
	return names
}

// The #341 direction: a tool the handler dispatches with no executor
// behind it. Every call fails at runtime, and only at runtime.
func TestMCPExecutors_EveryDispatchedToolHasAnExecutor(t *testing.T) {
	res := sourceguard.Check(dispatchedToolNames(t), registeredExecutorNames(t), dispatchAllowlist)
	if len(res.Missing) > 0 {
		t.Errorf("MCP tools dispatched with no registered executor: %v\n"+
			"Every call to these fails with `action executor not registered`, sanitised "+
			"to -32603 — the tool is dead. Add an `exec.<name>` key to mcpExecutorMap in "+
			"axikernel.go (this is #341; #343 fixes it).", res.Missing)
	}
	if len(res.StaleAllowances) > 0 {
		t.Errorf("dispatchAllowlist entries that no longer excuse anything: %v\n"+
			"Delete them — a stale excuse disarms the guard for that tool.", res.StaleAllowances)
	}
}

// The other direction, and the one that names the cause. An executor
// nothing dispatches to is dead weight on its own; an orphan that pairs
// with a missing executor of a different name is a rename that stopped
// half way, which is what #341 was.
func TestMCPExecutors_EveryExecutorIsDispatched(t *testing.T) {
	res := sourceguard.Check(registeredExecutorNames(t), dispatchedToolNames(t), executorAllowlist)
	if len(res.Missing) > 0 {
		t.Errorf("registered executors no MCP tool dispatches to: %v\n"+
			"Either a tool was renamed and its executor key was not (check the missing "+
			"names reported by TestMCPExecutors_EveryDispatchedToolHasAnExecutor for the "+
			"other half of the rename), or the executor is dead code. Rename it, delete "+
			"it, or add an executorAllowlist entry saying why it is registered "+
			"unreachable.", res.Missing)
	}
	if len(res.StaleAllowances) > 0 {
		t.Errorf("executorAllowlist entries that no longer excuse anything: %v", res.StaleAllowances)
	}
}

// The third list. A dispatched name with no `mcpTools()` action never
// reaches an executor at all — axi rejects the invocation before
// dispatch — so this catches the same defect one layer earlier, and an
// unreachable action is a tool advertised in `tools/list` that nothing
// can call.
func TestMCPExecutors_ActionsAndDispatchAgree(t *testing.T) {
	actions := make([]string, 0, len(mcpTools()))
	for _, a := range mcpTools() {
		actions = append(actions, a.Name)
	}
	sort.Strings(actions)
	dispatched := dispatchedToolNames(t)

	if res := sourceguard.Check(dispatched, actions, nil); len(res.Missing) > 0 {
		t.Errorf("tools dispatched through the kernel with no mcpTools() action: %v\n"+
			"axi rejects the invocation before it reaches an executor.", res.Missing)
	}
	if res := sourceguard.Check(actions, dispatched, nil); len(res.Missing) > 0 {
		t.Errorf("mcpTools() actions nothing dispatches: %v\n"+
			"They are advertised to agents and unreachable.", res.Missing)
	}
}
