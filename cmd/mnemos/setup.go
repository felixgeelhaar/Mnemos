package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.klarlabs.de/mnemos/internal/store"
)

// handleSetup implements `mnemos setup [claude-code]` — the one-command path
// from a fresh binary to a working "central brain" wired into Claude Code.
//
// It does three things so the user doesn't hand-edit any JSON:
//  1. ensures the knowledge-base directory exists (global by default,
//     ./.mnemos with --project),
//  2. registers the `mnemos mcp` stdio server with Claude Code (user scope by
//     default, so every project shares one brain), and
//  3. runs the doctor probes and prints exactly what to do next.
//
// Everything is idempotent: re-running reports "already registered" rather
// than erroring, and --force replaces an existing registration.
func handleSetup(args []string, f Flags) {
	opts, err := parseSetupArgs(args, f.DB)
	if err != nil {
		exitWithMnemosError(false, err)
		return
	}
	// --force and --dry-run are global flags stripped by ParseFlags, so they
	// arrive via f rather than args. Fold them into the local options.
	opts.force = opts.force || f.Force
	opts.print = opts.print || f.DryRun

	switch opts.target {
	case "claude-code":
		runSetupClaudeCode(opts)
	default:
		exitWithMnemosError(false, NewUserError(
			"unsupported setup target %q (supported: claude-code)", opts.target))
	}
}

type setupOpts struct {
	target  string // integration to wire up (default "claude-code")
	project bool   // scope the brain to ./.mnemos instead of the global default
	force   bool   // replace an existing MCP registration
	print   bool   // show the plan without applying it
	dsn     string // explicit DB DSN override
}

// parseSetupArgs parses setup's own flags. The brain DSN arrives via globalDSN
// (the global --db flag, parsed in ParseFlags) rather than a local --db case.
func parseSetupArgs(args []string, globalDSN string) (setupOpts, error) {
	opts := setupOpts{target: "claude-code", dsn: globalDSN}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--project":
			opts.project = true
		case "--print":
			opts.print = true
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, NewUserError("unknown setup flag %q", arg)
			}
			opts.target = arg
		}
	}
	return opts, nil
}

func runSetupClaudeCode(opts setupOpts) {
	scope := "user"
	dsn := opts.dsn
	if dsn == "" {
		if opts.project {
			dsn = "sqlite://" + projectDBPath()
		} else {
			dsn = "sqlite://" + globalDBPath()
		}
	}
	if opts.project {
		scope = "project"
	}

	fmt.Println("Setting up Mnemos as a central brain for Claude Code")
	fmt.Printf("  scope:  %s (%s)\n", scope, ternary(opts.project, "this project", "all projects"))
	fmt.Printf("  brain:  %s\n\n", store.RedactDSN(dsn))

	// 1. Make sure the storage directory exists so the first write succeeds.
	if !opts.print {
		if err := ensureDBDir(dsn); err != nil {
			exitWithMnemosError(false, err)
			return
		}
	}

	// 2. Register the MCP server with Claude Code. setup targets the local
	// SQLite brain, so the DSN is safe to inline.
	bin := selfPath()
	if err := registerClaudeCode(bin, dsn, scope, dsnInlineSafe(dsn), opts.force, opts.print); err != nil {
		exitWithMnemosError(false, err)
		return
	}

	if opts.print {
		return
	}

	// 3. Verify and tell the user what's next.
	fmt.Println("\nVerifying...")
	report := runDoctorChecks(context.Background())
	printDoctorHuman(report)

	printSetupNextSteps(scope)
}

// registerClaudeCode wires `mnemos mcp` into Claude Code via the `claude` CLI.
// When the CLI isn't on PATH it falls back to printing the exact command and a
// project-scoped .mcp.json the user can drop in.
//
// inlineDSN controls whether the brain DSN is pinned into Claude's config via
// `-e MNEMOS_DB_URL=…`. It is true for local SQLite (no secret, keeps the brain
// deterministic regardless of cwd) and false for DSNs with credentials, which
// live in the 0600 config file the server discovers instead.
func registerClaudeCode(bin, dsn, scope string, inlineDSN, force, printOnly bool) error {
	claude, lookErr := exec.LookPath("claude")

	// The server name must precede -e: Claude's --env flag is variadic and
	// would otherwise swallow the name as a second KEY=value.
	addArgs := []string{"mcp", "add", "--scope", scope, "--transport", "stdio", "mnemos"}
	if inlineDSN {
		addArgs = append(addArgs, "-e", "MNEMOS_DB_URL="+dsn)
	}
	addArgs = append(addArgs, "--", bin, "mcp")

	if printOnly || lookErr != nil {
		fmt.Println("Register the MCP server by running:")
		fmt.Printf("  claude %s\n", strings.Join(addArgs, " "))
		if lookErr != nil {
			fmt.Println("\n(the `claude` CLI was not found on your PATH; run the command above once it is available,")
			fmt.Println(" or add the JSON below to a .mcp.json at your project root)")
			fmt.Println(mcpJSONSnippet(bin, dsn, inlineDSN))
		}
		return nil
	}

	// Idempotency: if already registered, treat as success unless --force.
	if mcpServerExists(claude) {
		if !force {
			fmt.Println("✓ mnemos MCP server already registered (re-run with --force to replace)")
			return nil
		}
		fmt.Println("· removing existing mnemos registration (--force)")
		_ = runClaude(claude, "mcp", "remove", "--scope", scope, "mnemos")
	}

	if out, err := runClaudeOutput(claude, addArgs...); err != nil {
		return NewSystemError(err, "failed to register MCP server with Claude Code:\n%s", strings.TrimSpace(out))
	}
	fmt.Println("✓ registered mnemos MCP server with Claude Code")
	return nil
}

