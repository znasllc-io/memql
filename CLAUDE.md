# memQL - Time-Series Memory Graph Database

**Type:** Time-series database with event-driven automations and SI integration
**Language:** Go + MemQL DSL
**Stack:** PostgreSQL + TimescaleDB extension
**Purpose:** Store and query time-series memory nodes with semantic relationships

---

## Quick Start

```bash
# Start complete development environment (Docker)
docker compose -f docker/docker-compose.full.yml up --build

# Run tests
go test ./...

# Build binary (BFF is the default, no tag needed)
go build -o bin/memql .

# Build node-type binary (voice, cognition, agent, planner)
go build -tags voice -o bin/memql-voice .
```

---

## Project Structure

```
memQL/
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
│   ├── <namespace>/   One directory per namespace (agents, cluster,
│   │   │              cognition, common, curriculum, data, identity,
│   │   │              knowledge, memql, observability, planner,
│   │   │              platform, policies, providers, router, safety,
│   │   │              workbench, worker)
│   │   ├── concepts.memql     Concept definitions (schemas)
│   │   ├── mutations.memql    Mutation functions
│   │   ├── queries.memql      Query functions
│   │   ├── specs.memql        Specification predicates
│   │   ├── shapes.memql       Reusable shape templates
│   │   ├── builtins.memql     Go-backed executors
│   │   ├── tools.memql        SI tool definitions
│   │   ├── prompts.memql      SI prompt schemas (+ prompts/*.tmpl)
│   │   ├── automations.memql  Event-driven workflows
│   │   └── ...                (not every namespace carries every construct)
│   └── _reference/    Per-construct authoring reference skeletons
│                      (_concept / _shape / _spec / _trait / _agent)
├── integrations/      External services + DSL-callable capabilities (Go)
├── component/         Core Go components
│   ├── bus/           Channel-based inter-component communication (Go)
│   ├── config/        Centralized env var loading (Go)
│   ├── node/          Distributed node system (identity, peer mesh, bootstrap)
│   ├── memql/dslfs/   MEMQL_DSL_PATH on-disk override / embedded FS picker
│   ├── architecture/  Auto-generated architecture model (UML/C4 from source)
│   ├── observe/       Per-invocation observability runtime (FQN-keyed)
│   ├── genesis/       Sealed env envelope + repo-root .env override (localenv.go)
│   └── ...            (memql, grpc, events, database, server, auth, etc.)
├── core/              Shared utilities (logger, env, id)
├── cmd/               Command-line tools (healthcheck, memqlfmt, memqlmigrate, admin-preview)
├── scripts/           Database and migration scripts
├── infra/             Infrastructure configuration
│   └── cluster/       (legacy GCP configs removed; AKS manifests live in deploy/k8s/)
├── docs/              Documentation
├── docker/            Full Docker stack + cluster mode
└── .claude/
    ├── config/            Staging environment configuration
    ├── launch.json        Dev server configurations
    └── permissions.json   Access control configuration
```

---

## Key Directories

| Directory | Purpose | Language | CLAUDE.md |
|-----------|---------|----------|-----------|
| `dsl/<ns>/automations.memql` | Event-driven automations | MemQL | — |
| `dsl/<ns>/queries.memql` | Query functions | MemQL | — |
| `dsl/<ns>/mutations.memql` | Mutation functions | MemQL | — |
| `dsl/<ns>/specs.memql` | Specification predicates | MemQL | — |
| `dsl/<ns>/tools.memql` | SI tool definitions | MemQL | — |
| `dsl/<ns>/prompts.memql` | SI prompt schemas (+ `prompts/*.tmpl`) | MemQL | — |
| `dsl/providers/providers.memql` | SI provider configurations | MemQL | — |
| `dsl/<ns>/shapes.memql` | Reusable shape templates | MemQL | — |
| `dsl/policies/policies.memql` | SI provider-selection policies | MemQL | — |
| `integrations/` | External service integrations + DSL capabilities | Go | [→](integrations/CLAUDE.md) |
| `component/` | Core service components | Go | [→](component/CLAUDE.md) |
| `component/bus/` | Channel-based component communication bus | Go | -- |
| `component/config/` | Centralized configuration loading | Go | -- |
| `component/node/` | Distributed node system (bootstrap, peers, mesh) | Go | [→](component/node/CLAUDE.md) |
| `docs/` | Documentation | Markdown | [→](docs/CLAUDE.md) |

---

## Documentation

**Start here:** [docs/public/overview/quickstart.md](docs/public/overview/quickstart.md) - Get running in 5 minutes

**Full index:** [GLOSSARY.md](GLOSSARY.md) - Find any documentation

**Tech stack:** [docs/public/overview/tech-stack.md](docs/public/overview/tech-stack.md) - Deployment practices

**Operations:**
- [Environment variables](docs/public/operate/env-vars.md) -- bootstrap envelope vs. concept-stored config; how to add / rotate / override
- [Auto-generated architecture diagrams](docs/internal/design/auto-generated-diagrams.md) -- the static topology model + observe runtime + cockpit drill-down navigator. Includes `.env` repo-root override flow (`component/genesis/localenv.go`) and `MEMQL_OBSERVE_LEVEL`.

**Core concepts:**
- [Architecture](docs/public/concepts/architecture.md)
- [MemQL Language](docs/public/language/memql.md)
- [Functions](docs/public/language/functions.md)
- [Events](docs/public/concepts/events.md)
- [Node Identifier Conventions](docs/public/concepts/identifiers.md) -- canonical id format, who composes it, anti-patterns
- [MemQL Authoring Rules & Gotchas](docs/public/language/authoring-rules.md) -- read before writing `.memql` files
- [LLM cost control (defense in depth)](docs/public/ai/llm-cost-control.md) -- the layered guardrails (kill-switch, rate ceiling, automation budget, loop caps) that make a runaway spend loop structurally impossible; every `MEMQL_LLM_*` / budget env var + how to repro safely. Read before touching `si_guard.go`, an LLM loop, or an automation that drives model calls.
- [Tool ↔ Knowledge Domain Pattern](docs/public/concepts/tool-knowledge-domain-pattern.md) -- when a capability has operational knowledge (CoPresent Control, Computer Use, etc.), put it in a knowledge domain that the tool requires, not in the agent prompt template. Read before adding capability-bundled documentation.

**Tooling:**
- **memql-cockpit** -- terminal-native IDE and operations console (display name "memQL Cockpit"). Lives in its own repo at `github.com/znasllc-io/memql-cockpit`; consult that repo's CLAUDE.md and Makefile.

---

## Development Workflow

### Development Environment (Docker)
```bash
# Start full stack (PostgreSQL + memQL)
docker compose -f docker/docker-compose.full.yml up --build

# Start with Polyphon voice pipeline
docker compose -f docker/docker-compose.full.yml -f docker/docker-compose.polyphon.yml up --build

# Start with NemoClaw coding agent
docker compose -f docker/docker-compose.full.yml -f docker/docker-compose.nemoclaw.yml up --build

# Start cluster mode (bff + cognition + planner)
docker compose -f docker/docker-compose.cluster.yml up --build

# Stop everything
docker compose -f docker/docker-compose.full.yml down

# View logs
docker compose -f docker/docker-compose.full.yml logs -f
```

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
go test ./...
```

---

## Branch Workflow

memQL uses a single long-lived branch: `main`. Core engine, wire
protocol, and product-specific DSL (concepts, queries, mutations,
shapes, automations, tools, prompts under `dsl/cognition/concepts.memql`,
`dsl/cognition/tools/`, etc.) all live here. A separate
`bff/copresent` branch was retired on 2026-04-20 once the dual-branch
overhead stopped paying for itself.

**Rules of engagement:**

1. **Commit directly to `main`.** Use a short-lived feature branch only
   when PR review is genuinely useful; otherwise a focused commit on
   main is fine.
2. **Pre-release -- no backwards-compat shims or deprecation windows.**
   When a contract changes, fix both memQL and the consumer (typically
   CoPresent) at once and delete what is no longer needed. Do not add
   legacy adapters, fallback code paths, or "keep working while we
   migrate" layers.
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
| **Start development** | `docker compose -f docker/docker-compose.full.yml up --build` | Full stack (PostgreSQL + memQL) |
| **Start cluster (staging parity, blessed)** | `make dev-cluster-up` | 2-replica mesh matching staging; the default local topology (memql#1260). `make dev-cluster-down` to stop |
| **Stop services** | `docker compose -f docker/docker-compose.full.yml down` | Stop all services |
| **View logs** | `docker compose -f docker/docker-compose.full.yml logs -f` | Service logs |
| **Run tests** | `go test ./...` | Go tests |
| **Build binary** | `go build -o bin/memql .` | Build BFF binary (default) |
| **Connect DB** | `psql postgres://memql:memql_dev@localhost:5432/memql` | Database shell |

---

## Architecture & Tech Stack

### Core Technologies
- **Language:** Go 1.26.1+
- **Database:** PostgreSQL 16 + TimescaleDB
- **API:** gRPC (`MemqlService.Stream` is the primary surface) + HTTP for the documented exceptions (OAuth, health, file uploads, Polyphon room tokens) + WebSocket bridge to the gRPC stream for browsers (`/memql/ws`)
- **SI:** Centralized provider system (OpenAI, Anthropic). All SI ops on gRPC; HTTP path retired.
- **Auth:** in-house identity service (magic-link + JWT, JWKS-published)
- **Query Language:** MemQL DSL

