package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnthropicClientComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" {
			t.Fatalf("missing api key header")
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Fatalf("missing anthropic-version header")
		}

		resp := anthropicResponse{
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{{Type: "text", Text: "hello from claude"}},
			Model: "claude-sonnet-4-20250514",
		}
		resp.Usage.InputTokens = 10
		resp.Usage.OutputTokens = 5

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer server.Close()

	client := NewAnthropicClient(server.URL, "test-key", "claude-sonnet-4-20250514")
	resp, err := client.Complete(context.Background(), []Message{
		{Role: RoleSystem, Content: "you are helpful"},
		{Role: RoleUser, Content: "hi"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "hello from claude" {
		t.Fatalf("got content %q, want %q", resp.Content, "hello from claude")
	}
	if resp.InputTokens != 10 || resp.OutputTokens != 5 {
		t.Fatalf("token counts wrong: input=%d output=%d", resp.InputTokens, resp.OutputTokens)
	}
}

func TestOpenAIClientComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("missing auth header")
		}

		resp := openAIResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{{Message: struct {
				Content string `json:"content"`
			}{Content: "hello from openai"}}},
			Model: "gpt-4o-mini",
		}
		resp.Usage.PromptTokens = 8
		resp.Usage.CompletionTokens = 4

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer server.Close()

	client := NewOpenAIClient(server.URL, "test-key", "gpt-4o-mini", "openai")
	resp, err := client.Complete(context.Background(), []Message{
		{Role: RoleSystem, Content: "you are helpful"},
		{Role: RoleUser, Content: "hi"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "hello from openai" {
		t.Fatalf("got content %q, want %q", resp.Content, "hello from openai")
	}
}

func TestGeminiClientComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		// The key must travel in a header, not the query string: a ?key= URL
		// is echoed back inside *url.Error on any transport failure, and those
		// errors are logged and returned to MCP clients.
		if got := r.Header.Get("x-goog-api-key"); got != "test-key" {
			t.Fatalf("x-goog-api-key header = %q, want %q", got, "test-key")
		}
		if r.URL.Query().Has("key") {
			t.Fatalf("api key must not appear in the query string: %s", r.URL.RawQuery)
		}

		resp := geminiResponse{
			Candidates: []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			}{{Content: struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			}{Parts: []struct {
				Text string `json:"text"`
			}{{Text: "hello from gemini"}}}}},
		}
		resp.UsageMetadata.PromptTokenCount = 6
		resp.UsageMetadata.CandidatesTokenCount = 3

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer server.Close()

	client := NewGeminiClient(server.URL, "test-key", "gemini-2.0-flash")
	resp, err := client.Complete(context.Background(), []Message{
		{Role: RoleSystem, Content: "you are helpful"},
		{Role: RoleUser, Content: "hi"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "hello from gemini" {
		t.Fatalf("got content %q, want %q", resp.Content, "hello from gemini")
	}
}

func TestAnthropicClientErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid api key"}}`)) //nolint:errcheck
	}))
	defer server.Close()

	client := NewAnthropicClient(server.URL, "bad-key", "claude-sonnet-4-20250514")
	_, err := client.Complete(context.Background(), []Message{{Role: RoleUser, Content: "hi"}})
	if err == nil {
		t.Fatal("expected error for 401 status")
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Run("missing provider", func(t *testing.T) {
		t.Setenv("MNEMOS_LLM_PROVIDER", "")
		_, err := ConfigFromEnv()
		// When Ollama is running locally, ConfigFromEnv auto-detects it
		// and returns a valid config instead of an error.
		if OllamaAvailable() {
			if err != nil {
				t.Fatalf("expected Ollama auto-detect to succeed, got: %v", err)
			}
		} else {
			if err == nil {
				t.Fatal("expected error when provider is not set and Ollama is unavailable")
			}
		}
	})

	t.Run("anthropic requires key", func(t *testing.T) {
		t.Setenv("MNEMOS_LLM_PROVIDER", "anthropic")
		t.Setenv("MNEMOS_LLM_API_KEY", "")
		_, err := ConfigFromEnv()
		if err == nil {
			t.Fatal("expected error when api key is missing for anthropic")
		}
	})

	t.Run("valid anthropic config", func(t *testing.T) {
		t.Setenv("MNEMOS_LLM_PROVIDER", "anthropic")
		t.Setenv("MNEMOS_LLM_API_KEY", "sk-test")
		t.Setenv("MNEMOS_LLM_MODEL", "")
		t.Setenv("MNEMOS_LLM_BASE_URL", "")
		cfg, err := ConfigFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Provider != ProviderAnthropic {
			t.Fatalf("got provider %q", cfg.Provider)
		}
		if cfg.Model != "claude-sonnet-4-20250514" {
			t.Fatalf("got model %q", cfg.Model)
		}
	})

	t.Run("ollama no key needed", func(t *testing.T) {
		t.Setenv("MNEMOS_LLM_PROVIDER", "ollama")
		t.Setenv("MNEMOS_LLM_API_KEY", "")
		t.Setenv("MNEMOS_LLM_MODEL", "")
		t.Setenv("MNEMOS_LLM_BASE_URL", "")
		cfg, err := ConfigFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.BaseURL != "http://localhost:11434" {
			t.Fatalf("got base url %q", cfg.BaseURL)
		}
	})
}

