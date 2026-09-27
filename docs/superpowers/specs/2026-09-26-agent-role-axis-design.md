# Agent-Role Axis for Tools -- Design

- **Date:** 2026-09-26
- **Status:** approved by the owner ("split into two gates"); implemented for
  memql#5438 in the dsl-v1-followups epic.
- **Scope:** how a `tool` says WHO may be offered it and WHO may call it. Two
  new gates, the caller kinds they read, the load-time checks, the
  deprecation of `@allowedRoles`, its rewrite, and the forge surface that was
  its largest user.
- **Deliberately NOT here:** changing which surfaces may call a tool at all.
  Tools stay an agent surface plus the MCP connector; queries, mutations and
  capabilities remain the way everything else reaches the engine.

## The problem

`@allowedRoles(...)` compared ONE role string, and what that string was
depended on the path a call arrived by:

| Path | What was stamped as the "acting role" | Where |
|---|---|---|
| An agent's tool loop | the acting agent's `v1:agents:agent.role` (`assistant` / `specialist`) | `component/grpc/agent_turn_handlers.go`, `integrations/agent/replier.go` (`stampActingAgentRoleIfMissing`) |
| `CallToolMsg` on the agent node | the `agent_role` envelope metadata a forwarder threads | `component/grpc/server.go` |
| The MCP connector | the authenticated PERSON's cluster role (`owner`, `writer`, ...) | `component/mcp/tool_surface.go` |

So one list meant two different things. `dsl/library`, `dsl/compose` and
`dsl/agents` gated on agent kinds; `dsl/forge` gated on person roles, and
worked only because MCP stamped a person's role as if it were an agent's. And
`dsl/work/capabilities.memql` gated its four tools on `("assistant",
"system-planner")`: `system-planner` is the planner agent's `roleSlug`, no path
ever stamps a roleSlug, and the planner's `role` is `specialist` -- so when a
work run fell back to the seeded planner (`reasoningAgent`,
`integrations/planner/work_compile_draft.go`), the four tools
`scopeWorkExecution` offered it were refused on every call.

The replacement epic memql#5375 proposed (`@requiresRank` +
`@requiresCapability`) cannot say what the agent half says: those judge the
PERSON, and substituting rank for agent kind would have let every specialist
call the assistant-only tools (`ensureAgent`, `produceArtifact`) that the
delegation baseline depends on.

## The decision: two gates, one per axis

