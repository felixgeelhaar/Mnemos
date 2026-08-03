package main

import (
	"errors"
	"strings"

	"go.klarlabs.de/mcp/protocol"
)

// User-fixable failures must reach the MCP caller as such (#355).
//
// # The problem
//
// A tool dispatched through the axi kernel reported `-32603 internal error` for
// EVERY failure, including ones the caller could have fixed immediately: an
// unparseable timestamp, an invalid enum, an id that does not exist. An agent
// told `invalid risk_level "audit"` retries with a valid one; an agent told
// `internal error` has no move at all. That is the difference this file exists
// to restore.
//
// The operator was never blind — mcp-go's publicError logs the full error to
// stderr before replacing it, and always did. Only the caller was.
//
// # Why a marker inside the message
//
// axi flattens an executor error into domain.FailureReason{Code, Message},
// hardcoding Code to "EXECUTION_ERROR" and keeping only execErr.Error() — two
// plain strings, with no hook for the executor to influence either
// (axi@v1.4.0 domain/execution.go:256). dispatchAxiTool then re-wraps that as a
// bare fmt.Errorf, so any Go error TYPE is gone long before publicError decides
// what the caller sees.
//
// The marker therefore has to travel in the one channel that survives: the
// message text. This is a protocol between code we own on both ends — we emit
// the token in userInputError and we consume it in userInputMessage — not a
// heuristic that guesses intent from arbitrary text. An unmarked error is
// untouched, so the failure direction is "stays opaque", never "leaks".
//
// # Why not simply surface every kernel failure
//
// publicError exists so internal detail cannot reach a peer, and redacting DSNs
// from error paths was itself a past fix (#282). A blanket passthrough would
// re-open exactly that. Marking is opt-in per call site so that exposing a
// message is a deliberate act by someone who has looked at what it contains.
const userInputMarker = "mnemos/user-input: "

// userInputError marks err as caused by the caller's input, so the MCP boundary
// can return it as -32602 with its message intact instead of -32603.
//
// Only use it where the message is safe to show a peer: it must describe what
// the CALLER supplied, never server state. A wrapped nil returns nil so call
// sites can wrap unconditionally.
func userInputError(err error) error {
	if err == nil {
		return nil
	}
	if isUserInputError(err) {
		return err
	}
	return &userInputErr{err: err, pe: protocol.NewInvalidParams(err.Error())}
}

// userInputErr satisfies BOTH routes a tool error can take, which is the whole
// reason it is a type rather than a wrapped string.
//
//   - DIRECT (no kernel): publicError does errors.As(err, &mcpErr), so Unwrap
//     returning the *protocol.Error makes the clean message pass through
//     verbatim as -32602.
//   - VIA THE KERNEL: axi keeps only execErr.Error(), so the marker in Error()
//     is the only thing that survives; dispatchAxiTool reads it back.
//
// Covering one route and not the other is exactly the bug this fixes — the
// first attempt handled the kernel path alone, and three tools stayed opaque
// because in stdio mode they never enter the kernel.
type userInputErr struct {
	err error
	pe  *protocol.Error
}

func (e *userInputErr) Error() string { return userInputMarker + e.err.Error() }

// Unwrap returns the protocol error so errors.As finds it at the MCP boundary.
// The underlying cause is not needed by any caller once the message is set.
func (e *userInputErr) Unwrap() error { return e.pe }

// isUserInputError reports whether err is already marked, in-process.
func isUserInputError(err error) bool {
	var u *userInputErr
	return errors.As(err, &u)
}

// userInputMessage extracts the caller-facing message from a kernel failure
// string, and reports whether it was marked.
//
// The kernel prefixes its own context ("govwrite: write_decision:
// EXECUTION_ERROR: ..."), so the marker is located anywhere in the string
// rather than only at the front, and everything before it is dropped — that
// prefix is internal plumbing the caller has no use for.
func userInputMessage(s string) (string, bool) {
	i := strings.Index(s, userInputMarker)
	if i < 0 {
		return "", false
	}
	msg := strings.TrimSpace(s[i+len(userInputMarker):])
	if msg == "" {
		return "", false
	}
	return msg, true
}

// userInputProtocolError converts a marked error into the -32602 the caller
// should see, or returns nil when err is not marked and must stay opaque.
func userInputProtocolError(err error) error {
	if err == nil {
		return nil
	}
	if msg, ok := userInputMessage(err.Error()); ok {
		return protocol.NewInvalidParams(msg)
	}
	return nil
}
