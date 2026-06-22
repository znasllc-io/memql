---
title: Plugin SDK -- the pack extension contract
audience: public
status: stable
area: build
sinceVersion: 0.9.88
owner: znas
---

# Plugin SDK -- the pack extension contract

A **pack** extends memQL with product-specific behavior: Go integrations plus a
`.memql` DSL bundle, compiled into a node-type binary via build tags. The
in-tree CoPresent pack (`memql-bff-copresent`) is the reference consumer.

This page is the **contract reference** for the Go surface a pack targets -- the
`PluginContext` it receives, the `PluginFactory` it implements, the
registration primitives it calls, and the **contract version** the loader
checks at startup. For an end-to-end "build your first pack" walkthrough, see
[Building a pack](building-a-pack.md) -- the developer guide, with the in-tree
`examples/referencepack` reference pack as its worked example.

> **Scope.** Packs are **compiled in via build tags** -- there is no runtime
> (non-compiled) pack loading, by design. "Loading a pack" means linking its
> Go package into a binary (so its `init()` registrations run) and embedding
> its `.memql` tree. The contract below is the stable surface that linkage
> targets.

---

## Contract version

`memql.PluginContractVersion` (in `component/memql/plugins.go`) is the **major
version** of the extension surface: the `PluginContext` fields, the
`PluginFactory` signature, and the registration primitives.

- **Current: `1`.**
- It bumps **only on a breaking change** to that surface -- a removed/renamed
  `PluginContext` field, a changed `PluginFactory` signature, a changed
  registration primitive.
- **Additive, backward-compatible changes do NOT bump it** -- a new
  `PluginContext` field or a new optional primitive leaves existing packs
  compiling and loading unchanged.

A pack records the version it was built against when it registers (see below).
At startup the loader (`app.materializePlugins`) calls
`PluginRegistration.ValidateContract` for every registered pack and **rejects**
-- fatal, with a descriptive error -- any pack whose declared version is
incompatible with the core's `PluginContractVersion`. Compatibility is
**exact-major equality**: a pack built against a retired older contract (the
core may have removed surface it relies on) or one that needs a newer core than
the running binary provides (the core lacks surface it relies on) both fail
loudly rather than silently mis-binding.

`memql.CheckPluginContractCompat(version int) error` is the pure, unit-testable
form of that check.

---

## PluginFactory

```go
type PluginFactory func(pctx PluginContext) (IntegrationProvider, error)
```

A pack's factory builds its `IntegrationProvider` (the DSL-callable capability
set) from the live `PluginContext`.

- Returning an **error** aborts startup (fatal log). Use this only for a true
  misconfiguration a degraded mode cannot paper over.
- Returning **`(nil, nil)`** is the documented **opt-out** signal: the pack is
  compiled in but its dependencies are not satisfied in this environment (e.g.
  object storage with no bucket configured). The loader logs `plug-in opted
  out` and continues. The factory should log its own warning if the opt-out is
  worth reporting.

---

## PluginContext -- the surface a pack receives

`PluginContext` is the narrow, stable Go surface handed to every factory. A
pack either finds what it needs here (or on `Engine`) or it does not reach into
`app/` internals. Callbacks are lazily evaluated, so a pack that stashes the
context still observes live state.

| Capability | Type | Use |
|---|---|---|
| `Logger` | `*slog.Logger` | structured logging |
| `Engine` | `IntegrationEngineAccess` | DSL execution, prompt render, tool dispatch, streaming provider lookups |
| `BunDB` | `func() *bun.DB` | pooled DB handle (bulk queries/mutations); `nil` on a DB-less binary |
| `DirectBunDB` | `func() *bun.DB` | direct (non-pooled) handle for session-scoped work (advisory locks, leader election) -- never for bulk |
| `VisionProvider` | `func() common.VisionAIProvider` | default vision-capable AI provider, or `nil` |
| `EmbeddingProviderByName` | `func(name string) (EmbeddingAIProvider, error)` | named embedding provider |
| `ResolvePartitionFromContext` | `func(ctx) string` | active **partition** (the canonical tenant scope) for a request; `"default"` if unset |
| `ResolveVariable` | `func(ctx, name) (string, error)` | partition-scoped plaintext variable, falling back to the global |
| `ResolveSystemVariable` | `func(ctx, name) (string, error)` | instance-wide plaintext variable |
| `ResolveSecret` | `func(ctx, name) (string, error)` | partition-scoped encrypted secret, falling back to the global |
| `ResolveSystemSecret` | `func(ctx, name) (string, error)` | instance-wide encrypted secret |
| `Providers` | `*ProviderRegistry` | AI provider registry (stable pointer) |
| `Policies` | `*PolicyRegistry` | AI Router policy registry (stable pointer) |
| `Agents` | `*AgentRegistry` | DSL-declared agent registry (stable pointer) |

