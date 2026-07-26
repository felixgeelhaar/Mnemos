package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"go.klarlabs.de/mnemos/internal/consolidate"
	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/store"
	"go.klarlabs.de/mnemos/internal/trust"
)

// handleFloatBack implements local float-back — the upward flow that promotes
// important learnings from a repo/workspace brain (a "sub-region") into the
// user's personal central brain. It is the local twin of the hosted
// tenant→global promotion (`consolidate --promote`); see
// docs/deployment-modes.md ("One topology at two scales").
//
//	mnemos float-back [--from <path|dsn>] [--to <central dsn>]
//	                  [--min-trust X] [--apply | --dry-run]
//
// Source (--from) is the repo/workspace brain for a path (resolved via the same
// workspace / .mnemos walk-up the hooks use), or an explicit DSN. Central (--to)
// is the personal central brain — the global default DSN (MNEMOS_DB_URL, else
// the XDG global path). Refuses when source == central.
//
// The LOCAL gate is IMPORTANCE / GENERALITY, not privacy (single owner): a claim
// floats when its trust_score ≥ --min-trust AND its statement is not
// repo-specific, OR it is explicitly tagged "remember globally"
// (`claim record --global`), which floats unconditionally. --dry-run (the
// DEFAULT) only reports the JSON plan; --apply writes the floated claims into
// the central brain (repo-specific absolute paths stripped, de-duplicated
// against what is already central so re-running never duplicates).
func handleFloatBack(args []string, _ Flags) {
	from, to := "", ""
	minTrust := consolidate.DefaultMinTrust
	apply := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--from":
			if i+1 >= len(args) {
				exitWithMnemosError(false, NewUserError("--from requires a path or dsn"))
				return
			}
			from = args[i+1]
			i++
		case "--to":
			if i+1 >= len(args) {
				exitWithMnemosError(false, NewUserError("--to requires a central dsn"))
				return
			}
			to = args[i+1]
			i++
		case "--min-trust":
			if i+1 >= len(args) {
				exitWithMnemosError(false, NewUserError("--min-trust requires a value"))
				return
			}
			v, err := strconv.ParseFloat(args[i+1], 64)
			if err != nil || v < 0 || v > 1 {
				exitWithMnemosError(false, NewUserError("--min-trust must be a number in [0, 1]"))
				return
			}
			minTrust = v
			i++
		case "--apply":
			apply = true
		case "--dry-run":
			apply = false
		default:
			exitWithMnemosError(false, NewUserError("unknown float-back flag %q", args[i]))
			return
		}
	}

	srcDSN, err := resolveFloatSource(from)
	if err != nil {
		exitWithMnemosError(false, err)
		return
	}
	centralDSN := resolveCentralDSN(to)

	if sameBrain(srcDSN, centralDSN) {
		exitWithMnemosError(false, NewUserError(
			"source and central are the same brain (%s); nothing to float up", redactDSN(centralDSN)))
		return
	}

	ctx := context.Background()

	inputs, err := loadFloatInputs(ctx, srcDSN)
	if err != nil {
		exitWithMnemosError(false, err)
		return
	}

	existing, err := loadCentralStatements(ctx, centralDSN)
	if err != nil {
		exitWithMnemosError(false, err)
		return
	}

	plan := consolidate.PlanFloatBack(inputs, minTrust, existing)

	out := map[string]any{
		"source":        redactDSN(srcDSN),
		"central":       redactDSN(centralDSN),
		"min_trust":     minTrust,
		"dry_run":       !apply,
		"floated_count": len(plan.Floated),
		"skipped_count": len(plan.Skipped),
		"floated":       plan.Floated,
		"skipped":       plan.Skipped,
	}

	if apply {
		written, err := applyFloatBack(ctx, centralDSN, plan.Floated)
		if err != nil {
			exitWithMnemosError(false, err)
			return
		}
		out["applied"] = true
		out["written"] = written
	}

	emitJSON(out)
}

