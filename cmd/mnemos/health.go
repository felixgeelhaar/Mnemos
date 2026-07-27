package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"go.klarlabs.de/mnemos/internal/embedding"
	"go.klarlabs.de/mnemos/internal/llm"
)

// healthCheck is one row in the deep health response. Status is one
// of "ok", "skipped" (no provider configured), "warn" (advisory —
// e.g. an orphaned home brain; does not fail health), or "failed".
// Detail carries either the failure message or a brief "what we
// tried" hint when ok.
type healthCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// healthCheckResult buckets the per-check results so a deep-mode
// caller can read pass/fail at a glance.
type healthCheckResult struct {
	Healthy bool          `json:"healthy"`
	Checks  []healthCheck `json:"checks"`
}

// runHealthChecks probes each subsystem with bounded latency. Each
// probe contributes one healthCheck row; the overall Healthy flag is
// false if any probe failed (skipped probes don't fail health — they
// just mean the operator hasn't configured that path).
func runHealthChecks(ctx context.Context, db *sql.DB) healthCheckResult {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	checks := []healthCheck{
		probeDB(probeCtx, db),
		probeLLM(probeCtx),
		probeEmbedding(probeCtx),
	}

	healthy := true
	for _, c := range checks {
		if c.Status == "failed" {
			healthy = false
		}
	}
	return healthCheckResult{Healthy: healthy, Checks: checks}
}

// probeDB writes a sentinel value into a real but harmless table
// (revoked_tokens — keys are JWT IDs, easy to make a clearly-fake
// one) inside a transaction that's always rolled back, so we
// confirm WAL/journal/file permissions without leaving any data.
func probeDB(ctx context.Context, db *sql.DB) healthCheck {
	if db == nil {
		return healthCheck{Name: "sqlite", Status: "failed", Detail: "no DB handle"}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return healthCheck{Name: "sqlite", Status: "failed", Detail: err.Error()}
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO revoked_tokens (jti, revoked_at, expires_at) VALUES (?, ?, ?)`,
		"healthcheck-"+time.Now().UTC().Format(time.RFC3339Nano),
		time.Now().UTC().Format(time.RFC3339Nano),
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return healthCheck{Name: "sqlite", Status: "failed", Detail: "write probe: " + err.Error()}
	}
	return healthCheck{Name: "sqlite", Status: "ok", Detail: "write probe rolled back"}
}

// probeLLM verifies the configured LLM provider can be constructed.
// We don't actually call out to the provider here — that would
// charge the user money on every health check. A construction-time
// success means env vars decode cleanly and the client picked a
// transport; the first real call still has to validate end-to-end.
//
// The one exception is Ollama, where the model inventory is local and
// free to read — see checkOllamaModel.
//
// "skipped" means no provider env var is set AND no Ollama is
// running — the operator hasn't asked for LLM features. "failed"
// means a provider was requested but the config or client construction
// is broken.
func probeLLM(_ context.Context) healthCheck {
	if !llmExplicitlyConfigured() && !llm.OllamaAvailable() {
		return healthCheck{Name: "llm", Status: "skipped", Detail: "no provider configured (set MNEMOS_LLM_PROVIDER or run Ollama)"}
	}
	cfg, err := llm.ConfigFromEnv()
	if err != nil {
		return healthCheck{Name: "llm", Status: "failed", Detail: err.Error()}
	}
	if _, err := llm.NewClient(cfg); err != nil {
		return healthCheck{Name: "llm", Status: "failed", Detail: err.Error()}
	}
	return checkOllamaModel("llm", cfg.Provider, cfg.Model, cfg.BaseURL)
}

// probeEmbedding mirrors probeLLM for the embedding provider.
// Same trade-off: construction-time only, no network call.
func probeEmbedding(_ context.Context) healthCheck {
	if !embeddingExplicitlyConfigured() && !llm.OllamaAvailable() {
		return healthCheck{Name: "embedding", Status: "skipped", Detail: "no provider configured (set MNEMOS_EMBED_PROVIDER or run Ollama)"}
	}
	cfg, err := embedding.ConfigFromEnv()
	if err != nil {
		return healthCheck{Name: "embedding", Status: "failed", Detail: err.Error()}
	}
	if _, err := embedding.NewClient(cfg); err != nil {
		return healthCheck{Name: "embedding", Status: "failed", Detail: err.Error()}
	}
	return checkOllamaModel("embedding", cfg.Provider, cfg.Model, cfg.BaseURL)
}

// checkOllamaModel turns a constructed provider config into the final health
// result, verifying for Ollama that the configured model is actually installed.
//
// A reachable daemon is not evidence the model is there. Ollama answers a
// request for a missing model with a 404 per call, so the pipeline keeps
// running while every extraction fails — a degradation that is invisible
// precisely because the process stays healthy. Observed in production: the
// models had been removed, doctor reported "llm ok" from construction alone,
// and capture ran rule-based for days with nothing surfacing it.
//
// This stays within the no-paid-calls rule that governs the probes above:
// listing installed models is local and free, so it is checked; inference is
// not. Remote providers are returned untouched.
//
// An unreadable inventory is "warn", never "failed": not being able to ask is
// not the same as knowing the model is gone.
func checkOllamaModel(name string, provider llm.Provider, model, baseURL string) healthCheck {
	detail := fmt.Sprintf("provider=%s model=%s", provider, model)
	if provider != llm.ProviderOllama || strings.TrimSpace(model) == "" {
		return healthCheck{Name: name, Status: "ok", Detail: detail}
	}

	present, err := llm.OllamaModelPresent(baseURL, model)
	switch {
	case err != nil:
		return healthCheck{
			Name:   name,
			Status: "warn",
			Detail: fmt.Sprintf("%s — could not verify the model is installed: %v", detail, err),
		}
	case !present:
		return healthCheck{
			Name:   name,
			Status: "failed",
			Detail: fmt.Sprintf("%s is not installed; every call will fail with 404 — run: ollama pull %s", detail, model),
		}
	}
	return healthCheck{Name: name, Status: "ok", Detail: detail}
}

func llmExplicitlyConfigured() bool {
	return strings.TrimSpace(os.Getenv("MNEMOS_LLM_PROVIDER")) != ""
}

func embeddingExplicitlyConfigured() bool {
	return strings.TrimSpace(os.Getenv("MNEMOS_EMBED_PROVIDER")) != "" ||
		strings.TrimSpace(os.Getenv("MNEMOS_LLM_PROVIDER")) != ""
}
