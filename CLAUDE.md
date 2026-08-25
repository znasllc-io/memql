# MemQL - the AI memory platform

**Type:** AI platform -- agents, automations, and voice on a time-series memory graph
**Language:** Go + MemQL DSL
**Stack:** PostgreSQL + TimescaleDB extension
**Purpose:** Run agents, automations, and voice against a time-series memory graph

> **Positioning is load-bearing, not marketing** (memql#3843). MemQL is an AI
> platform *built on* a time-series memory graph; it is not a database, and no
> public-facing file may say it is. The embedded TimescaleDB Community Edition
> is licensed under the Timescale License (TSL), whose §2.1(b) "Value Added
> Products or Services" grant is what makes self-hosting free for our model --
> and §3.10 prong (i) withholds that grant from a product that is "primarily
> [a] database storage or operations" product. Describing the storage layer
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
make dev PULL_INFRA=1        # ...also refresh infra images (postgres/azurite/livekit)
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
go build -tags voice -o bin/memql-voice .

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
- **integration** — talk to other DBs/services (`integrations/`). Shopify, email, telephony live here
- **pack** — client-agnostic product feature (Plugin SDK v1 / `examples/referencepack`). Intake "plugin" means this

`dsl/todos`, `dsl/calendar`, `dsl/campaigns` are **core**. Packs cannot shadow them.
`memql.RegisterPlugin` is the Go registration primitive. It is not a fourth runtime.

```
MemQL/
├── app/               Phased service bootstrap (Go). Build() orchestrator +
│                      one file per phase: config -> database -> engine ->
│                      integrations -> transport -> cluster
├── dsl/               Consolidated MemQL DSL tree, flattened to per-namespace
│   │                  per-construct files (see DSL Tree Layout below)
│   ├── <namespace>/   concepts / mutations / queries / specs / shapes /
│   │                  builtins / tools / prompts / automations .memql
│   └── _reference/    Per-construct authoring reference skeletons
├── integrations/      External services + DSL-callable capabilities (Go)
├── brand/             Visual identity as plain CSS custom properties. Imported
│                      by BOTH clients/portal (Vite) and component/identity/web
│                      (standalone Tailwind CLI) -- they share no package
│                      manager, and CSS variables are the one format both
│                      consume. Never copied: brand_shared_source_test.go fails
│                      the build on a --memql-* token, an @theme block or an
│                      @font-face defined outside it (memql#4266)
├── clients/           Surfaces built ON the platform -- the inward-facing
│   │                  mirror of integrations/. The engine carries exactly one,
│   │                  the worked example the memql-project template copies
│   └── portal/        MemQL Portal, served by component/edge as site #1
├── component/         Core Go components (memql, grpc, events, database,
│   │                  server, auth, edge, language, ...)
│   ├── bus/           Channel-based inter-component communication
│   ├── node/          Distributed node system (identity, peer mesh, bootstrap)
│   ├── architecture/  Auto-generated architecture model (UML/C4 from source)
│   ├── observe/       Per-invocation observability runtime (FQN-keyed)
│   └── envregistry/   Env-var registry: manifest, boot validation, domain
│                      derivations, legacy aliases, repo-root .env override
├── core/              Shared utilities (logger, env, id) + dslfs (the
│                      MEMQL_DSL_PATH on-disk override / embedded FS picker)
├── cmd/               CLI tools (healthcheck, memqlfmt, memqlmigrate,
│                      memqllint, frontdoorhosts, frontdoorpaths, ...)
├── deploy/k8s/        GitOps manifests: base + components + per-env overlays
├── scripts/           Shell scripts (k3d bring-up, deploy, release, install,
│                      migrations) + lib/capability.sh, the capability runtime
├── docs/              Documentation
├── docker/            Dockerfile + db init + nginx assets
└── .claude/           Claude Code project state. This repo is PUBLIC, so
                       .gitignore ignores `.claude/*` and negates back only
                       what should travel with the project (memql#3344):
                       skills/, commands/, agents/, settings.json are tracked;
                       settings.local.json, epics/, prds/ and worktrees/ are not
```

---

## Key Directories

Several directories carry their own CLAUDE.md, and it is the first thing to
read before editing that tree. This is a list, not a rule -- a directory
without one is normal.

| Directory | Purpose | CLAUDE.md |
|-----------|---------|-----------|
| `dsl/<ns>/*.memql` | Automations, queries, mutations, specs, tools, prompts, shapes per namespace | — |
| `dsl/providers/providers.memql` | AI provider configurations | — |
| `dsl/policies/policies.memql` | AI provider-selection policies | — |
| `integrations/` | External service integrations + DSL capabilities (Go) | [→](integrations/CLAUDE.md) |
| `clients/` | Surfaces built ON the platform (SPAs, landing pages, apps) | [→](clients/README.md) |
| `clients/portal/` | MemQL Portal -- the graphical ops console, served by `component/edge` as site #1. Nexus (`src/nexus/`) is its 3D surface | [→](clients/README.md) |
| `component/` | Core service components (Go) | [→](component/CLAUDE.md) |
| `component/language/` | The MemQL front end: lexer, parser, rewriter, AST, compiler, registries | [→](component/language/CLAUDE.md) |
| `component/node/` | Distributed node system (bootstrap, peers, mesh) | [→](component/node/CLAUDE.md) |
| `component/architecture/` | Auto-generated architecture model | [→](component/architecture/CLAUDE.md) |
| `component/observe/` | Per-invocation observability runtime | [→](component/observe/CLAUDE.md) |
| `sdk/go/` | Go SDK -- the public client surface | [→](sdk/go/CLAUDE.md) |
| `docs/` | Documentation | [→](docs/CLAUDE.md) |

---

## Documentation

**Start here:** [docs/public/overview/quickstart.md](docs/public/overview/quickstart.md) — get running in 5 minutes.
**Full index:** [GLOSSARY.md](GLOSSARY.md) — find any documentation.

**Core concepts:**
- [Component vs integration vs pack](docs/public/concepts/component-integration-pack.md) -- the three words; intake "plugin" means pack
- [Architecture](docs/public/concepts/architecture.md) · [Events](docs/public/concepts/events.md) · [Tech stack](docs/public/overview/tech-stack.md)
- [MemQL Language](docs/public/language/memql.md) · [Functions](docs/public/language/functions.md)
- [MemQL Authoring Rules & Gotchas](docs/public/language/authoring-rules.md) -- read before writing `.memql` files
- [Node Identifier Conventions](docs/public/concepts/identifiers.md) -- canonical `{concept}:{shortId}` internally vs the BARE-ids client contract at every wire seam (engine bare-ifies on egress, resolves bare args on inbound; clients never compose/parse/compare canonical ids), the `(concept, id)` keying rule, anti-patterns
- [LLM cost control (defense in depth)](docs/public/ai/llm-cost-control.md) -- the layered guardrails that make a runaway spend loop structurally impossible. Read before touching `ai_guard.go`, an LLM loop, or an automation that drives model calls
- [Tool ↔ Knowledge Domain Pattern](docs/public/concepts/tool-knowledge-domain-pattern.md) -- when a capability has operational knowledge, put it in a knowledge domain the tool requires, not in the agent prompt template

**Operations:**
- [Environment variables](docs/public/operate/env-vars.md) -- bootstrap envelope vs. concept-stored config; how to add / rotate / override
- [Auto-generated architecture diagrams](docs/internal/design/auto-generated-diagrams.md) -- static topology model + observe runtime + cockpit drill-down

**Tooling:** **memql-cockpit** -- terminal-native IDE and operations console.
Lives in its own repo at `github.com/znasllc-io/memql-cockpit`; consult that
repo's CLAUDE.md and Makefile.

---

## Development Workflow

### Development Environment (k3d + ArgoCD)

The k3d + ArgoCD cluster is the local dev topology (memql#2061 / E0 -- Argo
parity) and the ONLY supported local run path. It mirrors the cloud cluster
(AKS + ArgoCD + the k8s base in `deploy/k8s/`), so the same manifests and
reconciliation path run locally and in the cloud. Multi-node is the default
(#2067): use `make up SERVERS=2` + `make scale N=2` for full cross-node mesh
testing. Commands are in Quick Start above; the full k3d runbook and
port-forward reference is
[docs/public/operate/reproduce-the-cloud-locally.md](docs/public/operate/reproduce-the-cloud-locally.md).

Migrations run automatically on startup.

### Testing

```bash
make test                      # the whole tree
MEMQL_REQUIRE_DB=1 make test   # ...and make a missing database a FAILURE, not a skip
```

**Do NOT verify with `go test ./...`. It does not run the engine** (memql#4032).
This is a multi-module workspace -- `go.work` lists 49 modules -- and a relative
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
module; `Makefile`'s `ALL_PKGS` has been that since memql#3165, and CI subtracts
from the same selector via `scripts/ci/db-gated-packages.sh`.

The failure mode is silent and confidence-INCREASING: edit `component/memql`,
run the bare command, see `ok` across 64 packages, and report the change
verified. Nothing in the output hints that the package you edited was never
compiled into a test binary. `TestDocumentedTestCommandCoversTheEngine`
(`claude_md_test_command_test.go`) fails the build if this section ever
documents a command that misses the engine again.

**A second, independent way to get a meaningless green: db-gated tests skip.**
Every Postgres-backed case self-skips when it cannot reach a database, and
`MEMQL_REQUIRE_DB=1` is what turns that skip into a failure
(`component/database/dbtest`). Two traps:

- **An open port 5432 is not evidence of a database.** With the k3d cluster up,
  `k3d-memql-serverlb` publishes 5432, so the connection is accepted and then
  EOFs -- so it does not fail fast, it fails silently as "unreachable" and every
  db-gated case skips.
- The default DSN is `postgres://memql:memql_dev@localhost:5432/memql`. Point
  `MEMQL_DATABASE_DSN` at a real Postgres+TimescaleDB+pgvector, or run the
  db-gated trees the way CI does.

The trees carrying most of the engine's real coverage are owned by the
`db-tests` lane rather than by `make test`. The set changes (it has grown twice
since memql#4032 was filed), so ask the script rather than trusting a count
written down here:

```bash
# what CI actually runs (the canonical set lives in the script)
scripts/ci/db-gated-packages.sh --trees
MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=... go test -count=1 ./component/memql/...
```

### Image builds: LOCAL Docker for dev, BUILD SERVER for deploys (HARD RULE)

Where a container image is built depends ONLY on where it runs:

- **Local development** -- build images in your **local Docker** and import
  them into k3d via `make dev`. Fast, throwaway, never pushed to ACR.
- **Deploys to the CLOUD** -- images MUST be built on the **GitHub
  build server** (GitHub Actions, OIDC -> ACR `acrmemql`), NOT on an operator
  machine. This spans the repos in the project:
  - `memql` -> `.github/workflows/build-engine-images.yml` builds **every**
    node type (identity / bff / cognition / agent / planner / voice /
    workbench / mcp / edge) as one set of **product-agnostic** engine images.
  - the product's DSL-bundle repo -> a tiny **data-only bundle image** the
    engine mounts at runtime via `MEMQL_DSL_PATH`.
  - the product client repo -> its `build-spa-image.yml`.
  Each is `workflow_dispatch` on `main` with a `version` input; tags are
  immutable; the build server is the source of truth for deployable images
  (reproducible, native linux/amd64, provenance, no dev-laptop drift).

Do NOT hand-build + push release images locally (`az acr build`,
`make release`, `docker push`) for a cloud deploy -- that path is
superseded by the build server. A release is `{engine version, bundle digest,
client digest}` pinned in **one overlay** (`deploy/k8s/overlays/<env>`): build
the engine images, pin those three digests, merge -> ArgoCD reconciles. See
[docs/public/operate/deploy-bundle-runbook.md](docs/public/operate/deploy-bundle-runbook.md).

---

## Branch Workflow

MemQL uses a single long-lived branch: `main`. Core engine, wire protocol and
DSL all live here.

1. **Every change goes through a branch + PR. `main` refuses direct pushes**
   -- a repository ruleset, not a convention (`pull_request`,
   `required_status_checks`, `merge_queue`, `deletion`, `non_fast_forward`), so
   a one-line docs fix needs a PR exactly like a feature does.

   Branch, push, open the PR, let CI go green, then **enqueue it**:

   ```bash
   gh pr merge <n> --repo znasllc-io/memql   # bare: no strategy, no --delete-branch
   ```

   **The bare form is the whole instruction, and both flags you would reach for
   are wrong.** `--delete-branch` is REFUSED (`Cannot use -d or
   --delete-branch when merge queue enabled`) because the queue deletes the
   branch itself. `--merge` is ignored -- the queue's `merge_method` is already
   `MERGE`, so the strategy is not yours to pass.

   **It ENQUEUES rather than merges, and the wait is by design.**
   `min_entries_to_merge_wait_minutes` is 5 and it batches under
   `grouping_strategy: ALLGREEN`, so a PR sits at `OPEN` with `mergedAt: null`
   for minutes with nothing wrong. Re-running the command answers `is already
   queued to merge`, which is CONFIRMATION rather than an error -- reading it
   as one and "retrying" is the natural mistake.

   **A queued PR can go `DIRTY`, and it will stay there.** When a sibling lands
   underneath it, `mergeStateStatus` becomes `DIRTY` and does not resolve
   itself; rebase on `origin/main` and force-push. The failure is silent in a
   specific way: a watcher looking only for merged / failed / clean cannot see
   `DIRTY` at all, and its silence is indistinguishable from "still queued".
2. **Merging your own PR: the owner uses the BYPASS, never a settings change.**
   The ruleset requires a **code-owner review**, and that requirement stays on.
   It cannot be combined with self-approval, because **GitHub never lets a pull
   request's author approve it** -- there is no toggle for that at repository,
   ruleset, organisation or enterprise level, and on your own PR the Approve
   control is simply not rendered. So the policy "the owner's approval is
   required, and the owner may proceed on their own work" is expressed as one
   setting plus one bypass:

   ```
   require_code_owner_review: true                     <- the requirement, kept
   bypass_actors: RepositoryRole(admin), pull_request  <- the owner, on their own PR
   ```

   Use the script, which reports the policy it is bypassing before it acts and
   refuses on a red or unfinished build -- the bypass skips a REVIEW that cannot
   be given, never a failing check:

   ```bash
   scripts/dev/merge-as-owner.sh --pr=<n> --check   # policy + readiness, merges nothing
   scripts/dev/merge-as-owner.sh --pr=<n>           # merge
   ```

   **Do not "fix" this by lowering `require_code_owner_review`.** That removes
   the requirement for everyone, which is a different policy from the one
   intended, and it is the change a future reader will reach for first.

3. **Pre-release -- no backwards-compat shims or deprecation windows.** When a
   contract changes, fix both MemQL and the consumer at once and delete what is
   no longer needed. No legacy adapters, fallback paths, or "keep working while
   we migrate" layers.
4. **Stage files by explicit path** (`git add <file>`) -- never `git add -A` or
   `git add .`. The repo owner runs multiple Claude sessions against this
   working tree, and untracked files from another session must not get swept
   into your commit.

**What triggers a frontend team ping:** a change that alters a wire contract
the frontend depends on (removed/renamed endpoints, changed required request
fields, new required response fields, new gRPC message types the client must
handle to get a complete response). Call it out explicitly in the commit body
so the repo owner can relay it. Backend-internal refactors that leave the wire
identical -- file moves, renamed internal functions, which node owns a handler
-- don't need frontend coordination.


---

## Architecture & Tech Stack

### Core Technologies
- **Language:** Go 1.26.1+
- **Database:** PostgreSQL 16 + TimescaleDB
- **API:** gRPC (`MemqlService.Stream` is the primary surface) + HTTP for the documented exceptions (OAuth, health, file uploads, Polyphon room tokens) + WebSocket bridge to the gRPC stream for browsers (`/memql/ws`)
- **AI:** Centralized provider system (OpenAI, Anthropic). All AI ops on gRPC.
- **Auth:** in-house identity service (magic-link + JWT, JWKS-published)
- **Query Language:** MemQL DSL

### Deploy targets

MemQL ships **one installation shape** (epic memql#3943). There is no
staging-versus-production dimension inside the product: an operator who
wants a second environment installs a second instance, with its own
domain and its own ArgoCD. What varies is the deploy TARGET, which is a
different axis and carries its own field (`provider`):

| Target | Database | Service | Provider |
|--------|----------|---------|----------|
| **Local** | CloudNativePG in k3d | k3d + ArgoCD (`make up`) | `docker-local` |
| **Cloud** | Self-hosted CloudNativePG on AKS | Azure Kubernetes Service | `azure` |

**Key Principle:** the local cluster is completely isolated from any cloud
install's database -- they are separate installations, not environments of one.

### Hardware Requirements
Development happens on macOS and Linux (amd64/arm64) -- CI runs on
`ubuntu-latest`, and `scripts/dev/install-deps.sh`, `scripts/dev/proto-gen.sh`,
and `scripts/identity/build-css.sh` all branch on `darwin`/`linux`.

**Full tech stack details:** [docs/public/overview/tech-stack.md](docs/public/overview/tech-stack.md)

### System Architecture

```
Front door (TLS 443) -> bff gRPC :50051 (h2c)
                     -> bff-http :8085 (the documented HTTP exceptions)
   |
   MemQL Engine  <-  Automations System  <-  Functions System
   |
   +-- AI Provider Registry (OpenAI, Anthropic)
   |     +-- AI gRPC messages on MemqlService.Stream:
   |         AiChatMsg, AiSpeechMsg, AiTranscribeMsg, AiSuggestMsg
   +-- Integrations (cognition, audio, ...)
   +-- MemQL Sense (Tokenize, Complete, Diagnose, Hover, SignatureHelp)
   |
   PostgreSQL + TimescaleDB
   (time-series memory nodes; PK: (id, createdAt))
```

### Distributed Node Architecture (Cluster Mode)

MemQL uses **Go build tags** to compile separate binaries for each node type.
A tag selects which `app/build_*.go` runs, and therefore which integrations and
transport layers a node WIRES UP.

**Tags are a wiring mechanism, not a size mechanism.** Every node binary is
within ~5.5% of every other one: the tag gating stops at `app/`, so untagged
packages keep the heavy vendor set constant in every build (a `planner` binary
still links 79 `pion/*` packages via `integrations/telephony`). Never justify a
build tag by expected binary size -- the measurements are in
[docs/public/build/build-tags.md](docs/public/build/build-tags.md#binary-size).

```bash
go build .                       # bff        (default)
go build -tags voice .           # voice      (CGO_ENABLED=1 required)
go build -tags cognition .       # cognition
go build -tags agent .           # agent
go build -tags planner .         # planner
go build -tags edge .            # edge       (serves hosted sites + the portal)
```

The nine node types are identity, bff, cognition, agent, planner, voice,
workbench, mcp, edge. The mesh/product ones:

- **BFF** (default): backend for frontend, domain-specific API surface
- **Voice**: voice transport (audio WS, LiveKit)
- **Cognition**: cognition pipeline, Polyphon
- **Agent**: task execution, AI work, tool calling
- **Planner**: task planning and orchestration
- **Edge**: serves this cluster's hosted web surfaces -- every hosted SPA and
  the MemQL Portal itself (site #1, no special path) -- by resolving the
  request `Host` header to a `v1:platform:site` graph row (epic memql#3700)

Nodes discover each other via mesh and share one PostgreSQL + TimescaleDB
database. Inter-node communication uses the `NodeService` gRPC bidirectional
stream; events bridge across nodes with dedup and TTL.

**Build tag reference:** [docs/public/build/build-tags.md](docs/public/build/build-tags.md)

#### Multi-node is the DEFAULT -- design, implement, AND test for cross-node

The cluster (2 replicas per mesh node) is the runtime in the **cloud** and in
the local **parity cluster**, and it is the topology every feature must be
designed and tested against. Never reason about a feature as if it runs in a
single process. The blessed local repro is `make up SERVERS=2` + `make scale
N=2`; a cloud cluster spends most of its idle life at `make scale N=0`, and
parking it at one replica instead would make it unable to catch this bug class.

**When implementing:** any state/context/event that crosses a node boundary
needs EXPLICIT plumbing -- it does NOT travel implicitly.
- Session / in-memory state (caches, waiters, fields like `voiceAgentSpaceId`)
  lives on exactly ONE node. A different node handling a **proxied /
  forwarded** request (`AiForwardRouter`, `proxySI`, `NodeService` forwards)
  does NOT see it -- thread it through the message or metadata and resolve it
  on the receiving side.
- Every cross-node event-bus pub/sub needs a **routing rule**
  (`node.RegisterRoutingRule`) or it silently dies in cluster mode.
- Before calling a feature done, ask: *which node holds this state, and which
  node needs it?* If those differ, you have cross-node work to do.

**When testing:** a green single-node unit test is a FALSE signal for
cross-node behaviour. Tests MUST exercise the hop -- a handler running on a
session WITHOUT the originating node's local state, context surviving a
proxy/forward, an event consumed on a different replica. Add coverage to the
cluster-e2e harness (`test/clustere2e/`) and/or the proxy-path tests
(`component/grpc/ai_forward_test.go`); the test should FAIL against
single-node-assuming code and PASS with the cross-node fix. This has shipped
green single-node and broken in the mesh repeatedly (#1448, #1412, #1388). See
[docs/public/operate/reproduce-the-cloud-locally.md](docs/public/operate/reproduce-the-cloud-locally.md).

**memql#4352 closed the WORKER half of that class.** A cockpit machine's
`WorkerService` stream terminates on ONE agent replica, so at two replicas the
turn found the machine on a coin flip and reported `no_worker_available` for a
laptop the user could see was on. `WorkerForward*` on `NodeService.Stream` now
forwards the dispatch to the replica named by `connectedNodeId` and routes the
answer back by request id. Its gate is an IN-PROCESS hop test
(`integrations/agent/worker/forward_hop_test.go`) wiring the real router to the
real handler, not a `clustere2e` lane -- a live-cluster gate is skipped on every
CI lane and every developer machine, and a gate skipped by default cannot be
what stands between a feature and the bug it prevents.

#### Node image source: product-agnostic engine images + runtime DSL delivery (#2472)

**The engine is the whole platform.** Every node type ships as a
**product-agnostic engine image** from THIS repo's Dockerfile
(`BUILD_TAGS=<type>`). There are no per-product node images. Reusable
capabilities (chat, daily-space, avatar, ...) live in the engine as **generic,
DSL-configurable features**, never product code.

**Product DSL is delivered at runtime, not compiled in.** A product ships its
DSL as a tiny data-only **bundle image**; the `dsl-bundle` kustomize component
runs it as an init-container that copies the `.memql` tree into a shared volume
the node reads at `MEMQL_DSL_PATH`. A "bff" is just a plain engine `bff` node
fronting a product's bundle -- a deploy concern. A release is
`{engine version, bundle digest, client digest}`.

**What "never product code" is actually enforced by** -- two narrow guards, not
a general one, so know their edges (memql#3326):

- `TestEngineIsProductNeutral` (`product_neutrality_test.go`) -- a
  **banned-names list** of the specific product names this repo shed. It cannot
  notice a product arriving under a name nobody thought to ban.
- `TestClientsDirectoryIsAllowlisted` (`clients_allowlist_test.go`) -- an
  **allowlist of `clients/` inhabitants**. An unlisted directory fails.

Everything else -- generic-vs-product Go in `component/`, a product-shaped
concept in `dsl/` -- rests on review. Write new product-shaped code as a
DSL-configurable feature or keep it downstream; do not read the guards as proof
that anything unflagged is neutral. Genuinely-bespoke product Go (rare) becomes
a thin optional `bff/` pack module in the product repo. Full rationale:
[docs/internal/design/platform-consolidation.md](docs/internal/design/platform-consolidation.md).

#### Environment parity -- one topology everywhere (NON-NEGOTIABLE)

The local cluster and a cloud cluster run the **same topology, the same
deployment process, and the same connection model**. Only **configuration
values** and **hardware resources** differ -- never the shape of the system.

- FIXED everywhere: the node topology, the GitOps base+overlay+ArgoCD deploy
  path (`make up` locally applies the same manifests ArgoCD applies in the
  cloud), and the client connection (ingress -> TLS -> gRPC -> `bff`, dialed as
  `https://api.<domain>`).
- ALLOWED to vary (overlay/registry values, not architecture): image
  digests/tags, replicas/resources, domain, DNS source (hosts vs real), TLS
  source (mkcert vs cert-manager), ingress controller (traefik vs nginx
  annotations), secrets source.
- **Reject in review:** port-forward-as-connection, target-specific commands
  (`run-local`), `if env=="local"` branches, or a second way to deploy.

Ask of any change: *is this the shape of the system (→ base/component,
everywhere) or a value (→ overlay)?* The standard:
[docs/public/operate/environment-parity.md](docs/public/operate/environment-parity.md).

**ONE INSTALLATION SHAPE (epic memql#3943).** MemQL has no
staging-versus-production dimension. An operator who wants a second environment
installs a second instance -- its own domain, its own database. There is one
cloud overlay (`deploy/k8s/overlays/cloud`), one ArgoCD Application (`memql`),
one namespace (`memql`), and everything in that overlay is a VALUE over
`deploy/k8s/base`.

**Therefore: no `if env == "..."` in engine code.**
`TestNoEnvironmentBranchingInEngineCode` (`environment_branching_test.go`) fails
the build on engine code so much as NAMING `prod` / `production` / `staging`, in
any form -- comparison, switch case or map key -- and its exemption map is EMPTY.
`development` / `local` stay outside that gate: they distinguish deploy TARGETS
(k3d vs AKS), which carry their own field, `provider` (`docker-local` | `azure`).

**Local cluster (memql#2061 / Epic 0).** `make up` brings up k3d + ArgoCD +
`deploy/k8s/overlays/local` + seeded secrets; `make dev [NODE=<type>]` rebuilds
after source edits; `make down` tears it down. It is the ONLY supported local
run path. What makes it parity rather than a lookalike:

- **Same manifests, same reconciliation as AKS.** Scale to 2 replicas per mesh
  node with `make up SERVERS=2` + `make scale N=2`; each pod carries a unique
  `MEMQL_NODE_ID` via `fieldRef: metadata.name` exactly as in the cloud.
- **Clients connect exactly as in the cloud** -- the `api.memql.localhost`
  traefik front door (TLS on 443, mkcert wildcard, h2c gRPC to `svc/bff:50051`).
  There is NO local-only port-forward in the connection path; raw kubectl
  port-forwards remain for low-level debugging only.
- **The domain is a VALUE** (memql#3593). `make up DOMAIN=lab.example.com`
  serves any domain, seeded as the single `MEMQL_DOMAIN` key every node derives
  its issuer, CORS origins and OAuth redirect URIs from
  (`component/envregistry/domain.go`). **No file under `deploy/` names a domain.**
- **The engine bff is a COMPONENT, not the base** --
  `deploy/k8s/components/engine-bff`, so a product cluster bringing its own
  `bff-<product>` never collides with a base-shipped bff.
- **`make status` is the litmus** -- per-pod node ids, plus a check that every
  identity replica publishes the same JWKS keyset (divergent keysets fail
  roughly half of all auth, memql#3400).

Runbook: [docs/public/operate/reproduce-the-cloud-locally.md](docs/public/operate/reproduce-the-cloud-locally.md).

#### Client-tool relay (agent → browser, across nodes)

The tool registry supports **client-executed tools** (tools whose implementation
runs in the browser). In single-binary mode the agent's `InvokeClientTool`
writes directly to the browser's stream and parks on a session-scoped waiter. In
cluster mode the agent and browser live on different nodes, so the
`ClientToolCall` envelope needs a cross-node round-trip via the graph event bus:

1. Cognition intercepts `ClientToolCall` in `consumeAgentTurnStream` and inserts
   a `v1:cognition:client:tool:request` node (`emitClientToolRequest`).
2. Browsers subscribed to the space dispatch the tool locally and insert a
   matching `v1:cognition:client:tool:response` (`emitClientToolResponse`).
3. Cognition subscribes to those responses, wraps the payload in a
   `ClientToolResult`, and calls `AgentForwarder.ForwardContinuation` so the
   agent's service-scoped waiter fires and the parked tool loop returns.

The relay lives in `integrations/cognition/client_tool_relay.go`.

### Component Bus (Channel-Based Communication)

Components communicate via typed Go channels carrying protobuf-defined messages
(`component/bus/bus.proto`). This provides true concurrency, backpressure, and
symmetry with the distributed gRPC model.

```
  gRPC/HTTP ──► EngineRequests ──► MemQL Engine ──► Database (internal)
                                       │
                                       ├──► IntegrationRequests ──► Integration Dispatcher
                                       │
                                       └──► EventPublishCh ──► Event Bus ──► Subscribers
                                                                    │
  All Components ──► TelemetryCh ──► Telemetry Collector            ▼
                                                              Automations
```

- **Protobuf messages** -- All inter-component messages defined in `component/bus/bus.proto`
- **ReplyTo pattern** -- Request-response over channels via embedded reply channel
- **Default buffer** -- 64 items per channel, configurable via `ChannelConfig`
- **Telemetry hooks** -- Channel fill-level, send/drop counters for future dynamic sizing
- **Ready() signaling** -- All components expose `Ready() <-chan struct{}` for parallel startup

---

## Endpoint Protocol Policy (gRPC-First)

**IMPORTANT: This policy is a hard requirement for all MemQL development.**

gRPC is the **default and required** protocol for all internal and service-to-service
endpoints in MemQL. HTTP endpoints are allowed **only** when an external protocol
requirement makes gRPC impossible.

### Decision Criteria

When adding a new endpoint or capability to MemQL, apply this decision tree:

1. **Is this a service-to-service call?** (e.g., frontend to MemQL, bridge agent to MemQL)
   - YES: **Must be gRPC** -- add a new message type to `memql.proto`
2. **Is this consumed by a browser client?**
   - YES: Route through the existing WebSocket bridge (`/memql/ws`), which tunnels to `MemqlService.Stream` gRPC -- **still gRPC under the hood**
3. **Does the external service require HTTP?** (e.g., OAuth callbacks, webhook handlers)
   - YES: HTTP is allowed as a documented exception (see below)
4. **When in doubt:** Ask the user. Default answer is gRPC.

### Allowed HTTP Exceptions

These endpoints **must** remain HTTP because an external protocol requires it.
Each was approved individually; the shared reason is that the other party
dictates the wire (a browser, a mail client, a probe, a third-party webhook).

| Category | Endpoints | Reason |
|----------|-----------|--------|
| **Auth (identity service)** | `/login`, `/auth/magic-link`, `/auth/complete`, `POST /auth/landing`, `GET /auth/magic-link/status`, `POST /auth/magic-link/finish`, `/auth/logout`, `/oauth/token`, `/auth/refresh`, `/.well-known/jwks.json`, `POST /auth/webauthn/{register,login}/{begin,finish}`, `POST /device/code`, `GET+POST /device`, `GET /enroll` | OAuth 2.0 / magic-link needs HTTP redirects, browser form posts and JWKS publishing. WebAuthn is a **browser API** -- there is no gRPC form of "the user touched their security key". RFC 8628 device grant is **defined over HTTP**. `GET /enroll` is a page a person opens from a link, arriving before any application code exists to speak a protocol. The three memql#4302 magic-link routes: `POST /auth/landing` is the form post a GET used to do (a GET now renders and never changes state, so mail scanners stop burning links); `/auth/magic-link/status` is the requesting tab's poll, gated on the `memql_ml` cookie and 404 to anyone without it; `POST /auth/magic-link/finish` must be a real form POST because the reply is a 303 the tab must NAVIGATE. All identity routes are declared on the identity server's own route table, not `component/server`'s, so the bff path generator is untouched |
| **Health check** | `/healthz` | Docker and Kubernetes probes expect HTTP GET |
| **WebSocket upgrades** | `/memql/ws`, `/memql/audio` | Browsers need an HTTP upgrade |
| **File uploads** | `/spaces/{id}/attachments` | Multipart form-data maps poorly to gRPC |
| **Site bundle publish** | `POST /sites/{id}/bundles` (bff only) | Same reasoning as attachments (memql#3713): a CI job hands over an arbitrary tree of files -- unknown paths, count and content types -- which is what multipart exists to carry and a fixed protobuf schema does not. `component/edge.Publisher` makes it atomic: the bundle lands under a content-addressed version prefix and only then does the site row's `bundleRef` flip. Authorization is a `class="service_account"` JWT; declared in `HandlerAuthorizedPaths()`, never `PublicPaths()`. Served by the bff, never the edge (which is wildcard-routed by site hostname) |
| **Inbound webhooks** | `POST /inbound/{source}` (bff only) | The third party dials US and will POST to a URL and nothing else (memql#2957). Deny-by-default source allowlist + per-source HMAC; `HandlerAuthorizedPaths()`. See [inbound-delivery.md](docs/public/operate/inbound-delivery.md) |
| **One-click unsubscribe** | `GET+POST /unsubscribe` (bff only) | Here the third party is the RECIPIENT'S MAIL CLIENT (memql#3348). RFC 8058 is a contract with Gmail / Outlook / Yahoo, and without it there is no one-click unsubscribe. The GET/POST split is load-bearing: mail clients PREFETCH links, so a GET with the side effect unsubscribes people who never clicked. Authorization is an HMAC-signed token carrying (owner, recipient, campaign), verified before any row is read, with the impersonated identity coming out of the signed payload rather than a parameter. `HandlerAuthorizedPaths()` + `SelfAuthenticatedPaths()`. See [campaign-sending.md](docs/public/operate/campaign-sending.md) |
| **Library artifacts** | `POST /artifacts`, `GET /artifacts/{id}/content` (bff only) | Upload is the multipart reasoning already recorded for attachments, approved by the owner in this epic's design record (memql#4341, D1) the way memql#3713 approved the bundle route. `GET .../content` is the export side of the same bytes and STREAMS through the bff after re-resolving the row under the caller's actor -- never a redirect, because there are no signed URLs here and a redirect would move authorization from the graph to whoever holds a URL. Both are ordinary AUTHENTICATED routes, so they appear in none of the three aggregates; `server.ArtifactPaths()` routes them. Cap: `MEMQL_LIBRARY_MAX_UPLOAD_BYTES` (default 256 MB) |

### The front door's HOST set is generated too (memql#3767)

The host set is DERIVED from the closed **role** set plus the platform's own
site, not maintained as a list:

| Role | Host |
|---|---|
| api | `api.<domain>` |
| identity | `identity.<domain>` |
| mcp | `mcp.<domain>` |
| sites | `portal.<domain>` (site #1, its own exact rule), `*.<domain>`, plus the apex |

**Every host is a SINGLE label under the domain, and that is a ROUTING fact.**
An Ingress wildcard matches exactly ONE label, so the one `*.<domain>` rule
routes every present and future site to the edge.

**It is NOT a certificate fact (memql#4224).** The cloud ClusterIssuer solves
HTTP-01 only: ACME cannot issue a wildcard over HTTP-01, and ONE wildcard
dnsName fails the WHOLE order. So the front-door certificate names EXACT hosts
only (`api.`, `identity.`, `mcp.`, `portal.`, the apex); every Ingress lists
exactly its own exact rule hosts under `tls`; and the union of those lists
equals the dnsNames (`deploy/k8s/overlays/frontdoor_hosts_test.go` gates all
three). The wildcard RULE stays with no certificate behind it: a customer site
hostname on the cloud front door needs its own Certificate and exact-host
Ingress WHERE THE OVERLAY DECLARES NO DNS-01 ISSUER. Both cloud overlays now
declare one (memql#4347) plus a second, wildcard Certificate that the edge
Ingress's wildcard rule carries, so a freshly deployed site is live over TLS
with no operator step; the render gate reads the SOLVER rather than the
issuer's name, so #4224's exact-host rule holds by default. The portal carries an exact rule because
ingress-nginx builds a certificate-bearing server block per RULE host, never
per tls host.

`cmd/frontdoorhosts` writes `front-door.generated.yaml` into each instance
overlay; `component/envregistry/domain.go` composes the node's own issuer /
CORS origins / redirect URIs from the SAME rule through `component/frontdoor`;
and `component/memql`'s SeedMaterializer seeds the portal site row's hostname
from it. One derivation, three consumers -- a second copy would disagree, and
the disagreement is an issuer nothing is served at, which presents as "sign-in
is broken" with every manifest looking correct.

Adding a ROLE is a design change, not a configuration change. The LOCAL
overlay's five front-door files stay hand-authored (traefik, not nginx) but are
gated against the same derivation. Locally the mkcert pair is still a wildcard,
which is a TLS-source VALUE and the one place local is more permissive than the
cloud: **a site that works over https locally is no evidence it has a
certificate in the cloud.** Details:
[docs/public/operate/front-door.md](docs/public/operate/front-door.md).

### How an HTTP path reaches the front door (GENERATED, memql#3703)

Every HTTP path above needs its own Ingress rule, and **that rule list is
generated, not authored** (`cmd/frontdoorpaths`, emitted between the markers in
`deploy/k8s/overlays/local/api-front-door.yaml`). An ingress controller's
backend protocol is a per-**Service** setting, so the bff's gRPC edge (`bff`,
:50051, h2c) and its HTTP edge (`bff-http`, :8085) are two Services over one
Deployment -- and a path with no rule falls through to the `/` h2c catch-all.
**That is not a 404: it is an HTTP/1.1 request handed to an h2c backend, which
fails with a protocol error naming nothing.**

Three things about the derivation are load-bearing:

- **It is per-ROUTE, not per-authentication-tier.** `PublicPaths()` +
  `HandlerAuthorizedPaths()` + `SelfAuthenticatedPaths()` answer *who may reach
  this without a bearer*. An **authenticated** HTTP route appears in none of
  them, which is how `/spaces/` and `/polyphon/*` came to be served by the bff
  and routed by nothing. The generator unions the aggregates **and** every
  per-route declaration a bff-tagged build mounts.
- **It over-approximates for a path the bff does NOT serve.** Identity-only
  paths are kept: a rule for a path this backend does not serve costs a 404,
  while omitting one for a path it does costs a protocol error naming nothing.
- **That pricing INVERTS for a path the bff DOES serve, and this is the trap.**
  There, adding a rule makes the endpoint externally reachable, and for
  anything in `PublicPaths()` (which the verifier bypasses) that means
  exposure. `/metrics` is the case: unauthenticated *because* in-cluster-only,
  and mounted on every node type. Hence a fourth classification,
  `servedButNotExternallyRouted` (`/metrics`, `/api/concepts*`).
  **"When in doubt, include" applies only to the previous bullet.**

Two gates make it non-recurring: `TestFrontDoorPathsAreNotStale`
(`make frontdoor-paths-check`) and `TestEveryServerPathDeclarationIsClassified`,
which AST-scans `component/server` for every `func …Paths() []string` and fails
when one is classified by none of the four maps. **So a new HTTP path
DECLARATION either reaches the front door or breaks the build.**

Note the word *declaration*: a route mounted through `handleRoute` with an
inline path literal and no `*Paths()` declaration is invisible to the
generator, and the boot check that would catch it
(`AssertUnauthenticatedSurface`) runs only when the node installs **no**
verifier -- which the bff does. **Declare new HTTP routes with a `*Paths()`
function**; that is what puts them inside the gate. Do not hand-edit the
generated block, and do not "simplify" the generator back to the three
aggregates -- both changes look like cleanups.

### gRPC-Only Endpoints

Everything below lives on `MemqlService.Stream`; cross-node proxying rides
`AiForwardRouter`.

| Category | gRPC Message Types | Handler |
|----------|--------------------|---------|
| **AI service-to-service** | `AiChatMsg`, `AiSpeechMsg`, `AiTranscribeMsg`, `AiSuggestMsg` (space / group / agent) | `ai_handlers.go` |
| **Streaming transcription** | `AiTranscribeStreamStart` / `Chunk` / `End` + `AiTranscribeStreamDelta` / `Complete` | `ai_transcribe_stream.go` -- multi-message flow keyed by `request_id`, forwarded BFF -> Voice via `AiForwardRouter.ForwardContinuation` |
| **Polyphon internal** | `PolyphonRoomTokenMsg`, `PolyphonStatusMsg`, `PolyphonUtteranceMsg` | `polyphon_handlers.go` |
| **Concepts API** | `ConceptsListMsg`, `ConceptsSubscribeMsg` (+ `follow=true` -> `ConceptsRegistryDelta` stream, memql#4238) | `concepts_handlers.go` |
| **Guest invites** | `SendGuestInviteMsg`, `ResolveGuestInviteMsg`, `ResendGuestInviteEmailMsg`, `CancelGuestInviteMsg` | `guest_handlers.go` |

### For AI Agents and Developers

When implementing new functionality in MemQL:

1. **Never add new HTTP endpoints** without explicit user approval
2. **Default to gRPC** -- add message types to `component/grpc/memql.proto`
3. If you believe HTTP is needed, **ask the user first** and document the reasoning
4. Reference this section when making the decision
5. All new gRPC messages follow the existing multiplexed stream pattern:
   - Add request type to `MemqlClientMessage.oneof payload`
   - Add response type to `MemqlServerMessage.oneof payload`
   - Add handler in `component/grpc/server.go`

---

## AI Integration

MemQL centralizes all AI operations through a pluggable provider system:
unified interfaces (`ChatAIProvider`, `VisionAIProvider`, `TTSAIProvider`,
`ChatStreamProvider`) over OpenAI (chat, vision, TTS, STT) and Anthropic (chat,
vision) backends. Provider records live in `dsl/providers/providers.memql`;
selection is the configured default, or per-request via the `provider`
parameter.

**The Anthropic credential is a static key locally and workload identity
federation in the cloud** (epic memql#4333). The engine presents the pod's
projected Kubernetes token and the SDK exchanges it for a one-hour bearer, so
no long-lived vendor key is at rest. All four ids or none: a partial config
REFUSES BOOT rather than falling back to a key the cutover deletes. Cutover,
deny reasons and `memql provider-auth check`:
[docs/public/operate/auth/anthropic-federation.md](docs/public/operate/auth/anthropic-federation.md).

### AI Endpoints (gRPC on `MemqlService.Stream`)

- `AiChatMsg` / `AiChatResult` / `AiStreamChunk` -- chat completions (streaming + non-streaming)
- `AiSpeechMsg` / `AiSpeechResult` -- text-to-speech
- `AiTranscribeMsg` / `AiTranscribeResult` -- speech-to-text (batch)
- `AiTranscribeStreamStart` / `Chunk` / `End` -> `AiTranscribeStreamDelta` / `Complete` -- real-time streaming transcription
- `AiSuggestMsg` / `AiSuggestResult` -- carries `domain` ∈ {spaces, spaceTitle,
  agents, groups, groupDescription, agentCardSummary, spaceCardSummary,
  groupCardSummary, knowledge}. The rich `spaces` / `agents` / `groups` domains
  return full payloads (description + suggested members + roles); `spaceTitle`
  and `groupDescription` are the lightweight one-line paths used by Create
  Space / Create Group; the three `*CardSummary` domains generate the LLM body
  that lands on a canvas-creation card.

Cross-node proxying (BFF -> Voice, BFF -> Agent, ...) rides `AiForwardRequest` /
`AiForwardResponse` on `NodeService.Stream`. Handlers:
`component/grpc/{ai_handlers,ai_transcribe_stream,ai_forward}.go`. Handlers emit
a short error id via `generateErrorId()` (format `ERR-{6 hex}`), visible in slog
JSON output as `"errorId":"ERR-..."`.

### Voice + Video Pipeline (Go voice-agent)

The realtime voice + video channel is owned by the **Go voice-agent** in
[`integrations/voice/agent/`](integrations/voice/agent/), shipped as the
`voice-agent` subcommand of the `memql-voice` binary (`make voice`,
CGO_ENABLED=1, `-tags voice`). It joins LiveKit rooms as the General
Assistant's voice + video participant. Specialists are text-only by design;
they never publish into the LiveKit room.

```
LiveKit room
   |
   +-- OpenAI Realtime STT (user audio in)
   |              v
   |        memql gRPC client         (VoiceAgentTurnRequest -> Delta)
   |              v
   |        memql cognition           (BYO conductor + agent tool loop)
   |              v
   +-- OpenAI TTS                     (token-by-token input streaming)
   |              v
   +-- Anam or Simli avatar           (lip-synced video)
```

Two executors, selected by `MEMQL_VOICE_EXECUTOR`: `realtime` (default --
OpenAI gpt-realtime speech-to-speech) and `cascade` (the STT -> cognition ->
TTS path above). Key files under `integrations/voice/agent/`: `config.go` /
`bootstrap.go` (env + `class="voice_agent"` token resolution), `grpc_client.go`
(the `VoiceAgent*` contract on `MemqlService.Stream`), `cascade.go` /
`stt_pipeline.go` / `tts_pipeline.go` / `turntaking.go`,
`realtime_executor.go` / `realtime_lifecycle.go` / `realtime_budget.go`,
`persona.go` / `grounding.go` / `instructions.go`, and
`avatar_room_voice.go` (`//go:build voice`) whose CGO-free vendor REST core
lives in the shared `integrations/avatarvendor` package.

Auth: identity-issued `class="voice_agent"` JWT bearer, pinned to the
`VoiceAgent*` message surface by
`component/grpc/voice_agent_stream_interceptor.go`. The voice-agent cannot
write graph rows directly; memql does that server-side. Mint via
`make voice-agent-token`, or self-bootstrap with `MEMQL_NODE_BOOTSTRAP_TOKEN` +
`MEMQL_IDENTITY_VERIFIER_BASE_URL` + `MEMQL_VOICE_AGENT_INSTANCE_ID` (see
`docs/public/operate/auth/voice-agent-jwt.md`).

Env (full list in the runbook): `MEMQL_GRPC_ADDR`, `LIVEKIT_URL` /
`LIVEKIT_API_KEY` / `LIVEKIT_API_SECRET`, `MEMQL_OPENAI_API_KEY` (required),
`MEMQL_VOICE_EXECUTOR`, `MEMQL_VOICE_ROOM_NAME`,
`MEMQL_VOICE_IDLE_TEARDOWN_SECONDS` (default 60 -- stops the auto-join
dispatcher wedging on an empty room, #1378), `MEMQL_VOICE_MAX_ROOMS` (default
8, #1395 -- the dispatcher serves each human-occupied room in its own isolated
session; cross-replica double-serve is prevented by skipping rooms that already
contain a `-ga` participant), `MEMQL_REALTIME_*`, `VOICE_AGENT_TOKEN`,
`MEMQL_AVATAR_VENDOR` (`anam` default / `simli` / `none`) + the vendor key.

**Deployment gotcha:** LOCALLY the voice lane uses a **LiveKit Cloud** project
(Epic #2184) and is GATED on the operator's credentials (memql#2416) -- without
`LIVEKIT_URL` / `LIVEKIT_API_KEY` / `LIVEKIT_API_SECRET` in the environment at
`make up` / `make secrets`, voice + voice-agent scale to 0 with a loud warning,
because the binaries fail-fast on the missing env by design. Export the creds
and re-run `make secrets` to enable. The cloud stays self-hosted
(`deploy/k8s/base/livekit.yaml`) via ESO/Key Vault.

**Canonical voice catalog (`integrations/voice/voices.go`).** Every agent
carries a canonical, provider-agnostic voice name (alto / soprano / tenor /
baritone / ...) on `providerConfig.voice.voiceId`, plus a `gender` enum;
cognition resolves canonical -> provider voice id at TTS-publish time via
`MEMQL_POLYPHON_VOICE_PROVIDER`. Voice is auto-assigned at agent creation and
never edited by the user. Builtins: `voicePickForGender` + `voiceResolve`. The
General Assistant is hardcoded to "alto"; specialists pick from whatever the
owner's other agents have not used.

**Per-agent audio + video control.** `v1:agents:agent.audioControl` +
`videoControl` (`always_on` | `always_off` | `mirror_user`, default
`mirror_user`) seed the defaults; `v1:cognition:audioOverride` +
`videoOverride` carry per-(space, agent) session overrides. The voice-agent
consults override -> default for video at session start; audio mirrors the
user's mic state under `mirror_user`. Mutations `setAgentAudioOverride` /
`setAgentVideoOverride`; queries `audioOverridesForSpace` /
`videoOverridesForSpace`. `avatarPersonaId` + `avatarVendor` carry the
vendor-issued persona id minted from a still image -- empty for legacy or
specialist agents, which makes the voice-agent disable the avatar plugin and
fall back to audio-only.

### Cognition (Routing + Conductor)

Cognition decides whether and which agent should respond to an utterance, then
dispatches the turn. The text path uses a **single LLM brain**: the conductor
(`dsl/cognition/prompts/conductorTurn.tmpl`) emits both the routing decision
(fitScore / turnMode / handoff / severity) and the per-agent plan (primary /
sequence / chime-ins / instructions) in one structured-output call. The
standalone router LLM call fires only for voice utterances now
(latency-sensitive); fast-path mention dispatch bypasses both. Lives in
`integrations/cognition/{cognition_handler,conductor_consult,ai_router}.go`.

**Capability-aware routing.** Both the conductor and the voice-path router see
each candidate agent's tool list, so a specialist whose keywords loosely match
an action it has no tool for gets penalized. Tool-fit mismatch drops fitScore
by 0.4+; a total tool gap routes to the GA with `turnMode=escalation_notice`.

**Conversational continuity.** The conductor receives an explicit
`lastResponder` input (computed in `conductor_consult.go`). The continuity
meta-principle in `conductorTurn.tmpl` requires the primary to stay with that
agent when the user's turn is a follow-up shape ("ok cool", "btw", "what
about") and there's no @-mention or domain pivot. Plugs the "GA jumps in to
defer to the specialist" failure mode.

**Greet-on-join pacing.** `integrations/cognition/greet_on_join.go` serializes
greetings per-space: 3s initial delay, 4s minimum gap between consecutive
greetings. That serialization is process-local; cross-replica exactly-once is
enforced by `dispatchGate.tryGreet` (#1386), the same Postgres advisory-lock
gate as `tryDispatch` but keyed on (space, agent) under a distinct lock class.
The greeting directive is "familiar" for ALL agents -- every agent is one the
user created and named, so the "Hi, I'm X" opener is forbidden across the board.

### Agent reply envelope (`respondToUser`)

Every user-facing chat reply from an agent is delivered through a single
structured-output envelope, not free-form prose. The agent ends every turn with
a sentinel `respondToUser` tool call carrying `{response, citations[]}`; the
streaming tool loop intercepts the call by name (no engine executor exists for
it), parses the args as `Envelope`, and uses that as the turn's final text +
citations. See `integrations/agent/envelope.go` and
`integrations/agent/streaming.go`; the prompt enforces it via the OUTPUT
CONTRACT block at the top of `dsl/cognition/prompts/cognitionReply.tmpl`.

`citations` is a list of `{domainId, matchedPhrase}` pairs naming
knowledge-domain sources the agent drew from; cognition stamps them on the
inserted `v1:cognition:utterance.citations` field. The frontend wraps each
`matchedPhrase` substring with a clickable chip linking to the named domain.
When the agent used no trained sources, citations is an empty array.

### Coding Agent -- the seam, and its one inhabitant

The container-executor seam spent its whole life EMPTY (memql#4120):
`RegisterContainerExecutor` existed, `Task.executionSurface="containerExecutor"`
+ `executorBackend` existed, and nothing in the tree called the registry -- so
a `containerExecutor` Task had nowhere to land. **`cockpit-app` is now its
first and only inhabitant** (epic memql#4358): it runs Claude Code or Codex
headless on a machine the USER owns, through the worker stream, with MemQL's
tools reachable from inside the app over MCP.

- **The seam.** `component/planner`'s `RegisterContainerExecutor(name, exec)`.
  Lookup keys on the part BEFORE the colon, so a Task naming
  `cockpit-app:claude-code` reaches the one registered `cockpit-app` backend
  and the app id rides the suffix -- growing the app list is a value change,
  not a release. `ValidateExecutorBackend` refuses an unregistered name at
  TASK CREATION rather than at dispatch: the old behaviour produced a Task
  that looked queued, sat there, and failed much later with an error naming a
  registry lookup rather than the typo.
- **The backend.** `integrations/agent/worker/cockpitapp.go`, registered from
  `init()` under the `agent` build tag -- only an agent node holds worker
  streams, so only an agent node can serve one. It reuses `preDispatchCheck`
  UNEXPORTED, in the same package, on purpose: an app run needs exactly the
  gates `workerHost` needs (per-task approval, kill switch, standing scope,
  classifier), and a second copy of those gates is a copy that drifts.
- **Legacy fields, still present, still unused by anything here.**
  `v1:agents:agent.claw` (bool) + `clawWorkspace`, read by
  `integrations/cognition/ai_responder.go`'s `ClawCapable()`; display strings
  in `integrations/cognition/tool_labels.go` for `clawExecuteTask` /
  `clawReadFile` / `clawListFiles` / `clawSearchCode`. No `tool claw*` exists
  under `dsl/`, no sidecar in any manifest, and no `NEMOCLAW_*` / `OPENCLAW_*`
  / `CLAW_*` env var. These are NOT what `cockpit-app` uses -- do not wire
  them together on the assumption that they are.

### Local apps as execution surfaces (epic memql#4358)

Delegating a task to an app the user already pays for, on a machine they own.
Full record:
[docs/public/operate/local-apps.md](docs/public/operate/local-apps.md).

**Transport is the worker stream; MCP is the back-channel.** The engine cannot
dial a machine behind NAT -- the stream the cockpit opened outward IS the
tunnel. Each run is handed a per-run credential and the `mcp.<domain>`
endpoint so the app can use MemQL's tools.

Four rules carry the design, and each has a failure mode that motivated it:

- **The runnable app set is CLOSED in the engine** (`claude-code`, `codex`). A
  cockpit may report any id; unknown ids are stored on the registration and
  produce no routing label, so a newer cockpit never makes the engine attempt
  a protocol it does not have.
- **A machine is selectable for an app only when it is BOTH `allowed` (the
  machine's own `policy.yaml apps.allow`) and `signedIn`.** Otherwise
  selection commits a plan to a machine that then refuses the run. Selection
  itself is the **Fleet router** (`integrations/agent/worker/router.go`, epic
  memql#4349) asked for the `app:<id>` label; nothing here picks between
  machines, because a second selector disagrees with the first. A session runs
  only on the replica holding that machine's stream -- the app-session envelope
  has no cross-node forward yet, so a machine on a sibling replica is SKIPPED
  during selection rather than failing the run.
- **A run is a SESSION, not a dispatch.** `AppSessionStart / Chunk / Control /
  End` on `WorkerService.Stream`; a `ToolDispatch` carries one timeout and
  returns one result, and a headless `claude -p` runs for an hour emitting
  output the whole way. Chunk `seq` is monotonic and out-of-order or duplicate
  chunks are DROPPED -- a transcript is a record, and interleaving a replayed
  chunk corrupts it in a way no later reader can detect.
- **Delegation is a PREFERENCE WITH A FALLBACK** (`v1:worker:delegationPolicy`).
  If no machine with an allowed, signed-in app is online, the task runs
  in-process. A plan never waits for a laptop to wake up.

**The back-channel credential's `sub` is the OWNING USER's id.** That is the
whole security story: the delegated app reads over MCP as that user, so row
authz applies to it exactly as to their browser, and the service-account
interceptor's surface pin plus `role=system` keep it off every credential
mutation and every cluster-owner gate. It is minted through
`POST /node/bootstrap` with `tokenClass="app_session"`, which WIDENS what the
bootstrap secret buys -- previously machine principals only. Four things
narrow it: the named user must exist, the TTL is hard-capped at 8h, the
surface stays read/query-pinned, and the session id is the token label.

**It is NOT revocable, and nothing should imply otherwise.** The DB-free
JWKS verify path is what lets it work on every node without a lookup, so
there is no row to strike and revoking one token means rotating the cluster
signing key. Standing in for revocation: the short hard-capped lifetime, the
cockpit deleting the MCP config file at end, and renewal-in-place so no single
bearer is ever long-lived.

**Subscription spend is counted and does not burn the dollar ceiling.**
`v1:router:call.billing` + `executionSurface`, `plan.tokenSpentSubscription`.
The two caps want opposite answers: the DOLLAR ceiling must EXCLUDE
subscription tokens (MemQL was not billed, so counting them parks a plan over
money nobody was charged -- and the more the user leans on what they already
pay for, the sooner their plans would stop), while the LOOP caps must INCLUDE
the call (a runaway loop that routed through a subscription is still a runaway
loop). Billing falls to `unknown` whenever the app's usage report OR the
machine's subscription signal is silent; it is never inferred, because the
number the owner asked for is only worth having if silence stays visible.

**The cockpit half lives in `memql-cockpit`** -- app detection, `apps.allow`,
the session runner, the MCP config writer and its cleanup, the Library
pull/push, and the `open` kind. This repo fixes the protocol and the engine
side.

### Workers (computer_use_headless / computer_use_embodied)

The "workers" feature lets agents drive the user's own machine via a tool
surface: shell exec, filesystem, HTTP fetch, and (under the computer-use build)
mouse + keyboard + screenshot. Runbook:
[docs/public/operate/workers-runbook.md](docs/public/operate/workers-runbook.md).

The capability is split into two mode-specific slugs so the headless slice and
the embodied slice can be granted independently. Authorization stays unified --
both modes act on the user's machine, so the consent is one decision. The
sandboxed first-choice surface for headless work is the Workbench, below.

- **Agent capabilities:** `computer_use_headless` -> `workerHost` + the
  cross-cutting trio (`workerStatus`, `requestComputerUseScope`,
  `canvasPublish`); `computer_use_embodied` -> `workerComputer` + the same
  trio. Slug expansion map: `component/memql/worker_caps.go`.
- **Tools:** `workerHost` (HEADLESS) and `workerComputer` (COMPUTERUSE), both
  discriminated-union tools under `dsl/worker/`.
- **Gateway:** `WorkerService.Stream` gRPC on the agent node. Auth via
  worker-specific tokens (`mql_wkr_<43 base64url chars>`, the `worker_token`
  variant on `v1:identity:identity`). The interceptor admits these tokens on
  the WorkerService path only and rejects them everywhere else.
- **Token mint:** server-side via `CreateWorkerTokenMsg` / `RevokeWorkerTokenMsg`
  on `MemqlService.Stream`. The plain token comes back in the reply ONCE; only
  the SHA-256 hash persists (`component/identity/workertoken/`).
- **Worker side:** `memql-cockpit worker run`, a run mode of the Cockpit binary
  built from the `memql-cockpit` repo. macOS TCC / Linux X11 pre-flight via
  `memql-cockpit-computeruse worker setup`.
- **Per-user routing, and NO machine id.** Every worker is owned by exactly one
  `v1:identity:user`; only agents in that user's sessions reach it. The dispatch
  builtins take `requireLabels` / `preferLabels` and **no `workerId`** (design
  D4): an agent says what the work NEEDS, the owner's policy decides where it
  lands, so **a hallucinated machine id is a failure mode this surface does not
  have**. `no_worker_available` names every machine and why it was ruled out.
- **Permission model:** three layers checked BEFORE dispatch -- agent
  capability flag, standing scope on
  `v1:agents:agentAuthorization.computerUseScope` (observe / full -- `interact`
  is a RETIRED tier kept in the enum for old rows and read as `full`),
  per-Plan kill switch on `v1:identity:user.preferences.computerUseEnabled`.
  Out-of-scope calls transition the calling Plan to `awaitingFeedback` with
  `feedbackReason=scope_elevation_required`. Consent is decided BEFORE routing,
  so routing only ever chooses among machines the user already consented to; the
  card names the requirement AND today's choice, because an Allow covers the
  task on any machine that matches (D6).
- **The router** (`integrations/agent/worker/router.go`, epic memql#4349).
  `v1:worker:routingPolicy` -- one active row per user, ABSENT for most, and the
  router then applies `firstFit` + `nextMatching`, exactly the pre-router
  behaviour. Four strategies, all STABLE sorts over REGISTRATION order (from the
  row, never from connection order) so every replica agrees: `firstFit`
  (registration order), `roundRobin` (oldest `lastSelectedAt` -- a timestamp on
  the row rather than a counter, so two replicas rotate identically with no
  shared state), `leastLoaded` (`activeCount` against the capability's cap,
  tie-broken on absolute count so an uncapped machine does not take everything),
  `labelMatch` (most `preferLabels` hits). `fallback=nextMatching` re-picks ONLY
  after a refusal BEFORE start (D5) -- an exec that lost its stream may have run.
  Policy and agent requirements are AND-ed; a conflict is left UNSATISFIABLE
  rather than resolved toward either side.
- **Two label maps, and they must not become one.** `labels` is what the cockpit
  reports and is OVERWRITTEN from the `Register` message on every reconnect;
  `operatorLabels` is what the owner set and no register/heartbeat path writes
  it. An operator tag in `labels` is erased by the machine carrying it, roughly
  whenever the lid closes. `refreshWorkerRegistration` enforces the split by NOT
  NAMING the field -- `update{}` is a read-merge, so the prohibition is the
  ABSENCE of a line and "completing the field list" is what would break it
  (`displayName` is absent for the same reason). Routing matches the MERGE,
  operator side winning.
- **`online` is DERIVED, never stored:** unrevoked AND non-zero `lastSeenAt`
  within `OnlineWindow` = 2 x `HeartbeatBatchInterval` = **30s** (the flush is
  15s, the cockpit's own beat -- it was 60s, and nothing read `lastSeenAt`
  BECAUSE it was a minute stale). Exactly two implementations,
  `component/worker.IsOnline` and `clients/portal/src/fleet/online.ts`, held
  together by `TestFleetOnlineWindowMatchesPortal`. Deriving it from the
  in-memory registry is refused: that answers "connected to ME", and the fleet
  needs "connected to ANY replica".
- **Cross-node dispatch (memql#4352):** `connectedNodeId` names the replica
  holding the stream (stamped on register + every flush, cleared on disconnect);
  any other replica forwards over `WorkerForward*`. `refused_before_start` is the
  re-pick predicate and the one wire field that must never be guessed. The
  receiver re-checks only what it alone can know -- ownership against the
  verified `ForwardedAuthority` (never the envelope's owner field) and
  revocation -- because the consent gates already ran on the sender. No enable
  flag: local dispatch here is not a degraded path, it is a call that cannot
  work.
- **Row tier + borrowed authority.** `v1:worker:registration` and
  `routingPolicy` declare the composite `@rowAuthz(owner=..., clusterOwner)`, so
  the operator can support a machine they do not own. The READ gate has no
  internal-origin bypass: an unstamped read returns ZERO ROWS, not an error. A
  worker authenticates as `worker:<id>`, so `component/worker`'s store runs every
  registration read and write under `auth.ContextWithUserActor` for the owner the
  `worker_token`'s identity row named -- the `createAuthActivity` shape. It must
  NOT stamp internal origin, and is deliberately absent from `call_origin.go`'s
  allowlist: every context in that package descends from a worker's own inbound
  stream.
- **Operator surface:** `/fleet/machines` in the portal -- pair a machine,
  rename it (`displayName`), edit its operator labels, revoke it, edit the
  routing policy, and read each call's `routing` record (policy, strategy,
  candidates considered in try order, why each was rejected, what it was
  rerouted from).
- **Audit + hardening:** security signals on `v1:identity:auditEvent`;
  per-call telemetry on `v1:worker:invocation`
  (`WORKER_INVOCATION_RETENTION_DAYS` default 90); per-call rlimits on Linux +
  Darwin via `policy.shell.max_*`; optional setuid drop via
  `policy.shell.run_as_user`; loopback-only metrics at `127.0.0.1:9100/metrics`.
- **Install:** `scripts/install/install-{mac,linux}.sh`.

### Workbench (workbench_use)

The "workbench" is the default first-choice surface for any HEADLESS work an
agent needs to do -- writing files, running shell commands, fetching URLs. It is
a per-Plan sandboxed Linux working directory in the cluster; nothing on the
user's machine is touched. Computer-use is the FALLBACK for headless work the
workbench cannot do (macOS-only tooling, computer-use control, files already on
the user's computer). See
[docs/public/operate/workbench-runbook.md](docs/public/operate/workbench-runbook.md)
and [docs/internal/ops/workbench-production.md](docs/internal/ops/workbench-production.md).

- **Agent capability:** `workbench_use`. Universal -- injected into every role's
  `lockedToolSlugs`. No scope grants, no kill switch; the blast radius is the
  per-Plan directory tree.
- **Tools:** `workbenchHost` (discriminated by `action`: exec / fs_read /
  fs_write / fs_list / fs_stat / http_fetch). Lives in a product DSL bundle,
  not the engine tree; the wire path goes through the `workbenchDispatchHost`
  builtin in `dsl/workbench/builtins.memql` to
  `integration.workbench.dispatchHost`.
- **The environment hint and the reroute (memql#4353).**
  `workbenchDispatchHost` takes an OPTIONAL `environment { os, needs[] }`, with
  `needs` from a closed four-value set (`display` / `gpu` / `macos_tooling` /
  `user_files`) naming exactly the things a workbench is not. A mismatch returns
  a typed `environment_mismatch` carrying the unmet needs, having run NOTHING --
  it replaces a failure that arrived three layers down and named nothing (a
  `defaults read` on Linux, an xdotool with no `DISPLAY`). An unknown need is the
  SEPARATE code `invalid_environment_hint`, so a typo can never read as "the
  workbench cannot do this" and send a call to somebody's laptop. Omitted means
  no hint, and there is deliberately no default -- a guessed one would refuse
  calls that would have worked. On a mismatch the tool loop re-dispatches the
  SAME call to the fleet and **the dispatcher's existing gate decides**: only
  `denied_no_per_task_approval` / `denied_by_scope` raise the consent card;
  everything else is the answer, `kill_switch_engaged` included (a deliberate no
  is not a missing card). The knowledge corpus's ban on silently switching to the
  user's machine stands; what is removed is re-ASKING for consent already given.
  `needs` -> scope/labels lives in `integrations/agent/worker/scope.go`, beside
  the ladder it reads: `user_files` alone is `observe`, everything else and
  anything unrecognised is `full`.
- **Per-Plan workspace:** under `MEMQL_WORKBENCH_ROOT/{planId}/` (default
  `/var/lib/memql/workbenches/`), lazy-provisioned on first call, persisting
  across calls within a Plan, torn down on Plan terminal status via the
  `releaseWorkspaceOnPlanTerminal` automation.
- **Concept:** `v1:workbench:workspace` -- per-Plan row carrying status
  (provisioned / released), storageRoot, lifecycle timestamps, plus `nodeId` and
  `ownerUserId` (memql#4354). It declares the composite
  `@rowAuthz(owner=..., clusterOwner)`; `ownerUserId` is stamped from the parent
  plan's `requestedBy` at provision time, and a call whose `planId` does not
  resolve to a readable plan is now REFUSED (`workspace_owner_unresolved`)
  rather than run -- a row written under a blank actor is readable by nobody,
  including the operator answering "where did my file go".
- **Replica affinity (memql#4354).** Base runs 2 workbench replicas and a
  workspace is a FILESYSTEM, which does not follow the request. `nodeId` names
  the replica holding the directory and the peer picker prefers it, falling back
  to any-fit only when that node is gone. Any-fit alone gave one plan two
  directories on two disks and told neither side -- a call wrote a file and the
  next call, landing on the other replica, did not find it, both reporting
  success. On node loss the orphan row is released `node_lost` and a FRESH
  workspace is provisioned: **files are NOT migrated**, because there is nothing
  to copy them from. The log line and the row state ship; a canvas card does not
  (canvas is pack-only, so a node-loss card must come from a product-bundle
  automation off the `node_lost` release).
- **Modes.** Cluster mode is the deployed default: a dedicated `workbench`
  node-type binary hosts the workspaces and agent nodes route via
  `NodeService.Stream` (`WorkbenchForwardRequest` / `Response`). Base sets
  `MEMQL_WORKBENCH_REMOTE=1` on the agent; the dialer needs
  `MEMQL_WORKER_PEERS=workbench=<addr>`. **The remote flag is an ASSERTION, not
  a preference:** with it set and no reachable workbench peer, a call is REFUSED
  (`no_workbench_peer`) rather than run on the agent's own disk -- it used to
  degrade silently, which is how a dropped peer seed stayed invisible for its
  whole life. In-process fallback is `MEMQL_WORKBENCH_REMOTE` unset, or the
  explicit `MEMQL_WORKBENCH_LOCAL_FALLBACK=1` under the remote flag.
- **Operator surface:** `/fleet/workbenches` in the portal -- the replicas and
  the per-plan workspaces on each, live and released, with `node_lost` spelled
  out. `/fleet/machines` is its worker counterpart. Both are live: the
  `graph.node.*` events for `v1:worker:registration`, `v1:worker:routingPolicy`
  and `v1:workbench:workspace` carry broadcast routing rules
  (`component/node/routing.go`) because those rows are written on the agent and
  read on the page the bff serves -- without them default-deny leaves the list
  correct on load and frozen after, which looks like it is working.
  `v1:worker:invocation` is excluded on volume grounds.
- **Routing preference:** `dsl/cognition/prompts/cognitionReply.tmpl` and the
  `workbench` knowledge domain (auto-attached via `replier.go` when the
  expanded tool list includes `workbenchHost`) instruct the agent to prefer
  workbench over computer-use, and to surface a "workbench can't do this --
  needs computer use" message rather than silently retrying.

---

## Authentication

The in-house **identity service** (`component/identity`) is the authentication
provider for the cluster. It runs as its own node-type binary (`make identity`)
and owns magic-link auth, WebAuthn passkeys, enrolment tokens, OAuth-style
token endpoints (`/oauth/token`, `/auth/refresh`), the JWKS feed at
`/.well-known/jwks.json`, a public web UI (`/login`, `/auth/complete`,
`/setup`, `/legal/*`, `/me/*`), and PAT issuance for CLI clients
(`mql_pat_<...>`).

Other binaries (bff / voice / cognition / agent / planner / workbench / mcp)
verify identity-issued JWTs locally via the per-node verifier
(`component/identity/verifier`), which fetches JWKS on a 5-min background
refresh and on demand for unknown `kid` headers. They never see the private key.
`MEMQL_IDENTITY_VERIFIER_BASE_URL` configures the verifier;
`MEMQL_IDENTITY_BASE_URL` configures the identity service itself.

**What is worth knowing before touching this tree:**

- **Magic links are device-bound and approve-on-click** (epic memql#4300).
  Issue mints a nonce whose digest is stored as `magicLinkRequest.bindingHash`
  and hands the plaintext to the requesting browser as `memql_ml`
  (`HttpOnly; Secure; SameSite=Lax; Path=/auth`). A link only COMPLETES in a
  browser holding that cookie; a click anywhere else only APPROVES, and the
  requesting tab polls and finishes itself. **A session can only ever land on
  the device that asked for it.** `GET /auth/complete` renders and writes
  nothing (prefetchers are harmless); consume is a compare-and-swap under a
  Postgres advisory lock, which is load-bearing because approve-on-click gives
  one request two legitimate finishers.
- **`signInPolicy` on `v1:identity:user`** (memql#4304) is `any` (default) or
  `passkey_only`, which disables sign-in LINKS: a request writes no row, sends
  no link, redirects identically (no enumeration signal) and mails a notice.
  Enabling it requires an active passkey, server-enforced. Owners/admins can
  RESET it to `any` over `IdentityAdminMsg` -- one direction only, so an admin
  cannot lock a colleague out of their own account. `sharedMailbox` is a hint
  from a local-part heuristic that gates nothing and drives copy.
- **A new-sign-in email fires on every `authSession` row** (memql#4305), from
  the one seam that creates them. No action link, deliberately: an
  unauthenticated revoke link mailed to a shared mailbox is a DoS handle for
  everyone who can read it. Refresh rotations never send it.
- **Passkeys are usernameless** (memql#3407): the login challenge carries an
  EMPTY `allowCredentials` and resolves the assertion by credential id alone,
  which is why that id is unique cluster-wide. RP id derives from
  `MEMQL_IDENTITY_BASE_URL`, never from the request Host. The challenge holds
  the in-flight OAuth context server-side, so `finish` mints an auth code and
  **no client learns which factor ran**. A sign-count regression is refused and
  audited as the cloned-authenticator signal. Revoke on `/me/devices` is a SOFT
  delete -- the row is audit history and its credential id must stay taken.
- **Enrolment tokens** (`mql_enr_<43>`, memql#3408) are the task that removes
  email from the critical path: single-use, TTL'd, authorizing exactly ONE
  action -- register a passkey as the named user. `GET /enroll` renders; the
  ceremony presents `Authorization: Enrolment <token>`. Issued by an
  owner/admin from the portal's People surface, or by the install wizard via
  `memql enrolment-token mint` inside the identity pod.
- **The admin web app is gone.** What remains at `/admin/*` is the sign-in
  pages and a root that answers `410 Gone`; the screens live in the MemQL
  portal, gated by `component/identity/adminops` over `IdentityAdminMsg`.
  `DeployControlService` exists only on the identity node, but a bff FORWARDS
  the deploy RPCs over `NodeService.Stream` carrying the caller as a verified
  `ForwardedAuthority`, so owner-only gates run against the originating human
  rather than the relaying node (`component/grpc/deploy_control_forward.go`).

**Authentication is ON by default everywhere** (local and cloud alike -- env
parity). The master toggle is `MEMQL_IDENTITY_ENABLED`: on verifier-consuming
nodes it defaults to `true` and is set explicitly `false` ONLY to disable auth
for troubleshooting -- the node then skips the verifier and admits every stream
as a synthetic `local-dev` cluster owner
(`component/grpc/local_dev_stream_interceptor.go`), with a loud boot-time
SECURITY warning and the `memql_auth_enabled` gauge pinned to 0. It is a config
value present everywhere, never an architecture branch; **never set it false in
a cloud cluster.** Disabling auth is the toggle, NOT blanking
`MEMQL_IDENTITY_VERIFIER_BASE_URL` (an empty verifier URL fatals the node).

**Two operator credentials, deliberately separate since memql#3519.**
`MEMQL_MASTER_KEY` DECRYPTS; `MEMQL_OPERATOR_KEY` AUTHENTICATES the
`Authorization: Operator <key>` bearer that admits a stream as a synthetic
cluster owner. They were one value, which made a key the installer wrote into a
world-readable `~/.bashrc` a cluster-owner bearer token over the network. No
fallback -- an unseeded cluster refuses operator streams.

See [docs/public/operate/auth/](docs/public/operate/auth/):
- [access-model.md](docs/public/operate/auth/access-model.md) -- enforcement layers and role spectrum.
- [user-provisioning.md](docs/public/operate/auth/user-provisioning.md) -- registration modes and magic-link flow.
- [identity-service.md](docs/public/operate/auth/identity-service.md) -- operator-side env vars + key management.
- [operator-credential.md](docs/public/operate/auth/operator-credential.md) -- `MEMQL_OPERATOR_KEY` + rotation sequencing for both keys.
- [recovery-key.md](docs/public/operate/auth/recovery-key.md) -- the owner BREAK-GLASS credential (epic memql#3958): `mql_rec_<43>`, bound to one owner, authorizing exactly one action (register a passkey as that owner) and REFUSED while the owner still holds a usable sign-in route. Single-use: redeeming spends it and mints an unclaimed successor, so a leaked key is worth one passkey registration and the cluster is never without a route back in.
- [service-account-jwt.md](docs/public/operate/auth/service-account-jwt.md) -- the `class="service_account"` machine identity (#691): the deploy gate / automation credential that verifies on the BFF/mesh via JWKS (where a PAT can't), surface-pinned to the read/query path.

---

## DSL Tree Layout

The DSL tree is **flattened per construct**: every namespace gets one
directory under `dsl/<namespace>/`, and within it each construct kind
is consolidated into a single `<construct>s.memql` file (e.g.
`dsl/cognition/queries.memql`, `dsl/identity/concepts.memql`,
`dsl/providers/providers.memql`). The flattened tree is produced
by the [`scripts/restructure-by-construct`](scripts/restructure-by-construct/main.go)
regenerator. Authoring reference skeletons live under
`dsl/_reference/` (`_concept`, `_shape`, `_spec`, `_trait`, `_agent`).
Loaders read through `Source()`, which routes through
[`core/dslfs`](core/dslfs/dslfs.go).

### `MEMQL_DSL_PATH` — runtime product-DSL delivery

`MEMQL_DSL_PATH` mounts **additional product-DSL domains from disk at
boot**, so a **product-agnostic engine image runs a product's DSL with zero
compiled-in product code** (platform-consolidation, #2472). When it is set,
`dsl.MountRuntimeDomainsFromEnv` (called before the first tree walk) scans
the root for product-domain sub-directories and registers each into the
unified tree via `RegisterTree(domain, os.DirFS(<root>/<domain>))` — the same
call the embedded tree uses (`dsl/embed.go`), sourced from disk instead of the
embed FS.
No-op when unset.

Layout mirrors the embedded tree — one directory per product namespace:

```
$MEMQL_DSL_PATH/
  <productDomain>/
    concepts.memql  queries.memql  mutations.memql  tools.memql  ...
    prompts/*.tmpl
```

Semantics:
- **Adds new domains.** A directory whose name collides with a core embedded
  domain (e.g. `cognition`) is skipped — the embedded tree owns that
  namespace. `MEMQL_DSL_PATH` delivers *product* domains, it does not patch
  core ones.
- **Fail-loud.** The mounted tree loads through the same strict-boot gate as
  the embedded tree: a malformed construct refuses boot (`MEMQL_DSL_ALLOW_SKIPS`
  is the operator break-glass).
- **Directories beginning with `_` / `.` are skipped** (soft-disable /
  hidden), matching the walker convention.

Delivery: a product ships its DSL as a tiny data-only bundle image; an
init-container copies the `.memql` tree into a shared volume the node reads
at `MEMQL_DSL_PATH` (see `deploy/k8s/base`). Also handy for dev hacking
(point at an in-tree product `dsl/`) and test fixtures.

## DSL dependency tree

How the DSL constructs lean on each other. Each layer can only depend
*downward*; cycles are rejected at load time.

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
                          '-- Policies  (provider selection)
```

**One line each** (each construct has its own full section below):

- **Concepts** are pure schema; every other construct references one or more concept ids.
- **Shapes** are reusable field-projection templates, `@row` and/or `@actor`. No composition verb.
- **Specs** are atomic boolean predicates, signature-bound to one shape XOR concept. The binding picks the eval strategy: concept / `@row` → SQL `WHERE` fragment; `@actor` → in-process context-spec. A `trait` is the one deliberately-unbound row predicate.
- **Mutations** write via the bare `insert { }` / `update { }` block. One write per body.
- **Builtins** wrap Go integrations behind a declarative schema.
- **Providers** are AI vendor + model + auth records; **prompts** pin a default provider and render templates over it.
- **Queries** stitch concept + filter (specs) + projection (shapes) + args into a typed read.
- **Automations** are event-triggered side-effects; they consume the layers above and never the reverse.
- **Tools** are the AI-facing surface of queries + mutations + builtins.
- **Policies** are empty-bodied AI provider-selection records. Caller-based authz / feature-gating decisions are **specs**, not policies.

**Construct files live under `dsl/<namespace>/<construct>s.memql`** — one
consolidated file per construct kind per namespace; policies are consolidated
in `dsl/policies/policies.memql`.

## Argument resolution

All DSL constructs share one model for declaring inputs and one
namespace pair for reading them. `ctx` is gone from the author
surface entirely.

**How args get declared (the canonical authoring surface):**

| Construct kind | Where args go |
|---|---|
| Query / mutation / logic / automation | `args { ... }` sub-block inside the body |
| Builtin / tool / prompt | Body fields directly — the body IS the schema |

`args { ... }` field syntax: `<name> <type> [@required] [@enum("a", "b", ...)] [@maxLength(N)] [@pattern("re")]`. Omitting
`@required` makes the field optional. Describe the field with a `///` doc
comment on the line above it — `@description` and `@default` are both rejected
at load (memql#3336, #991).

**How args get read inside the body:**

| Name pattern | Source | Available in |
|---|---|---|
| `args.X` | Caller-passed arg declared in `args { ... }` | every body |
| `actor.X` | Resolved auth context (`userId`, `role`, `identityId`, `isClusterOwner`, `primaryEmail`, `now`) | every body |
| `now` | RFC3339 timestamp captured at eval start | every body |
| `partition` | Active partition for this call | every body |
| `config.X` | Allow-listed config (`component/config/policy_exposable.go`) | every body |
| `X`, `id`, `concept`, `type`, `createdAt`, `createdBy`, `schema` | Row fields / intrinsics | queries' `filter` + `shape` only (SQL pushdown) |

For automations, `args` is the automation's own declared `args { ... }`
block: at fire time the trigger payload is bound INTO that contract and
validated against it (`@required` / type / `@enum` / `@pattern`), and a
violation refuses the run rather than binding a partial map
(`component/automations/args_binding.go`, memql#2352). An automation with no
args block binds nothing. The triggering **event** rides its own `event`
envelope (`event.topic` / `event.kind` / `event.payload.<field>`), which a
step conventionally forwards to logic as `logic name ( event )`; the logic
declares `event` in its args block and reads `args.event.payload.<field>`.

**Declared and used, in both directions.** An `args` field declared but
never referenced is refused at load, and (memql#3626) so is an `args.X`
a body READS but never declares. The second direction covers two failures
one silence used to hide: an author typo (`args.userld`) whose field is
simply absent from the write, and a caller-supplied undeclared name that is
bound and written having passed no declared-schema check at all —
`validateFunctionArgs` iterates DECLARED fields, and `rejectUnknownArgs` is
gated behind the MCP boundary.

**Reserved engine names.** `now`, `actor`, `partition`, `config`,
`trace` are reserved as top-level identifiers. An `args` field that
collides with one of these names is rejected at load time — keeps
the resolution rules unambiguous. The **call site** refuses the same
names in argument position (memql#3626): since no args block may declare
one, `mutation m(now: 1)` could never bind and was silently dropped. A
repeated argument name (`m(a: 1, a: 2)`) is refused for the same reason
a repeated annotation argument is — the map collapses last-wins, so the
value a reader sees is not the value the engine uses.

**Retired author-side forms (all rejected at parse time).** Every
construct is authored in the struct form. These shapes are gone and the
parser refuses them with a migration hint — do not write them, and do
not "restore" them when you see one in an old diff:

- `func (Query|Mutation|Spec|Tool|Prompt|Provider|Builtin) NAME(ctx any)`
  — receiver-function wrapping. (`func (Receiver)` survives only as the
  internal rewriter target the engine's parser consumes; authors never
  write it, and `ctx` in it is a placeholder identifier — bodies
  reference `args.X`.)
- The `@use*` annotation family (`@useConcept`, `@useShape`, `@useQuery`,
  `@useMutation`, `@useLogic`, `@useBuiltin`, ...) — replaced by file-top
  `use` imports.
- `@concepts(...)` / `@shape("name")` bindings — replaced by the
  two-identifier construct signature.
- `@input { ... }` — the prompt body IS the field list.
- `include` in a shape body.
- `;`-AND / `,`-OR filter separators, `has`, and the `?.` optional-chain
  prefix.

Only `dsl/_reference/*.memql` still shows these, deliberately, as
don't-do-this skeletons.

## Policies

The live `policy` construct is an **AI provider-selection record**:
empty-bodied, annotated with `@primary` / `@fallback` /
`@maxLatencyMs` / `@preferredRole`, consolidated in
`dsl/policies/policies.memql` and consumed by the AI Router to pick
chat/voice/embedding providers. That is the only policy surface with
live constructs.

```memql
@primary("streamClaudeSonnet")
@fallback("stream54Pro")
@description("Default chat policy for non-operator agents.")
policy balancedChat { }
```

**There is no decision-policy tier.** Auth / feature-gating / vendor
decisions live in Go (`component/safety` ships the risk×scope decision
matrix) and in **specs** — use a bare spec conjunct for caller-based
boolean checks (admin / owner / permission), which run as in-process
context-specs or compile to SQL. Do not reach for `policy` for these;
`engine.EvaluatePolicy` and `func (Policy)` do not exist.

## Key Concepts

### Authorization model

Per-row authorization is the only gate (see
[docs/public/operate/auth/per-row-authz-audit.md](docs/public/operate/auth/per-row-authz-audit.md)).
Every query and mutation in the DSL classifies as **owned** (filter
on `ownerUserId == actor.userId`), **granted** (relationship
predicate gates on actor.userId), **admin** (cluster-owner spec), or
**public** (`@public` annotation). The classification test in
`test/dslconformance/conformance_test.go` hard-fails on any new unclassified
construct.

**Row admission also gates SUBSCRIPTIONS** (memql#4309). A `graph.node.*`
event reaches a subscribed stream only if the same function that admits the
row on a read admits it for that stream's actor -- so a concept's declared
tier decides what its live feed delivers, and a concept that declares
nothing admits everyone on BOTH paths (which is the standing undeclared long
tail, not a subscription defect). A `granted` row cannot be decided against
one row, so it arrives id-only with `payload_omitted` for the client to
re-read through the authorized path. Non-graph subscription kinds
(`TELEMETRY` / `MESSAGE` / `AI_STREAM` / `ALL`) carry node-level events with
no row owner to decide by and are owner/admin-only at subscribe time
(memql#4311).

A fifth declaration FORM composes two buckets:
`@rowAuthz(owner="<field>", clusterOwner)` -- the owner, or a cluster owner
(memql#4312). It is the owned tier with the admin gate ORed in, not a new
tier, and it exists because a plain `owner=` tier has no cluster-owner
bypass -- so declaring an operator surface plain-owned hides every other
user's rows from the operator too. The write guard ignores the second
argument.

The partition dimension that historically gated tenant isolation is
retired in #56 (phases 1-7 landed; phase 8 sweeps the remaining
cross-repo stragglers + the DSL `partition="*"` automation kwarg). The
`partition` wire field is already removed (`reserved "partition"` in
`component/grpc/memql.proto`); nothing derives scope from the envelope.

### Concepts
Schemas for nodes (like tables in SQL).

```memql
concept agent {
  ownerUserId  string  @required
  // ...
}
```

**Relationships carry two independent axes** (memql#3652). `@relationship`
declares a cross-concept edge, and the two axes must not be confused:

```memql
@relationship(type="references", as="respondsAs", field="agentId", target=agent, direction="outgoing")
```

- **`type`** — what the ENGINE does with the edge. A **closed** set the engine
  owns: `parent`, `owns`, `createdBy`, `alias`, `equals`, `contains`,
  `references`. It drives id canonicalization, traversal, and the
  collection/reference node-type invariants. An unrecognized value **refuses
  boot** — the error names the `as=` form as the way out.
- **`as`** — what the edge MEANS to the domain (`assignedTo`, `repliesTo`).
  **Open**: any lowerCamelCase identifier, validated for FORM only and never
  against a list, so a new domain verb never needs an engine release. Optional;
  every declaration predating #3652 is unlabelled and stays valid.

Writing a domain verb in the `type` slot is the natural mistake and the reason
the split exists: `dependsOn` and `formedFrom` each cost an engine release as
structural types before being retired to labels (memql#3655). **Never add a
membership check to `as`** — that rebuilds the treadmill, and a test guards it.

`field` may be a dotted path when the pointer sits inside a nested object block
(`field="lineage.originatingPlanId"`); the engine walks it on both the write and
filter side (memql#3672). The field must be declared on the concept — on the
TARGET concept for `direction="incoming"`, since the FK lives on the far side.

Full authoring reference: [docs/public/language/memql.md](docs/public/language/memql.md#relationships)
and `dsl/_reference/_concept.memql` section 11.

### Nodes
Individual records with time-series history. IDs are
`{concept}:{shortId}`:

```
v1:common:agent:a9f3b7c2...
v1:cluster:node:bff-local
```

### Automations
Event-driven workflows. The `@trigger` annotation keys off an event
name plus the target concept, using keyword args (the live form
across `dsl/<ns>/automations.memql`):

```memql
@trigger(event="node.created", concept="v1:cognition:participant", partition="*")
automation autoJoinSI { ... }
```

A time-driven automation uses the `schedule` kwarg instead:
`@trigger(schedule="0 */10 * * * *")`.

> **#56 phase 8 caveat:** the `partition="*"` kwarg is still required
> while the event topic carries a partition segment. That segment goes
> away in phase 8, after which the kwarg drops.

### Functions
Reusable query and mutation functions. Both use the struct form --
it is the only author-facing shape (see "Retired author-side forms"
above).

**Concept binding lives in the construct signature.** The
two-identifier signature `query <Concept> <name>`,
`mutate <Concept> <name>`, `seed <Concept> <name>`, and
`shape <Concept> <name>` names the bound concept directly; the
loader resolves the concept name through the file's file-top
imports.

**Cross-file dependencies go through file-top `use` imports.** Every
construct another file pulls into local scope (shapes, traits,
specs, mutations, queries, logic, builtins, prompts, providers,
tools) is declared via a dotted-path import:

```memql
use cognition.concepts.{ participant, space }
use cognition.shapes.{ participantFull }
use common.traits.{ isActiveRecord, isNotDeleted }
```

The dotted path maps to a file on disk (`cognition.concepts` →
`dsl/cognition/concepts.memql`); the brace-list names the
constructs imported into local scope.

The bound concept's payload is referenced from filter clauses by the
**bare property name**, and from mutation bodies via the bare
`insert { ... }` / `update { ... }` block without re-stating the
concept id.

**Canonical filter-clause syntax** (enforced at LOAD time by
`component/memql/dslgate`, and over this repo's corpus by
`test/dslconformance/conformance_test.go`, which runs the same detector):

> **Where these rules are enforced (memql#3629, memql#4051).** The CONTRACT
> gates -- retired operator forms, the two `row.` namespace rules, the per-row
> authz user-scope bucket, the admin-gate composition rule, and the
> cross-namespace import rule -- run inside `MemQLEngine.Init`, land on the
> `LoadReport`, and are refused by strict boot (`MEMQL_DSL_ALLOW_SKIPS` is the
> break-glass). That is what covers a **product DSL bundle** mounted at
> `MEMQL_DSL_PATH`, which no Go test in this repo walks; `cmd/memqllint` runs
> the same `Init` offline. House-style gates (naming, redundant annotations,
> canonical short forms) stay test-only -- failing a fleet's boot over a
> convention would be worse than the drift.
>
> **Write a new gate against `dslgate.ScanFiles`, which takes the whole
> corpus.** Init used to loop `ScanSource` per file, so a cross-corpus rule
> ("where is this name DECLARED") could not be one of them and lived on a
> conformance test instead -- enforced over this repo at PR time and over a
> product bundle nowhere. There is no longer a tier of gate that boot cannot
> reach; reintroducing one is the mistake to avoid.

- Payload fields: **bare property** (`status`, `ownerUserId`) — never
  `<conceptName>.<field>`.
- Row intrinsics: the **`row.` namespace** — `row.id`, `row.concept`,
  `row.type`, `row.createdAt`, `row.createdBy`, `row.provenance.<leaf>`.
  A filter mixes two field surfaces under one syntax, and payload
  properties are bare, so a bare `id` is indistinguishable from a
  payload property while compiling to entirely different SQL (a table
  column vs a JSONB path). Enforced by
  `TestFilterIntrinsicsUseRowNamespace`. A spec/trait body reads its
  signature-bound fields bare and REJECTS `row.*`; mutation
  `insert`/`update` blocks write `id:` as a target key, not a reference.
- Sort keys take the same namespace — `sort "row.createdAt", "desc"`,
  for the same reason (`TestSortKeysUseRowNamespace`). Payload sort keys
  stay bare (`sort "version", "desc"`), `provenance` has no sort form,
  and the runtime/SDK sort surface accepts either spelling.
- **One Go boolean grammar:** `&&` (AND), `||` (OR), parens `( )` with Go
  precedence. `!` (NOT) lexes and parses, but is refused by every
  ASTConverter surface -- filters and specs get the expression-led scope
  error, logic bodies and collection lambdas get "NOT/! does not convert"
  (memql#3630). Its working homes are the two surfaces served by the runtime
  STRING evaluator (`component/automations.Evaluator`): an automation
  cond-step condition, and a trigger `@filter` -- `evaluateTriggerFilter`
  builds the same evaluator, so the two accept the same grammar. Everywhere
  the AST converter runs, write the `!=` comparison form instead.
- Membership is the single `in` operator: `args.x in list`
  or `kind in ["a", "b"]` (payload props bare).
- Prefix selection is `<field> startsWith <prefix>` (memql#4208): a string
  literal, a list of string literals (starts with ANY of), or an
  `args.<field>` resolving to either. Parameterized `^@ ANY(text[])` in
  SQL; an EMPTY list and a BLANK prefix match nothing -- a selection,
  never a pass-through (authoring-rules.md §32). Filters and spec bodies
  only; the automations condition grammar refuses it by name.
- Arg-conditional predicates use the `when(args.x) { <expr> }` guard:
  if `args.x` is absent the guarded block AND its connective are
  dropped as if never written (unambiguous under `||`).
- When a trait spec covers the predicate (e.g. `isActiveRecord`
  for `active==true`), the trait is mandatory. Inline
  `active==true` / `deleted==false` are rejected
  by the conformance test.

**Argument resolution** follows the rules in the "Argument resolution"
section above: `args.X` for caller-passed args, bare `now` / `actor.X` /
`partition` / `config.X` for engine-provided values, no `ctx`.

**Annotations** in the args block:
- `@required` — non-optional
- `@enum("a", "b", "c")` — restricts to a value set
- `@maxLength(N)`, `@pattern("re")`
- `@description` is **not** valid on an args field (rejected at load). An
  arg description is the `///` doc comment on the line above the field.
  A `tool` / `prompt` / `builtin` field DOES keep its `@description` —
  those bodies ARE the schema.
- `@default` is **not** valid on an args field (rejected at load). Apply
  a default in the body with the `??` operator (`args.X ?? <default>`).
  A concept-field `@default` is NOT a substitute — it is never applied
  on insert either, so `??` is the only mechanism that fills a value.
  `a ?? b ?? c` folds to what `coalesce(a, b, c)` produces; the
  shorthand is the authored form and
  `test/dslconformance/no_coalesce_longhand_test.go` gates the corpus on
  it (`memqlmigrate --rewrite=null-coalesce` converts).
  **`??` is BLANK-coalescing, not null-coalescing:** it falls through on
  an empty OR WHITESPACE-ONLY string as well as on absent/null, so a
  caller who deliberately clears a text field gets the default written
  back. `false` / `0` / `[]` / `{}` are kept. Deliberate, and specified
  in [authoring-rules.md §28](docs/public/language/authoring-rules.md);
  `@noUnset` is the targeted opt-out for a field a blank must not
  overwrite.

Queries:
```memql
use cognition.concepts.{ participant }
use cognition.shapes.{ participantFull }
use common.traits.{ isActiveRecord }

@description("Get space participants")
query participant spaceParticipants {
  args {
    spaceId  string  @required
  }
  filter  spaceId==args.spaceId && isActiveRecord
  shape   participantFull
}
```

Mutations:
```memql
use cognition.concepts.{ space }

@description("Create a cognition space")
mutate space mutationCreateSpace {
  args {
    spaceId  string  @required
    name     string  @required
  }
  insert {
    id:        args.spaceId
    name:      args.name
    status:    "active"
    createdAt: now
    createdBy: actor.userId
  }
}
```

`update { id: ..., ... }` is the partial-update counterpart for
mutations that read-merge-validate-write an existing row instead of
inserting a new one. Exactly one `insert` OR `update` block per
mutation.

### Logic

Imperative procedure called from an automation step. `args { ... }`
declares inputs; `body { ... }` is a sequence of named statements
ending in `return <expr>`. The single-statement form is the common
case:

```memql
use common.builtins.{ ensureDailySpaceForUser }

@enabled
@description("On user creation, ensure today's daily space exists.")
logic logicProvisionDailySpaceOnUserCreate {
  args {
    event object @required
  }
  body {
    return ensureDailySpaceForUser({ userId: args.event.payload.id })
  }
}
```

Multi-statement bodies (intermediate `name := <call>` steps with
side effects, followed by a trailing `return <expr>`) execute via
the `LogicRunner` wired into the engine at startup: the runner
walks intermediate steps in dependency order through the same step
registry the automation scheduler uses, then evaluates the
trailing `return <expr>` as the function's return value.

Logic functions don't write `ctx.output = ...`; the body's
trailing `return <expr>` is the function's return value.

### Prompts
AI prompt templates with input schemas and default providers. Struct
form, mirrors concepts / shapes / tools / providers / builtins —
the body is a bare input-schema field list, no `@input` wrapper.
Logic prompts (routing / suggest / classification) use the
structured-output path (`ChatStructuredProvider.CallChatStructured`);
prose prompts (agent replies to users) use regular chat.
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
Struct form, mirrors concepts / shapes / tools; base providers carry
vendor-level auth + type.
```memql
@base
@type("OpenAI")
provider openai {
  auth { apiKey env("MEMQL_AI_OPENAI_API_KEY") }
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

**Lifecycle annotations (`@enabled` / `@disabled`).** Providers accept the same
lifecycle flags as functions / builtins / prompts / specs / seeds. `@enabled`
is the explicit-on default (a no-op). `@disabled` skips the provider at load --
**not registered, no auth resolution attempted**, so it emits zero "registered
as unavailable" warnings while staying in the tree for a future re-enable.
`@disabled` on a `@base` **propagates** to every child that `@extends` it: mark
the base disabled until its API key is seeded. Dependents degrade gracefully --
a policy whose `@primary` is disabled routes via its `@fallback`; a prompt
whose `@defaultProvider` is disabled falls back to the default.

> **Semantics of `@disabled` (shared across every construct that takes it).**
> It means the construct is **not loaded/active at runtime right now**. It does
> NOT mean deprecated, abandoned, or exempt from updates / maintenance /
> refactors / conformance. It is a reversible on/off switch; disabled
> constructs are still maintained. ("Deprecated / abandoned" is a separate axis
> carried by `@deprecated`.) Canonical statement:
> `component/language/ast/ast.go` at the `AttrEnabled` / `AttrDisabled` consts.

### Shapes
Reusable data projections — declared in struct form. Each shape
declares its **kind** (where its fields come from) via `@row` and/or
`@actor`. At least one is required; both is allowed (mixed shape).
Each path becomes a template entry keyed by the path's terminal
segment.

**Row shapes** project a concept's payload + row intrinsics. The bound
concept is named by the **signature** `shape <Concept> <name>` (the
short-name resolves through the file-top `use ...concepts.{ ... }`
import):
```memql
use cognition.concepts.{ space }

@description("Space summary card")
@row
shape space spaceCard {
  row.id
  name
  description
  row.createdAt
}
```

**Actor shapes** project the engine envelope (the authenticated
actor + engine timestamp). They carry no signature concept. Closed
field set, enforced at load: `actor.userId` / `actor.role` /
`actor.identityId` / `actor.isClusterOwner` / `actor.primaryEmail` /
`actor.now`. Bare `config.<key>` is the config read; shapes do not
project it.
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

**Mixed shapes** carry both `@row` and `@actor` — useful for predicates
that compare row fields against actor context (e.g. "rows I created" =
`row.createdBy == actor.userId`). The row concept is signature-bound;
payload props are bare, `row.X` / `actor.X` are the ambient prefixes:
```memql
use cognition.concepts.{ space }

@row
@actor
shape space ownedSpace {
  row.id
  ownerId
  actor.userId
}
```

**No composition.** `include` is NOT a shape verb and is REJECTED at
load. To share a projection, repeat the paths -- or drop the body
entirely and take the default projection over the bound concept, which
is the direction the tree is moving anyway.

**Every body path is checked at load**: a bare payload
property must be a declared field of the bound concept, the bound
concept must resolve (an ambiguous bare name disambiguates through the
shape's own domain), two paths may not collapse onto the same terminal
key, and the declared kind must match the body -- `actor.*` needs
`@actor`, `row.*` / bare payload needs `@row`, and a shape must declare
at least one.

No `func`, no `@template`, no `node("…")` wrapping. Shapes have no
inputs and no return; the body is a path list.

### Specs
Atomic boolean predicates — **signature-bound** (epic #2281). A spec
binds exactly one shape XOR concept in its signature
(`spec <boundName> <name>`, resolved via the file-top `use` import) and
the body `return`s a boolean over **bare** field names. The binding
picks the evaluation strategy:

- **Row-specs** bind a concept or a `@row` shape. They compile into a
  SQL `WHERE` fragment and push down to the database for filtering.
- **Context-specs** bind an `@actor` shape (the only gateway to the auth
  envelope). They evaluate in-process; named as a bare conjunct for
  actor-based checks like "is admin," "owns partition," etc.

A spec body never reads `actor.*` / `row.*` directly (bind a shape that
projects it and read the projected key bare). A `trait` is the one
deliberately-unbound row predicate (bare payload fields, validated at
the call site).

```memql
use cognition.concepts.{ participant }

@enabled
@description("Matches guest participants")
spec participant isGuestParticipant {
  return isGuest == true             // concept-bound row-spec
}

use common.shapes.{ actorEnvelope }

@enabled
@description("Actor holds an admin role")
spec actorEnvelope requiresAdmin {
  return role == "admin"                        // @actor-bound context-spec
}

@enabled
@description("Matches records with active==true field")
trait isActiveRecord {
  return active == true                         // unbound cross-concept trait
}
```
A spec/trait body is a single `return <boolean expression>` over bare
field names; there is no `ctx` envelope and no parameter. A
bare-expression body with no `return` is rejected at parse time.

**Caller-context checks use specs, not policies.** Author the predicate
as a context-spec in `dsl/<namespace>/specs.memql` and name it as a bare
conjunct; the `policy` construct is provider-selection only.

### Tools
AI-callable tool definitions — struct form, mirrors how concepts +
shapes read. The body is a list of input-schema fields with types
and annotations (`@required`, `@default`, `@enum`, `@description`).
```memql
@enabled
@description("Search for users")
@handler(type="query", query="concept==v1:memql:backend:user")
@executionTime("fast")
tool searchUsers {
  active  boolean  @description("Filter by active status")
  limit   integer  @default("10") @description("Max results to return")
}
```

**A tool declaration is CHECKED at load.** Four gates, all fail-loud —
nothing here degrades silently:

- **`@handler` argument names are closed** (`type`, `name`, `query`, `url`,
  `method`) and `type` is required. `@rateLimit` is closed the same way,
  and a non-integer value is refused.
- **The handler is validated at load** — unknown type, missing function name /
  query / URL — and a tool must carry a handler at all unless it is
  `@clientExecution` (whose body lives in the browser).
- **The handler's TARGET is resolved** against the function + builtin
  registry at boot (`tool_handler_resolution.go`), so a handler naming a
  function that does not exist is a load problem strict boot refuses, not a
  mid-turn failure the first time a model calls the tool. A builtin is
  reached through `@handler(type="function", name="<builtin>")`; there is no
  `"builtin"` handler type.
- **Field types and field annotations are closed sets.** An unknown type is
  refused rather than emitted as `"string"` (which would tell the model
  "string", coerce the `@default` to a string, and hand a `@required
  integer` handler argument `"10"`).

### Integration Capabilities
Go-backed operations callable from the DSL via
`@executor("integration.X.Y")`. Struct form, mirrors concepts /
shapes / tools / providers / prompts. The body's field list is the
builtin's input schema; the actual implementation is the Go
integration named by `@executor`.
```memql
@enabled
@description("Score an utterance for an AI participant")
@executor("integration.cognition.scoreUtterance")
@args(profile="object")
builtin cognitionScore {
  spaceId        string  @required
  participantId  string  @required
  utterance      string  @required
}
```

Available integrations (core, registered via the plug-in system):
actionSearch, agents, auth, avatardirect, chat, dailyspace, database,
deployversion, email, embedding, files, azureblob (as `storage`),
harnessRecall, harnessTrace, identity, knowledge, library, liveknowledge,
openairealtime, rbac, router, shopify, similarity, telephony, timeutil,
voice, workbench, plus node-type-scoped ones (cognition, agent, stt,
openaiVoice) wired explicitly in `app/integrations_*.go` when their
dependencies sit outside the stable `PluginContext` surface. `training`
is a product-repo pack, not part of engine-only core.

`shopify` is a CONNECTOR rather than an ordinary integration, and the
reference implementation of `component/memql/sync.Connector` (epic
memql#4389): it owns one external system's data, its model is GENERATED
from that system's schema at a pinned version (`cmd/shopifyschema` ->
`dsl/shopify/generated/`, 65 concepts), and its five verbs return
MirrorWrites for the runtime to apply rather than writing themselves.
Read [integrations/CLAUDE.md](integrations/CLAUDE.md) before writing a
second one; the operator runbook is
[docs/public/operate/shopify-connector.md](docs/public/operate/shopify-connector.md)
and the headless storefront's side is
[docs/public/operate/shopify-storefront-checklist.md](docs/public/operate/shopify-storefront-checklist.md).

### Extension Points

Three ways to extend MemQL, in preference order:

1. **DSL files** (`.memql`) -- queries, mutations, specs, automations,
   prompts, providers, shapes, tools, builtins. Always the first choice.
2. **Self-registering plug-ins** -- Go integrations that call
   `memql.RegisterPlugin(name, factory)` from `init()`. The factory
   receives a narrow `PluginContext` (Logger, Engine, BunDB getter,
   VisionProvider, EmbeddingProviderByName, partition/variable
   resolvers). Build tags on the calling file control which binaries
   include the registration. Use this path to add product-specific Go
   without touching `app/` internals. See `component/memql/plugins.go`.
3. **Explicit `app/` wiring** -- reserved for first-party integrations
   whose dependencies don't fit `PluginContext` (cognition, agent,
   stt). Lives in `app/integrations_*.go` with build tags.

Event routing is also plug-in-registerable: `node.RegisterRoutingRule(...)`
declares forwarding patterns from `init()`, and build tags on the caller
decide which binaries include the registration. Forwarding is default-deny --
block rules evaluate first, then forward rules, and an event matching
neither stays local.

**There is no concept-ownership registry.** Which node does a concept's
work is decided by routing rules plus which binary's build tags compile
the subscriber -- there is no per-concept dispatch table to register into.

### MemQL Sense (Language Intelligence)
Language service for .memql files, exposed via gRPC on `MemqlService.Stream`:
- **Tokenize** -- Semantic tokens for syntax highlighting (keywords, identifiers, strings, annotations, concepts)
- **Complete** -- Context-aware autocompletion (annotations, receiver types, functions, concepts, builtins)
- **Diagnose** -- Errors and warnings (lexer, parser, semantic validation)
- **Hover** -- Symbol info at cursor (function docs, concept schemas, annotation docs)
- **SignatureHelp** -- Function parameter help inside call arguments

Package: `component/memql/sense/` -- pure Go, no gRPC dependency. gRPC handlers in `component/grpc/sense_handlers.go`.

### Data origins -- Mirror, Origin, Native (epic memql#4378)

**MemQL is the origin of what it owns, a faithful mirror of what it does not,
and every concept says which.** Two declarations, three derived states, and
deliberately no fourth -- "shared", two systems both authoring one domain, is
the option the model rejects and the vocabulary cannot say.

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

**"Read-only by construction" is literal, and STRICTER than the row-authz write
guard.** `executeWrite` refuses every write to a mirror concept -- mutation,
tool handler, raw insert, staged write -- that does not come from the connector
its `@origin` names, with `mirror_write_refused{origin}` and an audit line.
Neither of `rowauthz_write_guard.go`'s two escapes applies: internal origin says
*the engine* is writing when the question is whether *shopify* is, and a cluster
owner's edit to a mirror is reverted by the next reconcile exactly like anyone
else's -- accepting it would be a write that appears to work and does not last.
So a mutation bound to a mirror must be `@serverOnly`, or it is generated into
both SDKs as a method that can only fail (gated by
`TestMirrorConceptsHaveNoClientReachableMutation`).

**Connectors are named actors, not a bypass.** `auth.ConnectorActor(name)` is
admitted by row admission to the concepts whose `@origin` or `@mirroredTo` names
it, **regardless of tier, and to nothing else** -- including the ~88 undeclared
concepts that admit everyone, which is the half that makes it targeted. No
request can mint one: `RoleConnector` is outside `ValidRoles()` and outside the
rank model. A connector is an *integration* implementing the contract in
`component/memql/sync`; it is not a fourth extension word.

**Registration has two halves.** `sync.Declare(name)` runs from an `init()` and
says *this build serves a connector by this name*; `sync.Bind(c)` attaches the
implementation later. The boot check reads the first, because `MemQLEngine.Init`
runs BEFORE integrations are wired (`app/build_*.go`). An unresolvable name
**refuses boot**: a mirror nobody fills reads as an empty catalog and a mirror
target nobody drains accumulates outbox entries forever -- both are silent. A
build that has declared no connectors at all cannot tell a typo from a correct
name and says so at Warn rather than refusing; every node binary and
`cmd/memqllint` declare them, so every real boot checks.

Full doc: [docs/public/concepts/data-origins.md](docs/public/concepts/data-origins.md).

### Infrastructure concepts

Inventories live in the `.memql` files; what follows is what a reader needs to
know that the schema does not say.

**Platform** (`dsl/platform/concepts.memql`): `site` -- a hosted web surface,
now a "deployable" (memql#4344). The edge resolves the request `Host` to one of
these rows and serves its `bundleRef`; the row carries `ownerUserId` (empty =
cluster-owned, which the seeded portal row stays), `artifactId`, a typed
`binding`, `kind` including `shopify_storefront`, and the composite tier. A
user's hostname is `<slug>.<domain>` against a reserved set DERIVED from
`frontdoor.Roles()` + the portal, so a new role can never become claimable by
omission; Android / iOS / macOS deliberately have NO enum values, being
distribution rather than hostnames ([deployables.md](docs/public/operate/deployables.md)).
Plus `globalSecret` / `globalVariable` (cluster-scoped config),
`outboundRequest` / `inboundRequest`, `missingCapability`, plus `dataOrigin` --
a VIRTUAL projection (the `v1:router:modelCatalog` pattern, never persisted) of
every concept's data-origins declaration, produced by the `dataOrigins` builtin
from the live registry (memql#4378).

**Library** (`dsl/library/concepts.memql`): `artifact` is a thin INDEX row
(memql#693) owning no content -- one per item, carrying the provenance/type
spine the Artifacts page lists on. `file` is the sixth backing row and the only
one with bytes (memql#4340); `fileChunk` holds the embeddings search-by-meaning
runs over. All three declare `@rowAuthz(owner="ownerUserId", clusterOwner)`.
The index's `source` enum is the UNION of every backing concept's own, because
the promotions pass the backing value straight through -- a value one can hold
and the index cannot is a promotion that refuses at execute time
([library.md](docs/public/operate/library.md)).

**Cluster** (`dsl/cluster/concepts.memql`): `node`, `nodeType` (optional
`codeReference` links the row to its architecture-model service id, consumed by
the cockpit's Topology drill-down), `spawnEvent`, `cluster` / `database` /
`identityProvider`, plus the deploy pair:
- `deployment` -- append-only, one timeline per deploymentId (pending ->
  in_progress -> succeeded|failed; superseded/rolled_back). The
  deploy-as-a-pack source of truth (#1872).
- `deploymentNodeSpec` -- per-node-type child, one timeline per (deploymentId,
  nodeType) carrying version + replicas + imageDigest. **Engine-as-spine:** an
  empty `version` resolves against the deployment's engine version, a non-empty
  one pins the node type. Read the full set via `nodeSpecsForDeployment`.

**Observability** (`dsl/observability/`, loaded by every node --
[design](docs/internal/design/auto-generated-diagrams.md)): `codeProfile`
(live per-FQN verbosity override; CDC events feed the observe runtime's cache
via `CodeProfileSubscriber`), `invocation` (per-call records on the
`code_invocation` hypertable), `codeMetric` (per-(FQN, window) aggregates on
the `code_invocation_1m` / `_1h` continuous aggregates, driving the cockpit
Topology overlay). Clients read metrics through `codeMetricsInWindow`
(memql#4208): one bucket, one `[windowStart, windowEnd)` range, `codeReference
startsWith` any of the caller's prefixes -- the prefix-scoped read the portal's
module drill-in uses instead of a capped client-side walk.

**Identity** (`dsl/identity/concepts.memql`, loaded by every node -- full model
in [access-model.md](docs/public/operate/auth/access-model.md)): `user` (the
person; cluster-wide role owner / admin / developer / writer / reader; prefs),
`authSession` (per-token, used for revocation), `magiclink`,
`accessRequest`, `invitation`, `delegation` (agent acting through a user's
identity, bounded role/scope/lifetime), plus:
- `identity` -- a credential set owned by a user, a discriminated union keyed
  on `identityType` (magic-link verified email, oauth token, api key/PAT,
  service account, worker token, badge, account token, passkey). The `passkey`
  variant is the only one whose stored material is PUBLIC (a COSE key),
  because possession is proved by a signature rather than a digest match.
- `auditEvent` / `authActivity` -- TWO logs since memql#4328, and the split is
  what keeps the portal's Audit Trail readable. `auditEvent` records DECISIONS
  and security signals (sign-in, session created/revoked, role change,
  `refresh_token_reuse_detected`); `authActivity` records routine MECHANICS --
  refresh-token rotations, the blocked ones, grace-window accepts,
  PAT-authenticated requests -- which are two orders of magnitude more numerous.
  The Trail is a generic concept walk with no filter, so the split is
  structural rather than something every reader has to remember.
  `authActivity.action` is a CLOSED enum of four values, unlike its sibling's;
  it is the first concept in the tree to declare
  `@rowAuthz(owner="<field>", clusterOwner)`, which is what lets a person read
  their OWN activity and a cluster owner read everyone's (a non-owner admin
  gets `authActivityForSelf` -- the composite's escape is the owner ROLE); and
  its `retiredTokenHash` is the evidence refresh-token reuse detection keys on
  (memql#4329). Real retention applies:
  `MEMQL_IDENTITY_AUTH_ACTIVITY_RETENTION_DAYS` (default 30), hard-deleted
  daily from Go, unlike `auditEvent`'s count-only sweep -- so detection reaches
  back exactly that far.
- `enrolmentToken` -- single-use, TTL'd, authorizing exactly one action:
  register a passkey as the named user (memql#3408). What makes a FIRST
  credential obtainable with no mailbox. Mirrors `workerPairingCode` rather
  than extending `invitation` -- it has no invitee, no inviter to render into a
  message and no product scope, and its single-use marker is a `consumedAt`
  stamp rather than `respondedAt` + a participation status.


---

## Feature Notes

### Canvas + Spaces

Under platform consolidation (#2472) the space lifecycle (three-state + daily
spaces) is an **engine-generic feature** rather than product code; the core
participant/session/utterance machinery is engine-side
(`dsl/cognition/mutations.memql`: joinSpaceAsHuman, leaveSpace,
addAgentToSpace, ...).

The canvas timeline (the `canvasState` concept) is still delivered as **product
DSL** at runtime through the product's bundle (`MEMQL_DSL_PATH`); its physical
absorption into the engine is mid-migration, so treat canvas as product-owned
for now. Product rows ride the chat-reply delivery substrate via
`node.RegisterChatReplyConcept`.

### Nexus -- the portal's living map of a goal (memql#4369)

`clients/portal/src/nexus/` is the console's one 3D surface: a
`v1:planner:plan` and its world -- the planner, the specialists it raised,
its semantic tasks by phase, the artifacts it produced and the constructs it
authored -- materializing as the system works, then replayable from the rows'
own timestamps. Three pages under one goal (Map, Constructs, Replay) behind a
**Nexus** rail group.

Four things about it are load-bearing rather than stylistic:

- **`scene/` is pure and imports no three.js.** `layout(world)`,
  `events(world)` and `scene(world, at)` are functions over rows, tested on
  fixtures with no GPU, and shared by the Map and Replay. The canvas draws
  what they return. `events()` **invents nothing**: a moment with no
  timestamp produces no event, because a scrubber is read as evidence.
- **The feed resolves EVERY live event through the authorized read**, payload
  or `payload_omitted` alike, and drops it when the read refuses (design D6).
  One code path, so the branch that would trust a payload does not exist to
  be forgotten -- and `plan`'s coming `granted` tier changes nothing here.
- **The scene is a lazy chunk.** three.js + fiber + drei are the portal's
  largest dependency and no other page uses them; only
  `map/NexusCanvas.tsx` may import them, and `nexusMap.test.tsx` fails the
  build if anything else does. The frame loop is `frameloop="demand"` and its
  governor evaluates the predicate PER FRAME -- a boolean captured at render
  would either spin forever or never wake.
- **One goal at a time, YOURS, and part of that is client-side today.**
  `v1:planner:plan` is undeclared (memql#4366), so `planById` answers for any
  id; Nexus refuses to draw a goal whose `requestedBy` is not the caller's
  own user id. That is a client-side filter, labelled as one everywhere it
  appears, and the residual is recorded in
  [per-row-authz-audit.md](docs/public/operate/auth/per-row-authz-audit.md).

Operator doc: [portal.md](docs/public/operate/portal.md). Design:
`docs/superpowers/specs/2026-08-22-nexus-living-map-of-a-goal-design.md`.

### Invitations (Identity Primitive)

Token-hashed invitation credential for user and guest flows, under
`v1:identity:invitation`. Two gRPC messages drive the guest flow:

- `SendGuestInviteMsg` -- authenticated space owner. Mints a 32-byte token,
  stores only its SHA-256 hash on the `Invitation` record, sends the email via
  the `email` integration plug-in.
- `ResolveGuestInviteMsg` -- unauthenticated public call from the product
  `/join/<token>` landing page. Returns scope + inviter metadata or a typed
  status (`invalid` / `expired` / `already_accepted` / `cancelled`).

Guest authentication is `Authorization: Guest <token>`.
`NewGuestAwareStreamInterceptor` wraps the identity-verifier interceptor,
validates the token against the invitation registry, and builds a guest
`AccessContext` under the `identity.guest` claim key (subject
`guest:<invitationId>`). The MemQL WS bridge accepts it as
`?guest_token=<token>` since browsers cannot set custom headers on the upgrade.

Key files: `dsl/identity/{concepts,queries,shapes}.memql`,
`component/grpc/guest_handlers.go` + `guest_stream_interceptor.go`, and
`integrations/email/` (self-registering plug-in exposing
`integration.email.sendEmail` -- GraphSender via Microsoft Graph `sendMail`
preferred, SMTPSender fallback, LogSender for dev; env `AZURE_TENANT_ID` /
`AZURE_CLIENT_ID` / `AZURE_CLIENT_SECRET` / `MAIL_SENDER` / `MAIL_FROM_NAME`,
or the `SMTP_*` family, or neither).

**The guest write path is ENGINE DSL, split across two domains** (memql#4258).
The five constructs `guest_handlers.go` names once existed in no `.memql` file
at all, so every guest-invite write failed at execute with `function "..." not
found`. The split follows what each half is about:

| Construct | Where | Why there |
|---|---|---|
| `createGuestInvitation` | `dsl/identity/mutations.memql` | the credential -- beside `createUserInvitation` / `revokeUserInvitation` |
| `markGuestInvitationAccepted` | same | same |
| `markGuestInvitationKicked` | same | also the CANCEL path; a soft cancel, so the tokenHash stays taken |
| `rotateGuestInvitationToken` | same | resend keeps one generation on `previousTokenHash` |
| `createGuestParticipant` | `dsl/cognition/mutations.memql` | the membership -- the SPACE is cognition's |

All five are `@serverOnly`: each writes a `tokenHash`, which is the whole
credential the guest-auth interceptor matches on, so a client-reachable create
is a credential-forging primitive.

The three update-shaped ones take MINIMAL arguments -- `update{}` has been a
read-merge since memql#1628, so re-supplying every discriminator is dead weight
that an undeclared-argument DISCARD hides.
`component/grpc/render_query_args_parse_test.go` gates both directions: every
rendered call site resolves through the real front end, and every argument name
it passes must be one the mutation declares.

### Email campaigns + the sending engine

Campaigns are ordinary graph state (memql#3323) plus a Go sending engine
(memql#3348). Seven concepts under `dsl/campaigns/`: five operator-facing and
owned-tier (`audience`, `recipient`, `template`, `campaign`, `delivery`), two
engine-owned and clusterOwner-tier (`sendJob`, `suppression`).

**The two identities is the design.** A send touches rows belonging to somebody
else, and the engine BORROWS the owner's authority rather than out-ranking it.
`component/campaigns`' drain worker runs its clusterOwner-tier reads (the job
queue, the suppression list) under the engine's own operator identity, and
everything owned under
`auth.ContextWithUserActor(ctx, job.campaignOwnerUserId)`. That owner value is
copied off a campaign row the STARTING CALLER had already read under their own
actor, so it can never name a user the caller could not act as.

**Four product decisions, each recorded next to its code:**
- *Suppression is CLUSTER-WIDE and digest-keyed.* One deployment, one sending
  mailbox, one reputation -- so one list. The row id is the SHA-256 of the
  normalized address and the only readable field is the domain. Enforced at the
  POINT OF SEND, before the recipient row's own status.
- *A hard bounce suppresses; it does NOT delete the membership.* Deleting
  destroys the audit trail and lets the next import resurrect a dead address.
- *Idempotency is the ledger.* One `v1:campaigns:delivery` row per (campaign,
  recipient) at a derived id; the batch is "roster minus ledger, plus retries
  that are due". The absence of the row IS the work queue.
- *Two rate limits.* Ours is a per-process token bucket
  (`MEMQL_CAMPAIGNS_SEND_RATE_PER_MINUTE`); theirs is the 429, surfaced as a
  typed `email.SendError` and honoured by parking the job until its
  `Retry-After`.

**RFC 8058 one-click** rides two headers, which forced the Graph sender onto
its base64-MIME form (Graph's structured payload only carries `x-` headers).
`GET+POST /unsubscribe` is a documented HTTP exception.

**The unsubscribe token names its key**
(`u2.<keyId>.<owner>.<recipient>.<campaign>.<tag>`, memql#3458), verified
against a ring of two: `MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET` signs,
`..._SECRET_PREVIOUS` only verifies. The key id is a truncated HMAC **of the
key**, not a slot -- a link minted today is clicked on a node where that secret
has since become the previous one. `_PREVIOUS` is a permanent second reader
key, NOT a migration window: unsubscribe links never expire, so **the window is
counted in rotations, not days** -- rotate at most once for any reason short of
key compromise.

Not built: an automated warming ramp, and a scheduler for `scheduledAt`.
Runbook: [docs/public/operate/campaign-sending.md](docs/public/operate/campaign-sending.md).

### Planner / Knowledge / Validation

The schema is stable, so new features add fields/automations without migrations.

**Concepts**:
- `v1:planner:plan` -- a user-visible unit of work. parentPlanId (sub-plan
  nesting), kind, status (queued / routing / running / paused / awaitingFeedback
  / needsAgent / succeeded / failed / cancelled), goal, ownerAgentId,
  requestedBy, triggerSource, recommendationCardId, input / output,
  refinementContext, phases[], estimate, token budget/spend bookkeeping,
  metrics, pause + feedback + chat-anchor bookkeeping.
- `v1:planner:task` -- one executable step inside a Plan, never recursive.
  Carries phase tag, executionSurface (inProcess / containerExecutor) +
  executorBackend, metrics, parking fields.
- `v1:planner:taskState` -- persisted Task working state for async parking.
- `v1:agents:agentAuthorization` -- standing tiered-trust authorization.
- `v1:knowledge:document` -- container/manifest for analyzed user files.
- `v1:knowledge:spreadsheetRow` / `imageRegion` -- typed per-format items.
- `v1:knowledge:validationEvent` -- append-only validation audit log.
- `v1:knowledge:domainEntitySchema` / `entityIndex` -- cross-file dedup:
  per-domain entity schema (inferred on second-Document trigger,
  user-confirmed once) and the sha256-keyed lookup table, with a force-add
  escape valve for entity-schema misfires.

`v1:common:knowledgeDomain` carries scope (workspace / private) + ownerId;
`v1:common:documentChunk` carries documentId + validationStatus.

**Analysis path.** The attachment HTTP handler creates the queued Plan +
`plan.created` card synchronously, then runs extract + summarize +
`CompleteAnalyzePlan` on a detached goroutine, so the user gets instant
acknowledgement and the `plan.completed` card lands when the work finishes
(`runAnalysisAsync` in `component/server/attachment_handler.go`).

**Planner Agent loop.** `integrations/planner/agent_loop.go` invokes the
`plannerAgent` prompt on a new userGoal Plan; the prompt emits a structured
decision (decompose / dispatchTask / createSpecialist / markPlanSucceeded /
escalate) and the loop dispatches it, re-invoking until terminal.

**The cost-safety structure around that loop is the part to respect.** It is
defense in depth and every layer is load-bearing:

- A hard process-wide LLM rate ceiling and an identical-request circuit breaker
  at the provider HTTP chokepoint (`component/memql/ai_guard.go`).
- A CUMULATIVE per-plan token/call budget checked before every `plannerAgent`
  call, persisted so it survives cycles and retries; on exceed the Plan parks
  rather than making another call (`component/planner/budget.go`).
- Complexity triage that routes a trivial deliverable to ONE cheap path instead
  of the decompose loop; model tiering that defaults cheap and escalates only
  on an explicit stuck signal.
- An up-front estimate + user-approval gate, gated specialist
  creation/training, phased execution with per-phase checkpoints,
  deterministic-first result verification, and a no-task-`markPlanSucceeded`
  convergence guard.

Read [docs/public/ai/llm-cost-control.md](docs/public/ai/llm-cost-control.md)
before touching any of it. `produceArtifact` rides the unified loop rather than
a bypass: triage shortcuts to ONE direct production turn (`startPlanDirect`),
with the rate ceiling, caps and tiering as structural backstops. An earlier
hardcoded bypass was reverted precisely because those backstops did not yet
exist -- do not reintroduce one.

## Notes for Claude Code CLI

- Use [GLOSSARY.md](GLOSSARY.md) to find specific documentation.
- Several directories carry their own CLAUDE.md, and it is the first thing to
  read before editing that tree: `component/`, `component/language/`,
  `component/node/`, `component/architecture/`, `component/observe/`,
  `integrations/`, `sdk/go/`, `docs/`. This is a list, not a rule -- a
  directory without one is normal (memql#4121).
- The local k3d cluster is self-contained (`make up`; no manual setup needed),
  and migrations run automatically on startup.

### Makefile + shell-script convention

The Makefile is for **simple commands and target wiring**. Anything
multi-step, conditional, or long enough to need line-continuations gets
extracted into a shell script under `scripts/`, and the Makefile target becomes
a one-liner that calls it.

- **Stays inline:** single commands (`go build`, `go test`, `kubectl rollout
  restart`), short pipelines (~3 lines or fewer), `.PHONY`, target
  dependencies, simple variable substitutions.
- **Goes into `scripts/<area>/<name>.sh`:** conditionals, retry loops,
  multi-step orchestration, user-facing error messages -- anything "complex
  enough that you'd want to test it independently of make."

Shell-script rules: `#!/usr/bin/env bash` + `set -euo pipefail` (drop `-e` for
status reporters where one failure shouldn't abort the rest); **function-based
structure** with `main()` at the bottom, never a sequential blob; source a
shared `scripts/<area>/*.sh` helper for common functions; `.sh` extension,
executable. Reference: `scripts/k3d/{up,dev,status}.sh` behind one-liner
targets.

#### Capability scripts (the hardened successor)

A **capability script** is a deploy/ops script that is also the deterministic
backend behind a DSL `action`, so it must run **identically** whether an
automation or a human invokes it. It adopts the **capability-script contract**
([docs/internal/design/capability-script-contract.md](docs/internal/design/capability-script-contract.md),
#2221) -- the convention above **plus**:

- **non-interactive** -- no `read -p` / `select`; a destructive confirmation is
  an explicit `--confirm=<phrase>` param, never a blocking prompt;
- **structured params in** -- `--flag=value` > stdin JSON (`--params-stdin`) >
  documented defaults (no env tier; a script passes an env-resolved value as
  the default); no positional args;
- **structured result out** -- exactly one JSON envelope on **stdout**, all
  human logs on **stderr**;
- **honest, stable exit codes** (0 ok; 2 bad param; 3 refused; 4 prerequisite
  missing; 5 op failed);
- **no decisions inside** -- no branching on environment/version/role (that
  lives in DSL `logic`); only mechanical idempotency branches.

They `source scripts/lib/capability.sh` (`cap_init` / `cap_param` / `cap_ok` /
`cap_fail` / `cap_info`-to-stderr / `--print-spec`).
`scripts/lib/capability_contract_test.go` enforces the contract on every script
that sources the library and gates non-interactivity across
`scripts/{k3d,deploy,release}`; the Go effect seam parses the envelope via
`deploycontrol.ParseCapabilityResult`. Use this contract for any new script a
DSL `action` will drive.

### Documentation Style Guidelines

**No Emojis:** All documentation, skills, and CLI responses must use professional formatting without emojis. Use:
- Checkboxes: `[ ]` for unchecked, `[x]` for checked
- Text indicators: "SUCCESS:", "ERROR:", "WARNING:", "INFO:"
- Standard markdown formatting for emphasis
- This applies to: documentation files, skill outputs, CLI responses, and all user-facing text
