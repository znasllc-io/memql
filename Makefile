# memQL Makefile
# Source of truth for all build, test, run, and development commands.
#
# Usage:
#   make                   Build all binaries
#   make help              Show all available targets
#   make up                Start the staging-parity dev cluster (k3d + ArgoCD)
#   make test              Run all tests

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

GO          := go
GOFLAGS     := -v
CGO_ENABLED := 0
BIN_DIR     := bin
VERSION     := $(shell cat VERSION 2>/dev/null || echo "dev")

# ---------------------------------------------------------------------------
# Build targets
# ---------------------------------------------------------------------------

.PHONY: all build build-all bff voice cognition agent planner workbench mcp identity identity-templ identity-tailwind identity-assets identity-build healthcheck

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

## Build mcp node binary (Model Context Protocol server, epic memql#1529)
mcp:
	$(GO) build $(GOFLAGS) -tags mcp -o $(BIN_DIR)/memql-mcp .

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

## Mint a class="voice_agent" JWT for the running local k3d cluster's
## voice-agent process. Execs the identity binary inside the identity
## pod so the mint runs against the same DB + Ed25519 key the live
## service uses, then prints the bearer to stdout. Used to inject
## VOICE_AGENT_TOKEN at bring-up. Override INSTANCE / TTL / OUT /
## NAMESPACE as needed. See docs/auth/voice-agent-jwt.md.
voice-agent-token:
	@kubectl exec -n "$${NAMESPACE:-memql}" deploy/identity -- /app/memql voice-agent-token mint \
		--instance-id="$${INSTANCE:-voice-agent-local}" \
		$${TTL:+--ttl=$$TTL} \
		$${OUT:+--out=$$OUT}

## Mint a class="node" JWT for the given cluster node. Seeds
## MEMQL_NODE_TOKEN for a cluster-node binary (without it the receiving
## NodeService interceptor rejects with `authorization header missing`
## and inter-node peer calls fail silently). memql#338.
##
## Execs the identity binary inside the identity pod of the running k3d
## cluster. Override NODE / TYPE / TTL / OUT / NAMESPACE as needed.
node-token:
	@kubectl exec -n "$${NAMESPACE:-memql}" deploy/identity -- /app/memql node-token mint \
		--node-id="$${NODE:-bff-local}" \
		--node-type="$${TYPE:-bff}" \
		$${TTL:+--ttl=$$TTL} \
		$${OUT:+--out=$$OUT}

.PHONY: identity-signing-key

## Generate a fresh base64 Ed25519 signing seed for IDENTITY_SIGNING_KEY_B64
## (znasllc-io/memql#550). Seal the printed value into the genesis envelope
## so every identity replica derives the same key + JWKS (enables identity
## HA / multi-replica without an RWO key PVC). Rotate by re-generating,
## re-sealing, and rolling the deployment.
##   make identity-signing-key
identity-signing-key:
	@head -c 32 /dev/urandom | base64

# ---------------------------------------------------------------------------
# Run targets
# ---------------------------------------------------------------------------

.PHONY: cluster-e2e db

## Cross-replica delivery gate (memql#1261): boot the 2-replica k3d +
## ArgoCD cluster (deploy/k8s/overlays/local, scaled to 2 via scale.sh)
## and run test/clustere2e. Green once the Phase-1 durable backbone
## lands. Pass a known user JWT via MEMQL_E2E_TOKEN, or let the harness
## seed one via the identity OAuth flow. See scripts/test/cluster-e2e.sh
## (header) -- the k3d gate is correct-by-construction but unvalidated in
## CI (#2088); it needs the owner secret + a real run.
cluster-e2e:
	bash scripts/test/cluster-e2e.sh

## Connect to the development database (after `make up`, via the k3d
## postgres port-forward on :5432).
db:
	psql postgres://memql:memql_dev@localhost:5432/memql

# ---------------------------------------------------------------------------
# k3d / ArgoCD local cluster (Argo-parity, E0 -- #2061)
# ---------------------------------------------------------------------------
# Pure-Argo inner loop: images built locally, imported into k3d, ArgoCD
# reconciles the local overlay. No direct-apply bypass ever.
#
# Prerequisites: docker, k3d, kubectl (brew install k3d kubectl)
#
# Quick start:
#   make up          # fresh bring-up: cluster + ArgoCD + secrets + images, wait healthy
#   make up-refresh  # clean slate: nuke + repave (fresh DB), then the same bring-up
#   make down        # tear down cluster
#   make secrets     # (re-)seed k8s Secrets in a running cluster
#
# Optional env overrides:
#   MEMQL_K3D_CLUSTER          cluster name (default: memql)
#   MEMQL_K3D_TARGET_REVISION  ArgoCD git revision (default: current branch)
#   MEMQL_K3D_SERVERS          k3d server count (default: 1)
#   MEMQL_K3D_AGENTS           k3d agent count (default: 0)

