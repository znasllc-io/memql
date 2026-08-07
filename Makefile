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
# GOFLAGS is REASSIGNED here, not appended to, and that is load-bearing beyond
# verbosity: GNU make re-exports a variable it reassigns, so every recipe runs
# with exactly this value regardless of what the caller exported. That is what
# keeps `make arch-model` -- and therefore the reproducibility gate -- immune to
# a stray `GOFLAGS=-tags=...` in the environment, which changes the extracted
# model (20186 nodes / 108689 edges under -tags=voice, vs 20182 / 121609).
# Do not add `export` or change this to `+=` without re-checking that gate.
GOFLAGS     := -v
CGO_ENABLED := 0
BIN_DIR     := bin
VERSION     := $(shell cat VERSION 2>/dev/null || echo "dev")

# The module path, and the all-packages selector spelled in terms of it.
#
# ALL_PKGS is `$(MODULE)/...` rather than `./...` (memql#3165). The two select
# the same 183 packages today -- one module, no go.work -- and diverge the
# moment a second `go.mod` lands anywhere beneath the root: `./...` then means
# "the module rooted at the working directory", so every package that moved
# into the new module silently drops out of `make test`, `make vet` and
# `make fmt` with no diagnostic and exit 0. Naming the module keeps the
# selector honest, and makes a per-module lane an edit here rather than a
# scope loss nobody notices.
MODULE      := github.com/znasllc-io/memql
ALL_PKGS    := $(MODULE)/...

# ---------------------------------------------------------------------------
# Build targets
# ---------------------------------------------------------------------------

##@ Build
##> Staging/prod images are built on the GitHub build server, not here (see CLAUDE.md).
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

## Build voice node binary (CGO; needs libopus-dev / libopusfile-dev / libsoxr-dev).
## The Go voice-agent's LiveKit server-sdk-go + media-sdk pull a CGO
## libopus/opusfile/soxr dependency (see docs/voice/
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

##@ VS Code / LSP
##> `make vscode-install` builds+installs the extension for THIS host; then reload the editor window.
.PHONY: memql-lsp vscode-grammar vscode-package vscode-install
## Build the memql-lsp binary (offline VS Code language server)
memql-lsp:
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/memql-lsp ./cmd/memql-lsp

## Regenerate the VS Code TextMate grammar from dslspec (run on GrammarVersion bump)
vscode-grammar: memql-lsp
	$(BIN_DIR)/memql-lsp gen-grammar editors/vscode/syntaxes/memql.tmLanguage.json

## Package the VS Code extension into a .vsix (bundles the host-platform binary by default)
vscode-package:
	bash scripts/vscode/package.sh

## Build the extension AND (re)install it into local VS Code, then reload to pick
## up server changes. The refresh-my-editor loop after a local LSP/extension edit.
## Override the target editor with EDITOR_CMD (e.g. make vscode-install EDITOR_CMD=cursor).
vscode-install:
	bash scripts/vscode/install.sh $(if $(EDITOR_CMD),--editor-cmd=$(EDITOR_CMD),)

##@ Identity (auth web app)
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

##@ Tokens & keys
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

##@ Cluster tests & DB
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

##@ Local cluster (k3d + ArgoCD -- the blessed local topology, #2061)
##> This IS the GitOps deploy path (same manifests as staging); `make deploy` is break-glass only.
.PHONY: up up-refresh down secrets dev status scale

## Fresh k3d bring-up: cluster + ArgoCD + secrets + images, wait healthy (SERVERS=2 AGENTS=1 for multi-node)
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
## (see docs/public/operate/downstream-stacks.md). In the common case a
## product runs on the product-agnostic engine images and DELIVERS ITS
## DSL AT RUNTIME: a data-only bundle image mounted via the
## deploy/k8s/components/dsl-bundle init-container at MEMQL_DSL_PATH, with
## APP_NAME + OVERLAY_PATH + REPO_URL registering the product's own ArgoCD
## Application. The CARRIER_REPO + CARRIER_NODES [+ CARRIER_CONTEXT] hook
## (build a subset of node images from a product carrier repo's Dockerfile)
## is the LEGACY compile-time path, retiring with the platform consolidation
## (#2472) -- prefer the runtime bundle for new stacks.
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

## Tear down the local k3d cluster (PURGE=1 also removes the kubeconfig context)
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

