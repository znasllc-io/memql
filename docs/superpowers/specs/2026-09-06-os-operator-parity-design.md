# MemQL OS operator parity -- Design

- **Date:** 2026-09-06
- **Status:** built. Every decision below was made against engine source and
  is recorded with what it rests on; three of them CORRECT the issue text
  that asked for them, and those are marked.
- **Epic:** memql#5009, the remainder of the portal-removal inventory
  (memql#4984). Issues memql#5010 (Concepts), memql#5011 (four cluster-state
  surfaces), memql#5012 (the Shopify connector's operator surface),
  memql#5013 (four gaps inside existing apps), memql#5020 (the ts-viewkit
  arrangement module).
- **Scope:** `clients/os/src/apps/{concepts,cluster,stores}/` (new),
  `clients/os/src/cluster/` (a shared vocabulary), edits inside
  `clients/os/src/apps/{files,fleet,accounts}/`, `clients/os/src/main.tsx`
  and `chrome/Shell.tsx` (one boot-time reader), `component/memql`
  (one authorization gate), `sdk/ts-viewkit` (a removal),
  `editors/vscode` (one command).

---

## 1. The through-line: an absent number is not a zero

Every surface in this epic reports figures an operator acts on -- how far a
connector is behind, how many rows drifted, how many dead letters are
waiting, what a delegated run spent. For each there is a state where the
honest answer is not a number, and the failure mode is always the same
shape: rendering it as `0`.

A zero is a measurement. It says *we looked, and the answer is none*. An
absent figure says *we did not look, or we could not*. They lead to opposite
actions, and `?? 0` turns the second into the first silently.

So `clients/os/src/cluster/figure.ts` makes them different VALUES -- a
`Figure` carries a measured number or an `AbsentReason`, never both --
and `FigureValue.tsx` renders the absent form as an em dash carrying its
reason. This mirrors `component/proving/figure` in Go, which made the same
decision for the same reason in epic memql#4993.

The portal had this right by hand on one table, with a comment reading
"never run" is not "ran clean". Carrying it as a type rather than as a habit
is what stops the fifth surface getting it wrong.

`reading.ts` is the second half: these surfaces read things the graph does
not broadcast, so they read once, **print when they looked**, and offer to
look again -- the Accounts ledger's rule. Each reading settles on its own,
never under one `Promise.all`, because a single combined await lets the one
read that WILL be refused decide the state of the reads that succeeded.

---

## 2. Decisions that CORRECT the issue text

### D1. Modules is `{ any: ["owner", "admin"] }`, not developer

memql#5011 says "Modules and Data origins are engineering surface
(developer)". Against the engine that is wrong in both halves, and shipping
it would have produced the exact failure the OS's own manifest contract
warns about: a section offered below the engine's floor opens on a refusal
for everyone in it, and reads as "this app is broken".

The chain, in source:

- `handleModulesList` / `handleModuleDetail` call
  `memqlengine.AuthorizeModuleRead` (`component/grpc/module_handlers.go`).
- That is `auth.AtLeastAdmin` (`component/memql/module_registry.go:155`).
- Which is `roleHasCapability(role, "create", "principal")`
  (`component/auth/rbac.go:213`).
- And `component/auth/rbac_model.go` gives **developer only `read` on
  `principal`** -- the model says "deliberately NOT `create` on `principal`".

So `AtLeastAdmin` admits {owner, admin} and refuses developer. Under the one
role ladder developer ranks 300, ABOVE admin's 200, so `{ min: "admin" }`
would admit precisely the role the engine refuses. This is the second
genuinely non-monotonic gate in the shell, and it takes the same `{ any }`
form Settings -> Integrations does.

### D2. Data origins and the Audit trail are owner, not developer

`syncStatesAll` filters `actor.isClusterOwner==true`
(`dsl/platform/queries.memql`), and `v1:platform:syncState` declares
`@rowAuthz(clusterOwner)`. `recentAuditEvents` carries the same filter and
`v1:identity:auditEvent` declares the same tier.

