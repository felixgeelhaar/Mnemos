// Package sourceguard reads what the code actually does — from its AST —
// so that a hand-maintained list which has drifted from it is a test
// failure instead of a silent dead feature.
//
// Why this exists: this repository keeps several lists that a human must
// remember to update when they change the code beside them, and nothing
// checks them. Two had already drifted when this package was written:
//
//   - `mcpExecutorMap` in cmd/mnemos/axikernel.go keys executors by
//     `exec.<tool>`. The v0.85.0 brain-native rename renamed the tools
//     (`list_claims`→`list_beliefs`, `list_contradictions`→
//     `list_dissonances`, `remember_event`→`remember_episode`) and not
//     the executor keys, so three MCP tools — one of them a write —
//     dispatched to an executor that was never registered and failed
//     with `-32603` in every release for three weeks (#341, fixed by
//     #343). Every test passed throughout: the tools were registered as
//     kernel actions, they appeared in `tools/list`, and the only way to
//     find out was to call one.
//   - `commands` in cmd/mnemos/ux.go calls itself "the full set of
//     top-level commands" and drives typo correction. Six dispatched
//     commands were missing from it, including `health`, the vitals
//     entry point — so `mnemos helth` suggested nothing.
//
// Both are the same defect class as the projection drift #340 guards in
// internal/store/schemaguard: a hand-maintained list, no failing path,
// and a symptom nobody attributes to the list. The set-comparison
// semantics are literally that package's — [Check] and [Result] are its
// [schemaguard.Check] and [schemaguard.Result] — so "an entry with an
// empty reason excuses nothing" and "a satisfied allowance fails as
// stale" mean exactly the same thing in both guards, from one
// implementation. Only the vocabulary differs: names, not columns.
//
// The extraction here is AST-based, never regex, and that is
// load-bearing rather than fastidious. A `case "x":` regex over
// cmd/mnemos/main.go misses `case "verify", "reconsolidate":` — the
// multi-value clause at main.go:293 — and reports `reconsolidate` as a
// command that does not exist. A guard that invents drift gets disabled
// by the first person it wastes an hour of, so [SwitchCaseStrings] reads
// `ast.CaseClause.List`, which is a slice, and the problem does not
// arise. For the same reason both extractors return an error rather than
// a partial result when they meet something they cannot read (a
// non-literal case expression, a dispatch whose tool name is a
// variable): a construct the guard silently skips is a construct drift
// can hide in.
package sourceguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"go.klarlabs.de/mnemos/internal/store/schemaguard"
)

// Result is the outcome of one comparison. Aliased from schemaguard so
// the two guard families cannot drift in their notion of what a stale
// allowance is.
type Result = schemaguard.Result

// Check reports which of want is absent from got, excusing the names in
// allow (name -> reason). An entry with an empty reason excuses nothing,
// and an entry that no longer excuses anything is reported in
// [Result.StaleAllowances] — an excuse must not outlive the thing it
// excuses, or it disarms the guard for that name the next time it goes
// missing.
func Check(want, got []string, allow map[string]string) Result {
	return schemaguard.Check(want, got, allow)
}

