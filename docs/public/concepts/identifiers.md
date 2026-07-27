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
**Audience:** engineers writing Go code, MemQL DSL, or any client that
consumes memQL over the wire (browser / WS, the SDK, the LLM tool loop,
the voice-agent).

memQL uses **one** id form internally and a **different** id form on the
wire toward clients. Get this distinction and every "why is this id short
here but long there" question answers itself:

- **Canonical `{concept}:{shortId}`** everywhere INSIDE the process
  boundary and across the node mesh: storage, the internal event bus,
  cross-node routing, automations, DSL bodies, Go integrations.
- **Bare `{shortId}`** at every seam where data leaves toward a foreign
  brain: query results, subscription events, tool results, voice-agent
  acks. The engine strips the `{concept}:` prefix on the way out and
  resolves a bare id back on the way in. **Clients never compose, parse,
  or compare a canonical id.**

The rest of this doc is the canonical form + the internal composition
rules (unchanged), then the bare-ids client contract (new as of #2438).

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
  The engine assembles it from the concept declaration in
  `dsl/<namespace>/concepts.memql`: the `@version` major flows into
  the `v1:` prefix, `@namespace` supplies the domain, and the
  declaration header supplies the entity name.
- **shortId** -- a per-instance identifier, often a UUID but
  sometimes a deterministic content hash or a human-readable slug
  (`bff-local`, `general_assistant`).

The full id is what the database stores in the `id` column of
`MemoryNodes`, what the internal event bus carries, and what every Go
integration and DSL body sees. It is the canonical address of a node
**inside** the boundary. Toward clients it is bare-ified (below).

> **History:** the format used to carry a leading `{partition}:`
> segment. That dimension was removed in #56 phase 6; every id is now
> a plain `{concept}:{shortId}`.

---

## The bare-ids client contract

**Locked decision (#2438):** canonicalization is a SERVER concern only.
The client/SDK surface never composes, parses, or compares canonical ids.
The **engine owns both directions** at its wire seams.

### Outbound: the engine bare-ifies at every egress seam

On the way out, the engine strips the leading `{concept}:` off any string
shaped like a canonical node id, so the client receives a bare shortId.
This is structural, not a per-field registry: one pass covers all 122
outgoing `@relationship` FK payload fields, the pack-owned `*partitionId`
space-id fields that `@relationship` cannot express engine-side (target
`v1:cognition:space` is pack-owned), the `v1:cognition:chunk.replyId`
forward-reference, and any pack-registered concept.

The bare-ifier + its seams live in `component/memql/wire_bareids.go`
(shipped in #2441 / A2). The egress seams:

| Seam | Entry point |
|---|---|
| Query results (single node / shaped rows) | `toAPIMemoryNode`, `ToAPIResult` (`component/memql/model_helpers.go`) |
| Graph bundles | `WireBareifyBundle` |
| Subscription / graph.node events | `BareifyEventPayload` |
| Tool results to the LLM loop | `WireBareifyData` / `executeResultToToolJSON` |
| Point surfaces | `ClientToolCall` relay, voice-agent acks, guest reflections |

All of these funnel to the single gRPC egress chokepoint
(`sendServerMessage` in `component/grpc/server.go`); the browser reaches
it through the WS bridge (`/memql/ws`), which tunnels to the same
`MemqlService.Stream`.

### Enforcement: the wire contract is machine-checked

The recogniser is one shared regex,
`memqlengine.WireCanonicalIdPattern()`:

```
^v[0-9]+:[a-z0-9]+:[a-zA-Z0-9_]+:.+
```

The trailing `:.+` **requires** a 4th (shortId) segment, so a bare
3-segment concept TYPE (`v1:cognition:space`) does NOT match and is
preserved -- concept ids and subscription topics stay canonical (see the
keying rule below). The anchor means a topic string
(`graph.node.created.v1:cognition:utterance`) does not match either.

The egress bare-ifier and the wire-contract SCANNER
(`component/grpc/wire_bare_ids_test.go`) share this pattern and one
concept-carrier key allowlist (`WireConceptCarrierKeys`:
`concept` / `topic` / `eventKind` / `type` / `schema`), so they can never
drift. The scanner captures every outbound `MemqlServerMessage` at
`sendServerMessage` and fails the build if any id-position string still
matches the pattern -- a canonical-id leak is structurally impossible, and
a cross-node case in `test/clustere2e/wire_bare_ids_test.go` proves it
holds through the mesh.

### Inbound: bare args resolve server-side

A client sends a **bare** id; the engine resolves it. There is no client
composition step. Two resolution paths, both accepting bare AND canonical
from internal callers (so the transition needs no shims):

- **`id == args.X`** on a query/mutation -- `resolveFullId`
  (`component/memql/executor_filter.go`) composes the bare arg against the
  construct's signature-bound concept. A colon-bearing value must already
  be a well-formed canonical id under that concept, or it errors loudly
  (wrong-concept / legacy-prefixed ids no longer silently match nothing --
  A1 hardening).
- **`payload.<field> == args.X`** where `<field>` carries an outgoing
  `@relationship` -- `canonicalizeRelationshipComparisons` rewrites the
  bare RHS to canonical against the field's target concept before the
  filter runs (matched by the same insert-time
  `canonicalizeRelationshipFields`, so stored + queried forms agree).

DSL authors: never put a canonical `@pattern("^v1:...")` on a client-facing
arg -- that would force the client to compose a canonical id. The
conformance rule `dsl.TestNoCanonicalPatternOnArgs` and the sdk-gen gate
reject it.

### The (concept, id) client keying rule

Because an outbound id is bare, it is only unique **within its concept**.
Clients key a row by the pair `(concept, id)`, never by the id alone:

- The concept comes from the row's own `concept` field (first-class on
  every event payload), the structured graph subscription the client
  opened (`concept` + `actions`, memql#2460 -- the client already knows
  what it subscribed to), or the construct's bound concept. Clients never
  parse it out of the `topic` string.
- sdk-gen emits this metadata so a client never hardcodes it:
  `generated_concepts.ts` exports `Concepts`, `BoundConcepts`
  (construct -> concept), `CDCTopics` (`graph.node.<action>.<concept>`),
  and `CDCFilters` (`node.<action>.<concept>`). (Engine consumers get the
  Go equivalents in `sdk/go/client/generated_concepts.go`.)

Two rows with the same bare id under different concepts are different
rows; comparing bare ids across concepts is a bug.

### Fields that are bare BY CONTRACT

A few payload fields hold a foreign id that is stored bare on purpose --
they are not canonical-shaped, so the egress bare-ifier leaves them
untouched and inbound resolution does not canonicalize them. Treat their
bare form as the contract, not a producer bug:

- `documentChunk.domainId`, `documentChunk.sourceUtteranceId`,
  `documentChunk.sourceAgentId` -- bare by contract (A0 #2439; the
  descriptions say "bare id of ...").
- `forge.requestEvent.requestId` -- normalized to the short form via
  `shortId()` so the audit trail keys consistently (#1859).
- correlation UUIDs (`callId`, `correlationId`, ...) -- not node ids.

These are the deliberate-bare-reference class carried in the A1
`@relationship` exemption table
(`dsl.TestIdBearingFieldsDeclareRelationship`). A field that is a genuine
canonical FK, by contrast, declares an `@relationship` so both directions
canonicalize it internally and the egress pass bare-ifies it uniformly.

---

## Composition rules (Go / internal code)

Inside the boundary, ids are canonical and there is exactly one way to
compose one, in `core/id`:

```go
id.BuildNodeId(concept, shortId)
// returns "{concept}:{shortId}"
```

The inverse:

```go
id.ParseNodeId("v1:cognition:utterance:abc")
// → concept="v1:cognition:utterance", shortId="abc"
```

Use these in Go / internal code. Do **not** hand-roll
`strings.Split(":", id)` or `strings.LastIndex(id, ":")` -- those break on
shortIds that contain colons and they couple every caller to the format.

This is internal machinery. It has no client analogue: a client never
calls anything like `BuildNodeId`/`ParseNodeId` because it never holds a
canonical id in the first place.

### The shortId shape rule (`ValidateShortId`)

`core/id.ValidateShortId` accepts exactly two shapes and rejects
everything else:

1. **bare** -- no colons (`abc`, a UUID, a slug).
2. **concept-qualified with a bare remainder** -- `<concept>:<short>`
   where `<short>` itself has no colons.

A compound, colon-bearing remainder (a stale `<partition>:v1:...` prefix,
or a shortId built by gluing in another row's full id) is rejected (A0
tightening). `Concept.Create` runs this on every write.

---

## Who composes the full id (internal writers)

There are two writer paths, both server-side:

### 1. The mutation runtime (default)

Most mutations pass a **bare shortId** in the `insert` block (the target
concept comes from the `mutation <Concept> <name>` signature):

```memql
mutate utterance createUtterance {
  args {
    utteranceId  string  @required
  }
  insert {
    id: args.utteranceId   // bare shortId; engine composes the full id
    // ... payload fields ...
  }
}
```

The engine's `Concept.Create()` composes the full id at write time via
`Concept.storageId(nodeId)`, which calls `id.BuildNodeId(c.Name, trimmed)`
when the supplied id isn't already concept-qualified. This is the path
almost every mutation takes.

### 2. The dispatch-site composer (when the id has to be known up-front)

Some flows need the full id **before** insert, because earlier-arriving
nodes reference it. The canonical example is the streaming reply:

```
agent turn starts
  → cognition mints replyId
  → emits N text:chunk nodes, each carrying replyId in its `replyId` field
  → finally inserts a v1:cognition:utterance with id == replyId
```

The chunks reach the client before the utterance commits. The client keys
its in-flight bubble by `replyId` and de-dups against the committed
`utterance.id`. For that to work, the chunks' `replyId` and the committed
`utterance.id` must be the same string -- and both are bare-ified in
lockstep on egress (the bare-ifier's replyId lockstep is covered by
`TestBareifyEventPayload_ReplyIdLockstep`). Internally the cognition
handler composes the canonical string once:

```go
// integrations/cognition/cognition_handler.go
func composeReplyId(ctx context.Context) string {
    return id.BuildNodeId(memorynodes.ConceptCognitionUtterance, uuid.NewString())
}
```

If you add a "stamp the id on auxiliary nodes" flow, follow the same
recipe: compose the canonical id once at the dispatch site, stamp it
everywhere it's referenced, pass it to the eventual `insert()`. Egress
bare-ification keeps the client-visible forms consistent for free.

---

## Anti-patterns

These are the band-aids this doc exists to prevent (all internal / Go /
DSL -- the client surface has no id-composition to get wrong):

- **Re-deriving the concept on the read side.** If internal consumers
  call `lastIndexOf(':')` or split on `:` to "match" ids, the producer
  disagreed with the canonical form. Fix the producer; use
  `id.ParseNodeId`.

- **Mixing canonical and bare ids in the same payload field across rows.**
  Pick one and document it on the field's `@description`. A canonical FK
  declares an `@relationship` so the engine canonicalizes both directions;
  a deliberate bare reference is exempted (see the bare-by-contract class).

- **Building a shortId by gluing in another row's full canonical id.**
  Two landed bugs came from this: the seed materializer wrote
  `trainerAgent-v1:identity:user:user-30bf...` (colons -> validator
  reject; strip to the shortId with `id.ParseNodeId` first), and the
  checkpoint writer wrote `"checkpoint:" + executionId` (duplicated the
  concept name). `ValidateShortId` rejects both.

- **Prefixing the shortId with the concept name** (issue #53) or with a
  **kind / variant discriminator** (memql-cockpit#49, the old
  `daily-<user>-<date>` recipe). The concept is already in the canonical
  position; variant info belongs in a payload field the consumer can
  filter on. **Rule:** the shortId is the bare unique part (uuid / hash /
  slug) and nothing else. Conformance test:
  `dsl.TestNoShortIdConceptPrefix`.

- **Hand-rolling a deterministic id** with `fmt.Sprintf("%s-%s", ...)`,
  `sha256.Sum256`, etc. The central helper exists:
  `core/id.New().MustFromMap(map[string]any{...})` gives a 64-char hex
  string that satisfies determinism + idempotency and centralises the
  format. Don't embed concept / kind names in the hash seed -- the input
  map keys namespace the hash. In DSL bodies the equivalents are
  `id.Slugify`-style slugs or a `hash(...)` / `canonicalId(...)`
  expression on the `id:` field (see `suppressGreetOnJoin` in the pack's
  mutations).

---

## Quick reference

### Go / internal code

| You need... | Use |
|---|---|
| Compose a full id at mutation call time | Just pass the bare shortId; engine composes |
| Compose a full id at dispatch time (referenced before insert) | `id.BuildNodeId(concept, shortId)` |
| Split a full id into parts | `id.ParseNodeId(id)` |
| Mint a fresh opaque shortId for an instance row | `id.NewShortId()` |
| Build a deterministic shortId from a stable factor set (repeat calls collapse on the id-conflict path) | `id.New().MustFromMap(map[string]any{...})` |
| Build a kebab-case shortId for a catalog row | `id.Slugify(name)` |
| Cognition: mint a replyId for a streaming reply | `composeReplyId(ctx)` (`integrations/cognition/cognition_handler.go`) |
| Resolve a client-supplied bare id in a filter | Nothing -- `resolveFullId` / relationship canonicalization does it |

### Client / SDK (browser, tool loop, voice-agent, SDK consumer)

| You need... | Do |
|---|---|
| Send an id to the engine | Send the **bare** shortId as-is. Never prepend `{concept}:` |
| Compare two ids you received | Bare string equality **within the same concept** (`a === b`) |
| Uniquely key a row | The pair `(concept, id)` -- concept from the event's `concept` field / the structured subscription / `BoundConcepts`, id bare |
| Know a construct's or subscription's concept | Import `Concepts` / `BoundConcepts` / `CDCTopics` / `CDCFilters` from the generated SDK (`generated_concepts.ts`) |
| Strip / parse / compose a canonical id | **Nothing.** There is no client id-composition. If you reach for it, the producer or your keying is wrong |

> There is no client `idUtils` (no `stripConceptPrefix`, no `matchesId`).
> The SPA's `src/lib/memql/idUtils.ts` is deleted in the client cutover
> (#2443); clients hold bare ids and key by `(concept, id)`.
