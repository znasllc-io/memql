# App access grants -- Design (sub-project E of the access program)

- **Date:** 2026-09-11
- **Epics:** filed 2026-09-12 from this record, four of them; the numbers are in the plan
  (`docs/superpowers/plans/2026-09-11-app-access-grants-plan.md`) and in each epic's
  body.
- **Status:** approved in the 2026-09-11 brainstorm. Every fork below was put to the
  owner and answered; the per-section reasoning says what each choice rejected.
- **Program:** extends the access program (`2026-09-07-access-program.md`). Sub-projects
  A (groups and grants) and B (roles as data) are the mechanisms this record builds on; C
  (the Users app) is where the administration surface will eventually live. This record
  adds the one axis the program did not have: a grant whose subject is a person or a
  group rather than a role, over apps and named parts of apps.
- **Scope:** the `v1:rbac:grant` concept and its resolution rule; apps and parts of apps
  as capability resources; the governance and audit of grants; the MemQL OS switch from
  hand-written role floors to one effective-capabilities read; Deployables as the first
  app to carry the vocabulary, with the data-layer half (the account tie on packages);
  and, as a prerequisite epic that does not depend on any of it, the two engine defects
  and the cloud repair that made deployables look broken.
- **Not here:** expiry on a grant; grants over hosted websites (a deployable's own
  visitors); per-action grants below the named-part level; a live subscription to grant
  rows; the full Users-app treatment of the administration surface (record C).

## What prompted this

Two people share the cloud cluster: the owner and a developer. Both deploy. The owner
reported that deployables were not separated or attributed by person, that both should
see each other's, and that a group rather than the creating person should probably own
them. Investigation found that everything he observed came from two engine defects, not
from the ownership model, and then the conversation turned to the model he actually
wants: access to apps by role, by group and by person, and within an app by part.

The four failures and their causes, recorded so the prerequisite epic stands alone:

| Symptom | Cause | Where |
|---|---|---|
| Go live refused: "row is not this caller's to write" | `createSite` stamps the deployer as owner; the very next write, `recordSitePackageOrigin`, runs under internal origin on the deployer's own context, and `applySiteOwnerStamp` treats that as a deployment writer whose own id was just stamped, so it deletes the owner. The row becomes cluster-owned on its second version. Publish still succeeds because it runs as a synthetic cluster owner | `component/packages/store.go` (`bindSiteToPackage`, `writeInternal`), `component/memql/platform_site_hostname_policy.go` (`applySiteOwnerStamp`), `component/memql/executor_mutation.go`. Fixed by PR #5284 |
| Archiving a source did not free its hostnames | `archivePackage` reads the source's sites under the caller; the owner term hides cluster-owned rows from a developer, so nothing is deleted and the uniqueness probe, which reads `deleted` alone and ignores row authz, keeps the names taken | `component/packages/integration.go` (`handleArchivePackage`), `dsl/platform/queries.memql` (`sitesForPackage`). Same cause |
| "no address yet" in the list, an address on the detail page | The list invents a placeholder row only when no site row for the app is in the caller's feed; after the erasure the deployer cannot read their own site. The detail page reads the address off the deployment row, which the deployer still owns | `clients/os/src/apps/deployables/list.ts`, `page/ComposePage.tsx`. Same cause |
| "deployment ... belongs to a different package" on confirm | `resumeParked` compares the stored `packageId`, canonical `v1:platform:package:<id>` because the field is an outgoing relationship, against the id the client sent, which is bare. Bare-ification happens only at the gRPC edge. The same raw compare sits at retry, cancel and rollback | `component/packages/pipeline.go` (`resumeParked`, `fetchFor`), `integration.go` (cancel), `rollback.go` |

Two secondary defects found beside them: archiving a source does not close its parked
runs, and `packagesByRepoUrl` carries no status filter, so the upstream feed keeps
touching archived packages.

## Locked decisions

