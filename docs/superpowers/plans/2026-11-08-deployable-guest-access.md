# Deployable guest access Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An operator can let anyone open one gate of a deployable without a code, through a guest pass, and turn it off again; code holders are unaffected.

**Architecture:** A guest pass is a `siteAccessCode` row with `guest: true` whose `codeHash` is a fixed digest of the site id, so the existing digest index finds it and no typed code can. A fifth access route, `POST /_memql/access/guest`, opens an ordinary access session on it. A new builtin mints it; the existing expiry and remove builtins change and end it; the OS Access codes dialog gains a Guest access part.

**Tech Stack:** Go (component/memql, component/server, component/edge, integrations/siteaccess), MemQL DSL (dsl/platform), React and TypeScript (clients/os), Vitest.

**Spec:** `docs/superpowers/specs/2026-11-08-deployable-guest-access-design.md`, amending `docs/superpowers/specs/2026-10-29-deployable-access-codes-design.md`.

## Global Constraints

- Work only in `/Users/znas/memql-guest-access`, branch `feat/deployable-guest-access`, by absolute path. Never touch `/Users/znas/memql` or `/Users/znas/projects/memql`. Never stash, reset or switch branches. Never push or merge: the controller lands the branch.
- Every task's tests fail first (record the command and the failing output), then pass.
- DB-lane Go tests carry `MEMQL_REQUIRE_DB=1`, `-p 1` and a `MEMQL_DATABASE_DSN` the controller gives you; never port 5432's default database.
- Generated files are regenerated, never hand-edited: `make sdk-gen` (check `make sdk-gen-check`) and `make concept-snapshot`.
- OS UI follows `clients/os/DESIGN.md`: kit pieces only, status never colour alone, an act that is not legal is absent rather than disabled, no chips, badges or toasts in rows.
- Commits are `<area>: <sentence>`, staged by explicit path (never `git add -A`), ending with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. No emojis anywhere.
- No plain code, token or call argument is ever logged, audited or put in a reply other than the mint's.
- Exact copy, used verbatim:
  - refusal, second pass: "This deployable already lets guests in. Change when guest access ends, or turn it off first."
  - refusal, regenerate: "Guest access has no code to regenerate. Change when it ends, or turn it off."
  - closed reply body: `{"state":"closed"}`; bad body reply: `{"error":"invalid_body"}`.

## Review Focus

- A site that turned guest access off and on again holds several rows with the same digest: the guest route must open the one not removed, never the first row found. Test in Task 4.
- Two operators allowing guests at the same moment must leave one pass, not two. Test in Task 3.
- Removing the pass must end a guest session that is open now, on its next request. Test in Task 4.
- A crowd of guest opens must not spend the unlock budget, and the reverse. Test in Task 5.
- A guest pass must never appear as a code in the OS list, and no step may show a code for it. Test in Task 6.

---

### Task 1: Guest-pass primitives

**Files:**
- Modify: `component/memql/site_access_code.go`
- Test: `component/memql/site_access_code_test.go`

