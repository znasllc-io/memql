# memQL Makefile
# Source of truth for all build, test, run, and development commands.
#
# Usage:
#   make              Build all binaries
#   make help         Show all available targets
#   make dev          Start full development stack (Docker)
#   make test         Run all tests

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

GO          := go
GOFLAGS     := -v
CGO_ENABLED := 0
BIN_DIR     := bin
VERSION     := $(shell cat VERSION 2>/dev/null || echo "dev")

# Docker
COMPOSE      := docker compose
COMPOSE_FULL := -f docker/docker-compose.full.yml
COMPOSE_POLY := -f docker/docker-compose.polyphon.yml
COMPOSE_CLAW := -f docker/docker-compose.nemoclaw.yml
COMPOSE_CLST := -f docker/docker-compose.cluster.yml

# ---------------------------------------------------------------------------
# Build targets
# ---------------------------------------------------------------------------

.PHONY: all build build-all server bff voice cognition agent planner identity identity-templ identity-tailwind identity-assets identity-build healthcheck

## Build all binaries (standalone + healthcheck)
all: build healthcheck

## Build the standalone memQL server (all components)
build:
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/memql .

## Build all binaries including node-type variants
build-all: build bff voice cognition agent planner identity healthcheck

## Build BFF node binary
bff:
	$(GO) build $(GOFLAGS) -tags bff -o $(BIN_DIR)/memql-bff .

## Build voice node binary
voice:
	$(GO) build $(GOFLAGS) -tags voice -o $(BIN_DIR)/memql-voice .

## Build cognition node binary
cognition:
	$(GO) build $(GOFLAGS) -tags cognition -o $(BIN_DIR)/memql-cognition .

## Build agent node binary
agent:
	$(GO) build $(GOFLAGS) -tags agent -o $(BIN_DIR)/memql-agent .

## Build planner node binary
planner:
	$(GO) build $(GOFLAGS) -tags planner -o $(BIN_DIR)/memql-planner .

## Generate Go from .templ files for the identity web app.
## Uses `go run` so contributors don't need templ on their PATH.
identity-templ:
	$(GO) run github.com/a-h/templ/cmd/templ generate -path component/identity/web/templ

## Compile Tailwind input -> static/app.css for the identity web app.
## The script auto-downloads the standalone Tailwind CLI for the host
## platform on first run (cached under bin/tools/).
identity-tailwind:
	bash scripts/identity/build-css.sh

## Build identity node binary (in-house auth provider). Always runs
## templ generate AND tailwind compile first so the binary picks up
## the latest .templ + class changes without the contributor having
## to remember.
identity: identity-templ identity-tailwind
	$(GO) build $(GOFLAGS) -tags identity -o $(BIN_DIR)/memql-identity .

## Generate web-app assets for the identity binary (Phase 3)
identity-assets:
	bash scripts/identity/build-assets.sh

## Build the identity binary with web-app assets ready
identity-build: identity-assets identity

## Build the health check binary (for Docker distroless containers)
healthcheck:
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/healthcheck ./cmd/healthcheck

# ---------------------------------------------------------------------------
# Realtime voice + video (Python voice-agent, LiveKit Agents 1.5)
# The legacy Go Bridge Agent was retired in Initiative C Phase 11.
# ---------------------------------------------------------------------------

.PHONY: voice-agent voice-agent-run voice-agent-test voice-agent-docker voice-loop-test-livekit

## Install Python deps + regenerate proto stubs for the voice-agent process.
## Idempotent. Re-run after editing pyproject.toml or memql.proto.
voice-agent:
	bash scripts/voice-agent/install.sh

## Run the voice-agent worker locally. Reads voice-agent/.env if present.
voice-agent-run:
	bash scripts/voice-agent/run.sh start

## Run the voice-agent's pytest suite.
voice-agent-test:
	bash scripts/voice-agent/test.sh

