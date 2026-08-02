package mnemos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/felixgeelhaar/chronos"
	"github.com/felixgeelhaar/chronos/embed"
	"github.com/google/uuid"
	axidomain "go.klarlabs.de/axi/domain"
	"go.klarlabs.de/bolt"
	"go.klarlabs.de/mnemos/internal/consolidate"
	"go.klarlabs.de/mnemos/internal/credit"
	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/embedding"
	"go.klarlabs.de/mnemos/internal/kernel"
	"go.klarlabs.de/mnemos/internal/llm"
	"go.klarlabs.de/mnemos/internal/pipeline"
	"go.klarlabs.de/mnemos/internal/plasticity"
	"go.klarlabs.de/mnemos/internal/ports"
	"go.klarlabs.de/mnemos/internal/query"
	"go.klarlabs.de/mnemos/internal/relate"
	"go.klarlabs.de/mnemos/internal/salience"
	"go.klarlabs.de/mnemos/internal/store"
	"go.klarlabs.de/mnemos/internal/trust"
	"go.klarlabs.de/mnemos/providers"
)

// chronosEventNamespace is the deterministic UUID namespace used when
// hashing string ids (run id, event type) into uuid.UUID for Chronos
// EntityState identifiers. Stable across processes so the same
// (RunID, Type) tuple always maps to the same Chronos series.
var chronosEventNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("mnemos://events"))

// memory is the concrete implementation of [Memory] returned by [New].
type memory struct {
	conn    *store.Conn
	actorID string
	info    Info   // immutable runtime config snapshot, exposed via Info()
	cfg     config // retained so Tenant() can re-run the New assembly
	dsn     string // resolved storage DSN, tenant-rewritten by Tenant()
	// tenant is the tenant id this view is scoped to, read from the DSN's
	// `tenant=` param; it is "" for an unscoped store (no tenant param), which
	// preserves the legacy chronos keys. When non-empty it is mixed into the
	// chronos EntityID/ScopeID so a SINGLE shared chronos engine keeps tenants
	// isolated — chronos scopes by entity/scope, not tenant, so without this
	// two tenants' temporal signals would merge.
	tenant    string
	extractor *pipeline.Extractor
	relator   relate.Engine
	query     query.Engine
	embedder  embedding.Client

	// chronos is the bundled or user-supplied temporal engine. Always
	// non-nil after a successful [New].
	chronos *embed.Engine
	// chronosOwned is true when [New] constructed the engine itself
	// and is responsible for closing it. False when the caller
	// supplied one via [WithChronos].
	chronosOwned bool

	// wkernel is the in-process governed axi kernel every public write
	// routes through. Built unconditionally by [New] — there is no
	// direct-write fallback, so no write can bypass the evidence chain
	// + budget. See governed_writes.go.
	wkernel *kernel.Governed
	// logger backs the kernel's domain-event log. Nil keeps axi chatter
	// off stderr (the library default); cmd/mnemos can supply its own.
	logger *bolt.Logger

	// lastSession holds the axi session recorded by the most recent
	// public write, exposed via [Memory.LastWriteSession]. Guarded by
	// sessionMu for concurrent writers.
	sessionMu   sync.RWMutex
	lastSession *axidomain.ExecutionSession

	// tokenBudget is the cumulative language-model token budget across all
	// writes on this Memory (set via [WithTokenBudget]). Zero means no
	// cumulative cap. tokensSpent accumulates the per-write token spend
	// reported on each session's "llm" evidence record; once it reaches
	// the budget, further writes are rejected pre-execution. Atomic so
	// concurrent writers account safely.
	tokenBudget int64
	tokensSpent atomic.Int64

	// embedWG tracks in-flight embedAsync goroutines and embedCancel stops
	// them, so [Close] can drain rather than pull the connection out from
	// under them.
	//
	// Both are required for correctness, not tidiness. embedAsync re-read
	// m.conn *inside* its goroutine while Close set that field to nil with no
	// synchronisation: a data race, and a nil dereference that panics the whole
	// process — on a long-lived `mnemos mcp` server that is every other
	// in-flight tool call, not just this one. Draining also stops a short-lived
	// CLI (`mnemos claim add`) from exiting before the embedding lands, which
	// silently persisted claims that semantic recall could never reach.
	embedWG     sync.WaitGroup
	embedCtx    context.Context
	embedCancel context.CancelFunc
}

var _ Memory = (*memory)(nil)

// setLastSession records s as the session of the most recent write.
func (m *memory) setLastSession(s *axidomain.ExecutionSession) {
	m.sessionMu.Lock()
	m.lastSession = s
	m.sessionMu.Unlock()
}

// LastWriteSession implements [Memory.LastWriteSession]. Returns nil
// before any write has been performed.
func (m *memory) LastWriteSession() WriteSession {
	m.sessionMu.RLock()
	s := m.lastSession
	m.sessionMu.RUnlock()
	if s == nil {
		return nil
	}
	return newWriteSession(s)
}

// Remember implements [Memory.Remember]. The write routes through the
// axi kernel: this method validates input and builds the invocation; the
// rememberExecutor performs the ingest -> extract -> relate -> persist
// pipeline and records the evidence chain. See governed_writes.go.
func (m *memory) Remember(ctx context.Context, item Item) error {
	if strings.TrimSpace(item.Content) == "" {
		return errors.New("mnemos: Remember: Content is required")
	}
	_, err := dispatchWrite[rememberOutput](ctx, m, actionRemember, rememberInput{Item: item})
	return err
}

// RememberClaim implements [Memory.RememberClaim]. The write routes
// through the axi kernel; the rememberClaimExecutor persists the claim
// (and any evidence links) and records the claim id in the evidence
// chain. The generated claim id is returned to the caller.
func (m *memory) RememberClaim(ctx context.Context, item ClaimItem) (string, error) {
	if strings.TrimSpace(item.Text) == "" {
		return "", errors.New("mnemos: RememberClaim: Text is required")
	}
	// Spec non-negotiable: "Claims require evidence. An assertion without
	// evidence is rejected." Fail-closed here, before the governed write
	// runs, so no evidence-less claim is ever persisted. Blank ids do not
	// count as evidence.
	if !hasEvidence(item.EventIDs) {
		return "", errors.New("mnemos: RememberClaim: at least one evidence EventID is required (claims require evidence)")
	}
	out, err := dispatchWrite[rememberClaimOutput](ctx, m, actionRememberClaim, rememberClaimInput{Claim: item})
	if err != nil {
		return "", err
	}
	m.embedAsync(out.ClaimID, "claim", item.Text)
	return out.ClaimID, nil
}

// RecordDecision persists an agent decision (belief -> plan -> outcome audit
// record) through the governed write path and returns its id. Statement is
// required; RiskLevel defaults to "medium" when empty. This is the public
// entry point for the decision audit trail that [Memory.AuditTrail]-style
// consumers read back via [Memory.GetDecision] / [Memory.ListDecisions].
func (m *memory) RecordDecision(ctx context.Context, d Decision) (string, error) {
	if strings.TrimSpace(d.Statement) == "" {
		return "", errors.New("mnemos: RecordDecision: Statement is required")
	}
	out, err := dispatchWrite[recordDecisionOutput](ctx, m, actionRecordDecision, recordDecisionInput{Decision: d})
	if err != nil {
		return "", err
	}
	// Embed the decision so it is semantically recallable alongside events and
	// claims (same async, best-effort embed-on-write path).
	m.embedAsync(out.DecisionID, "decision", d.Statement+" "+d.Reasoning)
	return out.DecisionID, nil
}

// SetClaimLifecycle implements [Memory.SetClaimLifecycle].
func (m *memory) SetClaimLifecycle(ctx context.Context, claimID string, lifecycle ClaimLifecycle) error {
	if strings.TrimSpace(claimID) == "" {
		return errors.New("mnemos: SetClaimLifecycle: claimID is required")
	}
	_, err := dispatchWrite[setClaimLifecycleOutput](ctx, m, actionSetClaimLifecycle, setClaimLifecycleInput{
		ClaimID:   claimID,
		Lifecycle: lifecycle,
	})
	return err
}

// GetDecision returns the decision with the given id, or a not-found error.
func (m *memory) GetDecision(ctx context.Context, id string) (Decision, error) {
	if strings.TrimSpace(id) == "" {
		return Decision{}, errors.New("mnemos: GetDecision: id is required")
	}
	d, err := m.conn.Decisions.GetByID(ctx, id)
	if err != nil {
		return Decision{}, fmt.Errorf("mnemos: GetDecision: %w", err)
	}
	return toPublicDecision(d), nil
}

// ListDecisions returns recorded decisions (most-recent first), capped at
// limit (0 = no cap). It is the read side of the decision audit trail.
func (m *memory) ListDecisions(ctx context.Context, limit int) ([]Decision, error) {
	all, err := m.conn.Decisions.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("mnemos: ListDecisions: %w", err)
	}
	out := make([]Decision, 0, len(all))
	for _, d := range all {
		out = append(out, toPublicDecision(d))
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Recall implements [Memory.Recall].
func (m *memory) Recall(ctx context.Context, q Query) ([]Result, error) {
	results, _, err := m.recall(ctx, q)
	return results, err
}

// RecallWithSufficiency implements [Memory.RecallWithSufficiency]: Recall plus
// the "feeling of knowing" verdict, built from the aggregate answer confidence
// the underlying query already computes (Recall discards it).
func (m *memory) RecallWithSufficiency(ctx context.Context, q Query) ([]Result, Sufficiency, error) {
	results, ans, err := m.recall(ctx, q)
	if err != nil {
		return nil, Sufficiency{}, err
	}
	return results, sufficiencyOf(ans), nil
}

// RecallWithEffort implements [Memory.RecallWithEffort]: a first pass, then one
// bounded wider+deeper pass when the Expected-Value-of-Control budget
// (stakes × (1 − first-pass confidence)) warrants it and the first pass was not
// already sufficient. Non-regressive: the escalated pass is adopted only when it
// is stronger (at least as many claims AND higher confidence).
func (m *memory) RecallWithEffort(ctx context.Context, q Query, stakes float64) ([]Result, Sufficiency, EffortReport, error) {
	baseResults, baseAns, err := m.recall(ctx, q)
	if err != nil {
		return nil, Sufficiency{}, EffortReport{}, err
	}
	baseBreadth := q.Limit
	if baseBreadth <= 0 {
		baseBreadth = len(baseResults) // no explicit cap — the breadth that came back
	}
	report := EffortReport{
		Budget:  EffortBudget(stakes, baseAns.Confidence),
		Passes:  1,
		Hops:    q.Hops,
		Breadth: baseBreadth,
	}
	baseSuf := sufficiencyOf(baseAns)

	// Escalate a single bounded pass only when the stakes-weighted uncertainty
	// warrants it AND we are not already grounded.
	if report.Budget < EffortEscalationThreshold || baseSuf.Sufficient {
		return baseResults, baseSuf, report, nil
	}
	eq := q
	eq.Hops = q.Hops + int(math.Ceil(report.Budget*EffortMaxExtraHops))
	eq.Limit = baseBreadth + int(math.Round(report.Budget*EffortMaxExtraBreadth))
	report.Passes = 2

	escResults, escAns, eerr := m.recall(ctx, eq)
	if eerr == nil && strongerAnswer(escAns, baseAns) {
		report.Hops = eq.Hops
		report.Breadth = eq.Limit
		return escResults, sufficiencyOf(escAns), report, nil
	}
	// Escalation did not help (or errored): keep the first pass. The effort was
	// spent (Passes == 2) but the answer is unchanged — never regressed.
	return baseResults, baseSuf, report, nil
}

// RecallWithContext implements [Memory.RecallWithContext]: base query recall, then
// a stable re-rank that promotes results connected (over the epistemic graph) to
// the caller's current context — spreading activation from the train of thought.
func (m *memory) RecallWithContext(ctx context.Context, q Query, activeContext string) ([]Result, error) {
	results, _, err := m.recall(ctx, q)
	if err != nil {
		return nil, err
	}
	activeContext = strings.TrimSpace(activeContext)
	if activeContext == "" || len(results) < 2 {
		return results, nil // no context, or nothing to reorder
	}
	// Spread activation: expand the context over the epistemic graph and record a
	// decaying activation per reached claim. Hop expansion IS the spread. Reuse the
	// query's Lifecycle so the activation source respects the same visibility.
	activated, _, aerr := m.recall(ctx, Query{Text: activeContext, Hops: ActivationHops, Lifecycle: q.Lifecycle})
	if aerr != nil {
		return results, nil // best-effort: spreading failed → plain recall order
	}
	activation := make(map[string]float64, len(activated))
	for _, a := range activated {
		act := math.Pow(ActivationDecay, float64(a.HopDistance))
		if act > activation[a.ClaimID] {
			activation[a.ClaimID] = act
		}
	}
	if len(activation) == 0 {
		return results, nil
	}
	// Stable re-rank: base score preserves the recall order (len-i); activation
	// promotes connected results by up to ActivationBoost positions.
	type scored struct {
		r Result
		s float64
	}
	ranked := make([]scored, len(results))
	for i, r := range results {
		ranked[i] = scored{r, float64(len(results)-i) + ActivationBoost*activation[r.ClaimID]}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].s > ranked[j].s })
	out := make([]Result, len(ranked))
	for i, sc := range ranked {
		out[i] = sc.r
	}
	return out, nil
}

