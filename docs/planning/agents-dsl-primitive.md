# Plan: agents as a first-class DSL primitive

> **Status:** Phases 0-5 shipped on `feature/agents-dsl-primitive`.
> Phase 6 deferred (genuinely premature; reasoning at the bottom).
> Follow-ups identified but not in scope for this branch:
> row materialization, full tool loop in dispatch, retiring the
> (absent-here) `provisionGeneralAssistantOnUserCreate` automation.
> **This document deletes itself when the deferred follow-ups land
> + memql is in steady state**, per the no-stale-docs convention.

## Context

Today memQL has eleven DSL primitives — `concept`, `query`, `mutation`,
`automation`, `policy`, `prompt`, `provider`, `tool`, `builtin`, `shape`,
`spec` — declared in `.memql` files and loaded by the engine at startup.
**Agents are not among them.** The `v1:agents:agent` concept (canonical
since PR #3's namespace cleanup) holds rich agent state, but every agent
today is created either via the CoPresent UI or by hand-written
provisioning automations (`provisionGeneralAssistantOnUserCreate`).

This adds a twelfth primitive: `agent`. Each `.memql` agent file declares
a callable AI entity that materializes as `v1:agents:agent` row(s) at
startup AND becomes invocable from DSL via a new `agent(name, args)`
builtin.

Net effect: system agents (the General Assistant baseline, future
"PR reviewer" / "doc generator" agents) become declarative files instead
of hand-rolled provisioning automations + ad-hoc Go code paths.
Automations and policies compose agents the way they already compose
prompts and tools.

## Locked decisions

| # | Decision | Value |
|---|---|---|
| 1 | What the `agent` primitive is | Both: a declarative template that produces `v1:agents:agent` rows AND a callable invocation handle |
| 2 | Field scope declarable in DSL | Baseline + capability bundle: `name`, `description`, `personality`, `role`, `roleSlug`, `gender`, `providerConfig.llm.*`, `capabilities.*`, `triggerBehavior`, `audioControl`, `videoControl`. Skips personalization (avatar persona, voice id, colorIndex, groupIds) — those stay user-mutated |
| 3 | Invocation surface | Builtin function: `agent("name", args) → { response: string, citations: []object }` |
| 4 | Materialization model | Per-agent `@scope("global"\|"perUser")` annotation |
| 5 | `systemPrompt` source | `@templateFile("templates/<name>.tmpl")` annotation; loader reads at startup, stamps into the row's `systemPrompt` |
| 6 | Reference-typed collections | `tools: [tool("respondToUser"), ...]` and `knowledge: [knowledgeDomain("baseline")]` — cross-reference enforced at load time |
| 7 | Top-level `knowledge` field | Distinct from `capabilities.domains[]` (routing categories). `knowledge` binds to `v1:common:knowledgeDomain` rows for RAG retrieval |
| 8 | Template substitution | Static — `.tmpl` content is loaded verbatim. No per-invocation `{{var}}` substitution |
| 9 | `provisionGeneralAssistantOnUserCreate` fate | Retired in Phase 5; loader takes over per-user materialization |
| 10 | `capabilities.tools[]` field type | Tighten from `[]string` to typed reference collection in Phase 6 follow-up |

## Canonical declaration

```memql
@version("1.0.0")
@namespace("agents")
@scope("perUser")
@visibility("bff", "cognition", "agent")
@templateFile("templates/generalAssistant.tmpl")
@description("Per-user General Assistant.")
agent generalAssistant {
  role:        "general_assistant"
  roleSlug:    "general_assistant"
  name:        "General Assistant"
  description: "Designated fallback when no specialist fits."
  personality: "Friendly, capable, proactive."
  gender:      "female"

  providerConfig {
    llm {
      policyName:  "balancedChat"
      temperature: 0.7
      maxTokens:   4000
    }
  }

  capabilities {
    avatar: true; lipSync: true; vision: true; voiceToVoice: true; claw: false
    tools:    [tool("respondToUser"), tool("uiClick"), tool("uiNarrate"), tool("uiDescribe")]
    domains:  []
    keywords: []
  }

  knowledge: [knowledgeDomain("generalAssistantBaseline")]

  triggerBehavior {
    autoJoin:          true
    greetOnJoin:       true
    interruptionStyle: "polite"
    speakWhen:         "always"
  }

  audioControl: "mirror_user"
  videoControl: "mirror_user"
}
```

File layout:

```
dsl/agents/v1/<namespace>/<name>.memql
dsl/agents/v1/<namespace>/templates/<name>.tmpl
```

The full syntax reference (every annotation, every field, every reference
type, scope semantics, invocation contract) lives at
[`dsl/_reference/_agent.memql`](../../dsl/_reference/_agent.memql).

## Implementation phases

### Phase 0 — Groundwork (shipped)

- `dsl/_reference/_agent.memql` — syntax reference (non-loading)
- `docs/planning/agents-dsl-primitive.md` — this file

No engine / parser changes. Lays the contract for phases 1-5.

### Phase 1 — Parser + AST for `agent` keyword

- `component/language/parser/`: tokenize `agent`, add `parseAgentDecl()`.
- `component/language/ast/`: new `AgentDecl` node carrying fields,
  annotations, and a back-pointer to the source file for
  `@templateFile` resolution.
- Allowed annotations enumerated per the locked-decisions table.
  Body fields enumerated per the baseline-subset rule.
  Anything else: reject with a friendly error pointing at the reference
  doc.
- Parser unit tests: round-trip the strawman; reject malformed cases.

### Phase 2 — Compiler + cross-reference validation

- `component/language/compiler/`: `compileAgentDecl()` lowers the AST
  into an engine-internal `AgentDef`.
- Cross-ref validation at compile time: every `tool("name")` resolves to
  a registered `tool` construct; every `knowledgeDomain("name")` resolves
  to a known knowledge-domain id.
- `@templateFile` content loaded at compile time, stored on the `AgentDef`.

### Phase 3 — Loader + materialization

- `component/memql/agent_loader.go` (new): walks
  `dsl/agents/v1/**/*.memql`, compiles each, registers in an in-memory
  registry keyed by name.
- For each registered agent, materializes concept row(s) per `@scope`:
  - `global` → one row in `_system`, `ownerUserId=""`, system-marked.
  - `perUser` → one row per existing user + subscribe to
    `v1:identity:user` create events for ongoing materialization.
- Idempotent: deterministic row ids
  (`sysagent-{global|user}-{slug}-{userId?}`).
- Re-run behavior: stamp DSL-declared fields into existing rows
  (preserve user-personalization fields).
- Wire the loader into `component/memql/engine.go` startup.

### Phase 4 — `agent(...)` builtin

- `dsl/agents/v1/builtins.memql` (new):

  ```memql
  @enabled
  @description("Invoke a DSL-registered agent. Returns the reply envelope.")
  @executor("integration.agents.invoke")
  builtin agent {
    name         string    @required
    utterance    string    @required
    spaceContext object
    history      []object
  }
  ```

- `integrations/agents/invoke.go` (new): thin wrapper that resolves the
  agent name → registry entry → row → existing `ForwardTurn` dispatch +
  tool loop. Returns the `respondToUser` envelope as the builtin's value.

### Phase 5 — Declare GA in DSL + retire automation

- `dsl/agents/v1/generalAssistant.memql` + `templates/generalAssistant.tmpl`
- Delete the `provisionGeneralAssistantOnUserCreate` automation and any
  Go code paths that special-cased it.
- End-to-end test against a fresh cluster.

### Phase 6 — Tighten `capabilities.tools[]` (deferred — see reasoning)

The brainstorm called for tightening `v1:agents:agent.capabilities.tools`
from `[]string` to a typed reference collection. After implementation
ramped up, this turned out to be premature for two compounding reasons:

1. **Tools are not concept-row-backed.** `tool` DSL constructs live in
   an in-memory `ToolRegistry`, not as `v1:tools:tool` rows. The
   `@relationship(type="references", field="tools", target="...")`
   decorator that would express the typed reference needs a concrete
   target concept; today there is no such concept.
2. **The safety the brainstorm wanted is already in place at load
   time.** `LoadUnifiedAgents` (Phase 3) calls `validateAgentToolRefs`
   against the live `ToolRegistry`, rejecting typo'd `tool("...")`
   refs in agent declarations before the agent is registered. The
   schema-level enforcement Phase 6 would add is a duplicate guarantee
   on top of the loader-level one.

Phase 6 lands when (and if) tools become concept-row-backed -- a
separate initiative that affects the entire DSL surface, not just
agents. Re-evaluate at that point.

## Critical files

| Phase | Path | Action |
|---|---|---|
| 0 | `dsl/_reference/_agent.memql` | New |
| 0 | `docs/planning/agents-dsl-primitive.md` | New (this file) |
| 1 | `component/language/parser/agent.go` | New |
| 1 | `component/language/parser/lexer.go` | Recognize `agent` keyword |
| 1 | `component/language/ast/agent.go` | New `AgentDecl` AST node |
| 1 | `component/language/parser/agent_test.go` | New |
| 2 | `component/language/compiler/agent.go` | New |
| 2 | `component/language/compiler/agent_test.go` | New |
| 3 | `component/memql/agent_loader.go` | New |
| 3 | `component/memql/agent_loader_test.go` | New |
| 3 | `component/memql/engine.go` | Wire loader into startup |
| 4 | `dsl/agents/v1/builtins.memql` | New |
| 4 | `integrations/agents/invoke.go` | New |
| 4 | `integrations/agents/invoke_test.go` | New |
| 5 | `dsl/agents/v1/generalAssistant.memql` | New |
| 5 | `dsl/agents/v1/templates/generalAssistant.tmpl` | New |
| 5 | `dsl/<wherever>/automations/provisionGeneralAssistantOnUserCreate.memql` | Delete |
| 5 | Any Go code referencing the retired automation | Update / delete |
| 6 | `dsl/agents/concepts.memql` | Tighten `capabilities.tools` field type |

## Open issues for execution to resolve

- **System marker on `v1:agents:agent`**: how DSL-materialized rows are
  distinguished from user-created rows. Options: a new `system bool
  @default(false)` field, or a sentinel `createdBy="system:dsl-loader"`.
  Affects Phase 3 loader and the immutability story for DSL-declared
  fields on user-mutated rows.
- **`@scope("global")` invocation context**: actor + partition threading
  when `agent("prReviewer", args)` is called from an automation that has
  no obvious actor (e.g. a cron-triggered automation).
- **Hot-reload during dev**: agent file edits picking up without a
  cluster restart. Mirror whatever `prompt` / `tool` constructs do.
- **`roleSlug` uniqueness conflict during Phase 5 migration**: if a user
  has manually created an agent with `roleSlug: "general_assistant"`
  before the loader runs, materialization would collide. Phase 3 loader
  needs a "skip if user already has an agent with this roleSlug" guard
  with a warning log.

## Out of scope (this branch)

- Hot-reload of agent files in a running cluster.
- Per-invocation prompt-template substitution (`{{userName}}`). Static
  templates only for v1.
- Agent versioning / rollback. DSL file is the source of truth.
- Cockpit UI for browsing DSL-declared vs user-created agents.

## Deferred follow-ups (separate commits / sessions)

These were in the original plan's scope but turned out to need their
own focused work after Phases 0-5 landed:

- **Row materialization.** `LoadUnifiedAgents` populates the
  `AgentRegistry` (in-memory) but does NOT yet stamp `v1:agents:agent`
  rows for `@scope("perUser")` (per-user) or `@scope("global")`
  (singleton in `_system`) agents. The `agent(...)` builtin works
  against the registry directly, so the feature is functional for
  DSL invocation. Cognition's existing dispatch path (which selects
  agents from concept rows) is unaffected. Materialization is needed
  before the existing `provisionGeneralAssistantOnUserCreate`
  automation can be retired.
