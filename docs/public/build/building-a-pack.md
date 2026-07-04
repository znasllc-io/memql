---
title: Building a pack -- extend memQL with your own domain
audience: public
status: stable
area: build
sinceVersion: 0.9.7
owner: znas
---

# Building a pack -- extend memQL with your own domain

A **pack** is the unit of product-specific extension in memQL: a bundle of Go
integration code plus an embedded `.memql` DSL subtree that drops into the
engine and runs alongside the core domains. The in-tree CoPresent pack
(`memql-bff-copresent`) is the production reference consumer; this guide walks
you through building one from scratch, using the minimal **reference pack** at
[`examples/referencepack/`](../../../examples/referencepack) as the worked
example.

If you want the bare contract reference instead of a walkthrough -- the exact
`PluginContext` fields, the `PluginFactory` signature, the registration
primitives -- read [Plugin SDK](plugin-sdk.md). This guide assumes that page as
background and shows how the pieces fit together.

> **Scope.** Packs are **compiled in via build tags** -- there is no runtime
> (non-compiled) pack loading, by design. "Loading a pack" means linking its Go
> package into a binary so its `init()` registrations run, and embedding its
> `.memql` tree. See [Build Tags](build-tags.md) for the node-type tag model.

---

## What a pack is, exactly

A pack is **three registration primitives**, all called from a build-tag-gated
`init()`:

1. **`memql.RegisterPluginForContract(name, version, factory)`** -- registers
   your Go `IntegrationProvider` (the DSL-callable capability set) against the
   Plugin SDK **contract version** you built against. The loader rejects a pack
   whose declared version is incompatible with the core's
   `memql.PluginContractVersion`, so a stale pack fails loudly at startup
   instead of silently mis-binding. (`RegisterPlugin` stamps the current version
   implicitly; third-party packs SHOULD pin it explicitly with
   `RegisterPluginForContract`.)
2. **`dsl.RegisterTree(domain, fs.FS)`** -- mounts your embedded `.memql`
   subtree under `domain/` in the unified DSL tree. **Namespace ownership** is
   validated here.
3. **`node.RegisterRoutingRule(rule)`** -- a cross-node event routing rule,
   required for any event that must cross a node boundary. The minimal reference
   pack does not cross nodes, so it skips this one. (Every cross-node event-bus
   pub/sub needs a routing rule or it silently dies in cluster mode.)

Everything else -- concepts, builtins, tools, automations, prompts, queries,
mutations, specs, shapes -- lives in the embedded `.memql` tree and rides in via
`RegisterTree`. The Go side is only the protocol-adapter capabilities the DSL
cannot express.

---

## The three registration primitives

### 1. Register your Go integration (contract-versioned)

Your pack exposes Go-backed operations to the DSL through an
`memql.IntegrationProvider`: it returns a stable `IntegrationName()` and a list
of `IntegrationCapability` values, each a named handler. A capability named
`composeGreeting` on an integration named `referencepack` becomes callable from
the DSL under the FQN `integration.referencepack.composeGreeting`.

A `memql.PluginFactory` builds that provider from the live `PluginContext`:

```go
func NewProvider(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
    // pluck DB getters, providers, resolvers off pctx here (this minimal
    // pack needs none); return (nil, nil) to opt out when deps are missing.
    return &Provider{}, nil
}
```

You register it pinned to the contract version you built against:

```go
memql.RegisterPluginForContract("referencepack", memql.PluginContractVersion, NewProvider)
```

The app bootstrap (`app.materializePlugins`) calls
`PluginRegistration.ValidateContract` for every registered pack and aborts
startup -- fatal, with a descriptive error -- on any incompatibility.
`memql.CheckPluginContractCompat(version)` is the pure, unit-testable form of
that check.

> **Order gotcha (custom harnesses / tests).** A pack's builtin resolves its
> Go capability by FQN (`integration.<name>.<capability>`) at **dispatch
> time**, against the integration capabilities registered on the engine.
> Register the provider **after** `engine.Init` -- registering before Init,
> then calling Init, leaves the capability out of the dispatch map and the
> builtin fails with `unknown builtin executor "integration.<name>.<cap>"`.
> The normal app path is automatic: `app.materializePlugins` registers every
> pack provider *after* the engine is initialized. You only have to think
> about this when wiring an engine by hand (e.g. an integration test). See
> `examples/referencepack/live_e2e_test.go`.

### 2. Register your embedded DSL tree (namespace-owned)

Embed your `.memql` files and mount them under your domain:

