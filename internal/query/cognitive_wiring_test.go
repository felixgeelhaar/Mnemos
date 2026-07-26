package query_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// searchedDirs are the packages that build AnswerOptions on a production read
// path. Relative to internal/query.
var searchedDirs = []string{"../..", "../../cmd/mnemos"}

// TestAnswerOptionsApplyCognitiveDefaults fails if any production construction
// of query.AnswerOptions is not followed by .WithCognitiveDefaults().
//
// # WHY THIS EXISTS
//
// The cognitive behaviours (priming, salience, Hebbian strengthening,
// reconsolidation, inhibition) used to be set in exactly ONE of the five places
// AnswerOptions is built — the CLI. MCP, REST and the embedded store each
// constructed the struct with only the fields they cared about, so all five
// behaviours sat at their zero value on every surface anyone actually uses.
//
// Nothing surfaced it. A brain that silently stops strengthening associations
// returns results that look completely normal; there is no error, no empty
// response, no metric that moves. The features were implemented, tested,
// documented and ADR'd, and unreachable. The bug was not in any of them — it
// was in the four call sites that never mentioned them.
//
// A struct literal with optional fields cannot be made safe by the type system,
// so the guard is here instead: build AnswerOptions on a read path and you must
// say what happens to the cognitive fields.
//
// If this fires, append .WithCognitiveDefaults() to the literal. If the site is
// genuinely not a retrieval path (a test fixture, a projection), move it out of
// the scanned packages or mark the line `cognitive-defaults-ok: <reason>`.
func TestAnswerOptionsApplyCognitiveDefaults(t *testing.T) {
	const exemptMarker = "cognitive-defaults-ok"

	found := 0

	for _, dir := range searchedDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}

		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
				continue
			}

			path := filepath.Join(dir, name)
			fset := token.NewFileSet()

			file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}

			exempt := map[int]bool{}
			for _, g := range file.Comments {
				for _, c := range g.List {
					if strings.Contains(c.Text, exemptMarker) {
						exempt[fset.Position(c.Pos()).Line] = true
					}
				}
			}

			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isAnswerOptions(lit.Type) {
					return true
				}
				found++
				pos := fset.Position(lit.Pos())
				if exempt[pos.Line] || litHasCognitiveCall(file, lit) {
					return true
				}
				t.Errorf("%s: query.AnswerOptions built without .WithCognitiveDefaults() — this read path "+
					"gets no priming, no salience, no Hebbian strengthening, no reconsolidation and no "+
					"inhibition, silently.", pos)

				return true
			})
		}
	}

	// Guard the guard: if the detector stops recognising the literal, it passes
	// vacuously and the class it exists to catch returns unnoticed.
	if found == 0 {
		t.Fatal("no query.AnswerOptions literals found in the scanned packages — the detector no longer " +
			"matches the code it is meant to check")
	}
}

func isAnswerOptions(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)

	return ok && pkg.Name == "query" && sel.Sel.Name == "AnswerOptions"
}

// litHasCognitiveCall reports whether the literal is the receiver of a
// .WithCognitiveDefaults() call, i.e. `query.AnswerOptions{...}.WithCognitiveDefaults()`.
func litHasCognitiveCall(file *ast.File, lit *ast.CompositeLit) bool {
	hit := false

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "WithCognitiveDefaults" {
			return true
		}
		if inner, ok := sel.X.(*ast.CompositeLit); ok && inner == lit {
			hit = true
		}

		return true
	})

	return hit
}