// RecallWithConflicts implements [Memory.RecallWithConflicts]: base recall plus the
// contested frontier — for each recalled claim, the currently-valid claims that
// contradict it over the epistemic graph. Strongest challenger first.
func (m *memory) RecallWithConflicts(ctx context.Context, q Query) ([]Result, []Conflict, error) {
	results, _, err := m.recall(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	if len(results) == 0 {
		return results, nil, nil
	}
	resultByID := make(map[string]Result, len(results))
	ids := make([]string, len(results))
	for i, r := range results {
		ids[i] = r.ClaimID
		resultByID[r.ClaimID] = r
	}
	rels, err := m.conn.Relationships.ListByClaimIDs(ctx, ids)
	if err != nil {
		return results, nil, nil // best-effort: no graph → no conflicts, results stand
	}
	// Endpoints not already recalled need a validity check before they can count
	// as live counter-evidence (a superseded/invalidated challenger is resolved).
	need := map[string]struct{}{}
	for _, rel := range rels {
		if rel.Type != domain.RelationshipTypeContradicts {
			continue
		}
		for _, id := range []string{rel.FromClaimID, rel.ToClaimID} {
			if _, ok := resultByID[id]; !ok {
				need[id] = struct{}{}
			}
		}
	}
	valid := map[string]domain.Claim{}
	if len(need) > 0 {
		list := make([]string, 0, len(need))
		for id := range need {
			list = append(list, id)
		}
		if claims, cerr := m.conn.Claims.ListByIDs(ctx, list); cerr == nil {
			for _, c := range claims {
				if c.ValidTo.IsZero() && c.Lifecycle != domain.ClaimLifecycleSuperseded {
					valid[c.ID] = c
				}
			}
		}
	}
	// challengerOf returns the text/trust of a claim usable as counter-evidence,
	// whether it was itself recalled (valid by construction) or externally valid.
	challengerOf := func(id string) (text string, trust float64, ok bool) {
		if r, isResult := resultByID[id]; isResult {
			return r.Text, r.TrustScore, true
		}
		if c, isValid := valid[id]; isValid {
			return c.Text, c.TrustScore, true
		}
		return "", 0, false
	}
	var conflicts []Conflict
	seen := map[string]struct{}{}
	for _, rel := range rels {
		if rel.Type != domain.RelationshipTypeContradicts {
			continue
		}
		// A contradiction is undirected here: whichever endpoint is recalled is the
		// contested claim, the other (if a live claim) is its counter-evidence.
		for _, pair := range [2][2]string{{rel.FromClaimID, rel.ToClaimID}, {rel.ToClaimID, rel.FromClaimID}} {
			recalledID, otherID := pair[0], pair[1]
			recalled, isRecalled := resultByID[recalledID]
			if !isRecalled {
				continue
			}
			text, trust, ok := challengerOf(otherID)
			if !ok {
				continue
			}
			key := recalledID + "\x00" + otherID
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			conflicts = append(conflicts, Conflict{
				ClaimID: recalledID, ClaimText: recalled.Text,
				ContradictingID: otherID, ContradictingText: text, ContradictingTrust: trust,
			})
		}
	}
	// Strongest counter-evidence first.
	sort.SliceStable(conflicts, func(i, j int) bool {
		return conflicts[i].ContradictingTrust > conflicts[j].ContradictingTrust
	})
	return results, conflicts, nil
}

// sufficiencyOf builds the public "feeling of knowing" from a raw answer.
func sufficiencyOf(ans domain.Answer) Sufficiency {
	return Sufficiency{
		Confidence: ans.Confidence,
		ClaimCount: len(ans.Claims),
		Sufficient: len(ans.Claims) > 0 && ans.Confidence >= RecallSufficiencyFloor,
		Floor:      RecallSufficiencyFloor,
	}
}

// strongerAnswer reports whether b is a non-regressive improvement over a: at
// least as many claims AND strictly higher confidence. Mirrors the rule the
// internal corrective-retrieval pass uses so effort never trades breadth for a
// marginally higher score.
func strongerAnswer(b, a domain.Answer) bool {
	return len(b.Claims) >= len(a.Claims) && b.Confidence > a.Confidence
}

// Blocks implements [Memory.Blocks].
func (m *memory) Blocks(ctx context.Context, owner string) ([]Block, error) {
	if m.conn.Blocks == nil {
		return nil, ErrBlocksUnsupported
	}
	rows, err := m.conn.Blocks.List(ctx, strings.TrimSpace(owner))
	if err != nil {
		return nil, fmt.Errorf("mnemos: Blocks: %w", err)
	}
	out := make([]Block, 0, len(rows))
	for _, b := range rows {
		out = append(out, Block{Owner: b.Owner, Label: b.Label, Value: b.Content, UpdatedAt: b.UpdatedAt})
	}
	return out, nil
}

// SetBlock implements [Memory.SetBlock]. An empty value deletes the block; an
// over-limit value is rejected (working memory is bounded on purpose).
func (m *memory) SetBlock(ctx context.Context, owner, label, value string) error {
	if m.conn.Blocks == nil {
		return ErrBlocksUnsupported
	}
	owner, label = strings.TrimSpace(owner), strings.TrimSpace(label)
	if owner == "" || label == "" {
		return errors.New("mnemos: SetBlock: owner and label are required")
	}
	if len(value) > WorkingMemoryBlockLimit {
		return ErrBlockTooLarge
	}
	if value == "" {
		return m.conn.Blocks.Delete(ctx, owner, label)
	}
	return m.conn.Blocks.Upsert(ctx, domain.WorkingMemoryBlock{
		Owner: owner, Label: label, Content: value, UpdatedAt: time.Now().UTC(),
	})
}

// AppendBlock implements [Memory.AppendBlock]: append a line, then evict oldest
// lines to stay within the block limit.
func (m *memory) AppendBlock(ctx context.Context, owner, label, text string) error {
	if m.conn.Blocks == nil {
		return ErrBlocksUnsupported
	}
	owner, label = strings.TrimSpace(owner), strings.TrimSpace(label)
	if owner == "" || label == "" {
		return errors.New("mnemos: AppendBlock: owner and label are required")
	}
	cur, _, err := m.conn.Blocks.Get(ctx, owner, label)
	if err != nil {
		return fmt.Errorf("mnemos: AppendBlock: %w", err)
	}
	combined := cur.Content
	if combined != "" && text != "" {
		combined += "\n"
	}
	combined += text
	combined = evictToLimit(combined, WorkingMemoryBlockLimit)
	return m.conn.Blocks.Upsert(ctx, domain.WorkingMemoryBlock{
		Owner: owner, Label: label, Content: combined, UpdatedAt: time.Now().UTC(),
	})
}

// evictToLimit trims content to fit limit by dropping WHOLE lines from the FRONT
// (oldest first) — the working-memory eviction policy: the cap is the attention
// budget, and the freshest lines win. If a single trailing line still exceeds the
// limit it is hard-truncated to its last `limit` characters.
func evictToLimit(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	lines := strings.Split(s, "\n")
	for len(lines) > 1 && len(strings.Join(lines, "\n")) > limit {
		lines = lines[1:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// recall is the shared core: it runs the query and maps claims to results,
// returning the raw domain.Answer too so callers that want the aggregate
// confidence (RecallWithSufficiency) can read it without a second pass.
func (m *memory) recall(ctx context.Context, q Query) ([]Result, domain.Answer, error) {
	if strings.TrimSpace(q.Text) == "" {
		return nil, domain.Answer{}, errors.New("mnemos: Recall: Text is required")
	}

	// The embedded store is the path every consuming product uses (senat-os,
	// pet-medical). It built AnswerOptions without the cognitive behaviours, so
	// those brains never primed, never strengthened, never reconsolidated.
	opts := query.AnswerOptions{
		Hops:           q.Hops,
		AsOf:           q.AsOf,
		RecordedAsOf:   q.RecordedAsOf,
		IncludeHistory: q.IncludeHistory,
		Lifecycle:      domain.ClaimLifecycle(q.Lifecycle),
		Consumer:       consumerMode(q.AgentConsumer),
	}.WithCognitiveDefaults()

	var ans domain.Answer
	var err error
	if q.RunID != "" {
		ans, err = m.query.AnswerForRunWithOptions(ctx, q.Text, q.RunID, opts)
	} else {
		ans, err = m.query.AnswerWithOptions(ctx, q.Text, opts)
	}
	if err != nil {
		return nil, domain.Answer{}, fmt.Errorf("mnemos: answer: %w", err)
	}

	results := make([]Result, 0, len(ans.Claims))
	for _, c := range ans.Claims {
		results = append(results, Result{
			ClaimID:     c.ID,
			Text:        c.Text,
			Type:        string(c.Type),
			Confidence:  c.Confidence,
			TrustScore:  c.TrustScore,
			HopDistance: ans.ClaimHopDistance[c.ID],
			Provenance:  ans.ClaimProvenance[c.ID],
		})
	}
	if q.Limit > 0 && len(results) > q.Limit {
		results = results[:q.Limit]
	}
	// Suppress unused-context: the underlying engine is sync today; ctx
	// is reserved for future cancellation propagation through the
	// query.Engine surface.
	_ = ctx
	return results, ans, nil
}

// Get implements [Memory.Get]. Exact lookup by claim id. Returns a
// not-found error when the id is unknown.
func (m *memory) Get(ctx context.Context, claimID string) (Claim, error) {
	id := strings.TrimSpace(claimID)
	if id == "" {
		return Claim{}, errors.New("mnemos: Get: claimID is required")
	}
	claims, err := m.conn.Claims.ListByIDs(ctx, []string{id})
	if err != nil {
		return Claim{}, fmt.Errorf("mnemos: Get: %w", err)
	}
	for _, c := range claims {
		if c.ID == id {
			return toPublicClaim(c), nil
		}
	}
	return Claim{}, fmt.Errorf("mnemos: Get: claim %q not found", id)
}

// Scan implements [Memory.Scan]. Returns claims whose valid-time interval
// overlaps the [ScanQuery] window, ordered by ValidFrom, honouring Limit.
//
// A zero Limit no longer means "the whole brain": it means
// [ScanMaxResults], which is also the ceiling on an explicit Limit. Scan is
// reachable over MCP and HTTP, where Limit: 0 made a full corpus dump a one-line
// request. Use [memory.ScanBounded] to learn whether the answer was truncated.
func (m *memory) Scan(ctx context.Context, q ScanQuery) ([]Claim, error) {
	rep, err := m.ScanBounded(ctx, q)
	return rep.Claims, err
}

// ScanBounded is [Memory.Scan] reporting the bounds it applied (see
// [BoundedCognition]).
func (m *memory) ScanBounded(ctx context.Context, q ScanQuery) (ScanReport, error) {
	var bounds Bounds
	limit := capLimit(&bounds, q.Limit, ScanMaxResults, ScanMaxResults)
	all, err := m.conn.Claims.ListAll(ctx)
	if err != nil {
		return ScanReport{}, fmt.Errorf("mnemos: Scan: %w", err)
	}
	bounds.Scanned = len(all)
	bounds.Considered = len(all)
	out := make([]Claim, 0, len(all))
	for _, c := range all {
		if !claimOverlapsWindow(c, q.ValidFrom, q.ValidUntil) {
			continue
		}
		out = append(out, toPublicClaim(c))
	}
	bounds.Available = len(out)
	// Order by ValidFrom (the endpoint's own ranking) BEFORE cutting, so a
	// truncated scan is the earliest window, not an arbitrary slice of the store's
	// listing order. Id breaks ties so equal-timestamp windows page stably.
	out = topN(out, limit, func(a, b Claim) bool {
		if !a.ValidFrom.Equal(b.ValidFrom) {
			return a.ValidFrom.Before(b.ValidFrom)
		}
		return a.ID < b.ID
	})
	if bounds.Available > len(out) {
		bounds.cut(BoundReasonResultLimit)
	}
	bounds.finish(len(out))
	return ScanReport{Claims: out, Bounds: bounds}, nil
}

// claimOverlapsWindow reports whether a claim's valid-time interval
// [ValidFrom, ValidTo) overlaps the [from, until) window. Zero bounds on
// either side act as open (unbounded) edges. A claim with a zero ValidFrom
// is treated as "valid since before tracking began" (negative infinity);
// a zero ValidTo means "still valid" (positive infinity).
func claimOverlapsWindow(c domain.Claim, from, until time.Time) bool {
	// Claim ends before the window starts → no overlap.
	if !from.IsZero() && !c.ValidTo.IsZero() && !c.ValidTo.After(from) {
		return false
	}
	// Claim starts at/after the window ends → no overlap.
	if !until.IsZero() && !c.ValidFrom.IsZero() && !c.ValidFrom.Before(until) {
		return false
	}
	return true
}

// toPublicClaim projects an internal domain.Claim into the stable public
// [Claim] read shape. ValidTo's zero value ("still valid") maps to a nil
// ValidUntil; CreatedAt maps to the transaction-time RecordedAt.
func toPublicClaim(c domain.Claim) Claim {
	out := Claim{
		ID:         c.ID,
		Statement:  c.Text,
		Type:       string(c.Type),
		Confidence: c.Confidence,
		TrustScore: c.TrustScore,
		ValidFrom:  c.ValidFrom,
		RecordedAt: c.CreatedAt,
		Lifecycle:  ClaimLifecycle(c.Lifecycle),
	}
	if !c.ValidTo.IsZero() {
		vt := c.ValidTo
		out.ValidUntil = &vt
	}
	return out
}

// toPublicDecision maps the internal domain decision onto the public shape.
func toPublicDecision(d domain.Decision) Decision {
	return Decision{
		ID:           d.ID,
		Statement:    d.Statement,
		Plan:         d.Plan,
		Reasoning:    d.Reasoning,
		RiskLevel:    string(d.RiskLevel),
		Beliefs:      d.Beliefs,
		Alternatives: d.Alternatives,
		OutcomeID:    d.OutcomeID,
		ChosenAt:     d.ChosenAt,
		CreatedBy:    d.CreatedBy,
		CreatedAt:    d.CreatedAt,
	}
}

// RememberEvent implements [Memory.RememberEvent]. The write routes
// through the axi kernel; the rememberEventExecutor appends the event,
// forwards it to the bundled Chronos engine, and records the event id in
// the evidence chain. The action is idempotent on event id.
func (m *memory) RememberEvent(ctx context.Context, e Event) error {
	if e.At.IsZero() {
		return errors.New("mnemos: RememberEvent: At is required")
	}
	if strings.TrimSpace(e.Content) == "" {
		return errors.New("mnemos: RememberEvent: Content is required")
	}
	e.ID = strings.TrimSpace(e.ID)
	out, err := dispatchWrite[rememberEventOutput](ctx, m, actionRememberEvent, rememberEventInput{Event: e})
	if err != nil {
		return err
	}
	m.embedAsync(out.EventID, "event", e.Content)
	return nil
}

// embedAsync embeds text and stores the vector for (entityID, entityType) in the
// BACKGROUND, so recall can rank it semantically without adding an embedding
// round-trip to the write path. It is:
//
//   - opt-in: a no-op unless an embedder is configured (WithSharedProvider /
//     WithEnhancedMode) and the backend has an embeddings repository;
//   - best-effort: it never blocks or fails the originating write — an embedding
//     is derived, recomputable state (this mirrors [pipeline.GenerateEmbeddings],
//     which also Upserts embeddings directly rather than through the write kernel);
//   - idempotent: the upsert is keyed on (entity_id, entity_type), so a re-embed
//     (model change, backfill) overwrites cleanly.
func (m *memory) embedAsync(entityID, entityType, text string) {
	if m.embedder == nil || m.conn == nil || m.conn.Embeddings == nil {
		return
	}
	if entityID == "" || strings.TrimSpace(text) == "" {
		return
	}
	// Capture the repository NOW, on the caller's goroutine. Re-reading m.conn
	// inside the goroutine races with Close (which nils it) and dereferences
	// nil if Close wins — a panic, not a returned error. The captured handle
	// stays valid: a closed connection makes Upsert return an error, which the
	// path below already logs.
	embeddings := m.conn.Embeddings
	embedder := m.embedder

	m.embedWG.Add(1)
	go func() {
		defer m.embedWG.Done()
		// Derived from embedCtx so Close cancels in-flight work instead of
		// waiting out the full 30s timeout. Fall back to Background for a
		// memory built by some path that skipped the constructor — nil here
		// would panic in WithTimeout.
		parent := m.embedCtx
		if parent == nil {
			parent = context.Background()
		}
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		vectors, err := embedder.Embed(ctx, []string{text})
		if err != nil || len(vectors) == 0 || len(vectors[0]) == 0 {
			if m.logger != nil {
				m.logger.Ctx(ctx).Warn().Err(err).Str("entity_id", entityID).Str("entity_type", entityType).Msg("mnemos: async embed skipped")
			}
			return
		}
		if err := embeddings.Upsert(ctx, entityID, entityType, vectors[0], embedding.ModelIDOf(embedder), m.actorID); err != nil {
			if m.logger != nil {
				m.logger.Ctx(ctx).Warn().Err(err).Str("entity_id", entityID).Str("entity_type", entityType).Msg("mnemos: async embed upsert failed")
			}
		}
	}()
}

// eventToEntityState maps a public mnemos.Event into a chronos.EntityState
// suitable for the bundled in-process engine. See [memory.RememberEvent]
// for the mapping rationale.
func (m *memory) eventToEntityState(e Event, eventID string) chronos.EntityState {
	typ := strings.TrimSpace(e.Type)
	if typ == "" {
		typ = "untyped"
	}
	runID := e.RunID
	if runID == "" {
		runID = "default"
	}
	meta := map[string]string{
		"mnemos_event_id": eventID,
		"event_type":      typ,
	}
	if c := strings.TrimSpace(e.Content); c != "" {
		if len(c) > 200 {
			c = c[:200]
		}
		meta["content_preview"] = c
	}
	// Tenant-scope the entity + scope keys so a single shared chronos engine
	// never merges two tenants' series (chronos isolates by entity/scope, not
	// tenant). Tenant ids are NUL-free (validated by tenantRE in Tenant()), so
	// the NUL separator unambiguously delimits the tenant prefix from the
	// value — a tenant-prefixed key can never equal an unscoped key.
	entityKey := typ
	if m.tenant != "" {
		entityKey = m.tenant + "\x00" + typ
		meta["tenant"] = m.tenant
	}
	// Use the event's real numeric features when present so Chronos detects
	// patterns on actual VALUES; fall back to the presence default (a single
	// 1.0) so cadence is still tracked for presence-only events.
	features := e.Features
	if len(features) == 0 {
		features = []float64{1.0}
	}
	return chronos.EntityState{
		ID:        uuid.New(),
		EntityID:  uuid.NewSHA1(chronosEventNamespace, []byte(entityKey)),
		ScopeID:   m.chronosScopeID(runID),
		Timestamp: e.At.UTC(),
		Features:  features,
		Labels:    []string{"event"},
		Meta:      meta,
	}
}

// isValidClaimType reports whether t is acceptable for a write: a built-in
// claim type, or one the consumer registered via WithClaimTypes. This is the
// configurable-vocabulary check the write executor enforces at the boundary.
func (m *memory) isValidClaimType(t domain.ClaimType) bool {
	if domain.IsBuiltinClaimType(t) {
		return true
	}
	for _, extra := range m.cfg.extraClaimTypes {
		if domain.ClaimType(extra) == t {
			return true
		}
	}
	return false
}

// chronosScopeID derives the Chronos scope UUID for a run, applying the same
// tenant mix + default-run rule eventToEntityState uses to STORE events — so
// [Memory.Signals] reads signals from the exact scope events were written under.
func (m *memory) chronosScopeID(runID string) uuid.UUID {
	if runID == "" {
		runID = "default"
	}
	scopeKey := runID
	if m.tenant != "" {
		scopeKey = m.tenant + "\x00" + runID
	}
	return uuid.NewSHA1(chronosEventNamespace, []byte(scopeKey))
}

// Timeline implements [Memory.Timeline].
func (m *memory) Timeline(ctx context.Context, q TimelineQuery) ([]Event, error) {
	var events []domain.Event
	var err error
	if q.RunID != "" {
		events, err = m.conn.Events.ListByRunID(ctx, q.RunID)
	} else {
		events, err = m.conn.Events.ListAll(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("mnemos: list events: %w", err)
	}

	out := make([]Event, 0, len(events))
	for _, ev := range events {
		if !q.From.IsZero() && ev.Timestamp.Before(q.From) {
			continue
		}
		if !q.To.IsZero() && ev.Timestamp.After(q.To) {
			continue
		}
		typ := ev.Metadata["event_type"]
		if len(q.Types) > 0 && !slices.Contains(q.Types, typ) {
			continue
		}
		out = append(out, Event{
			ID:       ev.ID,
			At:       ev.Timestamp,
			Type:     typ,
			Content:  ev.Content,
			Metadata: copyMetaWithout(ev.Metadata, "event_type"),
			RunID:    ev.RunID,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })

	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// Signals implements [Memory.Signals]. It asks the bundled Chronos engine to
// detect temporal patterns over the run's scope (recomputing from the stored
// event series, so the read reflects everything ingested so far), then projects
// them onto the public [Signal] shape. No Chronos wired, or nothing detected,
// yields (nil, nil) — an empty result means "all quiet", never an error.
func (m *memory) Signals(ctx context.Context, q SignalQuery) ([]Signal, error) {
	if m.chronos == nil {
		return nil, nil
	}
	scopeID := m.chronosScopeID(q.RunID)
	detected, err := m.chronos.Detect(ctx, []uuid.UUID{scopeID})
	if err != nil {
		return nil, fmt.Errorf("mnemos: detect signals: %w", err)
	}
	runID := q.RunID
	if runID == "" {
		runID = "default"
	}
	out := make([]Signal, 0, len(detected))
	for _, s := range detected {
		if q.MinConfidence > 0 && s.Confidence < q.MinConfidence {
			continue
		}
		out = append(out, Signal{
			Pattern:     string(s.Pattern),
			RunID:       runID,
			Strength:    s.Strength,
			Confidence:  s.Confidence,
			Class:       string(s.ConfidenceClass),
			DetectedAt:  s.DetectedAt,
			WindowStart: s.Window.Start,
			WindowEnd:   s.Window.End,
		})
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

// discardLogger backs [memory.log] when no logger was wired (library consumers that
// never call WithLogger) — a nil *bolt.Logger would panic, so calls route here to /dev/null.
var discardLogger = bolt.New(bolt.NewJSONHandler(io.Discard))

// log returns the memory's structured logger, or a discard logger when none was wired
// (ADR 0021), so operational log calls are nil-safe.
func (m *memory) log() *bolt.Logger {
	if m.logger != nil {
		return m.logger
	}
	return discardLogger
}

// Consolidate implements [Memory.Consolidate] — the maintenance "sleep" pass.
func (m *memory) Consolidate(ctx context.Context, opts ConsolidateOptions) (ConsolidateResult, error) {
	threshold := opts.DedupeThreshold
	if threshold <= 0 {
		threshold = 0.92
	}
	plan, err := pipeline.PlanSemanticDedupe(ctx, m.conn, threshold)
	if err != nil {
		return ConsolidateResult{}, fmt.Errorf("mnemos: consolidate: plan dedupe: %w", err)
	}
	res := ConsolidateResult{ClaimsScanned: plan.ClaimsScanned, DuplicateGroups: len(plan.Merges)}
	if opts.DryRun {
		// Preview only — report what dedupe WOULD collapse; no mutation, and
		// forgetting (which acts on freshly-recomputed trust) is not simulated.
		return res, nil
	}
	if len(plan.Merges) > 0 {
		merged, err := pipeline.ApplySemanticDedupe(ctx, m.conn, plan)
		if err != nil {
			return res, fmt.Errorf("mnemos: consolidate: apply dedupe: %w", err)
		}
		res.Merged = merged
	}
	// Recompute trust (confidence × corroboration × freshness) — the
	// renormalisation half of the sleep pass. Merging changed evidence counts,
	// and freshness decays with wall-clock time regardless. Best-effort: a
	// scorer-less backend still consolidated correctly.
	if scorer, ok := m.conn.Claims.(ports.TrustScorer); ok {
		now := time.Now().UTC()
		if n, terr := scorer.RecomputeTrust(ctx, func(confidence float64, evidenceCount int, latestEvidence time.Time) float64 {
			return trust.Score(confidence, evidenceCount, latestEvidence, now)
		}); terr == nil {
			res.TrustRefreshed = n
		}
	}
	// Learning dynamics (ADR 0015) share one scan of the recent prediction-error
	// series: neuromodulation derives the global plasticity gain from it (§2), and
	// replay folds each belief's surprise into its rehearsal priority (§3). Computed
	// once here — after the trust recompute, before credit and replay consume it —
	// and only when a consumer is enabled, so the common (no-learning-dynamics) pass
	// pays nothing.
	var (
		surpriseSeries  []float64
		surpriseByClaim map[string]float64
	)
	if opts.Plastic || opts.ReplayTopK > 0 {
		series, byClaim, oerr := m.observedSurprises(ctx)
		if oerr != nil {
			return res, fmt.Errorf("mnemos: consolidate: observe surprise: %w", oerr)
		}
		surpriseSeries, surpriseByClaim = series, byClaim
	}
	// Credit assignment (ADR 0014): propagate the signed prediction error of each
	// resolved Expectation back to the beliefs that informed the decision it
	// predicted — validated predictions credit their beliefs, refuted ones blame
	// them, as a bounded, attributed trust delta on top of the evidence-based base
	// just recomputed. This is the missing half of learning: trust now reflects
	// not only "is this corroborated?" but "did acting on it work out?". Runs here,
	// right after the base recompute and BEFORE forgetting, so a belief reality
	// refuted can be swept up by --forget-below-trust in the same pass.
	//
	// When Plastic (ADR 0015) the update rate is modulated: the global neuromodulatory
	// gain (from the surprise series above) and, per belief, its metaplastic
	// resistance — both bounded so trust still moves at most ±CreditCap.
	var beliefChanges []beliefTrustChange // captured for the ADR-0018 journal below
	if opts.AssignCredit {
		gain := 1.0
		if opts.Plastic {
			gain = plasticity.Gain(surpriseSeries, plasticity.DefaultRecentWindow, plasticitySensitivity())
			res.PlasticityGain = gain
		}
		credited, changes, cerr := m.assignCredit(ctx, opts.Plastic, gain)
		if cerr != nil {
			return res, fmt.Errorf("mnemos: consolidate: assign credit: %w", cerr)
		}
		res.Credited = credited
		beliefChanges = changes
	}
	// Unified salience / stakes (ADR 0013 §4): derive each belief's consequence-
	// severity weight from the risk level and outcome severity of the decisions it
	// informed, writing it to confidence_components (no schema change — the same
	// map credit reuses). Salience biases retrieval (`query --salient`) and the
	// replay priority below, so it runs before replay so a freshly-tagged belief is
	// rehearsed preferentially in the same pass.
	if opts.AssignSalience {
		tagged, serr := m.deriveSalience(ctx)
		if serr != nil {
			return res, fmt.Errorf("mnemos: consolidate: assign salience: %w", serr)
		}
		res.SalienceTagged = tagged
	}
	// Replay-set selection (ADR 0015 §3), computed here — after salience derivation,
	// before forgetting — so the same top-K memories rehearsed at the end of the pass
	// are shielded from this pass's trust-decay pruning below (the rehearse↔prune
	// coupling that gives CLS its interference protection). Ranking = trust × salience
	// × recency × surprise; freshening happens last so it forward-protects too.
	var replayIDs []string
	var replayProtected map[string]bool
	if opts.ReplayTopK > 0 {
		ids, rerr := m.rankForReplay(ctx, surpriseByClaim, opts.ReplayTopK)
		if rerr != nil {
			return res, fmt.Errorf("mnemos: consolidate: rank replay: %w", rerr)
		}
		replayIDs = ids
		replayProtected = make(map[string]bool, len(ids))
		for _, id := range ids {
			replayProtected[id] = true
		}
	}
	// Active forgetting: prune stale, low-trust, non-promoted claims — the
	// offline synaptic renormalisation the brain does during sleep. Reduced
	// retrievability, not erasure (marked deprecated, history preserved). A memory in
	// the replay set is protected (ADR 0015 §3).
	if opts.ForgetBelowTrust > 0 {
		forgotten, protectedSkipped, ferr := m.forgetStaleClaims(ctx, opts.ForgetBelowTrust, replayProtected)
		if ferr != nil {
			return res, fmt.Errorf("mnemos: consolidate: forget: %w", ferr)
		}
		res.Forgotten = forgotten
		res.ReplayProtected = protectedSkipped
	}
	// Surprise-driven forgetting: a belief an observed outcome REFUTED should stop
	// surfacing — the prediction-error loop closing. Independent of trust decay.
	if opts.ForgetRefuted {
		refuted, rerr := m.forgetRefutedClaims(ctx)
		if rerr != nil {
			return res, fmt.Errorf("mnemos: consolidate: forget refuted: %w", rerr)
		}
		res.Refuted = refuted
	}
	// Confirmation routing: freshen claims a later outcome BORE OUT, so validated
	// beliefs resist forgetting — the mirror of ForgetRefuted. Together they route
	// prediction-error both ways: confirmed kept fresh, refuted let go.
	if opts.ReinforceValidated {
		validated, verr := m.reinforceValidatedClaims(ctx)
		if verr != nil {
			return res, fmt.Errorf("mnemos: consolidate: reinforce validated: %w", verr)
		}
		res.Validated = validated
	}
	// Skill synthesis: re-derive lessons + playbooks from accumulated experience —
	// the auto-trigger arrow that keeps the skill layer current without a manual CLI
	// run. Runs before reinforcement so freshly-derived playbooks tune this pass.
	if opts.Synthesize {
		sr, serr := m.Synthesize(ctx)
		if serr != nil {
			return res, fmt.Errorf("mnemos: consolidate: synthesize: %w", serr)
		}
		res.LessonsSynthesized = sr.LessonsDerived
		res.PlaybooksSynthesized = sr.PlaybooksDerived
	}
	// Skill reinforcement: bend each playbook's confidence toward the observed
	// success rate of the outcomes on the actions its lessons were derived from —
	// the learning half of the sleep pass, closing the loop so a write-once skill
	// store becomes self-tuning. Deterministic, no LLM; independent of the
	// forgetting steps above.
	if opts.ReinforcePlaybooks {
		reinforced, perr := m.reinforcePlaybooks(ctx)
		if perr != nil {
			return res, fmt.Errorf("mnemos: consolidate: reinforce playbooks: %w", perr)
		}
		res.PlaybooksReinforced = reinforced
	}
	// Prioritized replay: rehearse the most important memories so they resist the
	// decay that prunes the mundane — the SWS rehearsal stage. Forward-looking (it
	// freshens for future passes), so it runs last. The set was chosen before
	// forgetting (above) so the same memories were also protected this pass.
	if opts.ReplayTopK > 0 {
		replayed, rperr := m.replayFreshen(ctx, replayIDs)
		if rperr != nil {
			return res, fmt.Errorf("mnemos: consolidate: replay: %w", rperr)
		}
		res.Replayed = replayed
	}
	// Hebbian decay (ADR 0015 §5): pull co-activated association edges back toward the
	// base 1.0 so edge strength reflects RECENT use, not a lifetime tally — the sleep-
	// pass complement to the `query --hebbian` write-back. Never deletes an edge. A
	// no-op on backends that don't persist edge strength.
	if opts.DecayAssociations {
		if strengthener, ok := m.conn.Relationships.(ports.RelationshipStrengthener); ok {
			decayed, derr := strengthener.DecayAssociations(ctx, associationDecayRetain)
			if derr != nil {
				return res, fmt.Errorf("mnemos: consolidate: decay associations: %w", derr)
			}
			res.AssociationsDecayed = decayed
		}
	}
	// Inhibition decay (ADR 0016): pull each claim's retrieval-suppression weight back
	// toward 0 so a competitive-inhibition penalty is temporary — a formerly-beaten
	// contradiction loser recovers its retrievability unless the winner keeps winning.
	// Never touches trust.
	if opts.DecayInhibition {
		decayed, ierr := m.decayInhibition(ctx)
		if ierr != nil {
			return res, fmt.Errorf("mnemos: consolidate: decay inhibition: %w", ierr)
		}
		res.InhibitionDecayed = decayed
	}
	// Cognitive journal (ADR 0018): record what this pass did — the pass-level result +
	// a PredictiveError snapshot, and the per-belief credit trust moves — for research
	// and constant tuning. A side record; a journal failure surfaces (it is the
	// operator's data) but is only reached under the explicit --journal flag.
	if opts.Journal {
		if err := m.journalConsolidation(ctx, res, beliefChanges); err != nil {
			return res, fmt.Errorf("mnemos: consolidate: journal: %w", err)
		}
	}
	// Operational log (ADR 0021): one structured line per consolidation pass — the "what
	// did the sleep pass do" record for Loki/Grafana. Not reached on a dry run (returned
	// earlier).
	m.log().Ctx(ctx).Info().
		Int("scanned", res.ClaimsScanned).
		Int("merged", res.Merged).
		Int("credited", res.Credited).
		Int("forgotten", res.Forgotten).
		Int("refuted", res.Refuted).
		Int("validated", res.Validated).
		Int("replayed", res.Replayed).
		Int("associations_decayed", res.AssociationsDecayed).
		Int("inhibition_decayed", res.InhibitionDecayed).
		Int("lessons_synthesized", res.LessonsSynthesized).
		Int("playbooks_synthesized", res.PlaybooksSynthesized).
		Float64("plasticity_gain", res.PlasticityGain).
		Msg("mnemos: consolidation pass")
	// Anomaly: a pass that prunes/refutes an unusually large number of beliefs is worth
	// an alert.
	if pruned := res.Forgotten + res.Refuted; pruned >= massForgetWarnThreshold {
		m.log().Ctx(ctx).Warn().
			Int("forgotten", res.Forgotten).
			Int("refuted", res.Refuted).
			Msg("mnemos: consolidation pruned a large number of beliefs")
	}
	return res, nil
}

// massForgetWarnThreshold is how many beliefs a single consolidation pass must
// forget+refute before it logs a warning (ADR 0021) — a possible over-pruning anomaly.
const massForgetWarnThreshold = 100

// journalConsolidation writes one pass-level cognitive-journal entry (the
// ConsolidateResult + a PredictiveError snapshot) plus one belief_trust entry per
// credit-driven trust move (ADR 0018). A no-op on backends without a journal.
func (m *memory) journalConsolidation(ctx context.Context, res ConsolidateResult, changes []beliefTrustChange) error {
	if m.conn.Journal == nil {
		return nil
	}
	now := time.Now().UTC()
	passID := uuid.NewString()

	pe, err := m.PredictiveError(ctx)
	if err != nil {
		return fmt.Errorf("predictive-error snapshot: %w", err)
	}
	passJSON, err := json.Marshal(struct {
		Result          ConsolidateResult `json:"result"`
		PredictiveError PredictiveError   `json:"predictive_error"`
	}{res, pe})
	if err != nil {
		return fmt.Errorf("marshal pass entry: %w", err)
	}
	entries := make([]domain.JournalEntry, 0, 1+len(changes))
	entries = append(entries, domain.JournalEntry{
		ID: passID, At: now, Kind: domain.JournalKindConsolidation, Data: string(passJSON),
	})
	for i, ch := range changes {
		b, err := json.Marshal(struct {
			PassID string  `json:"pass_id"`
			Before float64 `json:"before"`
			After  float64 `json:"after"`
			Delta  float64 `json:"delta"`
		}{passID, ch.Before, ch.After, ch.After - ch.Before})
		if err != nil {
			return fmt.Errorf("marshal belief_trust entry: %w", err)
		}
		entries = append(entries, domain.JournalEntry{
			ID:        fmt.Sprintf("%s:%d", passID, i),
			At:        now,
			Kind:      domain.JournalKindBeliefTrust,
			SubjectID: ch.ClaimID,
			Data:      string(b),
		})
	}
	return m.conn.Journal.Append(ctx, entries)
}

// inhibitionDecayRetain sheds half of a claim's competitive-inhibition weight each
// consolidation pass, and inhibitionDecayFloor is the residue below which the component
// is removed outright — so a suppression fully clears after a few sleeps unless renewed.
const (
	inhibitionDecayRetain = 0.5
	inhibitionDecayFloor  = 0.01
)

// decayInhibition pulls every claim's competitive-inhibition weight (ADR 0016) toward 0,
// preserving all other confidence_components and passing trust through UNCHANGED (the
// suppression is retrievability-only). Reuses the credit writer; a no-op on backends
// that don't persist confidence_components.
func (m *memory) decayInhibition(ctx context.Context) (int, error) {
	writer, ok := m.conn.Claims.(ports.BeliefCreditWriter)
	if !ok {
		return 0, nil
	}
	all, err := m.conn.Claims.ListAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("list claims: %w", err)
	}
	decayed := 0
	for _, c := range all {
		cur, has := c.ConfidenceComponents[domain.InhibitionComponentKey]
		if !has || cur <= 0 {
			continue
		}
		merged := make(map[string]float64, len(c.ConfidenceComponents))
		for k, v := range c.ConfidenceComponents {
			merged[k] = v
		}
		if next := cur * inhibitionDecayRetain; next < inhibitionDecayFloor {
			delete(merged, domain.InhibitionComponentKey) // fully cleared
		} else {
			merged[domain.InhibitionComponentKey] = next
		}
		if err := writer.ApplyBeliefCredit(ctx, c.ID, merged, c.TrustScore); err != nil {
			return decayed, fmt.Errorf("decay inhibition %s: %w", c.ID, err)
		}
		decayed++
	}
	return decayed, nil
}

// associationDecayRetain is the fraction of an edge's over-base strength kept per
// consolidation pass (ADR 0015 §5): 0.8 sheds 20% of the excess each sleep, so a burst
// of co-activation fades over several passes unless renewed — recency-weighted, and
// asymptotic to the base 1.0 (never below, never deleted).
const associationDecayRetain = 0.8

// replayRecencyHalfLifeDays sets how fast replay priority decays with a claim's
// age — recent memories are rehearsed preferentially (recency-weighted replay).
const replayRecencyHalfLifeDays = 30.0

// replaySurpriseWeight bounds how much a belief's prediction error lifts its replay
// priority (ADR 0015 §3): a fully-surprising belief is rehearsed up to
// (1 + replaySurpriseWeight)× a same-salience unsurprising one. Kept modest so
// surprise reorders within a salience tier rather than overriding it.
const replaySurpriseWeight = 1.0

// rankForReplay returns the currently-valid claim IDs ranked by replay priority,
// highest first, truncated to the top k (ADR 0015 §3). Priority = intrinsic
// importance (trust.Salience) × recency × the bounded STAKES multiplier (ADR 0013
// §4) × a surprise factor (1 + replaySurpriseWeight·surprise): a high-consequence,
// recently-active, high-prediction-error belief is rehearsed first. surpriseByClaim
// may be nil (surprise factor collapses to 1). The ranking is computed before
// forgetting so the returned set can also be protected from the same pass's pruning.
func (m *memory) rankForReplay(ctx context.Context, surpriseByClaim map[string]float64, k int) ([]string, error) {
	if k <= 0 {
		return nil, nil
	}
	all, err := m.conn.Claims.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("list claims: %w", err)
	}
	evidence, err := m.conn.Claims.ListAllEvidence(ctx)
	if err != nil {
		return nil, fmt.Errorf("list evidence: %w", err)
	}
	evidenceCount := make(map[string]int, len(all))
	for _, e := range evidence {
		evidenceCount[e.ClaimID]++
	}
	now := time.Now().UTC()
	type pri struct {
		id    string
		score float64
	}
	ranked := make([]pri, 0, len(all))
	for _, c := range all {
		if !c.ValidTo.IsZero() {
			continue // only rehearse currently-valid memories
		}
		// Priority = intrinsic importance (trust.Salience) × recency × a bounded
		// STAKES multiplier (ADR 0013 §4) × a surprise factor (ADR 0015 §3): a
		// high-consequence, high-prediction-error belief is rehearsed preferentially.
		// Every factor is a re-weighting, not a gate — the currently-valid check above
		// still decides eligibility, so they reorder what is already admitted and never
		// smuggle in an ineligible memory. A neutral, unsurprising claim yields 1.0
		// multipliers, leaving the base score intact.
		surprise := 0.0
		if surpriseByClaim != nil {
			if s, ok := surpriseByClaim[c.ID]; ok {
				surprise = s
			}
		}
		score := trust.SalienceOf(c, evidenceCount[c.ID]) *
			replayRecency(c, now) *
			consolidate.SaliencePriorityMultiplier(c.EffectiveSalience()) *
			(1 + replaySurpriseWeight*surprise)
		ranked = append(ranked, pri{c.ID, score})
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	if len(ranked) > k {
		ranked = ranked[:k]
	}
	ids := make([]string, len(ranked))
	for i, p := range ranked {
		ids[i] = p.id
	}
	return ids, nil
}

// replayFreshen rehearses the given claims by bumping their freshness — the write
// half of prioritized experience replay. Returns the count actually freshened.
func (m *memory) replayFreshen(ctx context.Context, ids []string) (int, error) {
	now := time.Now().UTC()
	replayed := 0
	for _, id := range ids {
		// Rehearse: bump last_verified (0 half-life keeps any existing override).
		if err := m.conn.Claims.MarkVerified(ctx, id, now, 0); err != nil {
			return replayed, fmt.Errorf("replay claim %s: %w", id, err)
		}
		replayed++
	}
	return replayed, nil
}

// replayRecency is a [0,1] recency weight for replay priority: exponential decay
// over the age of the claim's freshest signal (last-verified / valid-from / created).
func replayRecency(c domain.Claim, now time.Time) float64 {
	ref := c.CreatedAt
	if c.LastVerified.After(ref) {
		ref = c.LastVerified
	}
	if c.ValidFrom.After(ref) {
		ref = c.ValidFrom
	}
	if ref.IsZero() {
		return 1.0
	}
	days := now.Sub(ref).Hours() / 24
	if days <= 0 {
		return 1.0
	}
	return math.Exp(-days / replayRecencyHalfLifeDays)
}

// refutedClaimIDs returns the ids of claims whose LATEST outcome verdict is
// "refuted" — an observed outcome (a refutes edge) contradicted the belief and no
// later outcome re-validated it. Shared shape with Calibration's verdict logic.
// latestClaimVerdicts returns, per claim that carries any validates/refutes edge,
// whether its LATEST verdict was a refutation (true) or a confirmation (false).
func (m *memory) latestClaimVerdicts(ctx context.Context) (map[string]bool, error) {
	validates, err := m.conn.EntityRels.ListByKind(ctx, string(domain.RelationshipTypeValidates))
	if err != nil {
		return nil, fmt.Errorf("list validates: %w", err)
	}
	refutes, err := m.conn.EntityRels.ListByKind(ctx, string(domain.RelationshipTypeRefutes))
	if err != nil {
		return nil, fmt.Errorf("list refutes: %w", err)
	}
	type verdict struct {
		refuted bool
		at      time.Time
	}
	latest := make(map[string]verdict)
	adopt := func(edges []domain.EntityRelationship, refuted bool) {
		for _, e := range edges {
			if e.ToType != domain.RelEntityClaim || e.ToID == "" {
				continue
			}
			if cur, ok := latest[e.ToID]; !ok || e.CreatedAt.After(cur.at) {
				latest[e.ToID] = verdict{refuted: refuted, at: e.CreatedAt}
			}
		}
	}
	adopt(validates, false)
	adopt(refutes, true)
	out := make(map[string]bool, len(latest))
	for id, v := range latest {
		out[id] = v.refuted
	}
	return out, nil
}

func (m *memory) refutedClaimIDs(ctx context.Context) (map[string]struct{}, error) {
	verdicts, err := m.latestClaimVerdicts(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{})
	for id, refuted := range verdicts {
		if refuted {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

// reinforceValidatedClaims freshens claims whose latest outcome verdict CONFIRMED
// them (a borne-out prediction) — routing the confirmation half of prediction-error
// into the freshness knob so validated beliefs resist forgetting (the flashbulb
// effect). The refutation half is handled by ForgetRefuted; together they route
// the surprise signal both ways.
func (m *memory) reinforceValidatedClaims(ctx context.Context) (int, error) {
	verdicts, err := m.latestClaimVerdicts(ctx)
	if err != nil {
		return 0, err
	}
	if len(verdicts) == 0 {
		return 0, nil
	}
	all, err := m.conn.Claims.ListAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("list claims: %w", err)
	}
	now := time.Now().UTC()
	n := 0
	for _, c := range all {
		if !c.ValidTo.IsZero() {
			continue // skip invalidated claims
		}
		refuted, ok := verdicts[c.ID]
		if !ok || refuted {
			continue // only claims whose latest verdict confirmed them
		}
		if err := m.conn.Claims.MarkVerified(ctx, c.ID, now, 0); err != nil {
			return n, fmt.Errorf("reinforce validated claim %s: %w", c.ID, err)
		}
		n++
	}
	return n, nil
}

// plasticitySensitivity reads the neuromodulation sensitivity knob (ADR 0015 §2) —
// how strongly recent surprise volatility scales the global plasticity gain. Default
// 1.0 (active whenever --plastic is set); MNEMOS_PLASTICITY_SENSITIVITY=0 disables
// just the neuromodulation half, leaving per-belief metaplasticity on.
func plasticitySensitivity() float64 {
	v := os.Getenv("MNEMOS_PLASTICITY_SENSITIVITY")
	if v == "" {
		return 1.0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 1.0
	}
	return f
}

// observedSurprises gathers the observed prediction errors of the beliefs that
// informed recorded decisions — the shared substrate for the two ADR-0015 mechanisms
// that consume surprise: neuromodulation (the time-ordered series, for the global
// plasticity gain) and prioritized replay (the per-belief map, for rehearsal
// priority). Each belief carrying a resolved (observed) expectation contributes
// exp.Surprise() once; the series is ordered oldest→newest by the expectation's
// creation time. Empty (not an error) on stores without decisions/expectations.
func (m *memory) observedSurprises(ctx context.Context) ([]float64, map[string]float64, error) {
	if m.conn.Expectations == nil || m.conn.Decisions == nil {
		return nil, nil, nil
	}
	decisions, err := m.conn.Decisions.ListAll(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list decisions: %w", err)
	}
	type obs struct {
		at       time.Time
		surprise float64
	}
	var samples []obs
	byClaim := map[string]float64{}
	seen := map[string]bool{}
	for _, d := range decisions {
		for _, b := range d.Beliefs {
			if seen[b] {
				continue
			}
			seen[b] = true
			exp, ok, gerr := m.conn.Expectations.Get(ctx, b)
			if gerr != nil {
				return nil, nil, fmt.Errorf("get expectation %s: %w", b, gerr)
			}
			if !ok {
				continue
			}
			s, ok := exp.Surprise()
			if !ok {
				continue // unobserved prediction — no error yet
			}
			byClaim[b] = s
			samples = append(samples, obs{at: exp.CreatedAt, surprise: s})
		}
	}
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].at.Before(samples[j].at) })
	series := make([]float64, len(samples))
	for i, s := range samples {
		series[i] = s.surprise
	}
	return series, byClaim, nil
}

// assignCredit runs credit assignment (ADR 0014): it propagates the signed
// prediction error of each resolved Expectation back to the beliefs that informed
// the Decision it predicted, as a bounded, attributed, idempotent trust delta on
// top of the evidence-based base trust. Returns the number of beliefs credited.
//
// Linkage. An Expectation attaches to a belief claim; a Decision lists that claim
// among its Beliefs. So the chain the ADR names — Decision → Beliefs[] → Outcome,
// with Expectation surprise — is walked by: for each decision, look up the
// expectation on each of its beliefs; when one has an observation, reconcile it
// into a signed error and split that credit equally across ALL of the decision's
// beliefs (they were the load-bearing inputs). The pure [credit.Assign] computes
// the contributions; this method is the I/O shell.
//
// Attribution + idempotency. Each contribution is stored in the belief's
// confidence_components map under a key that encodes the driving decision and
// prediction — the audit trail the ADR-0011 guardrail requires. The map is
// rewritten by assignment (credit entries replaced, other components preserved),
// and the trust delta is applied as base+creditSum where base is the freshly
// recomputed evidence trust — so re-running produces byte-identical components and
// the same trust, never a double-credit.
//
// Degradation. A no-op when the store lacks decisions or expectations, or when the
// claim repository does not persist the confidence_components audit map
// ([ports.BeliefCreditWriter]) — credit is never applied where it could not be
// attributed. Relies on RecomputeTrust having run first (Consolidate does so
// immediately before), so each claim's TrustScore is the evidence-based base.
func (m *memory) assignCredit(ctx context.Context, metaplastic bool, gain float64) (int, []beliefTrustChange, error) {
	if m.conn.Expectations == nil || m.conn.Decisions == nil {
		return 0, nil, nil
	}
	now := time.Now().UTC()
	writer, ok := m.conn.Claims.(ports.BeliefCreditWriter)
	if !ok {
		return 0, nil, nil // backend does not persist the credit audit map — skip, don't guess
	}

	decisions, err := m.conn.Decisions.ListAll(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("list decisions: %w", err)
	}
	// Fetch the expectation attached to each belief a decision relied on.
	exps := make(map[string]domain.Expectation)
	for _, d := range decisions {
		for _, b := range d.Beliefs {
			if _, seen := exps[b]; seen {
				continue
			}
			exp, ok, gerr := m.conn.Expectations.Get(ctx, b)
			if gerr != nil {
				return 0, nil, fmt.Errorf("get expectation %s: %w", b, gerr)
			}
			if ok {
				exps[b] = exp
			}
		}
	}
	contribs := credit.Assign(decisions, exps)

	all, err := m.conn.Claims.ListAll(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("list claims: %w", err)
	}
	credited := 0
	var changes []beliefTrustChange
	for _, c := range all {
		fresh := contribs[c.ID]
		// Preserve non-credit components; replace all credit:* keys with the fresh set.
		merged := make(map[string]float64)
		hadCredit := false
		for k, v := range c.ConfidenceComponents {
			if credit.IsCreditKey(k) {
				hadCredit = true
				continue
			}
			merged[k] = v
		}
		for _, ct := range fresh {
			merged[ct.Key] = ct.Delta
		}
		if len(fresh) == 0 && !hadCredit {
			continue // no credit now, none before → nothing to write
		}
		// base+creditSum: c.TrustScore is the evidence base (RecomputeTrust just ran),
		// so applying the clamped credit sum each pass is idempotent and self-healing.
		// Under Plastic (ADR 0015) the sum is modulated by the belief's metaplastic
		// resistance (crystallization) and the global neuromodulatory gain; with it off
		// (resistance=1, gain=1) this is exactly credit.SumFor — behaviour unchanged.
		resistance := 1.0
		if metaplastic {
			resistance = credit.ResistanceFor(c, now)
		}
		newTrust := clamp01(c.TrustScore + credit.SumForModulated(fresh, resistance, gain))
		if err := writer.ApplyBeliefCredit(ctx, c.ID, merged, newTrust); err != nil {
			return credited, changes, fmt.Errorf("apply belief credit %s: %w", c.ID, err)
		}
		if len(fresh) > 0 {
			credited++
			// Record the trust move for the cognitive journal (ADR 0018) — only when a
			// credit was actually applied this pass.
			changes = append(changes, beliefTrustChange{ClaimID: c.ID, Before: c.TrustScore, After: newTrust})
		}
	}
	return credited, changes, nil
}

// beliefTrustChange is one belief's credit-driven trust move in a consolidation pass,
// captured for the cognitive journal (ADR 0018).
type beliefTrustChange struct {
	ClaimID string
	Before  float64
	After   float64
}

// deriveSalience derives the unified salience / stakes weight (ADR 0013 §4) for
// beliefs that informed a Decision and writes it to the belief's
// confidence_components map under domain.SalienceComponentKey — the same
// no-new-column pattern credit assignment uses, so no store schema changes.
//
// Sourcing. For each decision, every belief it lists gets a candidate salience
// from the pure [salience.Compute]: the decision's RiskLevel and, when the
// decision has a linked outcome, that outcome's severity (higher risk/severity ⇒
// higher salience). A belief cited by several decisions takes the MAX across them
// ("highest stakes wins"). The derived value is then combined by max with any
// value already on the claim, so an explicit override (claim record --salience /
// claim salience set) is never lowered and re-running is idempotent (max is a
// stable fixpoint).
//
// Persistence reuses ports.BeliefCreditWriter.ApplyBeliefCredit — the existing
// component-writer capability — passing the claim's CURRENT trust score unchanged
// (salience is inert to trust; only credit moves it). Degrades to a no-op when the
// store lacks decisions or a component-persisting claim repository. Returns the
// number of beliefs whose salience component was written.
func (m *memory) deriveSalience(ctx context.Context) (int, error) {
	if m.conn.Decisions == nil {
		return 0, nil
	}
	writer, ok := m.conn.Claims.(ports.BeliefCreditWriter)
	if !ok {
		return 0, nil // backend does not persist confidence_components — skip, don't guess
	}

	decisions, err := m.conn.Decisions.ListAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("list decisions: %w", err)
	}

	// Derived stakes per belief: the max salience implied by every decision that
	// took it as a load-bearing input.
	derived := make(map[string]float64)
	for _, d := range decisions {
		in := salience.Inputs{RiskLevel: d.RiskLevel}
		if strings.TrimSpace(d.OutcomeID) != "" && m.conn.Outcomes != nil {
			if oc, oerr := m.conn.Outcomes.GetByID(ctx, d.OutcomeID); oerr == nil {
				in.Outcome = oc.Result
				in.HasOutcome = true
			}
		}
		s := salience.Compute(in)
		for _, b := range d.Beliefs {
			if strings.TrimSpace(b) == "" {
				continue
			}
			if s > derived[b] {
				derived[b] = s
			}
		}
	}
	if len(derived) == 0 {
		return 0, nil
	}

	all, err := m.conn.Claims.ListAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("list claims: %w", err)
	}
	tagged := 0
	for _, c := range all {
		want, isBelief := derived[c.ID]
		if !isBelief {
			continue
		}
		cur, hadExplicit := c.Salience()
		// Highest stakes wins: never lower an existing (possibly explicit) salience.
		if hadExplicit && cur >= want {
			continue
		}
		// Salience TAGGING only elevates: a derived value at or below the neutral
		// baseline carries no stakes signal worth recording, so an unmarked belief a
		// low/medium-risk decision touched is left neutral rather than pushed down.
		if !hadExplicit && want <= domain.NeutralSalience {
			continue
		}
		merged := make(map[string]float64, len(c.ConfidenceComponents)+1)
		for k, v := range c.ConfidenceComponents {
			merged[k] = v
		}
		merged[domain.SalienceComponentKey] = want
		// Trust is unchanged — salience is inert to trust (only credit moves it).
		if err := writer.ApplyBeliefCredit(ctx, c.ID, merged, c.TrustScore); err != nil {
			return tagged, fmt.Errorf("write salience for %s: %w", c.ID, err)
		}
		tagged++
	}
	return tagged, nil
}

// forgetRefutedClaims invalidates currently-valid, non-promoted claims that an
// observed outcome refuted. Like forgetStaleClaims it closes valid-time (recall
// stops surfacing them; history preserved). Promoted claims are exempt — a
// refuted human belief is a Hypercorrection for review, not a silent forget.
func (m *memory) forgetRefutedClaims(ctx context.Context) (int, error) {
	refuted, err := m.refutedClaimIDs(ctx)
	if err != nil {
		return 0, err
	}
	if len(refuted) == 0 {
		return 0, nil
	}
	all, err := m.conn.Claims.ListAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("list claims: %w", err)
	}
	now := time.Now().UTC()
	n := 0
	for _, c := range all {
		if !c.ValidTo.IsZero() {
			continue // already invalidated
		}
		if c.Lifecycle == domain.ClaimLifecyclePromoted {
			continue // human-endorsed: surfaces as a hypercorrection, never silently forgotten
		}
		if _, ok := refuted[c.ID]; !ok {
			continue
		}
		if err := m.conn.Claims.SetValidity(ctx, c.ID, now); err != nil {
			return n, fmt.Errorf("invalidate refuted claim %s: %w", c.ID, err)
		}
		n++
	}
	return n, nil
}

// forgetStaleClaims invalidates non-promoted claims whose trust is below the
// floor by setting their valid-to to now — so recall (which filters by valid
// time) stops surfacing them, while the claim + its history are preserved (a
// salienceProtectFloor is the intrinsic-importance threshold above which a claim
// is kept by forgetStaleClaims even when its trust has fallen below the forget
// floor. Tuned so only genuinely consequential claims (a decision, or a well-
// corroborated + verified finding — see trust.Salience's weighting) clear it,
// while the low-confidence, single-source, unverified tail is still let go.
const salienceProtectFloor = 0.66

// point-in-time query can still see what was once believed). This is forgetting
// as reduced retrievability, not erasure. Promoted (human-endorsed) claims are
// never forgotten, regardless of decay. Already-invalidated claims are skipped.
// Intrinsically salient claims are also protected — see the salience gate below —
// as are claims in the same pass's replay set (protected, ADR 0015 §3), passed in.
// Returns (forgotten, protectedSkipped): the second counts claims that WOULD have
// been forgotten but were spared by the replay-set protection.
func (m *memory) forgetStaleClaims(ctx context.Context, belowTrust float64, protected map[string]bool) (int, int, error) {
	all, err := m.conn.Claims.ListAll(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("list claims: %w", err)
	}
	// Evidence counts feed the salience score's corroboration term. One scan of
	// all links → per-claim count; cheap next to iterating every claim.
	evidence, err := m.conn.Claims.ListAllEvidence(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("list evidence: %w", err)
	}
	evidenceCount := make(map[string]int, len(all))
	for _, e := range evidence {
		evidenceCount[e.ClaimID]++
	}
	now := time.Now().UTC()
	forgotten := 0
	protectedSkipped := 0
	for _, c := range all {
		if !c.ValidTo.IsZero() {
			continue // already invalidated (superseded or previously forgotten)
		}
		if c.Lifecycle == domain.ClaimLifecyclePromoted {
			continue // never forget human-endorsed knowledge
		}
		if c.TrustScore >= belowTrust {
			continue // still trusted enough to keep surfacing
		}
		// Salience gate (C1): trust decays, intrinsic importance does not. A
		// consequential claim — a decision, a corroborated + verified finding, a
		// high-authority source — is kept even once its trust has faded, mirroring
		// how the brain protects salient memories from the forgetting that prunes
		// the mundane. Only the low-salience tail is let go. See trust.Salience.
		if trust.SalienceOf(c, evidenceCount[c.ID]) >= salienceProtectFloor {
			continue
		}
		// Replay-set protection (ADR 0015 §3): a memory rehearsed this pass is not
		// pruned by the same pass — the rehearse↔prune coupling. Counted so the
		// result can report how many low-trust memories replay actually shielded.
		if protected[c.ID] {
			protectedSkipped++
			continue
		}
		if err := m.conn.Claims.SetValidity(ctx, c.ID, now); err != nil {
			return forgotten, protectedSkipped, fmt.Errorf("invalidate stale claim %s: %w", c.ID, err)
		}
		forgotten++
	}
	return forgotten, protectedSkipped, nil
}

// hypercorrectionTrustFloor is how established the challenged claim must be for a
// contradiction to count as a hypercorrection alert. Below it (and unpromoted),
// the belief wasn't entrenched enough for new counter-evidence to be surprising,
// so its contradiction is ordinary epistemic churn, not an alert.
const hypercorrectionTrustFloor = 0.7

// HypercorrectionDefaultLimit is the alert count [Memory.Hypercorrections]
// returns when the caller names none. The surface had no limit at all, so the
// response grew with the corpus — 300 alerts on a 10k-claim brain, ~1,500 on a
// 50k one, each carrying two claim statements. It is the front of a curation
// queue, and nobody reconciles more than a screenful at a time.
const HypercorrectionDefaultLimit = 50

// Hypercorrections implements [Memory.Hypercorrections]. It reads the epistemic
// graph — no recomputation — so it's a cheap, on-demand metacognition query.
//
// Bounded to [HypercorrectionDefaultLimit] most-established-first alerts; use
// [memory.HypercorrectionsBounded] for a different limit or to learn whether the
// answer was truncated.
func (m *memory) Hypercorrections(ctx context.Context) ([]Hypercorrection, error) {
	rep, err := m.HypercorrectionsBounded(ctx, 0)
	return rep.Hypercorrections, err
}

// HypercorrectionsBounded is [Memory.Hypercorrections] reporting the bounds it
// applied (see [BoundedCognition]). Bounds.Available is the true alert count —
// the detection pass is complete, only the response is capped.
func (m *memory) HypercorrectionsBounded(ctx context.Context, limit int) (HypercorrectionReport, error) {
	var bounds Bounds
	limit = capLimit(&bounds, limit, HypercorrectionDefaultLimit, MaxCognitiveResults)
	all, err := m.hypercorrectionList(ctx)
	if err != nil {
		return HypercorrectionReport{}, err
	}
	bounds.Scanned = len(all)
	bounds.Considered = len(all)
	bounds.Available = len(all)
	// hypercorrectionList already ranks most-established-first, so this cut keeps
	// the front of the curation queue rather than an arbitrary prefix.
	out := all
	if len(out) > limit {
		out = out[:limit]
		bounds.cut(BoundReasonResultLimit)
	}
	bounds.finish(len(out))
	return HypercorrectionReport{Hypercorrections: out, Bounds: bounds}, nil
}

// hypercorrectionList is the complete, ranked detection pass. Callers that need
// a COUNT rather than a page (the dissonance level of [Memory.PredictiveError])
// use it directly so the capped response never distorts a metric.
func (m *memory) hypercorrectionList(ctx context.Context) ([]Hypercorrection, error) {
	rels, err := m.conn.Relationships.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("mnemos: Hypercorrections: list relationships: %w", err)
	}
	var contradicts []domain.Relationship
	for _, r := range rels {
		if r.Type == domain.RelationshipTypeContradicts {
			contradicts = append(contradicts, r)
		}
	}
	if len(contradicts) == 0 {
		return nil, nil
	}
	claims, err := m.conn.Claims.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("mnemos: Hypercorrections: list claims: %w", err)
	}
	byID := make(map[string]domain.Claim, len(claims))
	for _, c := range claims {
		byID[c.ID] = c
	}
	// establishment ranks the two sides of a contradiction: human-promoted
	// knowledge outranks any trust score; otherwise rank by trust.
	establishment := func(c domain.Claim) float64 {
		if c.Lifecycle == domain.ClaimLifecyclePromoted {
			return 1.0 + c.TrustScore
		}
		return c.TrustScore
	}

	var out []Hypercorrection
	for _, r := range contradicts {
		a, aok := byID[r.FromClaimID]
		b, bok := byID[r.ToClaimID]
		if !aok || !bok {
			continue
		}
		// Only currently-valid claims: a contradiction of an already-forgotten
		// belief (valid-time closed) is history, not a live alert.
		if !a.ValidTo.IsZero() || !b.ValidTo.IsZero() {
			continue
		}
		// Deprecated is the other way a belief is retired. It closes status
		// rather than valid-time, so it was missed here: deprecating a claim —
		// via `prune --narration`, `forget`, or a human call — left its
		// contradictions counting as live dissonance forever, and the
		// brain-health vital could never be improved by retiring bad beliefs.
		// A retired belief's disagreements are history by the same reasoning
		// the valid-time check above already applies.
		if a.Status == domain.ClaimStatusDeprecated || b.Status == domain.ClaimStatusDeprecated {
			continue
		}
		// Superseding either side RESOLVES the conflict: a reviewer retires the
		// stale belief (or dismisses the bad challenger) by transitioning it to
		// superseded, and the alert clears. This is the resolution hook — a
		// superseded claim on either side means a human already adjudicated it.
		if a.Lifecycle == domain.ClaimLifecycleSuperseded || b.Lifecycle == domain.ClaimLifecycleSuperseded {
			continue
		}
		// The contradicted side is the more-established of the pair; the other is
		// the challenger prompting re-examination.
		contradicted, challenger := a, b
		if establishment(b) > establishment(a) {
			contradicted, challenger = b, a
		}
		promoted := contradicted.Lifecycle == domain.ClaimLifecyclePromoted
		if !promoted && contradicted.TrustScore < hypercorrectionTrustFloor {
			continue // the challenged belief wasn't established enough to be an alert
		}
		out = append(out, Hypercorrection{
			ContradictedClaimID:  contradicted.ID,
			ContradictedText:     contradicted.Text,
			ContradictedTrust:    contradicted.TrustScore,
			ContradictedPromoted: promoted,
			ChallengingClaimID:   challenger.ID,
			ChallengingText:      challenger.Text,
			DetectedAt:           r.CreatedAt,
		})
	}
	// Most-established-first: promoted before unpromoted, then by trust desc — the
	// front of the curation queue.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ContradictedPromoted != out[j].ContradictedPromoted {
			return out[i].ContradictedPromoted
		}
		return out[i].ContradictedTrust > out[j].ContradictedTrust
	})
	return out, nil
}

