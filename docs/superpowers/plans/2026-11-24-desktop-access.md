# Desktop Access Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Everyone but the primary owners and developers stays out of MemQL OS unless model access is set up for them. A held person sees a page whose reference an administrator finds in Logs.

**Architecture:** A caller-only engine builtin, `desktopAccess`, decides two things: whether the caller is exempt, and which model sources are set up for them (asleep counts). For a held caller it mints a reference and writes one log line synchronously, through a recorder that `app/` wires to the log store. The OS gate reads it at entry and checks again on focus, on reconnect, and every 30 seconds while the page shows. Ask's no-model messages read the same sources, so they stop offering Fleet to people who cannot open it.

**Tech Stack:** Go (`component/memql`, `component/logstore`, `app`), MemQL DSL (`dsl/common`, `dsl/platform`), TypeScript and React (`clients/os`), generated SDKs (`make sdk-gen`).

**Spec:** `docs/superpowers/specs/2026-11-24-desktop-access-design.md` (approved by the owner 2026-11-24). Build it AFTER the memory-search plan (spec section 8).

## Global Constraints

- **Exempt:** `auth.MayManageFleet(role)` on the person's STORED role, read fresh. Never the AccessContext's `RoleStandIn`. `admin` is not exempt.
- **What counts:**
  - A machine model counts when it does not only make embeddings, has `StructuredOutput`, and has `ContextWindow >= MinimumContextWindow` (8192). Online is not required.
  - An app on the person's own machine counts while offline and while "out of usage". "Measured signed out" does not count.
  - A key counts when it is usable, including "not checked yet", or when it is held for credit. Paused, refused or "owner inactive" does not count.
  - The cluster's account counts when `VendorDoorFor(...).Cluster` is true.
- **Reference:** the format is `ERR-` plus 6 lowercase hex characters, matching `^ERR-[0-9a-f]{6}$`. A passed-back value that does not match is treated as absent, and is never written.
- **Writing the line:** it is written synchronously before the reference is returned. If the write fails, the answer is held with an EMPTY reference. A re-check that passes back a valid reference with an unchanged reasons digest writes nothing and returns the same reference.
- **Client side:** a failed read ("unknown") lets the person in. Open windows never bypass a held page.
- **Transport:** no new HTTP endpoint. The builtin runs on `MemqlService.Stream`.
- **Words:** the OS copy follows `clients/os/DESIGN.md` (read `.agents/skills/memql-ui-design/SKILL.md` first). The page uses `kit/EntryLayout`. No emojis.
- **Process:**
  - Stage files by explicit path.
  - Go tests run through `make test` and the db trees with `MEMQL_REQUIRE_DB=1` (memory `local-test-database-container`), never bare `go test ./...`.
  - OS checks are `make os-test` and `make os-typecheck`.
  - SDKs: `make sdk-gen`, then `make sdk-gen-check`.
- **Docs:** they change in the same commits as the behaviour they describe.

## Review Focus

- A share removed while the person is on the desktop: their next check (focus or reconnect) holds them.
- A machine lent to a GROUP the person belongs to counts as lent to them.
- A borrowed-authority context whose `Role` is a stand-in: the exemption still reads the stored role.
- Two tabs checking at once: each keeps its own reference, and an unchanged re-check from either writes nothing.
- A forged or malformed reference passed back is never echoed into the log.

---

### Task 1: The sources a person has set up

**Files:**
- Create: `component/memql/desktop_access.go`
- Test: `component/memql/desktop_access_test.go`

