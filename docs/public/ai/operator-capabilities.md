---
title: MemQL Operator Capabilities
audience: public
status: stable
area: ai
sinceVersion: 0.9.0
owner: znas
---

# MemQL Operator Capabilities

> **Last updated:** 2026-05-19

This document is the single index of agent capability slugs and how
they expand into concrete tool names. Authoring an agent seed (`seed`
construct under `dsl/agents/`) declares `capabilities.tools[]` --
this list mixes concrete tool names like `uiClick` with high-level
slugs like `copresent_control`. The engine's expansion rules live
in `component/memql/operator_caps.go`; this doc is the human-readable
view.

---

## Why slugs at all?

Two pressures:

1. **Author surface stability.** When the engine adds or renames a
   primitive (e.g. `uiSelect` -> `uiPickFromList`), every agent seed
   referencing the old name would have to be updated. Capability
   slugs absorb that change inside the expansion table.
2. **Authorization stays unified.** The `computer_use_headless` and
   `computer_use_embodied` slugs split for tooling reasons, but the
   scope-grant / kill-switch / knowledge-domain model is one
   decision. Slugs let the product surface "did you allow this
   agent to drive your computer?" without enumerating the seven
   underlying tools.

---

## 1. The capability slugs (current)

| Slug | Mode | Expands to | Authorization |
|------|------|------------|---------------|
| `copresent_control` | Browser SPA control (the CoPresent product) | `uiRequestControl`, `uiReleaseControl`, `uiReadState`, `uiDescribe`, `uiClick`, `uiType`, `uiSelect`, `uiHighlight`, `uiNavigate`, `uiPointerTo`, `uiAskUser`, `uiWaitFor`, `uiRetry`, `uiNarrate`, `agentUpdateSelf`, `similarTo` | CoPresent's own control fence rules. |
| `computer_use_headless` | Shell / filesystem / HTTP on the user's machine | `workerHost`, `workerStatus`, `requestComputerUseScope`, `canvasPublish` | Three layers: agent capability flag, standing scope on `v1:agents:agentAuthorization.computerUseScope` (observe / interact / full), per-Plan kill switch on `v1:identity:user.preferences.computerUseEnabled`. |
| `computer_use_embodied` | Mouse / keyboard / screenshot on the user's machine | `workerComputer`, `workerStatus`, `requestComputerUseScope`, `canvasPublish` | Same three layers as headless. Both modes share the auth model. |
| `workbench_use` | Sandboxed Linux per-Plan in the cluster | `workbenchHost`, `canvasPublish` | Universal -- default-on for every agent. No scope grants, no kill switch, no per-agent gating. Blast radius is contained to the per-Plan directory tree. |

**Defined in** `component/memql/operator_caps.go`. Each slug-to-tools
mapping is a single entry in the `capabilitySlugs` map.

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
    "computer_use_headless",   # slug
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
seed order; the slug `computer_use_headless` is replaced by its
expansion.

---

## 3. Authorization model (computer-use family)

The two computer-use modes share authorization because both act on
the user's machine. Three layers are checked BEFORE dispatch (see
[docs/public/operate/workers-runbook.md](../operate/workers-runbook.md) for the
operator-side narrative):

1. **Agent capability flag.** Does the agent declare
   `computer_use_headless` or `computer_use_embodied` in its
   `capabilities.tools[]`?
2. **Standing scope.** `v1:agents:agentAuthorization.computerUseScope`
   = `observe` / `interact` / `full`. Determines what the agent may
   call.
3. **Per-Plan kill switch.** `v1:identity:user.preferences.computerUseEnabled`.
   The floating widget in the CoPresent session chrome flips this
   flag; an out-of-scope or disabled call transitions the calling
   Plan to `awaitingFeedback` with `feedbackReason=scope_elevation_required`.

`workbench_use` has no scope grants, no kill switch, no per-agent
gating -- the per-Plan directory is the blast radius and it's torn
down with the Plan.

---

## 4. Adding a new capability slug

Three files touch:

1. `component/memql/operator_caps.go` -- define the slug, its
   expansion list (`<Name>CapabilityNames`), and add it to the
   `capabilitySlugs` map.
2. `dsl/agents/concepts.memql` -- if the new slug needs to be
   advertised in the agent-role catalog (`v1:agents:agentRole.lockedToolSlugs`
   or `defaultToolSlugs`), update the role definitions.
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
| `claw` | Coding-agent flag (`v1:agents:agent.claw`). Toggles OpenClaw / NemoClaw tools for the agent. Not part of `capabilities.tools[]`. | `dsl/agents/concepts.memql`, agent edit modal in CoPresent. |
| `assistant` / `agent` / `delegate` | Role slugs on `v1:agents:agent.role` and `roleSlug`. | `dsl/agents/agent.memql` (per-role seeds). |
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
| Slug-to-tools expansion table | `component/memql/operator_caps.go` |
| Tool definitions | `dsl/copresent/tools.memql`, `dsl/agents/tools.memql`, `dsl/workbench/tools.memql` |
| Authorization model | `v1:agents:agentAuthorization`, `v1:identity:user.preferences` |
| Operator runbook (computer use) | `docs/public/operate/workers-runbook.md` |
| Workbench runbook | `docs/public/operate/workbench-runbook.md` |
