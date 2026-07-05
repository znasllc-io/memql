---
title: Concept ID Versioning
audience: public
status: stable
area: concepts
sinceVersion: 0.9.0
owner: znas
---

# Concept ID Versioning

## Overview

Concept IDs follow the format `v{MAJOR}:{namespace}:{entity}[:{subtype}]` where the
major version is a monotonically increasing identifier (`v1`, `v2`, etc.).

The engine assembles the ID from the concept declaration itself: the MAJOR segment of
the `@version("MAJOR.MINOR.PATCH")` annotation flows into the `v<MAJOR>:` prefix, the
`@namespace("...")` annotation supplies the namespace, and the concept name comes from
the declaration header. Authors never write the prefix by hand. MINOR and PATCH document
additive, non-breaking schema evolution within a major version; bumping MAJOR is the
breaking-change signal -- it changes the concept ID, every row id, every event topic,
and every cross-concept reference. See `dsl/_reference/_concept.memql` for the full
annotation surface.

## Current Version: v1

All active concepts use the `v1` prefix. The full inventory is defined as Go constants in
`component/database/memory-nodes/concept_ids.go`.

## Adding v2 Concepts

The infrastructure already supports multiple versions. To introduce a v2 concept:

### 1. Bump the concept's major version

Concepts live in `dsl/<namespace>/concepts.memql`. Stamp the new major version on the
declaration:

```memql
@version("2.0.0")
@namespace("cognition")
@description("Participant record, v2 schema.")
concept participant {
  // v2 fields ...
}
```

The loader registers it as `v2:cognition:participant`.

### 2. Add a Go constant

In `component/database/memory-nodes/concept_ids.go`:

```go
const ConceptV2CognitionParticipant = "v2:cognition:participant"
```

Add it to `AllFilesystemConcepts()` so startup validation covers it.

### 3. Add automations (if needed)

Add v2-specific event handlers to `dsl/cognition/automations.memql`:

```memql
@enabled
@trigger(event="node.created", concept="v2:cognition:participant", partition="*")
@description("Handle v2 participant creation")
automation handleV2Participant {
  step run {
    logic handleV2Participant { event: event }
  }
}
```

## Coexistence

- v1 and v2 concepts coexist in the same database and event bus
- Each version has its own schema and can evolve independently
- CDC events include the full concept ID: the topic carries
  `v2:cognition:participant`, not `v1:...`
- Subscriptions target a specific version via the full concept ID in the topic pattern

## Concept ID Registry

### Backend (Go)

Typed constants in `component/database/memory-nodes/concept_ids.go` provide compile-time
safety. These are validated at startup against the loaded concept registry.

### API Endpoint

`GET /api/concepts` returns all registered concepts with version, domain, entity,
description, and type metadata. Also includes available base topics and system topics.
Client SDKs and consumer-side codegen (e.g. a product frontend) build their typed
concept catalogs from this endpoint.
