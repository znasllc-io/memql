---
title: Build Tags -- Node Type Binaries
audience: public
status: stable
area: build
sinceVersion: 0.9.0
owner: znas
---

# Build Tags -- Node Type Binaries

memQL uses Go build tags to compile separate binaries for each node type in the distributed cluster. Each binary includes only the code, integrations, and transport layers relevant to its purpose.

## Node Types

| Type | Tag | Purpose | Binary Size |
|------|-----|---------|-------------|
| **bff** | (none, default) | Backend for frontend | ~25 MB |
| **voice** | `voice` | Voice transport (audio WS, LiveKit) | ~30 MB |
| **cognition** | `cognition` | Cognition pipeline, Polyphon | ~35 MB |
| **agent** | `agent` | Task execution, SI work, tool calling | ~43 MB |
| **planner** | `planner` | Task planning and orchestration | ~25 MB |

## Building

```bash
# BFF (default, no tag needed)
go build -o bin/memql .

# Node-type-specific binaries
go build -tags voice -o bin/memql-voice .
go build -tags cognition -o bin/memql-cognition .
go build -tags agent -o bin/memql-agent .
go build -tags planner -o bin/memql-planner .
```

Tags are **mutually exclusive** -- never combine them (e.g., `-tags "bff cognition"`).

## Docker

```bash
# BFF (default)
docker build .

# Node type
docker build --build-arg BUILD_TAGS=voice .
docker build --build-arg BUILD_TAGS=cognition .
docker build --build-arg BUILD_TAGS=agent .
docker build --build-arg BUILD_TAGS=planner .
```

To build all node types:
```bash
go build .                                         # bff (default)
go build -tags voice .                             # voice
go build -tags cognition .                         # cognition
go build -tags agent .                             # agent
go build -tags planner .                           # planner
```

## Architecture

### How It Works

The `app/` package contains build-tagged files that control which bootstrap phases run:

```
app/
  app.go                        # Shared: App struct, newApp(), fatal()
  build_default.go              # Build() for default (BFF, no tag)
  build_bff.go                  # Build() for bff (explicit tag)
  build_cognition.go            # Build() for cognition
  build_agent.go                # Build() for agent
  build_planner.go              # Build() for planner
  config.go                     # Phase 1: config + auth (all nodes)
  database.go                   # Phase 2: database + concepts (all nodes)
  engine.go                     # Phase 3: engine + bus + automations (all nodes)
  engine_polyphon.go            # Polyphon cognition init (cognition only)
  integrations.go               # integrationsCore(): database + auth (all nodes)
  integrations_bff.go           # integrationsBFF() (bff)
  integrations_cognition.go     # integrationsCognition() (cognition)
  integrations_agent.go         # integrationsAgent() (agent)
  integrations_cognition_init.go # Cognition integration setup (cognition only)
  integrations_files.go         # File/storage integrations (agent only)
  integrations_stt.go           # STT provider selection (cognition + agent)
  transport.go                  # transportBase() + createHTTPServer() (all nodes)
  transport_bff.go              # BFF transport (base + HTTP)
  transport_cognition.go        # Polyphon endpoints
  transport_agent.go            # SI HTTP + audio + attachments
  transport_minimal.go          # Minimal (planner)
  transport_voice.go            # wirePolyphonEndpoints (cognition only)
  cluster.go                    # Phase 6: node bootstrap + DB-based peer discovery
  adapters.go                   # Engine adapters (shared)
```

### What Each Node Includes

| Component | BFF (default) | Voice | Cognition | Agent | Planner |
|-----------|:------------:|:-----:|:---------:|:-----:|:-------:|
| Config + Auth | x | x | x | x | x |
| Database + Concepts | x | x | x | x | x |
| MemQL Engine | x | x | x | x | x |
| gRPC Server (`MemqlService.Stream`) | x | x | x | x | x |
| WebSocket Bridge (`/memql/ws`) | x | x | x | x | x |
| Cluster Node Bootstrap (Worker dial / Parent connect) | x | x | x | x | x |
| Polyphon Cognition | | | x | | |
| Cognition Integration | | | x | | |
| Polyphon HTTP (`/polyphon/room-token`, `/polyphon/status`) | | x | | | |
| Audio WebSocket (`/memql/audio`) | | x | | x | |
| Attachment Upload (`/spaces/{id}/attachments`) | | | | x | |
| STT Provider | | x | x | x | |
| File / Storage / Email integrations | | | | x | |
| Agent SI tool-loop + replier + suggest | | | | x | |

### Compile-Time Node Type

Each binary knows its compiled type via `node.CompiledNodeType()`. For tagged binaries, this takes precedence over the `MEMQL_NODE_TYPE` env var. For the default (BFF) binary, the env var is still respected as a fallback.

```go
import "github.com/znasllc-io/memql/component/node"

compiled := node.CompiledNodeType()
// default binary → NodeTypeBFF
// voice binary  → NodeTypeVoice
```

## Testing

```bash
# All tests must pass for each tag
go test ./...
go test -tags voice ./...
go test -tags cognition ./...
go test -tags agent ./...
go test -tags planner ./...
```

## Adding Code to a Node Type

When adding new functionality, consider which node types need it:

1. **All nodes**: Put in untagged shared files
2. **Specific node types**: Use `//go:build` constraints

Common tag patterns:
```go
//go:build !voice && !cognition && !agent && !planner     // default (BFF) only
//go:build !voice && !agent && !planner                   // cognition + default (BFF)
//go:build !voice && !cognition && !planner               // agent + default (BFF)
//go:build !voice && !planner                             // cognition + agent + default (BFF)
//go:build voice || cognition                             // voice + cognition
```

The key principle: move **import statements** to tag-specific files. Excluding a Go package import is what actually reduces binary size. The `.memql` DSL files are small (~212KB total) and are always embedded.
