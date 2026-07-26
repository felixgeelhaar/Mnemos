package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/store"
)

// Doc-sync writes a repo's high-signal learnings into an agent-facing markdown
// file (AGENTS.md by default, or CLAUDE.md) so any LLM working in the repo picks
// them up natively — Claude Code and other agents auto-load these files. mnemos
// owns only a delimited block; everything the human wrote around it is
// preserved. The block is regenerated from the repo brain on `mnemos sync-docs`
// and after a repo-scoped session capture.

const (
	docBeginMarker = "<!-- mnemos:begin (generated — edits inside are re-synced into the brain) -->"
	docEndMarker   = "<!-- mnemos:end -->"
	// maxDocFacts caps the facts section so the agent file stays readable. The
	// committed doc is a curated top-by-trust projection, not the full brain —
	// `mnemos rebuild` reconstructs this projection, not deprecated claims or
	// embeddings (see docs/repo-brain-workflow.md). All decisions and open
	// questions are always included; only low-trust facts beyond this cap drop.
	maxDocFacts = 40
)

// upsertManagedBlock returns content with the mnemos-managed block (between the
// markers) inserted or replaced by block, preserving everything else. block is
// the inner content only (no markers). If no markers are present the block is
// appended; an empty file becomes just the block.
func upsertManagedBlock(existing, block string) string {
	wrapped := docBeginMarker + "\n" + strings.TrimRight(block, "\n") + "\n" + docEndMarker
	b := strings.Index(existing, docBeginMarker)
	e := strings.Index(existing, docEndMarker)
	if b >= 0 && e > b {
		return existing[:b] + wrapped + existing[e+len(docEndMarker):]
	}
	if strings.TrimSpace(existing) == "" {
		return wrapped + "\n"
	}
	return strings.TrimRight(existing, "\n") + "\n\n" + wrapped + "\n"
}

