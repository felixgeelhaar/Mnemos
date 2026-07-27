package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"go.klarlabs.de/mnemos/internal/auth"
	"go.klarlabs.de/mnemos/internal/consolidate"
	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/store"
)

// handlePromote runs ADR 0011 Phase B — the tenant→global promotion pass — as a
// one-shot CLI command:
//
//	mnemos consolidate --promote [--min-tenants N] [--gate auto|operator]
//	                   [--tenant-dsn <dsn> ... | --all-tenants --db <dsn>]
//	                   [--global-dsn <dsn>] [--sensitive <token> ...]
//	                   [--curate|--contribute --token <jwt>]
//	                   [--dry-run | --apply]
//
//	mnemos consolidate --promote approve <id> --global-dsn <dsn>
//
// Two eligibility-cleared promotion paths (ADR 0012), both applied only to
// class-level subjects (individual/unknown lessons are excluded up front and
// never promote):
//   - Emergent (default): a class-level lesson corroborated across ≥ MinTenants
//     distinct tenants. Corroboration is the quality+privacy signal.
//   - Curated (--curate/--contribute): a class-level lesson from a SINGLE source,
//     bypassing the corroboration gate but requiring a curator token bearing the
//     promote:global scope (--token <jwt> or MNEMOS_TOKEN). De-identification and
//     the contradiction gate still apply.
//
// It reads the synthesized Schemas (Lessons) from each tenant, runs the pure
// promotion engine (internal/consolidate) with its five gates, and emits a
// structured, auditable plan as JSON.
//
// Tenant input is one of two alternatives:
//   - --tenant-dsn <dsn> ... : an explicit federation of separate tenant stores.
//   - --all-tenants --db <dsn> : enumerate the tenants of ONE multi-tenant store
//     and read each one's lessons scoped to that tenant — the hosted shape,
//     where tenants are namespaces (sqlite/mysql/local libsql) or RLS scopes
//     (postgres) inside a single brain (ADR 0007). Each tenant is read under its
//     own scope only; no cross-tenant bleed at read time.
//
// Writing is OPT-IN. --dry-run (the default) never writes: an operator inspects
// the plan first. --apply persists the surviving candidates to the global
// (neocortex) store identified by --global-dsn: under GateAuto they land Active
// (in force); under GateOperator they land Pending, awaiting an explicit
// `--promote approve <id>` to activate. Only de-identified fields (statement,
// scope, polarity, corroboration/evidence counts, confidence, surprise) ever
// cross into the global store — see consolidate.PromotedLesson.ToGlobalSchema.
//
// Live multi-tenant reads: --all-tenants enumerates the tenants of a single
// multi-tenant store (store.EnumerateTenants) and reads each tenant's lessons
// under that tenant's scope — physical namespace isolation for sqlite/mysql/
// local libsql, and an explicit tenant-filtered read for postgres RLS. Only the
// READ path changed; the engine's gates and the de-identified write path are
// unchanged, so the no-leak guarantee is preserved.
func handlePromote(args []string, f Flags) {
	opts, err := parsePromoteOpts(args, f)
	if err != nil {
		exitWithMnemosError(false, err)
		return
	}

	ctx := context.Background()

	// Operator approval sub-verb: activate a single pending global record.
	if opts.approveID != "" {
		handlePromoteApprove(ctx, opts)
		return
	}

	if opts.allTenants && len(opts.tenantDSNs) > 0 {
		exitWithMnemosError(false, NewUserError("--all-tenants and --tenant-dsn are alternative inputs; supply one, not both"))
		return
	}

	// ADR 0012 curator gate: the curated single-source path requires a token
	// bearing promote:global. Enforce it BEFORE reading any tenant data so an
	// unauthorized --curate run does nothing.
	if opts.curate {
		if err := verifyCuratorToken(ctx, opts.token, opts.curatorRevocationDSN()); err != nil {
			exitWithMnemosError(false, err)
			return
		}
	}

	// Load per-tenant lessons. Two alternative inputs:
	//
	//   --all-tenants --db <dsn> : enumerate the tenants OF ONE multi-tenant
	//     store (namespace-per-tenant for sqlite/mysql/local libsql, or the
	//     tenant column under RLS for postgres) and read each tenant's lessons
	//     scoped to that tenant. This is the hosted-deployment shape (ADR 0007 +
	//     0011): one brain, many tenant partitions.
	//
	//   --tenant-dsn <dsn> ...   : an explicit federation of separate tenant
	//     stores (the offline/local shape).
	//
	// With neither, fall back to the default store as a single tenant — which,
	// by the corroboration gate, can never promote anything (it demonstrates the
	// no-leak floor rather than erroring).
	var tenants []consolidate.TenantLessons
	var dsns []string
	if opts.allTenants {
		baseDSN := opts.db
		if baseDSN == "" {
			baseDSN = resolveDSN()
		}
		scopes, err := store.EnumerateTenants(ctx, baseDSN)
		if err != nil {
			exitWithMnemosError(false, NewSystemError(err, "enumerate tenants of %q", store.RedactDSN(baseDSN)))
			return
		}
		for _, s := range scopes {
			// ADR 0012 Path A: union the tenant's operational lessons with the
			// knowledge schemas synthesized from its class-level claims. Both feed
			// the same promotion engine; the eligibility gate (0) blocks
			// individual/unknown either way. Claims were read under this tenant's
			// scope by the enumerator (namespace isolation, or an explicit
			// WHERE tenant filter for postgres) so no cross-tenant bleed occurs.
			knowledge := knowledgeSchemasFromClaims(s.Claims)
			lessons := make([]domain.Lesson, 0, len(s.Lessons)+len(knowledge))
			lessons = append(lessons, s.Lessons...)
			lessons = append(lessons, knowledge...)
			tenants = append(tenants, consolidate.TenantLessons{Tenant: s.Tenant, Lessons: lessons})
			dsns = append(dsns, s.DSN)
		}
	} else {
		dsns = opts.tenantDSNs
		if len(dsns) == 0 {
			dsns = []string{resolveDSN()}
		}
		for _, dsn := range dsns {
			conn, err := store.Open(ctx, dsn)
			if err != nil {
				exitWithMnemosError(false, NewSystemError(err, "open tenant store %q", store.RedactDSN(dsn)))
				return
			}
			lessons, err := conn.Lessons.ListAll(ctx)
			if err != nil {
				_ = conn.Close()
				exitWithMnemosError(false, NewSystemError(err, "list lessons for %q", store.RedactDSN(dsn)))
				return
			}
			// ADR 0012 Path A: also read the tenant's claims and synthesize
			// knowledge schemas from the class-level subset (individual/unknown are
			// skipped, fail-closed), unioning them with the operational lessons.
			claims, err := conn.Claims.ListAll(ctx)
			_ = conn.Close()
			if err != nil {
				exitWithMnemosError(false, NewSystemError(err, "list claims for %q", store.RedactDSN(dsn)))
				return
			}
			lessons = append(lessons, knowledgeSchemasFromClaims(claims)...)
			tenants = append(tenants, consolidate.TenantLessons{Tenant: dsn, Lessons: lessons})
		}
	}

	// Optional global-knowledge source for the contradiction gate.
	var global consolidate.GlobalKnowledge
	if opts.globalDSN != "" {
		global = &storeGlobalKnowledge{dsn: opts.globalDSN}
	}

	// Prediction-error ranking signal: the DIRECT Lesson→Expectation edge —
	// a lesson's backing actions → the decisions/claims those actions produced
	// → the Expectation on those claims → the peak surprise (see
	// newStoreSurpriseSource).
	surprise, err := newStoreSurpriseSource(ctx, dsns)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "load prediction-error signal"))
		return
	}

	// Resolve the display default so the audit output reflects the effective
	// threshold the engine will apply (the engine defaults MinTenants internally).
	if opts.engine.MinTenants <= 0 {
		opts.engine.MinTenants = consolidate.DefaultMinTenants
	}

	p := consolidate.NewPromoter(global, surprise)
	res, err := p.Promote(ctx, tenants, opts.engine)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "promote"))
		return
	}

	// Operational log (ADR 0021): promotion (tenant→global) is an important epistemic
	// event — record what crossed into the shared brain.
	newStderrLogger().Info().
		Int("tenants_scanned", len(tenants)).
		Int("promoted", len(res.Promoted)).
		Int("pending", len(res.Pending)).
		Int("dissonant", len(res.Dissonant)).
		Int("skipped", len(res.Skipped)).
		Bool("apply", opts.apply).
		Str("gate", string(opts.engine.Gate)).
		Msg("mnemos: promotion")

	out := map[string]any{
		"tenants_scanned": len(tenants),
		"min_tenants":     opts.engine.MinTenants,
		"gate":            string(opts.engine.Gate),
		"curated":         opts.curate,
		"dry_run":         !opts.apply,
		"promoted":        res.Promoted,
		"pending":         res.Pending,
		"dissonant":       res.Dissonant,
		"skipped":         res.Skipped,
	}

	// --apply persists the surviving candidates to the neocortex store.
	if opts.apply {
		written, err := applyPromotion(ctx, opts.globalDSN, res)
		if err != nil {
			exitWithMnemosError(false, err)
			return
		}
		out["applied"] = true
		out["written_active"] = written.active
		out["written_pending"] = written.pending
		out["global_dsn"] = opts.globalDSN
	}

	emitJSON(out)
}

