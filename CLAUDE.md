# MemQL - the AI memory platform

**Type:** AI platform -- agents, automations, and voice on a time-series memory graph
**Language:** Go + MemQL DSL
**Stack:** PostgreSQL + TimescaleDB extension
**Purpose:** Run agents, automations, and voice against a time-series memory graph

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
├── dsl/               Consolidated MemQL DSL tree, per-namespace per-construct
│   ├── <namespace>/   concepts / mutations / queries / specs / shapes /
│   │                  builtins / tools / prompts / automations .memql
│   └── _reference/    Per-construct authoring reference skeletons
├── integrations/      External services + DSL-callable capabilities (Go)
├── brand/             Visual identity as plain CSS custom properties, imported
│                      by BOTH clients/portal (Vite) and component/identity/web
│                      (Tailwind CLI) -- they share no package manager, and CSS
│                      variables are the one format both consume. Never copied:
│                      brand_shared_source_test.go fails the build on a
│                      --memql-* token, an @theme block or an @font-face
│                      defined outside it (memql#4266)
├── clients/           Surfaces built ON the platform -- the inward-facing
│   │                  mirror of integrations/. The engine carries exactly one
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
| `clients/os/` | MemQL OS -- the desktop shell, served at `os.<domain>`. **Read its README before adding an app or a live surface**: the live-collection contract (a collection does nothing until `retain()`), which concepts are actually broadcast, and the arrival-cue rule (a heartbeat is not news) are all rules a new surface gets wrong by default | [→](clients/os/README.md) |
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

- [Component vs integration vs pack](docs/public/concepts/component-integration-pack.md) -- the three words; intake "plugin" means pack
- [Architecture](docs/public/concepts/architecture.md) · [Events](docs/public/concepts/events.md) · [Tech stack](docs/public/overview/tech-stack.md)
- [MemQL Language](docs/public/language/memql.md) · [Functions](docs/public/language/functions.md)
- [MemQL Authoring Rules & Gotchas](docs/public/language/authoring-rules.md) -- read before writing `.memql` files
- [Node Identifier Conventions](docs/public/concepts/identifiers.md) -- canonical `{concept}:{shortId}` internally vs the BARE-ids client contract at every wire seam (the engine bare-ifies on egress and resolves bare args on inbound; clients never compose, parse or compare canonical ids), the `(concept, id)` keying rule, anti-patterns
- [`core/num`](core/num/num.go) -- the ONE narrowing from a decoded payload
  number to a Go `int`, in three NAMED answers (saturate / zero /
  caller-default). Read it before writing a `func …(v any) int`: a bare
  `int(x)` in a `float64` or `int64` arm is implementation-defined out of range
  and answers with the integer indefinite value, and
  `TestEveryPayloadNarrowingCarriesAnAnswer` fails the build on one that
  declares no answer (memql#4779)
- [LLM cost control (defense in depth)](docs/public/ai/llm-cost-control.md) -- read before touching `ai_guard.go`, an LLM loop, or an automation that drives model calls
- [Tool ↔ Knowledge Domain Pattern](docs/public/concepts/tool-knowledge-domain-pattern.md) -- when a capability has operational knowledge, put it in a knowledge domain the tool requires, not in the agent prompt template
- [Environment variables](docs/public/operate/env-vars.md) -- bootstrap envelope vs. concept-stored config; how to add / rotate / override
- [Auto-generated architecture diagrams](docs/internal/design/auto-generated-diagrams.md) -- static topology model + observe runtime + cockpit drill-down

**Tooling:** **MemQL Cockpit** -- the fleet worker runtime + cluster CLI,
installed as the `memql` command on operator machines. Lives in its own repo at
`github.com/znasllc-io/memql-cockpit`; consult that repo's CLAUDE.md.

> **This repo also builds a `bin/memql`, and the collision is deliberate.** The
> engine's binary ships only inside container images and runs in pods; the
> Cockpit's is installed on an operator's machine. They never share a PATH, so
> the names do not collide in practice -- do not "fix" it by renaming either.

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
module. The failure mode is silent and confidence-INCREASING: edit
`component/memql`, run the bare command, see `ok` across 64 packages, and report
the change verified. `TestDocumentedTestCommandCoversTheEngine`
(`claude_md_test_command_test.go`) fails the build if this section ever
documents a command that misses the engine again.

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
  them into k3d via `make dev`. Fast, throwaway, never pushed to ACR.
- **Deploys to the CLOUD** -- images MUST be built on the **GitHub build
  server** (GitHub Actions, OIDC -> ACR `acrmemql`), NOT on an operator
  machine. This spans the repos in the project:
  - `memql` -> `.github/workflows/build-engine-images.yml` builds **every**
    node type (identity / bff / cognition / agent / planner / voice /
    workbench / mcp / edge) as one set of **product-agnostic** engine images.
  - the product's DSL-bundle repo -> a tiny **data-only bundle image** the
    engine mounts at runtime via `MEMQL_DSL_PATH`.
  - the product client repo -> its `build-spa-image.yml`.

  Each is `workflow_dispatch` on `main` with a `version` input; tags are
  immutable; the build server is the source of truth for deployable images.

Do NOT hand-build + push release images locally (`az acr build`,
`make release`, `docker push`) for a cloud deploy -- that path is superseded by
the build server. A release is `{engine version, bundle digest, client digest}`
pinned in **one overlay** (`deploy/k8s/overlays/<env>`): build the engine
images, pin those three digests, merge -> ArgoCD reconciles. See
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
   are wrong.** `--delete-branch` is REFUSED (`Cannot use -d or --delete-branch
   when merge queue enabled`) because the queue deletes the branch itself;
   `--merge` is ignored, since the queue's `merge_method` is already `MERGE`.

   **It ENQUEUES rather than merges, and the wait is by design.**
   `min_entries_to_merge_wait_minutes` is 5 and it batches under
   `grouping_strategy: ALLGREEN`, so a PR sits at `OPEN` with `mergedAt: null`
   for minutes with nothing wrong. Re-running the command answers `is already
   queued to merge` -- confirmation, not an error.

   **A queued PR can go `DIRTY`, and it will stay there.** When a sibling lands
   underneath it, `mergeStateStatus` becomes `DIRTY` and does not resolve
   itself; rebase on `origin/main` and force-push. A watcher looking only for
   merged / failed / clean cannot see `DIRTY` at all, and its silence is
   indistinguishable from "still queued".
2. **Merging your own PR: the owner uses the BYPASS, never a settings change.**
   The ruleset requires a code-owner review and that requirement stays on, but
   **GitHub never lets a pull request's author approve it** -- there is no
   toggle for that at any level. So "the owner's approval is required, and the
   owner may proceed on their own work" is one setting plus one bypass:

   ```
   require_code_owner_review: true                     <- the requirement, kept
   bypass_actors: RepositoryRole(admin), pull_request  <- the owner, on their own PR
   ```

   Use the script, which reports the policy it is bypassing and refuses on a red
   or unfinished build -- the bypass skips a REVIEW that cannot be given, never
   a failing check:

   ```bash
   scripts/dev/merge-as-owner.sh --pr=<n> --check   # policy + readiness, merges nothing
   scripts/dev/merge-as-owner.sh --pr=<n>           # merge
   ```

   **Do not "fix" this by lowering `require_code_owner_review`** -- that removes
   the requirement for everyone, which is a different policy.
3. **Pre-release -- no backwards-compat shims or deprecation windows.** When a
   contract changes, fix both MemQL and the consumer at once and delete what is
   no longer needed. No legacy adapters, fallback paths, or "keep working while
   we migrate" layers.
4. **Stage files by explicit path** (`git add <file>`) -- never `git add -A` or
   `git add .`. The repo owner runs multiple Claude sessions against this
   working tree, and untracked files from another session must not get swept
   into your commit.

**What triggers a frontend team ping:** a change that alters a wire contract the
frontend depends on (removed/renamed endpoints, changed required request fields,
new required response fields, new gRPC message types the client must handle to
get a complete response). Call it out explicitly in the commit body.
Backend-internal refactors that leave the wire identical don't need frontend
coordination.

---

## Architecture & Tech Stack

### Core Technologies
- **Language:** Go 1.26.1+
- **Database:** PostgreSQL 16 + TimescaleDB
- **API:** gRPC (`MemqlService.Stream` is the primary surface) + HTTP for the documented exceptions (OAuth, health, file uploads, Polyphon room tokens) + WebSocket bridge to the gRPC stream for browsers (`/memql/ws`)
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
packages keep the heavy vendor set constant in every build. Never justify a
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

The cluster (2 replicas per mesh node) is the runtime in the cloud and in the
local parity cluster, and it is the topology every feature must be designed and
tested against. Never reason about a feature as if it runs in a single process.
The blessed local repro is `make up SERVERS=2` + `make scale N=2`.

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

memql#4352 closed the WORKER half of that class: a cockpit machine's
`WorkerService` stream terminates on ONE agent replica, so at two replicas a
turn found the machine on a coin flip. `WorkerForward*` on `NodeService.Stream`
now forwards the dispatch to the replica named by `connectedNodeId`. Its gate is
an IN-PROCESS hop test (`integrations/agent/worker/forward_hop_test.go`) wiring
the real router to the real handler, not a `clustere2e` lane -- a live-cluster
gate is skipped on every CI lane and every developer machine, and a gate skipped
by default cannot be what stands between a feature and the bug it prevents.

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
fronting a product's bundle -- a deploy concern.

**What "never product code" is actually enforced by** -- two narrow guards, not
a general one, so know their edges (memql#3326):

- `TestEngineIsProductNeutral` (`product_neutrality_test.go`) -- a
  **banned-names list** of the specific product names this repo shed. It cannot
  notice a product arriving under a name nobody thought to ban.
- `TestClientsDirectoryIsAllowlisted` (`clients_allowlist_test.go`) -- an
  **allowlist of `clients/` inhabitants**. An unlisted directory fails.

Everything else -- generic-vs-product Go in `component/`, a product-shaped
concept in `dsl/` -- rests on review. Genuinely-bespoke product Go (rare)
becomes a thin optional `bff/` pack module in the product repo. Full rationale:
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
[environment-parity.md](docs/public/operate/environment-parity.md).

There is one cloud overlay (`deploy/k8s/overlays/cloud`), one ArgoCD Application
(`memql`), one namespace (`memql`), and everything in that overlay is a VALUE
over `deploy/k8s/base`. **Therefore: no `if env == "..."` in engine code.**
`TestNoEnvironmentBranchingInEngineCode` (`environment_branching_test.go`) fails
the build on engine code so much as NAMING the tier words, in any form --
comparison, switch case or map key -- and its exemption map is EMPTY.
`development` / `local` stay outside that gate: they distinguish deploy TARGETS
(k3d vs AKS), which carry their own field, `provider`.

What makes the local cluster parity rather than a lookalike:

- **Same manifests, same reconciliation as AKS**, and each pod carries a unique
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

- **ReplyTo pattern** -- request-response over channels via embedded reply channel
- **Default buffer** -- 64 items per channel, configurable via `ChannelConfig`
- **Telemetry hooks** -- channel fill-level, send/drop counters
- **Ready() signaling** -- all components expose `Ready() <-chan struct{}`

---

## Endpoint Protocol Policy (gRPC-First)

**IMPORTANT: This policy is a hard requirement for all MemQL development.**

gRPC is the **default and required** protocol for all internal and
service-to-service endpoints. HTTP endpoints are allowed **only** when an
external protocol requirement makes gRPC impossible.

1. **Service-to-service call?** → **Must be gRPC** (add a message type to `memql.proto`).
2. **Consumed by a browser client?** → route through the WebSocket bridge
   (`/memql/ws`), which tunnels to `MemqlService.Stream` -- still gRPC underneath.
3. **Does the external service require HTTP?** (OAuth callbacks, webhooks) →
   HTTP is allowed as a documented exception, below.
4. **When in doubt:** ask the user. Default answer is gRPC.

### Allowed HTTP Exceptions

These endpoints **must** remain HTTP because an external protocol requires it.
Each was approved individually; the shared reason is that the other party
dictates the wire (a browser, a mail client, a probe, a third-party webhook).

| Category | Endpoints | Reason |
|----------|-----------|--------|
| **Auth (identity service)** | `/login`, `/auth/magic-link`, `/auth/complete`, `POST /auth/landing`, `GET /auth/magic-link/status`, `POST /auth/magic-link/finish`, `/auth/logout`, `/oauth/token`, `/auth/refresh`, `/.well-known/jwks.json`, `POST /auth/webauthn/{register,login}/{begin,finish}`, `POST /device/code`, `GET+POST /device`, `GET /enroll` | OAuth 2.0 / magic-link needs HTTP redirects, browser form posts and JWKS publishing; WebAuthn is a **browser API**; RFC 8628 device grant is **defined over HTTP**; `GET /enroll` is a page a person opens from a link, before any application code exists to speak a protocol. The magic-link three (memql#4302): a GET now renders and never changes state so mail scanners stop burning links, `POST /auth/landing` is the form post it used to do, `/auth/magic-link/status` is the requesting tab's poll gated on the `memql_ml` cookie, and `finish` must be a real form POST because the reply is a 303 the tab must NAVIGATE. All identity routes are declared on the identity server's own route table, not `component/server`'s |
| **Health check** | `/healthz` | Docker and Kubernetes probes expect HTTP GET |
| **WebSocket upgrades** | `/memql/ws`, `/memql/audio` | Browsers need an HTTP upgrade |
| **File uploads** | `/spaces/{id}/attachments` | Multipart form-data maps poorly to gRPC |
| **Site bundle publish** | `POST /sites/{id}/bundles` (bff only) | A CI job hands over an arbitrary tree of files -- unknown paths, count and content types -- which is what multipart exists to carry and a fixed protobuf schema does not (memql#3713). `component/edge.Publisher` makes it atomic: the bundle lands under a content-addressed version prefix and only then does the site row's `bundleRef` flip. Authorization is a `class="service_account"` JWT; declared in `HandlerAuthorizedPaths()`, never `PublicPaths()`. Served by the bff, never the edge |
| **Inbound webhooks** | `POST /inbound/{source}` (bff only) | The third party dials US and will POST to a URL and nothing else (memql#2957). Deny-by-default source allowlist + per-source HMAC; `HandlerAuthorizedPaths()`. [inbound-delivery.md](docs/public/operate/inbound-delivery.md) |
| **One-click unsubscribe** | `GET+POST /unsubscribe` (bff only) | Here the third party is the RECIPIENT'S MAIL CLIENT (memql#3348); RFC 8058 is a contract with Gmail / Outlook / Yahoo. The GET/POST split is load-bearing: mail clients PREFETCH links, so a GET with the side effect unsubscribes people who never clicked. Authorization is an HMAC-signed token carrying (owner, recipient, campaign), verified before any row is read. `HandlerAuthorizedPaths()` + `SelfAuthenticatedPaths()` |
| **Campaign open/click tracking** | `GET /t/o/{token}`, `GET /t/c/{token}` (bff only) | The same third party as `/unsubscribe`, one row above: the RECIPIENT'S MAIL CLIENT (memql#4823, owner-approved under program P3). A pixel is an `<img src>` a mail client fetches and a tracked link is one a reader follows -- neither has a gRPC form, and both are GETs because that is what the client will issue. Authorization is an HMAC over the same key ring the unsubscribe token uses under a DIFFERENT context string, so neither token verifies as the other. The click destination lives INSIDE the signed payload rather than in a query parameter, which is what makes the redirect open-redirect-proof. Neither ever shows a human a failure: the pixel always answers the 1x1 GIF and records only on a valid signature, and a bad click token renders the same "link is not valid" page an unknown-key unsubscribe link gets, never a 500 or a redirect. **The token must be a SINGLE PATH SEGMENT** -- `SelfAuthenticatedPaths()`' exemption is bounded to one segment under the mount, so a token containing `/` is 401'd before the handler runs (base64url or the dot-delimited unsubscribe shape; standard base64 is not safe here). `server.TrackingPaths()` -> `HandlerAuthorizedPaths()` + `SelfAuthenticatedPaths()`; the literals must agree with `campaigns.TrackingOpenPath` / `TrackingClickPath` or the front door routes a path no mail client fetches |
| **Library artifacts** | `POST /artifacts`, `GET /artifacts/{id}/content`, plus the chunked-session family `POST /artifacts/uploads`, `GET /artifacts/uploads/{id}`, `PUT /artifacts/uploads/{id}/chunks/{n}`, `POST /artifacts/uploads/{id}/complete` (bff only) | Upload is the multipart reasoning already recorded for attachments (memql#4341, D1). The session family (memql#4782, owner-approved in that design session) is the same file-transfer exception grown production-grade: files past the one-shot threshold arrive as 16 MiB chunks staged against Azure block blobs -- replica-agnostic by construction, resumable via the inventory `GET`, verified at `complete` (staged bytes must equal the declared size) -- which is byte transport and exactly what HTTP is for. `GET .../content` STREAMS through the bff after re-resolving the row under the caller's actor, now honoring single-range `Range` (206) -- never a redirect, because there are no signed URLs here and a redirect would move authorization from the graph to whoever holds a URL. All are ordinary AUTHENTICATED routes, so they appear in none of the three aggregates; `server.ArtifactPaths()` routes them (the session paths live under the `/artifacts` prefix). Caps: `MEMQL_LIBRARY_MAX_UPLOAD_BYTES` (default 4 GiB, per file) and `MEMQL_LIBRARY_USER_QUOTA_BYTES` (default 100 GiB, per user) |

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

**It is NOT a certificate fact (memql#4224).** ACME cannot issue a wildcard over
HTTP-01, and ONE wildcard dnsName fails the WHOLE order -- so the front-door
certificate names EXACT hosts only, every Ingress lists exactly its own exact
rule hosts under `tls`, and the union of those lists equals the dnsNames
(`deploy/k8s/overlays/frontdoor_hosts_test.go` gates all three). The wildcard
RULE stays with no certificate behind it WHERE THE OVERLAY DECLARES NO DNS-01
ISSUER; both cloud overlays now declare one (memql#4347) plus a wildcard
Certificate the edge Ingress carries, so a freshly deployed site is live over
TLS with no operator step. The render gate reads the SOLVER rather than the
issuer's name, so #4224's exact-host rule holds by default. The portal carries
an exact rule because ingress-nginx builds a certificate-bearing server block
per RULE host, never per tls host.

`cmd/frontdoorhosts` writes `front-door.generated.yaml` into each instance
overlay; `component/envregistry/domain.go` composes the node's own issuer / CORS
origins / redirect URIs from the SAME rule through `component/frontdoor`; and
`component/memql`'s SeedMaterializer seeds the portal site row's hostname from
it. One derivation, three consumers -- a second copy would disagree, and the
disagreement is an issuer nothing is served at, which presents as "sign-in is
broken" with every manifest looking correct.

Adding a ROLE is a design change, not a configuration change. The LOCAL
overlay's five front-door files stay hand-authored (traefik, not nginx) but are
gated against the same derivation, and its mkcert pair is still a wildcard --
the one place local is more permissive than the cloud, so **a site that works
over https locally is no evidence it has a certificate in the cloud.**
[front-door.md](docs/public/operate/front-door.md).

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
  There, adding a rule makes the endpoint externally reachable, and for anything
  in `PublicPaths()` (which the verifier bypasses) that means exposure.
  `/metrics` is the case: unauthenticated *because* in-cluster-only, and mounted
  on every node type. Hence a fourth classification,
  `servedButNotExternallyRouted` (`/metrics`, `/api/concepts*`). **"When in
  doubt, include" applies only to the previous bullet.**

Two gates make it non-recurring: `TestFrontDoorPathsAreNotStale`
(`make frontdoor-paths-check`) and `TestEveryServerPathDeclarationIsClassified`,
which AST-scans `component/server` for every `func …Paths() []string` and fails
when one is classified by none of the four maps.

Note the word *declaration*: a route mounted through `handleRoute` with an
inline path literal and no `*Paths()` declaration is invisible to the generator,
and the boot check that would catch it (`AssertUnauthenticatedSurface`) runs
only when the node installs **no** verifier -- which the bff does. **Declare new
HTTP routes with a `*Paths()` function**; that is what puts them inside the
gate. Do not hand-edit the generated block, and do not "simplify" the generator
back to the three aggregates -- both changes look like cleanups.

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

1. **Never add new HTTP endpoints** without explicit user approval.
2. **Default to gRPC** -- add message types to `component/grpc/memql.proto`.
3. If you believe HTTP is needed, **ask the user first** and document the reasoning.
4. All new gRPC messages follow the existing multiplexed stream pattern: request
   type to `MemqlClientMessage.oneof payload`, response type to
   `MemqlServerMessage.oneof payload`, handler in `component/grpc/server.go`.

---

## AI Integration

All AI operations go through a pluggable provider system: unified interfaces
(`ChatAIProvider`, `VisionAIProvider`, `TTSAIProvider`, `ChatStreamProvider`)
over OpenAI (chat, vision, TTS, STT) and Anthropic (chat, vision). Provider
records live in `dsl/providers/providers.memql`; selection is the configured
default, or per-request via the `provider` parameter.

**The Anthropic credential is a static key locally and workload identity
federation in the cloud** (epic memql#4333) -- the engine presents the pod's
projected Kubernetes token and the SDK exchanges it for a one-hour bearer, so
no long-lived vendor key is at rest. All four ids or none: a partial config
REFUSES BOOT rather than falling back to a key the cutover deletes.
[anthropic-federation.md](docs/public/operate/auth/anthropic-federation.md).

### AI Endpoints (gRPC on `MemqlService.Stream`)

- `AiChatMsg` / `AiChatResult` / `AiStreamChunk` -- chat completions (streaming + non-streaming)
- `AiSpeechMsg` / `AiSpeechResult` -- text-to-speech
- `AiTranscribeMsg` / `AiTranscribeResult` -- speech-to-text (batch)
- `AiTranscribeStreamStart` / `Chunk` / `End` -> `AiTranscribeStreamDelta` / `Complete` -- streaming transcription
- `AiSuggestMsg` / `AiSuggestResult` -- carries `domain` ∈ {spaces, spaceTitle,
  agents, groups, groupDescription, agentCardSummary, spaceCardSummary,
  groupCardSummary, knowledge, viewArrangement, viewCompose}.

Two things about `AiSuggest` are load-bearing:

- **`viewArrangement` / `viewCompose` render DSL prompts**, not Go copies
  (`component/memql/suggest_views.go`, declared in
  `dsl/portalviews/prompts.memql`); a nil renderer is an ERROR rather than a
  fallback, because a built-in substitute would serve a prompt nobody can find
  in the tree. **When adding a domain, the registration IS the feature; test
  it** -- `viewArrangement` was called by the portal and registered by nothing
  until memql#4667, and the registry's unknown-domain error reaches the user as
  "suggestions are not available on this cluster", the same sentence a cluster
  with no provider gets.
- **`AiSuggestResult.usage` is ABSENT when nothing was reported** -- zero and
  "not measured" are different answers, and a client falls back to its own
  estimate only if the field is missing.

Cross-node proxying (BFF -> Voice, BFF -> Agent, ...) rides `AiForwardRequest` /
`AiForwardResponse` on `NodeService.Stream`. Handlers:
`component/grpc/{ai_handlers,ai_transcribe_stream,ai_forward}.go`, which emit a
short error id via `generateErrorId()` (`ERR-{6 hex}`), visible in slog output
as `"errorId":"ERR-..."`.

### Voice + Video Pipeline (Go voice-agent)

The realtime voice + video channel is owned by the Go voice-agent in
[`integrations/voice/agent/`](integrations/voice/agent/), shipped as the
`voice-agent` subcommand of `memql-voice` (`make voice`, CGO_ENABLED=1,
`-tags voice`). It joins LiveKit rooms as the General Assistant's voice + video
participant; specialists are text-only by design and never publish into the
room.

```
LiveKit room -> OpenAI Realtime STT -> memql gRPC (VoiceAgentTurnRequest -> Delta)
             -> memql cognition (conductor + agent tool loop)
             -> OpenAI TTS -> Anam or Simli avatar (lip-synced video)
```

Two executors, selected by `MEMQL_VOICE_EXECUTOR`: `realtime` (default --
OpenAI gpt-realtime speech-to-speech) and `cascade` (the STT -> cognition -> TTS
path above). The CGO-free vendor REST core for avatars lives in the shared
`integrations/avatarvendor` package.

- **Auth:** identity-issued `class="voice_agent"` JWT, pinned to the
  `VoiceAgent*` message surface by
  `component/grpc/voice_agent_stream_interceptor.go`. The voice-agent cannot
  write graph rows directly; memql does that server-side. Mint via
  `make voice-agent-token` or self-bootstrap
  ([voice-agent-jwt.md](docs/public/operate/auth/voice-agent-jwt.md)).
- **Deployment gotcha:** LOCALLY the voice lane uses a LiveKit Cloud project and
  is GATED on the operator's credentials (memql#2416) -- without `LIVEKIT_URL` /
  `LIVEKIT_API_KEY` / `LIVEKIT_API_SECRET` at `make up` / `make secrets`, voice
  + voice-agent scale to 0 with a loud warning, because the binaries fail-fast
  on missing env by design. The cloud stays self-hosted
  (`deploy/k8s/base/livekit.yaml`).
- **Env** (full list in the runbook): `MEMQL_GRPC_ADDR`, the three `LIVEKIT_*`,
  `MEMQL_OPENAI_API_KEY` (required), `MEMQL_VOICE_EXECUTOR`,
  `MEMQL_VOICE_ROOM_NAME`, `MEMQL_VOICE_IDLE_TEARDOWN_SECONDS` (default 60,
  #1378 -- stops the dispatcher wedging on an empty room),
  `MEMQL_VOICE_MAX_ROOMS` (default 8, #1395 -- cross-replica double-serve is
  prevented by skipping rooms already containing a `-ga` participant),
  `MEMQL_REALTIME_*`, `VOICE_AGENT_TOKEN`, `MEMQL_AVATAR_VENDOR` (`anam`
  default / `simli` / `none`) + the vendor key.
- **Canonical voice catalog** (`integrations/voice/voices.go`): every agent
  carries a provider-agnostic voice name (alto / soprano / tenor / baritone /
  ...) on `providerConfig.voice.voiceId` plus a `gender` enum; cognition
  resolves canonical -> provider voice id at TTS-publish time via
  `MEMQL_POLYPHON_VOICE_PROVIDER`. Auto-assigned at agent creation, never
  user-edited. Builtins `voicePickForGender` + `voiceResolve`.
- **Per-agent audio + video control:** `v1:agents:agent.audioControl` +
  `videoControl` (`always_on` | `always_off` | `mirror_user`, default
  `mirror_user`) seed the defaults; `v1:cognition:audioOverride` +
  `videoOverride` carry per-(space, agent) session overrides. Mutations
  `setAgentAudioOverride` / `setAgentVideoOverride`; queries
  `audioOverridesForSpace` / `videoOverridesForSpace`. An empty
  `avatarPersonaId` makes the voice-agent disable the avatar plugin and fall
  back to audio-only.

### Cognition (Routing + Conductor)

Cognition decides whether and which agent responds to an utterance, then
dispatches the turn. The text path uses a **single LLM brain**: the conductor
(`dsl/cognition/prompts/conductorTurn.tmpl`) emits both the routing decision
(fitScore / turnMode / handoff / severity) and the per-agent plan in one
structured-output call. The standalone router LLM call fires only for voice
utterances (latency-sensitive); fast-path mention dispatch bypasses both.
`integrations/cognition/{cognition_handler,conductor_consult,ai_router}.go`.

- **Capability-aware routing.** Conductor and voice router both see each
  candidate's tool list; tool-fit mismatch drops fitScore by 0.4+, and a total
  tool gap routes to the GA with `turnMode=escalation_notice`.
- **Conversational continuity.** The conductor gets an explicit `lastResponder`
  input and must keep the primary with that agent on a follow-up shape absent
  an @-mention or domain pivot -- plugs "GA jumps in to defer to the specialist".
- **Greet-on-join pacing** (`greet_on_join.go`): 3s initial delay, 4s minimum
  gap, process-local. Cross-replica exactly-once is `dispatchGate.tryGreet`
  (#1386), a Postgres advisory-lock gate keyed on (space, agent). The greeting
  directive is "familiar" for ALL agents -- every agent is one the user created
  and named, so the "Hi, I'm X" opener is forbidden across the board.

### Agent reply envelope (`respondToUser`)

Every user-facing chat reply is delivered through a structured-output envelope,
not free-form prose. The agent ends every turn with a sentinel `respondToUser`
tool call carrying `{response, citations[]}`; the streaming tool loop
intercepts it by name (no engine executor exists for it), parses the args as
`Envelope`, and uses that as the turn's final text + citations.
`integrations/agent/{envelope,streaming}.go`; enforced by the OUTPUT CONTRACT
block at the top of `dsl/cognition/prompts/cognitionReply.tmpl`.

`citations` is a list of `{domainId, matchedPhrase}` pairs naming
knowledge-domain sources; cognition stamps them on
`v1:cognition:utterance.citations` and the frontend turns each `matchedPhrase`
into a chip linking to the domain. Empty array when no trained sources were used.

### Coding Agent -- the container-executor seam

The seam spent its whole life EMPTY (memql#4120). **`cockpit-app` is its first
and only inhabitant** (epic memql#4358): Claude Code or Codex, headless, on a
machine the USER owns, through the worker stream, with MemQL's tools reachable
over MCP.

- **The seam:** `component/planner`'s `RegisterContainerExecutor(name, exec)`.
  Lookup keys on the part BEFORE the colon, so `cockpit-app:claude-code`
  reaches the one registered `cockpit-app` backend with the app id on the
  suffix -- growing the app list is a value change, not a release.
  `ValidateExecutorBackend` refuses an unregistered name at TASK CREATION
  rather than at dispatch.
- **The backend:** `integrations/agent/worker/cockpitapp.go`, registered from
  `init()` under the `agent` build tag (only an agent node holds worker
  streams). It reuses `preDispatchCheck` UNEXPORTED in the same package on
  purpose -- an app run needs exactly the gates `workerHost` needs, and a
  second copy of those gates is a copy that drifts.
- **Legacy `claw` fields are still present and unused by any of this.**
  `v1:agents:agent.claw` + `clawWorkspace`, read by `ClawCapable()` in
  `integrations/cognition/ai_responder.go`, plus display strings in
  `tool_labels.go`. No `tool claw*` under `dsl/`, no sidecar, no `CLAW_*` env
  var. Do not wire them to `cockpit-app` on the assumption that they belong
  together.

### Local apps as execution surfaces (epic memql#4358)

Delegating a task to an app the user already pays for, on a machine they own.
Full record: [local-apps.md](docs/public/operate/local-apps.md).

**Transport is the worker stream; MCP is the back-channel.** The engine cannot
dial a machine behind NAT -- the stream the cockpit opened outward IS the
tunnel. Each run gets a per-run credential and the `mcp.<domain>` endpoint.

- **The runnable app set is CLOSED in the engine** (`claude-code`, `codex`). A
  cockpit may report any id; unknown ids are stored on the registration and
  produce no routing label, so a newer cockpit never makes the engine attempt a
  protocol it does not have.
- **A machine is selectable only when BOTH `allowed`** (its own `policy.yaml
  apps.allow`) **and `signedIn`.** Selection is the Fleet router asked for the
  `app:<id>` label; nothing else picks between machines. A session runs only on
  the replica holding that machine's stream -- there is no cross-node forward
  for the app-session envelope yet, so a machine on a sibling replica is SKIPPED
  during selection rather than failing the run.
- **A run is a SESSION, not a dispatch.** `AppSessionStart / Chunk / Control /
  End` on `WorkerService.Stream`. Chunk `seq` is monotonic; out-of-order or
  duplicate chunks are DROPPED, because a transcript is a record and an
  interleaved replay corrupts it undetectably.
- **Delegation is a PREFERENCE WITH A FALLBACK**
  (`v1:worker:delegationPolicy`): no allowed, signed-in, online machine means
  the task runs in-process. A plan never waits for a laptop to wake up.
- **The back-channel credential's `sub` is the OWNING USER's id**, so row authz
  applies to the app exactly as to their browser, and the service-account
  surface pin plus `role=system` keep it off every credential mutation and
  cluster-owner gate. Minted via `POST /node/bootstrap` with
  `tokenClass="app_session"`, which WIDENS what the bootstrap secret buys; four
  things narrow it -- the named user must exist, the TTL is hard-capped at 8h,
  the surface stays read/query-pinned, and the session id is the token label.
  **It is NOT revocable** (the DB-free JWKS verify path is what lets it work on
  every node); standing in for revocation are the short lifetime, the cockpit
  deleting the MCP config at end, and renewal-in-place.
- **Subscription spend is counted and does not burn the dollar ceiling.**
  `v1:router:call.billing` + `executionSurface`, `plan.tokenSpentSubscription`.
  The DOLLAR ceiling EXCLUDES subscription tokens (MemQL was not billed); the
  LOOP caps INCLUDE the call (a runaway loop is still a runaway loop). Billing
  falls to `unknown` whenever the app's usage report or the machine's
  subscription signal is silent; it is never inferred.

The cockpit half (app detection, `apps.allow`, the session runner, the MCP
config writer and its cleanup, Library pull/push, the `open` kind) lives in
`memql-cockpit`. This repo fixes the protocol and the engine side.

### Workers (computer_use_headless / computer_use_embodied)

Agents drive the user's own machine: shell exec, filesystem, HTTP fetch, and
(under the computer-use build) mouse + keyboard + screenshot. Runbook:
[workers-runbook.md](docs/public/operate/workers-runbook.md). The sandboxed
first-choice surface for headless work is the Workbench, below.

- **Capabilities:** `computer_use_headless` -> `workerHost`;
  `computer_use_embodied` -> `workerComputer`; both plus the cross-cutting trio
  (`workerStatus`, `requestComputerUseScope`, `canvasPublish`). Two slugs so the
  slices grant independently; authorization stays one decision, because both act
  on the user's machine. Expansion map: `component/memql/worker_caps.go`.
- **Gateway:** `WorkerService.Stream` on the agent node, authenticated by
  `mql_wkr_<43>` tokens (the `worker_token` variant on `v1:identity:identity`),
  which the interceptor admits on that path only. Mint/revoke server-side via
  `CreateWorkerTokenMsg` / `RevokeWorkerTokenMsg`; the plain token comes back
  ONCE and only the SHA-256 hash persists (`component/identity/workertoken/`).
- **Worker side:** `memql worker run` / `memql worker setup`, run modes of the
  `memql` command built by `memql-cockpit`. Install:
  `scripts/install/install-{mac,linux}.sh`.
- **Per-user routing, and NO machine id.** Every worker is owned by one
  `v1:identity:user`. The dispatch builtins take `requireLabels` /
  `preferLabels` and **no `workerId`** (D4): an agent says what the work NEEDS,
  the owner's policy decides where it lands -- so **a hallucinated machine id is
  a failure mode this surface does not have**. `no_worker_available` names every
  machine and why it was ruled out.
- **Permission model:** three layers checked BEFORE dispatch -- the agent
  capability flag, standing scope on
  `v1:agents:agentAuthorization.computerUseScope` (observe / full; `interact` is
  RETIRED, kept in the enum for old rows and read as `full`), and the per-Plan
  kill switch `v1:identity:user.preferences.computerUseEnabled`. Out-of-scope
  calls park the Plan at `awaitingFeedback` with
  `feedbackReason=scope_elevation_required`. Consent is decided BEFORE routing,
  so routing only ever chooses among machines already consented to.
- **The router** (`integrations/agent/worker/router.go`, epic memql#4349):
  `v1:worker:routingPolicy`, one active row per user and ABSENT for most (the
  router then applies `firstFit` + `nextMatching`, the pre-router behaviour).
  Four strategies -- `firstFit`, `roundRobin`, `leastLoaded`, `labelMatch` --
  all STABLE sorts over REGISTRATION order taken from the row rather than
  connection order, so every replica agrees with no shared state.
  `fallback=nextMatching` re-picks ONLY after a refusal BEFORE start (D5); an
  exec that lost its stream may have run. Policy and agent requirements are
  AND-ed and a conflict is left UNSATISFIABLE rather than resolved toward either
  side. Labels match EXACTLY -- there is no "any value" form.
- **Two label maps, and they must not become one.** `labels` is reported by the
  cockpit and OVERWRITTEN from `Register` on every reconnect; `operatorLabels`
  is the owner's and no register/heartbeat path writes it.
  `refreshWorkerRegistration` enforces the split by NOT NAMING the field --
  `update{}` is a read-merge, so the prohibition is the ABSENCE of a line and
  "completing the field list" is what would break it (`displayName` is absent
  for the same reason). Routing matches the MERGE, operator side winning.
- **`online` is DERIVED, never stored:** unrevoked AND `lastSeenAt` within
  `OnlineWindow` = 2 x `HeartbeatBatchInterval` = **30s**. Exactly two
  implementations, `component/worker.IsOnline` and
  `clients/portal/src/fleet/online.ts`, held together by
  `TestFleetOnlineWindowMatchesPortal`. Deriving it from the in-memory registry
  is refused: that answers "connected to ME", and the fleet needs "connected to
  ANY replica".
- **Cross-node dispatch (memql#4352):** `connectedNodeId` names the replica
  holding the stream; any other replica forwards over `WorkerForward*`.
  `refused_before_start` is the re-pick predicate and the one wire field that
  must never be guessed. The receiver re-checks only what it alone can know --
  ownership against the verified `ForwardedAuthority` (never the envelope's
  owner field) and revocation.
- **Row tier + borrowed authority.** `v1:worker:registration` and
  `routingPolicy` declare `@rowAuthz(owner=..., clusterOwner)`, and the READ
  gate has no internal-origin bypass: an unstamped read returns ZERO ROWS, not
  an error. A worker authenticates as `worker:<id>`, so `component/worker`'s
  store runs every registration read and write under `auth.ContextWithUserActor`
  for the owner named by the token's identity row -- and must NOT stamp internal
  origin, which is why it is deliberately absent from `call_origin.go`'s
  allowlist.
- **Operator surface:** `/fleet/machines` in the portal -- pair, rename
  (`displayName`), edit operator labels, revoke, edit the routing policy, read
  each call's `routing` record.
- **Audit + hardening:** security signals on `v1:identity:auditEvent`; per-call
  telemetry on `v1:worker:invocation` (`WORKER_INVOCATION_RETENTION_DAYS`
  default 90); per-call rlimits on Linux + Darwin via `policy.shell.max_*`;
  optional setuid drop via `policy.shell.run_as_user`; loopback-only metrics at
  `127.0.0.1:9100/metrics`.

### Workbench (workbench_use)

The default first-choice surface for HEADLESS agent work -- writing files,
running shell commands, fetching URLs -- as a per-Plan sandboxed Linux working
directory in the cluster. Nothing on the user's machine is touched. Computer-use
is the FALLBACK for what the workbench cannot do (macOS-only tooling,
computer-use control, files already on the user's computer).
[workbench-runbook.md](docs/public/operate/workbench-runbook.md) ·
[workbench-production.md](docs/internal/ops/workbench-production.md).

- **Capability:** `workbench_use`, universal -- injected into every role's
  `lockedToolSlugs`. No scope grants, no kill switch; the blast radius is the
  per-Plan directory tree.
- **Tool:** `workbenchHost`, discriminated by `action` (exec / fs_read /
  fs_write / fs_list / fs_stat / http_fetch). It lives in a product DSL bundle,
  not the engine tree; the wire path is the `workbenchDispatchHost` builtin in
  `dsl/workbench/builtins.memql` to `integration.workbench.dispatchHost`.
- **The environment hint and the reroute (memql#4353).**
  `workbenchDispatchHost` takes an OPTIONAL `environment { os, needs[] }`,
  `needs` from a closed four-value set (`display` / `gpu` / `macos_tooling` /
  `user_files`) naming exactly the things a workbench is not. A mismatch returns
  a typed `environment_mismatch` having run NOTHING; an UNKNOWN need is the
  separate code `invalid_environment_hint`, so a typo can never read as "the
  workbench cannot do this" and send a call to somebody's laptop. Omitted means
  no hint, and there is deliberately no default -- a guessed one would refuse
  calls that would have worked. On a mismatch the tool loop re-dispatches the
  SAME call to the fleet and the dispatcher's existing gate decides: only
  `denied_no_per_task_approval` / `denied_by_scope` raise the consent card;
  everything else is the answer, `kill_switch_engaged` included. `needs` ->
  scope/labels is `integrations/agent/worker/scope.go` (`user_files` alone is
  `observe`; everything else and anything unrecognised is `full`).
- **Per-Plan workspace** under `MEMQL_WORKBENCH_ROOT/{planId}/` (default
  `/var/lib/memql/workbenches/`): lazy-provisioned on first call, persists across
  calls within a Plan, torn down on Plan terminal status by the
  `releaseWorkspaceOnPlanTerminal` automation. The row is
  `v1:workbench:workspace` (status, storageRoot, lifecycle timestamps, `nodeId`,
  `ownerUserId`), declaring `@rowAuthz(owner=..., clusterOwner)`. `ownerUserId`
  is stamped from the parent plan's `requestedBy`, and a call whose `planId`
  does not resolve to a readable plan is REFUSED
  (`workspace_owner_unresolved`) rather than run -- a row written under a blank
  actor is readable by nobody, including the operator answering "where did my
  file go" (memql#4354).
- **Replica affinity (memql#4354).** Base runs 2 workbench replicas and a
  workspace is a FILESYSTEM, which does not follow the request. `nodeId` names
  the replica holding the directory and the peer picker prefers it, falling back
  to any-fit only when that node is gone -- any-fit alone gave one plan two
  directories on two disks and told neither side. On node loss the orphan row is
  released `node_lost` and a FRESH workspace is provisioned: **files are NOT
  migrated.** The log line and the row state ship; a canvas card does not
  (canvas is pack-only).
- **Modes.** Cluster mode is the deployed default: a dedicated `workbench`
  node-type binary hosts the workspaces, agent nodes route via
  `WorkbenchForwardRequest` / `Response` on `NodeService.Stream`. Base sets
  `MEMQL_WORKBENCH_REMOTE=1`; the dialer needs
  `MEMQL_WORKER_PEERS=workbench=<addr>`. **The remote flag is an ASSERTION, not
  a preference:** set, with no reachable peer, a call is REFUSED
  (`no_workbench_peer`) rather than run on the agent's own disk. In-process
  fallback is the flag unset, or the explicit
  `MEMQL_WORKBENCH_LOCAL_FALLBACK=1` under it.
- **Operator surface:** `/fleet/workbenches` (replicas + per-plan workspaces,
  live and released); `/fleet/machines` is its worker counterpart. Both are live
  because the `graph.node.*` events for `v1:worker:registration`,
  `v1:worker:routingPolicy` and `v1:workbench:workspace` carry broadcast routing
  rules (`component/node/routing.go`) -- those rows are written on the agent and
  read on the page the bff serves, and without them default-deny leaves the list
  correct on load and frozen after, which looks like it is working.
  `v1:worker:invocation` is excluded on volume grounds.
- **Routing preference:** `cognitionReply.tmpl` and the `workbench` knowledge
  domain (auto-attached by `replier.go` when the expanded tool list includes
  `workbenchHost`) instruct the agent to prefer workbench over computer-use and
  to surface a "workbench can't do this -- needs computer use" message rather
  than silently retrying.

## Authentication

The in-house **identity service** (`component/identity`) is the authentication
provider for the cluster. It runs as its own node-type binary (`make identity`)
and owns magic-link auth, WebAuthn passkeys, enrolment tokens, OAuth-style token
endpoints (`/oauth/token`, `/auth/refresh`), the JWKS feed at
`/.well-known/jwks.json`, a public web UI (`/login`, `/auth/complete`, `/setup`,
`/legal/*`, `/me/*`), and PAT issuance for CLI clients (`mql_pat_<...>`).

Other binaries verify identity-issued JWTs locally via the per-node verifier
(`component/identity/verifier`), which fetches JWKS on a 5-min background
refresh and on demand for unknown `kid` headers. They never see the private key.
`MEMQL_IDENTITY_VERIFIER_BASE_URL` configures the verifier;
`MEMQL_IDENTITY_BASE_URL` configures the identity service itself.

**What is worth knowing before touching this tree:**

- **Magic links are device-bound and approve-on-click** (epic memql#4300). The
  requesting browser holds the `memql_ml` cookie whose digest is
  `magicLinkRequest.bindingHash`; a link only COMPLETES there, and a click
  anywhere else only APPROVES while the requesting tab polls and finishes
  itself. **A session can only ever land on the device that asked for it.**
  `GET /auth/complete` renders and writes nothing (prefetchers are harmless);
  consume is a compare-and-swap under a Postgres advisory lock, because
  approve-on-click gives one request two legitimate finishers.
- **`signInPolicy` on `v1:identity:user`** (memql#4304) is `any` (default) or
  `passkey_only`, which disables sign-in LINKS: a request writes no row, sends
  no link, redirects identically (no enumeration signal) and mails a notice.
  Enabling it requires an active passkey. Owners/admins can RESET it to `any`
  over `IdentityAdminMsg` -- one direction only, so an admin cannot lock a
  colleague out of their own account.
- **A new-sign-in email fires on every `authSession` row** (memql#4305), from
  the one seam that creates them. No action link, deliberately: an
  unauthenticated revoke link mailed to a shared mailbox is a DoS handle for
  everyone who can read it. Refresh rotations never send it.
- **Passkeys are usernameless** (memql#3407): the login challenge carries an
  EMPTY `allowCredentials` and resolves by credential id alone, which is why
  that id is unique cluster-wide. RP id derives from `MEMQL_IDENTITY_BASE_URL`,
  never from the request Host. The challenge holds the in-flight OAuth context
  server-side, so **no client learns which factor ran**. A sign-count regression
  is refused and audited as the cloned-authenticator signal. Revoke on
  `/me/devices` is a SOFT delete -- the row is audit history and its credential
  id must stay taken.
- **Enrolment tokens** (`mql_enr_<43>`, memql#3408) remove email from the
  critical path: single-use, TTL'd, authorizing exactly ONE action -- register a
  passkey as the named user. `GET /enroll` renders; the ceremony presents
  `Authorization: Enrolment <token>`.
- **The admin web app is gone.** `/admin/*` keeps the sign-in pages and a root
  that answers `410 Gone`; the screens live in the portal, gated by
  `component/identity/adminops` over `IdentityAdminMsg`.
  `DeployControlService` exists only on the identity node, but a bff FORWARDS
  the deploy RPCs over `NodeService.Stream` carrying the caller as a verified
  `ForwardedAuthority`, so owner-only gates run against the originating human
  rather than the relaying node (`component/grpc/deploy_control_forward.go`).

**Authentication is ON by default everywhere** (local and cloud alike). The
master toggle is `MEMQL_IDENTITY_ENABLED`: on verifier-consuming nodes it
defaults to `true` and is set `false` ONLY to disable auth for troubleshooting
-- the node then skips the verifier and admits every stream as a synthetic
`local-dev` cluster owner (`component/grpc/local_dev_stream_interceptor.go`),
with a loud boot-time SECURITY warning and the `memql_auth_enabled` gauge pinned
to 0. **Never set it false in a cloud cluster.** Disabling auth is that toggle,
NOT blanking `MEMQL_IDENTITY_VERIFIER_BASE_URL` (an empty verifier URL fatals
the node).

**Two operator credentials, deliberately separate since memql#3519.**
`MEMQL_MASTER_KEY` DECRYPTS; `MEMQL_OPERATOR_KEY` AUTHENTICATES the
`Authorization: Operator <key>` bearer that admits a stream as a synthetic
cluster owner. They were one value, which made a key the installer wrote into a
world-readable `~/.bashrc` a cluster-owner bearer token over the network. No
fallback -- an unseeded cluster refuses operator streams.

See [docs/public/operate/auth/](docs/public/operate/auth/):
[access-model.md](docs/public/operate/auth/access-model.md) (enforcement layers
and role spectrum) ·
[user-provisioning.md](docs/public/operate/auth/user-provisioning.md) ·
[identity-service.md](docs/public/operate/auth/identity-service.md) (operator
env vars + key management) ·
[operator-credential.md](docs/public/operate/auth/operator-credential.md)
(rotation sequencing for both keys) ·
[service-account-jwt.md](docs/public/operate/auth/service-account-jwt.md) (the
`class="service_account"` machine identity, #691 -- verifies on the BFF/mesh via
JWKS where a PAT can't, surface-pinned to the read/query path).

- [oidc-federation.md](docs/public/operate/auth/oidc-federation.md) -- Entra ID
  / generic OIDC as an UPSTREAM provider (epic memql#4611), discovery-driven so
  Entra is a configuration rather than a code path. Three load-bearing rules:
  **the email must be VERIFIED before it can link** to an existing account (an
  unverified claim is a string the directory did not check, and linking on it is
  account takeover); **`(issuer, subject)` outranks email** once it exists, so a
  rename moves nobody's account; and **exclusive mode exempts the OWNER**, or a
  cluster whose IdP is unreachable would have nobody able to sign in. A
  half-configured provider REFUSES BOOT rather than presenting a button that
  fails per-user.
- [recovery-key.md](docs/public/operate/auth/recovery-key.md) -- the owner
  BREAK-GLASS credential (epic memql#3958): `mql_rec_<43>`, bound to one owner,
  authorizing exactly one action (register a passkey as that owner) and REFUSED
  while the owner still holds a usable sign-in route. Single-use: redeeming
  spends it and mints an unclaimed successor, so a leaked key is worth one
  passkey registration and the cluster is never without a route back in.

## DSL Tree Layout

The DSL tree is **flattened per construct**: every namespace gets one directory
under `dsl/<namespace>/`, and within it each construct kind is consolidated into
a single `<construct>s.memql` file (e.g. `dsl/cognition/queries.memql`,
`dsl/identity/concepts.memql`, `dsl/providers/providers.memql`). The flattened
tree is produced by
[`scripts/restructure-by-construct`](scripts/restructure-by-construct/main.go).
Authoring reference skeletons live under `dsl/_reference/` (`_concept`,
`_shape`, `_spec`, `_trait`, `_agent`). Loaders read through `Source()`, which
routes through [`core/dslfs`](core/dslfs/dslfs.go).

### `MEMQL_DSL_PATH` — runtime product-DSL delivery

`MEMQL_DSL_PATH` mounts **additional product-DSL domains from disk at boot**, so
a product-agnostic engine image runs a product's DSL with zero compiled-in
product code (#2472). When set, `dsl.MountRuntimeDomainsFromEnv` (called before
the first tree walk) scans the root for product-domain sub-directories and
registers each into the unified tree via `RegisterTree(domain,
os.DirFS(<root>/<domain>))` -- the same call the embedded tree uses. No-op when
unset. Layout mirrors the embedded tree, one directory per product namespace:

```
$MEMQL_DSL_PATH/
  <productDomain>/
    concepts.memql  queries.memql  mutations.memql  tools.memql  ...
    prompts/*.tmpl
```

- **Adds new domains.** A directory colliding with a core embedded domain (e.g.
  `cognition`) is skipped -- the embedded tree owns that namespace.
- **Fail-loud.** The mounted tree loads through the same strict-boot gate as the
  embedded tree: a malformed construct refuses boot (`MEMQL_DSL_ALLOW_SKIPS` is
  the operator break-glass).
- **Directories beginning with `_` / `.` are skipped** (soft-disable / hidden).

Delivery: a product ships its DSL as a tiny data-only bundle image; an
init-container copies the `.memql` tree into a shared volume the node reads at
`MEMQL_DSL_PATH` (`deploy/k8s/base`). Also handy for dev hacking and fixtures.

## DSL dependency tree

How the DSL constructs lean on each other. Each layer can only depend
*downward*; cycles are rejected at load time. Each construct has its own section
below.

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

Two rules the diagram does not show: a `trait` is the one deliberately-unbound
row predicate, and **policies are provider selection only** -- caller-based
authz / feature-gating decisions are **specs**.

**Construct files live under `dsl/<namespace>/<construct>s.memql`** -- one
consolidated file per construct kind per namespace; policies are consolidated in
`dsl/policies/policies.memql`.

## Argument resolution

All DSL constructs share one model for declaring inputs and one namespace pair
for reading them. `ctx` is gone from the author surface entirely.

**How args get declared:**

| Construct kind | Where args go |
|---|---|
| Query / mutation / logic / automation | `args { ... }` sub-block inside the body |
| Builtin / tool / prompt | Body fields directly — the body IS the schema |

`args { ... }` field syntax:
`<name> <type> [@required] [@enum("a", "b", ...)] [@maxLength(N)] [@pattern("re")] [@minimum(N)] [@maximum(N)]`.
Omitting `@required` makes the field optional. Describe the field with a `///`
doc comment on the line above it -- `@description` and `@default` are both
rejected at load (memql#3336, #991).

**How args get read inside the body:**

| Name pattern | Source | Available in |
|---|---|---|
| `args.X` | Caller-passed arg declared in `args { ... }` | every body |
| `actor.X` | Resolved auth context (`userId`, `role`, `identityId`, `isClusterOwner`, `primaryEmail`, `now`) | every body |
| `now` | RFC3339 timestamp captured at eval start | every body |
| `partition` | Active partition for this call | every body |
| `config.X` | Allow-listed config (`component/config/policy_exposable.go`) | every body |
| `X`, `id`, `concept`, `type`, `createdAt`, `createdBy`, `schema` | Row fields / intrinsics | queries' `filter` + `shape` only (SQL pushdown) |

For automations, `args` is the automation's own declared `args { ... }` block:
at fire time the trigger payload is bound INTO that contract and validated
against it (`@required` / type / `@enum` / `@pattern`), and a violation refuses
the run rather than binding a partial map
(`component/automations/args_binding.go`, memql#2352). The triggering **event**
rides its own `event` envelope (`event.topic` / `event.kind` /
`event.payload.<field>`), which a step conventionally forwards to logic as
`logic name ( event )`; the logic declares `event` in its args block and reads
`args.event.payload.<field>`.

**Declared and used, in both directions.** An `args` field declared but never
referenced is refused at load, and (memql#3626) so is an `args.X` a body READS
but never declares -- covering both an author typo (`args.userld`, silently
absent from the write) and a caller-supplied undeclared name that would be bound
and written having passed no declared-schema check at all.

**Reserved engine names.** `now`, `actor`, `partition`, `config`, `trace` are
reserved as top-level identifiers; an `args` field colliding with one is
rejected at load. The **call site** refuses the same names in argument position
(memql#3626) -- since no args block may declare one, `mutation m(now: 1)` could
never bind and was silently dropped. A repeated argument name (`m(a: 1, a: 2)`)
is refused for the same reason a repeated annotation argument is: the map
collapses last-wins, so the value a reader sees is not the value the engine uses.

**Retired author-side forms (all rejected at parse time).** Every construct is
authored in the struct form. These shapes are gone and the parser refuses them
with a migration hint -- do not write them, and do not "restore" them when you
see one in an old diff:

- receiver-function wrapping (`func (Query) NAME(ctx any)` and its siblings).
  `func (Receiver)` survives only as the internal rewriter target the engine's
  parser consumes; authors never write it.
- The `@use*` annotation family (`@useConcept`, `@useShape`, `@useQuery`, ...)
  — replaced by file-top `use` imports.
- `@concepts(...)` / `@shape("name")` bindings — replaced by the
  two-identifier construct signature.
- `@input { ... }` — the prompt body IS the field list.
- `include` in a shape body.
- `;`-AND / `,`-OR filter separators, `has`, and the `?.` optional-chain prefix.

Only `dsl/_reference/*.memql` still shows these, deliberately, as
don't-do-this skeletons.

## Policies

The live `policy` construct is an **AI provider-selection record**:
empty-bodied, annotated with `@primary` / `@fallback` / `@maxLatencyMs` /
`@preferredRole`, consolidated in `dsl/policies/policies.memql` and consumed by
the AI Router to pick chat/voice/embedding providers.

```memql
@primary("streamClaudeSonnet")
@fallback("stream54Pro")
@description("Default chat policy for non-operator agents.")
policy balancedChat { }
```

**There is no decision-policy tier.** Auth / feature-gating / vendor decisions
live in Go (`component/safety` ships the risk×scope decision matrix) and in
**specs** -- use a bare spec conjunct for caller-based boolean checks (admin /
owner / permission). `engine.EvaluatePolicy` and `func (Policy)` do not exist.

## Key Concepts

### Authorization model

Per-row authorization is the only gate (see
[per-row-authz-audit.md](docs/public/operate/auth/per-row-authz-audit.md)).
Every query and mutation in the DSL classifies as **owned** (filter on
`ownerUserId == actor.userId`), **granted** (relationship predicate gates on
actor.userId), **admin** (cluster-owner spec), or **public** (`@public`). The
classification test in `test/dslconformance/conformance_test.go` hard-fails on
any new unclassified construct.

**Row admission also gates SUBSCRIPTIONS** (memql#4309). A `graph.node.*` event
reaches a subscribed stream only if the same function that admits the row on a
read admits it for that stream's actor -- so a concept's declared tier decides
what its live feed delivers, and a concept that declares nothing admits everyone
on BOTH paths (the standing undeclared long tail, not a subscription defect). A
`granted` row cannot be decided against one row, so it arrives id-only with
`payload_omitted` for the client to re-read through the authorized path.
Non-graph subscription kinds (`TELEMETRY` / `MESSAGE` / `AI_STREAM` / `ALL`)
carry node-level events with no row owner to decide by and are owner/admin-only
at subscribe time (memql#4311).

A fifth declaration FORM composes two buckets:
`@rowAuthz(owner="<field>", clusterOwner)` -- the owner, or a cluster owner
(memql#4312). It is the owned tier with the admin gate ORed in, not a new tier,
and it exists because a plain `owner=` tier has no cluster-owner bypass -- so
declaring an operator surface plain-owned hides every other user's rows from the
operator too. The write guard ignores the second argument.

**RANK (epic memql#4832) adds three more ARGUMENTS of the owned tier, not more
tiers** -- flags for the same reason `clusterOwner` is one: four sites switch on
the owned tier and a new tier value falls silently out of all four.

- `rankVisible` -- reads widen to "the owner, OR anyone at or above the OWNER'S
  rank" (D2). Peers included at every rung; the comparison is `<=`.
- `rankStrict` -- writes widen to "your own row, or one owned by someone
  STRICTLY below you" (D3), **and narrow: it WITHDRAWS the cluster-owner write
  escape**, so peer rows are read-only owner-to-owner included. Requires
  `rankVisible`.
- `unowned="<role>"` -- a PRESENT but EMPTY owner is the deployment's row, which
  has no rank to compare; this names the actor rank from which it is readable.
  An ABSENT owner key stays denied.

The owner's rank is resolved **per request** from the principal table and never
stamped on the row: promoting somebody retroactively changes who may see rows
they already own. The SQL half pushes down as an `in` list, because a
post-filter-only rank term makes a page of peer-owned rows read as EXHAUSTION to
the cursor. **An unresolvable rank floor DENIES** -- the natural spelling
(`actorRank >= rankOf(slug)`) reads correctly and fails open, since every rank
clears 0.

D4: `MaintenanceActor`, the seed materializer, an automation's system actor and
borrowed authority carry `AccessContext.Unranked` and the rank rules do not
govern them -- without it every retention sweep and boot seed becomes a
peer-write and stops.

**`@requiresRank("<role>")` is the SURFACE half** (D6): an actor-rank FLOOR on a
query / mutation / logic, enforced at execution and validated at LOAD. It gates
WHO MAY CALL; `@rowAuthz` still decides WHICH ROWS come back. It is the enforced
counterpart to MemQL OS's per-surface `roles: { min }`, which stays and is now a
mirror -- both permanent, neither a stand-in for the other. Declared on the
CONSTRUCT because a surface is a set of constructs and an app id from a browser
is a claim, not a fact.

**One role ladder, and the shell holds none of it** (D1). `v1:rbac:role` carries
`rank` plus `aliases` (the user row's `writer`/`reader` are aliases of the
catalog's `user`/`viewer`), the OS reads `activeRoles`, and
`component/auth/role_ladder_client_parity_test.go` fails the build if a client
ships an ordering of its own or the three readings disagree. **developer (300)
outranks admin (200)** -- every `roles: { min: "admin" }` written under the old
OS ordering changed meaning when this landed, and each was re-read against what
it was trying to say.

The partition dimension that historically gated tenant isolation is retired in
#56 (phases 1-7 landed; phase 8 sweeps the remaining cross-repo stragglers + the
DSL `partition="*"` automation kwarg). The `partition` wire field is already
removed (`reserved "partition"` in `component/grpc/memql.proto`); nothing
derives scope from the envelope.

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
  boot** -- the error names the `as=` form as the way out.
- **`as`** — what the edge MEANS to the domain (`assignedTo`, `repliesTo`).
  **Open**: any lowerCamelCase identifier, validated for FORM only and never
  against a list, so a new domain verb never needs an engine release. Optional.

Writing a domain verb in the `type` slot is the natural mistake and the reason
the split exists: `dependsOn` and `formedFrom` each cost an engine release as
structural types before being retired to labels (memql#3655). **Never add a
membership check to `as`** -- that rebuilds the treadmill, and a test guards it.

`field` may be a dotted path when the pointer sits inside a nested object block
(`field="lineage.originatingPlanId"`); the engine walks it on both the write and
filter side (memql#3672). The field must be declared on the concept -- on the
TARGET concept for `direction="incoming"`, since the FK lives on the far side.

Full authoring reference:
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

Event-driven workflows. The `@trigger` annotation keys off an event name plus
the target concept, using keyword args:

```memql
@trigger(event="node.created", concept="v1:cognition:participant", partition="*")
automation autoJoinSI { ... }
```

A time-driven automation uses the `schedule` kwarg instead:
`@trigger(schedule="0 */10 * * * *")`.

> **#56 phase 8 caveat:** the `partition="*"` kwarg is still required while the
> event topic carries a partition segment. That segment goes away in phase 8,
> after which the kwarg drops.

### Functions

Reusable query and mutation functions, in the struct form -- the only
author-facing shape (see "Retired author-side forms" above).

**Concept binding lives in the construct signature.** The two-identifier
signature `query <Concept> <name>`, `mutate <Concept> <name>`,
`seed <Concept> <name>` and `shape <Concept> <name>` names the bound concept
directly; the loader resolves the name through the file's file-top imports.

**Cross-file dependencies go through file-top `use` imports.** Every construct
another file pulls into local scope (shapes, traits, specs, mutations, queries,
logic, builtins, prompts, providers, tools) is declared via a dotted path:

```memql
use cognition.concepts.{ participant, space }
use cognition.shapes.{ participantFull }
use common.traits.{ isActiveRecord, isNotDeleted }
```

The dotted path maps to a file on disk (`cognition.concepts` →
`dsl/cognition/concepts.memql`); the brace-list names the constructs imported.
The bound concept's payload is referenced from filter clauses by the **bare
property name**, and from mutation bodies via the bare `insert { ... }` /
`update { ... }` block without re-stating the concept id.

> **Where the rules below are enforced (memql#3629, memql#4051).** The CONTRACT
> gates -- retired operator forms, the two `row.` namespace rules, the per-row
> authz user-scope bucket, the admin-gate composition rule, the cross-namespace
> import rule -- run inside `MemQLEngine.Init`, land on the `LoadReport`, and
> are refused by strict boot (`MEMQL_DSL_ALLOW_SKIPS` is the break-glass). That
> is what covers a **product DSL bundle** mounted at `MEMQL_DSL_PATH`, which no
> Go test in this repo walks; `cmd/memqllint` runs the same `Init` offline.
> House-style gates (naming, redundant annotations, canonical short forms) stay
> test-only. **Write a new gate against `dslgate.ScanFiles`, which takes the
> whole corpus** -- there is no longer a tier of gate that boot cannot reach,
> and reintroducing one is the mistake to avoid.

**Canonical filter-clause syntax** (enforced at LOAD time by
`component/memql/dslgate`, and over this repo's corpus by
`test/dslconformance/conformance_test.go`, which runs the same detector):

- Payload fields: **bare property** (`status`, `ownerUserId`) — never
  `<conceptName>.<field>`.
- Row intrinsics: the **`row.` namespace** — `row.id`, `row.concept`, `row.type`,
  `row.createdAt`, `row.createdBy`, `row.provenance.<leaf>`. A filter mixes two
  field surfaces under one syntax, so a bare `id` is indistinguishable from a
  payload property while compiling to entirely different SQL (a table column vs
  a JSONB path). Enforced by `TestFilterIntrinsicsUseRowNamespace`. A spec/trait
  body reads its signature-bound fields bare and REJECTS `row.*`; mutation
  `insert`/`update` blocks write `id:` as a target key, not a reference.
- Sort keys take the same namespace — `sort "row.createdAt", "desc"`
  (`TestSortKeysUseRowNamespace`). Payload sort keys stay bare; `provenance` has
  no sort form; the runtime/SDK sort surface accepts either spelling.
- **One Go boolean grammar:** `&&`, `||`, parens `( )` with Go precedence. `!`
  lexes and parses but is refused by every ASTConverter surface (memql#3630);
  its working homes are the two surfaces served by the runtime STRING evaluator
  (`component/automations.Evaluator`) -- an automation cond-step condition and a
  trigger `@filter`. Everywhere else write the `!=` comparison form.
- Membership is the single `in` operator: `args.x in list` or
  `kind in ["a", "b"]` (payload props bare).
- Prefix selection is `<field> startsWith <prefix>` (memql#4208): a string
  literal, a list of them (starts with ANY of), or an `args.<field>` resolving
  to either. Parameterized `^@ ANY(text[])` in SQL; an EMPTY list and a BLANK
  prefix match nothing -- a selection, never a pass-through. Filters and spec
  bodies only; the automations condition grammar refuses it by name.
- Arg-conditional predicates use the `when(args.x) { <expr> }` guard: if
  `args.x` is absent the guarded block AND its connective are dropped as if
  never written (unambiguous under `||`).
- When a trait spec covers the predicate (e.g. `isActiveRecord` for
  `active==true`), the trait is mandatory; inline `active==true` /
  `deleted==false` are rejected by the conformance test.

**Annotations** in the args block:
- `@required`; `@enum("a", "b", "c")`; `@maxLength(N)`; `@pattern("re")`.
- `@minimum(N)` / `@maximum(N)` — INCLUSIVE numeric bounds (memql#4522). A
  discrete numeric SET has no annotation: `@enum` takes string literals only,
  and opening it to numbers would compare a parsed member against the float64 a
  JSON caller sends under `reflect.DeepEqual` and refuse every value --
  fail-closed and silent about why. Express a small numeric set as bounds plus a
  UI that offers only the members.
- `@description` is **not** valid on an args field (rejected at load) -- an arg
  description is the `///` doc comment on the line above it. A `tool` / `prompt`
  / `builtin` field DOES keep its `@description`; those bodies ARE the schema.
- `@default` is **not** valid on an args field (rejected at load). Apply a
  default in the body with `args.X ?? <default>`. A concept-field `@default` is
  NOT a substitute -- it is never applied on insert either, so `??` is the only
  mechanism that fills a value. `a ?? b ?? c` folds to what `coalesce(a, b, c)`
  produces, and `test/dslconformance/no_coalesce_longhand_test.go` gates the
  corpus on the shorthand (`memqlmigrate --rewrite=null-coalesce` converts).
  **`??` is BLANK-coalescing:** it falls through on an empty OR
  WHITESPACE-ONLY string as well as on absent/null, so a caller who deliberately
  clears a text field gets the default written back; `false` / `0` / `[]` / `{}`
  are kept. Specified in
  [authoring-rules.md §28](docs/public/language/authoring-rules.md); `@noUnset`
  is the targeted opt-out for a field a blank must not overwrite.

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

`update { id: ..., ... }` is the partial-update counterpart for mutations that
read-merge-validate-write an existing row. Exactly one `insert` OR `update`
block per mutation.

### Logic

Imperative procedure called from an automation step. `args { ... }` declares
inputs; `body { ... }` is a sequence of named statements ending in
`return <expr>`. The single-statement form is the common case:

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

Multi-statement bodies (intermediate `name := <call>` steps with side effects,
followed by a trailing `return <expr>`) execute via the `LogicRunner`: the
runner walks intermediate steps in dependency order through the same step
registry the automation scheduler uses, then evaluates the trailing `return`.
Logic functions don't write `ctx.output = ...`.

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
lifecycle flags as functions / builtins / prompts / specs / seeds. `@enabled` is
the explicit-on default (a no-op). `@disabled` skips the provider at load --
**not registered, no auth resolution attempted** -- so it emits zero "registered
as unavailable" warnings while staying in the tree for a future re-enable.
`@disabled` on a `@base` **propagates** to every child that `@extends` it.
Dependents degrade gracefully: a policy whose `@primary` is disabled routes via
its `@fallback`; a prompt whose `@defaultProvider` is disabled falls back to the
default.

> **Semantics of `@disabled` (shared across every construct that takes it).**
> It means the construct is **not loaded/active at runtime right now**. It does
> NOT mean deprecated, abandoned, or exempt from updates / refactors /
> conformance -- it is a reversible on/off switch, and disabled constructs are
> still maintained. ("Deprecated / abandoned" is the separate `@deprecated`
> axis.) Canonical statement: `component/language/ast/ast.go` at the
> `AttrEnabled` / `AttrDisabled` consts.

### Shapes

Reusable data projections. Each shape declares its **kind** (where its fields
come from) via `@row` and/or `@actor`. At least one is required; both is
allowed. Each path becomes a template entry keyed by the path's terminal
segment.

**Row shapes** project a concept's payload + row intrinsics; the bound concept
is named by the signature `shape <Concept> <name>`:

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

**Actor shapes** project the engine envelope (the authenticated actor + engine
timestamp) and carry no signature concept. Closed field set, enforced at load:
`actor.userId` / `actor.role` / `actor.identityId` / `actor.isClusterOwner` /
`actor.primaryEmail` / `actor.now`. Bare `config.<key>` is the config read;
shapes do not project it.

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

**Mixed shapes** carry both `@row` and `@actor` -- useful for predicates that
compare row fields against actor context (`row.createdBy == actor.userId`).

**No composition.** `include` is NOT a shape verb and is REJECTED at load. To
share a projection, repeat the paths -- or drop the body entirely and take the
default projection over the bound concept.

**Every body path is checked at load**: a bare payload property must be a
declared field of the bound concept, the bound concept must resolve (an
ambiguous bare name disambiguates through the shape's own domain), two paths may
not collapse onto the same terminal key, and the declared kind must match the
body. No `func`, no `@template`, no `node("…")` wrapping; shapes have no inputs
and no return.

### Specs

Atomic boolean predicates -- **signature-bound** (epic #2281). A spec binds
exactly one shape XOR concept in its signature (`spec <boundName> <name>`,
resolved via the file-top `use` import) and the body `return`s a boolean over
**bare** field names. The binding picks the evaluation strategy:

- **Row-specs** bind a concept or a `@row` shape. They compile into a SQL
  `WHERE` fragment and push down to the database.
- **Context-specs** bind an `@actor` shape (the only gateway to the auth
  envelope). They evaluate in-process; named as a bare conjunct for actor-based
  checks like "is admin".

A spec body never reads `actor.*` / `row.*` directly (bind a shape that projects
it and read the projected key bare). A `trait` is the one deliberately-unbound
row predicate (bare payload fields, validated at the call site).

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
  return role == "admin"             // @actor-bound context-spec
}

@enabled
@description("Matches records with active==true field")
trait isActiveRecord {
  return active == true              // unbound cross-concept trait
}
```

A spec/trait body is a single `return <boolean expression>` over bare field
names; there is no `ctx` envelope and no parameter, and a bare-expression body
with no `return` is rejected at parse time.

**Caller-context checks use specs, not policies.** Author the predicate as a
context-spec in `dsl/<namespace>/specs.memql` and name it as a bare conjunct.

### Tools

AI-callable tool definitions. The body is a list of input-schema fields with
types and annotations (`@required`, `@default`, `@enum`, `@description`).

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

**A tool declaration is CHECKED at load.** Four gates, all fail-loud:

- **`@handler` argument names are closed** (`type`, `name`, `query`, `url`,
  `method`) and `type` is required. `@rateLimit` is closed the same way, and a
  non-integer value is refused.
- **The handler is validated at load** -- unknown type, missing function name /
  query / URL -- and a tool must carry a handler at all unless it is
  `@clientExecution` (whose body lives in the browser).
- **The handler's TARGET is resolved** against the function + builtin registry
  at boot (`tool_handler_resolution.go`), so a handler naming a function that
  does not exist is a load problem strict boot refuses, not a mid-turn failure.
  A builtin is reached through `@handler(type="function", name="<builtin>")`;
  there is no `"builtin"` handler type.
- **Field types and field annotations are closed sets.** An unknown type is
  refused rather than emitted as `"string"` (which would tell the model
  "string", coerce the `@default` to a string, and hand a `@required integer`
  handler argument `"10"`).

### Integration Capabilities

Go-backed operations callable from the DSL via `@executor("integration.X.Y")`.
The body's field list is the builtin's input schema; the implementation is the
Go integration named by `@executor`.

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
dependencies sit outside the stable `PluginContext` surface. `training` is a
product-repo pack, not part of engine-only core.

`shopify` is a CONNECTOR rather than an ordinary integration, and the reference
implementation of `component/memql/sync.Connector` (epic memql#4389): it owns
one external system's data, its model is GENERATED from that system's schema at
a pinned version (`cmd/shopifyschema` -> `dsl/shopify/generated/`, 65 concepts),
and its five verbs return MirrorWrites for the runtime to apply rather than
writing themselves. Read [integrations/CLAUDE.md](integrations/CLAUDE.md) before
writing a second one; operator runbooks are
[shopify-connector.md](docs/public/operate/shopify-connector.md) and
[shopify-storefront-checklist.md](docs/public/operate/shopify-storefront-checklist.md).

### Extension Points

Three ways to extend MemQL, in preference order:

1. **DSL files** (`.memql`) -- queries, mutations, specs, automations, prompts,
   providers, shapes, tools, builtins. Always the first choice.
2. **Self-registering plug-ins** -- Go integrations that call
   `memql.RegisterPlugin(name, factory)` from `init()`. The factory receives a
   narrow `PluginContext` (Logger, Engine, BunDB getter, VisionProvider,
   EmbeddingProviderByName, partition/variable resolvers). Build tags on the
   calling file control which binaries include the registration. Use this path
   to add product-specific Go without touching `app/` internals. See
   `component/memql/plugins.go`.
3. **Explicit `app/` wiring** -- reserved for first-party integrations whose
   dependencies don't fit `PluginContext` (cognition, agent, stt). Lives in
   `app/integrations_*.go` with build tags.

Event routing is also plug-in-registerable: `node.RegisterRoutingRule(...)`
declares forwarding patterns from `init()`, and build tags on the caller decide
which binaries include the registration. Forwarding is default-deny -- block
rules evaluate first, then forward rules, and an event matching neither stays
local.

**There is no concept-ownership registry.** Which node does a concept's work is
decided by routing rules plus which binary's build tags compile the subscriber.

### MemQL Sense (Language Intelligence)

Language service for .memql files, exposed via gRPC on `MemqlService.Stream`:
**Tokenize** (semantic tokens), **Complete** (context-aware autocompletion),
**Diagnose** (lexer / parser / semantic errors), **Hover** (symbol info) and
**SignatureHelp**. Package: `component/memql/sense/` -- pure Go, no gRPC
dependency; handlers in `component/grpc/sense_handlers.go`.

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
else's. So a mutation bound to a mirror must be `@serverOnly`, or it is
generated into both SDKs as a method that can only fail (gated by
`TestMirrorConceptsHaveNoClientReachableMutation`).

**Connectors are named actors, not a bypass.** `auth.ConnectorActor(name)` is
admitted by row admission to the concepts whose `@origin` or `@mirroredTo` names
it, **regardless of tier, and to nothing else**. No request can mint one:
`RoleConnector` is outside `ValidRoles()` and outside the rank model. A
connector is an *integration* implementing the contract in
`component/memql/sync`; it is not a fourth extension word.

**Registration has two halves.** `sync.Declare(name)` runs from an `init()` and
says *this build serves a connector by this name*; `sync.Bind(c)` attaches the
implementation later. The boot check reads the first, because `MemQLEngine.Init`
runs BEFORE integrations are wired. An unresolvable name **refuses boot**: a
mirror nobody fills reads as an empty catalog and a mirror target nobody drains
accumulates outbox entries forever -- both silent.

Full doc: [data-origins.md](docs/public/concepts/data-origins.md).

### Infrastructure concepts

Field inventories live in the `.memql` files; what follows is only what the
schema does not say.

- **Platform** (`dsl/platform/concepts.memql`) -- `site`, a hosted web surface
  and now a "deployable" (memql#4344): the edge resolves the request `Host` to
  one of these rows and serves its `bundleRef`. A user's hostname is
  `<slug>.<domain>` against a reserved set DERIVED from `frontdoor.Roles()` +
  the portal, so a new role can never become claimable by omission; Android /
  iOS / macOS deliberately have NO enum values, being distribution rather than
  hostnames ([deployables.md](docs/public/operate/deployables.md)). Also
  `globalSecret` / `globalVariable`, `outboundRequest` / `inboundRequest`,
  `missingCapability`, and `dataOrigin` -- a VIRTUAL projection (the
  `v1:router:modelCatalog` pattern, never persisted) of every concept's
  data-origins declaration.
- **Library** (`dsl/library/concepts.memql`) -- `artifact` is a thin INDEX row
  owning no content (memql#693); `file` is the only backing row with bytes
  (memql#4340) and `fileChunk` holds its embeddings. All three declare
  `@rowAuthz(owner="ownerUserId", clusterOwner)`. The index's `source` enum is
  the UNION of every backing concept's own, because promotions pass the backing
  value straight through ([library.md](docs/public/operate/library.md)). An
  archive is a soft delete and a RESTORE is its plain inverse (memql#4784) --
  but the restore is a CLIENT-DRIVEN PAIR, because `archiveFileOnArtifactArchive`
  runs artifact->file and a mirror of it would fire on every artifact update
  and close a cycle. `file.linkState` (epic memql#4783) records how a copy
  stands against the machine it was pushed from: the engine stamps `synced` on
  any upload naming a `(uploadedFromWorkerId, uploadedFromPath)` -- which is
  also the key a re-push versions on, on BOTH upload routes -- and only ever
  FLAGS, so a deletion at the origin never touches the copy. `watchedFolder`
  (memql#4841) is the ARRANGEMENT those states come from -- one folder on one
  of the owner's machines, kept arriving in a Library folder, set up at Files
  -> Backups and swept by that machine's cockpit. The arrangement lives in the
  graph so it survives the machine being asleep; the PATH is still the
  machine's to refuse (`policy.yaml` `backup.roots`, default-deny), and it says
  so through `originState=refused_by_policy` -- a value of its own rather than
  a flavour of `unreadable`, because the repair lives on that machine and
  nowhere else. `lastSweepAt` is server-stamped for `linkCheckedAt`'s reason,
  and an ABSENT `originState` is "no cockpit has reported yet", never `ok`.
- **Cluster** (`dsl/cluster/concepts.memql`) -- `node`, `nodeType`,
  `spawnEvent`, `cluster` / `database` / `identityProvider`, plus the deploy
  pair: `deployment` (append-only, one timeline per deploymentId; the
  deploy-as-a-pack source of truth, #1872) and `deploymentNodeSpec`
  (per-node-type child: version + replicas + imageDigest). **Engine-as-spine:**
  an empty `version` resolves against the deployment's engine version, a
  non-empty one pins the node type. Read the set via `nodeSpecsForDeployment`.
  **`database` and `identityProvider` are singletons at LITERAL ids**
  (`v1:cluster:database:primary`, `v1:cluster:identityProvider:primary`,
  `v1:cluster:cluster:self`) and that is load-bearing three times over
  (memql#4766): it is the only way the cross-links could be written at all (a
  mutation step cannot hand its inserted id to a later one), it makes a
  re-write a new VERSION of one logical row rather than a duplicate, and it is
  therefore why **they refresh on every bff start** instead of being written
  once. Their gate is `clusterInfraRefresh` ("a bff started"), NOT
  `bootstrapCluster` ("the cluster does not exist yet") -- sharing one gate is
  what left half their fields permanently empty. **No `status` field survives
  on either**: `database.status` is structurally unanswerable (the row lives in
  the database it describes, so a successful read can only say `healthy`) and
  `identityProvider.status` / `lastVerifiedAt` had no writer honest at that
  granularity. Do not re-add either; probe live and say when you looked.
- **Observability** (`dsl/observability/`, loaded by every node) --
  `codeProfile` (live per-FQN verbosity override, fed to the observe runtime by
  `CodeProfileSubscriber`), `invocation` (the `code_invocation` hypertable) and
  `codeMetric` (the `code_invocation_1m` / `_1h` continuous aggregates). Clients
  read through `codeMetricsInWindow`: one bucket, one `[windowStart, windowEnd)`
  range, `codeReference startsWith` any of the caller's prefixes (memql#4208).
  [Design](docs/internal/design/auto-generated-diagrams.md).
- **Identity** (`dsl/identity/concepts.memql`, loaded by every node) -- `user`,
  `authSession`, `magiclink`, `accessRequest`, `invitation`, `delegation`,
  `enrolmentToken`, and `identity`, a credential set that is a discriminated
  union keyed on `identityType`; the `passkey` variant is the only one whose
  stored material is PUBLIC (a COSE key), because possession is proved by a
  signature rather than a digest match. Full model:
  [access-model.md](docs/public/operate/auth/access-model.md).
- **Two audit logs, not one** (memql#4328), because the Trail is a generic
  concept walk with no filter: `auditEvent` records DECISIONS and security
  signals, `authActivity` records routine MECHANICS (token rotations, blocked
  ones, grace-window accepts, PAT-authenticated requests) two orders of
  magnitude more numerous. `authActivity` was the first concept to declare
  `@rowAuthz(owner="<field>", clusterOwner)` -- a person reads their OWN
  activity, a cluster owner everyone's, a non-owner admin gets
  `authActivityForSelf` -- and its `retiredTokenHash` is the evidence
  refresh-token reuse detection keys on (memql#4329). Real retention applies:
  `MEMQL_IDENTITY_AUTH_ACTIVITY_RETENTION_DAYS` (default 30), hard-deleted daily
  from Go unlike `auditEvent`'s count-only sweep, so detection reaches back
  exactly that far.

## Feature Notes

### Canvas + Spaces

Under platform consolidation (#2472) the space lifecycle (three-state + daily
spaces) is an **engine-generic feature** rather than product code; the core
participant/session/utterance machinery is engine-side
(`dsl/cognition/mutations.memql`: joinSpaceAsHuman, leaveSpace, addAgentToSpace,
...).

The canvas timeline (the `canvasState` concept) is still delivered as **product
DSL** at runtime through the product's bundle (`MEMQL_DSL_PATH`); its physical
absorption into the engine is mid-migration, so treat canvas as product-owned
for now. Product rows ride the chat-reply delivery substrate via
`node.RegisterChatReplyConcept`.

### Views, layouts and living pages (epic memql#4661)

**The arrangement system IS the page system.** Every portal page that shows data
is a `PageManifest` -- a title, a line of copy, and sections of one concept each
-- rendered by `clients/portal/src/pages/ArrangedPage.tsx`. The predefined
views, the composed ones, the fleet pages, Artifacts, Deployables, the Me tabs
and the Nexus goal page all go through it; there is no branch for which kind a
page is, because there is no longer a difference.

- **Absent means stack, and absent means standard.** A section's `layout` and an
  entry's `role` are additive, and every `v1:portalviews:view` row stored before
  them names neither. Nothing writes the defaults -- if absent ever meant
  anything else, the release that changed it would silently re-lay-out every
  view every person has.
- **`sanitizeArrangement` is the ONE gate**, and it repairs the RENDERED value
  and never the stored row: unknown layout to stack; focus with no hero PROMOTED
  (the library's own ranking) rather than demoted; split with no detail pane to
  stack; unexpressible roles ignored but their element kept. A page manifest's
  `required` entries are re-inserted here, which stops a regeneration producing
  a valid arrangement of a page that no longer does its job.
- **AI runs on ONE explicit action and never at render.** A render path that
  reached a provider would cost money on every page view, change a page under
  somebody mid-read, and make the console unusable with no provider configured.
  Resolution is: the caller's override row -> else the seed -> never a model.
- **A regeneration is a per-user row** (`kind="override"` + `targetPageId` on
  `v1:portalviews:view`, gated by one filter conjunct and one `@serverSet`
  field), and **revert is an APPEND** -- the history grows and nothing is
  destroyed, which is why reverting twice is coherent. `composedViews` filters
  `kind!="override"` and NOT `kind=="composed"`, because `!=` is null-safe
  against a non-empty string (memql#1685) and every pre-epic row has no `kind`.
- **Two CLOSED registries**, and their laziness differs on purpose. A `scene`
  (`clients/portal/src/nexus/scene/registry.tsx`) is lazy because three.js is
  the portal's largest dependency and the registry is reachable from every
  arranged page; `nexusMap.test.tsx` enforces that only canvas modules import
  it. A `widget` (`clients/portal/src/widgets/`) is a form, so it is static:
  lazy where the dependency is heavy AND isolated, not lazy because a registry
  is a natural boundary.

**Element personality lives in view-kit** (`sdk/ts-viewkit/src/cell.ts`), so
every consumer improves at once: datetimes elapsed with the exact instant on
`title` AND `datetime`, enums as pills, booleans as a dot plus the FIELD's
label, numbers compact and tabular, ids and unresolved references in the data
voice. **Nothing renders blank** -- an absent value is an em dash and an
unresolvable lookup is the id. **Lookups are batched client-side, never joined**
(`sdk/ts/src/client/lookups.ts`, one read per (concept, id set) with
coalescing); row authz is untouched, so a target the caller may not read simply
does not come back.

Design record:
`docs/superpowers/specs/2026-08-26-views-layouts-personality-regeneration-design.md`.

### Nexus -- the portal's living map of a goal (memql#4369)

`clients/portal/src/nexus/` is the console's one 3D surface: a `v1:planner:plan`
and its world -- the planner, the specialists it raised, its semantic tasks by
phase, the artifacts it produced and the constructs it authored -- materializing
as the system works, then replayable from the rows' own timestamps. Three pages
under one goal (Map, Constructs, Replay).

- **`scene/` is pure and imports no three.js.** `layout(world)`, `events(world)`
  and `scene(world, at)` are functions over rows, tested on fixtures with no
  GPU, and shared by the Map and Replay. `events()` **invents nothing**: a
  moment with no timestamp produces no event, because a scrubber is read as
  evidence.
- **The feed resolves EVERY live event through the authorized read**, payload or
  `payload_omitted` alike, and drops it when the read refuses. One code path, so
  the branch that would trust a payload does not exist to be forgotten.
- **The scene is a lazy chunk.** Only `map/NexusCanvas.tsx` may import three.js /
  fiber / drei, and `nexusMap.test.tsx` fails the build if anything else does.
  The frame loop is `frameloop="demand"` and its governor evaluates the
  predicate PER FRAME -- a boolean captured at render would either spin forever
  or never wake.
- **One goal at a time, YOURS, and part of that is client-side today.**
  `v1:planner:plan` is undeclared (memql#4366), so `planById` answers for any
  id; Nexus refuses to draw a goal whose `requestedBy` is not the caller's own
  user id. That is a client-side filter, labelled as one everywhere it appears,
  and the residual is recorded in
  [per-row-authz-audit.md](docs/public/operate/auth/per-row-authz-audit.md).

Operator doc: [portal.md](docs/public/operate/portal.md). Design:
`docs/superpowers/specs/2026-08-22-nexus-living-map-of-a-goal-design.md`.

### Invitations (Identity Primitive)

Token-hashed invitation credential for user and guest flows, under
`v1:identity:invitation`. Two gRPC messages drive the guest flow:
`SendGuestInviteMsg` (authenticated space owner -- mints a 32-byte token, stores
only its SHA-256 hash, sends the email) and `ResolveGuestInviteMsg`
(unauthenticated public call from the product `/join/<token>` page, returning
scope + inviter metadata or a typed status).

Guest authentication is `Authorization: Guest <token>`.
`NewGuestAwareStreamInterceptor` wraps the identity-verifier interceptor,
validates the token against the invitation registry, and builds a guest
`AccessContext` under the `identity.guest` claim key (subject
`guest:<invitationId>`). The WS bridge accepts it as `?guest_token=<token>`
since browsers cannot set custom headers on the upgrade.

Key files: `dsl/identity/{concepts,queries,shapes}.memql`,
`component/grpc/guest_handlers.go` + `guest_stream_interceptor.go`, and
`integrations/email/` (self-registering plug-in exposing
`integration.email.sendEmail` -- GraphSender via Microsoft Graph `sendMail`
preferred, SMTPSender fallback, LogSender for dev; env `AZURE_TENANT_ID` /
`AZURE_CLIENT_ID` / `AZURE_CLIENT_SECRET` / `MAIL_SENDER` / `MAIL_FROM_NAME`, or
the `SMTP_*` family, or neither).

**"or neither" is a LOCAL-ONLY option (memql#4477).** `LogSender` returned `nil`,
so mail did not fail silently -- it failed UPWARD: the wizard said the link was
sent and the audit row recorded success. So log-only is a choice a local install
may make and an error everywhere else, decided from `MEMQL_DOMAIN`: unset, a
loopback literal, `*.localhost` or `*.local.<domain>` keeps it; anything else
REFUSES BOOT, naming the four Graph vars and the SMTP pair. Break-glass:
`MEMQL_EMAIL_ALLOW_LOG_ONLY=true`. Four layers changed together -- boot (the
plug-in factory errors and `materializePlugins` fatals), send (a `LogSender`
built anywhere else returns a permanent `SendError`), the audit row
(`outcome=failure`, while the ROW and the HTTP response stay identical so a
response cannot enumerate registered addresses), and the portal (`unhealthy`).

**`Mail.Send` (Application) is tenant-wide until it is scoped** (memql#4478) --
its Entra display name is literally "Send mail as any user". Narrowing it to the
one sender mailbox needs an Exchange `ApplicationAccessPolicy`, which is
Exchange Online PowerShell and NOT reachable from `az`. Any automation that adds
a secret must pass `az ad app credential reset --append`: without it the command
DELETES every existing secret on the registration.
[azure-entry-install.md](docs/public/operate/azure-entry-install.md#mailsend-is-tenant-wide-until-you-scope-it).

**The guest write path is ENGINE DSL, split across two domains** (memql#4258):
`createGuestInvitation`, `markGuestInvitationAccepted`,
`markGuestInvitationKicked` (also the CANCEL path -- a soft cancel, so the
tokenHash stays taken) and `rotateGuestInvitationToken` live in
`dsl/identity/mutations.memql`; `createGuestParticipant` lives in
`dsl/cognition/mutations.memql`, because the SPACE is cognition's. All five are
`@serverOnly`: each writes a `tokenHash`, the whole credential the guest-auth
interceptor matches on, so a client-reachable create is a credential-forging
primitive. The three update-shaped ones take MINIMAL arguments -- `update{}` has
been a read-merge since memql#1628, so re-supplying every discriminator is dead
weight that an undeclared-argument DISCARD hides.
`component/grpc/render_query_args_parse_test.go` gates both directions.

### Email campaigns + the sending engine

Campaigns are ordinary graph state (memql#3323) plus a Go sending engine
(memql#3348). **Thirteen** concepts under `dsl/campaigns/`: nine
operator-facing on the COMPOSITE tier `@rowAuthz(owner="ownerUserId",
clusterOwner)` (`audience`, `recipient`, `template`, `senderIdentity`,
`campaign`, `delivery`, `consentEvent`, `engagementEvent`, `emailRule`), four
engine-owned and clusterOwner-tier (`sendJob`, `suppression`,
`reputationWindow`, `warmupState`). Runbook:
[campaign-sending.md](docs/public/operate/campaign-sending.md).

**The composite tier is oversight, not sharing.** A cluster owner READS every
operator's campaigns and drives the builtins on them (their gate is "can the
caller read the campaign row") and CANNOT rewrite the rows -- the write guard
ignores the second argument (memql#4312). A plain owner tier has no
cluster-owner escape on the READ path, so operator oversight would not merely
be unimplemented, it would not be EXPRESSIBLE, and every fleet-wide campaigns
view would silently render one person's subset.

**The account tie is a record, never a visibility scope** (accounts D1).
`accountId` + a `forAccount` relationship on `campaign` / `audience` /
`template` / `senderIdentity` / `emailRule`; recipients inherit through their
audience; `campaignsForAccount` is the rollup. **No query narrows a read
because of it.** Requiring one at create is APP behaviour, not schema.

**A `senderIdentity` is a mailbox declaration with NO secret material.**
Authentication stays the cluster's one Graph credential. Resolution is
`campaign.senderIdentityId` -> else the env default, and **the engine never
infers an identity from `accountId`** -- prefill is UX, resolution is
explicit. A missing or `disabled` identity is REFUSED, never defaulted: a
silent fallback mails a client's list from the wrong mailbox under the wrong
SPF/DKIM and the send looks successful.

**The two identities is the design.** A send touches rows belonging to somebody
else, and the engine BORROWS the owner's authority rather than out-ranking it:
`component/campaigns`' drain worker runs its clusterOwner-tier reads (the job
queue, the suppression list) under the engine's own operator identity, and
everything owned under
`auth.ContextWithUserActor(ctx, job.campaignOwnerUserId)`. That owner value is
copied off a campaign row the STARTING CALLER had already read under their own
actor, so it can never name a user the caller could not act as.

- *Suppression is CLUSTER-WIDE and digest-keyed* -- one deployment, one sending
  mailbox, one reputation. The row id is the SHA-256 of the normalized address
  and the only readable field is the domain. Enforced at the POINT OF SEND,
  before the recipient row's own status.
- *A hard bounce suppresses; it does NOT delete the membership.* Deleting
  destroys the audit trail and lets the next import resurrect a dead address.
- *Idempotency is the ledger.* One `v1:campaigns:delivery` row per (campaign,
  recipient) at a derived id; the batch is "roster minus ledger, plus retries
  that are due". The absence of the row IS the work queue.
- *Two rate limits.* Ours is a per-process token bucket
  (`MEMQL_CAMPAIGNS_SEND_RATE_PER_MINUTE`); theirs is the 429, surfaced as a
  typed `email.SendError` and honoured by parking the job until its
  `Retry-After`.

**RFC 8058 one-click** rides two headers, which forced the Graph sender onto its
base64-MIME form (Graph's structured payload only carries `x-` headers).

**The unsubscribe token names its key**
(`u2.<keyId>.<owner>.<recipient>.<campaign>.<tag>`, memql#3458), verified
against a ring of two: `MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET` signs,
`..._SECRET_PREVIOUS` only verifies. The key id is a truncated HMAC **of the
key**, not a slot -- a link minted today is clicked on a node where that secret
has since become the previous one. `_PREVIOUS` is a permanent second reader key,
NOT a migration window: unsubscribe links never expire, so **the window is
counted in rotations, not days** -- rotate at most once for any reason short of
key compromise.

**Open/click tracking rides the SAME key ring under a different context
string**, so an unsubscribe token can never verify as a tracking one or the
reverse. `GET /t/o/{token}` and `GET /t/c/{token}` are owner-approved HTTP
exceptions for the `/unsubscribe` reason (the mail client dictates the wire);
the destination URL is INSIDE the signed payload rather than in a query
parameter, which is what makes the redirect open-redirect-proof. The token
must be a SINGLE PATH SEGMENT -- the self-authenticated exemption is bounded
to one segment under the mount, so a token containing `/` is 401'd before the
handler runs. Hits land on `v1:campaigns:engagementEvent`, unique by DELIVERY
rather than by address.

**Two figures `campaignStats` refuses to invent.** A unique open/click count
folded from a bounded read that came back AT its bound reports `unmeasured`
rather than a floor dressed as a total; there is no per-campaign soft-bounce
figure at all, because nothing measures one. An absent figure and a zero are
different answers.

**An event-email rule is a FORM; a generated authored automation is the
MECHANISM** (memql#4829). `v1:campaigns:emailRule` is what the app lists and
edits; `campaignActivateEmailRule` renders a construct DETERMINISTICALLY from
it (the LLM `authoringEmit` path stays off) and arms it through the runtime
authoring pipeline. A shipped automation plus a lookup table cannot express
this: an automation's `@trigger` names ONE concept at load time. **Two lanes,
chosen by who receives** -- cluster roles ride `stageOutboundRequest`
(allowlist, no unsubscribe, suppression NEITHER consulted nor written);
audience and row-address recipients ride `campaignSendToRecipient`. **The
actor trap applies:** the generated construct runs under `AuthorContext`
(the author's userId, role writer, origin CLIENT), so owned rows are the
author's or invisible, no `@serverOnly` construct is reachable, and
cluster-wide questions must be asked from Go.

The scheduler (`campaignScheduleSend`), the evidence-driven warming ramp, and
bounce/complaint feedback ingestion are all built -- the runbook is current.
On Graph, nothing dials us: the mailbox reader
(`MEMQL_EMAIL_NDR_POLL_SECONDS`) stages DSNs from the sending mailbox, and
they are acted on only once `graph-mailbox=rfc3464` is in
`MEMQL_CAMPAIGNS_FEEDBACK_SOURCES`. The multi-account campaigns program is
specced in
[2026-09-01-email-campaigns-program-design.md](docs/superpowers/specs/2026-09-01-email-campaigns-program-design.md),
with a record per sub-project:
[the campaigns backend](docs/superpowers/specs/2026-09-01-campaigns-backend-design.md),
[integration config](docs/superpowers/specs/2026-09-01-integration-config-design.md),
[the Campaigns OS app](docs/superpowers/specs/2026-09-01-campaigns-os-app-design.md),
[event emails](docs/superpowers/specs/2026-09-01-event-emails-design.md).

### Planner / Knowledge / Validation

The schema is stable, so new features add fields/automations without migrations.
Fields are in the `.memql` files; the concepts are `v1:planner:plan` (a
user-visible unit of work, with sub-plan nesting, a nine-value status, phases,
estimate and token budget/spend bookkeeping), `v1:planner:task` (one executable
step, never recursive, carrying `executionSurface` inProcess |
containerExecutor + `executorBackend`), `v1:planner:taskState` (persisted
working state for async parking), `v1:agents:agentAuthorization` (standing
tiered-trust authorization), the `v1:knowledge:*` family (`document`,
`spreadsheetRow` / `imageRegion`, the append-only `validationEvent`, and
`domainEntitySchema` / `entityIndex` for cross-file dedup), plus
`v1:common:knowledgeDomain` and `v1:common:documentChunk`.

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
defense in depth and every layer is load-bearing: a process-wide LLM rate
ceiling and an identical-request circuit breaker at the provider HTTP chokepoint
(`component/memql/ai_guard.go`); a CUMULATIVE per-plan token/call budget checked
before every `plannerAgent` call and persisted so it survives cycles and
retries, parking the Plan rather than making another call
(`component/planner/budget.go`); complexity triage that routes a trivial
deliverable to ONE cheap path; model tiering that defaults cheap and escalates
only on an explicit stuck signal; an up-front estimate + approval gate; gated
specialist creation/training; phased execution with per-phase checkpoints;
deterministic-first verification; and a no-task-`markPlanSucceeded` convergence
guard. Read [llm-cost-control.md](docs/public/ai/llm-cost-control.md) before
touching any of it.

`produceArtifact` rides the unified loop rather than a bypass: triage shortcuts
to ONE direct production turn (`startPlanDirect`), with the rate ceiling, caps
and tiering as structural backstops. An earlier hardcoded bypass was reverted
precisely because those backstops did not yet exist -- do not reintroduce one.

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
([docs/internal/design/capability-script-contract.md](docs/internal/design/capability-script-contract.md),
#2221) -- the convention above **plus**:

- **non-interactive** -- no `read -p` / `select`; a destructive confirmation is
  an explicit `--confirm=<phrase>` param, never a blocking prompt;
- **structured params in** -- `--flag=value` > stdin JSON (`--params-stdin`) >
  documented defaults (no env tier); no positional args;
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
`deploycontrol.ParseCapabilityResult`.

### Documentation Style Guidelines

**No Emojis:** All documentation, skills, and CLI responses must use
professional formatting without emojis. Use checkboxes (`[ ]` / `[x]`), text
indicators ("SUCCESS:", "ERROR:", "WARNING:", "INFO:") and standard markdown for
emphasis. This applies to documentation files, skill outputs, CLI responses, and
all user-facing text.
