# Agent-Role Axis for Tools -- Design

- **Date:** 2026-09-26
- **Status:** approved by the owner ("split into two gates"); implemented for
  memql#5438 in the dsl-v1-followups epic, and revised by its review round
  (items 1-6, marked where they land below).
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
`MEMQL_MCP_USER`), and a session with none is offered no tool as well as
refused every call (review round, item 4). A person is not an agent kind, so
`@requiresAgentRole` refuses them and hides the tool from their `tools/list`.

### Enforcement points

One decision (`component/memql/tool_gate.go`), asked on every path:

- **Call:** `ExecuteTool` asks `ToolCallRefusal` before any handler runs, so an
  agent loop, the engine's own tool loop, `CallToolMsg` and MCP `tools/call`
  are all held to it. `handleCallTool` asks it first only to answer cleanly.
- **Listing:** gRPC `ListToolsMsg` and MCP `tools/list` ask `ToolListed`, which
  is the call decision less one rule: a caller that is NEITHER kind is
  described the tool set, as the gRPC surface always described it to callers
  that stamp neither. A person over MCP is held to the identity rule in both.
  `TestAToolIsListedExactlyWhenItIsCallable` holds the two to one decision.
- Every refusal reads as a PERMISSION refusal to `ClassifyToolError`, so the
  agent loop's repeat-failure breaker treats it as one.

**Cross-node.** `CallToolMsg` runs on the agent node: the agent kind arrives
threaded in the envelope's `agent_role` metadata, and the person as the
VERIFIED forwarded authority the mesh binds onto the stream's context
(`bindForwardedContext`). `toolCallerContext` keeps that actor as it was
proved -- role ceiling included -- rather than re-resolving the claims beside
it, which could disagree; only a direct stream, which carries claims alone,
resolves its actor the way `handleExecuteQuery` does. An agent turn forwarded
to the agent node carries the person the same way. No state lives on one node
that another needs.

A tool's floor is enforced through `refuseBelowRequiredRank`, so an
internal-origin call passes it and an unresolvable floor or caller fails
closed, as on a query. **It judges the PERSON at the rank they hold, never a
stand-in role** (review round, item 1). Two actors carry one:
borrowed authority (`auth.ContextWithUserActor`) and work restored without a
captured grant (`auth.ContextWithPersistedOwner`) both assert `writer`
whatever the person holds, and judged as they stood an agent acting for a
reader cleared a writer floor while one acting for an admin failed an admin
floor. Both now say so (`AccessContext.RoleStandIn`), and `toolFloorContext`
reads the person's role from the principal table instead --
`organizationUserRole`, the read the account scope already makes for the same
borrowed actor -- once per tool caller (a memo the two caller stamps open). A
person the table does not resolve holds no role and clears no floor; the
stand-in is never the fallback. The replacement is for the judgment only: the
call still runs under the stand-in, which bounds what the work may write. The
marker is process-local -- `ForwardedAuthority` carries no field for it, and
no tool loop runs on the far side of a mesh hop today. A QUERY floor under
borrowed authority is unchanged, so a tool admitted at its person's rank can
still meet a handler query whose own floor the stand-in fails; that refusal is
closed, not open.

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

The rewrite writes the lowest role AS WRITTEN -- `writer` and `reader` in this
tree, `user` or `viewer` in a list that spelled the catalog's slug. A first
boot validates floors before the catalog is seeded, against the compiled base
ladder (`auth.RoleRank`), which knew only the legacy slugs, so
`@requiresRank("user")` refused to load there and in the offline lint. The
compiled ladder now ranks every name the seed gives a rung, slug and alias
alike (review round, item 2), `TestEngineRankModelMatchesTheSeeds` holds each
to its seeded rank, and a memqllint test lints a rewritten tree clean.

**Every person-list rewrite widens its gate, and the rewrite says so** (review
round, item 3). In an agent's tool loop `@allowedRoles` compared the agent's
own role, which no person role matches, so a person list refused every agent;
`@requiresRank` judges the person an agent acts for, so after the rewrite an
agent acting for somebody at or above the floor can call the tool -- and so
can a custom role ranked there. Each such rewrite is reported on stderr beside
the uses left (`AllowedRolesRewrite.Note`); an agent-list rewrite admits
nothing new and is not.

**Comments inside a list survive the rewrite** (review round, item 6). An
agent list keeps its argument list exactly as written under the new name; a
person list's rung carries each note as a block comment; a line comment that
would end that block early leaves the use for its author. Past the window, one
use is ONE strict-boot problem, at the author's position: the tool loader's
coded echo of the same parser refusal, positioned in the declaration it parsed,
is dropped by the deprecated-form pass.

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

## Procedure replay (review round, item 5)

A learned procedure's replay of a recorded MCP step (`runProcedureMCP`,
`app/procedure_dispatch.go`) ran the call as the owner's reasoning AGENT,
stamped with the agent row's roleSlug (`assistant`, or `system-planner`, which
is no agent role), so every gate was decided on the wrong axis. Only an app
session records, and the MCP surface calls a tool as a PERSON, so the replay
is now a person over MCP (`WithMCPHumanCaller`) decided by the actor the
surface built for the recording: **the owner, holding no role** -- an
app-session credential carries no role claim, and the HTTP head takes a
session's role from the claim.

Deliberately not the owner's own rank, which the stand-in resolution above
would give a borrowed actor: that would admit a rank-floored read the
recording was refused, and a replay asks for no more than its recording had.
`TestAnAppSessionActsAsItsOwnerHoldingNoRole` (`component/mcp`) pins the
app session's actor and says the replay must change with it; whether an app
session SHOULD hold its owner's role over MCP, as the gRPC surface resolves the
same credential to, is a separate decision (below). The server's owner and
agent still win over recorded ones as tool arguments.

## Findings recorded, not changed here

- `CallToolMsg`'s `agent_role` metadata is read from the client envelope as
  sent; no in-repo client sets it, and nothing strips it from a client stream,
  so a direct stream client can assert an agent kind. The rank floor still
  judges the person the stream authenticated as. Pre-existing; worth its own
  issue.
- An app session is resolved differently by the two surfaces: gRPC's
  `LoadFromClaims` gives the credential its owner's stored role (surface-pinned
  to reads), while the MCP head takes the role from the token's claim, which an
  app-session credential does not carry -- so over MCP it clears no rank floor
  and no `@allowedRoles` person list. The replay follows the MCP answer
  (above). Aligning the two is an authorization decision, not a refactor.
- Work restored without a captured grant asserts `writer` for DATA whatever
  its owner holds, so a reader's background run writes as a writer. The tool
  floor no longer reads that role; the data authority is unchanged here.