// knowledgeSchemasFromClaims synthesizes ADR 0012 Path A knowledge schemas from
// a tenant's claims: it keeps only ACTIVE claims (contested/deprecated beliefs
// are not promotable knowledge) and hands them to consolidate.SynthesizeKnowledgeSchemas,
// which itself keeps only the class-level subset (individual/unknown are skipped,
// fail-closed). The returned schemas are transient promotion inputs whose
// Evidence holds claim ids — they are unioned with the tenant's operational
// lessons and never persisted into the lessons table.
func knowledgeSchemasFromClaims(claims []domain.Claim) []domain.Lesson {
	active := make([]domain.Claim, 0, len(claims))
	for _, c := range claims {
		if c.Status == domain.ClaimStatusActive {
			active = append(active, c)
		}
	}
	return consolidate.SynthesizeKnowledgeSchemas(active)
}

// writeCounts records how many global records a promotion apply wrote.
type writeCounts struct {
	active  int
	pending int
}

// applyPromotion persists a promotion Result to the global (neocortex) store.
// GateAuto survivors (res.Promoted) are written Active; GateOperator survivors
// (res.Pending) are written Pending. Dissonant and skipped candidates are never
// written. Only de-identified fields cross (via ToGlobalSchema).
//
// Re-promotion refreshes the evidence, never the verdict. GlobalSchemaID is
// content-addressed, so every pass upserts the SAME row for a recurring
// schema — and the repository's upsert overwrites every column, status
// included. A schema an operator had already approved (`--promote approve`,
// pending → active) therefore reverted to pending on the next
// `--promote --gate operator --apply`, silently un-doing a human decision that
// nothing in the run had re-examined. Approval is a human act; only `approve`
// grants it, and only an explicit revocation should take it away.
//
// So the corroboration/confidence/surprise figures — the things this pass
// actually recomputed — are written through, while an existing ACTIVE row keeps
// its status, its original promoted_at, and the actor recorded on it: the audit
// trail of when and by whom it entered force.
func applyPromotion(ctx context.Context, globalDSN string, res consolidate.Result) (writeCounts, error) {
	var wc writeCounts
	if globalDSN == "" {
		return wc, NewUserError("--apply requires --global-dsn (the neocortex store to write to)")
	}
	conn, err := store.Open(ctx, globalDSN)
	if err != nil {
		return wc, NewSystemError(err, "open global store %q", store.RedactDSN(globalDSN))
	}
	defer func() { _ = conn.Close() }()
	if conn.GlobalSchemas == nil {
		return wc, NewUserError("global store %q does not support promoted-schema persistence", store.RedactDSN(globalDSN))
	}

	now := time.Now().UTC()
	write := func(cands []consolidate.PromotedLesson, status domain.GlobalSchemaStatus) (int, error) {
		written := 0
		for _, c := range cands {
			gs := c.ToGlobalSchema(status, now, domain.SystemUser)
			prior, found, err := conn.GlobalSchemas.GetByID(ctx, gs.ID)
			if err != nil {
				return written, NewSystemError(err, "read existing global schema %q", gs.ID)
			}
			if found {
				gs = preserveApproval(gs, prior)
			}
			if err := conn.GlobalSchemas.Upsert(ctx, gs); err != nil {
				return written, NewSystemError(err, "persist global schema %q", gs.ID)
			}
			if gs.Status == domain.GlobalSchemaStatusActive {
				written++
			}
		}
		return written, nil
	}
	activeFromAuto, err := write(res.Promoted, domain.GlobalSchemaStatusActive)
	if err != nil {
		return wc, err
	}
	activeFromPending, err := write(res.Pending, domain.GlobalSchemaStatusPending)
	if err != nil {
		return wc, err
	}
	wc.active = activeFromAuto + activeFromPending
	wc.pending = len(res.Promoted) + len(res.Pending) - wc.active
	return wc, nil
}