| # | Decision | Choice (owner-approved) |
|---|---|---|
| D1 | Sign of a grant | A grant to a person or a group can widen and narrow: `allow` and `deny` at every level, as a role's capabilities already can |
| D2 | Precedence | Most specific wins: person over group over role. Within one level, deny wins. The alternative, deny anywhere wins, was rejected because it makes "give this one person access" fail silently whenever any group they are in says no |
| D3 | The unit | The app, refined by named parts. Not per action, not per construct. Which specific rows a part shows stays with row authorization |
| D4 | Door and contents | An app grant opens the door; row authorization decides the contents. Granting Deployables shows a person the deployables they are admitted to by owner, account or rank. Shared visibility between people comes from tying rows to an account whose group they share, never from the app grant |
| D5 | Governance | A grant is written only by a role holding `update` on `principal`; the writer must hold the capability being granted; the subject ranks no higher than the writer (a group ranks as its highest-ranked active member; an empty group ranks zero); nobody grants to themselves. Only the owner can therefore write a grant whose subject is an owner, and not for themselves, so no grant ever bars the cluster owner |
| D6 | Mechanism | A new `v1:rbac:grant` concept beside the role catalog (approach 2). Roles keep meaning "definition of a role". Widening `v1:rbac:capability` to carry a subject was rejected because it mixes definitions and assignments, which have different lifecycles and caches; per-app access concepts were rejected as N copies of one rule |
| D7 | Resources | An app is the resource type `app:<appId>`, a part is `app:<appId>/<part>`, in the open `resourceType` field the catalog already uses. Opening an app is `read` on `app:<id>`; a part is `execute` on the part. A part exists by being seeded on at least one role, which is also what the load-time check on `@requiresCapability` reads |
| D8 | Where a deny is enforced | On the engine for actions and for app-only reads; on the OS alone for a read shared by several apps, which exposes nothing a deny is meant to hide. `@requiresCapability` stays single-valued |
| D9 | Resolution cost | Group and user grants are resolved per request and memoised on the context beside the account scope. The role catalog stays cached and reloaded on its events. No new cache, no new cross-node reload |
| D10 | The OS | One `effectiveCapabilitiesForActor()` read replaces every `roles: { min }` and `roles: { any }` in the registry with `requires: "app:<id>"` or `requires: "app:<id>/<part>"`. A Go parity gate fails the build when a `requires:` names a resource no seed declares |
| D11 | Freshness | The shell re-reads the effective set on sign-in, on window focus and after a grant write in that browser. The engine honours a grant on the next request regardless, so the lag is a hidden control appearing late, never one that works when it should not |
| D12 | Deployables' data-layer half | `account="accountId"` on `v1:platform:package` and `packageDeployment`, matching `site`; a new deployable is tied to the cluster's own account unless a client is picked. This finishes for deployables the departure from the accounts rule "an account is a record, never a visibility scope" that `site` already made in epic memql#5165 |
| D13 | Custom groups | A group with no account, which today "grants nothing and exists to organize", can be a grant subject: it grants apps and still grants no rows |
| D14 | Order | The two defects and the cloud repair ship first, as their own epic, because they do not depend on a single decision above and they are why deployables looked broken |

## 1. The grant concept and the resolution rule

### The concept

`v1:rbac:grant`, in the `rbac` domain beside `role` and `capability`:

| Field | Meaning |
|---|---|
| `subjectKind` | `user` or `group`. A role is never a subject here; a role's capabilities stay on `v1:rbac:capability` |
| `subjectId` | the `v1:identity:user` or `v1:identity:group` row |
| `verb` | the catalog's five verbs |
| `resourceType` | the catalog's open string, so an app or a part is written the way `deployment` or `construct` is |
| `effect` | `allow` or `deny` |
| `grantedBy` | the user who wrote it |
| `active` | soft-deactivate, as `capability.active`, so history survives |