// generateRepoLearnings renders the managed-block markdown from the currently
// resolved brain: active decisions, top-trust facts, and open questions
// (hypotheses). Deprecated claims are omitted. repoRoot is used only to stamp
// the repo's tenant identity (git remote, else path) into the header so a clone
// can tie the committed doc back to its repo tenant. Returns the inner block.
func generateRepoLearnings(ctx context.Context, repoRoot string) (string, error) {
	conn, err := openConn(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	claims, err := conn.Claims.ListAll(ctx)
	if err != nil {
		return "", err
	}

	var decisions, facts, questions []domain.Claim
	for _, c := range claims {
		if c.Status == domain.ClaimStatusDeprecated {
			continue
		}
		switch c.Type {
		case domain.ClaimTypeDecision:
			decisions = append(decisions, c)
		case domain.ClaimTypeHypothesis:
			questions = append(questions, c)
		default:
			facts = append(facts, c)
		}
	}
	// Highest-trust facts first, capped.
	sort.SliceStable(facts, func(i, j int) bool { return facts[i].TrustScore > facts[j].TrustScore })
	if len(facts) > maxDocFacts {
		facts = facts[:maxDocFacts]
	}

	var b strings.Builder
	b.WriteString("## Repo learnings (mnemos)\n\n")
	b.WriteString("_Auto-maintained from this repository's mnemos brain. Follow these when working here; ")
	b.WriteString("you may edit inside the markers and mnemos will fold your changes back into the brain._\n")
	if tenant := repoTenantKey(repoRoot); tenant != "" {
		fmt.Fprintf(&b, "\n_Repo tenant: `%s`_\n", tenant)
	}

	writeSection := func(title string, cs []domain.Claim, showTrust bool) {
		if len(cs) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n### %s\n", title)
		for _, c := range cs {
			flag := ""
			if c.Status == domain.ClaimStatusContested {
				flag = " ⚠ contested"
			}
			text := strings.TrimSpace(c.Text)
			if showTrust && c.TrustScore > 0 {
				fmt.Fprintf(&b, "- %s (trust %.2f)%s\n", text, c.TrustScore, flag)
			} else {
				fmt.Fprintf(&b, "- %s%s\n", text, flag)
			}
		}
	}
	writeSection("Decisions", decisions, false)
	writeSection("Conventions & facts", facts, true)
	writeSection("Open questions", questions, false)

	if len(decisions)+len(facts)+len(questions) == 0 {
		b.WriteString("\n_(no durable learnings recorded yet)_\n")
	}
	return b.String(), nil
}

// syncRepoDocs regenerates the managed learnings block from the brain at
// brainDSN (empty = the resolved default) and writes it into <repoRoot>/fileName,
// preserving any human content. Returns the path and whether the file changed.
func syncRepoDocs(ctx context.Context, repoRoot, fileName, brainDSN string) (path string, changed bool, err error) {
	var block string
	gen := func() { block, err = generateRepoLearnings(ctx, repoRoot) }
	if brainDSN != "" {
		withBrainDSN(brainDSN, gen)
	} else {
		gen()
	}
	if err != nil {
		return "", false, err
	}

	path = filepath.Join(repoRoot, fileName)
	existing := ""
	if data, rerr := os.ReadFile(path); rerr == nil { //nolint:gosec // repo-local doc path
		existing = string(data)
	}
	updated := upsertManagedBlock(existing, block)
	if updated == existing {
		// Already current: baseline the hash to what's on disk (== block).
		writeStoredDocHash(repoRoot, fileName, blockHash(block))
		return path, false, nil
	}
	if werr := os.WriteFile(path, []byte(updated), 0o644); werr != nil { //nolint:gosec // agent-facing doc, world-readable by design
		return path, false, werr
	}
	// Baseline the stored hash AFTER the write succeeds, so the brief-time
	// sync-back can tell a later human edit apart from mnemos's own output —
	// and never treats a failed write as a phantom edit.
	writeStoredDocHash(repoRoot, fileName, blockHash(block))
	// Keep the derived index + hash sidecars out of git so only the doc is
	// committed. Best-effort.
	ensureRepoGitignore(repoRoot)
	return path, true, nil
}

// ensureRepoGitignore makes sure the repo's .gitignore excludes the derived
// repo-brain index and hash sidecars, so AGENTS.md is committed but the local,
// rebuildable .db is not. Idempotent: appends only patterns not already
// present. Best-effort — never fails a caller.
func ensureRepoGitignore(repoRoot string) {
	patterns := []string{
		".mnemos/mnemos.db",
		".mnemos/mnemos.db-shm",
		".mnemos/mnemos.db-wal",
		".mnemos/.*.sha",
	}
	path := filepath.Join(repoRoot, ".gitignore")
	existing := ""
	if data, err := os.ReadFile(path); err == nil { //nolint:gosec // repo-local
		existing = string(data)
	}
	have := make(map[string]bool)
	for line := range strings.SplitSeq(existing, "\n") {
		have[strings.TrimSpace(line)] = true
	}
	var add []string
	for _, p := range patterns {
		if !have[p] {
			add = append(add, p)
		}
	}
	if len(add) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString(existing)
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		b.WriteByte('\n')
	}
	if existing != "" {
		b.WriteByte('\n')
	}
	b.WriteString("# mnemos: derived repo-brain index (rebuild with `mnemos rebuild`)\n")
	for _, p := range add {
		b.WriteString(p + "\n")
	}
	_ = os.WriteFile(path, []byte(b.String()), 0o644) //nolint:gosec // repo-local ignore file
}

