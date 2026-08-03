package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.klarlabs.de/mcp/protocol"
)

// A marked error must reach the caller as -32602 on the DIRECT route.
//
// publicError does errors.As(err, &mcpErr) and returns a *protocol.Error
// verbatim; anything else it replaces with a generic -32603. So the marked
// error has to unwrap to one, or a tool that never enters the kernel stays
// opaque — which is exactly what happened to memory_resolve_dissonance,
// record_decision and record_outcome on the first attempt at this fix.
func TestUserInputError_UnwrapsToAProtocolErrorForTheDirectRoute(t *testing.T) {
	err := userInputError(fmt.Errorf("invalid risk_level %q", "audit"))

	var pe *protocol.Error
	if !errors.As(err, &pe) {
		t.Fatal("errors.As found no *protocol.Error — publicError will flatten this to -32603")
	}
	if pe.Code != protocol.CodeInvalidParams {
		t.Errorf("code = %d, want %d (invalid params)", pe.Code, protocol.CodeInvalidParams)
	}
	if pe.Message != `invalid risk_level "audit"` {
		t.Errorf("message = %q, want the cause verbatim", pe.Message)
	}
	if strings.Contains(pe.Message, userInputMarker) {
		t.Error("the internal marker leaked into the caller-facing message")
	}
}

// …and as -32602 on the KERNEL route, where only a string survives.
//
// axi flattens an executor error into FailureReason{Code, Message}, keeping
// just execErr.Error(), so the marker embedded in Error() is the only channel
// left. dispatchAxiTool reads it back out.
func TestUserInputError_SurvivesTheKernelStringFlattening(t *testing.T) {
	err := userInputError(errors.New("invalid outcome result \"audit\""))

	// What axi would store, including the context the kernel prepends.
	flattened := "govwrite: write_outcome: EXECUTION_ERROR: append outcome: " + err.Error()

	msg, ok := userInputMessage(flattened)
	if !ok {
		t.Fatal("marker not found in the flattened failure — the caller gets -32603")
	}
	if msg != `invalid outcome result "audit"` {
		t.Errorf("message = %q, want the cause with the kernel's plumbing prefix stripped", msg)
	}
}

// An UNMARKED error must stay opaque. This is the property that makes the whole
// mechanism safe to sit next to publicError.
//
// publicError exists so internal detail cannot reach a peer, and redacting DSNs
// from error paths was itself a past fix (#282). Marking is opt-in precisely so
// that exposing a message is a deliberate act by someone who has read it; the
// default must remain "reveal nothing".
func TestUserInputError_UnmarkedErrorsStayOpaque(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"plain", errors.New("something went wrong")},
		{"dsn", fmt.Errorf("open store: %w",
			errors.New("postgres://mnemos:hunter2@db.internal:5432/mnemos: connection refused"))},
		{"path", errors.New("lstat /Users/someone/.local/share/mnemos/mnemos.db: permission denied")},
		{"kernel", errors.New("EXECUTION_ERROR: internal invariant violated")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := userInputProtocolError(tc.err); got != nil {
				t.Errorf("unmarked error was surfaced as %v — it must stay opaque", got)
			}
			if _, ok := userInputMessage(tc.err.Error()); ok {
				t.Error("unmarked error matched the marker")
			}
			var pe *protocol.Error
			if errors.As(tc.err, &pe) {
				t.Error("unmarked error unwraps to a protocol error, so publicError would pass it through")
			}
		})
	}
}

// The secret in an unmarked DSN error must not reach a peer by any route.
func TestUserInputError_DoesNotLeakACredentialBearingDSN(t *testing.T) {
	const secret = "hunter2"
	err := fmt.Errorf("open store: postgres://mnemos:%s@db.internal:5432/mnemos: refused", secret)

	if got := userInputProtocolError(err); got != nil {
		t.Fatalf("a DSN error was surfaced: %v", got)
	}
	if msg, ok := userInputMessage(err.Error()); ok {
		t.Fatalf("a DSN error matched the marker and would be shown as %q", msg)
	}
}

// Wrapping is idempotent and nil-safe, so call sites can wrap unconditionally
// without producing a doubled marker that userInputMessage would mis-split.
func TestUserInputError_IsIdempotentAndNilSafe(t *testing.T) {
	if got := userInputError(nil); got != nil {
		t.Errorf("userInputError(nil) = %v, want nil", got)
	}
	once := userInputError(errors.New("bad input"))
	twice := userInputError(once)
	if once != twice {
		t.Error("double-wrapping produced a new error; the marker would appear twice")
	}
	if n := strings.Count(twice.Error(), userInputMarker); n != 1 {
		t.Errorf("marker appears %d times, want 1", n)
	}
}
