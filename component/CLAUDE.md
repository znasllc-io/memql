# Component Directory

**Purpose:** Core Go service components
**Language:** Go
**Type:** Reusable service modules (database, server, auth, etc.)

---

## STRUCTURE Directory Structure

```
component/
├── CLAUDE.md           # This file
├── component.go       # Component wiring and lifecycle
├── memql/             # Core MemQL query engine
│   └── sense/         # MemQL Sense language intelligence (tokenize, complete, diagnose, hover, signature)
├── database/          # Database providers
│   └── memory-nodes/  # PostgreSQL + TimescaleDB
├── server/            # HTTP/WebSocket servers
│   ├── memqlws/       # MemQL WebSocket
│   ├── audiows/       # Audio WebSocket
│   └── polyphonws/    # Polyphon WebSocket (multi-agent voice)
├── auth/              # Shared auth context helpers + RBAC + delegation
├── identity/          # In-house identity service (magic-link, JWT, JWKS, admin UI, PAT)
│   └── verifier/      # Per-node JWT verifier (used by bff/voice/cognition/agent/planner)
├── polyphon/          # Polyphon multi-agent voice pipeline
├── fileprocessor/     # File processing (PDF, DOCX, images, text)
├── cache/             # Caching layer
├── bus/               # Channel-based inter-component communication
│   └── gen/           # Generated protobuf code (bus.proto)
├── config/            # Centralized configuration loading
├── events/            # Event bus
├── grpc/              # gRPC server (MemqlService)
├── node/              # Distributed node system
│   └── gen/           # Generated gRPC code (NodeService)
├── language/          # MemQL parser/compiler
└── service/           # Service utilities
```

---

## TASKS Key Components

### bus/ - **Component Communication Bus**
**Purpose:** Channel-based inter-component communication with protobuf messages

**Key Files:**
- `bus.proto` - Protobuf definitions for all internal messages (27 types)
- `channel.go` - Generic `Channel[T]` wrapper with telemetry hooks
- `replyto.go` - Request-response pattern over channels (ReplyTo)
- `wiring.go` - `Wiring` registry holding all typed channels
- `message.go` - Message factory with correlation ID tracking
- `telemetry.go` - Channel metrics collection (fill-level, send/drop counts)

### config/ - **Configuration Loading**
**Purpose:** Centralized env var loading into `ConfigSnapshot` protobuf

**Key Files:**
- `config.go` - Reads all env vars at startup, implements Dependency interface

### memql/ - **Core Query Engine**
**Purpose:** Executes MemQL queries, manages automations and functions

**Key Files:**
- `engine.go` - Main query engine
- `engine_bus.go` - Channel-based request handler (Execute, RenderPrompt, ToolExec, VariableResolve, IntegrationDispatch)
- `executor.go` - Query execution logic
- `parser.go` - MemQL query parsing
- `function_loader.go` - Function loading and registration
- `ai_tool_loop.go` - AI tool calling loop (MCP integration)
- `ai_providers.go` - AI provider registry (OpenAI, Anthropic) with ChatAIProvider, VisionAIProvider, TTSAIProvider, ChatStreamProvider interfaces
- `integration_provider.go` - IntegrationProvider interface and IntegrationCapability struct
- `integration_registry.go` - Thread-safe registry for integration providers and capabilities
- `integration_engine.go` - IntegrationEngineAccess narrow interface for integrations
- `prompt_loader.go` - Loads Prompt definitions from `dsl/<ns>/prompts.memql` (template bodies under `dsl/<ns>/prompts/*.tmpl`)
- `provider_loader.go` - Loads Provider definitions from `dsl/providers/providers.memql`
- `shape_loader.go` - Loads Shape definitions sliced out of `dsl/<ns>/<construct>.memql`
- `arch.md` - Architecture documentation

**What It Does:**
- Parses and executes MemQL queries
- Manages automation lifecycle
- Registers and executes functions (Query, Mutation, Automation, Prompt, Provider, Shape)
- Loads prompts, providers, and shapes from `.memql` files
- Handles AI tool calling (MCP integration)

### database/memory-nodes/ - **Database Layer**
**Purpose:** PostgreSQL + TimescaleDB connection and migrations

**What It Does:**
- Partition-isolated data storage (PK: partition, id, createdAt)
- Database connection pooling
- Automatic migrations
- Health checks
- Query execution

### server/ - **HTTP & WebSocket Servers**
**Purpose:** Expose memQL via HTTP API and WebSocket connections

**Components:**
- `memqlws/` - MemQL query WebSocket
- `audiows/` - Real-time audio WebSocket
- `polyphonws/` - Polyphon multi-agent voice WebSocket
- HTTP handlers for REST API

### polyphon/ - **Multi-Agent Voice Pipeline**
**Purpose:** Polyphon voice orchestration for group conversations

**What It Does:**
- Score-engine-based turn management for multi-agent conversations
- Session lifecycle management
- ASR/TTS provider bridge (OpenAI); LLM provider bridge (OpenAI, Anthropic) for AI
- Turn policy and scoring

### AiSuggest extension point (suggest domains)

