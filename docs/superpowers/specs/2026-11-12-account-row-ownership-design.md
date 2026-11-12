---
title: Account row ownership -- every row records the account it belongs to, or says it is cluster-wide
audience: internal
status: approved
area: design
sinceVersion: 0.31.35
owner: znas
---

# Account row ownership

Owner brief, 2026-11-11 and 2026-11-12, in chat. This is the first piece of the
[accounts program](2026-11-12-accounts-program-design.md), which wants complete
separation between accounts and an export of one account's data. Both need the same
thing first: a row has to say which account it belongs to. Today about 40 concepts
carry an account and about 230 do not.

This piece makes every row record its account. It changes nothing a person can see
except the Accounts app's inventory. Reads are untouched; sign-in separation and
export read the stamp later.

**Approvals.** The owner, in chat, on 2026-11-12: chose extending the existing
`accountId` field over a new row column and over deriving the account at read time;
approved the six design sections one by one; approved landing the piece in one push
with the two adjustments in D10 and D9; and then accepted every recommendation in
this record, including the three additions made while it was written (the `shared`
form in D7, the spelling rule in D2, and the index in section F), and asked for it
to be built, landed and deployed. The section "Refinements made while planning"
came after that acceptance, and sections B to M below were then rewritten to
describe what was built. Where the decisions table and a refinement differ, the
refinement is what shipped.

## Locked decisions

| # | Decision | Chosen |
|---|---|---|
| D1 | The declaration | Every concept carries exactly one `@belongsTo`. Its rows belong to an account, or follow a parent's account, or are cluster-wide |
| D2 | One field | The account is always recorded in `accountId`. Declaring `@belongsTo(account)` makes that field a reference to `v1:accounts:account` with no `@relationship` line |
| D3 | Recording is not sharing | `@belongsTo` records. The `account=` argument of `@rowAuthz` is what opens rows to an account's members, and stays a separate declaration |
| D4 | Who fills it in | The engine, at insert, in the single write path. No mutation changes |
| D5 | Where it comes from | In order: the parent row, an explicit choice, the call's account |
| D6 | It does not change | An update carries the account forward. Moving a row between accounts is a governed act that arrives with export |
| D7 | Cluster-provided rows | A concept may declare `shared`: its rows normally belong to an account, and a row with none is provided by the cluster to every account. **Added while writing** |
| D8 | Existing rows | Assigned in the background by six rules, first match wins. Two cases are never guessed. Every version of a row is stamped |
| D9 | One account per row | The five concepts that take a list gain the single field now. The list is removed in the export piece's push |
| D10 | The rollout switch | Stamping and assignment start only when every live node runs a release that knows the field. The switch flips one way |
| D11 | The inventory | One read answers what an account holds, by area. The Accounts page shows it beside the assignment result |
| D12 | The gate | In the engine and its packs, an undeclared concept fails the build. In a product bundle it warns, is counted and fails the linter, and becomes a load refusal two minor releases later |
| D13 | Delivery | One push to `main`, then one small push in `memql-fylo` |

## Refinements made while planning

Reading the code for the implementation plan, and then building it, changed
seventeen points. Each is recorded here with its reason and overrides the
decision it names. The owner accepted the design on 2026-11-12 and has not yet
read these.

| # | Refinement | Why | Overrides |
|---|---|---|---|
| R1 | **The switch is the engine's existing reader handshake.** Two plain tables, `account_stamping_storage` and `account_stamping_readers`: every node advertises itself, and the switch flips once every non-stopped node has. There is no `rolloutGate` concept and no `engineVersion` on node rows | Four switches of this shape already exist (`liveness.go`, `schema_compactor.go`). Older nodes write other nodes' rows, so a new key on `v1:cluster:node` would be refused by them, which is the very hazard the switch exists to avoid | E |
| R2 | **An operator can hold the switch:** `MEMQL_ACCOUNT_STAMPING_ACTIVATE=false`. The default is automatic | Once rows are stamped, an older engine cannot update them, so the release that turns stamping on cannot be rolled back. The first production deploy is held | E |
| R3 | **An undeclared concept never refuses the load.** A test over the engine's tree and its packs fails the build. A mounted bundle's undeclared concepts are named in one warning at load and counted, and the linter reports them without failing. The scheduled refusal moves to the export piece | Tests define 740 concepts inline and register them the way a pack does, and a load warning fails every accepting case of the conformance corpus | B, D12 |
| R4 | **The existing organization rules stay in charge of their 22 concepts.** The stamp only fills in a missing account, after they have run. "An update never changes the account" and "a child may not name a different account than its parent" apply to concepts whose account no caller states | Those concepts already stamp, validate and move their account (`validateOrganizationOwnership`, `validateOrganizationTransfer`). Applying the new rules to them would break moves that work today | C, D6 |
| R5 | **No default to the primary account at write time.** A row no source decides is written without an account and the job assigns it. A child whose parent has no account yet waits for the parent | A stamp never changes, so a row written before its parent was assigned would be wrong for good | C |
| R6 | **An empty string is "no account" everywhere** | Ten mutations write `accountId: ""` | C, F |
| R7 | **No implied relationship.** The stamp writes the bare id, as the owner stamp does, and a concept that declares a relationship on the field gets the canonical spelling from the existing step. Readers match both spellings | An implied reference would change how eight existing concepts store the field and break their queries | B, D2 |
| R8 | **`via` may name its concept:** `parent="<concept id>"`, for a field that carries no relationship and stores a bare id | The store-scoped concepts hold `storeId` that way | B |
| R9 | **The call's account is a context key, worked out lazily from the actor.** `ForwardedAuthority` carries it when an entry point set it | About a hundred call sites rebuild the access context and would drop a field | C |
| R10 | **The job assigns the newest version of a row only, in place,** and its liveness copy. It runs continuously, not once | The engine's own attribution migration does it that way. Rewriting every version of every row would roughly double those tables until they are vacuumed | F, D8 |
| R11 | **The inventory's capability is `execute app:accounts/inventory`** | A named part is `execute` by the registry's convention, and a seeded `read` that no section names fails two gates | H |
| R12 | **The five multi-account concepts keep accepting lists in this push.** `accountId` is derived from the list on every write; only the two multi-select pickers in the OS become single | The editor extension and the SDKs send lists | G |
| R13 | **A fifth key, `stated`:** `@belongsTo(account, stated)` says the concept's own mutations name the account. The engine never fills the field there, never refuses a change to it, and the assignment never touches its rows. Every concept that already carried `accountId`, 39 of them including the 22 organization-owned ones, is `stated` | R4 needed a declaration to stand on. Without one the stamp could not tell a concept whose writers decide from one that simply had not been filled yet | B, C, F |
| R14 | **`rbac.capability` and `rbac.grant` are cluster-wide for now.** The sign-in piece decides how a grant is scoped | Neither carries an account today, and a grant's subject can be a person, who is cluster-wide | I |
| R15 | **A person with an account-scoped role acts in that role's account,** before their memberships are consulted, and a person the principal table cannot resolve acts in none | A scoped role bounds everything its holder reaches to that account, so an older membership elsewhere must not decide; and a failed read is not "belongs to none" | C |
| R16 | **A run acts in the account of the row that triggered it,** read off the event and kept in the run's recorded trigger so a resumed or adopted run acts in the same one | The run's own actor is the cluster, which belongs to no account | C |
| R17 | **The deprecation-window entry for an undeclared bundle concept is not made in this push.** It arrives with the export piece, which is the first reader that cannot tolerate one | The window starts counting the day it is entered, and nothing reads the declaration of a bundle concept yet | B, D12 |

