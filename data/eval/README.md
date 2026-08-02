# Mnemos Evaluation Framework

This directory contains test cases for evaluating the Mnemos extraction and relationship detection engines.

## Structure

```
data/eval/
├── README.md                     # This file
├── schema.yaml                   # Format specification
├── eval_test.go                  # Test runner with precision/recall metrics
├── claim_types.yaml              # Claim type classification tests (10 cases)
├── deduplication.yaml            # Deduplication tests (8 cases)
├── contested_detection.yaml      # Contradiction detection tests (10 cases)
├── confidence_scoring.yaml       # Confidence score tests (8 cases)
├── edge_cases.yaml               # Edge case tests (12 cases)
├── boundaries.yaml               # Boundary condition tests (12 cases)
├── real_world.yaml               # Real document tests (8 cases)
├── robustness.yaml               # Noisy/messy input tests (10 cases)
├── relationship_detection.yaml   # Relate engine eval (12 cases)
├── llm_extraction.yaml           # LLM extraction tests (requires API key)
├── retrieval.yaml                # Ranker comparison, MRR per strategy
└── brainbench/                   # Cognitive-process A/B experiments (see below)
```

The `*.yaml` files in this directory are **component** evals: given this input,
did the extractor / relater / ranker produce the right output? `brainbench/` is a
different unit of work and is run by a different command — see below.

## Running the Evaluation

```bash
# Run all tests (strict matching, excludes robustness and LLM)
go test ./data/eval/ -run TestAllCases -v

# Precision/recall metrics across all categories
go test ./data/eval/ -run TestPrecisionRecall -v

# Relationship detection precision/recall
go test ./data/eval/ -run TestRelationshipDetection -v

# Robustness tests (substring matching for noisy inputs)
go test ./data/eval/ -run TestRobustness -v

# Run by category
go test ./data/eval/ -run TestClaimTypes -v
go test ./data/eval/ -run TestContestedDetection -v
go test ./data/eval/ -run TestRealWorld -v

# LLM extraction eval (requires MNEMOS_LLM_PROVIDER)
MNEMOS_LLM_PROVIDER=openai MNEMOS_LLM_API_KEY=... go test ./data/eval/ -run TestLLMExtraction -v
```

## Evaluation Dimensions

### Extraction Quality (TestPrecisionRecall)
- **Precision** — What fraction of extracted claims are correct?
- **Recall** — What fraction of expected claims are found?
- **F1 Score** — Harmonic mean of precision and recall
- Reports per-category and aggregate metrics

### Relationship Detection (TestRelationshipDetection)
- **Precision** — Of detected relationships, how many are correct?
- **Recall** — Of true relationships, how many are detected?
- Tests both supports and contradicts relationship types
- Includes false-positive tests (unrelated claims should produce no relationships)

### Robustness (TestRobustness)
- Tests extraction on noisy, messy real-world inputs
- Emails with signatures, Slack messages, markdown with code blocks
- Bullet lists, abbreviations, URLs, redundant text, table-like data
- Uses substring matching (rule-based engine preserves prefix noise)

### Strict Tests (TestAllCases)
- Exact-match validation for clean inputs
- Covers: claim types, deduplication, contested detection, confidence scoring, edge cases, boundaries, real-world documents

### Cognitive-process benefit (brainbench/)

Everything above measures a component in isolation. `brainbench/` measures
something the component evals structurally cannot: whether the continuous
cognitive processes (consolidate, forget, reinforce, credit, salience,
synthesize, decay) actually **improve** a brain — by seeding two byte-identical
brains, running the process set on one, and diffing the outcomes.

```bash
make brain-eval          # human table + machine-readable brainbench.json
make brain-eval-strict   # exit 1 if any scored metric degraded
```

It is a separate command, not part of `make test`, because each scenario seeds
and measures four SQLite brains. See `brainbench/README.md` for the method, the
metric directions, and — importantly — the list of what it does not measure.

## Adding New Tests

1. Add test cases to the appropriate YAML file (or create a new one)
2. Follow the schema in `schema.yaml`
3. Tag cases appropriately for filtering
4. For noisy inputs, add to `robustness.yaml` (uses substring matching)
5. For relationship tests, add to `relationship_detection.yaml`