// TestConfigFromEnv_DropsForeignProviderBaseURL pins the fix for the silent
// failure a mixed config/env setup produces: a config file pins
// provider=ollama + base_url=localhost:11434, then MNEMOS_LLM_PROVIDER is
// overridden to a cloud provider without a matching base_url. The stale Ollama
// URL must not be handed to the cloud client, or every call fails silently.
func TestConfigFromEnv_DropsForeignProviderBaseURL(t *testing.T) {
	ollama := DefaultBaseURL(ProviderOllama)

	t.Run("cloud provider ignores another provider's default URL", func(t *testing.T) {
		t.Setenv("MNEMOS_LLM_PROVIDER", "anthropic")
		t.Setenv("MNEMOS_LLM_API_KEY", "sk-test")
		t.Setenv("MNEMOS_LLM_BASE_URL", ollama) // stale, from a prior ollama config
		cfg, err := ConfigFromEnv()
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if cfg.BaseURL != DefaultBaseURL(ProviderAnthropic) {
			t.Fatalf("stale ollama URL leaked through: got %q, want %q", cfg.BaseURL, DefaultBaseURL(ProviderAnthropic))
		}
	})

	t.Run("a provider's own default URL is kept", func(t *testing.T) {
		t.Setenv("MNEMOS_LLM_PROVIDER", "ollama")
		t.Setenv("MNEMOS_LLM_BASE_URL", ollama)
		cfg, err := ConfigFromEnv()
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if cfg.BaseURL != ollama {
			t.Fatalf("a provider's own default must survive, got %q", cfg.BaseURL)
		}
	})

	t.Run("a genuine custom endpoint is kept", func(t *testing.T) {
		const custom = "https://llm-gateway.internal:8443"
		t.Setenv("MNEMOS_LLM_PROVIDER", "anthropic")
		t.Setenv("MNEMOS_LLM_API_KEY", "sk-test")
		t.Setenv("MNEMOS_LLM_BASE_URL", custom)
		cfg, err := ConfigFromEnv()
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if cfg.BaseURL != custom {
			t.Fatalf("a custom endpoint (not any provider's default) must survive, got %q", cfg.BaseURL)
		}
	})

	t.Run("openai-compat keeps a provider-default-looking URL", func(t *testing.T) {
		// openai-compat against a local Ollama's OpenAI endpoint is legitimate,
		// so its URL is never treated as a stale pairing.
		t.Setenv("MNEMOS_LLM_PROVIDER", "openai-compat")
		t.Setenv("MNEMOS_LLM_MODEL", "local-model")
		t.Setenv("MNEMOS_LLM_BASE_URL", ollama)
		cfg, err := ConfigFromEnv()
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if cfg.BaseURL != ollama {
			t.Fatalf("openai-compat endpoint must be honored verbatim, got %q", cfg.BaseURL)
		}
	})
}