The row id is derived from `(subjectKind, subjectId, verb, resourceType)`, the way
`groupMembership` derives its id from `(group, user)`; re-granting writes a new version,
revoking writes `active: false`. The concept is `@rowAuthz(clusterOwner, rankFloor="admin")`
for reads, and every mutation is `@serverOnly`, written by a Go builtin under internal
origin, exactly as group and membership rows are. A grant is the deployment's record of a
decision, not anyone's property, and a client-reachable write would be a primitive for
granting yourself an app.

### The resolution rule

One function answers every "may this actor do verb on resource" question, replacing the
role-shaped `auth.Capable(role, verb, resourceType)` at its seven call sites
(`component/memql/requires_capability.go`, both gates; `component/grpc/data_capability_gate.go`,
two reads; `integrations/groups/guards.go`; and the comment in `component/grpc/server.go`
that names the request-path caller):

1. Start from the role catalog: does the actor's role hold `(verb, resourceType)`, with the
   catalog's own deny-wins rule inside that level.
2. Overlay the actor's group grants. Any active group grant on `(verb, resourceType)`
   replaces the role's answer. If the actor's groups disagree, deny wins within the level,
   since no group is more specific than another.
3. Overlay the actor's own grants. An active user grant replaces whatever the previous
   levels said.

Most specific wins across levels, deny wins within a level. An unknown role, or a person
with no grants, resolves exactly as today. `MaintenanceActor`, the seed materializer, an
automation's system actor and borrowed authority are not principals and are not consulted
here, as the rank rules already do not govern them.

### Where the actor's groups come from

`accountScopeFor` already resolves an actor's memberships once per request and memoises
them on the context. The grant resolver reads the same memo rather than issuing a second
membership query, so a request costs one membership read whichever gates it passes. Group
and user grants are read per request from the graph and memoised alongside.

### Deliberately not in the model

No expiry on a grant: nothing today has one, and a stale grant is revoked by a person, not
by a clock. No role as a subject: a role's abilities are edited on the role, and putting
them in two places is how they drift.

## 2. The app and part vocabulary

### Naming

An app is `app:<appId>`, using the OS registry's app id. A part is `app:<appId>/<part>`.
Opening an app is `read` on `app:<id>`; a part is `execute` on `app:<id>/<part>`. The other
three verbs stay legal but unused for apps, so a future need does not require a grammar
change.

### How a part comes to exist

The load-time check on `@requiresCapability` (`knownResources` in
`component/memql/requires_capability.go`) already builds its vocabulary from the resource
types the seeded capability rows name. A part therefore exists by being seeded on at least
one role. Every part must have a default answer for every role anyway (section 5), and
this keeps one source of truth. A construct declaring a misspelled part, say
`app:deployables/publsh` where the seeded part is `app:deployables/publish`, refuses boot
with the known parts in the message, which is the check that exists today for
`deployment` or `construct`.

### Where the annotation goes

On the constructs that belong to exactly one app: its builtins and mutations, and its
reads where the read is the app (Logs, Benchmarks). A read shared by several apps,
`sitesAll` being the example, stays without an app annotation and is governed by row
authorization alone: the annotation is single-valued and "any of these" semantics are not
being added, and a shared read exposes nothing a deny is meant to hide. Stated plainly:
an app deny is enforced on actions and on app-only reads; on a shared read it is enforced
by the OS alone (D8).

### Enforcement

`refuseBelowRequiredCapability` on the direct call and `refusePlanBelowRequiredCapability`
on every plan that expands the construct, both switched from the role-shaped resolver to
the actor-shaped one. No new gate.

### Deployables, the first app to carry the vocabulary

| Resource | Covers |
|---|---|
| `app:deployables` | open the app |
| `app:deployables/sources` | add or edit a source, its credential, its auto-deploy switch |
| `app:deployables/deploy` | analyze, confirm, retry, cancel a run |
| `app:deployables/publish` | go live, pause, roll back |
| `app:deployables/retire` | deactivate an app, archive a source, delete a deployable |
| `app:deployables/domains` | bind or remove a custom domain |

Other apps declare theirs the same way when they are touched. An app with no parts is just
`app:<id>`.