- **Full streaming tool loop in `agents.invoke`.** Today the handler
  does a one-shot `ChatSIProvider.CallChat` (system prompt + user
  utterance → response text). Tomorrow it runs the same streaming
  tool loop the cognition `ForwardTurn` path uses, intercepting
  `respondToUser` for the envelope. Mirror
  `integrations/agent/streaming.go`.
- **Retire `provisionGeneralAssistantOnUserCreate`.** The automation
  is referenced by stale comments in this branch but the actual
  declaration lives on the CoPresent BFF branch. Retire it from THAT
  branch once row materialization is in place here.
- **CoPresent UI tools as DSL constructs.** The strawman GA tool
  list (uiClick, uiNarrate, uiDescribe, respondToUser) requires
  CoPresent-side `tool` DSL constructs that don't exist in memql core
  today. When CoPresent grows DSL-declared UI tools, wire them onto
  the GA's `capabilities.tools` (cross-ref will validate cleanly).

## Verification (per phase)

1. **Phase 0** (shipped): reference + planning docs exist and read
   coherently. No engine changes. `make test` clean.
2. **Phase 1** (shipped): 8 parser unit tests pass — round-trip
   canonical decl, global scope, reject unknown annotation, reject
   invalid `@scope`, reject unknown body field, reject wrong ref kind,
   reject missing decl, reject legacy `func` form.
3. **Phase 2** (shipped): 7 compile + registry tests pass — canonical
   compile round-trip, scope/role defaults, global scope, registry
   Upsert/Get/Names + nil-safety, validateAgentToolRefs.
4. **Phase 3** (shipped — registry only): `LoadUnifiedAgents` smoke
   test in `TestUnifiedLoadersCoverNewTree` runs cleanly against the
   live tree. Bootstrap wires the registry into `MemQLEngine.agents`.
   Row materialization deferred (see follow-ups).
5. **Phase 4** (shipped): 7 integration handler tests pass — nil
   inputs, capability shape, missing arg validation, unregistered
   agent error, resolved-agent envelope round-trip. Builtin count
   incremented from 45 to 46.
6. **Phase 5** (shipped): GA agent count 0 → 1 in
   `TestUnifiedLoadersCoverNewTree`. `dispatch()` now calls
   `ChatSIProvider.CallChat` against the resolved provider when one
   is available, falls back to the deterministic stub when the
   provider registry is nil (tests stay fast + offline).
7. **Phase 6**: deferred. See "Phase 6 — Tighten capabilities.tools[]"
   above for reasoning.
