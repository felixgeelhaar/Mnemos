package main

import (
	"context"
	"path/filepath"
	"testing"

	"go.klarlabs.de/mnemos/internal/auth"
	"go.klarlabs.de/mnemos/internal/store"
)

// record_action persists input.RunID verbatim onto the Action, but the handler
// never called enforceRunScope — so a token restricted to one run could write
// Actions into any run, and could also write unscoped ones (the guard's
// fail-closed empty-run branch was skipped entirely).

func TestMCPRunRecordAction_EnforcesRunScope(t *testing.T) {
	ctx := runScopedCtx("alpha")
	base := mcpRecordActionInput{Kind: "deploy", Subject: "api"}

	beta := base
	beta.RunID = "beta"
	if _, err := mcpRunRecordAction(ctx, "usr_alice", beta); err == nil {
		t.Error("recording an action into a run outside the allowlist must be denied")
	}

	if _, err := mcpRunRecordAction(ctx, "usr_alice", base); err == nil {
		t.Error("an unscoped action from a run-restricted token must be denied (fail-closed)")
	}

	blank := base
	blank.RunID = "  "
	if _, err := mcpRunRecordAction(ctx, "usr_alice", blank); err == nil {
		t.Error("a blank run id must not bypass the guard")
	}
}

func TestMCPRunRecordAction_AllowedRunAndUnrestrictedCallersStillWrite(t *testing.T) {
	dsn := "sqlite://" + filepath.Join(t.TempDir(), "brain.db")
	t.Setenv("MNEMOS_DB_URL", dsn)

	// A token allowed on "alpha" writes into alpha.
	out, err := mcpRunRecordAction(runScopedCtx("alpha"), "usr_alice",
		mcpRecordActionInput{Kind: "deploy", Subject: "api", RunID: "alpha"})
	if err != nil {
		t.Fatalf("allowed run must be writable: %v", err)
	}
	if out.ID == "" {
		t.Fatal("expected an action id")
	}

	// stdio (no claims) keeps writing unscoped actions.
	if _, err := mcpRunRecordAction(context.Background(), "usr_alice",
		mcpRecordActionInput{Kind: "deploy", Subject: "api"}); err != nil {
		t.Fatalf("unauthenticated caller must be unaffected: %v", err)
	}

	// An authenticated token with no run allowlist is unrestricted too.
	unrestricted := withClaims(context.Background(), &auth.Claims{})
	if _, err := mcpRunRecordAction(unrestricted, "usr_alice",
		mcpRecordActionInput{Kind: "deploy", Subject: "api"}); err != nil {
		t.Fatalf("token without a run allowlist must be unaffected: %v", err)
	}

	conn, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = conn.Close() }()
	scoped, err := conn.Actions.ListByRunID(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(scoped) != 1 {
		t.Fatalf("want exactly the one alpha action, got %d", len(scoped))
	}
}
