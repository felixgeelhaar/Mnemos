package pipeline

import (
	"path/filepath"
	"testing"
)

// The regression: the extraction cache defaulted to the RELATIVE path
// "data/cache/llm-extraction", so it resolved against the process's working
// directory. mnemos runs from the user's repository, so cache files were
// written into whichever project the session was in — measured across six
// repositories for the sibling durability cache, 6,108 files in the worst case,
// none of them gitignored.
//
// Asserting absoluteness is asserting that a cache cannot be created inside a
// user's project again.
func TestExtractionCacheDir_IsAbsoluteAndUnderTheDataDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/xdg-test")

	got := ExtractionCacheDir()
	if !filepath.IsAbs(got) {
		t.Fatalf("ExtractionCacheDir() = %q, must be absolute or it lands in the CWD", got)
	}
	if want := "/tmp/xdg-test/mnemos/cache/llm-extraction"; got != want {
		t.Errorf("ExtractionCacheDir() = %q, want %q", got, want)
	}
}

// A cache split by working directory is not one cache but N partial ones, so a
// response paid for in one project is a miss in the next. Sharing the brain's
// data directory is what makes the cache actually cache.
func TestExtractionCacheDir_HonoursXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/somewhere-else")

	if got, want := ExtractionCacheDir(), "/tmp/somewhere-else/mnemos/cache/llm-extraction"; got != want {
		t.Errorf("ExtractionCacheDir() = %q, want %q", got, want)
	}
}
