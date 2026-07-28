package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Incremental capture (ADR-adjacent to the SessionEnd capture in hook.go).
//
// One 20KiB capture at SessionEnd is too much work for a slow local model: the
// whole transcript in one extraction request overruns the per-request LLM
// timeout, so the run degrades to rule-based. Individual exchanges, by
// contrast, are tiny — median ~136 chars in a real transcript — and extract in
// seconds.
//
// So capture continuously instead of all at once: the Stop hook fires after
// each assistant response and PreCompact fires before context is summarised
// away, each ingesting only the transcript lines written since the last run.
// SessionEnd then sweeps whatever remains, which is usually nothing.
//
// These run inside the user's editor loop, so the work is detached: the hook
// process spawns a worker and exits immediately, and the user never waits on
// extraction. Everything here is fail-open — a missing offset, an unreadable
// transcript, or a dead worker costs at most one chunk of knowledge, never the
// session.

// captureStateDir holds one offset file per session, under the same XDG data
// dir as the global brain so state travels with the installation.
func captureStateDir() string {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join("data", "capture-state")
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "mnemos", "capture-state")
}

// captureStateFile maps a session id to its offset file. The id comes from the
// hook payload, so it is hashed rather than trusted as a path component: a
// session id containing separators must not write outside the state dir.
func captureStateFile(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(captureStateDir(), hex.EncodeToString(sum[:16])+".offset")
}

// loadCaptureOffset returns how far into the transcript this session has
// already been captured. Unknown sessions and unreadable state start at 0,
// which re-captures rather than silently skipping.
func loadCaptureOffset(sessionID string) int64 {
	if strings.TrimSpace(sessionID) == "" {
		return 0
	}
	data, err := os.ReadFile(captureStateFile(sessionID)) //nolint:gosec // path is hash-derived
	if err != nil {
		return 0
	}
	off, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || off < 0 {
		return 0
	}
	return off
}

// storeCaptureOffset records the new high-water mark. Written only after the
// pipeline has persisted the chunk, so a failure re-captures rather than
// dropping knowledge.
func storeCaptureOffset(sessionID string, off int64) error {
	if strings.TrimSpace(sessionID) == "" {
		return nil
	}
	if err := os.MkdirAll(captureStateDir(), 0o750); err != nil {
		return err
	}
	return os.WriteFile(captureStateFile(sessionID), []byte(strconv.FormatInt(off, 10)), 0o600)
}

// extractTranscriptTextFrom pulls conversational text written at or after
// offset, returning the text and the new end-of-file offset to record.
//
// An offset past EOF means the transcript was truncated or replaced, so it
// restarts from 0: re-capturing is idempotent enough (content-addressed
// downstream) and is far better than going permanently blind on a session.
func extractTranscriptTextFrom(path string, offset int64, maxBytes int) (string, int64) {
	f, err := os.Open(path) //nolint:gosec // path supplied by Claude Code hook payload
	if err != nil {
		return "", offset
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return "", offset
	}
	size := info.Size()
	if offset > size {
		offset = 0
	}
	if offset == size {
		return "", offset
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", offset
	}

	data, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		return "", offset
	}
	// A partial trailing line (the writer may be mid-append) is left for the
	// next run: rewind to the last newline so nothing is lost or half-parsed.
	if idx := strings.LastIndexByte(string(data), '\n'); idx >= 0 {
		data = data[:idx+1]
	}

	// Advance by what was actually consumed, not by the size of the window we
	// read. The window is 4 MiB; maxBytes is 8–20 KiB, so the extractor
	// routinely stops long before the window ends. Advancing by the window
	// would mark the unread remainder as captured and skip it permanently.
	text, consumed := transcriptTextFromLines(string(data), maxBytes)
	lastSpan.set(string(data[:min(consumed, len(data))]))
	return text, offset + int64(consumed)
}