.PHONY: up up-refresh down secrets dev status scale

## Fresh bring-up of the local k3d cluster, end to end: create cluster,
## install ArgoCD (pinned v2.13.3, same as staging), apply the memql-local
## Application, seed k8s Secrets, build + import the engine images, and wait
## for the mesh to become Available. Idempotent -- safe on an existing cluster.
##
## This is the GitOps path: the local engine "deploy" IS the ArgoCD sync of the
## local overlay (the primary deploy path, same as staging). It is NOT the
## cockpit break-glass path -- `make deploy` is the imperative cockpit-delegated
## path (I16, #2227). `make up` stays a cluster-bootstrap launcher so the local
## inner loop never depends on the owner-gated cockpit deploy.
## Downstream product stacks reuse this target with their own overrides
## (see docs/public/operate/downstream-stacks.md): CARRIER_REPO +
## CARRIER_NODES [+ CARRIER_CONTEXT] build a subset of node images from the
## product repo's Dockerfile; APP_NAME + OVERLAY_PATH + REPO_URL register
## the product's own ArgoCD Application.
##   make up                       # current branch as targetRevision
##   make up REVISION=main         # pin to main
##   make up SERVERS=2 AGENTS=1   # multi-node (see E0.5 / #2067)
up:
	@bash scripts/k3d/bringup.sh \
		$${CLUSTER:+--cluster=$${CLUSTER}} \
		$${NAMESPACE:+--namespace=$${NAMESPACE}} \
		$${REVISION:+--revision=$${REVISION}} \
		$${SERVERS:+--servers=$${SERVERS}} \
		$${AGENTS:+--agents=$${AGENTS}} \
		$${REPO_URL:+--repo-url="$${REPO_URL}"} \
		$${APP_NAME:+--app-name="$${APP_NAME}"} \
		$${OVERLAY_PATH:+--overlay-path="$${OVERLAY_PATH}"} \
		$${EXTRA_PORTS:+--extra-ports="$${EXTRA_PORTS}"} \
		$${APP_PROJECT:+--app-project="$${APP_PROJECT}"} \
		$${PROJECT_MANIFEST:+--project-manifest="$${PROJECT_MANIFEST}"} \
		$${CARRIER_REPO:+--carrier-repo="$${CARRIER_REPO}"} \
		$${CARRIER_NODES:+--carrier-nodes="$${CARRIER_NODES}"} \
		$${CARRIER_CONTEXT:+--carrier-context="$${CARRIER_CONTEXT}"} \
		$${NO_SECRETS:+--no-secrets}

## Clean-slate local environment: nuke + repave. Tears down the cluster
## (wiping the in-cluster DB by construction), then runs the same bring-up
## as 'make up' (cluster + ArgoCD + secrets + images, wait healthy).
## Idempotent. Honors the same overrides as 'make up'.
##   make up-refresh                      # full nuke + rebuild
##   make up-refresh SERVERS=2 AGENTS=1   # multi-node clean slate
up-refresh:
	@bash scripts/k3d/bringup.sh --clean \
		$${CLUSTER:+--cluster=$${CLUSTER}} \
		$${NAMESPACE:+--namespace=$${NAMESPACE}} \
		$${REVISION:+--revision=$${REVISION}} \
		$${SERVERS:+--servers=$${SERVERS}} \
		$${AGENTS:+--agents=$${AGENTS}} \
		$${REPO_URL:+--repo-url="$${REPO_URL}"} \
		$${APP_NAME:+--app-name="$${APP_NAME}"} \
		$${OVERLAY_PATH:+--overlay-path="$${OVERLAY_PATH}"} \
		$${EXTRA_PORTS:+--extra-ports="$${EXTRA_PORTS}"} \
		$${APP_PROJECT:+--app-project="$${APP_PROJECT}"} \
		$${PROJECT_MANIFEST:+--project-manifest="$${PROJECT_MANIFEST}"} \
		$${CARRIER_REPO:+--carrier-repo="$${CARRIER_REPO}"} \
		$${CARRIER_NODES:+--carrier-nodes="$${CARRIER_NODES}"} \
		$${CARRIER_CONTEXT:+--carrier-context="$${CARRIER_CONTEXT}"}

## Tear down the local k3d cluster.
## Pass PURGE=1 to also remove the kubeconfig context.
down:
	@bash scripts/k3d/down.sh \
		$${CLUSTER:+--cluster=$${CLUSTER}} \
		$${PURGE:+--purge}

## (Re-)seed the k8s Secrets in a running k3d cluster. Safe to re-run.
## Required after 'make up NO_SECRETS=1' or if secrets drift.
secrets:
	@bash scripts/k3d/seed-secrets.sh \
		$${CLUSTER:+--namespace=$${NAMESPACE:-memql}}

