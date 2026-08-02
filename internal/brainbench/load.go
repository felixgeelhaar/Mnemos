package brainbench

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LoadScenarios reads every *.yaml scenario in dir.
//
// A malformed or invalid scenario is a hard error, never a skip. A silently
// skipped scenario turns into a report that looks complete while quietly
// covering less than it claims — and the skipped one is disproportionately
// likely to be the one someone just edited.
func LoadScenarios(dir string) ([]Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("brainbench: read scenario dir %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names) // stable scenario order across filesystems

	out := make([]Scenario, 0, len(names))
	seen := make(map[string]string, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path) //nolint:gosec // repo-local fixture path
		if err != nil {
			return nil, fmt.Errorf("brainbench: read %s: %w", path, err)
		}
		sc, err := ParseScenario(data)
		if err != nil {
			return nil, fmt.Errorf("brainbench: %s: %w", path, err)
		}
		if prev, dup := seen[sc.ID]; dup {
			return nil, fmt.Errorf("brainbench: scenario id %q defined in both %s and %s", sc.ID, prev, path)
		}
		seen[sc.ID] = path
		out = append(out, sc)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("brainbench: no scenarios found in %s", dir)
	}
	return out, nil
}
