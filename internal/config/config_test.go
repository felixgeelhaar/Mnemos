package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "mnemos.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadAndEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
db:
  url: postgres://localhost/mnemos
  max_conns: 25
llm:
  provider: anthropic
  api_key: sk-test
  timeout: 30s
serve:
  port: 8080
feedback:
  decay: 0.9
federation:
  enabled: true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	env := cfg.EnvOverrides()

	want := map[string]string{
		"MNEMOS_DB_URL":             "postgres://localhost/mnemos",
		"MNEMOS_DB_MAX_CONNS":       "25",
		"MNEMOS_LLM_PROVIDER":       "anthropic",
		"MNEMOS_LLM_API_KEY":        "sk-test",
		"MNEMOS_LLM_TIMEOUT":        "30s",
		"MNEMOS_SERVE_PORT":         "8080",
		"MNEMOS_FEEDBACK_DECAY":     "0.9",
		"MNEMOS_FEDERATION_ENABLED": "true",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, env[k], v)
		}
	}
	// Empty leaves must not appear (would shadow downstream defaults).
	if _, ok := env["MNEMOS_LLM_MODEL"]; ok {
		t.Errorf("empty leaf MNEMOS_LLM_MODEL should be omitted")
	}
}

// EnvOverrides is the single registry of settings the config file can supply.
// Adding a MNEMOS_* var to the code without a YAML leaf makes it env-only —
// invisible in a user's config and unreviewable in a deployment. This asserts
// the registry stays complete for the sections we have deliberately mapped, so
// a new env-only knob is a conscious choice rather than drift.
//
// It reads a fully-populated config rather than enumerating names, so a new
// leaf is covered automatically once it is added to the fixture.
func TestEnvOverridesRegistryIsComplete(t *testing.T) {
	var c Config
	// Every section must be represented here; a new section with no entry is
	// the drift this test exists to catch.
	sections := map[string]int{
		"MNEMOS_DB_":        0,
		"MNEMOS_LLM_":       0,
		"MNEMOS_EMBED_":     0,
		"MNEMOS_SERVE_":     0,
		"MNEMOS_TELEMETRY_": 0,
	}
	c.DB.URL = "x"
	c.LLM.Provider = "x"
	c.LLM.ExtractBatchChars = "1"
	c.Embed.Provider = "x"
	c.Serve.Port = "1"
	c.Serve.PublicReads = "true"
	c.Telemetry.OptIn = "1"
	c.Query.Hebbian = "true"
	c.Capture.Timeout = "1m"

	env := c.EnvOverrides()
	for k := range env {
		for prefix := range sections {
			if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
				sections[prefix]++
			}
		}
	}
	for prefix, n := range sections {
		if n == 0 {
			t.Errorf("no %s* setting reached EnvOverrides; the section is not wired up", prefix)
		}
	}
	// The knobs that were env-only before this audit must stay reachable.
	for _, k := range []string{
		"MNEMOS_EXTRACT_BATCH_CHARS", "MNEMOS_PUBLIC_READS",
		"MNEMOS_HEBBIAN", "MNEMOS_CAPTURE_TIMEOUT",
	} {
		if _, ok := env[k]; !ok {
			t.Errorf("%s regressed to env-only; it must be settable in mnemos.yaml", k)
		}
	}
}

// The YAML schema is the reviewable surface: a setting reachable only through
// an environment variable is one a user cannot see in their config. This locks
// in the query/capture/serve/llm knobs that were env-only, so a new one is a
// deliberate choice rather than an oversight.
func TestQueryTogglesReachableFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
query:
  spreading_activation: true
  salience: true
  hebbian: true
  reconsolidate: true
  inhibit: true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	env := cfg.EnvOverrides()
	for _, k := range []string{
		"MNEMOS_SPREADING_ACTIVATION", "MNEMOS_SALIENCE",
		"MNEMOS_HEBBIAN", "MNEMOS_RECONSOLIDATE", "MNEMOS_INHIBIT",
	} {
		if env[k] != "true" {
			t.Errorf("env[%s] = %q, want %q", k, env[k], "true")
		}
	}
}

