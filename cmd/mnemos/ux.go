package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.klarlabs.de/mnemos/internal/domain"
)

func claimTypePriority(t domain.ClaimType) int {
	switch t {
	case domain.ClaimTypeDecision:
		return 0
	case domain.ClaimTypeFact:
		return 1
	default:
		return 2
	}
}

// useASCII returns true when the terminal likely cannot render Unicode/emoji.
func useASCII() bool {
	term := os.Getenv("TERM")
	lang := os.Getenv("LANG")
	if term == "dumb" || term == "linux" {
		return true
	}
	if lang != "" && !strings.Contains(strings.ToLower(lang), "utf") {
		return true
	}
	return false
}

// icon returns the Unicode icon or its ASCII fallback.
func icon(unicode, ascii string) string {
	if useASCII() {
		return ascii
	}
	return unicode
}

func printWelcome() {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  Mnemos - Local-first knowledge engine")
	fmt.Println("  Eliminating AI hallucination through evidence-backed claims")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("")
	fmt.Println("  Quick start:")
	fmt.Println("    mnemos process --text \"Your text here\"")
	fmt.Println("    mnemos query \"Your question\"")
	fmt.Println("")
	fmt.Println("  Documentation: https://github.com/klarlabs-studio/mnemos")
	fmt.Println("")
}

func printExtractionSummary(claims []domain.Claim, rels []domain.Relationship) {
	facts := 0
	decisions := 0
	hypotheses := 0
	contested := 0
	contradictions := 0

	for _, c := range claims {
		switch c.Type {
		case domain.ClaimTypeFact:
			facts++
		case domain.ClaimTypeDecision:
			decisions++
		case domain.ClaimTypeHypothesis:
			hypotheses++
		}
		if c.Status == domain.ClaimStatusContested {
			contested++
		}
	}

	for _, r := range rels {
		if r.Type == domain.RelationshipTypeContradicts {
			contradictions++
		}
	}

	warn := icon("⚠️", "(!)")

	fmt.Println("")
	fmt.Println("  Extraction Summary:")
	fmt.Println("  ┌──────────────────────────────┐")
	fmt.Printf("  │ Facts:          %-5d        │\n", facts)
	fmt.Printf("  │ Decisions:      %-5d        │\n", decisions)
	fmt.Printf("  │ Hypotheses:     %-5d        │\n", hypotheses)
	if contested > 0 {
		fmt.Printf("  │ Contested:      %-5d %s     │\n", contested, warn)
	}
	if contradictions > 0 {
		fmt.Printf("  │ Contradictions: %-5d %s     │\n", contradictions, warn)
	}
	fmt.Println("  └──────────────────────────────┘")
	fmt.Println("")

	if contested > 0 || contradictions > 0 {
		fmt.Println("  Run 'mnemos query --human \"What contradicts?\"' to see details.")
	}
}

func printFirstRunHints() {
	bulb := icon("💡", "*")
	fmt.Printf("  %s Tips:\n", bulb)
	fmt.Println("    - Use 'mnemos process --text <text>' for quick extraction")
	fmt.Println("    - Use 'mnemos query <question>' to query your knowledge")
	fmt.Println("    - Use '--run <id>' with query to scope to a specific run")
	fmt.Println("")
}

// commands is the full set of top-level commands, used both for typo
// suggestions (suggestCommand) and as the single source of truth for what the
// dispatcher accepts. Keep in sync with the switch in main.go.
//
// "keep in sync" was the whole mechanism, and it failed: six dispatched
// commands drifted out of this list — `health` (the vitals entry point),
// `journal`, `predictive-error`, `classify-durability`, `float-back`, `global`
// — so `mnemos helth` suggested nothing at all. Nothing failed, because a
// missing entry has no correctness consequence; it only degrades the error
// message shown to a user who is already lost. Both directions are now a test
// failure (cli_commands_guard_test.go), which is what "keep in sync" needed to
// mean all along.
var commands = []string{
	"init", "setup", "doctor",
	"mcp", "serve",
	"ingest", "extract", "relate", "process", "claim",
	"query", "entities", "extract-entities", "metrics", "quality", "curiosity", "audit",
	"health", "journal", "predictive-error",
	"decision", "action", "outcome", "incident",
	"synthesize", "lessons", "playbook", "export", "import", "history",
	"resolve", "trust", "verify", "reconsolidate",
	"user", "token", "agent", "repo-tenant",
	"registry", "push", "pull",
	"reset", "delete-claim", "delete-event", "reembed", "recompute-trust", "recompute-contested",
	"recompute-half-life", "classify-durability", "prune", "dedup", "consolidate",
	"sync-docs", "rebuild", "workspace", "float-back", "global", "hook",
}

// suggestCommand returns the closest known command to input, or "" if none is close.
func suggestCommand(input string) string {
	best := ""
	bestDist := 3 // max edit distance to suggest
	for _, cmd := range commands {
		d := levenshtein(input, cmd)
		if d < bestDist {
			bestDist = d
			best = cmd
		}
	}
	return best
}

func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr := make([]int, lb+1)
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			ins := curr[j-1] + 1
			del := prev[j] + 1
			sub := prev[j-1] + cost
			curr[j] = ins
			if del < curr[j] {
				curr[j] = del
			}
			if sub < curr[j] {
				curr[j] = sub
			}
		}
		prev = curr
	}
	return prev[lb]
}

func printIngestHint(runID string) {
	fmt.Fprintf(os.Stderr, "\nTip: Run 'mnemos extract --run %s' then 'mnemos relate' to make this content queryable,\n     or use 'mnemos process' for all-in-one.\n", runID)
}

func isFirstRun(dbPath string) bool {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		dir := filepath.Dir(dbPath)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return true
		}
		return true
	}
	return false
}

func printClaimPreview(claims []domain.Claim, maxDisplay int) {
	if len(claims) == 0 {
		return
	}

	// Sort: decisions first, then by confidence descending.
	sorted := make([]domain.Claim, len(claims))
	copy(sorted, claims)
	sort.Slice(sorted, func(i, j int) bool {
		ti, tj := claimTypePriority(sorted[i].Type), claimTypePriority(sorted[j].Type)
		if ti != tj {
			return ti < tj
		}
		return sorted[i].Confidence > sorted[j].Confidence
	})

	fmt.Println("  Top Claims:")
	for i := 0; i < len(sorted) && i < maxDisplay; i++ {
		c := sorted[i]
		typeIcon := icon("•", "-")
		switch c.Type {
		case domain.ClaimTypeDecision:
			typeIcon = icon("✓", "+")
		case domain.ClaimTypeHypothesis:
			typeIcon = icon("?", "?")
		case domain.ClaimTypeTestResult:
			typeIcon = icon("⚗", "T")
		}
		status := ""
		if c.Status == domain.ClaimStatusContested {
			status = " [CONFLICT]"
		}
		text := c.Text
		if len(text) > 50 {
			text = text[:47] + "..."
		}
		fmt.Printf("    %s %s%s\n", typeIcon, text, status)
	}
	if len(claims) > maxDisplay {
		fmt.Printf("    ... and %d more\n", len(claims)-maxDisplay)
	}
}