**Interfaces:**
- Consumes:
  - `(e *MemQLEngine) fleetCatalogForCaller(ctx) ([]FleetModel, error)`.
  - `FleetModel.Machines` and `FleetMachine.OwnerUserId` / `Online` (`fleet_provider.go`).
  - `(r *ProviderRegistry) AppDoors(ctx, actingUserId) ([]AppDoor, error)`: it keeps offline machines, own machines only.
  - `(r *ProviderRegistry) VendorKeysAt(ctx, VendorAccountQuery) VendorKeyAnswer`, which gives `Usable` and `PassedOver{Account, Reason}`. Ask it once per vendor with `Chosen: true`.
  - `(r *ProviderRegistry) VendorDoorFor(ctx, actingUserId) VendorDoor`.
  - The key-reason strings in `integrations/vendorkeys/resolver.go` (:121 paused, :292 refused, :295 credit, :307 owner inactive).
- Produces:
  - Kind constants `AccessSourceMachine = "machine"`, `AccessSourceApp = "app"`, `AccessSourceKey = "key"`, `AccessSourceClusterAccount = "clusterAccount"`.
  - State constants `AccessReady = "ready"`, `AccessAsleep = "asleep"`, `AccessOutOfCredit = "outOfCredit"`, `AccessOutOfUsage = "outOfUsage"`.
  - `type AccessSource struct { Kind, State, LenderUserId, Label string; Own bool }`, with json tags in lowerCamel.
  - `func (e *MemQLEngine) desktopAccessSources(ctx context.Context, actingUserId string) (sources []AccessSource, reasons []string, err error)`.
  - `func accessReasonsDigest(reasons []string) string`: the sorted reasons joined by newline, sha256, first 16 hex characters.

Each source that does not count adds ONE plain reason. The reason names a lender's id only for something lent to this person:
- "no machine of theirs or lent to them offers a chat model"
- "the key lent by <id> is paused by its owner"
- "the key lent by <id> was refused by <vendor>"
- "the key lent by <id> belongs to someone who is no longer an active user"
- "their own key was refused by <vendor>"
- "may use the cluster's account, but none is set up"
- "no app on their machines is allowed and signed in"

- [ ] **Step 1: Write the failing tests.** Reuse `engineWithFleet`, `stubFleet`, `userCtx`, `stubApps`, `fakeVendorKeys`, `personContext` and `openToEverybody`. The tests and their exact assertions:
  - `TestAnOfflineLentMachineCountsAsSetUp`: the catalog has a model with `StructuredOutput`, `ContextWindow` 8192 and not `Embeddings`, on a machine owned by `lender` with `Online=false`. Want one source `{Kind:"machine", State:"asleep", Own:false, LenderUserId:"lender"}` and no reasons.
  - `TestTheirOwnOnlineMachineIsReady`: `{machine, ready, Own:true}`.
  - `TestAnEmbeddingOnlyModelIsNotAccess`: no sources. The reasons contain "no machine of theirs or lent to them offers a chat model".
  - `TestAModelBelowTheContextFloorIsNotAccess`: `ContextWindow` 4096 gives no sources.
  - `TestAnAppOnAnOfflineMachineCountsAsSetUp`: `{app, asleep, Own:true}`.
  - `TestACreditHeldKeyCountsAndAPausedOneDoesNot`: one `PassedOver` with the credit reason gives `{key, outOfCredit}`. One with the paused reason adds the "paused" reason and no source.
  - `TestTheClusterAccountCountsWhereItPaysForThePerson`: with `VendorDoor.Cluster` true, want `{clusterAccount, ready}`.
  - `TestPermissionWithNoClusterAccountIsItsOwnReason`: the person holds the part but `ClusterSetUp` is false. The reason is "may use the cluster's account, but none is set up".
  - `TestTheDigestIgnoresReasonOrder`: `accessReasonsDigest([a, b]) == accessReasonsDigest([b, a])`, and it has 16 characters.
