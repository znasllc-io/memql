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

## E4.4 — LLM repair loop

`component/healing/repair_loop.go`. On a precondition-miss (E4.1), the loop
asks an LLM to **propose** one or more typed patches (E4.3) that would heal
the construct so the precondition holds.

Flow (`RepairLoop.Propose(ctx, miss, base)`):

1. **Deploy-spine refusal** — if the injected `DeploySpineGuard` says the
   construct is authored/deterministic spine, return no proposal and **do
   not even call the model**. The spine is never LLM-healed.
2. **Remediation grounding (optional)** — an injected `RemediationFeeder`
   (built on the actions substrate / `searchActions`) surfaces prior
   remediations for similar misses as additional grounding. A feeder error
   is non-fatal.
3. **Structured-output call** — `common.ChatStructuredProvider.CallChatStructured`
   with a JSON schema that constrains the model to the four patch kinds
   (`{patches: [...]}` — a top-level object, as OpenAI json_schema mode
   requires).
4. **Never trust the model** — every returned patch is `Patch.Validate()`'d,
   and (when `base != nil`) dry-run `Apply`'d; a patch that fails either is
   **dropped**. A response of only bad patches yields an empty proposal set,
   not an error. `maxPatches` caps the count.
5. **No write side effect** — proposals are **returned for human validation
   (E4.5)**, never applied.

Testability: the loop depends only on a `common.ChatStructuredProvider` and
the injected feeder/guard, so a **stub provider** returning canned
typed-patch JSON drives the whole loop deterministically — no real model, no
engine, no DB. `MissFromEventPayload` maps the `healing.precondition.missed`
event payload (E4.1) into the `PreconditionMiss` the loop consumes.

The repair loop **proposes**; it does not decide. The decision — which
proposal becomes a live overlay override, gated by role and captured as a
version — is E4.5.

## E4.5 — Human validation + capture-as-version, blast-radius-scaled by role

A proposed override (E4.4) is **human-validated** before it becomes a live
overlay, and the validation effort is **blast-radius-scaled by Epic 1 role**.

### Concept additions (`v1:healing:healedOverride`)

| Field | Role |
|-------|------|
| `blastRadius` | `personal` \| `shared` \| `spine_adjacent` — how widely the heal propagates; bigger radius ⇒ higher rank to validate |
| `validationStatus` | `proposed` \| `validated` \| `rejected` |
| `validatedBy` / `validatedAt` | who decided + when (audit) |
| `rejectionReason` | why a validator declined (audit — a rejection is recorded, not dropped) |

### Mutations

- `validateOverride` — ACCEPT: read-merges, flips `valid` false→true +
  `validationStatus` proposed→validated, stamps `validatedBy`/`validatedAt`,
  bumps `version` (**capture-as-version** — each accepted heal is a new
  version, append-only). Once validated the override is resolution-eligible.
- `rejectOverride` — DECLINE: sets `validationStatus=rejected` (`valid` stays
  false, so it is never resolution-eligible), records `rejectionReason`.

### Blast-radius rank gate

`component/memql/healing_validation_rankbound.go`,
`validateHealingValidationRankBound`, wired in `executeWrite` by concept name.
It gates **only the accept transition** (valid false→true; the prior-valid
flag `meta.priorHealingValid` is captured in the read-merge) on the actor's
role rank meeting the blast-radius floor:

| blastRadius | min rank | role |
|-------------|----------|------|
| `personal` | 100 | user |
| `shared` | 200 | admin |
| `spine_adjacent` | 300 | developer |

The owner validates any radius; a system actor bypasses (seed
re-materialization). Ranks are read from the live `v1:rbac:role` catalog
(`resolveActorRank` / `lookupRoleRankBySlug`), so a re-ranked/custom role
resolves correctly. Fails **closed**: an unresolved rank is 0, below every
floor, so the validation is denied. This is the role-gating Epic 4 ties to
Epic 1's RBAC ranks — the wider a heal's reach, the more trust required to
accept it. Mirrors `validateRbacCustomRoleRankBound`.

## E4.6 — Cockpit healed packs + end-to-end

The self-healing loop, exercisable **end to end**, plus the Cockpit surface.

### End-to-end (`component/healing/end_to_end_test.go`)

`TestSelfHealing_EndToEnd` wires all five pieces in one flow, engine/DB-free
(the store + role gate modeled by the injected `OverlayLookup` the production
resolver consumes):

```
precondition-miss (E4.1) → MissFromEventPayload
  → repair-loop proposal via a STUB model (E4.4)
    → typed-patch Apply (E4.3)
      → proposed overlay override, valid=false (E4.2): resolution falls back to BASE
        → role-gated human validation flips valid=true + new version (E4.5)
          → the two-tier resolver PREFERS the validated overlay over base,
            carrying the healed (relativized) literal; base stays untouched
```

`TestPatchPreconditionShapeParity` is the E4.3 follow-up: it asserts
`healing.PatchPrecondition` and `automations.Precondition` have identical
exported-field sets (name + order), so the shape-mirror (kept to avoid the
`automations → memql` import cycle) can't drift silently.

### Cockpit surface (`memql-cockpit/cli/healing/`)

A UI-agnostic `Controller` over the active cluster's `QueryClient` (the same
provider pattern the Concepts tab uses): `ListOverrides` (the override
history for a construct, newest version first), `Validate` (accept → a
validated, versioned overlay), `Reject` (record a rejection). The blast-radius
role gate is enforced server-side; the Cockpit surfaces the rejection error
verbatim. Tested against a fake client (no gRPC stream), driven by the SDK's
`client.ResultFromRows` test-support constructor.

Multi-node aware: every read/write goes through the engine's named
query/mutation surface, which resolves against the shared store, so the
Cockpit sees the same overrides + resolution outcome on any replica.
