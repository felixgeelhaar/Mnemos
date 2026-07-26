package query

import (
	"testing"

	"go.klarlabs.de/mnemos/internal/domain"
)

// TestFilterByQuery is the regression for a field that was declared,
// documented, accepted by POST /v1/context and plumbed all the way into
// BuildContextBlock — and then never read.
//
// A caller asking for "database migrations" received the entire run's context
// block, so MaxTokens truncated the claims they wanted in favour of ones they
// had not asked for. No error, a perfectly well-formed block, the wrong
// content: the failure mode with no symptom.
func TestFilterByQuery(t *testing.T) {
	claims := []domain.Claim{
		{ID: "a", Text: "The database migration failed on the staging replica"},
		{ID: "b", Text: "Checkout latency regressed after the deploy"},
		{ID: "c", Text: "Migrations now run in a separate init container"},
	}

	t.Run("empty query keeps everything (the documented fallback)", func(t *testing.T) {
		t.Parallel()

		if got := filterByQuery(claims, ""); len(got) != len(claims) {
			t.Errorf("got %d claims, want all %d — an empty Query means 'this whole run'", len(got), len(claims))
		}
		if got := filterByQuery(claims, "   "); len(got) != len(claims) {
			t.Errorf("whitespace-only query must behave as empty, got %d", len(got))
		}
	})

	t.Run("a query narrows to matching claims", func(t *testing.T) {
		t.Parallel()

		got := filterByQuery(claims, "Migration")
		if len(got) == len(claims) {
			t.Fatal("filter did nothing — this is exactly the bug: the field was accepted and ignored")
		}

		ids := map[string]bool{}
		for _, c := range got {
			ids[c.ID] = true
		}
		// Case-insensitive, because docTokens lowercases.
		if !ids["a"] {
			t.Errorf("want the matching claim, got %v", ids)
		}
		if ids["b"] {
			t.Error("the unrelated latency claim must not survive the filter")
		}
		// Documents a real limitation rather than hiding it: docTokens does not
		// stem, so the plural does NOT match the singular. Kept deliberately —
		// see filterByQuery. If this ever starts passing, the tokenizer gained
		// stemming and the retrieval path changed with it, which is the only
		// way this should change.
		if ids["c"] {
			t.Error("unexpected: \"Migrations\" matched \"Migration\" — the tokenizer now stems, " +
				"so update filterByQuery's comment and this expectation together")
		}
	})

	t.Run("no match yields nothing, not everything", func(t *testing.T) {
		t.Parallel()

		if got := filterByQuery(claims, "kubernetes ingress"); len(got) != 0 {
			t.Errorf("got %d claims, want 0 — silently widening a filter back to "+
				"the full set is the behaviour being fixed", len(got))
		}
	})
}