## 3. Governance and audit

### Who may write a grant

Two builtins in the `rbac` domain, `grantSet` (allow or deny, one row) and `grantRevoke`,
`@serverOnly` handlers under `integration.rbac.*` beside `roleCreate` / `roleUpdate` /
`roleDeactivate`, writing under internal origin after these checks in this order:

1. The caller holds `update` on `principal`. Today that is owner and developer; a custom
   role can be given it.
2. The caller holds the capability being granted, resolved through the section 1 rule for
   the caller themselves. Nobody hands out, or denies, an app they do not have.
3. The subject ranks no higher than the caller. For a user subject, the user's rank. For a
   group subject, the rank of its highest-ranked active member, so a developer cannot deny
   an app to a group that contains the owner. An empty group ranks zero.
4. Nobody grants to themselves, mirroring the membership rule that nobody adds themselves
   to a group.

The rank comparison reads the same ladder every other rank decision reads. An
unresolvable rank denies. Each refusal is a typed code the OS prints beside the control,
following the deployables refusal pattern.

### Audit

Every write lands on `v1:identity:auditEvent` with a new `targetType` value `grant` and
actions `grant_set` and `grant_revoked`, `targetId` the grant row, and subject, verb,
resource and effect in `detail`. The identity audit enum contract test walks writers
against the enum, so adding the value is an edit in two places or the build fails.

### Reads

`grantsForSubject(kind, id)` and `grantsForResource(resourceType)` for the administration
screen, floored at admin through `@requiresRank`; and `effectiveCapabilitiesForActor()`,
the resolved set for the caller and nobody else, which the OS reads to decide what to
show. The effective read is the one construct that must not be admin-floored, since every
signed-in person needs their own answer.

## 4. The MemQL OS

### One read replaces the hand-written floors

At sign-in the shell calls `effectiveCapabilitiesForActor()` once and holds the answer
where the actor's role lives today (`clients/os/src/system/roles.ts`). Every place that
asks `roleAdmits(role, { min })` asks instead whether the effective set holds `read` on
`app:<id>`. Registry entries change from `roles: { min: ... }` and `roles: { any: [...] }`
to `requires: "app:<id>"`, and a section or control inside an app declares
`requires: "app:<id>/<part>"`. The registry stops being a second copy of the policy and
becomes a list of resource names.

### A missing part

The OS hides the control. If a call reaches the engine anyway, the engine refuses with
the capability code and the refusal copy pattern prints it beside the control. The OS is
presentation, the engine is the gate, as the registry's own comments already state.

### Freshness

Grant rows are cluster-owner tier and are not broadcast, so a person's shell does not
learn of a grant written for them by somebody else until it re-reads: on sign-in, on
window focus, and after any grant write made in that browser (D11). A live subscription
is deliberately not part of this.

### The parity gate

A Go test in the pattern of `component/memql/site_kind_os_parity_test.go` reads every
`requires:` value out of the OS registry and fails the build when one names a resource no
seeded capability declares.

### Administration surface, minimal

A new Settings section, Access, visible to roles holding `update` on `principal`. Two
views over the same rows: by person or group, a matrix of apps and parts with three states
(inherited from role, allowed, denied, each showing where the answer came from); and by
app, who currently holds it. Writes go through the two builtins and show their refusal
codes. No drag and drop, no bulk edit, no invitations from here.

### Attribution

The Deployables list and source page show "deployed by" from the run's `requestedBy` and
the source's owner, both already on the rows. The Sources settings group lists the
viewer's own credentials first and other people's under an owner-view heading: the
`sourceCredential` concept keeps its `clusterOwner` branch (what an owner sees is metadata,
never a token; dropping the branch would leave a departed person's grant, which auto-deploy
fetches under, with nobody able to see or revoke it), and the presentation is what changes.

## 5. First consumer and migration

### The seeds reproduce today's floors exactly

