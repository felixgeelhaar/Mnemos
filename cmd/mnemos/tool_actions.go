package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
)

// Deriving operational actions and outcomes from a session transcript.
//
// mnemos models learning as action -> outcome -> lesson -> playbook, and every
// layer of it was implemented, tested, and empty: 0 actions and 0 outcomes
// against 86,190 claims. ADR 0002 expects outcomes to arrive from explicit API
// calls or pull adapters (Prometheus and friends). In a Claude Code session
// neither happens, so the loop had no input and the whole skill layer sat idle.
//
// The signal was already there and being discarded. Every transcript carries
// paired tool_use / tool_result blocks — one session sampled here had 753 pairs,
// 22 of them errors — and the capture path steps over them ("skipping non-text
// blocks"). A tool call IS an action; its result IS the outcome; is_error IS the
// verdict. Nothing needs to be invented, only read.
//
// What this deliberately does NOT do is record all 753. ActionItem.Subject
// documents the reason: "Same subject + kind across actions is what forms a
// corroborating cluster." Lessons come from REPETITION, so the value is in
// stable, recurring (kind, subject) pairs — "go test on mnemos" observed a
// hundred times — not in one-off shell invocations. Recording everything would
// bury that signal under noise, which is precisely the failure that filled this
// brain with narration.
//
// So: a tight allowlist of operations whose success or failure means something,
// and silence for everything else. It is meant to be extended deliberately, one
// verb at a time, rather than opened up to whatever ran.

// toolAction is one derived action and its observed outcome.
type toolAction struct {
	Kind    string // operational verb: test, build, lint, push, …
	Subject string // what it acted on — the repository
	Command string // the command, for the action's metadata
	Failed  bool   // from the tool_result's is_error
}

// commandKinds maps a substring of a shell command to the operational kind it
// represents. Order matters: the first match wins, so more specific patterns
// must precede general ones ("gh pr merge" before "git").
//
// Grounded in what a real session actually ran (542 bash calls) rather than
// guessed: the verbs below are the ones that recur across sessions AND whose
// failure is informative. `cd`, `grep`, `ls`, `echo` and friends are
// deliberately absent — they are how a command is assembled, not an operation
// whose outcome teaches anything.
var commandKinds = []struct{ match, kind string }{
	{"go test", "test"},
	{"go build", "build"},
	{"go vet", "vet"},
	{"golangci-lint", "lint"},
	{"gofmt", "format"},
	{"nox scan", "security-scan"},
	{"gh pr merge", "merge"},
	{"gh pr create", "open-pr"},
	{"gh release", "release"},
	{"git push", "push"},
	{"git commit", "commit"},
	{"brew upgrade", "upgrade"},
	{"brew install", "install"},
	{"npm test", "test"},
	{"npm run build", "build"},
	{"pytest", "test"},
	{"cargo test", "test"},
	{"cargo build", "build"},
	{"docker build", "build"},
	{"kubectl apply", "deploy"},
	{"terraform apply", "deploy"},
}

// classifyCommand maps a shell command to an operational kind, reporting false
// when it is not one of the operations worth learning from.
//
// It searches the WHOLE command rather than its first word: real commands are
// compound, and in the sampled session the single most common leading token was
// `cd` (216 of 542) because almost everything is `cd <dir> && <the real work>`.
// First-token classification would have labelled the majority "cd" and missed
// every operation underneath.
func classifyCommand(cmd string) (string, bool) {
	c := strings.ToLower(cmd)
	for _, ck := range commandKinds {
		if strings.Contains(c, ck.match) {
			return ck.kind, true
		}
	}
	return "", false
}