// preserveApproval carries an already-granted global approval forward onto the
// refreshed record. Everything the promotion pass genuinely recomputed
// (statement, scope, polarity, corroboration counts, confidence, surprise) is
// left as the new value; only the review verdict and its provenance survive
// from the prior row.
//
// It is one-directional on purpose. An active row stays active — a re-run under
// `--gate operator` must not un-approve it. A pending row is free to become
// active, because that is the gate legitimately deciding the schema now clears
// automatically.
func preserveApproval(next, prior domain.GlobalSchema) domain.GlobalSchema {
	if prior.Status != domain.GlobalSchemaStatusActive {
		return next
	}
	next.Status = domain.GlobalSchemaStatusActive
	if !prior.PromotedAt.IsZero() {
		next.PromotedAt = prior.PromotedAt
	}
	if prior.CreatedBy != "" {
		next.CreatedBy = prior.CreatedBy
	}
	return next
}

// handlePromoteApprove activates a single pending global schema by id.
func handlePromoteApprove(ctx context.Context, opts promoteOpts) {
	if opts.globalDSN == "" {
		exitWithMnemosError(false, NewUserError("approve requires --global-dsn"))
		return
	}
	conn, err := store.Open(ctx, opts.globalDSN)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "open global store %q", store.RedactDSN(opts.globalDSN)))
		return
	}
	defer func() { _ = conn.Close() }()
	if conn.GlobalSchemas == nil {
		exitWithMnemosError(false, NewUserError("global store %q does not support promoted-schema persistence", opts.globalDSN))
		return
	}
	if err := conn.GlobalSchemas.Approve(ctx, opts.approveID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			exitWithMnemosError(false, NewUserError("no global schema with id %q", opts.approveID))
			return
		}
		exitWithMnemosError(false, NewSystemError(err, "approve global schema %q", opts.approveID))
		return
	}
	emitJSON(map[string]any{
		"approved":   opts.approveID,
		"status":     string(domain.GlobalSchemaStatusActive),
		"global_dsn": opts.globalDSN,
	})
}