### Environment Architecture

| Environment | Database | Service | Access |
|-------------|----------|---------|--------|
| **Development** | Docker PostgreSQL + TimescaleDB | Docker memQL container | All developers |
| **Staging** | Tiger Cloud (Timescale Cloud) | Azure Kubernetes Service (`aks-memql-staging`) | All developers |
| **Production** | Tiger Cloud (separate instance) | Azure Kubernetes Service | Senior/Lead only |

### Hardware Requirements
- **Platform:** macOS (Apple Silicon)
- **Devices:** MacBook Pro or MacBook Air (M1/M2/M3 chips)
- **Reason:** Standardized development environment

**Full tech stack details:** [docs/public/overview/tech-stack.md](docs/public/overview/tech-stack.md)

### System Architecture
```
┌─────────────────────────────────────────────────────┐
│                  HTTP/WebSocket API                 │
│           (Nginx LB: 8080 / gRPC: 50050)           │
├─────────────────────────────────────────────────────┤
│                                                     │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────┐ │
│  │   MemQL      │  │ Automations  │  │ Functions│ │
│  │   Engine     │◄─┤   System     │◄─┤  System  │ │
│  └──────┬───────┘  └──────────────┘  └──────────┘ │
│         │                                          │
│    ┌────┴────────┐ ┌──────────────┐               │
│    │ SI Provider │ │ Integrations │ ┌──────────┐ │
│    │  Registry   │ │ (Cognition,  │ │ NemoClaw │ │
│    │(OpenAI,     │ │  Audio, etc) │ │ (Coding  │ │
│    │ Anthropic)  │ └──────────────┘ │  Agent)  │ │
│    │             │                   └──────────┘ │
│    └────┬────────┘                                │
│         │                                          │
│    ┌────┴────────────────────┐  ┌──────────────┐ │
│    │  SI gRPC Messages       │  │ MemQL Sense  │ │
│    │  (MemqlService.Stream): │  │ (Language     │ │
│    │  AiChatMsg, AiSpeechMsg,│  │  Intelligence)│ │
│    │  AiTranscribeMsg,       │  │ Tokenize,     │ │
│    │  AiSuggestMsg (space /  │  │ Complete,     │ │
│    │  group / agent)         │  │ Diagnose,     │ │
│    └─────────────────────────┘  │ Hover,        │ │
│                                  │ Signature     │ │
│                                  └──────────────┘ │
│                                                     │
├─────────────────────────────────────────────────────┤
│          PostgreSQL + TimescaleDB                   │
│   (Partition-isolated time-series memory nodes)     │
│   PK: (partition, id, createdAt)                    │
└─────────────────────────────────────────────────────┘
```

### Distributed Node Architecture (Cluster Mode)

memQL uses **Go build tags** to compile separate binaries for each node type.
Each tagged binary includes only the integrations, transport layers, and Go
packages relevant to its purpose, reducing binary size by up to 53%.

```bash
go build .                       # bff        (25 MB, default)
go build -tags voice .           # voice      (30 MB)
go build -tags cognition .       # cognition  (35 MB, -34%)
go build -tags agent .           # agent      (43 MB, -19%)
go build -tags planner .         # planner    (25 MB, -53%)
```

```
        ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐
        │   BFF    │ │  Voice   │ │Cognition │ │ Planner  │ │  Agent   │
        │  Node    │ │  Node    │ │  Node    │ │  Node    │ │  Node    │
        └──────────┘ └──────────┘ └──────────┘ └──────────┘ └──────────┘
         backend      voice        cognition     planning     task exec
         for front.   transport    pipeline       orchestr.    SI work
```

- **BFF** (default): Backend for frontend, domain-specific API surface
- **Voice**: Voice transport (audio WS, LiveKit)
- **Cognition**: Cognition pipeline, Polyphon
- **Agent**: Task execution, SI work, tool calling
- **Planner**: Task planning and orchestration

Nodes discover each other via mesh. All nodes share a single
PostgreSQL + TimescaleDB database. Inter-node communication uses `NodeService` gRPC
bidirectional stream. Events bridge across nodes with dedup and TTL.

#### Node image source: carrier-built vs engine-built (#1053) -- ENFORCED RULE