`AiSuggestMsg` dispatch lives on `MemqlService.Stream` (`grpc/ai_handlers.go`).
The handler is generic: it resolves a registered handler for the request
`domain` via `memql.RegisterSuggestDomain` / `LookupSuggestDomain`
(`component/memql/suggest_registry.go`) and runs the shared structured-output
path. The `knowledge` domain registers from core
(`component/memql/suggest_knowledge.go`); the product domains
(spaces / spaceTitle / agents / groups / groupDescription / *CardSummary /
guide) register from the product's own suggest plugin — a thin Go module
that self-registers via `RegisterSuggestDomain` from the product repo, so
engine-only core carries no product suggest helpers (memql#1959). Under the
consolidated platform (memql#2472) the engine ships product-agnostic and the
product's DSL rides in at runtime via `MEMQL_DSL_PATH`; the product suggest
handlers that still need Go remain a transitional product-repo plugin (not
core), pending engine-generic absorption or bundle delivery.

### fileprocessor/ - **File Processing**
**Purpose:** Extract content from uploaded files

**What It Does:**
- PDF text extraction
- DOCX text extraction
- Image description via VisionAIProvider interface (OpenAI/Anthropic)
- Plain text handling

### auth/ & identity/ - **Authentication**
**Purpose:** User authentication and authorization

**What It Does:**
- Identity-issued JWT validation via per-node verifier
  (`identity/verifier`) on every non-identity binary
- Magic-link auth, OAuth-style token endpoints, JWKS publishing,
  and the admin web app (the identity binary itself)
- Personal Access Token (PAT) issuance for CLI clients
- Identity / role context propagation (auth package helpers)
- Per-row authorization is enforced inside DSL queries + mutations
  (see docs/public/operate/auth/per-row-authz-audit.md)

### events/ - **Event Bus**
**Purpose:** Publish/subscribe event system for automations

**What It Does:**
- Event publishing (node created, updated, etc.)
- Event subscription
- Async event processing
- Event filtering

### cache/ - **Caching Layer**
**Purpose:** In-memory caching for performance

**What It Does:**
- TTL-based cache
- LRU eviction
- Cache invalidation

### language/ - **MemQL Language**
**Purpose:** Parser and compiler for MemQL DSL

**Components:**
- `parser/` - Lexer and parser
- `compiler/` - AST to execution plan

---

## CONFIG Component Architecture

```
┌─────────────────────────────────────────────┐
│            HTTP/gRPC API                    │
│         (server/, grpc/)                    │
└─────────────────┬───────────────────────────┘
                  │ EngineRequests channel
         ┌────────▼────────┐
         │  Component Bus  │ (bus/)
         │  (Wiring)       │ Typed Go channels + protobuf messages
         └────────┬────────┘
                  │
         ┌────────▼────────┐
         │  MemQL Engine   │
         │  (memql/)       │
         └────────┬────────┘
                  │
    ┌─────────────┼─────────────┐
    │             │             │
┌───▼────┐  ┌────▼────┐  ┌─────▼──────┐
│Database│  │ Events  │  │Integrations│
│(internal) │(EventPub│  │(IntReqs ch)│
└────────┘  │  ch)    │  └────────────┘
            └─────────┘
```

---

## START memQL Engine Deep Dive

The heart of the system - executes all MemQL queries.

### Key Responsibilities

1. **Query Execution**
   - Parse MemQL syntax
   - Compile to execution plan
   - Execute against database
   - Return shaped results

2. **Automation Management**
   - Load automations from disk
   - Register event triggers
   - Execute on events/schedule
   - Track execution history

3. **Function Registry**
   - Load functions from disk
   - Validate function signatures
   - Execute with type checking
   - Cache compiled functions

4. **AI Tool Integration**
   - Expose functions as MCP tools
   - Handle tool calling from AI
   - Bounded iteration loop
   - Error handling

### Performance Optimizations

- **Concept Cache** - Cache concept schemas
- **Function Compilation** - Compile functions once
- **Query Planning** - Optimize execution plans

---

## Component Interfaces

### Dependency Interface (all components)
```go
type Dependency interface {
    Start(ctx context.Context)
    Stop(ctx context.Context)
    IsRunning() bool
    Order() int
    ComponentName() ComponentName
    Ready() <-chan struct{} // Closed when component is ready (parallel startup)
}
```

### SetWiring Pattern
Components that participate in channel-based communication accept bus wiring:
```go
func (c *MyComponent) SetWiring(w *bus.Wiring) { c.wiring = w }
```
Components with SetWiring: Engine, gRPC Server, HTTP Server, Automations Scheduler, Node EventBridge.

### Engine Interface
```go
type MemQLEngine interface {
    Execute(ctx context.Context, query string) (any, error)
    InvokeAI(ctx context.Context, templateId string, data map[string]any) (any, error)
    InvokeAIChatWithTools(ctx context.Context, templateId string, data map[string]any) (string, error)
    RegisterIntegration(provider IntegrationProvider) error
    SetWiring(w *bus.Wiring) // Channel-based communication
}
```

### IntegrationEngineAccess Interface

Narrow interface for integrations -- excludes AI orchestration methods (InvokeAI,
InvokeAIChatWithTools). Integrations receive this instead of the full engine.

```go
type IntegrationEngineAccess interface {
    RegisterIntegration(provider IntegrationProvider) error
    Execute(ctx context.Context, query string) (any, error)
    RenderPrompt(templateId string, data map[string]any) (string, error)
    ChatStreamProvider() common.ChatStreamProvider
    ChatStreamProviderByName(name string) common.ChatStreamProvider
    ChatStreamWithToolsProviderByName(name string) common.ChatStreamWithToolsProvider
    ToolDefinitionsForNames(names []string) []common.ToolDefinition
    ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error)
}
```

### Database Interface
```go
type Database interface {
    Query(ctx context.Context, query string, args ...any) (Rows, error)
    Exec(ctx context.Context, query string, args ...any) (Result, error)
    Close() error
}
```

### Event Bus Interface
```go
type EventBus interface {
    Publish(ctx context.Context, event Event) error
    Subscribe(pattern string, handler EventHandler) (Unsubscribe, error)
}
```

---

## START Adding New Components

### 1. Create Component Package
```bash
mkdir -p component/<component-name>
```

### 2. Define Component
```go
// component/<name>/<name>.go
package <name>

type Component struct {
    // dependencies
    logger *slog.Logger
}

func New(logger *slog.Logger, ...) *Component {
    return &Component{
        logger: logger,
    }
}

func (c *Component) Start(ctx context.Context) error {
    c.logger.Info("component started")
    return nil
}

func (c *Component) Stop(ctx context.Context) error {
    c.logger.Info("component stopped")
    return nil
}
```

### 3. Wire in app/ bootstrap

New components are wired in the appropriate phase file in the `app/` package.
The bootstrap runs phases in order: config -> database -> engine -> integrations -> transport -> cluster.

```go
// In the relevant app/phase_*.go file (e.g., app/integrations.go):
myComponent := <name>.New(a.Logger, ...)
if err := a.engine.RegisterIntegration(myComponent); err != nil {
    a.fatal("failed to register <name> integration", "error", err)
}
a.Dependencies = append(a.Dependencies, myComponent)
```

The `app.Build()` function in `app/app.go` orchestrates all phases and returns
the final `App` with populated `Dependencies` slice. `main.go` calls `Build()`
and then starts/stops dependencies.

---

## Debugging Components

### Check Component Startup
```bash
kubectl logs -n memql deploy/bff | grep "component.*started"
```

### Watch Component Activity
```bash
# MemQL engine
kubectl logs -n memql deploy/bff -f | grep "memQLEngine"

# Database
kubectl logs -n memql deploy/bff -f | grep "memoryNodesDB"

# Events
kubectl logs -n memql deploy/bff -f | grep "eventBus"
```

### Performance Monitoring
```bash
# Cache hit rates
kubectl logs -n memql deploy/bff | grep "cache.*hit"

# Query performance
kubectl logs -n memql deploy/bff | grep "query.*ms"
```

---

## DOCS See Also

- [memql/arch.md](memql/arch.md) - MemQL engine architecture
- [Architecture Overview](../docs/public/concepts/architecture.md) - System architecture
- [MemQL Language](../docs/public/language/memql.md) - Query language
- [docs/public/overview/quickstart.md](../docs/public/overview/quickstart.md) -- Dev setup

---

## CHECK Key Components Reference

| Component | Purpose | Key Files |
|-----------|---------|-----------|
| **memql/** | Query engine | `engine.go`, `executor.go`, `parser.go`, `function_loader.go`, `ai_tool_loop.go` (AI tool loop), `ai_providers.go` (AI providers), `prompt_loader.go`, `provider_loader.go`, `shape_loader.go` |
| **database/** | Database layer | `memory-nodes/database.go` |
| **server/** | HTTP/WS servers | `server.go`, `memqlws/`, `audiows/`, `polyphonws/` |
| **auth/** | Auth context helpers + RBAC + delegation + identity resolver | `context.go`, `identity.go`, `rbac.go`, `security.go`, `identity_resolver.go` |
| **identity/** | In-house identity service (magic-link, JWT issuance, JWKS, admin UI, PAT) | `identity.go`, `keys.go`, `jwt.go`, `jwks.go`, `verifier/` (per-node verifier) |
| **polyphon/** | Voice pipeline | `cognition.go`, `session.go` |
| **fileprocessor/** | File processing | `processor.go` |
| **events/** | Event bus | `bus.go` |
| **cache/** | Caching | `cache.go` |
| **grpc/** | gRPC server | `server.go`, `ai_handlers.go`, `polyphon_handlers.go`, `concepts_handlers.go`, `sense_handlers.go` |
| **language/** | Parser/compiler | `parser/`, `compiler/` |
| **memql/sense/** | MemQL Sense language intelligence | `sense.go` (types), `tokenize.go`, `complete.go`, `diagnose.go`, `hover.go`, `signature.go`, `builtins.go`, `context.go` |

---

**For complete component documentation:** See component-specific arch.md files
