# Access Clarity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An operator can see, test and set exactly which apps an organization's people can open, and a store's review moderators are chosen from that organization's people.

**Architecture:** One new engine read answers "what can this person actually open, and why" with the engine's own resolver (role, groups, person grants, scoped-role limits, organization scoping), and the OS Access screen, role testing and the account group page show that answer instead of re-deriving it in TypeScript. Role testing learns custom and organization-scoped roles. The reviews pack gains a read of the people who can moderate a store.

**Tech Stack:** Go (engine), memQL DSL edition 2026, React + TypeScript + vitest (`clients/os`).

**Spec:** the owner's request of 2026-11-06: a Fylo moderator must be a Fylo organization user; Fylo's people may open only Email and Fylo's Deployables; the screens must stop suggesting otherwise. Production facts: account group `acct-<accountId>` holds `allow read app:deployables` and `allow execute app:deployables/reviews`; the moderator's role `fylo-moderator` is scoped to the account (rank 20) and holds only `read data` and `update data`. Code map: Settings > Access re-derives answers in `clients/os/src/apps/settings/access/model.ts` because `effectiveCapabilitiesForActor` answers only the caller (`integrations/rbac/grants.go:192-205`); it ignores `ScopedRoleMayUse` (`component/auth/rbac_assignment.go:262-282`) and organization scoping (`component/memql/organization_ownership.go:205-270`, `component/memql/organization_capabilities.go:24-71`); its blue "Allow" is the act, drawn where the state is read.

## Global Constraints

- The engine is client-agnostic: no organization names or ids in code or tests beyond fixtures.
- Every new read keeps the floor of the reads it replaces: grants and effective access of someone else are `@requiresRank("admin")`, and nobody reads a person who outranks them.
- Role testing keeps every existing safety rule (`component/identity/roletest/service.go:61-170`, `component/grpc/role_test_handlers.go:13-37`): operator owner or developer, fresh lookup, no owner personas, a developer cannot test what they do not hold, one persona per operator and role, 30-minute lifetime, audit on start and end.
- An act that is not legal is absent, not disabled (`clients/os/DESIGN.md` rule 12). No horizontal dividers on touched surfaces (`data-os-dividers="none"`), no chips, badges or toasts in rows, kit pieces only (`RecordList`, `RecordRow`, `Panel`, `Notice`, `AnchoredPicker`, `ActionBar`).
- New `@sdk` builtins regenerate the SDKs (`make sdk-gen`) in the same commit.
- Go DB tests only against `MEMQL_DATABASE_DSN='postgres://memql:memql_dev@127.0.0.1:55432/memql_email_a?sslmode=disable' MEMQL_REQUIRE_DB=1`, never port 5432.
- Commits: `<area>: <sentence>`, explicit paths, message ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. No push.

## Review Focus

1. **A person in two organizations** must see each organization's answer separately, never one merged "can open": Task 1, `TestEffectiveAccessIsPerOrganization`.
2. **An organization-scoped role on a cluster app** (Logs, Settings, Fleet) must read "can't open" even with a group grant that allows it: Task 1, `TestAScopedRoleCannotOpenClusterAppsWhateverItsGroupGrants`.
3. **A role test of an organization role** must end with no membership left behind: Task 3, `TestEndingAnOrganizationRoleTestRemovesItsMembership`.
4. **The removal approver** cannot be set to someone outside the store's organization or to an operator: Task 4, `TestSettingsRefuseAnApproverWhoCannotModerate`.
5. **A developer viewing Access** must not see answers for an owner: Task 1, `TestEffectiveAccessRefusesSomeoneWhoOutranksTheCaller`.

---

### Task 1: The engine answers what someone can open, and why

