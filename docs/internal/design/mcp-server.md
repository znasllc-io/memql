---
title: memQL MCP server — node role, capability tiers, and tool surface
audience: internal
status: design
area: internal
sinceVersion: 0.9.0
owner: znas
---

# memQL MCP server — node role, capability tiers, and tool surface

Phase 0 design for exposing memQL over the Model Context Protocol (MCP) to
external clients (Claude Desktop / Claude Code and any other MCP host). This
document records the architecture decision, the security model, the tool
surface, and a phased implementation plan. It changes no runtime behavior on
its own; every load-bearing claim about the tree as it stands cites a real
`path:line`.

> **Headline.** The MCP server is a **new node role inside the `memql`
> repo**, selected by a `mcp` build tag — the same mechanism that already
> produces the `bff`, `agent`, `cognition`, `voice`, `identity`, `planner`,
> and `workbench` binaries. It is a protocol head over the engine's existing
> tool surface, not a separate repo. Access is governed by **two orthogonal,
> server-enforced gates**: a per-deployment **capability tier** and a
> per-tool **role** (including a new `developer` role). v1 ships **all three
> tiers**, with Tier 2 (Authoring) as the default.

---

## 1. Where it lives — decision

### 1.1 The repo split is "generic engine vs. carrier", not "one repo per thing"

The workspace contains four git repos with a clear division of labor:

- **`memql`** — the engine. One Go codebase compiled into distinct *node
  roles* via build tags. Each role is a `build_*.go` + `transport_*.go` pair
  wiring different integrations/transports onto the same engine
  (`app/build_bff.go`, `app/build_voice.go`, `app/build_default.go`, …).
  The Makefile cuts one binary per tag (`-tags bff -o memql-bff`,
  `-tags voice -o memql-voice`, etc. — see `Makefile:46,54,58,62,66,70,88`).
- **the product carrier repo** — a *carrier*: a separate repo that pins the
  engine (`require github.com/znasllc-io/memql v0.9.0` plus a local
  `replace => ../memql` for dev) and compiles client-specific DSL +
  `RegisterPlugin()` Go code together with the engine into one deployable BFF
  node image. Its README states it was "Lifted from `…/memql` … as part of
  the monorepo carve-up." It exists separately because it is **client-specific**.
- **`memql-cockpit`** — the CLI: a separate repo that imports the engine as a
  module (`require github.com/znasllc-io/memql …`). Separate because it is an
  **independent external consumer**.

The rule the codebase already follows: **generic transports and node roles
live inside `memql`; a thing gets its own repo only when it is client-specific
(the BFF) or an independent consumer (the cockpit).**

### 1.2 An MCP server is a generic protocol head → it belongs in `memql`

memQL already represents its tools in MCP-compatible form
(`ToolDefinitionToMCP` / `ToolListToMCP`, `component/memql/tool_loader.go:20-51`)
and already bridges model-driven tool calls through the authorized backend
path for the realtime voice agent
(`integrations/voice/agent/mcp_tool_bridge.go`). What is missing is a process
that *speaks the MCP wire protocol to external clients*. That is a new
**transport/protocol head over the existing engine tool surface** — exactly
the same category as the gRPC server, the WebSocket browser bridge, and the
voice realtime bridge, all of which live in `memql`.

A generic MCP server is **not** client-specific (unlike the product BFF) and
**not** an external consumer (unlike the cockpit). Splitting it into its own
repo would force it to pin a frozen engine version and reach back through the
SDK for a tool surface that already lives natively in the engine — taking on
the BFF's three-link pin-chain overhead for no isolation benefit.

**Decision: implement the MCP server as a new `mcp` node role inside `memql`.**

Concretely:

- `app/build_mcp.go` — `//go:build mcp`, a `Build(...)` that wires
  config/auth, database/concepts, engine/bus, core integrations, the MCP
  transport, and the cluster, mirroring `app/build_bff.go`.
- `app/transport_mcp.go` — the MCP protocol head (stdio and/or HTTP/SSE).
- Makefile target: `-tags mcp -o $(BIN_DIR)/memql-mcp`, mirroring the existing
  role targets.

