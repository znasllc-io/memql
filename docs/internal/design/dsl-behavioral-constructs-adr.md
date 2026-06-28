---
title: Behavioral DSL constructs -- logic / action / automation contract
audience: internal
status: accepted
area: internal
sinceVersion: 0.9.6
owner: znas
---

# ADR: Behavioral DSL constructs -- the logic / action / automation contract

> **Status: ACCEPTED (owner sign-off 2026-06-27).** This ADR freezes the
> behavioral half of the MemQL DSL (logic, action, automation) into a precise,
> enforced contract *before* either humans or the planner author constructs
> against it. It is a decision record, not an implementation. It is the contract
> the action library (`docs/internal/planning/action-library.md`), the
> deployment bundle, and the retrieval-augmented planner (#587) all build to.
> All §2 rulings and the former open questions (§7) are decided; implementation
> is tracked by the issues enumerated in the root handoff file.
>
> Date: 2026-06-27.

## 1. Context

MemQL's DSL has two halves at very different maturity levels.

The **static / data half** -- `concept`, `shape`, `spec`, `trait`, `query`,
`mutation` -- is mature: it has authoring reference skeletons
(`dsl/_reference/_concept.memql`, `_shape`, `_spec`, `_trait`, `_agent`), a
clear separation of read (`queries/`) and write (`mutations/`) trees, and
enforcement through the authoring sandbox.

The **behavioral / dynamic half** -- `logic`, `automation`, and the not-yet-
shipped `action` -- was never given the same treatment, and it shows:

- There are **no reference skeletons** for `_logic`, `_automation`, or
  `_action`. The contract for the behavioral constructs lives only in authors'
  heads.
- `logic` has no enforced discipline. `bootstrapCluster`
  (`dsl/cluster/logic.memql`) performs **three** writes (`createDatabase`,
  `createIdentityProvider`, `createCluster`) in one body -- read and write
  freely intermixed.
- Every automation in `dsl/cluster/automations.memql` is a **pass-through
  wrapper** whose entire body is `step run { logic X { event: event } }`.
- `@entrypoint` (`component/memql/entrypoint_logic.go`, #1707) exists purely as
  a workaround: it auto-generates a wrapping automation so that a bare `logic`
  becomes invocable -- a patch over a missing model, with its own live/dry-run
  divergence to babysit.
- The `action` construct is **parsed** as an automation step type (#1758:
  `action { ref: "act_x@3", args, surface }`) but the library, executor, and any
  real authored actions are unshipped (`docs/internal/planning/action-library.md`,
  status "Proposed. Not shipped").

Two forces make fixing this urgent now. First, the **deployment bundle**: we
want `make`-style operations (clone a version, build, deploy, promote, rollback)
expressed as first-class, replayable DSL so deployment to local / staging /
production is driven by MemQL automations rather than imperative shell. Second,
the **planner** (#587 + the action library) will soon *generate* logic, actions,
and automations with an LLM. A behavioral contract that is merely conventional
will be violated at scale the moment a model writes to it. The contract must be
**enforced** to be safe to generate against.

## 2. Decision -- five behavioral constructs, one call-graph

We adopt a five-construct behavioral model with a one-line mental model:

> **logic decides, mutations persist, actions touch the world, queries read,
> automations orchestrate and react.**

| Construct | Role | May call | May NOT |
|---|---|---|---|
| `query` | Pure read of the graph | queries, read-only builtins | any write |
| `mutation` | Exactly one atomic graph write (one aggregate boundary) | builtins; a read-modify-write on its own aggregate | write more than one aggregate; carry a trigger |
| `logic` | Pure decision + composition (control flow, shaping); returns a value | queries, other logic, read-only builtins | mutations, actions, triggers, side effects |
| `action` | Exactly one **external** capability call on a surface (`shell.*`, `fs.*`, `http.*`, `integration.*`, MCP) | one capability on a resolved surface | call logic / query / mutation / automation; touch the graph |
| `automation` | The only reactive + composing construct | logic, query, mutation, action, sub-automation; branch (`switch` / success-failure), loop (`forEach`), fan-out (`parallel`) | -- |

The five rulings that produce this table, each decided deliberately:

### 2.1 Logic is pure (Q1)

`logic` performs **no** mutations and **no** actions. It reads via queries,
composes, decides, and returns a value. Deterministic and unit-testable by
construction.

Rationale: pure logic is the only form that survives the action library's
replay-and-verify thesis (a logic with a hidden write cannot be replayed
token-free and fingerprint-verified); it is what command-query separation
actually requires; and it keeps logic reusable from any context. The
single-writer instinct is **preserved but relocated** to the automation step
(decide via logic -> persist via one mutation) -- it is an aggregate-boundary
discipline, which belongs at the orchestration boundary, not inside logic.

### 2.2 Composites collapse into automations (Q2)

There is **one** sequencer. `kind=composite` from the action-library draft is
removed. The library holds two first-class object kinds: **actions** (atomic)
and **automations** (composed). A "composite" is just an automation that has
been **promoted into the library** with version + `intent` embedding +
`reliability` + the `candidate -> active -> deprecated` status ladder.

Rationale: two constructs that both sequence steps is a permanent
explain-the-difference tax on humans and the planner. The action-library draft
(§5.3) already notes a composite and an automation share the same `action(...)`
reference; the only gap was that automations were trigger-only. We close that:
an automation may be **triggered OR invoked by reference with params**. Promotion
stays trivial -- lift a run's steps into an automation block. This also resolves
the draft's open question on `skill` (skills are capability bundles *above*
automations; nothing sits between).

> **Implementation status (I12 / #2229).** The library lifecycle this section
> describes -- version + `intent` embedding + `reliability` + the
> `candidate -> active -> deprecated` status ladder, invoke-by-reference with
> params, and the planner's retrieval-augmented REUSE / ADAPT / SYNTHESIZE
> composition -- **shipped under the action-library epic #1734** (phases
> #1735-#1740 + activation #1758, all merged). The runtime carries it on the
> `v1:actions:action` concept (`dsl/actions/concepts.memql`): primitives
> (`kind=primitive`) and **composites** (`kind=composite`, `steps[]` of child
> action refs). A composite IS the "automation promoted into the library" this
> section names -- same versioned, embedded, reliability-scored, status-laddered
> object; the only terminology drift is that the shipped concept tags the
> composed kind `composite` rather than `automation`. Invoke-by-reference is
> `action("id@version")` (`component/harness/actionpin`), retrieval is
> `searchActions` (pgvector over `intent`, `integrations/actionsearch`),
> composition is `component/harness/actionplan` (REUSE >= 0.82 / ADAPT >= 0.60 /
> SYNTHESIZE), and reliability reinforcement is `component/harness/actionreplay`.
> DevOps-DSL-epic item I12 (#2229) is therefore satisfied by #1734 and was closed
> as already-delivered; a future rename of the composed `kind` to `automation`
> would be cosmetic.

### 2.3 Action = external-capability only (Q3)

An action performs exactly one **external** capability call on a surface and
**never** touches the MemQL graph. The dividing line is "what does it touch?":
outside world -> action; our graph -> mutation; read/compute -> query / logic /
read-only builtin.

Rationale: one write path to the graph, or the data layer's guarantees (single
auditable CDC-emitting write side, per-row authz, the deployment specs) collapse.
"Surface" only means anything for external state -- the graph is reached
identically regardless of surface. The two governance models differ
(`(sideEffectClass, surface)` trust for actions; role/spec authz for mutations),
as do replay semantics (fingerprint + escalate for actions; append-only +
idempotency-key for mutations). Cost: an action cannot self-persist its result --
it returns a value and the next automation step is a mutation that writes it
(the decide -> act -> persist rhythm).

### 2.4 Reactivity is automations-only; `@entrypoint` is retired (Q4)

Triggers (event / schedule / CDC) exist **only** on automations. Logic never
carries a trigger and is never a standalone entry point. `@entrypoint` is removed;
its two conflated jobs split to where they belong:

- "react to an event" -> an **automation** (triggered).
- "be callable by name from outside (run_automation / AI / MCP)" -> a **tool**
  with `@handler(type="function", name="theLogic")` -- the existing governed,
  arg-schema'd, `@mcp`-exposable surface.
- "call it from other DSL" -> just call it directly.

This leaves exactly one reactive surface (automation) and one external-invocation
surface (tool) in the language.

To remove the pass-through *ceremony* without hiding the wiring, add a terse,
explicit single-step automation form (illustrative):

```
automation registerNode @trigger(event="system.startup") => logic registerNode
```

We choose explicit terse sugar over compiler auto-synthesis: the reactive surface
stays greppable, and we avoid re-introducing the live/dry-run divergence that
`entrypoint_logic.go` has to manage. The build invariant flips from
`ValidateEntryPointLogics` to a more general dead-logic lint (every logic must be
referenced by some logic / query / tool / automation).

### 2.5 The contract is enforced, not merely agreed (Q5)

We ship two complementary artifacts from a single source of truth:

1. **Reference skeletons** `dsl/_reference/_logic.memql`, `_action.memql`,
   `_automation.memql`, matching the existing skeleton pattern -- the human-facing
   authoring contract and few-shot examples for the planner.
2. **A call-graph validator** wired into the authoring sandbox crossref
   (`component/memql/authoring_sandbox_crossref.go`, so violations are rejected at
   define -> promote time) **and** CI, enforcing the §2 table.

Rationale: an unenforced contract is a suggestion, and the receipts
(`bootstrapCluster`, the pass-throughs, `@entrypoint`) prove it drifts.
Enforcement is the highest-leverage decision because the planner will *generate*
these constructs -- the validator is the fitness function that makes
machine-authoring safe rather than a drift amplifier.

## 3. Integrations and builtins

Integrations are **not** a new construct; they are a capability backend reached
via **builtins** carrying `@executor("integration.<domain>.<fn>")`. They split by
side effect, which the §2 contract already handles:

- **read-only** integration (e.g. `integration.auth.resolveUser`) -> a builtin
  callable from queries and logic.
- **side-effecting** integration (e.g. `integration.github.tagRelease`,
  `integration.slack.post`) -> reachable **only** through an **action** (its
  capability resolves to `integration.*` instead of `shell.*`/`fs.*`).

Corollary (a small cleanup the validator enforces): any builtin used inside a
query or logic must be read-only; side-effecting executors are reclassified so
they are reached only via actions.

## 4. Worked example -- the deployment bundle

The deployment capability is today split three ways
(`dsl/deployment/specs.memql` auth gates, the deploy data model + records in
`dsl/cluster/`, and the imperative engine in `component/deploycontrol/`). We
**centralize** it into one `deployment` pack expressed in the §2 contract:

- **actions** -- the make/ops primitives, atomic and replayable:
  `cloneRepoAtVersion`, `buildImages`, `importToK3d`, `applyOverlay` / `argoSync`,
  `promote`, `rollback`, plus integration-backed actions (tag a release, notify
  Slack). Each is one capability on a deploy-runner / workbench surface.
- **logic** -- the decisions: which version, is the gate green, does the actor
  pass `requiresOwner` / `requiresDeveloperOrAbove`, what is the next semver. Pure
  over queries.
- **mutations** -- the existing `createDeployment` / `updateDeploymentStatus`
  records (one write per step).
- **automations** -- the orchestration: triggered by `deploy.requested.<env>`,
  stepping clone -> build -> deploy -> gate -> (success) record + promote /
  (failure) rollback + mark-failed.
- **`component/deploycontrol`** -- demoted from "the deployer" to **the capability
  backend** the deploy actions resolve to; its executor / driver / lockfile /
  semver primitives become capability implementations.

Authoring this by hand now (before the planner generates it) yields a real,
contract-conformant example that validates the model end-to-end.

## 5. Consequences / migration

Enforcing the contract surfaces existing debt that must be paid:

- Refactor `bootstrapCluster` (3 writes in one logic) into automation steps or a
  single aggregate-owning mutation.
- Convert the four pass-through automations in `dsl/cluster/automations.memql` to
  the terse one-line form.
- Reclassify any side-effecting builtins as actions.
- Remove `@entrypoint` and `entrypoint_logic.go`; rewrite its real users
  (`serviceVersionProbe`, etc.) as a one-line automation or a tool; swap the
  `ValidateEntryPointLogics` audit for the dead-logic lint.
- Add the three reference skeletons and the call-graph validator.

Net engine change is a **reduction** in moving parts (synthesis machinery
deleted) plus one validator.

## 6. Phased delivery

Enforcement is phased so it never gates on unshipped constructs:

- **Phase 1 -- enforce what exists today.** Logic-purity (no writes), single
  graph-write path (one mutation per aggregate), trigger-monopoly on automations,
  read-only-builtin rule. Add `_logic` + `_automation` skeletons. Convert the
  pass-throughs; add the terse automation sugar; retire `@entrypoint`.
- **Phase 2 -- action construct + rules.** When the action library lands its
  Phase 1 (concept + step executor), enforce the action rules (one external
  capability; no calls into logic/query/mutation/automation) and add the
  `_action` skeleton. Build the deployment actions on it.
- **Phase 3 -- library lifecycle.** Invoke-by-reference automations,
  version/embedding/reliability/status on both actions and automations, and the
  planner's retrieval-augmented composition against the §2 contract.

Start strict on the hard invariants; tighten iteratively so no legitimate pattern
is blocked on day one.

## 7. Resolved decisions (formerly open)

All decided 2026-06-27; recorded here so implementation has no ambiguity.

- **Terse automation + surface syntax.** The terse single-step form is
  `automation NAME @trigger(event="...") => logic logicName`; the engine
  forwards the trigger payload as `event` when the target logic declares args,
  and calls it with an empty object otherwise. Action surface binding follows the
  action-library draft: `action("act_x@3") { args { ... } } on surface(<expr>)`,
  where omitting `on surface(...)` defers to the resolver. Exact tokenization is
  finalized in the Phase 1 parser work, but this is the committed shape.
- **Capability vocabulary + `sideEffectClass`.** The capability namespaces are
  `fs.*`, `shell.*`, `http.*`, `integration.*`, `mcp.*`. `sideEffectClass`
  (`read` / `write` / `exec`) is declared **on the capability**, never on the
  action, so it cannot be spoofed by an authored or generated action.
- **Mutation single-write guard.** The validator enforces **exactly one
  graph-write call per mutation body**; a read of the *same* aggregate
  (read-modify-write) is permitted, any second aggregate write is rejected. This
  closes the back-door risk explicitly.
- **Dead-logic lint phasing.** It runs as a **warning** during the Phase 1
  migration window and is promoted to a **hard CI gate** once `@entrypoint`
  removal and the pass-through conversions land (end of Phase 1).

## Appendix -- decisions at a glance

1. Logic is pure (no writes, no actions, no triggers). [Q1]
2. Composites collapse into automations; library = actions + automations. [Q2]
3. Action = exactly one external capability on a surface; graph writes stay
   mutations; side-effecting integrations are reachable only via actions. [Q3]
4. Reactivity is automations-only; `@entrypoint` retired (tools = external
   invocation); terse one-line automation sugar replaces pass-through ceremony.
   [Q4]
5. The contract is enforced via reference skeletons + a call-graph validator in
   the authoring sandbox and CI. [Q5]
