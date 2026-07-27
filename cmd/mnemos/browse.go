package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/store"
)

const (
	defaultListLimit = 50
	maxListLimit     = 200
)

type mcpListClaimsInput struct {
	Type   string `json:"type,omitempty" jsonschema:"description=Filter by claim type: fact, hypothesis, or decision"`
	Status string `json:"status,omitempty" jsonschema:"description=Filter by claim status: active, contested, or deprecated"`
	RunID  string `json:"runId,omitempty" jsonschema:"description=Restrict to claims whose evidence comes from this run. Required when the caller's token carries a run allowlist."`
	Limit  int    `json:"limit,omitempty" jsonschema:"description=Max number of claims to return (default 50, cap 200)"`
	Offset int    `json:"offset,omitempty" jsonschema:"description=Number of claims to skip"`
}

type mcpListClaimsOutput struct {
	Claims []domain.Claim `json:"claims"`
	Total  int            `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

type mcpListContradictionsInput struct {
	RunID  string `json:"runId,omitempty" jsonschema:"description=Restrict to contradictions whose BOTH claims come from this run. Required when the caller's token carries a run allowlist."`
	Limit  int    `json:"limit,omitempty" jsonschema:"description=Max number of contradictions to return (default 50, cap 200)"`
	Offset int    `json:"offset,omitempty" jsonschema:"description=Number of contradictions to skip"`
}

type mcpContradictionPair struct {
	RelationshipID string `json:"relationshipId"`
	FromClaimID    string `json:"fromClaimId"`
	FromClaimText  string `json:"fromClaimText"`
	ToClaimID      string `json:"toClaimId"`
	ToClaimText    string `json:"toClaimText"`
	CreatedAt      string `json:"createdAt"`
}

type mcpListContradictionsOutput struct {
	Contradictions []mcpContradictionPair `json:"contradictions"`
	Total          int                    `json:"total"`
	Limit          int                    `json:"limit"`
	Offset         int                    `json:"offset"`
}

// The browse tools (list_beliefs / list_decisions / list_dissonances) hand back
// raw knowledge-base rows, so they are just as run-sensitive as query_knowledge
// — and they used to skip enforceRunScope entirely, letting a token minted for
// one run page through the whole brain. Both entry points now run the same
// guard as every other run-carrying tool: a run outside the allowlist is
// denied, and an unscoped listing from a run-restricted token is denied
// fail-closed (it would span every run). Unauthenticated stdio callers and
// tokens without an allowlist are unaffected.
func mcpRunListClaims(ctx context.Context, input mcpListClaimsInput) (mcpListClaimsOutput, error) {
	limit, offset := normalizePagination(input.Limit, input.Offset)

	if input.Type != "" && !validClaimType(input.Type) {
		return mcpListClaimsOutput{}, fmt.Errorf("invalid type %q (want fact, hypothesis, or decision)", input.Type)
	}
	if input.Status != "" && !validClaimStatus(input.Status) {
		return mcpListClaimsOutput{}, fmt.Errorf("invalid status %q (want active, contested, or deprecated)", input.Status)
	}
	runID := strings.TrimSpace(input.RunID)
	if err := enforceRunScope(ctx, runID); err != nil {
		return mcpListClaimsOutput{}, err
	}

	conn, err := openConn(ctx)
	if err != nil {
		return mcpListClaimsOutput{}, err
	}
	defer closeConn(conn)

	claims, total, err := listClaimsFiltered(ctx, conn, input.Type, input.Status, runID, limit, offset)
	if err != nil {
		return mcpListClaimsOutput{}, err
	}
	return mcpListClaimsOutput{
		Claims: claims,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	}, nil
}

func mcpRunListContradictions(ctx context.Context, input mcpListContradictionsInput) (mcpListContradictionsOutput, error) {
	limit, offset := normalizePagination(input.Limit, input.Offset)

	runID := strings.TrimSpace(input.RunID)
	if err := enforceRunScope(ctx, runID); err != nil {
		return mcpListContradictionsOutput{}, err
	}

	conn, err := openConn(ctx)
	if err != nil {
		return mcpListContradictionsOutput{}, err
	}
	defer closeConn(conn)

	pairs, total, err := listContradictionPairs(ctx, conn, runID, limit, offset)
	if err != nil {
		return mcpListContradictionsOutput{}, err
	}
	return mcpListContradictionsOutput{
		Contradictions: pairs,
		Total:          total,
		Limit:          limit,
		Offset:         offset,
	}, nil
}

// listClaimsFiltered loads the candidate claims and filters/paginates in
// memory. The CLI scale (≤ 100k claims) makes a full ListAll cheap
// and keeps the port surface free of bespoke filter parameters.
//
// A non-empty runID narrows the candidate set to claims derived from that
// run's events before any other filter, so a run-scoped caller can never see a
// row belonging to another run.
func listClaimsFiltered(ctx context.Context, conn *store.Conn, claimType, status, runID string, limit, offset int) ([]domain.Claim, int, error) {
	all, err := listCandidateClaims(ctx, conn, runID)
	if err != nil {
		return nil, 0, err
	}
	filtered := make([]domain.Claim, 0, len(all))
	for _, c := range all {
		if claimType != "" && string(c.Type) != claimType {
			continue
		}
		if status != "" && string(c.Status) != status {
			continue
		}
		filtered = append(filtered, c)
	}
	// Sort by created_at descending for the most-recent-first browse
	// experience. ListAll returns ascending, but the run-scoped path resolves
	// claims by event id, so sort explicitly rather than relying on a reverse.
	sort.SliceStable(filtered, func(i, j int) bool {
		return filtered[i].CreatedAt.After(filtered[j].CreatedAt)
	})
	total := len(filtered)
	page := paginate(filtered, limit, offset)
	return page, total, nil
}

// listCandidateClaims returns every claim (runID empty) or only the claims
// whose evidence points at an event in runID. Claims carry no run column of
// their own — the run lives on the source event — so the scoped path walks
// events → claim evidence through the ports.
func listCandidateClaims(ctx context.Context, conn *store.Conn, runID string) ([]domain.Claim, error) {
	if runID == "" {
		all, err := conn.Claims.ListAll(ctx)
		if err != nil {
			return nil, fmt.Errorf("list claims: %w", err)
		}
		return all, nil
	}
	events, err := conn.Events.ListByRunID(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list events for run %q: %w", runID, err)
	}
	if len(events) == 0 {
		return nil, nil
	}
	eventIDs := make([]string, 0, len(events))
	for _, ev := range events {
		eventIDs = append(eventIDs, ev.ID)
	}
	claims, err := conn.Claims.ListByEventIDs(ctx, eventIDs)
	if err != nil {
		return nil, fmt.Errorf("list claims for run %q: %w", runID, err)
	}
	// ListByEventIDs can repeat a claim backed by several events in the run.
	seen := make(map[string]struct{}, len(claims))
	deduped := make([]domain.Claim, 0, len(claims))
	for _, c := range claims {
		if _, dup := seen[c.ID]; dup {
			continue
		}
		seen[c.ID] = struct{}{}
		deduped = append(deduped, c)
	}
	return deduped, nil
}

// listContradictionPairs assembles every contradicts edge with the
// surrounding claim text using ports — Relationships.ListAll +
// Claims.ListByIDs for the hop, then in-memory pagination.
//
// A non-empty runID keeps only edges whose BOTH endpoints belong to that run.
// Requiring both is deliberate: the pair is hydrated with each claim's text,
// so admitting an edge with one foot outside the run would hand the caller the
// text of a claim it is not scoped to read.
func listContradictionPairs(ctx context.Context, conn *store.Conn, runID string, limit, offset int) ([]mcpContradictionPair, int, error) {
	allRels, err := conn.Relationships.ListAll(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list relationships: %w", err)
	}
	var runClaims map[string]struct{}
	if runID != "" {
		scoped, err := listCandidateClaims(ctx, conn, runID)
		if err != nil {
			return nil, 0, err
		}
		runClaims = make(map[string]struct{}, len(scoped))
		for _, c := range scoped {
			runClaims[c.ID] = struct{}{}
		}
	}
	contradictions := make([]domain.Relationship, 0)
	for _, r := range allRels {
		if string(r.Type) != "contradicts" {
			continue
		}
		if runClaims != nil {
			if _, ok := runClaims[r.FromClaimID]; !ok {
				continue
			}
			if _, ok := runClaims[r.ToClaimID]; !ok {
				continue
			}
		}
		contradictions = append(contradictions, r)
	}
	// most-recent-first
	for i, j := 0, len(contradictions)-1; i < j; i, j = i+1, j-1 {
		contradictions[i], contradictions[j] = contradictions[j], contradictions[i]
	}
	total := len(contradictions)
	page := paginate(contradictions, limit, offset)

	// Resolve claim text for the page only — saves materialising
	// every claim row when the operator only asked for one screen.
	claimIDSet := map[string]struct{}{}
	for _, r := range page {
		claimIDSet[r.FromClaimID] = struct{}{}
		claimIDSet[r.ToClaimID] = struct{}{}
	}
	claimIDs := make([]string, 0, len(claimIDSet))
	for id := range claimIDSet {
		claimIDs = append(claimIDs, id)
	}
	textByID := map[string]string{}
	if len(claimIDs) > 0 {
		claims, err := conn.Claims.ListByIDs(ctx, claimIDs)
		if err != nil {
			return nil, 0, fmt.Errorf("resolve claim texts: %w", err)
		}
		for _, c := range claims {
			textByID[c.ID] = c.Text
		}
	}
	pairs := make([]mcpContradictionPair, 0, len(page))
	for _, r := range page {
		pairs = append(pairs, mcpContradictionPair{
			RelationshipID: r.ID,
			FromClaimID:    r.FromClaimID,
			FromClaimText:  textByID[r.FromClaimID],
			ToClaimID:      r.ToClaimID,
			ToClaimText:    textByID[r.ToClaimID],
			CreatedAt:      r.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return pairs, total, nil
}

func normalizePagination(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func validClaimType(s string) bool {
	switch domain.ClaimType(s) {
	case domain.ClaimTypeFact, domain.ClaimTypeHypothesis, domain.ClaimTypeDecision, domain.ClaimTypeTestResult:
		return true
	}
	return false
}

func validClaimStatus(s string) bool {
	switch domain.ClaimStatus(s) {
	case domain.ClaimStatusActive, domain.ClaimStatusContested, domain.ClaimStatusResolved, domain.ClaimStatusDeprecated:
		return true
	}
	return false
}

func validClaimVisibility(s string) bool {
	switch domain.Visibility(s) {
	case domain.VisibilityPersonal, domain.VisibilityTeam, domain.VisibilityOrg:
		return true
	}
	return false
}
