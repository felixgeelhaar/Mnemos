//go:build windows

package kernel

import "os"

// openAuditFile opens the evidence log for appending.
//
// Windows has no O_NOFOLLOW equivalent for os.OpenFile, so the symlink
// refusal enforced on unix (see audit_open_unix.go) cannot be expressed
// here. The exposure is also narrower: creating a symlink on Windows needs
// SeCreateSymbolicLinkPrivilege (admin, or Developer Mode), so the local
// user who could plant one can generally tamper with the audit file
// directly regardless.
func openAuditFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}
