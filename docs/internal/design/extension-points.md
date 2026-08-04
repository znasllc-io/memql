---
title: Extension-point audit -- cognition / voice / planner (memql#1922)
audience: internal
status: historical
area: internal
sinceVersion: 0.9.0
owner: znas
---

# Extension-point audit: cognition / voice / planner (memql#1922)

Status: completed 2026-06-22 for Epic 2 (platform / plugin), issue 2.3
[G:2.1]. Spec: `docs/internal/program/02-epic-platform-plugin.md`.

## Problem

Epic 2 turns memQL into a host that third-party **packs** drop into.
2.1 shipped the versioned Plugin SDK (`component/memql/plugins.go`:
`PluginContext` / `PluginFactory` / `RegisterPlugin` + a contract
version; reference in `docs/public/build/plugin-sdk.md`). 2.3 must
**confirm that a pack can actually inject behavior into the three core
services** -- cognition, voice, planner -- and decide whether the
event/automation + routing surface is sufficient or whether a few
explicit in-process hook interfaces are needed.

This document is the audit: a per-service list of the concrete
extension points a pack uses today, with file:symbol evidence, and the
sufficiency decision.

## How a pack extends memQL (the four mechanisms)

A pack injects behavior through exactly four mechanisms, in preference
order. None of the three core services adds a fifth.

| # | Mechanism | Primitive | Build-tag | Touches `app/`? |
|---|-----------|-----------|-----------|-----------------|
| 1 | DSL tree | `dsl.RegisterTree` (`dsl/embed.go`) -- concepts / queries / mutations / specs / **automations** / prompts / providers / shapes / tools / builtins | gated by the `.go` file holding the embed | no |
| 2 | Self-registering plug-in | `memql.RegisterPlugin(name, factory)` from `init()`; factory gets `PluginContext` | gated by the calling file | no |
| 3 | Cross-node event routing | `node.RegisterRoutingRule(RoutingRule{...})` from `init()` (`component/node/routing.go`) | gated by the calling file | no |
| 4 | In-process synchronous hook (registry) | `component/planner` `RegisterContainerExecutor(name, impl)` from `init()` | build-tag-neutral | no |

Two facts up front, because they shape the conclusion:

- **There is no `RegisterConceptOwnership`.** Nothing in the repo
  declares it, as a function or as a method:
  `grep -rnE '^func (\([^)]*\) )?RegisterConceptOwnership' --include='*.go'`
  returns nothing. Which node owns a concept's work is decided entirely
  by **routing rules** (mechanism 3) plus which binary's build tags
  compile the subscriber.

  The command is narrowed to *declarations* on purpose. A bare
  `grep -rni 'RegisterConceptOwnership' --include='*.go'` returned
  nothing when this was written, but `docs_go_extension_points_test.go`
  now names the symbol in its retired allowlist, so the bare form
  returns hits -- and a reader running it would conclude the primitive
  exists. That is the exact inversion of the confusion the gate was
  built to prevent (memql#2967), so the doc has to hand over a command
  that stays true.

  It is not a primitive that was never built: it existed, and was
  deleted with `component/node/query_proxy.go` in `ac3a751e`
  ("simplify: drop per-node @visibility filtering + concept-ownership
  routing", 2026-05-16). The
  stale prose this audit flagged in `CLAUDE.md`, `integrations/arch.md`
  and `docs/public/operate/downstream-stacks.md` outlived that removal
  by 74 days; corrected in memql#2922 (PR #2966).
- **`AgentForwarder` is internal transport, not a pack hook.** Both
  cognition (`integrations/cognition/agent_forward.go:21`
  `AgentForwarder`) and planner (`integrations/planner/integration.go:72`
  `AgentForwarder`) take an `AgentForwarder` via `SetAgentForwarder`,
  but it is wired from `app/cluster.go` to the first-party
  `AiForwardRouter`. It is the cross-node turn-dispatch substrate, not
  a surface a pack implements. It is listed below for completeness but
  is **not** a pack extension point.

---

## Cognition

Cognition decides whether and which agent responds to an utterance and
dispatches the turn. It is wired through the **explicit `app/` path**
(its deps -- Polyphon score engine, embedding func, `AgentForwarder`,
AI router -- exceed `PluginContext`), so cognition itself is core, not
a pack. A pack extends cognition's *behavior* without editing it:

| Extension point | Mechanism | Evidence | What a pack changes |
|-----------------|-----------|----------|---------------------|
| Utterance -> response dispatch | DSL automation + event | `dsl/cognition/automations.memql` `generateResponse` `@trigger(event="cognition.response.requested")`; in-process subscription `integrations/cognition/cognition.go` `Start()` subscribes `graph.node.created.v1:cognition:utterance` -> `handleUtteranceForCognition` | nothing in Go; a pack writes utterances and lets the core path run |
| Auto-join / bootstrap on new space/participant | DSL automation | `dsl/cognition/automations.memql` `autoJoinAI` (`node.created` / `v1:cognition:space`), `bootstrapSession` (`v1:cognition:participant`) | a pack adds its own `node.created` automations in its namespace |
| **Who** responds (routing / turn-taking) | DSL prompt | `dsl/cognition/prompts/conductorTurn.tmpl` (single LLM brain: primary / sequence / chimeIns / fitScore), `cognitionRouting.tmpl` (voice fast-path) | a pack ships agent records + a prompt override; the conductor reads each agent's `role` / `domains` / `tools` |
| **How** an agent replies | DSL prompt | `dsl/cognition/prompts/cognitionReply.tmpl` (OUTPUT CONTRACT -> `respondToUser` envelope), schema in `dsl/cognition/prompts.memql` | reply style, citations, skip-flags -- all template-driven |
| Plan triage from chat | DSL prompt | `dsl/cognition/prompts/cognitionPlanTriage.tmpl` | a pack tunes when an utterance escalates to a Plan |
| Cross-node receipt of cognition events | Routing rule | `component/node/routing.go` core defaults: `graph.node.created.v1:cognition:*`, `graph.node.updated.v1:cognition:*` (broadcast), `cognition.response.audio` -> Voice, `voice.gate.directive` -> BFF | a pack with a new cross-node concept adds `node.RegisterRoutingRule` from its own `init()` |
| Per-agent capability surface | DSL | agent records + skills (`ResolveSkills` in `integrations/cognition/cognition.go`) | tools / domains a pack's agents carry |
| (internal, not a pack hook) Cross-node turn dispatch | Go iface wired by `app/` | `integrations/cognition/agent_forward.go:21` `AgentForwarder`, installed via `SetAgentForwarder` | n/a -- first-party transport |

**Verdict (cognition): event/automation + routing + DSL prompts are
sufficient.** Every behavior the product pack injects (who answers,
how, when a plan spawns, what an agent can do) is expressible as a
graph write + an automation + a prompt/agent record. No synchronous
in-process pack hook is missing.

---

## Voice

Voice owns realtime transport (LiveKit, STT/TTS, the avatar) and the
`VoiceAgent*` gRPC contract. The voice integration is the thinnest of
the three -- a **self-registering plug-in**:

```go
// integrations/voice/plugin.go
memql.RegisterPlugin("voice", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
    return New(pctx.Logger), nil
})
```

| Extension point | Mechanism | Evidence | What a pack changes |
|-----------------|-----------|----------|---------------------|
| Voice catalog -> provider voice id | Plug-in capability + DSL builtin | `integrations/voice/capabilities.go` (`pickForGender`, `resolve`); `integrations/voice/voices.go` (`PickVoiceForGender`, `ResolveVoice`); DSL builtins `voicePickForGender` / `voiceResolve` | call the builtins from DSL; the catalog is data |
| Per-agent voice / persona / avatar | DSL concept fields | `v1:agents:agent.voiceId` / `avatarPersonaId` / `avatarVendor` / `audioControl` / `videoControl`; session overrides `v1:cognition:audioOverride` / `videoOverride` | stamp fields at agent creation; voice-agent reads them |
| Voice routing/turn decision (the gate) | Routing rule + cognition prompt | `component/node/routing.go` `voice.gate.directive` -> BFF, `cognition.response.audio` -> Voice; the decision itself is `conductorTurn.tmpl` / `cognitionRouting.tmpl` on cognition | a pack tunes voice behavior through the **cognition** prompts, not a voice-specific Go hook |
| Realtime / cascade executor selection | Config (env) | `MEMQL_VOICE_EXECUTOR` (`integrations/voice/agent/`) | per-deploy config, not pack code |

The key finding for voice: **voice has essentially no product-specific
decision logic of its own.** Routing, persona, and reply content all
flow from DSL (agent records + cognition prompts) and config. The voice
node is transport; the brain is cognition.

**Verdict (voice): event/automation + routing + DSL/config are
sufficient.** A pack never needs to compile Go against the voice node
to specialize voice behavior.

---

## Planner

Planner orchestrates Plans -> Tasks. It is wired through the explicit
`app/` path (`app/integrations_planner_init.go`
`setupPlannerIntegration`, `//go:build planner`) because it needs the
direct DB getter, the cluster claimer, and the event bus -- beyond
`PluginContext`.

| Extension point | Mechanism | Evidence | What a pack changes |
|-----------------|-----------|----------|---------------------|
| Plan intake (decompose loop) | Event subscription driven by graph writes | `integrations/planner/integration.go` `Start()` subscribes `graph.node.created.v1:planner:plan` -> `agentLoop.HandlePlanCreated` (`integrations/planner/agent_loop.go`) | a pack creates Plan rows; the loop runs |
| Cross-node receipt of plan events | Routing rule | `component/node/routing.go` `graph.node.created.v1:planner:*` / `updated` / `deleted` (broadcast) | core default already forwards; a pack adds rules only for its own new cross-node concepts |
| Planner decision surface | DSL prompt | `dsl/agents/prompts/plannerAgent.tmpl` + schema; structured enum decompose / dispatchTask / createSpecialist / markPlanSucceeded / escalate | a pack tunes decomposition by overriding the prompt |
| Specialized Plan kinds | DSL automation + event handlers | `trainSpecialist` (#644), `embedDomainItems` (#645), `refineAnalysis` (`handleRefinementPlan`) -- pack-created Plan kinds claimed by their own subscribers | a pack adds a new Plan `kind` + a subscriber gated by its build tag |
| **Task execution backend** | **In-process synchronous hook (registry)** | `component/planner/executor.go`: `ContainerExecutor` iface (`Backend()`, `Run(ctx, ExecutorRequest, ProgressCallback) (ExecutorResult, error)`), `RegisterContainerExecutor(name, impl)` (init-time), `LookupContainerExecutor(name)` (dispatch-time) | **a pack registers a backend the planner calls synchronously** when `Task.executorBackend` matches |
| Token-budget enforcement | Go guard (core) | `component/planner/budget.go` `CheckCall`; wired `integrations/planner/agent_loop_budget.go` | n/a -- core safety, not a pack knob |
| (internal, not a pack hook) Cross-node turn dispatch | Go iface wired by `app/` | `integrations/planner/integration.go:72` `AgentForwarder` | n/a -- first-party transport |

### The one genuine in-process hook already exists

The `ContainerExecutor` registry in `component/planner/executor.go` is
**exactly the "plugin implements; the service calls it synchronously in
its pipeline" shape** the issue asks about:

- A backend implements `ContainerExecutor` and calls
  `planner.RegisterContainerExecutor("nemoclaw", impl)` from its
  package `init()`.
- The planner, at Task-dispatch time, calls
  `planner.LookupContainerExecutor(Task.executorBackend)` and invokes
  `Run(...)` **synchronously in-process**, streaming progress through
  the `ProgressCallback`.
- It is **build-tag-neutral** (the registry compiles into every
  binary) and touches no `app/` internals -- a pack can register a
  backend from any package that the planner binary's build tags pull
  in.

This is the precedent the Plugin SDK should point to whenever a service
genuinely needs synchronous pack-supplied behavior (a deterministic
result-verifier, an alternate executor backend, etc.): a small
init-time `Register<Hook>` + dispatch-time `Lookup<Hook>` registry over
a narrow interface. It is already the documented model for executor
backends; no new instance is warranted today.

**Verdict (planner): event/automation + routing + DSL prompts cover
plan intake and decisioning; the ONE place planner needs a synchronous
pack-supplied behavior -- the Task execution backend -- already has a
build-tag-neutral hook (`ContainerExecutor` / `RegisterContainerExecutor`).**

---

## Decision: no new Go surface

**Events + automations + routing rules + DSL prompts are sufficient for
all three services, and the single synchronous-hook need that does
exist (planner's executor backend) is already covered by an existing,
build-tag-neutral registry.** Therefore 2.3 adds **no new Go extension
surface.**

Evidence for "sufficient":

1. **Cognition** -- every product decision (who responds, how, when a
   plan spawns, agent capabilities) is a graph write + automation +
   prompt/agent record. The only Go interface cognition calls
   synchronously per-turn, `AgentForwarder`, is first-party cross-node
   transport wired by `app/`, not a pack hook.
2. **Voice** -- carries no product decision logic of its own; routing
   and reply come from cognition DSL, persona/voice from agent fields,
   executor from config. A pack never compiles Go against voice.
3. **Planner** -- plan intake and decomposition are event- and
   prompt-driven; the one synchronous extension (executor backend) is
   already `ContainerExecutor` + `RegisterContainerExecutor`.

Adding speculative hook interfaces now (a "cognition response hook," a
"voice turn hook") would be surface with **zero current consumer** --
the same anti-pattern that retired the decision-policy tier (#984). The
ABI line stays narrow: `PluginContext` + the four registration
primitives, with `ContainerExecutor` as the worked example of the
synchronous-hook pattern for the rare case a future service needs one.

### When a future service *would* warrant a new hook

For the record, so a later contributor doesn't re-litigate this: add a
new in-process hook only when **all** of these hold, and model it on
`ContainerExecutor`:

- the service must call pack code **synchronously inside a single
  request/turn** (an event round-trip would change the result, not just
  add latency), AND
- the pack must **return a value the service consumes** (not just
  observe / fire-and-forget -- those are events), AND
- there is a **concrete consumer** in hand (not "a pack might want to").

If those hold: a narrow interface + an init-time `Register<Hook>` + a
dispatch-time `Lookup<Hook>` registry, build-tag-neutral, in the
owning `component/<service>` package -- the shape
`component/planner/executor.go` already uses.

## Acceptance checklist

- [x] Documented list of extension points per core service (cognition /
  voice / planner) -- the three tables above, with file:symbol
  evidence.
- [x] Sufficiency decision recorded with evidence: events + automation +
  routing + DSL are sufficient; no new hook interface needed.
- [x] The one existing synchronous in-process hook
  (`ContainerExecutor` / `RegisterContainerExecutor`) documented as the
  worked example, with its usage (NemoClaw registers "nemoclaw"; the
  planner looks it up by `Task.executorBackend`).
- [x] No new Go surface added (so no new example/test required by the
  acceptance clause "any new hook interfaces implemented + covered by
  an example").

## References

- `component/memql/plugins.go` -- Plugin SDK (2.1).
- `docs/public/build/plugin-sdk.md` -- public contract reference.
- `dsl/embed.go` -- `RegisterTree`.
- `component/node/routing.go` -- `RegisterRoutingRule` + core defaults.
- `component/planner/executor.go` -- `ContainerExecutor` /
  `RegisterContainerExecutor` / `LookupContainerExecutor`.
- `integrations/voice/plugin.go`, `integrations/voice/capabilities.go`,
  `integrations/voice/voices.go` -- voice plug-in.
- `integrations/planner/integration.go`,
  `integrations/planner/agent_loop.go`,
  `app/integrations_planner_init.go` -- planner wiring.
- `integrations/cognition/cognition.go`,
  `integrations/cognition/agent_forward.go`,
  `dsl/cognition/automations.memql`, `dsl/cognition/prompts.memql` --
  cognition wiring + DSL surface.