// handleRebuild implements `mnemos rebuild` — reconstruct the gitignored repo
// brain index (<repo>/.mnemos/mnemos.db) from the committed agent doc's managed
// block after a fresh clone. Reads AGENTS.md (or --claude / --file) at the repo
// root, ingests its learnings into a freshly-created repo brain, and baselines
// the sync-back hash.
func handleRebuild(args []string, f Flags) {
	fileName := "AGENTS.md"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--claude":
			fileName = "CLAUDE.md"
		case "--agents":
			fileName = "AGENTS.md"
		case "--file":
			if i+1 >= len(args) {
				exitWithMnemosError(f.Verbose, NewUserError("--file requires a filename"))
				return
			}
			fileName = args[i+1]
			i++
		default:
			exitWithMnemosError(f.Verbose, NewUserError("unknown rebuild flag %q", args[i]))
			return
		}
	}

	cwd, _ := os.Getwd()
	repoRoot := gitTopLevel(cwd)
	if repoRoot == "" {
		repoRoot = cwd
	}
	docPath := filepath.Join(repoRoot, fileName)
	data, err := os.ReadFile(docPath) //nolint:gosec // repo-local doc
	if err != nil {
		exitWithMnemosError(f.Verbose, NewUserError("no %s at repo root %s — nothing to rebuild from", fileName, repoRoot))
		return
	}
	inner, ok := extractManagedBlock(string(data))
	if !ok {
		exitWithMnemosError(f.Verbose, NewUserError("%s has no mnemos managed block — nothing to rebuild from", fileName))
		return
	}
	text := blockBulletsText(inner)
	if strings.TrimSpace(text) == "" {
		exitWithMnemosError(f.Verbose, NewUserError("the mnemos block in %s has no learnings to rebuild from", fileName))
		return
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".mnemos"), 0o750); err != nil {
		exitWithMnemosError(f.Verbose, NewSystemError(err, "create .mnemos dir"))
		return
	}
	brainDSN := "sqlite://" + filepath.Join(repoRoot, ".mnemos", "mnemos.db")

	ctx := context.Background()
	withBrainDSN(brainDSN, func() {
		_, _ = mcpRunProcessText(ctx, "rebuild:"+fileName, mcpProcessTextInput{
			Text:          text,
			UseLLM:        f.LLM,
			UseEmbeddings: f.Embed,
		})
	})
	// Baseline the sync-back hash to the committed block so the next brief
	// doesn't treat it as a fresh human edit and re-ingest.
	writeStoredDocHash(repoRoot, fileName, blockHash(inner))
	ensureRepoGitignore(repoRoot)

	lines := strings.Count(strings.TrimSpace(text), "\n") + 1
	fmt.Printf("Rebuilt repo brain %s from %s (%d learning line(s)).\n", store.RedactDSN(brainDSN), docPath, lines)
}

// ---- sync-back: fold human edits of the managed block into the brain ----

var trustAnnotationRE = regexp.MustCompile(`\s*\(trust [0-9.]+\)`)

// extractManagedBlock returns the inner text between the markers (trimmed) and
// whether a block was present.
func extractManagedBlock(content string) (string, bool) {
	b := strings.Index(content, docBeginMarker)
	e := strings.Index(content, docEndMarker)
	if b < 0 || e <= b {
		return "", false
	}
	return strings.TrimSpace(content[b+len(docBeginMarker) : e]), true
}

func blockHash(inner string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(inner)))
	return hex.EncodeToString(sum[:])
}

// docHashPath is the sidecar that records the hash of the last mnemos-generated
// block for a given doc. It lives under .mnemos/ (local, gitignored state).
func docHashPath(repoRoot, fileName string) string {
	return filepath.Join(repoRoot, ".mnemos", "."+fileName+".sha")
}