## Inner-loop dev: rebuild image(s) + import into k3d + restart Deployment(s) (NODE=<type> for one node)
## No direct-apply bypass -- ArgoCD owns the manifests; only pods restart.
## Downstream product stacks run the product-agnostic engine images and
## deliver their DSL at runtime via the dsl-bundle component (MEMQL_DSL_PATH);
## the CARRIER_REPO + CARRIER_NODES hook (build a subset of node images from a
## carrier repo's Dockerfile) is the LEGACY compile-time path, retiring with
## #2472.
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

##@ Test & SDK
.PHONY: test test-v test-cover test-polyphon sdk-gen sdk-gen-check sdk-ts-install sdk-ts-typecheck dsl-lint viewkit-install viewkit-typecheck viewkit-test vscode-deps vscode-test

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

# ARCH_MODEL_OUT lets the drift gate regenerate to a temp file through THIS
# target, so the flag set exists exactly once in the repo. Declared above the
# doc block so it does not separate the '## ' summary from its target, which
# would drop arch-model out of `make help` (TestMakeHelpCompleteness).
ARCH_MODEL_OUT ?= component/architecture/embedded/topology.model.json

## Regenerate the checked-in architecture model
## (component/architecture/embedded/topology.model.json), which the cockpit's
## Topology tab consumes.
##
## THE FLAGS ARE LOAD-BEARING (memql#2844). --calls is not a default, and the
## artifact contains the call graph: 121k edges with it, 21k without. Before
## this target nothing recorded that, so `go run ./cmd/memql-arch` -- the
## documented command -- produced a file 100k edges smaller than the one in
## git, and the resulting 900k-line diff made refreshing the model impossible
## in practice. --reproducible blanks generated_at and the absolute workspace
## path so the output depends only on the code.
arch-model:
	$(GO) run ./cmd/memql-arch --root . --types --calls --cluster memql \
		--reproducible --out $(ARCH_MODEL_OUT)

## CI gate: regenerate the architecture model and diff against the checked-in
## copy. Fails if the code changed without the model being refreshed -- the
## drift that left ToggleComputerUseEnabledArgs.UserId in the model in 13
## places after #2840 removed it. Pair with `make arch-model` locally to fix.
##
## Also enforced by TestArchitectureModelIsNotStale so it runs in the ordinary
## `go test ./...` lane, which needs no workflow change.
arch-model-check:
	$(GO) test -count=1 -run TestArchitectureModelIsNotStale ./component/architecture/

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

## Install view-kit (@znasllc-io/memql-view-kit) dev dependencies. Idempotent.
viewkit-install:
	cd sdk/ts-viewkit && npm install --no-audit --no-fund

## Typecheck view-kit. Runs `tsc --noEmit` against sdk/ts-viewkit.
viewkit-typecheck:
	cd sdk/ts-viewkit && npm run typecheck

## Run the view-kit test suite via node:test. Zero runtime deps.
viewkit-test:
	cd sdk/ts-viewkit && npm test

## Build the workspace packages the extension depends on via `file:`. Their
## dist/ must exist before `npm ci` in editors/vscode can resolve them, so a
## clean checkout needs this first.
##
## sdk/ts uses `npm install`, not `npm ci`: its package-lock.json is not
## committed, and `npm ci` fails without one. This matches the existing
## sdk-ts-install target. sdk/ts-viewkit does commit its lockfile, so it gets
## the reproducible `npm ci`.
vscode-deps:
	cd sdk/ts && npm install --no-audit --no-fund && npm run build
	cd sdk/ts-viewkit && npm ci --no-audit --no-fund && npm run build

## Run the VS Code extension's unit tests. Covers only modules that do not
## import `vscode`; the API layer is exercised by packaging, not unit tests.
vscode-test: vscode-deps
	cd editors/vscode && npm ci --no-audit --no-fund && npm test

## Run all tests
test:
	$(GO) test $(ALL_PKGS)

## Run all tests with verbose output
test-v:
	$(GO) test -v $(ALL_PKGS)

## Run tests with coverage report
test-cover:
	$(GO) test -coverprofile=coverage.out $(ALL_PKGS)
	$(GO) tool cover -func=coverage.out
	@rm -f coverage.out

## Run Polyphon/cognition tests only
test-polyphon:
	$(GO) test ./component/polyphon/... ./integrations/cognition/...

