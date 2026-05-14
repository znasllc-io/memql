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
{partition}:{concept}:{shortId}
```

Examples:

| Full id | partition | concept | shortId |
|---|---|---|---|
| `default:v1:cognition:utterance:474e57df-...` | `default` | `v1:cognition:utterance` | `474e57df-...` |
| `_system:v1:cluster:node:bff-local` | `_system` | `v1:cluster:node` | `bff-local` |
| `acme:v1:copresent:agent:a9f3b7c2...` | `acme` | `v1:copresent:agent` | `a9f3b7c2...` |

Where:

- **partition** -- `"default"` for unscoped tenant data, `"_system"`
  for `@scope("global")` concepts, or a tenant-chosen string in
  multi-tenant deploys. Always present in stored ids.
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

---

## Composition rules

There's exactly one way memQL composes a full id, in `core/id`:

```go
id.BuildNodeId(partition, concept, shortId)
// returns "{partition}:{concept}:{shortId}"
```

Empty partition defaults to `id.DefaultPartition` (`"default"`).
Helpers in the same package answer the inverse questions:

```go
id.HasPartition("default:v1:cognition:utterance:abc")  // true
id.HasPartition("v1:cognition:utterance:abc")          // false (no partition prefix)

id.ParseNodeId("default:v1:cognition:utterance:abc")
// → partition="default", concept="v1:cognition:utterance", shortId="abc"

id.StripPartition("default:v1:cognition:utterance:abc")
// → ("default", "v1:cognition:utterance:abc")

id.PrependPartition("default", "v1:cognition:utterance:abc")
// → "default:v1:cognition:utterance:abc"
```

Use these. Do **not** hand-roll `strings.Split(":", id)` or
`strings.LastIndex(id, ":")` -- those break on partitions that
themselves contain colons (rare but legal) and they couple every
caller to the format.

---

## Who composes the full id

There are two writer paths:

### 1. The mutation runtime (default)

Most callers pass a **bare shortId** to `insert()`:

```memql
insert("v1:cognition:utterance", id="abc-123", payload={...})
```

The engine's `Concept.Create()` method composes the full id at
write time using `Concept.storageId(partition, nodeId)` -- that
function calls `id.BuildNodeId(partition, c.Name, trimmed)` if
the supplied id isn't already partition-qualified. The partition
comes from the request context via `MemQLEngine.resolvePartition(ctx)`.

This is the path almost every mutation takes. Callers don't need
to know the partition.

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
    partition := memql.PartitionFromContext(ctx)
    if partition == "" {
        partition = id.DefaultPartition
    }
    return id.BuildNodeId(partition, memorynodes.ConceptCognitionUtterance, uuid.NewString())
}
```

The result is byte-identical to what `Concept.Create` would have
produced if the bare UUID had been passed through the normal
mutation path. When the utterance later goes through `insertSIResponse`,
the engine sees an already-qualified id (`id.HasPartition` returns
true) and stores it unchanged.

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

- **Re-deriving the partition on the read side.** If consumers
  end up calling `lastIndexOf(':')` or splitting on `:` to "match"
  ids, that's a sign the producer disagreed with the canonical
  form. Fix the producer.

- **Mixing canonical and bare ids in the same field across rows.**
  Pick one and document it on the field's `@description`.

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
| Compose a full id at dispatch time (you'll reference it before insert) | `id.BuildNodeId(partition, concept, shortId)` |
| Get partition from request context | `memql.PartitionFromContext(ctx)` (falls back to `id.DefaultPartition`) |
| Check if an id is already qualified | `id.HasPartition(id)` |
| Split a full id into parts | `id.ParseNodeId(id)` |
| Cognition: mint a replyId for a streaming agent reply | `composeReplyId(ctx)` in `integrations/cognition/cognition_handler.go` |

Frontend equivalents:

| You need... | Use |
|---|---|
| Compare two ids that should be canonical | Raw string equality (`a === b`) |
| Extract the short id for a debug label or channel key | `stripConceptPrefix(id)` from `lib/memql/idUtils.ts` |
| Tolerate a stale producer that emits bare ids | `matchesId(received, target)` from `lib/memql/idUtils.ts` (last resort -- log it as a producer bug) |
