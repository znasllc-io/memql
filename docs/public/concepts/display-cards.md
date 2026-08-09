---
title: Display Cards and the Fallback Contract
audience: public
status: stable
area: concepts
sinceVersion: 0.15.0
owner: znas
---

# Display Cards and the Fallback Contract

**Status:** authoritative reference

A **display card** is the handful of fields a view puts in a row's
chrome: what to call the row, what to say under the name, and what
badge to hang off the end. It is declared once, on the concept, and
every concept-agnostic surface -- the portal, the Cockpit's Concepts
tab, the VS Code concept panel, anything built on
[`@znasllc-io/memql-view-kit`](../../../sdk/ts-viewkit) -- reads the
same declaration.

The point of putting it on the concept is that **a concept renders the
day it is declared**. No renderer learns about `v1:telephony:number`;
the concept says `primary="e164"` and every view already knows what to
do. Concept-specific rendering code is the failure mode this whole
mechanism exists to prevent.

---

## 1. Declaring a card

```memql
/// A phone number owned by the cluster.
@displayCard(primary="e164", secondary="carrier", tertiary="purpose", status="status")
concept number {
  e164     string!  @description("E.164 number, e.g. +14155550123.")
  carrier  string
  purpose  enum("inbound", "outbound", "both")
  status   enum("active", "releasing", "released")
}
```

Four slots, in order of prominence:

| Slot | Required | Meaning |
|---|---|---|
| `primary` | yes | What the row is CALLED. The clickable label. |
| `secondary` | no | One supporting line. Quieter than primary. |
| `tertiary` | no | A second supporting line, equal weight to secondary. |
| `status` | no | Where the row sits in its lifecycle. Rendered as a badge. |

**Every slot NAMES A FIELD; it is not a value.** `status="active"`
means "read the field called `active`", not "this row is active".

### What a slot may name

The loader
(`component/database/memory-nodes/concept_parser.go`) validates each
slot at load time and refuses to boot on a bad one:

- The name must be a **top-level concept property** or one of the
  displayable **row intrinsics**: `id`, `createdAt`, `createdBy`,
  `concept`, `type`.
- The named field's type must be **displayable**: `string`, `enum`,
  `bool`, `datetime`, `int`, `float`.
- **Object and array fields are rejected.** They do not reduce to a
  single cell, and the renderers refuse to stringify them for the same
  reason -- `[object Object]` in a row label hides the mistake instead
  of showing it. If a future card genuinely needs a projection of a
  nested value, the renderer contract changes first, deliberately.
- `primary` is mandatory whenever the annotation is present. A card
  with no primary is a card with no label.

The annotation goes **first** in the concept's annotation block, below
the `///` doc comment and above `@rowAuthz` / `@namespace`.

---

## 2. Not every concept gets a card

A card is a claim that some field on the row identifies it to a human.
For a lot of concepts that claim is false. When the only candidates are
foreign keys, secret hashes, timestamps, or multi-kilobyte blobs, a
`primary` slot is a forced choice that says nothing the row id does not
already say -- and it is worse than nothing, because it looks
deliberate.

Those concepts declare the decision instead:

```memql
/// Persisted working state of a parked Task.
// @no-displayCard: A per-Task working-memory blob: nested objects, arrays, and a
//   multi-kilobyte reasoning transcript. The one scalar (`taskId`) is a
//   foreign key, not an identity -- the row's identity IS its Task, which the
//   row id already carries.
concept taskState { ... }
```

The marker is an ordinary comment, not an annotation: it changes
nothing at runtime, and it deliberately stays out of the concept's
description (the `///` block), which is user-facing prose about the
data, not a note about rendering. It sits in the same slot the
`@displayCard` annotation would have occupied, so a reader of the
concept sees the decision either way.

**Every concept in the tree is in exactly one of these two buckets**,
and `test/dslconformance/displaycard_inventory_test.go` fails the build
otherwise. A concept declared tomorrow cannot merge until someone
decides, in one line, which bucket it belongs to. The guard also
rejects a marker whose reason is too short to be an argument, and a
concept carrying both a card and a marker (a contradiction -- the card
wins at render time, so the marker would be a false statement about the
concept).

---

## 3. The fallback contract

