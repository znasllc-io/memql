<p align="center">
  <img src="assets/memql-lockup.png" alt="memQL" width="500">
</p>

<h1 align="center">memQL</h1>

<p align="center">
  <strong>AI-native time-series memory graph with a single DSL.</strong><br>
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

memQL is a distributed time-series memory graph with its own DSL — a single language for declaring concepts (schemas), queries, mutations, tools, and event-driven automations side-by-side, then executing them across specialized nodes.

It replaces the integration glue AI-native teams typically hand-write — vector store + workflow engine + AI gateway + voice stack — with one deployable primitive. A team that would otherwise stitch together four systems can declare an agent's memory, behavior, and triggers in one DSL file and run them on a memQL cluster.

## Why memQL?

Agent and voice deployments today are integration-heavy. Most of the engineering effort is plumbing — keeping state consistent across a vector store, an orchestrator, a tool registry, and a model provider. memQL collapses that plumbing: concepts and queries live in the same place; tools, automations, and workflows reference them directly; the engine handles consistency, time-series storage, and execution.

## Example

```
@version("1.0.0")
@namespace("acme")
concept ticket {
  subject     string   @required
  priority    string   @default("normal")
  status      enum("open", "closed") @default("open")
}

@enabled
@handler(type="query", query="concept==v1:acme:ticket && payload.priority==\"$args.priority\"")
@executionTime("fast")
@description("List tickets by priority")
tool listByPriority {
  priority string @required
}
```

A concept (schema) and an LLM-callable tool in the same file, same language. Add queries, mutations, or event-driven automations right next to them.

---

## Quick Start

```bash
# Start the local cluster (k3d + ArgoCD)
make up

# Run tests
go test ./...

# Deploy to staging (Azure AKS)
make deploy VERSION=X
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

### Development (k3d + ArgoCD)
- **Database:** PostgreSQL + TimescaleDB pod in the local k3d cluster
- **Service:** memQL node pods reconciled by ArgoCD from `deploy/k8s/overlays/local`
- **Access:** All developers
- **Command:** `make up`

### Staging (Cloud)
- **Database:** TimescaleDB Cloud (Tiger Cloud)
- **Service:** Azure Kubernetes Service (AKS, cluster `aks-memql-staging`)
- **Access:** All developers
- **Command:** `make deploy VERSION=X`

### Production (Cloud)
- **Database:** TimescaleDB Cloud (Tiger Cloud) - separate instance
- **Service:** Azure Kubernetes Service (AKS)
- **Access:** Senior/Lead developers only
- **Deploy:** Promote a validated version (see docs/public/operate/deployment-strategy.md)

**Full details:** [docs/public/overview/tech-stack.md](docs/public/overview/tech-stack.md)

---

## Development

### Prerequisites

**Hardware:**
- macOS with Apple Silicon (M1/M2/M3)
- MacBook Pro or MacBook Air
- 16GB RAM minimum (32GB recommended)

**Software:**
- Go 1.26.1+ (ARM64 build)
- Docker Desktop for Mac (Apple Silicon)
- k3d + kubectl (`brew install k3d kubectl`)
- Azure CLI (`az`) — for staging/prod deploys
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
   go test ./...

   # View logs
   kubectl logs -n memql deploy/bff -f
   ```

4. **Deploy to staging for integration testing**
   ```bash
   make deploy VERSION=X
   ```

5. **Commit to `main`** (focused commits) or open a feature branch + PR
   when review is genuinely useful. Stage by explicit path:
   ```bash
   git add path/to/changed.file
   git commit -m "domain: imperative subject"
   git push origin main
   ```

---

## Project Structure