| Gate | Question | Values | Judged against |
|---|---|---|---|
| `@requiresAgentRole("assistant", ...)` | WHICH AGENT is calling? | the `v1:agents:agent.role` enum: `assistant`, `specialist` | the acting agent's role |
| `@requiresRank("<role>")` on a tool | how senior is the PERSON the call is for? | the role ladder (`dsl/rbac` seeds plus the cluster's catalog) | the authenticated user over MCP; the user an agent acts for |

A tool may carry either, both (both must pass) or neither. With neither it is
callable by every agent and by an authenticated person over MCP.

### Vocabulary

- **Agent roles are the concept's own enum.** The load reads them from the
  `v1:agents:agent` declaration in the concept registry it built
  (`agentRoleVocabulary`, `component/memql/tool_gate_load.go`), so the gate
  and the column an agent's role is written in cannot disagree. The offline
  rewrite and the language server read the same declaration from the embedded
  tree (`parser.RoleVocabularyFromTree`). There is no Go list of agent roles.
- **Person roles are the ladder**, exactly as for a query's floor: validated
  at load by the same check (`validateRequiresRankSlugs` for functions, the
  tool pass beside it), resolved at call time through `rankLadder`.

### Caller kinds

A tool call is never inferred from a role string's spelling any more. The
context says who is calling (`ToolCaller`, `component/memql/tool_context.go`):

- **An agent** -- `WithActingAgentRole`, stamped as before by the agent turn
  handler, the replier's self-resolution and the `CallToolMsg` metadata.
  Nothing changes for agent loops.
- **A person over MCP** -- `WithMCPHumanCaller`, stamped by `callMCPTool` and
  `listMCPTools` in place of the old `WithActingAgentRole(ctx, role)`.

`ExecuteTool`'s rule "tools are agent-only" becomes **"an agent, or an
authenticated person over MCP"**: the person needs an identity on the context
(the HTTP head verifies a bearer; a stdio session names one with
`MEMQL_MCP_USER`). A person is not an agent kind, so `@requiresAgentRole`
refuses them and hides the tool from their `tools/list`.

### Enforcement points

One decision (`component/memql/tool_gate.go`), asked on every path:

- **Call:** `ExecuteTool` asks `ToolCallRefusal` before any handler runs, so an
  agent loop, the engine's own tool loop, `CallToolMsg` and MCP `tools/call`
  are all held to it. `handleCallTool` asks it first only to answer cleanly.
- **Listing:** gRPC `ListToolsMsg` and MCP `tools/list` ask `ToolListed`, which
  is the call decision without the caller-kind rule -- a listing describes
  tools, and the gRPC surface has always described them to callers that stamp
  neither kind. `TestAToolIsListedExactlyWhenItIsCallable` holds the two to
  one decision.
- Every refusal reads as a PERMISSION refusal to `ClassifyToolError`, so the
  agent loop's repeat-failure breaker treats it as one.

**Cross-node.** `CallToolMsg` runs on the agent node: the agent kind arrives
threaded in the envelope's `agent_role` metadata, and the person as the
forwarded authority the mesh binds onto the stream's context
(`bindForwardedContext`), which `toolCallerContext` hands to the rank floor as
an `AccessContext` -- the same actor `handleExecuteQuery` binds. An agent turn
forwarded to the agent node carries the person the same way. No state lives on
one node that another needs.

### Load-time checks

A gate whose value names nothing reads like a restriction while deciding
nothing, so both refuse a strict boot (coded, in brackets, and in the
conformance corpus):

- `requires_agent_role_unknown` -- a `@requiresAgentRole` value outside the
  agent concept's enum, naming the tool, its file and line, the legal values,
  and `@requiresRank` as the way out when the gate was meant for a person. A
  roleSlug (`system-planner`) is refused by this rule.
- `requires_rank_unknown` -- a `@requiresRank` slug the ladder does not know,
  on a tool, query, mutation or logic alike (the function refusal carries the
  code too now).

## The deprecation window

`@allowedRoles` is a public language form, so it leaves through the window
(`component/language/deprecation`, rule `deprecated_allowed_roles`):
**deprecated in 0.24.0, stops loading in 0.26.** Inside the window it keeps its
exact behaviour -- one string, `""` read as `specialist`, the person's role
string over MCP -- with one edge closed: a person over MCP with NO role was
refused outright before (the surface stamped nothing), so `""` does not become
`specialist` for them now.

Every use warns naming both replacements and the rewrite, on the load report,
in `memqllint`, in Sense and in the language server (which offers the rewrite
as a quick fix), and is counted on
`memql_dsl_deprecated_uses_total{rule="deprecated_allowed_roles"}`. Once the
window is spent the parser refuses the spelling the way it refuses an expired
`array(T)`.

## The migration

`memqlmigrate --rewrite=allowed-roles` carries each use to what it meant, and
leaves -- reporting why on stderr, never guessing -- every list it cannot carry
across exactly:

| The list | Becomes |
|---|---|
| agent roles only | `@requiresAgentRole` with the same values |
| person roles only, forming a floor | `@requiresRank("<the list's lowest role, as written>")` |
| agent and person roles mixed | left: the tool may need both gates |
| a value neither vocabulary knows (a roleSlug, a custom role, a typo) | left |
| person roles that skip a rung above their lowest | left: not a floor |
| a construct that already carries the replacement | left: it would be written twice |

In this repository: 28 of the tree's 32 uses, across `dsl/agents`,
`dsl/compose`, `dsl/library` and `dsl/forge`, were rewritten (14 agent-kind
lists, 14 person lists); the four `dsl/work/capabilities.memql` uses were left
by the rewrite
(`system-planner`) and fixed by hand to `@requiresAgentRole("assistant",
"specialist")`, which is what makes them callable by the planner. `packs/`,
`examples/`, `deploy/fleet` and the product bundles carry no use.

The rewrite writes the lowest role AS WRITTEN (`writer`, `reader`), not its
catalog slug (`user`, `viewer`). A first boot validates floors before the
catalog is seeded, against the compiled base ladder, which knows the legacy
slugs and not `user` / `viewer`; `@requiresRank("user")` is refused there and
by the offline lint. The two spellings name one rung.

## Forge: the deliberate change in meaning

The forge tools gated on person-role lists and now gate on rank floors:
the developer tools `@requiresRank("writer")`, the team tools
`@requiresRank("reader")`. What that changes, deliberately:

- **A custom role is admitted by its rank.** A role ranked between two named
  ones (a 150 "lead") reaches the developer tools, which no slug list could
  name. So does the catalog's member slug `user`, which the lists never named
  (they named its alias `writer`).
- **The three gates on the developer tier are one floor.** memql#4112 is the
  failure of three hand-kept lists (the tools' `@allowedRoles`, the
  `forgeDeveloper` spec on the validation queue, `forgeRequestRoleAllowed` on
  the transition) disagreeing silently -- an empty queue, not an error. A spec
  cannot read a rank, so `forgeDeveloper` could not have agreed with a rank
  floor: it is gone, the validation queue declares the same
  `@requiresRank("writer")`, and the transition rule reads the same floor
  (`forgeDeveloperFloor`) through the same ladder.
  `TestForgeDeveloperTierIsOneFloor` holds the three together.
- **A caller below the tier is refused, not shown an empty queue**, on the
  validation queue query as on its tool.
- **An agent acting for a person can reach a forge tool** when a skill offers
  it, judged by that person's rank. Before, no agent kind matched a person-role
  list.
- Unchanged: the approval queue returns rows to the owner alone
  (`forgeApprover` stays, and is exact -- owner is the top rung and nothing
  aliases it), and only an owner may approve.

## Findings recorded, not changed here

- `CallToolMsg`'s `agent_role` metadata is read from the client envelope as
  sent; no in-repo client sets it, and nothing strips it from a client stream,
  so a direct stream client can assert an agent kind. The rank floor still
  judges the person the stream authenticated as. Pre-existing; worth its own
  issue.
- The compiled base ladder's missing `user` / `viewer` (above) is why the
  rewrite keeps the legacy spellings; teaching the fallback the catalog slugs
  would let a floor name them on a first boot.