A node that executes **CoPresent DSL** (the `agentReply` prompt, copresent
tools/concepts -- consumed by the agent/cognition/planner integrations) MUST be
**carrier-built**: compiled from `memql-bff-copresent/Dockerfile` with
`BUILD_TAGS=<type>` and the workspace parent as build context, so the CoPresent
DSL subtree is mounted at compile time via `RegisterTree`. The pure-engine
`memql-<type>` image (this repo's `Dockerfile`) does NOT carry the CoPresent DSL
and will fail with `unknown prompt template "agentReply"`.

| Node | Build source |
|------|--------------|
| **bff, cognition, agent, planner, workbench** | **carrier** (`memql-bff-copresent/Dockerfile` + `BUILD_TAGS=<type>`, CGO=0) |
| **voice** | engine voice-runtime (CGO; transport/forwarding only, no CoPresent refs) |
| **identity** | engine (auth service, no CoPresent refs) |

This MUST be identical in every environment -- local cluster
(`docker-compose.cluster.yml`), staging, prod, and client deploys. The build
pipeline enforces it: `scripts/deploy/aks-deploy.sh` (`CARRIER_NODE_TYPES`)
carrier-builds the set; the cluster compose builds the same nodes from the
carrier Dockerfile. The pure-engine `memql-<type>` images are only for a
memQL-standalone (no CoPresent) deployment.

**Build tag reference:** [docs/public/build/build-tags.md](docs/public/build/build-tags.md)
**Local cluster (staging parity -- THE blessed local topology, memql#1260):** `make dev-cluster-up` (background) / `make dev-cluster-down`, or `make dev-cluster` (foreground); `make dev-cluster-restart[-purge]` force a fresh `--no-cache` rebuild after Go/MemQL source edits. Uses `docker-compose.cluster.yml`. The cluster mirrors staging along the **mesh-delivery path** (memql#1212): 2 replicas per mesh node (bff/cognition/voice/agent/planner/workbench) with per-replica unique node ids (hostname-derived via `os.Hostname()`, the compose equivalent of staging's `fieldRef: metadata.name`), plus the copresent SPA + LiveKit behind a single-origin nginx front door (http://localhost:8085). `make dev-cluster-status` prints the per-replica node ids (parity litmus). A few nodes off that path diverge for concrete local-host reasons (identity at 1 replica, voice-agent opt-in via the polyphon overlay, lifecycle probes deferred to Phase 3) -- each is enumerated + justified in the divergence audit in the runbook: [docs/public/operate/reproduce-staging-locally.md](docs/public/operate/reproduce-staging-locally.md). Reach for the cluster whenever a change can touch cross-node delivery, replica fan-out, or node lifecycle.
**Local single-node (fast path):** `docker compose -f docker/docker-compose.full.yml up --build` (`make dev`) -- Postgres + bff + voice only. Quicker to boot for engine/DSL/single-binary work that does NOT exercise the multi-replica mesh; it structurally cannot reproduce cross-replica delivery bugs, so use the cluster for anything mesh-shaped.

#### Client-tool relay (agent → browser, across nodes)

The memQL tool registry supports **client-executed tools** (tools whose
implementation runs in the browser, e.g. UI-drive helpers). In
single-binary mode the agent's `InvokeClientTool` writes directly to
the browser's stream and parks on a session-scoped waiter. In cluster
mode the agent and browser live on different nodes, so the
`ClientToolCall` envelope needs a cross-node round-trip. memQL does
this via the graph event bus:

1. Cognition intercepts `ClientToolCall` in `consumeAgentTurnStream`
   and inserts a `v1:cognition:client:tool:request` node (via
   `mutationEmitClientToolRequest`).
2. Browsers subscribed to the space pick the event up, dispatch the
   tool locally, and insert a matching
   `v1:cognition:client:tool:response` (via
   `mutationEmitClientToolResponse`).
3. Cognition subscribes to those responses, wraps the payload in a
   `ClientToolResult` envelope, and calls
   `AgentForwarder.ForwardContinuation` so the agent's
   service-scoped waiter fires and the parked tool loop returns.

Shipped; the relay lives in `integrations/cognition/client_tool_relay.go`. The three consumer bridges on the CoPresent side (`OperatorClientToolBridge`, `ClientToolRelayBridge`, `DelegateTakeoverBridge`) are mounted on `DashboardPage` and ride the same protocol.

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

- **Protobuf messages** -- All inter-component messages defined in `component/bus/bus.proto` (27 types)
- **ReplyTo pattern** -- Request-response over channels via embedded reply channel
- **Default buffer** -- 64 items per channel, configurable via `ChannelConfig`
- **Telemetry hooks** -- Channel fill-level, send/drop counters for future dynamic sizing
- **Ready() signaling** -- All components expose `Ready() <-chan struct{}` for parallel startup

---

## Endpoint Protocol Policy (gRPC-First)

**IMPORTANT: This policy is a hard requirement for all memQL development.**

gRPC is the **default and required** protocol for all internal and service-to-service
endpoints in memQL. HTTP endpoints are allowed **only** when an external protocol
requirement makes gRPC impossible.

### Decision Criteria

When adding a new endpoint or capability to memQL, apply this decision tree:

1. **Is this a service-to-service call?** (e.g., frontend to memQL, bridge agent to memQL)
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
| **Auth (identity service)** | `/auth/login`, `/auth/magic-link`, `/auth/complete`, `/auth/logout`, `/oauth/token`, `/auth/refresh`, `/.well-known/jwks.json` | OAuth 2.0 / magic-link flow requires HTTP redirects, browser form posts, and JWKS publishing |
| **Health check** | `/healthz` | Docker and Kubernetes health probes expect HTTP GET |
| **WebSocket upgrades** | `/memql/ws`, `/memql/audio` | Browser clients need HTTP upgrade to establish WebSocket |
| **File uploads** | `/spaces/{id}/attachments` | Multipart form-data uploads map poorly to gRPC |

### gRPC-Only Endpoints (HTTP Retired)

The legacy SI and Polyphon HTTP paths have been removed. Everything lives
on `MemqlService.Stream` now; cross-node proxying rides `AiForwardRouter`.

| Category | gRPC Message Types | Handler |
|----------|--------------------|---------|
| **SI service-to-service** | `AiChatMsg`, `AiSpeechMsg`, `AiTranscribeMsg`, `AiSuggestMsg` (space / group / agent) | `ai_handlers.go` |
| **Streaming transcription** | `AiTranscribeStreamStart` / `Chunk` / `End` + `AiTranscribeStreamDelta` / `Complete` | `ai_transcribe_stream.go` -- multi-message flow keyed by `request_id`, forwarded BFF -> Voice via `AiForwardRouter.ForwardContinuation` |
| **Polyphon internal** | `PolyphonRoomTokenMsg`, `PolyphonStatusMsg`, `PolyphonUtteranceMsg` | `polyphon_handlers.go` |
| **Concepts API** | `ConceptsListMsg`, `ConceptsSubscribeMsg` | `concepts_handlers.go` |
| **Guest invites** | `SendGuestInviteMsg`, `ResolveGuestInviteMsg`, `ResendGuestInviteEmailMsg`, `CancelGuestInviteMsg` | `guest_handlers.go` |

### For SI Agents and Developers

When implementing new functionality in memQL:

1. **Never add new HTTP endpoints** without explicit user approval
2. **Default to gRPC** -- add message types to `component/grpc/memql.proto`
3. If you believe HTTP is needed, **ask the user first** and document the reasoning
4. Reference this section when making the decision
5. All new gRPC messages follow the existing multiplexed stream pattern:
   - Add request type to `MemqlClientMessage.oneof payload`
   - Add response type to `MemqlServerMessage.oneof payload`
   - Add handler in `component/grpc/server.go`

---

## SI Integration

memQL centralizes all SI operations through a pluggable provider system:

### Provider System
- **Multi-provider architecture** - Unified interfaces (`ChatSIProvider`, `VisionSIProvider`, `TTSSIProvider`, `ChatStreamProvider`) with pluggable backends
- **OpenAI providers** - GPT-4, GPT-5-mini for chat, vision, TTS, and STT
- **Anthropic providers** - Claude Opus, Sonnet, Haiku for chat and vision
- **Provider configuration** - MemQL provider records in `dsl/providers/providers.memql`
- **Provider selection** - Default provider via config, or per-request via `provider` parameter

### SI Endpoints (gRPC on `MemqlService.Stream`)

All SI operations go through gRPC message types on the single bidirectional
stream `MemqlService.Stream`:

- `AiChatMsg` / `AiChatResult` / `AiStreamChunk` -- chat completions (streaming + non-streaming)
- `AiSpeechMsg` / `AiSpeechResult` -- text-to-speech
- `AiTranscribeMsg` / `AiTranscribeResult` -- speech-to-text (batch)
- `AiTranscribeStreamStart` / `Chunk` / `End` -> `AiTranscribeStreamDelta` / `Complete` -- real-time streaming transcription
- `AiSuggestMsg` / `AiSuggestResult` -- carries `domain` ∈ {spaces, spaceTitle, agents, groups, groupDescription, agentCardSummary, spaceCardSummary, groupCardSummary, knowledge}. `spaceTitle` is the lightweight purpose -> title path used by Create Space; `groupDescription` is its mirror (name -> one-line description) used by Create Group. The rich `spaces` / `agents` / `groups` domains return full payloads (description + suggested members + roles). The three `*CardSummary` domains generate the LLM body that lands on the agent / space / group canvas-creation cards. `knowledge` powers the CoPresent KnowledgeModal's domain picker.

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
   +-- Deepgram Nova-3 STT (user audio in)
   |              |
   |              v
   |        memql gRPC client         (VoiceAgentTurnRequest -> Delta)
   |              |
   |              v
   |        memql cognition           (BYO conductor + agent tool loop)
   |              |
   |              v
   +-- Deepgram Aura-2 TTS            (token-by-token input streaming)
   |              |
   |              v
   +-- Anam or Simli avatar           (lip-synced video)
```

The agent supports two executors selected by `MEMQL_VOICE_EXECUTOR`:
`cascade` (default -- the Deepgram STT -> cognition -> Deepgram TTS
path above) and `realtime` (OpenAI gpt-realtime speech-to-speech).

Key files (all under `integrations/voice/agent/`):
- `config.go` / `bootstrap.go` -- env loading + class="voice_agent"
  token resolution (`ResolveVoiceAgentToken`).
- `grpc_client.go` -- speaks memql's `VoiceAgent*` gRPC contract on
  `MemqlService.Stream`. TurnRequest in, TurnDelta stream out;
  specialists are dispatched server-side and land in chat via the
  normal agent path.
- `cascade.go` / `stt_pipeline.go` / `tts_pipeline.go` /
  `turntaking.go` -- the Deepgram cascade + turn-taking / barge-in.
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
- `MEMQL_DEEPGRAM_API_KEY` -- Deepgram (STT + TTS).
- `MEMQL_VOICE_EXECUTOR` -- `cascade` (default) or `realtime`.
- `MEMQL_VOICE_ROOM_NAME` -- room to join (falls back when no
  `--room` flag is passed).
- `OPENAI_API_KEY` / `MEMQL_REALTIME_*` -- required on the realtime
  executor path only.
- `VOICE_AGENT_TOKEN` -- identity-issued `class="voice_agent"` JWT
  (#109). Mint via `JWTIssuer.IssueVoiceAgentAccessToken`
  (`make voice-agent-token`); or self-bootstrap via
  `MEMQL_NODE_BOOTSTRAP_TOKEN` + `IDENTITY_VERIFIER_BASE_URL` +
  `MEMQL_VOICE_AGENT_INSTANCE_ID`. See `docs/public/operate/auth/voice-agent-jwt.md`.
- `MEMQL_AVATAR_VENDOR` -- `anam` (default) or `simli` or `none`.
- `ANAM_API_KEY` / `SIMLI_API_KEY` -- vendor keys.

Make targets:
- `make voice` -- build the `memql-voice` binary (carries the
  `voice-agent` subcommand).
- `make voice-agent-token` -- mint a `class="voice_agent"` JWT for the
  local cluster (used by `scripts/dev/refresh.sh`).

Docker: the `voice-agent` service in
`docker/docker-compose.polyphon.yml` runs the `memql-voice` image (the
`voice-runtime` CGO stage) with `command: voice-agent`.

`integrations/deepgram/` + `integrations/openai/` on the Go side stay
-- they still serve the `/memql/audio` WebSocket path for voice-first
creation modals. The earlier Python voice-agent (LiveKit Agents 1.5)
and the legacy Go Bridge Agent have both been retired.

**Canonical voice catalog (`integrations/voice/voices.go`).** Every
agent carries a canonical voice name (alto / soprano / tenor /
baritone / ...) on `providerConfig.voice.voiceId`, plus a
`gender` enum on the agent record. The catalog is gender-bucketed
and provider-agnostic; the cognition handler resolves canonical ->
provider voice id at TTS-publish time via the active
`POLYPHON_VOICE_PROVIDER`. Voice is auto-assigned at agent creation
(see CreateAgentModal on the CoPresent side) and never edited by
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
under `mirror_user`. Mutations: `mutationSetAgentAudioOverride`,
`mutationSetAgentVideoOverride`. Queries: `queryAudioOverridesForSpace`,
`queryVideoOverridesForSpace`.

**Avatar persona.** `v1:agents:agent.avatarPersonaId` +
`avatarVendor` carry the vendor-issued persona / face id minted from a
still image uploaded via the agent edit modal. Empty for legacy or
specialist agents -- voice-agent disables the avatar plugin and falls
back to audio-only.

See [integrations/CLAUDE.md](integrations/CLAUDE.md) for the Go-side
voice-related integrations (deepgram, openai, voices catalog) that
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
`conductor_consult.go`, `si_router.go`.

**Capability-aware routing.** Both the conductor (and the voice-path
router) see each candidate agent's tool list, so a specialist whose
keywords loosely match an action it has no tool for ("guide me around
the app" hitting an HR specialist) gets penalized; the general
assistant with `uiDescribe` / `uiClick` / `uiNarrate` wins. Tool-fit
mismatch drops fitScore by 0.4+; total tool gap routes to the GA with
`turnMode=escalation_notice`.

**Conversational continuity.** The conductor receives an explicit
`lastResponder` input (computed in `conductor_consult.go` from the
transcript -- the most-recent SI participant to speak before this
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
once). The greeting directive is "familiar" by default for ALL
agents -- every agent in CoPresent is one the user created and
named themselves, so the directive forbids the "Hi, I'm X" opener
across the board.

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

### Coding Agent (OpenClaw / NemoClaw)
- **Currently:** OpenClaw (MIT license) - Open-source SI coding/automation agent, hardened
  - Pinned to v2026.2.26+ (patches CVE-2026-25253: 1-click RCE)
  - Gateway bound to internal network only, community skills disabled
  - Shared instance for all agents, each with isolated workspace
  - Per-agent workspaces at `/workspaces/{agentId}/`
  - SI calls routed through memQL's centralized provider system
- **Upgrade path:** NVIDIA NemoClaw (Apache 2.0) adds OpenShell sandboxing — swap image when container is published
- **Development:** Docker Compose overlay (`docker-compose.nemoclaw.yml`, port 18789)
- **Cloud:** runs as a sidecar container alongside the agent node on AKS
- **Agent capability:** `claw` flag on agent concept enables coding tools
- **Tools:** `clawExecuteTask`, `clawReadFile`, `clawListFiles`, `clawSearchCode` (claw coding-agent tool surface; defined alongside the agent tool definitions)

### Workers (computer_use_headless / computer_use_embodied)

The "workers" feature lets agents drive the user's own machine
via a tool surface: shell exec, filesystem, HTTP fetch, and (under
the GUI build) mouse + keyboard + screenshot. All seven phases of
the implementation plan have shipped (see
[docs/public/operate/workers-runbook.md](docs/public/operate/workers-runbook.md)); the plan
document itself is gone per the no-stale-docs convention.

The legacy umbrella slug `computer_use` was split into two
mode-specific slugs on 2026-05-17 so the headless slice (shell /
fs / http on the user's machine) and the embodied slice (GUI on
the user's machine) can be granted independently. Authorization
(scope grants, kill switch, knowledge domain) stays unified --
both modes act on the user's machine, so the consent is one
decision. See `component/memql/operator_caps.go` for the slug
expansion map. The sandboxed first-choice surface for headless
work is the Workbench, documented in the next section.

- **Agent capabilities (split slugs):**
  - `computer_use_headless` -- expands to `workerHost` + the
    cross-cutting trio (`workerStatus`, `requestComputerUseScope`,
    `canvasPublish`). Shell / fs / http on the user's machine.
  - `computer_use_embodied` -- expands to `workerComputer` + the
    same cross-cutting trio. Mouse / keyboard / screenshot on the
    user's machine.
- **Tools:** `workerHost` (HEADLESS) and `workerComputer` (GUI),
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
  (`make cockpit` / `make cockpit-gui`). The GUI build wraps RobotGo
  for screenshot + mouse + keyboard. macOS TCC / Linux X11 pre-flight
  via `memql-cockpit-gui worker setup`.
- **Per-user routing:** every worker is owned by exactly one
  v1:identity:user; agents in that user's sessions are the only
  callers admitted by the registry.
- **Permission model (Q9):** three layers checked BEFORE dispatch
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
- **Frontend:** `?panel=workers` in copresent shows the
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
(macOS-only tooling, GUI control, files already on the user's
computer).

See [docs/public/operate/workbench-runbook.md](docs/public/operate/workbench-runbook.md) for the
MVP test path and [docs/internal/ops/workbench-production.md](docs/internal/ops/workbench-production.md)
for the cluster-mode deployment plan (deferred until
production cutover).

- **Agent capability:** `workbench_use` slug. Universal --
  injected into every role's `lockedToolSlugs` so newly-created
  agents always have it. No scope grants, no kill switch, no
  per-agent gating; the blast radius is contained to the per-Plan
  directory tree.
- **Tools:** `workbenchHost` (discriminated by `action`: exec /
  fs_read / fs_write / fs_list / fs_stat / http_fetch). Lives in
  `dsl/copresent/tools.memql`; the wire path goes through the
  `workbenchDispatchHost` builtin in `dsl/workbench/builtins.memql`
  to `integration.workbench.dispatchHost`.
- **Per-Plan workspace:** filesystem state lives under
  `MEMQL_WORKBENCH_ROOT/{planId}/` (default
  `/var/lib/memql/workbenches/`). Lazy-provisioned on first call.
  Persists across calls within a Plan so multi-Task agents can
  share files; torn down on Plan terminal status via the
  `releaseWorkspaceOnPlanTerminal` automation calling the
  `workbenchTeardownDirectory` builtin.
- **Concept:** `v1:workbench:workspace` -- per-Plan row carrying
  status (provisioned / released), storageRoot, lifecycle
  timestamps. Defined in `dsl/workbench/concepts.memql`. The
  current MVP integration does not write the concept row from Go
  (lifecycle tracking is in-process + on-disk); the cross-node
  version exercises it.
- **Modes:**
  - **Single-node (MVP, default):** the agent node runs the
    workbench integration in-process. Workspaces live on the agent
    container's disk. Toggle: `MEMQL_WORKBENCH_REMOTE` unset or
    falsy.
  - **Cluster mode (future production):** a dedicated `workbench`
    node-type binary (`make workbench`) hosts the workspaces.
    Agent nodes route via `NodeService.Stream`
    (`WorkbenchForwardRequest` / `WorkbenchForwardResponse`).
    Toggle: `MEMQL_WORKBENCH_REMOTE=1` on agent nodes +
    `MEMQL_WORKER_PEERS=workbench=<addr>` for the dialer. See
    `docs/internal/ops/workbench-production.md`.
- **Routing preference:** the agent's prompt template
  (`dsl/copresent/prompts/agentReply.tmpl`) and the workbench
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

- Magic-link auth as the primary login path.
- OAuth-style token endpoints (`/oauth/token`, `/auth/refresh`).
- The JWKS feed at `/.well-known/jwks.json`.
- A public web UI (`/auth/login`, `/auth/complete`, `/setup`,
  `/legal/*`, `/me/*`).
- An admin web app at `/admin/*` (users, sessions, audit, JWKS,
  cluster settings, partition management).
- Personal Access Token (PAT) issuance for CLI clients
  (`mql_pat_<...>`).

Other binaries (bff / voice / cognition / agent / planner) verify
identity-issued JWTs locally via the per-node verifier
(`component/identity/verifier`), which fetches the JWKS document
on a 5-min background refresh and on demand for unknown `kid`
headers. They never see the private key.

`IDENTITY_VERIFIER_BASE_URL` configures the verifier;
`IDENTITY_BASE_URL` configures the identity service itself. See
[docs/public/operate/auth/identity-service.md](docs/public/operate/auth/identity-service.md) for
the operator-side narrative.

See [docs/public/operate/auth/](docs/public/operate/auth/):
- [access-model.md](docs/public/operate/auth/access-model.md) -- enforcement
  layers and role spectrum.
- [user-provisioning.md](docs/public/operate/auth/user-provisioning.md) --
  registration modes and magic-link flow.
- [identity-service.md](docs/public/operate/auth/identity-service.md) --
  operator-side env vars + key management.
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
`dsl/providers/providers.memql`). This replaces the older
per-construct + per-namespace nested skeleton (the retired
`dsl/<version>/<type>/<version>/<namespace>/...` tree). The flattened tree is produced
by the [`scripts/restructure-by-construct`](scripts/restructure-by-construct/main.go)
regenerator. Authoring reference skeletons live under
`dsl/_reference/` (`_concept`, `_shape`, `_spec`, `_trait`, `_agent`).
The per-type Go packages still expose embedded FS variables, but
loaders read through `Source()`, which routes through
[`component/memql/dslfs`](component/memql/dslfs/dslfs.go).

### `MEMQL_DSL_PATH` override

When `MEMQL_DSL_PATH` is unset, the binary reads its baked-in embedded
tree. When `MEMQL_DSL_PATH=/path/to/dsl-root` is set and the root
contains a namespace sub-directory with the expected
`<construct>s.memql` files (e.g. `<root>/cognition/queries.memql`),
that tree reads from disk instead of the embedded copy. Per-namespace
partial overrides are supported — missing-from-disk constructs fall
back to embedded.

Use cases:
- Dev hacking — point at the in-tree `dsl/` so a restart picks up
  edits without rebuilding the binary.
- Per-deploy patches — overlay a small subset on top of an immutable
  image.
- Tests / fixtures — pin loaders to a fixture tree.

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
   │ @row /  │ │ inserts │ │ Go-backed│ │ SI vendor  │
   │ @actor  │ │ on rows │ │ executors│ │ + model    │
   │ + traits│ │         │ │          │ │            │
   └────┬────┘ └─────────┘ └──────────┘ └────────────┘
        │                                       │
        ▼                                       ▼
   ┌─────────┐                              ┌────────┐
   │  Specs  │                              │Prompts │
   │@shape→  │                              │tmpl +  │
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
           │ event →    │◄───┤ SI-callable│
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
  `statusRowTrait`, etc.). The legacy `@concepts(...)` binding
  annotation is retired. Shapes can `include` other shapes for
  composition + aliasing.
- **Specs** are atomic boolean predicates. The eval strategy is
  derived from the spec body's field references (`@shape("name")` is
  an optional pin, not required):
  - body references `payload.X` / row intrinsics → spec compiles to a
    SQL `WHERE` fragment.
  - body references `actor.X` → spec evaluates in-process against the
    auth-context envelope.
  - mixed (row + actor) → spec compiles to SQL with actor fields
    bound as query parameters.
- **Mutations** write to concepts via the bare `insert { ... }` /
  `update { ... }` block (target from the signature). One write per
  body.
- **Builtins** wrap Go integrations behind a declarative schema, so
  they look like regular DSL function calls.
- **Providers** are SI vendor + model + auth records; **prompts**
  pin a default provider and pull rendered templates over it.
- **Queries** stitch concept + filter (specs) + projection (shapes)
  + args into a typed read. Phase B 2026-05: the struct form
  `query NAME { concept ... filter ... shape ... }` is the canonical
  shape; procedural `func (Query)` remains for queries that need
  branching or multi-step composition.
- **Automations** are event-triggered side-effects. They consume
  the layers above them and never the other way around.
- **Tools** are the SI-facing surface of queries + mutations +
  builtins. The tool loop binds tool-call args to handler args and
  forwards.
- **Policies** are empty-bodied SI provider-selection records
  (`@primary` / `@fallback` / `@maxLatencyMs` / `@preferredRole`),
  consumed by the SI Router to resolve a provider chain. (The
  retired decision-policy tier — caller-based authz / feature-gating
  decisions — is gone, #984; use **specs** via `spec("name")` for
  caller-context boolean checks.)

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
| Struct query / mutation | `args { ... }` sub-block inside the body |
| Procedural func / automation / policy | File-top `args { ... }` block above the `func (...)` |
| Builtin / tool / prompt | Body fields directly — the body IS the schema |

`args { ... }` field syntax: `<name> <type> [@required] [@enum("a", "b", ...)] [@default(<expr>)] [@description("...")]`. Omitting
`@required` makes the field optional.

**How args get read inside the body:**

| Name pattern | Source | Available in |
|---|---|---|
| `args.X` | Caller-passed arg declared in `args { ... }` | every body |
| `actor.X` | Resolved auth context (`userId`, `role`, `identityId`, `isClusterOwner`, `partitions`) | every body |
| `now` | RFC3339 timestamp captured at eval start | every body |
| `partition` | Active partition for this call | every body |
| `config.X` | Allow-listed config (`component/config/policy_exposable.go`) | every body |
| `payload.X`, `id`, `concept`, `type`, `createdAt`, `createdBy`, `schema` | Row fields / intrinsics | queries' `filter` + `shape` only (SQL pushdown) |

For automations, the triggering event is bound as `args`, so
`args.topic`, `args.kind`, and `args.payload.<field>` are how you
reach the event from within the automation body.

**Reserved engine names.** `now`, `actor`, `partition`, `config`,
`trace` are reserved as top-level identifiers. An `args` field that
collides with one of these names is rejected at load time — keeps
the resolution rules unambiguous.

**Procedural form (internal only).** The struct-form rewriter
expands every author-side block to a `func (Receiver) NAME(ctx any)
(any, error) { return <expr>, nil }` shape for the engine's parser.
The `ctx` parameter name is a placeholder identifier only -- the
body references `args.X` directly (the parser recognises both
`args.X` and `ctx.X` and resolves them to the same caller-arg AST
node). Authors should never write the procedural shape directly --
use the struct form (`query NAME { args { ... } filter ... shape
... }`, `logic NAME { args { ... } body { ... return <expr> } }`,
etc.).

**Receivers exempt entirely:**
- **Specs** keep their `bool` return — they compile into SQL filter
  predicates (row-specs) or evaluate in-process against caller fields
  (context-specs). No ctx envelope, no return wrapping.
- **Declarative receivers** (Tool / Prompt / Provider / Shape /
  Builtin) have no procedural body — they are schema declarations
  consumed by the engine. No ctx envelope.

## Policies

The live `policy` construct is an **SI provider-selection record**:
empty-bodied, annotated with `@primary` / `@fallback` /
`@maxLatencyMs` / `@preferredRole`, consolidated in
`dsl/policies/policies.memql` and consumed by the SI Router to pick
chat/voice/embedding providers. That is the only policy surface with
live constructs.

```memql
@primary("streamClaudeSonnet")
@fallback("stream54Pro")
@description("Default chat policy for non-operator agents.")
policy balancedChat { }
```

**Decision-policy tier — RETIRED (#984).** The cross-cutting
decision model (`func (Policy) { @tier / @audited / @traces_persisted
... if policy(...){} if spec(...){} }`, `engine.EvaluatePolicy`,
core/bff tiering) was documented + wired but carried **zero live
constructs** across the entire tree, so it has been retired. Auth /
feature-gating / vendor decisions live in Go (`component/safety`
ships the #231 risk×scope decision matrix) and in **specs** — use
`spec("name")` for caller-based boolean checks (admin / owner /
permission), which run as in-process context-specs and compile to
SQL or evaluate against the auth envelope.

> Cleanup status: fully removed. The dead tooling (`make
> policies-lint` / `policies-trace` + `scripts/policies/`) went in the
> first pass; the Go machinery, gRPC handler, proto messages, and TS
> SDK helper went in the second (#984 Phase 2). The shared expression-
> evaluation helpers the live spec evaluator depended on were lifted
> into `component/memql/expression_evaluator.go`; `policy_evaluator.go`
> / `policy_function_loader.go` / the `EvaluatePolicy` RPC handler /
> the `EvaluatePolicyMsg` + `EvaluatePolicyResult` proto messages (oneof
> 70 / 110, now `reserved`) / the TS `evaluatePolicy` helper are gone.
> Verified end-to-end: zero `func (Policy)` constructs in the tree and
> zero `EvaluatePolicy` callers across copresent, the bff, cockpit, and
> the SDK.

## Key Concepts

### Authorization model

Per-row authorization is the only gate (see
[docs/public/operate/auth/per-row-authz-audit.md](docs/public/operate/auth/per-row-authz-audit.md)).
Every query and mutation in the DSL classifies as **owned** (filter
on `payload.ownerUserId == actor.userId`), **granted** (relationship
predicate gates on actor.userId), **admin** (cluster-owner spec), or
**public** (`@public` annotation). The classification test in
`dsl/conformance_test.go` hard-fails on any new unclassified
construct.

The partition dimension that historically gated tenant isolation is
retired in #56 (phases 1-7 of which are landed; phase 8 sweeps the
remaining cross-repo wire stragglers). `envelope.partition` is still
on the gRPC wire as a no-op until phase 8.

### Concepts
Schemas for nodes (like tables in SQL).

```memql
concept agent {
  ownerUserId  string  @required
  // ...
}
```

### Nodes
Individual records with time-series history. IDs are
`{concept}:{shortId}`:

```
v1:common:agent:a9f3b7c2...
v1:cluster:node:bff-local
```

### Automations
Event-driven workflows. Trigger patterns key off concept:

```memql
@trigger(event="graph.node.created.*.v1:cognition:participant")
automation autoJoinSI { ... }
```

> **#56 phase 8 caveat:** the event topic still embeds a partition
> segment (`graph.node.created.{partition}.{concept}`), which is why
> trigger patterns carry a `.*.` wildcard between `created` and the
> concept name. That segment goes away in phase 8.

### Functions
Reusable query and mutation functions. Both default to the struct
form. Procedural `func (Query|Mutation) NAME(ctx any) (any, error)`
is reserved for functions that need branching or multi-step
composition.

**Concept binding lives in the construct signature** (locked in
2026-05 via the import-model pivot; PR #47 / #48 / #49). The
two-identifier signature `query <Concept> <name>`,
`mutation <Concept> <name>`, `seed <Concept> <name>`, and
`shape <Concept> <name>` names the bound concept directly; the
loader resolves the concept name through the file's file-top
imports. The legacy per-construct `@useConcept(<name>)` annotation
is retired and rejected at parse time.

**Cross-file dependencies go through file-top `use` imports.** Every
construct another file pulls into local scope (shapes, traits,
specs, mutations, queries, logic, builtins, prompts, providers,
tools) is declared via a dotted-path import:

```memql
use cognition.concepts.{ participant, space }
use cognition.shapes.{ participantFull }
use common.traits.{ traitIsActiveRecord, traitIsNotDeleted }
```

The dotted path maps to a file on disk (`cognition.concepts` →
`dsl/cognition/concepts.memql`); the brace-list names the
constructs imported into local scope. The legacy `@use*`
annotation family (`@useConcept`, `@useShape`, `@useQuery`,
`@useMutation`, `@useLogic`, `@useBuiltin`, etc.) is retired and
rejected at parse time with a migration-pointing error.

The bound concept's payload is referenced from filter clauses as
`payload.<field>` and from mutation bodies via the bare
`insert { ... }` / `update { ... }` block without re-stating the
concept id.

**Canonical filter-clause syntax** (enforced by
`dsl/conformance_test.go`):

- Payload fields: `payload.<field>` — never `<conceptName>.<field>`
- Intrinsics (`id`, `concept`, `createdAt`, `createdBy`,
  `partition`, `type`, `schema`): bare names
- **One Go boolean grammar** (operator standardization #971): `&&`
  (AND), `||` (OR), `!` (NOT), parens `( )` with Go precedence
  (`!` > comparisons > `&&` > `||`). The legacy `;`-AND and `,`-OR
  separators are retired in authored filters and rejected by the
  conformance test (`TestNoRetiredOperatorForms`).
- Membership is the single `in` operator: `args.x in payload.list`
  or `payload.kind in ["a", "b"]`. `has` (its reverse) is retired.
- Arg-conditional predicates use the `when(args.x) { <expr> }` guard:
  if `args.x` is absent the guarded block AND its connective are
  dropped as if never written (unambiguous under `||`). The `?.`
  optional-chain prefix it replaces is retired.
- When a trait spec covers the predicate (e.g. `traitIsActiveRecord`
  for `payload.active==true`), the trait is mandatory. Inline
  `payload.active==true` / `payload.deleted==false` are rejected
  by the conformance test.

**Argument resolution.** Caller-passed args declared in the
`args { ... }` block are referenced as `args.X` in the body. The
`ctx` envelope is gone from the author surface — no `ctx.input.X`,
no `ctx.X` shorthand. Engine-provided values (`now`, `actor.X`,
`partition`, `config.X`) are bare top-level names; an arg whose
name collides with one of those is rejected at load time.

**Annotations** in the args block:
- `@required` — non-optional
- `@enum("a", "b", "c")` — restricts to a value set
- `@description("...")`, `@maxLength(N)`, `@pattern("re")`
- `@default` is **not** valid on an args field (it was never applied —
  rejected at load, #991). Apply a default in the body via
  `coalesce(args.X, <default>)`, or use a concept-field `@default`
  (those ARE honored on insert).

Queries:
```memql
use cognition.concepts.{ participant }
use cognition.shapes.{ participantFull }
use common.traits.{ traitIsActiveRecord }

@description("Get space participants")
query participant querySpaceParticipants {
  args {
    spaceId  string  @required
  }
  filter  payload.spaceId==args.spaceId && traitIsActiveRecord
  shape   participantFull
}
```

Mutations:
```memql
use cognition.concepts.{ space }

@description("Create a cognition space")
mutation space mutationCreateSpace {
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
SI prompt templates with input schemas and default providers. Struct
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
Two legacy forms are retired (both rejected at parse time):
- `func (Prompt) name(ctx any) { ... }` — receiver-function wrapping.
- `@input { ... }` — body-level wrapper around the field list.

### Providers
SI provider configurations (OpenAI, Anthropic, Google, Groq, Mistral,
xAI). Struct form, mirrors concepts / shapes / tools.
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
    apiKey  env("MEMQL_SI_OPENAI_API_KEY")
  }
}
```
The legacy `func (Provider) name { ... }` form is retired; the
parser rejects it with a migration hint.

**Lifecycle annotations (`@enabled` / `@disabled`).** Providers accept
the same lifecycle flags as functions / builtins / prompts / specs /
seeds. `@enabled` is the explicit-on default (a no-op). `@disabled`
skips the provider at load -- it is **not registered and no auth
resolution is attempted**, so it emits zero "registered as unavailable"
warnings while staying in the tree for a future re-enable. `@disabled`
on a `@base` **propagates**: every child that `@extends` it is skipped
too. Use it to turn a keyless vendor lane off cleanly (e.g. mark the
`google` / `groq` / `mistral` `@base` `@disabled` until their
`MEMQL_SI_*_API_KEY` is seeded). Dependents degrade gracefully -- a
policy whose `@primary` is disabled routes via its `@fallback`; a prompt
whose `@defaultProvider` is disabled falls back to the default.

```memql
@disabled
@base
@type("Google")
provider google {
  auth { apiKey env("MEMQL_SI_GOOGLE_API_KEY") }
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
import); the legacy `@concepts("v1:...")` binding annotation is retired
and rejected at load:
```memql
use cognition.concepts.{ space }

@description("Space summary card")
@row
shape space spaceCard {
  row.id
  row.payload.name
  row.payload.description
  row.createdAt
}
```

**Actor shapes** project the engine envelope (the authenticated
actor + engine timestamp + allow-listed config). They carry no
signature concept. Closed field set:
`actor.userId` / `actor.role` / `actor.identityId` /
`actor.isClusterOwner` / `actor.now` /
`actor.config.<allow-listed-key>`.
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
`row.payload.createdBy == actor.userId`). The row concept is
signature-bound:
```memql
use cognition.concepts.{ space }

@row
@actor
shape space ownedSpace {
  row.id
  row.payload.ownerId
  actor.userId
}
```

**Composition.** A shape can `include` another shape; transitive
inclusion is supported, cycles + field collisions are errors. Pure
aliasing is just a shape whose body is a single `include` line:
```memql
@row
shape space spaceCardAlias {
  include spaceCard
}
```

No `func`, no `@template`, no `node("…")` wrapping. Shapes have no
inputs and no return; the body is a path list (+ optional `include`
statements).

### Specs
Atomic boolean predicates — struct form, mirrors how concepts +
shapes read. The body is a single boolean expression. The engine
classifies a spec by walking its field references and picks the
evaluation strategy:

- **Row-specs** reference `payload.X` and/or row intrinsics
  (`id`, `concept`, `type`, `createdAt`, `createdBy`, `schema`).
  They compile into a SQL `WHERE` fragment and push down to the
  database for filtering.
- **Context-specs** reference `actor.X` only (e.g. `actor.role`,
  `actor.isClusterOwner`). They evaluate in-process; called from
  policies via `spec("name")` for actor-based checks like "is
  admin," "owns partition," etc.

Bodies that mix both flavors are rejected at load time.

**`@shape("name")` is optional** on a spec (only a handful of specs
carry it — the eval strategy is derived from the spec body's field
references, not the shape). When present it documents/pins the shape
the predicate reads: concept-specific specs pin a concept-bound `@row`
shape; actor-side specs an `@actor` shape; cross-concept specs a
**trait shape** (a `@row` shape, signature-bound to its concept — the
predicate scaffolds in `dsl/common/shapes.memql`: `activeRowTrait`,
`statusRowTrait`, `deletedRowTrait`, `archivedRowTrait`,
`savedRowTrait`, `validationRowTrait`).

```memql
@enabled
@description("Matches participants with human participantType")
@shape("participantFull")
spec specIsHumanParticipant {
  payload.participantType == "human"            // row-spec
}

@enabled
@description("Actor holds an admin or owner role")
@shape("actorEnvelope")
spec requiresAdmin {
  actor.role == "admin"                         // context-spec
}

@enabled
@description("Matches records with active==true field")
@shape("activeRowTrait")
spec specIsActiveRecord {
  payload.active == true                        // cross-concept row-spec
}
```
Specs are exempt from the ctx-envelope return contract that queries /
mutations / policies / automations follow — no `ctx`, no `return`,
no parameter. The legacy
`func (Spec) name(ctx any) bool { return <expr> }` form is retired
and rejected at parse time.

**Caller-context checks use specs, not policies.** The decision-policy
tier that once hosted caller-based boolean predicates is retired
(#984). Author the predicate as a context-spec in
`dsl/<namespace>/specs.memql` and call it via `spec("name")`; the live
`policy` construct is provider-selection only.

### Tools
SI-callable tool definitions — struct form, mirrors how concepts +
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
The legacy `func (Tool)` form is retired; the parser rejects it
with a migration hint.

### Integration Capabilities
Go-backed operations callable from the DSL via
`@executor("integration.X.Y")`. Struct form, mirrors concepts /
shapes / tools / providers / prompts. The body's field list is the
builtin's input schema; the actual implementation is the Go
integration named by `@executor`.
```memql
@enabled
@description("Score an utterance for an SI participant")
@executor("integration.cognition.scoreUtterance")
@args(profile="object")
builtin cognitionScore {
  spaceId        string  @required
  participantId  string  @required
  utterance      string  @required
}
```
The legacy `func (Builtin) name { ... }` form is retired; the
parser rejects it with a migration hint.

Available integrations (core, registered via the plug-in system):
auth, database, email, embedding, files, gcs (as `storage`), identity,
knowledge, liveavatar, router, similarity, training, plus node-type-
scoped ones (cognition, agent, stt, openaiVoice, deepgram) wired
explicitly in `app/integrations_*.go` when their dependencies sit
outside the stable `PluginContext` surface.

### Extension Points

Three ways to extend memQL, in preference order:

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

Routing + concept ownership are also plug-in-registerable:
`node.RegisterRoutingRule(...)` for event routing patterns,
`node.RegisterConceptOwnership(prefix, nodeType)` for which node type
handles queries against a given concept prefix. Both called from
`init()` (see `integrations/copresent/routing.go` for an example).

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
- `v1:platform:partition` -- Data isolation boundary (standard, dedicated, personal)

### Cluster Concepts
Distributed node system metadata (dsl/cluster/concepts.memql)
- `v1:cluster:node` -- Registered node in the cluster
- `v1:cluster:nodeType` -- Node type definition (bff, voice, cognition, agent, planner). Optional `codeReference` field links this row to its architecture-model service id (consumed by the cockpit's Topology drill-down).
- `v1:cluster:spawnEvent` -- Lifecycle event for node state transitions (legacy name)
- `v1:cluster:cluster`, `v1:cluster:database`, `v1:cluster:identityProvider` -- topology bookkeeping

### Observability Concepts
Runtime side of the architecture framework (dsl/observability/, all `@scope("global")`).
See [docs/internal/design/auto-generated-diagrams.md](docs/internal/design/auto-generated-diagrams.md) for the full design.
- `v1:observability:codeProfile` -- live per-FQN verbosity override. CDC events feed the observe runtime's in-process cache via `CodeProfileSubscriber`.
- `v1:observability:invocation` -- per-call records backed by the `code_invocation` TimescaleDB hypertable.
- `v1:observability:codeMetric` -- per-(FQN, window) aggregates backed by the `code_invocation_1m` / `_1h` continuous aggregates. Drives the cockpit Topology overlay (n / p95 / err% per node).

### Identity Concepts
Auth + access metadata (dsl/identity/concepts.memql, all `@scope("global")`)
- `v1:identity:user` -- the person; cluster-wide role (owner / admin / writer / reader); preferences (theme, archive retention, daily-space toggle, voice mode, CoPresent Control settings)
- `v1:identity:identity` -- a credential set owned by a user (magic-link verified email, oauth token, api key/PAT, service account, worker token)
- `v1:identity:authSession` -- per-token session record (used for revocation)
- `v1:identity:magiclink` -- single-use magic-link credential (token-hashed)
- `v1:identity:auditEvent` -- append-only audit trail for the identity service
- `v1:identity:accessRequest` -- waitlist-mode access request
- `v1:identity:partitionAccess` -- per-(user, partition) grant
- `v1:identity:invitation` -- token-hashed invitation credential for guest/user flows
- `v1:identity:delegation` -- agent acting through a user's identity (bounded role/scope/lifetime)

See [docs/public/operate/auth/access-model.md](docs/public/operate/auth/access-model.md) for the full model.

---

## Environments

| Environment | Database | Application | Developer Access | Purpose |
|-------------|----------|-------------|------------------|---------|
| **Development** | Docker (localhost) | Docker | All developers | Local development |
| **Staging** | Tiger Cloud (Timescale) | Azure Kubernetes Service | All developers | Integration testing |
| **Production** | Tiger Cloud (separate) | Azure Kubernetes Service | Senior/Lead only | Live system |

**Key Principle:** Development environment is completely isolated from staging and production databases.

---

## Feature Notes

### Canvas state (v1)

The CoPresent canvas (the center surface of every space) is now a
per-space immutable timeline of `v1:copresent:canvasState` rows. The
mutable-scene protocol that lived here previously (with the
`v1:copresent:canvas`, `v1:copresent:canvas:element`,
`v1:copresent:exhibit`, and `v1:copresent:scene:update` concepts and
their `presentExhibit` / `dismissExhibit` / `updateScene` mutations)
has been deleted -- those concepts had zero producers and zero
consumers and the new model picked one concept over four.

Schema highlights (the `v1:copresent:canvasState` concept body):

- `space` -- target `v1:cognition:space.id` (every viewer of that
  space gets the row, subject to visibility filtering).
- `kind` -- `card` | `document` | `dataview` | `graph`. Picks the
  frontend renderer.
- `data` -- per-kind shape (named `data` to avoid collision with
  the reserved `payload` intrinsic; gotcha #19).
- `visibility` -- `public` (every space participant) | `private`
  (only `forUserId`; always the space owner under v1 permission
  rules).
- `actor` -- `{kind: "agent"|"user"|"system", ...}`. Drives the
  frontend's CanvasAuthorBadge.
- `importance` -- `notify` (pings the canvas bell) | `ambient` (lands
  silently on the timeline). Cognition is the post-hoc authority and
  can promote / coalesce.

Two paths write canvasState rows:

1. **Tool path** (agent presentations, public visibility): the
   `canvas.publish` tool. Invoked by agents from inside their tool
   loop with `kind` + `data` + optional `importance` / `note`.
2. **Frontend direct mutation path** (owner-private welcome cards):
   the `createCanvasState` mutation. The CoPresent
   frontend calls this at the end of every create-modal flow --
   agent.created (AgentsListPanel), group.created (SettingsListPanel),
   and space.created (useCreateAndJoinSpace). All three use the same
   mutation and row shape; the only reason they live on the frontend
   instead of in automations is that they need to stamp the bare-form
   space id (matching what `setActiveSpace` leaves behind) and -- for
   agent / group -- they need active-space context the graph event
   doesn't carry.

Queries: `canvasStatesForSpace` (public), `privateCanvasStatesForViewer`
(per-viewer private). Two queries because the memql query parser
doesn't have an OR operator yet; the frontend merges the streams.
Shape: `canvasStateFull`.

Full design rationale (frontend + backend): copresent's
`docs/canvas/v1-plan.md`.

### Spaces (three-state lifecycle + daily spaces)

`v1:cognition:space` carries `status` ∈ {active, saved, archived,
scheduled} and a `kind` ∈ {regular, daily}.

- **active** -- working space, default state.
- **saved** -- user manually preserved. Never auto-deletes.
- **archived** -- hidden from the active list. `archivedAt` +
  `expiresAt` are stamped at archive time
  (`archivedAt + User.preferences.archiveRetentionDays`); the
  `purgeExpiredArchivedSpaces` cron (daily 02:00 UTC) hard-deletes
  rows whose `expiresAt < now`. The query is a plain expiresAt
  comparison -- no per-row user lookup at sweep time. Bumping
  `archiveRetentionDays` from 30 to 60 rescues currently-archived
  rows because the cron reads expiresAt that was stamped under the
  current preference.
- **scheduled** -- future-dated meeting. Untouched by the purge.

`kind=daily` is a per-user singleton provisioned client-side
(`useDailySpace` on CoPresent), keyed by `(userHash, dailyDateKey)`
where dateKey is computed in the user's local timezone. Daily spaces
are private, pinned at the top of the active list, and rolled over
each day per `User.preferences.dailySpaceRolloverAction`
(`archive` default, or `save`).

`User.preferences` carries the lifecycle controls: `timezone` (IANA
name), `archiveRetentionDays` (30 default, 60 picker), and
`dailySpaceEnabled` toggle. CoPresent Control settings persist server-
side too: `cursorTweenMs`, `takeoverMode` (clean / dim),
`interactivePace` (quick / steady / deliberate).

Mutations: `createSpace`, `archiveSpace`, `saveSpace`,
`restoreSpace`, `deleteSpaceNow`, `createDailySpace`. Queries:
`activeSpaces`, `savedSpaces`, `archivedSpaces`,
`expiredArchivedSpaces`, `allArchivedSpacesAcrossUsers`. All in
`dsl/cognition/mutations.memql` and `dsl/cognition/queries.memql`.

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
partition checks). The memQL WS bridge accepts the token as
`?guest_token=<token>` since browsers cannot set custom headers on
the WebSocket upgrade.

Shipped. Key files:
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

Product-specific mutations (creating product-scoped invitations,
joining spaces as a guest, participation lifecycle) live in
`dsl/cognition/mutations.memql`.

### Planner / Knowledge / Validation (v1)

Full schema surface for the brainstorm-locked v1 design. The
backing implementation ships incrementally; the schema is stable
so new features add fields/automations without migrations.

**Concepts**:

- `v1:planner:plan` -- a user-visible unit of work. Carries
  parentPlanId (sub-plan nesting), kind, status (queued / routing
  / running / paused / awaitingFeedback / needsAgent / succeeded
  / failed / cancelled), goal, ownerAgentId, requestedBy,
  triggerSource, recommendationCardId, input / output,
  refinementContext, phases[] (Q3 outline+per-phase),
  estimate (Q5), tokenBudget / tokenSpent / tokenAllocatedToChildren
  / tokenCapDisabled (Q6), metrics (Q7), pause + feedback +
  chat-anchor bookkeeping.
- `v1:planner:task` -- one executable step inside a Plan, never
  recursive. Carries phase tag, executionSurface (inProcess /
  containerExecutor) + executorBackend (Q13), metrics, parking
  fields.
- `v1:agents:agentAuthorization` -- standing authorization
  per Q4 tiered-trust model.
- `v1:planner:taskState` -- persisted Task working state for
  async parking + planner re-invocation (Q18).
- `v1:knowledge:document` -- container/manifest for analyzed user
  files (Q8). Owns attached-domain list, validation rollup,
  supersession back-pointers, lazy-embedding status.
- `v1:knowledge:spreadsheetRow`, `v1:knowledge:imageRegion` --
  typed per-format item concepts (Q8). Native column predicates
  for spreadsheet rows; bbox + caption + embedding for images.
- `v1:knowledge:validationEvent` -- append-only audit log for
  every validation transition (Q15).
- `v1:knowledge:domainEntitySchema` -- per-domain entity schema
  for cross-file dedup (Q17). Inferred on second-Document
  trigger; user-confirmed once.
- `v1:knowledge:entityIndex` -- the dedup lookup table keyed by
  sha256(normalized key field values) (Q17/Q26). Force-add escape
  valve for entity-schema misfires.

Plus expanded existing concepts: `v1:common:knowledgeDomain`
gains scope (workspace + private per Q21) + ownerId;
`v1:common:documentChunk` gains documentId + validationStatus.

**Frontend surfaces** (in copresent):

- Right-panel views: `?panel=knowledge` (KnowledgeListPanel),
  `?panel=tasks` (TasksListPanel). Header nav has 5 tiles now:
  Spaces / Agents / Knowledge / Tasks / Settings.
- Canvas card variants: `plan.created`, `plan.completed`
  (with Validate / Reject / Attach-to-domain / Refine actions),
  `plan.needsAgent` (with [Create X agent] / [Assign to ▾]),
  `plan.awaitingFeedback` (with per-kind response widgets).
- Knowledge page: two-column layout, single-step create-domain
  flow with workspace/private scope picker, drag-onto-row file
  upload, attached-Document list per selected domain.

**Wiring path (v0.x synchronous)**: HTTP attachment upload ->
existing TextExtractor + AISummarizer -> `EnginePlanStore.
CreateAndCompleteAnalyzePlan` chains:
  1. mutationCreatePlan (queued)
  2. mutationCreateCanvasState (plan.created card)
  3. mutationCreateTask (queued)
  4. mutationUpdatePlanStatus + mutationUpdateTaskStatus to
     running, then succeeded with output payloads
  5. mutationCreateDocument (the v1:knowledge:document container)
  6. mutationCreateCanvasState (plan.completed card with
     documentId so the card actions target the right row)

Refinement child Plans (kind='refineAnalysis') spawned by the
front-end Refine action are picked up by the
`handleRefinementPlan` automation (triggers on
graph.node.created.*.v1:planner:plan filtered by
payload.kind=='refineAnalysis'); the automation drives the child
Plan through queued -> running -> succeeded and emits its own
plan.completed card. v0.x acknowledges feedback as the result;
LLM-backed re-analysis ships with the async planner integration.

**Now shipped (subsequent-rounds work landed)**:

- **Async planner activation** -- the attachment HTTP handler
  creates the queued Plan + plan.created card synchronously, then
  launches a detached goroutine that runs extract + summarize +
  CompleteAnalyzePlan with a background context. User sees instant
  acknowledgement; plan.completed card lands when work finishes.
  See `runAnalysisAsync` in `component/server/attachment_handler.go`.

- **Estimation system** -- heuristic estimate computed at Plan
  creation time (`heuristicEstimateAnalyzeFile` in plan_store.go)
  and stamped on `Plan.estimate` so the canvas card's estimate
  strip renders immediately. LLM-backed estimate via the
  `planEstimate` prompt template ships when the planner integration
  consumes it. Historical bucket query
  `queryHistoricalPlanMetrics` backs the blending logic.

- **LLM-backed refinement** -- the `refineAnalysis` prompt
  + handleRefinementPlan automation rewritten to call
  `si("refineAnalysis", ...)` against the parent's output.

- **Validation cascade** -- cascadeSupersession + cascadeValidationToItems
  automations propagate Document-level validation transitions to
  predecessor + per-row items.

- **Feedback timeout auto-pause** -- cron */5min cron automation
  scanning awaitingFeedback Plans whose timeoutAt has passed.

- **needsAgent re-route** -- triggers on agent creation with
  originatingPlanId set.

- **Chat completion subordinate line + auto-collapse** --
  ChatPlanCompletionLine front-end component mounted in ChatPanel.

- **Per-item validation drawer for spreadsheets** --
  SpreadsheetItemDrawer wired into PlanCompletedCard's "Review
  individual rows…" toggle.

- **Pause / Resume / Cancel** -- Tasks-page row controls wired
  to mutationUpdatePlanStatus.

- **Container-executor registry** -- `component/planner/executor.go`
  ships the `RegisterContainerExecutor` / `LookupContainerExecutor`
  pattern. NemoClaw + future homegrown variants register at
  init() time; the planner picks the backend via Task.executorBackend.

- **Token-budget enforcement** -- `component/planner/budget.go`
  ships the pre-call `CheckCall` helper. It is wired into the Planner
  Agent decompose loop (`integrations/planner/agent_loop_budget.go`,
  memql#819): before every `plannerAgent` call the loop checks a
  CUMULATIVE per-plan ceiling (`Plan.metrics.llmCallCount` + token
  budget, persisted so it survives across cycles/retries) and parks the
  Plan on exceed rather than making another LLM call. The 75%/90%
  soft-warning canvas cards remain the Go side's responsibility -- the
  original `tokenBudgetSoftWarning` automation was deleted because
  computing `spent / budget` needs arithmetic the MemQL parser doesn't
  support yet. NOTE: a deeper goal-resolution restructure (cost-aware
  routing, model tiering, up-front token estimate + user-approval
  threshold) is tracked in epic memql#836 -- the current cap bounds
  spend but does not make a trivial request cheap.

- **Cognition plan-triage prompt** --
  `dsl/cognition/prompts/cognitionPlanTriage.tmpl` (schema in
  `dsl/cognition/prompts.memql`) ships the per-message classification
  (needsPlan + planHint). Wiring
  into the live chat-message dispatch is the final integration
  step the planner-side handler will pick up.

- **Entity-schema inference prompt** --
  the `inferEntitySchema` prompt ships the
  per-domain schema proposal called by the entity-inference Plan
  on second-Document trigger.

**Planner Agent loop (shipped) + the goal-resolution restructure
(planned).** The planner-node-owned decompose loop exists
(`integrations/planner/agent_loop.go`): on a new userGoal Plan it
invokes the `plannerAgent` prompt, which emits a structured decision
(decompose / dispatchTask / createSpecialist / markPlanSucceeded /
escalate), and the loop dispatches it, re-invoking until terminal.
Safety guards are wired (memql#818): a cumulative per-plan LLM budget
(#819), a lean prompt projection (#820, strips role embeddings), 429
backoff (#821), a convergence/no-progress guard (#822), and a global
identical-request circuit breaker at the provider HTTP chokepoint
(#825, `component/memql/si_guard.go`).

Goal-resolution restructure (epic memql#836) -- SHIPPED. The cost-safety
structure is now in place: a hard process-wide LLM rate ceiling at the
provider HTTP chokepoint (#834, `si_guard.go`), complexity triage that
routes a trivial deliverable to ONE cheap path instead of the decompose
loop (#837), model tiering that defaults the planner to a cheap tier and
escalates to Opus+thinking only on an explicit stuck signal (#838), an
up-front token estimate + user-approval gate that parks an expensive plan
before it spends (#839), gated specialist creation/training so a one-off
never auto-trains (#842), phased execution with per-phase checkpoints
(#840), deterministic-first result verification (#841), and lowered +
binding per-plan caps with a no-task-markPlanSucceeded convergence guard
(#843).

`produceArtifact` (the conversational "make me a file" deliverable) now
flows through the unified loop (memql#835): its old hardcoded
HandlePlanCreated bypass was removed; it enters invokeAndDispatch where
the approval gate auto-runs it (tiny estimate) and the triage recognizes
it as a known single deliverable, shortcutting to ONE direct production
turn (`startPlanDirect` -> running -> the owning agent writes the file via
the workbench) -- ZERO plannerAgent decompose calls, exactly as the old
bypass did, but now as a first-class routing decision with the rate
ceiling + lowered caps + tiering as structural backstops. The earlier
#823 attempt was reverted (#832) precisely because those backstops did
not yet exist; they do now. The synchronous-in-handler path still covers
the analyzeFile case end-to-end.

## Need Help?

1. **Documentation:** Check [GLOSSARY.md](GLOSSARY.md)
2. **Quick start:** See [docs/public/overview/quickstart.md](docs/public/overview/quickstart.md)
3. **Logs:** `docker compose -f docker/docker-compose.full.yml logs -f`

---

## Notes for Claude Code CLI

- Each directory has a CLAUDE.md explaining its purpose
- Use GLOSSARY.md to find specific documentation
- Docker stack is self-contained (no manual setup needed)
- Migrations run automatically on startup

### Makefile + shell-script convention

The Makefile is for **simple commands and target wiring**. Anything
multi-step, conditional, or long enough to need line-continuations
gets extracted into a shell script under `scripts/` and the
Makefile target becomes a one-liner that calls it.

Concretely:

- **Stays inline in the Makefile:** single commands (`go build`,
  `go test`, `docker compose up`), short pipelines (~3 lines or
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
- **Source `scripts/<area>/lib.sh`** for shared functions
  (`check_docker`, `wait_for_memql`, etc.) so individual scripts
  stay focused.
- File extension `.sh`, executable (`chmod +x`).
- Named "shell scripts" in docs (the umbrella term); they're
  technically Bash scripts since we use `[[`, arrays, `function`
  keyword, etc.

Current example: `scripts/dev/{lib,refresh,status}.sh`
implements `make dev-refresh` and `make dev-status`. The
Makefile targets are one-liners (`bash scripts/dev/refresh.sh`).

### Documentation Style Guidelines

**No Emojis:** All documentation, skills, and CLI responses must use professional formatting without emojis. Use:
- Checkboxes: `[ ]` for unchecked, `[x]` for checked
- Text indicators: "SUCCESS:", "ERROR:", "WARNING:", "INFO:"
- Standard markdown formatting for emphasis
- This applies to: documentation files, skill outputs, CLI responses, and all user-facing text
