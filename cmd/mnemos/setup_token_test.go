package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The hosted-MCP registration used to interpolate the live bearer JWT into the
// `claude mcp add --header …` argv — readable by any local user with `ps` for
// the child's lifetime — and echoed the same string to stdout on the
// print / claude-not-found path, into shell history and CI logs. Both halves
// are pinned here.

const fakeToken = "eyJhbGciOiJIUzI1NiJ9.SUPER-SECRET-BEARER.sig"

func TestClaudeHTTPAddArgs_NeverCarriesTheToken(t *testing.T) {
	args := claudeHTTPAddArgs("https://brain.example.com/mcp", fakeToken, "user")
	for _, a := range args {
		if strings.Contains(a, fakeToken) {
			t.Fatalf("argv leaks the bearer token: %q", args)
		}
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, tokenHeaderPlaceholder) {
		t.Errorf("an authenticated endpoint must still register an Authorization header: %q", joined)
	}

	// No token at all → no header argument.
	plain := claudeHTTPAddArgs("https://brain.example.com/mcp", "", "user")
	for _, a := range plain {
		if a == "--header" {
			t.Fatalf("unauthenticated endpoint should register no header: %q", plain)
		}
	}
}

func TestRegisterClaudeCodeHTTP_PrintPathNeverEchoesTheToken(t *testing.T) {
	out := captureSetupStdout(t, func() {
		if err := registerClaudeCodeHTTP("https://brain.example.com/mcp", fakeToken, "user", false, true); err != nil {
			t.Fatalf("registerClaudeCodeHTTP: %v", err)
		}
	})
	if strings.Contains(out, fakeToken) {
		t.Fatalf("printed plan leaks the bearer token:\n%s", out)
	}
	if !strings.Contains(out, "${MNEMOS_TOKEN}") {
		t.Errorf("printed plan should show the placeholder header:\n%s", out)
	}
	if !strings.Contains(out, "MNEMOS_TOKEN=") {
		t.Errorf("printed plan should explain how to supply the token:\n%s", out)
	}
}

// End-to-end over the real exec path: a stub `claude` on PATH records the argv
// it was invoked with, proving the credential never reaches the process table.
func TestRegisterClaudeCodeHTTP_ExecArgvNeverCarriesTheToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub is POSIX-only")
	}
	bin := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	stub := filepath.Join(bin, "claude")
	// `mcp get` must fail so the caller treats the server as unregistered and
	// proceeds to `mcp add`; every other invocation succeeds.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + argvLog + "\n" +
		"if [ \"$1\" = mcp ] && [ \"$2\" = get ]; then exit 1; fi\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil { //nolint:gosec // test stub must be executable
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	out := captureSetupStdout(t, func() {
		if err := registerClaudeCodeHTTP("https://brain.example.com/mcp", fakeToken, "user", false, false); err != nil {
			t.Fatalf("registerClaudeCodeHTTP: %v", err)
		}
	})
	if strings.Contains(out, fakeToken) {
		t.Errorf("stdout leaks the bearer token:\n%s", out)
	}

	recorded, err := os.ReadFile(argvLog) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	got := string(recorded)
	if !strings.Contains(got, "add") {
		t.Fatalf("expected the stub to see an `mcp add`, got:\n%s", got)
	}
	if strings.Contains(got, fakeToken) {
		t.Fatalf("the bearer token reached the child's argv:\n%s", got)
	}
	if !strings.Contains(got, "${MNEMOS_TOKEN}") {
		t.Errorf("expected the placeholder header in the argv, got:\n%s", got)
	}
}

// captureSetupStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureSetupStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}