## Build the voice-agent Docker image (LiveKit Agents 1.5 + Deepgram + memql gRPC).
## Override avatar vendor: AVATAR=simli make voice-agent-docker
voice-agent-docker:
	docker build \
		--build-arg AVATAR=$${AVATAR:-anam} \
		-f voice-agent/Dockerfile \
		-t memql-voice-agent:dev .

## End-to-end voice-loop test against the LiveKit Agents path
## (replaces voice-loop-test-deepgram once Phase 10 cutover lands).
## Requires MEMQL_DEEPGRAM_API_KEY in the calling shell.
voice-loop-test-livekit:
	@if [ -n "$(CSV)" ]; then \
		bash scripts/voice/loop-test-livekit.sh --csv "$(CSV)"; \
	else \
		bash scripts/voice/loop-test-livekit.sh; \
	fi

# ---------------------------------------------------------------------------
# Run targets
# ---------------------------------------------------------------------------

.PHONY: run dev dev-polyphon dev-nemoclaw dev-cluster dev-cluster-restart dev-cluster-restart-purge dev-cluster-stop dev-cluster-logs dev-stop dev-logs dev-ps dev-nginx-reload dev-rebuild-node voice-trace voice-trace-now db

## Run the standalone server locally
run: build
	./$(BIN_DIR)/memql

## Start full development stack (PostgreSQL + memQL) in Docker
dev:
	$(COMPOSE) $(COMPOSE_FULL) up --build

## Start development stack in background
dev-bg:
	$(COMPOSE) $(COMPOSE_FULL) up --build -d

## Start with Polyphon voice pipeline
dev-polyphon:
	$(COMPOSE) $(COMPOSE_FULL) $(COMPOSE_POLY) up --build

## Start with NemoClaw coding agent
dev-nemoclaw:
	$(COMPOSE) $(COMPOSE_FULL) $(COMPOSE_CLAW) up --build

## Start cluster mode (bff + cognition + planner)
dev-cluster:
	$(COMPOSE) $(COMPOSE_CLST) up --build

## Restart the cluster with FRESH binaries: stop, force --no-cache
## rebuild of every image (so every Go layer re-runs against the
## current source), then bring up detached. Named volumes are
## PRESERVED so postgres data survives the restart -- only code +
## containers get replaced. Use this after editing Go / MemQL /
## prompt template source: layered build-cache hits on the Go stage
## are otherwise hard to diagnose when a node produces stale
## behaviour despite looking "rebuilt".
dev-cluster-restart:
	@echo "[restart] Stopping cluster (volumes preserved)…"
	$(COMPOSE) $(COMPOSE_CLST) down --remove-orphans
	@echo "[restart] Pruning dangling images…"
	-@docker image prune -f >/dev/null 2>&1
	@echo "[restart] Rebuilding all images --no-cache (this takes a few minutes)…"
	$(COMPOSE) $(COMPOSE_CLST) build --no-cache
	@echo "[restart] Starting cluster…"
	$(COMPOSE) $(COMPOSE_CLST) up -d
	@echo ""
	@echo "Cluster containers (data preserved):"
	@docker ps --filter "name=memql-" --format "  {{.Names}}\t{{.Status}}"

## Restart the cluster with FRESH binaries AND a FRESH database.
## Same as dev-cluster-restart but also drops named volumes so
## postgres starts empty and every seed (domains, chunks, agents,
## identity) re-runs from scratch.
dev-cluster-restart-purge:
	@echo "[WARNING] Purging cluster volumes — all data will be lost."
	@echo "[purge] Stopping cluster + dropping volumes…"
	$(COMPOSE) $(COMPOSE_CLST) down -v --remove-orphans
	@echo "[purge] Pruning dangling images…"
	-@docker image prune -f >/dev/null 2>&1
	@echo "[purge] Rebuilding all images --no-cache (this takes a few minutes)…"
	$(COMPOSE) $(COMPOSE_CLST) build --no-cache
	@echo "[purge] Starting cluster with fresh DB…"
	$(COMPOSE) $(COMPOSE_CLST) up -d
	@echo ""
	@echo "Cluster containers (fresh DB):"
	@docker ps --filter "name=memql-" --format "  {{.Names}}\t{{.Status}}"