### D3. The audit trail's floor is the ONLY thing that can stop it lying

This is the finding memql#5011 asked for, and the mechanism is worth stating
because it is not obvious.

Row admission **returns zero rows, not an error**. So a non-owner calling
`recentAuditEvents` gets `[]` with nothing to distinguish it from a cluster
where nothing has happened. The surface cannot detect the refusal and
therefore cannot explain it -- there is no error to render.

A client-side floor is not a nicety here; it is the only mechanism available.
`{ min: "owner" }` on the section means the person who would be lied to never
reaches the surface, and an owner's empty result can then honestly read "no
events recorded".

The portal rendered this list with **no client-side gate at all**, so a
non-owner got an empty timeline and no indication it was not the truth.

---

## 3. Placement

### D4. A Cluster app, not four Settings sections

memql#5011 left placement open. The split is: **Settings is what you SET;
the Cluster app is what the cluster IS.** Modules, data origins, agents and
the audit trail are read-only inspection of composition and state, and none
of them is a preference.

Sections, each with the floor its engine gate earns:

| Section | Floor | Why |
|---|---|---|
| Readiness | app floor | see D5 |
| Modules | `{ any: ["owner","admin"] }` | D1 |
| Data origins | `{ min: "owner" }` | D2 |
| Agents | app floor | see D6 |
| Audit trail | `{ min: "owner" }` | D3 |
| Logs | `{ min: "admin" }` | the engine's log floor, spec L3 |
| Settings | app floor | |

App floor `{ min: "admin" }` = {admin, developer, owner}.

The Settings app already has a section called Cluster. That is accepted
rather than worked around, on the precedent the Logs app records: an app and
a section may share a name where they are the same subject at two scopes.

### D5. First run is a PLACE, not a gate

memql#5013 asks whether the portal's first-run gate becomes a card, a
Settings prompt, or nothing, and says to record the answer either way.

The answer is a **Readiness section in the Cluster app**, and the reasoning
turns on what the portal's gate actually was. It was client-side only
(`FirstRunGate.tsx` gated the console, not the cluster), it latched per
session, and its own skip existed in one mode. It enforced nothing. It was a
signpost wearing a gate's clothes.

So the two facts it surfaced are worth keeping and the ambush is not. The
section reads `inferenceStatus({})` and `passkeysForSelf({})` -- the same
two reads -- and states each with where to fix it. It never blocks anything.

