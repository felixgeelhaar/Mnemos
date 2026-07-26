// Package runscope enforces the F.4.b run-scoped token boundary: a bearer
// restricted to a set of run ids must not read or write across that boundary
// via evidence links.
//
// It lives here, rather than beside the HTTP handlers, because the check has to
// be identical on every transport that accepts evidence. It was package-private
// to cmd/mnemos, so gRPC AppendBeliefs could not call it and returned
// Unimplemented instead — fail-closed and honest, but a capability REST had and
// gRPC did not. A security check that exists on one transport is a security
// check someone will route around.
package runscope

import (
	"context"

	"go.klarlabs.de/mnemos/internal/store"
)

// checkEventRunsAllowed returns ("", nil) when every supplied event
// id maps to a run_id present in the allowed whitelist, otherwise
// it returns the first offending (eventID, runID) pair.
//
// Empty allowed slice short-circuits to allowed=true (no
// restriction). Unknown event ids are treated as "not in whitelist" —
// an agent referencing a nonexistent event from a write payload is
// suspicious and not worth distinguishing from a cross-run leak.
//
// Backend-agnostic: uses conn.Events.ListByIDs through the registry
// instead of raw SQL.
func CheckEventRunsAllowed(ctx context.Context, conn *store.Conn, eventIDs []string, allowed []string) (badEventID, badRunID string, err error) {
	if len(allowed) == 0 || len(eventIDs) == 0 {
		return "", "", nil
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		allowedSet[a] = struct{}{}
	}
	// Dedup the input.
	seen := map[string]struct{}{}
	uniq := make([]string, 0, len(eventIDs))
	for _, id := range eventIDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}

	events, err := conn.Events.ListByIDs(ctx, uniq)
	if err != nil {
		return "", "", err
	}
	found := make(map[string]string, len(events))
	for _, ev := range events {
		found[ev.ID] = ev.RunID
	}
	for _, id := range uniq {
		runID, ok := found[id]
		if !ok {
			return id, "", nil
		}
		if _, allowedRun := allowedSet[runID]; !allowedRun {
			return id, runID, nil
		}
	}
	return "", "", nil
}

// claimEventIDs returns the de-duplicated set of event ids that the
// given claim ids are linked to via claim_evidence. Used to derive
// the run-id surface for run-scope enforcement on relationship and
// embedding writes that reference claims rather than events directly.
func ClaimEventIDs(ctx context.Context, conn *store.Conn, claimIDs []string) ([]string, error) {
	if len(claimIDs) == 0 {
		return nil, nil
	}
	links, err := conn.Claims.ListEvidenceByClaimIDs(ctx, claimIDs)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(links))
	for _, link := range links {
		if _, dup := seen[link.EventID]; dup {
			continue
		}
		seen[link.EventID] = struct{}{}
		out = append(out, link.EventID)
	}
	return out, nil
}
