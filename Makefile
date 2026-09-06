# Universal Financial Control Plane — developer/CI task runner.
# Recipes are shell-portable (Windows cmd, Git Bash, macOS, Linux). Anything non-trivial is a Go program under scripts/.
# CI calls these same targets.

GO      ?= go
PNPM    ?= pnpm
DOCKER  ?= docker
BIN     := $(CURDIR)/bin
MODULE  := github.com/nodal/controlplane
GIT_SHA := $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo nogit)
LDFLAGS := -s -w -X $(MODULE)/internal/config.BuildVersion=$(GIT_SHA)

# Race-critical packages (PART 216)
RACE_PKGS := ./internal/capital/... ./internal/ledger/... ./internal/execution/... ./internal/reconciliation/... ./internal/event/... ./internal/settlement/... ./internal/signing/...

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show targets
	@$(GO) run ./scripts/maketargets Makefile

# ---------------------------------------------------------------------------
# Local environment
# ---------------------------------------------------------------------------
.PHONY: dev stop infra-up infra-down infra-logs infra-reset
dev: infra-up migrate ## Start local infra, migrate, then run api + workers (foreground)
	$(GO) run ./scripts/devrun

stop: infra-down ## Stop everything

infra-up: ## Start Postgres/Redis/Redpanda/ClickHouse/Temporal/MinIO
	$(DOCKER) compose up -d --wait

infra-down: ## Stop local infra (keeps volumes)
	$(DOCKER) compose down

infra-reset: ## DESTROY local infra volumes (LOCAL DATA ONLY)
	$(DOCKER) compose down -v

infra-logs: ## Tail local infra logs
	$(DOCKER) compose logs -f --tail=100

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
.PHONY: build build-web tidy gen sqlc proto openapi-client
build: ## Build all Go binaries into ./bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/ ./cmd/...

build-web: ## Build the web app
	$(PNPM) --filter web build

tidy: ## go mod tidy + verify
	$(GO) mod tidy
	$(GO) mod verify

gen: sqlc proto openapi-server openapi-client ## Regenerate all generated code

sqlc: ## Generate typed SQL (requires ./bin/sqlc or sqlc on PATH)
	$(GO) run ./scripts/tool sqlc generate

proto: ## Lint + generate protobuf/gRPC code (proto/ → internal/gen/proto)
	cd proto && $(GO) run ../scripts/tool buf lint && $(GO) run ../scripts/tool buf generate

proto-breaking: ## Check proto backward compatibility against the main branch
	cd proto && $(GO) run ../scripts/tool buf breaking --against ../.git#branch=main,subdir=proto

openapi-server: ## Generate the Go strict server + types from openapi/openapi.yaml (internal/gen/api)
	cd openapi && $(GO) run ../scripts/tool oapi-codegen -config oapi-codegen.yaml openapi.yaml

openapi-client: ## Generate TypeScript client from openapi/openapi.yaml
	$(PNPM) --filter @controlplane/generated-client generate

# ---------------------------------------------------------------------------
# Database
# ---------------------------------------------------------------------------
.PHONY: migrate migrate-status migrate-test seed
migrate: ## Apply migrations (uses DATABASE_MIGRATE_URL or local default)
	$(GO) run ./cmd/migrate up

migrate-status: ## Show migration status
	$(GO) run ./cmd/migrate status

migrate-test: ## Migration tests: clean apply, checksum, ledger-preserving down
	$(GO) test -count=1 -tags=integration ./test/integration/migrations/...

seed: ## Seed clearly-labelled LOCAL fake users/assets (refused outside LOCAL/DEV/TEST)
	$(GO) run ./scripts/seed

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------
.PHONY: test unit property race fuzz contract integration e2e e2e-web chaos load security test-all
test: unit property race ## Default developer test set

unit: ## Go unit tests
	$(GO) test -count=1 -timeout=10m ./internal/... ./cmd/... ./scripts/... ./packages/...

property: ## Property-based financial tests
	# Property tests live beside the code they constrain, named Prop*/Property*,
	# not in a separate tree. ./test/property/... used to be listed here and does
	# not exist: `go test` treats a missing package path as a hard error, so this
	# target failed outright rather than running the property tests it names.
	$(GO) test -count=1 -timeout=20m -run 'Prop|Property' ./internal/...