- [ ] **Step 2:** Run `make test` (or `go test github.com/znasllc-io/memql/component/memql -run 'TestAnOffline|TestTheirOwn|TestAnEmbedding|TestAModelBelow|TestAnAppOn|TestACreditHeld|TestTheClusterAccountCounts|TestPermissionWithNo|TestTheDigest'`). Expected: FAIL, because the functions are undefined.
- [ ] **Step 3:** Implement `desktopAccessSources` and `accessReasonsDigest` in `component/memql/desktop_access.go`. Reuse the eligibility rule of `inferenceDoors` (`fleet_catalog_read.go:219`) with the `Online()` test dropped. Do not copy it, or the two will drift: extract a shared predicate `chatModelMeetsMinimum(m FleetModel) bool` and use it in both places. Map a key's `PassedOver` reason to a state by comparing it with the resolver's own reason constants, never by substring of vendor words.
- [ ] **Step 4:** Run the same command. Expected: PASS. Then run the whole `component/memql` package unit tests: PASS.
- [ ] **Step 5: Commit**
```bash
git add component/memql/desktop_access.go component/memql/desktop_access_test.go component/memql/fleet_catalog_read.go
git commit -m "memql: the model sources a person has set up, asleep included"
```

### Task 2: The exemption, the reference and the hold line

**Files:**
- Modify: `component/memql/desktop_access.go`
- Test: `component/memql/desktop_access_test.go`

**Interfaces:**
- Consumes: `desktopAccessSources` and `accessReasonsDigest` from Task 1; `auth.MayManageFleet`; `ContextWithFreshRead`; an existing engine read of a `v1:identity:user` row by id (the builder picks one that returns `role` and `active`).
- Produces:
  - `type AccessHoldLine struct { Reference, UserId string; Reasons []string }`
  - `func SetAccessHoldRecorder(record func(ctx context.Context, line AccessHoldLine) error)`: package-level, the same pattern as `SetEmbedderBindingReader`. With no recorder installed, writing counts as failed.
  - `func newAccessReference() string`
  - `var accessReferencePattern = regexp.MustCompile("^ERR-[0-9a-f]{6}$")`
  - `type DesktopAccess struct { Admitted, Exempt bool; Sources []AccessSource; Reference, ReasonsDigest string }`
  - `func (e *MemQLEngine) desktopAccess(ctx context.Context, actingUserId, passedReference, passedDigest string) (DesktopAccess, error)`

**Rules:**
- A call that names nobody (`auth.NamesNoPerson`) is refused with an error.
- Exempt means `Admitted=true, Exempt=true`. Sources are still computed, because Ask uses them.
- Any source means `Admitted=true`.
- Otherwise the person is held:
  - When `passedReference` matches the pattern and `passedDigest` equals the current digest, return it and write nothing.
  - When the reference is valid and the digest differs, write one line under the SAME reference.
  - Otherwise mint a new reference and write one line.
  - A failed write returns `Reference: ""`.

- [ ] **Step 1: Write the failing tests:**
  - `TestAnOwnerOrDeveloperIsAdmittedWithNothingSetUp`, for stored roles `owner` and `developer`.
  - `TestAnAdminWithNothingSetUpIsHeld`.
  - `TestAnAccountScopedRoleIsHeld`.
  - `TestTheExemptionReadsTheStoredRoleNotAStandIn`: the AccessContext role is `owner` with `RoleStandIn`, and the stored role is `user`. Want held.
  - `TestAHeldAnswerWritesOneLineBeforeItReturnsTheReference`: a recorder stub gets exactly one line, its reference matches the pattern and equals the answer's, and the reasons are non-empty.
  - `TestARecorderFailureHoldsWithNoReference`.
  - `TestCheckingAgainWithTheSameReasonsWritesNothing`: two calls, and the second passes back the reference and digest. The recorder is called once and both references are equal.
  - `TestChangedReasonsWriteANewLineUnderTheSameReference`.
  - `TestAMalformedReferenceIsNeverWritten`: passing `"ERR-zzzz"` makes the line carry a freshly minted reference, never `"ERR-zzzz"`.
  - `TestAnAdmittedAnswerWritesNothing`.
  - `TestACallThatNamesNobodyIsRefused`.
  - `TestAnotherReplicaKeepsThePassedReference`: two engines over the same stubs, where the second receives the first's reference and digest. Same reference, and no line.