## Stop the cluster (keeps volumes)
dev-cluster-stop:
	$(COMPOSE) $(COMPOSE_CLST) down

## Follow cluster logs (all nodes)
dev-cluster-logs:
	$(COMPOSE) $(COMPOSE_CLST) logs -f

## Full rebuild — force Docker to rebuild images from scratch
dev-rebuild:
	$(COMPOSE) $(COMPOSE_FULL) down
	$(COMPOSE) $(COMPOSE_FULL) build --no-cache
	IDENTITY_VERIFIER_BASE_URL= $(COMPOSE) $(COMPOSE_FULL) up -d

## Stop all development services
dev-stop:
	$(COMPOSE) $(COMPOSE_FULL) down
	$(COMPOSE) $(COMPOSE_CLST) down 2>/dev/null || true

## View development service logs (follow)
dev-logs:
	$(COMPOSE) $(COMPOSE_FULL) logs -f

## Show running development services
dev-ps:
	$(COMPOSE) $(COMPOSE_FULL) ps

## Tail the end-to-end voice latency waterfall.
##
## Each voice turn emits structured log lines stamped with
## "voice trace" + a stable trace id (utterance id on the cognition
## side, space:agent on the bridge side) at every stage of the
## pipeline:
##
##   bridge.stt.flush          -- silence detected, OGG handed to STT
##   bridge.stt.complete       -- Whisper returned (sttMs)
##   bridge.utterance.posted   -- BFF accepted the row (postMs)
##   cognition.agent.start     -- agent LLM call begins (routeMs)
##   cognition.agent.complete  -- reply text in hand (agentLlmMs)
## Tail voice-path log lines emitted by the voice-agent + memql
## cluster. Pre-Initiative-C this targeted bridge-agent containers;
## the voice-agent process now emits the same `voice trace` markers
## from its own logs.
voice-trace:
	@$(COMPOSE) $(COMPOSE_FULL) $(COMPOSE_POLY) logs -f voice-agent bff cognition voice 2>&1 | grep --line-buffered "voice trace"

## Same as voice-trace but pre-loads the last 5 minutes of history
## so you don't have to re-utter to see a waterfall when you're
## starting the tail mid-session.
voice-trace-now:
	@$(COMPOSE) $(COMPOSE_FULL) $(COMPOSE_POLY) logs -f --since 5m voice-agent bff cognition voice 2>&1 | grep --line-buffered "voice trace"

## Reload the nginx LB so its DNS cache picks up any cluster nodes
## that got recreated since the last reload. Useful after a partial
## rebuild like `docker compose up -d --build agent` -- without a
## reload, nginx keeps trying to reach the old container's IP and
## the cockpit-worker reconnect loop fails with
## "Unimplemented: unknown service WorkerService". The fully-restarted
## flows (dev-refresh, dev-cluster-restart, dev-rebuild) recreate
## nginx as part of `compose up` and don't need this.
##
## Defense in depth -- the live nginx config also runs Docker's
## embedded resolver with a 10s TTL on every variable-based upstream,
## so partial-rebuild staleness self-heals after 10 seconds even
## without a reload. This target just makes it instant.
dev-nginx-reload:
	@docker exec memql-lb nginx -s reload && echo "[nginx] reloaded"