race: ## Race detector on concurrency-critical packages
	$(GO) test -count=1 -race -timeout=20m $(RACE_PKGS)

fuzz: ## Run every fuzz target briefly (FUZZTIME=30s default)
	$(GO) run ./scripts/fuzzall -fuzztime=$(or $(FUZZTIME),30s)

contract: ## Provider contract tests against recorded fixtures
	$(GO) test -count=1 -timeout=10m ./test/contract/...

integration: ## Integration tests (need DATABASE_URL etc.; skipped with reason if absent)
	$(GO) test -count=1 -timeout=30m -tags=integration ./test/integration/...

e2e: ## API-level end-to-end tests
	$(GO) test -count=1 -timeout=30m -tags=integration,e2e ./test/e2e/...

e2e-web: ## Playwright critical-path UI tests
	$(PNPM) --filter web test:e2e

chaos: ## Chaos tests (fault injection)
	$(GO) test -count=1 -timeout=30m -tags=integration,chaos ./test/chaos/...

load: ## k6 load test (SCRIPT=test/load/<name>.js; needs BASE_URL, SESSION, ACCOUNT_ID — see test/load/README.md)
	$(GO) run ./scripts/tool k6 run $(or $(SCRIPT),test/load/portfolio_read.js)

load-inspect: ## Validate every k6 script without running it
	@for f in test/load/*.js; do $(GO) run ./scripts/tool k6 inspect $$f >/dev/null && echo "ok  $$f"; done

security: ## Security tests (IDOR, tenant isolation, agent escalation, webhook forgery, ...)
	$(GO) test -count=1 -timeout=20m -tags=integration ./test/security/...

test-all: unit property race fuzz contract integration e2e chaos security migrate-test verify-audit ## Everything locally executable

# ---------------------------------------------------------------------------
# Quality / security scanning
# ---------------------------------------------------------------------------
.PHONY: fmt fmt-check vet lint staticcheck vuln sast secrets iac-scan container-scan sbom lint-web typecheck-web test-web check
fmt: ## gofmt + goimports
	$(GO) run ./scripts/tool gofumpt -l -w ./cmd ./internal ./scripts ./test ./packages

fmt-check: ## Fail if unformatted
	$(GO) run ./scripts/fmtcheck ./cmd ./internal ./scripts ./test ./packages

vet: ## go vet
	$(GO) vet ./...

staticcheck: ## staticcheck
	$(GO) run ./scripts/tool staticcheck ./...

lint: fmt-check vet staticcheck ## All Go linters (+ financial lint rules)
	$(GO) run ./scripts/tool golangci-lint run ./...
	$(GO) run ./scripts/lintfin ./...

vuln: ## govulncheck
	$(GO) run ./scripts/tool govulncheck ./...

sast: ## gosec
	$(GO) run ./scripts/tool gosec -quiet ./...

secrets: ## gitleaks
	$(GO) run ./scripts/tool gitleaks detect --no-banner --redact -v

iac-scan: ## Terraform/IaC scan (trivy config)
	$(GO) run ./scripts/tool trivy config --exit-code 1 --severity HIGH,CRITICAL infra/

container-scan: ## Scan every image listed in dist/images.txt (built by `make images`)
	$(GO) run ./scripts/supplychain scan

sbom: ## Generate SBOM (syft) into dist/sbom.spdx.json
	$(GO) run ./scripts/supplychain sbom

lint-web: ## Frontend lint
	$(PNPM) --filter web lint

typecheck-web: ## Frontend type check
	$(PNPM) --filter web typecheck

test-web: ## Frontend unit/component tests
	$(PNPM) --filter web test

check: lint vuln unit ## Fast pre-commit gate

# ---------------------------------------------------------------------------
# Audit / operations
# ---------------------------------------------------------------------------
.PHONY: verify-audit restore-drill images
verify-audit: ## Verify audit hash chain, signatures, Merkle roots, object hashes
	$(GO) run ./cmd/audit-worker verify

restore-drill: ## Local backup → restore → boot → reconciliation dry-run
	$(GO) run ./scripts/restoredrill

images: ## Build container images tagged with git SHA
	$(GO) run ./scripts/images -tag $(GIT_SHA)

# ---------------------------------------------------------------------------
# Tooling bootstrap
# ---------------------------------------------------------------------------
.PHONY: tools
tools: ## Install pinned developer tools into ./bin
	$(GO) run ./scripts/tool install