// transcriptBlock is the subset of a transcript content block this needs.
type transcriptBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`          // tool_use
	Name      string          `json:"name"`        // tool_use
	Input     json.RawMessage `json:"input"`       // tool_use
	ToolUseID string          `json:"tool_use_id"` // tool_result
	IsError   bool            `json:"is_error"`    // tool_result
}

// toolTranscriptLine is this file's view of a transcript line. Named apart from
// hook.go's transcriptLine, which models the same JSONL for a different purpose
// (prose extraction) and keeps its content raw.
type toolTranscriptLine struct {
	Message struct {
		Content []transcriptBlock `json:"content"`
	} `json:"message"`
}

// deriveToolActions reads a transcript span and returns the classified actions
// with their outcomes, pairing each tool_use to its tool_result by id.
//
// Unpaired calls are dropped rather than recorded as unknown: an action whose
// outcome was never observed teaches nothing, and a pile of "unknown" verdicts
// would skew the success rates that playbook reinforcement is computed from.
// A tool still running when the span was read is simply picked up next time.
func deriveToolActions(transcript string, subject string) []toolAction {
	type pending struct{ kind, command string }
	uses := map[string]pending{}
	var out []toolAction

	for _, line := range strings.Split(transcript, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var tl toolTranscriptLine
		if err := json.Unmarshal([]byte(line), &tl); err != nil {
			continue // a partial trailing line is normal; skip it
		}
		for _, b := range tl.Message.Content {
			switch b.Type {
			case "tool_use":
				if b.Name != "Bash" || b.ID == "" {
					continue
				}
				var in struct {
					Command string `json:"command"`
				}
				if err := json.Unmarshal(b.Input, &in); err != nil {
					continue
				}
				kind, ok := classifyCommand(in.Command)
				if !ok {
					continue
				}
				uses[b.ID] = pending{kind: kind, command: in.Command}
			case "tool_result":
				p, ok := uses[b.ToolUseID]
				if !ok {
					continue
				}
				delete(uses, b.ToolUseID)
				out = append(out, toolAction{
					Kind:    p.kind,
					Subject: subject,
					Command: p.command,
					Failed:  b.IsError,
				})
			}
		}
	}
	return out
}

// actionSubject names what an action acted on: the repository directory. It is
// the clustering key, so it must be stable across sessions — a path would not
// be, and an absolute path would leak the developer's home directory into the
// brain.
func actionSubject(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return "unknown"
	}
	return filepath.Base(cwd)
}

// recordToolActions writes the operations found in a transcript span to the
// action/outcome layer — the input that layer never had.
//
// Hosted brains are skipped: the tenant routing for a hosted capture is decided
// server-side, and recording actions against whichever store this machine
// resolves would attribute them to the wrong brain.
//
// Every failure here is swallowed. This runs inside a detached capture worker
// whose actual job is persisting the conversation; an action is an enrichment,
// and a brain that captured the session but missed an operation is strictly
// better than one that lost the session trying to record it.
func recordToolActions(ctx context.Context, ev hookEvent, span string) {
	if hostedConfigured() || strings.TrimSpace(span) == "" {
		return
	}
	actions := deriveToolActions(span, actionSubject(ev.Cwd))
	if len(actions) == 0 {
		return
	}
	for _, a := range actions {
		out, err := mcpRunRecordAction(ctx, "claude-code", mcpRecordActionInput{
			Kind:    a.Kind,
			Subject: a.Subject,
			Actor:   "claude-code",
			RunID:   ev.SessionID,
			Metadata: map[string]string{
				"command": truncateCommand(a.Command),
				"source":  "transcript",
			},
		})
		if err != nil || out.ID == "" {
			continue
		}
		result := "success"
		if a.Failed {
			result = "failure"
		}
		_, _ = mcpRunRecordOutcome(ctx, "claude-code", mcpRecordOutcomeInput{
			ActionID: out.ID,
			Result:   result,
			Source:   "transcript",
		})
	}
}

// maxCommandMetadata bounds the command stored alongside an action. A heredoc
// or a generated one-liner can run to kilobytes, and this is context for a
// human reading an action, not content to be searched.
const maxCommandMetadata = 300

func truncateCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if len(cmd) <= maxCommandMetadata {
		return cmd
	}
	return cmd[:maxCommandMetadata] + "…"
}
