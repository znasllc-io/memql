---
title: Module Registry -- Enablement Semantics, Umbrella Vocabulary, Registry Shape
audience: internal
status: design approved; ready for implementation
area: design
date: 2026-08-20
owner: znas
surface: engine (component/memql, component/grpc, dsl, app) + portal (consumer)
---

# Module Registry Design

Make the locked extension vocabulary (component / integration / pack) a
runtime reality the engine can report and the portal can manage. This spec
settles five things for epic memql#4183: the umbrella word, the registry
shape, the enablement semantics per kind, the authorization model, and the
wire surface. The implementing children are memql#4188 (inventory API),
memql#4189 (pack enable/disable), and memql#4190 (harness-as-pack, the
proving migration). The portal consumes all of it in epic memql#4184.

Nothing here adds a fourth extension kind, a second deploy path, or a
runtime Go loader. Packs stay compiled in; what becomes runtime state is
whether an instance RUNS them.

---

## 1. The umbrella word: "module"

**"Module" is the collective noun over the things an operator sees in the
registry. It is not a fourth extension kind.** The three extension words
stay exactly as locked by memql#4135/#4161 and gated by the memql#4166
tests: a *component* is engine internals, an *integration* talks to
somebody else's system, a *pack* is a client-agnostic product feature.
"Module" is what you say when you mean "any of them, as a manageable
unit" -- the way "vehicle" covers cars and trucks without being a third
kind of car.