## Inner-loop dev: rebuild image(s), import into k3d, restart Deployment(s).
## No direct-apply bypass -- ArgoCD owns the manifests; only pods restart.
## Downstream product stacks pass CARRIER_REPO + CARRIER_NODES to build a
## subset of node images from the product repo's Dockerfile instead.
##   make dev                          # rebuild + restart all app node types
##   make dev NODE=cognition           # one node type
##   make dev NODE=mcp,cognition       # comma-separated list
##   make dev PULL_INFRA=1            # pull + re-import infra images
dev:
	@bash scripts/k3d/dev.sh \
		$${NODE:+--node=$${NODE}} \
		$${PULL_INFRA:+--pull-infra} \
		$${CLUSTER:+--cluster=$${CLUSTER}} \
		$${NAMESPACE:+--namespace=$${NAMESPACE}} \
		$${CARRIER_REPO:+--carrier-repo="$${CARRIER_REPO}"} \
		$${CARRIER_NODES:+--carrier-nodes="$${CARRIER_NODES}"} \
		$${CARRIER_CONTEXT:+--carrier-context="$${CARRIER_CONTEXT}"} \
		$${NO_WAIT:+--no-wait}

## Print k3d cluster status + mesh litmus (unique MEMQL_NODE_ID per pod).
## Use after scaling to verify cross-node mesh formation.
status:
	@bash scripts/k3d/status.sh \
		$${CLUSTER:+--cluster=$${CLUSTER}} \
		$${NAMESPACE:+--namespace=$${NAMESPACE}} \
		$${APP_NAME:+--app-name="$${APP_NAME}"}

## Scale all app Deployments to N replicas. Use N=2 for cross-node mesh
## testing (E0.5); N=1 to reset to single-node default.
## ArgoCD ignoreDifferences excludes /spec/replicas so selfHeal won't revert.
##   make scale N=2   # multi-node (2 replicas per Deployment)
##   make scale N=1   # single-node (default)
scale:
	@bash scripts/k3d/scale.sh \
		--replicas=$${N:?usage: make scale N=<replicas>} \
		$${CLUSTER:+--cluster=$${CLUSTER}} \
		$${NAMESPACE:+--namespace=$${NAMESPACE}}

# ---------------------------------------------------------------------------
# Test targets
# ---------------------------------------------------------------------------

.PHONY: test test-v test-cover test-polyphon sdk-gen sdk-gen-check sdk-ts-install sdk-ts-typecheck dsl-lint

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

.PHONY: vet fmt lint tidy generate proto-gen proto-gen-check

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

## Regenerate the pinned-toolchain proto bindings (component/grpc + node).
## The fix command for `proto-gen-check`. component/bus is excluded (it needs
## a consumer-touching toolchain normalization first -- see memql#928).
proto-gen:
	@bash scripts/dev/proto-gen.sh

## CI gate: regenerate the pinned proto bindings, then diff against the
## checked-in tree (ignoring the cosmetic protoc version stamp). Fails if a
## .proto evolved without the generator running. Mirrors `sdk-gen-check`.
proto-gen-check:
	@bash scripts/dev/proto-gen.sh --check

# ---------------------------------------------------------------------------
# Docker image targets
# ---------------------------------------------------------------------------

.PHONY: release publish-releases

## Cut an immutable release image memql:<VERSION> from VERSION + the short
## git SHA (znasllc-io/memql#493, epic #491). memQL is the upstream module;
## the single number CoPresent pins (deploy/backend-version, copresent#140)
## is this image tag, not a go.mod require. The X.Y.Z tag is write-once:
## pushing over an existing tag is refused without --allow-overwrite.
## Implementation lives in scripts/release/release.sh per the function-based
## shell-script convention (CLAUDE.md).
##   make release                                   # local image, VERSION semver prefix
##   make release VERSION=2.4.0                      # explicit version, local only
##   make release VERSION=2.4.0 ACR=acrmemql PUSH=1  # build + push to shared ACR
##   make release VERSION=2.4.0 ACR=acrmemql PUSH=1 DRY_RUN=1   # plan only
release:
	@bash scripts/release/release.sh \
		$${VERSION:+--version=$$VERSION} \
		$${REGISTRY:+--registry=$$REGISTRY} \
		$${ACR:+--acr=$$ACR} \
		$${PUSH:+--push} \
		$${ALLOW_OVERWRITE:+--allow-overwrite} \
		$${DRY_RUN:+--dry-run} \
		$(ARGS)

