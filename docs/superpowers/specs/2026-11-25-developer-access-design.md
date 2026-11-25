# A developer does what an owner does, below the owner

Date: 2026-11-25. Requested by José (cluster owner) in a call with Jesús (10:07 AM, Discord); the reading of each
permission against José's words is Jesús's session's, for José to confirm or overrule line by line.

## Requirement

José, on the call (times are minutes into the recording):

- 40:39 "Oye, pero eso ya está built-in. […] Eso de darle accesos a los apps."
- 40:52 "No, pero tú como developer debes de poder hacer eso."
- 40:55 Jesús: "voy a ver si soy capaz. Y si no, te añado en el branch la habilidad de yo poder hacerlo."
  José: "Sí, exactamente."
- 41:01 "Lo único que un developer no debe tener acceso, y creo que todavía we don't get there because right now it's
  just budgets, es básicamente como billing information, obviously owner functionality and information, que pues le
  puedes preguntar a Claude qué. Los permisos obviamente un developer no debe para nada poder ver lo de owners ni los
  files, ni nada, o sea, mantener ese tree pues que hablamos. Pero fuera de ahí un developer debe poder hacer casi
  absolutamente todo lo que hace un owner."
- 41:49 Jesús: a developer should see why a deployment failed and fix it or send José the fix. José: "Sí."

So the rule: a developer does almost everything an owner does. The exceptions are billing (today: budgets), the
owner's own functions and information, and the owners' files. "Mantener ese tree": the rank tree stays, so nobody
reaches a person ranked at or above them.

## What the owner role had that the developer role lacked

Read from `dsl/rbac/seeds.memql` (owner 112 rows, developer 96): seventeen pairs.

| Permission | What it is | Admin had it | Decision |
|---|---|---|---|
| `create` / `update` / `delete` on `principal` | add, edit, re-role, suspend and remove people; write app grants | yes | **given**, bounded by the rank tree |
| `create` / `update` on `group` | create a group, rename it, place and remove members | yes | **given** |
| `create` / `update` / `delete` on `role` | author custom roles | create, update | **given** (delete too, as the owner has) |
| `create` on `agent` | create agents | yes | **given** |
| `read app:settings/pipelines` | the Pipelines readiness item: is the cluster's GitHub App installed and current | no | **given**, read only |
| `read` / `execute app:cluster/capacity` | the background resource budget | no | owner only: budgets are the billing José named |
| `execute app:settings/clusteraccount` | the cluster's vendor account pays for one's model calls | no | owner only: billing |
| `read` / `execute app:cluster/memorysearch` | which model memory search uses | no | owner only: it picks the vendor account the cluster pays from (record 2026-11-23) |
| `read app:cluster/audit` | the audit trail of every person's acts | no | owner only: it shows the owners' acts |
| `read app:cluster/origins` | Settings > Data, the data origins | no | owner only for now: its acts are owner-only in the engine (`component/datasync`), so a developer would open a screen that does nothing |

The first four rows are the gap José described: an admin (rank 200) could do each of them and a developer (rank 300)
could not. Changing a group's apps, the case that started this, is the grant write, whose first guard is `update` on
`principal` (`integrations/rbac/grants.go`).

## Design

1. **The developer role is seeded with the people-work verbs** (`dsl/rbac/seeds.memql`, mirrored in
   `component/auth/rbac_model.go`): every verb on `principal`, `create` and `update` on `group`, `create`, `update`
   and `delete` on `role`, `create` on `agent`, and `read app:settings/pipelines`. Nothing else is seeded.
2. **An owner stays out of reach through the rank tree, not through a missing verb.** `auth.GovernPrincipal` already
   states it: an owner-ranked target is an owner's alone, an owner reaches everyone, anybody reaches themselves, and
   otherwise the caller must strictly outrank the target. A developer therefore acts on admins, members, viewers and
   client roles, and never on an owner or another developer. `auth.MayAssignRole` keeps a developer from naming a
   role at or above developer; the role and grant builtins keep what a developer hands out to a subset of what a
   developer holds.
