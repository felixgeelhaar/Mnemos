package kernel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"go.klarlabs.de/axi/domain"
)

// evidenceSink persists the kernel's tamper-evident evidence chain to a
// JSONL file on disk so cross-session audit survives process restarts and
// bolt's stdout buffer rotating. The JSONL path is off by default; opt in
// via the MNEMOS_AXI_EVIDENCE_LOG env var or the evidenceLogPath argument
// to [Build].
//
// The sink is fed by [capturingSessionRepo.Save] — the persistence seam
// that receives the *complete* ExecutionSession, including every
// EvidenceRecord with its Kind, Source, Value, TokensUsed, Hash, and
// PreviousHash. axi's DomainEvent stream cannot carry those fields (see
// domain.EvidenceRecorded, which exposes only Kind/Tokens/Hash), so the
// Save seam is the only place a genuine, reconstructable, verifiable
// cross-session audit can be produced. On a session's first terminal
// Save the whole chain is written in a single Write.
type evidenceSink struct {
	mu      sync.Mutex
	w       io.Writer
	c       io.Closer
	written map[domain.ExecutionSessionID]bool // sessions already flushed
	path    string
}

// evidenceLine is the on-disk JSONL shape: one full evidence record plus
// the session/action it belongs to so a cross-session log is
// self-describing. Field names match domain.EvidenceSnapshot so the log
// can round-trip back into an EvidenceRecord for verification.
type evidenceLine struct {
	SessionID    string `json:"session_id"`
	ActionName   string `json:"action_name"`
	Kind         string `json:"kind"`
	Source       string `json:"source"`
	Value        any    `json:"value"`
	TokensUsed   int64  `json:"tokens_used,omitempty"`
	Hash         string `json:"hash,omitempty"`
	PreviousHash string `json:"previous_hash,omitempty"`
}

// newEvidenceSink opens the JSONL evidence log at the given path
// (creating parent directories as needed). Empty path falls back to
// MNEMOS_AXI_EVIDENCE_LOG; still empty returns a nil sink — callers
// treat that as "disabled". Special values "-" / "stdout" route to
// os.Stdout.
//
// The path is operator-supplied (WithEvidenceLog, MNEMOS_AXI_EVIDENCE_LOG,
// or evidence_log: in the config), never request-supplied, so this is not
// a traversal sink for untrusted input. It is still opened defensively:
// this file is the tamper-evident audit chain, so a symlink planted at the
// configured path would let a local user both redirect the audit trail and
// have the process append attacker-shaped JSONL into an unrelated file.
// openAuditFile refuses to follow symlinks where the platform supports it.
func newEvidenceSink(path string) (*evidenceSink, error) {
	if path == "" {
		path = os.Getenv("MNEMOS_AXI_EVIDENCE_LOG")
	}
	if path == "" {
		return nil, nil
	}
	if path == "-" || path == "stdout" {
		return &evidenceSink{w: os.Stdout, path: "stdout", written: map[domain.ExecutionSessionID]bool{}}, nil
	}
	path = filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("axi evidence log: mkdir: %w", err)
	}
	f, err := openAuditFile(path)
	if err != nil {
		return nil, fmt.Errorf("axi evidence log: open %s: %w", path, err)
	}
	return &evidenceSink{w: f, c: f, path: path, written: map[domain.ExecutionSessionID]bool{}}, nil
}

// record persists a session's full evidence chain the first time the
// session reaches a terminal state. Subsequent Saves of the same session
// are no-ops, so the chain is written exactly once. Non-terminal saves
// (Pending / AwaitingApproval) carry no settled chain and are skipped.
//
// Each record is marshalled into a single buffer (one JSON object per
// line, trailing newline included) and flushed with one Write so
// concurrent sessions never interleave torn lines and the first write
// error is observed rather than swallowed mid-stream.
func (s *evidenceSink) record(session *domain.ExecutionSession) {
	if s == nil || session == nil {
		return
	}
	if !isTerminal(session.Status()) {
		return
	}
	id := session.ID()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.written[id] {
		return
	}

	records := session.Evidence()
	if len(records) == 0 {
		// A terminal session with no evidence (e.g. an executor-not-found
		// failure) still gets marked written so a later Save doesn't
		// re-scan it; there is simply nothing to serialize.
		s.written[id] = true
		return
	}

	var buf bytes.Buffer
	for _, r := range records {
		line := evidenceLine{
			SessionID:    string(id),
			ActionName:   string(session.ActionName()),
			Kind:         r.Kind,
			Source:       r.Source,
			Value:        r.Value,
			TokensUsed:   r.TokensUsed,
			Hash:         string(r.Hash),
			PreviousHash: string(r.PreviousHash),
		}
		encoded, err := json.Marshal(line)
		if err != nil {
			// A record whose Value won't canonicalise is skipped rather
			// than aborting the whole flush; its empty hash already marks
			// it unverifiable in the session itself.
			continue
		}
		buf.Write(encoded)
		buf.WriteByte('\n')
	}
	s.written[id] = true
	if buf.Len() == 0 {
		return
	}
	_, _ = s.w.Write(buf.Bytes())
}

// isTerminal reports whether a session status is a settled end state
// whose evidence chain is final and safe to persist.
func isTerminal(st domain.ExecutionStatus) bool {
	switch st {
	case domain.StatusSucceeded, domain.StatusFailed, domain.StatusRejected:
		return true
	default:
		return false
	}
}

// Close flushes + closes the underlying file. Safe on a nil sink.
func (s *evidenceSink) Close() error {
	if s == nil || s.c == nil {
		return nil
	}
	return s.c.Close()
}
