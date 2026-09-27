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
| **agent** | `agent` | Task execution, AI work, tool calling, streaming transcription |
| **planner** | `planner` | Task planning and orchestration |
| **workbench** | `workbench` | Sandboxed per-Plan Linux execution surface |
| **mcp** | `mcp` | MCP (Model Context Protocol) server -- engine tool surface to external MCP hosts (epic memql#1529) |
| **edge** | `edge` | Serves this cluster's hosted web surfaces (every SPA/website + MemQL OS) by resolving the request `Host` header to a `v1:platform:site` row. Excludes file processing and the agent tool surface (epic memql#3700) |

The `bff` tag and the no-tag default build the same node; `bff` exists so a
manifest can name it explicitly.

## Building

```bash
# BFF (default, no tag needed)
go build -o bin/memql .

# Node-type-specific binaries
go build -tags agent -o bin/memql-agent .
go build -tags planner -o bin/memql-planner .
go build -tags workbench -o bin/memql-workbench .
go build -tags mcp -o bin/memql-mcp .
go build -tags edge -o bin/memql-edge .
```

Tags are **mutually exclusive** -- never combine them (e.g., `-tags "bff agent"`).

### MCP node configuration (epic memql#1529)

The `mcp` node enforces two orthogonal, server-side authz gates:

- `MEMQL_MCP_MODE` -- the capability tier (Gate A): `sealed` (execute named
  constructs only), `authoring` (**default** -- adds `define`), or `inline`
  (adds ad-hoc `query`).
- `MEMQL_MCP_ROLE` -- the cluster role of the PERSON the session acts as, for
  the per-construct gate (Gate B): what a reflected tool's `@requiresRank`
  floor (and the deprecated `@allowedRoles`) judges a call against. On HTTP an
  empty value takes the role from the caller's verified token; on stdio it
  means the session holds no role, which clears no rank floor. Authoring
  (`define`) and inline (`query`) require the `owner` or `developer` role. The
  `developer` role is engineering power (author / inline / write) but not
  user-management power.
- `MEMQL_MCP_USER` -- the PERSON the session acts as. On HTTP an empty value
  takes the user from the caller's verified token. **A stdio session needs it
  to run a reflected DSL tool** (memql#5438): a tool is called by an agent or
  by an authenticated person, so a session with no identity is offered no
  reflected tool by `tools/list` and refused every one by `tools/call`.

## Docker

```bash
# BFF (default)
docker build .

# Node type
docker build --build-arg BUILD_TAGS=agent .
docker build --build-arg BUILD_TAGS=planner .
```

To build all node types:
```bash
go build .                                         # bff (default)
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
  build_agent.go                # Build() for agent
  build_planner.go              # Build() for planner
  build_workbench.go            # Build() for workbench
  build_edge.go                 # Build() for edge
  build_identity.go             # Build() for identity
  build_mcp.go                  # Build() for mcp
  config.go                     # Phase 1: config + auth (all nodes)
  database.go                   # Phase 2: database + concepts (all nodes)
  engine.go                     # Phase 3: engine + bus + automations (all nodes)
  engine_authored.go            # Authored-automation wiring
  integrations.go               # integrationsCore(): database + auth (all nodes)
  integrations_bff.go           # integrationsBFF() (bff)
  integrations_agent.go         # integrationsAgent() (agent)
  integrations_planner.go       # integrationsPlanner() (planner)
  integrations_planner_init.go  # Planner integration setup (planner only)
  integrations_identity.go      # Identity integrations (identity)
  integrations_workbench.go     # Workbench integrations (workbench)
  integrations_worker_agent.go  # Worker gateway (agent)
  integrations_deploy_control.go # DeployControlService (identity)
  integrations_stt.go           # STT provider selection (every node but planner)
  transport.go                  # transportBase() + createHTTPServer() (all nodes)
  transport_bff.go              # BFF transport (base + HTTP)
  transport_agent.go            # AI HTTP + streaming transcription
  transport_artifacts.go        # Library artifact upload + content routes
  transport_blobstore.go        # Blob-store wiring
  transport_edge.go             # Edge site serving
  transport_inbound.go          # POST /inbound/{source}
  transport_mcp.go              # MCP server transport
  transport_sites.go            # POST /sites/{id}/bundles
  transport_unsubscribe.go      # GET+POST /unsubscribe
  transport_minimal.go          # Minimal (planner)
  transport_tracking.go         # GET /t/o/{token}, /t/c/{token}
  cluster.go                    # Phase 6: node bootstrap + DB-based peer discovery
  adapters.go                   # Engine adapters (shared)
```

### What Each Node Includes

| Component | BFF (default) | Agent | Planner | Workbench |
|-----------|:------------:|:-----:|:-------:|:---------:|
| Config + Auth | x | x | x | x |
| Database + Concepts | x | x | x | x |
| MemQL Engine | x | x | x | x |
| gRPC Server (`MemqlService.Stream`) | x | x | x | x |
| WebSocket Bridge (`/memql/ws`) | x | x | x | x |
| Cluster Node Bootstrap (Worker dial / Parent connect) | x | x | x | x |
| Streaming transcription (`AiTranscribeStream*`) | | x | | |
| STT Provider | x | x | | x |
| Library artifact routes (`/artifacts`) | x | | | |
| Worker gateway (`WorkerService.Stream`) | | x | | |
| Workbench workspaces | | | | x |
| File / Storage / Email integrations | | x | | |
| Agent AI tool-loop + suggest | | x | | |

### Compile-Time Node Type

Each binary knows its compiled type via `node.CompiledNodeType()`. For a tagged
binary that takes precedence over `MEMQL_NODE_TYPE`, full stop: a disagreeing
env var is logged as a misconfiguration and ignored. For the untagged (default
BFF) binary the env var selects the type, and a value outside the mesh set is
honoured verbatim rather than falling back to bff.

```go
import "github.com/znasllc-io/memql/component/node"

compiled := node.CompiledNodeType()
// default binary  → NodeTypeBFF
// agent binary    → NodeTypeAgent
// identity binary → NodeTypeIdentity

node.CompiledNodeTypeIsTagged()
// default binary → false   (MEMQL_NODE_TYPE selects)
// -tags bff      → true    (the tag wins; same VALUE, different meaning)
```

**That last pair is why the flag exists** (memql#5115). Until it did, the env
var won for every mesh type -- so a `-tags agent` binary whose manifest said
`bff` reported `NodeTypeBFF`, an agent by every wiring decision in
`app/build_agent.go` and a bff to the `Type == NodeTypeBFF` gate in
`app/cluster.go` that starts the worker mesh's dialer. `identity` and `edge`
had no `compiled_<type>.go` at all and so compiled as the bff default, which is
the same failure with the tag removed rather than overridden.

## Testing

```bash
# Use `make test`, NOT `go test ./...` -- the bare command misses this
# repo's own engine modules (component/memql, component/database,
# component/language), since go.work lists 49 workspace modules and a
# relative pattern only covers the root module (memql#4032).
make test

# A genuinely tag-scoped run needs the full module path too, for the
# same reason -- `go test -tags agent ./...` would miss the engine:
go test -tags agent github.com/znasllc-io/memql/...
go test -tags planner github.com/znasllc-io/memql/...
```

## Binary size

Every node binary is within ~5.5% of every other one. Measured on `main` with
the Makefile's default flags (`CGO_ENABLED=0`, no `-ldflags` strip), Go 1.26.6,
linux/amd64, **before the cognition and voice node types were removed** -- so
the two rows below that name them are a record of that measurement, not of a
build you can reproduce today:

| Tag | Bytes | |
|---|---:|---|
| `workbench` | 80,357,957 | smallest |
| (default) | 80,376,923 | |
| `edge` | 80,551,975 | |
| `bff` | 80,605,611 | |
| `mcp` | 80,620,514 | |
| `cognition` | 80,885,960 | node type since removed |
| `planner` | 80,927,315 | |
| `agent` | 81,635,835 | |
| `identity` | 84,799,953 | largest |

`voice`, also since removed, was CGO-only and never comparable on these flags.

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
   that every build imports, so the vendor set is constant. The same shape
   holds for `Azure/azure-sdk-for-go` (37 packages), `google/cel-go` (22),
   `anthropics/anthropic-sdk-go` (22) and `redis/go-redis` (15).

   The single largest instance of it has since gone away on its own. Under the
   measurement above, a `planner` binary -- which has nothing to do with
   realtime media -- still linked 79 `pion/*`, 36 `livekit/*` and 9 `webrtc`
   packages, dragged in by three untagged first-party packages
   (`component/polyphon`, `integrations/avatardirect`, `integrations/telephony`).
   All three were deleted with the voice and cognition node types, so that
   vendor set is no longer linked into anything. Reason 2 still holds in shape;
   its worst case is gone, and the table above has not been re-measured since.

Reason 1 is not ours to fix. **Re-measure before quoting any number here.**

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
//go:build !bff && !agent && !planner && !identity && !workbench && !mcp && !edge
//go:build agent                                          // agent only
//go:build agent || planner                               // agent + planner
//go:build !planner                                       // everything but planner
```

**The negative form has to name every tag in the set, and the set is
seven.** `bff`, `agent`, `planner`, `identity`, `workbench`, `mcp`,
`edge` -- a `!`-chain that omits one silently compiles the file into that
node type as well. The canonical spelling is
[`app/build_default.go`](../../../app/build_default.go)'s own constraint;
copy it rather than reconstructing the chain by hand, and update both
when a node type is added. Prefer the POSITIVE form (`//go:build agent`)
wherever it expresses the same thing -- it does not grow when an eighth
node type arrives.

The key principle: move **import statements** to tag-specific files. Excluding a Go package import is what actually reduces binary size. The `.memql` DSL files are small (a few MB total -- `du -sh dsl/` to check the current size) and are always embedded.