The registry also reports a fourth ROW GROUP that is not an extension kind
at all: **node-type modules** (bff, voice, cognition, agent, planner,
workbench, mcp, edge, identity). A node type is a deployment unit -- a
build-tag wiring of components and integrations -- not a way to extend the
engine. It appears in the registry because an operator managing "what does
this cluster run" needs it on the same screen, and because one of them
(voice) already has a real enablement mechanism (credential-gated
scale-to-zero, memql#2416) that the registry must report rather than
rebuild.

`docs/public/concepts/component-integration-pack.md` gains a short
"Modules" section saying exactly this (the memql#4198 docs child extends
it further). The #4166 vocabulary gates are unaffected: they ban
"plugin"-as-category, and nothing here reintroduces it.

## 2. Registry shape

### 2.1 What enumerates, from where

The registry is assembled at request time from sources that already exist.
No new bookkeeping registry is introduced; the design's whole bet is that
the engine already knows these facts and has simply never been asked to
report them in one place.

| Kind | Enumerated from | Granularity decision |
|---|---|---|
| `component` | `component/envregistry` manifest `component:` values | One row per manifest component (ai, identity, engine, campaigns, ...). This is a real, maintained, closed vocabulary that already joins to the env-var surface -- which is the thing the portal detail page needs. Enumerating `component/` Go packages was rejected (too fine, no operator meaning, no env join); enumerating architecture-model services was rejected (arch.yaml declares exactly one service, `service:memql`, so the model cannot distinguish subsystems). |
| `integration` | `memql.RegisteredPlugins()` (what this binary compiled in) joined with the live `IntegrationRegistry` (what this node materialized) | One row per registered plugin name that is NOT a pack domain (see binding, section 4.3). Explicit `app/` wirings (cognition, agent, stt) appear via the live registry when materialized. |
| `pack` | `dsl.ListPackDomains()` (registered trees) joined with `memql.RegisteredPlugins()` via the pack binding | One row per pack domain (referencepack, deploypack, shopifypack, reviewspack, harness after memql#4190). |
| `node-type` | `v1:cluster:nodeType` + `v1:cluster:node` + `v1:cluster:deploymentNodeSpec` graph rows | One row per node type the graph knows. Replicas from the latest deploymentNodeSpec per (deploymentId, nodeType); liveness from `v1:cluster:node` health/lastSeen. |

### 2.2 The module row

```
Module {
  kind          component | integration | pack | node-type
  name          manifest component / plugin name / pack domain / node type
  description   from the source registry where one exists
  state         see per-kind states below
  stateDetail   one honest sentence (e.g. "disabled by v1:platform:packState;
                restart required to apply", "LiveKit credentials absent")
  scope         node | cluster        -- which truth tier this row's state is
  envComponent  the manifest component: key(s) this module's env vars carry
  fqnPrefixes   [] -- observability join keys (section 6)
  codeReference architecture-model id where one exists (node-types)
}
```

Per-kind states:

- `component`: always `built_in`. Components are not switchable and the
  registry does not pretend otherwise.
- `integration`: `active` (materialized on the answering node),
  `compiled_out` (known to the catalog, not in this binary's registry),
  `opted_out` (factory returned (nil, nil) -- the documented opt-out, e.g.
  harnessRecall without an embedding provider). State is DERIVED from
  config/credential presence; the registry reports it and never duplicates
  it into a stored toggle.
- `pack`: `enabled` / `disabled` (the persisted per-instance toggle,
  section 4) plus `loaded` / `inert` for what this node actually did at
  its own boot -- the two can disagree between a flip and the restart that
  applies it, and the row says so rather than papering over it.
- `node-type`: `running` (live nodes seen), `scaled_to_zero`,
  `credential_gated` (the voice case: required creds absent, deploy layer
  holds replicas at 0), `not_deployed`.

### 2.3 Per-node vs cluster-wide honesty

The inventory must not present one binary's view as the mesh's. The split,
stated once and carried on every row:

- **Cluster-scope facts** come from the shared graph and are true for
  every node that reads them: pack enablement state (`v1:platform:
  packState`), node-type rows, deployment specs, replica counts. Rows
  built from these carry `scope: cluster`.
- **Node-scope facts** come from the answering binary's own registries and
  environment: which integrations are compiled in / materialized, which
  DSL domains this node loaded or skipped, which env vars are set here.
  Rows built from these carry `scope: node`, and the RESPONSE envelope
  carries `reportingNodeId` + `reportingNodeType` so a client can say
  "as reported by bff-xyz" instead of "the cluster".

There is no cross-node fan-out in v1. A bff cannot enumerate what a
cognition binary wired up, and the registry says so (`compiled_out` means
"not in the binary that answered", nothing more). The env-var `set` bit is
evaluated on the answering node, which in practice mirrors the mesh
because every Deployment shares the `memql-secrets` envFrom -- but the row
is still labeled node-scope because that sharing is a deploy convention,
not an engine guarantee. If a later need demands per-node-type inventory,
the seam is a NodeService forward, not a redesign.

The cluster-e2e case for memql#4188/#4189 rides exactly this split: flip a
pack's state through one connection, read the inventory through
connections landing on other replicas, and the cluster-scope state must
agree everywhere while `loaded`/`inert` honestly reflects each node's
last boot.

## 3. Env-var surface (module detail)

The detail view joins the module's `envComponent` against the envregistry
manifest (`component/envregistry/manifest.go`), which already carries
name, secret-vs-variable (list membership), kind, scope, default,
description, and per-node-type requiredness. Per entry the engine reports:

```
ModuleEnvVar {
  name, description, secret, scope, requiredForNodeTypes
  set          bool   -- os.LookupEnv non-empty OR manifest default present,
                         evaluated on the answering node
  value        string -- ONLY for non-secret variables; secrets NEVER carry
                         a value, masked or otherwise
}
```

**Secrets never leave the engine, in any form.** No masked prefix, no
last-four, no reveal RPC. The manifest's secrets/variables split is the
authority on which is which; a variable is non-secret by that list's
definition. This is deliberately stricter than "masked values" -- a mask
that carries any derivative of the value invites a reveal endpoint later,
and the operator story ("is it set, and what does it mean") needs no
value at all.

## 4. Enablement semantics per kind

One registry, three mechanisms -- report, don't rearchitect:

| Kind | Mechanism | New in this epic? |
|---|---|---|
| pack | persisted per-instance toggle, honored at boot | YES (memql#4189) |
| integration | derived from config/credential presence | no -- report only |
| node-type | replica scale (graph) + deploy-layer credential gating | no -- report only |
| component | none (built-in) | no |

### 4.1 Where pack state lives: `v1:platform:packState`

A new platform concept (dsl/platform/concepts.memql), clusterOwner-tier
like its siblings `globalSecret`/`globalVariable`:

```
concept packState {
  packDomain  string  @required     -- the registered pack domain
  enabled     bool    @required
  reason      string                -- operator note, shown in the portal
}
```

One row per pack at the deterministic id `v1:platform:packState:<domain>`;
the append-only version history IS the audit trail of flips, and the
latest version wins. **Absence of a row means enabled** -- so existing
installs see zero behavior change when a pack ships, and a fresh database
needs no seeding. A row naming an unregistered domain is reported by the
inventory as `stateDetail: "names no registered pack"` and otherwise
ignored.

State lives in the shared graph, never in per-node env, so every node
resolves the same answer (multi-node default, non-negotiable). The
existing env precedents (`MEMQL_DSL_ALLOW_SKIPS`, `MEMQL_ACTION_REPLAY_
ENABLED`) chose env because they must be readable before the database
exists; pack enablement does not have that constraint (section 4.2) and
choosing env here would recreate exactly the per-node drift this design
exists to prevent.

The flip is a DSL mutation `mutate packState setPackEnabled` (admin
bucket of the per-row authz classification -- cluster-owner spec, so the
conformance test stays green), invoked by the gRPC write handler under
the caller's actor. Owner-only, audited (section 5).

### 4.2 When it is read, and what it gates: mounted-inert

**The boot read happens in phase 3, after the database starts and before
`engine.Init` runs the DSL loaders** -- the same in-between window
`canResolve()` already documents for provider auth resolution
(component/memql/engine_variables.go). Concretely, in `app/engine.go`
between `a.db.Ready()` and `a.engine.Init(...)`: one narrow read of the
latest `v1:platform:packState` rows via the started bun handle, producing
a disabled-domain set handed to the engine and kept on the App. A fresh
database (no table yet, first boot) reads as "no rows" and every pack is
enabled -- the read must treat a missing relation as empty, not fatal.

**A disabled pack is mounted-inert, not unmounted:**

- Its tree stays REGISTERED (`dsl.RegisterTree` at init() time is
  unconditional), so the namespace stays owned and a second pack claiming
  the same domain is refused exactly as today. Disablement never frees a
  namespace.
- Its CONCEPTS still load. Schemas are declarative and inert; loading them
  keeps cross-domain `use <pack>.concepts.{...}` imports resolving, keeps
  `@relationship` targets valid, and keeps rows written before the flip
  browsable through the generic concept surfaces. Disabling a pack must
  not strand its data unreadable.
- Every BEHAVIORAL construct is skipped at load: queries, mutations,
  builtins, tools, prompts, logic, automations, specs, shapes. They are
  absent from the registries, exactly as the memql#4189 acceptance
  demands -- a model cannot call the tools, an automation never fires, the
  SDK call returns "no such function".
- Its Go factories are not materialized (`app/plugins.go` consults the
  pack binding, section 4.3), and pack-conditional `app/` wiring (the
  harness reconciler, memql#4190) checks the same set.

Two consequences, stated honestly rather than hidden:

- **Strict boot still validates a disabled pack's concepts** (they load),
  but NOT its behavioral files (they are skipped before the loaders and
  the dslgate corpus pass see them). A malformed query in a disabled pack
  will surface at re-enable time, at boot, fail-loud -- which is when it
  matters. `MEMQL_DSL_ALLOW_SKIPS` keeps its separate break-glass role
  unchanged and is not the disable mechanism.
- **A flip is restart-required in v1.** The mutation reply and the portal
  both say so in as many words ("saved; takes effect as each node
  restarts"). The inventory's `enabled` vs `loaded`/`inert` split makes
  the in-between state visible instead of lying about it. Live
  re-registration was considered and rejected for v1: unloading tools/
  automations from a running engine touches every registry's lifecycle
  and buys little, since flips are rare operator actions.

### 4.3 The pack binding (Go half ↔ DSL half)

A pack is one domain with two halves registered through two primitives
(`RegisterTree` + `RegisterPlugin*`), and today nothing links them. A tiny
association fixes that without touching the 27 existing `RegisterPlugin`
callers:

```go
// component/memql: BindPluginToPack associates an integration plugin
// name with the pack domain that owns it. materializePlugins skips
// plugins bound to a disabled domain; the inventory uses the same
// binding to fold a pack's integrations into its row.
func BindPluginToPack(pluginName, packDomain string)
```

Contract packs call it next to `RegisterPluginForContract`; the harness
integrations call it from their own init()s (`BindPluginToPack(
"harnessRecall", "harness")`). An unbound plugin is an integration row;
a bound one folds under its pack.

### 4.4 Integrations and node-types: report, don't duplicate

An integration's "enablement" IS its configuration: harnessRecall opts
out without an embedding provider, email falls back to LogSender without
credentials, voice's whole lane scales to zero without LiveKit creds
(scripts/k3d/seed-secrets.sh `gate_voice_lane`, ArgoCD ignoring
/spec/replicas -- that mechanism is correct and KEPT). The registry
reports these states from their existing sources and stores nothing. The
worked example: the voice node-type row shows `credential_gated` when the
manifest's voice-required LiveKit vars are unset, with replicas from the
graph -- which is precisely what an operator needs to know before asking
"why is voice down".

## 5. Authorization

- **Reads (list + detail): owner or admin**, via the `adminops` shape --
  a policy decision (`auth.AtLeastAdmin` over the resolved
  `AccessContext`) made in one place, with the gRPC handler staying thin
  and unhooked from role logic. Below admin the surface effectively does
  not exist (the portal hides it entirely; the engine answers
  permission_denied inside the result payload, never a stream-fatal Go
  error). Module inventory is cluster-operating state, not data -- the
  pack-browser's "definitions are not data" argument does not extend to
  env-var set-ness.
- **Write (setPackEnabled): owner only**, and every call -- including
  refusals -- emits one `v1:identity:auditEvent`, mirroring
  `component/identity/adminops.authorize`. The DSL mutation's admin-tier
  classification is the second, independent layer under it.
- **Secrets: no unmasked value ever crosses the wire** (section 3), so
  there is nothing for a leaked admin token to reveal here.

## 6. Wire surface (gRPC-first)

New message types on `MemqlService.Stream` in `component/grpc/memql.proto`,
following the pack-browser request/result pattern and the established
numbering (client oneof next free = 109; server = 131):

```proto
// client oneof
ModulesListMsg      modules_list       = 109;  // owner/admin
ModuleDetailMsg     module_detail      = 110;  // owner/admin; kind + name
SetPackEnabledMsg   set_pack_enabled   = 111;  // owner; domain + enabled + reason

// server oneof
ModulesListResult   modules_list_result    = 131;
ModuleDetailResult  module_detail_result   = 132;
SetPackEnabledResult set_pack_enabled_result = 133;
```

- Every result carries `request_id`, `error_code`/`error_message` (status
  inside the payload, never a handler error -- a returned error tears down
  the multiplexed stream), and the reporting-node envelope facts.
- `ModulesListResult` carries the module rows (section 2.2).
  `ModuleDetailResult` adds the env-var surface (section 3) and health.
- `SetPackEnabledResult` echoes prior and new state plus
  `restart_required: true` -- the honesty bit the portal Dialog renders.
- Handlers live in a new `component/grpc/module_handlers.go`, delegating
  policy to a small `component/memql` (or sibling) registry package that
  owns assembly + authorization, `adminops`-shaped.
- The typed Go SDK gains a thin read surface following `sdk/go/pack`'s
  rule (no `memqlv1` type leaks); `make sdk-gen` + `make proto-gen` both
  rerun, and their `-check` gates stay green.

Health per module row is v1-minimal: node-types report graph health;
packs/integrations report their load/materialization outcome. Deeper
health (probe endpoints) is out of scope here.

## 7. Observability join

The portal's drill-in (memql#4192) filters `v1:observability:codeMetric` /
`invocation` rows by `codeReference`. Module rows carry the join keys the
engine can honestly assert today:

- integrations (and packs' integration halves): `fqnPrefixes =
  ["integration.<name>."]` -- matching the `@executor` FQN convention.
- node-types: `codeReference` from `v1:cluster:nodeType.codeReference`
  (the existing architecture-model bridge).
- components: no mapping in v1. The manifest component vocabulary does not
  map onto invocation FQNs, and inventing a lossy mapping would fake the
  surface. If the drill-in needs it, that is an engine-side gap to file,
  per the memql#4192 instruction, not something to approximate here.

**The engine-side gap is closed by memql#4208**, as a DSL query rather
than a Stream message. `codeMetricsInWindow`
(`dsl/observability/queries.memql`) takes the module's `fqnPrefixes`, an
optional exact `codeReference`, the bucket and a `[windowStart,
windowEnd)` range, and selects rows with the `startsWith` filter predicate
that issue added to the language (`codeReference startsWith
args.prefixes` -- starts with ANY of, compiled to a parameterized `^@
ANY(text[])`). The portal drill-in issues that one query for the selected
window and walks its keyset cursor to exhaustion; the 3 x 200 client-side
cap and the coverage footer are gone. The unmapped `component` rows stay
unmapped: the portal renders "no code reference mapped for this module"
and issues no read, and the predicate's own rule -- an empty prefix list
with no exact key selects nothing -- means the engine would return an
empty result rather than a cluster-wide scan if it did.

## 8. Harness-as-pack (the proving migration, memql#4190)

The harness is the proof that a substantial, engine-adjacent capability
can be a module. Shape of the migration, per the inventory taken for this
design:

- **Files stay at `dsl/harness/`.** The embed directive in `dsl/embed.go`
  drops `all:harness` from the CORE embedFS var; a sibling var in a new
  `dsl/harness_pack.go` embeds the same files and an init() registers
  them via `RegisterTree("harness", ...)`. Because `coreDomains()` reads
  the core embedFS root, removing the token automatically frees the
  domain for pack registration -- one edit, no 977-line file moves, and
  the per-package `embed_inventory_test` counts stay put.
- **The Go half stays where it is**: `integrations/harnessrecall` /
  `integrations/harnesstrace` keep their `RegisterPlugin` init()s and add
  `BindPluginToPack(..., "harness")`. The blank imports in
  `app/plugins_core.go` stay.
- **`setupHarnessReconciler` (planner/agent builds) consults the disabled
  set** before wiring the reconciler, replay, and observation sink.
- **The one hard cross-domain edge is kept working by mounted-inert
  semantics**: `dsl/actions/concepts.memql` imports harness `plan`/`step`
  as relationship targets; concepts always load, so a disabled-harness
  boot still resolves. No compat shim, no field surgery.
- **Default enabled; disabling is the new capability.** A disabled-harness
  mesh boots green, `make test` and the db-gated trees pass in both
  states, and the cluster-e2e case exercises a disabled-harness read
  through multiple replicas.
- Docs: the harness page notes it is a module of the platform
  (memql#4197/#4198 carry the story-level rewrite).

Known consumers verified degradable: agent-side `HarnessRole` scoping
reads a hint key (no DSL call); action replay is env-gated OFF by
default; `component/memql/harness_step_validation.go` guards inserts that
can no longer be produced (dead but harmless when disabled); routing
rules: none exist for harness events.

## 9. Test plan

- Unit: pack-binding registry; disabled-set plumbing; loader skip logic
  (behavioral constructs absent, concepts present); manifest join;
  masking (a secret entry never carries a value -- asserted structurally).
- db-gated (`component/memql`, `component/grpc`, `examples/referencepack`
  trees): boot with a packState row disabling referencepack -> tools/
  builtins absent, concept present, namespace collision still refused;
  flip via the mutation; inventory reflects enabled-vs-loaded honestly.
- cluster-e2e (`test/clustere2e`, behind the existing env gates): disable
  referencepack through one connection, read inventory across >=4
  connections (replica spread), assert cluster-scope agreement; the
  disabled-harness mesh case for memql#4190.
- Gates that must stay green and are expected to move: `proto-gen-check`,
  `sdk-gen-check`, `arch-model-check`, `embed_inventory_test` (dsl file
  count unchanged by design), `area_graph_dag_test`, the dslconformance
  classification (new mutation lands in the admin bucket), the docs
  gates for the vocabulary section.

## 10. Alternatives considered

- **Env-var toggles per pack** (the `MEMQL_ACTION_REPLAY_ENABLED` shape):
  rejected -- per-node env is exactly how a two-replica mesh ends up
  running two different products; the graph is the only shared truth.
- **`v1:platform:globalVariable` rows instead of a dedicated concept**:
  rejected -- enablement deserves a typed schema, its own authz tier, and
  an id an operator can find; stringly-typed variables hide all three.
- **Unmounting a disabled pack's tree entirely**: rejected -- it frees the
  namespace (collision refusal must survive), strands written rows, and
  breaks cross-domain concept imports (the harness/actions edge) for no
  operational gain over mounted-inert.
- **Live (no-restart) flips in v1**: rejected as scope -- registry
  lifecycle surgery for a rare operator action; the design keeps the seam
  (state is read in one place) so a later epic can add it.
- **Components as architecture-model services or Go packages**: rejected
  (section 2.1) -- one declared service; packages carry no operator
  meaning and no env join.
- **Masked secret values (prefix/last-four)**: rejected -- "set" is the
  operator fact; any value derivative invites a reveal endpoint.
