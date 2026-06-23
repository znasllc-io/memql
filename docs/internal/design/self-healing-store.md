# Self-healing two-tier base/overlay store (Epic 4)

Status: E4.1 + E4.2 landed. E4.3–E4.6 in progress.

The self-healing layer lets a precondition miss (E4.1) trigger an LLM
repair loop (E4.4) that proposes a TYPED patch (E4.3), which a human
validates (E4.5) and which is captured as a healed override resolved over
a two-tier base/overlay store (E4.2). The deploy spine stays
authored/deterministic — it is guarded by preconditions but never
LLM-healed.

## E4.1 — Preconditions first-class + miss signal

A `precondition NAME { check / literal / description }` block on an
automation (see [functions.md](../../public/language/functions.md#preconditions-self-healing))
is a deterministic boolean check evaluated before any step runs. A miss
aborts the run cleanly and emits `healing.precondition.missed` (see
[events.md](../../public/concepts/events.md#self-healing-events)) — the
clean repair trigger AND the cross-machine portability signal. The miss
topic forwards across the mesh via a `healing.#` broadcast routing rule
(`component/node/routing.go`); `automation.#` is mesh-blocked, so the
signal needs its own topic.

## E4.2 — Two-tier base/overlay store

A self-healable construct (an automation, a precondition, a guard, a
literal) has two tiers of definition:

- **BASE** — the immutable, authored/embedded definition. The deploy
  spine's source of truth; never LLM-healed. Supplied by a `BaseProvider`
  wired to the embedded construct.
- **OVERLAY** — a healed override: a `v1:healing:healedOverride` data row
  produced by the repair loop and human-validated. It shadows its base
  when, and only when, it is a VALID, active overlay.

**Resolution prefers a valid overlay override and falls back to base.**
This mirrors the Epic 1 RBAC base/custom hybrid (immutable `predefined`
base roles + dynamic custom data, newest-wins).

### Concept — `v1:healing:healedOverride`

`dsl/healing/concepts.memql`. Per-row authz: owned. Key fields:

| Field | Role |
|-------|------|
| `baseConstructId` | the construct this row defines/overrides (the resolution key) |
| `tier` | `base` \| `overlay` — the immutability axis |
| `overrideData` | the healed construct body (the typed-patch result, E4.3) |
| `valid` | only a VALID override shadows base — an unvalidated heal is invisible to resolution |
| `version` | monotonic; each accepted heal captures a new version (E4.5) |
| `active` | soft-deactivate; an inactive override reverts resolution to base/older |

### Immutability guard

`component/memql/healing_base_immutable_validation.go`,
`validateHealingBaseImmutable`, wired in `executeWrite` by concept name
with a prior-row `tier==base` flag (`meta.priorBaseTier`). It rejects any
non-system-actor write to a `tier=base` row — the override-is-data
contract: a healed override can never be forged as base to escape data-tier
treatment. Exact analogue of `validateRbacBaseRoleImmutable`.

### Resolution

`component/healing/resolver.go`, `Resolver.Resolve(ctx, baseConstructId)`:

1. valid, active overlay override with a body → return its `overrideData`
   (`TierOverlay`).
2. otherwise → return the base definition (`TierBase`).
3. neither → error.

Fail-closed: an overlay-lookup error degrades to base (the deterministic
spine), never silently wins or hard-fails while a base exists.

The overlay read is the `resolveValidOverride` DSL query (owned +
tier=overlay + valid + active + newest-version). The resolver is pure of
engine/DB types (like `actionreplay`): the caller injects the
`OverlayLookup` (backed by the query) and the `BaseProvider` (backed by the
embedded construct).

### Multi-node

Overrides live in the shared Postgres, so every node resolves the same
newest-valid override; a write to the concept rides the `cache.invalidate`
broadcast (already mesh-forwarded) so a cached resolution drops on every
replica. The resolver holds no cross-node state.

## Mutations (E4.2)

- `proposeOverride` — writes a `tier=overlay`, `valid=false` row. The
  repair loop (E4.4) proposes; human validation (E4.5) flips `valid=true`
  and makes it resolution-eligible. An unvalidated heal can never silently
  take effect.

## E4.3 — Typed-patch model

`component/healing/patch.go`. A `Patch` is a TYPED transform from a base
construct (the automation/precondition JSON) to the healed overlay
`overrideData` (E4.2). Four kinds — the locked vocabulary of how the
repair loop heals a construct:

| Kind | Required fields | Effect |
|------|-----------------|--------|
| `add-precondition` | `precondition{id,check}` | append a deterministic guard to `preconditions[]` (dup id rejected) |
| `insert-guard` | `target`, `guard` | set / AND a boolean `condition` onto a step (strengthens, never silently replaces) |
| `relativize-literal` | `target`, `replacement` (+ optional `literal` safety check) | replace a machine-specific literal with a relative ref (`$config.X` / `$event.payload.X`) — the **portability heal** |
| `rebind-param` | `target`, `replacement` | rebind a param/arg ref (`$steps.a.result` → `$steps.b.result`) — the data-flow heal |

API:

- `Validate()` — kind is one of the four + per-kind required fields present.
- `Apply(base)` — deep-COPIES base (the base tier is never mutated), applies
  the kind-specific transform, then runs the **result-loads gate**
  (`validateConstructLoads`: preconditions have id+check, no dup ids; steps
  have ids). A patch whose target is absent, or that produces an unloadable
  construct (blank check, dup guard), is **rejected** — a heal can never
  blank or corrupt a construct.

`Target` is a dot-path into the construct; `steps.<id>` selects the step by
id, then descends (`steps.run.input.path`). The patch model is pure data
(no engine/LLM/DB) so the repair loop (E4.4) can produce `Patch` values via
a stub model and this package applies + validates them deterministically.
A patch's `Apply` output, stored as a `healedOverride` overrideData,
resolves through the two-tier resolver and shadows base (tested end-to-end).