- [ ] **Step 2:** Run them. Expected: FAIL, because they are undefined.
- [ ] **Step 3:** Implement `desktopAccess`, `SetAccessHoldRecorder` and `newAccessReference` (3 random bytes, lowercase hex).
- [ ] **Step 4:** Run them: PASS. Run the package unit tests: PASS.
- [ ] **Step 5: Commit** (`git add` the two files): "memql: hold a person with no model access, with a reference written first".

### Task 3: The builtin, its row and the SDKs

**Files:**
- Modify:
  - `dsl/common/builtins.memql`: add after `inferenceStatus` at :639.
  - `dsl/platform/concepts.memql`: a virtual `concept desktopAccess` beside `inferenceStatus` at :836.
  - `component/memql/engine_types.go`: `BuiltinExecutorDesktopAccess = "desktopAccess"`.
  - `component/memql/executor_builtin.go`: register it in `initBuiltinExecutorHandlers`.
  - `component/memql/desktop_access.go`: the handler.
  - The generated `sdk/ts/src/client/generated_builtins.ts` and `sdk/go/client/generated_builtins.go`, through `make sdk-gen`.
  - `component/memql/fleet_catalog_read_cost_test.go`: add the reader to its map if the test requires it.
- Test: `component/memql/desktop_access_test.go`

**Interfaces:**
- DSL: `@sdk @executor("desktopAccess") builtin desktopAccess { reference string  reasonsDigest string }`. Each field gets a `///` doc and the builtin gets an `@description`. No `@requiresCapability`: it answers only for the caller, like `inferenceStatus`.
- The concept's fields:
  - `admitted bool!`
  - `exempt bool!`
  - `reference string`
  - `reasonsDigest string`
  - `sources []object`, each `{kind, state, own, lenderUserId, label}`
- Produces: `func (e *MemQLEngine) evaluateDesktopAccess(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error)`. It returns ONE row, with id `current`, concept `v1:platform:desktopAccess`, and the caller from `actingUserFromContext`. A refusal (names nobody) is an error.

- [ ] **Step 1: Write the failing test** `TestDesktopAccessIsExactlyOneRow`, modelled on `TestInferenceStatusIsExactlyOneRow`. One row, id `current`, and the payload has `admitted` and `exempt`.
- [ ] **Step 2:** Run it: FAIL.
- [ ] **Step 3:** Add the DSL, the concept, the constant, the registration and the handler. Run `make sdk-gen`.
- [ ] **Step 4:** Run the test: PASS. `make sdk-gen-check`: clean. `make test`: no new failure. If the DSL conformance or the builtin catalog tests ask for the new builtin to be classified, classify it exactly as `inferenceStatus` is.
- [ ] **Step 5: Commit** with explicit paths: the two DSL files, the three Go files, and the two generated SDK files. Message: "memql: desktopAccess answers whether the caller may enter MemQL OS".

### Task 4: Write the hold line to the log store, synchronously

**Files:**
- Modify: `component/logstore/sink.go` (add `InsertNow`); the `app/` file that installs the log store sink. Find it with `grep -rn "logstore\." app/`.
- Test:
  - `component/logstore/sink_test.go`: the unit test, with a stub `InsertFunc`.
  - `component/logstore/insert_now_db_test.go`: db-gated.

**Interfaces:**
- Produces: `func (s *Sink) InsertNow(ctx context.Context, l logger.Line, st Stamp) error`. It builds the row with `rowFromLine` and inserts it through the sink's insert function, bypassing the queue, the level floor and the rate bucket. A nil sink returns `ErrNoStore`.
- Wiring in `app/`: `memql.SetAccessHoldRecorder(...)` writes:
  - `logger.Line{Level: slog.LevelWarn, Component: "desktopAccess", Message: "MemQL OS held this person: no model access is set up", Subject: <bare user id>, SubjectConcept: "v1:identity:user", Attributes: {"reference": ref, "reasons": reasons}}`
  - with `Stamp{NodeType, Node, UserId}` from this node.

