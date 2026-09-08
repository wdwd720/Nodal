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
	# --wait is given the long-lived services explicitly. Passing no list makes
	# compose wait on every service including cp-minio-init, a one-shot job that
	# creates the buckets and then exits 0 — and `--wait` treats any container
	# that exits as a failure, so `make infra-up` returned 2 while every service
	# was in fact healthy. Locally that is invisible because the stack is
	# already up; in CI it failed the whole chaos job.
	$(DOCKER) compose up -d minio-init
	$(DOCKER) compose up -d --wait postgres redis redpanda clickhouse temporal minio

infra-down: ## Stop local infra (keeps volumes)
	$(DOCKER) compose down

infra-reset: ## DESTROY local infra volumes (LOCAL DATA ONLY)
	$(DOCKER) compose down -v

infra-logs: ## Tail local infra logs
	$(DOCKER) compose logs -f --tail=100

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
.PHONY: build build-web tidy gen sqlc proto openapi-client seed-economy
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
	# buf generate execs protoc-gen-go and protoc-gen-go-grpc BY NAME from PATH,
	# so unlike every other tool here they cannot be reached through
	# `go run ./scripts/tool`. They must be installed into ./bin first and ./bin
	# put on PATH. This worked locally only because a developer's ./bin is
	# already populated and on PATH; in CI it failed with
	# `plugin protoc-gen-go: executable file not found in $$PATH`.
	$(GO) run ./scripts/tool install -only protoc-gen-go,protoc-gen-go-grpc
	cd proto && PATH="$(CURDIR)/bin:$$PATH" $(GO) run ../scripts/tool buf lint && 		PATH="$(CURDIR)/bin:$$PATH" $(GO) run ../scripts/tool buf generate

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

seed-economy: seed ## Seed the internal economy: Credit asset, Credits, a seller and a catalogue
	# Domain A is otherwise unreachable in development: no Credit asset means
	# nobody has Credits, and nothing to buy means the marketplace, the payout
	# page and the load scripts all measure an empty catalogue.
	#
	# It activates NO capability gate. MARKETPLACE is high risk, so switching it
	# on takes three principals and four evidence references; a script that did
	# it would be filling a control with fiction. The command prints the steps.
	$(GO) run ./scripts/seedeconomy
	# The GLOBAL risk policy. A native-market trade is evaluated against it and
	# fails closed without one, so an economy seeded without this has a
	# catalogue and no market. These are STARTER limits nobody signed off, which
	# is why riskpolicy refuses to write them outside LOCAL/DEV/TEST.
	$(GO) run ./scripts/riskpolicy -operator local-dev -reason "make seed-economy: the compiled-in development limits"

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------
.PHONY: test unit property race fuzz contract integration integration-list integration-race e2e e2e-web chaos load security test-all
test: unit property race ## Default developer test set

unit: ## Go unit tests
	# ./test/docs, ./test/reachability and ./test/source are here rather than
	# under `integration` because they need no database. They check that every
	# test the readiness documents cite exists, that every method which moves
	# money has a caller a deployment can reach, and that no file contains a
	# bidirectional control character. All three belong to the fast tier: a
	# broken citation, an unreachable control and source that renders as
	# something other than what it is should be caught by the same run that
	# catches a broken package.
	$(GO) test -count=1 -timeout=10m ./internal/... ./cmd/... ./scripts/... ./packages/... ./test/docs/... ./test/reachability/... ./test/source/...