// lastSpan carries the raw JSONL that the most recent extract actually
// CONSUMED, so the caller can derive tool actions from exactly the same bytes
// it is about to advance the offset past.
//
// The consumed prefix, not the whole window, is the load-bearing detail. The
// window is 4 MiB while the extractor stops at 8–20 KiB, so deriving actions
// from everything read would re-derive the unconsumed tail on every subsequent
// pass and record the same operation over and over — inflating exactly the
// success rates that playbook reinforcement is computed from.
//
// A package-level handoff rather than a changed signature: extractTranscriptTextFrom
// has several call sites (including tests) that want the text and nothing else,
// and widening the return for one consumer would churn all of them.
var lastSpan spanBuffer

type spanBuffer struct{ raw string }

func (s *spanBuffer) set(raw string) { s.raw = raw }

// take returns the buffered span and clears it, so a caller can never derive
// actions twice from one read.
func (s *spanBuffer) take() string {
	raw := s.raw
	s.raw = ""
	return raw
}

// spawnDetachedCapture re-execs this binary as a background worker for the
// given hook sub-command and hands it the event JSON on stdin, then returns
// without waiting. The user's editor loop never blocks on extraction.
func spawnDetachedCapture(sub string, payload []byte) error {
	// This site cannot route through spawnWorker: it hands the payload over as a
	// real file descriptor (see below), which the shared helper has no way to
	// express. The guard is therefore repeated here rather than inherited —
	// re-execing a test binary forks the whole suite (see selfexec.go).
	if underTest() {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, workerArgs([]string{"hook", sub, "--worker"})...) //nolint:gosec // self-exec with fixed args

	// Hand the payload over as a real file descriptor, not an io.Reader.
	// os/exec services a non-*os.File Stdin with a copier goroutine in THIS
	// process — and the hook calls os.Exit as soon as Start returns, killing
	// that goroutine before it writes. The child would then read empty stdin,
	// decode a zero event, and silently do nothing. A *os.File is passed
	// straight to the child as fd 0, with no copier involved.
	tmp, err := os.CreateTemp("", "mnemos-hook-*.json")
	if err != nil {
		return err
	}
	// Unlink now: on Unix the child keeps the open descriptor, so the payload
	// survives while leaving nothing behind if we die before it runs.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		_ = tmp.Close()
		return err
	}
	defer func() { _ = tmp.Close() }()
	cmd.Stdin = tmp
	// Detach so the worker outlives this process (platform-specific).
	cmd.SysProcAttr = detachSysProcAttr()
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return err
	}
	// Release so the child is not left a zombie when this process exits.
	return cmd.Process.Release()
}

// hookCaptureIncremental handles Stop and PreCompact. Unless it is the worker,
// it spawns one and returns immediately.
func hookCaptureIncremental(ev hookEvent, isWorker bool, raw []byte) {
	if strings.TrimSpace(ev.TranscriptPath) == "" {
		return
	}
	// Honor the strategy at runtime, not just at install time: changing
	// capture.strategy should take effect without re-running `mnemos init`,
	// and the hooks may already be installed from an earlier choice.
	if captureStrategy() != captureIncremental {
		return
	}
	if !isWorker {
		// Best-effort: if spawning fails there is simply no incremental capture
		// this turn, and SessionEnd still sweeps the remainder.
		_ = spawnDetachedCapture("capture-incremental", raw)
		return
	}
	captureRange(ev, incrementalChunkBytes)
}

// incrementalChunkBytes caps one incremental chunk. Exchanges are small
// (median ~136 chars), so this bounds a burst — a long tool-heavy turn, or the
// backlog PreCompact sees — rather than a typical turn.
const incrementalChunkBytes = 8 * 1024

// captureRange captures the not-yet-seen span of the session's transcript and
// advances the offset only after the pipeline reports success.
func captureRange(ev hookEvent, maxBytes int) {
	ctx, cancel := context.WithTimeout(context.Background(), captureBudget())
	defer cancel()
	captureRangeCtx(ctx, ev, maxBytes)
}

