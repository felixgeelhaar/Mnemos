package llm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ollamaTagsTimeout bounds the model-inventory lookup. This runs on health
// checks against a local daemon, so it is short by design: a slow or wedged
// Ollama should make the check inconclusive quickly, not stall the caller.
const ollamaTagsTimeout = 2 * time.Second

// OllamaModelPresent reports whether model is installed on the Ollama instance
// at baseURL.
//
// This exists because a reachable daemon says nothing about whether the model
// it was configured with is still there. Ollama answers requests for a missing
// model with a 404 per call, so extraction degrades silently: the pipeline
// keeps running, every LLM call fails, and nothing surfaces it. Listing the
// installed models is free and local, which is what makes it safe to do on a
// health check where an actual inference call would not be.
//
// A non-nil error means the inventory could not be read at all — unreachable,
// non-200, or undecodable. Callers must treat that as "cannot tell" rather than
// as a missing model.
func OllamaModelPresent(baseURL, model string) (bool, error) {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = DefaultBaseURL(ProviderOllama)
	}

	client := &http.Client{Timeout: ollamaTagsTimeout}
	resp, err := client.Get(base + "/api/tags")
	if err != nil {
		return false, fmt.Errorf("list ollama models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("list ollama models: unexpected status %d", resp.StatusCode)
	}

	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, fmt.Errorf("list ollama models: %w", err)
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, fmt.Errorf("list ollama models: %w", err)
	}

	want := normalizeOllamaModel(model)
	for _, m := range payload.Models {
		if normalizeOllamaModel(m.Name) == want {
			return true, nil
		}
	}
	return false, nil
}

// normalizeOllamaModel makes the implicit ":latest" tag explicit so a model
// pulled as "nomic-embed-text" (stored as "nomic-embed-text:latest") still
// matches config that names it without a tag.
func normalizeOllamaModel(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if n != "" && !strings.Contains(n, ":") {
		return n + ":latest"
	}
	return n
}
