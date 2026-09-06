---
title: Build Tags -- Node Type Binaries
audience: public
status: stable
area: build
sinceVersion: 0.9.0
owner: znas
---

# Build Tags -- Node Type Binaries

> **Platform consolidation (#2472):** build tags still select which node-type
> binary compiles, but every node type now ships as a **product-agnostic**
> image -- product DSL is delivered at **runtime** via `MEMQL_DSL_PATH`
> (a bundle image + init-container), not compiled in. There are no
> product-specific build tags and no carrier-built nodes in the common case.
> See the topology in the engine `CLAUDE.md` and
> [../../internal/design/platform-consolidation.md](../../internal/design/platform-consolidation.md).

MemQL uses Go build tags to compile separate binaries for each node type in the
distributed cluster. Each binary selects its own `app.Build` and the transport /
integration wiring that goes with it.

**Build tags are a WIRING mechanism, not a size mechanism.** They decide which
`app/build_*.go` runs and therefore what a node does -- they do not currently make
the binary meaningfully smaller. See [Binary size](#binary-size) for the measured
numbers and why.

## Node Types

| Type | Tag | Purpose |
|------|-----|---------|
| **bff** | (none, default) | Backend for frontend |
| **identity** | `identity` | Identity service: magic-link + JWT, JWKS publishing, admin ops |
| **voice** | `voice` | Voice transport (audio WS, LiveKit). CGO-only |
| **cognition** | `cognition` | Cognition pipeline, Polyphon |
| **agent** | `agent` | Task execution, AI work, tool calling |
| **planner** | `planner` | Task planning and orchestration |
| **workbench** | `workbench` | Sandboxed per-Plan Linux execution surface |
| **mcp** | `mcp` | MCP (Model Context Protocol) server -- engine tool surface to external MCP hosts (epic memql#1529) |
| **edge** | `edge` | Serves this cluster's hosted web surfaces (every SPA/website + the MemQL Portal, site #1) by resolving the request `Host` header to a `v1:platform:site` row. Excludes the voice pipeline, the cognition pipeline, file processing, and the agent tool surface (epic memql#3700) |

The `bff` tag and the no-tag default build the same node; `bff` exists so a
manifest can name it explicitly.

## Building

```bash
# BFF (default, no tag needed)
go build -o bin/memql .

# Node-type-specific binaries
go build -tags voice -o bin/memql-voice .
go build -tags cognition -o bin/memql-cognition .
go build -tags agent -o bin/memql-agent .
go build -tags planner -o bin/memql-planner .
go build -tags workbench -o bin/memql-workbench .
go build -tags mcp -o bin/memql-mcp .
go build -tags edge -o bin/memql-edge .
```

Tags are **mutually exclusive** -- never combine them (e.g., `-tags "bff cognition"`).

### MCP node configuration (epic memql#1529)

The `mcp` node enforces two orthogonal, server-side authz gates:

- `MEMQL_MCP_MODE` -- the capability tier (Gate A): `sealed` (execute named
  constructs only), `authoring` (**default** -- adds `define`), or `inline`
  (adds ad-hoc `query`).
- `MEMQL_MCP_ROLE` -- the role the session acts as for the per-construct gate
  (Gate B). Empty -> the engine's `specialist` default. Authoring (`define`)
  and inline (`query`) require the `owner` or `developer` role. The
  `developer` role is engineering power (author / inline / write) but not
  user-management power.

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

The `app/` package contains build-tagged files that control which bootstrap phases run. This list is illustrative, not exhaustive -- run `ls app/build_*.go` for the full current set of node-type `Build()` files:

```
app/
  app.go                        # Shared: App struct, newApp(), fatal()
  build_default.go              # Build() for default (BFF, no tag)
  build_bff.go                  # Build() for bff (explicit tag)
  build_cognition.go            # Build() for cognition
  build_agent.go                # Build() for agent
  build_planner.go              # Build() for planner
  build_voice.go                # Build() for voice
  build_workbench.go            # Build() for workbench
  build_edge.go                 # Build() for edge
  build_identity.go             # Build() for identity
  build_mcp.go                  # Build() for mcp
  config.go                     # Phase 1: config + auth (all nodes)
  database.go                   # Phase 2: database + concepts (all nodes)
  engine.go                     # Phase 3: engine + bus + automations (all nodes)
  engine_polyphon.go            # Polyphon cognition init (cognition only)
  engine_authored.go            # Authored-automation wiring
  integrations.go               # integrationsCore(): database + auth (all nodes)
  integrations_bff.go           # integrationsBFF() (bff)
  integrations_cognition.go     # integrationsCognition() (cognition)
  integrations_cognition_init.go # Cognition integration setup (cognition only)
  integrations_agent.go         # integrationsAgent() (agent)
  integrations_planner.go       # integrationsPlanner() (planner)
  integrations_planner_init.go  # Planner integration setup (planner only)
  integrations_identity.go      # Identity integrations (identity)
  integrations_voice.go         # Voice integrations (voice)
  integrations_workbench.go     # Workbench integrations (workbench)
  integrations_worker_agent.go  # Worker gateway (agent)
  integrations_deploy_control.go # DeployControlService (identity)
  integrations_stt.go           # STT provider selection (cognition + agent)
  transport.go                  # transportBase() + createHTTPServer() (all nodes)
  transport_bff.go              # BFF transport (base + HTTP)
  transport_agent.go            # AI HTTP + audio + attachments
  transport_attachments.go      # Attachment upload endpoint
  transport_audio.go            # /memql/audio WebSocket
  transport_edge.go             # Edge site serving
  transport_inbound.go          # POST /inbound/{source}
  transport_mcp.go              # MCP server transport
  transport_sites.go            # POST /sites/{id}/bundles
  transport_unsubscribe.go      # GET+POST /unsubscribe
  transport_minimal.go          # Minimal (planner)
  transport_voice.go            # Voice transport
  transport_voice_endpoints.go  # wirePolyphonEndpoints
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
| Agent AI tool-loop + replier + suggest | | | | x | |

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
# Use `make test`, NOT `go test ./...` -- the bare command misses this
# repo's own engine modules (component/memql, component/database,
# component/language), since go.work lists 49 workspace modules and a
# relative pattern only covers the root module (memql#4032).
make test

# A genuinely tag-scoped run needs the full module path too, for the
# same reason -- `go test -tags voice ./...` would miss the engine:
go test -tags voice github.com/znasllc-io/memql/...
go test -tags cognition github.com/znasllc-io/memql/...
go test -tags agent github.com/znasllc-io/memql/...
go test -tags planner github.com/znasllc-io/memql/...
```

## Binary size

Every node binary is currently within ~5.5% of every other one. Measured on
`main` with the Makefile's default flags (`CGO_ENABLED=0`, no `-ldflags` strip),
Go 1.26.6, linux/amd64:

| Tag | Bytes | |
|---|---:|---|
| `workbench` | 80,357,957 | smallest |
| (default) | 80,376,923 | |
| `edge` | 80,551,975 | |
| `bff` | 80,605,611 | |
| `mcp` | 80,620,514 | |
| `cognition` | 80,885,960 | |
| `planner` | 80,927,315 | |
| `agent` | 81,635,835 | |
| `identity` | 84,799,953 | largest |

`voice` is CGO-only (`CGO_ENABLED=1`, `-tags voice`) and is not comparable on
these flags.

This page and root `CLAUDE.md` used to claim tagging reduced binary size "by up
to 53%", with a table running 25 MB to 43 MB. That has not been true for some
time; memql#4106 measured it and found the cause.

### Why the tags do not shrink anything

Two independent reasons, both structural:

1. **~32 MiB of every binary is one stdlib symbol.**
   `crypto/internal/fips140/drbg.memory` is a 33,554,432-byte data symbol
   emitted by Go 1.26 regardless of build tags, `GOFIPS140`, or anything this
   repo controls. That is ~40% of each binary, identical everywhere, and it
   compresses the *relative* spread between node types toward zero.

2. **The tag gating stops at `app/`.** `go list -deps` counts 115 first-party
   packages in the default build and 116-118 under any node tag -- the tags
   move 3-5 packages. Everything heavy is reached through UNTAGGED packages
   that every build imports, so the vendor set is constant. Concretely, a
   `planner` binary -- which has nothing to do with realtime media -- still
   links 79 `pion/*`, 36 `livekit/*` and 9 `webrtc` packages, pulled in by
   three untagged first-party packages:

   - `component/polyphon` -> `livekit/protocol/auth`
   - `integrations/avatardirect` -> `livekit/protocol`
   - `integrations/telephony` -> `livekit/server-sdk-go/v2` (this is the one
     that drags in all of `pion`)

   The same shape holds for `Azure/azure-sdk-for-go` (37 packages),
   `google/cel-go` (22), `anthropics/anthropic-sdk-go` (22) and
   `redis/go-redis` (15).

Reason 1 is not ours to fix. Reason 2 is: putting a node-type constraint on the
media/vendor-facing integrations would restore real differentiation. That is a
wiring refactor with mesh consequences, tracked separately -- do not assume the
current numbers are a floor.

**When adding code, size is not the argument for a build tag.** Reach for one
because a node type should not RUN something (an integration it must not
register, a transport it must not open), not because you expect the binary to
shrink.

## Adding Code to a Node Type

When adding new functionality, consider which node types need it:

1. **All nodes**: Put in untagged shared files
2. **Specific node types**: Use `//go:build` constraints

Common tag patterns:
```go
// "default only" must exclude EVERY node tag -- see below.
//go:build !bff && !voice && !cognition && !agent && !planner && !identity && !workbench && !mcp && !edge
//go:build cognition                                      // cognition only
//go:build agent || planner                               // agent + planner
//go:build voice || cognition                             // voice + cognition
```

**The negative form has to name every tag in the set, and the set is
nine.** `bff`, `voice`, `cognition`, `agent`, `planner`, `identity`,
`workbench`, `mcp`, `edge` -- a `!`-chain that omits one silently
compiles the file into that node type as well. The canonical spelling is
[`app/build_default.go`](../../../app/build_default.go)'s own constraint;
copy it rather than reconstructing the chain by hand, and update both
when a node type is added. Prefer the POSITIVE form (`//go:build
cognition`) wherever it expresses the same thing -- it does not grow when
a tenth node type arrives.

The key principle: move **import statements** to tag-specific files. Excluding a Go package import is what actually reduces binary size. The `.memql` DSL files are small (a few MB total -- `du -sh dsl/` to check the current size) and are always embedded.
