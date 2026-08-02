// Command brainbench runs the cognitive-process evaluation harness: a paired
// A/B experiment that measures whether consolidation actually improves what a
// brain knows, rather than merely that it ran.
//
// Usage:
//
//	go run ./tools/brainbench                                  # human report
//	go run ./tools/brainbench -json brainbench.json            # machine-readable
//	go run ./tools/brainbench -scenarios data/eval/brainbench  # explicit fixtures
//	go run ./tools/brainbench -fail-on-regression              # exit 1 on any degraded metric
//
// See internal/brainbench for the method and its limitations; the limitations
// are also embedded in every report it emits.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"go.klarlabs.de/mnemos/internal/brainbench"

	// The harness opens sqlite:// DSNs for every arm.
	_ "go.klarlabs.de/mnemos/internal/store/sqlite"
)

func main() {
	var (
		scenarioDir string
		jsonOut     string
		workDir     string
		human       bool
		failOnReg   bool
	)
	flag.StringVar(&scenarioDir, "scenarios", "data/eval/brainbench", "directory of scenario YAML files")
	flag.StringVar(&jsonOut, "json", "", "write the machine-readable report to this path (\"-\" for stdout)")
	flag.StringVar(&workDir, "work", "", "keep experiment databases here instead of a temp dir (for debugging)")
	flag.BoolVar(&human, "human", true, "print the human-readable report to stdout")
	flag.BoolVar(&failOnReg, "fail-on-regression", false, "exit 1 when any scored metric degraded")
	flag.Parse()

	if err := run(scenarioDir, jsonOut, workDir, human, failOnReg); err != nil {
		fmt.Fprintf(os.Stderr, "brainbench: %v\n", err)
		os.Exit(2)
	}
}

func run(scenarioDir, jsonOut, workDir string, human, failOnReg bool) error {
	scenarios, err := brainbench.LoadScenarios(scenarioDir)
	if err != nil {
		return err
	}

	// Experiment databases are throwaway by default: each arm is written to,
	// measured once, and never valid to reuse. Keeping them around is a
	// debugging affordance, not the norm.
	if workDir == "" {
		dir, err := os.MkdirTemp("", "brainbench-")
		if err != nil {
			return fmt.Errorf("create work dir: %w", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		workDir = dir
	} else if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}

	ctx := context.Background()
	results := make([]brainbench.ScenarioResult, 0, len(scenarios))
	for _, sc := range scenarios {
		res, err := brainbench.Run(ctx, sc, workDir)
		if err != nil {
			return err
		}
		results = append(results, res)
	}

	report := brainbench.BuildReport(results)

	if human {
		if err := report.WriteHuman(os.Stdout); err != nil {
			return err
		}
	}
	if jsonOut != "" {
		if jsonOut == "-" {
			if err := report.WriteJSON(os.Stdout); err != nil {
				return err
			}
		} else {
			f, err := os.Create(jsonOut) //nolint:gosec // operator-supplied output path
			if err != nil {
				return fmt.Errorf("create %s: %w", jsonOut, err)
			}
			if err := report.WriteJSON(f); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "brainbench: wrote %s\n", jsonOut)
		}
	}

	// Exit 1 is opt-in. A regression is a finding the harness must SHOW; making
	// it break the build by default would create pressure to weaken scenarios
	// until they stop reporting one, which is precisely the failure this
	// harness exists to prevent.
	if failOnReg && report.HasRegression() {
		return fmt.Errorf("%d scored metric(s) degraded under the process set", report.Summary.Worse)
	}
	return nil
}
