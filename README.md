<p align="center">
  <img src="assets/memql-lockup.png" alt="memQL" width="500">
</p>

<h1 align="center">memQL</h1>

<p align="center">
  <strong>The AI memory platform: agents, automations, and voice on a time-series memory graph.</strong><br>
  Unifies concepts, queries, agent workflows, and voice into deployable primitives.
</p>

<p align="center">
  <a href="https://github.com/znasllc-io/memql/actions/workflows/ci.yml"><img src="https://github.com/znasllc-io/memql/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/znasllc-io/memql?color=blue" alt="License"></a>
  <img src="https://img.shields.io/github/go-mod/go-version/znasllc-io/memql" alt="Go version">
  <img src="https://img.shields.io/github/last-commit/znasllc-io/memql" alt="Last commit">
  <a href="https://goreportcard.com/report/github.com/znasllc-io/memql"><img src="https://goreportcard.com/badge/github.com/znasllc-io/memql" alt="Go Report Card"></a>
</p>

<p align="center"><sub><em>Designed and built with Claude as co-author.</em></sub></p>

> **Status: Alpha / pre-1.0 — not production-ready.** memQL is under active development. The DSL, engine API, and wire surface are still evolving; expect breaking changes between commits. Suitable for experimentation, prototyping, and early-design feedback today.

---

## What is memQL?

memQL is a distributed AI platform built on a time-series memory graph, with its own DSL — a single language for declaring concepts (schemas), queries, mutations, tools, and event-driven automations side-by-side, then executing them across specialized nodes.

It replaces the integration glue AI-native teams typically hand-write — vector store + workflow engine + AI gateway + voice stack — with one deployable primitive. A team that would otherwise stitch together four systems can declare an agent's memory, behavior, and triggers in one DSL file and run them on a memQL cluster.

## Why memQL?

Agent and voice deployments today are integration-heavy. Most of the engineering effort is plumbing — keeping state consistent across a vector store, an orchestrator, a tool registry, and a model provider. memQL collapses that plumbing: concepts and queries live in the same place; tools, automations, and workflows reference them directly; the engine handles consistency, time-series storage, and execution.

## Example

A concept (schema), a query over it, and an LLM-callable tool wired to that query — the same shape every real domain in `dsl/` uses (this one is trimmed from `dsl/todos/`):

```memql
@namespace("todos")
@description("A user-owned to-do item.")
@rowAuthz(owner="ownerUserId")
concept todo {
  ownerUserId  string!
  title        string!
  done         bool  @default("false")
}

@actor
@description("List the caller's to-dos, optionally filtered by completion.")
query todo todos {
  args {
    done  bool
  }
  filter  ownerUserId==actor.userId && when(args.done) { done==args.done }
}

@handler(type="query", query="query todos(done: $args.done)")
@executionTime("fast")
@description("List the caller's to-dos.")
tool todosList {
  done  boolean  @description("Filter by completion: true for done, false for open. Omit for everything.")
}
```

`@rowAuthz(owner="ownerUserId")` makes the query's `ownerUserId==actor.userId` filter a load-time-enforced authorization tier, not just a convention — a caller can never read another user's rows through this query. Add mutations or event-driven automations right next to them, in the same file family.

---

## Quick Start

```bash
# Start the local cluster (k3d + ArgoCD)
make up

# Run tests
make test
```

**Full setup guide:** [docs/public/overview/quickstart.md](docs/public/overview/quickstart.md)

---

## Documentation

- **[CLAUDE.md](CLAUDE.md)** - Project overview and architecture
- **[docs/public/overview/quickstart.md](docs/public/overview/quickstart.md)** - 5-minute setup guide
- **[GLOSSARY.md](GLOSSARY.md)** - Complete documentation index
- **[docs/public/overview/tech-stack.md](docs/public/overview/tech-stack.md)** - Tech stack and deployment practices

---

## Tech Stack

### Backend
- **Language:** Go 1.26.1+
- **Database:** PostgreSQL 16 + TimescaleDB
- **API:** gRPC (primary) + WebSocket bridge for browsers + HTTP for OAuth callbacks / health / file uploads
- **AI:** Centralized provider system (OpenAI, Anthropic) on `MemqlService.Stream`
- **Auth:** in-house identity service (magic-link + JWT, JWKS-published)

### Query Language
- **MemQL DSL:** Custom query language for time-series graphs
- **Constructs:** Concepts, queries, mutations, shapes, specs, tools, prompts, automations -- declared in `.memql` files under `dsl/<namespace>/`
- **Automations:** Event- and schedule-triggered workflows

---

## Environments