// CallStringArgs returns the distinct string literals passed at position
// argIndex to every call of funcName in the non-test Go files of dir.
//
// Generic calls are matched through their instantiation, so
// `dispatchAxiTool[mcpQueryOutput](ctx, k, nil, "query_knowledge", in)`
// is found by funcName "dispatchAxiTool" — the callee of such a call is
// an ast.IndexExpr (or ast.IndexListExpr for several type arguments)
// wrapping the identifier, not the identifier itself.
//
// Test files are excluded deliberately: the guard's subject is the
// shipped surface, and a name a test dispatches is not a name users can
// reach.
//
// Two situations are errors rather than a quietly shorter list, because
// both would make a caller's comparison vacuously green: finding no call
// at all (the function was renamed, or dir is wrong), and a call whose
// argIndex is not a string literal (a computed name the guard cannot
// follow — and therefore a name that could go missing unnoticed).
func CallStringArgs(dir, funcName string, argIndex int) ([]string, error) {
	if argIndex < 0 {
		return nil, fmt.Errorf("sourceguard: argIndex %d is negative", argIndex)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("sourceguard: read %s: %w", dir, err)
	}

	fset := token.NewFileSet()
	var found []string
	var parseErr error

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("sourceguard: parse %s: %w", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || calleeName(call.Fun) != funcName {
				return true
			}
			if len(call.Args) <= argIndex {
				parseErr = fmt.Errorf("sourceguard: %s: call to %s has %d args, want > %d",
					fset.Position(call.Pos()), funcName, len(call.Args), argIndex)
				return false
			}
			lit, err := stringLiteral(call.Args[argIndex])
			if err != nil {
				parseErr = fmt.Errorf("sourceguard: %s: argument %d of %s: %w",
					fset.Position(call.Args[argIndex].Pos()), argIndex, funcName, err)
				return false
			}
			found = append(found, lit)
			return true
		})
		if parseErr != nil {
			return nil, parseErr
		}
	}

	if len(found) == 0 {
		return nil, fmt.Errorf("sourceguard: no call to %s found in %s — "+
			"the guard would compare against an empty set", funcName, dir)
	}
	return dedupe(found), nil
}

// SwitchCaseStrings returns the distinct string literals of every case
// clause of the switch on the identifier tag in the Go file at path.
//
// A multi-value clause (`case "verify", "reconsolidate":`) contributes
// every one of its values: ast.CaseClause.List is a slice, which is the
// whole reason this reads the AST instead of matching `case "..."` with
// a regex. The default clause (List == nil) contributes nothing.
//
// Exactly one switch must have that tag. Zero means the guard has
// nothing to compare against; more than one means the guard cannot know
// which is the dispatcher, and guessing is how a guard starts reporting
// names that are not commands. A case expression that is not a string
// literal is an error for the same reason as in [CallStringArgs].
func SwitchCaseStrings(path, tag string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("sourceguard: parse %s: %w", path, err)
	}

	var switches []*ast.SwitchStmt
	ast.Inspect(file, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		if ident, ok := sw.Tag.(*ast.Ident); ok && ident.Name == tag {
			switches = append(switches, sw)
		}
		return true
	})

	switch len(switches) {
	case 1:
	case 0:
		return nil, fmt.Errorf("sourceguard: no switch on %q in %s — "+
			"the guard would compare against an empty set", tag, path)
	default:
		return nil, fmt.Errorf("sourceguard: %d switches on %q in %s — "+
			"the guard cannot tell which one dispatches", len(switches), tag, path)
	}

	var found []string
	for _, stmt := range switches[0].Body.List {
		clause, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		for _, expr := range clause.List {
			lit, err := stringLiteral(expr)
			if err != nil {
				return nil, fmt.Errorf("sourceguard: %s: case of switch on %q: %w",
					fset.Position(expr.Pos()), tag, err)
			}
			found = append(found, lit)
		}
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("sourceguard: switch on %q in %s has no string cases", tag, path)
	}
	return dedupe(found), nil
}

// calleeName returns the identifier a call expression resolves to,
// unwrapping generic instantiation (`f[T]`, `f[T, U]`) and package
// qualification (`pkg.f`). Anything else — a call through a variable, a
// method value on an expression — returns "" and simply does not match.
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	case *ast.IndexExpr:
		return calleeName(f.X)
	case *ast.IndexListExpr:
		return calleeName(f.X)
	default:
		return ""
	}
}

// stringLiteral unquotes an expression that must be an untyped string
// literal. A constant reference or concatenation is refused rather than
// resolved: the guard's value is that the name it compares is the name
// in the source, verbatim.
func stringLiteral(expr ast.Expr) (string, error) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", fmt.Errorf("not a string literal (%T) — the guard cannot follow it, "+
			"so drift could hide behind it", expr)
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", fmt.Errorf("unquote %s: %w", lit.Value, err)
	}
	return s, nil
}

// dedupe returns the distinct values of in, sorted, so a guard's failure
// message reads the same on every machine.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
