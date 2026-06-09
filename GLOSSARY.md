# memQL Documentation Glossary
## Complete Documentation Index

Quick access to all memQL documentation by topic.

---

## START Getting Started

| Document | Description |
|----------|-------------|
| **[QUICKSTART](docs/public/overview/quickstart.md)** | Get running in 5 minutes |
| **[CLAUDE.md](CLAUDE.md)** | Project overview for SI assistants |
| **[Docker Setup](docker/README.md)** | Full Docker stack details |

---

## Core Concepts

| Topic | Document | Description |
|-------|----------|-------------|
| **Architecture** | [docs/public/concepts/architecture.md](docs/public/concepts/architecture.md) | System design and components (comprehensive) |
| **MemQL Language** | [docs/public/language/memql.md](docs/public/language/memql.md) | Query language reference (comprehensive) |
| **Functions** | [docs/public/language/functions.md](docs/public/language/functions.md) | Function system reference |
| **Events** | [docs/public/concepts/events.md](docs/public/concepts/events.md) | Event system and subscriptions |
| **Permissions** | [docs/public/concepts/permissions-and-access-control.md](docs/public/concepts/permissions-and-access-control.md) | Access control model |
| **Attributes** | [docs/public/language/attribute-matrix.md](docs/public/language/attribute-matrix.md) | Attribute schema reference |
| **Naming Conventions** | [docs/public/language/naming-conventions.md](docs/public/language/naming-conventions.md) | MemQL naming conventions |
| **Reserved Names** | [docs/public/language/reserved.md](docs/public/language/reserved.md) | Single index of every reserved identifier (engine names, row intrinsics, caller envelope, keywords, annotations, import aliases) |
| **Operator Capabilities** | [docs/public/ai/operator-capabilities.md](docs/public/ai/operator-capabilities.md) | Capability slugs (copresent_control, computer_use_headless, computer_use_embodied, workbench_use) and how they expand into concrete tools |
| **Identifier Conventions** | [docs/public/concepts/identifiers.md](docs/public/concepts/identifiers.md) | Canonical node id format, dispatch-site composition, anti-patterns |
| **Specifications** | [docs/public/language/specifications.md](docs/public/language/specifications.md) | MemQL specifications reference |
| **Automations** | [automations/CLAUDE.md](automations/CLAUDE.md) | Event-driven workflows |
| **Query Library** | [queries/CLAUDE.md](queries/CLAUDE.md) | Query function library |
| **Integrations** | [integrations/CLAUDE.md](integrations/CLAUDE.md) | External services (SI, audio, voice) |
| **Component Bus** | [component/bus/](component/bus/) | Channel-based inter-component communication (proto, channels, wiring) |
| **Configuration** | [component/config/](component/config/) | Centralized env var loading into ConfigSnapshot proto |
| **Data Validation** | [docs/public/concepts/data-validation.md](docs/public/concepts/data-validation.md) | Draft/checked/confirmed lifecycle, policies, identity requirements |
| **Partitions** | [CLAUDE.md](CLAUDE.md#partitions) | Data isolation boundaries, multi-tenant deployment, partition-qualified IDs and event topics |
| **MemQL Sense** | [component/memql/sense/](component/memql/sense/) | Language intelligence service (tokenize, complete, diagnose, hover, signature) via gRPC |

---

## Development Guides

| Topic | Document | Description |
|-------|----------|-------------|
| **Quick Start** | [docs/public/overview/quickstart.md](docs/public/overview/quickstart.md) | Get running in 5 minutes |
| **Env Vars** | [docs/public/operate/env-vars.md](docs/public/operate/env-vars.md) | Bootstrap envelope + memQL concept storage for secrets/variables |
| **Docker Setup** | [docker/README.md](docker/README.md) | Full local Docker stack |
| **Tests** | `go test ./...` | Standard Go test suite (no separate harness) |

---

## SI & Voice

| Topic | Document | Description |
|-------|----------|-------------|
| **SI Provider System** | [component/memql/si_providers.go](component/memql/si_providers.go) | Centralized provider registry (OpenAI, Anthropic) with pluggable interfaces |
| **SI gRPC Messages** | [component/grpc/ai_handlers.go](component/grpc/ai_handlers.go) | `AiChatMsg`, `AiSpeechMsg`, `AiTranscribeMsg`, `AiSuggestMsg` on `MemqlService.Stream` |
| **Provider Configuration** | [providers/v1/](providers/v1/) | MemQL provider definitions for OpenAI and Anthropic (e.g., `chat54Mini.memql`, `claudeSonnet.memql`) |
| **Prompt Templates** | [prompts/v1/](prompts/v1/) | MemQL prompt definitions (e.g., `agentReply.memql` on the agent node, `conductorTurn.memql` / `conductorCompaction.memql` on cognition) |
| **Shape Templates** | [shapes/v1/](shapes/v1/) | MemQL shape definitions -- one per concept (e.g., `participantFull.memql`, `agentFull.memql`, `spaceFull.memql`) |
| **Integrations Overview** | [integrations/CLAUDE.md](integrations/CLAUDE.md) | All integrations (SI, audio, voice) |
| **Polyphon Architecture** | [docs/public/operate/voice-bringup-verification.md](docs/public/operate/voice-bringup-verification.md) | Multi-agent voice pipeline (LiveKit + ASR/TTS) |
| **Claw Tools** | [tools/v1/claw/](tools/v1/claw/) | OpenClaw/NemoClaw coding agent tools (.memql) |
| **Claw Compose** | [docker/docker-compose.nemoclaw.yml](docker/docker-compose.nemoclaw.yml) | OpenClaw (hardened) Docker overlay for development |
| **Space Concept** | [concepts/v1/cognition/space/concept.memql](concepts/v1/cognition/space/concept.memql) | Three-state lifecycle (active/saved/archived/scheduled) + daily-space kind |
| **Audio Streaming** | [docs/public/build/audio-streaming.md](docs/public/build/audio-streaming.md) | Audio WebSocket + gRPC streaming transcription |
| **Cognition (Routing + Conductor)** | `integrations/cognition/cognition_handler.go` | Unified single-LLM-brain text dispatch (router lives only on the voice path) |

---

## Component Documentation

| Component | Document | Description |
|-----------|----------|-------------|
| **Automations** | [automations/CLAUDE.md](automations/CLAUDE.md) | Automation system |
| **Query Library** | [queries/CLAUDE.md](queries/CLAUDE.md) | Query function library |
| **Integrations** | [integrations/CLAUDE.md](integrations/CLAUDE.md) | Integration layer (14 DSL-callable capabilities) |
| **Components** | [component/CLAUDE.md](component/CLAUDE.md) | Go service components |
| **MemQL Engine** | [component/memql/arch.md](component/memql/arch.md) | Core query engine |
| **Distributed Nodes** | [component/node/CLAUDE.md](component/node/CLAUDE.md) | Node system (bootstrap, peers, mesh) |

---

## Distributed Architecture (Cluster Mode)

| Topic | Document | Description |
|-------|----------|-------------|
| **Node System** | [component/node/CLAUDE.md](component/node/CLAUDE.md) | Node types, bootstrap strategy, peer mesh, NodeService proto |
| **Cluster Concepts** | [concepts/v1/cluster/](concepts/v1/cluster/) | Cluster concepts (node, node-type, spawn-event) |
| **Cluster Docker** | [docker/docker-compose.cluster.yml](docker/docker-compose.cluster.yml) | Multi-node local development |
| **AKS Manifests** | [deploy/k8s/](deploy/k8s/) | Per-node Kubernetes manifests (see DEPLOYMENT_STRATEGY.md) |

---

## Docker & Infrastructure

| Topic | Document | Description |
|-------|----------|-------------|
| **Docker Setup** | [docker/README.md](docker/README.md) | Full Docker stack (4 compose variants) |
| **Docker Compose** | [docker-compose.full.yml](docker/docker-compose.full.yml) | Single-node service definitions |
| **Cluster Compose** | [docker-compose.cluster.yml](docker/docker-compose.cluster.yml) | Multi-node cluster mode |
| **Infrastructure** | [INFRASTRUCTURE.md](INFRASTRUCTURE.md) | Infrastructure overview |

---

## Common Commands

| Task | Command | Description |
|------|---------|-------------|
| **Start dev environment** | `docker compose -f docker/docker-compose.full.yml up --build` | Start full Docker stack |
| **Stop dev environment** | `docker compose -f docker/docker-compose.full.yml down` | Stop Docker services |
| **View logs** | `docker compose -f docker/docker-compose.full.yml logs -f` | Stream container logs |
| **Database shell** | `psql postgres://memql:memql_dev@localhost:5432/memql` | Open PostgreSQL shell |
| **Run tests** | `go test ./...` | Run Go test suite |
| **Deploy to staging** | `make deploy VERSION=X` | Deploy to Azure AKS (see DEPLOYMENT_STRATEGY.md) |

---

## By Directory

Quick access to directory-level documentation:

| Directory | CLAUDE.md | Purpose |
|-----------|-----------|---------|
| `/` (root) | [CLAUDE.md](CLAUDE.md) | Project overview |
| `/automations` | [automations/CLAUDE.md](automations/CLAUDE.md) | Event-driven workflows |
| `/queries` | [queries/CLAUDE.md](queries/CLAUDE.md) | Query functions |
| `/integrations` | [integrations/CLAUDE.md](integrations/CLAUDE.md) | External services |
| `/component` | [component/CLAUDE.md](component/CLAUDE.md) | Go components |
| `/docs` | [docs/CLAUDE.md](docs/CLAUDE.md) | Documentation index |
| `/docker` | [docker/README.md](docker/README.md) | Docker setup |

---

## Quick Search

### Authentication & Security
- [Access Model](docs/public/operate/auth/access-model.md) - Identity / user / partition-access data model + verifier middleware
- [User Provisioning](docs/public/operate/auth/user-provisioning.md) - Registration modes, magic-link flow, invitations
- [Identity Service (Operator Guide)](docs/public/operate/auth/identity-service.md) - Env vars, key management, anti-abuse tuning

### Workers (Computer Use)
- [Workers Runbook](docs/public/operate/workers-runbook.md) - Operator guide: install, permission model, audit, common ops, failure modes

### CLI / TUI (memql-cockpit)
- [cli/CLAUDE.md](cli/CLAUDE.md) - Canonical-TUI rule: every interactive subcommand uses `cli/ui` + `cli/canvas`. Multi-tab IDE + single-panel wizard layouts.

### Database & Data
- [Architecture](docs/public/concepts/architecture.md) - System architecture
- [MemQL Language](docs/public/language/memql.md) - Query language reference
- [Concept Seeding](docs/public/concepts/concept-seeding.md) - Seeding data
- [TimescaleDB Setup](docker/README.md) - Local database

### Testing & Debugging
- `go test ./...` -- standard Go test suite
- [Troubleshooting](docs/public/overview/quickstart.md#troubleshooting) - Common issues

### Deployment & Infrastructure
- [Deployment Strategy](DEPLOYMENT_STRATEGY.md) - Deploy to Azure AKS (topology, gates, promotion)
- [Deployment Console](docs/public/operate/deployment-console.md) - Operator guide: admin/owner UI (identity portal + cockpit) to read deploy state and deploy/promote/rollback from the UI, with confirm + audit
- [Infrastructure Overview](INFRASTRUCTURE.md) - All environments
- [Docker Setup](docker/README.md) - Local Docker stack

---

## [NOTE] Documentation by Status

### [OK] Active Documentation
Core docs currently maintained and up-to-date

### [LIST] Planning Documentation
Planning docs are added as needed and removed when implemented: [docs/planning/](docs/planning/)

---

## [HELP] Can't Find What You Need?

1. **Check this glossary** - Use browser search (Cmd+F / Ctrl+F)
2. **Check directory CLAUDE.md** - Each directory explains its contents
3. **Ask Claude Code CLI** - It can search and explain
4. **Check recent commits** - Documentation might be in progress

---

**Last Updated:** April 29, 2026
