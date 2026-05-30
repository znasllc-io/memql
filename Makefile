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

## Build voice node binary. The Go voice-agent's LiveKit server-sdk-go +
## media-sdk pull a CGO libopus/opusfile/soxr dependency (see docs/voice/
## 451-livekit-go-room-participation.md, Caveat 1), so this target overrides
## the repo-wide CGO_ENABLED=0 default. Requires libopus-dev / libopusfile-dev
## / libsoxr-dev (apt) or opus-dev / opusfile-dev / soxr-dev (apk) installed.
voice:
	CGO_ENABLED=1 $(GO) build $(GOFLAGS) -tags voice -o $(BIN_DIR)/memql-voice .

## Build cognition node binary
cognition:
	$(GO) build $(GOFLAGS) -tags cognition -o $(BIN_DIR)/memql-cognition .

## Build agent node binary
agent:
	$(GO) build $(GOFLAGS) -tags agent -o $(BIN_DIR)/memql-agent .

## Build planner node binary
planner:
	$(GO) build $(GOFLAGS) -tags planner -o $(BIN_DIR)/memql-planner .

## Build workbench node binary
workbench:
	$(GO) build $(GOFLAGS) -tags workbench -o $(BIN_DIR)/memql-workbench .

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
# Realtime voice + video (Go voice-agent, integrations/voice/agent)
# The voice-agent is the `voice-agent` subcommand of the memql-voice
# binary (`memql-voice voice-agent`); build it with `make voice`. The
# Python voice-agent (LiveKit Agents 1.5) and the legacy Go Bridge Agent
# have both been retired.
# ---------------------------------------------------------------------------

.PHONY: voice-agent-token

## Mint a class="voice_agent" JWT for the running local cluster's
## voice-agent process. Execs the identity binary inside the
## memql-identity container so the mint runs against the same DB +
## Ed25519 key the live service uses, then prints the bearer to
## stdout. Used by scripts/dev/refresh.sh to inject
## VOICE_AGENT_TOKEN at bring-up. Override INSTANCE / TTL / OUT as
## needed; defaults match the dev compose setup. See
## docs/auth/voice-agent-jwt.md.
voice-agent-token:
	@docker exec memql-identity /app/memql voice-agent-token mint \
		--instance-id="$${INSTANCE:-voice-agent-local}" \
		$${TTL:+--ttl=$$TTL} \
		$${OUT:+--out=$$OUT}

## Mint a class="node" JWT for the given cluster node. Used by
## scripts/dev/refresh.sh + the bootstrap-tokens target below to
## seed MEMQL_NODE_TOKEN for every cluster-node binary in the dev
## docker stack (without it the receiving NodeService interceptor
## rejects with `authorization header missing` and inter-node
## peer calls fail silently). memql#338.
##
## Override NODE / TYPE / TTL / OUT as needed; bff-local defaults
## match the dev compose setup.
node-token:
	@docker exec memql-identity /app/memql node-token mint \
		--node-id="$${NODE:-bff-local}" \
		--node-type="$${TYPE:-bff}" \
		$${TTL:+--ttl=$$TTL} \
		$${OUT:+--out=$$OUT}

## Mint a class="node" JWT for every node-type the dev docker-compose
## stack runs (bff, voice, cognition, agent, planner) and write a
## `.env.local.node-tokens` snippet the compose env_file consumes via
## `${MEMQL_<TYPE>_NODE_TOKEN}` indirection. Idempotent -- each call
## mints fresh tokens (existing rows are not revoked; just supersede).
##
## Run this after `docker compose up` brings the identity service
## healthy, then `docker compose up -d --force-recreate` the cluster
## nodes so they pick up the new env. `make dev-refresh` runs the
## same flow automatically via scripts/dev/refresh.sh. memql#338.
dev-node-tokens-bootstrap:
	@bash scripts/dev/mint-node-tokens.sh

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
## Tail voice-path log lines emitted by the Go voice-agent + memql
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

.PHONY: test test-v test-cover test-polyphon policies-lint policies-trace sdk-gen sdk-gen-check sdk-ts-install sdk-ts-typecheck dsl-lint

## Regenerate the typed SDK surface from the DSL tree. Reads every
## query / mutation / logic under dsl/**/*.memql and emits typed
## methods on the Go SDK (sdk/go/client.QueryClient). The TS typed
## methods are no longer generated here -- the runtime core
## (@znasllc-io/memql-sdk-core, sdk/ts) is client-agnostic, and each
## product BFF generates its own typed surface from core + its DSL
## (see #171 / #172 / #43). Re-run after any DSL change that touches a
## construct's args / signature / shape. The drift gate
## (sdk-gen-check) catches stale checkouts in CI.
##
## Multi-root: the generator (and the importable sdk/gen package it
## wraps) accepts repeatable / comma-separated --dsl roots and merges
## constructs from all of them deterministically. memQL itself passes
## a single root (the core DSL). A product BFF -- a separate Go module
## that depends on github.com/znasllc-io/memql -- imports sdk/gen and
## calls gen.Generate over `core DSL ∪ its own DSL`, and wires its own
## drift gate by calling gen.Generate with Check=true over the same
## merged roots (a non-nil error means "regenerate and commit").
sdk-gen:
	$(GO) run ./scripts/sdk-gen --dsl=dsl --out=sdk/go/client --ts-out=