## Surgical "rebuild + restart ONE node, then reload nginx" loop.
## Defaults to the agent node (the one most often touched during
## worker / computer_use feature work). Override with NODE=:
##     make dev-rebuild-node NODE=cognition
##     make dev-rebuild-node NODE=bff
## Compared to running `docker compose ... up -d --build <node>`
## directly, this sequence ALSO reloads nginx so the LB picks up
## the new container's IP without the 10s resolver-TTL window.
dev-rebuild-node:
	@if [ -z "$(NODE)" ]; then NODE=agent; else NODE=$(NODE); fi; \
	echo "[rebuild-node] Rebuilding $$NODE..."; \
	$(COMPOSE) $(COMPOSE_FULL) up -d --build $$NODE; \
	echo "[rebuild-node] Reloading nginx..."; \
	docker exec memql-lb nginx -s reload >/dev/null && echo "[nginx] reloaded"

## Connect to the development database
db:
	psql postgres://memql:memql_dev@localhost:5432/memql

# ---------------------------------------------------------------------------
# Test targets
# ---------------------------------------------------------------------------

.PHONY: test test-v test-cover test-polyphon policies-lint policies-trace

## Run all tests
test:
	$(GO) test ./...

## Lint the policies tree -- validates @tier annotation matches directory
## placement, downward-only delegation across the bff -> core boundary,
## and that no policy file declares an annotation it doesn't own. Phase 0
## ships the placeholder target; Phase 6 of the policies feature
## fills in the real checks.
policies-lint:
	@bash scripts/policies/lint.sh

## Evaluate a single policy with a JSON args literal and dump the
## structured trace tree. Useful for debugging policy decisions
## without booting the full cluster. Phase 0 ships the placeholder
## target; Phase 6 of the policies feature fills in the real
## debug runner.
##
## Usage: make policies-trace POLICY=avatarVendorChoice ARGS='{"expectedDurationMinutes":30}'
policies-trace:
	@bash scripts/policies/trace.sh "$(POLICY)" '$(ARGS)'

## Run all tests with verbose output
test-v:
	$(GO) test -v ./...

## Run tests with coverage report
test-cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out
	@rm -f coverage.out

## Run Polyphon/cognition tests only
test-polyphon:
	$(GO) test ./component/polyphon/... ./integrations/cognition/...

# ---------------------------------------------------------------------------
# Code quality
# ---------------------------------------------------------------------------

.PHONY: vet fmt lint tidy generate

## Run go vet on all packages
vet:
	$(GO) vet ./...

## Format all Go files
fmt:
	$(GO) fmt ./...

## Run vet + fmt (quick lint)
lint: fmt vet

## Tidy go.mod dependencies
tidy:
	$(GO) mod tidy

## Run code generation (protobuf, etc.)
generate:
	$(GO) generate ./...

# ---------------------------------------------------------------------------
# Local dev TLS (mkcert)
# ---------------------------------------------------------------------------

.PHONY: setup-tls

## Generate locally-trusted TLS certs for the dev cluster. Requires
## mkcert (`brew install mkcert nss` on macOS). Idempotent --
## re-run any time to refresh certs.
setup-tls:
	bash scripts/dev/setup-tls.sh

# ---------------------------------------------------------------------------
# Docker image targets
# ---------------------------------------------------------------------------

.PHONY: docker docker-bff docker-voice docker-cognition docker-agent docker-planner

## Build the default Docker image (BFF)
docker:
	docker build -f docker/memql.Dockerfile -t memql:latest .

## Build BFF Docker image
docker-bff:
	docker build -f docker/memql.Dockerfile --build-arg BUILD_TAGS=bff -t memql:bff .

## Build voice Docker image
docker-voice:
	docker build -f docker/memql.Dockerfile --build-arg BUILD_TAGS=voice -t memql:voice .

## Build cognition Docker image
docker-cognition:
	docker build -f docker/memql.Dockerfile --build-arg BUILD_TAGS=cognition -t memql:cognition .

## Build agent Docker image
docker-agent:
	docker build -f docker/memql.Dockerfile --build-arg BUILD_TAGS=agent -t memql:agent .

## Build planner Docker image
docker-planner:
	docker build -f docker/memql.Dockerfile --build-arg BUILD_TAGS=planner -t memql:planner .

