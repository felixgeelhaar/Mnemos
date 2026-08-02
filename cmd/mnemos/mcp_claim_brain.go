package main

import (
	"context"
	"fmt"
	"strings"

	"go.klarlabs.de/mcp/protocol"
)

// Brain resolution for the MCP tools that act on a belief BY ID.
//
// query_knowledge federates the global brain with this session's
// repo/workspace overlay (mcpRunQueryScoped, ADR 0010 + ADR 0011 Phase C), so
// an agent routinely holds a belief id that exists ONLY in the workspace brain
// — query_knowledge even tags it `belief_provenance: "repo"`. The by-id tools
// did not federate: they opened the process's own (global) store, found
// nothing, and returned a bare Go error, which the MCP boundary sanitises into
// `-32603 internal error` (mcp's publicError replaces any non-protocol error
// with a generic internal error so nothing leaks).
//
// The consequence is the point of #341: recall surfaces a stale workspace
// belief and there is NO API path to correct it. An agent can read the wrong
// answer and quote it, but cannot fetch, deprecate, forget or resolve it. The
// reporter had a superseded clinical-safety belief live in recall for weeks and
// had to fix it by editing the workspace SQLite file by hand — including the
// claim_status_history row, i.e. hand-maintaining the audit invariant the API
// exists to hold.
//
// Two properties are load-bearing:
//
//  1. A WRITE MUST LAND IN THE BRAIN THAT HOLDS THE BELIEF. Deprecating a
//     workspace belief against the global brain either does nothing or writes a
//     phantom row that no read path will ever reconcile with the original. So
//     the owning brain is resolved from the id BEFORE the writer is opened, and
//     it is pinned on the context the writer is built from (withDSNOverride)
//     rather than left to resolveDSN(). Callers must use the RETURNED context.
//
//  2. AN ID IN NO REACHABLE BRAIN IS A `NOT FOUND`, NOT AN INTERNAL ERROR.
//     -32603 reads as a server fault, indistinguishable from a scoping
//     boundary; fixing the scoping and leaving the error opaque would leave the
//     next person equally stuck. A *protocol.Error passes through publicError
//     verbatim, so the caller gets -32001 plus the brains that were searched.

const (
	// claimBrainGlobal / claimBrainRepo use the same vocabulary as
	// query_knowledge's belief_provenance, so an agent that read an id from a
	// federated query recognises the tier it was resolved to.
	claimBrainGlobal = "global"
	claimBrainRepo   = "repo"
)

// claimBrain names the brain a set of belief ids resolved to. DSN is empty for
// the process's own brain, where no context override is needed (and, in
// multi-tenant mode, must not be applied — an un-tenanted override is refused
// by resolveDSNForContext).
type claimBrain struct {
	Tier string
	DSN  string
}

// mcpScopeToClaimBrain resolves which brain holds every supplied belief id and
// returns a context pinned to it. Callers pass the returned context to
// openConn/openWriter so the read AND the write both address that brain.
//
// Resolution order matters: the repo/workspace overlay is probed first and wins
// when both brains hold the same id, because query_knowledge merges the tiers
// repo-first under the default precedence policy — so the copy the agent
// actually read, and therefore means to correct, is the workspace one. Writing
// to the global copy instead would silently no-op against the belief the agent
// saw.
//
// In multi-tenant mode mcpRepoBrainDSN() is empty by design (the overlay
// resolves from the SERVER's cwd, so it is the same un-tenanted file for every
// tenant), so this degrades to "probe the request's own tenant-scoped brain"
// and the isolation boundary is unchanged.
func mcpScopeToClaimBrain(ctx context.Context, ids ...string) (context.Context, claimBrain, error) {
	want := dedupeClaimIDs(ids)
	if len(want) == 0 {
		return ctx, claimBrain{}, protocol.NewInvalidParams("a belief id is required")
	}

	globalHas, globalErr := claimIDsPresent(ctx, want)

	repoDSN := mcpRepoBrainDSN()
	repoCtx := ctx
	var repoHas map[string]bool
	if repoDSN != "" {
		repoCtx = withDSNOverride(ctx, repoDSN)
		// A broken overlay must not take the global path down with it: an
		// unreadable workspace file should still let a global belief be
		// corrected. The probe error is deliberately dropped here and only the
		// global one is reported if nothing resolves.
		repoHas, _ = claimIDsPresent(repoCtx, want)
	}

	switch {
	case len(repoHas) == len(want):
		return repoCtx, claimBrain{Tier: claimBrainRepo, DSN: repoDSN}, nil
	case len(globalHas) == len(want):
		return ctx, claimBrain{Tier: claimBrainGlobal}, nil
	}

	if globalErr != nil {
		// The store itself could not be read. That is a genuine internal fault,
		// not a missing belief — do not dress it up as a not-found.
		return ctx, claimBrain{}, globalErr
	}

	searched := describeSearchedBrains(ctx, repoDSN)
	var missing []string
	for _, id := range want {
		if !repoHas[id] && !globalHas[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return ctx, claimBrain{}, protocol.NewNotFound(fmt.Sprintf(
			"belief %s not found in any reachable brain (searched %s)",
			strings.Join(missing, ", "), searched))
	}
	// Every id exists, but no single brain holds them all. This API cannot write
	// atomically across two stores, and picking one brain would silently drop the
	// other side of the pair — so refuse and say exactly why.
	return ctx, claimBrain{}, protocol.NewInvalidParams(fmt.Sprintf(
		"beliefs %s are held by different brains (searched %s); act on beliefs that share one brain",
		strings.Join(want, ", "), searched))
}

// claimIDsPresent reports which of ids the brain addressed by ctx holds.
func claimIDsPresent(ctx context.Context, ids []string) (map[string]bool, error) {
	conn, err := openConn(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn(conn)
	rows, err := conn.Claims.ListByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	found := make(map[string]bool, len(rows))
	for _, c := range rows {
		found[c.ID] = true
	}
	return found, nil
}

// dedupeClaimIDs trims, drops empties, and removes duplicates while preserving
// order. Deduping is what lets the callers above compare len(found) == len(want)
// to mean "this brain holds all of them".
func dedupeClaimIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// describeSearchedBrains renders the brains a lookup covered, for the not-found
// message. DSNs are redacted because a networked brain's DSN can carry a
// password and this string reaches the caller verbatim.
func describeSearchedBrains(ctx context.Context, repoDSN string) string {
	global := "the configured brain"
	if dsn, err := resolveDSNForContext(ctx); err == nil {
		global = "global " + redactDSN(dsn)
	}
	if repoDSN == "" {
		return global
	}
	return "workspace " + redactDSN(repoDSN) + " and " + global
}
