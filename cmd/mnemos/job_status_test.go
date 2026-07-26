package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.klarlabs.de/mnemos/internal/workflow"
)

// TestJobStatusesAreDeclaredInTheMachine fails if any command sets a job status
// the workflow state machine does not know.
//
// # WHY A SOURCE-LEVEL TEST
//
// `mnemos quality` called job.SetStatus("computing", ""), and "computing" is not
// a state in internal/workflow/status_machine.go — not a missing transition, an
// entirely undeclared state. So the command failed on its FIRST action, every
// time, for everyone, with:
//
//	invalid workflow status transition: running -> computing
//
// A message that names neither the command nor the fix. The command had never
// worked, and nothing caught it: the status is a bare string literal, so the
// compiler is happy, and no test exercised the one line that fails.
//
// This is the general defect, not that one typo. Statuses are strings passed to
// a machine that validates at runtime, so every new SetStatus call is a fresh
// chance to invent a state and only discover it when a user runs the command.
// Walking the AST turns that into a build-time failure.
//
// If this fires you have two honest options: use one of the existing states
// (they cover load / extract / relate / save / embed / query), or add the new
// state to the machine WITH its transitions. Do not add it to an allowlist here.
func TestJobStatusesAreDeclaredInTheMachine(t *testing.T) {
	declared := workflow.DeclaredStatuses()
	if len(declared) == 0 {
		t.Fatal("no declared statuses returned; the guard would pass vacuously")
	}

	valid := make(map[string]bool, len(declared))
	for _, s := range declared {
		valid[s] = true
	}

	// Guard the guard: if the machine ever stops reporting the states this test
	// exists to check against, it must fail loudly rather than pass on an empty
	// set.
	for _, must := range []string{"running", "querying", "completed", "failed"} {
		if !valid[must] {
			t.Fatalf("declared statuses %v are missing %q — the machine or its reporter changed shape", declared, must)
		}
	}

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SetStatus" || len(call.Args) == 0 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				// A computed status cannot be checked here; that is a reason not to
				// compute them, but not something this test can catch.
				return true
			}
			status, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if !valid[status] {
				t.Errorf("%s: SetStatus(%q) — no such state in the workflow machine, so this call fails at "+
					"runtime on every invocation. Use one of %v, or declare the state with its transitions.",
					fset.Position(lit.Pos()), status, declared)
			}

			return true
		})
	}
}