Four concepts were declared `account` instead of following a parent, because they
are among the 22 that state their own account: `recipient`, `journeyEnrollment`,
`groupMembership` and `packageDeployment`. One generated concept,
`v1:shopify:channel`, already had a Shopify field named `accountId`; it becomes
`sourceAccountId`. Four of the reviews pack's five concepts already copy the
account from the site a review arrived through, so they are `stated` too.

## A. What exists today

- **The field.** About 40 concepts declare `accountId`, 30 of them with the sharing
  argument below, and five declare the list `accountIds` (Library items, work goals,
  compositions, compose templates, recipes).
- **Sharing.** `@rowAuthz(..., account="<field>")` widens reads and writes to the
  members of the account's group (`component/memql/rowauthz_account.go`). It reads a
  string or a list.
- **Validation.** For 22 concepts named in a Go list, a write must name an active
  account the caller may work in (`validateOrganizationOwnership`,
  `component/memql/organization_ownership.go`). An absent value defaults to the
  primary account for a cluster operator and to the caller's one account otherwise.
- **The owner stamp.** The engine already fills in the owner field on a raw insert
  from the `@rowAuthz(owner=...)` declaration (`component/memql/rowauthz_insert_stamp.go`).
  The account stamp sits beside it.
- **The annotation registry.** Every annotation, where it may be written and what it
  takes is declared once in `component/language/annotations/registry.go`.
- **Nothing carries an account between nodes.** `auth.AccessContext` has no account
  field, and neither has `ForwardedAuthority` in `component/node/node.proto`.

## B. The declaration (D1, D2, D3, D12)

```memql
@belongsTo(account)                    // its rows belong to an account
concept note { accountId string ... }

@belongsTo(account, via="goalId")      // its rows belong to the account of the row `goalId` names
concept run { accountId string ... }

@belongsTo(account, shared)            // as the first, and a row with no account is cluster-provided
concept skill { accountId string ... }

@belongsTo(account, stated)            // its own mutations name the account; the engine leaves it alone
concept campaign { accountId string ... }

@belongsTo(account, via="storeId", parent="v1:shopify:store")   // `via` holds a bare id with no @relationship
concept review { accountId string ... }

@belongsTo(cluster)                    // cluster-wide: never inside an account
concept node { ... }
```

`belongsTo` is registered on the `Concept` receiver with the keys `account`,
`cluster`, `shared` and `stated` as flags and `via` and `parent` as strings
(`component/language/annotations/registry.go`,
`component/language/parser/belongsto_binding.go`). A concept is **engine-managed**
when it is account-scoped and not `stated` (`BelongsToDecl.EngineManaged`): those
are the concepts the stamp fills and the assignment walks.

**Checked at load**

| Check | Where | Refusal |
|---|---|---|
| Both `account` and `cluster`, or neither; `shared`, `stated`, `via` or `parent` beside `cluster`; `stated` or `shared` beside `via`; `parent` without `via` | the annotation | `belongs_to_conflict` |
| An account form on a concept with no `accountId` field, or one that is not a string | the concept | `belongs_to_field` |
| `via` is empty or names a field the concept does not declare | the concept | `belongs_to_via_unknown` |
| A concept shares with an account through `@rowAuthz(account=...)` and declares `cluster` | the concept | `belongs_to_sharing_mismatch` |
| `parent=` is empty or names a concept that is not registered | all concepts | `belongs_to_parent_unknown` |
| `via` resolves to a concept that declares `cluster` | all concepts | `belongs_to_via_cluster` |
| The `via` declarations form a cycle, a concept that follows itself included | all concepts | `belongs_to_via_cycle` |

The last three land on the load report as strict-boot problems, so a bundle is
held to them as the engine's tree is. A parent that has declared nothing is
accepted: it is a declaration still to be written, usually in another repository.

