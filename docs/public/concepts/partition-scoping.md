---
title: Partition scoping -- the canonical tenant scope
audience: public
status: stable
area: concepts
sinceVersion: 0.9.88
owner: znas
---

# Partition scoping -- the canonical tenant scope

`partition` is memQL's **canonical tenant/scope primitive**. When core code
(the engine and the core services) needs to scope work to a tenant, it scopes
by **partition** -- never by a product notion like a pack's `spaceId`.

This page is the reference for the partition-scoping API and the rule that
keeps core product-agnostic. It pairs with the [Plugin SDK](../build/plugin-sdk.md),
where `ResolvePartitionFromContext` is exposed to packs.

> **Adopt, don't invent.** The partition primitive already exists
> (`v1:platform:partition`, `v1:platform:partitionSecret` /
> `partitionVariable`, the context resolvers below). This is the one to use;
> there is no new partition concept.

---

## What partition is (and what #56 changed)

A **partition** is a tenant scope key carried on the request context.

- **Today** it is the live scope for **per-tenant configuration and secrets**:
  the resolvers fall back partition -> global, so a tenant's BYOK key or
  feature flag wins over the instance default
  (`v1:platform:partitionSecret` / `partitionVariable` ->
  `globalSecret` / `globalVariable`). It also tags event topics and log
  fields.
- **`#56` retired partition's *data-plane* role** -- it no longer gates
  per-row storage isolation, and `envelope.partition` is a no-op on the wire.

The platformization program **re-establishes `partition` as the canonical
tenant scope for core services**: the scope key core threads through context
and resolves with the API below. Epic 3 re-points the core call sites that
currently lean on `spaceId` onto this scope. That is a deliberate direction,
not a contradiction of #56: #56 removed the old per-row partition column;
this adopts the surviving partition primitive as the one scope vocabulary core
speaks.

---

## The scoping API

| Symbol | Where | Use |
|---|---|---|
| `memql.PartitionScope(ctx) string` | `component/memql/partition_scope.go` | the single sanctioned read for core -- returns the context partition, or `DefaultPartition`; needs no engine |
| `(*MemQLEngine).ResolvePartitionFromContext(ctx) string` | `component/memql/engine.go` | engine-aware resolve (context override -> engine default -> `"default"`); the form exposed to packs on `PluginContext` |
| `memql.PartitionFromContext(ctx) string` | `component/memql/partition_context.go` | raw read (empty string if unset) |
| `memql.ContextWithPartition(ctx, p) context.Context` | `component/memql/partition_context.go` | attach a partition to a context |
| `memql.DefaultPartition` | `component/memql/partition_scope.go` | the `"default"` fallback scope |

Rule of thumb: **read the active scope with `PartitionScope` (or
`ResolvePartitionFromContext` inside the engine / from a pack); never derive a
scope from a product id.**

---

## Worked example: mapping a product concept (`space`) onto a partition

A product pack's `space` (`v1:cognition:space`) is a **product concept scoped
*by* a partition** -- it is not itself a scope key. The mapping is
one-directional:

```
request  --carries-->  partition P   (the tenant scope)
                          |
                          +--  space S1   (a product space within tenant P)
                          +--  space S2
                          +--  agents, knowledge, ... (all within P)
```

So a core operation that today reaches for `spaceId` to "scope" should instead
read the partition off the request:

```go
// WRONG (core leaking a product scope key):
//   scope := req.SpaceId
//
// RIGHT (core scoping by the canonical primitive):
scope := memql.PartitionScope(ctx)        // any core package
// or, inside the engine / from a pack:
scope := engine.ResolvePartitionFromContext(ctx)
```

The `space` still exists -- the product pack owns it and may filter its own
rows by `spaceId` *within* the active partition. What changes is that **core
stops treating `spaceId` as the scope**: the tenant boundary is the partition,
and a space is one of many things that live inside it.

The actual re-pointing of the existing core call sites is **Epic 3.2** (issue
#1899); this issue makes the target real, documented, and lint-guarded.

---

## Guardrail: no new `spaceId` in core

A test ratchet -- `TestNoNewSpaceIdInCore` in
`component/memql/partition_scope_lint_test.go` -- fails when a core `.go` file
outside the grandfathered baseline
(`component/memql/testdata/spaceid_core_baseline.txt`) introduces `spaceId`.

- The ~51 files that reference `spaceId` today are baselined and get cleaned up
  by Epic 3.2.
- **New core code must scope by partition**, so a fresh `spaceId` reference
  trips the lint with a pointer back here.
- As Epic 3.2 removes `spaceId` from a baselined file, it prunes that path from
  the baseline (additions-only ratchet -- a baseline file that no longer
  references `spaceId` is harmless, but pruning keeps the list honest).
