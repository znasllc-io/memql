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

Concept IDs follow the format `{version}:{domain}:{entity}[:{subtype}]` where version
is a monotonically increasing identifier (`v1`, `v2`, etc.).

The version prefix is derived from the filesystem directory structure under `concepts/`.

## Current Version: v1

All active concepts use the `v1` prefix. The full inventory is defined as Go constants in
`component/database/memory-nodes/concept_ids.go`.

## Adding v2 Concepts

The infrastructure already supports multiple versions. To introduce a v2 concept:

### 1. Create the concept directory

```
concepts/v2/cognition/participant/
  concept.memql      # Concept definition with $id "v2:cognition:participant"
```

The `deriveConceptName()` function in `concept_loader.go` validates `v[0-9]+` directories
and will automatically load `v2:cognition:participant`.

### 2. Add a Go constant

In `component/database/memory-nodes/concept_ids.go`:

```go
const ConceptV2CognitionParticipant = "v2:cognition:participant"
```

Add it to `AllFilesystemConcepts()` so startup validation covers it.

### 3. Add automations (if needed)

Create `automations/v2/cognition/` for v2-specific event handlers:

```memql
@trigger(event="graph.node.created.v2:cognition:participant")
func (Automation) handleV2Participant() { ... }
```

### 4. Frontend codegen picks it up automatically

Run `npm run codegen:concepts` in the frontend after deploying the backend. The new v2
concepts appear in `generated/concepts.ts` automatically.

## Coexistence

- v1 and v2 concepts coexist in the same database and event bus
- Each version has its own schema and can evolve independently
- CDC events include the full concept ID: `graph.node.created.v2:cognition:participant`
- Subscriptions can target specific versions or use glob patterns (`graph.node.created.v*:cognition:participant`)

## Concept ID Registry

### Backend (Go)

Typed constants in `component/database/memory-nodes/concept_ids.go` provide compile-time
safety. These are validated at startup against the loaded concept registry.

### Frontend (TypeScript)

Generated constants in `src/lib/memql/generated/concepts.ts` are produced by
`scripts/codegen-concepts.mjs` which fetches from the `GET /api/concepts` endpoint.

Re-generate after backend changes: `npm run codegen:concepts`

### API Endpoint

`GET /api/concepts` returns all registered concepts with version, domain, entity,
description, and type metadata. Also includes available base topics and system topics.