# ---------------------------------------------------------------------------
# Dev-secrets workflow
# ---------------------------------------------------------------------------
# Operator tooling for populating a local memQL with the secrets and
# variables declared in scripts/secrets/manifest.yaml. Values live in
# the developer's personal ~/.memql/dev-secrets.yaml (gitignored).
# See docs/planning/env-var-refactor-plan.md for the design.

.PHONY: secrets-init secrets-seed secrets-list secrets-edit secret-set secret-delete variable-set variable-delete db-purge db-purge-and-reseed dev-fresh dev-refresh dev-status

## Interactively create / update ~/.memql/dev-secrets.yaml from the
## committed manifest at scripts/secrets/manifest.yaml. Prompts only
## for entries that don't already have a value. Values are masked for
## secrets, plaintext for variables.
secrets-init:
	$(GO) run ./scripts/secrets init

## Read ~/.memql/dev-secrets.yaml and push every entry into the
## running memQL. Secrets are encrypted locally with MEMQL_MASTER_KEY
## before insert. Requires a running cluster (make dev or dev-cluster)
## and MEMQL_MASTER_KEY exported in this shell.
secrets-seed:
	$(GO) run ./scripts/secrets seed

## Show manifest entries and whether each has a value in the local
## yaml. Safe to run without MEMQL_MASTER_KEY.
secrets-list:
	$(GO) run ./scripts/secrets list

## Open ~/.memql/dev-secrets.yaml in $$EDITOR for batch edits.
secrets-edit:
	$${EDITOR:-vi} $$HOME/.memql/dev-secrets.yaml

## One-off secret upsert without touching the yaml.
## Usage: make secret-set NAME=OPENAI_API_KEY VALUE=sk-... SCOPE=global [KIND=vendor_api_key] [PARTITION=name]
secret-set:
	@[ -n "$(NAME)" ] || (echo "NAME is required"; exit 1)
	@[ -n "$(VALUE)" ] || (echo "VALUE is required"; exit 1)
	@[ -n "$(SCOPE)" ] || (echo "SCOPE=global|partition is required"; exit 1)
	$(GO) run ./scripts/secrets set secret "$(NAME)" "$(VALUE)" --scope=$(SCOPE) $(if $(PARTITION),--partition=$(PARTITION)) $(if $(KIND),--kind=$(KIND))

## One-off soft-delete of a secret.
## Usage: make secret-delete NAME=OPENAI_API_KEY SCOPE=global [PARTITION=name]
secret-delete:
	@[ -n "$(NAME)" ] || (echo "NAME is required"; exit 1)
	@[ -n "$(SCOPE)" ] || (echo "SCOPE=global|partition is required"; exit 1)
	$(GO) run ./scripts/secrets delete secret "$(NAME)" --scope=$(SCOPE) $(if $(PARTITION),--partition=$(PARTITION))

## One-off plaintext variable upsert.
## Usage: make variable-set NAME=MEMQL_DEFAULT_CHAT_PROVIDER VALUE=chat54Mini SCOPE=global
variable-set:
	@[ -n "$(NAME)" ] || (echo "NAME is required"; exit 1)
	@[ -n "$(VALUE)" ] || (echo "VALUE is required"; exit 1)
	@[ -n "$(SCOPE)" ] || (echo "SCOPE=global|partition is required"; exit 1)
	$(GO) run ./scripts/secrets set variable "$(NAME)" "$(VALUE)" --scope=$(SCOPE) $(if $(PARTITION),--partition=$(PARTITION))

## One-off soft-delete of a plaintext variable.
variable-delete:
	@[ -n "$(NAME)" ] || (echo "NAME is required"; exit 1)
	@[ -n "$(SCOPE)" ] || (echo "SCOPE=global|partition is required"; exit 1)
	$(GO) run ./scripts/secrets delete variable "$(NAME)" --scope=$(SCOPE) $(if $(PARTITION),--partition=$(PARTITION))