func readStoredDocHash(repoRoot, fileName string) string {
	data, err := os.ReadFile(docHashPath(repoRoot, fileName)) //nolint:gosec // repo-local state file
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func writeStoredDocHash(repoRoot, fileName, hash string) {
	_ = os.WriteFile(docHashPath(repoRoot, fileName), []byte(hash+"\n"), 0o644) //nolint:gosec // local state
}

// blockBulletsText pulls the bullet lines out of the managed block and strips
// mnemos's own annotations (trust scores, contested flag), yielding clean text
// to re-ingest. Section headers and boilerplate are skipped, so only the actual
// facts/decisions — including whatever a human added — flow back into the brain.
func blockBulletsText(inner string) string {
	var lines []string
	for raw := range strings.SplitSeq(inner, "\n") {
		ln := strings.TrimSpace(raw)
		if !strings.HasPrefix(ln, "- ") {
			continue
		}
		s := strings.TrimSpace(strings.TrimPrefix(ln, "- "))
		s = trustAnnotationRE.ReplaceAllString(s, "")
		s = strings.ReplaceAll(s, "⚠ contested", "")
		s = strings.TrimSpace(s)
		if s != "" {
			lines = append(lines, s)
		}
	}
	return strings.Join(lines, "\n")
}

// syncBackFromDocs detects a human edit to the managed block of <repoRoot>/
// fileName (its hash differs from the last mnemos-generated one) and, if so,
// ingests the block's bullet lines into the brain at brainDSN, then re-baselines
// the hash so it converges (no re-ingest next time). useLLM selects extraction
// quality — callers on the latency-sensitive session-start path pass false
// (fast, rule-based). It NEVER rewrites the doc, so the human's text is left in
// place (no data loss). Best-effort / fail-open.
func syncBackFromDocs(ctx context.Context, repoRoot, fileName, brainDSN string, useLLM bool) {
	data, err := os.ReadFile(filepath.Join(repoRoot, fileName)) //nolint:gosec // repo-local doc
	if err != nil {
		return
	}
	inner, ok := extractManagedBlock(string(data))
	if !ok {
		return
	}
	cur := blockHash(inner)
	stored := readStoredDocHash(repoRoot, fileName)
	if stored == "" {
		// No baseline yet (e.g. a fresh clone): adopt current without ingesting,
		// so mnemos's own generated content isn't mistaken for a human edit.
		writeStoredDocHash(repoRoot, fileName, cur)
		return
	}
	if cur == stored {
		return // unchanged since mnemos last wrote it
	}

	text := blockBulletsText(inner)
	if strings.TrimSpace(text) == "" {
		writeStoredDocHash(repoRoot, fileName, cur)
		return
	}
	ingest := func() {
		// Dedup in the pipeline collapses lines already known, so only the
		// human's additions become new claims.
		_, _ = mcpRunProcessText(ctx, "human-edit:"+fileName, mcpProcessTextInput{
			Text:          text,
			UseLLM:        useLLM,
			UseEmbeddings: useLLM,
		})
	}
	if brainDSN != "" {
		withBrainDSN(brainDSN, ingest)
	} else {
		ingest()
	}
	// Re-baseline the stored hash to the human-edited content and LEAVE THE
	// HUMAN'S TEXT IN PLACE. We deliberately never regenerate the block from
	// sync-back: if extraction didn't turn an edit into a claim, regenerating
	// would wipe the human's note. The block is rewritten only by an explicit
	// `mnemos sync-docs` (which absorbs edits first). Re-baselining prevents
	// re-ingesting the same edit next time.
	writeStoredDocHash(repoRoot, fileName, cur)
}

// repoTenantKey returns a stable identity for the repo containing dir: the git
// remote URL (origin, else the first remote) when present — portable across
// clones and teammates — falling back to the repo root path when there is no
// remote. Empty when dir isn't inside a git repo. This is the federation key for
// repo-scoped knowledge (ADR 0007 tenant model); physical placement is separate.
func repoTenantKey(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	if remote := gitRemoteURL(dir); remote != "" {
		return remote
	}
	if root := gitTopLevel(dir); root != "" {
		return "path:" + root
	}
	return ""
}

// deriveHostedTenant maps a repo identity (repoTenantKey — git remote or path)
// to a stable, charset-safe tenant id for a hosted central brain (ADR 0009): a
// `repo_<slug>_<sha6>` that matches the `tnt`-claim charset and is identical
// across clones of the same remote, so teammates share the repo tenant. Empty
// for an empty key.
func deriveHostedTenant(repoKey string) string {
	repoKey = strings.TrimSpace(repoKey)
	if repoKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(repoKey))
	h := hex.EncodeToString(sum[:6]) // 12 hex chars — ample against collisions
	var b strings.Builder
	for _, r := range strings.ToLower(repoKey) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	slug := strings.Trim(b.String(), "_")
	const maxSlug = 40
	if len(slug) > maxSlug {
		slug = slug[:maxSlug]
	}
	if slug == "" {
		slug = "x"
	}
	return "repo_" + slug + "_" + h
}

