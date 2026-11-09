# Access clarity follow-ups (plan)

Date: 2026-11-08. Branch: `fix/access-followups`, cut from `origin/main` at
bcc165994. Spec: the follow-up list the parent session handed over on
2026-11-08 (the deferred minors and parked items of the access-clarity work,
merged to main at 11d4773fc; its ledger is the record of where each came
from). There is no separate spec file; this plan quotes the list.

## Global Constraints

- Work only in `/Users/znas/projects/memql/.claude/worktrees/access-followups`,
  by absolute path. Never touch `/Users/znas/projects/memql` itself; never
  stash, reset or switch a branch; never push or merge (the controller does).
- Each item gets a test that fails first (RED evidence in the report: the
  command and the failing output), then passes (GREEN).
- Go tests always carry
  `MEMQL_DATABASE_DSN='postgres://memql:memql_dev@127.0.0.1:55432/memql_followups_a?sslmode=disable'`
  and `MEMQL_REQUIRE_DB=1` for DB lanes, with `-p 1`. Never port 5432, never
  the database `memql_email_a`.
- UI rules: clean and minimal; kit pieces only; no horizontal dividers; no
  chips, badges or toasts in rows; an act that is not legal is absent, not
  disabled (`clients/os/DESIGN.md` rule 12).
- Commits are `<area>: <sentence>` with explicit paths (`git add <paths>`,
  never `-A`), and end with
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. No emojis
  anywhere.
- Never change permissions or grants in production.
- Generated files are regenerated, never hand-edited: `make sdk-gen` (check:
  `make sdk-gen-check`), `make concept-snapshot`.

## Task 1: The Pipelines door is answered per organization

The engine decides `read app:pipelines` per organization
(`component/memql/personal_source_authorization.go` personalSourceCapable
asks it in each organization; the OS `clients/os/src/apps/deployables/parts.ts`
partsForOrganization asks `holdsForOrganization(acct, "read", "app:pipelines")`),
but the pair is missing from the per-organization pairs in
`component/memql/organization_capabilities.go` (resolveOrganizationDecisions,
the `pairs` map). So `effectiveAccessForSubject` answers it once, with
accountId empty, and the caller's own organization entries never carry it.