Every OS app gets a default `read app:<id>` capability on the roles the registry admits
today: the `min: "admin"` apps on owner, developer and admin; the `any: ["owner",
"developer"]` apps on those two; the `min: "writer"` apps on user and above; an app with no
floor on every role, viewer included. The parity gate pins the registry to the seeds, so
the day the switch lands nobody's desktop changes. The five Deployables parts are seeded on
owner and developer only.

### Deployables gets the data-layer half

1. `account="accountId"` on `v1:platform:package` and `packageDeployment`, plus the
   `accountId` field `package` lacks (`packageDeployment` stamps it from the package).
   Additive, so the concept snapshot regenerates without a retirement entry.
2. The compose flow ties a new deployable to the cluster's own account unless a client is
   picked. Both people on the cluster today are in its group by the staff rule, so they see
   each other's from that day, and a person added to that group sees them too.
3. The concept description records the departure from the accounts rule (D12).

### Custom groups

The `group` concept doc gets the sentence from D13.

### The two defects are prerequisites, not part of the model

- Owner erasure: PR #5284 records whether this write's delta named `ownerUserId` and skips
  the undo on an existing row whose delta did not; its unit and database tests pin the
  pipeline's exact context shape and the operator take-back. A one-off repair for the
  cloud, scoped to `v1:platform:site` rows whose owner is empty and whose `packageId` is
  set, re-stamps the owner from the deployment row's `requestedBy`.
- Id spelling: the four raw compares (`resumeParked`, `fetchFor`, cancel, rollback) compare
  through `BareShortId` on both sides, and `run_identity_test.go` gains a case with a bare
  id on the request and a canonical one on the row.
- Beside them: archiving a source closes its `awaiting_confirm` runs as `cancelled`, and
  the upstream feed's `packagesByRepoUrl` read excludes archived packages.

### What the switch retires

`roles: { min }` and `roles: { any }` in the OS registry, and the role-shaped
`auth.Capable` signature. The pre-release rule applies: no shim, both sides change in one
PR.

## 6. Testing

- **Resolution rule** (`component/auth`, pure): role allows and a group denies gives
  deny; role denies and a user allows gives allow; two groups disagree gives deny; a user
  grant beats a group grant; no grants answers exactly what the catalog answers; an unknown
  role with no grants holds nothing; inactive rows are ignored.
- **Governance** (pure): one case per refusal, and the empty-group-ranks-zero case.
- **Enforcement through the engine** (database-gated, `db-tests` lane): a user-role actor
  is refused a Deployables part; the same actor with a user grant succeeds; with a group
  grant succeeds; a group deny over a role allow is refused; a plan that expands a gated
  construct is refused as the direct call is. A role-only resolver passes none of the
  grant cases, which is what proves the seven call sites moved.
- **Vocabulary**: a misspelled part refuses boot naming the known parts; the OS parity gate
  fails on an unseeded `requires:`; the seed-mirror parity test agrees with the registry
  app by app.
- **Cross-node**: a grant written under one node's context is honoured under another's on
  the next request, a two-context test against one database.
- **Audit**: every grant write produces an audit row with the new target type.
- **The prerequisites**: the owner-erasure database test and its negative control (a
  cluster owner's create still lands cluster-owned); bare request against canonical row
  resumes, and the canonical-on-both-sides case stays.
- **The OS**: no registry entry still carries `roles:`; a missing part hides its control;
  an engine refusal prints its copy.

## Delivery

Four epics, in order, each in at most two PRs. The plan document carries the tasks and the
PR split; the epic issues carry the same split in their bodies.

| Epic | Depends on | PRs |
|---|---|---|
| Deployables ownership prerequisites | nothing | 2: PR #5284 (open), then the id compare, the repair and the two feed defects |
| Access grants: the engine | nothing | 2: the concept, the resolver and the call-site switch; then governance, audit and the reads |
| Access grants: apps as resources, Deployables first | the engine epic | 2: the seeds, the Deployables parts and the parity gate; then the account tie |
| Access grants: the OS | the two above | 2: the registry switch and attribution; then the Access settings section |