An undeclared concept still has to render. Previously it "degraded to
the row id", which was correct but *emergent* -- it was whatever an
undefined `primary` happened to produce. The rule below is the stated
contract, implemented once in
[`sdk/ts-viewkit/src/displayCard.ts`](../../../sdk/ts-viewkit/src/displayCard.ts)
and pinned by `sdk/ts-viewkit/test/displayCard.test.ts`.

### Rule 1 -- a declared card is honoured verbatim

Inference never runs for a concept that declares a card. It never fills
an omitted slot and never overrides a declared one. An author who
declared `primary` and no `status` chose to have no status badge;
inventing one would make the annotation mean less than it says.

### Rule 2 -- an undeclared concept renders through an inferred card

Derived from **field names on the rows only**:

| Slot | Inferred from (in order) | Otherwise |
|---|---|---|
| `primary` | `name`, `title`, `label`, `displayName`, `slug` | the row id |
| `secondary` | `description`, `summary`, `subtitle` | omitted |
| `tertiary` | *never inferred* | omitted |
| `status` | `status`, `state`, `outcome`, `verdict`, `health`, `active`, `enabled`, `done` | omitted |

A candidate counts only when some row carries it as a **non-empty
scalar**. Objects and arrays are skipped, mirroring the loader's rule
for declared slots -- the inferred path must not become a way around
it. A boolean `false` DOES count: `active: false` is information, not
absence.

`tertiary` is deliberately not inferred. There is no honest generic
answer for a third slot, and guessing one would add a line of noise to
every undeclared concept in the tree.

### Rule 3 -- inference is resolved once per row set

One field is chosen for the whole list, not per row. Deriving per row
would let two rows of the same concept label themselves off different
fields, and the list would read as a jagged mix of names and ids. A row
that lacks the chosen field falls back to its own id.

### Rule 4 -- the candidate lists are field names, nothing else

A concept id or entity name in those lists would be exactly the
concept-specific renderer code this mechanism exists to prevent. The
lists are exported (`PRIMARY_NAME_FIELDS`, `SECONDARY_NAME_FIELDS`,
`STATUS_NAME_FIELDS`) so a consumer rendering its own chrome resolves
the same slots the row list does rather than re-deriving them.

---

## 4. Boolean status slots

A boolean status slot used to render the literal `true` / `false` --
the value of a field whose NAME the reader cannot see. A boolean is not
a label.

**The label is the field name; the value only decides whether it is
asserted or negated.**

| Field | Value | Badge reads |
|---|---|---|
| `active` | `true` | `active` |
| `active` | `false` | `not active` |
| `done` | `true` | `done` |
| `isError` | `true` | `error` |
| `isError` | `false` | `not error` |
| `status` | `"in_progress"` | `in_progress` |

The only transform on the field name is dropping an `is` / `has`
predicate prefix, because "not isError" is worse than the boolean it
replaced. Nothing else is rewritten -- a field name is the author's
word for the thing, and prettifying further would start guessing at
English. A field that merely begins with those letters (`issued`,
`hashed`) is left alone.

Non-boolean values pass through untouched: an enum already IS a label.

The `data-status` attribute keeps the **raw** stringified value
(`"true"`, `"false"`, `"in_progress"`), so a host colouring badges with
`.vk-row-status[data-status="failed"]` is unaffected by the prose
transform. Prose is for people; data attributes are for stylesheets.

---

## 5. Choosing slots well

- **`primary` should be stable.** A field that changes on every write
  (a current-speaker pointer, a last-activity timestamp) relabels the
  same row repeatedly. Prefer the marker to a moving primary.
- **`status` should classify, not measure.** An enum or a lifecycle
  boolean, not a counter.
- **Do not use `primary="id"` reflexively** -- it is legal (`id` is a
  displayable intrinsic) and right when the id genuinely IS the
  operator-facing name (`v1:cluster:node`'s `bff-local`), but for a
  hashed id it is just the fallback with extra words.
- **A one-slot card is fine.** `@displayCard(primary="title")` is a
  complete, honest declaration.

## See also

- [Node Identifier Conventions](identifiers.md) -- what a row id is,
  and why `primary` falling back to it is always safe.
- [MemQL Authoring Rules & Gotchas](../language/authoring-rules.md)
- `test/dslconformance/displaycard_inventory_test.go` -- the guard.
- `sdk/ts-viewkit/src/displayCard.ts` -- the implementation.