// tokenHeaderPlaceholder is the Authorization header value written into Claude
// Code's MCP registration when the hosted endpoint needs a bearer token.
//
// The literal JWT is deliberately NOT used. It used to be interpolated into
// the `claude mcp add --header …` argv, which puts a live credential in the
// process table for any local user to read with `ps`, echoed the same string
// to stdout on the print / claude-not-found path (shell history, CI logs), and
// then left it at rest in Claude's own config — contradicting the documented
// invariant that the hosted token lives only in the 0600 mnemos config. The
// placeholder keeps the registration shape identical while the secret stays in
// that one file; Claude Code expands ${MNEMOS_TOKEN} from the environment when
// it connects, and `mnemos`'s own config loader already exports exactly that
// variable from the `server.token` key.
const tokenHeaderPlaceholder = "Authorization: Bearer ${MNEMOS_TOKEN}"

// claudeHTTPAddArgs builds the `claude mcp add` argv for a remote endpoint.
// Split out from registerClaudeCodeHTTP so a test can assert the credential
// never reaches the process table: the token argument decides *whether* an
// Authorization header is registered, never *what* is in it.
func claudeHTTPAddArgs(url, token, scope string) []string {
	args := []string{"mcp", "add", "--scope", scope, "--transport", "http", "mnemos", url}
	if token != "" {
		args = append(args, "--header", tokenHeaderPlaceholder)
	}
	return args
}

// registerClaudeCodeHTTP registers a REMOTE Mnemos MCP server (HTTP transport)
// in Claude Code, for a hosted `mnemos mcp --http` endpoint. When the endpoint
// needs a bearer token the registration carries a ${MNEMOS_TOKEN} placeholder
// header rather than the credential itself — see tokenHeaderPlaceholder.
func registerClaudeCodeHTTP(url, token, scope string, force, printOnly bool) error {
	claude, lookErr := exec.LookPath("claude")

	addArgs := claudeHTTPAddArgs(url, token, scope)

	if printOnly || lookErr != nil {
		fmt.Println("Register the remote MCP server by running:")
		fmt.Printf("  claude %s\n", strings.Join(addArgs, " "))
		if token != "" {
			printTokenEnvNote()
		}
		return nil
	}

	if mcpServerExists(claude) {
		if !force {
			fmt.Println("✓ mnemos MCP server already registered (re-run with --force to replace)")
			return nil
		}
		fmt.Println("· removing existing mnemos registration (--force)")
		_ = runClaude(claude, "mcp", "remove", "--scope", scope, "mnemos")
	}

	if out, err := runClaudeOutput(claude, addArgs...); err != nil {
		return NewSystemError(err, "failed to register remote MCP server:\n%s", strings.TrimSpace(out))
	}
	fmt.Println("✓ registered remote mnemos MCP server with Claude Code")
	if token != "" {
		printTokenEnvNote()
	}
	return nil
}

// printTokenEnvNote explains how the placeholder header resolves. Printed
// instead of the token, never alongside it.
func printTokenEnvNote() {
	fmt.Println()
	fmt.Println("  The registration references ${MNEMOS_TOKEN} instead of embedding your token,")
	fmt.Println("  so the credential is never placed on a command line or in Claude's config.")
	fmt.Println("  Export it in the shell you launch Claude Code from:")
	fmt.Println("      export MNEMOS_TOKEN=<your token>")
	fmt.Println("  The token itself is stored under `server.token` in the 0600 mnemos config")
	fmt.Println("  written above; the recall/brief/capture hooks read it from there already.")
}

// mcpServerExists reports whether Claude Code already knows a server named
// "mnemos" (any scope). `claude mcp get` exits non-zero when it doesn't.
func mcpServerExists(claude string) bool {
	return runClaude(claude, "mcp", "get", "mnemos") == nil
}

func runClaude(claude string, args ...string) error {
	_, err := runClaudeOutput(claude, args...)
	return err
}

