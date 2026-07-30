---
title: Shape-drift hardening
audience: internal
status: draft
area: internal
sinceVersion: 0.9.0
owner: znas
---

# Shape-drift hardening

**Status:** Proposed. Not shipped.
**Priority:** Medium — passive bug with loud failure mode.
**Owner:** TBD (memQL core).
**Related:** Any product that persists concept fields and updates records
via a full-payload mutation (all of them today).

## What happened

On 2026-04-19 a production agent named Lyra silently lost her
`role` field. The failure surfaced as "tool `uiReadState` is not
allowed for caller role `""`" — the Operator tools are scoped to
`general_assistant`, her record got rewritten with role missing, so
the AllowedRoles gate rejected every call.

Root cause was mechanical:

1. The `v1:agents:agent` concept declares
   `role enum("specialist", "general_assistant") @default("specialist")`.
2. The `agentFull` shape projects the concept back to API callers. It
   listed every payload field **except `role`** — an oversight when
   `role` was added.
3. The frontend's agent-queries use `shape(…, "agentFull")`. So
   `Agent.role` arrived as `undefined` on the client.
4. Every update path (toggle-active, color-index backfill, Edit+Save)
   re-sends the whole payload via `updateAgent`, wiring
   `role: agent.role`. Undefined.
5. `deepStripNulls` + JSON serialization dropped the field.
6. `updateAgent` does `insert({ id, payload: args.payload })`
   — **full replace**, append-only. The new version had no `role`.
7. On the next read, cognition's `agentPayload.Role = ""`. AllowedRoles
   rejected everything. Silent data loss, loud failure.

Shipped fix (band-aid-adjacent): add `node("payload.role")` to the
`agentFull` shape. Now round-trips preserve role.

The class of bug is broader: **any concept field not projected by a
shape used in the update flow can silently vanish the next time that
record is written.** We should harden against it structurally.

## Why the fix-the-shape pattern isn't enough

- It's invisible. Adding a concept field doesn't surface the
  corresponding shape update as a required change — it just works
  until someone updates a record, then it doesn't.
- The failure mode is loud (AllowedRoles reject, query breaks) but
  the root cause (a field that was dropped three updates ago) is
  miles from the failure site. Takes investigation every time.
- It's a foot-gun that scales with the codebase. More concepts, more
  shapes, more chances to miss.

## Proposals

Two options, orthogonal. Do A first; do B if the class of bug keeps
surfacing even after A.

### A. Shape-completeness lint

**What:** A test (or `make lint` target) that fails if any concept
field isn't projected by every shape that targets that concept.

**Where:** `component/memql/` (wherever concept + shape registries
are introspectable in Go), with a `TestShapesCoverAllConceptFields`
style test that runs as part of `go test ./...`.

**Implementation sketch:**

```go
// Pseudo-code
for conceptName, concept := range registry.Concepts() {
    conceptFields := concept.PayloadFields() // from .memql parse
    for _, shape := range registry.ShapesTargeting(conceptName) {
        missing := conceptFields.Minus(shape.ProjectedFields())
        if len(missing) > 0 {
            t.Errorf("shape %q is missing concept %q fields: %v",
                shape.Name, conceptName, missing)
        }
    }
}
```

**Knobs:**

