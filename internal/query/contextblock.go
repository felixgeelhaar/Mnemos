package query

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// ContextBlockOptions tunes a Context Block build. Zero values are
// safe defaults: empty Query falls back to "all active claims for
// the run", MaxTokens=0 means "no truncation".
type ContextBlockOptions struct {
	RunID     string
	Query     string
	MaxTokens int
}

// approxCharsPerToken is the rough ratio used by ContextBlockOptions.
// Real tokenizers vary — we err on the safe side so generated blocks
// stay under the requested budget.
const approxCharsPerToken = 4

// BuildContextBlock returns a stable, agent-ready string summarising
// the memory state for one run: top claims by trust, surfaced
// contradictions, and a footer with counts. Designed to be dropped
// directly into a system prompt.
//
// Layout:
//
//	# Memory context (run <id>)
//	## Active claims (N)
//	- [cl_xxx · type · trust 0.91] text
//	## Contradictions (M)
//	- cl_xxx ⊥ cl_yyy
//	## Footer
//	Generated <ts>. claims=N contradictions=M
//
// Section ordering is fixed; agents can rely on the layout.
func (e Engine) BuildContextBlock(ctx context.Context, opts ContextBlockOptions) (string, error) {
	if opts.RunID == "" {
		return "", fmt.Errorf("BuildContextBlock: RunID is required")
	}

	events, err := e.eventLister().ListByRunID(ctx, opts.RunID)
	if err != nil {
		return "", fmt.Errorf("list events for run %s: %w", opts.RunID, err)
	}
	if len(events) == 0 {
		return formatEmpty(opts.RunID), nil
	}

	eventIDs := make([]string, 0, len(events))
	for _, ev := range events {
		eventIDs = append(eventIDs, ev.ID)
	}

	claims, err := e.claims.ListByEventIDs(ctx, eventIDs)
	if err != nil {
		return "", fmt.Errorf("list claims for run %s: %w", opts.RunID, err)
	}

	// Drop deprecated claims; keep active + contested + resolved so
	// the agent can see which decisions were superseded with reason.
	active := excludeDeprecated(claims)

	// Honour the Query filter. This field was declared, documented ("empty
	// Query falls back to all active claims for the run"), accepted by
	// POST /v1/context and plumbed all the way here — and then never read. A
	// caller asking for "database migrations" got the entire run's context
	// block, so MaxTokens truncated the claims they wanted in favour of ones
	// they did not ask for. No error, a perfectly well-formed block, just the
	// wrong content.
	active = filterByQuery(active, opts.Query)

	// Stable trust-then-id ordering. Trust score may not be set on
	// every backend; fall back to confidence then to ID.
	sort.SliceStable(active, func(i, j int) bool {
		ti, tj := claimSortKey(active[i]), claimSortKey(active[j])
		if ti != tj {
			return ti > tj
		}
		return active[i].ID < active[j].ID
	})

	// Pull contradicts edges scoped to this run's claims via
	// ListByClaimIDs, then filter to type=contradicts.
	claimIDSet := make(map[string]struct{}, len(active))
	claimIDList := make([]string, 0, len(active))
	for _, c := range active {
		claimIDSet[c.ID] = struct{}{}
		claimIDList = append(claimIDList, c.ID)
	}
	allRels, err := e.relationships.ListByClaimIDs(ctx, claimIDList)
	if err != nil {
		return "", fmt.Errorf("list relationships for claims: %w", err)
	}
	relevantContradictions := make([]domain.Relationship, 0, len(allRels))
	for _, r := range allRels {
		if r.Type != domain.RelationshipTypeContradicts {
			continue
		}
		// ListByClaimIDs may include edges where only one endpoint is
		// in the run; require both to be in-run for a clean section.
		_, fromInRun := claimIDSet[r.FromClaimID]
		_, toInRun := claimIDSet[r.ToClaimID]
		if fromInRun && toInRun {
			relevantContradictions = append(relevantContradictions, r)
		}
	}

	return formatContextBlock(opts, active, relevantContradictions), nil
}

func formatEmpty(runID string) string {
	return fmt.Sprintf(
		"# Memory context (run %s)\n## Active claims (0)\n## Contradictions (0)\n## Footer\nGenerated %s. claims=0 contradictions=0\n",
		runID, time.Now().UTC().Format(time.RFC3339),
	)
}