# ---------------------------------------------------------------------------
# Code quality
# ---------------------------------------------------------------------------

##@ Quality & codegen
.PHONY: vet fmt lint tidy generate proto-gen proto-gen-check prs-stalled claims-stale arch-model arch-model-check

## Run go vet on all packages
vet:
	$(GO) vet $(ALL_PKGS)

## Format all Go files
fmt:
	$(GO) fmt $(ALL_PKGS)

## Run vet + fmt (quick lint)
lint: fmt vet

## Report PRs nobody is advancing: green + mergeable + un-enqueued (memql#2833),
## and closed-without-merging whose branch and issue are both still live
## (memql#2887). READ-ONLY: it never enqueues, because green is not reviewed.
## IDLE_MINUTES=15 tightens the idle threshold -- head-commit age, not last
## activity. CLOSED_MAX_AGE_MINUTES=43200 widens the closed-PR lookback from its
## 14-day default. REPO=owner/name retargets.
prs-stalled:
	bash scripts/dev/stalled-prs.sh

## Report claimed:* labels no live session is holding (memql#2834). READ-ONLY by
## default. Pass APPLY=1 to remove the label from CLOSED issues whose claim has
## also gone cold; claims on OPEN issues are always reported, never swept.
##
## $(filter ...), not a bare $(if $(APPLY),...): make's $(if) tests emptiness,
## not truth, so APPLY=0 / APPLY=no / APPLY=false would all have passed --apply
## and written to GitHub. On a target whose whole safety story is "mutation is
## opt-in", APPLY=0 meaning yes inverts the operator's obvious intent. Anything
## outside {1,true,yes,on} -- including APPLY=Y and APPLY=2 -- is read as false,
## which is the fail-safe direction.
claims-stale:
	bash scripts/dev/stale-claims.sh $(if $(filter 1 true yes on,$(APPLY)),--apply,)

## Tidy go.mod dependencies
tidy:
	$(GO) mod tidy

## Run code generation (protobuf, etc.)
generate:
	$(GO) generate $(ALL_PKGS)

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

##@ Release (engine image)
##> Staging is GitOps: bump the digest in deploy/k8s/overlays/staging + merge -> ArgoCD reconciles.
.PHONY: release

## Cut an immutable engine release image memql:<VERSION> from VERSION + the
## short git SHA (znasllc-io/memql#493, epic #491). The engine ships every node
## type product-agnostic; this image tag is the engine-version leg of a
## release's {engine version, bundle digest, client digest} triple, pinned in
## one deploy overlay (not a standalone backend-version file). The X.Y.Z tag is
## write-once: pushing over an existing tag is refused without --allow-overwrite.
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

##@ Dev tooling
.PHONY: install-deps genesis-seal env-registry-sync env-registry-check
.PHONY: setup-agents verify-agents

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

## Install the agent stack: the GitHub MCP server, the Superpowers plugin,
## and the CCPM skill. Idempotent -- every step probes current state first, so
## a repeat run changes nothing. Needs GITHUB_PAT in the environment or in a
## gitignored .env at the repo root. See docs/AGENTS.md.
##   make setup-agents            install what is missing
##   make setup-agents ARGS=--update   also refresh what is already installed
setup-agents:
	@bash scripts/setup-agents.sh $(ARGS)

## Read-only status check for the agent stack; exits non-zero if anything is
## missing. Installs nothing, so it is safe to run as a gate. Fix with
## 'make setup-agents'.
verify-agents:
	@bash scripts/verify-agents.sh

# ---------------------------------------------------------------------------
# Deploy gates (engine ops)
# ---------------------------------------------------------------------------

##@ Deploy gates (engine ops)
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

##@ Deploy (break-glass -- ArgoCD owns staging; use only when Argo is unavailable)
.PHONY: deploy

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

# ---------------------------------------------------------------------------
# Utility targets
# ---------------------------------------------------------------------------

##@ Utility
.PHONY: clean version help help-check

## Remove build artifacts
clean:
	rm -rf $(BIN_DIR)/ coverage.out

## Print the current version
version:
	@echo $(VERSION)

## Show all available targets (auto-generated from the '##' doc comments -- never drifts)
help:
	@bash scripts/make/help.sh

## Drift guard: fail if any target lacks a '## ' doc comment (also run by `make test`)
help-check:
	@bash scripts/make/help.sh --check && echo "OK: every Makefile target is documented"
