package query

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// AnswerForRunWithOptions used to call answerWithEvents directly, so
// opts.Hebbian / Reconsolidate / Inhibit — which WithCognitiveDefaults sets true
// on every production path — were read nowhere on run-scoped recall. `mnemos
// query --run`, MCP recall with a run id, REST /v1/search with a run id and the
// embedded store all silently never strengthened, never reconsolidated and never
// inhibited. These tests pin all three, plus a structural guard so a future
// entry point cannot skip the seam again.

func TestAnswerForRun_StrengthensCoactivation(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	const question = "alpha widget beta gamma"

	events := fakeEventRepo{events: []domain.Event{
		{ID: "ev_1", RunID: "run_1", Content: "alpha widget beta gamma delta", Timestamp: now},
	}}
	claims := make([]domain.Claim, 0, 4)
	for i := 0; i < 4; i++ {
		claims = append(claims, domain.Claim{
			ID: claimID(i), Text: claimID(i) + " content", Type: domain.ClaimTypeFact,
			Status: domain.ClaimStatusActive, Confidence: 0.8, CreatedAt: now,
		})
	}
	edges := map[string][]domain.Relationship{}
	edge("r_0_1", domain.RelationshipTypeSupports, "c0", "c1", edges)
	recorder := &[][]string{}
	engine := NewEngine(events, fakeClaimRepo{claims: claims}, fakeRelationshipRepo{rels: edges, strengthenedWith: recorder})

	// Off stays off on the run path too.
	if _, err := engine.AnswerForRunWithOptions(context.Background(), question, "run_1", AnswerOptions{}); err != nil {
		t.Fatalf("AnswerForRunWithOptions: %v", err)
	}
	if len(*recorder) != 0 {
		t.Fatalf("Hebbian off must not strengthen, got %d calls", len(*recorder))
	}

	if _, err := engine.AnswerForRunWithOptions(context.Background(), question, "run_1", AnswerOptions{Hebbian: true}); err != nil {
		t.Fatalf("AnswerForRunWithOptions: %v", err)
	}
	if len(*recorder) != 1 {
		t.Fatalf("run-scoped recall must strengthen co-activated edges exactly once, got %d calls", len(*recorder))
	}
	if set := (*recorder)[0]; !contains(set, "c0") || !contains(set, "c1") {
		t.Errorf("strengthen set %v should include the co-retrieved c0 and c1", set)
	}
}

func TestAnswerForRun_Reconsolidates(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	const question = "alpha widget beta gamma"

	events := fakeEventRepo{events: []domain.Event{
		{ID: "ev_1", RunID: "run_1", Content: "alpha widget beta gamma delta", Timestamp: now},
	}}
	claims := []domain.Claim{
		{ID: "c0", Text: "c0 content", Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive, Confidence: 0.8, CreatedAt: now},
		{ID: "c1", Text: "c1 content", Type: domain.ClaimTypeFact, Status: domain.ClaimStatusActive, Confidence: 0.8, CreatedAt: now},
	}
	recorder := &[]string{}
	engine := NewEngine(events,
		fakeClaimRepo{claims: claims, verifiedRecorder: recorder},
		fakeRelationshipRepo{rels: map[string][]domain.Relationship{}})

	if _, err := engine.AnswerForRunWithOptions(context.Background(), question, "run_1", AnswerOptions{}); err != nil {
		t.Fatalf("AnswerForRunWithOptions: %v", err)
	}
	if len(*recorder) != 0 {
		t.Fatalf("reconsolidation off must not re-verify, got %v", *recorder)
	}

	if _, err := engine.AnswerForRunWithOptions(context.Background(), question, "run_1", AnswerOptions{Reconsolidate: true}); err != nil {
		t.Fatalf("AnswerForRunWithOptions: %v", err)
	}
	if len(*recorder) == 0 {
		t.Fatal("run-scoped recall must reconsolidate the recalled beliefs")
	}
}