- [ ] **Step 1: Write the failing tests:**
  - `TestInsertNowWritesOnceAndReportsFailure`: a stub insert receives exactly one row with the reference in `Attributes`. A stub error is returned unchanged.
  - `TestInsertNowLineIsFoundBySearchingItsReference`: db-gated. Insert one line, then `Search(ctx, db, Query{From, To, Text: ref})` returns it.
- [ ] **Step 2:** Run them (the db test with `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=postgres://memql:memql_dev@127.0.0.1:55432/memql?sslmode=disable`): FAIL.
- [ ] **Step 3:** Implement `InsertNow` and the app wiring.
- [ ] **Step 4:** Run them: PASS. Run `scripts/ci/module-integrity.sh`: clean, because `app` already imports both packages.
- [ ] **Step 5: Commit**: "logstore: InsertNow for a line that must be findable before it is shown".

### Task 5: The held page in MemQL OS

**Files:**
- Create:
  - `clients/os/src/chrome/useDesktopAccess.ts`
  - `clients/os/src/chrome/AccessHeld.tsx`
  - `clients/os/test/chrome/accessHeld.test.tsx`
- Modify:
  - `clients/os/src/chrome/CoreGate.tsx`. For the non-exempt branch, `ToldVariant` (:248) gives way to `AccessHeld` when held. Exempt people keep today's behaviour.
  - `clients/os/test/chrome/coreGate.test.tsx`: the role-variant expectations at :177 that change.
  - `clients/os/src/styles/index.css`, only if the page needs a rule. Reuse `.os-entry-layout > .os-wizard` (:8539).

**Interfaces:**
- `export type DesktopAccessAnswer = { state: "unknown" } | { state: "admitted"; exempt: boolean; sources: AccessSourceRow[] } | { state: "held"; reference: string; reasonsDigest: string }`
- `export type AccessSourceRow = { kind: string; state: string; own: boolean; lenderUserId?: string; label?: string }`
- `export function useDesktopAccess(enabled: boolean): DesktopAccessAnswer & { recheck(): void }`. It calls `connection.query.desktopAccess({ reference, reasonsDigest })` with the last held values, and checks again:
  - on window focus;
  - when the connection returns;
  - every 30_000 ms while held and `document.visibilityState === "visible"`;
  - on `recheck()`.