## Publish GitHub Releases for stack versions from the release lockfiles
## (znasllc-io/memql#1097). Idempotent -- existing Releases are skipped.
## The memql Releases are also published automatically by CI on push
## (.github/workflows/publish-releases.yml); the component Releases
## (bff-copresent, copresent) need `az` + a cross-org token, so run this
## from the primary checkout after a release.
##   make publish-releases                       # memql + components (all)
##   make publish-releases REPO=memql            # memql only
##   make publish-releases REPO=components        # bff-copresent + copresent
##   make publish-releases DRY_RUN=1              # plan only
publish-releases:
	@bash scripts/release/publish-releases.sh \
		$${REPO:+--repo=$$REPO} \
		$${VERSION:+--version=$$VERSION} \
		$${DRY_RUN:+--dry-run} \
		$(ARGS)

# The per-node `docker-*` targets (docker / docker-bff / docker-voice /
# docker-cognition / docker-agent / docker-planner) were removed in #2205:
# immutable release images come from `make release` (and the GitHub build
# server for staging/prod), while the local inner loop is `make dev` (build +
# k3d import). A hand-built `docker build` image fed neither path.

# ---------------------------------------------------------------------------
# Genesis envelope / dev tooling
# ---------------------------------------------------------------------------
# Authoring of env vars / secrets lives in `memql-cockpit genesis init`
# (writes ~/.memql/genesis.znas). `make up` seeds the decrypted envelope
# into the k3d cluster's k8s Secrets via scripts/k3d/seed-secrets.sh.

.PHONY: install-deps genesis-seal env-registry-sync env-registry-check

## Regenerate the embedded genesis manifest snapshot
## (component/genesis/manifest.yaml) from the authored registry
## (scripts/secrets/manifest.yaml). Run after editing the authored file;
## TestEmbeddedManifestInSync fails CI otherwise. (Epic 7 / memql#2104)
env-registry-sync:
	@bash scripts/secrets/sync-embedded-manifest.sh

## Drift-check the env-var registry both ways: a var read in code but not
## registered, or a registry entry that appears nowhere in the repo, fails.
## The shared classifier behind the CI gate (memql#2105).
env-registry-check:
	$(GO) run ./cmd/envscan -check

## Seal a plaintext .env into ~/.memql/genesis.znas (the encrypted
## envelope scripts/k3d/seed-secrets.sh decrypts into k8s Secrets at
## `make up`). Headless equivalent of the cockpit's first-launch genesis
## wizard: parse + manifest-validate + encrypt under MEMQL_MASTER_KEY
## (reused from your environment when present, generated + printed on
## first use).
##   make genesis-seal ENV_FILE=~/Downloads/local.genesis.env
genesis-seal:
	@test -n "$(ENV_FILE)" || { echo "usage: make genesis-seal ENV_FILE=/path/to/local.genesis.env"; exit 1; }
	$(GO) run ./cmd/genesis-seal --env-file=$(ENV_FILE)

## Install + verify every build-time tool the dev workflow needs:
## protoc + protoc-gen-go + protoc-gen-go-grpc (auto-installed when
## missing) plus go / docker / k3d / kubectl (verified only -- printed
## install hint if missing). Idempotent. Run before 'make generate'
## or after a fresh clone so 'make up' isn't surprised by a missing tool.
install-deps:
	@bash scripts/dev/install-deps.sh

# ---------------------------------------------------------------------------
# Azure deployment (staging foundation -- epic #491)
# ---------------------------------------------------------------------------
# Idempotent bootstrap that installs/verifies + authenticates the
# toolchain (az + containerapp ext, gh, tiger, docker, jq, psql) and
# creates/converges the core Azure resources (resource group, shared
# Basic ACR, per-env Key Vault, Container Apps environment) plus loads
# secrets into the Key Vault. Re-runnable -- the second consecutive run
# is a no-op. Implementation lives in .claude/scripts/deploy-setup.sh
# per the function-based-shell-script convention (CLAUDE.md). Per #492,
# this target lives under .claude/scripts/ rather than scripts/ because
# it's the deploy-tier bootstrap defined by the Skills+Scripts
# architecture, not a dev-loop helper.

.PHONY: deploy-setup

## Bootstrap the Azure deployment foundation for ENV (staging|production,
## default staging). Idempotent: installs/verifies the toolchain and
## creates-or-converges the resource group, shared Basic ACR, per-env
## Key Vault, and Container Apps environment, then loads secrets into the
## Key Vault. Pass DRY_RUN=1 to print the plan without mutating, or
## ARGS=... to forward extra flags (e.g. --secrets-file=...). Run with
## `--help` semantics via ARGS=--help.
##   make deploy-setup                          # staging
##   make deploy-setup DRY_RUN=1                # staging, plan only
##   make deploy-setup ENV=production DRY_RUN=1
##   make deploy-setup ARGS=--secrets-file=~/.memql/deploy.staging.env
deploy-setup:
	@bash .claude/scripts/deploy-setup.sh \
		--env=$${ENV:-staging} \
		$${DRY_RUN:+--dry-run} \
		$(ARGS)

.PHONY: conn-headroom-check