func TestAnswerForRun_InhibitsContradictionLoser(t *testing.T) {
	now := time.Now().UTC()
	events, claimRepo, relRepo := makeContradictingSetup(now, 0.92, 0.71)
	writes := map[string]map[string]float64{}
	claimRepo.creditWrites = &writes
	engine := NewEngine(events, claimRepo, relRepo)

	if _, err := engine.AnswerForRunWithOptions(context.Background(), "deployment strategy", "r",
		AnswerOptions{Consumer: domain.ConsumerAgent}); err != nil {
		t.Fatalf("AnswerForRunWithOptions: %v", err)
	}
	if len(writes) != 0 {
		t.Fatalf("inhibit off must not write, got %v", writes)
	}

	if _, err := engine.AnswerForRunWithOptions(context.Background(), "deployment strategy", "r",
		AnswerOptions{Consumer: domain.ConsumerAgent, Inhibit: true}); err != nil {
		t.Fatalf("AnswerForRunWithOptions: %v", err)
	}
	comps, ok := writes["cl_low"]
	if !ok {
		t.Fatalf("run-scoped recall must inhibit the beaten loser; writes=%v", writes)
	}
	if comps[domain.InhibitionComponentKey] <= 0 {
		t.Errorf("cl_low inhibition should be positive, got %v", comps[domain.InhibitionComponentKey])
	}
}

// TestAnswerEntryPointsApplyRetrievalWriteBacks is the structural guard. Every
// EXPORTED Engine method that hands a domain.Answer back to a caller must end at
// applyRetrievalWriteBacks — directly, or by delegating to another such method.
//
// The class this catches has no symptom: an entry point that skips the seam
// returns a completely normal-looking answer, it just quietly stops learning
// from the recall. That is exactly how AnswerForRunWithOptions drifted, after
// the same class had already been fixed once at the AnswerOptions construction
// sites (see TestAnswerOptionsApplyCognitiveDefaults, which guards the other
// half: options being SET). This guards the options being READ.
func TestAnswerEntryPointsApplyRetrievalWriteBacks(t *testing.T) {
	const writeBackFn = "applyRetrievalWriteBacks"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	type entryPoint struct {
		pos  token.Position
		body *ast.BlockStmt
	}
	found := map[string]entryPoint{}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil || !fn.Name.IsExported() {
				continue
			}
			if !isEngineReceiver(fn.Recv) || !returnsAnswer(fn.Type.Results) {
				continue
			}
			found[fn.Name.Name] = entryPoint{pos: fset.Position(fn.Pos()), body: fn.Body}
		}
	}

	// Guard the guard: if the detector stops matching, it passes vacuously and
	// the class it exists to catch returns unnoticed.
	if len(found) == 0 {
		t.Fatal("no exported Engine methods returning domain.Answer found — the detector no longer matches the code it checks")
	}

	for name, ep := range found {
		if callsAny(ep.body, map[string]bool{writeBackFn: true}) {
			continue
		}
		// Delegation to another entry point counts: Answer → AnswerWithOptions.
		delegates := make(map[string]bool, len(found))
		for other := range found {
			if other != name {
				delegates[other] = true
			}
		}
		if callsAny(ep.body, delegates) {
			continue
		}
		t.Errorf("%s: Engine.%s returns a domain.Answer without calling %s — this recall path gets no "+
			"Hebbian strengthening, no reconsolidation and no inhibition, silently.", ep.pos, name, writeBackFn)
	}
}

func isEngineReceiver(recv *ast.FieldList) bool {
	if recv == nil || len(recv.List) != 1 {
		return false
	}
	t := recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	id, ok := t.(*ast.Ident)

	return ok && id.Name == "Engine"
}

func returnsAnswer(results *ast.FieldList) bool {
	if results == nil {
		return false
	}
	for _, f := range results.List {
		sel, ok := f.Type.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "domain" && sel.Sel.Name == "Answer" {
			return true
		}
	}

	return false
}

// callsAny reports whether body contains a method call whose selector name is
// in names (e.g. `e.applyRetrievalWriteBacks(...)`).
func callsAny(body *ast.BlockStmt, names map[string]bool) bool {
	hit := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && names[sel.Sel.Name] {
			hit = true
		}

		return true
	})

	return hit
}
