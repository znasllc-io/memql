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

```bash
# --- k3d + ArgoCD (the local run path) ---
# Prerequisites: docker, k3d, kubectl (brew install k3d kubectl)
make up          # fresh bring-up: cluster + ArgoCD + secrets + images, wait healthy
make dev         # inner-loop: rebuild images -> import -> restart pods
make status      # mesh litmus (unique MEMQL_NODE_ID per pod;
                 # one shared identity signing keyset)
make up-refresh  # clean slate: nuke + repave (fresh DB), then the same bring-up
make down        # tear down

# Multi-node mesh testing (2 replicas per Deployment -- cloud parity):
make up SERVERS=2 AGENTS=1
make scale N=2
make status   # verify unique MEMQL_NODE_IDs + shared identity keyset

# Run tests -- `make test`, NOT `go test ./...` (see Testing below)
make test

# Build binary (BFF is the default, no tag needed)
go build -o bin/memql .

# Build node-type binary (voice, cognition, agent, planner)
go build -tags voice -o bin/memql-voice .
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
├── app/               Phased service bootstrap (Go)
│   ├── app.go         Build() orchestrator + Overrides
│   ├── config.go      Phase 1: config + auth middleware
│   ├── database.go    Phase 2: database + concepts
│   ├── engine.go      Phase 3: engine + bus + automations
│   ├── integrations.go Phase 4: integration registration
│   ├── transport.go   Phase 5: gRPC + HTTP + WS endpoints
│   ├── cluster.go     Phase 6: distributed node bootstrap
│   └── adapters.go    Engine adapter types
├── dsl/               Consolidated MemQL DSL tree (every .memql file),
│   │                  flattened to per-namespace per-construct files
│   ├── <namespace>/   One directory per namespace (actions, agents,
│   │   │              authoring, calendar, campaigns, capabilities,
│   │   │              cluster, cognition, common, data, deployment,
│   │   │              forge, harness, healing, identity, install,
│   │   │              integrations, knowledge, library, memql, notes,
│   │   │              observability, planner, platform, policies,
│   │   │              portalviews, providers, rbac, router, safety, shopify,
│   │   │              telephony, todos, workbench, worker)
│   │   ├── concepts.memql     Concept definitions (schemas)
│   │   ├── mutations.memql    Mutation functions
│   │   ├── queries.memql      Query functions
│   │   ├── specs.memql        Specification predicates
│   │   ├── shapes.memql       Reusable shape templates
│   │   ├── builtins.memql     Go-backed executors
│   │   ├── tools.memql        AI tool definitions
│   │   ├── prompts.memql      AI prompt schemas (+ prompts/*.tmpl)
│   │   ├── automations.memql  Event-driven workflows
│   │   └── ...                (not every namespace carries every construct)
│   └── _reference/    Per-construct authoring reference skeletons
│                      (_concept / _shape / _spec / _trait / _agent)
├── integrations/      External services + DSL-callable capabilities (Go)
├── brand/             The product's visual identity, as plain CSS custom
│                   properties: tokens, the Tailwind v4 @theme bridge, the
│                   three self-hosted faces, the mark and the favicon. Imported
│                   by BOTH clients/portal (Vite) and component/identity/web
│                   (the standalone Tailwind CLI, embedded in the Go binary) --
│                   they share no package manager, and CSS variables are the
│                   one format both consume. Never copied: brand_shared_source_test.go
│                   fails the build on a --memql-* token, an @theme block or an
│                   @font-face defined outside it (memql#4266)
├── clients/           Surfaces built ON the platform -- the mirror of
│   │                  integrations/ (which points outward). One directory per
│   │                  client; the engine carries exactly one, which is the
│   │                  worked example the memql-project template copies.
│   ├── README.md      The convention: what belongs here, and the wiring a
│   │                  client needs (npm package, Go server, CI lane + bucket,
│   │                  Dockerfile stage, deploy component)
│   └── portal/        MemQL Portal -- the platform's graphical operations
│                      console, the Cockpit's browser sibling. React + TS +
│                      Vite + Tailwind; served by component/edge as site #1
├── component/         Core Go components
│   ├── bus/           Channel-based inter-component communication (Go)
│   ├── config/        Centralized env var loading (Go)
│   ├── node/          Distributed node system (identity, peer mesh, bootstrap)
│   ├── architecture/  Auto-generated architecture model (UML/C4 from source)
│   ├── observe/       Per-invocation observability runtime (FQN-keyed)
│   ├── envregistry/   Env-var registry: manifest, boot validation, domain
│   │                  derivations, legacy aliases, repo-root .env override
│   │                  (localenv.go). Was `genesis/` until memql#3963; the
│   │                  sealed-envelope half it shared a directory with is gone
│   └── ...            (memql, grpc, events, database, server, auth, edge, etc.)
├── core/              Shared utilities (logger, env, id)
│   └── dslfs/         MEMQL_DSL_PATH on-disk override / embedded FS picker
├── cmd/               Command-line tools (healthcheck, memqlfmt, memqlmigrate,
│                      memqllint, frontdoorhosts, frontdoorpaths, admin-preview, etc.)
├── deploy/k8s/        GitOps manifests: base + components + per-env overlays
├── scripts/           Shell scripts (k3d bring-up, deploy, release, install,
│                      migrations) + `lib/capability.sh`, the capability runtime
├── docs/              Documentation
├── docker/            Dockerfile + db init + nginx assets
└── .claude/           Claude Code project state. This repo is PUBLIC, so
    │                   .gitignore ignores `.claude/*` and negates back only
    │                   what should travel with the project (memql#3344)
    ├── skills/        Project skills -- tracked
    ├── commands/      Project slash commands -- tracked
    ├── agents/        Project subagent definitions -- tracked
    ├── settings.json  Shared project settings -- tracked
    ├── settings.local.json   Personal permission overrides -- NEVER tracked
    ├── epics/ prds/   CCPM working state -- not tracked; the durable record is
    │                  the GitHub Issues CCPM syncs them to
    └── worktrees/     Local checkouts -- not tracked
```

---

## Key Directories

| Directory | Purpose | Language | CLAUDE.md |
|-----------|---------|----------|-----------|
| `dsl/<ns>/automations.memql` | Event-driven automations | MemQL | — |
| `dsl/<ns>/queries.memql` | Query functions | MemQL | — |
| `dsl/<ns>/mutations.memql` | Mutation functions | MemQL | — |
| `dsl/<ns>/specs.memql` | Specification predicates | MemQL | — |
| `dsl/<ns>/tools.memql` | AI tool definitions | MemQL | — |
| `dsl/<ns>/prompts.memql` | AI prompt schemas (+ `prompts/*.tmpl`) | MemQL | — |
| `dsl/providers/providers.memql` | AI provider configurations | MemQL | — |
| `dsl/<ns>/shapes.memql` | Reusable shape templates | MemQL | — |
| `dsl/policies/policies.memql` | AI provider-selection policies | MemQL | — |
| `integrations/` | External service integrations + DSL capabilities | Go | [→](integrations/CLAUDE.md) |
| `clients/` | Surfaces built ON the platform (SPAs, landing pages, apps). Plural + first-class, the inward-facing mirror of `integrations/`. The engine carries one inhabitant -- the portal -- as the worked example downstream repos copy | TypeScript | [→](clients/README.md) |
| `clients/portal/` | MemQL Portal -- the platform's graphical ops console (React + Vite + Tailwind), served by `component/edge` as site #1 (its own hostname, `bundleRef: file:///app/portal`) | TypeScript | [→](clients/README.md) |
| `component/` | Core service components | Go | [→](component/CLAUDE.md) |
| `component/bus/` | Channel-based component communication bus | Go | -- |
| `component/config/` | Centralized configuration loading | Go | -- |
| `component/language/` | The MemQL front end: lexer, parser, struct-form rewriter, AST, compiler, and the annotation / spec / clause / pagination registries | Go | [→](component/language/CLAUDE.md) |
| `component/node/` | Distributed node system (bootstrap, peers, mesh) | Go | [→](component/node/CLAUDE.md) |
| `docs/` | Documentation | Markdown | [→](docs/CLAUDE.md) |

---

## Documentation

**Start here:** [docs/public/overview/quickstart.md](docs/public/overview/quickstart.md) - Get running in 5 minutes

**Full index:** [GLOSSARY.md](GLOSSARY.md) - Find any documentation

**Tech stack:** [docs/public/overview/tech-stack.md](docs/public/overview/tech-stack.md) - Deployment practices

**Operations:**
- [Environment variables](docs/public/operate/env-vars.md) -- bootstrap envelope vs. concept-stored config; how to add / rotate / override
- [Auto-generated architecture diagrams](docs/internal/design/auto-generated-diagrams.md) -- the static topology model + observe runtime + cockpit drill-down navigator. Includes `.env` repo-root override flow (`component/envregistry/localenv.go`) and `MEMQL_OBSERVE_LEVEL`.

**Core concepts:**
- [Component vs integration vs pack](docs/public/concepts/component-integration-pack.md) -- the three words; intake "plugin" means pack
- [Architecture](docs/public/concepts/architecture.md)
- [MemQL Language](docs/public/language/memql.md)
- [Functions](docs/public/language/functions.md)
- [Events](docs/public/concepts/events.md)
- [Node Identifier Conventions](docs/public/concepts/identifiers.md) -- canonical `{concept}:{shortId}` internally vs the BARE-ids client contract at every wire seam (engine bare-ifies on egress, resolves bare args on inbound; clients never compose/parse/compare canonical ids), the `(concept, id)` keying rule, who composes ids, anti-patterns
- [MemQL Authoring Rules & Gotchas](docs/public/language/authoring-rules.md) -- read before writing `.memql` files
- [LLM cost control (defense in depth)](docs/public/ai/llm-cost-control.md) -- the layered guardrails (kill-switch, rate ceiling, automation budget, loop caps) that make a runaway spend loop structurally impossible; every `MEMQL_LLM_*` / budget env var + how to repro safely. Read before touching `ai_guard.go`, an LLM loop, or an automation that drives model calls.
- [Tool ↔ Knowledge Domain Pattern](docs/public/concepts/tool-knowledge-domain-pattern.md) -- when a capability has operational knowledge (UI takeover, Computer Use, etc.), put it in a knowledge domain that the tool requires, not in the agent prompt template. Read before adding capability-bundled documentation.

**Tooling:**
- **memql-cockpit** -- terminal-native IDE and operations console (display name "MemQL Cockpit"). Lives in its own repo at `github.com/znasllc-io/memql-cockpit`; consult that repo's CLAUDE.md and Makefile.

---

## Development Workflow

### Development Environment (k3d + ArgoCD)

The k3d + ArgoCD cluster is the local dev topology (memql#2061 /
E0 -- Argo parity). It mirrors the cloud cluster (AKS + ArgoCD + the
k8s base in `deploy/k8s/`) so the same manifests and reconciliation
path run locally and in the cloud. Multi-node is the default (#2067):
use `make up SERVERS=2 + make scale N=2` for full cross-node
mesh testing.

**Prerequisites:** docker, k3d, kubectl (`brew install k3d kubectl`).

```bash
# Bootstrap (creates cluster + installs ArgoCD + seeds secrets):
make up                       # single-node default
make up SERVERS=2 AGENTS=1   # multi-node (for cross-node mesh testing)

# Inner-loop dev (after code change):
make dev                      # rebuild + import + restart ALL app nodes
make dev NODE=bff             # single node (faster)
make dev PULL_INFRA=1        # refresh infra images (postgres/azurite/livekit)

# Multi-node scaling:
make scale N=2      # 2 replicas per Deployment
make status                   # litmus: unique MEMQL_NODE_ID per pod +
                              #         one shared identity signing keyset

# Secrets (re-seed if changed):
make secrets

# Tear down:
make down                     # keep kubeconfig
make down PURGE=1             # also remove kubeconfig context
```

See [docs/public/operate/reproduce-the-cloud-locally.md](docs/public/operate/reproduce-the-cloud-locally.md)
for the full k3d runbook and port-forward reference.

### Building
```bash
# Standalone binary (all components)
go build -o bin/memql .

# Node-type binaries
go build -tags bff -o bin/memql-bff .
go build -tags cognition -o bin/memql-cognition .
go build -tags agent -o bin/memql-agent .
go build -tags planner -o bin/memql-planner .
```

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

MemQL uses a single long-lived branch: `main`. Core engine, wire
protocol, and DSL all live here.

**Rules of engagement:**

1. **Every change goes through a branch + PR. `main` refuses direct
   pushes.** This is enforced by a repository ruleset, not by
   convention: `gh api repos/znasllc-io/memql/rules/branches/main`
   returns `pull_request`, `required_status_checks` and `merge_queue`
   (plus `deletion` and `non_fast_forward`), so `git push origin main`
   fails with `push declined due to repository rule violations` no
   matter how small the change. A one-line docs fix needs a PR exactly
   like a feature does.

   Branch, push, open the PR, let CI go green, then **enqueue it**:

   ```bash
   gh pr merge <n> --repo znasllc-io/memql   # bare: no strategy, no --delete-branch
   ```

   **The bare form is the whole instruction, and both flags you would
   reach for are wrong.** `--delete-branch` is REFUSED outright --
   `Cannot use -d or --delete-branch when merge queue enabled` -- because
   the queue deletes the branch itself. `--merge` is merely ignored with
   `The merge strategy for main is set by the merge queue`: the queue's
   `merge_method` is already `MERGE`, so squash merges stay disabled
   repo-wide and the strategy is not yours to pass.

   **It ENQUEUES rather than merges, and the wait is by design.** The
   queue's `min_entries_to_merge_wait_minutes` is 5 and it batches under
   `grouping_strategy: ALLGREEN`, so a PR sits at `OPEN` with
   `mergedAt: null` for minutes with nothing wrong. Re-running the
   command answers `is already queued to merge`, which is the
   CONFIRMATION it worked rather than an error -- reading it as one and
   "retrying" is the natural mistake.

   **A queued PR can go `DIRTY`, and it will stay there.** When a
   sibling lands underneath it, `mergeStateStatus` becomes `DIRTY` and
   does not resolve itself; rebase on `origin/main` and force-push.
   Worth stating because the failure is silent in a specific way: a
   watcher looking only for merged / failed / clean cannot see `DIRTY`
   at all, and its silence is indistinguishable from "still queued".
2. **Pre-release -- no backwards-compat shims or deprecation windows.**
   When a contract changes, fix both MemQL and the consumer at once and
   delete what is no longer needed. Do not add legacy adapters, fallback
   code paths, or "keep working while we migrate" layers.
3. **Stage files by explicit path** (`git add <file>`) -- never
   `git add -A` or `git add .`. The repo owner runs multiple Claude
   sessions against this working tree and untracked files from another
   session must not get swept into your commit.

**What triggers a frontend team ping:** if the backend change alters
a wire contract the frontend depends on (removed/renamed `/si/*`
endpoints, changed required request fields, new required response
fields, new gRPC message types the client must handle to get a
complete response), call it out explicitly in the commit body /
summary so the repo owner can relay to the frontend team. Backend-
internal refactors that leave the wire identical -- file moves,
renamed internal functions, which node owns a handler -- don't need
frontend coordination.

---

## Common Tasks

| Task | Command | Description |
|------|---------|-------------|
| **Bootstrap k3d cluster** | `make up` | Fresh bring-up: cluster + ArgoCD + secrets + images, wait healthy (memql#2061 / Epic 0) |
| **Inner-loop rebuild** | `make dev [NODE=<type>]` | Build image -> k3d import -> kubectl rollout restart |
| **Clean slate (nuke + repave)** | `make up-refresh` | Tear down + recreate cluster (fresh DB), rebuild images, wait healthy |
| **Cluster litmus** | `make status` | Verify unique MEMQL_NODE_ID per pod, and that every identity replica publishes the same JWKS keyset (mesh parity check) |
| **Multi-node scaling** | `make scale N=2` | 2 replicas per Deployment for cross-node mesh testing, in namespace `memql` (`NAMESPACE=` overrides) |
| **Re-seed secrets** | `make secrets` | Idempotent; use after cluster recreate |
| **Tear down cluster** | `make down` | Delete k3d cluster (PURGE=1 also removes kubeconfig) |
| **Run tests** | `make test` | Go tests. NOT `go test ./...` -- that misses the engine's own modules (memql#4032); see Testing |
| **Build binary** | `go build -o bin/memql .` | Build BFF binary (default) |
| **Connect DB** | `psql postgres://memql:memql_dev@localhost:5432/memql` | Database shell (after `make up`) |
| **Regenerate the front door** | `make frontdoor` | Both generators, in order: hosts, then paths |
| **Regenerate front-door hosts** | `make frontdoor-hosts` | Re-emit the cloud overlay's `front-door.generated.yaml` from the closed role set (memql#3767). Run after changing a role or the composition rule |
| **Regenerate front-door paths** | `make frontdoor-paths` | Re-emit the bff's HTTP Ingress rules in every api front door from `component/server`'s path declarations. Run after adding an HTTP route |
| **Front-door gates** | `make frontdoor-hosts-check` / `make frontdoor-paths-check` | Fail when a generated front door or path block is stale. The path drift catches an HTTP path nothing routes -- which does not 404, it hands HTTP/1.1 to an h2c backend (memql#3703); the host drift catches a service reachable at a name nothing serves |

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
┌─────────────────────────────────────────────────────┐
│   Front door (TLS 443) -> bff gRPC :50051 (h2c)     │
│                        -> bff-http :8085 (exceptions)│
├─────────────────────────────────────────────────────┤
│  ┌──────────────┐  ┌──────────────┐  ┌──────────┐   │
│  │   MemQL      │  │ Automations  │  │ Functions│   │
│  │   Engine     │◄─┤   System     │◄─┤  System  │   │
│  └──────┬───────┘  └──────────────┘  └──────────┘   │
│         │                                            │
│    ┌────┴────────┐ ┌──────────────┐                  │
│    │ AI Provider │ │ Integrations │                  │
│    │  Registry   │ │ (Cognition,  │                  │
│    │(OpenAI,     │ │  Audio, etc) │                  │
│    │ Anthropic)  │ └──────────────┘                  │
│    └────┬────────┘                                   │
│         │                                            │
│    ┌────┴────────────────────┐  ┌──────────────┐     │
│    │  AI gRPC Messages       │  │ MemQL Sense  │     │
│    │  (MemqlService.Stream): │  │ (Language    │     │
│    │  AiChatMsg, AiSpeechMsg,│  │ Intelligence)│     │
│    │  AiTranscribeMsg,       │  │ Tokenize,    │     │
│    │  AiSuggestMsg (space /  │  │ Complete,    │     │
│    │  group / agent)         │  │ Diagnose,    │     │
│    └─────────────────────────┘  │ Hover, Sig.  │     │
│                                 └──────────────┘     │
├─────────────────────────────────────────────────────┤
│          PostgreSQL + TimescaleDB                   │
│   (time-series memory nodes; PK: (id, createdAt))   │
└─────────────────────────────────────────────────────┘
```

### Distributed Node Architecture (Cluster Mode)

MemQL uses **Go build tags** to compile separate binaries for each node type.
A tag selects which `app/build_*.go` runs, and therefore which integrations and
transport layers a node WIRES UP.

**Tags are a wiring mechanism, not a size mechanism.** Every node binary is
within ~5.5% of every other one (80.4-84.8MB, `CGO_ENABLED=0`, no strip). Two
structural reasons, both measured in memql#4106: ~32 MiB of every binary is a
single Go 1.26 stdlib symbol (`crypto/internal/fips140/drbg.memory`), and the
tag gating stops at `app/` -- `go list -deps` moves only 3-5 of ~116 first-party
packages per tag, so untagged packages keep the heavy vendor set constant in
every build (a `planner` binary still links 79 `pion/*` packages via
`integrations/telephony`). Full numbers + the three offending imports:
[docs/public/build/build-tags.md](docs/public/build/build-tags.md#binary-size).
Never justify a build tag by expected binary size:

```bash
go build .                       # bff        (~80 MB, default)
go build -tags voice .           # voice      (CGO_ENABLED=1 required; not measured here)
go build -tags cognition .       # cognition  (~81 MB)
go build -tags agent .           # agent      (~82 MB)
go build -tags planner .         # planner    (~81 MB)
go build -tags edge .            # edge       (serves hosted sites + the portal)
```

This diagram shows only the mesh/product node types; the complete 9-type
list (identity, bff, cognition, agent, planner, voice, workbench, mcp, edge)
is in "The engine is the whole platform" below.

```
        ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐
        │   BFF    │ │  Voice   │ │Cognition │ │ Planner  │ │  Agent   │ │   Edge   │
        │  Node    │ │  Node    │ │  Node    │ │  Node    │ │  Node    │ │  Node    │
        └──────────┘ └──────────┘ └──────────┘ └──────────┘ └──────────┘ └──────────┘
         backend      voice        cognition     planning     task exec    sites +
         for front.   transport    pipeline       orchestr.    AI work    portal
```

- **BFF** (default): Backend for frontend, domain-specific API surface
- **Voice**: Voice transport (audio WS, LiveKit)
- **Cognition**: Cognition pipeline, Polyphon
- **Agent**: Task execution, AI work, tool calling
- **Planner**: Task planning and orchestration
- **Edge**: Serves this cluster's hosted web surfaces -- every hosted SPA/website
  and the MemQL Portal itself (site #1, no special path) -- by resolving the
  request `Host` header to a `v1:platform:site` graph row (epic memql#3700)

Nodes discover each other via mesh. All nodes share a single
PostgreSQL + TimescaleDB database. Inter-node communication uses `NodeService` gRPC
bidirectional stream. Events bridge across nodes with dedup and TTL.

#### Multi-node is the DEFAULT -- design, implement, AND test for cross-node

The cluster (2 replicas per mesh node) is the runtime in the **cloud** and in
the local **parity cluster**, which is the topology every feature must be
designed and tested against. Never reason about a feature as if it runs in a
single process.

> A cloud cluster spends most of its life scaled to ZERO when idle
> (`make scale N=0`) -- the saving is the idle time, not the width. Parking it
> at one replica instead would make it unable to catch the class below, and the
> money that saves is the fraction of the time it is up at all.
>
> The local 2-replica parity cluster (`make up SERVERS=2` + `make scale N=2`) is
> the blessed repro -- it is the topology a developer can iterate against.

This has bitten us repeatedly -- a fix ships
with green single-node tests and silently breaks in the mesh (e.g.
realtime `ListTools` proxied bff->agent read `voiceAgentSpaceId` off the
wrong-node session and failed open to the full 517-tool registry, #1448;
the voice gate directive had no mesh routing rule, #1412; snapshot peers
reaped after a bff cutover, #1388).

**When implementing:** any state/context/event that crosses a node
boundary needs EXPLICIT plumbing -- it does NOT travel implicitly.
- Session / in-memory state (caches, waiters, fields like
  `voiceAgentSpaceId`) lives on exactly ONE node. A different node
  handling a **proxied / forwarded** request (`AiForwardRouter`,
  `proxySI`, `NodeService` forwards) does NOT see it -- thread it
  through the message or metadata and resolve it on the receiving side.
- Every cross-node event-bus pub/sub needs a **routing rule**
  (`node.RegisterRoutingRule`) or it silently dies in cluster mode.
- Before calling a feature done, ask: *which node holds this state, and
  which node needs it?* If those differ, you have cross-node work to do.

**When testing:** a green single-node unit test is a FALSE signal for
cross-node behaviour. Tests MUST exercise the hop -- a handler running on
a session WITHOUT the originating node's local state, context surviving a
proxy/forward, an event consumed on a different replica. Add coverage to
the cluster-e2e harness (`test/clustere2e/`) and/or the proxy-path tests
(`component/grpc/ai_forward_test.go`); the test should FAIL against
single-node-assuming code and PASS with the cross-node fix. The blessed
local repro is the 2-replica parity cluster (`make up SERVERS=2` +
`make scale N=2`) -- the only topology that reproduces this
bug class. See
[docs/public/operate/reproduce-the-cloud-locally.md](docs/public/operate/reproduce-the-cloud-locally.md).

#### Node image source: product-agnostic engine images + runtime DSL delivery (platform consolidation #2472)

**The engine is the whole platform.** Every node type -- identity, bff,
cognition, agent, planner, voice, workbench, mcp, edge -- ships as a
**product-agnostic engine image** from THIS repo's Dockerfile
(`BUILD_TAGS=<type>`). There are no per-product node images and no
carrier-built nodes in the common case. Reusable capabilities (chat,
daily-space, avatar, ...) live in the engine as **generic, DSL-configurable
features**, never product code.

**What "never product code" is actually enforced by** -- two narrow guards, not
a general one, so know their edges (memql#3326):

- `TestEngineIsProductNeutral` (`product_neutrality_test.go`) -- a **banned-names
  list**. It sweeps every tracked file, path and body, for the specific product
  names this repo shed. It cannot notice a product arriving under a name nobody
  thought to ban.
- `TestClientsDirectoryIsAllowlisted` (`clients_allowlist_test.go`) -- an
  **allowlist of `clients/` inhabitants**. `clients/` is where a client
  application would land, so it gets structural enforcement: an unlisted
  directory fails. The engine hosts the platform's own console (the portal); a
  customer's SPA belongs in a product repo built from the `memql-project`
  template.

Everything else -- generic-vs-product Go in `component/`, a product-shaped
concept in `dsl/` -- rests on review. Write new product-shaped code as a
DSL-configurable feature or keep it downstream; do not read the guards as
proof that anything unflagged is neutral.

**Product DSL is delivered at runtime, not compiled in.** A product ships its
DSL as a tiny data-only **bundle image**; the `dsl-bundle` kustomize component
(`deploy/k8s/components/dsl-bundle`) runs it as an init-container that copies
the `.memql` tree into a shared volume the node reads at `MEMQL_DSL_PATH`.
`dsl.MountRuntimeDomainsFromEnv` (see the `MEMQL_DSL_PATH` section above)
mounts each product domain via `RegisterTree` at boot, so a plain engine image
runs any product's DSL with zero compiled-in product code. A "bff" is just a
plain engine `bff` node fronting a product's bundle -- a deploy concern.

Topology (one product-agnostic engine mesh; a product = a DSL bundle + a
client; a release = `{engine version, bundle digest, client digest}`):

```
   product-agnostic engine images (memql, one public repo)
   identity · bff · cognition · agent · planner · voice · workbench · mcp
        ▲  mounts the product's DSL bundle at MEMQL_DSL_PATH
        │  (data-only image → init-container → shared volume)
   DSL bundle (product)     +     client (SPA)      ← the only per-product artifacts
```

Genuinely-bespoke product Go (rare) becomes a thin optional `bff/` pack
module in the product repo. Full rationale:
[docs/internal/design/platform-consolidation.md](docs/internal/design/platform-consolidation.md).

**Build tag reference:** [docs/public/build/build-tags.md](docs/public/build/build-tags.md)

#### Environment parity -- one topology everywhere (NON-NEGOTIABLE)

The local cluster and a cloud cluster run the **same topology, the same
deployment process, and the same connection model**. Only **configuration
values** and **hardware resources** differ -- never the shape of the system.
FIXED everywhere: the node topology, the GitOps base+overlay+ArgoCD deploy path
(`make up` locally applies the same manifests ArgoCD applies in the cloud), and
the client connection (ingress -> TLS -> gRPC -> `bff`, dialed as
`https://api.<domain>`). ALLOWED to vary (overlay/registry values, not
architecture): image digests/tags, replicas/resources, domain, DNS source
(hosts vs real), TLS source (mkcert vs cert-manager), ingress controller
(traefik vs nginx annotations), secrets source. **Reject in review:**
port-forward-as-connection, target-specific commands (`run-local`), `if
env=="local"` branches, or a second way to deploy -- the moment local diverges
in *shape* it stops proving anything about the cloud. Ask of any change: *is
this the shape of the system (→ base/component, everywhere) or a value (→
overlay)?* The standard: [docs/public/operate/environment-parity.md](docs/public/operate/environment-parity.md).

**ONE INSTALLATION SHAPE (epic memql#3943).** MemQL has no
staging-versus-production dimension. An operator who wants a second environment
installs a second instance -- its own cluster or at least its own ArgoCD, its
own domain, its own database. There is one cloud overlay
(`deploy/k8s/overlays/cloud`), one ArgoCD Application (`memql`), one namespace
(`memql`), and everything in that overlay is a VALUE over
`deploy/k8s/base`. This reverses epic memql#3748, which put two environments in
two namespaces of one cluster and separated their data with a Postgres schema
search path.

**Therefore: no `if env == "..."` in engine code.** There is no environment for
a branch to read, and a branch that invented one would be the second way to
deploy this standard rejects. `TestNoEnvironmentBranchingInEngineCode`
(`environment_branching_test.go`) fails the build on engine code so much as
NAMING `prod` / `production` / `staging`, in any form -- comparison, switch case
or map key -- and its exemption map is now EMPTY, so nothing in the tree is
excused. `development` / `local` stay outside that gate: they distinguish deploy
TARGETS (k3d vs AKS), which the design keeps and which carries its own field,
`provider` (`docker-local` | `azure`).

**Local cluster (cloud parity -- THE blessed local topology, memql#2061 /
Epic 0).** `make up` brings up k3d + ArgoCD + the local overlay at
`deploy/k8s/overlays/local` + seeded secrets; `make dev [NODE=<type>]`
rebuilds an image, imports it into k3d, and rolls the Deployment after
Go/MemQL source edits; `make down` tears it down. It is the ONLY supported
local run path -- a single-node stack structurally cannot reproduce the
resilient-mesh class of bugs. What makes it parity rather than a lookalike:

- **Same manifests, same reconciliation as AKS** -- it mirrors the cloud
  along the **mesh-delivery path** (memql#1212). Scale to 2 replicas per
  mesh node (bff/cognition/voice/agent/planner/workbench/edge) with
  `make up SERVERS=2` + `make scale N=2`; each pod carries a unique
  `MEMQL_NODE_ID` via `fieldRef: metadata.name` exactly as in the cloud.
- **Clients connect exactly as in the cloud.** The Cockpit and SDKs reach
  the `api.memql.localhost` traefik front door (TLS on 443 with the mkcert
  `*.memql.localhost` wildcard `memql-front-door-tls`, forwarding h2c gRPC
  to `svc/bff:50051`) -- the local analog of the cloud nginx ingress.
  `identity.memql.localhost` works the same way. This is **env parity,
  non-negotiable**: there is NO local-only port-forward in the connection
  path (the standard:
  [docs/public/operate/environment-parity.md](docs/public/operate/environment-parity.md)).
  Raw kubectl port-forwards (postgres `:5432`, `svc/bff 50051`,
  `svc/identity 8085`) remain for low-level debugging only.
- **The domain is a VALUE, not the shape of the system** (memql#3593).
  `make up DOMAIN=lab.example.com` serves any domain the operator brings,
  seeded as the single `MEMQL_DOMAIN` key of the `memql-domain` ConfigMap
  that every node derives its issuer, CORS origins and OAuth redirect URIs
  from at boot (`component/envregistry/domain.go`), plus two
  `kustomize.patches` on the ArgoCD Application for the Ingress hostnames
  when it differs from the committed default. **No file under `deploy/`
  names a domain.**
- **The engine bff is a COMPONENT, not the base.** Engine-only overlays opt
  into one product-agnostic `bff` via `deploy/k8s/components/engine-bff`
  (the Cockpit / ops edge, no bundle, #2472 Decision 5), so a product
  cluster bringing its OWN `bff-<product>` (same engine image + the
  `dsl-bundle` component mounting its bundle, plus its SPA) never collides
  with a base-shipped bff. Multiple bffs coexist in the one mesh.
- **`make status` is the litmus.** Per-pod node ids, plus a check that
  every identity replica publishes the same JWKS keyset -- divergent
  keysets fail roughly half of all auth (memql#3400).

Reach for the cluster whenever a change can touch cross-node delivery,
replica fan-out, or node lifecycle. Runbook:
[docs/public/operate/reproduce-the-cloud-locally.md](docs/public/operate/reproduce-the-cloud-locally.md).

#### Client-tool relay (agent → browser, across nodes)

The MemQL tool registry supports **client-executed tools** (tools whose
implementation runs in the browser, e.g. UI-drive helpers). In
single-binary mode the agent's `InvokeClientTool` writes directly to
the browser's stream and parks on a session-scoped waiter. In cluster
mode the agent and browser live on different nodes, so the
`ClientToolCall` envelope needs a cross-node round-trip. MemQL does
this via the graph event bus:

1. Cognition intercepts `ClientToolCall` in `consumeAgentTurnStream`
   and inserts a `v1:cognition:client:tool:request` node (via
   `emitClientToolRequest`).
2. Browsers subscribed to the space pick the event up, dispatch the
   tool locally, and insert a matching
   `v1:cognition:client:tool:response` (via
   `emitClientToolResponse`).
3. Cognition subscribes to those responses, wraps the payload in a
   `ClientToolResult` envelope, and calls
   `AgentForwarder.ForwardContinuation` so the agent's
   service-scoped waiter fires and the parked tool loop returns.

The relay lives in `integrations/cognition/client_tool_relay.go`. A client SPA mounts its consumer bridges (operator client-tool, relay, delegate-takeover) on its main page and rides the same protocol.

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

These endpoints **must** remain HTTP due to external protocol requirements:

| Category | Endpoints | Reason |
|----------|-----------|--------|
| **Auth (identity service)** | `/login`, `/auth/magic-link`, `/auth/complete`, `POST /auth/landing`, `GET /auth/magic-link/status`, `POST /auth/magic-link/finish`, `/auth/logout`, `/oauth/token`, `/auth/refresh`, `/.well-known/jwks.json`, `POST /auth/webauthn/register/{begin,finish}`, `POST /auth/webauthn/login/{begin,finish}`, `POST /device/code`, `GET+POST /device`, `GET /enroll` | OAuth 2.0 / magic-link flow requires HTTP redirects, browser form posts, and JWKS publishing. The four `/auth/webauthn/*` endpoints (register memql#3406, login memql#3407) are the same category: WebAuthn is a **browser API** -- the ceremony is `navigator.credentials.create()` / `.get()` running in the page, and the bytes it produces have to reach a server the browser can POST to. There is no gRPC form of "the user touched their security key". RP id derives from `MEMQL_IDENTITY_BASE_URL`, never from the request Host. The login pair is UNAUTHENTICATED by nature (it IS the authentication) and ends in the same OAuth auth code `/auth/complete` produces. The two `/device*` routes are the RFC 8628 device authorization grant (memql#3410): the RFC is **defined over HTTP** -- a device with no browser polls `/oauth/token`, and the human approves at a URL typed into a second device's browser. `/device` is a rendered page, so it is a browser-loads-its-own-UI case like identity's other web pages; `POST /device/code` is the grant's request half and belongs with `/oauth/token`, which it redeems against. `GET /enroll` (memql#3408) is a **page a person opens from a link** -- the one request shape that cannot be anything but HTTP, since it arrives before any application code exists to speak a protocol and exists precisely for someone holding no credential yet. Its single-use `mql_enr_` token is the authorization (`Authorization: Enrolment <token>` on the ceremony that follows, mirroring `/pair/redeem`); HTTPS required on issue AND redeem, per-IP rate-limited, every outcome audited with SourceIP. The three magic-link routes added by memql#4302 are the same category, and the owner approved them explicitly (design D8): `POST /auth/landing` is the browser form post that a GET used to do -- a GET now renders and never changes state, so mail scanners stop burning links; `GET /auth/magic-link/status` is the requesting tab's poll, gated on the `memql_ml` binding cookie and 404 to anyone without it; `POST /auth/magic-link/finish` is that tab completing its own sign-in, which has to be a real form POST because the reply is a 303 the tab must NAVIGATE (a fetch would follow it and strand the auth code). All three are declared on the identity server's own route table, not `component/server`'s, so the bff front-door path generator is untouched |
| **Health check** | `/healthz` | Docker and Kubernetes health probes expect HTTP GET |
| **WebSocket upgrades** | `/memql/ws`, `/memql/audio` | Browser clients need HTTP upgrade to establish WebSocket |
| **File uploads** | `/spaces/{id}/attachments` | Multipart form-data uploads map poorly to gRPC |
| **Site bundle publish** | `POST /sites/{id}/bundles` (bff only) | The reasoning already recorded above for `/spaces/{id}/attachments`: multipart bundles map poorly to gRPC (memql#3713, explicit owner approval on the issue). A CI job publishing a built site hands over an arbitrary, variable-shaped tree of files -- unknown paths, unknown count, mixed binary content types -- which is exactly the shape multipart form-data exists to carry and exactly the shape a fixed protobuf message schema does not. Every CI toolchain already knows how to POST a multipart body; none carries a MemQL gRPC client. `component/edge.Publisher` is what makes the deploy atomic once the bytes arrive: the whole bundle lands under a new content-addressed version prefix and only then does the site row's `bundleRef` flip, so a failed upload never leaves a half-published site reachable, and rollback is one more row write to bytes that are still there. Authorization is a `class="service_account"` identity-issued JWT (memql#691) the handler verifies itself; declared in `server.HandlerAuthorizedPaths()`, not `PublicPaths()`, for the same reason the inbound receiver is below: that list is consulted by the verifier middleware on every verifier-consuming node, so listing it there would make the route unauthenticated for every bearer instead of pinned to the service-account credential specifically. Served by the bff, never the edge node -- the edge is wildcard-routed by site hostname, so a site-agnostic publish endpoint has no coherent address there |
| **Inbound webhooks** | `POST /inbound/{source}` (bff only) | The third party dials US -- Shopify, Amazon SP-API, a POS will POST to a URL and nothing else, so there is no gRPC version of this capability (memql#2957). Deny-by-default source allowlist + per-source HMAC; declared in `server.HandlerAuthorizedPaths()`, not `PublicPaths()`. See [inbound-delivery.md](docs/public/operate/inbound-delivery.md) |
| **One-click unsubscribe** | `GET+POST /unsubscribe` (bff only) | The third party dials US, exactly as with the inbound webhook -- and here the third party is the RECIPIENT'S MAIL CLIENT (memql#3348). RFC 8058 one-click is a contract with Gmail / Outlook / Yahoo: they read the `List-Unsubscribe` header off a message we sent and POST `List-Unsubscribe=One-Click` to the URI they find there. There is no gRPC form of that conversation, and without it there is no one-click unsubscribe -- which the same providers now treat as a bulk-sender defect. GET renders a confirmation page (what a person clicking the link in the body reaches); POST performs the opt-out. The split is load-bearing: mail clients and security appliances PREFETCH links, so a GET with the side effect silently unsubscribes people who never clicked, which is precisely why the RFC specifies POST. Authorization is an HMAC-signed token carrying (owner, recipient, campaign) -- verified before any row is read, and the identity the handler then impersonates comes out of the signed payload rather than a parameter, so an unsigned request cannot aim it. Declared in `server.HandlerAuthorizedPaths()` + `SelfAuthenticatedPaths()`, not `PublicPaths()`. See [campaign-sending.md](docs/public/operate/campaign-sending.md) |

### The front door's HOST set is generated too (memql#3767)

The host set is DERIVED from the closed **role** set plus the platform's own
site, not maintained as a list:

| Role | Host |
|---|---|
| api | `api.<domain>` |
| identity | `identity.<domain>` |
| mcp | `mcp.<domain>` |
| sites | `portal.<domain>` (site #1, its own exact rule), `*.<domain>`, plus the apex |

**Every host is a SINGLE label under the domain, and that is a ROUTING fact.** An
Ingress wildcard matches exactly ONE label, so the one `*.<domain>` rule routes
every present and future site to the edge. **It is NOT a certificate fact
(memql#4224).** The cloud ClusterIssuer solves HTTP-01 only: ACME cannot issue a
wildcard over HTTP-01, and ONE wildcard dnsName fails the WHOLE order -- so the
Certificate that requested `*.<domain>` sat Pending, and once it was hand-edited
to exact names, the edge Ingress whose `tls.hosts` still said `*.<domain>` made
ingress-nginx serve its self-signed default for `portal.<domain>`. The
front-door certificate therefore names EXACT hosts only (`api.`, `identity.`,
`mcp.`, `portal.`, the apex); every Ingress lists exactly its own exact rule
hosts under `tls`; and the union of those lists equals the dnsNames
(`deploy/k8s/overlays/frontdoor_hosts_test.go` gates all three). The wildcard
RULE stays and has no certificate behind it: a customer site hostname on the
cloud front door needs its own Certificate and exact-host Ingress until a
DNS-01 solver exists. The portal carries an exact rule because ingress-nginx
builds a certificate-bearing server block per RULE host, never per tls host --
it is the one site whose name exists before any row does. (The host set was
once the product of role x ENVIRONMENT, with a label that hyphenated into role
hosts and nested into site hosts; epic memql#3943 removed the environment
dimension, so the product has one factor left.)

`cmd/frontdoorhosts` writes `front-door.generated.yaml` into each instance
overlay (`overlays/cloud`, `overlays/cloud-entry`);
`component/envregistry/domain.go` composes the node's own issuer / CORS origins /
redirect URIs from the SAME rule through `component/frontdoor`; and
`component/memql`'s SeedMaterializer seeds the portal site row's hostname from
it (`frontdoor.PortalHost`). One derivation, three consumers: a second copy of
the rule would disagree, and the disagreement is an issuer nothing is served at
-- or a certificate naming a host the site row does not carry -- which presents
as "sign-in is broken" with every manifest looking correct.

Adding a ROLE is a design change, not a configuration change. The LOCAL
overlay's five front-door files stay hand-authored (traefik, not nginx, and they
carry the measured priority reasoning from memql#3810), but they are gated
against the same derivation, so they cannot drift from it. Locally the mkcert
pair is still a `*.<domain>` + apex wildcard, which is a TLS-source VALUE and
the one place local is more permissive than the cloud: a site that works over
https locally is no evidence it has a certificate in the cloud.

Details: [docs/public/operate/front-door.md](docs/public/operate/front-door.md).

### How an HTTP path reaches the front door (GENERATED, memql#3703)

Every HTTP path above needs its own Ingress rule, and **that rule list is
generated, not authored**. An ingress controller's backend protocol is a
per-**Service** setting, so the bff's gRPC edge (`bff`, :50051, h2c) and its HTTP
edge (`bff-http`, :8085) are two Services over one Deployment -- and a path with
no rule falls through to the `/` h2c catch-all. **That is not a 404: it is an
HTTP/1.1 request handed to an h2c backend, which fails with a protocol error
naming nothing.** Hand-maintaining the list left `/inbound/{source}` and
`GET+POST /unsubscribe` -- two of the exceptions in the table above, both dialled
by third parties -- routed by no overlay in this repository at all.

`cmd/frontdoorpaths` emits the block between the markers in
`deploy/k8s/overlays/local/api-front-door.yaml`. Three things about the
derivation are load-bearing:

- **It is per-ROUTE, not per-authentication-tier.** `server.PublicPaths()` +
  `HandlerAuthorizedPaths()` + `SelfAuthenticatedPaths()` answer *who may reach
  this without a bearer*. An **authenticated** HTTP route appears in none of
  them, which is how `/spaces/` (attachment upload), `/polyphon/room-token` and
  `/polyphon/status` came to be served by the bff and routed by nothing. The
  generator unions the aggregates **and** every per-route declaration a
  bff-tagged build mounts.
- **It over-approximates for a path the bff does NOT serve.** Paths only the
  identity node serves (`JWKSPaths()`, `IdentityDiscoveryPaths()`) are kept:
  adding a rule for a path this backend does not serve costs a 404, while
  omitting one for a path it does costs a protocol error naming nothing.
- **That pricing INVERTS for a path the bff does serve, and this is the trap.**
  There, adding a rule does not cost a 404 -- it makes the endpoint externally
  reachable, and for anything in `PublicPaths()` (which the verifier bypasses)
  that means exposure. `/metrics` is the case: it is unauthenticated *because* it
  is in-cluster-only, and it is mounted on every node type. So there is a fourth
  classification, `servedButNotExternallyRouted`, for "the bff serves it and it
  must stay off the public ingress" -- `/metrics` and `/api/concepts*` are in it.
  **"When in doubt, include" applies only to the previous bullet.**

Two gates make it non-recurring. `TestFrontDoorPathsAreNotStale` fails when the
checked-in block is not what the generator produces
(`make frontdoor-paths-check`), and `TestEveryServerPathDeclarationIsClassified`
AST-scans `component/server` for every `func …Paths() []string` /
`…Routes() []string` and fails when one is classified by none of the generator's
four maps. **So a new HTTP path DECLARATION either reaches the front door or
breaks the build.**

Note the word *declaration*, because the stronger claim is false on the bff: a
route mounted through `handleRoute` with an inline path literal and no `*Paths()`
declaration of its own is invisible to the generator, and the boot check that
would otherwise catch it (`AssertUnauthenticatedSurface`) runs only when the node
installs **no** verifier (`app/transport.go:265`) -- which the bff does. Declare
new HTTP routes with a `*Paths()` function; that is what puts them inside the
gate. Do not hand-edit the block, and do not "simplify" the generator back to the
three aggregates -- its package comment says why, at length, because both changes
look like cleanups.

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

### Provider System
- **Multi-provider architecture** - Unified interfaces (`ChatAIProvider`, `VisionAIProvider`, `TTSAIProvider`, `ChatStreamProvider`) with pluggable backends
- **OpenAI providers** - GPT-4, GPT-5-mini for chat, vision, TTS, and STT
- **Anthropic providers** - Claude Opus, Sonnet, Haiku for chat and vision
- **Provider configuration** - MemQL provider records in `dsl/providers/providers.memql`
- **Provider selection** - Default provider via config, or per-request via `provider` parameter
- **Anthropic credential** - a static key locally, **workload identity federation** in the cloud (epic memql#4333). The engine presents the pod's projected Kubernetes token and the SDK exchanges it for a one-hour bearer, so no long-lived vendor key is at rest. All four ids or none; a partial config REFUSES BOOT rather than falling back to a key the cutover deletes. Cutover, deny reasons and `memql provider-auth check`: [docs/public/operate/auth/anthropic-federation.md](docs/public/operate/auth/anthropic-federation.md)

### AI Endpoints (gRPC on `MemqlService.Stream`)

All AI operations go through gRPC message types on the single bidirectional
stream `MemqlService.Stream`:

- `AiChatMsg` / `AiChatResult` / `AiStreamChunk` -- chat completions (streaming + non-streaming)
- `AiSpeechMsg` / `AiSpeechResult` -- text-to-speech
- `AiTranscribeMsg` / `AiTranscribeResult` -- speech-to-text (batch)
- `AiTranscribeStreamStart` / `Chunk` / `End` -> `AiTranscribeStreamDelta` / `Complete` -- real-time streaming transcription
- `AiSuggestMsg` / `AiSuggestResult` -- carries `domain` ∈ {spaces, spaceTitle, agents, groups, groupDescription, agentCardSummary, spaceCardSummary, groupCardSummary, knowledge}. `spaceTitle` is the lightweight purpose -> title path used by Create Space; `groupDescription` is its mirror (name -> one-line description) used by Create Group. The rich `spaces` / `agents` / `groups` domains return full payloads (description + suggested members + roles). The three `*CardSummary` domains generate the LLM body that lands on the agent / space / group canvas-creation cards. `knowledge` powers the product SPA's knowledge-domain picker.

Cross-node proxying (BFF -> Voice, BFF -> Agent, etc.) rides
`AiForwardRequest` / `AiForwardResponse` on `NodeService.Stream`.
Handlers: `component/grpc/ai_handlers.go`, `ai_transcribe_stream.go`,
`ai_forward.go`.

### Error Handling
gRPC handlers emit a short error id via `generateErrorId()` in
`component/grpc/ai_handlers.go` (format `ERR-{6 hex}`) and log errors
with context. Error ids are visible in slog JSON output as
`"errorId":"ERR-..."`.

### Voice + Video Pipeline (Go voice-agent)

The realtime voice + video channel is owned by the **Go voice-agent**
in [`integrations/voice/agent/`](integrations/voice/agent/), shipped as
the `voice-agent` subcommand of the `memql-voice` binary
(`memql-voice voice-agent`; build with `make voice`, CGO_ENABLED=1,
`-tags voice`). It joins LiveKit rooms as the General Assistant's
voice + video participant. Specialists are text-only by design (per
Initiative C); they never publish into the LiveKit room.

```
LiveKit room
   |
   |  (voice-agent subcommand -- Go, integrations/voice/agent)
   |
   +-- OpenAI Realtime STT (user audio in)
   |              |
   |              v
   |        memql gRPC client         (VoiceAgentTurnRequest -> Delta)
   |              |
   |              v
   |        memql cognition           (BYO conductor + agent tool loop)
   |              |
   |              v
   +-- OpenAI TTS                     (token-by-token input streaming)
   |              |
   |              v
   +-- Anam or Simli avatar           (lip-synced video)
```

The agent supports two executors selected by `MEMQL_VOICE_EXECUTOR`:
`realtime` (default -- OpenAI gpt-realtime speech-to-speech) and
`cascade` (the OpenAI STT -> cognition -> OpenAI TTS path above).

Key files (all under `integrations/voice/agent/`):
- `config.go` / `bootstrap.go` -- env loading + class="voice_agent"
  token resolution (`ResolveVoiceAgentToken`).
- `grpc_client.go` -- speaks memql's `VoiceAgent*` gRPC contract on
  `MemqlService.Stream`. TurnRequest in, TurnDelta stream out;
  specialists are dispatched server-side and land in chat via the
  normal agent path.
- `cascade.go` / `stt_pipeline.go` / `tts_pipeline.go` /
  `turntaking.go` -- the STT/TTS cascade + turn-taking / barge-in.
- `realtime_executor.go` / `realtime_lifecycle.go` /
  `realtime_budget.go` -- the gpt-realtime executor + guardrails.
- `persona.go` / `grounding.go` / `instructions.go` -- persona +
  grounding parity.
- `avatar_room_voice.go` (`//go:build voice`) -- the LiveKit room/media
  glue that mints the avatar's join token, forwards the assistant's PCM
  to the avatar, and handles barge-in. The CGO-free vendor REST/dispatch
  core it drives lives in the shared `integrations/avatarvendor` package
  (Anam default or Simli, selected by `MEMQL_AVATAR_VENDOR`; the persona's
  stamped `avatarVendor` wins over the runtime knob when set), so the
  direct/Guide avatar capability can reuse it too.

Auth: identity-issued `class="voice_agent"` JWT bearer, pinned to the
`VoiceAgent*` message surface by
`component/grpc/voice_agent_stream_interceptor.go`. The voice-agent
cannot write graph rows directly; memql does that server-side.

Env:
- `MEMQL_GRPC_ADDR` -- the BFF's gRPC address (e.g. `bff:50051`).
- `LIVEKIT_URL` / `LIVEKIT_API_KEY` / `LIVEKIT_API_SECRET` -- room
  transport.
- `MEMQL_OPENAI_API_KEY` -- OpenAI (STT + TTS + realtime); required.
- `MEMQL_VOICE_EXECUTOR` -- `realtime` (default) or `cascade`.
- `MEMQL_VOICE_ROOM_NAME` -- room to join (falls back when no
  `--room` flag is passed).
- `MEMQL_VOICE_IDLE_TEARDOWN_SECONDS` -- zero-human grace period
  before a joined session tears down (default 60) so the auto-join
  dispatcher can't wedge on an empty room (#1378).
- `MEMQL_VOICE_MAX_ROOMS` -- max rooms a single voice-agent replica
  serves concurrently in auto-join mode (default 8, #1395). The
  dispatcher discovers every human-occupied polyphon room with no
  voice-agent already present and serves each in its own isolated
  session, so two users in different spaces both get the GA at once;
  cross-replica double-serve is prevented by skipping rooms that
  already contain a `-ga` participant.
- `MEMQL_REALTIME_*` -- realtime executor tuning knobs.
- `VOICE_AGENT_TOKEN` -- identity-issued `class="voice_agent"` JWT
  (#109). Mint via `JWTIssuer.IssueVoiceAgentAccessToken`
  (`make voice-agent-token`); or self-bootstrap via
  `MEMQL_NODE_BOOTSTRAP_TOKEN` + `MEMQL_IDENTITY_VERIFIER_BASE_URL` +
  `MEMQL_VOICE_AGENT_INSTANCE_ID`. See `docs/public/operate/auth/voice-agent-jwt.md`.
- `MEMQL_AVATAR_VENDOR` -- `anam` (default) or `simli` or `none`.
- `MEMQL_ANAM_API_KEY` / `MEMQL_SIMLI_API_KEY` -- vendor keys.

Make targets:
- `make voice` -- build the `memql-voice` binary (carries the
  `voice-agent` subcommand).
- `make voice-agent-token` -- mint a `class="voice_agent"` JWT for the
  local k3d cluster (execs `/app/memql voice-agent-token mint` in the
  identity pod via `kubectl exec`).

Deployment: the `voice` Deployment runs the `memql-voice` image (the
`voice-runtime` CGO stage) with the `voice-agent` subcommand. LOCALLY the
voice lane uses a **LiveKit Cloud** project (Epic #2184; the local overlay
removes the self-hosted livekit-server), and the lane is GATED on the
operator's credentials (memql#2416): without `LIVEKIT_URL` /
`LIVEKIT_API_KEY` / `LIVEKIT_API_SECRET` in the environment at `make up` /
`make secrets`, voice + voice-agent scale to 0 with a loud warning (the
binaries fail-fast on the missing env by design, so running them without
creds is a guaranteed crash-loop). Export the creds and re-run
`make secrets` to enable. The cloud stays self-hosted
(`deploy/k8s/base/livekit.yaml`) via ESO/Key Vault.

`integrations/openai/` on the Go side also serves the `/memql/audio`
WebSocket path for voice-first creation modals.

**Canonical voice catalog (`integrations/voice/voices.go`).** Every
agent carries a canonical voice name (alto / soprano / tenor /
baritone / ...) on `providerConfig.voice.voiceId`, plus a
`gender` enum on the agent record. The catalog is gender-bucketed
and provider-agnostic; the cognition handler resolves canonical ->
provider voice id at TTS-publish time via the active
`MEMQL_POLYPHON_VOICE_PROVIDER`. Voice is auto-assigned at agent creation
(the product SPA's create-agent modal) and never edited by
the user. Two DSL builtins expose the catalog: `voicePickForGender`
+ `voiceResolve`. The General Assistant is hardcoded to canonical
"alto" (female); specialists pick from whichever voices are still
unused by the owner's other agents.

**Per-agent audio + video control.** `v1:agents:agent.audioControl`
+ `videoControl` (`always_on` | `always_off` | `mirror_user`, default
`mirror_user` for every new agent) seed the per-channel defaults.
`v1:cognition:audioOverride` + `videoOverride` carry per-(space, agent)
session overrides written by the PresencePanel orb-corner overlay
(`AgentOrbChannelToggle` -- click to toggle, long-press for the three-
mode menu). The voice-agent's avatar-gating path consults override ->
default for video at session start; audio mirrors the user's mic state
under `mirror_user`. Mutations: `setAgentAudioOverride`,
`setAgentVideoOverride`. Queries: `audioOverridesForSpace`,
`videoOverridesForSpace`.

**Avatar persona.** `v1:agents:agent.avatarPersonaId` +
`avatarVendor` carry the vendor-issued persona / face id minted from a
still image uploaded via the agent edit modal. Empty for legacy or
specialist agents -- voice-agent disables the avatar plugin and falls
back to audio-only.

See [integrations/CLAUDE.md](integrations/CLAUDE.md) for the Go-side
voice-related integrations (openai, voices catalog) that
the `/memql/audio` WebSocket still consumes.

### Cognition (Routing + Conductor)

Cognition decides whether and which agent should respond to an utterance,
then dispatches the turn. The text path uses a **single LLM brain**: the
conductor (`dsl/cognition/prompts/conductorTurn.tmpl`) emits both the
routing decision (fitScore / turnMode / handoff / severity) and the
per-agent plan (primary / sequence / chime-ins / instructions) in one
structured-output call. The standalone router LLM call only fires for
voice utterances now (latency-sensitive); fast-path mention dispatch
bypasses both. Lives in `integrations/cognition/cognition_handler.go`,
`conductor_consult.go`, `ai_router.go`.

**Capability-aware routing.** Both the conductor (and the voice-path
router) see each candidate agent's tool list, so a specialist whose
keywords loosely match an action it has no tool for ("guide me around
the app" hitting an HR specialist) gets penalized; the general
assistant with `uiDescribe` / `uiClick` / `uiNarrate` wins. Tool-fit
mismatch drops fitScore by 0.4+; total tool gap routes to the GA with
`turnMode=escalation_notice`.

**Conversational continuity.** The conductor receives an explicit
`lastResponder` input (computed in `conductor_consult.go` from the
transcript -- the most-recent AI participant to speak before this
human utterance). The "Conversational continuity" meta-principle in
`conductorTurn.tmpl` requires the primary to stay with that agent
when the user's turn is a follow-up shape ("ok cool", "btw", "what
about", "tell me more") and there's no @-mention or domain pivot.
Plugs the "GA jumps in to defer to the specialist" failure mode --
"how can you help me" after Faye's teaching turn now stays with
Faye instead of being routed to Sofia.

**Greet-on-join pacing.** `integrations/cognition/greet_on_join.go`
serializes greetings per-space: 3s initial delay before the first
greeting fires (giving the SPA time to dismiss the create modal +
finish the route transition), 4s minimum gap between consecutive
greetings (so multiple `greetOnJoin` agents don't all shout hi at
once). That per-space serialization is process-local; cross-replica
exactly-once is enforced by `dispatchGate.tryGreet` (#1386), the
same Postgres advisory-lock gate as the utterance dispatch path
(`tryDispatch`) but keyed on (space, agent) under a distinct lock
class -- so in a 2-replica deployment exactly one replica posts the
greeting instead of both. The greeting directive is "familiar" by
default for ALL agents -- every agent in the product is one the user
created and named themselves, so the directive forbids the
"Hi, I'm X" opener across the board.

### Agent reply envelope (`respondToUser`)

Every user-facing chat reply from an agent is delivered through a
single structured-output envelope, not free-form prose. The agent
ends every turn with a sentinel `respondToUser` tool call carrying
`{response, citations[]}`; the streaming tool loop intercepts the
call by name (no engine executor exists for it), parses the args as
`Envelope`, and uses that as the turn's final text + citations. See
`integrations/agent/envelope.go` for the schema and
`integrations/agent/streaming.go` for the interception path. The
prompt enforces it via the OUTPUT CONTRACT block at the top of
`dsl/cognition/prompts/cognitionReply.tmpl`.

`citations` is a list of `{domainId, matchedPhrase}` pairs naming
knowledge-domain sources the agent drew from; cognition stamps them
on the inserted `v1:cognition:utterance.citations` field via the
`AgentTurnCitation` proto on `AgentGenerateTurnComplete`. The
frontend wraps each `matchedPhrase` substring of the rendered text
with a clickable chip linking to the named knowledge domain. When
the agent used no trained sources, citations is an empty array.

### Coding Agent (OpenClaw / NemoClaw) -- a SEAM, not a running deployment

**Nothing in this repository runs a coding agent.** What exists is the
extension point one would plug into, plus the graph fields and display
strings that assume it. This section says which is which, because it
previously read as a description of a shipped deployment and named an AKS
sidecar no manifest implements (memql#4120).

What is IN the tree:

- **The seam.** `component/planner`'s `RegisterContainerExecutor(name,
  exec)` is the registry a container-executor backend self-registers into
  from `init()`; a Task routes to it by `executionSurface="containerExecutor"`
  + `executorBackend` (`dsl/planner/concepts.memql`). `"nemoclaw"` is the
  name the doc comments reserve for the first backend. **No package in this
  repo calls `RegisterContainerExecutor`**, so the registry is empty at
  runtime and a `containerExecutor` Task has nowhere to land.
- **The agent flag.** `v1:agents:agent.claw` (bool) + `clawWorkspace`
  (`dsl/agents/concepts.memql:46-47`), read by
  `integrations/cognition/ai_responder.go`'s `ClawCapable()`. The
  per-agent workspace convention `/workspaces/{agentId}/` is stated in
  `component/planner/executor.go`, not implemented here.
- **Display strings.** `integrations/cognition/tool_labels.go` formats
  progress labels for `clawExecuteTask` / `clawReadFile` / `clawListFiles`
  / `clawSearchCode`, and two cognition prompt templates name them as an
  example of a capability gap. Formatting a label for a tool does not
  define it.

What is NOT in the tree, despite having been claimed here:

- **No sidecar, cloud or local.** `deploy/k8s/base/agent.yaml` has exactly
  one container (`agent`); `grep -ri 'nemoclaw\|openclaw' deploy/` is empty.
  The parity-cluster sidecar was tracked in memql#1310 (now closed); the absence of any manifest here is tracked in memql#4120.
- **No tool definitions.** No `tool claw*` exists anywhere under `dsl/`.
  A product could ship them in its own bundle at `MEMQL_DSL_PATH`; the
  engine does not. (`component/memql/tool_claw_test.go` asserted they load
  and is `t.Skip`ped -- it covers the retired `dsl/v1` tree.)
- **No image, pin, or gateway config.** There is no OpenClaw/NemoClaw image
  reference, version pin, or gateway/network setting in this repo, so the
  hardening posture that used to be described here (version pin, internal-
  only gateway, disabled community skills) documents nothing that exists.
  Whoever registers the first backend owns re-establishing it.
- **No env vars.** No `NEMOCLAW_*` / `OPENCLAW_*` / `CLAW_*` name is read by
  any Go code or registered in `component/envregistry/manifest.yaml`.

### Workers (computer_use_headless / computer_use_embodied)

The "workers" feature lets agents drive the user's own machine
via a tool surface: shell exec, filesystem, HTTP fetch, and (under
the computer-use build) mouse + keyboard + screenshot. Runbook:
[docs/public/operate/workers-runbook.md](docs/public/operate/workers-runbook.md).

The capability is split into two mode-specific slugs so the headless
slice (shell / fs / http) and the embodied slice (mouse / keyboard /
screenshot) can be granted independently. Authorization (scope grants,
kill switch, knowledge domain) stays unified -- both modes act on the
user's machine, so the consent is one decision. See
`component/memql/worker_caps.go` for the slug expansion map. The
sandboxed first-choice surface for headless work is the Workbench,
documented in the next section.

- **Agent capabilities (split slugs):**
  - `computer_use_headless` -- expands to `workerHost` + the
    cross-cutting trio (`workerStatus`, `requestComputerUseScope`,
    `canvasPublish`). Shell / fs / http on the user's machine.
  - `computer_use_embodied` -- expands to `workerComputer` + the
    same cross-cutting trio. Mouse / keyboard / screenshot on the
    user's machine.
- **Tools:** `workerHost` (HEADLESS) and `workerComputer` (COMPUTERUSE),
  both discriminated-union tools under the `dsl/worker/` namespace.
- **Gateway:** `WorkerService.Stream` gRPC service on the agent
  node. Auth via worker-specific tokens
  (`mql_wkr_<43 base64url chars>` -- the `worker_token` variant on
  `v1:identity:identity`). The gRPC interceptor admits these
  tokens on the WorkerService path only and rejects them
  everywhere else.
- **Token mint:** server-side via `CreateWorkerTokenMsg` /
  `RevokeWorkerTokenMsg` on `MemqlService.Stream`. The plain
  token comes back in the reply ONCE; only the SHA-256 hash
  persists. Mint via `component/identity/workertoken/` (mirrors
  the `pat` package). The frontend's AddWorkerModal calls these
  directly so plaintext never lives outside the gRPC reply.
- **Worker side:** `memql-cockpit worker run` is a separate run
  mode of the Cockpit binary, built from the `memql-cockpit` repo
  (`make cockpit` / `make cockpit-computeruse`). The computer-use build wraps RobotGo
  for screenshot + mouse + keyboard. macOS TCC / Linux X11 pre-flight
  via `memql-cockpit-computeruse worker setup`.
- **Per-user routing:** every worker is owned by exactly one
  v1:identity:user; agents in that user's sessions are the only
  callers admitted by the registry.
- **Permission model:** three layers checked BEFORE dispatch
  -- agent capability flag, standing scope on
  `v1:agents:agentAuthorization.computerUseScope` (observe /
  interact / full), per-Plan kill switch on
  `v1:identity:user.preferences.computerUseEnabled`. Out-of-scope
  calls transition the calling Plan to `awaitingFeedback` with
  `feedbackReason=scope_elevation_required`.
- **Audit:** security signals on `v1:identity:auditEvent`;
  per-call telemetry on `v1:worker:invocation` with
  `WORKER_INVOCATION_RETENTION_DAYS` default 90.
- **Hardening:** per-call rlimits (`RLIMIT_CPU`, `RLIMIT_AS`,
  `RLIMIT_NOFILE`) on Linux + Darwin via
  `policy.shell.max_*` knobs; optional setuid drop to a
  dedicated user via `policy.shell.run_as_user`. Prometheus
  metrics endpoint at `127.0.0.1:9100/metrics` (loopback-only,
  no auth).
- **Frontend:** `?panel=workers` in the product SPA shows the
  WorkersListPanel; the floating ComputerUseKillSwitch widget in
  the session chrome flips `computerUseEnabled`.
- **Install:** `scripts/install/install-{mac,linux}.sh` install
  the binary, write `~/.memql/worker.yaml`, and register a
  LaunchAgent / user-systemd service.

### Workbench (workbench_use)

The "workbench" is the default first-choice surface for any
HEADLESS work an agent needs to do -- writing files, running
shell commands, fetching URLs. It is a per-Plan sandboxed Linux
working directory in the cluster; the agent drives it, the user
does not see it as a filesystem they can browse, and nothing on
the user's machine is touched. Computer-use (the user's machine)
is the FALLBACK for headless work the workbench cannot do
(macOS-only tooling, computer-use control, files already on the user's
computer).

See [docs/public/operate/workbench-runbook.md](docs/public/operate/workbench-runbook.md) for the
test path and [docs/internal/ops/workbench-production.md](docs/internal/ops/workbench-production.md)
for the cluster-mode deployment detail.

- **Agent capability:** `workbench_use` slug. Universal --
  injected into every role's `lockedToolSlugs` so newly-created
  agents always have it. No scope grants, no kill switch, no
  per-agent gating; the blast radius is contained to the per-Plan
  directory tree.
- **Tools:** `workbenchHost` (discriminated by `action`: exec /
  fs_read / fs_write / fs_list / fs_stat / http_fetch). Lives in
  a product DSL bundle (`MEMQL_DSL_PATH`), not the engine tree; the wire
  path goes through the `workbenchDispatchHost` builtin in
  `dsl/workbench/builtins.memql` to `integration.workbench.dispatchHost`.
- **Per-Plan workspace:** filesystem state lives under
  `MEMQL_WORKBENCH_ROOT/{planId}/` (default
  `/var/lib/memql/workbenches/`). Lazy-provisioned on first call.
  Persists across calls within a Plan so multi-Task agents can
  share files; torn down on Plan terminal status via the
  `releaseWorkspaceOnPlanTerminal` automation calling the
  `workbenchTeardownDirectory` builtin.
- **Concept:** `v1:workbench:workspace` -- per-Plan row carrying
  status (provisioned / released), storageRoot, lifecycle
  timestamps. Defined in `dsl/workbench/concepts.memql`.
- **Modes:**
  - **Cluster mode (the deployed default).** A dedicated `workbench`
    node-type binary (`make workbench`, `deploy/k8s/base/workbench.yaml`)
    hosts the workspaces; agent nodes route via `NodeService.Stream`
    (`WorkbenchForwardRequest` / `WorkbenchForwardResponse`). Base sets
    `MEMQL_WORKBENCH_REMOTE=1` on the agent; the dialer needs
    `MEMQL_WORKER_PEERS=workbench=<addr>`.
    **The remote flag is an ASSERTION, not a preference:** with it set
    and no reachable workbench peer, a workbench call is REFUSED
    (`no_workbench_peer`) rather than run on the agent's own disk. It
    used to degrade silently, which is how a dropped peer seed -- every
    call running on the agent pod -- stayed invisible for its whole life.
  - **In-process fallback.** `MEMQL_WORKBENCH_REMOTE` unset or falsy runs
    the integration on the agent node itself, with workspaces on that
    container's disk. Under the remote flag the same behaviour is its own
    explicit opt-in, `MEMQL_WORKBENCH_LOCAL_FALLBACK=1`, so "run this
    remotely" and "run it here if you must" are spelled differently.
- **Routing preference:** the agent's reply template
  (`dsl/cognition/prompts/cognitionReply.tmpl`) and the workbench
  knowledge domain (5 chunks in
  `integrations/knowledge/seed.go`) instruct the agent to prefer
  workbench over computer-use whenever both are available, and
  to surface a "workbench can't do this -- needs computer use"
  message when it hits a Linux/macOS or sandbox/host limitation
  rather than silently retrying.
- **Knowledge domain:** `workbench` -- auto-attached via
  `replier.go` when the agent's expanded tool list includes
  `workbenchHost`. Treated as a system-owned domain (no audible
  citations) per `appStructureDomainIds`.

---

## Authentication

The in-house **identity service** (`component/identity`) is the
authentication provider for the cluster. It runs as its own
node-type binary (`make identity`) and owns:

- Magic-link auth as the primary login path, **device-bound and
  approve-on-click** (epic memql#4300). Issue mints a 32-byte nonce alongside
  the token, stores only its digest as `magicLinkRequest.bindingHash`, and
  hands the plaintext to the requesting browser as `memql_ml`
  (`HttpOnly; Secure; SameSite=Lax; Path=/auth`). A link only COMPLETES in a
  browser holding that cookie; a click anywhere else only APPROVES the
  request, and the requesting tab -- sitting on `/check-email` -- polls, sees
  `approved`, and finishes itself. **A session can only ever land on the
  device that asked for it: if B clicks A's link, A signs in and B gets
  nothing.** That closes the group-alias race, where whoever read a shared
  mailbox first got the session -- on the identity path, a first-party cookie
  with no PKCE and no device check, enough to enrol their own passkey and hold
  permanent, mailbox-independent access A could neither see nor revoke.
  `GET /auth/complete` renders and writes nothing (so prefetchers are
  harmless), and consume is a compare-and-swap under a Postgres advisory lock
  -- load-bearing, because approve-on-click gives one request two legitimate
  finishers. What it does NOT fix is a colleague requesting their OWN link to
  the same mailbox; `signInPolicy` below is the answer to that.
- `sharedMailbox` + `signInPolicy` on `v1:identity:user` (memql#4304). The
  first is a hint set by a local-part heuristic
  (`component/identity/registration/shared_mailbox.go`) that gates nothing and
  drives copy. The second is `any` (default) or `passkey_only`, which disables
  sign-in LINKS: a request writes no row, sends no link, redirects identically
  (no enumeration signal) and mails a notice instead. Enabling it requires an
  active passkey, server-enforced. Owners and admins can RESET it to `any`
  over `IdentityAdminMsg` -- one direction only, so an admin cannot lock a
  colleague out of their own account.
- A new-sign-in email on every `authSession` row, fired from the one seam that
  creates them (memql#4305). No action link, deliberately: an unauthenticated
  revoke link mailed to a shared mailbox is a denial-of-service handle for
  everyone who can read it. Refresh rotations never send it.
- WebAuthn passkey **registration** (`POST /auth/webauthn/register/{begin,finish}`,
  memql#3406). Ceremony logic in `component/identity/webauthn/`; the RP id
  derives from `MEMQL_IDENTITY_BASE_URL`, challenges are single-use and
  TTL'd, and credentials are minted `residentKey=required` /
  `userVerification=required` so they are discoverable. Enrolment
  authorization is memql#3408.
- WebAuthn passkey **login** (`POST /auth/webauthn/login/{begin,finish}`,
  memql#3407). Usernameless: the challenge carries an EMPTY
  `allowCredentials` (no email has been typed) and resolves the assertion to
  a row by credential id alone, which is why that id is unique cluster-wide.
  The challenge also carries the in-flight OAuth context -- the same
  `clientId` / `redirectURI` / `state` / `codeChallenge` /
  `codeChallengeMethod` that `IssueMagicLink` stamps on a magic-link row --
  validated at begin and held server-side, so `finish` mints an auth code
  via `Store.CreateAuthCode` and returns the same client callback target a
  magic-link click produces. **No client learns which factor ran**: what
  reaches `/oauth/token` is an auth code, PKCE binding intact. A sign-count
  regression is refused and audited as the cloned-authenticator signal (a
  zero counter from an authenticator that does not implement one is not
  that case). The `/login` page carries a "Sign in with a passkey" control
  as a progressive enhancement; the magic-link form remains the path when
  no passkey exists, WebAuthn is unavailable, or no relying party is in
  scope.
- Passkey **management** on `/me/devices` (memql#3409): list (label,
  added, last used, AAGUID-derived model, and the backup posture that
  says whether losing the device loses the credential), rename, revoke,
  and enrol another via the #3406 ceremony. Revoke is a SOFT delete
  (`active=false`) -- the row is audit history and its credential id must
  stay taken, because revoking a row does not make the authenticator
  forget its private key. A revoke that would leave the account with NO
  sign-in route (no `magic_link` identity, no other passkey) is warned
  about explicitly before it happens; `component/identity/web/me_passkeys.go`
  resolves the target out of the caller's OWN self-scoped list, which is
  the ownership check, while the write runs under the system credential
  actor the memql#2513 guard requires.
- **Enrolment tokens + `GET /enroll`** (memql#3408) -- the task that removes
  email from the critical path. `mql_enr_<43>` (32 CSPRNG bytes, SHA-256 hex at
  rest, plaintext never persisted or logged), single-use via a `consumedAt`
  stamp, 15-minute default TTL capped at 24h. It authorizes exactly ONE action:
  register a passkey as the named user. `/enroll` validates and renders the
  registration page; the ceremony that follows presents
  `Authorization: Enrolment <token>` (the `/pair/redeem` shape). The four
  rejection states -- invalid / expired / already-used / revoked -- each render
  their own message, because each asks the holder for a different next step.
  Package: `component/identity/enrolment/`. Issued by an owner/admin from the
  portal's People surface over `IdentityAdminMsg` (gate in
  `component/identity/adminops`), or by the install wizard's `enrolmentLink`
  graph step via `memql enrolment-token mint` inside the identity pod -- which
  is the only authority available at that moment, since nothing can authenticate
  to a cluster whose owner has just been bootstrapped from env.
- OAuth-style token endpoints (`/oauth/token`, `/auth/refresh`).
- The JWKS feed at `/.well-known/jwks.json`.
- A public web UI (`/login`, `/auth/complete`, `/setup`,
  `/legal/*`, `/me/*`).
- What remains of the admin web app at `/admin/*`: the sign-in pages,
  and an `/admin/` root that answers `410 Gone`. The admin screens live
  in the MemQL portal; their owner/admin gate is
  `component/identity/adminops`, riding `IdentityAdminMsg` on
  `MemqlService.Stream`. `DeployControlService` shells out against an
  on-disk overlay checkout and so exists only on the identity node, but
  a bff FORWARDS the deploy RPCs here over `NodeService.Stream`
  (`DeployControlForwardRequest` / `Response`), carrying the caller as a
  verified `ForwardedAuthority` so the owner-only rollback and repair gates
  run against the originating human rather than the relaying node
  (`component/grpc/deploy_control_forward.go`). Repair (memql#4209) is an
  owner-only, observed re-sync of the installation's ArgoCD Application
  through the same Executor, recorded on the deployment timeline
  (`component/deploycontrol/repair.go`).
- Personal Access Token (PAT) issuance for CLI clients
  (`mql_pat_<...>`).

Other binaries (bff / voice / cognition / agent / planner / workbench /
mcp) verify identity-issued JWTs locally via the per-node verifier
(`component/identity/verifier`), which fetches the JWKS document
on a 5-min background refresh and on demand for unknown `kid`
headers. They never see the private key.

`MEMQL_IDENTITY_VERIFIER_BASE_URL` configures the verifier;
`MEMQL_IDENTITY_BASE_URL` configures the identity service itself. See
[docs/public/operate/auth/identity-service.md](docs/public/operate/auth/identity-service.md) for
the operator-side narrative.

**Authentication is ON by default everywhere** (local and cloud alike -- env
parity). The master toggle is `MEMQL_IDENTITY_ENABLED`:
on verifier-consuming nodes it defaults to `true` (auth enforced) and is set
explicitly `false` ONLY to disable auth for troubleshooting -- the node then
skips the verifier and admits every stream as a synthetic `local-dev` cluster
owner (see `component/grpc/local_dev_stream_interceptor.go`), with a loud
boot-time SECURITY warning and the `memql_auth_enabled` gauge pinned to 0. The
toggle is a config value present everywhere, never an architecture branch;
**never set it false in a cloud cluster.** Disabling auth is the toggle, NOT
blanking `MEMQL_IDENTITY_VERIFIER_BASE_URL` (an empty verifier URL fatals the
node).

See [docs/public/operate/auth/](docs/public/operate/auth/):
- [access-model.md](docs/public/operate/auth/access-model.md) -- enforcement
  layers and role spectrum.
- [user-provisioning.md](docs/public/operate/auth/user-provisioning.md) --
  registration modes and magic-link flow.
- [identity-service.md](docs/public/operate/auth/identity-service.md) --
  operator-side env vars + key management.
- [operator-credential.md](docs/public/operate/auth/operator-credential.md) --
  `MEMQL_OPERATOR_KEY`, the `Authorization: Operator <key>` bearer token that
  admits a stream as a synthetic cluster owner so tooling can reach a cluster
  before any user exists. **A different secret from `MEMQL_MASTER_KEY` since
  memql#3519**: the master key DECRYPTS, the operator key AUTHENTICATES. They
  were one value, which made a key the installer wrote into a world-readable
  `~/.bashrc` (and ESO delivers to production pods) a cluster-owner bearer
  token over the network. No fallback -- a cluster that has not been seeded
  `MEMQL_OPERATOR_KEY` refuses operator streams rather than accepting the old
  one. Rotation sequencing for both keys lives here.
- [recovery-key.md](docs/public/operate/auth/recovery-key.md) -- the owner
  BREAK-GLASS credential (epic memql#3958): `mql_rec_<43>`, SHA-256 at rest,
  bound to one owner, minted automatically with its plaintext never logged, and
  claimed on demand from inside the identity pod. It authorizes exactly one
  action -- register a passkey as that owner -- and is REFUSED while the owner
  still holds a usable sign-in route, which is what keeps it a break-glass key
  rather than a second password. Single-use: redeeming spends it and mints an
  unclaimed successor in the same breath, so a leaked key is worth one passkey
  registration and the cluster is never without a route back in. What replaced
  the sealed genesis envelope's one irreplaceable job, without a second bearer
  that also decrypts config.
- [service-account-jwt.md](docs/public/operate/auth/service-account-jwt.md) --
  the `class="service_account"` machine identity (#691): the deploy
  gate / automation credential that verifies on the BFF/mesh via
  JWKS (where a PAT can't), surface-pinned to the read/query path.
  Mint -> verify -> gate-usage, with diagrams.

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
*downward* on the layers above it; cycles are rejected at load time.

```
  ┌─────────────────────────────────────────────────────────────────┐
  │  Concepts                                                       │
  │  schemas + reserved intrinsics. The base of everything.         │
  └─────────────────────────────────────────────────────────────────┘
        │           │           │             │
        ▼           ▼           ▼             ▼
   ┌─────────┐ ┌─────────┐ ┌──────────┐ ┌────────────┐
   │ Shapes  │ │Mutations│ │ Builtins │ │ Providers  │
   │ @row /  │ │ inserts │ │ Go-backed│ │ AI vendor  │
   │ @actor  │ │ on rows │ │ executors│ │ + model    │
   │ + traits│ │         │ │          │ │            │
   └────┬────┘ └─────────┘ └──────────┘ └────────────┘
        │                                       │
        ▼                                       ▼
   ┌─────────┐                              ┌────────┐
   │  Specs  │                              │Prompts │
   │signature│                              │tmpl +  │
   │predicate│                              │schema  │
   └────┬────┘                              └────┬───┘
        │                                        │
        ▼                                        │
   ┌─────────┐                                   │
   │ Queries │                                   │
   │ filter+ │                                   │
   │ shape   │                                   │
   └────┬────┘                                   │
        │                                        │
        └────────┬───────────────────────────────┘
                 ▼
           ┌────────────┐    ┌────────────┐
           │ Automations│    │   Tools    │
           │ event →    │◄───┤ AI-callable│
           │ side-effect│    │ definitions│
           └────────────┘    └────────────┘
                 │
                 ▼
           ┌────────────┐
           │  Policies  │
           │ provider-  │
           │ selection  │
           └────────────┘
```

**How to read this:**

- **Concepts** are pure schema. Every other construct references one
  or more concept ids.
- **Shapes** are reusable field-projection templates. Every shape
  declares its kind via `@row` (concept payload + row intrinsics; the
  concept is named by the `shape <Concept> <name>` signature) and/or
  `@actor` (engine envelope, no signature concept). Trait shapes are
  `@row` shapes signature-bound to a generic trait concept —
  scaffolds for cross-concept predicates (`activeRowTrait`,
  `statusRowTrait`, etc.). Shapes have no composition verb -- to share
  a projection, repeat the paths or take the default projection.
- **Specs** are atomic boolean predicates. A spec **binds one shape XOR
  concept in its signature** (`spec <boundName> <name>`) and the body
  `return`s a boolean over bare field names. The binding picks the eval
  strategy (epic #2281):
  - concept or `@row` shape binding → row-spec, compiles to a SQL
    `WHERE` fragment.
  - `@actor` shape binding → context-spec, evaluates in-process against
    the auth envelope (named as a bare conjunct, e.g. `requiresAdmin`).
  A spec body never reads `actor.*` / `row.*` directly. A `trait` is the
  one deliberately-unbound row predicate (bare payload fields).
- **Mutations** write to concepts via the bare `insert { ... }` /
  `update { ... }` block (target from the signature). One write per
  body.
- **Builtins** wrap Go integrations behind a declarative schema, so
  they look like regular DSL function calls.
- **Providers** are AI vendor + model + auth records; **prompts**
  pin a default provider and pull rendered templates over it.
- **Queries** stitch concept + filter (specs) + projection (shapes)
  + args into a typed read. The struct form
  `query NAME { concept ... filter ... shape ... }` is the only
  author-facing shape.
- **Automations** are event-triggered side-effects. They consume
  the layers above them and never the other way around.
- **Tools** are the AI-facing surface of queries + mutations +
  builtins. The tool loop binds tool-call args to handler args and
  forwards.
- **Policies** are empty-bodied AI provider-selection records
  (`@primary` / `@fallback` / `@maxLatencyMs` / `@preferredRole`),
  consumed by the AI Router to resolve a provider chain. Caller-based
  authz / feature-gating decisions are **specs**, not policies.

**Construct files live under `dsl/<namespace>/<construct>s.memql`**
(concepts, specs, shapes, mutations, queries, builtins, providers,
prompts, tools, automations, traits — one consolidated file per
construct kind per namespace; policies are consolidated in
`dsl/policies/policies.memql`).

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
> **The entry point is `dslgate.ScanFiles`, and it takes the whole corpus.**
> Init used to loop `ScanSource` per file, which can express only per-file
> rules -- so the cross-namespace import gate (memql#3803), whose question is
> "where is this name DECLARED", could not be one of them. It lived on
> `ScanTree`, whose only caller was a conformance test, and was therefore
> enforced over this repo's `dsl/` at PR time and over a product bundle
> **nowhere**. memql#4051 ruled that a gap rather than a deliberate exemption:
> boot already holds the merged file set (`baseloader.ReadAll`), so the corpus
> pass adds no I/O, just ~79ms of regex over bytes already read. Write a new
> gate against `ScanFiles`; there is no longer a tier of gate that boot cannot
> reach, and reintroducing one is the mistake to avoid.

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
AI provider configurations (OpenAI, Anthropic -- the only supported
vendors). Struct form, mirrors concepts / shapes / tools.
```memql
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
Base providers (vendor-level auth + type) use the same form:
```memql
@base
@type("OpenAI")
provider openai {
  auth {
    apiKey  env("MEMQL_AI_OPENAI_API_KEY")
  }
}
```

**Lifecycle annotations (`@enabled` / `@disabled`).** Providers accept
the same lifecycle flags as functions / builtins / prompts / specs /
seeds. `@enabled` is the explicit-on default (a no-op). `@disabled`
skips the provider at load -- it is **not registered and no auth
resolution is attempted**, so it emits zero "registered as unavailable"
warnings while staying in the tree for a future re-enable. `@disabled`
on a `@base` **propagates**: every child that `@extends` it is skipped
too. Use it to turn a keyless vendor lane off cleanly (mark the `@base`
`@disabled` until its `MEMQL_SI_*_API_KEY` is seeded). Dependents
degrade gracefully -- a
policy whose `@primary` is disabled routes via its `@fallback`; a prompt
whose `@defaultProvider` is disabled falls back to the default.

```memql
@disabled
@base
@type("Acme")
provider acme {
  auth { apiKey env("MEMQL_SI_ACME_API_KEY") }
}
```

> **Semantics of `@disabled` (shared across every construct that takes
> it).** `@disabled` means the construct is **not loaded/active at
> runtime right now**. It does NOT mean the construct is deprecated,
> abandoned, exempt from updates / maintenance / refactors /
> conformance, or that it will not be used in the future. It is a
> reversible on/off switch; disabled constructs are still maintained and
> may be re-enabled at any time. ("Deprecated / abandoned" is a separate
> axis carried by `@deprecated`.) The canonical statement lives in
> `component/language/ast/ast.go` at the `AttrEnabled` / `AttrDisabled`
> const definition.

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
openairealtime, rbac, router, similarity, telephony, timeutil, voice,
workbench, plus node-type-scoped ones (cognition, agent, stt,
openaiVoice) wired explicitly in `app/integrations_*.go` when their
dependencies sit outside the stable `PluginContext` surface. `training`
is a product-repo pack, not part of engine-only core.

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

### Platform Concepts
Platform-level metadata (dsl/platform/concepts.memql)
- `v1:platform:site` -- a hosted web surface; the edge node resolves the request `Host` header to one of these rows and serves its `bundleRef`
- `v1:platform:globalSecret` / `globalVariable` -- cluster-scoped config storage
- `v1:platform:outboundRequest` / `inboundRequest` -- request bookkeeping
- `v1:platform:missingCapability` -- capability gaps recorded at runtime

### Cluster Concepts
Distributed node system metadata (dsl/cluster/concepts.memql)
- `v1:cluster:node` -- Registered node in the cluster
- `v1:cluster:nodeType` -- Node type definition (bff, voice, cognition, agent, planner). Optional `codeReference` field links this row to its architecture-model service id (consumed by the cockpit's Topology drill-down).
- `v1:cluster:spawnEvent` -- Lifecycle event for node state transitions
- `v1:cluster:deployment` -- Append-only deployment record (one timeline per deploymentId; status pending -> in_progress -> succeeded|failed; superseded/rolled_back). The deploy-as-a-pack source of truth for a deploy (#1872)
- `v1:cluster:deploymentNodeSpec` -- Per-node-type spec child of a deployment (Epic 2 / #2094): one append-only timeline per (deploymentId, nodeType) carrying version + replicas + imageDigest. Engine-as-spine: empty `version` resolves against the deployment's engine version; non-empty pins the node type. Read a deployment's full per-node set via `nodeSpecsForDeployment`
- `v1:cluster:cluster`, `v1:cluster:database`, `v1:cluster:identityProvider` -- topology bookkeeping

### Observability Concepts
Runtime side of the architecture framework (dsl/observability/; infrastructure metadata every node loads).
See [docs/internal/design/auto-generated-diagrams.md](docs/internal/design/auto-generated-diagrams.md) for the full design.
- `v1:observability:codeProfile` -- live per-FQN verbosity override. CDC events feed the observe runtime's in-process cache via `CodeProfileSubscriber`.
- `v1:observability:invocation` -- per-call records backed by the `code_invocation` TimescaleDB hypertable.
- `v1:observability:codeMetric` -- per-(FQN, window) aggregates backed by the `code_invocation_1m` / `_1h` continuous aggregates. Drives the cockpit Topology overlay (n / p95 / err% per node). Clients read it through `codeMetricsInWindow` (`dsl/observability/queries.memql`, memql#4208): one bucket, one `[windowStart, windowEnd)` range, `codeReference startsWith` any of the caller's prefixes or equal to an exact key -- the prefix-scoped read the portal's module drill-in uses instead of a capped client-side walk.

### Identity Concepts
Auth + access metadata (dsl/identity/concepts.memql; infrastructure metadata every node loads)
- `v1:identity:user` -- the person; cluster-wide role (owner / admin / developer / writer / reader); preferences (theme, archive retention, daily-space toggle, voice mode, UI-takeover settings)
- `v1:identity:identity` -- a credential set owned by a user (magic-link verified email, oauth token, api key/PAT, service account, worker token, badge, account token, passkey). A discriminated union keyed on `identityType`; the `passkey` variant (memql#3406) is the only one whose stored material is PUBLIC (a COSE key), because possession is proved by a signature rather than by a digest match
- `v1:identity:authSession` -- per-token session record (used for revocation)
- `v1:identity:magiclink` -- single-use magic-link credential (token-hashed)
- `v1:identity:auditEvent` -- append-only audit trail for the identity service: DECISIONS and
  SECURITY SIGNALS only since memql#4328 (sign-in, session created/revoked, role change, admin
  action, `refresh_token_reuse_detected`). `action` stays an unconstrained string -- many writers,
  and a closed enum would refuse a new decision at insert time
- `v1:identity:authActivity` -- routine authentication MECHANICS, split out of `auditEvent`
  (memql#4328): refresh-token rotations, the blocked ones, grace-window accepts,
  PAT-authenticated requests. Two orders of magnitude more numerous, so the Trail is clean by
  construction rather than by a filter. Four writers and a CLOSED `action` enum;
  `@rowAuthz(owner="actorUserId", clusterOwner)` -- the first composite tier in the tree, which is
  what lets a person read their own activity and a cluster owner read everyone's. Its
  `retiredTokenHash` is the evidence refresh-token reuse detection keys on (memql#4329), and REAL
  retention applies: `MEMQL_IDENTITY_AUTH_ACTIVITY_RETENTION_DAYS` (default 30), hard-deleted daily
  from Go by `component/identity/authactivity` -- so detection reaches back exactly that far
- `v1:identity:accessRequest` -- waitlist-mode access request
- `v1:identity:invitation` -- token-hashed invitation credential for guest/user flows
- `v1:identity:enrolmentToken` -- single-use, TTL'd credential authorizing exactly one action:
  register a passkey as the named user (memql#3408). What makes a FIRST credential obtainable
  with no mailbox. Mirrors `v1:identity:workerPairingCode` rather than extending `invitation` --
  an enrolment token has no invitee, no inviter to render into a message and no product scope,
  and its single-use marker is a `consumedAt` stamp rather than `respondedAt` + a participation
  status. Same SHA-256-hex hashing convention as every other credential row. Redeemed at
  `GET /enroll?code=...`; issued by an owner/admin over `IdentityAdminMsg`, or by the install
  wizard via `memql enrolment-token mint`
- `v1:identity:delegation` -- agent acting through a user's identity (bounded role/scope/lifetime)

See [docs/public/operate/auth/access-model.md](docs/public/operate/auth/access-model.md) for the full model.

---

## Feature Notes

### Canvas + Spaces

Under platform consolidation (#2472), the space lifecycle (three-state +
daily spaces) is an **engine-generic feature** rather than product code:
spaces are already a core, generic data model (Epic 3), and daily-space is
one of the reusable-capability absorptions. The core
participant/session/utterance machinery is engine-side
(`dsl/cognition/mutations.memql`: joinSpaceAsHuman, leaveSpace,
addAgentToSpace, ...).

The canvas timeline (the `canvasState` concept) is still delivered as
**product DSL** -- supplied at runtime through the product's DSL bundle
(`MEMQL_DSL_PATH`); its physical absorption into the engine is mid-migration,
so treat canvas as product-owned for now. Product rows ride the chat-reply
delivery substrate via `node.RegisterChatReplyConcept`.

### Invitations (Identity Primitive)

Token-hashed invitation credential for user and guest flows. Lives
under `v1:identity:invitation`; product-specific mutations layer on
top (e.g. `sendGuestInvite` in `dsl/cognition/mutations.memql`).

Two gRPC messages drive the guest flow:

- `SendGuestInviteMsg` -- authenticated space owner. Mints a 32-byte
  token, stores only its SHA-256 hash on the `Invitation` record, and
  sends the invitation email via the `email` integration plug-in.
- `ResolveGuestInviteMsg` -- unauthenticated public call from the
  product `/join/<token>` landing page. Returns scope + inviter
  metadata or a typed status (`invalid` / `expired` /
  `already_accepted` / `cancelled`).

Guest authentication is `Authorization: Guest <token>`. The
`NewGuestAwareStreamInterceptor` wraps the identity-verifier
interceptor, validates the token against the invitation registry,
and builds a guest `AccessContext` under the `identity.guest`
claim key (subject
`guest:<invitationId>`; scope carried in claims for downstream
partition checks). The MemQL WS bridge accepts the token as
`?guest_token=<token>` since browsers cannot set custom headers on
the WebSocket upgrade.

Key files:
- `dsl/identity/concepts.memql` -- the identity-owned `invitation` schema.
- `dsl/identity/queries.memql` -- `invitationByTokenHash` + `invitationById`.
- `dsl/identity/shapes.memql` -- the `invitationFull` shape.
- `component/grpc/guest_handlers.go` + `guest_stream_interceptor.go`.
- `integrations/email/` -- self-registering plug-in exposing
  `integration.email.sendEmail`. GraphSender (OAuth client-credentials
  against Microsoft Graph `sendMail`; preferred), SMTPSender
  (fallback), LogSender (dev). Env: `AZURE_TENANT_ID` /
  `AZURE_CLIENT_ID` / `AZURE_CLIENT_SECRET` / `MAIL_SENDER` /
  `MAIL_FROM_NAME` for Graph; `SMTP_HOST` / `SMTP_PORT` /
  `SMTP_USERNAME` / `SMTP_PASSWORD` / `SMTP_FROM_ADDR` /
  `SMTP_FROM_NAME` for SMTP; leave both unset for LogSender.

**The guest write path is ENGINE DSL, split across two domains** (memql#4258).
This section used to say the guest mutations were product-specific and lived
in `dsl/cognition/mutations.memql`. They were in neither place: the five
constructs `component/grpc/guest_handlers.go` names existed in no `.memql`
file anywhere, so on a cluster running the embedded tree every guest-invite
write failed at execute with `function "..." not found`. They are declared
now, and the split follows what each half is about:

| Construct | Where | Why there |
|---|---|---|
| `createGuestInvitation` | `dsl/identity/mutations.memql` | the credential -- beside the `kind="user"` twins `createUserInvitation` / `revokeUserInvitation` and the `invitation` concept itself |
| `markGuestInvitationAccepted` | same | same |
| `markGuestInvitationKicked` | same | also the CANCEL path; a soft cancel, so the tokenHash stays taken |
| `rotateGuestInvitationToken` | same | resend keeps one generation on `previousTokenHash` |
| `createGuestParticipant` | `dsl/cognition/mutations.memql` | the membership -- the SPACE is cognition's, not identity's |

All five are `@serverOnly`: each writes a `tokenHash`, which is the whole
credential the guest-auth interceptor matches on, so a client-reachable
version of the create is a credential-forging primitive. The authorization
that belongs in front of them -- the inviter's relationship to the space, the
guest's valid token -- is held by the handlers, so the boundary sits where the
authorization already lives.

The three update-shaped ones take MINIMAL arguments. Their call sites used to
re-supply every discriminator "so the latest-wins projection keeps the
context", which stopped being true at memql#1628 when `update{}` became a
read-merge. `revokeAuthSession` in the same tree had the identical defect from
two packages -- seven arguments against a two-argument declaration -- and it
survived because an undeclared argument is DISCARDED rather than refused
(`rejectUnknownArgs` is gated behind the MCP boundary).
`component/grpc/render_query_args_parse_test.go` now gates both directions:
every rendered call site resolves through the real front end, and every
argument name it passes must be one the mutation declares.

### Email campaigns + the sending engine

Campaigns are ordinary graph state (memql#3323) plus a Go sending engine
(memql#3348). Seven concepts under `dsl/campaigns/`: five operator-facing
and owned-tier (`audience`, `recipient`, `template`, `campaign`,
`delivery`), two engine-owned and clusterOwner-tier (`sendJob`,
`suppression`).

**The two identities is the design.** A send touches rows belonging to
somebody else, and the engine BORROWS the owner's authority rather than
out-ranking it. `component/campaigns`' drain worker runs its
clusterOwner-tier reads (the job queue, the suppression list) under the
engine's own operator identity, and everything owned -- the campaign, its
template, its audience, the delivery ledger -- under
`auth.ContextWithUserActor(ctx, job.campaignOwnerUserId)`. That owner value
is copied off a campaign row the STARTING CALLER had already read under
their own actor, so it can never name a user the caller could not act as.
Consequence: no path here reads or writes a row the campaign's owner could
not, and `delivery.ownerUserId` is stamped from `actor.userId` like every
other write in the domain -- which is what took the concept off
`ownerGateExemptions`.

**Four product decisions, each recorded next to its code:**
- *Suppression is CLUSTER-WIDE and digest-keyed.* One deployment, one
  sending mailbox, one SPF/DKIM identity, one reputation -- so one list.
  The row id is the SHA-256 of the normalized address and the only readable
  field is the domain, so being cluster-wide discloses no mailbox. Enforced
  at the POINT OF SEND, before the recipient row's own status, which is what
  "outranks every audience" means: a re-imported address whose row says
  `subscribed` is still refused.
- *A hard bounce suppresses; it does NOT delete the membership.* Deleting
  destroys the audit trail and lets the next import resurrect a dead
  address. A soft bounce does neither.
- *Idempotency is the ledger.* One `v1:campaigns:delivery` row per
  (campaign, recipient) at a derived id; the batch is "roster minus ledger,
  plus retries that are due". The absence of the row IS the work queue, so a
  resumed send needs nothing to remember.
- *Two rate limits.* Ours is a per-process token bucket
  (`MEMQL_CAMPAIGNS_SEND_RATE_PER_MINUTE`); theirs is the 429, surfaced as a
  typed `email.SendError` and honoured by parking the job until its
  `Retry-After`.

**RFC 8058 one-click** rides two headers, which forced the Graph sender onto
its base64-MIME form -- Graph's structured payload only carries `x-`
headers, and `List-Unsubscribe` is not one. `GET+POST /unsubscribe` is a
documented HTTP exception (see the table above).

**The unsubscribe token names its key** (`u2.<keyId>.<owner>.<recipient>.<campaign>.<tag>`,
memql#3458), and the node verifies against a ring of two:
`MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET` signs, `..._SECRET_PREVIOUS` only
verifies. The key id is a truncated HMAC **of the key**, not a slot or a
counter -- a link minted today is clicked on a node where that secret has since
become the previous one, so a positional label would be wrong exactly when it
matters. `_PREVIOUS` is a permanent second reader key, NOT a migration window:
an unsubscribe link has no expiry, so an old link keeps working forever until a
SECOND rotation retires the key that signed it. The window is counted in
rotations, not days; rotate at most once for any reason short of key
compromise. The worker warns at boot when a deployment that has already sent
holds only one key.

Not built: an automated warming ramp (needs reputation telemetry that does
not exist) and a scheduler for `scheduledAt`. Runbook:
[docs/public/operate/campaign-sending.md](docs/public/operate/campaign-sending.md).

### Planner / Knowledge / Validation

The schema is stable, so new features add fields/automations without
migrations.

**Concepts**:

- `v1:planner:plan` -- a user-visible unit of work. Carries
  parentPlanId (sub-plan nesting), kind, status (queued / routing
  / running / paused / awaitingFeedback / needsAgent / succeeded
  / failed / cancelled), goal, ownerAgentId, requestedBy,
  triggerSource, recommendationCardId, input / output,
  refinementContext, phases[], estimate, tokenBudget / tokenSpent /
  tokenAllocatedToChildren / tokenCapDisabled, metrics, pause +
  feedback + chat-anchor bookkeeping.
- `v1:planner:task` -- one executable step inside a Plan, never
  recursive. Carries phase tag, executionSurface (inProcess /
  containerExecutor) + executorBackend, metrics, parking fields.
- `v1:agents:agentAuthorization` -- standing tiered-trust authorization.
- `v1:planner:taskState` -- persisted Task working state for
  async parking + planner re-invocation.
- `v1:knowledge:document` -- container/manifest for analyzed user
  files. Owns attached-domain list, validation rollup,
  supersession back-pointers, lazy-embedding status.
- `v1:knowledge:spreadsheetRow`, `v1:knowledge:imageRegion` --
  typed per-format item concepts. Native column predicates
  for spreadsheet rows; bbox + caption + embedding for images.
- `v1:knowledge:validationEvent` -- append-only audit log for
  every validation transition.
- `v1:knowledge:domainEntitySchema` -- per-domain entity schema
  for cross-file dedup. Inferred on second-Document trigger;
  user-confirmed once.
- `v1:knowledge:entityIndex` -- the dedup lookup table keyed by
  sha256(normalized key field values). Force-add escape
  valve for entity-schema misfires.

`v1:common:knowledgeDomain` carries scope (workspace / private) +
ownerId; `v1:common:documentChunk` carries documentId +
validationStatus.

**Analysis path.** The attachment HTTP handler creates the queued Plan +
`plan.created` card synchronously, then runs extract + summarize +
`CompleteAnalyzePlan` on a detached goroutine with a background context, so
the user gets instant acknowledgement and the `plan.completed` card lands
when the work finishes (`runAnalysisAsync` in
`component/server/attachment_handler.go`). A heuristic estimate is stamped
on `Plan.estimate` at creation time so the card's estimate strip renders
immediately; `historicalPlanMetrics` backs the blending logic.
`cascadeSupersession` + `cascadeValidationToItems` propagate
Document-level validation transitions to predecessors and per-row items.

**Planner Agent loop.** The planner-node-owned decompose loop
(`integrations/planner/agent_loop.go`) invokes the `plannerAgent` prompt
on a new userGoal Plan; the prompt emits a structured decision (decompose
/ dispatchTask / createSpecialist / markPlanSucceeded / escalate) and the
loop dispatches it, re-invoking until terminal.

**The cost-safety structure around that loop is the part to respect.**
It is defense in depth and every layer is load-bearing:

- A hard process-wide LLM rate ceiling and an identical-request circuit
  breaker at the provider HTTP chokepoint (`component/memql/ai_guard.go`).
- A CUMULATIVE per-plan token/call budget checked before every
  `plannerAgent` call, persisted so it survives cycles and retries; on
  exceed the Plan parks rather than making another call
  (`component/planner/budget.go`, `integrations/planner/agent_loop_budget.go`).
- Complexity triage that routes a trivial deliverable to ONE cheap path
  instead of the decompose loop; model tiering that defaults to a cheap
  tier and escalates only on an explicit stuck signal.
- An up-front token estimate + user-approval gate that parks an expensive
  plan before it spends, gated specialist creation/training, phased
  execution with per-phase checkpoints, deterministic-first result
  verification, and a no-task-`markPlanSucceeded` convergence guard.

Read [docs/public/ai/llm-cost-control.md](docs/public/ai/llm-cost-control.md)
before touching any of it. `produceArtifact` (the conversational "make me a
file" deliverable) rides the unified loop rather than a bypass: triage
recognizes it as a known single deliverable and shortcuts to ONE direct
production turn (`startPlanDirect` -> running -> the owning agent writes the
file via the workbench), with the rate ceiling, caps and tiering as
structural backstops. An earlier hardcoded bypass was reverted precisely
because those backstops did not yet exist -- do not reintroduce one.

## Need Help?

1. **Documentation:** Check [GLOSSARY.md](GLOSSARY.md)
2. **Quick start:** See [docs/public/overview/quickstart.md](docs/public/overview/quickstart.md)
3. **Logs:** `kubectl logs -n memql deploy/<node> -f`

---

## Notes for Claude Code CLI

- Several directories carry their own CLAUDE.md, and it is the first thing
  to read before editing that tree: `component/`, `component/language/`,
  `component/node/`, `component/architecture/`, `component/observe/`,
  `integrations/`, `sdk/go/`, `docs/`. This is a list, not a rule -- a
  directory without one is normal, and the root claim used to read as
  though every directory had one (memql#4121).
- Use GLOSSARY.md to find specific documentation
- The local k3d cluster is self-contained (`make up`; no manual setup needed)
- Migrations run automatically on startup

### Makefile + shell-script convention

The Makefile is for **simple commands and target wiring**. Anything
multi-step, conditional, or long enough to need line-continuations
gets extracted into a shell script under `scripts/` and the
Makefile target becomes a one-liner that calls it.

Concretely:

- **Stays inline in the Makefile:** single commands (`go build`,
  `go test`, `kubectl rollout restart`), short pipelines (~3 lines or
  fewer), `.PHONY` declarations, target dependencies, simple
  variable substitutions like `make secret-set NAME=... VALUE=...`.
- **Goes into `scripts/<area>/<name>.sh`:** anything with
  conditionals, retry loops, multi-step orchestration, friendly
  user-facing error messages, or "complex enough that you'd want
  to test it independently of make."

Shell-script rules (per the global convention in
`~/.claude/CLAUDE.md`, applied here):

- `#!/usr/bin/env bash` shebang, `set -euo pipefail` at the top
  (drop `-e` for status-reporter scripts where individual
  failures shouldn't abort the rest).
- **Function-based structure** -- one function per responsibility,
  `main()` at the bottom calls them in order. No long sequential
  blob of commands.
- **Source a shared `scripts/<area>/*.sh` helper** for common
  functions (`check_docker`, cluster waits, etc.) so individual
  scripts stay focused.
- File extension `.sh`, executable (`chmod +x`).
- Named "shell scripts" in docs (the umbrella term); they're
  technically Bash scripts since we use `[[`, arrays, `function`
  keyword, etc.

Current example: `scripts/k3d/{up,dev,status}.sh` implements
`make up`, `make dev`, and `make status`. The Makefile targets are
one-liners (`bash scripts/k3d/up.sh`).

#### Capability scripts (the hardened successor)

A **capability script** is a deploy/ops script that is also the
deterministic backend behind a DSL `action` -- so it must run
**identically** whether an automation/action executor or a human
invokes it. These scripts adopt the **capability-script contract**
([docs/internal/design/capability-script-contract.md](docs/internal/design/capability-script-contract.md),
#2221), which is the function-based convention above **plus**:

- **non-interactive** -- no `read -p` / `select` prompts; a
  destructive confirmation is an explicit `--confirm=<phrase>` param,
  never a blocking prompt;
- **structured params in** -- `--flag=value` > stdin JSON
  (`--params-stdin`) > documented defaults (cap_param has no env tier;
  a script passes an env-resolved value as the default); no positional args;
- **structured result out** -- exactly one JSON envelope on **stdout**,
  all human logs on **stderr**;
- **honest, stable exit codes** (0 ok; 2 bad param; 3 refused; 4
  prerequisite missing; 5 op failed);
- **no decisions inside** -- no branching on environment/version/role
  (that lives in DSL `logic`); only mechanical idempotency branches.

They `source scripts/lib/capability.sh` (the shared runtime:
`cap_init` / `cap_param` / `cap_ok` / `cap_fail` / `cap_info`-to-stderr
/ `--print-spec`). The reference implementation is the `scripts/k3d/*`
engine-local path; `scripts/lib/capability_contract_test.go` enforces
the contract on every script that sources the library (and gates
non-interactivity across `scripts/{k3d,deploy,release}`). The
Go effect seam parses the envelope via
`deploycontrol.ParseCapabilityResult`. Use this contract for any new
script a DSL `action` will drive.

### Documentation Style Guidelines

**No Emojis:** All documentation, skills, and CLI responses must use professional formatting without emojis. Use:
- Checkboxes: `[ ]` for unchecked, `[x]` for checked
- Text indicators: "SUCCESS:", "ERROR:", "WARNING:", "INFO:"
- Standard markdown formatting for emphasis
- This applies to: documentation files, skill outputs, CLI responses, and all user-facing text