// predictiveErrorSurpriseSaturation maps a raw surprise (tolerance units; 1 = edge of
// the confirmed band) into [0,1] for the prediction-error surface: 0 → 0, an edge hit
// (1) → 0.5, a 2×-or-worse miss → 1. Bounds an otherwise unbounded refutation.
const predictiveErrorSurpriseSaturation = 2.0

func saturateSurprise(s float64) float64 {
	if s <= 0 {
		return 0
	}
	if s >= predictiveErrorSurpriseSaturation {
		return 1
	}
	return s / predictiveErrorSurpriseSaturation
}

// PredictiveError implements [Memory.PredictiveError] (ADR 0017): the read-only
// hierarchical prediction-error surface. It reads four existing signals — outcome
// surprise, schema surprise, active dissonance, calibration error — never writes, and
// aggregates only the levels that have data.
func (m *memory) PredictiveError(ctx context.Context) (PredictiveError, error) {
	levels := make([]PredictiveErrorLevel, 0, 4)

	// Level 1 — outcome: mean saturated surprise over resolved decision predictions.
	outcome := PredictiveErrorLevel{Level: "outcome"}
	series, _, err := m.observedSurprises(ctx)
	if err != nil {
		return PredictiveError{}, fmt.Errorf("mnemos: PredictiveError: outcome: %w", err)
	}
	outcome.Samples = len(series)
	if len(series) > 0 {
		sum := 0.0
		for _, s := range series {
			sum += saturateSurprise(s)
		}
		outcome.Error = sum / float64(len(series))
		outcome.Basis = fmt.Sprintf("mean outcome surprise over %d resolved prediction(s)", len(series))
	} else {
		outcome.Basis = "no resolved predictions yet"
	}
	levels = append(levels, outcome)

	// Level 2 — schema: mean saturated peak surprise over promoted generalizations.
	schema := PredictiveErrorLevel{Level: "schema"}
	if m.conn.GlobalSchemas != nil {
		schemas, serr := m.conn.GlobalSchemas.ListAll(ctx)
		if serr != nil {
			return PredictiveError{}, fmt.Errorf("mnemos: PredictiveError: schema: %w", serr)
		}
		sum := 0.0
		n := 0
		for _, sc := range schemas {
			if sc.HasSurprise {
				sum += saturateSurprise(sc.Surprise)
				n++
			}
		}
		schema.Samples = n
		if n > 0 {
			schema.Error = sum / float64(n)
			schema.Basis = fmt.Sprintf("mean peak surprise over %d generalization(s)", n)
		} else {
			schema.Basis = "no generalizations carry a surprise signal"
		}
	} else {
		schema.Basis = "backend has no schema tier"
	}
	levels = append(levels, schema)

	// Level 3 — dissonance: active high-stakes contradictions per belief.
	diss := PredictiveErrorLevel{Level: "dissonance"}
	// The complete list, not the capped response: this is a rate, so a page of
	// alerts would silently floor the dissonance error once the brain grew past
	// HypercorrectionDefaultLimit contradictions.
	hyper, herr := m.hypercorrectionList(ctx)
	if herr != nil {
		return PredictiveError{}, fmt.Errorf("mnemos: PredictiveError: dissonance: %w", herr)
	}
	claimCount, cerr := m.conn.Claims.CountAll(ctx)
	if cerr != nil {
		return PredictiveError{}, fmt.Errorf("mnemos: PredictiveError: dissonance count: %w", cerr)
	}
	diss.Samples = int(claimCount)
	if claimCount > 0 {
		diss.Error = math.Min(float64(len(hyper))/float64(claimCount), 1)
		diss.Basis = fmt.Sprintf("%d active hypercorrection(s) over %d belief(s)", len(hyper), claimCount)
	} else {
		diss.Basis = "no beliefs"
	}
	levels = append(levels, diss)

	// Level 4 — calibration: expected calibration error over adjudicated beliefs.
	cal := PredictiveErrorLevel{Level: "calibration"}
	c, calErr := m.Calibration(ctx)
	if calErr != nil {
		return PredictiveError{}, fmt.Errorf("mnemos: PredictiveError: calibration: %w", calErr)
	}
	cal.Samples = c.Samples
	if c.Samples > 0 {
		cal.Error = clamp01(c.ECE)
		cal.Basis = fmt.Sprintf("expected calibration error over %d adjudicated belief(s)", c.Samples)
	} else {
		cal.Basis = "no adjudicated beliefs yet"
	}
	levels = append(levels, cal)

	// Aggregate over the levels that HAVE data; hotspot = the highest-error such level.
	sum := 0.0
	n := 0
	hotspot := ""
	hi := -1.0
	for _, l := range levels {
		if l.Samples <= 0 {
			continue
		}
		sum += l.Error
		n++
		if l.Error > hi {
			hi = l.Error
			hotspot = l.Level
		}
	}
	total := 0.0
	if n > 0 {
		total = sum / float64(n)
	}
	return PredictiveError{Levels: levels, Total: total, Hotspot: hotspot}, nil
}