The trade-off is real and is recorded rather than hidden: a brand-new owner
who never opens the Cluster app is not told. The OS's own position is that a
first-run question which ambushes somebody mid-task is one they dismiss, and
a dismissed question needs somewhere to be remembered; this gives it a place
instead. `apps/accounts/FirstRunCard.tsx` is untouched and answers a
different question (naming the operator's own client record).

### D6. The Agents floor is editorial, and says so

`v1:agents:agent` **declares no row-authz tier at all**. `activeAgents`,
`allAgents` and `agentById` therefore return every agent in the cluster --
including `systemPrompt` and `providerConfig` -- to any authenticated
caller. That is pre-existing and this epic does not widen it.

The section's floor is presentation over an ungated read, not a mirror of an
engine gate, and both the manifest comment and the section say so. What the
floor does buy is that a generic operator surface does not make the standing
undeclared-tier long tail trivially DISCOVERABLE as well as reachable --
which are not the same fact.

`v1:agents:agentAuthorization` is the opposite case: it declares
`@rowAuthz(owner="userId")`, so that list is the caller's own grants and
nobody else's, **even for an owner**. The surface says so, because a
cluster-wide heading over a self-scoped list is a claim the surface cannot
support.

### D7. A Stores app, owner-only

memql#5012's subject is a live integration rather than the cluster, so it is
its own app rather than a Cluster section. Owner floor, matching every
Shopify query's `actor.isClusterOwner==true`.

**The per-domain acts stay in Data origins.** Backfill, per-domain pause,
retry and discard are the generic sync runtime
(`datasyncStartBackfill`, `datasyncSetSyncPaused`,
`datasyncRetryOutboxEntry`, `datasyncDiscardOutboxEntry`) and they act on a
(concept, connector) pair, not on a store. The store surface carries the
store-wide pause and the Shopify-specific subscription reconcile. Both
surfaces exist in this epic, so memql#5012's third checkbox is met across
the two -- and the split is the portal's own, whose comment says why: two
pages carrying the same three buttons is the duplication that design exists
to avoid.

**Credentials are references, never values.** `adminTokenRef`,
`storefrontTokenRef` and `webhookSecretRef` name `v1:platform:globalSecret`
rows. The add-a-store form asks for secret NAMES and can neither accept nor
display a token.

---

## 4. The Concepts app (memql#5010)

### D8. The declared-vs-observed join is the feature

A field list on its own is the DSL file read back, and an author can already
read the DSL file. What an operator cannot get anywhere else is the join,
and both directions of it are real defects with no other symptom:

- **A declared field nothing writes** reads exactly like a field whose value
  happens to be empty, so a mutation that quietly stopped setting it looks
  like data that is merely absent.
- **An undeclared key** is invisible to the DSL and to every shaped read,
  because a shape can only project declared fields.

Every observed claim is scoped to its sample -- `presentIn` is a count, and
the sentence names how many rows it rests on. A field missing from 200
loaded rows is evidence, not proof, and the surface does not say otherwise.

The declared half comes from `concept.fields` (epic memql#4661) rather than
from a JSON Schema riding on a row, which is what the portal had to do.
**Empty `fields` is a real answer and is not "no fields"** -- it means this
server publishes no shape -- so it has its own state rather than collapsing
into an empty list.

### D9. Arrivals are counted, not spliced

`browseConceptPage` walks `createdAt asc` with a cursor bound to that
ordering, so a row created while somebody reads belongs after pages the walk
has not reached. Splicing it would draw it among rows it does not belong
between, and the next page would fetch it again.

So new rows are counted in a band and the person is offered a reload. This
is also why the rows list is not `kit/LiveList`: that takes a
`LiveCollection`, whose model is one authoritative fold events are applied
INTO, and a paged walk is not one. Dressing it as one puts the arrival ring
on a row whose position is wrong.

An id-only event (`payloadOmitted`, the `granted` tier's fan-out) is re-read
through the ordinary authorized path, and a refusal drops it -- counting
those blind would tell somebody "4 new rows" about rows they may not read.

### D10. A concept with no `@displayCard` gets its id, not a guess

The tempting fallback -- `name`, then `title`, then `label` -- is a guess
that renders as a fact: a row whose `name` field holds something that is not
its name appears under a heading that is simply wrong, with nothing saying
it was inferred. An id is always true.

`@displayCard` is honoured; the RENDERING is the OS's own rather than
`sdk/ts-viewkit`'s, because the OS renders React through its own kit and
view-kit renders HTML strings for the VS Code webviews. Pulling in a package
to read four field names would be a dependency bought for a lookup.

### D11. An empty row window says "not readable by this account"

Row admission decides what reaches the browser, so an empty answer means
"none that you may read". "This concept is empty" would be the window
inventing a fact about the cluster.

### D12. The VS Code handoff is a query parameter, because there is no route

The portal answered `/concepts/:id`. MemQL OS is a desktop shell with no
router -- a window carries an app and a section, not a path -- so the
equivalent is `?concept=<id>`, read once at module scope in `main.tsx`,
scrubbed from the address bar with `replaceState`, and turned into an open
intent by a dispatcher in this app's tree.

That is modelled line for line on the GitHub-connect return
(`apps/deployables/sources/connectReturn.ts`), including where each half
lives: the capture is strictly earlier than `AuthProvider`'s own read of the
query string, and each reader removes only its own parameters.

An empty value is not a request. Honouring it would open the app on its list
and make a broken link indistinguishable from a working one.

A person whose role does not admit the app opens nothing: `openApp` refuses
an app the actor cannot see. A link is not a way past the launcher's gate.

**The extension's own button is an ADD, not a revert.** It was removed in
memql#4984 because it opened `/concepts/<id>` and after the retirement no
page answered that route; a menu item that 404s teaches nobody where the
rows are. It comes back pointed at the surface that now answers.

---

## 5. `dataOrigins` was documented as owner-gated and was not

`dsl/common/builtins.memql` states that `dataOrigins` and its two siblings
are owner-gated, and draws the contrast explicitly: `fleetModels` and
`inferenceStatus` are NOT, "and that is the difference from the three
above". `providerAuthStatus` enforced it. `evaluateDataOriginsExpression`
took `_ context.Context` -- it discarded its context entirely -- so the
claim was true of the documentation and false of the code.

There is no row for a tier to gate: the projection is virtual and
`v1:platform:dataOrigin` declares no `@rowAuthz`, and an undeclared concept
is delivered to every caller. The wall had to be in the executor or there
was no wall.

**Why it survived:** the health half beside it in the same page really was
gated, so the surface behaved as though it were owner-only because half of
it was.

Fixed to match its sibling, with a test whose negative half asserts the
refusal SENTENCE (a refusal for some other reason would pass a weaker test
while leaving the read open) and whose positive half proves the gate does
not simply refuse everybody.

---

## 6. memql#5020: the arrangement module is removed

The issue frames this as "a breaking change to a published package surface".
**It is not a published surface, and that premise was the whole question.**

- The org's GitHub Packages registry holds exactly one npm package,
  `memql-sdk-core`. `@znasllc-io/memql-view-kit` is not in it.
- The only publish workflow is `publish-sdk-core.yml`, working-directory
  `sdk/ts`, on `memql-sdk-core-v*` tags. Nothing anywhere publishes
  `sdk/ts-viewkit`; its `publishConfig` block is aspirational.
- Its only consumers are two in-repo `file:` dependencies:
  `clients/portal` (deleted by memql#4984) and `editors/vscode`.
- The extension imports four symbols -- `escapeHtml`, `renderToHtml`,
  `renderInstallSteps`, `viewKitStyles` -- and zero arrangement or layout.

**"Is this a published surface" is answered by the release pipeline, not by
the package's own metadata.** Every signal of a public surface was present
except the one that decides it.

With no external consumer possible, option 1 collapses and option 2
(`@deprecated` and keep) is what the branch-workflow rule refuses --
pre-release, no deprecation windows, delete what is no longer needed. So the
module and its layout sibling are removed with their tests.

`sanitizeArrangement`'s repair rules are carried here because they outlive
it: **absent means stack, absent means standard.** If "absent" ever came to
mean anything else, the release that changed it would silently re-lay-out
every stored arrangement with no migration and nothing in the row saying
what it used to look like. That is a compatibility property any future
grammar with optional layout fields needs, and it is why the rule was worth
writing down rather than simply deleting.

---

## 7. What this epic did NOT do

- **`v1:agents:agent`'s missing row-authz tier is not fixed here.** Declaring
  a tier narrows every existing read with no exemptions, and the planner and
  the replier both read agents; that is an engine change with a blast radius
  this epic has no business absorbing. It is recorded in D6 and named in the
  PR.
- **No AiSuggest domain was added.** `viewArrangement` and `uiAssist` went
  with the portal. If the Concepts app ever wants schema assistance that is
  a fresh `RegisterSuggestDomain` with a test on the registration itself --
  memql#4667 shipped a domain called-but-unregistered, and the registry's
  unknown-domain error reaches a user as "suggestions are not available on
  this cluster", the same sentence a cluster with no provider gets.
- **No arrangement engine.** memql#5010 says reaching for one is a decision
  rather than a shortcut, and it was not taken: these are hand-built
  sections under the twelve rules.
