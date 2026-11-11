# Wholesale from application to trade prices Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One wholesale application goes from the storefront form, through a terms acceptance on a private link and a decision by the client's own staff in the OS, to a buyer tagged in Shopify who pays trade prices, with the applicant and the staff told by email at each step and no step done by the engine team.

**Architecture:** The wholesale pack's automation-called builtins require internal origin, read as a named system actor and write as the application's merchant, behind `@serverOnly` mutations; every refusal to grant leaves an entitlement row naming its cause. Every wholesale row carries the `accountId` of its site, and an organization part `app:deployables/wholesale` gates builtins scoped to one application or one site. Terms are versioned documents reached by hashed private links; applicant mail rides a new transactional row lane in campaigns (no unsubscribe, a bounce still stops it) and staff mail rides the outbox. The OS gains a Wholesale section on the storefront deployable's page.

**Tech Stack:** Go (`packs/wholesalepack`, `component/campaigns`, `component/memql`, `component/server`, `component/edge`, `component/outbound`, `component/auth`, `integrations/shopify`), MemQL DSL (the pack's `dsl/`, `dsl/campaigns`, `dsl/shopify/overlay`, `dsl/rbac`), React and TypeScript (`clients/os`), Vitest.

**Spec:** `docs/superpowers/specs/2026-11-11-wholesale-end-to-end-design.md`. Executors read both. This plan covers units U1 to U12 and the documentation (spec section 15). U13 (suspend and reinstate) is a later plan: nothing here adds `suspend`, `reinstate`, the `suspended` state or their template fields. The spec's walk (section 11) and rollout (section 16) are operator steps on the owner's instance, not tasks.

## Global Constraints

- Work only in `/Users/znas/projects/memql/.claude/worktrees/wholesale`, branch `feat/wholesale-end-to-end`, by absolute path. Never touch `/Users/znas/projects/memql` itself or another worktree. Never stash, reset, switch branch or push. Tasks 8 and 9 begin with a rebase onto `main` that the controller performs or approves (see those tasks).
- Every task's tests fail first (record the command and the failing output), then pass.
- Database: Task 1 exports `WHOLESALE_DSN='postgres://memql:memql_dev@127.0.0.1:55432/memql_wholesale_a?sslmode=disable'`. Every `go test` carries `MEMQL_DATABASE_DSN="$WHOLESALE_DSN"`. Database lanes add `MEMQL_REQUIRE_DB=1` and `-p 1`. Never port 5432. Name packages; a bare `go test ./...` misses the engine and is not a verification. The whole tree is `make test`.
- Generated files are regenerated, never hand-edited. After a change to a concept anywhere (the pack's included): `make concept-snapshot` (it refuses a removal, which this plan never makes) and `make platform-graph`. After a change to a construct under `dsl/` (campaigns, shopify overlay, rbac seeds): `make sdk-gen`. Then the gates: `make sdk-gen-check`; `MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -count=1 -run 'TestConceptFieldSnapshotIsNotStale|TestPlatformGraph|TestOnlyAllowlistedPackagesStampInternalOrigin' .`; `MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -count=1 ./test/dslconformance/...`. The pack's own construct gates are `MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -count=1 ./packs/wholesalepack/ -run 'TestEveryConstructAnAdapterCallsIsDeclared|TestEveryPackBuiltinNamesARealCapability|TestWholesalePackTreeParses|TestPackShipsNoAutomationsAndNoLogic'`. If a gate names another stale file, run the make target its message names. These are "the generated-file gates" below.
- Fields are added, never removed or renamed (memql#5199); an enum gains values, never loses one. Existing mutation and builtin names and arguments stay; arguments are only added. Each concept this plan changes moves to `@version("1.1.0")` the first time it changes and stays there.
- Every wholesale mutation is `@serverOnly` from Task 2 on. A builtin takes no `@serverOnly`: a builtin only an automation may call refuses a call without internal origin in its handler, before it reads anything. The pack writes through `engineWriter`, which stamps `auth.ContextWithInternalOrigin` inline on its one `Execute`; the caller's actor still runs the mutation. Every new stamp in `packs/wholesalepack` is named in its entry of `call_origin_conformance_test.go`, and the entry's reason grows in the task that adds the stamp.
- Identities: automation-called builtins read under `auth.ContextWithSystemActor(ctx, provisionActor)` with `const provisionActor = "wholesale-provision"` and write under `auth.ContextWithUserActor(ctx, <the application's ownerUserId>)`. A cluster operator (`auth.IsClusterOperator(role)`) reads every store's applications and decides, adds or removes none.
- Derived row ids are bare slugs with no colon, built with the pack's `slugOf`: `wholesale-link-<purpose>-<slug(applicationId)>`, `wholesale-acceptance-<slug(applicationId)>`, `wholesale-email-<kind>-<slug(applicationId)>-<attempt>`. Lane request ids: `wholesale-<kind>-<slug(applicationId)>-<attempt>` (they must match `^[A-Za-z0-9_-]{8,160}$`). Staff notice request ids: `wholesale-staff-<event>-<slug(applicationId)>[-<slug(decisionId)>]`.
- The part is `app:deployables/wholesale`, `{RoleOwner, RoleDeveloper}`. Gated means `@requiresCapability("execute", "app:deployables/wholesale")`. Application-targeted (`v1:wholesale:application` by `applicationId`): `wholesaleRecordDecision`, `wholesaleGet`, `wholesaleRetryAccess`, `wholesaleRemoveBuyer`, `wholesaleResendEmail`. Site-targeted (`v1:platform:site` by `siteId`): `wholesaleList`, `wholesaleDecisionLog`, `wholesaleBuyers`, `wholesaleStoreSettings`, `wholesaleUpdateSettings`, `wholesaleDocuments`, `wholesalePublishDocument`, `wholesaleAddBuyer`, `wholesaleExportStore`. A site-targeted builtin also refuses a site not bound to the named store (serving or preview binding), as reviews' `siteForStore` does.
- Stages: `waitingForTerms`, `readyToReview` (both of state `submitted`), `approved`, `rejected`, `revoked`. Entitlement error codes: `no_settings`, `no_route`, `scope_missing`, `plan_ceiling`, `customer_refused`, `shopify_error`. Link refusals: `link_invalid`, `link_expired`, `link_used`. Email kinds: `terms`, `approved`, `rejected`, `revoked`. Staff events: `readyToReview`, `approved`, `rejected`, `revoked`, `accessNotGranted`.
- Exact sentences (verbatim, in Go refusals and the OS): "This application has not accepted the terms yet." / "Publish the terms, the MAP policy and the price sheet first." / "This store's wholesale program is not set up yet. The cluster's operators set it up." / the acceptance default "I have read and accept the Wholesale Terms and Conditions and the MAP Policy shown above." / the staff skip "the store names no address".
- No model or agent is called on any of these paths (W2). No file upload; documents are Markdown text in the row (X6).
- Several engine nodes: no new scheduled job and nothing kept in memory across requests. Every step a client automation calls is idempotent: applicant mail by lane request id, staff mail by outbox request id, a grant because a second `tagsAdd` changes nothing, a second `customerCreate` refused for a duplicate email re-reads and continues. The backfill is one operator call and safe to run twice at once.
- OS UI follows `clients/os/DESIGN.md`: kit components only; no dividers between rows (rule 1); facts in plain text, no pill, chip or badge in a row ("Record lists"); `Notice` for every refusal and failure, never a toast; filters behind `Refine` (rule 2); in-surface state is never a checkbox, but a form field may be (rule 10); acts in the one `ActionBar`, at most three, primary last, an act that is not legal is absent and never disabled (rule 12); a question in place uses `InPlaceAsk`; loading is `RecordListSkeleton` or `ContentSkeleton`, an empty state only after a successful empty read, a failed read is a `Notice` with Try again; a control whose part is not held is absent; tabs with `LocalTabs` ("A tab is a different kind of thing"). Check each screen in both themes and at both widths. Client checks, from `clients/os`: `npm run typecheck`, `npx vitest run <paths>`.
- Vocabulary: component, integration, pack; never "plugin". No emojis. The repository is public: no client, person, email address, store domain or instance hostname in code, tests, docs or commits. Fixtures use the Shopify harness's `acme-widgets.myshopify.com`, the pack tests' existing store ids, and `example.com` addresses. Write "a development store" and "the owner's instance".
- Commits are `<area>: <sentence>`, staged by explicit path (never `git add -A`), ending with the `Co-Authored-By:` line the implementing session's own instructions give.

## Review Focus

- **A second approval email.** The entitlement is one row at a derived id, so every re-provision and every Retry access writes another `granted` version and fires the client's "granted" automation again; a re-delivered event does the same on another node. A buyer expects one approval email. Test `TestASecondGrantedVersionSendsNoSecondApprovalEmail` in Task 14.
- **A link that still opens.** After Resend terms email, after a reject, or after a buyer is removed, the old link must stop opening; and a re-delivered application event must not mint a new token that kills the link already emailed. Tests `TestAReplacedTokenIsRefusedAsLinkInvalid` and `TestATermsLinkOfARejectedApplicationIsRefusedAsLinkUsed` in Task 13; `TestAPriceSheetLinkStopsOpeningOnceTheBuyerIsRemoved` and `TestASecondAutomaticTermsSendMintsNoNewLink` in Task 14.
- **One organization's staff reading another's application.** The part is held per organization, and a site-wide builtin names both a site and a store; a person holding the part in organization A who names A's site with B's store, or B's application id, must be refused. Tests `TestAPersonInOneOrganizationIsRefusedOnAnothersApplication` in Task 5 and `TestAStoreWideReadRefusesASiteNotBoundToTheStore` in Task 6.
- **An approval that silently grants nothing.** No route, no settings, a missing scope, or a client grant automation that is not wired leaves an approved application and no trade prices; a person expects the application to say why and offer Retry access. Tests `TestAnApprovalOnAStoreWithNoRouteLeavesAFailedRowNamingNoRoute` in Task 3 and the OS test "offers Retry access on an approved application with no access recorded" in Task 7.
- **An applicant email blocked by a newsletter unsubscribe.** An applicant who once unsubscribed from the store's newsletter, in the organization or cluster-wide, must still receive their own terms link; a hard bounce must still stop it. Tests `TestAnOrganizationUnsubscribeDoesNotStopARowMessage`, `TestAClusterWideUnsubscribeDoesNotStopARowMessage` and `TestAHardBounceSkipsARowMessage` in Task 11.

---

### Task 1: Workspace and baseline

**Files:** none changed.

- [ ] **Step 1:** Copy the git-ignored identity bundle: `cp -R /Users/znas/projects/memql/component/identity/web/static/. /Users/znas/projects/memql/.claude/worktrees/wholesale/component/identity/web/static/`.
- [ ] **Step 2:** In `sdk/ts` run `npm ci && npm run build`; in `clients/os` run `npm ci`.
- [ ] **Step 3:** `docker exec memql-testdb psql -U memql -d postgres -c 'create database memql_wholesale_a'`, then `export WHOLESALE_DSN='postgres://memql:memql_dev@127.0.0.1:55432/memql_wholesale_a?sslmode=disable'`.
- [ ] **Step 4:** Baseline, all expected PASS: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -p 1 -count=1 ./packs/wholesalepack/ ./packs/reviewspack/ ./component/campaigns/ ./integrations/shopify/ ./component/outbound/ ./component/auth/`; `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -p 1 -count=1 ./component/memql/ -run 'Shopper|Organization|Capability|Parts|Seed|Reviews'`; `MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -count=1 ./component/server/ ./component/edge/ -run 'Shopper'`; the generated-file gates; in `clients/os`, `npm run typecheck` and `npx vitest run test/deployables test/email`. Record each command and its result. A failure that exists before any change is the controller's call: stop and report it.

### Task 2: Provisioning works from a client automation (U1)

**Files:**
- Create: `packs/wholesalepack/origin.go`, `packs/wholesalepack/automation_db_test.go`
- Modify: `packs/wholesalepack/entitlement.go`, `packs/wholesalepack/application.go`, `packs/wholesalepack/reader.go`, `packs/wholesalepack/dsl/mutations.memql`, `packs/wholesalepack/dsl/concepts.memql` (`applicationDecision`), `packs/wholesalepack/dsl/builtins.memql` (descriptions of the two provisioning builtins), `call_origin_conformance_test.go`
- Test: `packs/wholesalepack/entitlement_test.go`, `packs/wholesalepack/pack_test.go`, `packs/wholesalepack/live_e2e_test.go`, `packs/wholesalepack/twoclients_test.go`

**Interfaces:**
- Produces, in `origin.go`: `func requireInternal(ctx context.Context, construct string) error` (nil when `auth.OriginFromContext(ctx).IsInternal()`, else `wholesale: <construct> is called by the store's automation, not by a client`).
- Produces, in `entitlement.go`: `const provisionActor = "wholesale-provision"`; `wholesaleProvisionEntitlement` and `wholesaleRevokeEntitlement` call `requireInternal` first, then read the application, its decisions, the settings and the store plan under the system actor, and call the adapter and write the entitlement under the application's merchant. `func (p *Provider) asMerchant(ctx context.Context, app map[string]any) (context.Context, error)` (refuses an application with no `ownerUserId`).
- Produces, in `reader.go`: `engineWriter.Write` stamps internal origin inline on its one `Execute`.
- Produces, in the DSL: every mutation in `mutations.memql` is `@serverOnly`. `createApplicationRow` takes `ownerUserId string!` as an argument (accepted, no longer stamped from the actor). `appendApplicationDecision` takes `ownerUserId string` (accepted). `writeEntitlementRow` takes `ownerUserId string!` (accepted, replacing the actor stamp). `applicationDecision` gains `ownerUserId string` ("the application's merchant, copied off it"), `@version("1.1.0")`; its row tier is unchanged until Task 5.
- Every writer of these mutations passes `ownerUserId`: `submitApplication` the borrowed site owner's id; `recordDecision` and `writeEntitlement` the application's `ownerUserId`.

Decisions:
- N1 is a suspicion read from the code. Step 1's database test settles it. If that test PASSES before any change, say so in the commit body, strike N1's row and its "On N1 and G6" mention from the spec's section 2 in the same commit, and still make every change below: X1 and X2 stand on G1 and G2, and X3 is what keeps the builtins from depending on whoever calls them.
- Existing tests that call the two provisioning builtins directly move to `auth.ContextWithInternalOrigin(asSiteOwner(merchant))`.
- The allowlist entry for `packs/wholesalepack`, beside reviews': "wholesale writes -- REQUEST-DERIVED and automation-run. Each judging builtin runs one @serverOnly pack mutation through reader.go's engineWriter, whose single INLINE stamp is the argument to that one Execute, after the builtin's own checks; wholesaleProvisionEntitlement and wholesaleRevokeEntitlement act only for a caller that already arrived with internal origin, refusing every other call before they read (entitlement_test.go)." Later tasks extend it where they add a stamp.

- [ ] **Step 1: Write the failing tests**

```go
// automation_db_test.go, database lane. Boots liveWholesaleEngine with testdata/clients/contoso mounted,
// as TestLiveE2E_BothFixtureClientsBootOverTheUneditedPack does, and a fake adapter registered with RegisterAdapter.
func TestAClientGrantAutomationProvisionsAnApprovedApplication(t *testing.T)
// open with the fake adapter, submit, decide approve as the merchant; within 10s entitlementForApplication
// (read as the merchant) answers state "granted", ownerUserId == merchant; the fake adapter was called once
func TestADirectClientCallToAppendApplicationDecisionIsRefused(t *testing.T)
// as the merchant with client origin: error; decisionsForApplication still answers nothing

// entitlement_test.go
func TestProvisionAndRevokeRefuseACallWithoutInternalOrigin(t *testing.T)   // a fake reader that fails the test on any read
func TestProvisionReadsAsTheSystemAndWritesAsTheMerchant(t *testing.T)      // reads: actor "wholesale-provision"; adapter call and write: actor == app ownerUserId; write args ownerUserId == merchant
func TestTheWriterStampsInternalOriginOnItsOneCall(t *testing.T)            // a fake engine records the origin of each Execute
// pack_test.go
func TestEveryPackMutationIsServerOnly(t *testing.T)                        // parses the tree; every mutation carries @serverOnly
func TestADecisionRecordsTheApplicationsMerchant(t *testing.T)              // recordDecision's write args carry ownerUserId == app ownerUserId
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -p 1 -count=1 ./packs/wholesalepack/ -run 'GrantAutomation|DirectClientCall|WithoutInternalOrigin|AsTheSystem|StampsInternalOrigin|ServerOnly|ApplicationsMerchant'` and `MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -count=1 -run TestOnlyAllowlistedPackagesStampInternalOrigin .`. Expected: FAIL (the automation test finds no entitlement row, "not readable by this caller"; the direct call succeeds; the allowlist test names `packs/wholesalepack` once the stamp exists).
- [ ] **Step 3:** Implement the Go and the DSL, add the allowlist entry, move existing direct calls to internal origin, then regenerate (a concept changed).
- [ ] **Step 4:** Run the same commands, the package whole in the database lane, and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `wholesale: provisioning runs from the store's automation, reading as the system and writing as the merchant`.

### Task 3: An approval that grants nothing says why (U2)

**Files:**
- Modify: `packs/wholesalepack/entitlement.go`, `packs/wholesalepack/adapter_shopify_b2b.go` (`outcomeFromReply`), `packs/wholesalepack/adapter_customer_tag.go`, `packs/wholesalepack/dsl/concepts.memql` (`entitlement`), `packs/wholesalepack/dsl/mutations.memql` (`writeEntitlementRow`)
- Test: `packs/wholesalepack/entitlement_test.go`

**Interfaces:**
- Consumes: Task 2's merchant write path.
- Produces, in the DSL: `entitlement` gains `errorCode string`, `shopifyCustomerId string`, `customerCreated bool`, `@version("1.1.0")`; `writeEntitlementRow` accepts all three.
- Produces, in Go: `const AdapterNone = "none"`; `const ErrorNoSettings = "no_settings"`, `ErrorNoRoute = "no_route"`, `ErrorScopeMissing = "scope_missing"`, `ErrorPlanCeiling = "plan_ceiling"`, `ErrorCustomerRefused = "customer_refused"`, `ErrorShopifyError = "shopify_error"`; `AdapterRefusal` gains `Code string`; `Outcome` gains `CustomerID string` and `CustomerCreated bool`; `func errorCodeOf(err error) string`.
- The reply contract an integration answers (Task 8 fills it from Shopify): `{status: "ok", reference, detail, customerId, customerCreated}` or `{status: "refused", reason, code}`. `outcomeFromReply` reads `code` into `AdapterRefusal.Code` and `customerId`, `customerCreated` into `Outcome`.

Decisions, the whole table:
- No settings row for the store: `failed`, `no_settings`, adapter `none`, no adapter call. Settings with an empty adapter: `failed`, `no_route`, adapter `none`, no adapter call (N3). Both write the row instead of returning an error.
- `Available` refusing (the B2B adapter on an unreadable plan) and a `WholesaleCeilingError` reply: `failed`, `plan_ceiling`.
- A refusal with a code: `failed` and that code. A refusal with no code: `failed`, `customer_refused`. The `unconfigured` reply (no Shopify connector on the cluster): `failed`, `shopify_error`, with its existing sentence.
- Any other error: `pending`, `shopify_error`, the error's text in `error`.
- Success: `errorCode` and `error` empty; `shopifyCustomerId` and `customerCreated` from the outcome.
- Revoke (X17): a prior row whose adapter is `none`, or no prior row at all, writes `revoked` with adapter `none` and calls no adapter.

- [ ] **Step 1: Write the failing tests**

```go
func TestAnApprovalOnAStoreWithNoSettingsLeavesAFailedRowNamingNoSettings(t *testing.T)
func TestAnApprovalOnAStoreWithNoRouteLeavesAFailedRowNamingNoRoute(t *testing.T)   // state failed, errorCode "no_route", adapter "none", zero adapter calls
func TestAPlanThatCannotBeReadIsPlanCeiling(t *testing.T)
func TestARefusalCarryingScopeMissingIsRecordedAsScopeMissing(t *testing.T)        // reply {status:"refused", code:"scope_missing"}
func TestARefusalWithNoCodeIsCustomerRefused(t *testing.T)
func TestAnUnconfiguredConnectorIsAFailedShopifyError(t *testing.T)
func TestATransientErrorIsPendingWithShopifyError(t *testing.T)
func TestASuccessRecordsTheCustomerAndClearsTheCode(t *testing.T)                  // reply customerId "gid://shopify/Customer/1", customerCreated true
func TestRevokingAnEntitlementThatNeverReachedAStoreCallsNoAdapter(t *testing.T)   // prior adapter "none": revoked, zero calls
func TestRevokingWithNoEntitlementRowWritesRevoked(t *testing.T)
```

  `TestNoAdapterConfiguredRefusesByName` changes to assert the row (`no_route`) instead of a returned error.
- [ ] **Step 2:** Run `MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -count=1 ./packs/wholesalepack/ -run 'NoSettings|NoRoute|PlanThatCannot|ScopeMissing|NoCode|Unconfigured|TransientError|RecordsTheCustomer|NeverReachedAStore|NoEntitlementRow|NoAdapterConfigured'`. Expected: FAIL.
- [ ] **Step 3:** Implement, then regenerate.
- [ ] **Step 4:** Run the same command, the package whole in the database lane, and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `wholesale: an approval that cannot grant leaves a row naming why`.

### Task 4: Every row carries its organization, and old rows are backfilled (U5)

**Files:**
- Create: `packs/wholesalepack/backfill.go`, `packs/wholesalepack/account_db_test.go`, `app/transport_shopper_surface_test.go`
- Modify: `component/memql/shopper_surface.go`, `component/memql/shopper_extension.go` (and the loader that registers extensions), `component/server/shopper_handler.go`, `app/transport_shopper_surface.go`, `packs/wholesalepack/application.go`, `packs/wholesalepack/entitlement.go`, `packs/wholesalepack/pack.go`, `packs/wholesalepack/dsl/{concepts,mutations,queries,builtins}.memql`
- Test: `component/memql/shopper_surface_test.go`, `component/server/shopper_handler_test.go`, `component/server/shopper_extension_handler_test.go`, `packs/wholesalepack/application_test.go`

**Interfaces:**
- Produces, engine-wide: `accountId` joins `reservedShopperFieldNames` ("stamped from the organization of the site the request arrived on"); `ShopperExtension.AcceptsAccount bool`, set by the loader when the extension's mutation declares an `accountId` argument; `ShopperSite.AccountID string`, read off the site row's `accountId` by `app/transport_shopper_surface.go`; the handler stamps `accountId` into the pack construct's arguments, and into the extension's when `AcceptsAccount`, after the declared fields so a form-supplied copy cannot win.
- Produces, in the DSL: `application` gains `accountId string`, `source enum("storefront", "staff")` (absent reads as storefront), `addedBy string`; `applicationDecision` gains `accountId string`; `entitlement` gains `accountId string`; `wholesaleSettings` gains `accountId string`, `siteId string`. Row tiers of `application`, `entitlement` and `wholesaleSettings` become `@rowAuthz(owner="ownerUserId", clusterOwner, account="accountId")` (the decision's tier changes in Task 5). `createApplicationRow` accepts `accountId`, `source`, `addedBy`; `appendApplicationDecision` and `writeEntitlementRow` accept `accountId`; `setWholesaleSettingsRow` accepts `accountId`, `siteId`. New `@serverOnly` mutations `backfillApplicationRow`, `backfillDecisionRow`, `backfillEntitlementRow`, `backfillSettingsRow`, each writing the row at its own id with every field it held plus `accountId` (and `ownerUserId` for a decision); new `@serverOnly` `query application applicationsWithoutAccount` (cluster tier, paginated); new `builtin wholesaleBackfillAccounts { storeId string }`.
- Produces, in Go: `wholesaleSubmitApplication` writes the stamped `accountId` and `source: "storefront"`; decisions and entitlements copy `accountId` off the application. `func (p *Provider) backfillAccounts(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error)`, answering `{tied int, alreadyTied int, untied: [{id, reason}]}`; `func accountForApplication(app map[string]any, sites []map[string]any) (account, reason string)`.

Decisions:
- `wholesaleBackfillAccounts` admits internal origin or a cluster operator and refuses anybody else before it reads (X19). It reads under the system actor and writes as each row's own owner. The account comes from the application's `siteId` site row; with no `siteId`, from the one site whose serving or preview binding is the application's store. Untied reasons, verbatim: "no site serves this store", "two sites serve this store", "its site has no organization". It ties the application, then its decisions (with `ownerUserId` from the application), its entitlement, and the store's settings. A row already tied is counted, never rewritten.
- An extension whose mutation declares no `accountId` argument is handed none and still writes.

- [ ] **Step 1: Write the failing tests**

```go
// component/memql
func TestAccountIdIsAReservedShopperName(t *testing.T)
func TestAnExtensionWithAnAccountIdArgumentAcceptsTheAccount(t *testing.T)
// component/server
func TestTheHandlerStampsTheSitesAccountIntoThePacksArguments(t *testing.T)
func TestTheHandlerStampsTheAccountIntoAnExtensionThatAcceptsIt(t *testing.T)
func TestAnExtensionWithoutAnAccountArgumentIsNotHandedOne(t *testing.T)
func TestAFormCannotSupplyItsOwnAccountId(t *testing.T)                 // a posted accountId=other loses to the site's
// app
func TestTheShopperSiteCarriesItsAccount(t *testing.T)
// packs/wholesalepack/application_test.go
func TestADecisionAndAnEntitlementCopyTheApplicationsAccount(t *testing.T)
// packs/wholesalepack/account_db_test.go, database lane; organizations and sites seeded as
// packs/reviewspack/live_helpers_e2e_test.go's seedOrganization and seedSiteIn do
func TestASubmissionOnASiteOfOrganizationAWritesTheApplicationAndTheExtensionRowWithA(t *testing.T) // northwind fixture mounted, its mutation given accountId
func TestTheBackfillTiesAnOldApplicationItsDecisionsAndItsEntitlement(t *testing.T)
func TestTheBackfillReportsARowWithNoResolvableSite(t *testing.T)       // untied[0].reason == "no site serves this store"
func TestTheBackfillRunTwiceTiesNothingMore(t *testing.T)               // second run: tied 0, alreadyTied == first run's tied
func TestTheBackfillRefusesACallerWhoIsNotAClusterOperator(t *testing.T)
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -p 1 -count=1 ./component/memql/ ./component/server/ ./app/ ./packs/wholesalepack/ -run 'AccountId|AcceptsTheAccount|SitesAccount|AccountIntoAn|NotHandedOne|OwnAccountId|CarriesItsAccount|CopyTheApplicationsAccount|OrganizationAWrites|Backfill'`. Expected: FAIL.
- [ ] **Step 3:** Implement, then regenerate.
- [ ] **Step 4:** Run the same command, `./packs/wholesalepack/` whole in the database lane, `MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -count=1 ./component/server/ -run Shopper`, and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `wholesale: every row carries the organization of the site it came through`.

### Task 5: The wholesale part, and deciding as the store's own people (U6, first half)

**Files:**
- Create: `component/memql/wholesale_capability_test.go`, `packs/wholesalepack/gate_test.go`, `packs/wholesalepack/settings.go`, `packs/wholesalepack/access_db_test.go`
- Modify: `component/auth/rbac_model.go`, `dsl/rbac/seeds.memql`, `component/memql/organization_action_capabilities.go`, `packs/wholesalepack/application.go`, `packs/wholesalepack/entitlement.go`, `packs/wholesalepack/pack.go`, `packs/wholesalepack/dsl/{concepts,mutations,builtins}.memql`
- Test: `packs/wholesalepack/application_test.go`, `packs/wholesalepack/live_e2e_test.go`

**Interfaces:**
- Consumes: Task 4's `accountId` on every row and `siteId` on settings; Task 2's identities; Task 3's entitlement path.
- Produces, engine-wide: `"app:deployables/wholesale": {RoleOwner, RoleDeveloper}`; seeds `cap-owner-execute-app-deployables-wholesale` and `cap-developer-execute-app-deployables-wholesale` beside reviews'; the part in `organizationDeployableCapability`; every builtin named in the Global Constraints' gate list as a case of `organizationCapabilityTarget` (names not yet declared are harmless there).
- Produces, in the DSL: `applicationDecision`'s tier becomes `@rowAuthz(owner="ownerUserId", clusterOwner, account="accountId")` (X5). `wholesaleSettings` gains `staffEmail string`. New `@serverOnly mutation wholesaleSettings putWholesaleSettingsRow` writing the whole merged row at `SettingsRowID(storeId)` with every settings field. `wholesaleRecordDecision` gains the part. `wholesaleSetSettings` gains `siteId string`. New gated builtins: `wholesaleRetryAccess { applicationId string! }`, `wholesaleStoreSettings { siteId string!, storeId string! }`, `wholesaleUpdateSettings { siteId string!, storeId string!, applicationsOpen bool, entitlementAdapter string, introText string, staffEmail string }`.
- Produces, in Go: `type Settings struct { Exists, ApplicationsOpen bool; EntitlementAdapter, IntroText, StaffEmail, AccountID, SiteID string }` and `func EffectiveSettings(row map[string]any) Settings` in `settings.go` (later tasks add fields); `func deciderOf(ctx context.Context) (string, error)`; `func (p *Provider) appendDecision(ctx context.Context, app map[string]any, transition, note string) error` (the one write path of a decision, used by `recordDecision` and Task 15); `func (p *Provider) siteForStore(ctx context.Context, siteID, storeID string) (map[string]any, error)`; `const StageWaitingForTerms = "waitingForTerms"`, `StageReadyToReview = "readyToReview"`; `func StageOf(state string, termsRequired, accepted bool) string`; `wholesaleApplicationState`'s reply gains `stage`.
- `wholesaleStoreSettings` answers `{exists, applicationsOpen, entitlementAdapter, introText, staffEmail, adapters: [registered names], may: {update bool}}`. Tasks 12, 13 and 14 add fields.

Decisions:
- `deciderOf` refuses a caller with no user ("wholesale: a decision needs a signed-in person") and a cluster operator ("wholesale: the cluster's operators host the store's applications and do not decide them; a person on the store's side decides") (X4).
- `StageOf`: `submitted` with `termsRequired` and not `accepted` is `waitingForTerms`; other `submitted` is `readyToReview`; any other state is itself. Until Task 13 every caller passes `accepted` false, and until Task 12 there is no `termsRequired` to read, so it is false.
- `wholesaleRetryAccess`: state `approved` runs the provisioning path; state `revoked` with an entitlement row that exists and is not `revoked` runs the revoke path; anything else is refused: "wholesale: there is no access to retry for an application that is <state>". It runs `applyEntitlement` in Go under Task 2's identities; it is not an internal-origin call.
- `wholesaleSetSettings` admits internal origin or a cluster operator only. With `siteId` it checks the site serves the store and writes `siteId` and the site's `accountId`.
- `wholesaleUpdateSettings` merges into the existing row through `putWholesaleSettingsRow` and refuses a store with no settings row with "This store's wholesale program is not set up yet. The cluster's operators set it up." An unknown adapter is refused by name; `""` and `none` both mean none. `staffEmail` is at most 320 characters with exactly one `@`, or empty.
- The pack's `gate_test.go` holds `var gatedBuiltins = map[string][2]string{...}` (name to concept and argument, from the Global Constraints' list) and asserts each builtin the tree declares carries the part and the argument; later tasks' builtins enter it as they are declared.
- `TestTheDeployablesPartsAreDeclaredOnTheirConstructs` loads only the platform tree, so it cannot see the pack; the wholesale part is pinned the way reviews' is, by `component/memql/wholesale_capability_test.go` and the pack's `gate_test.go`.
- Existing tests that decide as a cluster-owner actor move to a non-operator merchant; existing calls of `wholesaleSetSettings` as the site owner move to internal origin.

- [ ] **Step 1: Write the failing tests**

```go
// component/memql/wholesale_capability_test.go
func TestTheWholesaleBuiltinsNameTheirOrganizationTarget(t *testing.T)   // every name of the gate list maps to its (concept, argument)
// packs/wholesalepack/gate_test.go
func TestEveryGatedBuiltinDeclaresTheWholesalePartAndItsTarget(t *testing.T)
// application_test.go
func TestTheClustersOwnOperatorsCannotDecide(t *testing.T)
func TestStageOfASubmittedApplication(t *testing.T)                      // (submitted,true,false) waitingForTerms; (submitted,true,true) and (submitted,false,false) readyToReview; (approved,true,false) approved
func TestRetryAccessIsRefusedByNameForARejectedApplication(t *testing.T)
func TestRetryAccessRevokesWhenTheTagIsStillOn(t *testing.T)
func TestUpdateSettingsMergesAndRefusesAStoreWithNoSettings(t *testing.T) // exact sentence
func TestOnlyAnOperatorOrInternalOriginSetsUpAStore(t *testing.T)
func TestAStaffAddressWithoutAnAtIsRefused(t *testing.T)
// access_db_test.go, database lane; organizations, groups and grants as packs/reviewspack/live_people_e2e_test.go seeds them
func TestAPersonHoldingThePartApprovesTheirOrganizationsApplication(t *testing.T)
func TestAPersonInOneOrganizationIsRefusedOnAnothersApplication(t *testing.T)   // wholesaleRecordDecision and wholesaleRetryAccess on B's application: capability_not_held
func TestAClusterOperatorCannotDecide(t *testing.T)
func TestTheSiteOwnerReadsTheStaffMembersDecision(t *testing.T)                 // N2: decisionsForApplication as the merchant answers the staff member's row
func TestADecisionWrittenBeforeThisVersionIsReadableByTheMerchantAfterTheBackfill(t *testing.T) // seeded without ownerUserId and accountId, as reviews' live helpers seed rows
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -p 1 -count=1 ./component/memql/ ./packs/wholesalepack/ -run 'WholesaleBuiltinsName|GatedBuiltin|CannotDecide|StageOf|RetryAccess|UpdateSettings|SetsUpAStore|StaffAddress|HoldingThePart|OneOrganization|StaffMembersDecision|BeforeThisVersion'`. Expected: FAIL.
- [ ] **Step 3:** Implement, then `make sdk-gen` (seeds changed) and regenerate the concept snapshot and platform graph.
- [ ] **Step 4:** Run the same command, `MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -count=1 ./component/auth/ ./component/memql/ -run 'Seed|Parity|Deployables|Organization'`, `./packs/wholesalepack/` whole in the database lane and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `wholesale: the store's own people decide its applications, through an organization part`.

### Task 6: The read builtins the Wholesale section opens (U6, second half)

**Files:**
- Create: `packs/wholesalepack/reads.go`, `packs/wholesalepack/export.go`, `packs/wholesalepack/reads_test.go`, `packs/wholesalepack/reads_db_test.go`
- Modify: `component/memql/shopper_extension.go`, `packs/wholesalepack/pack.go`, `packs/wholesalepack/dsl/{builtins,queries}.memql`
- Test: `component/memql/shopper_surface_test.go`, `packs/wholesalepack/gate_test.go`

**Interfaces:**
- Consumes: Task 5's gate, `siteForStore`, `StageOf`, `EffectiveSettings`, `deciderOf`.
- Produces, in `component/memql`: `type ExtensionField struct { Name, Label string; Value any }`; `type ExtensionRow struct { Concept, Primary, Secondary string; Fields []ExtensionField }`; `func (e *MemQLEngine) ShopperExtensionRow(ctx context.Context, pack, form, rowID string) (ExtensionRow, bool, error)` reading the extension concept's row at `rowID` under `ctx`, returning every declared field except the reserved names, labelled by the field name split at case boundaries into lower-case words with the first capitalised (`taxNumber` is "Tax number"), and `Primary`, `Secondary` from the concept's `@displayCard` (X14). The pack reaches it through `type extensionReader interface { ShopperExtensionRow(ctx context.Context, pack, form, rowID string) (memql.ExtensionRow, bool, error) }`, asserted off the engine handle in `NewProvider`.
- Produces, in the DSL: `query applicationDecision decisionsForStore { args { storeId string! } }` (newest first, paginated); gated builtins `wholesaleList { siteId string!, storeId string!, stage string, search string, limit int, after string }`, `wholesaleGet { applicationId string! }`, `wholesaleDecisionLog { siteId string!, storeId string!, search string, limit int, after string }`, `wholesaleBuyers { siteId string!, storeId string!, search string, limit int, after string }`, `wholesaleExportStore { siteId string!, storeId string! }`.
- Replies:
  - `wholesaleList`: `{rows: [{applicationId, companyName, applicantName, applicantEmail, stage, primary, secondary, createdAt}], counts: {waitingForTerms, readyToReview, approved, rejected, revoked}, next, truncated}`. `stage` is one of the five stages or `all`; default `readyToReview`; `readyToReview` is oldest first, every other stage newest first. Search is a case-insensitive substring of company, applicant or email.
  - `wholesaleGet`: `{application: {id, storeId, siteId, companyName, applicantName, applicantEmail, source, addedBy, addedByName, createdAt}, state, stage, details: {state: "shown" | "none" | "unreadable", fields: [{name, label, value}]}, decisions: [{transition, decidedBy, decidedByName, note, createdAt}], entitlement: {state, errorCode, error, adapter, shopifyCustomerId, customerCreated, updatedAt} | null, operator bool, acts: [string]}`. Tasks 13 and 14 add `acceptance`, `termsLink` and `emails`.
  - `wholesaleDecisionLog`: `{rows: [{applicationId, companyName, transition, decidedBy, decidedByName, note, createdAt}], next}`.
  - `wholesaleBuyers`: `{rows: [{applicationId, companyName, applicantEmail, accessState, errorCode, grantedAt, shopifyCustomerId}], next}`, every application whose state is `approved`; `accessState` is the entitlement's state or `none`.
  - `wholesaleExportStore`: `{fileName: "wholesale-<slug(storeId)>-<YYYYMMDD>.json", json}`, the JSON holding `applications` (each with its `details`), `decisions`, `entitlements`, `settings`. Tasks 12 to 14 add their rows. It never carries a token hash.
- Produces, in Go: `type ActsInput struct { Stage, AccessState string; EntitlementExists, Operator bool }` and `func ActsFor(in ActsInput) []string`, with acts `reject`, `approve`, `remove`, `retryAccess`: operator none; `waitingForTerms` `[reject]`; `readyToReview` `[reject, approve]`; `approved` `[remove]` plus `retryAccess` when the access is not `granted`; `revoked` `[retryAccess]` only when an entitlement exists and is not `revoked`; `rejected` none. Task 14 adds `resendTerms` and `resendApproved`.

Decisions:
- `details.state`: `none` for `source: staff` or when no extension is declared; `unreadable` when the caller's read finds no row but a system read does; `shown` otherwise. Names of people (`decidedByName`, `addedByName`) are read through `userByIdSystem` inline under internal origin, as reviews' `canModerate` does, only after the gate admitted the caller.
- The list and the buyers read at most 1000 applications of the store per call, 100 a page, and answer `truncated: true` past that.

- [ ] **Step 1: Write the failing tests**

```go
// component/memql
func TestAnExtensionRowIsLabelledByItsFieldNamesInPlainWords(t *testing.T)   // taxNumber -> "Tax number"; storeId and submissionId absent
// reads_test.go
func TestTheListDefaultsToReadyToReviewOldestFirst(t *testing.T)
func TestTheListCountsEveryStage(t *testing.T)
func TestTheListSearchesCompanyApplicantAndEmail(t *testing.T)
func TestTheListCarriesTheExtensionsTwoDisplayFacts(t *testing.T)
func TestActsFollowTheStageAndTheAccess(t *testing.T)                          // the table above, row by row
func TestAnOperatorIsOfferedNoAct(t *testing.T)
func TestAStaffAddedApplicationHasNoFurtherDetails(t *testing.T)
func TestAnExtensionRowTheCallerCannotReadIsUnreadable(t *testing.T)
func TestTheExportCarriesNoTokenHash(t *testing.T)
// gate_test.go: the five builtins join gatedBuiltins
// reads_db_test.go, database lane
func TestAStoreWideReadRefusesASiteNotBoundToTheStore(t *testing.T)           // A's site with B's store: refused for wholesaleList and wholesaleExportStore
func TestAPersonHoldingThePartListsAndOpensTheirOrganizationsApplications(t *testing.T)
func TestAClusterOperatorReadsEveryTab(t *testing.T)
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -p 1 -count=1 ./component/memql/ ./packs/wholesalepack/ -run 'PlainWords|TheList|ActsFollow|OfferedNoAct|NoFurtherDetails|IsUnreadable|NoTokenHash|GatedBuiltin|NotBoundToTheStore|ListsAndOpens|ReadsEveryTab'`. Expected: FAIL.
- [ ] **Step 3:** Implement, then regenerate if the platform graph names the new builtins.
- [ ] **Step 4:** Run the same command, `./packs/wholesalepack/` whole in the database lane and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `wholesale: the store's people read its applications, buyers and decisions through gated builtins`.

### Task 7: The Wholesale section, applications and settings (U11)

**Files:**
- Create: `clients/os/src/apps/deployables/wholesale/WholesaleSection.tsx`, `ApplicationsTab.tsx`, `ApplicationPage.tsx`, `WholesaleSettingsPage.tsx`, `model.ts`, `useWholesale.ts` (all under `clients/os/src/apps/deployables/wholesale/`), `clients/os/test/deployables/wholesale.test.tsx`, `clients/os/test/deployables/wholesaleModel.test.ts`
- Modify: `clients/os/src/apps/deployables/parts.ts`, `clients/os/src/apps/deployables/page/DeployablePage.tsx`, `clients/os/src/apps/deployables/page/DeployableWorkspace.tsx` (`WorkspaceDetail` gains `"wholesale"`), `clients/os/test/deployables/harness.tsx`, `clients/os/test/deployables/parts.test.tsx`, `clients/os/test/seededAccess.ts` (where it lists the deployables parts)

**Interfaces:**
- Consumes: Task 6's replies; Task 5's `wholesaleStoreSettings`, `wholesaleUpdateSettings`, `wholesaleRecordDecision`, `wholesaleRetryAccess`.
- Produces, in `useWholesale.ts`, following `reviews/useReviews.ts` (its `builtinCall` and `refusalWords`): `interface WholesaleScope { siteId: string; storeId: string }`; `useWholesaleList(scope, stage, search, revision)`; `useWholesaleApplication(applicationId: string, revision: number): Reading<WholesaleApplication>`; `useWholesaleSettings(scope, revision): Reading<WholesaleSettings>`; `useWholesaleWrites(scope): { decide(applicationId: string, transition: "approve" | "reject" | "revoke", note: string): Promise<WriteOutcome>; retryAccess(applicationId: string): Promise<WriteOutcome>; updateSettings(changes: Partial<WholesaleSettingsChanges>): Promise<WriteOutcome> }`.
- Produces, in `model.ts`: `type WholesaleStage`; `stageWords(stage)`; `accessSentence(errorCode: string, error: string): string`; `actLabel(act: string): string`.
- Produces: `wholesale` in `DEPLOYABLE_PARTS`, `NO_PARTS` (false) and `ALL_PARTS` (true); `wholesale?: {...}` on the harness's `FakeSeed`, answering the reads and recording the writes.

Decisions and copy:
- Mounted as Reviews is: an `IconButton` "Wholesale" in the Head, drawn only with `can.wholesale` on a `shopify_storefront`, not system-owned, that fronts a store; the section takes the pane with its Head and `LocalTabs` Applications and Settings (Task 16 adds Buyers and Documents).
- Refine stages and words: Waiting for terms, Ready to review (default), Approved, Rejected, Removed, All. The stored `revoked` is drawn "Removed" everywhere (X21). A row: company as the name, applicant and email beneath, the two display facts as plain text, "waited <n> days" while submitted or the date otherwise; the stage word only under All.
- Empty: "Nothing ready to review" (or "Nothing <stage words>"). A closed store's Head meta: "Closed to new applications". For an operator, a `Caption` once on the list: "You can read this store's applications. Its own people decide them."
- One application: `Details` with *Application*, *Their details* ("No further details"; "These details are not readable to you"), *Access*, *Decisions*. Access sentences, verbatim: `no_route` "No access route is chosen in Settings."; `no_settings` "This store has no wholesale settings."; `scope_missing` "The store has not allowed MemQL to change customers. Its owner approves the new permissions from the store's admin."; `plan_ceiling` "The store's plan allows no more catalogs."; `customer_refused` and `shopify_error` "Shopify refused: " and the entitlement's `error`. Granted: "Trade prices on"; none: "Not granted yet".
- The bar draws exactly the engine's `acts`, mapped: `reject` Reject, `approve` Approve, `remove` Remove, `retryAccess` Retry access; primary last. Reject and Remove ask in place with an optional note (`TextArea`) and "Reject it / Keep it" or "Remove them / Keep them". Remove calls `wholesaleRecordDecision` with `revoke` until Task 16 moves it to `wholesaleRemoveBuyer`. After a write the page reads again; a refusal stands as a `Notice` above the bar with the engine's sentence.
- Settings slice one: *Applications* Open or closed (a choice); *Staff* Staff address; *Access* the route (Customer tag, Shopify B2B, None). A store with no settings shows the not-set-up sentence and no controls.

- [ ] **Step 1: Write the failing tests**

```ts
// wholesaleModel.test.ts
it("calls a revoked application Removed")
it("gives each error code its sentence")
// wholesale.test.tsx
it("draws no Wholesale button for somebody without the wholesale part")
it("draws no Wholesale button for a deployable that fronts no store")
it("lists Ready to review by default, oldest first, and asks the stage behind Refine")
it("draws the extension's two display facts as plain text, with no badge")
it("says Closed to new applications in the Head when the store is closed")
it("tells an operator once that the store's people decide, and draws no decide act")
it("draws Reject and Approve, Approve last, for an application ready to review")
it("draws only Reject for an application waiting for terms")
it("asks in place before rejecting and sends the note")
it("shows the engine's refusal as a Notice above the bar when somebody decided first")
it("offers Retry access on an approved application with no access recorded")
it("draws Retry access on a removed application only while the tag is still on")
it("says why access failed for each error code")
it("saves the route and the staff address, and says a store is not set up")
it("shows a failed read as a Notice with Try again")
```

- [ ] **Step 2:** In `clients/os` run `npx vitest run test/deployables/wholesale.test.tsx test/deployables/wholesaleModel.test.ts test/deployables/parts.test.tsx`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, `npm run typecheck` and `npx vitest run test/deployables test/settings test/users`. Expected: PASS. Check the section in both themes and at both widths.
- [ ] **Step 5: Commit** `os: a storefront's Wholesale section, where the store's people decide applications`.

### Task 8: The tag route finds or creates the customer, and only the pack may call it (U3)

**Before starting:** the store-mirror branch must have landed on `main` (W13). The controller rebases `feat/wholesale-end-to-end` onto `main`, resolves conflicts, then runs `make sdk-gen`, `make concept-snapshot` and `make platform-graph` and commits anything they changed as `build: regenerate after rebasing onto main`, and reruns Task 1's Step 4 suites. Do not start this task on a branch that has not been rebased: `integrations/shopify` is being changed there, and the managed scope set with `read_customers` and `write_customers` arrives with it.

**Files:**
- Modify: `integrations/shopify/wholesale.go`, `integrations/shopify/wholesale_calls.go`, `integrations/shopify/wholesale_capabilities.go`, `integrations/shopify/connect.go`, `dsl/shopify/overlay/builtins.memql`, `packs/wholesalepack/adapter_customer_tag.go`, `packs/wholesalepack/entitlement.go` (`engineCaller`, and the comment that says `failed`), `packs/wholesalepack/dsl/builtins.memql` (the provisioning description's `failed` claim), `docs/public/build/consuming-the-wholesale-pack.md` (the `failed` claim near line 209), `call_origin_conformance_test.go` (the pack's reason)
- Test: `integrations/shopify/wholesale_test.go`, `integrations/shopify/wholesale_capabilities_test.go` (new), `packs/wholesalepack/entitlement_test.go`

**Interfaces:**
- Consumes: Task 3's reply contract.
- Produces: `func (c *Connector) TagWholesaleCustomer(ctx context.Context, store Store, email, name, tag string) (gid string, created bool, err error)`; `func (c *Connector) findOrCreateCustomer(ctx context.Context, store Store, email, name string) (gid string, created bool, err error)`; `UntagWholesaleCustomer` returns nil when no customer has the email (X17); `func requireWholesaleCaller(ctx context.Context, construct string) error` (refuses without internal origin: `shopify: <construct> is called by the wholesale pack, never by a client`), called first in `handleTagWholesaleCustomer`, `handleUntagWholesaleCustomer`, `handleProvisionWholesale`, `handleRevokeWholesale` (X22); refusals carry a code through `type codedRefusal interface { RefusalCode() string }`, and `refusalNode` adds `code`; the tag reply adds `customerId` and `customerCreated`.
- Produces, in the DSL: `shopifyTagWholesaleCustomer` gains `buyerName string`. `ConnectScopes()` includes `write_customers`.
- Produces, in the pack: `engineCaller.Call` stamps `auth.ContextWithInternalOrigin` inline on its one `Execute`, after the pack's own checks; the customer-tag adapter passes `buyerName: g.BuyerName` and fills `Outcome.CustomerID` and `CustomerCreated` from the reply.

Decisions:
- Create: email, the name split by the existing `splitName`, and no marketing consent. A `customerCreate` user error saying the email is taken re-reads by email and continues (two grants at once). Other `customerCreate` user errors refuse with code `customer_refused`. An Admin answer of access denied on the customer read or create refuses with code `scope_missing`. `WholesaleCeilingError` refuses with code `plan_ceiling`.
- The guide, the builtin description and the comment say that a buyer with no customer gets one created and tagged, and that a refusal is `failed` with its code while an outage is `pending`.

- [ ] **Step 1: Write the failing tests** (with the harness's `fakeAdmin`)

```go
func TestAMissingCustomerIsCreatedOnceAndTagged(t *testing.T)                 // customerCreate called once with no marketing consent; reply customerCreated true
func TestASecondTagCreatesNothingAndAddsNoSecondTag(t *testing.T)             // second call: zero customerCreate, created false
func TestADuplicateEmailRefusalFromCreateReadsAgainAndTags(t *testing.T)
func TestUntaggingAnAddressWithNoCustomerSucceeds(t *testing.T)
func TestACustomerAccessDeniedIsScopeMissing(t *testing.T)                    // reply status refused, code scope_missing
func TestEachWholesaleBuiltinRefusesACallWithoutInternalOrigin(t *testing.T)  // all four; zero Admin calls
func TestTheCustomAppPathAsksForWriteCustomers(t *testing.T)
// packs/wholesalepack
func TestTheCallerStampsInternalOriginOnItsOneCall(t *testing.T)
func TestTheTagAdapterPassesTheBuyersName(t *testing.T)
```

- [ ] **Step 2:** Run `MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -count=1 ./integrations/shopify/ ./packs/wholesalepack/ -run 'MissingCustomer|SecondTag|DuplicateEmail|NoCustomerSucceeds|AccessDeniedIsScope|WithoutInternalOrigin|WriteCustomers|CallerStamps|BuyersName'`. Expected: FAIL.
- [ ] **Step 3:** Implement, edit the three texts, extend the allowlist reason (the caller's stamp), then `make sdk-gen`.
- [ ] **Step 4:** Run the same command, both packages whole (the pack in the database lane), `MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -count=1 -run 'TestDocs|TestRetiredVocabulary|TestPluginMeansPackVocabulary' .` and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `shopify: the wholesale tag creates a missing customer, and only the pack may tag`.

### Task 9: A live test of the tag route on a development store (U4)

**Before starting:** the branch is rebased as Task 8 says (Task 8 will have done it).

**Files:**
- Create: `integrations/shopify/wholesale_live_test.go`
- Modify: `packs/wholesalepack/live_e2e_test.go`

**Interfaces:**
- Consumes: the store-mirror live harness's variables, read as `integrations/shopify/mirror_live_test.go` reads them: `MEMQL_TEST_SHOPIFY_STORE_DOMAIN`, `MEMQL_TEST_SHOPIFY_ADMIN_TOKEN`; and this test's own optional `MEMQL_TEST_SHOPIFY_ADMIN_TOKEN_NO_CUSTOMERS` (a token of the same store without `write_customers`). Without the first two every test skips, naming the variable; the scope test also skips without the third.

Decisions:
- Each test creates its own customer address, `wholesale-live-<random>@example.com`, and deletes the customer it created afterwards. It never touches a customer it did not create.
- N5 is settled through the engine, with the Shopify integration registered, by a signed-in non-owner (`auth.RoleWriter`) with client origin calling `builtin shopifyTagWholesaleCustomer(...)`, around a transport that counts Admin calls.
- The automation path boots the pack engine with the contoso fixture and the Shopify integration against the development store. If `TestAreaGraphIsADAG` refuses a pack test importing `integrations/shopify`, the test moves to `wholesale_live_test.go` under the same name and imports the pack from there.

- [ ] **Step 1: Write the tests**

```go
func TestLiveAGrantWithoutWriteCustomersAnswersScopeMissing(t *testing.T)      // spec section 11 step 1
func TestLiveAMissingCustomerIsCreatedOnceAndTagged(t *testing.T)              // step 2: reply customerCreated true; the customer carries the tag
func TestLiveASecondProvisionCreatesNothingAndAddsNoSecondTag(t *testing.T)    // step 3
func TestLiveRevokeRemovesTheTagAndASecondRevokeSucceeds(t *testing.T)         // step 4
func TestLiveProvisionAgainReTags(t *testing.T)                                // step 5
func TestLiveASignedInNonOwnerCannotTagACustomerDirectly(t *testing.T)         // step 7: refused, zero Admin calls
// packs/wholesalepack/live_e2e_test.go
func TestLiveE2E_AClientGrantAutomationTagsTheBuyerOnADevelopmentStore(t *testing.T) // step 6: granted row, tag on
```

- [ ] **Step 2:** Run `MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -count=1 ./integrations/shopify/ ./packs/wholesalepack/ -run 'TestLive' -v`. Expected without credentials: SKIP, each naming its variable. With a development store's credentials: PASS.
- [ ] **Step 3:** Run `MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -count=1 -run TestAreaGraphIsADAG .`. Expected: PASS.
- [ ] **Step 4: Commit** `shopify: a live test of the wholesale tag route against a development store`.

---

> **CHECKPOINT: END OF RELEASE ONE.** Tasks 1 to 9 (U1, U2, U5, U6, U11, U3, U4) are release one. Before Task 10: run Task 18's steps 1 to 5 on this commit (a fresh database `memql_wholesale_r1`), record the results, and stop for the controller's review. The branch at this commit is what may merge to `main` as release one; the operator's rollout steps (spec section 16, step 2) follow it. Nothing after this line is needed for an approved buyer to be tagged.

---

### Task 10: A template says whether it is about one person's own request (U7, first half)

**Files:**
- Modify: `dsl/campaigns/concepts.memql` (`template`), `dsl/campaigns/builtins.memql` (`campaignSaveTemplate`), `component/campaigns/store.go` (`Template`), `component/campaigns/template.go`, `component/campaigns/schedule.go`, `component/campaigns/journey_save.go`, `clients/os/src/apps/email/templates/TemplatePage.tsx`, `clients/os/src/apps/email/rows.ts`, `clients/os/src/apps/email/campaigns/wizardModel.ts`, `clients/os/src/apps/email/automations/addModel.ts`
- Test: `component/campaigns/template_test.go`, `component/campaigns/template_db_test.go`, `component/campaigns/schedule_test.go`, `component/campaigns/journey_db_test.go`, `clients/os/test/email/templates/templatePage.test.tsx`, `clients/os/test/email/campaigns/addCampaign.test.tsx`, `clients/os/test/email/automations/automations.test.tsx`

**Interfaces:**
- Produces, in the DSL: `template.purpose enum("marketing", "transactional")`, absent reads as marketing (X10, G8); `campaignSaveTemplate` gains `purpose string`, refused unless empty, `marketing` or `transactional`.
- Produces, in Go: `Purpose string` (json `purpose`) on `Template`; `func (t Template) Transactional() bool`; `var errTransactionalTemplate = errors.New("This template is about one person's own request and has no unsubscribe, so it cannot be sent to a list.")`, returned when a campaign is scheduled or started, or a journey step saved, with a transactional template.
- Produces, in the OS: `templatePurpose(row: Row): "marketing" | "transactional"` in `rows.ts`; the template page offers a choice "Marketing" / "About one person's own request: no unsubscribe", with one `Caption` beside it: "Whether a message counts as transactional where you trade is your judgment of its words."; the campaign wizard's and the automation step's template choices leave transactional templates out.

- [ ] **Step 1: Write the failing tests**

```go
func TestATemplateWithNoPurposeIsMarketing(t *testing.T)
func TestSaveTemplateRefusesAPurposeOutsideTheTwo(t *testing.T)
func TestSaveTemplateKeepsTheTransactionalPurpose(t *testing.T)            // database lane
func TestACampaignRefusesATransactionalTemplate(t *testing.T)              // errTransactionalTemplate
func TestAJourneyStepRefusesATransactionalTemplate(t *testing.T)           // database lane
```

```ts
it("offers the purpose as a choice and saves transactional")
it("leaves transactional templates out of the campaign wizard's picker")
it("leaves transactional templates out of an automation step's picker")
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -p 1 -count=1 ./component/campaigns/ -run 'Purpose|Transactional'`; in `clients/os`, `npx vitest run test/email/templates/templatePage.test.tsx test/email/campaigns/addCampaign.test.tsx test/email/automations/automations.test.tsx`. Expected: FAIL.
- [ ] **Step 3:** Implement, then `make sdk-gen` and regenerate the concept snapshot and platform graph.
- [ ] **Step 4:** Run the same commands, `./component/campaigns/` whole in the database lane, `npm run typecheck`, `npx vitest run test/email`, and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `campaigns: a template says whether it is about one person's own request, and a list cannot use one that is`.

### Task 11: The transactional row lane (U7, second half)

**Files:**
- Create: `component/campaigns/send_row.go`, `component/campaigns/send_row_test.go`, `component/campaigns/send_row_db_test.go`
- Modify: `component/campaigns/render.go`, `component/campaigns/suppression.go`, `component/campaigns/single_send.go`, `component/campaigns/send_to_recipient.go`, `component/campaigns/deferred_send.go`, `component/campaigns/capabilities.go`, `dsl/campaigns/{concepts,builtins,queries}.memql`
- Test: `component/campaigns/suppression_test.go`, `component/campaigns/deferred_send_db_test.go`, `component/campaigns/render_placement_test.go`

**Interfaces:**
- Consumes: Task 10's `Template.Transactional()`.
- Produces, in Go: `type RowAddress struct { Concept, EmailField, AccountField string }`; `func RegisterRowAddress(a RowAddress)` (Go only; a DSL file cannot register one); `RenderOptions.Transactional bool` (`renderMessage` skips `errNoUnsubscribeLink`, the List-Unsubscribe pair and the appended footer; `RequireUnsubscribe` is not consulted); `func (s *Store) BlockingSuppression(ctx context.Context, accountID, digest string) (Suppression, bool, error)` (the cluster row and the organization row; the first whose reason is `hard_bounce`, `complaint` or `manual`; `unsubscribed` ignored) (X9, G7); `singleSend` gains `rowSend *rowSend` with `type rowSend struct { Recipient Recipient; Concept, RowID string }`; `func rowRecipientID(concept, rowID string) string` = `"row-" + slugOf(concept) + "-" + slugOf(rowID)`; capabilities `sendRowMessage` and `rowMessageStatus`.
- Produces, in the DSL:

```
@executor("integration.campaigns.sendRowMessage")
builtin campaignSendRowMessage {
  rowConcept        string!
  rowId             string!
  accountId         string!
  templateId        string!
  requestId         string!
  fields            object
  senderIdentityId  string
}
@executor("integration.campaigns.rowMessageStatus")
builtin campaignRowMessageStatus { requestIds list! }
```

  `delivery` gains `rowConcept string` and `rowId string`; `@serverOnly query delivery deliveriesByRequestIds { args { requestIds list! } }` (cluster tier).
- `campaignRowMessageStatus` answers one row per request id: `{requestId, status, skipReason, providerStatus, sentAt}`, read under the system actor, nothing else of the delivery.

Decisions:
- `campaignSendRowMessage` refuses a call without internal origin before anything is read ("campaigns.sendRowMessage: a message about one person's own request is sent by a trusted job, never by a client"), an unregistered concept, a row it cannot find, and a row whose account field is not `accountId`. It reads the row under the system actor, takes the address from the registered field, and runs `sendToRecipientOnce` with a recipient that exists only for this send, as the order route does, with `deferWhenLimited`.
- On a row send: a template that is not `ready` or not transactional is held with `templateHeld`, its sentence naming the template and "is not a published transactional template"; suppression is `BlockingSuppression`, a match ledgered `skipped` with its reason; render is `Transactional`.
- The ledger row of a row send carries `rowConcept` and `rowId`; the deferred replay rebuilds the row recipient from them instead of reading a recipient row (G6).
- G6 is a suspicion read from the code. Step 1's first test asks whether a rate-limited ORDER send is replayed. If it FAILS, order sends set `rowConcept: "v1:shopify:order"` and `rowId: <orderGid>` on their ledger row and the replay rebuilds the order recipient; say so in the commit. If it PASSES, say so in the commit and strike the order half of G6 from the spec's section 2 in the same commit; row sends carry the two fields either way, because a row recipient has no recipient row to read.

- [ ] **Step 1: Write the failing tests**

```go
// deferred_send_db_test.go, database lane, FIRST
func TestARateLimitedOrderSendIsReplayedWithItsBuyer(t *testing.T)
// send_row_test.go
func TestARowMessageCarriesNoListUnsubscribeHeaderAndNoFooter(t *testing.T)
func TestARowMessageRefusesACallWithoutInternalOrigin(t *testing.T)        // zero reads
func TestARowMessageRefusesAnUnregisteredConcept(t *testing.T)
func TestARowMessageRefusesARowOfAnotherOrganization(t *testing.T)
func TestAMarketingTemplateIsHeldOnTheLane(t *testing.T)
func TestADraftTransactionalTemplateIsHeld(t *testing.T)
// suppression_test.go
func TestBlockingSuppressionIgnoresAnUnsubscribe(t *testing.T)
func TestBlockingSuppressionFindsAnOrganizationBounceBehindAClusterUnsubscribe(t *testing.T)
// send_row_db_test.go, database lane, a concept registered with RegisterRowAddress in the test
func TestAnOrganizationUnsubscribeDoesNotStopARowMessage(t *testing.T)
func TestAClusterWideUnsubscribeDoesNotStopARowMessage(t *testing.T)
func TestAHardBounceSkipsARowMessage(t *testing.T)                         // and complaint, and manual
func TestARateLimitedRowSendIsReplayedWithItsRowRecipient(t *testing.T)
func TestASecondCallWithTheSameRequestIdSendsNothing(t *testing.T)
func TestRowMessageStatusAnswersEachRequestId(t *testing.T)
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -p 1 -count=1 ./component/campaigns/ -run 'RateLimited|RowMessage|MarketingTemplateIsHeld|DraftTransactional|BlockingSuppression|Unsubscribe|HardBounce|SameRequestId'`. Expected: FAIL (record what the order replay test did).
- [ ] **Step 3:** Implement, then `make sdk-gen` and regenerate the concept snapshot and platform graph.
- [ ] **Step 4:** Run the same command, `./component/campaigns/` and `./component/emailrules/` whole in the database lane, and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `campaigns: one message to the person a pack row names, with no unsubscribe, stopped by a bounce`.

### Task 12: Terms documents, and applications open only once they exist (U8, first half)

**Files:**
- Create: `packs/wholesalepack/documents.go`, `packs/wholesalepack/documents_test.go`
- Modify: `packs/wholesalepack/settings.go`, `packs/wholesalepack/application.go`, `packs/wholesalepack/reads.go`, `packs/wholesalepack/export.go`, `packs/wholesalepack/pack.go`, `packs/wholesalepack/dsl/{concepts,mutations,queries,builtins}.memql`
- Test: `packs/wholesalepack/gate_test.go`, `packs/wholesalepack/application_test.go`

**Interfaces:**
- Consumes: Task 5's settings builtins and `EffectiveSettings`; Task 6's export.
- Produces, in the DSL: concept `documentVersion` (`@rowAuthz(owner="ownerUserId", clusterOwner, account="accountId")`): `ownerUserId string!` (the store's merchant), `storeId string!`, `accountId string`, `kind enum("terms", "map", "priceSheet")!`, `label string!`, `body string!`, `sha256 string!`, `publishedBy string!`, `publishedAt datetime!`. `@serverOnly mutation documentVersion appendDocumentVersion` (insert, generated id); `query documentVersion documentVersionsForStore { args { storeId string!, kind string! } }` (newest first). `wholesaleSettings` gains `termsRequired bool`, `linkLifetimeDays int`, `acceptanceText string`, accepted by `putWholesaleSettingsRow`, and by `wholesaleUpdateSettings` as three new arguments. Gated `wholesalePublishDocument { siteId string!, storeId string!, kind string!, label string!, body string! }` and `wholesaleDocuments { siteId string!, storeId string!, versionId string }`.
- Produces, in Go: `const DefaultLinkLifetimeDays = 30`; `const DefaultAcceptanceText = "I have read and accept the Wholesale Terms and Conditions and the MAP Policy shown above."`; `const maxDocumentBytes = 200 * 1024`; `Settings` gains `TermsRequired bool`, `LinkLifetimeDays int`, `AcceptanceText string`; `func (p *Provider) currentDocuments(ctx context.Context, storeID string) (map[string]map[string]any, error)` (kind to newest version, absent when none); `func (p *Provider) termsRuleRefusal(ctx context.Context, storeID string, open, termsRequired bool) error` (refuses with "Publish the terms, the MAP policy and the price sheet first." when both are true and a kind has no version).
- Replies: `wholesalePublishDocument` `{documentVersionId, sha256}`; `wholesaleDocuments` `{kinds: [{kind, current: {id, label, publishedAt, publishedBy, publishedByName, sha256, body} | null, history: [{id, label, publishedAt, publishedBy, publishedByName, sha256}]}]}`, and with `versionId` that one version with its `body`. `wholesaleStoreSettings` gains `termsRequired`, `linkLifetimeDays`, `acceptanceText` (effective values) and `documents` (kind to `{label, sha256}` or null). The export gains `documentVersions`.

Decisions:
- `label` trimmed, 1 to 120 characters; `body` 1 to 204800 bytes; `sha256` the lowercase hex digest of the body's UTF-8 bytes; `publishedBy` the caller; `publishedAt` the engine clock; written as the site's owner. Versions are never updated.
- X8: `termsRequired` absent is false. `wholesaleUpdateSettings` and `wholesaleSetSettings` call `termsRuleRefusal` on the merged result. `wholesaleSubmitApplication` calls it at write time and refuses as it refuses a closed store. `linkLifetimeDays` outside 1 to 365 is refused.

- [ ] **Step 1: Write the failing tests**

```go
func TestADocumentVersionHoldsTheFingerprintOfItsBody(t *testing.T)          // body "hello": sha256 "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
func TestVersionsAreAppendOnlyAndTheNewestIsCurrent(t *testing.T)
func TestABodyOver200KBIsRefused(t *testing.T)                               // 204801 bytes
func TestALabelOver120CharactersIsRefused(t *testing.T)
func TestOpeningWithTermsRequiredIsRefusedWithoutAllThreeDocuments(t *testing.T) // exact sentence
func TestTurningOnTermsWhileOpenIsRefusedWithoutAllThreeDocuments(t *testing.T)
func TestASubmissionIsRefusedWhenTermsAreRequiredAndADocumentIsMissing(t *testing.T)
func TestTermsRequiredAbsentMeansFalse(t *testing.T)
func TestLinkLifetimeOutsideOneTo365IsRefused(t *testing.T)                  // 0 and 366
func TestTheAcceptanceSentenceDefaults(t *testing.T)
// gate_test.go: wholesalePublishDocument and wholesaleDocuments join gatedBuiltins
```

- [ ] **Step 2:** Run `MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -count=1 ./packs/wholesalepack/ -run 'DocumentVersion|AppendOnly|Over200KB|Over120|AllThreeDocuments|DocumentIsMissing|TermsRequiredAbsent|LinkLifetime|AcceptanceSentence|GatedBuiltin'`. Expected: FAIL.
- [ ] **Step 3:** Implement, then regenerate.
- [ ] **Step 4:** Run the same command, the package whole in the database lane and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `wholesale: a store publishes its terms, MAP policy and price sheet as versions`.

### Task 13: The private terms link and the acceptance record (U8, second half)

**Files:**
- Create: `packs/wholesalepack/links.go`, `packs/wholesalepack/acceptance.go`, `packs/wholesalepack/links_test.go`, `packs/wholesalepack/acceptance_db_test.go`
- Modify: `packs/wholesalepack/shopper.go`, `packs/wholesalepack/application.go`, `packs/wholesalepack/reads.go`, `packs/wholesalepack/export.go`, `packs/wholesalepack/pack.go`, `packs/wholesalepack/dsl/{concepts,mutations,queries,builtins}.memql`, `component/memql/shopper_surface.go`, `component/server/shopper_handler.go`, `component/edge/shopper.go`, `call_origin_conformance_test.go`
- Test: `component/memql/shopper_surface_test.go`, `component/server/shopper_handler_test.go`, `component/edge/shopper_test.go`

**Interfaces:**
- Consumes: Task 12's documents, `currentDocuments`, `EffectiveSettings`; Task 5's `StageOf`; Task 6's `wholesaleGet` and `ActsFor`.
- Produces, in the DSL: concept `privateLink` (tier as `documentVersion`): `ownerUserId string!`, `storeId string!`, `accountId string`, `siteId string`, `applicationId string!`, `purpose enum("terms", "priceSheet")!`, `tokenHash string!`, `issuedAt datetime!`, `expiresAt datetime`, `usedAt datetime`. Concept `acceptance` (same tier): `ownerUserId`, `storeId`, `accountId` (string), `applicationId string!`, `applicantEmail string!`, `signedName string!`, `acceptedAt datetime!`, `termsVersionId string!`, `termsSha256 string!`, `mapVersionId string!`, `mapSha256 string!`, `statement string!`, `clientAddress string`, `userAgent string`, `linkIssuedAt datetime`. `@serverOnly` mutations `putPrivateLinkRow` (a new version at the derived id) and `putAcceptanceRow` (`@createOnly`, at the derived id). Queries `privateLinkByTokenHash { storeId string!, tokenHash string! }` (projects no hash), `privateLinkForApplication { applicationId string!, purpose string! }`, `acceptanceForApplication { applicationId string! }`. Builtins `wholesaleTermsForLink { requestToken string!, storeId string! }`, `wholesaleAcceptTerms { requestToken string!, acceptTerms string!, acceptMap string!, signedName string!, storeId string!, siteId string, clientAddress string, userAgent string }`, `wholesalePriceSheetForLink { requestToken string!, storeId string! }` (`storeId` stamped by the handler).
- Produces, engine-wide: `clientAddress` and `userAgent` join `reservedShopperFieldNames`; `ShopperForm.Evidence bool`; `memql.ShopperClientHeader = "X-Memql-Shopper-Client"`, joining the edge's `shopperStampHeaders` and set to `shopperClientAddress(r)` after the inbound copy is deleted; for an `Evidence` form the handler stamps `clientAddress` from that header and `userAgent` from the request, cut to 512 bytes (X20).
- Produces, in Go: `func PrivateLinkRowID(purpose, applicationID string) string`; `func AcceptanceRowID(applicationID string) string`; `func mintToken() (token, hash string, err error)` (32 random bytes, base64url without padding; hash the hex SHA-256 of the token, as `packs/reviewspack/request.go:52-59`); `func (p *Provider) mintLink(ctx context.Context, app map[string]any, purpose string, now time.Time) (token string, expiresAt time.Time, err error)` (writes the new version; terms `expiresAt = now + linkLifetimeDays`, price sheet none); `func (p *Provider) linkFor(ctx context.Context, storeID, token, purpose string, now time.Time) (link, app map[string]any, err error)` returning `*LinkRefusal{Code}`; `const RefusalLinkInvalid = "link_invalid"`, `RefusalLinkExpired = "link_expired"`, `RefusalLinkUsed = "link_used"`; `var LinkRefusals = []string{RefusalLinkExpired, RefusalLinkUsed, RefusalLinkInvalid}`.
- Shopper surface: form `wholesale/acceptance` (construct `wholesaleAcceptTerms`; fields `requestToken` required 200, `acceptTerms` required 8, `acceptMap` required 8, `signedName` required 120; `Evidence: true`; OK `/wholesale/accepted`, error `/wholesale/link-problem`; `Refusals: LinkRefusals`); reads `wholesale/terms` (`wholesaleTermsForLink`, `requestToken` required, `Private: true`, `Refusals: LinkRefusals`) and `wholesale/priceSheet` (`wholesalePriceSheetForLink`, the same).
- Replies: terms `{companyName, documents: [{kind, label, body, sha256}] (terms then map), statement, expiresAt}`; price sheet `{companyName, document: {label, body, sha256, publishedAt}}`; never an email, an id or a hash. `wholesaleGet` gains `acceptance: {signedName, acceptedAt, termsLabel, termsSha256, mapLabel, mapSha256, clientAddress, userAgent} | null` and `termsLink: {expiresAt, expired} | null`; stage is computed with the real acceptance everywhere `StageOf` is called. The export gains `acceptances`.

Decisions:
- Verify (7.1): the hash must match a link of this store, else `link_invalid` (which covers a replaced link). Terms: `usedAt` set or the application no longer `submitted` is `link_used`; the clock at or past `expiresAt` is `link_expired`. Price sheet: the application `approved` and its entitlement `granted`, else `link_used`. Reads run under the site owner's borrowed authority, as `reviewRequestByTokenHash` is.
- Accept: re-verify; both boxes must arrive as `on`, else `invalid`; the trimmed name must not be empty (the handler's required check answers `invalid`); read the current terms and MAP versions again; write the acceptance, then the link's `usedAt`. A second post finds the acceptance (`@createOnly`) and answers `link_used`. The statement is the effective `acceptanceText`.
- Approve (7.2): `wholesaleRecordDecision` with `approve`, when `termsRequired` is true and `acceptanceForApplication` answers nothing, refuses with "This application has not accepted the terms yet."
- The allowlist reason grows: the shopper path's link check precedes the acceptance write.

- [ ] **Step 1: Write the failing tests**

```go
// component/memql, component/server, component/edge
func TestClientAddressAndUserAgentAreReservedShopperNames(t *testing.T)
func TestAnEvidenceFormIsStampedWithTheEdgesAddressAndTheBrowser(t *testing.T)   // a 600-byte User-Agent arrives as 512
func TestANonEvidenceFormIsGivenNoEvidence(t *testing.T)
func TestTheEdgeSetsTheShopperClientHeaderAndDropsAnInboundCopy(t *testing.T)
// links_test.go, with a fake clock, reader and writer
func TestTheTokenIsNeverStored(t *testing.T)                     // no write argument equals the token; tokenHash is its hex sha256
func TestAnExpiredTermsLinkIsRefusedAsLinkExpired(t *testing.T)
func TestAUsedTermsLinkIsRefusedAsLinkUsed(t *testing.T)
func TestATermsLinkOfARejectedApplicationIsRefusedAsLinkUsed(t *testing.T)
func TestAReplacedTokenIsRefusedAsLinkInvalid(t *testing.T)      // mint twice; the first token
func TestATokenOfAnotherStoreIsRefusedAsLinkInvalid(t *testing.T)
func TestTheTermsReadShowsNoEmailNoIdAndNoHash(t *testing.T)
func TestAnAcceptanceHoldsEveryField(t *testing.T)               // signedName "Ada Example", both version ids and fingerprints, statement, clientAddress, userAgent, linkIssuedAt
func TestBothBoxesMustArriveAsOn(t *testing.T)
func TestASecondAcceptanceAnswersLinkUsed(t *testing.T)
func TestApproveIsRefusedWithoutAnAcceptanceWhenTermsAreRequired(t *testing.T)   // exact sentence
func TestApproveNeedsNoAcceptanceWhenTermsAreNotRequired(t *testing.T)
func TestAPriceSheetLinkOpensOnlyWhileApprovedAndGranted(t *testing.T)
func TestAPriceSheetLinkShowsTheCurrentVersion(t *testing.T)
func TestTheTermsAndPriceSheetReadsArePrivate(t *testing.T)
func TestAnAcceptedApplicationIsReadyToReview(t *testing.T)
// acceptance_db_test.go, database lane
func TestATermsLinkIsAcceptedOnceThroughTheShopperPath(t *testing.T)
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -p 1 -count=1 ./component/memql/ ./packs/wholesalepack/ -run 'ReservedShopperNames|Token|TermsLink|TermsRead|Acceptance|BothBoxes|Approve|PriceSheet|ArePrivate|ReadyToReview'`, `MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -count=1 ./component/server/ ./component/edge/ -run 'Evidence|ShopperClient'`. Expected: FAIL.
- [ ] **Step 3:** Implement, extend the allowlist reason, then regenerate.
- [ ] **Step 4:** Run the same commands, `./packs/wholesalepack/` whole in the database lane, `MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -count=1 ./component/server/ ./component/edge/ -run Shopper` and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `wholesale: an applicant accepts the terms on a private link, and approval waits for it`.

### Task 14: The applicant's emails and the staff notices (U9)

**Files:**
- Create: `packs/wholesalepack/email.go`, `packs/wholesalepack/notify.go`, `packs/wholesalepack/email_test.go`, `packs/wholesalepack/email_db_test.go`
- Modify: `packs/wholesalepack/settings.go`, `packs/wholesalepack/reads.go`, `packs/wholesalepack/export.go`, `packs/wholesalepack/pack.go`, `packs/wholesalepack/dsl/{concepts,mutations,queries,builtins}.memql`, `component/outbound/config.go`, `call_origin_conformance_test.go`
- Test: `component/outbound/email_allowlist_test.go`, `packs/wholesalepack/gate_test.go`

**Interfaces:**
- Consumes: Task 11's `campaignSendRowMessage`, `campaignRowMessageStatus`, `campaigns.RegisterRowAddress`; Task 13's `mintLink`; Task 8's internal-origin `engineCaller`; Task 6's `ActsFor`.
- Produces, in `component/outbound`: `func EmailAdmitted(target string) bool` (the configured `EmailAllowlist` through `emailAllowed`).
- Produces, in the DSL: concept `emailSend` (tier as `documentVersion`): `ownerUserId`, `storeId`, `accountId` (string), `applicationId string!`, `kind enum("terms", "approved", "rejected", "revoked")!`, `attempt int!`, `requestId string!`, `templateId string`, `state enum("sent", "skipped", "failed", "pending")!`, `reason string`, `sentAt datetime`, `requestedBy string`. `@serverOnly mutation emailSend putEmailSendRow` (at the derived id); `query emailSend emailSendsForApplication { applicationId string! }` (newest first). `wholesaleSettings` gains `senderIdentityId`, `termsTemplateId`, `approvedTemplateId`, `rejectedTemplateId`, `revokedTemplateId` (strings), accepted by `putWholesaleSettingsRow` and by `wholesaleUpdateSettings` as five new arguments. Builtins: internal `wholesaleSendTermsEmail { applicationId string! }`, internal `wholesaleSendBuyerEmail { applicationId string!, kind string! }`, gated `wholesaleResendEmail { applicationId string!, kind string! }`, internal `wholesaleNotifyStaff { applicationId string!, event string!, subject string!, body string!, decisionId string }`.
- Produces, in Go: `Settings` gains `SenderIdentityID`, `TermsTemplateID`, `ApprovedTemplateID`, `RejectedTemplateID`, `RevokedTemplateID`; `func EmailSendRowID(kind, applicationID string, attempt int) string`; `func LaneRequestID(kind, applicationID string, attempt int) string`; `func StaffRequestID(event, applicationID, decisionID string) string`; `func fillPlaceholders(text string, values map[string]string) string` (only `{{companyName}}`, `{{applicantName}}`, `{{applicantEmail}}`, `{{state}}`, `{{decidedBy}}`, `{{note}}`, `{{adminUrl}}`; any other `{{...}}` is left as written). `Register` calls `campaigns.RegisterRowAddress(campaigns.RowAddress{Concept: "v1:wholesale:application", EmailField: "applicantEmail", AccountField: "accountId"})`, beside the shipped adapters, so a disabled pack registers nothing.
- Replies: each send answers its `emailSend` row. `wholesaleStoreSettings` gains `staffEmailAdmitted`, `senderIdentityId`, the four template ids, `templates: [{id, name}]` (the templates of the settings' `accountId` with status `ready` and purpose `transactional`, read through the `templates` query (`status: "ready"`) under the caller and filtered in Go) and `may.setSender`. `wholesaleGet` gains `emails: [{kind, attempt, state, reason, sentAt, requestedBy, delivery: {status, skipReason, providerStatus, sentAt} | null}]`, the delivery read through `campaignRowMessageStatus` inline under internal origin after the gate. `ActsInput` gains `TermsRequired, TermsTemplateChosen, ApprovedEmailSent bool`, and `ActsFor` gains `resendTerms` (on `waitingForTerms` when terms are required and a terms template is chosen, after `reject`: Reject; Resend terms email) and `resendApproved` (on `approved` with access `granted` once an `approved` send exists: Remove; Resend approval email; Retry access). The export gains `emailSends`.

Decisions:
- The lane call: `builtin campaignSendRowMessage(rowConcept: "v1:wholesale:application", rowId: <application id>, accountId, templateId, requestId, fields, senderIdentityId)` through `engineCaller` as the merchant. `fields`: `companyName`, `applicantName`, `signInEmail` (the applicant's email), `termsUrl` (`https://<site hostname>/wholesale/terms?t=<token>`), `termsExpiresOn` (`2 January 2006`), `priceSheetUrl` (`https://<site hostname>/wholesale/price-sheet?t=<token>`), `storeName` (the site's `title`, else its `hostname`). A lane hold or skip is recorded `skipped` with the lane's sentence; an error `failed` with its text; a deferral `pending`.
- Automatic sends are attempt 1. Before minting, each send reads its attempt-1 `emailSend` row: if it is `sent`, `skipped` or `pending`, it answers that row and mints nothing, so a re-delivered event sends nothing and never ends a link already emailed (E6).
- `wholesaleSendTermsEmail` skips, with these reasons, when terms are not required ("terms are not required on this store"), no terms template is chosen ("no terms email template is chosen"), or the application is not waiting for terms ("the application is not waiting for terms").
- `wholesaleSendBuyerEmail`: `approved` needs state `approved` and the entitlement `granted` (W3), else skipped "access is not granted yet", and mints the price-sheet link; `rejected` and `revoked` need that state. No template chosen for the kind: skipped "no <kind> email template is chosen".
- `wholesaleResendEmail`: `terms` while waiting for terms; `approved` while approved and granted with an attempt-1 send; attempt is the highest recorded plus one; `requestedBy` is the caller; a cluster operator is refused. Minting a new link ends the old one.
- `wholesaleNotifyStaff`: an empty `staffEmail` skips with "the store names no address". Otherwise it stages `stageServerOutboundRequest` with `medium: "email"`, `target: staffEmail`, the filled subject and body, `requestId` and `dedupeKey` from `StaffRequestID`, `requestedBy: "wholesale:staffNotice"`. `{{decidedBy}}` is the person's name, `{{state}}` the staff word (Removed for revoked), `{{adminUrl}}` the OS address as reviews' `adminBase()` builds it, followed by `/`. Nothing from the extension row is reachable.
- `senderIdentityId` in `wholesaleUpdateSettings` is refused for anybody but a cluster operator: "Only the cluster's operators choose the sender." A template id must be one of `templates` or empty.
- The allowlist reason grows: the two inline stamps on the lane calls and the status read.

- [ ] **Step 1: Write the failing tests**

```go
// component/outbound
func TestEmailAdmittedReadsTheConfiguredAllowlist(t *testing.T)
// email_test.go, with a fake caller recording lane calls
func TestNoTemplateMeansNoSend(t *testing.T)
func TestTheApprovedEmailWaitsForGrantedAccess(t *testing.T)               // entitlement pending: skipped, zero lane calls
func TestASecondGrantedVersionSendsNoSecondApprovalEmail(t *testing.T)     // two calls: one lane call, one emailSend row
func TestASecondAutomaticTermsSendMintsNoNewLink(t *testing.T)             // the link's tokenHash is unchanged
func TestAResendOfTermsIsAttemptTwoAndEndsTheOldLink(t *testing.T)
func TestTheLaneIsHandedTheClosedSetOfFields(t *testing.T)
func TestAStaffNoticeWithNoAddressIsSkipped(t *testing.T)                  // reason "the store names no address"
func TestStaffPlaceholdersAreFilledAndTheExtensionIsUnreachable(t *testing.T)   // "{{taxNumber}}" stays as written
func TestAReDeliveredStaffEventStagesNothingNew(t *testing.T)              // same requestId
func TestOnlyAnOperatorChoosesTheSender(t *testing.T)
func TestTheInternalSendsRefuseACallWithoutInternalOrigin(t *testing.T)
func TestAnOperatorCannotResend(t *testing.T)
// email_db_test.go, database lane, a transactional template published in the organization
func TestTheTermsEmailReachesTheLaneAndItsDeliveryShowsOnTheApplication(t *testing.T)
func TestAPriceSheetLinkStopsOpeningOnceTheBuyerIsRemoved(t *testing.T)
// gate_test.go: wholesaleResendEmail joins gatedBuiltins
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -p 1 -count=1 ./component/outbound/ ./packs/wholesalepack/ -run 'EmailAdmitted|NoTemplate|WaitsForGranted|SecondGranted|MintsNoNewLink|AttemptTwo|ClosedSetOfFields|StaffNotice|Placeholders|ReDelivered|ChoosesTheSender|InternalSends|CannotResend|TermsEmailReaches|StopsOpening|GatedBuiltin'`. Expected: FAIL.
- [ ] **Step 3:** Implement, extend the allowlist reason, then regenerate.
- [ ] **Step 4:** Run the same command, `./packs/wholesalepack/` whole in the database lane and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `wholesale: the applicant is mailed at each step, once, and the store's staff are told`.

### Task 15: Adding and removing a buyer by hand (U10)

**Files:**
- Create: `packs/wholesalepack/buyers.go`, `packs/wholesalepack/buyers_db_test.go`
- Modify: `packs/wholesalepack/pack.go`, `packs/wholesalepack/dsl/{builtins,queries}.memql`
- Test: `packs/wholesalepack/gate_test.go`

**Interfaces:**
- Consumes: Task 5's `appendDecision`, `deciderOf`, `siteForStore`; Task 12's `termsRuleRefusal`.
- Produces, in the DSL: `query application applicationsForStoreByEmail { args { storeId string!, applicantEmail string! } }`; gated `wholesaleAddBuyer { siteId string!, storeId string!, companyName string!, applicantName string!, applicantEmail string! }` answering `{applicationId}`; gated `wholesaleRemoveBuyer { applicationId string!, note string }` answering the decision receipt.

Decisions:
- Add (W7, X16): works whether applications are open or closed; refuses a cluster operator; refuses an email (compared case-insensitively, trimmed) with a `submitted` or `approved` application on the store: "This address already has an application on this store."; when terms are required, refuses with "Publish the terms, the MAP policy and the price sheet first." while a kind has no version. It writes the application at `wholesale-staff-<id.NewShortId()>`, owned by the site's owner, `accountId` and `siteId` from the site, `source: "staff"`, `addedBy` the caller, through `createApplicationRow`. Email 1 follows through the client's own automation on `node.created`, as for anyone.
- Remove: state must be `approved`, else "wholesale: only an approved buyer can be removed"; refuses a cluster operator; records a `revoke` through `appendDecision` with the caller as `decidedBy` and the note. The tag comes off through the client's revoke automation, or `wholesaleRetryAccess`.

- [ ] **Step 1: Write the failing tests** (database lane, organizations seeded as in Task 5)

```go
func TestAnAddedBuyerIsASubmittedApplicationOwnedByTheMerchantAndTiedToTheOrganization(t *testing.T) // source staff, addedBy the caller
func TestAddingABuyerWorksWhileApplicationsAreClosed(t *testing.T)
func TestAnAddressWithAnOpenApplicationIsRefused(t *testing.T)            // "Buyer@Example.com" against "buyer@example.com"
func TestAnAddressWhoseApplicationWasRejectedMayBeAddedAgain(t *testing.T)
func TestAddingABuyerNeedsTheDocumentsWhenTermsAreRequired(t *testing.T)
func TestRemoveRecordsARevokeByTheCaller(t *testing.T)
func TestRemoveIsRefusedForAnApplicationThatIsNotApproved(t *testing.T)
func TestAnOperatorCannotAddOrRemoveABuyer(t *testing.T)
// gate_test.go: wholesaleAddBuyer and wholesaleRemoveBuyer join gatedBuiltins
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -p 1 -count=1 ./packs/wholesalepack/ -run 'AddedBuyer|AddingABuyer|OpenApplicationIsRefused|MayBeAddedAgain|Remove|AddOrRemove|GatedBuiltin'`. Expected: FAIL.
- [ ] **Step 3:** Implement, then regenerate if the platform graph names the builtins.
- [ ] **Step 4:** Run the same command, the package whole and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `wholesale: the store's people add a buyer, who still accepts the terms, and remove one`.

### Task 16: The Wholesale section, complete (U12)

**Files:**
- Create, under `clients/os/src/apps/deployables/wholesale/`: `BuyersTab.tsx`, `AddBuyerPage.tsx`, `DocumentsTab.tsx`, `PublishDocumentPage.tsx`, `DocumentVersionPage.tsx`, `ExtensionDetails.tsx`; `clients/os/test/deployables/wholesaleTwo.test.tsx`
- Modify: `WholesaleSection.tsx`, `ApplicationPage.tsx`, `WholesaleSettingsPage.tsx`, `model.ts`, `useWholesale.ts`, `clients/os/test/deployables/harness.tsx`

**Interfaces:**
- Consumes: Tasks 12 to 15's builtins and replies.
- Produces, in `useWholesale.ts`: `useWholesaleBuyers(scope, search, revision)`; `useWholesaleDocuments(scope, revision)`; `useWholesaleDocumentVersion(scope, versionId)`; on `useWholesaleWrites`: `addBuyer(input: {companyName: string; applicantName: string; applicantEmail: string}): Promise<WriteOutcome & {applicationId?: string}>`, `removeBuyer(applicationId: string, note: string)`, `resendEmail(applicationId: string, kind: "terms" | "approved")`, `publishDocument(kind: "terms" | "map" | "priceSheet", label: string, body: string)`, `exportStore(): Promise<{fileName: string; json: string} | null>`.

Decisions and copy:
- Tabs: Applications, Buyers, Documents, Settings. Remove now calls `wholesaleRemoveBuyer`. The bar maps `resendTerms` "Resend terms email" and `resendApproved` "Resend approval email".
- Buyers: a row names the company, the buyer email and the access in words ("Trade prices on since 3 December", "Not on yet: " and the error code's sentence in lower case after the colon); a row opens the application's page. Head `AddButton` "Add buyer" opens a page: Company, Name, Email, and "They will be emailed the terms. They can buy at trade prices once they accept and someone approves."; bar Cancel; Add and send the terms. Empty: "No buyers yet."
- Documents: three sections, Wholesale terms, MAP policy, Price sheet, each with its `Subhead`, an Add control "Publish a new version", `Facts` for the current label, published on and by, and the fingerprint's first eight characters; earlier versions as a `RecordList` beneath, newest first, opening read-only. A kind with no version: "Not published. Applications cannot open while terms are required." Publish page: Label (`Input`), Text (`TextArea`), "Load from a file" for `.md` or `.txt` read in the browser, a preview of the rendered text, bar Cancel; Publish.
- Settings, complete: *Applications* Open or closed, Terms required (a checkbox: this is a form), Link lifetime in days; *Staff* the address and beneath it "The outbox delivers to this address." or "The outbox does not deliver to this address yet. The cluster's operators add it."; *Access* the route; *Emails* a `Select` per email kind, "Off" first, of the transactional templates, and the sender as a read-only fact "Sent as …"; *Acceptance* the sentence (`TextArea`); *Records* "Download every record".
- One application gains *Terms* ("Accepted by <name> on <date>", the two labels with eight characters of each fingerprint, the address and browser; or "Not accepted yet. The link expires on <date>." or "The link has expired.") and *Emails* (one line per kind: Sent on, Delivered, Bounced "the address does not accept mail", Not sent with the reason). *Their details* renders `details.fields` as `Facts` labelled by `label`.

- [ ] **Step 1: Write the failing tests**

```ts
it("lists approved buyers with their access in words, and opens the application")
it("adds a buyer and opens the new application, waiting for terms")
it("shows the engine's refusal for an address that already applied")
it("draws each document kind with its fingerprint, and says when one is not published")
it("loads a document from a file and publishes it")
it("opens an earlier version read-only")
it("refuses turning terms on before the documents exist, in the engine's words")
it("says whether the outbox delivers to the staff address")
it("offers only published transactional templates, with Off first")
it("downloads every record as one file")
it("shows the acceptance with both fingerprints, or when the link expires")
it("shows each email's delivery, and why one was not sent")
it("draws Resend terms email while an application waits for terms")
it("draws Resend approval email only once it was sent")
it("renders the client's own fields labelled in plain words")
it("removes a buyer through wholesaleRemoveBuyer")
it("draws no act in a state that refuses it")
```

- [ ] **Step 2:** In `clients/os` run `npx vitest run test/deployables/wholesaleTwo.test.tsx test/deployables/wholesale.test.tsx`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, `npm run typecheck` and `npx vitest run test/deployables`. Expected: PASS. Check each tab and page in both themes and at both widths.
- [ ] **Step 5: Commit** `os: the Wholesale section's buyers, documents, terms and emails`.

### Task 17: Documentation

**Files:**
- Modify: `docs/public/build/consuming-the-wholesale-pack.md`, `docs/public/operate/campaign-sending.md`, `docs/public/operate/deployables.md`, `docs/public/operate/shopify-connector.md`, `docs/public/operate/outbound-delivery.md`

Content, each under the page's existing headings where one fits:
- `consuming-the-wholesale-pack.md`: the entitlement states and the six error codes with what fixes each; the stages; the terms step and the two rules; the link rules (30 days by default, ended by a resend, an acceptance, a decision; the price sheet's link lasts while the buyer is approved and granted); every builtin with its reach (shopper, gated, internal, operators); the events a client triggers on (spec 5.7: `node.created` on application, acceptance and decision; `node.created` and `node.updated` on entitlement, with the `granted` and the `pending || failed` filters); a worked set of client automations as spec section 13 lists them, including the revoke wiring (N4) and the access-not-granted notice (X23); the staff placeholders; the `{{fields.<key>}}` set; the account field an extension declares; the backfill.
- `campaign-sending.md`: the row lane, the template purpose and its sentence "Whether a message counts as transactional where you trade is your judgment of its words", what stops a send (a hard bounce, a complaint, an operator's block) and what does not (an unsubscribe), and why it cannot be used for marketing.
- `deployables.md`: the Wholesale section, its four tabs, the part `app:deployables/wholesale` and how an organization grants it on the Users > Groups page.
- `shopify-connector.md`: `write_customers` on the custom-app path, used for the wholesale tag and for creating a missing customer.
- `outbound-delivery.md`: the store's staff address must be on the email allowlist, and the Settings tab says whether it is.

- [ ] **Step 1:** Run `MEMQL_DATABASE_DSN="$WHOLESALE_DSN" go test -count=1 -run 'TestDocs|TestRetiredVocabulary|TestPluginMeansPackVocabulary|TestMakeTargetCitationsNameRealTargets|TestEngineIsProductNeutral' .` before the edit. Expected: PASS.
- [ ] **Step 2:** Write the pages.
- [ ] **Step 3:** Run the same command. Expected: PASS.
- [ ] **Step 4: Commit** `docs: wholesale from application to trade prices, the row lane, and the Wholesale section`.

### Task 18: The whole tree

**Files:** none changed, unless a gate names one.

- [ ] **Step 1:** `docker exec memql-testdb psql -U memql -d postgres -c 'create database memql_wholesale_final'` (at the release-one checkpoint, `memql_wholesale_r1`).
- [ ] **Step 2:** `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN='postgres://memql:memql_dev@127.0.0.1:55432/memql_wholesale_final?sslmode=disable' make test`. Expected: PASS. The two known flakes on a busy machine are `integrations/email` `TestAzureFeedbackDiscoversPreviouslySavedOrganization` and `component/packages` `TestSynchronousConfirmationSurvivesItsCallerAndTheSweep`; run either again alone before calling it a failure.
- [ ] **Step 3:** For each tree `scripts/ci/db-gated-packages.sh --trees` names that this plan touched (`packs`, `component/memql`, `component/campaigns`, `component/server`, `integrations/shopify`, `app`), run it in the database lane with `-p 1` on the same fresh database. Expected: PASS.
- [ ] **Step 4:** `make sdk-gen-check`, `make concept-snapshot-check`, `make platform-graph-check`. Expected: PASS.
- [ ] **Step 5:** In `clients/os`: `npm run typecheck`, `npm test`, `npm run build`. Expected: PASS.
- [ ] **Step 6:** Commit anything a gate made you regenerate, as `build: regenerate after the wholesale change`.