property: ## Property-based financial tests (in-memory and database-backed)
	# Property tests live beside the code they constrain, named Prop*/Property*,
	# not in a separate tree. ./test/property/... used to be listed here and does
	# not exist: `go test` treats a missing package path as a hard error, so this
	# target failed outright rather than running the property tests it names.
	$(GO) test -count=1 -timeout=20m -run 'Prop|Property' ./internal/...
	# The second invocation is the point. Several properties are database-backed
	# and sit behind //go:build integration -- capital conservation, reservation
	# oversubscription, reconciliation convergence, balance convergence, the
	# ClickHouse look-ahead snapshot. The first line cannot compile those files,
	# so it silently ran none of them, while REQUIREMENTS_TRACEABILITY.md cited
	# `make property` as the operational evidence for R-150-1 and named those
	# exact tests (F-57). A target cited as proof has to execute the thing.
	$(GO) run ./scripts/inttest -run 'Prop|Property'

race: ## Race detector on concurrency-critical packages
	$(GO) test -count=1 -race -timeout=20m $(RACE_PKGS)

fuzz: ## Run every fuzz target briefly (FUZZTIME=30s default)
	$(GO) run ./scripts/fuzzall -fuzztime=$(or $(FUZZTIME),30s)

contract: ## Provider contract tests against recorded fixtures
	$(GO) test -count=1 -timeout=10m ./test/contract/...

integration: ## Integration tests: every //go:build integration package, one fresh database each
	# This used to be `go test -tags=integration ./test/integration/...`, and
	# ./test/integration/ holds exactly one package (the migration suite). The
	# database-backed proof of the financial core lives behind the build tag
	# inside internal/ and cmd/ — 40 packages that this target never ran, so
	# `make test-all` reported success having executed none of them. CI had
	# grown its own inline loop to work around it. scripts/inttest is the one
	# implementation both now call.
	$(GO) run ./scripts/inttest

integration-list: ## List the integration packages inttest would run
	$(GO) run ./scripts/inttest -list

integration-race: ## Race detector over the financial core WITH the integration tag
	# The internal-economy packages are in this list because their concurrency
	# properties are database-backed and therefore invisible to `make race`,
	# which runs without the integration tag: the credit consumption lock, the
	# market's stale-version refusal, the payout reservation and the commerce
	# purchase all race for the same rows.
	$(GO) run ./scripts/inttest -race -pkg '^\./internal/(capital|ledger|execution|reconciliation|event|settlement|signing|credit|nativemarket|payout|commerce)'

e2e: ## API-level end-to-end tests
	$(GO) test -count=1 -timeout=30m -tags=integration,e2e ./test/e2e/...

e2e-web: ## Playwright critical-path UI tests
	# The package is @controlplane/web and its script is `e2e`. This target
	# named neither correctly (`--filter web test:e2e`) and so had never run:
	# pnpm answered ERR_PNPM_RECURSIVE_RUN_NO_SCRIPT, which only surfaced once
	# CI executed for the first time.
	$(PNPM) --filter @controlplane/web e2e

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
	# -exclude-generated skips files carrying the "Code generated ... DO NOT EDIT."
	# header (internal/gen/**): protoc-gen-go emits unsafe.Slice/unsafe.StringData
	# for its raw descriptors (G103) and oapi-codegen emits enum constants whose
	# names trip the G101 word list. Neither is hand-written and `make gen`
	# overwrites any annotation added there, so the exclusion is the only stable
	# place to record the decision.
	#
	# -nosec-require-rules and -nosec-require-justification make gosec itself
	# enforce the review rule that every suppression names the rule it silences
	# and states the invariant that makes it safe: a bare `#nosec` now fails this
	# target instead of quietly hiding a finding.
	#
	# THE BLIND SPOT, stated because it is real (as in .gitleaks.toml): gosec
	# builds only the default tag set, so unlike `golangci-lint
	# --build-tags=integration` it never sees ./test/... or any *_test.go behind
	# //go:build integration, e2e or chaos. Those trees are covered by golangci's
	# gosec instead. Adding -tags here would widen the scan to a batch of
	# findings nobody has triaged, so it is a deliberate follow-up, not an
	# oversight.
	$(GO) run ./scripts/tool gosec -quiet -exclude-generated \
		-nosec-require-rules -nosec-require-justification -- ./...

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