## CI gate: regenerate, then diff against the checked-in tree. Fails
## if the DSL evolved without the generator running. Pair with
## `make sdk-gen` locally to fix.
sdk-gen-check:
	$(GO) run ./scripts/sdk-gen --check --dsl=dsl --out=sdk/go/client --ts-out=

## DSL lint: load the embedded DSL tree through the same
## dslimports.Load pipeline the engine runs at boot and fail on any
## parse / import / build diagnostics. Mirrors the CI gate so authors
## can catch the issue locally before pushing. Pass a single .memql
## file path to scope the report to that file + its imported
## neighbors.
dsl-lint:
	$(GO) run ./cmd/memqllint dsl/

## Install runtime-core (@znasllc-io/memql-sdk-core) dev dependencies
## (typescript). Idempotent.
sdk-ts-install:
	cd sdk/ts && npm install --no-audit --no-fund

## Typecheck the runtime core. Runs `tsc --noEmit` against sdk/ts. CI
## gates the core SDK through this target. Requires node + npm.
sdk-ts-typecheck:
	cd sdk/ts && npm run typecheck

## Run the runtime-core test suite. Compiles src + test via the
## tsconfig.test.json overlay and drives node:test against the
## emitted JS. Zero new runtime deps -- uses Node's built-in
## test runner. Requires node + npm.
sdk-ts-test:
	cd sdk/ts && npm test

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
# Database wipe / dev refresh
# ---------------------------------------------------------------------------
# Authoring of env vars / secrets lives in `memql-cockpit genesis init`
# (writes ~/.memql/genesis.znas). dev-refresh decrypts that file, brings
# up the stack, and seeds manifest-listed entries into the running
# memQL cluster as concept rows.

.PHONY: db-purge dev-fresh dev-refresh dev-status install-deps genesis-seal

## Seal a plaintext .env into ~/.memql/genesis.znas (the encrypted
## envelope dev-refresh decrypts at cluster start). Headless equivalent
## of the cockpit's first-launch genesis wizard: parse + manifest-validate
## + encrypt under MEMQL_MASTER_KEY (reused from your environment when
## present, generated + printed on first use).
##   make genesis-seal ENV_FILE=~/Downloads/local.genesis.env
genesis-seal:
	@test -n "$(ENV_FILE)" || { echo "usage: make genesis-seal ENV_FILE=/path/to/local.genesis.env"; exit 1; }
	$(GO) run ./cmd/genesis-seal --env-file=$(ENV_FILE)

## Install + verify every build-time tool the dev workflow needs:
## protoc + protoc-gen-go + protoc-gen-go-grpc (auto-installed when
## missing) plus go / docker / mkcert (verified only -- printed
## install hint if missing). Idempotent. Run before 'make generate'
## or after a fresh clone. Wired into 'make dev-refresh' so first-
## time clones aren't surprised by a missing protoc.
install-deps:
	@bash scripts/dev/install-deps.sh

## Wipe ALL local memQL data (docker compose down -v + up -d). The
## next dev-refresh will re-seed from ~/.memql/genesis.znas.
db-purge:
	$(COMPOSE) $(COMPOSE_FULL) down -v
	$(COMPOSE) $(COMPOSE_FULL) up -d --build

## Single-command "fresh testing stack": decrypt the operator's
## ~/.memql/genesis.znas (requires MEMQL_MASTER_KEY in env), wipe the
## database, restart the cluster with the full identity flow, then
## re-seed manifest-listed entries into the cluster.
##
## Steps:
##   1. Verify MEMQL_MASTER_KEY + locate genesis.znas; decrypt to a
##      temp .env (mode 0600); export every KEY=VALUE so docker
##      compose's interpolation finds them.
##   2. Knowledge-cache export (so the wipe doesn't burn LLM-seeded
##      chunks).
##   3. docker compose down -v + rebuild + up -d.
##   4. Wait for memQL gRPC to accept connections.
##   5. `secrets seed --env-file <temp>` pushes manifest entries as
##      concept rows.
##   6. Restore the knowledge cache from step 2.
##
## Alias: dev-fresh is the same recipe; either name works.
dev-refresh: dev-fresh

## Quick status snapshot: docker daemon? memQL gRPC reachable? what
## containers are running? Useful when 'dev-refresh' didn't behave
## as expected. Implementation lives in scripts/dev/status.sh per
## the function-based-shell-script convention (CLAUDE.md).
dev-status:
	@bash scripts/dev/status.sh

## Single-command "fresh testing stack" -- see dev-refresh.
## Implementation lives in scripts/dev/refresh.sh.
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
	@echo "DEV REFRESH"
	@echo "  Authoring of env vars / secrets is in memql-cockpit (see"
	@echo "  'memql-cockpit genesis init'). The targets below operate on"
	@echo "  the cluster + the decrypted genesis."
	@echo "  make install-deps              Install + verify build tools (protoc, plugins, go, docker, mkcert)"
	@echo "  make db-purge                  Wipe DB (no restore)"
	@echo "  make dev-refresh               Verify deps -> decrypt genesis -> wipe -> restart -> seed"
	@echo "                                 (dev-fresh works as an alias)"
	@echo "  make dev-status                Quick snapshot: docker daemon, gRPC handshake, container list"
	@echo ""
	@echo "UTILITY"
	@echo "  make clean        Remove build artifacts"
	@echo "  make version      Print current version"
	@echo "  make help         Show this help"
