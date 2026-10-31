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
| D4 | `package.dslDeployedBy` is written by a `@serverOnly` mutation immediately before the pointer moves; the existing guard covers the new field. A package declaring a fleet workflow stops there if the write fails; one that does not only warns. | After the pointer moves, any restart runs the new DSL, and a run that fails in the roll never reaches a later write while its retry finds nothing to stage. Going on would run new workflows on the previous person's machines. |
| D5 | A workflow's domain is read from where it was loaded: the first segment of `ConstructNamespaceForOrigin`. | The same spelling `dslDomains` holds, and nothing in the source can claim another domain. |
| D6 | No package, or more than one, listing the domain is a refusal. | Fail closed. Two packages may pin one namespace; picking one would be a guess about whose machines to use. |
| D7 | Each call re-reads the person's current role and requires `auth.CanAuthor`. | The same gate an automatic deploy answers. Someone who lost the right to author constructs does not keep running a workflow on their machines; a read that fails refuses. |
| D8 | The call runs on `route:local`, and refuses when this cluster's Local lists anything but fleet sources. | Fleet only: their machines first, then machines lent to them (memql#5662). A route never falls through, so no signed-in app session and no vendor is reached for work a visitor's click may have started. Local can be edited for the whole cluster, so the promise is checked rather than assumed. |
| D9 | The authority is bound for the one call: a verified forwarded authority at `RoleWriter`, as `integrations/pipelinesteps.ownerAuthority`. | That is what lets the hop cross nodes, and no statement outside the `ai` call runs under it. |
| D10 | A person already acting keeps their own fleet, still on Local. A run with an owner is left alone. | An authored automation runs as its author, and a person resuming a run acts for themselves, but the run may be one a visitor started. A goal's run is its owner's, on the route they chose. |
| D11 | Each automation decides for itself. Entering an automation sets or clears the declaration; a `logic` or `query` it calls inherits. | A flagged workflow calling an unflagged one must not lend it a person. |
| D12 | `@usesDeployerFleet` on a `before=` hook is a load error. | A write must not wait on a person's laptop. |
| D13 | Analysis reports the declaring workflows per domain, the deploy review names them, and they are part of the plan an automatic deploy compares. | Whoever presses Deploy is told which workflows will use their machines, and a push adding one waits for someone to read that. |

## Limits

Package DSL runs at internal origin and is trusted to name itself: DSL is server-side
authority for the whole cluster (`operate/packages.md`). A deployed package could call
`recordPackageDslDeployer` for another package. Refusing that needs the multi-tenant DSL
sandboxing that document says is not built.

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
- Seam: an unflagged run is untouched, a person acting keeps their fleet on Local
  (D10), a flagged run gets the person, a forwarded authority and `route:local`, and the
  caller's context is not changed (D1). A Local route listing anything else refuses
  (D8). Entering an unflagged automation clears it, and a resumed run keeps it (D11).
- Package: a deploy that stages DSL records the actor before the pointer moves, a roll
  that fails leaves it recorded, a failed write stops a package declaring a fleet
  workflow and not one that declares none, one that does not stage keeps the record, a
  rollback records its actor, a plan gaining a fleet workflow parks an automatic deploy,
  and the guard refuses the field without internal origin.
- Journal, against a database: a borrowed call on a blank-owner run records a model
  call the run's own reader finds.
- Hop: a borrowed call on a node holding no machine stream forwards with the person's
  authority and reaches `ForwardModelCall`.
- Deploy review: the report's list reaches the page.
