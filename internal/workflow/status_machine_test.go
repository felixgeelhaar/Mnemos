package workflow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"testing"
)

// TestDeclaredStatusesMatchTheMachine keeps DeclaredStatuses honest by parsing
// the State(...) literals out of the machine itself.
//
// Without this the list is just a second copy of the truth, and a second copy
// drifts — which is the whole failure being guarded against one level up: a
// status that looks declared, compiles, and does not exist.
func TestDeclaredStatusesMatchTheMachine(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "status_machine.go", nil, 0)
	if err != nil {
		t.Fatalf("parse status_machine.go: %v", err)
	}

	inMachine := map[string]bool{}

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		// Both `State("x")` on the builder and a bare State("x") read the same
		// here; only the callee name matters.
		name := ""
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		case *ast.Ident:
			name = fn.Name
		}
		if name != "State" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if s, err := strconv.Unquote(lit.Value); err == nil {
			inMachine[s] = true
		}

		return true
	})

	if len(inMachine) == 0 {
		t.Fatal("found no State(\"...\") literals — the parser stopped matching the machine's shape, " +
			"so this test would pass vacuously")
	}

	declared := map[string]bool{}
	for _, s := range DeclaredStatuses() {
		declared[s] = true
	}

	for s := range inMachine {
		if !declared[s] {
			t.Errorf("state %q exists in the machine but is missing from DeclaredStatuses()", s)
		}
	}
	for s := range declared {
		if !inMachine[s] {
			t.Errorf("DeclaredStatuses() lists %q but the machine has no such state — "+
				"any command setting it fails at runtime", s)
		}
	}

	if t.Failed() {
		t.Logf("machine=%v declared=%v", sortedKeys(inMachine), sortedKeys(declared))
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)

	return out
}