// Brain-health thresholds (ADR 0019). All five vitals are "higher is worse" rates or
// errors in [0,1], so one direction suffices: value >= crit → unhealthy, >= warn →
// degraded, else healthy.
const (
	healthFreeEnergyWarn, healthFreeEnergyCrit   = 0.40, 0.70
	healthCalibrationWarn, healthCalibrationCrit = 0.15, 0.30
	healthDissonanceWarn, healthDissonanceCrit   = 0.05, 0.15
	healthLowTrustWarn, healthLowTrustCrit       = 0.30, 0.60
	healthStalenessWarn, healthStalenessCrit     = 0.40, 0.70

	// healthLowTrustFloor is the trust below which a belief counts toward low_trust.
	healthLowTrustFloor = 0.30
	// healthSkillCoverage* grade the share of actions with NO observed outcome.
	// Generous thresholds: a run still in flight legitimately has unobserved
	// actions, so this is meant to catch a loop that never closes, not one
	// mid-cycle.
	healthSkillCoverageWarn, healthSkillCoverageCrit = 0.50, 0.90
	// healthStalenessHorizonDays: a currently-valid belief not verified/created within
	// this window counts toward staleness (a recency proxy for freshness decay).
	healthStalenessHorizonDays = 90.0

	// healthTrustDecayHorizonDays is how far ahead the trust_decay vital projects.
	// A month, for two reasons: it is the natural cadence at which anyone reviews
	// a brain, and it is shorter than every half-life in play (the 90-day default,
	// the 14-day volatile one), so the vital leads low_trust and staleness rather
	// than restating them after the fact.
	healthTrustDecayHorizonDays = 30.0
	// healthTrustDecay{Warn,Crit} are DERIVED, not chosen: they are the outflow
	// which, sustained over a quarter (three horizons), carries an entirely
	// trusted brain to low_trust's own warn / crit line on its own. Expressing
	// them as a share of the low_trust thresholds keeps the pair consistent —
	// re-tuning the floor's thresholds re-tunes the rate that fills it, instead
	// of leaving two constants to drift apart. (Approximate: it ignores the
	// compounding as the trusted base shrinks, which errs toward warning early.)
	healthTrustDecayWarn = healthLowTrustWarn / 3
	healthTrustDecayCrit = healthLowTrustCrit / 3
	// healthStaleExpectationCrit: an open-expectation backlog past this many overdue is
	// critical (the prediction loop is stalling).
	healthStaleExpectationCrit = 25
)

