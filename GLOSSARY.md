# memQL Documentation Glossary
## Complete Documentation Index

Quick access to all memQL documentation by topic.

---

## START Getting Started

| Document | Description |
|----------|-------------|
| **[QUICKSTART](QUICKSTART.md)** | Get running in 5 minutes |
| **[CLAUDE.md](CLAUDE.md)** | Project overview for SI assistants |
| **[Docker Setup](docker/README.md)** | Full Docker stack details |

---

## Core Concepts

| Topic | Document | Description |
|-------|----------|-------------|
| **Architecture** | [docs/core/arch.md](docs/core/arch.md) | System design and components (comprehensive) |
| **MemQL Language** | [docs/core/memql.md](docs/core/memql.md) | Query language reference (comprehensive) |
| **Functions** | [docs/core/memql-functions.md](docs/core/memql-functions.md) | Function system reference |
| **Events** | [docs/core/events.md](docs/core/events.md) | Event system and subscriptions |
| **Permissions** | [docs/core/permissions_and_access_control.md](docs/core/permissions_and_access_control.md) | Access control model |
| **Attributes** | [docs/core/attribute-matrix.md](docs/core/attribute-matrix.md) | Attribute schema reference |
| **Naming Conventions** | [docs/core/memql-naming-conventions.md](docs/core/memql-naming-conventions.md) | MemQL naming conventions |
| **Reserved Names** | [docs/core/memql-reserved.md](docs/core/memql-reserved.md) | Single index of every reserved identifier (engine names, row intrinsics, caller envelope, keywords, annotations, import aliases) |
| **Operator Capabilities** | [docs/core/operator-capabilities.md](docs/core/operator-capabilities.md) | Capability slugs (copresent_control, computer_use_headless, computer_use_embodied, workbench_use) and how they expand into concrete tools |
| **Identifier Conventions** | [docs/core/identifiers.md](docs/core/identifiers.md) | Canonical node id format, dispatch-site composition, anti-patterns |
| **Specifications** | [docs/core/memql-specifications.md](docs/core/memql-specifications.md) | MemQL specifications reference |
| **Automations** | [automations/CLAUDE.md](automations/CLAUDE.md) | Event-driven workflows |
| **Query Library** | [queries/CLAUDE.md](queries/CLAUDE.md) | Query function library |
| **Integrations** | [integrations/CLAUDE.md](integrations/CLAUDE.md) | External services (SI, audio, voice) |
| **Component Bus** | [component/bus/](component/bus/) | Channel-based inter-component communication (proto, channels, wiring) |
| **Configuration** | [component/config/](component/config/) | Centralized env var loading into ConfigSnapshot proto |
| **Data Validation** | [docs/core/data-validation.md](docs/core/data-validation.md) | Draft/checked/confirmed lifecycle, policies, identity requirements |
| **Partitions** | [CLAUDE.md](CLAUDE.md#partitions) | Data isolation boundaries, multi-tenant deployment, partition-qualified IDs and event topics |
| **MemQL Sense** | [component/memql/sense/](component/memql/sense/) | Language intelligence service (tokenize, complete, diagnose, hover, signature) via gRPC |

---

## Development Guides

| Topic | Document | Description |
|-------|----------|-------------|
| **Quick Start** | [QUICKSTART.md](QUICKSTART.md) | Get running in 5 minutes |
| **Env Vars** | [docs/guides/env-vars.md](docs/guides/env-vars.md) | Bootstrap envelope + memQL concept storage for secrets/variables |
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
| **Polyphon Architecture** | [docs/polyphon-architecture.md](docs/polyphon-architecture.md) | Multi-agent voice pipeline (LiveKit + ASR/TTS) |
| **Claw Tools** | [tools/v1/claw/](tools/v1/claw/) | OpenClaw/NemoClaw coding agent tools (.memql) |
| **Claw Compose** | [docker/docker-compose.nemoclaw.yml](docker/docker-compose.nemoclaw.yml) | OpenClaw (hardened) Docker overlay for development |
| **Claw Cloud Run** | [service.nemoclaw.yaml](service.nemoclaw.yaml) | Multi-container Cloud Run service (memQL + OpenClaw sidecar) |
| **Claw Build** | [cloudbuild.nemoclaw.yaml](cloudbuild.nemoclaw.yaml) | Cloud Build pipeline for OpenClaw deployment |
| **Space Concept** | [concepts/v1/cognition/space/concept.memql](concepts/v1/cognition/space/concept.memql) | Three-state lifecycle (active/saved/archived/scheduled) + daily-space kind |
| **Audio Streaming** | [docs/api/audio-streaming.md](docs/api/audio-streaming.md) | Audio WebSocket + gRPC streaming transcription |
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
| **Cloud Run Configs** | [infra/cluster/](infra/cluster/) | Per-node Cloud Run service configs |

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
| **Deploy to staging** | `gcloud run deploy` | Deploy to Cloud Run |

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
- [Access Model](docs/auth/access-model.md) - Identity / user / partition-access data model + verifier middleware
- [User Provisioning](docs/auth/user-provisioning.md) - Registration modes, magic-link flow, invitations
- [Identity Service (Operator Guide)](docs/auth/identity-service.md) - Env vars, key management, anti-abuse tuning
- [Service Account Setup](docs/SERVICE_ACCOUNT_SETUP.md) - GCP deployment service account

### Workers (Computer Use)
- [Workers Runbook](docs/workers/runbook.md) - Operator guide: install, permission model, audit, common ops, failure modes

### CLI / TUI (memql-cockpit)
- [cli/CLAUDE.md](cli/CLAUDE.md) - Canonical-TUI rule: every interactive subcommand uses `cli/ui` + `cli/canvas`. Multi-tab IDE + single-panel wizard layouts.

### Database & Data
- [Architecture](docs/core/arch.md) - System architecture
- [MemQL Language](docs/core/memql.md) - Query language reference
- [Concept Seeding](docs/core/concept_seeding.md) - Seeding data
- [TimescaleDB Setup](docker/README.md) - Local database

### Testing & Debugging
- `go test ./...` -- standard Go test suite
- [Troubleshooting](QUICKSTART.md#troubleshooting) - Common issues

### Deployment & Infrastructure
- [Deployment Strategy](DEPLOYMENT_STRATEGY.md) - Deploy to Cloud Run
- [Infrastructure Overview](INFRASTRUCTURE.md) - All environments
- [Service Account Setup](docs/SERVICE_ACCOUNT_SETUP.md) - GCP deployment credentials
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