## Connection-headroom deploy gate (memql#1820, from the #1817 53300 spike):
## check whether the fleet's projected DB connections fit the instance budget.
## Override the budget via env: MAX_CONNECTIONS, RESERVED_CONNECTIONS,
## MAX_OPEN_CONNS. Exits non-zero when the peak would exceed budget.
##   make conn-headroom-check
##   MAX_CONNECTIONS=300 make conn-headroom-check
conn-headroom-check:
	@bash scripts/deploy/conn-headroom-check.sh

# ===========================================================================
# PROVISION -- one-time Azure infrastructure bootstrap (idempotent)
# ===========================================================================

.PHONY: db-provision blob-provision livekit-provision

## Provision (create-or-verify) a dedicated Azure Storage account + blob
## container for ENV and print the connection string for inclusion in the
## genesis envelope (znasllc-io/memql#807, epic #805). Idempotent: re-running
## detects the existing account and container and prints the current conn string.
## Implementation lives in scripts/deploy/blob-provision.sh per the
## function-based shell-script convention (CLAUDE.md).
## NOTE: requires `az login`. Prints MEMQL_AZURE_STORAGE_CONNECTION_STRING for
## manual placement into ~/Downloads/<env>.genesis.env (not wired to Key Vault
## directly -- see docs/deploy/blob-provision.md for the full runbook).
##   make blob-provision                          # staging
##   make blob-provision DRY_RUN=1                # staging, plan only
##   make blob-provision ENV=production DRY_RUN=1
blob-provision:
	@bash scripts/deploy/blob-provision.sh \
		--env=$${ENV:-staging} \
		$${DRY_RUN:+--dry-run} \
		$(ARGS)

## Provision the managed Tiger Cloud DB (Timescale Community + pgvector)
## for ENV in Azure East US and wire its DSN into the per-env Key Vault
## (znasllc-io/memql#494, epic #491). Idempotent: re-running detects the
## existing service and only rewrites the DSN secret when it rotated.
## Implementation lives in scripts/deploy/tiger-provision.sh per the
## function-based shell-script convention (CLAUDE.md).
##   make db-provision                          # staging
##   make db-provision DRY_RUN=1                # staging, plan only
##   make db-provision ENV=production DRY_RUN=1
db-provision:
	@bash scripts/deploy/tiger-provision.sh \
		--env=$${ENV:-staging} \
		$${DRY_RUN:+--dry-run} \
		$(ARGS)

## Provision the self-hosted LiveKit shared-secret pair into the per-env Key
## Vault (znasllc-io/memql#1043). The committed ExternalSecret then syncs the
## livekit-secrets k8s Secret. Idempotent: re-runs REUSE the existing pair;
## --rotate generates a fresh one. Implementation lives in
## scripts/deploy/livekit-provision.sh per the function-based shell convention.
## NOTE: requires `az login`. DNS (livekit.<env>.copresent.ai) is a manual
## registrar step -- see docs/deploy/livekit-provision.md.
##   make livekit-provision                       # staging
##   make livekit-provision DRY_RUN=1             # staging, plan only
##   make livekit-provision ARGS=--rotate         # rotate the key/secret
##   make livekit-provision ENV=production DRY_RUN=1
livekit-provision:
	@bash scripts/deploy/livekit-provision.sh \
		--env=$${ENV:-staging} \
		$${DRY_RUN:+--dry-run} \
		$(ARGS)

# ===========================================================================
# BREAK-GLASS -- imperative deploy. ArgoCD OWNS local + staging (deploys =
# merges; selfHeal reverts out-of-band kubectl applies), so these targets are
# the escape hatch for when Argo is unavailable, plus the prod path until prod
# is on ArgoCD (#2207).
#
# THE BLESSED STAGING DEPLOY IS A GIT MERGE, NOT `make deploy`:
#   bump the image digest in deploy/k8s/overlays/staging + merge to main
#   -> ArgoCD reconciles. Rollback = `git revert` the overlay. See
#   docs/public/operate/deployment-strategy.md.
# ===========================================================================

.PHONY: deploy deploy-rollback