func gradeHigherWorse(value, warn, crit float64) HealthStatus {
	switch {
	case value >= crit:
		return HealthUnhealthy
	case value >= warn:
		return HealthDegraded
	default:
		return HealthOK
	}
}

// gradeWithSamples grades a vital only when something was actually measured.
//
// Zero samples yields HealthUnknown rather than the healthy that a 0.0 value
// would otherwise earn. Calibration is the live case: expected calibration error
// is 0.0 over an empty set, which is arithmetically true and reads as a clean
// bill of health for a signal nobody has fed.

// skillCoverageVital reports whether the derived layers are actually being
// built: outcomes observed per action, and whether any lesson was synthesised
// from them.
//
// Every other vital is computed over CLAIMS. Nothing measured the skill layer,
// so a brain could carry 86,505 beliefs with zero actions, zero outcomes and
// zero lessons and report five green vitals — which is exactly what happened
// here, and why finding it took a manual row count across 18 tables rather than
// a glance at `mnemos health`.
//
// The measure is the OUTCOME RATE rather than a raw count. Counts only say a
// store is non-empty; the rate says the loop is closing. An action with no
// observed outcome teaches nothing, and a pile of them is the specific failure
// worth surfacing: something is recording what was done but never what came of
// it.
//
// No actions at all is HealthUnknown, not unhealthy. A brain that has performed
// no operations has nothing to learn from yet, and grading that as a problem
// would make every fresh install look broken.
func (m *memory) skillCoverageVital(ctx context.Context) Vital {
	const name = "skill_coverage"
	if m.conn.Actions == nil || m.conn.Outcomes == nil {
		return Vital{name, 0, HealthUnknown, "action/outcome layer not supported by this store"}
	}
	actions, err := m.conn.Actions.ListAll(ctx)
	if err != nil {
		return Vital{name, 0, HealthUnknown, "could not read actions: " + err.Error()}
	}
	if len(actions) == 0 {
		return Vital{name, 0, HealthUnknown, "no actions recorded yet; nothing to learn from"}
	}
	outcomes, err := m.conn.Outcomes.ListAll(ctx)
	if err != nil {
		return Vital{name, 0, HealthUnknown, "could not read outcomes: " + err.Error()}
	}
	observed := map[string]struct{}{}
	for _, o := range outcomes {
		observed[o.ActionID] = struct{}{}
	}
	covered := 0
	for _, a := range actions {
		if _, ok := observed[a.ID]; ok {
			covered++
		}
	}
	rate := float64(covered) / float64(len(actions))

	lessons := 0
	if m.conn.Lessons != nil {
		if ls, lerr := m.conn.Lessons.ListAll(ctx); lerr == nil {
			lessons = len(ls)
		}
	}
	detail := fmt.Sprintf("%d/%d actions have an observed outcome; %d lesson(s) derived",
		covered, len(actions), lessons)

	// Graded on the UNCOVERED share so it reads like the other vitals, where
	// higher is worse.
	return Vital{name, rate, gradeHigherWorse(1-rate, healthSkillCoverageWarn, healthSkillCoverageCrit), detail}
}

