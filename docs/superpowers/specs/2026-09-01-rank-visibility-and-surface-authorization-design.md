# Rank Visibility and Surface Authorization -- Design

- **Date:** 2026-09-01
- **Status:** approved (in-session Q&A with the owner; every fork below records
  the choice that was made and why). **DELIVERED** -- memql#4833, #4834, #4836,
  #4837, #4838 and the ordering dependency #4817, in one PR. Both open items in
  section F were settled in implementation and the settlements are recorded
  there.
- **Scope:** the cluster's authorization FOUNDATION, not one app. One
  engine-authoritative role ladder consumed by both the engine and MemQL OS;
  two new `@rowAuthz` visibility rules (rank-visible reads, rank-strict
  writes); a server-side counterpart to the OS's presentation-only surface
  gate; and the seam customer-defined roles arrive through.
- **Prompted by:** the Accounts app (epic memql#4800), whose access question
  -- "a normal user should not reach this app at all" -- has no honest answer
  today, because the only gate available is a launcher filter that its own
  comment calls "UX, not a security boundary".
- **Deliberately NOT here:** the customer-role AUTHORING surface (adding a role
  from inside MemQL OS). This design fixes the model so that surface is
  expressible; building it is its own epic.

## Why

Owner's brief, condensed: *"I wanna enforce it, but I don't want a temporary
solution. I want the actual future-proof solution -- not just for this app but
for any other app. It shouldn't be limited to the UI/UX level, it should be
everything from a security perspective. If you're a user you shouldn't be
accessing other people's data at all. And even within the app itself, a normal
user might have access to an app, but that same app exposes more options if
you're an admin or an owner or a developer."*

Three things are true today and none of them is what that describes.

1. **The OS gate is presentation only, and says so.**
   `clients/os/src/system/roles.ts` gates apps, sections and widgets through
   one predicate, `roleAdmits`, whose own header reads: *"Presentation gating
   only: the engine's row admission stays the authority on every read; hiding
   an app here is UX, not a security boundary."* That is an accurate statement
   of what it does. It means "a normal user cannot access Accounts" is,
   currently, a claim about a launcher.

2. **Row authorization has no notion of rank.** The four tiers are `owner=`,
   `clusterOwner`, `via=` (a relationship spec) and `public`, plus the
   composite `owner=…, clusterOwner`. Over 236 concepts: 89 `clusterOwner`, 23
   owned, 15 composite, 3 owned on `userId`, and the rest declare nothing.
   None of those can say "rows belonging to people below me".

3. **Two role ladders exist and they disagree.**

   | | reader | writer/user | developer | admin | owner |
   |---|---|---|---|---|---|
   | Engine `component/auth/rbac_model.go` | 50 | 100 | **300** | **200** | 400 |
   | OS `clients/os/src/system/roles.ts` | 0 | 1 | **2** | **3** | 4 |

   The engine ranks developer ABOVE admin; the OS ranks admin above developer.
   Today the only symptom is a mis-sorted launcher. Under a model where
   visibility follows rank it is a security bug: the same request gets two
   opposite answers depending on which side answers it.

   The capability sets say why they diverged, and the reason matters:
   **admin holds full `principal` (user-management) verbs and no authoring;
   developer holds full `construct` verbs and no principal management.** They
   are orthogonal, and each ladder ordered them by whichever axis its author
   cared about.

## Locked decisions

| # | Decision | Choice (owner-approved) |
|---|---|---|
| D1 | One ladder | **The engine's ordering wins: developer (300) outranks admin (200).** The OS ladder stops being a TypeScript literal and becomes cluster state the shell reads. Two hand-maintained ladders is the defect; picking a winner without deleting the loser leaves it |
| D2 | Read visibility | **Rank-visible: you see rows owned by anyone at your rank OR BELOW.** Peers included, at every rung. A reader sees only their own; an admin sees admins and below; owners see owners |
| D3 | Write authority | **Rank-strict: you may write rows owned by someone STRICTLY below your rank.** Peer rows are read-only. **This applies at every rung including owner** -- an owner may read another owner's rows and may not modify them |
| D4 | Non-principals | The rank rules govern **PRINCIPALS**. The cluster's own characterised identities -- `MaintenanceActor`, the seed materializer, `ConnectorActor`, borrowed authority via `ContextWithUserActor` -- are not ranked and D3 does not apply to them. Without this, every retention sweep and boot seed becomes a peer-write and stops |
| D5 | Custom roles | Slot into the SAME spaced ladder as cluster rows (`v1:rbac:role`, ranks already spaced 50/100/200/300/400 with the comment "so custom roles can slot in"). A rank plus a capability set; no second ordering, no partial order |
| D6 | Surface gating | The OS's app/section/widget requirement gains a **server-side counterpart** so a hidden surface is also a refused one. The presentation gate is NOT replaced -- both are permanent, and neither is a stand-in for the other |
| D7 | Enforcement site | The existing `@rowAuthz` machinery, extended -- reads, writes AND subscriptions already funnel through it (memql#4309). A parallel mechanism would be a second answer to one question |

## A. What exists today (the ground this builds on)

More is in place than the surface suggests, and the design leans on all of it.

- **Roles are already DSL-defined data.** `dsl/rbac/seeds.memql` authors the
  base roles; `component/auth/rbac_model.go` mirrors them as a rank plus a
  capability set of (verb x resource) pairs -- `read`/`create`/`update`/`delete`
  over `principal`/`construct`/`data`/`deployment`/`agent`/`group`/`role`.
- **The ranks are already spaced for extension**, with the comment saying so.
  D5 is therefore a smaller change than it sounds: the numbers exist, nothing
  reads them from cluster state yet.
- **Multiple owners already work.** `IsClusterOwner()` is `Role == RoleOwner`
  -- a role check with no identity in it -- and
  `last_owner_deletion_validation.go` refuses only reaching ZERO active owners
  ("deleting one of two owners is fine"). Owners already see each other's rows
  through the `clusterOwner` tier. **Nothing needs building for multi-owner**;
  this design must simply not regress it.
- **Per-surface gating already exists in presentation**, at three levels:
  `RoleRequirement` is declarable on an app, on a SECTION, and on a widget
  (`clients/os/src/system/registry.ts`). So "the same app shows an admin more"
  is expressible in the shell today -- it is only unenforced.
- **Row admission already covers subscriptions** (memql#4309): a `graph.node.*`
  event reaches a stream only if the same function that admits the row on a
  read admits it for that stream's actor. Any tier added here inherits that.

## B. The rank rules

### B.1 The predicate

Reads (D2): `rank(roleOf(row.ownerUserId)) <= rank(actor.role)`
Writes (D3): `rank(roleOf(row.ownerUserId)) <  rank(actor.role)`, plus the
existing "your own row" case, which stays unconditional -- everyone may write
what they own.

### B.2 The cost, stated plainly

**This needs the ROW OWNER'S ROLE, which today's gate never looks up.** The
owned tier compiles to a string comparison pushed into SQL
(`payload->>'ownerUserId' = ?`) and `sameRowAuthzOwner` is a string compare in
Go. `rank(roleOf(owner))` is a second lookup per candidate row, and it is not
a static property: **promoting a user retroactively changes who can see their
rows**, which rules out denormalising the rank onto the row at write time.

Resolution: a per-request `userId -> rank` map, resolved once and shared by the
SQL term and the post-filter. A cluster's principal count is small and bounded
(this is an operator's cluster, not a consumer social graph), so one map per
request is affordable where a per-row join is not. The map is built from the
same `v1:identity:user` reads the actor envelope already performs.

**The SQL half must be pushed down, not post-filtered.** A post-filter-only
implementation would silently break pagination: the scan window fills with rows
the gate then drops, and a short page is read as exhaustion by the cursor
logic (`executor_filter.go` documents that trap for the existing gate).

### B.3 What D3 costs, recorded because it is the sharp edge

With peer writes refused at every rung, **no human can repair another human's
rows**. Two consequences follow and only one is acceptable as-is:

- *Admin-to-admin hand-off* now goes one rung up. Deliberate; the owner chose
  visibility-without-write precisely so the audit trail and the hand-off READ
  survive.
- *An owner who leaves the company* leaves rows no living principal can edit.
  D3 makes that permanent, and the previous behaviour (any cluster owner could
  write any row) is what made it a non-problem.

**The answer is ownership TRANSFER, not a write escape**, and it is the one
open item below. A transfer is auditable, names a new owner, and leaves the
rank rules untouched; a break-glass write escape is available to everyone who
can reach it and would hollow out D3 on the day it shipped.

## C. Surface authorization (D6)

The shell's `RoleRequirement` becomes a declaration with two consumers instead
of one.

- **Presentation (unchanged):** `roleAdmits` keeps filtering the launcher, the
  dock, open-by-id, section nav and widget placement. Hiding an action a caller
  cannot take beats letting them click it and reading a refusal.
- **Enforcement (new):** the same requirement, resolved server-side. A surface
  is a set of constructs, so the honest server-side statement is a capability
  requirement on the constructs the surface calls -- not a string called
  "accounts" that the engine would have to trust a client to report.

That is the crux of C and the reason it is not simply "send the app id": an app
id from a browser is a claim, not a fact. The requirement therefore lands where
the engine already decides things -- the construct -- and the app manifest's
`roles` field becomes the presentation MIRROR of a capability the constructs
themselves declare.

## D. Migration

- ~130 tier declarations exist; **none changes meaning.** D2/D3 add tiers, they
  do not redefine `owner=` or `clusterOwner`.
- ~106 concepts declare no tier and admit everyone on both the read and the
  subscription path. That standing long tail is not this epic's to close, but
  it is the reason "a user cannot see other people's data" is not true today
  and will not become true merely by shipping D2 -- **stated so the epic is not
  read as delivering a guarantee it does not**.
- The OS ladder literal is deleted in the same change that makes the ladder
  cluster state (D1). Leaving it as a fallback would preserve the divergence
  under a different name.

## E. Testing

- The ladder has ONE source: a test that fails if the OS ships a hardcoded
  ordering, in the spirit of `TestFleetOnlineWindowMatchesTheClients` (two
  implementations of one rule, pinned to agree).
- Rank rules are proved against a REAL engine and database, per rank pair, for
  read AND write AND subscription -- a green single-actor unit test is the
  false signal this area produces (`a-fake-engine-has-no-gates`).
- The pagination interaction (B.2) gets its own case: a page whose window is
  full of peer-owned rows must not read as exhaustion.
- D4 gets a case per non-principal actor class, because its failure mode is a
  retention sweep that silently retires nothing.

## F. Open decisions -- BOTH SETTLED IN IMPLEMENTATION

| # | Question | Settled as |
|---|---|---|
| O1 | Ownership transfer | **A cluster-owner action, per-user AND per-row, audited on `v1:identity:auditEvent` under a new `rowOwnership` targetType.** Cluster owner because "a rank above both parties" has no answer when both are owners -- the case that motivates the feature. Both scopes because the issue names them as different jobs: offboarding is per-user, a single stuck row is not. Refused when the destination does not exist or is deactivated, because transferring into a void recreates the problem silently -- the rows stay unwritable with an audit trail saying they were handed over. Cluster-owned rows are SKIPPED (the `self` account has no meaningful second owner) and self-owned tiers are excluded entirely (their "owner field" is the row's own id, so a transfer would rename the row). `integrations/identity/ownership_transfer.go` |
| O2 | Where a capability requirement is declared on a construct | **An annotation: `@requiresRank("<role>")`.** The spec form works today and was rejected for one reason -- `dslgate.AdminGateRe` recognises gates BY NAME, so every new spec is a new regex entry and a chance to ship a silent gate. That has already happened twice: `forgeDeveloper` and `forgeApprover` were live authorization conjuncts in production filters the composition rule had never once run on, and they were correctly written by luck rather than as a checked property. The annotation is validated at LOAD, so a typo refuses boot instead of ranking 0 and admitting everyone. `component/memql/requires_rank.go` |

### What implementation changed about the design

Three things were decided at the keyboard and are recorded here because the
text above does not imply them.

- **The rank rules are FLAGS on the owned tier, not new tiers**
  (`rankVisible`, `rankStrict`, `unowned="<role>"`). Same reasoning
  `ClusterOwnerBypass` carries: four sites switch on `Tier == RowAuthzOwned`,
  and a new tier value falls silently out of all four. `rankStrict` requires
  `rankVisible`, because a write rule with no matching read rule grants the
  authority to change a row the same caller cannot see.
- **`unowned="<role>"` was needed and is not in the design above.** D2 alone
  does not deliver memql#4837: an unowned row belongs to the DEPLOYMENT and has
  no owner's rank to compare, so the rank branch refuses it and the self
  account stays cluster-owner-only. The floor says what such a row is worth.
  An unresolvable floor DENIES -- the natural spelling
  (`actorRank >= rankOf(slug)`) reads correctly and fails OPEN, because every
  rank clears 0.
- **`rankStrict` withdraws the cluster-owner WRITE escape**, which section B.3
  implies without saying. "Peer rows are read-only including owner-to-owner"
  cannot be true while a blanket escape returns before any owner is resolved.
  Internal origin and unranked actors keep it (D4).

## F.1 Residuals, recorded rather than closed

Two things an adversarial review surfaced that are LATENT today and become
actionable the moment a concept declares `rankStrict`. **No concept does**, so
neither is reachable; both are written down because "nobody declared it yet" is
a fact about the tree, not a property of the code.

- **D4's flag set is not complete.** `Unranked` / `Synthetic` are set by the
  four documented constructors plus the campaigns drain worker and the fleet
  store (the two other synthetic actors carrying `RoleOwner`). Roughly ten more
  synthetic-actor constructors exist across `component/edge`, `component/mcp`,
  `component/harness`, `component/datasync`, `integrations/customdomain`,
  `integrations/planner` and `app/`, and they set neither flag. Under a
  `rankStrict` concept the `RoleOwner` ones would become peer-writes and their
  sweeps would stop silently -- the exact failure D4 exists to prevent. The
  first concept to declare `rankStrict` has to sweep that set; a gate that
  enumerates synthetic constructors would be the durable answer.
- **Two readers of `v1:rbac:role` disagree about case.**
  `lookupRoleRankBySlug` matches with `strings.EqualFold`; `roleLadder.rankOf`
  is deliberately case-sensitive, matching the shell (whose own test pins
  `"Owner"` as unrankable). Every slug in play is lowercase cluster data, so
  the two never diverge in practice -- but they are two answers to one
  question, which is the shape this epic exists to remove.

## G. Out of scope, and neighbors

- **The role-authoring surface** (adding a custom role from MemQL OS) -- D5
  makes it expressible; building it is its own epic.
- **memql#4817** (the seeded self account is not cluster-owned) is adjacent:
  it is the same question of what an unowned/system-owned row means under a
  rank model, and should be settled before D2 lands on `v1:accounts:account`.
- **The Accounts app's own gating** is the first consumer, not the design: once
  D6 exists, "developer and above" is a declaration rather than a launcher
  filter.