## BREAK-GLASS imperative deploy -- DELEGATES TO THE COCKPIT (I16, epic
## znasllc-io/memql#2212/#2227). `make` is a thin launcher now: it shells into
## `memql-cockpit deploy`, which embeds the engine automation runtime, loads the
## deployment bundle, and runs the PINNED `deployEngineCluster` automation
## (role-gated + audited + version-pinned) from OUTSIDE the target cluster. The
## env `switch` lives in that ONE automation -- `make` only forwards ENV.
## See DEVOPS_DSL_BUNDLE_HANDOFF.md "Execution model".
##
## NOT the normal staging path -- staging is GitOps (digest bump in
## overlays/staging + merge -> ArgoCD syncs). Use this break-glass path only
## when ArgoCD is unavailable, or for prod (ENV=production) until prod is on
## ArgoCD (#2207).
##
## OWNER-GATED (honest, not a silent failure): the cockpit's in-process engine
## carries no database yet, so DB-backed deployment steps cannot complete until
## the I13 runner surface + a live engine DB are wired (memql#2220/#2228). A real
## deploy reports `BLOCKED (owner-gated): ...` cleanly; a DRY_RUN is a clean
## no-op resolve. The cockpit binary is resolved by scripts/deploy/cockpit.sh
## (COCKPIT_BIN > PATH > built from the sibling ../memql-cockpit via `make
## cockpit`). There is NO fallback to the old script path.
##
## Forwarded knobs: ENV->--env, VERSION->--ref, DRY_RUN->--dry-run, plus the
## role gate (ROLE->--role or MEMQL_COCKPIT_ROLE; deny-by-default) and ACTOR->
## --actor for the audit trail. ARGS passes extra cockpit flags through.
##   make deploy ENV=production VERSION=0.9.6 ROLE=developer  # prod imperative roll-out
##   make deploy ENV=development VERSION=0.9.6 DRY_RUN=1 ROLE=developer  # resolve only, no changes
##   make deploy ENV=staging COCKPIT_BIN=/path/to/memql-cockpit  # pin the binary
deploy:
	@COCKPIT_BIN="$${COCKPIT_BIN:-}" bash scripts/deploy/cockpit.sh deploy \
		--env=$${ENV:-staging} \
		$${VERSION:+--ref=$$VERSION} \
		$${ROLE:+--role=$$ROLE} \
		$${ACTOR:+--actor=$$ACTOR} \
		$${DRY_RUN:+--dry-run} \
		$${APPLY:+--apply} \
		$(ARGS)

## Roll the memQL mesh back to a previous good release (deployment-v2 Phase 1,
## znasllc-io/memql#699). Rollback is a GIT REVERT of the digest-pinned overlay
## deploy/k8s/overlays/<env>, then reconcile -- NOT `kubectl rollout undo` (which
## reverted to the manifest tag, #684). Does NOT touch the managed Tiger Cloud
## DB. Impl in scripts/deploy/aks-rollback.sh.
##   make deploy-rollback ARGS=--list             # recent overlay changes
##   make deploy-rollback ARGS=--to=<commit>      # print git revert + reconcile
##   make deploy-rollback ARGS="--to=<commit> --apply"  # also re-converge
##   make deploy-rollback DRY_RUN=1               # plan only
deploy-rollback:
	@bash scripts/deploy/aks-rollback.sh \
		--env=$${ENV:-staging} \
		$${DRY_RUN:+--dry-run} \
		$(ARGS)

.PHONY: deploy-aks

## Apply the AKS Kubernetes manifests for the memQL node mesh
## (deploy/k8s/) to the current kubectl context (epic
## znasllc-io/memql#522 -- pivot from ACA). Applies the namespace then
## kustomize-applies the 7 node Deployments + Services. Idempotent. The
## `memql-secrets` Secret is a one-time prerequisite (real values, created
## out-of-band -- see deploy/k8s/secret.example.yaml); this target warns if
## it is absent. NO database is deployed -- nodes use managed Tiger Cloud.
## Impl lives in scripts/deploy/aks-apply.sh per the function-based
## shell-script convention (CLAUDE.md).
##   make deploy-aks                    # ENV=staging, apply to current context
##   make deploy-aks ENV=staging DRY_RUN=1   # server-side dry-run, no changes
deploy-aks:
	@bash scripts/deploy/aks-apply.sh \
		--env=$${ENV:-staging} \
		$${DRY_RUN:+--dry-run} \
		$(ARGS)

.PHONY: deploy-autoscaler

## Converge the AKS staging nodepool to the cluster-autoscaler sizing codified
## for #614 (DEPLOYMENT_STRATEGY.md §9): min 2 / max 5 on nodepool1 (B2s) so a
## rolling-update surge gets headroom automatically and scales back after.
## Idempotent. OWNER-GATED: enabling the autoscaler on shared cluster infra is
## a persistent cost decision -- run with DRY_RUN=1 to print the plan; drop it
## only to apply the live change. Impl in scripts/deploy/aks-autoscaler.sh.
##   make deploy-autoscaler DRY_RUN=1     # print the plan, change nothing
##   make deploy-autoscaler ARGS=--show   # read current autoscaler state
##   make deploy-autoscaler               # APPLY the codified sizing (owner-gated)
deploy-autoscaler:
	@bash scripts/deploy/aks-autoscaler.sh \
		--env=$${ENV:-staging} \
		$${DRY_RUN:+--dry-run} \
		$(ARGS)

# ===========================================================================
# OPERATOR RUNBOOKS -- staging release / reset / smoke (idempotent, fail-loud)
# ===========================================================================

.PHONY: release-staging reset-staging

