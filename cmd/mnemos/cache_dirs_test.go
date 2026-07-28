package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The regression: both cache paths were relative, so they resolved against the
// process's working directory. mnemos runs from the user's repository, so
// verdicts landed in whichever project the session happened to be in —
// measured across six repositories, 6,108 files in the worst case, none of them
// gitignored.
//
// Asserting absoluteness is asserting that a cache cannot be created inside a
// user's project again.
func TestDurabilityCacheDir_IsAbsoluteAndUnderDataDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/xdg-test")

	got := durabilityCacheDir()
	if !filepath.IsAbs(got) {
		t.Fatalf("durabilityCacheDir() = %q, must be absolute or it lands in the CWD", got)
	}
	if want := "/tmp/xdg-test/mnemos/cache/durability"; got != want {
		t.Errorf("durabilityCacheDir() = %q, want %q", got, want)
	}
}

// The cache must sit beside the brain, so one `mnemos` install has one cache
// rather than one per directory it is invoked from. A split cache is why an
// interrupted classification pass never resumed: each run read a different
// fragment. (The extraction cache's equivalent is covered in internal/pipeline,
// where its resolver lives.)
func TestDurabilityCacheDir_SharesTheBrainsDataDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/xdg-test")

	base := dataDir()
	got := durabilityCacheDir()
	if !strings.HasPrefix(got, base+string(filepath.Separator)) {
		t.Errorf("durability cache %q is not under the data dir %q", got, base)
	}
}