## Wipe ALL local memQL data (docker-compose down -v + up -d). Does
## NOT restore secrets or variables -- you run `make secrets-seed`
## after if you want them back.
db-purge:
	$(COMPOSE) $(COMPOSE_FULL) down -v
	$(COMPOSE) $(COMPOSE_FULL) up -d --build

## Wipe ALL local memQL data and immediately re-seed secrets +
## variables from ~/.memql/dev-secrets.yaml. The common iterative dev
## path: fresh DB without retyping keys. Waits ~10s for memQL to come
## back up before seeding.
db-purge-and-reseed:
	$(COMPOSE) $(COMPOSE_FULL) down -v
	$(COMPOSE) $(COMPOSE_FULL) up -d --build
	@echo "Waiting for memQL to accept connections..."
	@for i in 1 2 3 4 5 6 7 8 9 10 11 12; do \
		if nc -z localhost 50051 2>/dev/null; then \
			echo "memQL is up."; \
			exit 0; \
		fi; \
		sleep 1; \
	done; \
	echo "memQL did not come up within 12s; seed manually with: make secrets-seed"; exit 1
	$(GO) run ./scripts/secrets seed

## Single-command dev workflow: back up everything in the running
## memQL into ~/.memql/dev-secrets.yaml, nuke the database, restart
## the stack with the full identity flow, then re-seed secrets +
## variables from the yaml. The point: a freshly-rebuilt stack with
## the same config you had before. Used for testing.
##
## Steps in order:
##   1. Read MEMQL_MASTER_KEY from ~/.memql/dev-secrets.yaml. Errors
##      out with guidance if the yaml has no master key (operator
##      runs `make secrets-init` first).
##   2. Run `secrets-export`: pulls every active row from the running
##      memQL into the yaml so any value the dev `secret-set`'d
##      directly (without going through yaml) survives the purge.
##      No-ops gracefully if memQL isn't up yet.
##   3. `docker compose down -v` + rebuild + `up -d`. The full
##      identity stack (identity service + verifiers on every node)
##      comes up exactly as it does in production.
##   4. Wait for memQL gRPC to accept connections (handshake uses
##      the operator credential -- the master key from step 1 -- to
##      satisfy the verifier).
##   5. Run `secrets-seed`: pushes every entry in the yaml into the
##      fresh memQL, encrypting secrets under the master key. Same
##      operator credential authorizes the writes.
## Alias: most folks reach for `dev-refresh` first. Both names point
## at the same recipe so muscle memory wins either way.
dev-refresh: dev-fresh

## Quick status snapshot: docker daemon? memQL gRPC reachable? what
## containers are running? Useful when 'dev-refresh' didn't behave
## as expected. Implementation lives in scripts/dev/status.sh per
## the function-based-shell-script convention (CLAUDE.md).
dev-status:
	@bash scripts/dev/status.sh

## Single-command "fresh testing stack": back up running state,
## wipe the database, restart containers with the full identity
## flow, then re-seed from yaml. Implementation lives in
## scripts/dev/refresh.sh.
dev-fresh:
	@bash scripts/dev/refresh.sh

# ---------------------------------------------------------------------------
# Utility targets
# ---------------------------------------------------------------------------

.PHONY: clean version help

## Remove build artifacts
clean:
	rm -rf $(BIN_DIR)/ coverage.out

## Print the current version
version:
	@echo $(VERSION)