// floatbackOnCaptureEnabled reports whether the opt-in session-end float-back is
// turned on via MNEMOS_FLOATBACK_ON_CAPTURE (config key floatback.on_capture).
// Default OFF: only "1"/"true"/"yes" (case-insensitive) enable it, so the
// capture hook's behavior is byte-identical to before unless explicitly opted in.
func floatbackOnCaptureEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MNEMOS_FLOATBACK_ON_CAPTURE"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// floatBackOnCapture runs a best-effort, apply-mode float-back from a
// repo/workspace brain (srcDSN) up into the personal central brain at session
// end. It reuses the exact planning/apply pipeline `mnemos float-back --apply`
// runs (loadFloatInputs → PlanFloatBack → applyFloatBack), so the same
// importance/generality gate, path-stripping, and content-addressed dedup apply
// — a re-run never duplicates.
//
// It is FAIL-OPEN by construction: every error path returns silently so it can
// never fail or hang the capture hook. Callers gate it behind
// floatbackOnCaptureEnabled() and a short-timeout context. It is a no-op when
// srcDSN resolves to the central brain (nothing to float up).
func floatBackOnCapture(ctx context.Context, srcDSN string) {
	centralDSN := resolveCentralDSN("")
	if sameBrain(srcDSN, centralDSN) {
		return
	}
	inputs, err := loadFloatInputs(ctx, srcDSN)
	if err != nil {
		return
	}
	existing, err := loadCentralStatements(ctx, centralDSN)
	if err != nil {
		return
	}
	plan := consolidate.PlanFloatBack(inputs, consolidate.DefaultMinTrust, existing)
	if len(plan.Floated) == 0 {
		return
	}
	_, _ = applyFloatBack(ctx, centralDSN, plan.Floated)
}

// resolveFloatSource resolves the --from argument to a source store DSN. An
// explicit DSN (contains "://") is used verbatim; otherwise the argument is a
// filesystem path (defaulting to CWD) whose owning workspace brain — then repo
// .mnemos brain — is looked up exactly as the hooks/MCP do.
func resolveFloatSource(arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if strings.Contains(arg, "://") {
		return arg, nil
	}
	start := arg
	if start == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", NewSystemError(err, "resolve current directory")
		}
		start = cwd
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", NewUserError("bad --from path %q: %v", start, err)
	}
	if dsn, _, _ := resolveWorkspaceBrain(abs); dsn != "" {
		return dsn, nil
	}
	if dbPath, _, ok := findProjectDBFrom(abs); ok {
		return "sqlite://" + dbPath, nil
	}
	return "", NewUserError(
		"no repo or workspace brain found for %q\n  create one with `mnemos init --project` or `mnemos workspace create`, or pass an explicit --from <dsn>", start)
}

// resolveCentralDSN resolves the --to argument to the personal central brain
// DSN. Explicit --to wins; otherwise the global default (MNEMOS_DB_URL, else the
// XDG global path) — NOT the project walk-up, since the central brain is global.
func resolveCentralDSN(arg string) string {
	if arg = strings.TrimSpace(arg); arg != "" {
		return arg
	}
	if u := os.Getenv("MNEMOS_DB_URL"); u != "" {
		return u
	}
	return "sqlite://" + globalDBPath()
}

// loadFloatInputs reads every claim from the source brain and computes each
// claim's trust score (confidence × corroboration × freshness) from its evidence
// events, producing the engine's inputs.
func loadFloatInputs(ctx context.Context, srcDSN string) ([]consolidate.FloatInput, error) {
	conn, err := store.Open(ctx, srcDSN)
	if err != nil {
		return nil, NewSystemError(err, "open source brain %q", redactDSN(srcDSN))
	}
	defer func() { _ = conn.Close() }()

	claims, err := conn.Claims.ListAll(ctx)
	if err != nil {
		return nil, NewSystemError(err, "list source claims")
	}

	// Per-claim evidence count and freshest evidence timestamp, for trust.
	eventAt := map[string]time.Time{}
	if events, err := conn.Events.ListAll(ctx); err == nil {
		for _, e := range events {
			eventAt[e.ID] = e.Timestamp
		}
	}
	type agg struct {
		count  int
		latest time.Time
	}
	byClaim := map[string]*agg{}
	if links, err := conn.Claims.ListAllEvidence(ctx); err == nil {
		for _, l := range links {
			a := byClaim[l.ClaimID]
			if a == nil {
				a = &agg{}
				byClaim[l.ClaimID] = a
			}
			a.count++
			if t, ok := eventAt[l.EventID]; ok && t.After(a.latest) {
				a.latest = t
			}
		}
	}

	now := time.Now().UTC()
	inputs := make([]consolidate.FloatInput, 0, len(claims))
	for _, c := range claims {
		count, latest := 0, time.Time{}
		if a := byClaim[c.ID]; a != nil {
			count, latest = a.count, a.latest
		}
		score := trust.ScoreWithHalfLife(c.Confidence, count, latest, now, c.HalfLifeDays)
		inputs = append(inputs, consolidate.FloatInput{
			ID:                   c.ID,
			Text:                 c.Text,
			Type:                 string(c.Type),
			Active:               c.Status == domain.ClaimStatusActive,
			Confidence:           c.Confidence,
			Trust:                score,
			ConfidenceComponents: c.ConfidenceComponents,
		})
	}
	return inputs, nil
}

