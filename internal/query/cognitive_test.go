package query

import "testing"

// TestCognitiveEnabled_DefaultsOn pins the inversion: unset means ON.
//
// These behaviours are the memory model, not enhancements. The previous
// default-off meant a brain that never primed associations, never strengthened
// what fired together, and never refreshed what it recalled — and because a
// memory that quietly stops learning produces no error and no missing result,
// nothing would ever have surfaced it.
func TestCognitiveEnabled_DefaultsOn(t *testing.T) {
	for _, name := range []string{
		envSpreadingActivation, envSalience, envHebbian, envReconsolidate, envInhibit,
	} {
		t.Setenv(name, "")
		if !cognitiveEnabled(name) {
			t.Errorf("%s: unset must mean ON — these are the memory model, not opt-in extras", name)
		}
	}
}

// Opting out must stay possible: a replica or a forensic copy is a real
// deployment, and "recall rewrites the store" is genuinely wrong there.
func TestCognitiveEnabled_ExplicitOptOut(t *testing.T) {
	for _, falsy := range []string{"0", "false", "no", "off", "FALSE", " Off "} {
		t.Setenv(envHebbian, falsy)
		if cognitiveEnabled(envHebbian) {
			t.Errorf("%q must disable the behaviour", falsy)
		}
	}
	for _, truthy := range []string{"1", "true", "yes", "on", "TRUE"} {
		t.Setenv(envHebbian, truthy)
		if !cognitiveEnabled(envHebbian) {
			t.Errorf("%q must leave the behaviour on", truthy)
		}
	}
}

// A typo must not silently disable learning. "flase" is not a recognised falsy
// value, and the safe reading of an unparseable value is "keep working" — the
// alternative fails silently and permanently.
func TestCognitiveEnabled_UnparseableStaysOn(t *testing.T) {
	for _, junk := range []string{"flase", "nope", "disabled", "2"} {
		t.Setenv(envInhibit, junk)
		if !cognitiveEnabled(envInhibit) {
			t.Errorf("%q is not a recognised falsy value; it must not silently disable the behaviour", junk)
		}
	}
}

// WithCognitiveDefaults must never downgrade an explicit caller choice, and must
// leave unrelated fields alone.
func TestWithCognitiveDefaults_PreservesCallerIntentAndOtherFields(t *testing.T) {
	t.Setenv(envHebbian, "false")

	got := AnswerOptions{Hops: 3, MinTrust: 0.4, Hebbian: true}.WithCognitiveDefaults()

	if !got.Hebbian {
		t.Error("an explicit Hebbian:true must survive an env var that disables it — a per-query flag is a stronger signal than a default")
	}
	if got.Hops != 3 || got.MinTrust != 0.4 {
		t.Errorf("unrelated fields were modified: hops=%d minTrust=%v", got.Hops, got.MinTrust)
	}
	if !got.Prime || !got.Salient || !got.Reconsolidate || !got.Inhibit {
		t.Errorf("the other four must default on: %+v", got)
	}
}