`ResolvePartitionFromContext` is the sanctioned way for a pack to scope work to
a tenant: `partition` is the canonical tenant scope. The dedicated
partition-scoping reference lands with issue 2.2.

---

## Registration primitives

A pack wires itself in from `init()` functions in build-tag-gated `.go` files.
The build tags decide which node-type binaries include the registration.

| Primitive | Package | Registers |
|---|---|---|
| `RegisterPlugin(name, factory)` | `component/memql` | a `PluginFactory`, stamped against the **current** contract version |
| `RegisterPluginForContract(name, version, factory)` | `component/memql` | a `PluginFactory` with an **explicit** declared contract version (third-party packs SHOULD use this) |
| `RegisterTree(domain, fs.FS)` | `dsl` | an embedded `.memql` subtree, mounted under `domain/` in the unified DSL tree |
| `RegisterRoutingRule(rule)` | `component/node` | a cross-node event routing rule (required for any event that must cross a node boundary) |
| `RegisterReadinessCheck(name, check)` | `component/server` | a readiness probe contributing to `/readyz` |

Minimal shape:

```go
//go:build copresent

package mypack

import (
	"embed"

	memqldsl "github.com/znasllc-io/memql/dsl"
	"github.com/znasllc-io/memql/component/memql"
)

//go:embed all:*.memql all:prompts
var packFS embed.FS

func init() {
	// Pin the contract version this pack was built against.
	memql.RegisterPluginForContract("mypack", memql.PluginContractVersion, newProvider)
	memqldsl.RegisterTree("mypack", packFS)
}

func newProvider(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
	// ... build the IntegrationProvider from pctx ...
}
```

---

## Pack model + load-time validation

A **pack** is exactly these three registration primitives, called together from
a pack's build-tag-gated `init()`:

1. `RegisterPluginForContract(name, version, factory)` -- the Go
   `IntegrationProvider`, **contract-version-checked** by the loader.
2. `RegisterTree(domain, fs.FS)` -- the embedded `.memql` subtree, mounted
   under `domain/` and **namespace-validated**.
3. `RegisterRoutingRule(rule)` -- cross-node event routing (required for any
   event that must cross a node boundary).

memQL validates both halves at load time and **fails loudly on a violation** --
a broken pack aborts startup rather than silently mis-binding:

- **Contract version** -- `app.materializePlugins` calls
  `PluginRegistration.ValidateContract` for every registered pack and rejects
  (fatal) any pack whose declared `PluginContractVersion` is incompatible with
  the core's (exact-major equality; see "Contract version" above).
- **Namespace ownership** -- `RegisterTree` validates the pack's DSL domain via
  `dsl.ValidatePackDomain(domain, coreDomains, existing)` before mounting it. A
  domain must be non-empty, contain no `/`, and **collide with neither a core
  embedded domain nor another pack's already-registered domain**. A core domain
  is canonical and owned by memQL -- a pack cannot shadow or extend one. Two
  packs claiming the same namespace is ambiguous. Either collision **panics**
  at `init()` time (the only caller), consistent with `RegisterTree`'s other
  input guards, so the conflict surfaces at startup with an actionable message.

`dsl.ValidatePackDomain` is the pure, unit-testable form of the
namespace-ownership check (the analogue of `CheckPluginContractCompat` for the
DSL tree). The core domain set is read from the embedded tree's top-level
directories, so it stays in lockstep with the `//go:embed` directive.

> **Out of scope by design.** Runtime (non-compiled) pack loading is not
> supported -- packs stay embedded via build tags, like `memql-bff-copresent`
> today. Validation runs at startup against the compiled-in set; there is no
> dynamic-load path to validate.

---

## Stability promise

Within a major `PluginContractVersion`, the surface above is **append-only**:
fields and primitives are added, never removed or repurposed. A pack that
compiles and loads against version `N` keeps compiling and loading against any
later core that still reports version `N`. A breaking change bumps the version
and is announced; stale packs then fail closed at startup with a clear,
actionable error rather than mis-binding.