// projectTrustDecay evaluates one currently-valid belief against the freshness
// decay model twice — now, and at the end of the horizon — and reports whether
// it counts as trusted today and, if so, whether it stops being trusted before
// the horizon is out.
//
// Why the model rather than the stored trust_score: the persisted score is
// recomputed with the GLOBAL half-life (pipeline's defaultTrustScorer and
// Consolidate both call trust.Score), so it cannot see a belief the extractor
// classified as volatile — precisely the belief whose decay matters most. The
// per-claim HalfLifeDays is honoured everywhere it is read (query staleness,
// curiosity, float-back) except in the number that gets written down, so the
// vital reads the model, not the column. Both endpoints come from the same
// evaluation, so the comparison is internally consistent whatever the level is.
//
// Two deliberate conservatisms, both erring toward under-reporting decay:
//   - the freshness reference is the belief's own ValidFrom / LastVerified
//     rather than its freshest evidence event's timestamp, which this scan does
//     not load. ValidFrom is set from the SOURCE event, so it is never newer
//     than the true reference; an older reference has already decayed further
//     and is likelier to sit on the freshness floor, where nothing decays.
//   - the evidence count is the raw link count, not the independence-graded one
//     (grading needs each event's author). It can only overstate corroboration,
//     which lifts the level and delays the crossing.
//
// A belief with neither a ValidFrom nor a LastVerified cannot be dated at all,
// so its decay is unmeasurable and it is excluded from both counts rather than
// silently counted as stable.
func projectTrustDecay(c domain.Claim, evidenceCount int, now, horizon time.Time) (isTrusted, willDecay bool) {
	ref := trust.FreshnessRef(c.ValidFrom, c.LastVerified)
	if ref.IsZero() {
		return false, false
	}
	if trust.ScoreWithHalfLife(c.Confidence, evidenceCount, ref, now, c.HalfLifeDays) < healthLowTrustFloor {
		return false, false // already below the floor: low_trust owns it, there is no fall left
	}
	return true, trust.ScoreWithHalfLife(c.Confidence, evidenceCount, ref, horizon, c.HalfLifeDays) < healthLowTrustFloor
}