- `export function AccessHeld(props: { reference: string; canAddKey: boolean; onSignOut(): void; onAdded(): void })`.
  - It draws `EntryLayout`, the mark, the title "Your access is not set up yet", and the sentence "MemQL needs a model it can use for you. Ask your administrator to set one up, and give them this reference."
  - Then the reference with Copy (the kit's copy control), and "This page opens by itself once it is set up.".
  - Add an API key mounts `AddKeyWizard` (`apps/settings/apikeys/AddKeyWizard.tsx:53`) INSIDE the entry frame, through `useVendorKeys()`, following `auth/IdentityScreen.tsx:180-207`.
  - Then Sign out.
  - An empty reference hides the reference line.

- [ ] **Step 1: Write the failing tests** in `accessHeld.test.tsx`, using `fakeConnection` and `withSession` from `test/cluster/harness.tsx`:
  - "a held person sees the reference and can copy it".
  - "an unknown answer lets the person in".
  - "an admitted answer lets the person in".
  - "it checks again every 30 seconds while visible and lifts when admitted", with fake timers.
  - "it checks again on focus".
  - "a check again passes the reference back": the mock receives `{reference, reasonsDigest}`.
  - "adding a key happens on the page, and no dock, launcher or app window is drawn".
  - "a key added checks again".
  - "an empty reference shows no reference line".
  - "an owner with nothing set up reaches the desktop, or Set up this cluster when the cluster has no model".
- [ ] **Step 2:** `make os-test`: the new tests FAIL.
- [ ] **Step 3:** Implement the hook, the page and the CoreGate change. While held, the page replaces everything; the `windowsOpen` escape (CoreGate.tsx :108-119) never applies to it.
- [ ] **Step 4:** Run `make os-test` and `make os-typecheck`: PASS.
- [ ] **Step 5: Commit** with explicit paths: "os: a person with no model access stays at a page with a reference".

### Task 6: Ask's messages for people who cannot open Fleet

**Files:**
- Create:
  - `clients/os/src/ask/accessWords.ts`
  - `clients/os/test/ask/accessWords.test.ts`
- Modify:
  - `clients/os/src/ask/useAskReadiness.ts`: the `failed` sentence at :61.
  - `clients/os/src/ask/askFailure.ts`
  - `clients/os/src/ask/AskSurface.tsx`: :406, where Open Fleet shows only when Fleet can open.
  - `clients/os/src/ask/AskSheet.tsx`: :54, where `onOpenFleet` is passed.
  - `clients/os/test/ask/failure.test.tsx`, `readiness.test.tsx`, `recovery.test.tsx` (:77).

**Interfaces:**
- `export function unavailableSentence(sources: AccessSourceRow[], nameOf: (userId: string) => string): string`, with these sentences:
  - the person's own machine asleep: "Your machine {label} is offline. Ask works again when it is back."
  - a lent machine asleep: "The machine {name} lent you is offline. Ask {name}, or your administrator."
  - a lent key out of credit: "The API key {name} lent you is out of credit. Ask {name}, or your administrator."
  - their own key out of credit: "Your API key is out of credit."
  - an app out of usage: "{App} has used up its allowance for now."
  - otherwise: "No model could take this request. Ask your administrator."
- `askFailure(message: string, opts?: { canOpenFleet: boolean; sources?: AccessSourceRow[]; nameOf?: (id: string) => string }): { summary: string; fleet: boolean }`. `fleet` is true only when `canOpenFleet`. For people who can open Fleet the summary is unchanged.
- `canOpenFleet` comes from `useAppReach("fleet")`: `sections.includes("machines") && canOpenWindows` (the `InferenceStop.tsx:76-81` pattern).
- `nameOf` uses an existing people read open to a signed-in person. The builder finds one, falling back to "the person who lent it".

- [ ] **Step 1: Write the failing tests:**
  - every sentence above;
  - "no Open Fleet for a person who cannot open Fleet";
  - "Fleet stays for an owner";
  - the dock dot's failed sentence for a non-Fleet person names the asleep source.
- [ ] **Step 2:** `make os-test`: FAIL.
- [ ] **Step 3:** Implement. The sources come from the same `desktopAccess` answer the gate holds, exposed through the hook's admitted state. Do not add a second read.
- [ ] **Step 4:** `make os-test` and `make os-typecheck`: PASS.
- [ ] **Step 5: Commit**: "os(ask): say what is offline and whom to ask, and never offer a Fleet the person cannot open".

### Task 7: Documentation

**Files:**
- Modify:
  - `docs/public/operate/auth/access-model.md`. Add a section "Who gets into MemQL OS": the rule, the exemption (and the per-account change to come), what counts, the reference and how to search it in Logs, and that a failed check lets the person in.
  - `clients/os/README.md`: the access gate beside the readiness gate.
  - `docs/superpowers/specs/2026-11-24-desktop-access-design.md`: set the status to approved 2026-11-24, built.
  - The code comments in `CoreGate.tsx` that describe the old non-owner variant.
- [ ] **Step 1:** Write the sections. Run `go test github.com/znasllc-io/memql/docs/...` and the docs gates `make test` runs. Expected: PASS (front-matter and boundary gates).
- [ ] **Step 2: Commit**: "docs: who gets into MemQL OS, and how to read a held person's reference".
