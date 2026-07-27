package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ollamaStub serves an /api/tags inventory so the probes can be exercised
// without a real daemon.
func ollamaStub(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestProbeLLMFailsWhenOllamaModelMissing is the regression test for a silent
// production degradation: Ollama was running, so doctor reported "llm ok", while
// the configured model had been removed and every extraction 404'd for days.
func TestProbeLLMFailsWhenOllamaModelMissing(t *testing.T) {
	srv := ollamaStub(t, `{"models":[]}`)
	t.Setenv("MNEMOS_LLM_PROVIDER", "ollama")
	t.Setenv("MNEMOS_LLM_MODEL", "qwen2.5:14b")
	t.Setenv("MNEMOS_LLM_BASE_URL", srv.URL)

	check := probeLLM(context.Background())

	if check.Status != "failed" {
		t.Errorf("probeLLM() status = %q, want %q when the model is not installed", check.Status, "failed")
	}
	if !strings.Contains(check.Detail, "qwen2.5:14b") {
		t.Errorf("probeLLM() detail = %q, want it to name the missing model", check.Detail)
	}
	if !strings.Contains(check.Detail, "ollama pull") {
		t.Errorf("probeLLM() detail = %q, want it to suggest the fix", check.Detail)
	}
}

func TestProbeLLMOKWhenOllamaModelInstalled(t *testing.T) {
	srv := ollamaStub(t, `{"models":[{"name":"qwen2.5:3b"}]}`)
	t.Setenv("MNEMOS_LLM_PROVIDER", "ollama")
	t.Setenv("MNEMOS_LLM_MODEL", "qwen2.5:3b")
	t.Setenv("MNEMOS_LLM_BASE_URL", srv.URL)

	if check := probeLLM(context.Background()); check.Status != "ok" {
		t.Errorf("probeLLM() status = %q (%s), want %q", check.Status, check.Detail, "ok")
	}
}

// TestProbeLLMWarnsWhenInventoryUnreadable keeps "cannot tell" from being
// reported as a hard failure: an unreachable daemon is advisory, not proof the
// model is gone.
func TestProbeLLMWarnsWhenInventoryUnreadable(t *testing.T) {
	srv := ollamaStub(t, `{"models":[]}`)
	url := srv.URL
	srv.Close()

	t.Setenv("MNEMOS_LLM_PROVIDER", "ollama")
	t.Setenv("MNEMOS_LLM_MODEL", "qwen2.5:3b")
	t.Setenv("MNEMOS_LLM_BASE_URL", url)

	if check := probeLLM(context.Background()); check.Status != "warn" {
		t.Errorf("probeLLM() status = %q (%s), want %q", check.Status, check.Detail, "warn")
	}
}

// TestProbeLLMSkipsModelCheckForPaidProviders preserves the existing contract
// that health checks never spend money: only a local inventory listing is free,
// so remote providers stay construction-only and must not be probed.
func TestProbeLLMSkipsModelCheckForPaidProviders(t *testing.T) {
	t.Setenv("MNEMOS_LLM_PROVIDER", "anthropic")
	t.Setenv("MNEMOS_LLM_API_KEY", "sk-test-not-a-real-key")
	t.Setenv("MNEMOS_LLM_MODEL", "claude-sonnet-5")

	if check := probeLLM(context.Background()); check.Status != "ok" {
		t.Errorf("probeLLM() status = %q (%s), want %q for a remote provider", check.Status, check.Detail, "ok")
	}
}

func TestProbeEmbeddingFailsWhenOllamaModelMissing(t *testing.T) {
	srv := ollamaStub(t, `{"models":[{"name":"qwen2.5:3b"}]}`)
	t.Setenv("MNEMOS_EMBED_PROVIDER", "ollama")
	t.Setenv("MNEMOS_EMBED_MODEL", "nomic-embed-text")
	t.Setenv("MNEMOS_EMBED_BASE_URL", srv.URL)

	check := probeEmbedding(context.Background())

	if check.Status != "failed" {
		t.Errorf("probeEmbedding() status = %q, want %q when the model is not installed", check.Status, "failed")
	}
	if !strings.Contains(check.Detail, "nomic-embed-text") {
		t.Errorf("probeEmbedding() detail = %q, want it to name the missing model", check.Detail)
	}
}

// TestProbeEmbeddingOKWhenModelInstalledUntagged covers the tag rule end to end:
// config names the model without a tag, Ollama stores it as ":latest".
func TestProbeEmbeddingOKWhenModelInstalledUntagged(t *testing.T) {
	srv := ollamaStub(t, `{"models":[{"name":"nomic-embed-text:latest"}]}`)
	t.Setenv("MNEMOS_EMBED_PROVIDER", "ollama")
	t.Setenv("MNEMOS_EMBED_MODEL", "nomic-embed-text")
	t.Setenv("MNEMOS_EMBED_BASE_URL", srv.URL)

	if check := probeEmbedding(context.Background()); check.Status != "ok" {
		t.Errorf("probeEmbedding() status = %q (%s), want %q", check.Status, check.Detail, "ok")
	}
}