type promoteOpts struct {
	engine     consolidate.Options
	tenantDSNs []string
	allTenants bool
	db         string
	globalDSN  string
	apply      bool
	approveID  string
	// curate enables the ADR 0012 curated single-source path. It requires a
	// curator token (--token / MNEMOS_TOKEN) bearing the promote:global scope.
	curate bool
	// token is the curator JWT verified when curate is set.
	token string
}

func parsePromoteOpts(args []string, _ Flags) (promoteOpts, error) {
	out := promoteOpts{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--promote":
			// consumed as the mode selector; no value.
		case "approve":
			// Sub-verb: `--promote approve <id>`. The next positional arg is
			// the id of the global schema to activate.
			if i+1 >= len(args) {
				return out, NewUserError("approve requires a global schema id")
			}
			out.approveID = args[i+1]
			i++
		case "--min-tenants":
			if i+1 >= len(args) {
				return out, NewUserError("--min-tenants requires a value")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 1 {
				return out, NewUserError("--min-tenants must be a positive integer")
			}
			out.engine.MinTenants = n
			i++
		case "--gate":
			if i+1 >= len(args) {
				return out, NewUserError("--gate requires a value (auto|operator)")
			}
			switch args[i+1] {
			case "auto":
				out.engine.Gate = consolidate.GateAuto
			case "operator":
				out.engine.Gate = consolidate.GateOperator
			default:
				return out, NewUserError("--gate must be auto or operator")
			}
			i++
		case "--tenant-dsn":
			if i+1 >= len(args) {
				return out, NewUserError("--tenant-dsn requires a value")
			}
			out.tenantDSNs = append(out.tenantDSNs, args[i+1])
			i++
		case "--all-tenants":
			out.allTenants = true
		case "--db":
			if i+1 >= len(args) {
				return out, NewUserError("--db requires a value")
			}
			out.db = args[i+1]
			i++
		case "--global-dsn":
			if i+1 >= len(args) {
				return out, NewUserError("--global-dsn requires a value")
			}
			out.globalDSN = args[i+1]
			i++
		case "--sensitive":
			if i+1 >= len(args) {
				return out, NewUserError("--sensitive requires a value")
			}
			out.engine.SensitiveTokens = append(out.engine.SensitiveTokens, args[i+1])
			i++
		case "--curate", "--contribute":
			// ADR 0012 curated single-source path. Enables promotion of a
			// class-level fact from ONE source, bypassing cross-tenant
			// corroboration. Requires a curator token (verified in handlePromote).
			out.curate = true
			out.engine.Curated = true
		case "--token":
			if i+1 >= len(args) {
				return out, NewUserError("--token requires a value")
			}
			out.token = args[i+1]
			i++
		case "--apply":
			out.apply = true
		case "--dry-run":
			// Default; accepted for symmetry and explicitness.
			out.apply = false
		default:
			return out, NewUserError("unknown flag %q", args[i])
		}
	}
	return out, nil
}

