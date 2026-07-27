package llm

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func tagServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOllamaModelPresentFindsInstalledModel(t *testing.T) {
	srv := tagServer(t, `{"models":[{"name":"qwen2.5:3b"},{"name":"llama3.2:1b"}]}`)

	present, err := OllamaModelPresent(srv.URL, "qwen2.5:3b")
	if err != nil {
		t.Fatalf("OllamaModelPresent() error = %v", err)
	}
	if !present {
		t.Error("OllamaModelPresent() = false, want true for an installed model")
	}
}

// TestOllamaModelPresentReportsMissingModel is the case that went undetected in
// production: Ollama was running and healthy, but the configured model had been
// removed, so every extraction 404'd while doctor reported the LLM "ok".
func TestOllamaModelPresentReportsMissingModel(t *testing.T) {
	srv := tagServer(t, `{"models":[]}`)

	present, err := OllamaModelPresent(srv.URL, "qwen2.5:14b")
	if err != nil {
		t.Fatalf("OllamaModelPresent() error = %v", err)
	}
	if present {
		t.Error("OllamaModelPresent() = true, want false when no models are installed")
	}
}

// TestOllamaModelPresentImpliesLatestTag covers the naming rule that would
// otherwise produce a false alarm: Ollama stores an untagged pull as
// "<name>:latest", but config (and `ollama pull`) refer to it without the tag.
func TestOllamaModelPresentImpliesLatestTag(t *testing.T) {
	srv := tagServer(t, `{"models":[{"name":"nomic-embed-text:latest"}]}`)

	for _, requested := range []string{"nomic-embed-text", "nomic-embed-text:latest"} {
		present, err := OllamaModelPresent(srv.URL, requested)
		if err != nil {
			t.Fatalf("OllamaModelPresent(%q) error = %v", requested, err)
		}
		if !present {
			t.Errorf("OllamaModelPresent(%q) = false, want true", requested)
		}
	}
}

// TestOllamaModelPresentErrorsWhenUnreachable keeps "cannot tell" distinct from
// "definitely missing", so an unreachable daemon is never reported as a missing
// model.
func TestOllamaModelPresentErrorsWhenUnreachable(t *testing.T) {
	srv := tagServer(t, `{"models":[]}`)
	url := srv.URL
	srv.Close()

	if _, err := OllamaModelPresent(url, "qwen2.5:3b"); err == nil {
		t.Error("OllamaModelPresent() error = nil, want an error when the daemon is unreachable")
	}
}