## Show all available targets
help:
	@echo "memQL Makefile — v$(VERSION)"
	@echo ""
	@echo "BUILD"
	@echo "  make              Build standalone server + healthcheck"
	@echo "  make build        Build standalone server"
	@echo "  make build-all    Build all binaries (server + all node types)"
	@echo "  make bff          Build BFF node binary"
	@echo "  make voice        Build voice node binary"
	@echo "  make cognition    Build cognition node binary"
	@echo "  make agent        Build agent node binary"
	@echo "  make planner      Build planner node binary"
	@echo ""
	@echo "RUN"
	@echo "  make run          Run standalone server locally"
	@echo "  make dev          Start full dev stack in Docker (foreground)"
	@echo "  make dev-bg       Start full dev stack in Docker (background)"
	@echo "  make dev-polyphon Start dev stack with Polyphon voice pipeline"
	@echo "  make dev-nemoclaw Start dev stack with NemoClaw coding agent"
	@echo "  make dev-cluster               Start cluster mode (bff + cognition + planner)"
	@echo "  make dev-cluster-restart       Restart cluster fresh (down + rebuild + up -d)"
	@echo "  make dev-cluster-restart-purge Restart cluster AND wipe the database (down -v)"
	@echo "  make dev-cluster-stop          Stop the cluster"
	@echo "  make dev-cluster-logs          Follow cluster logs"
	@echo "  make dev-stop            Stop all development services"
	@echo "  make dev-logs            Follow development service logs"
	@echo "  make dev-ps              Show running development services"
	@echo "  make dev-nginx-reload    Reload nginx LB so its DNS cache picks up recreated nodes"
	@echo "  make dev-rebuild-node NODE=agent    Rebuild + restart ONE node, then reload nginx"
	@echo "  make voice-trace         Tail end-to-end voice latency waterfall (per-stage ms)"
	@echo "  make voice-trace-now     Same but pre-loads last 5m of voice trace history"
	@echo "  make db           Connect to development database (psql)"
	@echo ""
	@echo "TEST"
	@echo "  make test         Run all tests"
	@echo "  make test-v       Run all tests (verbose)"
	@echo "  make test-cover   Run tests with coverage report"
	@echo "  make test-polyphon Run Polyphon/cognition tests only"
	@echo ""
	@echo "QUALITY"
	@echo "  make vet          Run go vet"
	@echo "  make fmt          Format all Go files"
	@echo "  make lint         Run fmt + vet"
	@echo "  make tidy         Tidy go.mod dependencies"
	@echo "  make generate     Run code generation (protobuf)"
	@echo ""
	@echo "DOCKER"
	@echo "  make docker            Build default Docker image"
	@echo "  make docker-bff        Build BFF image"
	@echo "  make docker-voice      Build voice image"
	@echo "  make docker-cognition  Build cognition image"
	@echo "  make docker-agent      Build agent image"
	@echo "  make docker-planner    Build planner image"
	@echo ""
	@echo "DEV SECRETS"
	@echo "  make secrets-init              Interactively build ~/.memql/dev-secrets.yaml"
	@echo "  make secrets-seed              Push yaml values into running memQL (needs MEMQL_MASTER_KEY)"
	@echo "  make secrets-list              Show manifest vs yaml state"
	@echo "  make secrets-edit              Open yaml in \$$EDITOR"
	@echo "  make secret-set NAME=.. VALUE=.. SCOPE=global|partition [KIND=..] [PARTITION=..]"
	@echo "  make secret-delete NAME=.. SCOPE=.."
	@echo "  make variable-set NAME=.. VALUE=.. SCOPE=.."
	@echo "  make variable-delete NAME=.. SCOPE=.."
	@echo "  make db-purge                  Wipe DB (no restore)"
	@echo "  make db-purge-and-reseed       Wipe DB + re-apply dev-secrets.yaml"
	@echo "  make dev-refresh               Backup -> wipe -> restart -> re-seed (single-command testing flow)"
	@echo "                                 (dev-fresh works as an alias)"
	@echo "  make dev-status                Quick snapshot: docker daemon, gRPC handshake, container list"
	@echo ""
	@echo "UTILITY"
	@echo "  make clean        Remove build artifacts"
	@echo "  make version      Print current version"
	@echo "  make help         Show this help"