// verifyCuratorToken enforces the ADR 0012 curator authorization for the
// curated single-source promotion path. It resolves the token from --token or
// MNEMOS_TOKEN, validates its signature/expiry against the install's JWT secret,
// honours the revocation denylist of the store identified by revocationDSN, and
// requires the bearer to hold the promote:global scope (auth.Claims.CanCurate).
// It FAILS CLOSED — any missing/invalid token or missing scope is an error, so
// the curated path is unreachable without a valid curator capability.
func verifyCuratorToken(ctx context.Context, tokenStr, revocationDSN string) error {
	tokenStr = strings.TrimSpace(tokenStr)
	if tokenStr == "" {
		tokenStr = strings.TrimSpace(os.Getenv("MNEMOS_TOKEN"))
	}
	if tokenStr == "" {
		return NewUserError("curated promotion (--curate) requires a curator token bearing %q — pass --token <jwt> or set MNEMOS_TOKEN", domain.ScopePromoteGlobal)
	}

	_, projectRoot, _ := findProjectDB()
	secret, _, err := auth.LoadOrCreateSecret(auth.DefaultSecretPath(projectRoot))
	if err != nil {
		return NewSystemError(err, "load JWT secret to verify curator token")
	}

	conn, err := store.Open(ctx, revocationDSN)
	if err != nil {
		return NewSystemError(err, "open store %q to check curator-token revocation", store.RedactDSN(revocationDSN))
	}
	defer func() { _ = conn.Close() }()
	if conn.RevokedTokens == nil {
		return NewUserError("store %q cannot verify token revocation", store.RedactDSN(revocationDSN))
	}

	claims, err := auth.NewVerifier(secret, conn.RevokedTokens).ParseAndValidate(ctx, tokenStr)
	if err != nil {
		return NewUserError("curator token rejected: %v", err)
	}
	if !claims.CanCurate() {
		return NewUserError("curator token lacks the %q scope required for --curate", domain.ScopePromoteGlobal)
	}
	return nil
}

// curatorRevocationDSN picks the store whose revocation denylist authorizes the
// curator token: the global (neocortex) store when writing there, else the
// operator's base store, else the resolved default.
func (o promoteOpts) curatorRevocationDSN() string {
	switch {
	case o.globalDSN != "":
		return o.globalDSN
	case o.db != "":
		return o.db
	case len(o.tenantDSNs) > 0:
		return o.tenantDSNs[0]
	default:
		return resolveDSN()
	}
}

// storeGlobalKnowledge adapts a store DSN to the consolidate.GlobalKnowledge
// port. "Vetted" global claims are the active (in-force) claims of the global
// store; contested/deprecated claims are excluded so a stale belief does not
// mark a fresh cross-tenant candidate dissonant.
type storeGlobalKnowledge struct {
	dsn string
}