## ONE idempotent operator command to take staging to a VALIDATED release, or to
## RECOVER a release that went green-on-drift but is actually broken
## (znasllc-io/memql#1524, epic #1518). Full mode pre-flights the shared identity
## signing seed (#1515), then delegates to aks-deploy.sh -- build + push, apply
## the digest-pinned overlay identity-first, drift-assert, PROMOTE the bff
## Rollout (#1520), run the FUNCTIONAL post-deploy gate (#1519: bff promoted +
## JWKS coherent + BFF->agent auth), smoke the front door, record validated.
## VERIFY=1 is the 2026-06-16 RECOVERY as one command: skip build/apply and run
## ONLY the bff promote + functional gate + smoke against what is already live
## (heals a stuck/unpromoted, JWKS-incoherent release). Re-runnable + fail-loud.
## Impl in scripts/deploy/staging-release.sh.
##   make release-staging VERSION=0.9.61                 # full: build -> apply -> promote -> gate -> smoke
##   make release-staging VERSION=0.9.61 DRY_RUN=1       # full plan, no changes
##   make release-staging VERIFY=1                       # RECOVER a stuck/false-green release (promote+gate+smoke)
##   make release-staging SKIP_BUILD=1 VERSION=0.9.61    # roll already-pushed tags, then gate
release-staging:
	@bash scripts/deploy/staging-release.sh \
		--env=$${ENV:-staging} \
		$${VERSION:+--version=$$VERSION} \
		$${VERIFY:+--verify} \
		$${SKIP_BUILD:+--skip-build} \
		$${NO_SMOKE:+--no-smoke} \
		$${NO_GATE:+--no-gate} \
		$${DRY_RUN:+--dry-run} \
		$(ARGS)

## ONE idempotent operator command to RESET staging to a fresh, FULLY-USABLE
## state (znasllc-io/memql#1524): the auth-coherent DB wipe (#1500/#1522) plus a
## post-reset FUNCTIONAL verification (#1519) so a "fresh start" comes up with
## login working, JWKS coherent, and the mesh reconnected -- never the
## half-broken auth state that needed a manual reseal + mesh roll on 2026-06-16.
## DESTRUCTIVE + staging-only + context-guarded; the underlying wipe asks you to
## type 'reset staging' unless ARGS=--yes. Impl in scripts/deploy/staging-reset.sh.
##   make reset-staging DRY_RUN=1        # preview the reset + verify plan
##   make reset-staging                  # wipe staging, then verify usable
##   make reset-staging ARGS=--yes       # wipe non-interactively, then verify
reset-staging:
	@bash scripts/deploy/staging-reset.sh \
		--env=$${ENV:-staging} \
		$${DRY_RUN:+--dry-run} \
		$(ARGS)

.PHONY: staging-db-reset

## DESTRUCTIVE, MANUAL staging DB reset (znasllc-io/memql#1500), AUTH-COHERENT
## (znasllc-io/memql#1522): wipe the staging database back to a fresh EMPTY
## schema, for when many app iterations have left stale data behind and you want
## to start clean. NEVER part of a deploy -- it runs ONLY when you invoke it and
## confirm. Staging-only (refuses prod) + a kube-context guard + an interactive
## typed confirmation; the wipe runs as a one-shot in-cluster Job and the schema
## is rebuilt by the existing `memql migrate` Job. Auth stays coherent (#1522):
## it pre-flights the shared identity signing seed (#1515) and REFUSES if it is
## absent, brings identity up BEFORE the mesh so node tokens re-mint cleanly
## (#1521), and verifies JWKS is served afterward -- no manual reseal / mesh roll.
## The owner re-registers via magic-link afterward. Impl in
## scripts/deploy/staging-db-reset.sh.
##   make staging-db-reset DRY_RUN=1     # preview the plan, change nothing
##   make staging-db-reset               # wipe staging (asks you to type 'reset staging')
##   make staging-db-reset ARGS=--yes    # skip the prompt (still env/context guarded)
staging-db-reset:
	@bash scripts/deploy/staging-db-reset.sh \
		--env=$${ENV:-staging} \
		$${DRY_RUN:+--dry-run} \
		$(ARGS)

.PHONY: smoke-staging

