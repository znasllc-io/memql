---
title: MemQL Operator Capabilities
audience: public
status: stable
area: ai
sinceVersion: 0.9.0
owner: znas
---

# MemQL Operator Capabilities

> **Last updated:** 2026-07-05

This document is the single index of agent capability slugs and how
they expand into concrete tool names. Authoring an agent seed (`seed`
construct under `dsl/agents/`) declares `capabilities.tools[]` --
this list mixes concrete tool names like `workerStatus` with
high-level slugs like `workbench-use`. The expansion machinery is the
generic capability registry in
`component/memql/capability_registry.go`; the engine's own slug
bundles register from `component/memql/worker_caps.go`. This doc is
the human-readable view.

---

## Why slugs at all?

Two pressures:

1. **Author surface stability.** When the engine adds or renames a
   primitive (e.g. `workerStatus` -> `workerHealth`), every agent seed
   referencing the old name would have to be updated. Capability
   slugs absorb that change inside the expansion table.
2. **Authorization stays unified.** The `computer-use-headless` and
   `computer-use-embodied` slugs split for tooling reasons, but the
   scope-grant / kill-switch / knowledge-domain model is one
   decision. Slugs let the frontend surface "did you allow this
   agent to drive your computer?" without enumerating the
   underlying tools.

---

## 1. The capability slugs (engine-owned)

| Slug | Mode | Expands to | Authorization |
|------|------|------------|---------------|
| `computer-use-headless` | Shell / filesystem / HTTP on the user's machine | `workerHost`, `workerStatus`, `requestComputerUseScope`, `canvasPublish` | Three layers: agent capability flag, standing scope on `v1:agents:agentAuthorization.computerUseScope` (observe / interact / full), per-Plan kill switch on `v1:identity:user.preferences.computerUseEnabled`. |
| `computer-use-embodied` | Mouse / keyboard / screenshot on the user's machine | `workerComputer`, `workerStatus`, `requestComputerUseScope`, `canvasPublish` | Same three layers as headless. Both modes share the auth model. |
| `workbench-use` | Sandboxed Linux per-Plan in the cluster | `workbenchHost`, `canvasPublish` | Universal -- default-on for every agent. No scope grants, no kill switch, no per-agent gating. Blast radius is contained to the per-Plan directory tree. |

**Defined in** `component/memql/worker_caps.go`, registered into the
`capabilitySlugs` map in `component/memql/capability_registry.go` via
`RegisterCapabilitySlug` at `init()` time.

Capability slugs are **engine-owned**. Reusable capability bundles that
were once product-specific -- a UI-control bundle whose tools drive a
client, chat, daily-space, avatar-direct -- are absorbed into the engine
as **generic features**: they register from an engine bundle via
`RegisterCapabilitySlug(slug, tools, tags...)` at `init()` time, the same
path as the worker/workbench slugs above, and expand identically on every
product-agnostic engine image. A slug registered with the `operator` tag
(`CapabilityTagOperator`) marks tools that let an agent drive a UI on the
user's behalf; the agent replier keys its operator scope-fence and
app-profile decisions on that tag via `HasCapabilityTag`.

---

## 2. How expansion works

When an agent seed declares `capabilities.tools[]`, the list flows
through `ExpandCapabilitySlugs(raw []string) []string`:

- Concrete tool names pass through unchanged.
- Recognized slugs expand to their tool list.
- **Unknown slugs pass through unchanged** -- the downstream tool-loop
  filter rejects them with "unknown tool", surfacing the typo to the
  agent runtime rather than silently dropping the reference.
- Duplicates collapse to the first occurrence.

Example:

```yaml
# agent seed body fragment
capabilities {
  tools: [
    "computer-use-headless",   # slug
    "respondToUser",           # concrete
    "workerStatus",            # concrete (already in the headless expansion)
  ]
}
```

After expansion:

```
[workerHost, workerStatus, requestComputerUseScope, canvasPublish, respondToUser]
```

The duplicate `workerStatus` is collapsed; `respondToUser` stays in
seed order; the slug `computer-use-headless` is replaced by its
expansion.

---

## 3. Authorization model (computer-use family)

The two computer-use modes share authorization because both act on
the user's machine. Three layers are checked BEFORE dispatch (see
[docs/public/operate/workers-runbook.md](../operate/workers-runbook.md) for the
operator-side narrative):

1. **Agent capability flag.** Does the agent declare
   `computer-use-headless` or `computer-use-embodied` in its
   `capabilities.tools[]`?
2. **Standing scope.** `v1:agents:agentAuthorization.computerUseScope`
   = `observe` / `interact` / `full`. Determines what the agent may
   call.
3. **Per-Plan kill switch.** `v1:identity:user.preferences.computerUseEnabled`.
   The frontend's floating kill-switch widget flips this
   flag; an out-of-scope or disabled call transitions the calling
   Plan to `awaitingFeedback` with `feedbackReason=scope_elevation_required`.

`workbench-use` has no scope grants, no kill switch, no per-agent
gating -- the per-Plan directory is the blast radius and it's torn
down with the Plan.

---

## 4. Adding a new capability slug

Three places touch:

1. Register the slug. Engine-owned bundles live in
   `component/memql/worker_caps.go` -- define the expansion list
   (`<Name>CapabilityNames`) and call `RegisterCapabilitySlug` from
   `init()`. Slugs are engine-owned: a data-only runtime DSL bundle
   carries no Go, so there is no per-product registration path -- a
   reusable capability lives in the engine as a generic bundle here.
2. `dsl/agents/skills/` -- if the new slug needs to be advertised
   through the skill catalog (`v1:agents:skill.toolSlugs`), update
   the skill definitions.
3. This doc -- add the row to the table above.

The expansion is automatically picked up by
`ExpandCapabilitySlugs`; no further wiring is needed for the
tool-loop dispatcher.

---

## 5. Other slugs in the agent catalog (non-tool)

The agent seed body also references slug-like strings that are NOT
tool capabilities. Listing them here so authors don't confuse them
with the tool slugs above:

| Slug | What | Where |
|------|------|-------|
| `claw` | Coding-agent flag (`v1:agents:agent.claw`). Toggles OpenClaw / NemoClaw tools for the agent. Not part of `capabilities.tools[]`. | `dsl/agents/concepts.memql`, the frontend's agent edit modal. |
| `assistant` / `agent` / `delegate` | Role slugs on `v1:agents:agent.role` and `roleSlug`. | `dsl/agents/roles/` (per-role seeds). |
| `human` / `si` | `v1:cognition:participant.participantType`. | `dsl/cognition/concepts.memql`. |
| `mirror_user` / `always_on` / `always_off` | Audio / video control enum on agents. | `v1:agents:agent.audioControl`, `videoControl`. |

These are values, not capability identifiers. The author surface
distinguishes them by position: tool capabilities live inside
`capabilities.tools[]`; everything above is a top-level field on
the agent concept.

---

## 6. Reference

| Item | Source |
|------|--------|
| Capability registry (expansion, tags, boot self-check) | `component/memql/capability_registry.go` |
| Engine slug bundles | `component/memql/worker_caps.go` |
| Tool definitions | engine tools under `dsl/agents/tools/`; worker + workbench tool bodies in the engine `dsl/worker/` + `dsl/workbench/` namespaces; a product's UI-control tool bodies in its runtime DSL bundle's tools file |
| Authorization model | `v1:agents:agentAuthorization`, `v1:identity:user.preferences` |
| Operator runbook (computer use) | `docs/public/operate/workers-runbook.md` |
| Workbench runbook | `docs/public/operate/workbench-runbook.md` |