// trustDecayVital reports the share of currently-trusted beliefs whose trust
// falls through the low-trust floor within the horizon unless something
// re-verifies them — the rate at which the brain is LOSING trust, as against
// low_trust's count of what it has already lost.
//
// Framed as a crossing rate rather than as aggregate trust lost per unit time,
// because the aggregate is perverse: exponential decay is steepest right after
// evidence arrives, so a brain being actively fed would score WORST, and one
// whose every belief had already bottomed out on the freshness floor would
// score a clean zero. The crossing rate is monotone in the direction anyone
// actually cares about and is directly actionable — it names beliefs that need
// re-verifying this month.
//
// Unknown when no belief is currently above the floor: an empty brain, or one
// whose beliefs have all already decayed. Neither has trust left to lose, and
// reporting a green 0 for the second would be the exact failure #325 fixed.
func trustDecayVital(trusted, decaying int) Vital {
	if trusted == 0 {
		return Vital{"trust_decay", 0, HealthUnknown,
			"no belief is currently above the trust floor; there is no decay to measure"}
	}
	rate := float64(decaying) / float64(trusted)
	return Vital{"trust_decay", rate, gradeHigherWorse(rate, healthTrustDecayWarn, healthTrustDecayCrit),
		fmt.Sprintf("%d/%d trusted beliefs decay below trust %.2f within %.0f days unless re-verified",
			decaying, trusted, healthLowTrustFloor, healthTrustDecayHorizonDays)}
}

func gradeWithSamples(samples int, value, warn, crit float64) HealthStatus {
	if samples <= 0 {
		return HealthUnknown
	}
	return gradeHigherWorse(value, warn, crit)
}

