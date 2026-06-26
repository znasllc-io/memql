---
title: Epic 2 — Platform / plugin architecture
audience: internal
status: historical
area: internal
owner: znas
---

# Epic 2 — Platform / plugin architecture

Formalize MemQL as a plug-and-play platform: a stable extension contract that
third-party packs (Go plugin + `.memql` bundle) conform to, with `partition`
as the canonical tenant scope. **Session: S2. Starts at G1. Produces G2.**

**Repo:** `memql`.

## What already exists (build on it, don't reinvent)
- `memql.RegisterPlugin(name, factory)` + `PluginContext` (Engine, DB,
  providers, **`ResolvePartitionFromContext`**, variable/secret resolvers),
  build-tag-gated per node type — `component/memql/plugins.go`.
- `RegisterTree` (DSL bundle), `node.RegisterRoutingRule` (route concept
  events to node types), `RegisterReadinessCheck`, gRPC service registration.
- `partition` primitive: `v1:platform:partitionSecret`/`partitionVariable`,
  reserved `_system` global scope, partition→global fallback.
- The `mcp` node already builds engine-only (no CoPresent DSL) — proof packs
  are optional.

---

## Issue 2.1 — Formalize & version the plugin contract (SDK surface) [foundation]
**Problem:** The contract exists but isn't documented or version-pinned as a
public interface third parties can target.
**Approach:** Treat `PluginFactory` + `PluginContext` + the registration
primitives as a versioned **Plugin SDK**. Document each capability, freeze the
surface, add a contract version constant + compatibility check at load.
**Acceptance:** A `PluginContext`/`PluginFactory` reference exists with a
version; loader rejects packs built against an incompatible contract version.

## Issue 2.2 — Adopt `partition` as the canonical tenant scope [foundation] (produces G2 with 2.1)
**Problem:** Core leaks `spaceId` (a CoPresent notion) as a scope key; the real
primitive is `partition`.
**Approach:** Make `ResolvePartitionFromContext` / `partitionId` the single
sanctioned scoping mechanism for core. Add helpers + docs. Define how a product
concept (CoPresent's `space`) **maps onto** a partition. Do **not** yet rewrite
the leaked call sites — that's Epic 3.2; here we make the target real and
documented.
**Acceptance:** A documented partition-scoping API exists; a worked example
shows a product concept mapping to a partition; lint/guidance flags new
`spaceId` use in core.

## Issue 2.3 — Extension-point audit for cognition/voice/planner [G:2.1]
**Problem:** Need to confirm packs can inject behavior into core services.
**Approach:** Trace how CoPresent extends today (event/automation triggers like
`@trigger(event="graph.node.created.*.v1:cognition:utterance")` + routing
rules). Decide whether event/automation + routing is sufficient, or whether a
few **explicit in-process hook interfaces** (a plugin implements; the service
calls synchronously in its pipeline) are needed. Add the minimal hook
interfaces only where the event model can't express the need.
**Acceptance:** A documented list of extension points per core service; any new
hook interfaces implemented + covered by an example.

## Issue 2.4 — Pack model: registration, build-tags, validation [G:2.1] [P with 2.3]
**Approach:** Formalize the pack = `RegisterPlugin` (build-tag-gated Go) +
`RegisterTree` (`.memql`) + routing rules. Add load-time validation
(namespace ownership, no collisions with core, contract version). **Runtime
(non-compiled) pack loading is explicitly out of scope** — packs stay embedded
via build tags, like `memql-bff-copresent` today.
**Acceptance:** Pack load validates namespace ownership + contract version and
fails loudly on collision.

## Issue 2.5 — Reference pack + plugin developer guide [G:2.1,2.3,2.4]
**Approach:** A minimal example pack (a couple of concepts + one automation
hooking a core service + one integration) and a developer guide: "how to build
a pack that drops into MemQL." This is the artifact external developers follow.
**Acceptance:** The example pack builds, loads into a cluster, and extends a
core service end to end; guide walks a developer through it.

---

## Parallelization within S2
`2.1` first → `2.2` (with 2.1 → **opens G2**) and `2.3 [P] 2.4` → `2.5` closes
the epic. **G2 opens as soon as 2.1 + 2.2 merge** — S3 can begin core
re-pointing then, without waiting for 2.3–2.5.
