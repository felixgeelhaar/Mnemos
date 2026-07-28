package main

import "testing"

// The classifier must read the WHOLE command, not its first word.
//
// In a real session, 216 of 542 bash calls began with `cd` — almost everything
// is `cd <dir> && <the real work>`. First-token classification would label the
// majority "cd" and miss every operation underneath it.
func TestClassifyCommand_MatchesInsideCompoundCommands(t *testing.T) {
	for cmd, want := range map[string]string{
		"go test ./...":                                  "test",
		"cd /repo && go test -race ./...":                "test",
		"cd /repo && golangci-lint run ./...":            "lint",
		"git push -u origin main":                        "push",
		"gh pr merge 316 --squash":                       "merge",
		"nox scan . -severity-threshold high":            "security-scan",
		"cd x && go build ./... && echo done":            "build",
		"MNEMOS_JOB_TIMEOUT=10m go test ./internal/llm/": "test",
	} {
		got, ok := classifyCommand(cmd)
		if !ok {
			t.Errorf("classifyCommand(%q) did not classify; want %q", cmd, want)
			continue
		}
		if got != want {
			t.Errorf("classifyCommand(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// Silence is the design. Recording every shell call would bury the recurring
// (kind, subject) pairs that lessons are derived from — the same failure that
// filled this brain with narration claims.
func TestClassifyCommand_IgnoresPlumbing(t *testing.T) {
	for _, cmd := range []string{
		"cd /Users/felix/repo",
		"grep -rn TODO .",
		"ls -la",
		"echo hello",
		"sed -i '' s/a/b/ file",
		"cat go.mod",
	} {
		if kind, ok := classifyCommand(cmd); ok {
			t.Errorf("classifyCommand(%q) = %q, want no classification", cmd, kind)
		}
	}
}

// More specific patterns must win: "gh pr merge" is a merge, not a push.
func TestClassifyCommand_PrefersTheMoreSpecificPattern(t *testing.T) {
	if got, _ := classifyCommand("gh pr merge 1 --squash && git push"); got != "merge" {
		t.Errorf("expected the earlier, more specific pattern to win, got %q", got)
	}
}

const transcriptFixture = `
{"message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"cd /repo && go test ./..."}}]}}
{"message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":false}]}}
{"message":{"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"golangci-lint run ./..."}}]}}
{"message":{"content":[{"type":"tool_result","tool_use_id":"t2","is_error":true}]}}
{"message":{"content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"ls -la"}}]}}
{"message":{"content":[{"type":"tool_result","tool_use_id":"t3","is_error":false}]}}
{"message":{"content":[{"type":"tool_use","id":"t4","name":"Read","input":{"command":"go test"}}]}}
{"message":{"content":[{"type":"tool_result","tool_use_id":"t4","is_error":false}]}}
`

// The loop mnemos was missing: tool_use is the action, tool_result is the
// outcome, is_error is the verdict, tool_use_id joins them.
func TestDeriveToolActions_PairsUsesToResults(t *testing.T) {
	got := deriveToolActions(transcriptFixture, "mnemos")

	if len(got) != 2 {
		t.Fatalf("expected 2 classified actions (test + lint), got %d: %+v", len(got), got)
	}
	if got[0].Kind != "test" || got[0].Failed {
		t.Errorf("first action should be a successful test, got %+v", got[0])
	}
	if got[1].Kind != "lint" || !got[1].Failed {
		t.Errorf("second action should be a FAILED lint — is_error is the verdict, got %+v", got[1])
	}
	for _, a := range got {
		if a.Subject != "mnemos" {
			t.Errorf("subject must be the clustering key, got %q", a.Subject)
		}
	}
}

// A non-Bash tool is not an operational action even when its input happens to
// contain a matching string.
func TestDeriveToolActions_OnlyBash(t *testing.T) {
	for _, a := range deriveToolActions(transcriptFixture, "mnemos") {
		if a.Command == "go test" {
			t.Error("a Read tool call was recorded as an action")
		}
	}
}

// An action whose outcome was never observed teaches nothing, and recording it
// as "unknown" would skew the success rates playbook reinforcement computes.
// A call still running when the span was read is picked up on the next pass.
func TestDeriveToolActions_DropsUnpairedCalls(t *testing.T) {
	unpaired := `{"message":{"content":[{"type":"tool_use","id":"t9","name":"Bash","input":{"command":"go test ./..."}}]}}`
	if got := deriveToolActions(unpaired, "mnemos"); len(got) != 0 {
		t.Errorf("an unpaired tool_use must not be recorded, got %+v", got)
	}
}

// Transcript spans are read by byte offset, so a partial trailing line is
// normal and must not discard the whole span.
func TestDeriveToolActions_ToleratesAPartialTrailingLine(t *testing.T) {
	got := deriveToolActions(transcriptFixture+`{"message":{"content":[{"type":"tool_`, "mnemos")
	if len(got) != 2 {
		t.Errorf("a truncated final line must not lose earlier actions, got %d", len(got))
	}
}

func TestActionSubject(t *testing.T) {
	if got := actionSubject("/Users/felix/Developer/klarlabs/oss/mnemos"); got != "mnemos" {
		t.Errorf("actionSubject = %q, want %q — an absolute path is unstable and leaks $HOME", got, "mnemos")
	}
	if got := actionSubject(""); got != "unknown" {
		t.Errorf("actionSubject(\"\") = %q, want %q", got, "unknown")
	}
}