func worseHealth(a, b HealthStatus) HealthStatus {
	// HealthUnknown ranks BELOW ok: an unmeasured vital must not raise the
	// overall verdict, or every fresh brain would report degraded.
	rank := map[HealthStatus]int{HealthUnknown: -1, HealthOK: 0, HealthDegraded: 1, HealthUnhealthy: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// BrainHealth implements [Memory.BrainHealth] (ADR 0019): the unified read-only health
// verdict. It rolls up the cognitive vitals (reusing PredictiveError for the error
// signals) and runs the structural-integrity checks (orphan beliefs, dangling edges,
// stale-expectation backlog). Full-scan diagnostic; no writes.
func (m *memory) BrainHealth(ctx context.Context) (BrainHealth, error) {
	now := time.Now().UTC()
	claims, err := m.conn.Claims.ListAll(ctx)
	if err != nil {
		return BrainHealth{}, fmt.Errorf("mnemos: BrainHealth: list claims: %w", err)
	}
	evidence, err := m.conn.Claims.ListAllEvidence(ctx)
	if err != nil {
		return BrainHealth{}, fmt.Errorf("mnemos: BrainHealth: list evidence: %w", err)
	}
	pe, err := m.PredictiveError(ctx)
	if err != nil {
		return BrainHealth{}, fmt.Errorf("mnemos: BrainHealth: predictive error: %w", err)
	}
	cal, err := m.Calibration(ctx)
	if err != nil {
		return BrainHealth{}, fmt.Errorf("mnemos: BrainHealth: calibration: %w", err)
	}

	// --- Vitals ---
	// Reuse the PredictiveError dissonance level (active hypercorrections per belief).
	dissonance := 0.0
	for _, l := range pe.Levels {
		if l.Level == "dissonance" {
			dissonance = l.Error
		}
	}
	// low_trust + staleness over currently-valid beliefs.
	evidenceCount := make(map[string]int, len(claims))
	for _, e := range evidence {
		evidenceCount[e.ClaimID]++
	}
	claimIDs := make(map[string]struct{}, len(claims))
	validCount, lowTrust, stale, orphans := 0, 0, 0, 0
	// trust_decay reads the SAME beliefs forward: how many of those trusted today
	// stop being trusted within the horizon if nobody re-verifies them.
	decayHorizon := now.Add(time.Duration(healthTrustDecayHorizonDays * 24 * float64(time.Hour)))
	trusted, decaying := 0, 0
	for _, c := range claims {
		claimIDs[c.ID] = struct{}{} // every claim, so dangling-edge detection sees them all
		if !c.ValidTo.IsZero() {
			continue // valid-time closed → forgotten → not a currently-valid belief
		}
		if c.Status == domain.ClaimStatusDeprecated {
			// Deprecated is the other way a belief is retired — it closes STATUS
			// while leaving valid-time open — so this loop counted a deprecated
			// belief as "currently-valid" and let it inflate every vital:
			// validCount (the low-trust/staleness denominator) and, worse, the
			// orphan check, where a deprecated ungrounded belief kept raising an
			// integrity warning that deprecating it was supposed to clear. Same
			// class of bug as the dissonance vital counting contradictions into
			// deprecated beliefs. A retired belief is not a currently-valid one.
			continue
		}
		validCount++
		if c.TrustScore < healthLowTrustFloor {
			lowTrust++
		}
		ref := c.CreatedAt
		if c.LastVerified.After(ref) {
			ref = c.LastVerified
		}
		if now.Sub(ref).Hours()/24 > healthStalenessHorizonDays {
			stale++
		}
		if evidenceCount[c.ID] == 0 {
			orphans++ // a claim requires evidence — an orphan is a data-integrity smell
		}
		if t, d := projectTrustDecay(c, evidenceCount[c.ID], now, decayHorizon); t {
			trusted++
			if d {
				decaying++
			}
		}
	}
	rate := func(n int) float64 {
		if validCount == 0 {
			return 0
		}
		return float64(n) / float64(validCount)
	}
	lowTrustRate, stalenessRate := rate(lowTrust), rate(stale)

	vitals := []Vital{
		{"free_energy", pe.Total, gradeHigherWorse(pe.Total, healthFreeEnergyWarn, healthFreeEnergyCrit),
			fmt.Sprintf("overall prediction-error aggregate; most wrong at: %s", orNone(pe.Hotspot))},
		{"calibration", cal.ECE, gradeWithSamples(cal.Samples, cal.ECE, healthCalibrationWarn, healthCalibrationCrit),
			fmt.Sprintf("expected calibration error over %d adjudicated belief(s)", cal.Samples)},
		{"dissonance", dissonance, gradeHigherWorse(dissonance, healthDissonanceWarn, healthDissonanceCrit),
			"active high-stakes contradictions per belief"},
		{"low_trust", lowTrustRate, gradeHigherWorse(lowTrustRate, healthLowTrustWarn, healthLowTrustCrit),
			fmt.Sprintf("%d/%d valid beliefs below trust %.2f", lowTrust, validCount, healthLowTrustFloor)},
		{"staleness", stalenessRate, gradeHigherWorse(stalenessRate, healthStalenessWarn, healthStalenessCrit),
			fmt.Sprintf("%d/%d valid beliefs unverified in %.0f days", stale, validCount, healthStalenessHorizonDays)},
		trustDecayVital(trusted, decaying),
		m.skillCoverageVital(ctx),
	}

	// --- Pathologies (integrity checks that did not exist before) ---
	rels, err := m.conn.Relationships.ListAll(ctx)
	if err != nil {
		return BrainHealth{}, fmt.Errorf("mnemos: BrainHealth: list relationships: %w", err)
	}
	dangling := 0
	for _, r := range rels {
		if _, ok := claimIDs[r.FromClaimID]; !ok {
			dangling++
			continue
		}
		if _, ok := claimIDs[r.ToClaimID]; !ok {
			dangling++
		}
	}
	staleExpectations := 0
	if m.conn.Expectations != nil {
		open, oerr := m.conn.Expectations.ListOpen(ctx)
		if oerr != nil {
			return BrainHealth{}, fmt.Errorf("mnemos: BrainHealth: list open expectations: %w", oerr)
		}
		for _, exp := range open {
			if !exp.Horizon.IsZero() && exp.Horizon.Before(now) {
				staleExpectations++
			}
		}
	}
	orphanStatus := HealthOK
	if orphans > 0 {
		orphanStatus = HealthDegraded
	}
	danglingStatus := HealthOK
	if dangling > 0 {
		danglingStatus = HealthUnhealthy // referential corruption
	}
	staleExpStatus := HealthOK
	if staleExpectations >= healthStaleExpectationCrit {
		staleExpStatus = HealthUnhealthy
	} else if staleExpectations > 0 {
		staleExpStatus = HealthDegraded
	}
	pathologies := []Pathology{
		{"orphan_claims", orphans, orphanStatus, "currently-valid beliefs with zero evidence"},
		{"dangling_edges", dangling, danglingStatus, "relationships whose endpoint belief is missing"},
		{"stale_expectations", staleExpectations, staleExpStatus, "open predictions past their horizon (unreconciled)"},
	}

	// --- Overall verdict: worst of everything ---
	overall := HealthOK
	for _, v := range vitals {
		overall = worseHealth(overall, v.Status)
	}
	for _, p := range pathologies {
		overall = worseHealth(overall, p.Status)
	}
	return BrainHealth{Status: overall, Vitals: vitals, Pathologies: pathologies, At: now}, nil
}

func orNone(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// SnapshotHealth implements [Memory.SnapshotHealth] (ADR 0019): compute BrainHealth and
// append it to the cognitive journal (ADR 0018) as a `health` entry, so vital signs
// become a queryable time series. A no-op record on backends without a journal.
func (m *memory) SnapshotHealth(ctx context.Context) (BrainHealth, error) {
	h, err := m.BrainHealth(ctx)
	if err != nil {
		return h, err
	}
	if m.conn.Journal == nil {
		return h, nil
	}
	data, err := json.Marshal(h)
	if err != nil {
		return h, fmt.Errorf("mnemos: SnapshotHealth: marshal: %w", err)
	}
	entry := domain.JournalEntry{
		ID:   uuid.NewString(),
		At:   h.At,
		Kind: domain.JournalKindHealth,
		Data: string(data),
	}
	if err := m.conn.Journal.Append(ctx, []domain.JournalEntry{entry}); err != nil {
		return h, fmt.Errorf("mnemos: SnapshotHealth: append journal: %w", err)
	}
	return h, nil
}

// Calibration implements [Memory.Calibration]. Pure read over the outcome edges
// (validates / refutes) + claim confidences — no recomputation, no writes.
func (m *memory) Calibration(ctx context.Context) (Calibration, error) {
	validates, err := m.conn.EntityRels.ListByKind(ctx, string(domain.RelationshipTypeValidates))
	if err != nil {
		return Calibration{}, fmt.Errorf("mnemos: Calibration: list validates: %w", err)
	}
	refutes, err := m.conn.EntityRels.ListByKind(ctx, string(domain.RelationshipTypeRefutes))
	if err != nil {
		return Calibration{}, fmt.Errorf("mnemos: Calibration: list refutes: %w", err)
	}

	// One verdict per claim: an outcome validated (correct) or refuted (wrong) the
	// belief. If a claim was adjudicated more than once, the most recent outcome
	// wins — the latest observation is the truth we calibrate against.
	type verdict struct {
		correct bool
		at      time.Time
	}
	verdicts := make(map[string]verdict)
	adopt := func(edges []domain.EntityRelationship, correct bool) {
		for _, e := range edges {
			if e.ToType != domain.RelEntityClaim || e.ToID == "" {
				continue
			}
			if cur, ok := verdicts[e.ToID]; !ok || e.CreatedAt.After(cur.at) {
				verdicts[e.ToID] = verdict{correct: correct, at: e.CreatedAt}
			}
		}
	}
	adopt(validates, true)
	adopt(refutes, false)
	if len(verdicts) == 0 {
		return Calibration{}, nil
	}

	claims, err := m.conn.Claims.ListAll(ctx)
	if err != nil {
		return Calibration{}, fmt.Errorf("mnemos: Calibration: list claims: %w", err)
	}
	type claimInfo struct {
		conf   float64
		source string
	}
	infoByID := make(map[string]claimInfo, len(claims))
	for _, c := range claims {
		infoByID[c.ID] = claimInfo{conf: c.Confidence, source: c.CreatedBy}
	}

	// Per-source (author) running totals, for the reliability breakdown.
	type srcAcc struct {
		n        int
		sumConf  float64
		correct  int
		brierSum float64
	}
	bySource := make(map[string]*srcAcc)

	// Accumulate per confidence-decile bucket, and the running Brier sum.
	const nb = 10
	sumConf := make([]float64, nb)
	correct := make([]int, nb)
	count := make([]int, nb)
	var brierSum float64
	samples := 0
	for claimID, v := range verdicts {
		ci, ok := infoByID[claimID]
		if !ok {
			continue // adjudicated a claim we no longer have; skip
		}
		conf := ci.conf
		b := int(conf * nb)
		if b >= nb {
			b = nb - 1 // confidence 1.0 lands in the top bucket
		}
		if b < 0 {
			b = 0
		}
		count[b]++
		sumConf[b] += conf
		outcome := 0.0
		if v.correct {
			correct[b]++
			outcome = 1.0
		}
		sqErr := (conf - outcome) * (conf - outcome)
		brierSum += sqErr
		samples++

		sa := bySource[ci.source]
		if sa == nil {
			sa = &srcAcc{}
			bySource[ci.source] = sa
		}
		sa.n++
		sa.sumConf += conf
		sa.brierSum += sqErr
		if v.correct {
			sa.correct++
		}
	}

	res := Calibration{Samples: samples}
	var ece float64
	for b := 0; b < nb; b++ {
		if count[b] == 0 {
			continue
		}
		meanConf := sumConf[b] / float64(count[b])
		acc := float64(correct[b]) / float64(count[b])
		res.Buckets = append(res.Buckets, CalibrationBucket{
			Lower:          float64(b) / nb,
			Upper:          float64(b+1) / nb,
			Count:          count[b],
			MeanConfidence: meanConf,
			Accuracy:       acc,
		})
		ece += float64(count[b]) / float64(samples) * math.Abs(acc-meanConf)
	}
	res.ECE = ece
	res.Brier = brierSum / float64(samples)

	// Per-source breakdown, most-adjudicated first.
	for src, sa := range bySource {
		meanConf := sa.sumConf / float64(sa.n)
		acc := float64(sa.correct) / float64(sa.n)
		res.Sources = append(res.Sources, SourceCalibration{
			Source:         src,
			Samples:        sa.n,
			MeanConfidence: meanConf,
			Accuracy:       acc,
			Gap:            meanConf - acc,
			Brier:          sa.brierSum / float64(sa.n),
		})
	}
	sort.Slice(res.Sources, func(i, j int) bool {
		if res.Sources[i].Samples != res.Sources[j].Samples {
			return res.Sources[i].Samples > res.Sources[j].Samples
		}
		return res.Sources[i].Source < res.Sources[j].Source // stable tiebreak
	})
	return res, nil
}

// Close implements [Memory.Close].
//
// It drains background embedding work first. Without the drain, a handler that
// does `defer mem.Close()` (the remember_episode MCP tool, every short-lived
// CLI write) tore the connection away from a goroutine still mid-request.
func (m *memory) Close() error {
	if m.embedCancel != nil {
		m.embedCancel()
	}
	m.embedWG.Wait()

	var firstErr error
	if m.conn != nil {
		if err := m.conn.Close(); err != nil {
			firstErr = err
		}
		m.conn = nil
	}
	if m.chronos != nil && m.chronosOwned {
		if err := m.chronos.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	m.chronos = nil
	// Flush + close the kernel's durable evidence log (no-op when no
	// MNEMOS_AXI_EVIDENCE_LOG / WithEvidenceLog path was configured).
	if m.wkernel != nil {
		if err := m.wkernel.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// mergeMetadata produces the input metadata map for ingest.IngestText
// from an Item. The "source" key follows the same convention as the CLI
// (defaults to "raw_text" when none supplied).
func mergeMetadata(item Item) map[string]string {
	out := map[string]string{}
	maps.Copy(out, item.Metadata)
	if item.Source != "" {
		out["source"] = item.Source
	}
	return out
}

func copyMetaWithout(in map[string]string, skip string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if k == skip {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// newEventID returns a fresh, collision-resistant event id. Uses a
// random UUID rather than a timestamp so concurrent writes can't mint the
// same id (the same scheme internal/parser uses for its event ids).
func newEventID() string {
	return "ev_" + uuid.NewString()
}

// hasEvidence reports whether eventIDs contains at least one non-blank
// evidence reference. Whitespace-only ids do not count — the spec
// requires every stored claim to reference real backing evidence.
func hasEvidence(eventIDs []string) bool {
	return len(nonBlank(eventIDs)) > 0
}

// nonBlank returns the trimmed, non-empty entries of in, preserving order.
// Used to normalise evidence EventIDs before persistence.
func nonBlank(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// newClaimID returns a fresh, collision-resistant claim id. UUID-based
// for the same reason as [newEventID]: a UnixNano timestamp collides when
// two goroutines call RememberClaim within the same nanosecond.
func newClaimID() string {
	return "cl_" + uuid.NewString()
}

// newDecisionID returns a fresh, collision-resistant decision id.
func newDecisionID() string {
	return "dec_" + uuid.NewString()
}

// textGenAdapter wraps a [providers.TextGenerator] to satisfy the
// internal llm.Client contract Mnemos's engines consume.
type textGenAdapter struct {
	tg providers.TextGenerator
}

func newTextGenAdapter(tg providers.TextGenerator) llm.Client {
	return &textGenAdapter{tg: tg}
}

// Complete satisfies llm.Client by translating between the internal
// message shape and the public providers.TextGenerator surface.
func (a *textGenAdapter) Complete(ctx context.Context, messages []llm.Message) (llm.Response, error) {
	in := providers.GenerateTextInput{
		Messages: make([]providers.Message, len(messages)),
	}
	for i, m := range messages {
		in.Messages[i] = providers.Message{
			Role:    providers.Role(string(m.Role)),
			Content: m.Content,
		}
	}
	out, err := a.tg.GenerateText(ctx, in)
	if err != nil {
		return llm.Response{}, err
	}
	return llm.Response{
		Content:      out.Content,
		Model:        out.Model,
		InputTokens:  out.InputTokens,
		OutputTokens: out.OutputTokens,
	}, nil
}

// embedderAdapter wraps a [providers.Embedder] to satisfy the internal
// embedding.Client contract.
type embedderAdapter struct {
	e providers.Embedder
}

func newEmbedderAdapter(e providers.Embedder) embedding.Client {
	return &embedderAdapter{e: e}
}

// Embed satisfies embedding.Client by delegating to the public
// providers.Embedder.
func (a *embedderAdapter) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out, err := a.e.Embed(ctx, providers.EmbedInput{Texts: texts})
	if err != nil {
		return nil, err
	}
	return out.Vectors, nil
}

// ModelID satisfies embedding.ModelIdentifier by forwarding the wrapped
// provider's model id when it reports one (providers.ModelIdentifier), so
// recall can stamp + filter by model. Empty when the provider is unnamed.
func (a *embedderAdapter) ModelID() string {
	if mi, ok := a.e.(providers.ModelIdentifier); ok {
		return mi.ModelID()
	}
	return ""
}

// consumerMode maps the public Query.AgentConsumer switch onto the engine's
// consumer mode. Kept as a function so the zero value has one obvious meaning:
// a caller that says nothing gets contradictions explained, not silently
// resolved on its behalf.
func consumerMode(agent bool) domain.Consumer {
	if agent {
		return domain.ConsumerAgent
	}

	return domain.ConsumerUser
}