3. **Prerequisite: the identity-admin writes now apply that tree.** Seven writes checked only that the caller
   manages people at all: edit profile, display name, suspend and reinstate, reset sign-in policy, shared-mailbox
   flag, revoke a personal access token, rotate the recovery key. An admin could do each of them to the owner,
   and rotating the recovery key returns the new key in plain text. Each now calls `GovernPrincipal` on the target
   (`adminops.refuseOutrankedTarget`). This is its own commit and is correct without the rest of this record.
4. **Grants need no change.** `grantSet`'s four guards stay as they are; the developer role now passes the first.
   The other three already bound it: you grant only what you hold, never to a subject ranked above you, never to
   yourself.
5. **MemQL OS follows the catalog.** Users, Roles, Groups and Users > Access read `holds()` and the role catalog, so
   they open for a developer with no change of their own. One hard-coded check moved to the capability: Identity >
   Sign-in policy (`IdentityApp.tsx`), whose engine gate is the same `create` on `principal`.

## What changes for people

- **Developer:** manages people ranked below developer (profile, role up to admin, suspension, tokens, sign-in
  policy), groups and their members, custom roles, agents, and app grants to people and groups ranked no higher
  than developer; edits cluster settings (registration, brand, token lifetimes); revokes node tokens; sets an
  OAuth client's allowed origins; reviews access requests; reads the Pipelines readiness item.
- **Admin:** no longer edits, suspends, renames, resets or revokes a token of an owner, a developer or another admin.
  An admin still does all of it for members, viewers and client roles.
- **Owner:** nothing changes.

## Held as it was, by name

Two gates used `auth.AtLeastAdmin`, which is the `create`-on-`principal` capability and so would have opened with
the seeds. Neither is about managing people, so each now names owner and admin:

- The legacy deploy console (`component/deploycontrol` `authorize`: Rollback, RolloutAction, GetDeploymentStatus).
- Delegating another person's identity (`integrations/identity` `createDelegation`). It compares no ranks: an admin
  can delegate the owner's identity today.

## Not built, for José to decide

Admin passes and developer is refused, by hard-coded role, in hardened paths this record did not touch:

- Node drain (`component/grpc/node_maintenance_handlers.go`).
- Running an automation over the stream, the relay and HTTP (`automation_run_handlers.go`, `run_relay.go`,
  `component/server/server.go`).
- Non-graph subscription kinds, which carry every person's messages and AI streams (`component/grpc/server.go`).
- Personal fields on a raw browse of `v1:identity:user` (`rowauthz_pii_unbound.go`).
- The wildcard delegation scope (`component/identity/delegation_resolver.go`).
- The legacy deploy console and cross-identity delegation above.

Owner only today, and inside "almost everything an owner does" unless José counts them as owner functions:

- Reading other people's sign-in sessions and API-key lists has no rank filter (`sessionsForSubjectAdmin`,
  `patIdentitiesForUser`): an admin, and now a developer, can read an owner's. No token or key value is in them.
- Cluster configuration a developer cannot finish: binding the embedding model and memory search's model are owner
  only, while José's rule for an unconfigured cluster is that owners and developers get in to configure it.
- Release cutting, candidates and publication; rollback and repair; construct promotion; provider reload and
  verification; data-origin acts; retention, log and storage sweeps; the fleet operator reads; disconnecting
  another person's pipeline; the GitHub App registration; row-ownership transfer.
- Client data in the Shopify mirror and commerce concepts (orders, customers, products, payouts), custom domains,
  front doors, secrets, campaign and call operations: all `clusterOwner` rows a developer reads nothing of.
- Files: an owner reads every person's files; a developer reads only their own. Whether the tree should let a
  developer read the files of people ranked below them was not said on the call.

## Tests

- `component/identity/adminops/target_rank_guard_test.go`: each of the seven writes refuses admin and developer on an
  owner, refuses peers, admits a lower target and the caller's own account.
- `component/auth/admission_capability_test.go` `TestADeveloperManagesPeopleBelowItAndNeverAnOwner`: the verbs are
  held, and `GovernPrincipal` and `MayAssignRole` still refuse an owner, a peer and a role at or above developer.
- `component/auth` seed parity (`TestSeedMatchesCompiledMirror`), the migration truth tables, the adminops gate
  tables, the groups guard and the account-group builtin tests state the new rule.
- Not verified here: the change running on a cluster, and the Users > Access screen driven by hand as a developer.