## Repeatable end-to-end smoke test against the LIVE staging front door
## (znasllc-io/memql#535): TLS+DNS, identity health + JWKS (direct and via
## the app same-origin proxy), the magic-link login surface, the BFF
## /memql/ws upgrade, and the /memql/audio voice route. Baseline is
## read-only (no email, no auth). Opt-in DEEP checks: SMOKE_EMAIL issues a
## real magic link; MEMQL_SMOKE_TOKEN runs an authenticated query + the
## cross-node AI forward. Impl in scripts/deploy/staging-smoke-test.sh.
##   make smoke-staging                                  # baseline
##   make smoke-staging APP_HOST=app.copresent.ai        # smoke prod
##   make smoke-staging SMOKE_EMAIL=me@example.com        # + magic link
##   make smoke-staging MEMQL_SMOKE_TOKEN=mql_pat_xxx     # + live query
smoke-staging:
	@bash scripts/deploy/staging-smoke-test.sh $(ARGS)

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
	@echo "LOCAL (k3d + ArgoCD cluster -- the blessed local topology, #2061)"
	@echo "  make up                        Fresh bring-up: k3d + ArgoCD + secrets + images, wait healthy (single-node default)"
	@echo "  make up SERVERS=2 AGENTS=1     Multi-node cluster (for cross-node mesh testing)"
	@echo "  make dev [NODE=<type>]         Inner loop: rebuild image -> k3d import -> rollout restart"
	@echo "  make up-refresh                Clean slate: nuke + repave (fresh DB + ArgoCD), then the same bring-up"
	@echo "  make status                    Show per-pod MEMQL_NODE_IDs (mesh parity litmus) + ArgoCD sync"
	@echo "  make scale N=2                 Scale every app Deployment to N replicas (2 = staging parity)"
	@echo "  make secrets                   (Re-)seed the k8s Secrets in a running cluster"
	@echo "  make down [PURGE=1]            Tear down the k3d cluster (PURGE=1 also drops the kubeconfig context)"
	@echo "  make db                        Connect to development database (psql, via the postgres port-forward)"
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
	@echo "RELEASE"
	@echo "  make release                            Cut immutable memql:<VERSION> image (VERSION + short SHA)"
	@echo "  make release VERSION=X.Y.Z ACR=acrmemql PUSH=1   Build + push the pinnable release tag to the shared ACR"
	@echo "  (Staging/prod images are built on the GitHub build server, not locally -- see CLAUDE.md.)"
	@echo ""
	@echo "DEV TOOLING"
	@echo "  Authoring of env vars / secrets is in memql-cockpit (see"
	@echo "  'memql-cockpit genesis init'). 'make up' seeds the decrypted"
	@echo "  genesis envelope into the k3d cluster's k8s Secrets."
	@echo "  make install-deps              Install + verify build tools (protoc, plugins, go, docker, k3d, kubectl)"
	@echo "  make genesis-seal ENV_FILE=... Seal a plaintext .env into ~/.memql/genesis.znas"
	@echo ""
	@echo "PROVISION (one-time Azure infra bootstrap, idempotent; ENV=staging|production)"
	@echo "  make deploy-setup [DRY_RUN=1]  Azure + toolchain bootstrap (resource group, ACR, Key Vault, ACA env)"
	@echo "  make db-provision [DRY_RUN=1]  Provision Tiger Cloud DB + wire DSN to Key Vault"
	@echo "  make blob-provision [DRY_RUN=1] Provision Azure Storage account + container; print conn string (#807)"
	@echo "  make livekit-provision [DRY_RUN=1] Provision self-hosted LiveKit secret pair into Key Vault"
	@echo ""
	@echo "OPERATOR RUNBOOKS (staging)"
	@echo "  make release-staging VERSION=X.Y.Z  Build -> apply digest-pinned overlay -> promote -> functional gate -> smoke"
	@echo "  make release-staging VERIFY=1       Recover a stuck/false-green release (promote + gate + smoke only)"
	@echo "  make reset-staging                  Wipe staging to a fresh, auth-coherent, verified-usable state"
	@echo "  make staging-db-reset               Destructive manual staging DB wipe (typed confirmation)"
	@echo "  make smoke-staging                  End-to-end smoke test against the live staging front door"
	@echo ""
	@echo "DEPLOY -- the normal way:"
	@echo "  STAGING is GitOps: bump the image digest in deploy/k8s/overlays/staging and"
	@echo "  merge to main -> ArgoCD reconciles. Rollback = 'git revert' the overlay."
	@echo "  (release-staging above wraps build+digest-pin+promote+gate for you.)"
	@echo ""
	@echo "BREAK-GLASS (imperative deploy -- ArgoCD owns staging; use only when Argo is unavailable, or for prod until #2207)"
	@echo "  make deploy ENV=production VERSION=X.Y.Z  Imperative prod roll-out (build + push + apply + smoke)"
	@echo "  make deploy [DRY_RUN=1]                    Imperative apply (escape hatch; NOT the staging norm)"
	@echo "  make deploy-aks [DRY_RUN=1]                Apply-only manifests primitive (no build)"
	@echo "  make deploy-rollback ARGS=--list           git-revert-based overlay rollback"
	@echo "  make deploy-autoscaler [DRY_RUN=1]         Converge AKS nodepool autoscaler sizing (owner-gated)"
	@echo ""
	@echo "UTILITY"
	@echo "  make clean        Remove build artifacts"
	@echo "  make version      Print current version"
	@echo "  make help         Show this help"