A `via` field with no relationship and no `parent=` is legal: it holds canonical
ids of several concepts, and the engine resolves the concept from the id at write
time. `artifact.sourceConceptRef` is the case that needs it.

**No implied reference (R7).** The declaration does not make `accountId` a
relationship. The stamp writes the bare id, as the owner stamp does, and runs
before `canonicalizeRelationshipFields`, so a concept that declares an authored
`@relationship` on the field stores the canonical spelling and every other
concept stores the bare one. Every reader of the field matches both spellings.

**Readers go through the declaration.** The inventory, the assignment job and later
export never read `accountId` off an arbitrary row. They iterate the concepts that
declare an account form. A cluster-wide concept that happens to carry a field of
that name with another meaning (`v1:identity:accountEntitlement.accountId` is a user
id) is never consulted.

**Concepts that are never stored in the row table** (virtual projections, and
concepts backed by their own table such as `siteTraffic`) still declare. The
declaration describes what a read is scoped by; there is nothing to stamp.

**The gate (D12, as changed by R3 and R17)**

- **Engine tree, packs and the fleet bundle.** `TestEveryShippedConceptDeclaresBelongsTo`
  (`test/dslconformance/belongs_to_inventory_test.go`) walks `dsl/`, `packs/` and
  `deploy/fleet/dsl` and fails the build on a concept with no `@belongsTo`, naming
  every offender at once. Nothing refuses at load: tests register some 740
  concepts inline the way a pack does, and a refusal would fail all of them.
- **Product bundles** mounted at `MEMQL_DSL_PATH`. An undeclared concept loads.
  Each load writes one warning naming all of them and what to write, the gauge
  `memql_dsl_belongs_to_undeclared` carries their count, and `memqllint` prints a
  warning per concept with its file and line without failing. Their rows count as
  cluster-wide until they declare. The deprecation-window entry that would later
  turn this into a load refusal is left to the export piece.
- **Runtime-promoted concepts** are not checked in this piece.

## C. How a row gets its account (D4, D5, D6)

The stamp runs in `executeWrite` after the owner stamp and the organization rules,
for every ENGINE-MANAGED concept, on every surface: named mutations, the raw
`insert(` literal, staged writes, automations and connector writes
(`component/memql/account_stamp.go`). It returns before reading anything for a
`stated` concept, and it does nothing until the rollout switch in section E is on.

**The sources, in order**

1. **The row's own list**, for the five concepts that still carry `accountIds`
   (section G): the call's account when it is in the list, otherwise the first
   entry. It decides on a create, on a row that has no account yet, and when a
   write actually changes the list. A write that repeats the same list moves
   nothing. The list is the caller's own input on every surface, so for a
   person's write the choice is made only among the tags that person may write
   to (a cluster operator: any); a tag naming an account they do not belong to
   files nothing there, and with no tag left the list decides nothing.
2. **A value already on the row.** Kept: the stored account on an update, or an
   explicit choice on a create.
3. **The parent.** For a `via` concept the engine reads the row the field names,
   straight from the store. A parent with an account gives it to the child. A
   parent that exists with no account makes the child WAIT: it is written without
   one and the assignment in section F follows the parent later. A field that is
   empty, or names a row that does not exist, falls through.
4. **The call's account**, on a create only.
5. **Nothing.** The row is written without an account and the assignment gives it
   one on its next pass. There is no default to the primary account at write time
   (R5): a stamp never changes, so a guess would be wrong for good.

**The call's account** is a context key that an entry point states
(`auth.ContextWithCallAccount`), and otherwise it is resolved lazily from the actor
(`(*MemQLEngine).CallAccount`, `component/memql/call_account.go`). It is not a field
of `auth.AccessContext`: about a hundred call sites rebuild that struct beneath one
call and would drop it. It is never an authorization input.

| Who is calling | The call's account |
|---|---|
| A cluster operator (an unscoped role at admin rank or above) | The primary account |
| A person with an account-scoped role | That role's account |
| Any other person | The account they belong to; the oldest membership of a person in several, dated by the first version of the membership row; the primary account for a person in none. A person the principal table cannot resolve, and a read that failed, give none |
| An automation | The account of the row that triggered it, when the row has one and something stands behind the value: the concept is engine-managed, or it is one of the organization-owned concepts whose account is held to its writer's on every write (`memql.TriggerAccountTrusted`). A row of any other `stated` concept places nothing, because its own mutations wrote the value unchecked. The account is kept in the run's recorded trigger, so a resumed or adopted run acts in the same one. A scheduled automation has none; a sub-automation keeps its caller's |
| A shopper form or access-code visit | The site's account, from the site row the bff already re-reads under the owner |
| A forwarded request | Whatever the originating node stated, carried on `ForwardedAuthority.account_id` |
| A connector, a seed, a sweep, a system write | None |

**Rules that hold**

- **An update never fills in an account from the caller.** An update of a row that
  has none is not handed to whoever edited it. Only its parent can fill it.