// captureRangeCtx is captureRange under a caller-supplied budget. It reports
// whether it captured and committed a chunk, so a drain loop knows whether to
// continue: false means either nothing was left or the chunk failed — and in
// both cases continuing would spin, since the offset has not moved.
func captureRangeCtx(ctx context.Context, ev hookEvent, maxBytes int) bool {
	offset := loadCaptureOffset(ev.SessionID)
	text, newOffset := extractTranscriptTextFrom(ev.TranscriptPath, offset, maxBytes)
	span := lastSpan.take()
	if strings.TrimSpace(text) == "" {
		return false
	}
	if !captureTextCtx(ctx, ev, text) {
		return false // leave the offset alone so the next run retries this span
	}
	// Feed the action/outcome layer from the same span. Best-effort and after
	// the capture succeeded: the claims are the knowledge, and a brain that
	// records the conversation but misses an action is strictly better than one
	// that loses the conversation trying to record the action.
	recordToolActions(ctx, ev, span)
	// No session id means no offset can be persisted (storeCaptureOffset is a
	// no-op for one), so a further pass would re-read this exact span. Capture
	// it once and report no progress.
	if strings.TrimSpace(ev.SessionID) == "" {
		return false
	}
	if err := storeCaptureOffset(ev.SessionID, newOffset); err != nil {
		fmt.Fprintf(os.Stderr, "mnemos: capture offset not saved: %v\n", err)
		// The chunk IS persisted but the high-water mark is not, so a further
		// pass would re-ingest this same span forever. Stop the drain here.
		return false
	}
	// Report actual progress, never merely "a chunk succeeded". A drain loop
	// keyed on anything weaker spins for the whole budget the moment the
	// offset stops moving.
	return newOffset > offset
}

// Capture strategies. Which one is right depends on the provider, and the
// difference is not cosmetic:
//
//   - A slow local model cannot extract a whole transcript in one request at
//     all (measured: ~23 minutes for 20KiB on qwen2.5:14b), so it needs the
//     work spread across the session.
//   - A fast hosted model handles a whole transcript in one request, and
//     capturing per turn would instead mean an API call per turn — each
//     re-sending the ~5KB extraction prompt. That is real money for no gain.
const (
	// captureIncremental captures during the session (Stop + PreCompact), with
	// SessionEnd sweeping the remainder.
	captureIncremental = "incremental"
	// captureAtEnd captures once at SessionEnd. The original behavior.
	captureAtEnd = "end"
	// captureOff installs no capture hooks at all.
	captureOff = "off"
	// "auto" is also accepted (and is the default): it resolves to one of the
	// above from the configured provider, so it is never itself a result.
)

// captureStrategy resolves MNEMOS_CAPTURE_STRATEGY (`capture.strategy` in
// mnemos.yaml). Unset or unrecognised means auto, which never disables
// capture: a typo should not silently stop recording knowledge.
func captureStrategy() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MNEMOS_CAPTURE_STRATEGY"))) {
	case captureIncremental:
		return captureIncremental
	case captureAtEnd:
		return captureAtEnd
	case captureOff:
		return captureOff
	}
	return autoStrategy()
}

// autoStrategy picks a default. It resolves exactly one case from
// configuration and refuses to guess at the rest.
//
// The property that decides the right strategy is extraction LATENCY, and
// configuration does not carry it. Two earlier attempts here both failed on
// ordinary setups: keying on a loopback base URL misread a hosted model behind
// a local gateway, and keying on the model name misreads every aggregator —
// OpenRouter, Together, Fireworks, Groq and DeepInfra all serve open-weight
// models fast over an OpenAI-compatible API, so "llama" means slow under
// ollama and fast under OpenRouter. Chasing that with a provider list is a
// maintenance burden that is wrong again with each new service.
//
// ollama is the one case configuration settles: it is a local daemon, so it is
// always inference on this machine. Everything else defaults to `end`, which
// is the historical behavior and cannot surprise anyone with per-turn API
// spend. Users on a slow local runtime behind openai-compat set
// capture.strategy explicitly — see docs/integrations.md for the symptom that
// says they should.
func autoStrategy() string {
	// Hosted brain: the server runs extraction from its own config, so the
	// local provider says nothing about how long it takes. Without this, a
	// hosted client with ollama installed for something else would capture per
	// turn — one request per turn to the server where one per session would do.
	// A self-hosted server on slow hardware is a real case, but only its
	// operator knows, so that stays an explicit capture.strategy.
	if hostedConfigured() {
		return captureAtEnd
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("MNEMOS_LLM_PROVIDER")), "ollama") {
		return captureIncremental
	}
	return captureAtEnd
}