**Files:**
- Modify: `dsl/rbac/builtins.memql` (new builtin), `integrations/rbac/grants.go` (executor), `component/auth/grant_resolver.go` (expose provenance if needed), `component/memql/organization_capabilities.go` (evaluate for a given subject, not only the caller's context)
- Test: `integrations/rbac/grants_test.go`

**Interfaces:**
- Produces: builtin `effectiveAccessForSubject({ subjectKind: "user" | "group", subjectId: string })`, `@sdk`, `@requiresRank("admin")`, returning rows `{ verb, resourceType, held: bool, source: "role" | "group" | "person" | "scoped-role-limit" | "not-granted", sourceId: string, accountId: string }`.
  - For a user: every app door (`read app:<id>`) and part (`execute app:<id>/<part>`) the capability catalog names. Pairs the engine decides per organization (`organization_capabilities.go` pairs: `read app:email`, `read app:deployables`, `data`, organization deployable parts) get one row per organization the user belongs to, with `accountId`; other pairs one row with `accountId: ""`. A scoped role's barred resources read `held: false, source: "scoped-role-limit"`.
  - For a group: one row per grant the group holds (`source: "group"`, `held` from the effect).

- [ ] **Step 1: Write the failing tests**: `TestEffectiveAccessForAnOrganizationModerator` (fixture: scoped role holding `read data`, `update data`; account group grants `allow read app:deployables`, `allow execute app:deployables/reviews`; expect `read app:deployables` held from `group` in that account, `execute app:deployables/reviews` held from `group`, `read app:email` not held, `read app:logs` not held with `scoped-role-limit`), `TestAScopedRoleCannotOpenClusterAppsWhateverItsGroupGrants`, `TestEffectiveAccessIsPerOrganization`, `TestEffectiveAccessRefusesSomeoneWhoOutranksTheCaller`, `TestEffectiveAccessForAGroupListsItsOwnGrants`.
- [ ] **Step 2:** Run `go test ./integrations/rbac/ -run TestEffectiveAccess -count=1` (DB lane). Expected: FAIL.
- [ ] **Step 3:** Implement with the engine's own resolver, built for the target subject the way `component/identity/roletest/service.go:157` builds one; never re-derive rules in the executor.
- [ ] **Step 4:** Run it, `go test ./integrations/rbac/ ./component/auth/ ./component/memql/ -count=1` (DB lane), `make sdk-gen`, `go build ./...`. Expected: PASS.
- [ ] **Step 5:** Commit: `rbac: an operator can read what someone can open, and why`.

### Task 2: Settings > Access says what can be opened, and Allow looks like a button

**Files:**
- Modify: `clients/os/src/apps/settings/AccessSection.tsx` (:294-370), `clients/os/src/apps/settings/access/useAccess.ts`, `clients/os/src/apps/settings/access/model.ts` (person answers come from the engine read)
- Test: `clients/os/test/settings/access.test.tsx`

**Interfaces:**
- Consumes: `effectiveAccessForSubject` (Task 1).

Decisions:
- A person's rows come from `effectiveAccessForSubject`; the TypeScript resolver stays only for the "By app" view if it still needs it.
- Every row states its answer in words in the answer column: "Can open" or "Can't open", then where it comes from ("from the role Fylo moderator", "from the group Fylo", "granted to this person", "an organization role can't open this", "nothing grants it"), and "in <organization>" for per-organization rows.
- Group rows: "Granted", "Denied" or "Not granted by this group".
- Allow, Deny and "Use role" are kit buttons in a separate act column, labelled with the act, never in the answer position; the blue link style goes.

- [ ] **Step 1: Write the failing tests**: `a person row says can open and where it comes from`, `an organization role on a cluster app says it can't open`, `a per-organization row names the organization`, `group rows say not granted by this group`, `allow is a button in the act column`.
- [ ] **Step 2:** Run `npx vitest run test/settings/access.test.tsx` in `clients/os`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run it and `npm run typecheck`. Expected: PASS.
- [ ] **Step 5:** Commit: `os: Access says what each person can open and why, and its acts are buttons`.

### Task 3: Role testing for custom and organization roles

**Files:**
- Modify: `component/auth/role_testing.go` (:77-86), `component/identity/roletest/service.go` (:157, :217-248, :228, :350-381), `component/grpc/role_test_handlers.go`
- Create: `component/database/memory-nodes/migrations/<timestamp>_role_test_custom_roles.up.sql` and `.down.sql` (replace the `CHECK (role IN (...))` with a non-empty slug check on both tables)
- Modify: `dsl/identity/concepts.memql` (`groupMembership.origin` gains `role-test`), `clients/os/src/apps/users/RoleTestingSection.tsx` (:10, :58)
- Test: `component/auth/role_testing_test.go`, `component/identity/roletest/service_db_test.go`, `clients/os/test/users/roleTesting.test.tsx` (new)

Decisions:
- `MayTestRole` accepts any active role whose rank is strictly below the operator's, besides the four predefined ones; the existing per-capability check (the operator must hold what they test) applies to custom roles too.
- An organization-scoped role's persona gets one membership in that organization's account group, origin `role-test`; `validateRow` allows exactly that membership; ending or replacing the test removes it. The persona's display name is the role's name, with the organization's name for a scoped role. Audit detail carries the role slug and the account id.
- The OS lists the predefined roles as now, then the active custom roles the operator may test, each as "Test as <role name>" and "(<organization>)" for a scoped one.

- [ ] **Step 1: Write the failing tests**: `TestAnOperatorMayTestACustomRoleBelowThem`, `TestAnOperatorMayNotTestARoleAboveOrHoldingWhatTheyDoNot`, `TestAnOrganizationRoleTestIsAMemberOfThatOrganizationOnly`, `TestEndingAnOrganizationRoleTestRemovesItsMembership`, vitest `role testing offers custom and organization roles`.
- [ ] **Step 2:** Run them (DB lane for Go). Expected: FAIL.
- [ ] **Step 3:** Implement, including the migration.
- [ ] **Step 4:** Run them, `go test ./component/identity/... ./component/auth/ ./component/grpc/ -count=1` (DB lane), `go test ./dsl/... -count=1`, `npm run typecheck`. Expected: PASS.
- [ ] **Step 5:** Commit: `identity: role testing covers custom and organization roles`.

### Task 4: The organization's apps on its group page, and moderators chosen from its people

**Files:**
- Modify: `clients/os/src/apps/users/GroupPage.tsx` (:149-155), reuse `useGrantWrites` (`clients/os/src/apps/settings/access/useAccess.ts:245`)
- Modify: `packs/reviewspack/dsl/builtins.memql` (new read), `packs/reviewspack/reads.go`, `packs/reviewspack/settings.go` (:199-252 approver validation)
- Modify: `clients/os/src/apps/deployables/reviews/ReviewSettingsPage.tsx` (:167-175)
- Test: `clients/os/test/users/groups.test.tsx`, `packs/reviewspack/settings_test.go`, `packs/reviewspack/reads_test.go`, `clients/os/test/deployables/reviews.test.tsx`

**Interfaces:**
- Consumes: `effectiveAccessForSubject` (Task 1) for the group's rows.
- Produces: builtin `reviewsPeople({ storeId: string })` returning `{ userId, displayName, email, mayRemove: bool }` for the active members of the store's organization (all its groups, as `integrations/groups/people.go` collects them) who hold `execute app:deployables/reviews` in that organization and are not operators; callable by anyone who holds that part in the store's organization.

Decisions:
- Group page, "Organization access": an "Apps its people can open" list from the group's grants (an app per row, its parts beneath), each with "Remove"; "Add an app" opens an `AnchoredPicker` of the catalog's apps and grants `read app:<id>`. Shown only to those who hold `read app:settings/access`, as the existing button is.
- Reviews settings: the removal approver is chosen with an `AnchoredPicker` of `reviewsPeople`; a "Who can moderate" list names them; `notifyEmail` stays a field. `writeSettings` refuses an approver who is not in `reviewsPeople`: "Choose someone in this store's organization who can moderate reviews."

- [ ] **Step 1: Write the failing tests**: vitest `an account group lists the apps its people can open and adds one`, Go `TestReviewsPeopleListsTheStoresModerators`, `TestSettingsRefuseAnApproverWhoCannotModerate`, vitest `review settings pick the approver from the people who can moderate`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement, `make sdk-gen` for the new builtin.
- [ ] **Step 4:** Run them, `go test ./packs/reviewspack/ -count=1` (DB lane), `npm run typecheck`, `npx vitest run test/users test/deployables`. Expected: PASS.
- [ ] **Step 5:** Commit: `os: an organization's apps sit on its group page, and moderators are chosen from its people`.

## Finish

- [ ] Whole-branch review on the most capable model.
- [ ] Gates: `go build ./...`, `go vet` on the touched packages, `go test` on the touched packages (DB lane), `go test ./dsl/... .` (root needs the identity static files, already present), `npm run typecheck && npx vitest run` in `clients/os`.
- [ ] Merge into `main` and push (the owner's standing instruction for this program: no PR).