**Interfaces:**
- Produces: `func AccessGuestPassDigest(siteID string) string`, lowercase hex SHA-256 of `"guest:" + BareShortId(siteID)`; `AccessGuestPath = AccessSurfacePathPrefix + "guest"` (the bff path) and `AccessGuestInboundPath = AccessInboundPath + "/guest"` (on the site's origin), declared beside `AccessUnlockPath` and `AccessInboundPath`.

- [ ] **Step 1: Write the failing tests**

```go
func TestAccessGuestPassDigestIsFixedPerSite(t *testing.T) {
	// same site, bare or prefixed: equal; another site: different; 64 lowercase hex.
}
func TestNoTypedCodeReachesAGuestPassDigest(t *testing.T) {
	// AccessCodeWellFormed("guest:"+id) is false; for 1,000 MintAccessCode() codes,
	// AccessCodeDigest(code) != AccessGuestPassDigest(site).
}
```

- [ ] **Step 2:** Run `go test ./component/memql/ -run 'GuestPass|TypedCode' -count=1`. Expected: FAIL, undefined.
- [ ] **Step 3:** Implement the function and the two path constants.
- [ ] **Step 4:** Run the same command. Expected: PASS.
- [ ] **Step 5: Commit** `memql: add the guest pass digest and the guest route's paths`.

### Task 2: The concept field and the builtin declaration

**Files:**
- Modify: `dsl/platform/concepts.memql` (`siteAccessCode`), `dsl/platform/builtins.memql`, `component/memql/organization_action_capabilities.go`
- Regenerate: the SDKs (`make sdk-gen`) and the concept snapshot (`make concept-snapshot`)

**Interfaces:**
- Produces: field `guest bool` on `siteAccessCode`, its description saying it is written only by the mint and read as false when empty. `builtin siteAccessGuestPassCreate { siteId string!, label string!, gate string!, subject string, expiresAt datetime! }` with `@sdk`, `@executor("integration.siteAccess.guestPassCreate")`, `@args(profile="object")`, `@requiresCapability("execute", "app:deployables/access")`, a doc comment in the style of `siteAccessCodeCreate`'s, and "Returns {codeId, expiresAt}." The organization target for `siteAccessGuestPassCreate` is the site, as for `siteAccessCodeCreate`.

- [ ] **Step 1:** Run `go test ./component/memql/ -run 'OrganizationAction|Builtin' -count=1` and `make sdk-gen-check` after adding only the DSL. Expected: FAIL (target missing, generated files stale).
- [ ] **Step 2:** Add the target, run `make sdk-gen` and `make concept-snapshot`.
- [ ] **Step 3:** Run the same tests, `make sdk-gen-check` and `go test ./test/dslconformance/... -count=1`. Expected: PASS.
- [ ] **Step 4: Commit** `platform: declare the guest pass field and its builtin`.

### Task 3: Minting a guest pass

**Files:**
- Modify: `integrations/siteaccess/integration.go`, `integrations/siteaccess/store.go`
- Test: `integrations/siteaccess/integration_db_test.go`, `integrations/siteaccess/store_test.go`

**Interfaces:**
- Consumes: Task 1's `AccessGuestPassDigest`, Task 2's builtin.
- Produces: capability `guestPassCreate` on the integration, handled by `handleGuestPassCreate`, replying `{codeId, expiresAt}`. `newCode` and `codeView` gain `Guest bool`; `createCode` writes `guest`; the row reader reads it; `listRow` sets `"guest": true` for a pass. A store read `livePassForSite(ctx, siteID string) (codeView, bool, error)` that finds rows by `AccessGuestPassDigest(siteID)` and returns the one not removed.

Decisions:
- `mint`'s checks (run, caller, site, owner, package, gate, label, subject, expiry) are shared by both mints, so a guest pass is refused exactly as a code would be. Extract them rather than copy them.
- The guest mint reads `livePassForSite` inside the `onceMore` act and refuses with the second-pass sentence. Two concurrent mints rely on the site-version fence `createCode` already takes: the loser re-reads, finds the winner's pass, and refuses.
- A guest pass stores `codeHint` empty if the concept accepts it, else the four characters `----`, which no code's hint can be. The OS never shows it.
- `regenerate` refuses a guest pass with the regenerate sentence, before any secret is minted.
- The mint audits like a code's, with `"act": "guestPassCreate"` and no code in the detail.

- [ ] **Step 1: Write the failing tests** (DB lane):

```go
func TestGuestPassCreateRepliesWithNoCode(t *testing.T)        // reply keys are exactly codeId and expiresAt
func TestGuestPassRefusedWhileOneStands(t *testing.T)          // second create -> the second-pass sentence
func TestGuestPassAllowedAgainAfterRemoval(t *testing.T)       // create, remove, create -> two rows, one live
func TestConcurrentGuestPassCreatesLeaveOne(t *testing.T)      // two goroutines -> exactly one live pass
func TestRegenerateRefusesAGuestPass(t *testing.T)             // the regenerate sentence; generation unchanged
func TestListMarksTheGuestPass(t *testing.T)                   // guest true on the pass, absent on codes
func TestGuestPassRefusedWhereACodeWouldBe(t *testing.T)       // no owner, system site, no package, unknown gate, bad label, bad expiry
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=<given> go test -p 1 ./integrations/siteaccess/ -run 'GuestPass|Regenerate|ListMarks' -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, then the whole package. Expected: PASS.
- [ ] **Step 5: Commit** `siteaccess: mint a guest pass, and refuse to regenerate one`.

### Task 4: The guest route in the bff

**Files:**
- Modify: `component/server/access_handler.go`, `integrations/siteaccess/store.go` (the `AccessStore` implementation), and wherever the access routes are classified (`AccessSurfacePaths()`, `AccessSelfAuthenticatedPaths`, `HandlerAuthorizedPaths()`, `servedButNotExternallyRouted`), which should follow the `accessRoutes` table
- Test: `component/server/access_handler_test.go`, `integrations/siteaccess/integration_db_test.go`

**Interfaces:**
- Consumes: Task 1's paths, Task 3's store read.
- Produces: `AccessCode.Guest bool`; `AccessStore.GuestPassForSite(ctx context.Context, siteID string) (*AccessCode, error)`, nil with no error when the site has no pass that is not removed; `accessOpGuest = "guest"`; a fifth `accessRoutes` row `{http.MethodPost, memql.AccessGuestPath, accessOpGuest, memql.AccessSmallBodyBytes}`; `serveGuest(w, r, stamp) string`; `writeAccessSession` adds `"guest": code.Guest` to every session reply.

`serveGuest`, in order: a body that does not decode as a JSON object is `400 invalid_body` with no read; `GuestPassForSite`; none, or past its expiry, is `403 closed` with no site read; `answerSite`; `codeOpensSite` false is `403 closed`; then mint the token, `CreateSession` with the pass's generation, set the cookie, `writeAccessSession`. A store error is the existing `502 refused`. Metrics count `op="guest"`.

- [ ] **Step 1: Write the failing tests:**

```go
func TestGuestOpensASessionOnTheLivePass(t *testing.T)            // 200, cookie set, guest true, gate and subject of the pass
func TestGuestIsClosedWithoutAPass(t *testing.T)                  // 403 closed, no site read, no session written
func TestGuestIsClosedForARemovedOrExpiredPass(t *testing.T)
func TestGuestTakesTheLivePassAmongRemovedOnes(t *testing.T)      // DB lane, real store: create, remove, create
func TestGuestRefusesABodyThatIsNotAnObject(t *testing.T)         // "[]", "x", oversize -> 400 invalid_body, no read
func TestGuestIsClosedForAnotherOrganizationsCopy(t *testing.T)
func TestSessionRepliesSayWhetherTheyAreAGuests(t *testing.T)     // unlock false, session false for a code, true for a guest
func TestRemovingThePassEndsAnOpenGuestSession(t *testing.T)      // next GET -> 401 ended, cookie cleared
func TestAGuestCallIsStampedAsTheCodeIs(t *testing.T)             // accessCodeId the pass, label on a mutation only
```

- [ ] **Step 2:** Run `go test ./component/server/ -run 'Guest|SessionReplies|RemovingThePass' -count=1` and the DB-lane case. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run them, then `go test ./component/server/ ./integrations/siteaccess/ -count=1` (DB lane included) and the surface gate `AssertUnauthenticatedSurfaceDeclared`'s test. Expected: PASS.
- [ ] **Step 5: Commit** `server: open a guest session on a deployable's live guest pass`.

### Task 5: The guest route at the edge

**Files:**
- Modify: `component/edge/access.go`, the front-door route generator's input if it lists the access pairs
- Test: `component/edge/access_test.go`

**Interfaces:**
- Consumes: Task 1's `AccessGuestInboundPath` and `AccessGuestPath`.
- Produces: a fifth `accessRoutes` entry `{http.MethodPost, accessGuestPath, memql.AccessGuestPath, accessOpGuest, memql.AccessSmallBodyBytes}`; constants `accessGuestPerMinutePerAddress = 10` and `accessGuestPerMinutePerSite = 60`; `accessLimits` gains `guestAddress` and `guestSite` buckets, which `allow` spends for `accessOpGuest` only.

- [ ] **Step 1: Write the failing tests:**

```go
func TestAccessSurfaceAnswersFivePairsAndNoOthers(t *testing.T)   // POST guest routed; GET guest, POST guest/x not
func TestGuestOpensHaveTheirOwnBuckets(t *testing.T)               // 10 guest opens from one address -> 429; an unlock still passes; and the reverse
func TestGuestOpenChecksContentTypeOriginAndSize(t *testing.T)
```

- [ ] **Step 2:** Run `go test ./component/edge/ -run 'FivePairs|GuestOpen' -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./component/edge/ -count=1` and any front-door generator test. Expected: PASS.
- [ ] **Step 5: Commit** `edge: route the guest open, with buckets of its own`.

### Task 6: Guest access in MemQL OS

**Files:**
- Modify: `clients/os/src/apps/deployables/access/useAccessCodes.ts`, `clients/os/src/apps/deployables/access/AccessCodesPanel.tsx`
- Test: `clients/os/test/deployables/accessCodes.test.tsx`

**Interfaces:**
- Consumes: Task 2's builtin; `siteAccessCodeSetExpiry` and `siteAccessCodeRemove` as they are.
- Produces: `AccessCodeRow.guest: boolean`; the hook returns `codes` without the pass and `guestPass: AccessCodeRow | undefined`; `allowGuests(input: { label: string; gate: string; subject?: string; expiresAt: string }): Promise<boolean>` calling `siteAccessGuestPassCreate`, re-reading after, as the other writes do.

Copy, verbatim: part title "Guest access"; off "Off. Only someone with a code can open it."; act "Allow guests"; form fields "Name" (prefilled "Guests"), "Opens", "Subject", "Ends"; on "Ends in 12 days" (the existing expiry wording), "Ended" once past, "Last opened 2 minutes ago" or "Not opened yet"; acts "Change end date" and "Turn off", whose in-place ask reads "Everyone in as a guest is taken out now."

- [ ] **Step 1: Write the failing tests:**

```tsx
test("guest access reads off, and Allow guests is absent without gates")
test("allowing guests sends the name, the gate and the end, and shows no code")
test("guest access on shows what it opens, when it ends and when a guest last came in")
test("Turn off asks in place, then removes the pass")
test("the guest pass is never a row in the code list")
```

- [ ] **Step 2:** Run `npm --prefix clients/os test -- test/deployables/accessCodes.test.tsx`. Expected: FAIL.
- [ ] **Step 3:** Implement with the existing kit pieces the panel already uses (`Subhead`, `RecordList`, `InPlaceAsk`, the add form and the expiry presets).
- [ ] **Step 4:** Run the same command, then the whole deployables folder, then render the dialog through the QA harness in light and dark, off and on, plus a narrow capture, and read the images. Expected: PASS, and nothing cramped or out of place.
- [ ] **Step 5: Commit** `os: let an operator allow guests on a deployable`.

### Task 7: Documentation

**Files:**
- Modify: `docs/public/operate/site-access-codes.md` (a Guest access section: what it is, the route and replies, the client's obligation of the spec's section 7, the shared partition and what a product must keep out of a gate open to guests), `CLAUDE.md` (the Deployable access codes row: five pairs, the guest route and that only an operator creates a pass), `docs/public/operate/deployables.md`, `clients/os/README.md` where it describes the panel

- [ ] **Step 1:** Write the sections. Public docs link to no spec or plan.
- [ ] **Step 2:** Run `go test . -run 'Docs' -count=1` from the repository root. Expected: PASS.
- [ ] **Step 3: Commit** `docs: describe guest access`.

### Task 8: Land on main

- [ ] **Step 1:** `git fetch origin && git rebase origin/main`, resolving any conflict.
- [ ] **Step 2:** On the rebased tree run every package this touched, DB lanes included, `make sdk-gen-check`, the concept snapshot check, the deployables OS tests and the docs tests. Expected: PASS.
- [ ] **Step 3:** `git push origin HEAD:main`, then watch `MemQL / memql / push` for that commit to pass. A failure is fixed or reverted at once.
- [ ] **Step 4:** Delete the branch and the worktree, and check `git config --get-all remote.origin.fetch` holds no line for it.

### Task 9: Release

- [ ] **Step 1:** `VERSION` already reads 0.31.26 and v0.31.26 is untagged, so no bump.
- [ ] **Step 2:** In MemQL OS, Pipelines > Releases > Cut release, engine profile, from the newest green commit; approve, publish every destination and seal (the owner's go-ahead of 2026-11-08, "Yes, do all of it").
- [ ] **Step 3:** Read what v0.31.25..v0.31.26 changes in migrations and `COMPATIBILITY.md`, as the instance runbook's "Before deploying" says, and report whether a backup is needed before the instance syncs it.