**Client-specific corollary.** If a deployment needs to expose *one client's*
tools over MCP (e.g. a product's tools to Claude Desktop), the `mcp` transport
still lives in `memql`; the product's carrier repo simply compiles
that transport together with its client plugins — exactly as the BFF compiles
the engine with the product DSL today. The implementation stays in `memql`
either way.

---

## 2. Security model — two orthogonal gates

Every MCP call must pass **both** gates, and **both are enforced server-side
in the engine**, never merely by "not registering the tool". This is defense
in depth: a buggy or compromised MCP head cannot widen access beyond what the
engine itself permits. The gates sit alongside the engine's existing
server-side checks (the per-tool role gate at
`component/memql/tool_execution.go:319` via
`Tool.IsAllowedForRole`, `component/memql/tool_types.go:186-190`, plus the
engine's scope / kill-switch / agent-authorization checks).

### Gate A — Capability tier (per deployment)

A coarse, env-var-driven switch (`MEMQL_MCP_MODE`, or equivalent) that decides
which *classes* of operation the server permits at all. Defaults to Tier 2.

### Gate B — Role (per tool / per call)

The existing RBAC role of the caller, gated per construct via
`IsAllowedForRole`, extended with a new `developer` role (section 4).

A call is allowed only if the **tier enables the operation class** AND the
**caller's role passes the construct's gate**.

---

## 3. Capability tiers

memQL's roles today are `owner`, `admin`, `writer`, `reader`
(`component/auth/rbac.go:23-26`). The engine distinguishes *runtime* execution
(inline query text via `ExecuteQueryMsg`, `component/grpc/memql.proto:392`,
which carries a raw `query` string + `variables`) from *authored* constructs
(concepts, queries, mutations, specs, tools, prompts, automations declared in
`.memql` files). Critically, the runtime path is a **restricted subset**:
`component/memql/parser_langpath.go:142` rejects inline spec definitions
(`name := expr`) in a runtime query, and the same checker rejects trailing
`@timestamp`/`@latest` suffixes — "define the spec in the DSL and reference it
by name." Authoring itself is **not exposed over the wire today**: there is no
author/activate/publish message in the `MemqlService.Stream` union; authoring
runs server-side under an author's authz envelope through the authoring
subsystem (`component/memql/authoring_activation_engine.go`,
`authoring_capability_gate.go`, `authoring_capability_store.go`).

The tiers double as a **risk ladder** and a **delivery sequence**:

### Tier 1 — SEALED

Execute only already-embedded, named constructs.

- Exposed: every DSL `tool` (reflected 1:1), plus `run_query`,
  `run_mutation`, `run_automation` (by name + args).
- No defining, no inline.
- Built mostly on existing capability — except manual named-automation
  execution (section 6), which is net-new.

### Tier 2 — AUTHORING **(default)**

Tier 1 + define new named constructs by submitting `.memql` bundles.

- Adds `define`: submit a `.memql` file → validate → author → activate via the
  authoring subsystem. New constructs become callable by name afterward.
- Still no ad-hoc inline execution.
- Authored constructs are **session-scoped and non-durable by default**;
  promotion into the shared schema is a separate, owner-gated step (section 5).
- Net-new: authoring is not exposed over the wire today.

### Tier 3 — INLINE

Tier 2 + ad-hoc inline execution.

- Adds `query` (inline MemQL text), inline definitions (`name := expr`), and
  inline automation execution.
- Net-new: requires lifting the restrictions in
  `component/memql/parser_langpath.go`; largest security surface.
- Gated to `owner` / `developer` only.

> **v1 scope: all three tiers ship in v1.** Tier 2 is the default mode;
> Tier 1 is the lock-down option; Tier 3 is available behind the env flag and
> restricted to `owner`/`developer`.

---

## 4. The `developer` role

There is no `developer` role today (`component/auth/rbac.go:23-26` defines
only `owner`/`admin`/`writer`/`reader`). RBAC capabilities are composed by
explicit predicates (e.g. `CanWrite = owner || admin || writer`), not a pure
ladder — so `developer` is added as a first-class role with explicitly wired
capabilities rather than a single inserted rung.

**`developer` = engineering power, not admin power.**

- **Can:** author constructs (`define`), run inline DSL (`query`), and write
  data.
- **Cannot:** manage users / identities (remains `admin` / `owner`).
- `owner` is a superset of `developer`.
- `admin` is unchanged — it does **not** gain authoring or inline access.

New capability predicates:

- `CanAuthor() = owner || developer`
- `CanRunInline() = owner || developer`

These layer onto the existing RBAC without disturbing owner/admin/writer/reader
semantics. Touch points: the role enum and `AllRoles` list
(`component/auth/rbac.go`), `EffectiveRole`, and propagation into
`Tool.IsAllowedForRole` (`component/memql/tool_types.go:186-190`) and the
identity primary-role plumbing (`component/identity/store.go`,
`component/identity/config.go`).

---

## 5. Authoring durability — session-scoped + promotion

With Tier 2 as default, `define` must be safe-by-default. The model:

- `define` authors into a **scoped namespace tied to the caller's
  session/identity**, runs under that author's authz envelope (which the
  authoring subsystem already supports —
  `authoring_capability_gate.go:6` notes "An authored automation runs LIVE
  under its author's authz envelope"), and is **non-durable by default**: it
  lives for the session and is garbage-collected afterward.
- **Promotion** of an authored construct into the shared, durable schema is a
  **separate, higher-privileged action** (owner only, or a deliberate
  `persist: true` that only `owner` may set), ideally flowing through the same
  path as committing a `.memql` file to the repo so durable schema changes
  remain reviewable.

This keeps the global graph schema clean: developers prototype ad-hoc concepts
freely without touching shared state, and nothing becomes permanent without an
explicit owner-gated promotion.

---

## 6. Tool surface

### 6.1 Exposure strategy — targeted hybrid

MCP clients degrade when handed very large tool lists, so the surface is
deliberately curated:

- **DSL `tool` constructs → reflected 1:1** as individual MCP tools. They are
  designed to be model-callable and already carry role gates
  (`Tool.IsAllowedForRole`), so this is exactly their purpose.
- **Named queries / mutations / automations → generic dispatchers**
  (`run_query`, `run_mutation`, `run_automation`), each taking a `name` + args
  and role-checked per call against the named construct's own gate. Keeps the
  tool list clean while everything stays reachable.
- **`@mcp` annotation** — an opt-in DSL annotation that **promotes** a specific
  named query/mutation/automation into its own first-class MCP tool when it
  should be prominent in the client's tool list.

### 6.2 Capability (meta) tools

| Tool | Operation | Backed by | Tier | Role |
|---|---|---|---|---|
| `<toolname>` (reflected) | call a DSL tool | `CallToolMsg` (`memql.proto:629`) | 1 | per-tool gate |
| `run_query` | run a named query | `ExecuteQueryMsg` (named ref) | 1 | construct gate |
| `run_mutation` | run a named mutation | `executor_mutation.go:19` | 1 | construct gate |
| `run_automation` | run a named automation | new action-chain exec | 1 | construct gate |
| `define` | author `.memql` bundle (session-scoped) | authoring subsystem | 2 | `CanAuthor` |
| `query` | inline MemQL query text | `ExecuteQueryMsg` (`memql.proto:392`) | 3 | `CanRunInline` |
| `define`/`query` inline definitions | `name := expr` etc. | lift `parser_langpath.go:142` | 3 | `CanRunInline` |

### 6.3 `run_automation` semantics

Automations are event- (row-landing) or cron-triggered today; there is no
manual-trigger RPC, and the authoring sandbox already distinguishes
schedule-driven automations from those whose trigger names a single row
(`component/memql/authoring_sandbox_automation.go:26`). See also the existing
design note `docs/internal/design/authored-automations-954.md`.

`run_automation(name, input, dry_run?)`:

- Executes the automation's **action chain directly** under the author's
  envelope with an explicit `input` payload (skips trigger matching). This
  handles schedule-driven automations (no input row) and event-driven ones
  (pass the row as `input`) uniformly.
- `dry_run: true` executes without committing writes — a safe preview of what
  the automation would do.
- **Inline automation execution (Tier 3)** reuses this same action-chain path:
  submit automation DSL + input, run directly, never persist.
- Synthesize-event execution (fabricate the triggering row and let the normal
  event path fire) is a possible later addition for true end-to-end trigger
  testing; it is **not** the default.

### 6.4 Resources & prompts (MCP primitives beyond tools)

- **Resources** — concept schemas via `ConceptsListMsg`
  (`memql.proto:1086`); reading a concept resource runs a query for its rows.
  Live updates via `SubscribeMsg` / `ConceptsSubscribeMsg`
  (`memql.proto:420`) → MCP `resources/subscribe` + update notifications.
  memQL is subscription-native, so this is the most differentiated surface and
  also the largest transport lift (keeping the engine subscription alive
  across the MCP session).
- **Prompts** — DSL `prompt` definitions map naturally onto MCP prompts; a
  cheap, high-value add since prompts are first-class in the DSL.

---

## 7. Existing vs. net-new

**Existing (reuse):**

- MCP-shaped tool representation (`tool_loader.go:20-51`).
- Authorized tool-call path + per-tool role gate
  (`tool_execution.go:319`, `tool_types.go:186-190`).
- Inline query execution, restricted subset (`ExecuteQueryMsg`,
  `parser_langpath.go`).
- Named query / mutation execution (`executor_mutation.go`).
- Authoring subsystem internals (`authoring_*`), run under author envelope.
- Concept listing + subscriptions (`ConceptsListMsg`, `SubscribeMsg`).

**Net-new (build):**

- The `mcp` node role + transport (`build_mcp.go`, `transport_mcp.go`,
  Makefile target).
- The MCP protocol implementation (stdio + HTTP/SSE), tool/resource/prompt
  registration, session handling.
- The capability-tier gate (`MEMQL_MCP_MODE`) enforced server-side.
- The `developer` role + `CanAuthor` / `CanRunInline` predicates.
- Manual named-automation execution (action-chain exec + `dry_run`).
- `define` over the wire with session-scoped authoring + owner-gated promotion.
- Tier 3: lifting `parser_langpath.go` inline-definition restrictions; inline
  automation execution.
- The `@mcp` promotion annotation in the DSL + its handling in tool reflection.

---

## 8. Phased implementation plan

All phases target v1 (all three tiers). Phases are ordered so each lands
shippable value and de-risks the next.

**Phase 0 — Node role skeleton.** `app/build_mcp.go` + `app/transport_mcp.go`
+ Makefile `-tags mcp` target. Stand up an MCP server that connects to the
engine and serves an empty tool list. Wire health/lifecycle like the other
roles. *Outcome:* `memql-mcp` builds, boots, and an MCP client can connect.

**Phase 1 — Tier 1 read/exec surface.** Reflect DSL `tool` constructs 1:1;
implement `run_query` / `run_mutation` dispatchers over the existing executor
paths; enforce per-tool role gates. *Outcome:* a Claude client can list and
call existing tools and named queries/mutations against a deployment.

**Phase 2 — `developer` role + tier gate.** Add `RoleDeveloper`, `AllRoles`,
`EffectiveRole` wiring, and `CanAuthor` / `CanRunInline` predicates. Add the
`MEMQL_MCP_MODE` capability-tier gate, enforced server-side. *Outcome:* both
gates live; default deployment runs in Tier 2 mode (authoring enabled but not
yet implemented — gate returns "not yet" cleanly until Phase 3).

**Phase 3 — Tier 2 authoring (`define`).** Expose `define` over the wire:
validate + author + activate a `.memql` bundle through the authoring
subsystem, **session-scoped and non-durable**; add the owner-gated promotion
path into the durable schema. *Outcome:* default-tier deployment is fully
functional; developers can prototype constructs safely.

**Phase 4 — `run_automation` + `@mcp` promotion.** Implement manual
action-chain automation execution with `dry_run`; add the `@mcp` annotation
and promote-annotated named constructs into first-class MCP tools. *Outcome:*
automations are runnable/observable; prominent named constructs are
individually discoverable.

**Phase 5 — Tier 3 inline.** Lift the `parser_langpath.go` restrictions behind
the tier+role gate; expose `query` (inline) and inline automation execution,
restricted to `owner`/`developer`. *Outcome:* full inline power available in
the most-flexible mode.

**Phase 6 — Resources & prompts.** Concept resources, `resources/subscribe`
backed by engine subscriptions, and DSL `prompt` → MCP prompt mapping.
*Outcome:* memQL's memory/subscription surface is consumable as MCP resources.

Each phase carries server-side authz tests (mirroring `rbac_test.go`) and
gate-bypass tests asserting that neither a wrong tier nor a wrong role can
reach a gated operation.

---

## 9. Open items

- Transport: stdio first (local Claude Desktop/Code) vs. HTTP/SSE (hosted,
  multi-client) — which leads v1, and auth mapping for the remote case
  (JWT/JWKS already exists for the engine).
- Exact env-var name + values for `MEMQL_MCP_MODE` (`sealed` / `authoring` /
  `inline`).
- Promotion UX: does owner-gated promotion emit a `.memql` artifact / PR, or
  write directly to the durable schema store?
- Whether `developer` is grantable per-workspace or global, and how it maps to
  identity registration (`component/identity/registration/flow.go`).
- Garbage-collection policy + TTL for session-scoped authored constructs.
