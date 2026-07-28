package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWantsJSONArray(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prompt string
		want   bool
	}{
		{"extraction prompt", "Output format — a JSON array of objects:", true},
		{"durability prompt", `Reply with ONLY a JSON array of objects {"i": <index>}.`, true},
		{"case insensitive", "return a json array", true},
		{"object prompt", "Return ONLY a JSON object with a summary field.", false},
		{"json mentioned, no shape", "Return ONLY valid JSON.", false},
		{"no json at all", "Summarise the text.", false},
	} {
		if got := wantsJSONArray(tc.prompt); got != tc.want {
			t.Errorf("%s: wantsJSONArray(%q) = %v, want %v", tc.name, tc.prompt, got, tc.want)
		}
	}
}

// The regression, at the layer that produced it.
//
// `response_format: json_object` constrains the model to a top-level object, so
// a prompt asking for a top-level array cannot be satisfied — models return
// `{}`. That parses as valid JSON, fails to unmarshal into a slice, and sends
// extractBatch down its silent rule-based fallback. LLM extraction therefore
// never ran, and nothing anywhere reported an error.
//
// Asserting on the wire is what makes this stick: the bug was invisible in
// every response-level check because the response was well-formed.
func TestComplete_DoesNotForceObjectModeForAnArrayPrompt(t *testing.T) {
	var gotFormat string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ResponseFormat *struct {
				Type string `json:"type"`
			} `json:"response_format"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.ResponseFormat != nil {
			gotFormat = body.ResponseFormat.Type
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"[]"}}]}`))
	}))
	defer srv.Close()

	c := &OpenAIClient{
		provider: "ollama",
		model:    "test-model",
		baseURL:  srv.URL,
		http:     srv.Client(),
	}

	_, err := c.Complete(t.Context(), []Message{
		{Role: RoleSystem, Content: "Return ONLY valid JSON.\n\nOutput format — a JSON array of objects:"},
		{Role: RoleUser, Content: "some text"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotFormat != "" {
		t.Errorf("an array prompt must not set response_format (got %q) — json_object makes the model return {}", gotFormat)
	}
}

// The counterpart: a prompt that genuinely wants an object still gets the
// constraint, so this fix does not disable JSON mode wholesale.
func TestComplete_StillForcesObjectModeForAnObjectPrompt(t *testing.T) {
	var gotFormat string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ResponseFormat *struct {
				Type string `json:"type"`
			} `json:"response_format"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.ResponseFormat != nil {
			gotFormat = body.ResponseFormat.Type
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"}}]}`))
	}))
	defer srv.Close()

	c := &OpenAIClient{
		provider: "ollama",
		model:    "test-model",
		baseURL:  srv.URL,
		http:     srv.Client(),
	}

	_, err := c.Complete(t.Context(), []Message{
		{Role: RoleSystem, Content: "Return ONLY a JSON object describing the summary."},
		{Role: RoleUser, Content: "some text"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.EqualFold(gotFormat, "json_object") {
		t.Errorf("an object prompt should still set json_object, got %q", gotFormat)
	}
}
