# bash with pipefail, so a test piped through grep still fails the target when it fails.
SHELL       := bash
.SHELLFLAGS := -o pipefail -c

GO      ?= go
TOOL    := $(GO) tool -modfile=tools/go.mod
COMPOSE ?= docker compose

# Local infrastructure from compose.yaml.
DATABASE_URL ?= postgres://jupiter:jupiter@127.0.0.1:55432/jupiter?sslmode=disable
VAULT_DATABASE_URL ?= postgres://vault:vault@127.0.0.1:55433/vault?sslmode=disable

# Cases per property in test-property. `make test` uses rapid's default of 100.
RAPID_CHECKS ?= 1000

# Packages whose tests use rapid: the only ones that accept -rapid.* flags.
PROPERTY_PKGS = $(shell $(GO) list -tags=integration -f '{{.ImportPath}} {{join .TestImports " "}} {{join .XTestImports " "}}' ./... | awk '/pgregory.net\/rapid/ {print $$1}')

.DEFAULT_GOAL := help

.PHONY: help
help: ## List the targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-z0-9-]+:.*## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: check
check: generate-check fmt-check vet lint vuln test ## Everything CI checks except Docker-based tests

OPENAPI_OUT = internal/api/openapi/openapi.gen.go
PIXAPI_OUT = pkg/pixapi/pixapi.gen.go

.PHONY: generate
generate: ## Regenerate code from SQL (sqlc), api/openapi.yaml and the API Pix specification (oapi-codegen)
	$(TOOL) sqlc generate
	$(TOOL) oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml
	$(TOOL) oapi-codegen -config api/bacen-pix/oapi-codegen.yaml api/bacen-pix/openapi.yaml

.PHONY: generate-check
generate-check: ## Fail if generated code is stale
	$(TOOL) sqlc diff
	@$(MAKE) --no-print-directory stale-check CONFIG=api/oapi-codegen.yaml SPEC=api/openapi.yaml OUT=$(OPENAPI_OUT)
	@$(MAKE) --no-print-directory stale-check CONFIG=api/bacen-pix/oapi-codegen.yaml SPEC=api/bacen-pix/openapi.yaml OUT=$(PIXAPI_OUT)

.PHONY: stale-check
stale-check:
	@dir=$$(mktemp -d) && sed "s|^output: .*|output: $$dir/gen.go|" $(CONFIG) > $$dir/config.yaml && \
		$(TOOL) oapi-codegen -config $$dir/config.yaml $(SPEC) && \
		if ! diff -q $$dir/gen.go $(OUT) >/dev/null; then rm -rf $$dir; echo "$(OUT) is stale: run make generate" >&2; exit 1; fi; \
		rm -rf $$dir

.PHONY: fmt
fmt: ## Format the code
	$(TOOL) golangci-lint fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail if any file is not formatted
	$(TOOL) golangci-lint fmt --diff ./...

.PHONY: vet
vet: ## Run go vet, including code behind build tags
	$(GO) vet -tags=integration,e2e,simulation,load ./...

.PHONY: lint
lint: ## Run golangci-lint
	$(TOOL) golangci-lint run ./...

.PHONY: vuln
vuln: ## Report known vulnerabilities in reachable code
	$(TOOL) govulncheck ./...

.PHONY: test
test: ## Unit, property and architecture tests, with the race detector
	$(GO) test -race -count=1 ./...

.PHONY: test-property
test-property: ## Property tests only, including those against PostgreSQL, with RAPID_CHECKS cases each
	$(GO) test -count=1 -timeout=30m -tags=integration -run=Property $(PROPERTY_PKGS) -rapid.checks=$(RAPID_CHECKS)

.PHONY: bench-ledger
bench-ledger: ## The hot-account load recorded in docs/benchmarks
	JUPITER_LEDGER_BENCH=1 $(GO) test -count=1 -tags=integration -run=TestConcurrentWritersToAHotAccount -v ./internal/ledger/ | grep 'hot account'

PROMETHEUS_IMAGE = $(shell awk '/image: prom\/prometheus/ {print $$2}' compose.yaml)