memQL ships **one installation shape**: an operator who wants a second
environment installs a second instance, with its own domain and its own
ArgoCD — there is no staging-versus-production dimension inside the product.
What varies between a local dev cluster and a cloud install is the deploy
**target**, not the architecture:

| Target | Database | Service | Access |
|---|---|---|---|
| **Local** (`make up`) | self-hosted CloudNativePG in k3d | k3d + ArgoCD, reconciled from `deploy/k8s/overlays/local` | all developers |
| **Cloud** | self-hosted CloudNativePG (same operator + manifests as local) | Azure Kubernetes Service (AKS), reconciled from `deploy/k8s/overlays/cloud` | per the cluster's own role model |

Both targets run the **same self-hosted CloudNativePG database on the same
manifests** — the local/cloud split is DNS, TLS source, and secrets
provisioning, never the shape of the system. See
[docs/public/operate/database-platform.md](docs/public/operate/database-platform.md).

**Full details:** [docs/public/overview/tech-stack.md](docs/public/overview/tech-stack.md)

---

## Development

### Prerequisites

memQL development runs on **both Linux/amd64 and macOS/Apple Silicon** —
the local cluster's prerequisites (`docker`, `k3d`, `kubectl`) have no
platform-specific step on either. Linux/amd64 is a fully supported target
in its own right (`scripts/install/detect.sh`'s `SUPPORTED_OS`/`SUPPORTED_ARCH`
pair for the one-command installer), not a fallback from macOS.

**Software:**
- Go 1.26.1+
- Docker (Docker Desktop on macOS; the Docker Engine on Linux)
- k3d + kubectl (`brew install k3d kubectl` on macOS; your distro's package
  manager or the upstream install scripts on Linux)
- Azure CLI (`az`) — for cloud deploys only
- git

### Local Development Workflow

1. **Clone repository**
   ```bash
   git clone https://github.com/znasllc-io/memql.git
   cd memql
   ```

2. **Start the local cluster**
   ```bash
   make up
   ```

3. **Make changes and test**
   ```bash
   # Edit code
   # ...

   # Rebuild + reload the changed node into k3d
   make dev

   # Run tests
   make test

   # View logs
   kubectl logs -n memql deploy/bff -f
   ```

4. **Exercise the 2-replica parity cluster** for anything cross-node
   ```bash
   make up SERVERS=2 && make scale N=2 && make status
   ```

5. **Branch, PR, merge queue.** `main` refuses direct pushes — a
   repository ruleset enforces `pull_request` + `required_status_checks` +
   `merge_queue`, so `git push origin main` fails no matter how small the
   change. Stage by explicit path, then branch, push, and open a PR as
   usual; once CI is green, enqueue it:
   ```bash
   git add path/to/changed.file
   git commit -m "domain: imperative subject"
   gh pr merge <n> --repo znasllc-io/memql   # bare: enqueues into the merge queue
   ```

---

## Project Structure

```
memQL/
├── main.go              # Entry point (thin orchestrator)
├── app/                  # Phased service bootstrap
│   ├── app.go            # Build() orchestrator
│   ├── config.go         # Config + auth
│   ├── database.go       # Database + concepts
│   ├── engine.go         # Engine + bus + automations
│   ├── integrations.go   # Integration providers
│   ├── transport.go      # gRPC + HTTP + WebSocket
│   └── cluster.go        # Distributed node bootstrap
├── component/            # Core Go service components
│   ├── memql/            # Core query engine
│   ├── database/         # Database providers
│   ├── server/           # HTTP/WebSocket servers
│   └── auth/             # Authentication
├── integrations/         # External service integrations
│   ├── cognition/        # AI collaboration
│   └── voice/            # Voice + video pipeline (LiveKit room, avatar)
├── clients/              # Surfaces built ON the platform (SPAs, portal)
│   └── portal/           # memQL Portal -- the platform's ops console
├── dsl/                  # The MemQL DSL tree (one directory per namespace)
│   ├── cognition/        # e.g. concepts.memql, queries.memql, mutations.memql,
│   │                     #      tools.memql, automations.memql, ... per namespace
│   ├── identity/
│   ├── _reference/       # Authoring reference skeletons (not loaded)
│   └── ...
├── core/                 # Shared utilities (logger, env, id, dslfs)
├── cmd/                  # Command-line tools (memqllint, memqlfmt, memqlmigrate, ...)
├── scripts/              # k3d bring-up, deploy, release, install, migrations
├── sdk/                  # Generated client SDKs (Go, TS)
├── docs/                 # Documentation
│   ├── public/           # Published docs (overview, concepts, language, ai,
│   │                     #      build, operate) -- rendered on memql.io
│   └── internal/         # Design records, plans, internal runbooks
├── deploy/k8s/           # Kustomize manifests (base + overlays/local|cloud)
└── .claude/              # Configuration
```