func runClaudeOutput(claude string, args ...string) (string, error) {
	cmd := exec.Command(claude, args...) //nolint:gosec // fixed binary, controlled args
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mcpJSONSnippet(bin, dsn string, inlineDSN bool) string {
	env := ""
	if inlineDSN {
		env = fmt.Sprintf("\n      \"env\": { \"MNEMOS_DB_URL\": %q },", dsn) // dsn-redaction-ok: writes the real DSN into Claude's MCP config; inlineDSN is only set for credential-free DSNs (networked ones go to the 0600 config file instead)
	}
	return fmt.Sprintf(`
{
  "mcpServers": {
    "mnemos": {
      "type": "stdio",
      "command": %q,%s
      "args": ["mcp"]
    }
  }
}`, bin, env)
}

func printSetupNextSteps(scope string) {
	fmt.Println("\nNext steps:")
	fmt.Println("  1. Restart Claude Code (stdio servers reconnect on a fresh session).")
	if scope == "project" {
		fmt.Println("     Project-scoped servers prompt for workspace trust on first launch.")
	}
	fmt.Println("  2. Run  claude mcp list  or the  /mcp  slash command to confirm mnemos is connected.")
	fmt.Println("  3. In a session, ask Claude to use the mnemos tools, e.g.")
	fmt.Println("       \"remember that we chose Postgres for the billing service\"  (process_text / record_decision)")
	fmt.Println("       \"what do we know about the billing service?\"                (query_knowledge)")
	fmt.Println()
	fmt.Println("Tip: to make Claude record and recall automatically, add a line to your CLAUDE.md, e.g.")
	fmt.Println("  \"Use the mnemos MCP tools as long-term memory: record decisions/facts and query before answering.\"")
	fmt.Println()
	fmt.Println("Mnemos runs zero-config with rule-based extraction. For LLM-powered extraction and")
	fmt.Println("semantic search, add ~/.config/mnemos/config.yaml (see mnemos.example.yaml).")
}

// ensureDBDir creates the parent directory of a sqlite:// DSN so the first
// write doesn't fail on a missing path. Non-sqlite DSNs need no local dir.
func ensureDBDir(dsn string) error {
	path, ok := sqliteFilePath(dsn)
	if !ok {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return NewSystemError(err, "failed to create brain directory %s", dir)
	}
	return nil
}

// dsnInlineSafe reports whether a DSN can be embedded directly into Claude
// Code's config files (MCP `-e` / hook `--db`) without leaking a secret. Local
// SQLite and in-memory DSNs carry no credentials; networked backends
// (postgres/mysql/libsql) do, so those are stored in the 0600 config file
// instead and discovered by the server/hooks at runtime.
func dsnInlineSafe(dsn string) bool {
	if _, ok := sqliteFilePath(dsn); ok {
		return true
	}
	return strings.HasPrefix(dsn, "memory://")
}

// sqliteFilePath extracts the on-disk path from a sqlite DSN, returning
// (path, true) only for file-backed sqlite schemes.
func sqliteFilePath(dsn string) (string, bool) {
	for _, scheme := range []string{"sqlite://", "sqlite3://", "file://"} {
		if rest, ok := strings.CutPrefix(dsn, scheme); ok {
			// Strip any ?query parameters (e.g. ?_journal=WAL).
			if q := strings.IndexByte(rest, '?'); q >= 0 {
				rest = rest[:q]
			}
			if rest == "" || rest == ":memory:" {
				return "", false
			}
			return rest, true
		}
	}
	return "", false
}

// projectDBPath returns the absolute ./.mnemos/mnemos.db for the current
// working directory. Absolute so the registered MCP server resolves the same
// brain regardless of the directory Claude Code launches it from.
func projectDBPath() string {
	cwd, err := os.Getwd()
	if err != nil {
		return filepath.Join(".mnemos", "mnemos.db")
	}
	return filepath.Join(cwd, ".mnemos", "mnemos.db")
}

// selfPath returns the absolute path to this binary so the registration keeps
// working even when Claude Code launches with a different PATH. Falls back to
// the bare name (PATH lookup) if the executable path can't be determined.
//
// It deliberately does NOT resolve symlinks: os.Executable() already returns an
// absolute path (the invocation path, e.g. the stable /opt/homebrew/bin/mnemos
// symlink for a Homebrew install). Following that symlink would pin the
// registration to a versioned Cellar path (…/Cellar/mnemos/0.81.0/bin/mnemos)
// that `brew upgrade` deletes, silently breaking the MCP server and every hook.
// The stable symlink survives upgrades, so we keep it. Only fall back to
// symlink resolution when os.Executable() unexpectedly yields a relative path.
func selfPath() string {
	exe, err := os.Executable()
	return resolveBinPath(exe, err)
}

// resolveBinPath is the pure core of selfPath, split out so the symlink policy
// can be tested without depending on the test binary's own location.
func resolveBinPath(exe string, err error) string {
	if err != nil || exe == "" {
		return "mnemos"
	}
	if filepath.IsAbs(exe) {
		return exe
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}
