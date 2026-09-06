---
title: Partition scoping -- the canonical tenant scope
audience: public
status: stable
area: concepts
sinceVersion: 0.9.88
owner: znas
---

# Partition scoping -- the canonical tenant scope

`partition` is MemQL's **canonical tenant/scope primitive**. When core code
(the engine and the core services) needs to scope work to a tenant, it scopes
by **partition** -- never by an ad-hoc per-feature key.

This page is the reference for the partition-scoping API and the rule that
keeps core product-agnostic. It pairs with the [Plugin SDK](../build/plugin-sdk.md),
where `ResolvePartitionFromContext` is exposed to packs.

> **Adopt, don't invent.** The partition primitive already exists as the
> per-tenant config/secret concepts (`v1:platform:partitionSecret` /
> `partitionVariable`) plus the context resolvers below -- there is no
> separate `v1:platform:partition` registry concept. This is the one to use;
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
  per-row storage isolation, and the `partition` wire field was removed
  (`reserved "partition"` in `component/grpc/memql.proto`; the envelope
  no longer carries it and nothing derives scope from it).

The platformization program **re-establishes `partition` as the canonical
tenant scope for core services**: the scope key core threads through context
and resolves with the API below. That is a deliberate direction, not a
contradiction of #56: #56 removed the old per-row partition column; this
adopts the surviving partition primitive as the one scope vocabulary core
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

## Worked example: a product concept scoped *by* a partition

A concept that names a customer -- `v1:accounts:account`, and the `accountId`
every campaigns row carries -- is a **product concept scoped *by* a
partition**. It is not itself a scope key. The mapping is one-directional:

```
request  --carries-->  partition P   (the tenant scope)
                          |
                          +--  account A1   (a client within tenant P)
                          +--  account A2
                          +--  agents, knowledge, ... (all within P)
```

So a core operation that reaches for `accountId` to "scope" should instead
read the partition off the request:

```go
// WRONG (core leaking a product id into the scope position):
//   scope := req.AccountId
//
// RIGHT (core scoping by the canonical primitive):
scope := memql.PartitionScope(ctx)        // any core package
// or, inside the engine / from a pack:
scope := engine.ResolvePartitionFromContext(ctx)
```

The `account` still exists -- it records whose work a row is for, and a
product surface may group its own rows by it *within* the active partition.
What changes is that **core never treats a per-feature id as the scope**: the
tenant boundary is the partition, and everything else is one of the many
things that live inside it.
