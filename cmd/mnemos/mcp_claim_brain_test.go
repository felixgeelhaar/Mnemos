package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/mcp/protocol"
	mnemos "go.klarlabs.de/mnemos"
)

// Regression cover for #341: query_knowledge federates the global brain with
// the repo/workspace overlay, so an agent routinely holds a belief id that
// exists ONLY in the workspace brain. Every by-id tool used to look in the
// global brain alone and fail with an opaque -32603, leaving a stale workspace
// belief readable but uncorrectable.

// workspaceBrains sets up the exact shape the issue describes: a global brain,
// an opted-in workspace below $HOME, and a working directory inside it — so
// mcpRepoBrainDSN() resolves the overlay the same way the MCP server does from
// the directory Claude Code spawned it in.
func workspaceBrains(t *testing.T) (globalDSN, repoDSN string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("MNEMOS_URL", "") // local, not hosted

	proj := filepath.Join(home, "proj")
	if err := os.MkdirAll(filepath.Join(proj, ".mnemos"), 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	globalDSN = "sqlite://" + filepath.Join(home, "global.db")
	repoDSN = "sqlite://" + filepath.Join(proj, ".mnemos", "mnemos.db")
	t.Setenv("MNEMOS_DB_URL", globalDSN)
	t.Chdir(proj)

	if got := mcpRepoBrainDSN(); got != repoDSN {
		t.Fatalf("fixture wrong: mcpRepoBrainDSN() = %q, want %q", got, repoDSN)
	}
	return globalDSN, repoDSN
}

// rememberIn seeds a belief into one specific brain and returns its id.
func rememberIn(t *testing.T, dsn, text string) string {
	t.Helper()
	out, err := mcpRunRemember(withDSNOverride(context.Background(), dsn), "test", mcpRememberInput{
		Text: text, RunID: "run-341",
	})
	if err != nil {
		t.Fatalf("seed %q into %s: %v", text, dsn, err)
	}
	return out.ClaimID
}

// statusIn reports a belief's status in one specific brain, and whether the
// brain holds the row at all. The "holds the row at all" half is what catches a
// write landing in the wrong brain as a phantom insert.
func statusIn(t *testing.T, dsn, claimID string) (status string, present bool) {
	t.Helper()
	ctx := withDSNOverride(context.Background(), dsn)
	conn, err := openConn(ctx)
	if err != nil {
		t.Fatalf("open %s: %v", dsn, err)
	}
	defer closeConn(conn)
	rows, err := conn.Claims.ListByIDs(ctx, []string{claimID})
	if err != nil {
		t.Fatalf("ListByIDs in %s: %v", dsn, err)
	}
	if len(rows) == 0 {
		return "", false
	}
	return string(rows[0].Status), true
}

// A belief that lives only in the workspace brain must resolve to the workspace
// brain — the whole defect in #341.
func TestMCPScopeToClaimBrain_ResolvesWorkspaceOnlyBelief(t *testing.T) {
	_, repoDSN := workspaceBrains(t)
	id := rememberIn(t, repoDSN, "amoxicillin dose was superseded")

	ctx, brain, err := mcpScopeToClaimBrain(context.Background(), id)
	if err != nil {
		t.Fatalf("workspace belief did not resolve: %v", err)
	}
	if brain.Tier != claimBrainRepo || brain.DSN != repoDSN {
		t.Fatalf("resolved to %+v, want the workspace brain %q", brain, repoDSN)
	}
	if got, _ := dsnOverrideFromContext(ctx); got != repoDSN {
		t.Fatalf("returned context points at %q, want the workspace brain", got)
	}
}

// A global belief keeps resolving to the global brain: the fix must not drag
// existing single-brain traffic into the overlay.
func TestMCPScopeToClaimBrain_GlobalBeliefStaysGlobal(t *testing.T) {
	globalDSN, _ := workspaceBrains(t)
	id := rememberIn(t, globalDSN, "the global brain holds this one")

	ctx, brain, err := mcpScopeToClaimBrain(context.Background(), id)
	if err != nil {
		t.Fatalf("global belief did not resolve: %v", err)
	}
	if brain.Tier != claimBrainGlobal || brain.DSN != "" {
		t.Fatalf("resolved to %+v, want the global brain", brain)
	}
	if _, ok := dsnOverrideFromContext(ctx); ok {
		t.Fatal("global tier must not pin a brain override")
	}
}

// An id in no reachable brain is a scoping answer, not a server fault. It must
// come back as a structured NOT FOUND (-32001, message preserved by the MCP
// boundary) rather than the -32603 that made #341 hard to characterise.
func TestMCPScopeToClaimBrain_UnknownIDIsStructuredNotFound(t *testing.T) {
	workspaceBrains(t)

	_, _, err := mcpScopeToClaimBrain(context.Background(), "cl_no_such_belief")
	if err == nil {
		t.Fatal("unknown belief id resolved successfully")
	}
	var perr *protocol.Error
	if !errors.As(err, &perr) {
		t.Fatalf("error is %T (%v); an unknown id must be a protocol error, not a bare error that the MCP boundary turns into -32603", err, err)
	}
	if perr.Code != protocol.CodeNotFound {
		t.Errorf("code = %d, want %d (not found)", perr.Code, protocol.CodeNotFound)
	}
	if !strings.Contains(perr.Message, "cl_no_such_belief") {
		t.Errorf("message %q should name the belief that was not found", perr.Message)
	}
}

// The consequence that matters: an agent must be able to deprecate a stale
// workspace belief, and the write must land in the WORKSPACE brain. A write
// aimed at the global brain either does nothing or leaves a phantom row that no
// read path ever reconciles with the original.
func TestMCPForget_WritesToTheBrainHoldingTheBelief(t *testing.T) {
	globalDSN, repoDSN := workspaceBrains(t)
	id := rememberIn(t, repoDSN, "cats tolerate paracetamol")

	out, err := mcpRunForget(context.Background(), "agent", mcpForgetInput{
		ClaimID: id, Reason: "contradicted by newer evidence",
	})
	if err != nil {
		t.Fatalf("forget a workspace belief: %v", err)
	}
	if out.NewStatus != "deprecated" {
		t.Errorf("NewStatus = %q, want deprecated", out.NewStatus)
	}

	if status, present := statusIn(t, repoDSN, id); !present || status != "deprecated" {
		t.Errorf("workspace brain: status=%q present=%v, want deprecated", status, present)
	}
	if _, present := statusIn(t, globalDSN, id); present {
		t.Error("the global brain gained a phantom row for a workspace belief")
	}
}

func TestMCPMemoryDeprecate_WritesToTheBrainHoldingTheBelief(t *testing.T) {
	globalDSN, repoDSN := workspaceBrains(t)
	id := rememberIn(t, repoDSN, "the old clinical-safety guidance")

	if _, err := mcpRunMemoryDeprecate(context.Background(), "agent", mcpMemoryDeprecateInput{
		ClaimID: id, Reason: "superseded",
	}); err != nil {
		t.Fatalf("deprecate a workspace belief: %v", err)
	}
	if status, present := statusIn(t, repoDSN, id); !present || status != "deprecated" {
		t.Errorf("workspace brain: status=%q present=%v, want deprecated", status, present)
	}
	if _, present := statusIn(t, globalDSN, id); present {
		t.Error("the global brain gained a phantom row for a workspace belief")
	}
}

func TestMCPMemoryResolveDissonance_WritesToTheBrainHoldingBothBeliefs(t *testing.T) {
	globalDSN, repoDSN := workspaceBrains(t)
	winner := rememberIn(t, repoDSN, "the current dosing guidance")
	loser := rememberIn(t, repoDSN, "the superseded dosing guidance")

	if _, err := mcpRunMemoryResolve(context.Background(), "agent", mcpMemoryResolveInput{
		WinnerID: winner, LoserID: loser, Reason: "newer evidence",
	}); err != nil {
		t.Fatalf("resolve a workspace dissonance: %v", err)
	}
	if status, _ := statusIn(t, repoDSN, winner); status != "resolved" {
		t.Errorf("winner status = %q, want resolved", status)
	}
	if status, _ := statusIn(t, repoDSN, loser); status != "deprecated" {
		t.Errorf("loser status = %q, want deprecated", status)
	}
	for _, id := range []string{winner, loser} {
		if _, present := statusIn(t, globalDSN, id); present {
			t.Errorf("the global brain gained a phantom row for workspace belief %s", id)
		}
	}
}

// Two beliefs held by different brains cannot be resolved against each other:
// the write would have to span two stores. Refuse explicitly rather than pick
// one brain and silently drop the other side.
func TestMCPMemoryResolveDissonance_RefusesBeliefsInDifferentBrains(t *testing.T) {
	globalDSN, repoDSN := workspaceBrains(t)
	winner := rememberIn(t, repoDSN, "workspace-side belief")
	loser := rememberIn(t, globalDSN, "global-side belief")

	_, err := mcpRunMemoryResolve(context.Background(), "agent", mcpMemoryResolveInput{
		WinnerID: winner, LoserID: loser,
	})
	if err == nil {
		t.Fatal("a cross-brain resolve was accepted")
	}
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.CodeInvalidParams {
		t.Fatalf("error = %v (%T); want a structured invalid-params, not an opaque internal error", err, err)
	}
}

// get_belief is the read half of the same defect: the tool an agent reaches for
// first when recall surfaces something it distrusts.
func TestMCPGetClaim_ReadsAWorkspaceBelief(t *testing.T) {
	_, repoDSN := workspaceBrains(t)
	id := rememberIn(t, repoDSN, "workspace-only belief text")

	memFor := func(_ context.Context, b claimBrain) (mnemos.Memory, error) {
		dsn := b.DSN
		if dsn == "" {
			dsn = resolveDSN()
		}
		return mnemos.New(mnemos.WithStorage(dsn), mnemos.WithPassiveMode())
	}
	out, err := mcpRunGetClaim(context.Background(), memFor, mcpGetClaimInput{ClaimID: id})
	if err != nil {
		t.Fatalf("get a workspace belief: %v", err)
	}
	if out.ID != id || out.Statement != "workspace-only belief text" {
		t.Fatalf("got %+v, want the workspace belief", out)
	}
}

func TestMCPGetClaim_UnknownIDIsStructuredNotFound(t *testing.T) {
	workspaceBrains(t)
	memFor := func(_ context.Context, _ claimBrain) (mnemos.Memory, error) {
		return mnemos.New(mnemos.WithStorage(resolveDSN()), mnemos.WithPassiveMode())
	}
	_, err := mcpRunGetClaim(context.Background(), memFor, mcpGetClaimInput{ClaimID: "cl_nope"})
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.CodeNotFound {
		t.Fatalf("error = %v (%T); want a structured not-found", err, err)
	}
}

// byIDBeliefTools is the full set of MCP handlers that take a belief id and act
// on it. Every one of them must resolve the owning brain first — fixing only
// the four tools named in #341 would leave the identical trap in the rest, and
// "which by-id tool works against a workspace?" is exactly the question that
// made the bug expensive to characterise.
var byIDBeliefTools = []string{
	"mcpRunGetClaim",
	"mcpRunForget",
	"mcpRunUpdate",
	"mcpRunMemoryDeprecate",
	"mcpRunMemoryResolve",
	"mcpRunMemoryEscalate",
	"mcpRunMemoryPromote",
	"mcpRunRecordExpectation",
	"mcpRunRecordObservation",
	"mcpRunRecordFeedback",
}

// A static guard, because the failure mode is silent: a by-id tool that forgets
// to resolve the brain still compiles, still passes its own happy-path test
// against a single-brain fixture, and only breaks for users who have a
// workspace.
func TestByIDBeliefTools_ResolveTheOwningBrain(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob sources: %v", err)
	}
	fset := token.NewFileSet()
	found := map[string]bool{}
	scopes := map[string]bool{}
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			name := fn.Name.Name
			found[name] = true
			ast.Inspect(fn, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == "mcpScopeToClaimBrain" {
					scopes[name] = true
				}
				return true
			})
		}
	}
	for _, name := range byIDBeliefTools {
		if !found[name] {
			t.Errorf("%s no longer exists — update byIDBeliefTools", name)
			continue
		}
		if !scopes[name] {
			t.Errorf("%s takes a belief id but never calls mcpScopeToClaimBrain: it will only ever reach the global brain (#341)", name)
		}
	}
}
