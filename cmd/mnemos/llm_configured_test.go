package main

import "testing"

// A named provider must enable the LLM capture path.
func TestLLMConfigured_TrueForANamedProvider(t *testing.T) {
	t.Setenv("MNEMOS_LLM_PROVIDER", "openai")
	t.Setenv("MNEMOS_LLM_API_KEY", "test-key")

	if !llmConfigured() {
		t.Fatal("a named provider must enable LLM extraction")
	}
}

// The regression this replaces: `os.Getenv("MNEMOS_LLM_PROVIDER") != ""`.
//
// That test is true for ANY non-empty value, including one the client cannot
// build — so a typo'd provider switched the capture pipeline into LLM mode and
// then failed on every event. Asking the resolver rejects it up front and
// degrades to rule-based extraction cleanly, which is the documented behaviour.
//
// This is the half of the fix that is deterministically testable; the other half
// (auto-detecting a running Ollama when the variable is unset) needs a live
// daemon, and is the case that silently disabled LLM capture on this machine.
func TestLLMConfigured_FalseForAnUnsupportedProvider(t *testing.T) {
	t.Setenv("MNEMOS_LLM_PROVIDER", "not-a-real-provider")

	if llmConfigured() {
		t.Fatal("an unsupported provider must not enable LLM extraction — the old env-var check accepted any non-empty string")
	}
}
