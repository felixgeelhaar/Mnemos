package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoUnredactedDSNInMessages fails if any user-facing message in this package
// interpolates a DSN-shaped variable without routing it through a redactor.
//
// # WHY A SOURCE-LEVEL TEST
//
// v0.116.1 fixed "mnemos serve prints the database password on startup" by
// redacting that one boot line. It did not fix the class. Nine other sites —
// including `failed to open database at %q`, the path most likely to end up in
// an incident report — still interpolated the raw DSN, so a live Postgres
// password was printed into `kubectl logs` by a failing job. The success path
// was carefully redacted; the failure paths, where credentials actually travel,
// were not.
//
// Redaction applied by hand per call site is redaction that gets forgotten at
// the next call site, and the symptom only appears when something is already
// going wrong. A unit test of the redactor itself cannot catch that — it only
// proves the function works, not that anyone called it. This walks the AST
// instead and fails on the omission.
//
// If this test fires: wrap the argument in store.RedactDSN (or displayDSN,
// which resolves and redacts). Do not add the identifier to an allowlist unless
// you can show the value can never carry a password.
func TestNoUnredactedDSNInMessages(t *testing.T) {
	// Calls that render text a human or a log aggregator will see.
	rendering := map[string]bool{
		"Errorf": true, "Printf": true, "Sprintf": true, "Fprintf": true,
		"NewSystemError": true, "NewUserError": true, "Println": true, "Fatalf": true,
	}
	// Wrappers that make a DSN safe to print.
	safe := map[string]bool{
		"RedactDSN": true, "redactDSN": true, "displayDSN": true,
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

		file, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		// A DSN legitimately reaches output in one place: writing the real
		// connection string into a config file. Those sites opt out explicitly,
		// on the line, with a reason — an allowlist of identifiers would go
		// stale silently, whereas a comment sits where the next reader looks.
		exempt := map[int]bool{}
		for _, group := range file.Comments {
			for _, c := range group.List {
				if strings.Contains(c.Text, exemptMarker) {
					exempt[fset.Position(c.Pos()).Line] = true
				}
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !rendering[calleeName(call.Fun)] {
				return true
			}
			for _, arg := range call.Args {
				ident, ok := arg.(*ast.Ident)
				if !ok || !looksLikeDSN(ident.Name) {
					continue
				}
				pos := fset.Position(ident.Pos())
				if exempt[pos.Line] {
					continue
				}
				t.Errorf("%s: %s(..., %s) interpolates a DSN without redaction — "+
					"wrap it in store.RedactDSN. A DSN reaching a message is a password "+
					"reaching a log. If this site must emit the real DSN (writing a "+
					"config file), add a %q comment on the line with a reason.",
					pos, calleeName(call.Fun), ident.Name, exemptMarker)
			}

			return true
		})
	}

	// Guard the guard: if the detector stops recognising the shape it is
	// looking for, it silently passes forever.
	if !looksLikeDSN("dsn") || !looksLikeDSN("globalDSN") || looksLikeDSN("conn") {
		t.Fatal("looksLikeDSN no longer matches the identifiers this test exists to catch")
	}
	if !safe["RedactDSN"] {
		t.Fatal("the safe-wrapper set is empty or renamed; every call would be reported")
	}
}

// exemptMarker opts a single line out of the DSN-redaction check. Reserved for
// sites that must emit the real connection string (writing it into a config
// file), never for "this one is probably fine".
const exemptMarker = "dsn-redaction-ok"

// calleeName returns the function name of a call target, ignoring the receiver
// (so both Errorf and fmt.Errorf report as "Errorf").
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}

	return ""
}

// looksLikeDSN reports whether an identifier names a connection string. Matches
// `dsn`, `globalDSN`, `baseDSN`, `revocationDSN`… but not unrelated names.
func looksLikeDSN(name string) bool {
	return strings.HasSuffix(name, "DSN") || strings.EqualFold(name, "dsn")
}
