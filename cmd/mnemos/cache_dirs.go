package main

import "path/filepath"

// Cache locations for the two on-disk LLM caches.
//
// Both defaults inside internal/extract are RELATIVE ("data/cache/..."), so
// they resolve against whatever working directory the process happens to have.
// mnemos is invoked from the user's repository — the recall hook classifies
// durability, the capture hook extracts — so the caches were being written into
// whichever project the session was in.
//
// Measured before this fix: durability verdicts scattered across six
// repositories (6,108 files in one, 1,474 in another, plus four more), none of
// them gitignored, one `git add -A` away from being committed.
//
// The pollution is the visible half. The functional half is worse: a cache
// keyed to the current directory is not one cache but N partial ones, so a
// verdict earned while working in repo A is a miss in repo B. The durability
// classifier exists to make an interrupted pass resumable — that guarantee only
// holds if every pass reads the same cache. Splitting it by CWD is why
// long-running classification never converged: each run resumed a different
// fragment.
//
// Resolving both against dataDir() — the same per-user location as the brain
// itself — makes the cache global, which is what the resumability argument
// assumed all along.

// durabilityCacheDir is where session-local/durable verdicts are cached.
//
// The extraction cache's counterpart lives in internal/pipeline
// (ExtractionCacheDir) because that is where its only callers are. Deliberately
// not duplicated here: two resolvers for the same kind of path is how one of
// them drifts, which is the shape of the bug this file exists to fix.
func durabilityCacheDir() string {
	return filepath.Join(dataDir(), "cache", "durability")
}
