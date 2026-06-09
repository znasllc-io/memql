---
title: Node Identifier Conventions
audience: public
status: stable
area: concepts
sinceVersion: 0.9.0
owner: znas
---

# Node Identifier Conventions

**Status:** authoritative reference
**Audience:** engineers writing Go code, MemQL DSL, or any consumer
of memQL events / queries (including the CoPresent frontend).

This doc covers the format of memQL node ids, how they're composed,
who composes them, and which helpers to use. Read this once and the
many ad-hoc "strip the prefix here" band-aids stop being mysterious.

---

## The canonical format

Every stored node in memQL has a fully-qualified id of the shape:

```
{concept}:{shortId}
```

Examples:

| Full id | concept | shortId |
|---|---|---|
| `v1:cognition:utterance:474e57df-...` | `v1:cognition:utterance` | `474e57df-...` |
| `v1:cluster:node:bff-local` | `v1:cluster:node` | `bff-local` |
| `v1:agents:agent:a9f3b7c2...` | `v1:agents:agent` | `a9f3b7c2...` |

Where:

- **concept** -- exactly three colon-delimited segments
  (`{version}:{domain}:{entity}`, e.g. `v1:cognition:utterance`).
  This matches the on-disk concept folder layout
  `concepts/v1/cognition/utterance/`.
- **shortId** -- a per-instance identifier, often a UUID but
  sometimes a deterministic content hash or a human-readable slug
  (`bff-local`, `general_assistant`).

The full id is what the database stores in the `id` column of
`MemoryNodes`. It's also what every read returns (queries, graph
events, gRPC subscription payloads). Treat it as the canonical
address of a node.

> **History:** the format used to carry a leading `{partition}:`
> segment. That dimension was removed in #56 phase 6; every id is now
> a plain `{concept}:{shortId}`.

---

## Composition rules

There's exactly one way memQL composes a full id, in `core/id`:

```go
id.BuildNodeId(concept, shortId)
// returns "{concept}:{shortId}"
```

The inverse:

```go
id.ParseNodeId("v1:cognition:utterance:abc")
// → concept="v1:cognition:utterance", shortId="abc"
```

Use these. Do **not** hand-roll `strings.Split(":", id)` or
`strings.LastIndex(id, ":")` -- those break on shortIds that contain
colons (rare but legal) and they couple every caller to the format.

---

## Who composes the full id

There are two writer paths:

### 1. The mutation runtime (default)

Most callers pass a **bare shortId** to `insert()`:

```memql
insert("v1:cognition:utterance", id="abc-123", payload={...})
```

The engine's `Concept.Create()` method composes the full id at
write time using `Concept.storageId(nodeId)` -- that function calls
`id.BuildNodeId(c.Name, trimmed)` if the supplied id isn't already
concept-qualified.

This is the path almost every mutation takes.

### 2. The dispatch-site composer (when the id has to be known up-front)

Some scenarios require the full id to be known **before** the
node is inserted -- because the same id will be referenced by
other emitted nodes that arrive earlier on the wire. The
canonical example is the streaming-reply flow:

```
agent turn starts
  → cognition mints replyId
  → emits N text:chunk nodes, each carrying replyId in its `replyId` field
  → finally inserts a v1:cognition:utterance with id == replyId
```

The chunks arrive at the frontend before the utterance commits.
The frontend keys its in-flight bubble by `replyId` and de-dups
against the eventual committed `utterance.id`. For that to work
without per-consumer normalization, **the chunks' `replyId` and
the committed `utterance.id` must be the same canonical string**.

The cognition handler composes that string at dispatch time:

```go
// integrations/cognition/cognition_handler.go
func composeReplyId(ctx context.Context) string {
    return id.BuildNodeId(memorynodes.ConceptCognitionUtterance, uuid.NewString())
}
```

If you find yourself adding a "stamp the id on auxiliary nodes"
flow, follow the same recipe. Compose the full id once at the
dispatch site, stamp it everywhere it's referenced, and pass it
through to the eventual `insert()` as the canonical identifier.

---

## Anti-patterns

These are the band-aids this doc exists to prevent:

- **Stamping a bare UUID where a full id is expected.** If a node
  field semantically means "the id of the upcoming utterance,"
  stamp the canonical full id, not the bare UUID. The reader has
  to compare it to a real `utterance.id` somewhere.

- **Re-deriving the concept on the read side.** If consumers
  end up calling `lastIndexOf(':')` or splitting on `:` to "match"
  ids, that's a sign the producer disagreed with the canonical
  form. Fix the producer.