- **An account named by the writer is held to the writer's accounts.** That is a
  write that did not come from a mutation template and carries no internal
  origin, and also a person's write through a template, because a template that
  accepts `accountId` (a note's generic update does) passes its caller's value
  through. Such a write is refused when it names a different account from the
  stored one (`account_change_refused`), an account the caller is not a member of
  (`organization_forbidden`), or an account other than its parent's
  (`account_mismatch`). Server code that names an account is an author's decision
  and is left to the author.
- **A person's raw write does not borrow a parent's account.** A raw write names
  its parent with nothing but an id, and the stamp reads that parent asking
  nobody's permission. So on that surface a person's row takes the parent's
  account only when the person may write to that account; otherwise the row is
  recorded under their own, without a refusal. A mutation template that takes a
  parent id is its author's decision and follows the parent, and so does server
  code.
- **An empty string is no account** (R6), in a write and in a stored row.
- **The account travels across nodes.** `ForwardedAuthority` carries `account_id`
  when an entry point stated one. A node that handles a forwarded request uses it,
  and resolves the actor itself, over the same database, when none was stated.
- **The seam for sign-in.** The first row of the table is the only thing the
  sign-in piece replaces: "the account they belong to" becomes "the account they
  signed in to".

## D. Cluster-provided rows (D7)

**Added while writing; not yet seen by the owner.** Classifying the concepts showed
that some hold both kinds of row: the cluster's own, offered to every account, and
an account's.

- A seeded skill is the cluster's, and a skill a person authors is their account's.
- A role with no account is a cluster-wide role, and a scoped role is its account's.
- A suppression with no account is a cluster-wide safety block that has to keep
  protecting every account.

`@belongsTo(account, shared)` says so. For a `shared` concept:

- **A person's write is stamped like any other**, by the sources in section C.
- **A write with no source keeps no account.** That is what makes it
  cluster-provided: the seed materializer, a sweep, or a governed builtin an owner
  uses to create a cluster-wide row on purpose (a global role).
- **Existing rows with nothing to go on stay cluster-provided** instead of landing
  in the primary account (section F, rule 5).
- **There is no way to ask for it.** An empty account in a write is the same as
  no key (R6), on a `shared` concept as on any other, so a cluster-provided row
  of an engine-managed concept is only ever one that no source decided.

Six of the ten `shared` concepts are also `stated` (a role, a site, a knowledge
domain, a suppression, the two email transport concepts): their own rules already
say what a row with no account means, and the engine leaves them alone.

What a cluster-provided row means for a reader inside an account is the sign-in
piece's question. The intent recorded here: readable in every account, changeable
only from the primary account.

## E. The rollout switch (D10)

A deployment rolls one node at a time, so old and new nodes run side by side for a
few minutes. A new node that stamped `accountId` would write a key an old node's
schema does not declare, and the old node would refuse to update that row.

The switch is the engine's existing reader handshake (R1), the one the liveness
and observation stores use, on two plain tables of its own
(`component/database/memory-nodes/account_stamping.go`, migration
`20261112100000_account_ownership`).

- **Each node advertises.** While the switch is off, every node upserts its own row
  in `account_stamping_readers` every five seconds. A node of the previous release
  never does, and that is what holds the switch during a rollout.
- **Turning on.** The one row of `account_stamping_storage` flips to enabled once
  every `v1:cluster:node` whose latest health is not `stopped` has advertised
  within 30 seconds. The fleet is scanned at most once per five seconds for the
  whole cluster.
- **One way.** Nothing turns it off: rows carry the field from then on.
- **Each node keeps a copy.** `AccountOwnershipRunner`
  (`component/memql/account_ownership_runner.go`) runs on every node type, reads
  the switch once before the node reports ready, then every five seconds, sets the
  engine's flag when it is on, and behind it runs one batch of the assignment per
  tick. A replica can lag the one that turned the switch on by five seconds; a row
  it writes unstamped in that window is found by the assignment.
- **Before it turns on** the engine neither stamps nor assigns, and it writes the
  field for no one: an account NAMED in a write to an engine-managed concept is
  dropped, whoever named it, and the row keeps what it had stored. A value
  stored then would have met none of the checks in section C and would be
  believed once the switch was on, and the previous release could not update a
  row that carries the key. A held node stays in this state for good.

**The hold (R2).** `MEMQL_ACCOUNT_STAMPING_ACTIVATE=false`, read once at startup. A
held node still advertises, never turns the switch on, and neither stamps nor
assigns even after another replica has. It keeps the way back to the previous
release open only while it is set on EVERY replica. Once stamping is on, an older
engine cannot update a stamped row, so the release cannot be rolled back; the
first production deploy is held for that reason, and releasing it is the owner's
decision after a backup.

A release is not recorded on the node's own row instead, because nodes write each
other's rows and a node of the previous release would refuse a row carrying a key
it does not declare.

A cluster on one release, which is every fresh install and every local cluster,
turns the switch on within seconds of boot.

## F. Existing rows (D8)

New rows are stamped once the switch is on. This section assigns everything
written before, and keeps finding rows written unstamped after (a replica that
lagged the switch, a child that waited for its parent, a path that bypassed the
write path). It is `runAccountAssignmentBatch`
(`component/memql/account_assignment.go`), called by the runner of section E on
every node that is not held.

**What it touches.** Engine-managed concepts only, in `engineManagedOrder`:
parents before the rows that follow them, so a goal is assigned before its runs
and a run before its steps. It never touches a `stated` concept or a cluster-wide
one. A row is its newest version, and it has no account when that version's
`accountId` is absent or empty.

**The rules, first match wins**

1. **Tagged for several accounts** (the concept still declares `accountIds` and
   the row's list is not empty): among the tags the row's owner may place a row
   in (every tag for a cluster operator, the owner's own accounts otherwise), the
   owner's own account if it is one of them, otherwise the first. With no such
   tag the list decides nothing. A row whose owner cannot be resolved has its
   tags taken as written.
2. **Has a parent that exists:** the parent's account. A parent that exists with
   no account leaves the row unassigned, and it follows the parent on a later
   pass. For a `shared` or `stated` parent with no account that is its final state.
3. **A Shopify store:** the account of the site whose `binding` or
   `previewBinding` names it. Mirror rows and pack rows follow their store by
   rule 2.
4. **Has an owner:** the owner's account, by the person rows of the table in
   section C, through the same function the write path uses.
5. **Nothing to go on:** the primary account. For a `shared` concept the row
   stays cluster-provided instead.

One conversion runs first. Rows of `v1:shopify:channel` written before this piece
hold Shopify's own value in `accountId`. A value there that is not the id of one
of this cluster's accounts is moved to `sourceAccountId`, or simply dropped when
a later sync has already written `sourceAccountId`, and the row is then judged
as having no account. That relies on account rows never being hard-deleted.

**Never guessed.** Two cases are recorded in `account_assignment_conflict` and
left unassigned until an owner places them:

- `store_bound_twice`: a store bound by sites of two different accounts, along
  with everything that follows it.
- `library_fence_refused`: a Library row the retirement fence
  (`pipeline_library_retirement_fence`) does not allow to change.

Unassigned rows behave as they do today. Export of an account they touch waits
until an owner places them.

**How it runs**

- **In place, newest version only (R10).** The account is set on the one
  `(id, "createdAt")` row and on the row's copy in `memory_liveness` when it has
  one. No version is appended, so no event fires and no automation runs. Older
  versions are not rewritten: rewriting every version of every row would roughly
  double the table until it is vacuumed, and a row is read by its newest version.
- **Bounded batches.** A call claims one concept's row of
  `account_assignment_progress` with `FOR UPDATE SKIP LOCKED`, handles a batch of
  its rows in that same transaction and advances the cursor there, so a crash
  loses only its own batch and any replica resumes. One call examines at most 500
  rows in all and may move on to further concepts for about two seconds.
- **A batch that cannot finish gets smaller.** Each concept has its own batch
  size, 500 at first. A batch that fails, a deadline or a statement timeout
  included (a concept with large payloads rewrites a lot of data), halves it and
  sets the concept aside for 30 seconds so the others go on; a batch that
  succeeds grows it back. Without this one slow concept would hold up every
  concept that follows it.
- **Parents first, on every replica.** A concept's first pass does not start
  until the concept its rows follow has finished its own.
- **Continuous, and cheap once caught up.** The first pass walks every row of a
  concept. Later passes look only at the versions written since the previous
  pass STARTED (less a minute, for a write that committed late), at most once a
  minute per concept. A concept is walked in full again only while its last full
  pass left rows waiting on a parent, and then no more than once a day; a
  concept with nothing waiting is never re-walked. What that does not catch, and
  is accepted: a row whose commit landed more than a minute late on a concept
  with nothing waiting.
- **Deleted rows are skipped,** and a deleted parent counts as no parent.
- **Idempotent.** Running it twice changes nothing: the write itself is guarded
  on the row still having no account.
- **Cache.** A pass that changed a concept's rows invalidates that concept's
  cached reads.
- **Counted** on `memql_account_assignment_rows_total{outcome}`: `assigned`,
  `kept`, `shared`, `conflict`.

**The index. Added while writing.** The inventory, and later export and
separation, look rows up by account. One expression index serves all three:

```sql
CREATE INDEX IF NOT EXISTS idx_memorynodes_account
  ON "MemoryNodes" ((payload->>'accountId'), concept)
  WHERE (payload->>'accountId') IS NOT NULL AND (payload->>'accountId') <> '';
```

It is a Go migration built with `ensureNamedWorkIndex`, the way the existing
node-table indexes are.

## G. One account per row (D9)

Library items, work goals, compositions, compose templates and recipes take the
list `accountIds` today.

**In this push**

- Each gains the single `accountId` and its `@belongsTo` declaration.
- `accountId` is derived from the list on every write that carries one: the call's
  account when it is listed, otherwise the first entry.
- Their existing rows are converted by rule 1 of section F.
- Their mutations keep accepting a list (R12), because the editor extension and
  the SDKs send one. The two multi-select pickers in the OS, on a file and on a
  new goal, become a single choice that sends a one-element list.
- The list stays declared, and `@rowAuthz(account="accountIds")` keeps reading it,
  so nothing about who can see these rows changes.

**In the export piece's push**

Changed on 2026-11-12 by the
[account export design](2026-11-12-account-export-design.md), E7 and section J:
the list is narrowed to the row's own account and `@rowAuthz` keeps reading it,
because sharing by `accountId` would open every one of these rows to all members
of its account. Dropping the list goes to the sign-in piece, and
`campaignAccountId` is dropped one push after its writers stop. What follows is
the plan as it stood.

- `@rowAuthz` moves to `accountId`.
- `accountIds` is stripped from every stored version by a migration scoped by
  concept, and recorded in the `retired` ledger of
  `component/conceptfields/concept-fields.snapshot.json`.
- `campaigns.sendJob.campaignAccountId` is retired the same way; `sendJob` gains
  `accountId` now.

The split is forced by the rule that a field cannot be removed in the release that
still holds rows carrying it: those rows stop being writable.

## H. The inventory and the Accounts panel (D11)

Both reads are builtins of a small plug-in, `integrations/accounts`, registered on
every node type and declared in `dsl/accounts/builtins.memql`. Both are `@sdk`,
read-only and never cached, and both need `execute app:accounts/inventory` (R11),
a part seeded on owner and developer. They return counts and never a row.

**The read.** `accountInventory { accountId }` returns one line per concept that
holds rows for the account: the concept, its namespace and name, the number of
rows (distinct ids) and the number of versions, ordered by namespace then name,
plus one line with the total bytes of the account's current files. It is one
grouped count over the index in section F that matches both spellings of the id
and repeats the index's predicate. Only concepts that declare an account form are
counted, so a cluster-wide concept with a field of the same name is left out. An
unknown account is refused, and a count that would run past its deadline is
refused in plain words instead of surfacing a timeout.

**The status.** `accountAssignmentStatus { }` returns the switch (whether it is on
for the cluster, and whether the node that answered is held), the progress of
each concept, the rows per account, and the open conflicts, newest first, at most
200. The rows per account are returned only once the switch is on, and are left
out when counting them takes too long, so the status itself never fails on a
large table.

**The screen.** In `clients/os/src/apps/accounts/AccountDetail.tsx`:

- The "What is theirs" ledger, six bands from six queries before, is the
  account's inventory by area from the one read. It is on demand, with the time
  it was read and a re-read. An area opens to its concepts and their counts. The
  People band stays as it was, live, above it.
- An "Existing data" panel, on the primary account's page only, shows the
  assignment one state at a time: held, waiting for every node, running,
  finished with the rows per account, and what needs a decision.
- Both are built with the Fleet machine page's components and are absent, not
  disabled, for someone who does not hold the part.

The account page as a whole moves to the tabbed layout in the export piece, where
it gains its Export tab.

## I. The classification

Every concept in the engine tree, its packs and the one product bundle. `account`
means `@belongsTo(account)`; `via F` means `@belongsTo(account, via="F")`.

**Two readings run through the table**

- **A person's own machines and connections are cluster-wide.** A machine is the
  same machine whichever account its owner is signed in to, and a person's GitHub
  connection is theirs everywhere. People are cluster-wide, so these are too.
- **What a person makes is their account's.** Notes, files, goals, conversations.

| Namespace | Concepts | Declares | Why |
|---|---|---|---|
| accounts | account | cluster | The registry. An account row is the account |
| actions | action | account | A person's recorded, reusable action |
| actions | surface | cluster | An execution surface on a person's machine |
| agents | agent, agentRole | account, shared | Seeded templates and roles are the cluster's; one a person makes is their account's |
| agents | agentAuthorization, operatorMemory | account | A person's standing permission and notes |
| agents | skillChangeEvent | via targetAgentId | Audit of one agent |
| authoring | bundle | account | Constructs authored from one person's directive |
| authoring | construct, dependencyEdge | via bundleId | Parts of a bundle |
| authoring | ladderPolicy | cluster | One row of cluster-wide values |
| bench | run, sample | cluster | The proving suite's results for the deployment |
| calendar | calendarEvent, bookingHours | account | A person's calendar and published hours |
| calendar | booking | via hoursId | Taken against published hours |
| campaigns | audience, recipient, template, emailLayout, brandKit, senderIdentity, campaign, delivery, engagementEvent, consentEvent, emailRule, singleSendReceipt, campaignSeries, newsletterBinding, newsletterSignup, testSettings, journey, journeyEnrollment | account, stated | Carry `accountId` today, under the organization rules |
| campaigns | sendJob | via campaignId | The job that sends one campaign. Gains `accountId` |
| campaigns | suppression | account, shared, stated | A row with no account is a cluster-wide safety block |
| campaigns | reputationWindow, warmupState | cluster | Sending-reputation machinery, per sending identity |
| cluster | cluster, database, identityProvider, node, nodeType, spawnEvent, releaseCut | cluster | The deployment itself |
| commerce | productContent, customerNote, companyLocationNote, creditLimit, quote, approvalChain, salesRep, territory, reorderList | via storeId, parent `v1:shopify:store` | Authored for one store |
| common | attachment | account | A file attached to someone's work |
| compose | composition, composeTemplate, recipe | account | Gain the single field beside their list (section G) |
| compose | compositionInput | via compositionId | The saved request of one composition |
| data | record, policy | account | Validated records and the policy for a record type |
| data | log | via recordId | Audit of one record |
| deployment | deployment, deploymentNodeSpec | cluster | What the cluster runs |
| email | capturedMessage | cluster | The cluster's test inbox |
| email | connectionState, sendOperation | account, shared, stated | An account's transport state and receipts; the cluster's own sign-in row has no account |
| forge | project | account | A codebase a team develops against |
| forge | request | via projectId | A request against a project |
| forge | requestEvent | via requestId, parent `v1:forge:request` | A request's history |
| healing | healedOverride | account | A person's override of a construct |
| identity | user, identity, authSession, authCode, deviceCode, oauthClient, magicLinkRequest, enrolmentToken, workerPairingCode, githubConnectState, accessRequest, auditEvent, authActivity, clusterSettings, delegation, account, accountEntitlement | cluster | People, their sign-in, and the sign-in service. `account` here is the older customer record, not `v1:accounts:account` |
| identity | group, groupMembership, invitation | account, stated | A group grants one account, a membership copies it, an invitation asks someone into one |
| install | installRun | cluster | One install on one machine |
| knowledge | knowledgeDomain | account, shared, stated | An untagged domain is the cluster's shared knowledge |
| knowledge | documentChunk | via domainId, parent `v1:knowledge:knowledgeDomain` | One chunk of a domain |
| library | artifact | via sourceConceptRef | The index row follows the row it indexes; the id names the concept |
| library | file, folder, uploadSession, watchedFolder, generatedOutput, memory | account | A person's files and what the assistant keeps for them |
| library | fileVersion, fileChunk | via fileId | Versions and text of one file |
| library | documentComment | via artifactId | Feedback on one document |
| library | documentVersion | via documentId | One version of one document |
| memory | belief, consolidationCursor, workAcknowledgement, conversationEvidence | account | What the assistant has learned about a person's work |
| models | modelProfile, catalogResearchSettings, catalogReview | cluster | The model catalog |
| notes | note | account | A person's note |
| observability | codeProfile, invocation, codeMetric, logLine | cluster | The cluster's own telemetry |
| observability | siteTraffic | via siteId, parent `v1:platform:site` | A site's request counts. Table-backed |
| os | desktop, askConversation | account | A person's desktop and conversations in one account |
| os | attentionReceipt | cluster | Acknowledging a change once covers the person everywhere |
| pipelines | pipeline, run, channel | account, stated | Carry `accountId` today |
| planner | responsibility | account | A person's standing directive |
| platform | site | account, shared, stated | The seeded platform sites are the cluster's |
| platform | package, packageDeployment, accountFrontDoor, customDomain, siteAccessCode | account, stated | Carry `accountId` today: a deployable source and its runs, an account's reserved name, a site's domain and access codes |
| platform | sitePreviewGrant, sitePreviewObservation, siteAccessSession | via siteId | Belong to one site |
| platform | siteHealth | via siteId, parent `v1:platform:site` | A site's health. Table-backed |
| platform | outboxEntry | via rowRef | A pending delivery of one row; the id names the concept |
| platform | globalSecret, globalVariable, partitionSecret, partitionVariable, missingCapability, outboundRequest, inboundRequest, packState, embedderBinding, moduleReadiness, modelMeasurement, modelEvidence, syncState | cluster | Cluster configuration, queues and measurements |
| platform | sourceCredential, sourceConnection, externalConnection | cluster | A person's own connections |
| platform | dataOrigin, automationNode, automationLoopStop, providerAuthStatus, fleetModel, inferenceStatus, providersReloadResult, providerVerifyResult, providerConfigResult, moduleVerdict, readinessRecomputeResult | cluster | Virtual, never stored |
| rbac | role | account, shared, stated | A role with no account is cluster-wide; a scoped role is its account's |
| rbac | capability, grant | cluster | Cluster-wide for now (R14) |
| router | budget, call, route, modelCatalog, policyCatalog | cluster | Routing configuration, decision records, and a person's own routes |
| safety | classification, approvalRequest, outputScreening | account | A decision about work done in one account |
| skills | skill, skillEdge | account, shared | Seeded skills are the cluster's |
| todos | todo | account | A person's to-do |
| work | goal | account | Gains the single field beside its list (section G) |
| work | run | via goalId | An automation's run has no goal and takes the call's account |
| work | step, modelCall, approval, observation | via runId | Parts of one run |
| work | feedbackPolicy | cluster | One row of cluster-wide values |
| workbench | workspace, executionBinding | via runId | One run's workspace and placement |
| worker | invocation, registration, appSession, delegationPolicy, routingPolicy, modelPull, appSignIn, modelProbe, activity, energyProfile | cluster | A person's machines and what ran on them |
| shopify | store | account | Takes the account of the storefront it is connected for |
| shopify | complianceJob | via storeId, parent `v1:shopify:store` | A privacy request for one store |
| shopify (generated) | all 65 mirror concepts | via storeId, parent `v1:shopify:store` | Emitted by `cmd/shopifyschema` |
| pack: reviews | review, reviewDecision, reviewRequest, reviewSettings | account, stated | Already copy the account from the site a review arrived through |
| pack: reviews | moderationAction | via reviewId, parent `v1:reviews:review` | A decision about one review |
| pack: wholesale | application, wholesaleSettings | via storeId, parent `v1:shopify:store` | Arrive through one store |
| pack: wholesale | applicationDecision, entitlement | via applicationId, parent `v1:wholesale:application` | Follow one application |
| fleet bundle (`deploy/fleet/dsl`) | subscriber | account | A subscriber is one account's |
| fleet bundle | subscription | via subscriberId, parent `v1:fleet:subscriber` | One subscriber's subscription |
| fleet bundle | instance, usageMeter | via subscriptionId, parent `v1:fleet:subscription` | Belong to one subscription |
| fleet bundle | tierSpec | cluster | The catalog of tiers |
| bundle: memql-fylo | applicationDetail | via applicationId, parent `v1:wholesale:application` | Follows the wholesale application it extends. Declared in that repository's own push |

The four example trees (`examples/`) and the two client fixtures under
`packs/wholesalepack/testdata` are declared the same way, so the examples a reader
copies carry the annotation.

**Counts**, across `dsl/`, `packs/`, `deploy/fleet/dsl` and `examples/`: 284
concepts. 89 are cluster-wide. 195 belong to accounts: 39 state their own account
(six of them also `shared`), and 156 are engine-managed, of which 113 follow a
parent (the 65 generated mirror concepts among them), 39 take the call's account
and 4 are `shared`. 91 hand-written concepts gained the `accountId` field, and
the 65 generated ones gained it through the generator.

## J. Refusals and signals

| Signal | When |
|---|---|
| `belongs_to_conflict`, `belongs_to_field`, `belongs_to_via_unknown`, `belongs_to_sharing_mismatch`, `belongs_to_parent_unknown`, `belongs_to_via_cluster`, `belongs_to_via_cycle` | A declaration is refused at load (section B) |
| `account_change_refused` | A raw write names a different account from the stored one |
| `account_mismatch` | A raw write names an account other than its parent's |
| `organization_forbidden` | A raw write names an account the caller is not a member of. The existing code |
| `memql_dsl_belongs_to_undeclared` | Gauge: the mounted concepts that declare no `@belongsTo` |
| `memql_account_assignment_rows_total{outcome}` | The assignment's progress: `assigned`, `kept`, `shared`, `conflict` |
| `account_assignment_conflict` | The table of rows the assignment will not guess at, by kind |

There is no fallback counter: nothing is stamped with the primary account for want
of a source (R5).

## K. Testing

**Build gates**

- Every concept in `dsl/`, `packs/` and `deploy/fleet/dsl` declares `@belongsTo`
  (`TestEveryShippedConceptDeclaresBelongsTo`), and none shares with an account
  while declaring `cluster` (`TestNoConceptSharesWithAnAccountWhileClusterWide`).
- The annotation has its cells in the conformance corpus,
  `test/conformance/2026/cells/concept/belongsTo/` (six accepted, nine refused,
  each refusal with its code and message), and its three cross-concept refusals
  under `2026/negative/concept/`.
- The declarations of the two published examples match their corpus cases and
  the tutorial page.

**Against a real database**

- **Each source** (`component/memql/account_stamp_db_test.go`): a parent with an
  account, a parent without one, a missing parent, a person in one account, a
  cluster operator, a stated context key, and a system write that records none.
- **The rules:** an update keeps its account, a row with none is not handed to
  its editor, a re-insert over an existing id carries the account forward, the
  three raw-surface refusals, an empty string as no account, a `stated` concept
  left exactly as before, a `shared` concept's cluster-provided write, the list
  concepts, both spellings, and the switch off writing nothing.
- **The call's account** (`component/memql/call_account_db_test.go`,
  `component/auth/call_account_test.go`): every row of the table in section C,
  including the scoped role, the oldest membership and a failed read.
- **The run's account** (`component/automations/call_account_test.go`): each
  condition, the caller-supplied refusal, the recorded key, resume and adoption.
- **The hop** (`component/node/account_authority_test.go`,
  `component/grpc/forwarded_call_account_test.go`): a forwarded request carries
  the account the originating node stated. Two halves in one process.
- **The switch** (`component/memql/account_stamping_db_test.go`,
  `account_ownership_runner_test.go`): a fleet with a node that has not advertised
  keeps it off, a uniform fleet turns it on, nothing turns it off, a held node
  advertises and never acts.
- **Assignment** (`component/memql/account_assignment_db_test.go`): a fixture of
  legacy shapes produces an exact expected result; a second run changes nothing;
  a run stopped after its first batch resumes; only the newest version changes;
  no event is published; the index has its definition.
- **The canary sweep** (`component/memql/account_canary_db_test.go`): for every
  engine-managed concept read from the registry, a row written in each of two
  accounts records the account it was written in. A concept added later is
  covered with no change to the test. Export will reuse the sweep to prove that
  nothing leaks.
- **The inventory** (`integrations/accounts`): exact counts for two accounts
  across concepts, both spellings of the id, a cluster-wide concept with a field
  of the same name left out, and the capability gate through the engine.

**The OS.** Tests under `clients/os/test/accounts/` for the inventory panel and
the "Existing data" panel: loading, a settled empty read, a refusal with a retry,
each state of the assignment, and both absent for a caller who does not hold the
part.

## L. Delivery (D13)

One push to `main`, as engine 0.31.35, carrying all of it: the annotation and its
checks, every declaration, the call's account, the stamp, the switch and its
hold, the single field on the five list concepts, the index, the assignment, the
two builtins, the seeded part and the Accounts panels, with the language
reference, the authoring rules, `dsl/_reference/_concept.memql`, the
pack-building guide, `docs/public/operate/auth/organization-ownership.md` and the
root `CLAUDE.md`.

Before the push: the three local lanes against the CI database image on a spare
port (the packages outside the database-gated trees, `component/memql`, and the
other database-gated trees) with `MEMQL_REQUIRE_DB=1`, `make concept-snapshot`,
`cmd/memqllint`, the generated-file checks, and the OS typecheck and tests.

**Production is deployed held.** The instance sets
`MEMQL_ACCOUNT_STAMPING_ACTIVATE=false` on every engine workload before the
release is installed, so the first deploy adds the tables, the index and the
declarations and changes no row. Releasing the hold is the owner's decision,
after a backup and a look at the storage the assignment will need.

Then one push in `memql-fylo`: `applicationDetail` gains `accountId` and its
declaration. It follows the engine release, because an older engine refuses an
annotation it does not know.

## M. Known limits, and what is not verified

**Limits**

- A cluster operator's untagged work for a client lands in the primary account and
  stays behind until someone moves it. Moving arrives with export.
- A person in several accounts has everything they made before this piece assigned
  to their oldest membership.
- A row tagged for several accounts ends up in one, and the others lose it when the
  list is removed.
- On the five list concepts a person can still tag a row they own with an account
  they are not a member of, as they could before, and the tags still decide who
  the row is shared with. The RECORDED account does not follow such a tag: for a
  person's own write, and for an existing row by its owner, it is chosen only
  among the tags that person may write to. What the list may contain is decided
  with its removal in the export piece.
- Only the newest version of an existing row is assigned. A reader that walks a
  row's history, as export will, takes the account from the newest version.
- A child written while its parent has no account waits for the assignment, and a
  child of a parent that waited is found by the daily full pass, so it can be
  without an account for up to a day.
- The inventory counts a row under the account its newest and older versions
  name; rows do not move between accounts in this piece, so that is exact today.

**One row per person.** Three kinds of row exist once per person today and take
their id from the user id: the desktop (`v1:os:desktop`), the per-person rows of
`operatorMemory` and `consolidationCursor`, and per-person seeded agents. In this
piece each is stamped with the person's account like any other row. The sign-in
piece decides which become one per person per account. A desktop does.

**Not verified**

- The stamp and the assignment have not run against a production database. The
  first production deploy is held, so they first run there when the owner
  releases the hold.
- How long the index build takes, and how much the assignment writes, on a
  database the size of the owner's instance.
- A cluster of several nodes: the hop and the switch are tested with two halves
  in one process and against a real fleet table, not on a rolling deployment.
- The Accounts panels are verified by tests and have not been looked at in a
  browser.
- That every `via` field in section I is always set by its writers. Where one is
  not, the fall-through to the call's account in section C applies.

## N. Out of scope

- Any change to what a person can see or do.
- Export, import, and moving a row between accounts.
- Data held outside the row table. Export classifies it.
- Normalizing the stored spelling of `accountId` on `group` and `groupMembership`.
