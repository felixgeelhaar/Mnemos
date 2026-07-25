//go:build !windows

package kernel

import (
	"os"
	"syscall"
)

// openAuditFile opens the evidence log for appending, refusing to follow a
// symlink at the final path component (O_NOFOLLOW). The evidence chain is a
// tamper-evident audit artifact: if a local user can pre-place a symlink at
// the configured path, an append-mode open would silently redirect the whole
// chain into another file — defeating the audit and writing attacker-shaped
// JSONL wherever the link points. Failing closed is the right trade here; a
// misconfigured path surfaces as a startup error rather than a silent
// integrity hole.
func openAuditFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
}