Files: `component/memql/organization_capabilities.go`,
`integrations/rbac/grants_test.go`, `dsl/rbac/builtins.memql`
(effectiveAccessForSubject's @description), the SDK copies `make sdk-gen`
writes (`sdk/go/client/generated_builtins.go`,
`sdk/ts/src/client/generated_builtins.ts`),
`docs/public/operate/auth/access-model.md` (the paragraph near line 955 that
lists the per-organization pairs).

1. RED: add `TestEffectiveAccessAnswersThePipelinesDoorPerOrganization` to
   `integrations/rbac/grants_test.go`, modelled on
   `TestEffectiveAccessIsPerOrganization`: one writer in two organizations;
   Acme's organization group allows `read app:pipelines`, Beta's denies it.
   Expect `expectAccess(rows, acme, "read", "app:pipelines", true, "group",
   acmeGroup)`, `expectAccess(rows, beta, "read", "app:pipelines", false,
   "group", betaGroup)` and `expectNoMergedRow(rows, "read app:pipelines")`.
   Run it and record the failure.
2. GREEN: add `{Verb: auth.VerbRead, Resource: "app:pipelines"}` to the
   pairs, and say in the function's comment that opening Pipelines is one of
   the organization surfaces, and why (the two callers above).
3. An existing test that pins the exact per-organization pair set (for
   example `TestEffectiveCapabilitiesKeepsOrganizationDiscoverySeparate`, the
   caller's own read) may now see the new pair: update its expectation only
   if it fails, and name it in the report.
4. Say it where the pairs are listed: the builtin description ("opening
   Email, Deployables or Pipelines, the data verbs, an organization's
   deployable parts") and the access-model.md paragraph; `make sdk-gen`;
   `make sdk-gen-check` clean.
5. Run: `go build ./...`; `go vet ./component/memql/ ./integrations/rbac/`;
   `go test -p 1 ./integrations/rbac/` (DB lane); `go test -p 1
   ./component/memql/ -run 'Organization|PersonalSource|EffectiveCapabilit'`
   (DB lane); `go test ./dsl/... .`.

## Task 2: Reviews pack -- the approver resolved directly, the list capped after filtering

Files: `packs/reviewspack/reads.go`, `packs/reviewspack/settings.go`,
`packs/reviewspack/decide.go` (namesApprover), `packs/reviewspack/reads_test.go`,
`packs/reviewspack/settings_test.go`, and the live tests
(`packs/reviewspack/live_people_e2e_test.go`) if they need the new read.

Today `writeSettings` judges one named removal approver by reading the whole
roster (`moderatorsIn`: every member's user row and one engine resolution
each, up to 100 of each); `organizationMembers` cuts the member ids at
`maxPeople` BEFORE personas, operators and inactive people are dropped, and
says nothing when it cuts; `sameUser` uses `memql.BareShortId`, which strips
ANY concept prefix, and the approver id is stored as the caller spelled it;
the roster's sort key `displayName+" "+email` puts nameless people first.

Requirements:

1. The approver check resolves the named person directly. Membership: the
   person's active memberships (`groupsForUser`, read as the deployment the
   way the roster's reads are, `peopleActor`) in an active group of the
   store's organization (`groupsForAccount`) -- the same set
   `organizationMembers` draws from. Then the per-person verdict the roster
   applies: an active user row (`userByIdSystem`, internal origin), never a
   cluster operator, never a role test's stand-in (`auth.IsRoleTestUser`),
   and the engine's decisions for them (ResolveOrganizationDecisionsFor)
   holding the three pairs in that organization (`moderatesIn`). Every read
   under `auth.ContextWithReadFailures`: any failure refuses, as the roster
   does. No roster read on the write path. The per-person part (user row,
   operator / stand-in / active checks, the engine's answer) is ONE function
   that both `moderatorsIn` and the direct check call, so the picker and the
   write cannot disagree.
2. `removalApproverUserId` is stored as the matched person's bare id (the
   `v1:identity:user:` prefix stripped), whatever spelling the caller sent.
3. `sameUser`, and every place this package's people code makes a user id
   bare, strips only `v1:identity:user:` (one small helper): an id of another
   concept never names a person.
4. `organizationMembers` has no cap. `moderatorsIn` sorts the filtered list
   and keeps at most `maxPeople`; the `v1:reviews:people` receipt carries
   `truncated` (boolean): true when more people can moderate than are
   listed. Its doc comment says so. (Task 4 shows it in the OS.)
5. The roster sorts on what the person is called: the display name, else the
   email address, case-insensitively, then the user id.

Tests, each RED first:

- Naming `mod-1` on the `shopRoster` double asks the engine about `mod-1`
  only and reads only `mod-1`'s user row (count the double's reads); the
  double answers `groupsForUser` from its members. Every refusal case of
  `TestSettingsRefuseAnApproverWhoCannotModerate` (outsider, stand-in,
  operator, deactivated, departed, doorless, unknown) still refuses through
  the direct path; a failed read under it refuses.
- Naming `v1:identity:user:mod-1` stores `mod-1`.
- `sameUser("v1:accounts:account:mod-1", "mod-1")` is false;
  `sameUser("v1:identity:user:mod-1", "mod-1")` is true.
- A roster whose first `maxPeople` member ids (by sort order) cannot
  moderate, followed by three who can, lists those three, `truncated` false;
  a roster of `maxPeople`+1 people who can moderate lists `maxPeople` in name
  order, `truncated` true.
- A nameless person sorts by their address among named people.

Run: `go build ./...`; `go vet ./packs/reviewspack/`; `go test -p 1
./packs/reviewspack/` (DB lane, which runs the live tests).

## Task 3: Role testing -- the persona's name, the audit's organization, End's outcome; the origin description

Files: `component/identity/roletest/service.go`,
`component/identity/roletest/service_db_test.go`, a new migration pair
`component/database/memory-nodes/migrations/20261108120000_role_test_session_account.{up,down}.sql`
with a migration test beside
`component/database/memory-nodes/role_test_custom_roles_migration_test.go`,
`embed_inventory_test.go` (the component/database count, measured with
`go list`, never derived), `call_origin_conformance_test.go` (the roletest
entry, only if a new internal-origin stamp is added), `dsl/identity/concepts.memql`
(groupMembership `origin`, line ~780, and its doc comment ~771), the concept
snapshot (`make concept-snapshot`).

Requirements:

1. Start sets the audit's `accountId` before any refusal can return (today it
   is set after `mayTest` refuses, so a refused Start audits "").
2. Start refreshes an existing persona's display name when it is not
   `personaName(role, group)` -- the role's name changed, the organization's
   group was renamed, or the role moved to another organization -- before
   the session is issued. It writes that one field of the persona's own row
   and nothing else. Best effort: a failed rename logs a warning
   (`s.warn`) and Start goes on; the name is what the persona is called, not
   what it may do.
3. End records the organization the test ran in. Start stores it on the
   session row: a new nullable column `identity_role_test_sessions.account_id`,
   written by Start ('' for a global role). End's audit takes it from the row
   it revoked (`RETURNING ... account_id`); a row written before the column
   (NULL) falls back to the role's current scope, which is today's behaviour.
   The down migration drops the column.
4. When End revoked the session but releasing the membership failed, End
   reports success (returns nil, so the audit records success), and logs a
   warning; the membership stays until the operator's next Start or End,
   which releases it (the sweep, or this role's own next End).
5. `dsl/identity/concepts.memql`, groupMembership `origin`: a role-test
   membership is removed when the test is ended, or at the operator's next
   Start or End after it expires -- not "removed when the test ends". Same
   correction in the doc comment above it if it says the same. Regenerate the
   concept snapshot; if `make sdk-gen-check` sees the description, run
   `make sdk-gen`.

Tests, each RED first (service_db_test.go, DB lane):

- A refused Start of an organization role audits that organization's id.
- A persona whose role or organization was renamed between two Starts
  carries the new `personaName` after the second Start.
- End after the role's organization changed audits the organization the
  test ran in (or, if a scope change cannot be staged, End audits the
  `account_id` Start stored -- say which in the report).
- End whose membership release fails returns nil, leaves the session
  revoked, audits success and logs a warning (give the test Store a logger
  that captures warnings).
- The migration test: up adds the column, down removes it.
- The concept snapshot test fails on the edited description before
  `make concept-snapshot`.

Run: `go build ./...`; `go vet ./component/identity/roletest/
./component/database/...`; `go test -p 1 ./component/identity/roletest/
./component/database/memory-nodes/` (DB lane); `go test ./dsl/... .`;
`make sdk-gen-check`.

## Task 4: OS -- Access acts and header, the group page, reviews settings, SearchPicker's test

Files: `clients/os/src/apps/settings/access/model.ts`,
`clients/os/src/apps/users/GroupPage.tsx`,
`clients/os/src/apps/deployables/reviews/ReviewSettingsPage.tsx` (and
`useReviews.ts` / `model.ts` beside it for `truncated`), tests under
`clients/os/test/` (the existing access, users and deployables reviews
suites), and a new `clients/os/test/kit/searchPicker.test.tsx`.

Requirements:

1. `groupActs`: a group row reading "Not granted by this group" offers Deny
   as well as Allow (Deny wherever the group's answer is not already deny).
2. The header of `model.ts` (the "THE ROWS ARE RESOURCES" paragraph) says
   truthfully where an uncatalogued pair shows: a person's row under By
   person is drawn only where the engine answered, so a person grant on a
   pair the catalog does not name is seen (and revoked) under By app only.
   Comment-only: no test.
3. `GroupPage.tsx`: an archived organization group does not render
   `OrganizationApps` (so no warning Notice for it); an active one still does.
4. `ReviewSettingsPage.tsx`:
   - The approver can be cleared: where an approver is named in the draft and
     the viewer may name one (`canEdit && may.nameApprover`), a quiet kit
     Button clears it to "" so Save writes `removalApproverUserId: ""` (the
     engine accepts ""). Absent otherwise. Its accessible name contains its
     visible words.
   - When the people read fails, the Removal approver row says in a Caption
     why no one can be chosen (who can moderate could not be read); the Who
     can moderate row keeps its Notice with Try again.
   - When the people answer is `truncated` (Task 2), the Who can moderate
     list says in a Caption that only the first 100 by name are listed.
5. `searchPicker.test.tsx`: ArrowDown/ArrowUp move the active option
   (`aria-activedescendant`) and stop at the ends; Enter chooses the reached
   option, calls `onChoose` and closes; Enter with no reached option does
   nothing; Escape on an open list closes it, calls `onDismiss`, and does not
   reach a parent's keydown listener (stopPropagation); Escape on a closed
   list does reach it; Tab closes and calls `onDismiss`; keys during IME
   composition do nothing; typing filters by name and by detail, and
   `emptyText` shows when nothing matches. RED: show the suite catches a
   break by running it once against a deliberately broken picker (remove the
   stopPropagation and the isComposing guard), record the failures, restore.

Run in `clients/os`: `npm run typecheck && npx vitest run`.
