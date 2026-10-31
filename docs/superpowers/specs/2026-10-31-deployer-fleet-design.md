# A deployed workflow's model calls on its deployer's fleet

- **Status:** approved by the owner 2026-10-31.
- **Related:** memql#5662 (an automation under a person's borrowed authority is
  that person's work), `docs/public/operate/shared-machines.md`, design record
  2026-10-29 (`package.dslDomains`).

## The problem

An automation in a deployed package runs as the cluster. `builtin ai` under that
identity reaches a machine only when it is lent to everyone, a mode the OS no
longer offers, or a federated vendor. On a cluster with neither, every model call
a package's workflow makes is refused with `every_door_shut`, however many
machines its deployer has online.

Identity alone would not fix it. A workflow usually runs on a node that does not
hold the machine's stream, and `ForwardModelCall` refuses a hop with no forwarded
authority bound to the context (memql#3205). A background workflow has none.

## What it does

A workflow may declare `@usesDeployerFleet`. Each `builtin ai` call in that
workflow is then planned, admitted, dispatched and counted as the person who
staged the package's DSL, on the `Local` route. Everything else in the run stays
what it was: the cluster acting.

```memql
@usesDeployerFleet
@trigger(event="graph.node.created.v1:shop:statusRequest")
automation writeStatusBrief {
  args { id string! }
  written := builtin ai(templateId: "statusBrief", data: {evidence: args.id}) on error continue
}
```

## Decisions

| | Decision | Why |
|---|---|---|
| D1 | Only the model call borrows, never the run. | A bug in a workflow must not read or write as a person. Row authority, `@serverOnly` writes and the run's blank owner are untouched. |
| D2 | The workflow declares it; the DSL cannot name a person. | `@usesDeployerFleet` is a flag. The person comes from a server-written record, so no argument, row or prompt can choose whose machines serve the call. |
| D3 | The person is whoever last STAGED the package's DSL: the actor of the deploy or rollback that staged it. An automatic deploy's actor is the package owner, as it already is. | Consent by act. A developer deploying a package they do not own spends their own machines, not the owner's. A deploy that does not move the DSL does not move the person, unless none is recorded yet. |
| D4 | `package.dslDeployedBy` is written with `package.dslDomains`, at the same commit point, by a `@serverOnly` mutation; the existing guard covers the new field. | The write must not precede a commitment, for the reason the domain list's must not. |
| D5 | A workflow's domain is read from where it was loaded: the first segment of `ConstructNamespaceForOrigin`. | The same spelling `dslDomains` holds, and nothing in the source can claim another domain. |
| D6 | No package, or more than one, listing the domain is a refusal. | Fail closed. Two packages may pin one namespace; picking one would be a guess about whose machines to use. |
| D7 | Each call re-reads the person's current role and requires `auth.CanAuthor`. | The same gate an automatic deploy answers. Someone who lost the right to author constructs does not keep running a workflow on their machines; a read that fails refuses. |
| D8 | The call runs on `route:local`. | Fleet only: their machines first, then machines lent to them (memql#5662). A route never falls through, so no signed-in app session and no vendor is reached for work a visitor's click may have started. |
| D9 | The authority is bound for the one call: a verified forwarded authority at `RoleWriter`, as `integrations/pipelinesteps.ownerAuthority`. | That is what lets the hop cross nodes, and no statement outside the `ai` call runs under it. |
| D10 | A person already acting wins; the flag changes nothing. | An authored automation runs as its author, and a call in a person's context is already theirs. |
| D11 | Each automation decides for itself. Entering an automation sets or clears the declaration; a `logic` or `query` it calls inherits. | A flagged workflow calling an unflagged one must not lend it a person. |
| D12 | `@usesDeployerFleet` on a `before=` hook is a load error. | A write must not wait on a person's laptop. |
| D13 | Analysis reports the declaring workflows per domain, and the deploy review names them. | Whoever presses Deploy is told which workflows will use their machines. |

## What stays refused

`agent` and `runAgentTurn` are not covered: they carry tools, and tools are a
different consent. A refusal is an ordinary `ai` error, so `on error continue` and a
workflow's own failure path work as they do for any shut door.

## Prompts carrying visitor text

The seam adds no parsing and no new input. A workflow passing visitor text to a model
must pass it in the prompt's `data`, whose fields the prompt's body validates. The
`ai` builtin has no tools, so the reach of an injected instruction is the text of the
reply. `operate/shared-machines.md` states the rule.

## Tests

- Load: the flag compiles, a domain is recorded from the path, a `before=` hook is
  refused.
- Resolver: each refusal in D3, D6 and D7, and the happy path.
- Seam: an unflagged run is untouched, a person acting is untouched (D10), a flagged
  run gets the person, a forwarded authority and `route:local`, and the caller's
  context is not changed (D1). Entering an unflagged automation clears it (D11).
- Package: a deploy that stages DSL records the actor, one that does not keeps the
  record, a rollback records its actor, and the guard refuses the field without
  internal origin.
- Journal, against a database: a borrowed call on a blank-owner run records a model
  call the run's own reader finds.
- Hop: a borrowed call on a node holding no machine stream forwards with the person's
  authority and reaches `ForwardModelCall`.
- Deploy review: the report's list reaches the page.
