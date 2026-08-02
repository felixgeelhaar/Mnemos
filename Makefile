GO ?= go
BIN_DIR ?= bin
APP ?= mnemos
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)"

PROTO_DIR := proto
PROTO_GEN := proto/gen

# SQLC_VERSION pins the exact sqlc used to (re)generate internal/store/sqlite/sqlcgen.
# It MUST match the `// sqlc vX.Y.Z` header stamped into the committed generated
# files: an off-version sqlc silently rewrites the generated SQL and has, in the
# past, corrupted a query string. The `sqlc` target runs this exact version via
# `go run ...@$(SQLC_VERSION)` so whatever `sqlc` sits on the operator's PATH is
# never used. The same version is recorded as a `tool` dependency in go.mod.
SQLC_VERSION := v1.30.0

.PHONY: fmt lint test test-integration build cross check sqlc install release-snapshot release-check proto mutation mutation-trust mutation-relate mutation-query brain-eval brain-eval-strict

fmt:
	$(GO) fmt ./...

lint:
	$(GO) vet ./...
	golangci-lint run

test:
	$(GO) test ./...

# test-race mirrors what CI actually runs (.github/workflows/ci.yml).
# `make check` deliberately omits -race — it is ~10x slower and would make the
# inner loop painful — but that gap meant a local "all green" did not imply a
# green CI, and it hid an intermittent failure for some time. Run this before
# pushing anything touching concurrency, storage, or the write path.
test-race:
	$(GO) test -race -count=1 ./...

# test-integration spins up ephemeral postgres + mysql containers,
# runs the gated integration suites against them, and tears the
# containers down. Skips the run with a clear message if Docker
# is not available. Mirrors the GitHub Actions database-providers
# job so developers can reproduce CI locally.
test-integration:
	@command -v docker >/dev/null 2>&1 || { echo "docker not installed; skipping integration tests"; exit 0; }
	@scripts/test-integration.sh

build:
	mkdir -p $(BIN_DIR)
	$(GO) build $(LDFLAGS) -o $(BIN_DIR)/$(APP) ./cmd/mnemos

install:
	$(GO) install $(LDFLAGS) ./cmd/mnemos

sqlc:
	$(GO) run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate

proto:
	@mkdir -p $(PROTO_GEN)
	@protoc \
		--go_out=$(PROTO_GEN) --go_opt=paths=source_relative \
		--go-grpc_out=$(PROTO_GEN) --go-grpc_opt=paths=source_relative \
		-I$(PROTO_DIR) \
		$(shell find $(PROTO_DIR) -name '*.proto')
	$(GO) fmt ./$(PROTO_GEN)/...

# cross compiles every target the release builds. CI runs linux only
# (cross-platform: false in ci.yml), so a platform-specific mistake — a
# syscall field that exists on Unix but not Windows, say — otherwise gets
# discovered by GoReleaser after the tag is already pushed.
cross:
	@fail=0; \
	for t in windows/amd64 windows/arm64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		goos=$${t%/*}; goarch=$${t#*/}; \
		if GOOS=$$goos GOARCH=$$goarch go build ./... 2>/tmp/cross-$$goos-$$goarch.err; then \
			echo "  ok    $$t"; \
		else \
			echo "  FAIL  $$t"; sed 's/^/        /' /tmp/cross-$$goos-$$goarch.err | head -5; fail=1; \
		fi; \
	done; \
	exit $$fail

check: fmt lint test build cross

# nox-scan runs the security baseline scan and exits non-zero when any
# new finding is detected (anything not present in findings.json).
# Operators refresh the baseline by reviewing diffs in findings.json.
nox-scan:
	@if command -v nox >/dev/null 2>&1; then \
		nox scan .; \
	else \
		echo "nox not installed, skipping"; \
	fi

release-check:
	goreleaser check

release-snapshot:
	goreleaser release --snapshot --clean

# mutation runs the in-tree mutation harness against internal/trust
# and gates on a 0.70 kill rate. See docs/testing/mutation.md for
# rationale and the threshold-ratchet plan. Add internal/relate and
# internal/query as advisory (non-gating) runs.
mutation: mutation-trust

mutation-trust:
	$(GO) run ./tools/mutate -pkg ./internal/trust -threshold 0.70 -v

mutation-relate:
	$(GO) run ./tools/mutate -pkg ./internal/relate -threshold 0.0 -v -json mutation-relate.json

mutation-query:
	$(GO) run ./tools/mutate -pkg ./internal/query -threshold 0.0 -v -json mutation-query.json

# brain-eval answers "do the cognitive processes IMPROVE what the brain knows?"
# — a paired A/B experiment (control vs consolidated, on byte-identical brains)
# rather than the activity counters `mnemos consolidate` already prints. Writes
# a machine-readable report to brainbench.json alongside the human table.
#
# MNEMOS_DB_URL is pinned to a throwaway: every arm is a temp SQLite file, but
# an unset MNEMOS_DB_URL makes the resolver fall back to the developer's real
# global brain, which is how test fixtures once ended up in a live 118 MB store.
#
# It does NOT gate: a regression here is a finding to READ, and failing the
# build on one creates pressure to weaken scenarios until they stop reporting
# any. Use brain-eval-strict when you have a baseline you intend to hold.
brain-eval:
	@MNEMOS_DB_URL=memory://brain-eval $(GO) run ./tools/brainbench -json brainbench.json

brain-eval-strict:
	@MNEMOS_DB_URL=memory://brain-eval $(GO) run ./tools/brainbench -json brainbench.json -fail-on-regression