```go
//go:embed all:dsl
var packFS embed.FS

func Tree() fs.FS {
    sub, _ := fs.Sub(packFS, "dsl")
    return sub
}

dsl.RegisterTree("referencepack", Tree())
```

After this, every DSL loader that walks `dsl.Tree()` sees your files under
`referencepack/` alongside the core domains, and your concepts/tools/builtins
register into the same engine registries core uses.

**Namespace ownership is validated at registration.**
`dsl.RegisterTree` calls `dsl.ValidatePackDomain(domain, coreDomains, existing)`
before mounting, and **panics** on a violation:

- the domain must be non-empty and contain no `/`;
- it must **not collide with a core embedded domain** -- core domains are
  canonical and owned by memQL; a pack cannot shadow or extend one;
- it must **not collide with another pack's already-registered domain** -- two
  packs claiming the same namespace is ambiguous and rejected.

This is the namespace-ownership rule from the pack model
([Plugin SDK -> Pack model](plugin-sdk.md#pack-model--load-time-validation)).
Pick a unique domain. The core domain set is read from the embedded tree's
top-level directories, so it always reflects the real `//go:embed` directive.
Partition scoping -- the canonical *tenant* dimension a pack scopes its data to
-- is a separate axis; see [Partition scoping](../concepts/partition-scoping.md).

### 3. Register routing rules (only if you cross nodes)

If your pack emits an event on one node type that must be consumed on another,
register a routing rule with `node.RegisterRoutingRule(...)` -- otherwise the
event silently dies in cluster mode. The reference pack stays on one node, so it
omits this. The contract is documented in [Plugin SDK](plugin-sdk.md).

---

## Build-tag gating: how a pack is loaded (and how it is kept out)

A pack must run **only** in the binaries that should carry it. The mechanism is
Go build tags on the file that holds the pack's `init()`:

```go
//go:build copresent

package mypack

func init() {
    Register("mypack") // dsl.RegisterTree + memql.RegisterPluginForContract
}
```

The `init()` runs only when the binary is built with that tag, and the package
is anchored into the binary via a blank import in the app bootstrap. A node type
built without the tag never links the registration, so the pack never loads
there. This is exactly how `memql-bff-copresent` gates its `copresent` domain to
the carrier-built node types. See [Build Tags](build-tags.md).

**Keeping a pack out of production entirely.** The reference pack demonstrates
the inverse: a pack that `go build ./...` compiles (so CI verifies it builds)
but that **no production binary ever loads**. The trick is twofold:

1. The pack package (`pack.go`) is **normal, untagged Go** with **no
   unconditional `init()`** -- merely linking it in does *not* register it.
   Registration is the explicit `Register(domain)` function, which tests call
   directly.
2. A separate file `register_referencepack.go` carries
   `//go:build referencepack` and the only `init()`:

   ```go
   //go:build referencepack

   package referencepack

   func init() { Register(Domain) }
   ```

   This is the *real* build-tag-gated auto-register pattern a production pack
   uses -- but the `referencepack` tag is never set in any production build, so
   the `init()` never runs and the pack never auto-loads. Swap `referencepack`
   for your product tag (`copresent`, etc.) and you have a production pack.

The takeaway: **put the pack's `init()` in a build-tag-gated file**, never in
the always-compiled package body. That single rule decides where a pack loads.

---

## A guided tour of the reference pack

The reference pack at [`examples/referencepack/`](../../../examples/referencepack)
is intentionally minimal but real -- it builds, loads into the engine
registries, and extends a core service, proven by Go tests under the default
`go test ./...`. Here is every file and what it demonstrates.

```
examples/referencepack/
├── pack.go                      Go: embed + Domain/ContractVersion + Provider + NewProvider + Register
├── register_referencepack.go    Go: //go:build referencepack -> init() { Register(Domain) }
├── reference_pack_test.go        test: concept load + provider capability + contract gate (exported API)
└── dsl/
    ├── concepts.memql            one concept (greeting, owned-tier ownerUserId)
    ├── builtins.memql            one builtin backed by @executor("integration.referencepack.composeGreeting")
    ├── tools.memql               one tool surfacing the builtin to the agent tool loop
    └── automations.memql         one automation hooking the CORE v1:cognition:space node.created event
```

**`dsl/concepts.memql`** -- a single `greeting` concept. Its id assembles from
`@version("1.0.0")` + `@namespace("referencepack")` + the name `greeting` into
`v1:referencepack:greeting`. It carries an `ownerUserId` so it models the
**owned** authorization tier (per-row authz key) the same way core concepts do,
not just an empty schema.

**`dsl/builtins.memql`** -- a single `referencePackComposeGreeting` builtin whose
`@executor("integration.referencepack.composeGreeting")` names the pack's Go
capability. This is the DSL end of the wire; the Go `Provider.Capabilities()`
in `pack.go` is the other end. The FQN's middle segment
(`referencepack`) is the provider's `IntegrationName()`; the last segment
(`composeGreeting`) is the capability `Name`.

**`dsl/tools.memql`** -- a single `referencePackGreet` tool. Its
`@handler(type="function", name="referencePackComposeGreeting")` points at the
builtin by name, so an agent that calls the tool ultimately runs the pack's Go
handler. This is how a pack surfaces a capability into an agent's tool loop.

**`dsl/automations.memql`** -- a single automation that **hooks a core service
event**: `@trigger(event="node.created", concept="v1:cognition:space", ...)`.
When the core engine creates a `space` row, this pack-owned automation fires --
the core has no knowledge of the pack. Its step calls the pack's own builtin via
a kind-prefixed call (`builtin referencePackComposeGreeting ( userName: ownerUserId )` -- the owner bound via the automation's typed `args { }` contract, G5 #2367),
so it also exercises the pack's Go capability. The automation pulls the builtin
into scope with a file-top `use referencepack.builtins.{ referencePackComposeGreeting }`
import -- the standard cross-file dependency mechanism.

**`pack.go`** -- the Go core:

- `Domain` and `ContractVersion` consts (the latter pinned to
  `memql.PluginContractVersion` the pack compiled against).
- `Tree() fs.FS` -- the `//go:embed all:dsl` subtree, re-rooted so the files
  appear directly (so `RegisterTree(Domain, Tree())` mounts `concepts.memql` at
  `referencepack/concepts.memql`).
- `Provider` -- the `IntegrationProvider`, with `IntegrationName()` returning
  `referencepack` and `Capabilities()` returning the one `composeGreeting`
  capability whose handler builds a greeting node.
- `NewProvider(pctx)` -- the `PluginFactory`.
- `Register(domain string)` -- the single entry point that does
  `dsl.RegisterTree(domain, Tree())` + `memql.RegisterPluginForContract(domain,
  ContractVersion, NewProvider)`. `domain` is a parameter so a test can mount
  the same tree under a throwaway namespace.

**`register_referencepack.go`** -- the build-tag-gated `init()` (covered above).

### How the tests prove load + extend

Two test files, both running under the default `go test ./...`:

- [`examples/referencepack/reference_pack_test.go`](../../../examples/referencepack/reference_pack_test.go)
  (external `package referencepack_test`, exported API only):
  - mounts `Tree()` under a unique throwaway domain via `dsl.RegisterTree`, with
    `t.Cleanup(dsl.UnregisterTree)`;
  - runs `memql.LoadUnifiedConcepts` (the loader the engine runs at boot) and
    asserts `v1:referencepack:greeting` is now in `memorynodes.DefaultRegistry()`
    -- the pack **extending** the core concept registry;
  - builds `NewProvider` with a minimal `PluginContext` and asserts its
    `Capabilities()` include `composeGreeting` with a non-nil handler;
  - asserts `memql.CheckPluginContractCompat(ContractVersion) == nil` and that a
    `PluginRegistration{...}.ValidateContract()` passes.
- [`component/memql/reference_pack_load_test.go`](../../../component/memql/reference_pack_load_test.go)
  (in-package `package memql`, for the unexported `ToolRegistry` constructor):
  - mounts the pack's real `tools.memql` and runs `LoadUnifiedTools`, asserting
    `referencePackGreet` resolves in the engine's tool registry -- the pack
    **extending** the core tool surface;
  - loads the real `builtins.memql` via `LoadUnifiedBuiltins` and asserts the
    builtin registers;
  - runs `dslimports.Load` over the pack's whole `dsl/` tree, asserting every
    artifact (including the core-service-hook automation and its `use` import)
    parses and resolves.

Together they prove the pack builds, loads into the engine registries alongside
core, and extends both the concept and tool surfaces -- with no database.

---

## Build your own pack: the checklist

1. Create a package directory with a `dsl/` subdir for your `.memql` files.
2. Write your concept(s) with `@version` + `@namespace("yourdomain")`; model
   authz with an `ownerUserId` (owned tier) or the granted/admin/public pattern.
3. Write any builtins (`@executor("integration.yourdomain.<fn>")`), tools, and
   automations. Cross-file deps go through file-top `use` imports.
4. Implement an `IntegrationProvider` whose `IntegrationName()` matches your
   `@executor` middle segment and whose `Capabilities()` back each builtin.
5. Write a `PluginFactory` (`NewProvider(pctx)`).
6. Embed `dsl/` and write a `Register(domain)` that calls `dsl.RegisterTree` +
   `memql.RegisterPluginForContract` (+ `node.RegisterRoutingRule` if you cross
   nodes).
7. Put the auto-register `init()` in a **build-tag-gated file** for your product
   tag, and anchor the package via a blank import in the app bootstrap.
8. Pick a **unique domain** -- it must not collide with a core domain or another
   pack.
9. Verify with `go build ./...`, `go vet`, and a load-test modeled on the
   reference pack's.

---

## Production dogfood example: the deploy pack

The reference pack is the minimal teaching example. The **deploy pack** at
[`examples/deploypack/`](../../../examples/deploypack) (Epic 2 / #2095) is the
production-shaped sibling: it packages memQL's OWN deployment workflow as a pack,
dogfooding the model. Same primitives, but its capabilities are the REAL deploy
effects.

```
examples/deploypack/
├── pack.go                   Go: Provider holding a deploycontrol.Executor + engine; 4 effect capabilities
├── register_deploypack.go    Go: //go:build deploypack -> init() { Register(Domain) }
├── deploy_pack_test.go        test: capability exposure + per-effect routing (exported API)
└── dsl/
    ├── builtins.memql         5 builtins -> @executor("integration.deploypack.{commitOverlay,argoSync,runPromote,recordBack,observeReconciledState}")
    ├── automations.memql      two CDC automations on v1:cluster:deployment status
    └── logic.memql            driveDeploymentInProgress (promote + transition) + recordReconciledState (Model A record-back)
```

The pack also shows a pack **hooking a core CDC event**: `dsl/automations.memql`
triggers on `graph.node.updated.v1:cluster:deployment` and `dsl/logic.memql`
ports `component/deploycontrol/deploy.go`'s imperative apply+transition into a
declarative chain -- when a deployment enters `in_progress`, fire `runPromote`
(the live azure effect) and transition the record to `succeeded`/`failed` on the
in-band outcome. A second automation (`recordReconciledState`, the Model A
record-back loop) observes the ArgoCD-reconciled state via
`observeReconciledState` and appends the observed per-node readiness back into
the deployment concept once the deploy reports `succeeded`. The logic imports the
pack's own effects (`use deploypack.builtins.{ ... }`) AND a core mutation
(`use cluster.mutations.{ updateDeploymentStatus }`) -- the standard
cross-namespace `use` mechanism.

The key difference from the reference pack: the deploy pack's `Provider` holds a
**`deploycontrol.Executor`** (the SAME side-effect boundary the Deploy Console
uses -- `promote.sh` / `git` / `kubectl argo rollouts`) plus an engine handle.
Its `NewProvider(pctx)` builds the Executor anchored at `MEMQL_DEPLOY_REPO_ROOT`
(mirroring `app/integrations_deploy_control.go`) and takes the engine from
`pctx.Engine`. The four capabilities are the deploy effects:

- `commitOverlay` / `argoSync` -> `Executor.Git` (Model A: author + commit the
  overlay, push so ArgoCD reconciles -- never a direct cluster apply).
- `runPromote` -> `Executor.RunPromote` -- THE live azure deploy effect
  (`scripts/release/promote.sh` via the Argo Rollout), invoked through the same
  method the Deploy Console uses. Exposing it through the pack is **additive**;
  the imperative path is untouched until E2.5 thins it.
- `recordBack` -> engine mutations (`updateDeploymentStatus` +
  `createDeploymentNodeSpec`) -- Model A record-back: mirror the
  GitOps-reconciled state into the deployment concept.

This is the canonical example of a pack contributing **effects backed by an
existing Go side-effect boundary** rather than a fresh capability. The E2.3
chained automations fire these effects on deployment status transitions; the
pack is the substrate they call into.

---

## See also

- [Plugin SDK](plugin-sdk.md) -- the contract reference (PluginContext,
  PluginFactory, primitives, contract version, load-time validation).
- [Build Tags](build-tags.md) -- the node-type tag model that gates which
  binaries carry your pack.
- [Partition scoping](../concepts/partition-scoping.md) -- the canonical tenant
  dimension your pack scopes data to.
