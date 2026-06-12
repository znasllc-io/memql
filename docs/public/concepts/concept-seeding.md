---
title: Concept Seeding
audience: public
status: stable
area: concepts
sinceVersion: 0.9.0
owner: znas
---

# Concept Seeding

Platform baseline rows -- the records that should exist in every fresh
deployment without hard-coding payloads inside Go services -- are declared
directly in the DSL with the `seed` construct. A seed declaration lives in a
`dsl/<namespace>/*.memql` file next to the concept it materializes:

```memql
use agents.concepts.{ agentRole }

@description("Public-services orientation: DMV, voter registration, permits, social services, and government benefits.")
seed agentRole civic-navigator {
  slug:                  "civic-navigator"
  name:                  "Civic Services Navigator"
  category:              "civic"
  lockedSkillIds:        ["workbench-baseline", "civic-baseline"]
  maxSkills:             5
  recommendedPolicySlug: "balancedChat"
  predefined:            true
}
```

The signature is `seed <concept> <name> { ... }`. The body is a field list;
each field maps to a same-named argument of the concept's canonical create
mutation (see "Write path" below).

## Scope: global vs. per-user

A seed's scope is stamped with `@scope`:

- **`@scope("global")`** (the default when omitted): the seed materializes
  exactly one row, stored in the reserved `_system` partition. Used for
  catalog rows such as agent roles and avatar personas.
- **`@scope("perUser")`**: the seed fans out to one row per
  `v1:identity:user`. The materializer computes the row id as
  `<seedName>-<userId>` and stamps `ownerUserId=<userId>` automatically.
  A perUser seed body must NOT declare its own id -- the loader rejects
  that. Used for per-user baselines such as the Assistant
  (`dsl/agents/assistant.memql`) and Trainer Agent
  (`dsl/agents/trainerAgent.memql`).

Long prose fields can be sourced from a template file next to the
declaration via `@templateFile("templates/<name>.tmpl")`.

## When seeds materialize

The `SeedMaterializer` (`component/memql/seed_materializer.go`) runs two
trigger paths:

1. **Startup sweep.** On engine start, every registered seed is walked and
   materialized: global seeds become one row each; perUser seeds iterate
   every existing `v1:identity:user` and produce one row per user.
2. **Runtime hook.** After the sweep, the materializer subscribes to
   user-creation events. When a new user lands, it re-runs the perUser
   sweep for just that user. Global seeds are skipped (they do not fan out
   per user).

## Idempotency

Materialization is create-only with deterministic ids: memQL is
time-series, so repeat inserts with the same id stamp a new version while
reads still see one logical row. Operator or user edits to a seeded row
(e.g. renaming an assistant) survive engine restarts -- the seed declaration
is the baseline, not a reset.

## Write path

The materializer delegates row writes to the concept's existing canonical
create mutation, so the platform has a single write path. The convention:

```
use <namespace>.<conceptName>   ->   mutationCreate<ConceptName>
```

Each seed body field maps to a same-named mutation arg, and the concept's
id arg follows the case-corrected `<conceptName>Id` pattern (`agent` ->
`agentId`, `partition` -> `partitionId`).