func (g *storeGlobalKnowledge) VettedClaims(ctx context.Context) ([]domain.Claim, error) {
	conn, err := store.Open(ctx, g.dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	all, err := conn.Claims.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	vetted := make([]domain.Claim, 0, len(all))
	for _, c := range all {
		if c.Status == domain.ClaimStatusActive {
			vetted = append(vetted, c)
		}
	}
	return vetted, nil
}

// directSurpriseSource is a store-backed consolidate.SurpriseSource that ranks
// promotion candidates by the DIRECT Lesson→Expectation prediction-error edge.
//
// Linkage (the real model path, replacing the earlier scope-aggregation proxy):
//
//	Schema.Evidence  → action ids
//	Outcome.ActionID → the outcomes those actions produced
//	Decision.OutcomeID + Decision.Beliefs → the decisions those outcomes drove
//	                    and the belief claim ids that justified them
//	Expectation(ClaimID) → the forward prediction on each belief claim, whose
//	                       reconciled Surprise() is the prediction-error scalar
//
// The peak (max) surprise reachable from a lesson's backing actions is its
// prediction-error weight. byActionID caches that peak per action id (the join
// key on Schema.Evidence), so SurpriseFor is an O(evidence) lookup. A lesson
// none of whose actions reach a resolved expectation returns hasData=false and
// falls back to corroboration-count ranking in the engine.
type directSurpriseSource struct {
	byActionID map[string]float64
}

func (s *directSurpriseSource) SurpriseFor(_ context.Context, lesson domain.Lesson) (float64, bool) {
	peak, has := 0.0, false
	for _, actionID := range lesson.Evidence {
		v, ok := s.byActionID[actionID]
		if !ok {
			continue
		}
		if !has || v > peak {
			peak, has = v, true
		}
	}
	return peak, has
}

// newStoreSurpriseSource builds a directSurpriseSource by resolving, across every
// tenant store, the direct action→outcome→decision→belief→expectation path and
// recording the peak reconciled surprise reachable from each action id. Stores
// lacking the necessary repositories simply contribute no signal (never an
// error), and action ids are globally unique, so keying the merged map by action
// id keeps each tenant's signal correctly attributed.
func newStoreSurpriseSource(ctx context.Context, dsns []string) (consolidate.SurpriseSource, error) {
	byAction := map[string]float64{}
	for _, dsn := range dsns {
		conn, err := store.Open(ctx, dsn)
		if err != nil {
			return nil, err
		}
		if err := accumulateActionSurprise(ctx, conn, byAction); err != nil {
			_ = conn.Close()
			return nil, err
		}
		_ = conn.Close()
	}
	return &directSurpriseSource{byActionID: byAction}, nil
}

// accumulateActionSurprise walks one tenant store's decision/outcome/expectation
// graph and records, into byAction, the peak reconciled surprise reachable from
// each action id via the direct Lesson→Expectation edge. It is defensive: a
// backend missing decisions, outcomes, or expectations contributes nothing.
func accumulateActionSurprise(ctx context.Context, conn *store.Conn, byAction map[string]float64) error {
	if conn.Decisions == nil || conn.Outcomes == nil || conn.Expectations == nil {
		return nil
	}

	// outcomeID → belief claim ids of the decision that consumed that outcome.
	decisions, err := conn.Decisions.ListAll(ctx)
	if err != nil {
		return err
	}
	outcomeToBeliefs := map[string][]string{}
	for _, d := range decisions {
		if d.OutcomeID == "" || len(d.Beliefs) == 0 {
			continue
		}
		outcomeToBeliefs[d.OutcomeID] = append(outcomeToBeliefs[d.OutcomeID], d.Beliefs...)
	}
	if len(outcomeToBeliefs) == 0 {
		return nil
	}

	// claimID → reconciled surprise (memoised; only claims that back a decision
	// are ever queried).
	claimSurprise := map[string]float64{}
	surpriseFor := func(claimID string) (float64, bool, error) {
		if v, ok := claimSurprise[claimID]; ok {
			return v, true, nil
		}
		exp, ok, err := conn.Expectations.Get(ctx, claimID)
		if err != nil {
			return 0, false, err
		}
		if !ok {
			return 0, false, nil
		}
		s, meaningful := exp.Surprise()
		if !meaningful {
			return 0, false, nil
		}
		claimSurprise[claimID] = s
		return s, true, nil
	}

	outcomes, err := conn.Outcomes.ListAll(ctx)
	if err != nil {
		return err
	}
	for _, o := range outcomes {
		beliefs, ok := outcomeToBeliefs[o.ID]
		if !ok {
			continue
		}
		peak, has := 0.0, false
		for _, claimID := range beliefs {
			s, ok, err := surpriseFor(claimID)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if !has || s > peak {
				peak, has = s, true
			}
		}
		if !has {
			continue
		}
		if cur, ok := byAction[o.ActionID]; !ok || peak > cur {
			byAction[o.ActionID] = peak
		}
	}
	return nil
}