---

## Common Commands

| Task | Command |
|------|---------|
| **Start local cluster** | `make up` |
| **Tear down cluster** | `make down` |
| **Inner-loop rebuild + reload** | `make dev [NODE=<type>]` |
| **Run Go test suite** | `make test` |
| **Run tests with coverage** | `make test-cover` |
| **DSL lint** | `make dsl-lint` |
| **Break-glass deploy** | `make deploy VERSION=X` |
| **View pod logs** | `kubectl logs -n memql deploy/<node> -f` |
| **Database shell** | `psql postgres://memql:memql_dev@localhost:5432/memql` |

---

## Authentication

Every environment authenticates against the in-house **identity
service** (`component/identity`):
- Magic-link sign-in (no passwords)
- OAuth-style code exchange for SPAs (`/oauth/token`)
- JWKS-published EdDSA signing keys (`/.well-known/jwks.json`)
- Role-based access control (RBAC) per `v1:identity:user.role`
- Admin surfaces (people, tokens, keys, settings) live in the memQL portal

**Developer access:**
- **Local:** All developers (own machine)
- **Cloud:** per the cluster's own role model -- deploy and rollback are
  role-gated and audited (see docs/public/operate/auth/access-model.md)

---

## Testing

```bash
# Run all tests
make test

# Run with coverage
make test-cover
```

Always verify with `make test`. This is a multi-module workspace, and a
reflex `go test` invocation resolves inside one module only -- silently
skipping the engine's own modules and reporting a false "ok". See
[CLAUDE.md's Testing section](CLAUDE.md#testing) for the full explanation
and the `MEMQL_REQUIRE_DB=1` / db-gated-lane details.

---

## Local Cluster (k3d + ArgoCD)

Full stack with PostgreSQL + TimescaleDB (via CloudNativePG) + memQL node
pods, reconciled by ArgoCD from `deploy/k8s/overlays/local`:

```bash
# Bootstrap (cluster + ArgoCD + seeded secrets)
make up

# View pod logs
kubectl logs -n memql deploy/bff -f

# Tear down
make down
```

**Documentation:** [docs/public/operate/reproduce-the-cloud-locally.md](docs/public/operate/reproduce-the-cloud-locally.md)

---

## MemQL Language

MemQL DSL is a domain-specific query language for time-series memory graphs.

### Example Query

The named-args form is how a declared query is invoked from a logic body or
a tool handler (not a standalone top-level `.memql` declaration in its own
right):

```memql fragment
query activeHumanParticipants(partitionId: "space_123")
```

### Example Automation
```memql
@enabled
@trigger(event="node.created", concept="v1:cognition:space", partition="*")
@description("On space creation, auto-join the creator's assistant")
automation autoJoinSI {
  step run {
    logic autoJoinSI ( event )
  }
}
```

**Full reference:** [docs/public/language/memql.md](docs/public/language/memql.md)

---

## Deployment

memQL runs on Azure Kubernetes Service (AKS), reconciled by ArgoCD from
`deploy/k8s/overlays/cloud`. The blessed deploy is a GIT MERGE: bump the
`{engine version, bundle digest, client digest}` in that overlay and merge.

`make deploy VERSION=X` is the break-glass path for when ArgoCD is
unavailable — it delegates to the memQL Cockpit's pinned, role-gated,
audited `deployEngineCluster` automation. It is not the normal path.

See [docs/public/operate/deploy-bundle-runbook.md](docs/public/operate/deploy-bundle-runbook.md)
for deploy/topology (ACR `acrmemql.azurecr.io`, the database, and the migration
+ smoke gates).

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

1. Read [docs/public/overview/tech-stack.md](docs/public/overview/tech-stack.md)
2. Make changes and test locally (`make test`)
3. Exercise the 2-replica parity cluster for anything cross-node
4. Branch, PR, CI green, then `gh pr merge <n>` (bare) — every change,
   including a one-line docs fix, goes through the merge queue
5. Stage files by explicit path (`git add <file>`)

**Git workflow:** Single long-lived `main` branch. Pre-release: no
backwards-compat shims; fix both memQL and the consumer at once.

---

## License

Apache License 2.0 — see [LICENSE](LICENSE).

---

## Need Help?

1. **Quick start:** [docs/public/overview/quickstart.md](docs/public/overview/quickstart.md)
2. **Find documentation:** [GLOSSARY.md](GLOSSARY.md)
3. **Tech stack details:** [docs/public/overview/tech-stack.md](docs/public/overview/tech-stack.md)
4. **Component docs:** Check directory `CLAUDE.md` files
5. **Issues:** Create GitHub issue

---

**memQL - the AI memory platform: agents, automations, and voice on a time-series memory graph**
