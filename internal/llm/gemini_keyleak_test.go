package llm

import (
	"context"
	"strings"
	"testing"
)

// secretForLeakTest is a syntactically plausible key that is easy to grep for.
const secretForLeakTest = "AIzaSyLEAKCANARY0000000000000000000000"

// TestGeminiClient_ErrorDoesNotLeakAPIKey pins the property that a transport
// failure must not surface the API key.
//
// Putting the key in the query string ("?key=...") makes this easy to get
// wrong: when http.Client.Do fails it returns a *url.Error whose Error()
// embeds the full URL. Go's stripPassword redacts userinfo only — never the
// query — so wrapping that error with %w carries the live key into anything
// that formats it. In this repo those errors reach the server's stderr log and
// are returned to MCP clients, i.e. into an agent transcript.
func TestGeminiClient_ErrorDoesNotLeakAPIKey(t *testing.T) {
	// Port 1 on loopback refuses immediately, so Do() fails at the transport
	// layer and returns *url.Error without waiting on a dial timeout.
	c := NewGeminiClient("http://127.0.0.1:1", secretForLeakTest, "gemini-test")

	_, err := c.Complete(context.Background(), []Message{{Role: RoleUser, Content: "hello"}})
	if err == nil {
		t.Fatal("expected a transport error against an unreachable host")
	}
	if strings.Contains(err.Error(), secretForLeakTest) {
		t.Fatalf("API key leaked into the error string: %v", err)
	}
}
