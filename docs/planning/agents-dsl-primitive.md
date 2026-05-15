# Plan: agents as a first-class DSL primitive

> **Status:** Brainstorm + design locked. Phase 0 (groundwork) shipped.
> Phases 1-5 implementation pending. **This document deletes itself
> when Phase 5 lands**, per the no-stale-docs convention.

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

### Phase 6 — Tighten `capabilities.tools[]` (optional follow-up)

- Change the concept's `capabilities.tools` from `[]string` to a typed
  reference collection.
- Row-level migration for existing data.

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

## Out of scope

- Hot-reload of agent files in a running cluster.
- Per-invocation prompt-template substitution (`{{userName}}`). Static
  templates only for v1.
- Agent versioning / rollback. DSL file is the source of truth.
- Cockpit UI for browsing DSL-declared vs user-created agents.

## Verification (per phase)

1. **Phase 0**: reference + planning docs exist and read coherently. No
   engine changes. `make test` clean.
2. **Phase 1**: parser unit tests pass. Drop a sample agent file at
   `dsl/agents/v1/_sample.memql` (leading underscore so loader skips it);
   verify `go run ./cmd/admin-preview parse` (or equivalent) returns a
   clean AST.
3. **Phase 2**: cross-ref errors fire on missing tool / unknown knowledge.
4. **Phase 3**: `make dev-refresh`, query the materialized rows. Stop +
   re-run, verify idempotency. Touch a baseline field, verify update.
5. **Phase 4**: `agent("generalAssistant", { utterance: "hello" })` from
   a unit-test automation returns a non-empty response.
6. **Phase 5**: fresh cluster + new user → GA materializes from DSL; old
   provisioning automation gone; existing tests pass.