// Unset leaves must not emit an env var: an empty value would shadow the
// downstream default and silently turn a toggle off (or on) in ways the file
// does not say.
func TestUnsetQueryTogglesEmitNothing(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "query:\n  salience: true\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	env := cfg.EnvOverrides()
	if _, ok := env["MNEMOS_HEBBIAN"]; ok {
		t.Error("unset query.hebbian must not emit MNEMOS_HEBBIAN")
	}
	if env["MNEMOS_SALIENCE"] != "true" {
		t.Errorf("set leaf lost: %q", env["MNEMOS_SALIENCE"])
	}
}

// Every MNEMOS_* setting a user is expected to tune should be reachable from
// mnemos.yaml — the file exists so a deployment is reviewable in one place
// instead of reconstructed from a process's environment.
func TestTuningAndAuthPostureReachableFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
llm:
  extract_batch_chars: 8000
serve:
  public_reads: true
  metrics_public: false
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	env := cfg.EnvOverrides()
	for k, want := range map[string]string{
		"MNEMOS_EXTRACT_BATCH_CHARS": "8000",
		"MNEMOS_PUBLIC_READS":        "true",
		"MNEMOS_METRICS_PUBLIC":      "false",
	} {
		if env[k] != want {
			t.Errorf("env[%s] = %q, want %q", k, env[k], want)
		}
	}
}

func TestCaptureBlockEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
capture:
  strategy: incremental
  timeout: 5m
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.EnvOverrides()["MNEMOS_CAPTURE_TIMEOUT"]; got != "5m" {
		t.Errorf("MNEMOS_CAPTURE_TIMEOUT = %q, want %q", got, "5m")
	}
	if got := cfg.EnvOverrides()["MNEMOS_CAPTURE_STRATEGY"]; got != "incremental" {
		t.Errorf("MNEMOS_CAPTURE_STRATEGY = %q, want %q", got, "incremental")
	}
}

// The hosted brain's sleep cadence must be settable from the config file, not
// only from the environment: how often a production server consolidates is a
// deployment decision that belongs in reviewable config.
func TestServeConsolidateIntervalEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "serve:\n  consolidate_interval: 6h\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.EnvOverrides()["MNEMOS_CONSOLIDATE_INTERVAL"]; got != "6h" {
		t.Errorf("MNEMOS_CONSOLIDATE_INTERVAL = %q, want 6h", got)
	}

	// Unset must not shadow the built-in default with "".
	empty := writeConfig(t, dir, "serve:\n  port: 9000\n")
	ecfg, err := Load(empty)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := ecfg.EnvOverrides()["MNEMOS_CONSOLIDATE_INTERVAL"]; ok {
		t.Error("an unset consolidate_interval must not reach the environment")
	}
}

func TestServerBlockEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
server:
  url: https://brain.example.com
  token: tok-abc
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	env := cfg.EnvOverrides()
	if env["MNEMOS_URL"] != "https://brain.example.com" {
		t.Errorf("MNEMOS_URL = %q", env["MNEMOS_URL"])
	}
	if env["MNEMOS_TOKEN"] != "tok-abc" {
		t.Errorf("MNEMOS_TOKEN = %q", env["MNEMOS_TOKEN"])
	}
}

func TestPrecedenceBlockEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "precedence: global-wins\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if env := cfg.EnvOverrides(); env["MNEMOS_PRECEDENCE"] != "global-wins" {
		t.Errorf("MNEMOS_PRECEDENCE = %q, want global-wins", env["MNEMOS_PRECEDENCE"])
	}
	// An empty precedence leaf must be omitted so it never shadows the default.
	empty := writeConfig(t, dir, "db:\n  url: memory://\n")
	ecfg, err := Load(empty)
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if _, ok := ecfg.EnvOverrides()["MNEMOS_PRECEDENCE"]; ok {
		t.Error("empty precedence leaf should be omitted")
	}
}

func TestFloatbackOnCaptureEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "floatback:\n  on_capture: true\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if env := cfg.EnvOverrides(); env["MNEMOS_FLOATBACK_ON_CAPTURE"] != "true" {
		t.Errorf("MNEMOS_FLOATBACK_ON_CAPTURE = %q, want true", env["MNEMOS_FLOATBACK_ON_CAPTURE"])
	}
	// Default: an absent floatback block leaves the key unset so it never shadows
	// the downstream default (off).
	empty := writeConfig(t, dir, "db:\n  url: memory://\n")
	ecfg, err := Load(empty)
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if _, ok := ecfg.EnvOverrides()["MNEMOS_FLOATBACK_ON_CAPTURE"]; ok {
		t.Error("empty floatback.on_capture leaf should be omitted (default off)")
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "llm:\n  provdier: typo\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "llm: [unterminated\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
}

func TestHydrateEnvironmentWins(t *testing.T) {
	t.Setenv("MNEMOS_LLM_PROVIDER", "openai") // operator's export
	cfg := &Config{}
	cfg.LLM.Provider = "anthropic" // config file says otherwise
	cfg.LLM.Model = "claude-opus-4-8"

	applied, err := Hydrate(cfg)
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if got := os.Getenv("MNEMOS_LLM_PROVIDER"); got != "openai" {
		t.Errorf("environment should win: MNEMOS_LLM_PROVIDER = %q, want openai", got)
	}
	if got := os.Getenv("MNEMOS_LLM_MODEL"); got != "claude-opus-4-8" {
		t.Errorf("config should fill gap: MNEMOS_LLM_MODEL = %q", got)
	}
	// Only the gap-filled key is reported as applied.
	for _, k := range applied {
		if k == "MNEMOS_LLM_PROVIDER" {
			t.Errorf("Hydrate must not report an already-set var as applied")
		}
	}
}

func TestDiscoverPrecedence(t *testing.T) {
	dir := t.TempDir()

	// Explicit path wins even over env.
	t.Setenv(EnvConfigPath, "/from/env.yaml")
	if p, ok := Discover("/explicit.yaml"); !ok || p != "/explicit.yaml" {
		t.Errorf("explicit should win: got %q, %v", p, ok)
	}

	// Env used when no explicit path.
	if p, ok := Discover(""); !ok || p != "/from/env.yaml" {
		t.Errorf("env path expected: got %q, %v", p, ok)
	}

	// Walk-up discovery in a project dir. Empty reads as unset (Discover
	// treats "" as absent), and t.Setenv restores it after the test.
	t.Setenv(EnvConfigPath, "")
	proj := filepath.Join(dir, "proj")
	mn := filepath.Join(proj, ".mnemos")
	if err := os.MkdirAll(mn, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(mn, "mnemos.yaml")
	if err := os.WriteFile(cfgPath, []byte("llm:\n  provider: anthropic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(proj, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, sub)
	defer restore()
	// Resolve symlinks: on macOS /var is a symlink to /private/var, so the
	// working directory reported by os.Getwd (and thus the discovered path)
	// is the resolved form while cfgPath is not.
	wantPath, _ := filepath.EvalSymlinks(cfgPath)
	if p, ok := Discover(""); !ok || p != wantPath {
		t.Errorf("walk-up discovery expected %q: got %q, %v", wantPath, p, ok)
	}
}

func TestLoadAndHydrateMissingExplicitIsError(t *testing.T) {
	if _, _, err := LoadAndHydrate("/definitely/not/here.yaml"); err == nil {
		t.Fatal("missing explicit config should error")
	}
}

func TestLoadAndHydrateMissingImplicitIsFine(t *testing.T) {
	t.Setenv(EnvConfigPath, "") // empty reads as unset
	// chdir to an empty temp dir with no .mnemos and a fake XDG home.
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg-empty"))
	t.Setenv("HOME", dir)
	path, applied, err := LoadAndHydrate("")
	if err != nil {
		t.Fatalf("implicit miss should not error: %v", err)
	}
	if path != "" || applied != nil {
		t.Errorf("implicit miss should report no path/applied: %q, %v", path, applied)
	}
}

func chdir(t *testing.T, dir string) func() {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.Chdir(prev) }
}