func formatContextBlock(opts ContextBlockOptions, claims []domain.Claim, contradictions []domain.Relationship) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Memory context (run %s)\n", opts.RunID)

	fmt.Fprintf(&b, "## Active claims (%d)\n", len(claims))
	for _, c := range claims {
		line := fmt.Sprintf("- [%s · %s · trust %.2f] %s\n",
			c.ID, c.Type, c.TrustScore, c.Text)
		if opts.MaxTokens > 0 && approxTokenLen(b.String()+line) > opts.MaxTokens {
			break
		}
		b.WriteString(line)
	}

	fmt.Fprintf(&b, "## Contradictions (%d)\n", len(contradictions))
	for _, r := range contradictions {
		line := fmt.Sprintf("- %s ⊥ %s\n", r.FromClaimID, r.ToClaimID)
		if opts.MaxTokens > 0 && approxTokenLen(b.String()+line) > opts.MaxTokens {
			break
		}
		b.WriteString(line)
	}

	fmt.Fprintf(&b, "## Footer\nGenerated %s. claims=%d contradictions=%d\n",
		time.Now().UTC().Format(time.RFC3339), len(claims), len(contradictions))

	return b.String()
}

// approxTokenLen returns a rough token count (chars/4). Used only for
// budget enforcement; downstream prompts may use a real tokenizer.
func approxTokenLen(s string) int {
	return len(s) / approxCharsPerToken
}

// claimSortKey returns the score used to order active claims in the
// block. TrustScore is preferred when present (>0); otherwise
// Confidence; otherwise 0.
func claimSortKey(c domain.Claim) float64 {
	if c.TrustScore > 0 {
		return c.TrustScore
	}
	return c.Confidence
}

// eventLister returns the engine's event store as the narrower
// interface BuildContextBlock needs (ListByRunID).
func (e Engine) eventLister() interface {
	ListByRunID(ctx context.Context, runID string) ([]domain.Event, error)
} {
	type runListable interface {
		ListByRunID(ctx context.Context, runID string) ([]domain.Event, error)
	}
	if rl, ok := e.events.(runListable); ok {
		return rl
	}
	// Fallback: synthesize the interface against ListAll → in-memory filter.
	// Backends that don't implement ListByRunID still produce a correct,
	// just-slower context block.
	return runListAdapter{e: e}
}

type runListAdapter struct{ e Engine }

// ListByRunID returns all events belonging to the given runID.
func (r runListAdapter) ListByRunID(ctx context.Context, runID string) ([]domain.Event, error) {
	all, err := r.e.events.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Event, 0, len(all))
	for _, ev := range all {
		if ev.RunID == runID {
			out = append(out, ev)
		}
	}
	return out, nil
}

// excludeDeprecated removes claims that have been forgotten or superseded.
// Shared by the recall path and the run-scoped context block so "forgotten"
// means the same thing in both — they disagreed until the recall path was
// found returning deprecated claims a user had explicitly forgotten.
func excludeDeprecated(claims []domain.Claim) []domain.Claim {
	out := make([]domain.Claim, 0, len(claims))
	for _, c := range claims {
		if c.Status != domain.ClaimStatusDeprecated {
			out = append(out, c)
		}
	}
	return out
}

// filterByQuery narrows claims to those sharing a meaningful token with the
// query. An empty query is the documented "everything for this run" case.
//
// Deliberately a filter rather than a re-rank: BuildContextBlock's ordering is
// trust-first by design (the agent should see what the store is most sure of),
// and relevance ranking here would quietly compete with that.
//
// Matching is EXACT on docTokens — the same tokenizer the retrieval path uses,
// which lowercases and trims punctuation but does not stem. So "migrations"
// does not match "migration". That is a real limitation, kept deliberately:
// a filter that matched more loosely than the engine's own retrieval would be
// a second, divergent notion of relevance, and two disagreeing definitions is
// worse than one blunt one. If this needs stemming, the tokenizer is the place
// to add it, so both paths change together.
//
// If nothing matches, the caller gets an empty block rather than an unfiltered
// one: silently widening a filter back to everything is the behaviour this is
// fixing.
func filterByQuery(claims []domain.Claim, q string) []domain.Claim {
	tokens := docTokens(q)
	if len(tokens) == 0 {
		return claims
	}

	want := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		want[t] = struct{}{}
	}

	out := make([]domain.Claim, 0, len(claims))

	for _, c := range claims {
		for _, t := range docTokens(c.Text) {
			if _, ok := want[t]; ok {
				out = append(out, c)

				break
			}
		}
	}

	return out
}
