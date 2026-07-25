package embedding

import (
	"context"
	"strings"
	"testing"
)

const secretForLeakTest = "AIzaSyLEAKCANARY0000000000000000000000"

// TestGeminiEmbedder_ErrorDoesNotLeakAPIKey is the embedding-side twin of the
// check in internal/llm. This path is the more exposed of the two: its errors
// are wrapped up through the MCP tool layer and returned to the caller, so a
// key in the URL would land directly in an agent transcript.
func TestGeminiEmbedder_ErrorDoesNotLeakAPIKey(t *testing.T) {
	e := NewGeminiEmbedder("http://127.0.0.1:1", secretForLeakTest, "text-embedding-test")

	_, err := e.Embed(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatal("expected a transport error against an unreachable host")
	}
	if strings.Contains(err.Error(), secretForLeakTest) {
		t.Fatalf("API key leaked into the error string: %v", err)
	}
}
