# MemQL - the AI memory platform

**Type:** AI platform -- agents and automations on a time-series memory graph
**Language:** Go + MemQL DSL
**Stack:** PostgreSQL + TimescaleDB extension
**Purpose:** Run agents and automations against a time-series memory graph

> **Positioning is load-bearing, not marketing** (memql#3843). MemQL is an AI
> platform *built on* a time-series memory graph; it is not a database, and no
> public-facing file may say it is -- the embedded TimescaleDB Community
> Edition's TSL grant is withheld from a product that is "primarily [a]
> database storage or operations" product. Describing the storage layer
> precisely is fine ("backed by / built on a time-series memory graph");
> claiming MemQL *is* a database is not. `TestNoDatabaseProductClaims`
> (`database_positioning_test.go`) fails the build on the latter. Compliance
> pack: [docs/internal/ops/timescaledb-license-compliance.md](docs/internal/ops/timescaledb-license-compliance.md).

---

## Quick Start

**Prerequisites:** docker, k3d, kubectl (`brew install k3d kubectl`).

```bash
# --- k3d + ArgoCD: the ONLY supported local run path ---
make up                      # fresh bring-up: cluster + ArgoCD + secrets + images, wait healthy
make up SERVERS=2 AGENTS=1   # multi-node (cross-node mesh testing)
make dev                     # inner-loop: rebuild images -> import -> restart pods
make dev NODE=bff            # ...one node only (faster)
make dev PULL_INFRA=1        # ...also refresh infra images (postgres/azurite)
make scale N=2               # 2 replicas per Deployment (NAMESPACE= overrides `memql`)
make status                  # mesh litmus: unique MEMQL_NODE_ID per pod + one shared identity keyset
make secrets                 # re-seed secrets (idempotent; use after a cluster recreate)
make up-refresh              # clean slate: nuke + repave (fresh DB), then bring up
make down                    # tear down (PURGE=1 also removes the kubeconfig context)

# Tests -- see Testing below. A bare `go test ./...` does NOT reach the engine.
make test

# Build. BFF is the default (no tag needed); the other node types are under
# Distributed Node Architecture below.
go build -o bin/memql .

# Database shell (after `make up`)
psql postgres://memql:memql_dev@localhost:5432/memql

# Front door: regenerate after changing a role or adding an HTTP route
make frontdoor                                            # hosts, then paths
make frontdoor-hosts-check   # gates: fail when a generated front door
make frontdoor-paths-check   # or path block is stale

# Logs
kubectl logs -n memql deploy/<node> -f
```

---

## Project Structure

MemQL has **exactly three** extension words. Do not invent a fourth.
See [Component vs integration vs pack](docs/public/concepts/component-integration-pack.md).

- **component** — engine internals (`component/`: DSL lexer/AST, HTTP servers, bus, identity)
- **integration** — talk to other DBs/services (`integrations/`). Shopify and email live here
- **pack** — client-agnostic product feature (Plugin SDK v1 / `examples/referencepack`). Intake "plugin" means this

`dsl/todos`, `dsl/calendar`, `dsl/campaigns` are **core**. Packs cannot shadow them.
`memql.RegisterPlugin` is the Go registration primitive. It is not a fourth runtime.

**A pack's build tag decides whether it ships.** Images are built with
`BUILD_TAGS=<node type>` and nothing else, so a pack gated on its OWN NAME
(`referencepack`, `shopifypack`) reaches nothing, and one gated on a NODE TYPE
ships to that node (`examples/deploypack` is `//go:build identity`). A
**storefront pack** (`packs/`) links into every binary with no tag and is
governed by `v1:platform:packState`, whose absence means the pack's DECLARED
default (`dsl.RegisterPackDefault`; a storefront pack declares FALSE, and a
row always wins). **A shopper reaches a pack only through what it DECLARES**
(`component/memql/shopper_surface.go`): named forms and reads, on a deployable
whose `shopperForms` is on, under the site owner's borrowed authority, every
row stamped with the `storeId` of the binding it arrived through.
[building-a-pack.md](docs/public/build/building-a-pack.md).

```
MemQL/
├── app/               Phased service bootstrap (Go): Build() + one file per
│                      phase (config -> database -> engine -> integrations ->
│                      transport -> cluster)
├── dsl/               MemQL DSL tree, one dir per namespace, one file per
│                      construct kind; dsl/_reference/ holds authoring skeletons
├── integrations/      External services + DSL-callable capabilities (Go)
├── brand/             Visual identity as CSS custom properties, imported by
│                      BOTH clients/os and component/identity/web, never
│                      copied (brand_shared_source_test.go, memql#4266)
├── clients/os/        MemQL OS -- desktop shell + graphical ops console,
│                      served by component/edge as a site row
├── packs/             Storefront packs in the DEFAULT BUILD (epic memql#5532),
│                      mounted on every node by app/anchor_storefront_packs.go
│                      and shipped DISABLED. examples/ keeps the tag-gated packs
├── component/         Core Go components (memql, grpc, events, database,
│                      server, auth, edge, language, procedure, bus, node,
│                      architecture, observe, envregistry, ...)
├── core/              Shared utilities (logger, env, id) + dslfs (the
│                      MEMQL_DSL_PATH on-disk override / embedded FS picker)
├── cmd/               CLI tools (healthcheck, memqlfmt, memqlmigrate,
│                      memqllint, frontdoorhosts, frontdoorpaths, ...)
├── deploy/k8s/        GitOps manifests: base + components + per-env overlays
├── scripts/           Shell scripts + lib/capability.sh (capability runtime)
├── docs/              Documentation
├── docker/            Dockerfile + db init + nginx assets
└── .claude/           Claude Code project state. The repo is PUBLIC: skills/,
                       commands/, agents/, settings.json are tracked; the rest
                       is ignored (memql#3344)
```

---

## Key Directories

Several directories carry their own CLAUDE.md, and it is the first thing to
read before editing that tree. A directory without one is normal.

| Directory | Purpose | CLAUDE.md |
|-----------|---------|-----------|
| `dsl/<ns>/*.memql` | Automations, queries, mutations, specs, tools, prompts, shapes per namespace | — |
| `dsl/providers/providers.memql` | AI provider configurations | — |
| `dsl/policies/policies.memql` | AI provider-selection chains (paid-last when a paid fallback exists) | — |
| `dsl/rules/rules.memql` | The rules that map a call's metadata to a policy (all `@locked`) | — |
| `integrations/` | External service integrations + DSL capabilities (Go) | [→](integrations/CLAUDE.md) |
| `clients/` | Surfaces built ON the platform (SPAs, landing pages, apps) | [→](clients/README.md) |
| `clients/os/` | MemQL OS, served at `os.<domain>`. **Read its README before adding an app or a live surface**: the live-collection contract (a collection does nothing until `retain()`), which concepts are broadcast, and the arrival-cue rule (a heartbeat is not news) | [→](clients/os/README.md) |
| `component/` | Core service components (Go) | [→](component/CLAUDE.md) |
| `component/language/` | The MemQL front end: lexer, parser, rewriter, AST, compiler, registries | [→](component/language/CLAUDE.md) |
| `component/node/` | Distributed node system (bootstrap, peers, mesh) | [→](component/node/CLAUDE.md) |
| `component/architecture/` | Auto-generated architecture model | [→](component/architecture/CLAUDE.md) |
| `component/observe/` | Per-invocation observability runtime | [→](component/observe/CLAUDE.md) |
| `sdk/go/` | Go SDK -- the public client surface | [→](sdk/go/CLAUDE.md) |
| `docs/` | Documentation | [→](docs/CLAUDE.md) |

**OS attention:** when adding or meaningfully changing a user-facing capability,
consider whether people need to discover it. Declare a stable
`attentionChanges` ID and revision on that app's manifest when they do, and
wire and test a reachable acknowledgment destination. Refactors, fixes,
styling and rebuilds do not warrant a marker. Read
[Unseen changes](clients/os/README.md#unseen-changes-shared-attention-markers)
first. Ancestor navigation must never acknowledge an unseen child;
user/revision receipts are shared infrastructure, not a per-app local-storage
flag.

---

## Documentation

**Start here:** [docs/public/overview/quickstart.md](docs/public/overview/quickstart.md).
**Full index:** [GLOSSARY.md](GLOSSARY.md).

- [Component vs integration vs pack](docs/public/concepts/component-integration-pack.md) -- the three words; intake "plugin" means pack
- [Architecture](docs/public/concepts/architecture.md) · [Events](docs/public/concepts/events.md) · [Tech stack](docs/public/overview/tech-stack.md)
- [MemQL Language](docs/public/language/memql.md) · [Functions](docs/public/language/functions.md)
- [MemQL Authoring Rules & Gotchas](docs/public/language/authoring-rules.md) -- read before writing `.memql` files
- **Removing a field from a concept BRICKS its stored rows** (memql#5199,
  memql#5209): schemas are `additionalProperties: false` and a mutation's
  read-merge validates the MERGED payload, so a stored key the concept no
  longer declares makes the row unwritable on its NEXT WRITE. CI cannot see it
  (`db-tests` runs on a fresh database). The same is true of dropping an enum
  value and of adding a `@required` field to a concept with rows.
  `make concept-snapshot` REFUSES to drop a field from
  `component/conceptfields/concept-fields.snapshot.json` until the `retired`
  ledger records a repairing migration or a waiver. Write the migration
  SCOPED BY CONCEPT, never by key name
- [Node Identifier Conventions](docs/public/concepts/identifiers.md) -- canonical `{concept}:{shortId}` internally vs BARE ids at every wire seam (the engine bare-ifies on egress and resolves bare args inbound; clients never compose, parse or compare canonical ids)
- [`core/num`](core/num/num.go) -- the ONE narrowing from a decoded payload number to a Go `int`, in three NAMED answers (saturate / zero / caller-default). A bare `int(x)` on a `float64` is implementation-defined out of range; `TestEveryPayloadNarrowingCarriesAnAnswer` fails the build on a narrowing that declares no answer (memql#4779)
- [LLM cost control](docs/public/ai/llm-cost-control.md) -- read before touching `ai_guard.go`, an LLM loop, or an automation that drives model calls
- [Tool ↔ Knowledge Domain Pattern](docs/public/concepts/tool-knowledge-domain-pattern.md) -- operational knowledge goes in a knowledge domain the tool requires, not in the agent prompt template
- [Environment variables](docs/public/operate/env-vars.md) · [Auto-generated architecture diagrams](docs/internal/design/auto-generated-diagrams.md)

**Tooling:** **MemQL Cockpit** -- the fleet worker runtime + cluster CLI,
installed as the `memql` command on operator machines; its own repo is
`github.com/znasllc-io/memql-cockpit`. This repo's `bin/memql` is the engine
binary, which ships only inside container images; the two never share a PATH,
so do not "fix" the name collision by renaming either.

---

## Development Workflow

### Development Environment (k3d + ArgoCD)

The k3d + ArgoCD cluster is the local dev topology (memql#2061 / E0) and the
ONLY supported local run path. It mirrors the cloud cluster (AKS + ArgoCD + the
k8s base in `deploy/k8s/`), so the same manifests and reconciliation path run
locally and in the cloud. Multi-node is the default (#2067): use
`make up SERVERS=2` + `make scale N=2` for full cross-node mesh testing.
Commands are in Quick Start above; the full k3d runbook and port-forward
reference is
[docs/public/operate/reproduce-the-cloud-locally.md](docs/public/operate/reproduce-the-cloud-locally.md).

Migrations run automatically on startup.

### Testing

```bash
make test                      # the whole tree
MEMQL_REQUIRE_DB=1 make test   # ...and make a missing database a FAILURE, not a skip
```

**Do NOT verify with `go test ./...`. It does not run the engine** (memql#4032).
This is a multi-module workspace -- `go.work` lists 51 modules -- and a relative
pattern resolves inside whichever module owns the directory it is rooted at.
Measured:

| Command | Packages | `component/memql`? |
|---|---:|---|
| `go test ./...` | 64 | **no** |
| `go test ./component/...` | 3 | **no** |
| `make test` (`go test github.com/znasllc-io/memql/...`) | 208 | yes |

So `./...` misses `component/memql`, `component/database` and `component/language`
-- the engine, the executor, the row-authz gates and the DSL loader. `make test`
names the MODULE PATH instead, which is prefix-matched across every workspace
module. The failure mode is silent and confidence-INCREASING: the bare
command prints `ok` across 64 packages and never touched the engine.
`TestDocumentedTestCommandCoversTheEngine` (`claude_md_test_command_test.go`)
fails the build if this section ever documents a command that misses it.

**A second, independent way to get a meaningless green: db-gated tests skip.**
Every Postgres-backed case self-skips when it cannot reach a database, and
`MEMQL_REQUIRE_DB=1` is what turns that skip into a failure
(`component/database/dbtest`). Two traps:

- **An open port 5432 is not evidence of a database.** With the k3d cluster up,
  `k3d-memql-serverlb` publishes 5432, so the connection is accepted and then
  EOFs -- it fails silently as "unreachable" and every db-gated case skips.
- The default DSN is `postgres://memql:memql_dev@localhost:5432/memql`. Point
  `MEMQL_DATABASE_DSN` at a real Postgres+TimescaleDB+pgvector, or run the
  db-gated trees the way CI does.

The trees carrying most of the engine's real coverage are owned by the
`db-tests` lane rather than by `make test`, and the set changes -- ask the
script rather than trusting a count written down here:

```bash
# what CI actually runs (the canonical set lives in the script)
scripts/ci/db-gated-packages.sh --trees
MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=... go test -count=1 ./component/memql/...
```

### Image builds: LOCAL Docker for dev, BUILD SERVER for deploys (HARD RULE)

Where a container image is built depends ONLY on where it runs:

- **Local development** -- build images in your **local Docker** and import
  them into k3d via `make dev`. Never pushed to ACR.
- **Deploys to the CLOUD** -- images MUST be built on the **GitHub build
  server** (GitHub Actions, OIDC -> ACR `acrmemql`), never on an operator
  machine: `memql` -> `.github/workflows/build-engine-images.yml` builds every
  node type as one set of product-agnostic engine images; the product's
  DSL-bundle repo builds a data-only bundle image mounted via `MEMQL_DSL_PATH`;
  the product client repo builds its SPA image. Each is `workflow_dispatch` on
  `main` with a `version` input; tags are immutable.

Do NOT hand-build + push release images locally (`az acr build`,
`make release`, `docker push`) for a cloud deploy. A release is
`{engine version, bundle digest, client digest}` pinned in **one overlay**
(`deploy/k8s/overlays/<env>`): build the engine images, pin the three
digests, merge -> ArgoCD reconciles.
[deploy-bundle-runbook.md](docs/public/operate/deploy-bundle-runbook.md).

---

## Branch Workflow

MemQL uses a single long-lived branch: `main`.

1. **Every change goes through a branch + PR; `main` refuses direct pushes**
   (a repository ruleset: `pull_request`, `required_status_checks`,
   `merge_queue`, `deletion`, `non_fast_forward`). Branch, push, open the PR,
   let CI go green, then **enqueue it**:

   ```bash
   gh pr merge <n> --repo znasllc-io/memql   # bare: no strategy, no --delete-branch
   ```

   `--delete-branch` is REFUSED (the queue deletes the branch itself) and
   `--merge` is ignored (the queue's `merge_method` is already `MERGE`). The
   command ENQUEUES: the PR sits `OPEN` with `mergedAt: null` for minutes under
   the 5-minute `ALLGREEN` batch window, and re-running answers `is already
   queued to merge`, which is confirmation. A queued PR that goes `DIRTY` stays
   there: rebase on `origin/main` and force-push. **The strict up-to-date
   policy is OFF by decision** (memql#5481; recorded, with the command that
   verifies it, in [ruleset-baseline.md](docs/internal/ops/ruleset-baseline.md)):
   the queue builds and tests the exact tree that lands, so a sibling merge no
   longer forces `update-branch`. **The owner bypass below skips the queue**,
   so for it the base still matters: `merge-as-owner.sh` MEASURES the drift
   (`compare/<base>...<head>`, printed by `--check` as a `base` line) and
   refuses on a non-zero count or an unreadable comparison; forcing with
   `--admin` lands a tree CI never tested against the current base. It measures
   rather than reading `mergeStateStatus`, which names only the STRONGEST
   blocker: every PR here is `BLOCKED` on a code-owner review its author may
   not give, and that hides any staleness.
2. **Merging your own PR: the owner uses the BYPASS, never a settings change.**
   GitHub never lets a PR's author approve it, so the requirement stays on and
   the owner proceeds through the bypass (`require_code_owner_review: true` +
   `bypass_actors: RepositoryRole(admin), pull_request`):

   ```bash
   scripts/dev/merge-as-owner.sh --pr=<n> --check   # policy + readiness, merges nothing
   scripts/dev/merge-as-owner.sh --pr=<n>           # merge
   ```

   The script refuses on a red or pending check the RULESET REQUIRES
   (`ci-required`, an `if: always()` aggregate), and on a required check that
   has NOT REPORTED: `ci-required` gets no check run until every lane it
   `needs:` finishes, so for most of a CI run it is absent, which is neither
   red nor pending (memql#5709 merged through that). Every required context
   must be present with a SUCCESS. The rest are reported as
   `failed (not required)` (memql#5016). `install-cluster-e2e` is not required
   and can be red on pristine `main`, but it DOES test the branch, so a red
   there is a log to read. (CodeQL's `Analyze` jobs left the pull-request path
   in memql#5482 and now analyse every push to main.) If the required-check
   list cannot be read it refuses on everything. Do not "fix" this by lowering
   `require_code_owner_review`.
3. **Pre-release -- no backwards-compat shims or deprecation windows, EXCEPT in
   the DSL.** For Go seams, wire contracts, gRPC messages and env vars: fix
   MemQL and the consumer at once and delete what is no longer needed. The
   MemQL language is the carve-out (memql#5385, memql#5390), because a `.memql`
   bundle mounted at `MEMQL_DSL_PATH` lives in a repo this one cannot see: a
   public language form leaves through a **deprecation window** -- it keeps
   loading, every load WARNS naming the replacement and the release it stops
   loading at, every use is COUNTED on `memql_dsl_deprecated_uses_total{rule}`,
   and it refuses only after at least two minor releases. The forms in a window
   are published in [memql.md](docs/public/language/memql.md#forms-in-a-deprecation-window)
   and held to `component/language/deprecation` by a build gate. Retiring a
   form outright is still right where nothing outside this repo could have
   written it.
4. **Stage files by explicit path** (`git add <file>`) -- never `git add -A` or
   `git add .`. The repo owner runs multiple Claude sessions against this
   working tree, and another session's untracked files must not get swept in.

**What triggers a frontend team ping:** a change that alters a wire contract the
frontend depends on (removed/renamed endpoints, changed required request
fields, new required response fields, new gRPC message types). Call it out in
the commit body. Backend-internal refactors that leave the wire identical need
no coordination.

---

## Architecture & Tech Stack

### Core Technologies
- **Language:** Go 1.26.1+
- **Database:** PostgreSQL 16 + TimescaleDB
- **API:** gRPC (`MemqlService.Stream` is the primary surface) + HTTP for the documented exceptions (OAuth, health, file uploads) + WebSocket bridge to the gRPC stream for browsers (`/memql/ws`)
- **AI:** Centralized provider system (OpenAI, Anthropic). All AI ops on gRPC.
- **Auth:** in-house identity service (magic-link + JWT, JWKS-published)
- **Query Language:** MemQL DSL

**Full tech stack details:** [docs/public/overview/tech-stack.md](docs/public/overview/tech-stack.md)

### Deploy targets

MemQL ships **one installation shape** (epic memql#3943). There is no
staging-versus-production dimension inside the product: an operator who wants a
second environment installs a second instance, with its own domain and its own
ArgoCD. What varies is the deploy TARGET, which carries its own field
(`provider`):

| Target | Database | Service | Provider |
|--------|----------|---------|----------|
| **Local** | CloudNativePG in k3d | k3d + ArgoCD (`make up`) | `docker-local` |
| **Cloud** | Self-hosted CloudNativePG on AKS | Azure Kubernetes Service | `azure` |

**Key Principle:** the local cluster is completely isolated from any cloud
install's database -- they are separate installations, not environments of one.

Development happens on macOS and Linux (amd64/arm64); CI runs on
`ubuntu-latest`, and `scripts/dev/install-deps.sh`, `scripts/dev/proto-gen.sh`
and `scripts/identity/build-css.sh` all branch on `darwin`/`linux`.

### System Architecture

```
Front door (TLS 443) -> bff gRPC :50051 (h2c)
                     -> bff-http :8085 (the documented HTTP exceptions)
                     -> agent gRPC :50051 for WorkerService only (the cockpit's stream)
   |
   MemQL Engine  <-  Automations System  <-  Functions System
   |
   +-- AI Provider Registry (OpenAI, Anthropic)
   |     +-- AI gRPC messages on MemqlService.Stream:
   |         AiChatMsg, AiSpeechMsg, AiTranscribeMsg, AiSuggestMsg
   +-- Integrations (agent, workbench, library, ...)
   +-- MemQL Sense (Tokenize, Complete, Diagnose, Hover, SignatureHelp)
   |
   PostgreSQL + TimescaleDB
   (time-series memory nodes; PK: (id, createdAt))
```

### Distributed Node Architecture (Cluster Mode)

MemQL uses **Go build tags** to compile separate binaries for each node type.
A tag selects which `app/build_*.go` runs, and therefore which integrations and
transport layers a node WIRES UP. **Tags are a wiring mechanism, not a size
mechanism**: every node binary is within ~5.5% of every other one
([build-tags.md](docs/public/build/build-tags.md#binary-size)).

```bash
go build .                       # bff        (default)
go build -tags agent .           # agent
go build -tags planner .         # planner
go build -tags edge .            # edge       (serves hosted sites, the OS shell among them)
```

The seven node types are identity, bff, agent, planner, workbench, mcp, edge.
**BFF** is the backend for frontend; **Agent** runs task execution, AI work,
tool calling and streaming transcription; **Planner** plans and orchestrates;
**Edge** serves this cluster's hosted web surfaces (every hosted SPA and MemQL
OS itself, a site row with no special path) by resolving the request `Host`
to a `v1:platform:site` row (epic memql#3700). Nodes discover each other via
mesh and share one PostgreSQL + TimescaleDB database; inter-node communication
uses the `NodeService` gRPC bidirectional stream, and events bridge across
nodes with dedup and TTL.

**RETIRING a node type is the dangerous edit, not adding one** (memql#5057).
The set is spelled out in seven places: `app/build_<type>.go`,
`app/build_default.go`'s deny-list, `component/node/compiled_<type>.go` and
its deny-list (`compiled_default.go`), `ENGINE_NODE_TYPES` in
`scripts/lib/engine_build_args.sh` (from which `dev.sh`'s `VALID_NODES`
DERIVES), the `build-engine-images.yml` release matrix, and the `memql-<type>`
image references under `deploy/k8s/`. Missing one on the way OUT is silent,
because `build_default.go` is a DENY list: `go build -tags voice .` after
deleting `app/build_voice.go` produces a BFF under the retired name. Gates:
`scripts/ci/node_type_lists_test.go`, `engine_build_args_for_node` refusing an
unbuilt type, and the Dockerfile refusing a `BUILD_TAGS` with no
`app/build_<type>.go`. **`BUILD_TAGS=""` stays legal; it is the bff.**
`component/node/compiled_<type>.go` decides what a binary CALLS ITSELF
(`node.CompiledNodeType()`, memql#5115): the build tag wins, a disagreeing
`MEMQL_NODE_TYPE` is warned and ignored, and the env var selects the type for
UNTAGGED builds only.

#### Multi-node is the DEFAULT -- design, implement, AND test for cross-node

The cluster (2 replicas per mesh node) is the runtime in the cloud and locally,
and every feature must be designed and tested against it. Never reason about a
feature as if it runs in a single process. The blessed local repro is
`make up SERVERS=2` + `make scale N=2`.

**When implementing:** state that crosses a node boundary needs EXPLICIT
plumbing; it does NOT travel implicitly.
- Session / in-memory state (caches, waiters, per-stream fields) lives on
  exactly ONE node, and a node handling a **proxied / forwarded** request
  (`AiForwardRouter`, `proxySI`, `NodeService` forwards) does NOT see it --
  thread it through the message or metadata.
- A read-modify-write of one row across replicas needs a Postgres lock around
  the read and the write, a read that skips this node's result cache
  (`memql.ContextWithFreshRead`), and a version stamped after the one it read
  -- each alone was measured insufficient (memql#5431).
- Every cross-node event-bus pub/sub needs a **routing rule**
  (`node.RegisterRoutingRule`) or it silently dies in cluster mode.
- Before calling a feature done, ask: *which node holds this state, and which
  node needs it?*

**When testing:** a green single-node unit test is a FALSE signal for
cross-node behaviour. Tests MUST exercise the hop -- a handler on a session
WITHOUT the originating node's local state, context surviving a proxy/forward,
an event consumed on a different replica -- in `test/clustere2e/` and/or
`component/grpc/ai_forward_test.go`, failing against single-node-assuming code
(#1448, #1412, #1388). memql#4352 closed the WORKER half (`WorkerForward*`)
with an IN-PROCESS hop test (`integrations/agent/worker/forward_hop_test.go`),
because a live-cluster gate skipped on every CI lane cannot be what stands
between a feature and its bug.

#### Node image source: product-agnostic engine images + runtime DSL delivery (#2472)

**The engine is the whole platform.** Every node type ships as a
**product-agnostic engine image** from THIS repo's Dockerfile
(`BUILD_TAGS=<type>`); there are no per-product node images, and reusable
capabilities live in the engine as generic, DSL-configurable features.

**Product DSL is delivered at runtime, not compiled in.** A product ships its
DSL as a tiny data-only **bundle image**; the `dsl-bundle` kustomize component
runs it as an init-container that copies the `.memql` tree into a shared volume
the node reads at `MEMQL_DSL_PATH`. `dsl-bundle` and `dsl-packages` each add
ONE init container; **`dsl-mount` owns the volume, the mount and
`MEMQL_DSL_PATH`, applied exactly once** (memql#4933), and the `components:`
order is the init-container order. Two traps a render shows and a diff does
not: the `memql/product-dsl` label must be applied in a layer the components
CONSUME (a `labels:` block beside them selects nothing), and it must name only
the mesh nodes (`redis` has no `volumes:` key). Working shapes:
`deploy/k8s/components/examples/`, rendered by
`deploy/k8s/components/dsl-mount/component_test.go`.

"Never product code" is enforced by two narrow guards (memql#3326):
`TestEngineIsProductNeutral` (a banned-names list) and
`TestClientsDirectoryIsAllowlisted` (an allowlist of `clients/`). Everything
else rests on review; bespoke product Go becomes a thin optional `bff/` pack
module in the product repo
([platform-consolidation.md](docs/internal/design/platform-consolidation.md)).

#### Environment parity -- one topology everywhere (NON-NEGOTIABLE)

The local cluster and a cloud cluster run the **same topology, deployment
process and connection model**; only configuration VALUES and hardware differ.
FIXED: the node topology, the GitOps base+overlay+ArgoCD path (`make up`
applies the same manifests ArgoCD applies), and the client connection
(ingress -> TLS -> gRPC -> `bff`, dialed as `https://api.<domain>`). ALLOWED to
vary (overlay values): image digests/tags, replicas/resources, domain, DNS
source, TLS source (mkcert vs cert-manager), ingress controller, secrets
source. **Reject in review:** port-forward-as-connection, target-specific
commands, `if env=="local"` branches, or a second way to deploy
([environment-parity.md](docs/public/operate/environment-parity.md)).

One cloud overlay (`deploy/k8s/overlays/cloud`), one ArgoCD Application
(`memql`), one namespace (`memql`), everything in it a VALUE over
`deploy/k8s/base`. **No `if env == "..."` in engine code**:
`TestNoEnvironmentBranchingInEngineCode` fails the build on engine code so much
as NAMING the tier words, with an EMPTY exemption map. `development` / `local`
stay outside that gate: they distinguish deploy TARGETS, which carry their own
field, `provider`.

What makes local parity rather than a lookalike: the same manifests and
reconciliation as AKS, each pod with a unique `MEMQL_NODE_ID` via
`fieldRef: metadata.name`; clients connecting through the
`api.memql.localhost` traefik front door (TLS on 443, mkcert wildcard, h2c to
`svc/bff:50051`) with NO port-forward in the path; **the domain as a VALUE**
(memql#3593: `make up DOMAIN=lab.example.com`, the single `MEMQL_DOMAIN` key
every node derives issuer, CORS origins and redirect URIs from; no file under
`deploy/` names a domain); the engine bff as a COMPONENT
(`deploy/k8s/components/engine-bff`); and `make status` as the litmus (per-pod
node ids plus one shared identity JWKS keyset, memql#3400).

### Component Bus (Channel-Based Communication)

Components communicate via typed Go channels carrying protobuf-defined messages
(`component/bus/bus.proto`): true concurrency, backpressure, and symmetry with
the distributed gRPC model.

```
  gRPC/HTTP ──► EngineRequests ──► MemQL Engine ──► Database (internal)
                                       │
                                       ├──► IntegrationRequests ──► Integration Dispatcher
                                       │
                                       └──► EventPublishCh ──► Event Bus ──► Subscribers ──► Automations
  All Components ──► TelemetryCh ──► Telemetry Collector
```

ReplyTo pattern (request-response via an embedded reply channel); default
buffer 64 per channel (`ChannelConfig`); telemetry hooks for fill-level and
send/drop counters; every component exposes `Ready() <-chan struct{}`.

---

## Endpoint Protocol Policy (gRPC-First)

**IMPORTANT: This policy is a hard requirement for all MemQL development.**

gRPC is the **default and required** protocol for all internal and
service-to-service endpoints. HTTP endpoints are allowed **only** when an
external protocol requirement makes gRPC impossible.

1. **Service-to-service call?** → **Must be gRPC**: request type onto
   `MemqlClientMessage.oneof payload` and response type onto
   `MemqlServerMessage.oneof payload` in `component/grpc/memql.proto`, handler
   in `component/grpc/server.go`.
2. **Consumed by a browser client?** → route through the WebSocket bridge
   (`/memql/ws`), which tunnels to `MemqlService.Stream` -- still gRPC underneath.
3. **Does the external service require HTTP?** (OAuth callbacks, webhooks) →
   HTTP is allowed as a documented exception, below.
4. **When in doubt:** ask the user; the default answer is gRPC. **Never add a
   new HTTP endpoint without explicit user approval**, and document the
   reasoning when you do.

### Allowed HTTP Exceptions

These endpoints **must** remain HTTP because the other party dictates the wire
(a browser, a mail client, a probe, a third-party webhook). Each was approved
individually; a new one needs the same approval.

| Category | Endpoints | Reason |
|----------|-----------|--------|
| **Auth (identity service)** | `/login`, `/auth/magic-link`, `/auth/complete`, `POST /auth/landing`, `GET /auth/magic-link/status`, `POST /auth/magic-link/finish`, `/auth/logout`, `/oauth/token`, `/auth/refresh`, `/.well-known/jwks.json`, `POST /auth/webauthn/{register,login}/{begin,finish}`, `POST /device/code`, `GET+POST /device`, `GET /enroll` | OAuth 2.0 / magic-link need redirects, browser form posts and JWKS publishing; WebAuthn is a browser API; RFC 8628 is defined over HTTP; `/enroll` is a page opened from a link. Magic-link split (memql#4302): the GET renders and never changes state (mail scanners), `landing` is the form post, `status` is the requesting tab's poll gated on the `memql_ml` cookie, `finish` is a real form POST because the reply is a 303 the tab must NAVIGATE. All declared on identity's own route table |
| **Health check** | `/healthz` | Docker and Kubernetes probes expect HTTP GET |
| **GitHub Connect callback** | `GET /auth/github/callback` (identity only) | GitHub redirects the BROWSER here after authorize/install (memql#4912, P11). Starts on the stream (`githubConnectBegin`); writes a `v1:platform:sourceCredential` grant, not a sign-in. State is consumed once under a Postgres advisory lock; an `installation_id` without a valid state updates nothing (GitHub documents it as spoofable). On identity's route table -- in `component/server` it would route to the bff |
| **GitHub App setup** | `GET /auth/github/app/new`, `GET /auth/github/app/callback` (identity only) | GitHub's MANIFEST flow begins with a browser FORM POST to github.com and ends with a redirect carrying a one-time code (owner-approved 2026-09-20; `docs/superpowers/specs/2026-09-20-github-app-setup-design.md`). `new` is a page whose one job is that form post (the OS edge serves `form-action 'self'` for every site), composing the manifest from the cluster's own domain and writing nothing. `callback` spends the state once under the advisory lock FOR ITS OWN PURPOSE, re-checks the person is still an active cluster owner, refuses a managed app or one asking beyond contents+metadata read, seals the credentials under `MEMQL_MASTER_KEY` into `v1:platform:globalSecret`, then sends the browser on to install. Starts on the stream (`githubAppSetupBegin`) |
| **Shopify Connect callback** | `GET /auth/shopify/callback` (identity), `GET /auth/shopify/complete` (OS edge -> identity) | Shopify redirects the BROWSER after the shop's staff approve (`docs/superpowers/specs/2026-09-23-connect-shopify-design.md`); the relay to the OS-hosted completion route lets the host-only session cookie accompany validation and never spends state. Starts on the stream (`shopifyConnectBegin`, shop named from the storefront's publishing run); writes a `v1:shopify:store` row, sealed credentials and the storefront's binding (a SAVED app moves no store until approval, D12). Order of checks is the security: state LOOKED UP unspent and begun by THIS browser's session (D16), HMAC verified with `shop` matching, only then spent under the lock; the person re-judged under their REAL role; the code exchanged in a POST body with no redirect followed. Behind `identity.ShopifyConnect` (`integrations/shopify`, wired in `app/integrations_identity.go`) |
| **WebSocket upgrades** | `/memql/ws` | Browsers need an HTTP upgrade |
| **Site bundle publish** | `POST /sites/{id}/bundles` (bff only) | A CI job hands over an arbitrary tree of files, which multipart carries (memql#3713). `component/edge.Publisher` makes it atomic: content-addressed version prefix first, then the site row's `bundleRef` flips. Authorization is a `class="service_account"` JWT; `HandlerAuthorizedPaths()`, never `PublicPaths()`. Served by the bff, never the edge |
| **Inbound webhooks** | `POST /inbound/{source}` (bff only) | The third party dials US and will only POST to a URL (memql#2957). Deny-by-default source allowlist + per-source HMAC; `HandlerAuthorizedPaths()`. [inbound-delivery.md](docs/public/operate/inbound-delivery.md) |
| **One-click unsubscribe** | `GET+POST /unsubscribe` (bff only) | RFC 8058 is a contract with the recipient's MAIL CLIENT (memql#3348). Mail clients PREFETCH links, so the side effect is on POST only. HMAC-signed token carrying (owner, recipient, campaign), verified before any row is read. `HandlerAuthorizedPaths()` + `SelfAuthenticatedPaths()` |
| **Campaign open/click tracking** | `GET /t/o/{token}`, `GET /t/c/{token}` (bff only) | A pixel is an `<img src>` and a tracked link is one a reader follows (memql#4823, P3). HMAC over the unsubscribe key ring under a DIFFERENT context string; the click destination lives INSIDE the signed payload (open-redirect-proof). Never shows a failure: the pixel always answers the GIF, a bad click token renders the "link is not valid" page. **The token must be a SINGLE PATH SEGMENT** (base64url): `SelfAuthenticatedPaths()`' exemption is bounded to one segment. `server.TrackingPaths()`; literals must agree with `campaigns.TrackingOpenPath` / `TrackingClickPath` |
| **Library artifacts** | `POST /artifacts`, `GET /artifacts/{id}/content`, `POST /artifacts/uploads`, `GET /artifacts/uploads/{id}`, `PUT /artifacts/uploads/{id}/chunks/{n}`, `POST /artifacts/uploads/{id}/complete` (bff only) | Byte transport (memql#4341, memql#4782): 16 MiB chunks staged against Azure block blobs, resumable via the inventory `GET`. `GET .../content` STREAMS through the bff after re-resolving the row under the caller's actor, honoring single-range `Range` (206), never a redirect (no signed URLs). Ordinary AUTHENTICATED routes; `server.ArtifactPaths()` routes them. Caps: `MEMQL_LIBRARY_MAX_UPLOAD_BYTES` (4 GiB/file), `MEMQL_LIBRARY_USER_QUOTA_BYTES` (100 GiB/user) |
| **Shopper forms** | `POST /forms/{pack}/{name}`, `GET /reads/{pack}/{name}` (bff only) | A member of the PUBLIC posting a plain HTML form on a hosted storefront, no JavaScript, no MemQL identity (epic memql#5532). **A shopper is nobody to MemQL**: reach is declared PER ROUTE by a pack (`component/memql/shopper_surface.go`). Reached ONLY through the edge at `/_memql/forms/*` on a deployable's own origin, which owns the per-site switch (`site.shopperForms`, off by default), the rate limit, the size cap and the stamp (site, store, owner: stripped then set). `HandlerAuthorizedPaths()` + **`servedButNotExternallyRouted`**, so the edge is the only way in. The stamp is a POINTER: the bff re-reads the site row UNDER the named owner, so a forged owner reads zero rows. The write runs under that owner's borrowed authority and every row carries the `storeId` of the binding it arrived through (D9 from D7) |

### The front door's HOST set is generated too (memql#3767)

The host set is DERIVED from the closed **role** set plus the platform's own
sites (`frontdoor.PlatformSites()`: the OS shell and the VS Code landing page):

| Role | Host |
|---|---|
| api | `api.<domain>` |
| identity | `identity.<domain>` |
| mcp | `mcp.<domain>` |
| sites | `os.<domain>` and `vscode.<domain>` (exact rule each), `*.<domain>`, plus the apex |

**A platform site is a seeded `v1:platform:site` row, not a role** (memql#5518):
`systemOwned`, a directory the edge image ships (`/app/os`, `/app/vscode-site`),
served through the same resolver as a customer's site. Adding a third is one
entry in `PlatformSites()`, one seed in `dsl/platform/seeds.memql`, one build
step and one hand-authored local rule.

**Every host is a SINGLE label under the domain -- a ROUTING fact** (an Ingress
wildcard matches ONE label, so `*.<domain>` routes every site to the edge)
**and NOT a certificate fact** (memql#4224): ACME cannot issue a wildcard over
HTTP-01 and one wildcard dnsName fails the WHOLE order, so the front-door
certificate names EXACT hosts and every Ingress lists exactly its own rule
hosts under `tls` (`deploy/k8s/overlays/frontdoor_hosts_test.go`). The
wildcard RULE gets a certificate only where the overlay declares a DNS-01
issuer, which the cloud overlay does (memql#4347); the render gate reads the
SOLVER.

`cmd/frontdoorhosts` writes `front-door.generated.yaml` into each overlay;
`component/envregistry/domain.go` composes issuer / CORS origins / redirect
URIs from the SAME rule through `component/frontdoor`; the SeedMaterializer
seeds every platform site's hostname from it. A second copy would disagree,
which presents as "sign-in is broken" with every manifest looking correct.
Adding a ROLE is a design change. The LOCAL overlay's front-door files stay
hand-authored (traefik) but are gated against the same derivation, and its
mkcert pair is a wildcard, so **a site that works over https locally is no
evidence it has a certificate in the cloud.**
[front-door.md](docs/public/operate/front-door.md).

### How an HTTP path reaches the front door (GENERATED, memql#3703)

Every HTTP path above needs its own Ingress rule, and **that rule list is
generated, not authored** (`cmd/frontdoorpaths`, emitted between the markers in
`deploy/k8s/overlays/local/api-front-door.yaml`). An ingress controller's
backend protocol is per-**Service**, so the bff's gRPC edge (`bff`, :50051,
h2c) and its HTTP edge (`bff-http`, :8085) are two Services over one
Deployment, and a path with no rule falls through to the `/` h2c catch-all:
**not a 404, but an HTTP/1.1 request handed to an h2c backend, which fails
with a protocol error naming nothing.**

- **It is per-ROUTE, not per-authentication-tier.** `PublicPaths()` +
  `HandlerAuthorizedPaths()` + `SelfAuthenticatedPaths()` answer *who may reach
  this without a bearer*; an **authenticated** HTTP route appears in none of
  them. The generator unions the aggregates **and** every per-route declaration
  a bff-tagged build mounts.
- **It over-approximates for a path the bff does NOT serve.** Identity-only
  paths are kept: a spare rule costs a 404, a missing one costs a protocol
  error naming nothing.
- **That pricing INVERTS for a path the bff DOES serve, and this is the trap.**
  There, a rule makes the endpoint externally reachable, and for anything in
  `PublicPaths()` that means exposure (`/metrics` is unauthenticated *because*
  in-cluster-only). Hence the fourth classification,
  `servedButNotExternallyRouted` (`/metrics`, `/api/concepts*`). "When in
  doubt, include" applies only to the previous bullet.

Two gates: `TestFrontDoorPathsAreNotStale` (`make frontdoor-paths-check`) and
`TestEveryServerPathDeclarationIsClassified`, which AST-scans `component/server`
for every `func …Paths() []string` and fails when one is classified by none of
the four maps. A route mounted through `handleRoute` with an inline path
literal and no `*Paths()` declaration is invisible to the generator, and
`AssertUnauthenticatedSurface` runs only when the node installs **no**
verifier, which the bff does. **Declare new HTTP routes with a `*Paths()`
function.** Do not hand-edit the generated block, and do not "simplify" the
generator back to the three aggregates.

### gRPC-Only Endpoints

Everything below lives on `MemqlService.Stream`; cross-node proxying rides
`AiForwardRouter`.

| Category | gRPC Message Types | Handler |
|----------|--------------------|---------|
| **AI service-to-service** | `AiChatMsg`, `AiSuggestMsg` | `ai_handlers.go` |
| **Streaming transcription** | `AiTranscribeStreamStart` / `Chunk` / `End` + `AiTranscribeStreamDelta` / `Complete` | `ai_transcribe_stream.go` -- multi-message flow keyed by `request_id`, forwarded BFF -> Agent via `AiForwardRouter.ForwardContinuation`. The one caller is MemQL OS's Ask hold-to-talk |
| **Concepts API** | `ConceptsListMsg`, `ConceptsSubscribeMsg` (+ `follow=true` -> `ConceptsRegistryDelta` stream, memql#4238) | `concepts_handlers.go` |

---

## AI Integration

All AI operations go through a pluggable provider system (`ChatAIProvider`,
`VisionAIProvider`, `TTSAIProvider`, `ChatStreamProvider`) over OpenAI (chat,
vision, TTS, STT) and Anthropic (chat, vision). Provider records live in
`dsl/providers/providers.memql`.

**Selection is not per-call: a call declares a LEVEL and one seam decides**
(epic memql#5127) -- see [Levels, policies and rules](#levels-policies-and-rules)
and [ai-routing.md](docs/public/operate/ai-routing.md). A prompt's
`@defaultProvider` survives as an EXPLICIT PIN that wins over every rule, which
is why it is refused at load when it names a policy.

**Both vendor credentials are workload identity federation, everywhere, and
there is no manually entered API key left in the product** (epics memql#4333,
memql#5088): the engine presents the pod's projected Kubernetes token and
exchanges it for a short-lived bearer; a partial config REFUSES BOOT.
`TestNoVendorApiKeyEntryPoint` (`vendor_api_key_gate_test.go`) fails the build
if any retired key env var or the retired key-setting builtin reappears in a
tracked source file. **Read that file rather than restating its list here --
the gate walks this file too**; `vendor_api_key` as a globalSecret KIND stays
legitimate for the router's BYOK path and the Shopify connector.
[anthropic-federation.md](docs/public/operate/auth/anthropic-federation.md) ·
[openai-federation.md](docs/public/operate/auth/openai-federation.md).

**A LOCAL cluster reaches neither**: a k3d cluster's OIDC issuer is not
publicly reachable, so nothing it mints can be verified. The install wizard
collects no AI credential, `providerFederation` skips satisfied, and a
developer reaches models through a signed-in fleet machine or a local model.

### AI Endpoints (gRPC on `MemqlService.Stream`)

- `AiChatMsg` / `AiChatResult` / `AiStreamChunk` -- chat completions (streaming + non-streaming)
- `AiSpeechMsg` / `AiSpeechResult` -- text-to-speech
- `AiTranscribeMsg` / `AiTranscribeResult` -- speech-to-text (batch)
- `AiTranscribeStreamStart` / `Chunk` / `End` -> `AiTranscribeStreamDelta` / `Complete` -- streaming transcription
- `AiSuggestMsg` / `AiSuggestResult` -- carries `domain`; `knowledge` is the
  one registered domain since the portal's were retired (epic memql#4984).

**The `AiSuggest` registration IS the feature; test it** -- an unregistered
domain reaches the user as "suggestions are not available on this cluster",
the same sentence a cluster with no provider gets (memql#4667). And
`AiSuggestResult.usage` is ABSENT when nothing was reported: zero and "not
measured" are different answers.

Cross-node proxying (BFF -> Agent) rides `AiForwardRequest` /
`AiForwardResponse` on `NodeService.Stream`. Handlers:
`component/grpc/{ai_handlers,ai_transcribe_stream,ai_forward}.go`, which emit a
short error id via `generateErrorId()` (`ERR-{6 hex}`), visible in slog output
as `"errorId":"ERR-..."`.

### Coding Agent -- the container-executor seam

**`cockpit-app` is the seam's first and only inhabitant** (epic memql#4358):
Claude Code or Codex, headless, on a machine the USER owns, through the worker
stream, with MemQL's tools reachable over MCP.

- **The seam:** `component/planner`'s `RegisterContainerExecutor(name, exec)`.
  Lookup keys on the part BEFORE the colon, so `cockpit-app:claude-code`
  reaches the one registered `cockpit-app` backend with the app id on the
  suffix -- growing the app list is a value change, not a release.
  `ValidateExecutorBackend` refuses an unregistered name at TASK CREATION
  rather than at dispatch.
- **The backend:** `integrations/agent/worker/cockpitapp.go`, registered from
  `init()` under the `agent` build tag. It reuses the unexported
  `preDispatchCheck` on purpose: an app run needs exactly the gates
  `workerHost` needs, and a second copy drifts.

### Local apps as execution surfaces (epic memql#4358)

Delegating a task to an app the user already pays for, on a machine they own.
Full record: [local-apps.md](docs/public/operate/local-apps.md).

**Transport is the worker stream; MCP is the back-channel.** The stream the
cockpit opened outward IS the tunnel; each run gets a per-run credential and
the `mcp.<domain>` endpoint.

- **The runnable app set is CLOSED in the engine** (`claude-code`, `codex`);
  unknown ids are stored on the registration and produce no routing label.
- **A machine is selectable only when BOTH `allowed`** (its `policy.yaml
  apps.allow`) **and `signedIn`.** Selection is the Fleet router asked for the
  `app:<id>` label. A session runs only on the replica holding that machine's
  stream; a machine on a sibling replica is SKIPPED, not failed.
- **A run is a SESSION, not a dispatch.** `AppSessionStart / Chunk / Control /
  End` on `WorkerService.Stream`; chunk `seq` is monotonic and out-of-order or
  duplicate chunks are DROPPED.
- **Delegation is a PREFERENCE WITH A FALLBACK** (`v1:worker:delegationPolicy`):
  no allowed, signed-in, online machine means the task runs in-process.
- **The back-channel credential's `sub` is the OWNING USER's id**, so row authz
  applies to the app as to their browser; the surface pin plus `role=system`
  keep it off every credential mutation and cluster-owner gate. Minted via
  `POST /node/bootstrap` with `tokenClass="app_session"`: the user must exist,
  TTL capped at 8h, read/query-pinned, session id as the token label. **It
  carries `class="app_session"`, NOT `service_account`** (memql#4857): the one
  machine class whose subject is a person, admitted to the Library's byte
  routes as that user; `POST /sites/{id}/bundles` still names
  `service_account`, so an app session cannot publish a site. NOT revocable
  (DB-free JWKS verify); the short lifetime, the cockpit deleting the MCP
  config at end, and renewal-in-place stand in.
- **Subscription spend is counted and does not burn the dollar ceiling**
  (`v1:router:call.billing` + `executionSurface`): the DOLLAR ceiling EXCLUDES
  subscription tokens, the LOOP caps INCLUDE the call. Billing falls to
  `unknown` when the usage report or the subscription signal is silent.

The cockpit half lives in `memql-cockpit`; this repo fixes the protocol and
the engine side.

### Workers (computer_use_headless / computer_use_embodied)

Agents drive the user's own machine: shell exec, filesystem, HTTP fetch, and
(under the computer-use build) mouse + keyboard + screenshot. The FALLBACK for
what the sandboxed Workbench below cannot do. The full write-up -- capabilities
and their expansion map, the `WorkerService` gateway and `mql_wkr_` tokens, the
three-layer permission model, the router and its four strategies, the two label
maps, cross-node dispatch, row tier and borrowed authority, the Fleet operator
surface -- is in
[feature-notes.md](docs/internal/design/feature-notes.md#workers-computer_use_headless--computer_use_embodied);
the runbook is
[workers-runbook.md](docs/public/operate/workers-runbook.md). Four rules reach
outside that write-up:

- **Per-user routing, and NO machine id.** The dispatch builtins take
  `requireLabels` / `preferLabels` and **no `workerId`** (D4), so a
  hallucinated machine id is a failure mode this surface does not have.
  Labels match EXACTLY -- there is no "any value" form.
- **`online` is DERIVED, never stored:** unrevoked AND `lastSeenAt` within
  **30s**. Exactly two implementations, `component/worker.IsOnline` and
  `clients/os/src/apps/fleet/online.ts`, held together by
  `TestFleetOnlineWindowMatchesTheClients`.
- **Consent is decided BEFORE routing**, so routing only ever chooses among
  machines already consented to; `refused_before_start` is the cross-node
  re-pick predicate and the one wire field that must never be guessed.
- **Borrowed authority, and no internal-origin stamp.** `component/worker`'s
  store runs every registration read and write under
  `auth.ContextWithUserActor` for the token's owner, and is deliberately absent
  from `call_origin.go`'s allowlist.

### Workbench (workbench_use)

The default first-choice surface for HEADLESS agent work (files, shell, URLs)
as a per-RUN sandboxed Linux working directory in the cluster; nothing on the
user's machine is touched. Computer-use is the FALLBACK for what the workbench
cannot do (macOS-only tooling, computer-use control, files already on the
user's computer).
[workbench-runbook.md](docs/public/operate/workbench-runbook.md) ·
[workbench-production.md](docs/internal/ops/workbench-production.md).

- **Capability:** `workbench_use`, universal (injected into every role's
  `lockedToolSlugs`); no scope grants, no kill switch.
- **Tool:** `workbenchHost`, discriminated by `action` (exec / fs_read /
  fs_write / fs_list / fs_stat / http_fetch), in a product DSL bundle; the wire
  path is the `workbenchDispatchHost` builtin (`dsl/workbench/builtins.memql`)
  to `integration.workbench.dispatchHost`.
- **The environment hint and the reroute (memql#4353).**
  `workbenchDispatchHost` takes an OPTIONAL `environment { os, needs[] }`,
  `needs` from the closed set `display` / `gpu` / `macos_tooling` /
  `user_files`. A mismatch returns a typed `environment_mismatch` having run
  NOTHING; an UNKNOWN need is `invalid_environment_hint`, so a typo can never
  send a call to somebody's laptop. Omitted means no hint; there is no default.
  On a mismatch the tool loop re-dispatches the SAME call to the fleet; only
  `denied_no_per_task_approval` / `denied_by_scope` raise the consent card.
  `needs` -> scope/labels: `integrations/agent/worker/scope.go`.
- **Per-RUN workspace** under `MEMQL_WORKBENCH_ROOT/{runId}/` (default
  `/var/lib/memql/workbenches/`): lazy-provisioned, persists within a run, torn
  down on run terminal status by `releaseWorkspaceOnRunTerminal`, whose
  terminal set has FOUR values because `abandoned` is terminal for a run. Row:
  `v1:workbench:workspace`, `@rowAuthz(owner=..., clusterOwner)`,
  `ownerUserId` stamped from the parent run's owner; a `runId` that does not
  resolve to a readable run is REFUSED (`workspace_owner_unresolved`), because
  a row written under a blank actor is readable by nobody (memql#4354).
- **Replica affinity (memql#4354).** A workspace is a FILESYSTEM. `nodeId`
  names the replica holding it and the peer picker prefers it. On node loss the
  orphan row is released `node_lost` and a FRESH workspace is provisioned:
  **files are NOT migrated.**
- **Modes.** Cluster mode is the default: a dedicated `workbench` binary hosts
  the workspaces, agent nodes route via `WorkbenchForwardRequest` / `Response`
  on `NodeService.Stream`. Base sets `MEMQL_WORKBENCH_REMOTE=1`; the dialer
  needs `MEMQL_WORKER_PEERS=workbench=<addr>`. **The remote flag is an
  ASSERTION:** set with no reachable peer, a call is REFUSED
  (`no_workbench_peer`). In-process fallback is the flag unset, or
  `MEMQL_WORKBENCH_LOCAL_FALLBACK=1` under it.
- **Operator surface:** `/fleet/workbenches` and `/fleet/machines`, live
  because the `graph.node.*` events for `v1:worker:registration`,
  `routingPolicy` and `v1:workbench:workspace` carry broadcast routing rules
  (`component/node/routing.go`); without them the list is correct on load and
  frozen after. `v1:worker:invocation` is excluded on volume grounds.
- **Routing preference:** the agent reply prompt and the `workbench` knowledge
  domain (auto-attached by `replier.go` when the tool list includes
  `workbenchHost`) prefer workbench over computer-use and surface a "workbench
  can't do this" message rather than silently retrying.

## Authentication

The in-house **identity service** (`component/identity`) runs as its own
node-type binary and owns magic-link auth, WebAuthn passkeys, enrolment tokens,
OAuth-style token endpoints (`/oauth/token`, `/auth/refresh`), the JWKS feed at
`/.well-known/jwks.json`, the public web UI (`/login`, `/auth/complete`,
`/setup`, `/legal/*`, `/me/*`) and PAT issuance (`mql_pat_<...>`). Other
binaries verify identity-issued JWTs locally via `component/identity/verifier`
(JWKS on a 5-min refresh and on demand for unknown `kid`) and never see the
private key. `MEMQL_IDENTITY_VERIFIER_BASE_URL` configures the verifier,
`MEMQL_IDENTITY_BASE_URL` the service.

- **Magic links are device-bound and approve-on-click** (epic memql#4300). The
  requesting browser holds the `memql_ml` cookie whose digest is
  `magicLinkRequest.bindingHash`; a link only COMPLETES there, and a click
  elsewhere only APPROVES while the requesting tab polls. **A session can only
  land on the device that asked for it.** `GET /auth/complete` writes nothing;
  consume is a compare-and-swap under a Postgres advisory lock.
- **`signInPolicy` on `v1:identity:user`** (memql#4304): `any` or
  `passkey_only`, which disables sign-in LINKS (no row, no link, identical
  redirect, a mailed notice); requires an active passkey. Owners/admins can
  RESET it to `any` over `IdentityAdminMsg`, one direction only.
- **A new-sign-in email fires on every `authSession` row** (memql#4305), with
  no action link (an unauthenticated revoke link is a DoS handle). Refresh
  rotations never send it.
- **Passkeys are usernameless** (memql#3407): EMPTY `allowCredentials`,
  resolved by credential id alone (unique cluster-wide). RP id derives from
  `MEMQL_IDENTITY_BASE_URL`, never the request Host; no client learns which
  factor ran. A sign-count regression is refused and audited. Revoke on
  `/me/devices` is a SOFT delete; the credential id stays taken.
- **Enrolment tokens** (`mql_enr_<43>`, memql#3408): single-use, TTL'd,
  authorizing exactly ONE action (register a passkey as the named user).
  `GET /enroll` renders; the ceremony presents `Authorization: Enrolment <token>`.
- **The admin web app is gone.** `/admin/*` keeps the sign-in pages and a
  `410 Gone` root; the screens live in MemQL OS over `IdentityAdminMsg`
  (`component/identity/adminops`). `DeployControlService` exists only on the
  identity node; a bff FORWARDS the deploy RPCs over `NodeService.Stream`
  carrying the caller as a verified `ForwardedAuthority`
  (`component/grpc/deploy_control_forward.go`).

**Authentication is ON by default everywhere.** `MEMQL_IDENTITY_ENABLED=false`
is ONLY for troubleshooting: the node skips the verifier and admits every
stream as a synthetic `local-dev` cluster owner
(`component/grpc/local_dev_stream_interceptor.go`) with a loud SECURITY
warning and `memql_auth_enabled` pinned to 0. **Never set it false in a cloud
cluster.** Blanking `MEMQL_IDENTITY_VERIFIER_BASE_URL` is not the way to
disable auth; it fatals the node.

**Two operator credentials, deliberately separate** (memql#3519):
`MEMQL_MASTER_KEY` DECRYPTS; `MEMQL_OPERATOR_KEY` AUTHENTICATES the
`Authorization: Operator <key>` bearer that admits a stream as a synthetic
cluster owner. No fallback: an unseeded cluster refuses operator streams.

See [docs/public/operate/auth/](docs/public/operate/auth/):
[access-model.md](docs/public/operate/auth/access-model.md) ·
[user-provisioning.md](docs/public/operate/auth/user-provisioning.md) ·
[identity-service.md](docs/public/operate/auth/identity-service.md) ·
[operator-credential.md](docs/public/operate/auth/operator-credential.md) ·
[service-account-jwt.md](docs/public/operate/auth/service-account-jwt.md)
(`class="service_account"`, #691: verifies on the BFF via JWKS,
read/query-pinned) ·
[oidc-federation.md](docs/public/operate/auth/oidc-federation.md) (Entra ID /
generic OIDC upstream, epic memql#4611: **the email must be VERIFIED before it
can link**, **`(issuer, subject)` outranks email** once it exists, **exclusive
mode exempts the OWNER**; a half-configured provider REFUSES BOOT) ·
[recovery-key.md](docs/public/operate/auth/recovery-key.md) (the owner
BREAK-GLASS credential, epic memql#3958: `mql_rec_<43>`, bound to one owner,
one passkey registration, REFUSED while the owner still has a sign-in route;
redeeming spends it and mints an unclaimed successor).

## DSL Tree Layout

The DSL tree is **flattened per construct**: every namespace gets one directory
under `dsl/<namespace>/`, and within it each construct kind is consolidated into
a single `<construct>s.memql` file (e.g. `dsl/library/queries.memql`,
`dsl/identity/concepts.memql`, `dsl/providers/providers.memql`). The flattened
tree is produced by
[`scripts/restructure-by-construct`](scripts/restructure-by-construct/main.go).
Authoring reference skeletons live under `dsl/_reference/`, one per construct
(`_concept.memql`, `_logic.memql`, `_automation.memql`, ...). Loaders read through `Source()`, which
routes through [`core/dslfs`](core/dslfs/dslfs.go).

### `MEMQL_DSL_PATH` — runtime product-DSL delivery

`MEMQL_DSL_PATH` mounts **additional product-DSL domains from disk at boot**,
so a product-agnostic engine image runs a product's DSL with no compiled-in
product code (#2472). `dsl.MountRuntimeDomainsFromEnv` scans the root for
product-domain sub-directories and registers each via
`RegisterTree(domain, os.DirFS(<root>/<domain>))`, the same call the embedded
tree uses. Layout mirrors the embedded tree, one directory per namespace:

```
$MEMQL_DSL_PATH/
  <productDomain>/
    memql.toml      concepts.memql  queries.memql  mutations.memql  ...
    prompts/*.tmpl
```

- **Every domain declares its language line** in its own `memql.toml`
  (`memql = "1.0"`, `edition = "2026"`); one at the root is never read. A
  domain with none, or one newer than the engine, refuses boot
  (`memqlmigrate --rewrite=language-line -w <root>` adds it).
- **Adds new domains only**: a directory colliding with a core embedded domain
  is skipped.
- **Fail-loud**: the mounted tree loads through the same strict-boot gate as
  the embedded tree (`MEMQL_DSL_ALLOW_SKIPS` is the break-glass).
- Directories beginning with `_` / `.` are skipped (soft-disable / hidden).

## DSL dependency tree

Each layer depends only *downward*; cycles are rejected at load time.

```
Concepts                    schemas + reserved intrinsics; the base of everything
  |-- Shapes                @row / @actor field projections (+ traits)
  |     '-- Specs           signature-bound predicates
  |           '-- Queries   concept + filter (specs) + projection (shapes) + args
  |-- Mutations             insert / update on rows
  |-- Builtins              Go-backed executors
  '-- Providers             AI vendor + model
        '-- Prompts         template + input schema

Queries + Prompts --> Automations  (event -> side-effect)  <-- Tools (AI-callable)

Prompts (@level) --> Rules  (call metadata -> policy)
                       '-- Policies  (an ordered chain of doors)

Roles + Capabilities --> @requiresRank / @requiresCapability  (who may CALL)
```

A `trait` is the one deliberately-unbound row predicate; **policies are
provider selection only** (caller-based authz / feature-gating decisions are
**specs**); a **rule** is the half a policy cannot express, because a chain
says WHERE to look and never WHICH CALLS it is for. Construct files live under
`dsl/<namespace>/<construct>s.memql`; `dsl/policies` and `dsl/rules` are CORE,
so a pack cannot mount them. **An automation loads only from its domain's
`automations.memql`** (memql#5437): one declared in any other file refuses the
load (`construct_misplaced`, `dsl/construct_placement.go`).

## Argument resolution

All DSL constructs share one model for declaring inputs and reading them.
`ctx` is gone from the author surface entirely.

| Construct kind | Where args go |
|---|---|
| Query / mutation / logic / automation | `args { ... }` sub-block inside the body |
| Builtin / tool / prompt | Body fields directly — the body IS the schema |

`args { ... }` field syntax:
`<name> <type> [@required] [@enum("a", "b", ...)] [@maxLength(N)] [@pattern("re")] [@minimum(N)] [@maximum(N)]`.
Describe a field with a `///` doc comment on the line above it; `@description`
and `@default` are both rejected at load (memql#3336, #991).

| Name pattern | Source | Available in |
|---|---|---|
| `args.X` | Caller-passed arg declared in `args { ... }` | every body |
| `actor.X` | Resolved auth context (`userId`, `role`, `identityId`, `isClusterOwner`, `primaryEmail`, `now`) | every body |
| `now` | RFC3339 timestamp captured at eval start | every body |
| `partition` | Active partition for this call | every body |
| `config.X` | Allow-listed config (`component/config/policy_exposable.go`) | every body |
| `row.X`, `row.id`, `row.concept`, `row.type`, `row.createdAt`, `row.createdBy` | The row, through the lambda parameter a predicate names (`filter row => ...`) | a query's `filter` and `refine`, a spec or trait body, a trigger `@filter` |

A bare name is never a payload field: it is a lambda parameter, a reserved
root, a local or step name, or (when called) a predicate or catalog function.
The exception is a shape body, a path list where `name` is a payload field and
`row.id` an intrinsic.

For automations, the trigger payload is bound INTO the declared `args { ... }`
contract at fire time and validated (`@required` / type / `@enum` /
`@pattern`); a violation refuses the run (`component/automations/args_binding.go`,
memql#2352). The triggering **event** rides its own `event` envelope
(`event.topic` / `event.kind` / `event.payload.<field>`), forwarded to logic
as `logic name(event: event)`; the logic declares `event` in its args block
and reads `args.event.payload.<field>`.

**Declared and used, in both directions**: an `args` field declared but never
referenced is refused at load, and so is an `args.X` a body READS but never
declares (memql#3626). **Reserved engine names** `now`, `actor`, `partition`,
`config`, `trace` are rejected as `args` fields and refused in argument
position at the call site; a repeated argument name (`m(a: 1, a: 2)`) is
refused too.

**Retired author-side forms (all rejected at parse time).** Every construct is
authored in the struct form. The parser refuses each retired shape and NAMES
its replacement, so the enumeration lives in the language reference
([memql.md](docs/public/language/memql.md#retired-forms)) rather than here --
do not write them, and do not "restore" one when you see it in an old diff.
The families: receiver-function wrapping (`func (Query) NAME(ctx any)`, which
survives only as the internal rewriter target the engine's parser consumes),
the `@use*` annotation family (replaced by file-top `use` imports),
`@concepts(...)` / `@shape("name")` bindings (replaced by the two-identifier
construct signature), `@input { ... }`, `include` in a shape body, and the
retired body forms of epic memql#5370 -- `body { }`, `step` blocks, the terse
`=> logic` automation header, `steps.<id>`, a bare argument read, `partition=`
on `@trigger`, the `@schedule` annotation and `mutate` as the mutation keyword
(it is `mutation`, D13). `memqlmigrate --rewrite=bodies` rewrites the body
forms; the step accessors (`step("x")`, `input()`, `item()`, `index()`) have
none. Only `dsl/_reference/*.memql` still shows these, deliberately, as
don't-do-this skeletons.

**Retired expression spellings (edition 2026).** Each is refused at parse,
wherever a `.memql` file writes it, naming its replacement and
`memqlmigrate --rewrite=expressions`, which rewrites it (in VS Code the
language server's **Rewrite to edition 2026** quick fix makes the same edit).
The full list is [memql.md](docs/public/language/memql.md#retired-spellings)
and `parser.V1RetiredForms` is the one copy of it; the shapes to recognise are
the `when(args.x) { ... }` guard and the `?.` prefix, `;` and `,` as
connectives, `has` and `not in`, the `cond(` / `concat(` / `coalesce(` /
`exists(` / `len(` / `count(x)` / `contains(s, sub)` function family, `null`,
a filter or `@filter` without its lambda header, a `spec` or `trait` body
written `{ return ... }`, and `$args.` in a tool handler. The string a client
sends to `Execute` is the internal query form, which keeps its own grammar --
but it too refuses `;` and `,` as connectives (`retired_comma_connective`,
memql#5439; `parentOf(a, b)` is `parentOf(a || b)`).

## Levels, policies and rules

**Three nouns decide every model call, and a call site names none of them but
the first** (epic memql#5127; [ai-routing.md](docs/public/operate/ai-routing.md)).

- A **level** is how much intelligence a call needs: a CLOSED set of four,
  `fast`, `strong`, `reasoning`, `embeddings`. `@level` is REQUIRED on every
  `prompt`, embedded or bundle-mounted, and a Go call site with no prompt names
  its level in the request. **Modality is never declared**; it is derived from
  the call and interface-checked. A model name at a call site is a release
  every time the fleet changes.
- A **policy** is an ordered chain of places to look, a closed grammar: a
  provider name; `fleet:strongest` / `fleet:fastest` / `fleet:<modelId>`;
  `app:*` / `app:<id>` / `app:<id>:<model>`; `federation:cheapest` /
  `federation:strongest` / `federation:<providerName>`; `policy:<name>`.
  `fleet:*` is retired and refuses load. An app id is held to the engine's
  closed runnable set at LOAD (`core/airoute/apps.go`); `app:*:<model>` is
  REFUSED. `policy:<name>` is expanded at load and a cycle refuses load. The
  shipped set is `dsl/policies/policies.memql`.
- A **rule** maps a call's metadata to a policy, first match in precedence
  order. `@when` takes a closed key set (`level`, `modality`, `prompt`, `role`,
  `actorRole`, `tag`, `touches`), all present keys ANDed. Every shipped rule is
  `@locked`.

```memql
@when(prompt="agentReply", role="operator")
@level("reasoning")
@policy("localFirst")
@precedence(60)
@onUnavailable("degrade")
@locked
rule operatorReasoning { }
```

- **`role` and `actorRole` are different questions** (what is acting vs who is
  watching). An ABSENT `@when` key is no condition; a key written EMPTY matches
  only an empty value.
- **A precedence tie is a LOAD ERROR**, naming both rules and files.
- **`@locked` means three things, all enforced**: evaluates before every
  unlocked rule regardless of precedence; re-read from the embedded tree on
  every boot; no runtime-authored rule may carry it or take a shipped name.
  Both registries REFUSE a duplicate name across the whole corpus.
- **`embeddings` never degrades**, at any `@onUnavailable` setting (a degraded
  embedder answers in a DIFFERENT VECTOR SPACE). `fast` is the floor;
  `reasoning` walks down to `strong` to `fast`, and `servedLevel` / `degraded`
  are on every decision record.
- **`@maxLatencyMs`, `@maxTimeToFirstTokenMs`, `@preferredRole` and
  `DefaultForRole` are REMOVED.**

**One seam, enforced by a build gate.** Every model call builds a
`core/airoute.ResolveRequest`; a Go AST gate fails the build on a direct
provider-registry accessor (`Entry` / `ProviderEntry` / `ChatProvider` /
`DefaultChatProvider` / `StructuredChatProvider` / `ChatStructuredProvider` /
`SuggestChatProvider` / `ChatStreamProvider` / `VisionProvider` /
`EmbeddingProvider`) outside `component/router` and the registry. The
vocabulary lives in `core/airoute` because `component/router` imports
`component/memql`, where the call sites live.

**Every resolution is a decision record.** `v1:router:call` carries `level`,
`requestedLevel`, `servedLevel`, `servedModel`, `servedEffort`, `degraded`,
`rule`, `policy`, `door`, `considered`, `touches`, `minContextTokens`,
`machineOwnerUserId`, `callerKind` and `cacheKind`, read through
`routerDecisionsRecent`, never broadcast. `considered` is KEPT ON SUCCESS.
`callerKind` makes an empty `userId` an ANSWER (memql#5581: `system` is a sweep or seed, `unattributed` a Go call site that stamped no caller). `cacheKind` names the cache that answered: a hit is a decision with no provider call and REAL ZEROS for tokens and cost, ABSENT means a provider answered. One bounded queue, one writer; a full queue drops and counts. `servedModel` / `servedEffort` are
what the SURFACE REPORTED, EMPTY when it said nothing (epic memql#5391, D9).

**A tool-needing call resolved to an `app:` door becomes a SESSION** (epic
memql#5391, D7), `door`'s fourth value beside `local` / `app` / `federation`:
the app takes the whole STEP, drives its own loop, reaches MemQL's tools over
MCP and answers once with no tool calls. The step records a `childRunId`; a
call carrying NO step is refused at resolution; a session winner's chain is
itself alone, so a failed session parks rather than becoming a silent paid
call. Every artifact a session produces is stamped `producedBy`
`{app, model, effort, sessionId}`.

**Mechanism stays in Go.** Door classification, provider availability, the
refusal codes, `ai_guard.go` / `ai_guard_fleet.go` and
`component/work/budget.go` are not authorable and no rule reaches them. The
two halves meet at one place: **the federation hop asks the cost ceiling
before it is taken, and only when a local door preceded it in the chain.**

**There is no decision-policy tier.** Auth / feature-gating / vendor decisions
live in Go (`component/safety` ships the risk×scope decision matrix) and in
**specs** (`requiresOwner(actor)`). `engine.EvaluatePolicy` and `func (Policy)`
do not exist.

## Key Concepts

### Authorization model

Per-row authorization is the only gate
([per-row-authz-audit.md](docs/public/operate/auth/per-row-authz-audit.md)).
Every query and mutation classifies as **owned** (filter on
`row.ownerUserId == actor.userId`), **granted** (relationship predicate gates
on actor.userId), **admin** (cluster-owner spec), or **public** (`@public`);
`test/dslconformance/conformance_test.go` hard-fails on a new unclassified
construct.

**Row admission also gates SUBSCRIPTIONS** (memql#4309): a `graph.node.*` event
reaches a stream only if the function admitting the row on a read admits it
for that stream's actor, and a concept that declares nothing admits everyone
on BOTH paths. A `granted` row arrives id-only with `payload_omitted`.
Non-graph kinds (`TELEMETRY` / `MESSAGE` / `AI_STREAM` / `ALL`) are
owner/admin-only at subscribe time (memql#4311).

**`@rowAuthz(owner="<field>", clusterOwner)`** is the owned tier with the admin
gate ORed in, not a new tier (memql#4312); a plain `owner=` tier has no
cluster-owner bypass, so declaring an operator surface plain-owned hides every
other user's rows from the operator. The write guard ignores the second
argument.

**`account="<field>"` (epic memql#5165) is one more ARGUMENT of the owned
tier**: it ORs "anyone whose group ties them to this row's account" onto the
admission, resolved per request from `v1:identity:groupMembership` and
`v1:identity:group`. It widens WRITES as well as reads, the field may be a
string or a string list, and its type is checked at load (the lowering is a
jsonb containment test, which would also match a map key). Developer rank and
above are standing members of every account-kind group as a RULE.

**RANK (epic memql#4832) adds three more ARGUMENTS of the owned tier, not more
tiers** (four sites switch on the owned tier and a new tier value falls
silently out of all four):

- `rankVisible` -- reads widen to the owner OR anyone at or above the OWNER'S
  rank (D2; peers included, `<=`).
- `rankStrict` -- writes widen to your own row or one owned by someone STRICTLY
  below you (D3), **and WITHDRAW the cluster-owner write escape**. Requires
  `rankVisible`.
- `unowned="<role>"` -- a PRESENT but EMPTY owner is the deployment's row;
  this names the rank from which it is readable. An ABSENT owner key stays
  denied.

The owner's rank is resolved **per request**, never stamped on the row. The
SQL half pushes down as an `in` list (a post-filter-only rank term makes a
page of peer-owned rows read as EXHAUSTION). **An unresolvable rank floor
DENIES.** D4: `MaintenanceActor`, the seed materializer, an automation's system
actor and borrowed authority carry `AccessContext.Unranked`, or every sweep
and boot seed would become a peer-write and stop.

**`@requiresRank("<role>")` is the SURFACE half** (D6): an actor-rank FLOOR on
a query / mutation / logic / tool (on a tool it judges the person the call is
for, and decides what is listed to them, memql#5438), validated at LOAD and
enforced at execution. It gates WHO MAY CALL; `@rowAuthz` decides WHICH ROWS
come back. MemQL OS's per-surface `requires: "app:<id>"` (epic memql#5289) is
its mirror, not a stand-in.

**`@requiresCapability("<verb>", "<resource>")` is its SIBLING** (epic
memql#5166, D11): validated at LOAD against the five verbs and the seeded
resource kinds, enforced on the direct call and on every plan that EXPANDS the
construct. **A rank is a FLOOR and a capability is a GRANT**: developer ranks
300 above admin's 200 and holds strictly fewer verbs on `principal`. Declared
together, both must pass. `requiresAdmin`, `requiresOwnerOrAdmin` and
`requiresDeveloperOrAbove` are DELETED. `@requiresCapability` is legal on a
BUILTIN (epic memql#5288), whose executor asks itself; `@requiresRank` is not.

**Apps are resources** (epic memql#5288, D7): `read app:<id>` opens an OS app,
`execute app:<id>/<part>` is a named part, and a part EXISTS by being seeded on
a role in `dsl/rbac/seeds.memql` (a misspelled part refuses boot). **The OS
registry names resources, never floors** (epic memql#5289, D10): manifests,
floored sections and widgets carry `requires: "app:<id>"` /
`"app:<id>/<part>"`, the shell reads `effectiveCapabilitiesForActor()` at
sign-in (again on focus and after a grant written in that browser) and draws
a surface when the set holds `read` on its name. `holds()` in
`clients/os/src/system/roles.ts` is the one predicate, `accessEpoch` the
reactivity signal, `roles: { min }` is REFUSED, and
`TestOsRegistryRequiresMatchTheAppSeeds` pins every `requires:` to a seeded
`read app:*` row (module lists are `needs:`). A missing PART hides its
control; a call that reaches the engine anyway is refused
`capability_not_held:`-prefixed. Settings > Access
(`requires: "app:settings/access"`) writes and reads back grants.

**One role ladder, and the shell holds none of it** (D1): `v1:rbac:role`
carries `rank` plus `aliases` (`writer`/`reader` alias `user`/`viewer`), the OS
reads `activeRoles`, and `component/auth/role_ladder_client_parity_test.go`
fails the build on a client ordering of its own. **developer (300) outranks
admin (200).**

**A ROLE IS A ROW, AND THE ROWS ARE THE TRUTH** (epic memql#5166). A runtime
CATALOG loaded from `v1:rbac:role` and `v1:rbac:capability` at boot
(`component/memql/rbac_catalog.go`) is the ONE resolver every `Capable` call
reads, reloaded on both concepts' broadcast events; `rankLadder` reads the
same structure. The compiled `capabilitySets` map is the seed's MIRROR,
answering only before the rows are readable (`TestSeedMatchesCompiledMirror`).
**An unknown slug holds nothing and ranks 0.** `v1:identity:user.role`,
`invitation.inviteeRole` and `delegation.roleCeiling` are STRINGS carrying a
catalog slug or alias. `roleCreate` / `roleUpdate` / `roleDeactivate` author
one under rank-below-creator, rank-not-taken, grants-a-subset-of-the-caller's
and predefined-immutable; `auth.MayAssignRole` is the ONE rule both assignment
seams call. [access-model.md](docs/public/operate/auth/access-model.md).

**A GRANT is what a person or a group holds over and above their role** (epic
memql#5287): `v1:rbac:grant`, one `allow` / `deny` per `(verb, resource)`,
resolved by `auth.CapableFor(ctx, subject, verb, resource)` at every gate --
most specific wins across role, group and user, deny wins within a level, read
per request and never cached. Written only through `grantSet` / `grantRevoke`,
audited as `targetType: grant`
([grants](docs/public/operate/auth/access-model.md#grants-to-people-and-groups)).

The partition dimension is retired (#56): the `partition` wire field is
`reserved` in `component/grpc/memql.proto`, and `partition="*"` on a trigger is
refused at parse (`trigger_partition_retired`, epic memql#5370).

### Concepts

Schemas for nodes (like tables in SQL).

```memql
concept agent {
  ownerUserId  string  @required
  // ...
}
```

**Relationships carry two independent axes** (memql#3652):

```memql
@relationship(type="references", as="respondsAs", field="agentId", target=agent, direction="outgoing")
```

- **`type`** — what the ENGINE does with the edge. A **closed** set:
  `parent`, `owns`, `createdBy`, `alias`, `equals`, `contains`, `references`.
  It drives id canonicalization, traversal and the collection/reference
  node-type invariants; an unrecognized value **refuses boot**, naming `as=`
  as the way out.
- **`as`** — what the edge MEANS to the domain (`assignedTo`, `repliesTo`).
  **Open**: any lowerCamelCase identifier, validated for FORM only. Optional.
  **Never add a membership check to `as`**; a test guards it (memql#3655).

`field` may be a dotted path into a nested object block
(`field="lineage.originatingPlanId"`, memql#3672); it must be declared on the
concept -- on the TARGET concept for `direction="incoming"`. Reference:
[memql.md](docs/public/language/memql.md#relationships) and
`dsl/_reference/_concept.memql` section 11.

### Nodes
Individual records with time-series history. IDs are
`{concept}:{shortId}`:

```
v1:common:agent:a9f3b7c2...
v1:cluster:node:bff-local
```

### Automations

Automation cycles require a disproving trigger filter or `@loop`; `@mode`
controls concurrent runs per process. Prefer before-write bodies for
triggering-row adjustments. The default chain depth cap is 16
([loop protection](docs/public/language/memql.md#loop-protection)).

`@trigger` keys off an event name plus the target concept, and the
automation's `args { }` block is the contract the triggering row's payload is
bound into:

```memql
/// On to-do creation, promote it into the Library Records lens.
@trigger(event="node.created", concept="v1:todos:todo")
automation indexTodoOnCreate {
  args {
    id any
    ownerUserId any
    title any
  }

  persist := mutation createArtifact(
    sourceConceptRef: args.id,
    ownerUserId:      args.ownerUserId,
    lens:             "record",
    kind:             "todo",
    source:           "agent_generated",
    title:            args.title ?? "Untitled to-do",
    live:             false
  )
}
```

A time-driven automation uses `@trigger(schedule="0 */10 * * * *")` (six-field
cron, leading seconds); the `@schedule` annotation and `partition=` on a
trigger are refused at parse. An automation's statements are the body language
[Logic](#logic) describes, plus `publish`, `automation` and `action` calls,
which only an automation makes.

### Functions

Reusable query and mutation functions, in the struct form.

**Concept binding lives in the construct signature**: `query <Concept> <name>`,
`mutation <Concept> <name>`, `seed <Concept> <name>` and
`shape <Concept> <name>`; the loader resolves the name through the file-top
imports, and one that does not resolve refuses the load naming the import to
add (`signature_concept_unresolved`, memql#5433). **Cross-file dependencies go
through file-top `use` imports** (shapes, traits, specs, mutations, queries,
logic, builtins, prompts, providers, tools):

```memql
use worker.concepts.{ registration }
use worker.shapes.{ registrationFull }
use common.traits.{ isActiveRecord, isNotDeleted }
```

The dotted path maps to a file on disk (`worker.concepts` →
`dsl/worker/concepts.memql`). A filter reads the bound concept's row through
its lambda parameter; a mutation body writes it through the bare `insert { }`
/ `update { }` block.

> **Where the rules below are enforced (memql#3629, memql#4051).** The CONTRACT
> gates (retired operator forms, the `row.` namespace rules, the per-row authz
> bucket, the admin-gate composition rule, the cross-namespace import rule)
> run inside `MemQLEngine.Init`, land on the `LoadReport`, and are refused by
> strict boot (`MEMQL_DSL_ALLOW_SKIPS` is the break-glass) -- which is what
> covers a product bundle at `MEMQL_DSL_PATH`; `cmd/memqllint` runs the same
> `Init` offline. House-style gates stay test-only. **Write a new gate against
> `dslgate.ScanFiles`, which takes the whole corpus.**

**Canonical filter-clause syntax** (edition 2026; the parser refuses every
retired spelling naming its replacement, and `MemQLEngine.Init` lowers every
filter against the tier manifest, so a filter that cannot push down refuses
the load):

- A filter is a lambda over the row:
  `filter row => row.status == args.status && isActiveRecord(row)`. Every field
  goes through the parameter -- payload properties and intrinsics (`row.id`,
  `row.concept`, `row.type`, `row.createdAt`, `row.createdBy`,
  `row.provenance.<leaf>`) alike. A long filter continues on lines opening
  with `&&` or `||`.
- Sort keys keep their string form and the `row.` namespace for intrinsics --
  `sort "row.createdAt", "desc"`; payload sort keys stay bare; `provenance`
  has no sort form. An authored key must name a declared field or a sortable
  intrinsic, a direction is `asc` / `desc` in any case (`sort_key_unknown` /
  `sort_direction_unknown`), and a clause is written once
  (`query_clause_duplicate`; memql#5429).
- **One boolean grammar:** `&&`, `||`, `!` and parentheses
  ([precedence](docs/public/language/memql.md#operator-precedence)); no
  truthiness, so a condition must be boolean.
- Membership is `v in list`. Prefix selection is `row.<field> startsWith
  <prefix>` (memql#4208): a string literal, a list of them, or an `args.<field>`
  resolving to either; an EMPTY list and a BLANK prefix match nothing.
- An optional argument is guarded by an ordinary predicate:
  `(args.x == nil || row.f == args.x)` under `&&`,
  `(args.x != nil && row.f == args.x)` under `||`; the guard reads no row and
  folds to a plan constant.
- A missing field, JSON null, `nil` and `""` are one unset value in `==` and
  `!=`, and `!=` is null-safe
  ([absent values](docs/public/language/memql.md#absent-values)).
- Specs and traits are applied to their receiver: `isActiveRecord(row)`,
  `requiresOwner(actor)`. Where a trait covers the predicate it is mandatory;
  the conformance test rejects the inline comparison.

**Annotations** in the args block: `@required`; `@enum("a", "b", "c")` (string
literals only -- express a small numeric set as bounds plus a UI that offers
only the members); `@maxLength(N)`; `@pattern("re")`; `@minimum(N)` /
`@maximum(N)`, INCLUSIVE numeric bounds (memql#4522). `@description` is **not**
valid on an args field (the `///` doc comment is; a `tool` / `prompt` /
`builtin` field keeps it). `@default` is **not** valid on an args field nor on
a concept field (epic memql#5375; neither was ever applied on insert); apply a
default in the body with `args.X ?? <default>`, the only mechanism that fills
a value. It **stays** on a `tool` / `prompt` field (a `builtin` field refuses
it as misplaced), where the body IS the schema handed to the model and the
value must be a literal of the field's type (memql#5430). `a ?? b ?? c`
returns the first operand that does not fall through. **`??` is
BLANK-coalescing:** it falls through on an empty or whitespace-only string as
well as absent/null (`false` / `0` / `[]` / `{}` are kept), so a deliberately
cleared text field gets the default written back
([authoring-rules.md §28](docs/public/language/authoring-rules.md); `@noUnset`
is the targeted opt-out).

Queries:
```memql
use worker.concepts.{ registration }
use worker.shapes.{ registrationFull }
use common.traits.{ isActiveRecord }

@description("Get a user's registered machines")
query registration registrationsForOwner {
  args {
    ownerUserId  string  @required
  }
  filter  row => row.ownerUserId == args.ownerUserId && isActiveRecord(row)
  shape   registrationFull
}
```

Mutations:
```memql
use library.concepts.{ folder }

@description("Create a Library folder")
mutation folder createFolder {
  args {
    folderId  string  @required
    name      string  @required
  }
  insert {
    id:        args.folderId
    name:      args.name
    status:    "active"
    createdAt: now
    createdBy: actor.userId
  }
}
```

`update { id: ..., ... }` is the partial-update counterpart
(read-merge-validate-write). Exactly one `insert` OR `update` block per
mutation.

### Logic

A procedure an automation or another logic calls. `args { ... }` declares its
inputs, and its statements follow, the last of them a `return`. The
one-statement form is the common case:

```memql
/// Pure decide for the workspace-release sweep: every v1:workbench:workspace
/// of the updated run.
logic releaseWorkspaceOnRunTerminal {
  args {
    event object!
  }
  return query workspaceForRun(runId: args.event.payload.id ?? "")
}
```

A construct call names its kind and its arguments -- `query workspaceForRun(runId: ...)`,
`mutation createLibraryFolder(folderId: args.folderId, name: args.name)`. The
object-literal form `name({ k: v })` is refused, and the bare-name pun
(`logic decide(event)` for `event: event`) is retired.

A logic and an automation share one body language (epic memql#5370):
`name := <call>` binds, `if` / `for` / `switch` / `parallel` / `return`,
trailing `retry(n)` and `on error continue`. Statements run in the order
written; nothing is reordered by dependency, and there is no
`ctx.output = ...`. A logic may call queries, mutations, logic and builtins
and ends with `return`; `publish`, `automation` and `action` calls are an
automation's. Full language: [memql.md](docs/public/language/memql.md#bodies).

### Prompts

AI prompt templates with input schemas and default providers. Struct form; the
body is a bare input-schema field list, no `@input` wrapper. Logic prompts
(routing / suggest / classification) use the structured-output path
(`ChatStructuredProvider.CallChatStructured`); prose prompts use regular chat.

```memql
@description("Generate an agent reply for a space")
@defaultProvider("chat54Mini")
@templateFile("agentReply.tmpl")
prompt agentReply {
  space         object  @required
  history       []object
  spaceContext  object
}
```

### Providers

AI provider configurations (OpenAI, Anthropic -- the only supported vendors).
Base providers carry vendor-level auth + type.

```memql
@base
@vendor("OpenAI")
provider openai {
  auth {
    identityProviderId  env("MEMQL_AI_OPENAI_IDENTITY_PROVIDER_ID")
    serviceAccountId    env("MEMQL_AI_OPENAI_SERVICE_ACCOUNT_ID")
    identityTokenFile   env("MEMQL_AI_OPENAI_IDENTITY_TOKEN_FILE")
  }
}

@description("OpenAI GPT-5.4 Mini -- balanced cost/latency chat")
@extends("openai")
@model("gpt-5.4-mini")
provider chat54Mini {
  params {
    contextWindow        128000
    maxCompletionTokens  16384
    inputCostPerMillion  0.15
    outputCostPerMillion 0.60
  }
}
```

**`@disabled`** (the lifecycle flag shared by functions / builtins / prompts /
specs / seeds) skips the provider at load -- not registered, no auth
resolution, zero warnings -- while keeping it in the tree; on a `@base` it
propagates to every child. Dependents degrade gracefully (a policy routes via
its `@fallback`, a prompt falls back to the default). `@enabled` is RETIRED
(epic memql#5375) and refused at load naming
`memqlmigrate --rewrite=attributes`. `@disabled` means "not active right now",
not deprecated or exempt from refactors -- that is the separate `@deprecated`
axis (`component/language/ast/ast.go`, `AttrEnabled` / `AttrDisabled`).

### Shapes

Reusable data projections. Each shape declares its **kind** via `@row` and/or
`@actor` (at least one; both allowed). Each path becomes a template entry keyed
by its terminal segment.

**Row shapes** project a concept's payload + row intrinsics, bound by the
signature `shape <Concept> <name>`:

```memql
use library.concepts.{ folder }

@description("Folder summary card")
@row
shape folder folderCard {
  row.id
  name
  description
  row.createdAt
}
```

**Actor shapes** project the engine envelope and carry no signature concept.
Closed field set, enforced at load: `actor.userId` / `actor.role` /
`actor.identityId` / `actor.isClusterOwner` / `actor.primaryEmail` /
`actor.now`. Bare `config.<key>` is the config read; shapes do not project it.

```memql
@description("Actor identity envelope")
@actor
shape actorEnvelope {
  actor.userId
  actor.role
  actor.identityId
  actor.isClusterOwner
}
```

**Mixed shapes** carry both. **No composition**: `include` is REJECTED at load;
repeat the paths, or drop the body and take the default projection. **Every
body path is checked at load**: a bare payload property must be a declared
field of the bound concept, the concept must resolve (an ambiguous bare name
disambiguates through the shape's own domain), two paths may not collapse onto
the same terminal key, and the declared kind must match the body. Shapes have
no inputs and no return.

### Specs

Atomic boolean predicates, **signature-bound** (epic #2281):
`spec <boundName> <name> = row => <predicate>`, binding exactly one shape XOR
concept resolved via the file-top `use` import. The binding picks the strategy:

- **Row-specs** bind a concept or a `@row` shape and compile into a SQL `WHERE`
  fragment.
- **Context-specs** bind an `@actor` shape (the only gateway to the auth
  envelope), take `actor`, evaluate in-process and are applied to the caller:
  `requiresOwner(actor)`.

The parameter reads only what the binding projects. A `trait` is the one
deliberately-unbound row predicate (fields validated at the call site).

```memql
use worker.concepts.{ registration }

@description("Matches revoked machine registrations")
spec registration isRevokedRegistration = row => row.revokedAt != nil    // concept-bound row-spec

use common.shapes.{ actorEnvelope }

@description("Caller must hold the owner role -- the rollback gate (#1876)")
spec actorEnvelope requiresOwner = actor => actor.role == "owner"        // @actor-bound context-spec

@description("Matches records with active==true field")
trait isActiveRecord = row => row.active == true                        // unbound cross-concept trait
```

There is no `ctx` envelope; the `{ return <expr> }` body is retired and refused
with `memqlmigrate --rewrite=expressions` as the fix. **Caller-context checks
use specs, not policies**: author the predicate as a context-spec in
`dsl/<namespace>/specs.memql` and apply it to `actor` in the filter.

### Tools

AI-callable tool definitions. The body is a list of input-schema fields
(`@required`, `@default`, `@enum`, `@description`).

```memql
@description("Search for users")
@handler(type="query", query="paginate(query searchUsers(active: args.active), args.limit)")
@executionTime("fast")
tool searchUsers {
  active  boolean  @description("Filter by active status")
  limit   integer  @default("10") @description("Max results to return")
}
```

**A tool declaration is CHECKED at load**, fail-loud:

- **`@handler` argument names are closed** (`type`, `name`, `query`, `url`,
  `method`) and `type` is required. `@rateLimit` and `@scopes` are RETIRED
  (epic memql#5375: stored, advertised and enforced nowhere). `@allowedRoles`
  is DEPRECATED (memql#5438, refused from 0.26): it mixed the agent's role and
  the person's. Write `@requiresAgentRole(...)` (which agent calls, held to
  `v1:agents:agent.role`) and `@requiresRank` (the person);
  `memqlmigrate --rewrite=allowed-roles` rewrites it.
- **The handler is validated at load** and a tool must carry one. A query
  handler is ONE construct call, or that call inside `paginate(...)`, whose
  arguments read `args.<name>`; a raw filter or the retired `$args.`
  placeholder refuses the load. A webhook's url is one expression.
  `@clientExecution` is REFUSED at parse (it went with the conversational
  product, epic memql#4988).
- **The handler's TARGET is resolved** against the function + builtin registry
  at boot (`tool_handler_resolution.go`). A builtin is reached through
  `@handler(type="function", name="<builtin>")`; there is no `"builtin"` type.
  Every field the builtin REQUIRES must be a tool field too
  (`tool_handler_arg_undeclared`, memql#5436).
- **Field types and field annotations are closed sets.** An unknown type is
  refused rather than emitted as `"string"`, and a `@default` must be a
  literal of the field's type (`tool_default_type`, memql#5430).

### Integration Capabilities

Go-backed operations callable from the DSL via `@executor("integration.X.Y")`.
The body's field list is the builtin's input schema.

```memql
@description("Run one command on a per-run workbench workspace")
@executor("integration.workbench.dispatchHost")
@args(profile="object")
builtin workbenchDispatchHost {
  runId    string  @required
  action   string  @required
  command  string
}
```

Core integrations (registered via the plug-in system): agents, auth, database,
deployversion, email, embedding, files, azureblob (as `storage`),
harnessRecall, identity, knowledge, library, rbac, router, shopify,
similarity, timeutil, workbench, workTrace, plus node-type-scoped ones (agent,
stt) wired in `app/integrations_*.go`. `training` is a product-repo pack.

`shopify` is a CONNECTOR, the reference implementation of
`component/memql/sync.Connector` (epic memql#4389): it owns one external
system's data, its model is GENERATED from that system's schema at a pinned
version (`cmd/shopifyschema` -> `dsl/shopify/generated/`), and its five verbs
return MirrorWrites for the runtime to apply. Read
[integrations/CLAUDE.md](integrations/CLAUDE.md) before writing a second one;
runbooks: [shopify-connector.md](docs/public/operate/shopify-connector.md),
[shopify-storefront-checklist.md](docs/public/operate/shopify-storefront-checklist.md).

### Extension Points

Three ways to extend MemQL, in preference order:

1. **DSL files** (`.memql`) -- always the first choice.
2. **Self-registering plug-ins** -- Go integrations that call
   `memql.RegisterPlugin(name, factory)` from `init()`, receiving a narrow
   `PluginContext` (Logger, Engine, BunDB getter, ResolveVisionProvider,
   ResolveEmbeddingProvider, partition/variable resolvers). Build tags on the
   calling file control which binaries include the registration
   (`component/memql/plugins.go`).
3. **Explicit `app/` wiring** -- for first-party integrations whose
   dependencies don't fit `PluginContext` (agent, stt), in
   `app/integrations_*.go` with build tags.

Event routing is also plug-in-registerable: `node.RegisterRoutingRule(...)`
from `init()`. Forwarding is default-deny -- block rules first, then forward
rules, and an event matching neither stays local. **There is no
concept-ownership registry**: which node does a concept's work is decided by
routing rules plus which binary's build tags compile the subscriber.

### MemQL Sense (Language Intelligence)

Language service for .memql files, exposed via gRPC on `MemqlService.Stream`:
**Tokenize** (semantic tokens), **Complete** (context-aware autocompletion),
**Diagnose** (lexer / parser / semantic errors; given the file's path under the
DSL root, also Lower's `lower_*` load refusals, memql#5434), **Hover** (symbol info) and
**SignatureHelp**. Package: `component/memql/sense/` -- pure Go, no gRPC
dependency; handlers in `component/grpc/sense_handlers.go`.

### Data origins -- Mirror, Origin, Native (epic memql#4378)

**MemQL is the origin of what it owns, a faithful mirror of what it does not,
and every concept says which.** Two declarations, three states, and
deliberately no "shared":

| State | Changes made at | Copies elsewhere | Writable here |
|---|---|---|---|
| Mirror | an external origin | MemQL | **no** -- read-only by construction |
| Origin | MemQL | external mirrors, synced outbound | yes, and it propagates |
| Native | MemQL | nobody | yes |

```memql
@origin("shopify")                      concept product { ... }      // mirror
@origin("memql") @mirroredTo("shopify") concept creditLimit { ... }  // origin
                                        concept plan { ... }         // native
```

**"Read-only by construction" is literal, and STRICTER than the row-authz
write guard.** `executeWrite` refuses every write to a mirror concept that does
not come from the connector its `@origin` names (`mirror_write_refused{origin}`
plus an audit line), internal origin and cluster owner included. A mutation
bound to a mirror must be `@serverOnly`
(`TestMirrorConceptsHaveNoClientReachableMutation`).

**Connectors are named actors, not a bypass.** `auth.ConnectorActor(name)` is
admitted to the concepts whose `@origin` or `@mirroredTo` names it, regardless
of tier, and to nothing else; no request can mint one (`RoleConnector` is
outside `ValidRoles()` and the rank model). A connector is an *integration*
implementing `component/memql/sync`, not a fourth extension word.

**Registration has two halves.** `sync.Declare(name)` from an `init()` says
this build serves the connector; `sync.Bind(c)` attaches the implementation
later, because `MemQLEngine.Init` runs BEFORE integrations are wired. An
unresolvable name **refuses boot**: a mirror nobody fills reads as an empty
catalog, silently. [data-origins.md](docs/public/concepts/data-origins.md).

### Infrastructure concepts

Field inventories live in the `.memql` files; this is only what the schema
does not say.

- **Platform** (`dsl/platform/concepts.memql`) -- `site`, a hosted web surface
  and "deployable" (memql#4344): the edge resolves the request `Host` to one of
  these rows and serves its `bundleRef`. A user's hostname is `<slug>.<domain>`
  against a reserved set DERIVED from `frontdoor.Roles()` + the OS shell.
  Android / iOS / macOS have NO enum values; they are a written-down TARGET
  shape (epic memql#4885), reported `deployable_target_not_offered`
  ([deployables.md](docs/public/operate/deployables.md)). `package` /
  `packageDeployment` are the SOURCE and its append-only run timeline
  ([packages.md](docs/public/operate/packages.md)); `sourceCredential` is the
  personal token a private source fetches under, sealed once server-side and
  resolved under the **package owner's** actor (someone else's credential
  resolves zero rows: `credential_not_found`). There is no cluster-wide source
  credential. Also `globalSecret` / `globalVariable`, `outboundRequest` /
  `inboundRequest`, `missingCapability`, and `dataOrigin` (a VIRTUAL projection,
  never persisted).
- **Library** (`dsl/library/concepts.memql`) -- `artifact` is a thin INDEX row
  owning no content (memql#693); `file` is the only backing row with bytes
  (memql#4340); `fileChunk` holds its embeddings. All three declare
  `@rowAuthz(owner="ownerUserId", clusterOwner)`; the index's `source` enum is
  the UNION of every backing concept's ([library.md](docs/public/operate/library.md)).
  Archive is a soft delete and RESTORE its inverse (memql#4784), as a
  CLIENT-DRIVEN PAIR (a mirror of `archiveFileOnArtifactArchive` would close a
  cycle). `file.linkState` (epic memql#4783): `synced` is stamped on any
  upload naming `(uploadedFromWorkerId, uploadedFromPath)`, also the re-push
  key, and the engine only ever FLAGS. `watchedFolder` (memql#4841) is one
  folder on one of the owner's machines, swept by its cockpit; the PATH is the
  machine's to refuse (`policy.yaml` `backup.roots`, default-deny ->
  `originState=refused_by_policy`). An ABSENT `originState` is "no cockpit has
  reported yet", never `ok`.
- **Cluster** (`dsl/cluster/concepts.memql`) -- `node`, `nodeType`,
  `spawnEvent`, `cluster` / `database` / `identityProvider`, plus `deployment`
  (append-only, one timeline per deploymentId, #1872) and `deploymentNodeSpec`
  (per-node-type child; an empty `version` resolves against the deployment's
  engine version). Read the set via `nodeSpecsForDeployment`. **`database`
  and `identityProvider` are singletons at LITERAL ids**
  (`v1:cluster:database:primary`, `v1:cluster:identityProvider:primary`,
  `v1:cluster:cluster:self`, memql#4766), refreshed on every bff start under
  the `clusterInfraRefresh` gate, NOT `bootstrapCluster`. **Neither carries a
  `status` field**; do not re-add one -- probe live and say when you looked.
- **Observability** (`dsl/observability/`, every node) -- `codeProfile` (live
  per-FQN verbosity override), `invocation` (the `code_invocation` hypertable)
  and `codeMetric` (the `_1m` / `_1h` continuous aggregates), read through
  `codeMetricsInWindow` ([design](docs/internal/design/auto-generated-diagrams.md)).
  **`logLine` is the log store** (epic memql#4893): every node's log lines in
  the `log_line` hypertable, forwarded by `core/logger`'s fan-out sink and
  batched by `component/logstore` (bounded, non-blocking, drops on
  `memql_logs_dropped_total{reason}`), plus the OS front end's errors via
  `logsRecordClient`. Rows never enter the graph and never broadcast; the
  reads are `@sdk` builtins (`logsSearch`, `logsTail`, `logsSources`,
  `logsStatus`) floored at admin IN THEIR HANDLERS. `logger.Subject(concept,
  id)` is the seam between a line and its subject. Retention is the nightly
  `logsRetentionSweep` on the cron leader: archive to blob storage FIRST, and
  **no archive means no delete**. [logs.md](docs/public/operate/logs.md) ·
  [design](docs/superpowers/specs/2026-09-03-logs-design.md).
- **Identity** (`dsl/identity/concepts.memql`, every node) -- `user`,
  `authSession`, `magiclink`, `accessRequest`, `invitation`, `delegation`,
  `enrolmentToken`, and `identity`, a discriminated union keyed on
  `identityType`; the `passkey` variant's stored material is PUBLIC (a COSE
  key). [access-model.md](docs/public/operate/auth/access-model.md).
- **Two audit logs, not one** (memql#4328): `auditEvent` records DECISIONS and
  security signals, `authActivity` records routine MECHANICS (token rotations,
  grace-window accepts, PAT-authenticated requests), two orders of magnitude
  more numerous, on `@rowAuthz(owner="<field>", clusterOwner)` (a non-owner
  admin gets `authActivityForSelf`). Its `retiredTokenHash` is what
  refresh-token reuse detection keys on (memql#4329); retention
  `MEMQL_IDENTITY_AUTH_ACTIVITY_RETENTION_DAYS` (default 30), hard-deleted
  daily, so detection reaches back exactly that far.

## Feature Notes

The per-epic write-ups -- what each shipped feature area is, and the rules
about it the code does not say -- live in
[feature-notes.md](docs/internal/design/feature-notes.md): **email campaigns +
the sending engine**, **the work spine** (goals, runs, steps), **the proving
suite**, **workers** (computer use, the fleet router, the Fleet app),
**invitations**, **Nexus**, **views / layouts (retired)** and **planner /
knowledge / validation**. Read it before touching any of them.

Three of its rules reach far enough outside their own area to state here:

- **`v1:planner:plan`, `task` and `taskState` ARE RETIRED** (epic memql#5000).
  The work spine -- `v1:work:{goal,run,step,modelCall,approval,observation}`,
  every concept on the composite owner tier -- is the only model: `agent()` and
  `produceArtifact` open goals, a tool call is a `v1:work:observation`, and
  training is a goal. `v1:planner:responsibility` STAYS.
- **The run ceilings are enforced at the MODEL SEAM** (`component/memql`'s
  `modelSeam`, memql#5580), and a breach PARKS the run on a `budget` approval
  rather than failing it. Dollar ceilings EXCLUDE subscription and local spend;
  loop caps INCLUDE every call, whoever answered it.
- **MemQL OS has no runtime arrangement engine and is not getting one by
  default** (epic memql#4984): its answer is hand-built sections under the
  twelve interface rules in `clients/os/DESIGN.md`.

### Procedure learning (epic memql#5402)

Recordings generalized into parameterized constructs by a PURE module that
spends no model. `component/procedure` is a leaf module that requires NOTHING
but the standard library (a build-graph fact its own `purity_test.go` asserts;
every goalSignature it groups by is read off a row by the wiring), holding
canonicalize / symbolize / mine / structure / generalize / classify / score /
select as functions over values. `integrations/procedure` is the only half
that reads a row. Record: section 4 epic C of
[the recording-and-learning program](docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md).

- **Holes classify in D13's order -- data flow, then constant, then free** --
  on EVERY instance. `unexplained` (PARTIAL evidence) is the ONLY class that may
  reach the one bounded model call; a free hole asks nothing. A model's answer
  is parsed against a closed grammar and re-checked against every instance.
- **Compression is the score, two uses is the floor** (D14). A free parameter
  is priced ABOVE a data-flow hole; `Select` rewrites the corpus between
  acceptances.
- **The corpus is ONE owner's, read under their actor.** A disliked recording
  is excluded in the loader; a run with no `goalSignature` is skipped.
- **`goalSignature` is written LAST**, after the compile gate. Nothing
  auto-activates.

`@serverOnly` is refused on a builtin at parse, so both builtins carry their
gate in the HANDLER. `component/procedure/reference` is a research harness
(D6), skipped when `python3` is absent.

**Certification and replay (epic memql#5408)** put a learned procedure on a
ladder (candidate, shadow, canary, trusted, retired) decided by pure functions
in `component/work` (`Advance`, `DecideServe`, `Compare`) over the seeded
`v1:authoring:ladderPolicy:primary` row. **A person approves once, and only
the ladder writes the ladder**: shadow to canary is one `procedurePromotion`
approval pinned to `procedureHash`; the ladder fields are written with
internal origin only (`component/memql/construct_ladder_write_guard.go`). **A
replay run is never a recording** (`triggeredBy: procedure:<mode>`); a
recording inherits the goal's input WITHOUT the replay's own variable
(`component/work.GoalInput`). **A parameter is a value, never code**
(`component/procedure.Materialize`; a version whose parameter would be code
stays a candidate, `ReplayRisks`).
[learned-procedures.md](docs/public/operate/learned-procedures.md).

### Intervention, feedback and reuse (epic memql#5414)

A person steps into a finished run: `rerunStep` (a new VERSION with a
different level, model, effort, prompt or inputs), `moveRunHead`, `branchRun`,
`recordFeedback` (the AI Fluency framework's Discernment axes); long work is
cut into sections that ask the catalog before intelligence.
[intervention-and-feedback.md](docs/public/operate/intervention-and-feedback.md).

- **An override is ONE version of ONE step**, riding
  `common.RunContext.Override` for that execution only. Its prompt is the WHOLE
  session prompt only when an app session answered the version it replaces
  (`wholePrompt`).
- **The head is re-asserted, not only recorded**: moving it writes the chosen
  version again as the step's newest row-version (`reassertWorkStepVersion`),
  bringing the upstream it was computed from (`step.basis`).
- **A branch serves its prefix BY REFERENCE**; the prefix never executes.
- **A replay cannot honour an override**: a changed re-run of a
  procedure-served step hands the goal to the app
  (`integrations/procedure/rerun.go`).
- **Feedback is a `feedback` observation, and the validator never certifies.**
  `data.verdict`, `data.target.{stepKey, version}`, `data.axes`, `data.reason`;
  a dislike with no axis is refused, a later verdict is a new row. The answer
  validator writes a `decision` observation and `run.validation`, never a like
  (D22).

## Notes for Claude Code CLI

- Use [GLOSSARY.md](GLOSSARY.md) to find specific documentation, and read a
  tree's own CLAUDE.md (Key Directories above) before editing that tree.

### Makefile + shell-script convention

The Makefile is for **simple commands and target wiring**. Anything multi-step,
conditional, or long enough to need line-continuations gets extracted into a
shell script under `scripts/`, and the Makefile target becomes a one-liner that
calls it.

- **Stays inline:** single commands (`go build`, `go test`, `kubectl rollout
  restart`), short pipelines (~3 lines or fewer), `.PHONY`, target dependencies,
  simple variable substitutions.
- **Goes into `scripts/<area>/<name>.sh`:** conditionals, retry loops,
  multi-step orchestration, user-facing error messages -- anything "complex
  enough that you'd want to test it independently of make."

Shell-script rules: `#!/usr/bin/env bash` + `set -euo pipefail` (drop `-e` for
status reporters where one failure shouldn't abort the rest); **function-based
structure** with `main()` at the bottom, never a sequential blob; source a
shared `scripts/<area>/*.sh` helper for common functions; `.sh` extension,
executable. Reference: `scripts/k3d/{up,dev,status}.sh` behind one-liner targets.

#### Capability scripts (the hardened successor)

A **capability script** is a deploy/ops script that is also the deterministic
backend behind a DSL `action`, so it must run **identically** whether an
automation or a human invokes it. It adopts the capability-script contract
([capability-script-contract.md](docs/internal/design/capability-script-contract.md),
#2221) -- the convention above **plus**: non-interactive (a destructive
confirmation is an explicit `--confirm=<phrase>` param, never a prompt);
structured params in (`--flag=value` > stdin JSON via `--params-stdin` >
documented defaults; no positional args); exactly one JSON envelope on
**stdout**, all human logs on **stderr**; honest, stable exit codes (0 ok; 2
bad param; 3 refused; 4 prerequisite missing; 5 op failed); no decisions
inside (no branching on environment/version/role -- that lives in DSL `logic`;
only mechanical idempotency branches).

They `source scripts/lib/capability.sh` (`cap_init` / `cap_param` / `cap_ok` /
`cap_fail` / `cap_info`-to-stderr / `--print-spec`).
`scripts/lib/capability_contract_test.go` enforces the contract on every script
that sources the library and gates non-interactivity across
`scripts/{k3d,deploy,release,docs}`; the Go effect seam parses the envelope via
`deploycontrol.ParseCapabilityResult`.

### Documentation Style Guidelines

**No Emojis:** All documentation, skills, and CLI responses must use
professional formatting without emojis. Use checkboxes (`[ ]` / `[x]`), text
indicators ("SUCCESS:", "ERROR:", "WARNING:", "INFO:") and standard markdown for
emphasis. This applies to documentation files, skill outputs, CLI responses, and
all user-facing text.