// handleRepoTenant implements `mnemos repo-tenant` — print the current repo's
// identity and its derived hosted tenant id (ADR 0009), so an operator can mint
// a token for the repo's brain on a hosted central server.
func handleRepoTenant(args []string, f Flags) {
	for _, a := range args {
		exitWithMnemosError(f.Verbose, NewUserError("repo-tenant takes no flags (got %q)", a))
		return
	}
	cwd, _ := os.Getwd()
	key := repoTenantKey(cwd)
	if key == "" {
		exitWithMnemosError(f.Verbose, NewUserError("not inside a git repo (no remote or toplevel) — no repo tenant"))
		return
	}
	tenant := deriveHostedTenant(key)
	if f.JSON {
		emitJSON(map[string]string{"repo_key": key, "tenant": tenant})
		return
	}
	fmt.Printf("repo key: %s\ntenant:   %s\n\nMint a token for this repo's hosted brain:\n  mnemos token issue --user <id> --tenant %s\n", key, tenant, tenant)
}

func gitRemoteURL(dir string) string {
	if url := gitOutput(dir, "remote", "get-url", "origin"); url != "" {
		return url
	}
	// No origin: fall back to the first configured remote, if any.
	remotes := gitOutput(dir, "remote")
	if remotes == "" {
		return ""
	}
	first := strings.SplitN(strings.TrimSpace(remotes), "\n", 2)[0]
	return gitOutput(dir, "remote", "get-url", strings.TrimSpace(first))
}

func gitTopLevel(dir string) string {
	return gitOutput(dir, "rev-parse", "--show-toplevel")
}

// gitOutput runs git in dir and returns trimmed stdout, or "" on any error.
func gitOutput(dir string, args ...string) string {
	cmd := exec.Command("git", args...) //nolint:gosec // fixed binary, fixed subcommands
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// handleSyncDocs implements `mnemos sync-docs` — write the repo's learnings into
// an agent-facing markdown file (AGENTS.md by default; --claude for CLAUDE.md,
// or --file <name>). Resolves the repo brain from the CWD so it works inside a
// repo regardless of a pinned global DSN.
func handleSyncDocs(args []string, f Flags) {
	fileName := "AGENTS.md"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--claude":
			fileName = "CLAUDE.md"
		case "--agents":
			fileName = "AGENTS.md"
		case "--file":
			if i+1 >= len(args) {
				exitWithMnemosError(f.Verbose, NewUserError("--file requires a filename"))
				return
			}
			fileName = args[i+1]
			i++
		default:
			exitWithMnemosError(f.Verbose, NewUserError("unknown sync-docs flag %q", args[i]))
			return
		}
	}

	// Prefer the repo brain (repo root from CWD); fall back to the resolved
	// default when not inside a repo brain.
	dbPath, repoRoot, ok := findProjectDB()
	if !ok {
		exitWithMnemosError(f.Verbose, NewUserError(
			"not inside a repo brain (no .mnemos/ found walking up) — run `mnemos init --project` here first"))
		return
	}
	brainDSN := "sqlite://" + dbPath

	// Absorb any human edits of the managed block into the brain BEFORE
	// regenerating, so an edit that extracts into a claim survives the rewrite.
	// (Regeneration replaces the block — this is the explicit, user-initiated
	// refresh; durable prose belongs OUTSIDE the markers.)
	ctx := context.Background()
	syncBackFromDocs(ctx, repoRoot, fileName, brainDSN, f.LLM)

	path, changed, err := syncRepoDocs(ctx, repoRoot, fileName, brainDSN)
	if err != nil {
		exitWithMnemosError(f.Verbose, NewSystemError(err, "sync repo docs"))
		return
	}
	if changed {
		fmt.Printf("Updated %s with the repo's learnings (mnemos-managed block).\n", path)
	} else {
		fmt.Printf("%s already up to date.\n", path)
	}
}
