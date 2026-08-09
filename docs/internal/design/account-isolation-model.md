---
title: The account concept, and whether per-row authz is sufficient for account isolation
audience: internal
status: accepted
area: internal
sinceVersion: 0.9.6
owner: znas
---

# The `account` concept and its isolation model

> **Status: ACCEPTED (memql#3321).** This note records the isolation model for
> `v1:identity:account` **and its limits**. The data layer it describes shipped
> with it (`dsl/identity/{concepts,queries,mutations,shapes}.memql`); the
> operator UI is memql#3322.
>
> Everything asserted here about engine behaviour was read off the code in this
> checkout, not off the operator docs. Several published pages still describe
> `v1:identity:partitionAccess` and partition-as-isolation-boundary as live
> (`docs/public/operate/auth/access-model.md`), and
> `docs/public/operate/auth/per-row-authz-audit.md` still calls `@rowAuthz`
> inert. Both are stale — see "What the docs get wrong" at the bottom. That
> staleness is tracked as memql#3305; this note works around it rather than
> fixing those pages.

---

## 1. The answer, up front

**Is per-row authorization sufficient for real multi-tenancy? No — and it is
much closer than the framing of the question suggests, in one specific shape.**

Precisely:

- **For a single-owner record it is sufficient, and it is enforced by the
  engine rather than by the query author.** A concept that declares
  `@rowAuthz(owner="F")` has `F == actor.userId` ANDed into every read of it
  and every update of an existing row refused when `F` is not the actor. This
  is not a term someone has to remember to type. `account` is exactly this
  shape, which is why the concept is safe to ship today.
- **For anything with more than one legitimate reader it is not sufficient**,
  and the gap is structural rather than an unfinished sweep: the actor envelope
  carries no tenancy dimension, the `granted` tier has no expressible
  predicate, and `owned` and `admin` do not compose. Section 5 states all three
  and section 6 states what would close them.

The operational consequence is a rule, and it is the whole point of this note:

> **An `account` row is safe. Data hung off an `accountId` is not, until §6(a)
> and §6(b) land.**

---

## 2. What an `account` is

| Term | Meaning | Settled? |
|---|---|---|
| `clients/` | the folder of client-side surfaces (SPAs, landing pages, games) | yes |
| `account` | a customer the operator manages — the concept this note is about | yes |
| `oauthClient` | the OAuth-spec meaning; not ours to rename | yes |

`account` sits beside `accountEntitlement` in `dsl/identity/concepts.memql`,
so nothing shipped is renamed. The three "account"-ish things in the tree are
distinct and stay distinct:

- `v1:identity:user` — a **principal**. Logs in, holds credentials, is a
  subject.
- `v1:identity:account` — a **record about a customer**. Holds no credential,
  mints no session, is never a subject.
- `v1:identity:accountEntitlement` — a **billing cap** keyed on a paying
  account id, which in v1 is a `user.id` (`accountKind="user"`). It predates
  this concept and is **not** keyed on `account.id`; unifying them is a real
  question and deliberately not answered here, because `accountEntitlement`'s
  `accountKind` enum already reserves `"org"` for that decision.

---

## 3. The chosen model

Three sentences.

1. An `account` row is **operator-owned**: `ownerUserId` is `@serverSet` and
   stamped from `actor.userId` on every write, and the concept declares
   `@rowAuthz(owner="ownerUserId")`, so the **engine** — not each query author
   — injects `ownerUserId == actor.userId` into every read and refuses every
   update whose target row the actor does not own.
2. Account-scoped **data** (a later issue's problem) is scoped by a **direct,
   server-stamped `accountId` column on each such row**, never by a
   relationship walk.
3. Accounts are **flat** (no `parentAccountId`) and have **no login**.

### 3.1 Why a direct `accountId`, not a relationship walk

A memQL query's `filter` compiles to a single SQL `WHERE` over one concept's
payload. There is no join. So a relationship walk ("this row belongs to a
project, which belongs to an account, which the caller may see") has to run
either as N+1 reads or as an in-process post-filter, and **an in-process
post-filter is not a boundary** — it is a thing that happens after the database
already returned the rows, on a path the engine has more than one of.

The decisive case is graph expansion. `expandGraph` reaches rows through
`@relationship` definitions and **has no filter to AND anything into** — the
read-path enforcement file says so in those words, and it is why row admission
exists as a second mechanism alongside filter injection. A model whose
isolation lives in a walk is a model whose isolation is absent on the one read
path that walks.

A denormalized `accountId` is the only account predicate a row-spec can
express, the only one that pushes down into SQL, and the only one
`@rowAuthz` could ever inject. The cost is the usual denormalization cost —
the column has to be stamped correctly on every write — and §6(a) is what
makes that the engine's job rather than each mutation's.

### 3.2 Why accounts do not nest

A nesting edge (`parentAccountId`) makes the isolation predicate **transitive**:
"the caller may see account A, or any account whose parent chain reaches an
account the caller may see". A transitive predicate is not a filter conjunct.
It is a recursive traversal, which returns us to §3.1's in-process post-filter,
and it does so on the predicate that *is* the boundary rather than on a
convenience read.

The agency-with-sub-clients case is modelled as **N sibling accounts sharing an
operator**. That loses the rollup view ("show me everything under this
agency"), which is a reporting feature and can be recovered later with an
explicit grouping field — a non-transitive, single-hop equality that a filter
*can* express — rather than with a parent pointer.

### 3.3 Why an account has no login

An account is a record the operator's principals act **on**, not a principal
that acts. Giving it a login would mean minting `v1:identity:identity` rows
pointing at a non-user subject, and every gate in the tree resolves authority
through `actor.userId` — the actor envelope's closed field set is
`userId / role / identityId / isClusterOwner`. A customer-facing login, if it
is ever wanted, is a **`v1:identity:user`** whose reach into an account is a
grant row; it is not a field on `account` and not a second kind of subject.

This is also what keeps `primaryContactEmail` honest: it is `@pii` contact
data, and it is never matched against a magic-link or session lookup.

### 3.4 What an "account token" therefore is (added by memql#3322)

§3.3 rules out an account having a login. memql#3322 nevertheless has to ship a
credential surface — "issue, list, revoke tokens per account" is the operator
job the whole page exists for — so the tension has to be resolved out loud
rather than by quietly minting something that contradicts the paragraph above.

**The resolution: an account token is issued TO A USER, ON BEHALF OF an
account.** Its authenticated subject is the operator's `v1:identity:user`; the
account is a **binding** carried on the credential row
(`v1:identity:identity`, `identityType="account_token"`,
`credentials.accountId`). Nothing authenticates as an account. §3.3 holds
unamended.

That leaves the sharper question, and §5.2 answers it: **what does such a
credential authorize?** Two candidates, both rejected:

- *Authenticate as the account.* Ruled out by §3.3.
- *Authenticate as the operator, narrowed to the account.* Ruled out by §5.2.
  The narrowing is not expressible — the resolved actor carries no tenancy
  dimension, so an account predicate can only compare against a caller-supplied
  arg. A credential named for a customer that in fact carries the operator's
  entire authority is **worse than a plain PAT**, because its name understates
  its blast radius, and the audit doc's standing rule is that an apparent gate
  stops an auditor looking.

So the credential **authorizes nothing today, and that is a checked property
rather than an aspiration**: no verifier resolves the `mql_acct_` prefix, no
interceptor admits it, and — the load-bearing absence — `dsl/identity` declares
**no by-`keyHash` lookup** for the family. Every other credential family has
one precisely because an interceptor resolves a presented bearer through it, so
its absence is what keeps this one structurally inert. Both absences are pinned
by tests in `component/identity/accounttoken`.

What ships is custody: mint, show once, digest-only storage, list, revoke,
audit both ends. What is deferred is authorization — and it is deferred onto
**§6(b) specifically**. When `AccessContext` carries a resolved account set,
`credentials.accountId` on each credential row is exactly the value
`actor.accountIds` would be populated from, and §6(a)'s injected `account=`
tier is what would make the binding enforceable. The row is written now so the
data is there when the mechanism is; nothing is claimed for it in the meantime.

The operator-facing statement of all this, including why an account token must
not be handed to a customer, is
`docs/public/operate/auth/account-tokens.md`.

---

## 4. What is actually enforced today (measured, not assumed)

The tier is **not** inert. Read it off these files rather than off the audit
doc:

| Mechanism | File | Covers |
|---|---|---|
| Filter injection | `component/memql/rowauthz_enforce.go` (`enforceRowAuthzOnPlan`) | every read whose plan has a bound concept — the predicate is ANDed at the **root**, so an author's `a \|\| b` becomes `((a) \|\| (b)) && (authz)` |
| Row admission | same file (`rowAuthzAdmits`) | reads with no bound concept (a raw client-supplied query string) **and** graph expansion, which has no filter |
| Anonymous refusal | same file (`refuseRowAuthzWithoutActor`) | a read carrying no caller identity **errors** rather than comparing against `""` and returning rows owned by nobody |
| Write guard | `component/memql/rowauthz_write_guard.go` | `update` / status-flip / a raw `insert(` onto an id that already exists — the engine resolves the target row and refuses when the row's owner is not the actor |
| Create stamping | `component/memql/rowauthz_insert_stamp.go` | the raw-`insert(` create path that bypasses `accept`/`stamp` — the owner field is server-stamped from the declaration |
| Load-time refusal | `@serverSet` on the owner field | a mutation that writes the field from caller args **fails to load** |

Escapes from the write guard are enumerated in one place
(`rowAuthzWriteEscape`): internal origin stamped per-write by an allow-listed
package, and cluster owner. Nothing else.

So for `account` the ownership predicate holds on the read path, the write
path, the create path and the traversal path, and an unauthenticated read fails
closed. That is a genuinely different situation from "the author remembered to
type the conjunct", and the design leans on it deliberately: it is why
`updateAccount` and `archiveAccount` take a caller-supplied `accountId` without
needing `@serverOnly`.

---

## 5. Where per-row authz stops being sufficient

Three limits. Each is a property of the mechanism, not a missing sweep.

### 5.1 `owned` and `admin` do not compose

A concept declares **one** tier. `enforceRowAuthzOnPlan` ANDs the `owned`
predicate **unconditionally** — there is no cluster-owner escape on the read
path (the escape exists only in `rowAuthzAdmits`, the row-admission mechanism
for un-bound reads).

The consequence is sharp: over a concept declaring `owner="ownerUserId"`, an
admin-gated "show me every account in the cluster" query is narrowed to the
admin's **own** accounts and returns a confidently wrong answer. A
cross-operator roll-up is not merely unimplemented; it is **not expressible**
while the tier is declared.

This is why `dsl/identity/queries.memql` ships no such query. Refusing to write
it is the honest option; writing it and having it silently return a subset is
the failure mode this whole area exists to prevent.

### 5.2 The actor envelope has no tenancy dimension

`actor.*` is a closed set: `userId`, `role`, `identityId`, `isClusterOwner`
(plus `now` and allow-listed `config.*`). There is no `actor.accountId` and no
account set on the resolved `AccessContext`.

So an account predicate can only ever compare a payload field against a
**caller-supplied arg** — `filter accountId == args.accountId`. That is
authorization by honour system: the caller names the tenant whose rows they
would like. Every "account isolation" filter written today is that shape, and
no amount of care in writing it changes what it is.

### 5.3 The `granted` tier is declarable but has no predicate to inject

`rowAuthzPredicateExpr` handles `RowAuthzGranted` by injecting a
`SpecReferenceExpression` for the named spec. The tier works. What does not
exist is a spec that can express the relationship.

A spec binds **one** shape XOR concept. A row-spec reads bare payload fields
and is forbidden from reading `actor.*` (epic #2281); the only gateway to the
actor envelope is an `@actor` shape. The predicate a membership grant needs —
`actor.userId ∈ this row's member list` — therefore requires a **mixed**
(`@row` + `@actor`) shape, and **zero mixed shapes exist anywhere in `dsl/`**.
The form is documented; nothing has ever exercised it.

Until one does, `via=` is a tier with no authored instance, and "who else may
see this account" has no declarable answer. Which is the real reason §3 makes
`account` single-owner rather than a preference for simplicity.

### 5.4 Can the conformance test enforce account scoping? **No, and not by a small change**

The issue asks this directly, so here is the measurement rather than an
opinion.

`TestPerRowAuthzClassification` hard-fails a construct only when it lands in
the `flagged` bucket, and reaching that bucket requires
`userScopeFieldRe.MatchString(rowSelectionSurface(body))`. That regex is:

```
(^|[^.\w])(ownerUserId|userId|actorUserId|targetId|createdBy|requestedBy)\b
```

`accountId` is not in it. A query that selects rows **by `accountId` alone**
therefore references no user-scope field, is not flagged, and lands in `other`
— the largest column in the table and explicitly *not a finding*. Deleting the
account predicate from such a query leaves it loading, passing every gate, and
returning every account's rows.

Two further reasons a bare vocabulary extension would not be enough:

- **`@public` is checked ahead of everything else** in the classifier's switch
  and carries no runtime semantics. One `@public` on an account-scoped
  construct silences the classifier for it permanently (§7).
- **The gate reads the authored filter.** Under §5.2 the account predicate is
  `accountId == args.accountId`, and a gate cannot distinguish "scoped to the
  caller's account" from "scoped to whatever account the caller typed",
  because at the DSL level those are the same text.

So: the conformance test **can** be taught to notice an *absent* `accountId`
term (adding `accountId` to the vocabulary and a hard-fail bucket is real
value, and §6(c) asks for it), and it **cannot** be taught that a present one
is a boundary. The second half is the part that matters, and it is not the
test's to fix.

---

## 6. What would be needed

Stated as three concrete items, in dependency order.

**(a) `@rowAuthz` gains an injected `account="<field>"` tier.** The predicate
becomes a property of the concept, injected by `enforceRowAuthzOnPlan` and
guarded by `rowauthz_write_guard.go`, exactly as `owner=` is today. This is
what turns "the author remembered" into "the engine did it" for account
scoping, and it is a small change **given (b)** — the machinery is built,
tested and shipped; what it lacks is something to compare against.

**(b) A tenancy dimension on the resolved actor.** `AccessContext` must carry
the caller's account set, surfaced as `actor.accountId` (or an `in`-testable
`actor.accountIds`), resolved at auth time from a grant row rather than from a
request field. Without this, (a) has no right-hand side and every account
predicate stays caller-supplied. **This is the load-bearing item.** It is also
where the retired partition dimension actually lived — see §8 — so it is a
re-introduction of a *resolved* scope, deliberately on the actor rather than on
the wire.

**(c) A gate with an account vocabulary, and `@public` refused on
account-tiered concepts.** The account analogue of `userScopeFieldRe`, with a
hard-failing bucket; plus a load-time **refusal** (not an acknowledgement) of
`@public` on any concept declaring an account tier. §7 is why the second half
is not optional.

Ordering matters: (b) then (a) then (c). Shipping (a) without (b) declares a
tier whose predicate compares against a caller-supplied value, which is
*worse* than no tier — the audit doc's own standing rule is that a declared
tier stops an auditor looking.

---

## 7. Blast radius of a single mistaken `@public`

Asked directly by the issue; the answer is worse than "one query".

- **Scope: the whole concept, every tenant, every authenticated caller.**
  `@public` carries no runtime semantics. It is a marker the classifier reads,
  and it is matched **before** `owned`, `admin` and `flagged`. So one `@public`
  on an account-scoped read returns every account's rows for that concept to
  anyone with a token — and where the construct is reachable before auth, to
  anyone at all. Under partitions the request envelope still bounded that
  blast; nothing bounds it now.
- **It is also silent going forward.** `@public` **hard-blocks tier
  inference** — the `row-authz` codemod refuses to infer a tier for a concept
  with a `@public` sibling query, and a `public` tier injects nothing by
  declaration. So the mistake does not merely expose data today; it removes the
  concept from the one mechanism that would have closed the hole later, and it
  does so without any signal.
- **Review cannot see it.** `@public` is one token in a preamble and its only
  required companion is a comment. There is no second signature, no runtime
  behaviour change, and nothing that fails.

Hence §6(c): on a concept declaring an account tier, `@public` must be a **load
error**. Everywhere else it stays an acknowledgement.

---

## 8. Partitions are not available, and that is the correction that matters

Isolation between accounts **cannot** use partitions. They were retired in
memql#56. Verified in this checkout rather than taken from the docs:

- `component/grpc/memql.proto` carries `reserved "partition"` in three places
  (three separate messages) plus `reserved "partitions"` — the wire field is
  gone, not deprecated.
- `v1:identity:partitionAccess` does not exist. The only occurrence of the name
  anywhere in `dsl/` is a comment in `dsl/identity/mutations.memql` saying so.
- What survives under the name in `dsl/platform/concepts.memql` is
  `partitionSecret` / `partitionVariable`. Those are **config storage**, not
  tenancy — they hold per-scope secrets and variables and derive nobody's
  visibility.
- The `PartitionACL` middleware the audit doc used to cite is gone; neither the
  symbol nor its directory is in the tree.

So there is no envelope dimension under the DSL any more, and the per-row check
is not defense in depth — it is the only gate. §6(b) is, in effect, the
proposal to bring back a scope dimension **as a resolved property of the actor**
rather than as a field on the request, which is precisely the property the old
partition selector lacked.

### What the docs get wrong (memql#3305)

Do not calibrate against these until they are fixed:

- `docs/public/operate/auth/access-model.md` documents
  `v1:identity:partitionAccess` as a live concept with a field list. It does not
  exist.
- `docs/public/operate/auth/per-row-authz-audit.md` says "**Nothing is
  enforced**" of `@rowAuthz` and describes Phase 1 as inert. Read-path
  enforcement landed as memql#3172 and the write guard as memql#3174; the same
  page's `updateUser` discussion already contradicts its own status section.

---

## 9. The data layer that shipped with this note

`dsl/identity/concepts.memql`

```memql
@displayCard(primary="name", secondary="description", tertiary="primaryContactEmail", status="status")
@rowAuthz(owner="ownerUserId")
concept account { ... }
```

Fields: `ownerUserId` (`@serverSet`, the authz key), `name`, `status`
(`active` / `suspended` / `archived`), `description`, `primaryContactName`
(`@pii`), `primaryContactEmail` (`@pii`), `externalRef`, `archivedAt`,
`updatedAt`. One `@relationship(type="parent", field="ownerUserId",
target=user, direction="outgoing")`.

`dsl/identity/shapes.memql` — `accountFull`.

`dsl/identity/queries.memql` — `accounts` (caller's list, optional status
narrow, sorted + paginated) and `accountById` (single row, guarded). Both
`@actor`, both classified **owned**.

`dsl/identity/mutations.memql` — `createAccount`, `updateAccount`,
`archiveAccount`. All `@actor`; all stamp `ownerUserId` from `actor.userId` and
none accept it.

Three deliberate omissions, so they read as decisions rather than gaps:

- **No cross-operator roll-up query** — §5.1.
- **No `restoreAccount`** — memql#3321 scoped this pass to create / update /
  archive. It is a two-line sibling of `archiveAccount` when a surface needs
  it.
- **No authorization `spec`.** A mixed-shape spec
  (`accountOwnedByActor`) was designed and **rejected**: naming it as the
  filter conjunct would remove the literal `actor.userId` text that
  `TestPerRowAuthzClassification`'s `ownerScopeLeaf` matches on, silently
  reclassifying both reads out of `owned` into `other`. Trading a real
  classification for a nicer-looking filter is the exact shape of decoration
  this codebase rejects. The authorization statement lives where it is
  enforced: `@rowAuthz` on the concept, `@serverSet` on the field, and the
  literal conjunct in each filter.

---

## 10. Related

- memql#3321 — this note and the data layer
- memql#3322 — the operator UI over it, and §3.4's account-token resolution
  (`docs/public/operate/auth/account-tokens.md`)
- memql#3305 — the stale partition documentation §8 works around
- memql#56 — partition removal
- memql#2803 — the ruling that concept-declared row authz is worth building
- memql#3172 / memql#3174 / memql#3175 — read enforcement, write guard, create
  stamping
- memql#3173 — declaring tiers on the remaining undeclared concepts
