//go:build !windows

package kernel

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNewEvidenceSink_RefusesSymlink pins the audit-integrity property: the
// evidence chain must never be appended through a symlink, or a local user
// who can plant one redirects the whole tamper-evident log into a file of
// their choosing.
func TestNewEvidenceSink_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.jsonl")
	link := filepath.Join(dir, "evidence.jsonl")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	sink, err := newEvidenceSink(link)
	if err == nil {
		if sink != nil && sink.c != nil {
			_ = sink.c.Close()
		}
		t.Fatal("newEvidenceSink opened the evidence log through a symlink; the audit chain could be redirected")
	}
	if sink != nil {
		t.Errorf("sink = %v, want nil when the open is refused", sink)
	}
}

// TestNewEvidenceSink_OpensRegularFile is the companion: refusing symlinks
// must not break the ordinary case, including creating the file and its
// parent directories.
func TestNewEvidenceSink_OpensRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "evidence.jsonl")

	sink, err := newEvidenceSink(path)
	if err != nil {
		t.Fatalf("newEvidenceSink(%q): %v", path, err)
	}
	if sink == nil {
		t.Fatal("expected a sink for a regular path")
	}
	t.Cleanup(func() {
		if sink.c != nil {
			_ = sink.c.Close()
		}
	})
	if _, err := os.Stat(path); err != nil {
		t.Errorf("evidence log was not created: %v", err)
	}
}
