package grpc

import (
	"context"

	"go.klarlabs.de/mnemos/internal/store"
)

// The F.4.b run-scope boundary, as the gRPC surface needs it.
//
// A bearer whose token carries a run whitelist ("run" claim) must not read or
// write outside those runs. The REST surface has enforced this since F.4; gRPC
// enforced it on nothing, so the *same token* was a tighter grant over HTTP
// than over gRPC — and a caller who wanted the looser one only had to change
// transport. These helpers exist so the gRPC methods below can apply the same
// rule at the same point in the request: before the write, never after.
//
// Two shapes of reference have to be resolved to a run:
//   - an episode id, which carries its run directly;
//   - a belief id, whose run is only reachable through its evidence links.
//
// Both collapse to "does every episode this write touches belong to a run the
// token was granted".

// runAllowed reports whether runID is inside the bearer's whitelist. An empty
// whitelist means the token is unrestricted, which is the pre-F.4 posture and
// the one every tokenless/local deployment relies on.
func runAllowed(allowed []string, runID string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == runID {
			return true
		}
	}
	return false
}

// checkEventRunsAllowed returns ("", "", nil) when every supplied episode id
// resolves to a run inside allowed, otherwise the first offending (id, run).
//
// An unknown id is refused rather than ignored: distinguishing "this episode
// does not exist" from "this episode is another tenant's" would answer an
// existence question for an id the caller was never shown, and a write naming
// an episode it cannot see is not a case worth being generous to.
func checkEventRunsAllowed(ctx context.Context, conn *store.Conn, eventIDs, allowed []string) (badEventID, badRunID string, err error) {
	if len(allowed) == 0 || len(eventIDs) == 0 {
		return "", "", nil
	}
	uniq := dedup(eventIDs)
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
		if !runAllowed(allowed, runID) {
			return id, runID, nil
		}
	}
	return "", "", nil
}

// claimEventIDs resolves belief ids to the episode ids their evidence links
// point at — the only route from a belief to a run. A belief with no evidence
// contributes nothing, so a write referencing one is not blocked by the run
// check; the ≥1-evidence invariant is enforced where beliefs are created.
func claimEventIDs(ctx context.Context, conn *store.Conn, claimIDs []string) ([]string, error) {
	if len(claimIDs) == 0 {
		return nil, nil
	}
	links, err := conn.Claims.ListEvidenceByClaimIDs(ctx, dedup(claimIDs))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(links))
	for _, link := range links {
		out = append(out, link.EventID)
	}
	return dedup(out), nil
}

func dedup(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