- **Opt-outs.** Sometimes you genuinely want a field excluded (e.g.
  a secret that shouldn't leave the server). Support via an
  annotation on the shape: `@excludes("payload.secretToken")`. The
  lint recognises the annotation and doesn't flag.
- **Shape purpose.** Some shapes are intentionally narrow (list
  views, summary projections). The lint should distinguish "this
  shape is meant to be a subset" (annotate `@partial`) from "this
  shape is the full projection" (the default, gets checked).

**Pros:**

- Catches the bug at test time instead of on an angry user.
- Zero runtime cost.
- Maps to an existing mental model: concept fields ↔ shape fields.

**Cons:**

- Doesn't fix drift from the *consumer* side — if the frontend's
  `AgentPayload` TypeScript type forgets `role`, updates still lose
  it. That's a frontend concern, covered by the second approach.
- Lint-only fixes are catchable but still need remembering.

### B. Patch-style mutations

**What:** Mutations that merge a partial payload with the most recent
version instead of overwriting the whole record.

**Where:** New DSL builtin or a convention for `updateX` mutations.

**Status quo:**

```memql
// the product pack's updateAgent mutation
func (Mutation) updateAgent(args any) error {
  return insert({
    id: args.agentId,
    args.payload                 // full replace
  })
}
```

**Patch alternative:**

```memql
func (Mutation) updateAgent(args any) error {
  current := get(args.agentId)   // latest version's payload
  return insert({
    id: args.agentId,
    payload: merge(current.payload, args.payload)
  })
}
```

…with an official helper, so every `update*` mutation can be one
line:

```memql
func (Mutation) updateAgent(args any) error {
  return patch(args.agentId, args.payload)
}
```

**Semantics to pin down:**

- **Nil vs. omitted.** Caller passes `{"name":"X"}` — clearly only
  `name` is being changed. Caller passes `{"name":"X","role":null}`
  — is that "clear the role" or "don't touch the role"? Need an
  explicit convention (proposal: `null` = clear, absent = no-op; the
  DSL-level `args.payload` has to distinguish).
- **Concurrent writers.** Read-modify-write introduces a classic
  race window. Options: (a) accept last-writer-wins (fine for most
  concept updates); (b) add a `@version` field and use CAS; (c)
  serialize updates per-ID at the engine layer. Start with (a) and
  upgrade if we see conflicts in practice.
- **Append-only compatibility.** memQL stores each version; patch
  doesn't change that. Each `insert` still writes a new row with the
  fully-merged payload. Readers see the merged row, writers read the
  previous merged row. No behavioural change for queries.

**Pros:**

- Eliminates the class of bug entirely. Frontend can send
  `{"name": "X"}` and the rest of the record is preserved.
- Less frontend boilerplate (no need to read-then-write full agent
  payloads when toggling one flag).
- Small, well-defined surface area — a single new DSL helper.

**Cons:**

- Touches the mutation dispatch layer, needs proto/engine changes.
- Semantic decisions (nil vs. omitted) are load-bearing; get them
  wrong once and we're stuck with a footgun forever.
- Does NOT help read-side drift: a narrow shape still can't tell
  the frontend about a field that exists on disk. (That's approach
  A's job.)

**Recommendation:** do approach A first because it's cheaper, test-
only, and catches the common case. Do approach B when the team has
a quiet week and/or when we hit another instance of shape drift;
it's a nicer architecture overall but costs more to do right.

## Test strategy

For A:

1. Add a unit test in `component/memql/` (or wherever concept + shape
   registries are walked) that enumerates concept fields and shape
   projections and diffs.
2. Regression test: write a concept `Foo` with fields `{a, b, c}`, a
   shape `fooFull` projecting `{a, b}`, assert the lint fails with
   "shape `fooFull` missing field `c`".
3. Exempt `@partial` shapes from the check. Test the annotation is
   respected.

For B:

1. Concept `Foo` with fields `{a, b}`. `patch(id, {"a": "new"})`
   should preserve `b`. Assert.
2. `patch(id, {"a": null})` should clear `a` but preserve `b`. Assert.
3. Unknown-id patch: create or error? Pick one, test both paths.
4. Race: two concurrent `patch` calls on the same id with different
   keys should both land (last-writer-wins is fine, but both fields
   should survive if they don't collide). Assert.
5. Proto/engine: document the wire semantics.

## Non-goals

- Do NOT break the existing `insert({ id, payload })` pattern. Both
  full-replace and patch should coexist; callers that want "wipe and
  reset" (e.g. a delete-and-recreate flow) keep the full-replace
  semantics.
- Do NOT auto-generate shapes from concepts. Shapes are authored
  projections — they encode what the caller is allowed to see, which
  is often narrower than the full concept. Auto-generation would
  remove a useful security/privacy gate.

## Open questions

- Should the lint also walk the TypeScript `AgentPayload` /
  `Agent` types to verify the frontend consumes every shape field?
  Probably useful but cross-repo tooling adds complexity. Could be a
  `codegen:concepts` extension on the product-frontend side instead.
- Does memQL's query-projection engine already expose "which shape was
  used for this query" at query time? If so, the dispatch layer could
  emit a warning when a partial shape is used for a write path ("you
  fetched with `agentSummary` and you're updating — are you sure?").
  Probably overkill until we need it.
