package pipeline

import "testing"

func TestEpisodicFlagReadFromEnvNotJustMain(t *testing.T) {
	t.Setenv("MNEMOS_EPISODIC_EVENTS", "true")
	if !episodicTaggingEnabled() {
		t.Fatal("env must be honoured inside the package, so every entrypoint gets it")
	}
	t.Setenv("MNEMOS_EPISODIC_EVENTS", "")
	if episodicTaggingEnabled() {
		t.Fatal("unset must stay off — this one is opt-in by design (~78% precision)")
	}
	EnableEpisodicTagging = true
	defer func() { EnableEpisodicTagging = false }()
	if !episodicTaggingEnabled() {
		t.Fatal("the programmatic override must still work")
	}
}