- **Mixing canonical and bare ids in the same field across rows.**
  Pick one and document it on the field's `@description`.

- **Building a "shortId" by gluing in another row's full
  canonical id.** This produced two landed bugs:
  - The seed materializer wrote per-user agent rows with shortIds
    like `trainerAgent-v1:identity:user:user-30bf...` by
    concatenating `def.Name + "-" + userId` where `userId` was the
    full canonical id. The result has colons and the storage
    layer's validator rejects it.
    Strip the user's id down to its shortId first (use
    `id.ParseNodeId`), then concatenate: `def.Name + "-" + shortUserId`.
  - The checkpoint writer wrote `"checkpoint:" + executionId` as
    the shortId, duplicating the concept name inside the id.
    Concept is already in the canonical position; the shortId
    should be just `executionId`.

  `Concept.Create` rejects writes whose nodeId doesn't match one of
  the two legitimate shapes -- bare slug with no colons, or
  concept-prefixed (`v1:x:y:<short>`). See `validateShortId` in
  `component/database/memory-nodes/concept.go`.

- **Prefixing the shortId with the concept name** (issue #53).
  The concept is already in the canonical position; duplicating it
  in the shortId reads as redundant noise and produces inconsistent
  ids depending on the writer. **Rule:** the shortId is the bare
  unique part (uuid / hash / slug) and nothing else. Conformance
  test: `dsl.TestNoShortIdConceptPrefix`.

- **Prefixing the shortId with a kind / variant discriminator**
  (memql-cockpit#49). Same family as the above. The daily-space
  integration used `fmt.Sprintf("daily-%s-%s", shortUserId,
  dateKey)` to build the row's shortId. The `daily-` prefix is
  redundant with `payload.kind == "daily"` (which the mutation
  already stamps), and the hand-rolled `%s-%s-%s` recipe is
  exactly what `core/id.Engine.MustFromMap` exists to replace.
  **Rule:** when a row needs a *deterministic* id derived from
  some set of factors (so repeat-calls collapse on insert
  conflict), build it via `id.Engine.MustFromMap(map[string]any{
  ... }`. Don't hand-roll `fmt.Sprintf` recipes; don't embed
  concept / kind names in the resulting hash seed (the input map
  keys are what namespace the hash, not a leading string token).
  Variant info goes in payload fields the consumer can filter on.

- **Hand-rolling a deterministic id with `sha256.Sum256` /
  `fmt.Fprintf` etc.** Same anti-pattern; the central helper
  exists. `core/id.New().MustFromMap(...)` gives you a 64-char hex
  string that satisfies the determinism + idempotency axioms and
  centralises the format (so it can change later without touching
  every minting site). See `augmentPlanId` / `augmentChunkId` in
  `integrations/knowledge/augment_domain.go` for the migration
  shape -- those still hand-roll and should move to the helper.

The CoPresent frontend has a `stripConceptPrefix` helper for the
remaining legitimate cases (extracting a short id for a
short-channel-key, debug labels, etc.) -- see
`copresent/src/lib/memql/idUtils.ts`. Use it sparingly and never
for matching ids that are supposed to come from canonical sources.

---

## Quick reference

| You need... | Use |
|---|---|
| Compose a full id at mutation call time | Just pass the bare shortId; engine composes |
| Compose a full id at dispatch time (you'll reference it before insert) | `id.BuildNodeId(concept, shortId)` |
| Split a full id into parts | `id.ParseNodeId(id)` |
| Mint a fresh opaque shortId for an instance row | `id.NewShortId()` |
| Build a deterministic shortId from a stable factor set (so repeat calls collapse on the engine's id-conflict path) | `id.New().MustFromMap(map[string]any{...})` |
| Build a kebab-case shortId for a catalog row (stable human-chosen name) | `id.Slugify(name)` |
| Cognition: mint a replyId for a streaming agent reply | `composeReplyId(ctx)` in `integrations/cognition/cognition_handler.go` |

Frontend equivalents:

| You need... | Use |
|---|---|
| Compare two ids that should be canonical | Raw string equality (`a === b`) |
| Extract the short id for a debug label or channel key | `stripConceptPrefix(id)` from `lib/memql/idUtils.ts` |
| Tolerate a stale producer that emits bare ids | `matchesId(received, target)` from `lib/memql/idUtils.ts` (last resort -- log it as a producer bug) |
