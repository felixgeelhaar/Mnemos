package main

import "go.klarlabs.de/mnemos/internal/llm"

// llmConfigured reports whether this machine can reach an LLM for the capture
// pipeline — either because a provider is named in the environment, or because
// one was auto-detected.
//
// It exists so no call site re-implements the question as
// `os.Getenv("MNEMOS_LLM_PROVIDER") != ""`. That check looks equivalent and is
// not: llm.ConfigFromEnv falls back to detecting a locally running Ollama, so
// the bare variable reads empty on the zero-config setup the project documents.
//
// Cheap to call: the Ollama probe is a 500ms localhost request memoised behind a
// sync.Once, so repeated calls in one process cost nothing after the first.
func llmConfigured() bool {
	_, err := llm.ConfigFromEnv()
	return err == nil
}