// loadCentralStatements builds the dedup set: the normalized, path-stripped form
// of every claim already in the central brain, so a floated statement already
// present centrally is skipped.
func loadCentralStatements(ctx context.Context, centralDSN string) (map[string]struct{}, error) {
	conn, err := store.Open(ctx, centralDSN)
	if err != nil {
		return nil, NewSystemError(err, "open central brain %q", redactDSN(centralDSN))
	}
	defer func() { _ = conn.Close() }()

	claims, err := conn.Claims.ListAll(ctx)
	if err != nil {
		return nil, NewSystemError(err, "list central claims")
	}
	existing := make(map[string]struct{}, len(claims))
	for _, c := range claims {
		key := consolidate.NormalizeForDedup(consolidate.StripRepoSpecifics(c.Text))
		if key != "" {
			existing[key] = struct{}{}
		}
	}
	return existing, nil
}

// applyFloatBack writes the floated claims into the central brain through the
// governed writer's Artifacts batch — each claim with an anchoring evidence
// event, so the "claims require evidence" invariant holds. This is the same
// governed persistence path the ingest pipeline uses (no async embed, no delivery
// adapter touching a repo directly). Returns the number of claims written.
func applyFloatBack(ctx context.Context, centralDSN string, items []consolidate.FloatItem) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	w, err := govwrite.New(ctx, centralDSN, nil)
	if err != nil {
		return 0, NewSystemError(err, "open central brain %q", redactDSN(centralDSN))
	}
	defer closeWriter(w)

	actor := resolveCLIActor()
	now := time.Now().UTC()
	events := make([]domain.Event, 0, len(items))
	claims := make([]domain.Claim, 0, len(items))
	links := make([]domain.ClaimEvidence, 0, len(items))
	for _, it := range items {
		eventID := uuid.NewString()
		claimID := uuid.NewString()
		events = append(events, domain.Event{
			ID:            eventID,
			RunID:         "float-back",
			Content:       "floated up from a repo/workspace brain: " + it.Statement,
			SourceInputID: "float-back",
			Timestamp:     now,
			IngestedAt:    now,
			CreatedBy:     actor,
		})
		claims = append(claims, domain.Claim{
			ID:         claimID,
			Text:       it.Statement,
			Type:       domain.ClaimType(floatClaimType(it.Type)),
			Confidence: it.Confidence,
			Status:     domain.ClaimStatusActive,
			CreatedAt:  now,
			ValidFrom:  now,
			CreatedBy:  actor,
		})
		links = append(links, domain.ClaimEvidence{ClaimID: claimID, EventID: eventID})
	}
	if _, err := w.Artifacts(ctx, events, claims, links, nil); err != nil {
		return 0, NewSystemError(err, "write floated artifacts to central")
	}
	return len(claims), nil
}

// floatClaimType keeps a portable claim type as-is, but downgrades types that
// carry extra required provenance (test_result needs a TestID we do not float)
// to a plain fact, so the central write always validates.
func floatClaimType(t string) string {
	switch t {
	case string(domain.ClaimTypeFact), string(domain.ClaimTypeHypothesis), string(domain.ClaimTypeDecision):
		return t
	default:
		return string(domain.ClaimTypeFact)
	}
}

// sameBrain reports whether two DSNs point at the same store, so float-back can
// refuse a no-op (source == central). For SQLite it compares the resolved
// absolute file path; otherwise the trimmed DSN string.
func sameBrain(a, b string) bool {
	return canonBrain(a) == canonBrain(b)
}

func canonBrain(dsn string) string {
	if p, ok := sqliteFilePath(dsn); ok {
		if abs, err := filepath.Abs(p); err == nil {
			return "sqlite:" + abs
		}
		return "sqlite:" + p
	}
	return strings.TrimSpace(dsn)
}

// redactDSN returns a DSN with any password redacted, for human/plan display.
// redactDSN masks the password in a DSN before it reaches an error message.
//
// Delegates to store.RedactDSN so there is ONE implementation. There used to be
// three (this, store.RedactDSN, and displayDSN), applied ad hoc per call site —
// which is how nine error paths ended up printing live credentials while the
// success path was carefully redacted. Redaction applied by hand at each site
// is redaction that will be forgotten at the next site.
func redactDSN(dsn string) string {
	return store.RedactDSN(dsn)
}