.PHONY: check-alerts
check-alerts: ## Check deploy/prometheus's alert rules and run their unit tests with promtool (Docker)
	docker run --rm -v $(CURDIR)/deploy/prometheus:/rules:ro -w /rules --entrypoint promtool $(PROMETHEUS_IMAGE) check rules alerts.yml
	docker run --rm -v $(CURDIR)/deploy/prometheus:/rules:ro -w /rules --entrypoint promtool $(PROMETHEUS_IMAGE) test rules alerts_test.yml

.PHONY: bench-ledger-sweep
bench-ledger-sweep: ## The ledger's saturation sweep recorded in docs/benchmarks
	JUPITER_LEDGER_SWEEP=1 $(GO) test -count=1 -timeout=60m -tags=integration -run=TestLedgerSaturation -v ./internal/ledger/ | grep -E '\||^(---|ok|FAIL)'

.PHONY: bench-authorization
bench-authorization: ## The authorization path's load test recorded in docs/benchmarks; LOAD_LEVELS, LOAD_POOL_CONNS, LOAD_DIAGNOSE, LOAD_PROFILE_DIR
	$(GO) test -count=1 -timeout=60m -tags=load -run=TestAuthorizationPath -v ./test/load/ | grep -E 'load_test.go|^(---|ok|FAIL)'

.PHONY: test-integration
test-integration: ## Integration tests against real dependencies in Docker (testcontainers), including test/pci
	$(GO) test -race -count=1 -tags=integration -run=. ./...

.PHONY: test-e2e
test-e2e: ## The golden path, end to end
	$(GO) test -race -count=1 -tags=e2e ./test/e2e/...

SIM_PAYMENTS ?= 300
SIM_RAIL_SCENARIOS ?= 300

.PHONY: test-simulation
test-simulation: ## Deterministic simulation: SIM_PAYMENTS card payments and SIM_RAIL_SCENARIOS on every rail, with faults, for each of SIM_SEEDS; SIM_SEED replays one
	SIM_PAYMENTS=$(SIM_PAYMENTS) SIM_RAIL_SCENARIOS=$(SIM_RAIL_SCENARIOS) SIM_SEED=$(SIM_SEED) SIM_SEEDS=$(SIM_SEEDS) \
		$(GO) test -count=1 -timeout=120m -tags=simulation -run='$(SIM_RUN)' -v ./test/simulation/ | grep -E '_test.go:[0-9]+:|--- |^(ok|FAIL)'


.PHONY: tidy
tidy: ## Tidy both modules
	$(GO) mod tidy
	cd tools && $(GO) mod tidy

.PHONY: build
build: ## Build every binary into bin/
	$(GO) build -o bin/ ./cmd/...

.PHONY: migrate
migrate: ## Apply migrations to the local databases, Jupiter's and the vault's
	JUPITER_DATABASE_URL='$(DATABASE_URL)' $(GO) run ./cmd/jupiterctl migrate
	JUPITER_DATABASE_URL='$(VAULT_DATABASE_URL)' $(GO) run ./cmd/vault migrate

.PHONY: certs
certs: ## Write a development CA and the vault's, API's and worker's mTLS certificates to .certs/
	$(GO) run ./cmd/jupiterctl dev-certs .certs

.PHONY: merchant
merchant: ## Create a merchant on the local database: make merchant NAME="Loja"
	JUPITER_DATABASE_URL='$(DATABASE_URL)' $(GO) run ./cmd/jupiterctl merchant create "$(NAME)"

.PHONY: ledger-check
ledger-check: ## Verify ledger invariants on the local database
	JUPITER_DATABASE_URL='$(DATABASE_URL)' $(GO) run ./cmd/jupiterctl ledger check

.PHONY: up
up: ## Start local infrastructure and wait until every service is healthy
	$(COMPOSE) up --build --detach --wait --wait-timeout 180

.PHONY: down
down: ## Stop local infrastructure, keeping its volumes
	$(COMPOSE) down

.PHONY: clean
clean: ## Stop local infrastructure and delete its volumes and bin/
	$(COMPOSE) down --volumes
	rm -rf bin/

.PHONY: demo
demo: ## Walk through the golden path, narrating each step
	$(GO) test -count=1 -tags=e2e -run=TestGoldenPath -v ./test/e2e/ | grep -E 'golden_path_test.go|^(---|ok|FAIL)'