```
memQL/
├── main.go                 # Entry point (thin orchestrator)
├── app/                    # Phased service bootstrap
│   ├── app.go             # Build() orchestrator
│   ├── config.go          # Config + auth
│   ├── database.go        # Database + concepts
│   ├── engine.go          # Engine + bus + automations
│   ├── integrations.go    # Integration providers
│   ├── transport.go       # gRPC + HTTP + WebSocket
│   ├── cluster.go         # Distributed node bootstrap
│   └── adapters.go        # Engine adapter types
├── component/              # Go service components
│   ├── memql/             # Core query engine
│   ├── database/          # Database providers
│   ├── server/            # HTTP/WebSocket servers
│   └── auth/              # Authentication
├── integrations/          # External service integrations
│   ├── cognition/         # AI collaboration
│   └── audio/             # Audio streaming
├── dsl/                   # The MemQL DSL tree (one directory per namespace)
│   ├── cognition/         # e.g. concepts.memql, queries.memql, mutations.memql,
│   │                      #      tools.memql, automations.memql, ... per namespace
│   ├── identity/
│   ├── _reference/        # Authoring reference skeletons (not loaded)
│   └── ...
├── docs/                  # Documentation
│   ├── public/            # Published docs (overview, concepts, language, ai,
│   │                      #      build, operate) -- rendered on memql.io
│   └── internal/          # Design records, plans, internal runbooks
├── deploy/k8s/            # Kustomize manifests (base + overlays/local|staging|prod)
└── .claude/               # Configuration
```

---

## Common Commands

| Task | Command |
|------|---------|
| **Start local cluster** | `make up` |
| **Tear down cluster** | `make down` |
| **Inner-loop rebuild + reload** | `make dev [NODE=<type>]` |
| **Run Go test suite** | `go test ./...` |
| **Deploy to staging** | `make deploy VERSION=X` |
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
- Centralized user / partition-access management at `/admin/`

**Developer access:**
- **Development:** All developers (own machine)
- **Staging:** All developers (shared testing)
- **Production:** Senior/Lead developers only (live system)

---

## Testing

```bash
# Run all tests
go test ./...

# Run specific package tests
go test -v ./component/memql/...

# Run with coverage
go test -cover ./...
```

---

## Local Cluster (k3d + ArgoCD)

Full stack with PostgreSQL + TimescaleDB + memQL node pods, reconciled
by ArgoCD from `deploy/k8s/overlays/local`:

```bash
# Bootstrap (cluster + ArgoCD + seeded secrets)
make up

# View pod logs
kubectl logs -n memql deploy/bff -f

# Access database (via the k3d postgres port-forward)
psql postgres://memql:memql_dev@localhost:5432/memql

# Tear down
make down
```

**Documentation:** [docs/public/operate/reproduce-staging-locally.md](docs/public/operate/reproduce-staging-locally.md)

---

## MemQL Language

MemQL DSL is a domain-specific query language for time-series memory graphs.

### Example Query
```memql
// Find active human participants in a space
queryActiveHumanParticipants({
  "spaceId": "space_123"
})
```

### Example Automation
```memql
@enabled
@trigger(event="node.created", concept="v1:cognition:space", partition="*")
@description("On space creation, auto-join the creator's assistant")
automation autoJoinSI {
  step run {
    logic autoJoinSI { event: event }
  }
}
```

**Full reference:** [docs/public/language/memql.md](docs/public/language/memql.md)

---

## Deployment

memQL runs on Azure Kubernetes Service (AKS). Deploy to staging with
`make deploy VERSION=X` (`scripts/deploy/aks-deploy.sh`); production
promotes a validated version.

See the product pack repo's docs/operate/deployment-strategy.md for deploy/topology
(cluster `aks-memql-staging`, ACR `acrmemql.azurecr.io`, Tiger Cloud DB,
the migration + smoke gates, and the staging → prod promotion flow).

---

## Contributing

1. Read [docs/public/overview/tech-stack.md](docs/public/overview/tech-stack.md)
2. Make changes and test in development environment (`go test ./...`)
3. Deploy to staging for integration testing
4. Commit directly to `main` for focused changes, or open a PR when review is useful
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

**memQL - Time-series memory graph database for AI-powered collaboration**
