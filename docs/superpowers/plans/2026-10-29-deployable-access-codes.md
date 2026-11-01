# Deployable access codes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Any deployable can be given one or more server-checked access codes from MemQL OS, each opening one gate of the app, ending in real time at expiry or regeneration.

**Architecture:** Codes are minted like preview grants (digest only, shown once). Visitors reach declared constructs through one engine-owned route family on the site's own origin, shaped like the shopper surface: the edge routes, caps, rate-limits and stamps; the bff validates a session on every request and runs the construct under the site owner's borrowed authority. DSL declares reach with `@accessGate`.

**Tech Stack:** Go (engine, edge, bff, integrations), MemQL DSL, React + TypeScript + Vitest (MemQL OS).

**Spec:** `docs/superpowers/specs/2026-10-29-deployable-access-codes-design.md`. Read it first; this plan argues from it and does not repeat its reasoning.

## Global Constraints

- Branch `claude/deployable-access-codes`, off `main`. Stage files by explicit path; never `git add -A` or `git add .`. End every commit with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Verify Go with `make test` and module-path patterns (`go test github.com/znasllc-io/memql/component/edge/...`), never `go test ./...`, which misses the engine. A db-gated test that skips has proved nothing: use `MEMQL_REQUIRE_DB=1` against a real Postgres+TimescaleDB+pgvector, or say plainly that it did not run.
- Comments explain why a non-obvious branch exists, in the voice of the neighbouring file. No emojis anywhere.
- Code format: 25 Crockford base32 symbols in five hyphenated groups, 125 bits. Session token: `mql_acs_` plus 43 base64url characters from 32 random bytes. Both stored only as lowercase hex SHA-256.
- Expiry bounds: more than 1 minute and at most 366 days from now. Session cap: 24 hours. Label 1 to 80 characters. Subject at most 120 characters.
- Reserved arguments: `siteId`, `accessCodeId`, `accessGate`, `accessSubject`.
- Capability part: `app:deployables/access`, on owner and developer.
- Cookie: `__Host-memql_access=<token>; Path=/; HttpOnly; Secure; SameSite=Strict`, no `Domain`, no `Max-Age`. The edge removes it from everything it forwards but the four routes.
- Rate limits, per replica: unlock 10 per minute per (site, address) and 60 per minute per site; everything else 600 per minute per (site, address). Body caps: 1 KiB unlock and leave, 64 KiB call.
- The access decision is never cached and never logged with a code, token or call arguments.
- A session never merges or approves on GitHub. The finish line here is a green `MemQL / memql` check on an open PR.

## Review Focus

- A code pasted with surrounding whitespace, in lowercase, or with `O`, `I` or `L` typed for `0` and `1` must unlock. Test in Task 1.
- A double click on Create or Regenerate must not silently mint two credentials and leave the operator holding the wrong one. Test in Task 7.
- A code whose gate is no longer declared, because a domain was dropped from a bundle, must still list in the OS with an honest word, and its calls must `404`. Tests in Tasks 5 and 7.
- A visitor's second browser on one code: leaving in one ends that session only. Test in Task 5.
- A site taken offline while a visitor is inside: the edge's status switch answers first and nothing is forwarded. Test in Task 6.

---

### Task 1: Code and token primitives

**Files:**
- Create: `component/memql/site_access_code.go`
- Test: `component/memql/site_access_code_test.go`

**Interfaces:**
- Produces, all in package `memql`:
  - `func MintAccessCode() (code, digest, hint string, err error)`
  - `func NormalizeAccessCode(raw string) string`
  - `func AccessCodeDigest(raw string) string`, the SHA-256 hex of `NormalizeAccessCode(raw)`
  - `func MintAccessSessionToken() (token, digest string, err error)`
  - `func AccessSessionTokenDigest(token string) string`, trimming whitespace first
  - `const AccessSessionTokenPrefix = "mql_acs_"`, `const AccessSessionMaxAge = 24 * time.Hour`
  - `func ValidateAccessCodeExpiry(now, expiresAt time.Time) error`
  - `func AccessSessionEnd(issuedAt, codeExpiresAt time.Time) time.Time`, the earlier of the two limits

Mirror `component/memql/site_preview_token.go`, including returning the plain value and digest together so no call shape can store the plain value.

- [ ] **Step 1: Write the failing tests**

```go
func TestMintAccessCodeShape(t *testing.T) {
	code, digest, hint, err := MintAccessCode()
	// code matches ^[0-9A-HJKMNP-TV-Z]{5}(-[0-9A-HJKMNP-TV-Z]{5}){4}$
	// hint == last 4 symbols of code; digest == AccessCodeDigest(code)
}
func TestNormalizeAccessCodeForgivesHowPeopleTypeIt(t *testing.T) {
	// "  k7q4m-2xpdn 8rt3v\nWJ6HC-9FBZA " == "K7Q4M2XPDN8RT3VWJ6HC9FBZA"
	// O maps to 0; I and L map to 1
	// AccessCodeDigest of every spelling is equal
}
func TestMintAccessCodeIsNotRepeatable(t *testing.T)   // 1000 mints, no duplicates
func TestMintAccessSessionToken(t *testing.T)          // prefix, 43 chars after it, digest matches, whitespace trimmed
func TestValidateAccessCodeExpiry(t *testing.T)        // now+59s refused, now+61s ok, now+366d ok, now+367d refused, past refused
func TestAccessSessionEnd(t *testing.T)                // earlier of code expiry and issuedAt+24h, both directions
```

- [ ] **Step 2: Run to verify failure**

Run: `go test github.com/znasllc-io/memql/component/memql -run 'AccessCode|AccessSession' -count=1`
Expected: FAIL, undefined.

- [ ] **Step 3: Implement** using `crypto/rand`. Draw symbols without modulo bias (32 symbols, so 5 bits per symbol from random bytes). `ValidateAccessCodeExpiry` returns errors whose text an operator can act on, since the OS shows the sentence verbatim.

- [ ] **Step 4: Run to verify pass**, same command. Expected: PASS.

- [ ] **Step 5: Commit** `feat(platform): access code and session token primitives`.

### Task 2: `@accessGate` and its registry

**Files:**
- Modify: `component/language/annotations/registry.go` (keys beside `shopperFormExtensionKeys`, a `Query` and a `Mutation` receiver entry beside line 290, a doc entry beside line 508), `component/language/parser/ast.go` and the `ast` package (`AttrAccessGate`), `component/memql/function_types.go` (`AccessGate *AccessGateDecl` field **and** the `Clone` at line 279; the registry hands out clones, so a field `Clone` does not name is lost), `component/memql/function_loader.go` (read it for query and mutation), `component/memql/engine_bootstrap.go` (beside `loadShopperExtensions` at line 252), `component/memql/result_cache_policy.go` (add `v1:platform:siteAccess` to `cacheDenylistedConceptPrefixes` with the one-line reason)
- Create: `component/memql/access_gate.go`, `component/memql/access_gate_load.go`
- Test: `component/memql/access_gate_load_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces, in package `memql`:
  - `type AccessGateDecl struct{ Deployable, Gate string }`
  - `type AccessGateEntry struct { Deployable, Gate, Construct, Kind, Domain, Namespace string; Args []string; Stamped []string }`, with `func (e AccessGateEntry) Call() string` returning the qualified call text. `Args` are declared non-reserved names; `Stamped` are the reserved names the construct declares; `Kind` is `query` or `mutation`; `Domain` is the DSL domain of the concept the construct is bound to, taken as `shopperExtensionFromFunction` takes it.
  - `var ReservedAccessArgs = []string{"siteId", "accessCodeId", "accessGate", "accessSubject"}`
  - `func AccessGateFor(deployable, gate, construct string, domains []string) *AccessGateEntry`, nil on any miss. `domains` are the site's package's DSL domains; an empty list misses, so an unknown package fails closed.
  - `type AccessGateSummary struct { Gate string; UsesSubject bool; Constructs []string }`
  - `func AccessGatesFor(deployable string, domains []string) []AccessGateSummary`, sorted by gate. `UsesSubject` is true when any construct stamps `accessSubject`.
  - `func AccessGateEntries() []AccessGateEntry`, sorted, for package analysis
  - `func AccessCallPrefix(kind string) string`, mirroring `ShopperCallPrefix`

The registry is rebuilt on every load, as `shopperExtensions` is, so a dropped domain stops opening anything at the next boot. Faults land on the LoadReport as `memql.accessGateRegistration` so strict boot refuses them.

- [ ] **Step 1: Write the failing tests**, table-driven through a real DSL load, one per rule in spec section 7:

```go
func TestAccessGateLoadRules(t *testing.T) {
	// refused: on a logic or tool; written twice; missing deployable=; missing gate=;
	//          deployable "Client_Desk"; gate "9lives"; with @serverOnly; with @requiresRank;
	//          with @requiresCapability; reserved arg accessCodeId declared as int;
	//          two constructs named x under one deployable
	// accepted: a query and a mutation; the same construct name under two deployables
	// each refusal's text names the construct and what to change
}
func TestAccessGateRegistryAnswers(t *testing.T) {
	// AccessGateFor misses on wrong deployable, wrong gate and unknown construct
	// Args excludes reserved names; Stamped lists them; UsesSubject set only when declared
}
func TestAccessGateRegistryIsRebuiltOnLoad(t *testing.T)
func TestAccessGateClonePreservesTheDeclaration(t *testing.T) // Function.Clone keeps AccessGate
func TestSiteAccessConceptsAreNotDefaultCached(t *testing.T)  // isCacheDenylistedConcept("v1:platform:siteAccessCode") and Session
```

- [ ] **Step 2: Run to verify failure**

Run: `go test github.com/znasllc-io/memql/component/memql -run 'AccessGate|SiteAccessConcepts' -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement**, following `shopper_extension_load.go` for the declaration read, the pure conversion and the registry. `cmd/memql-lsp/editorparity_test.go` holds the annotation list editors see; update what it names.

- [ ] **Step 4: Run to verify pass**, same command, plus `go test github.com/znasllc-io/memql/component/language/... -count=1` and `go test github.com/znasllc-io/memql/cmd/memql-lsp/... -count=1`. Expected: PASS.

- [ ] **Step 5: Commit** `feat(language): @accessGate declares what an access code opens`.

### Task 3: Platform DSL, capability part and generated SDK

**Files:**
- Modify: `dsl/platform/concepts.memql`, `dsl/platform/shapes.memql`, `dsl/platform/queries.memql`, `dsl/platform/mutations.memql`, `dsl/platform/builtins.memql`, `dsl/rbac/seeds.memql` (after the `preview` block at line 714), `component/auth/rbac_model.go` (after line 276), `component/memql/organization_action_capabilities.go` (line 15 and the `organizationCapabilityTarget` switch), `component/memql/app_resource_os_parity_test.go` (the `want` map at line 204), `clients/os/src/apps/deployables/parts.ts` (`DEPLOYABLE_PARTS`, `NO_PARTS`, `ALL_PARTS`)
- Create: `integrations/siteaccess/plugin.go`, `integrations/siteaccess/integration.go` with handlers that return a not-implemented error for now, so the DSL resolves at boot
- Modify: `app/plugins_core.go` (blank import, as for `sitepreview` at line 50)
- Regenerate: `sdk/go/client/*`, `sdk/ts/src/client/*` via `make sdk-gen`; `component/architecture/embedded/platform.graph.json` via `make platform-graph`

**Interfaces:**
- Consumes: `@accessGate` is not used here; the platform constructs are operator-side.
- Produces, the DSL contract every later task reads:

| Construct | Annotations | Args | Returns |
| --- | --- | --- | --- |
| `concept siteAccessCode` | `@rowAuthz(owner="ownerUserId", clusterOwner, account="accountId")` | fields per spec section 4 | |
| `concept siteAccessSession` | `@rowAuthz(owner="ownerUserId", clusterOwner)`, `@retention(ttl="30d", keepLatest=false, archive="none")` | fields per spec section 4 | |
| `shape siteAccessCodeView` | `@row` | `row.id, siteId, label, gate, subject, codeHint, generation, issuedAt, expiresAt, revokedAt, row.createdAt`. Never `codeHash`. | |
| `shape siteAccessSessionFull` | `@row` | `row.id, siteId, codeId, generation, issuedAt, endedAt` | |
| `query siteAccessCodeRowsForSite` | `@actor @serverOnly @cache(0)` | `siteId string!` | `siteAccessCodeView` |
| `query siteAccessCodeById` | `@actor @serverOnly @cache(0) @unbounded(...)` | `codeId string!` | `siteAccessCodeView` |
| `query siteAccessCodeByHash` | `@actor @serverOnly @cache(0) @unbounded(...)` | `siteId string!`, `codeHash string!` | `siteAccessCodeView` |
| `query siteAccessSessionLatestForCode` | `@actor @serverOnly @cache(0)` | `codeId string!`, `generation int!` | the newest session of that generation by `issuedAt`, one row |
| `query siteAccessSessionByToken` | `@actor @serverOnly @cache(0) @unbounded(...)` | `tokenHash string!` | `siteAccessSessionFull` |
| `mutation createSiteAccessCode` | `@actor @serverOnly` | `codeId, siteId, accountId?, label, gate, subject?, codeHash, codeHint, issuedAt, expiresAt, lastChangedBy` | stamps `id`, `ownerUserId: actor.userId`, `generation: 1` |
| `mutation rotateSiteAccessCode` | `@serverOnly` | `codeId, codeHash, codeHint, generation, issuedAt, expiresAt, lastChangedBy` | |
| `mutation setSiteAccessCodeExpiry` | `@serverOnly` | `codeId, expiresAt, lastChangedBy` | |
| `mutation removeSiteAccessCode` | `@serverOnly` | `codeId, revokedAt, lastChangedBy` | |
| `mutation createSiteAccessSession` | `@actor @serverOnly` | `sessionId, siteId, codeId, generation, tokenHash, issuedAt` | stamps `id`, `ownerUserId: actor.userId` |
| `mutation endSiteAccessSession` | `@serverOnly` | `sessionId, endedAt` | |
| `builtin siteAccessCodeCreate` | `@sdk @executor("integration.siteAccess.create") @args(profile="object") @requiresCapability("execute","app:deployables/access")` | `siteId, label, gate, subject?, expiresAt` | `{codeId, code, expiresAt}` |
| `builtin siteAccessCodeRegenerate` | same, `.regenerate` | `codeId, expiresAt?` | `{codeId, code, expiresAt}` |
| `builtin siteAccessCodeSetExpiry` | same, `.setExpiry` | `codeId, expiresAt` | `{codeId, expiresAt}` |
| `builtin siteAccessCodeRemove` | same, `.remove` | `codeId` | `{codeId}` |
| `builtin siteAccessGates` | same, `.gates` | `siteId` | one row per gate: `{gate, usesSubject, constructs}` |
| `builtin siteAccessCodesForSite` | same, `.list` | `siteId` | one row per code in `siteAccessCodeView` |

Organization targets: `siteAccessCodeCreate`, `siteAccessCodesForSite` and `siteAccessGates` to `v1:platform:site` on `siteId`; `siteAccessCodeRegenerate`, `siteAccessCodeSetExpiry` and `siteAccessCodeRemove` to `v1:platform:siteAccessCode` on `codeId`.

- [ ] **Step 1: Write the failing tests.** Extend the parity `want` map with the five builtins and the query, all `app:deployables/access`. Add to `component/memql` a db-gated test mirroring `platform_site_preview_guard` that a client-origin `createSiteAccessCode` is refused by `@serverOnly`.

- [ ] **Step 2: Run to verify failure**

Run: `go test github.com/znasllc-io/memql/component/memql -run 'TheDeployablesPartsAreDeclared|SiteAccess' -count=1`
Expected: FAIL.

- [ ] **Step 3: Write the DSL, the seeds, the mirror and the part.** Copy `sitePreviewGrant`'s description voice, stating why each field exists. Mirror `siteById` for the composite tier's filter conjunct. Add the plugin skeleton.

- [ ] **Step 4: Regenerate and run every gate this touches**

Run: `make sdk-gen && make platform-graph && go test github.com/znasllc-io/memql/component/memql -run 'TheDeployablesPartsAreDeclared|SeedMatchesCompiledMirror|OsRegistryRequiresMatch|SiteAccess' -count=1 && go test github.com/znasllc-io/memql/component/auth/... -count=1 && go test github.com/znasllc-io/memql/test/dslconformance/... -count=1 && make dsl-lint && make sdk-gen-check`
Expected: PASS. `test/dslconformance` must classify the new constructs; fix the DSL rather than the gate.

- [ ] **Step 5: Commit** `feat(platform): DSL contract and capability part for access codes`, including the regenerated files by explicit path.

### Task 9: Bind gates to the package's DSL

Runs after Task 3 and before Task 4. It is numbered last only because it was added after an automated security review of Task 2.

**Files:**
- Modify: `dsl/platform/concepts.memql` (`package.dslDomains []string`, with a description in the voice of `declares`), the mutation and shape `recordPackageDeployables` writes through, `component/packages/pipeline.go` (beside line 720), the store method behind `recordPackageDeployables`, `component/packages/analyze.go` (after `AnalyzePackageDSL` at line 216), `component/packages/refusal.go` (`CodeDslAccessGateForeignDeployable = "dsl_access_gate_foreign_deployable"`), `component/memql` (`AnalyzePackageDSL`'s result carries the loaded gates), `docs/public/operate/packages.md` (the refusal table)
- Regenerate: the SDK via `make sdk-gen` if the concept change reaches it
- Test: `component/packages/analyze_test.go` or its neighbours, whichever hold the `dsl_refuses_boot` cases

**Interfaces:**
- Consumes: Task 2's `AccessGateEntry.Domain`.
- Produces: the `dslDomains` field on `v1:platform:package`, in the shape `packageById` and `packagesAll` return, read by Tasks 4 and 5. `result.AccessGates []AccessGateEntry` on `AnalyzePackageDSL`'s result. `AnalyzePackageDSL` runs with `publish=false`, so `AccessGateEntries()` does not see the candidate: return the entries out of `loadAccessGates` itself. `AccessGateEntry.Domain` is the concept-id namespace, so `dslDomains` must use that spelling, which under a `namespace.pin` is the pin rather than the directory name.

- [ ] **Step 1: Write the failing tests:**

```go
func TestAnalysisRefusesAGateNamingADeployableOutsideTheManifest(t *testing.T) // dsl_access_gate_foreign_deployable, scoped to the construct's file, fatal
func TestAnalysisAcceptsAGateNamingItsOwnDeployable(t *testing.T)
func TestAnalysisRefusesAGateBoundToADomainThePackageDoesNotShip(t *testing.T) // bound to a core or foreign concept: it could only ever 404, so say so at analysis
func TestAnalysisRecordsTheDomainsThePackageShips(t *testing.T)                // non-reserved only; rewritten whole; a domain dropped upstream leaves the list; recorded as the concept-id namespace, so a namespace.pin records the pin and not the directory
```

- [ ] **Step 2: Run to verify failure.** Run: `go test github.com/znasllc-io/memql/component/packages/... -run 'Analysis' -count=1`. Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify pass**, same command plus `make sdk-gen-check`, `make dsl-lint` and `go test github.com/znasllc-io/memql/test/dslconformance/... -count=1`. Expected: PASS.

- [ ] **Step 5: Commit** `feat(packages): bind access gates to the package that declares them`.

### Task 4: Operator integration

**Files:**
- Modify: `integrations/siteaccess/integration.go`, `call_origin_conformance_test.go` (allowlist entry with its reason, beside `integrations/sitepreview` at line 276), `scripts/ci/db-gated-packages.sh` (line 300), `scripts/ci/shard-timings.tsv`
- Create: `integrations/siteaccess/store.go`
- Test: `integrations/siteaccess/integration_db_test.go`, `integrations/siteaccess/main_dbschema_test.go`

**Interfaces:**
- Consumes: Task 1 primitives; Task 2 `AccessGatesFor`; Task 3 DSL; Task 9 `package.dslDomains`. Read the site's package under the caller to get the domains it passes.
- Produces, in package `siteaccess`, for Task 5's adapter. The store reads and updates rows as the cluster acting, with internal origin, so a row's current owner does not matter; only creates run under the site owner's actor, so a row is stamped with the site's owner at birth. The operator handlers authorize under the CALLER's actor, so the composite tier decides, as `integrations/sitepreview` does:

```go
type Code struct { ID, SiteID, AccountID, Gate, Subject string; Generation int; ExpiresAt time.Time; Revoked bool }
type Session struct { ID, SiteID, CodeID string; Generation int; IssuedAt time.Time; Ended bool }
func NewGraphStore(engine Engine) *GraphStore
func (s *GraphStore) CodeByHash(ctx context.Context, siteID, codeHash string) (*Code, error)
func (s *GraphStore) CodeByID(ctx context.Context, codeID string) (*Code, error)
func (s *GraphStore) SessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error)
func (s *GraphStore) CreateSession(ctx context.Context, owner string, in Session, tokenHash string) error
func (s *GraphStore) EndSession(ctx context.Context, sessionID string, at time.Time) error
```

A miss is `(nil, nil)`, never an error. Decisions on the fence:
- Every act passes `ContextWithRowVersionFence` with the `createdAt` of the view it read, as the witness. Without it, two regenerates that both read generation 1 both report success at generation 2, and the first operator's code is dead. A mint passes a site witness the same way.
- A witness applies to every write under that context, so it must not reach the audit write that follows the code's write, or that write is refused.
- A loss (`errors.Is(err, memql.ErrRowVersionChanged)`) re-reads and retries once, then reports the conflict in a sentence. An act never reports success for a write that did not land.
- Act-on-code handlers re-read the code's site under the caller, and refuse when it is unreadable. `siteAccessSessionLatestForCode` is read as the site's owner, since the session tier has no account arm; otherwise an account member sees an empty `lastOpenedAt`.
- A stale code, whose copied account differs from its site's, can be removed only by a cluster operator, since the organization gate judges the copy. It opens nothing, and it blocks a further move until removed; say so in the move refusal's next step if the handler can tell.

- [ ] **Step 1: Write the failing tests**, db-gated against a real engine, as `storefront_testing_db_test.go` does:

```go
func TestCreateReturnsTheCodeOnceAndStoresOnlyItsDigest(t *testing.T) // reply carries code; siteAccessCodesForSite never does; row's codeHash == AccessCodeDigest(code)
func TestCreateRefusals(t *testing.T) // table: no owner; system-owned; no packageDeployableName; undeclared gate; label ""; label 81 chars; subject 121 chars; expiry 30s; expiry 400d; a site the caller cannot read
func TestCreateCopiesOwnerAndAccountFromTheSite(t *testing.T)         // a developer mints; ownerUserId is the site's; lastChangedBy is the developer
func TestRegenerateReplacesTheSecretAndKeepsEverythingElse(t *testing.T) // same id, label, gate, subject; generation+1; old digest gone; expiry kept unless given
func TestRegenerateOfAnExpiredCodeRequiresANewExpiry(t *testing.T)
func TestRegenerateOfARemovedCodeIsRefused(t *testing.T)              // removal is final; the refusal says to add a new code
func TestSetExpiryKeepsTheCode(t *testing.T)                          // codeHash and generation unchanged; bounds enforced; an expired code revives
func TestRemoveIsSoft(t *testing.T)                                   // revokedAt set; row still readable; a second remove is harmless
func TestEveryOperatorActIsAudited(t *testing.T)                      // one auditEvent per act with targetType siteAccessCode, and no event contains the code
func TestGatesAnswerForThisDeployableOnly(t *testing.T)
func TestListCarriesLastOpenedForTheCurrentGeneration(t *testing.T)  // a regenerated code reads as not opened, though older sessions exist
func TestActsReReadTheCodesSiteUnderTheCaller(t *testing.T)
func TestAFenceLossIsRetriedOnceThenReported(t *testing.T)
func TestTwoRegeneratesFromOneReadDoNotBothSucceed(t *testing.T)
func TestTheAuditWriteIsNotFenced(t *testing.T)
func TestAMemberSeesLastOpened(t *testing.T)                  // read as the site's owner
func TestAnActThroughAnotherOrganizationIsRefused(t *testing.T)       // capability_not_held
```

- [ ] **Step 2: Run to verify failure**

Run: `MEMQL_REQUIRE_DB=1 go test github.com/znasllc-io/memql/integrations/siteaccess/... -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement.** Handlers run every gate before minting, in the order a person would ask them, as `handleOpen` does. Audit through `createAuditEvent` as `integrations/rbac/grants.go` does.

- [ ] **Step 4: Run to verify pass**, same command, plus `go test github.com/znasllc-io/memql -run TestOnlyAllowlistedPackagesStampInternalOrigin -count=1` and `go test github.com/znasllc-io/memql/test/dslconformance/... -run ServerOnly -count=1`. Expected: PASS.

- [ ] **Step 5: Commit** `feat(siteaccess): mint, regenerate, expire and remove access codes`.

### Task 5: The bff access handler

**Files:**
- Create: `component/server/access_handler.go`, `component/server/access_handler_test.go`, `app/transport_access_surface.go`
- Modify: `component/server/nethttp.go` (`AccessSurfacePaths()`), `component/server/unauthenticated_surface.go` (beside line 151), `cmd/frontdoorpaths/main.go` (`servedButNotExternallyRouted`, beside line 299), `app/transport_bff.go` (beside line 46), `component/memql/metrics` or wherever `memql_auth_rejects_total` is declared, for `memql_site_access_requests_total{op,result}`
- Regenerate: whatever `make` target `TestFrontDoorPathsAreNotStale` names, if it reports drift

**Interfaces:**
- Consumes: Task 1; Task 2 `AccessGateFor`, `ReservedAccessArgs`; Task 4 `GraphStore` through an adapter; `memql.MaterializeRows`. Render the call as `dslCall(entry.Call(), args)`: `Call()` is qualified, because the engine refuses an ambiguous bare name, and a bare call would let a same-named construct anywhere break a gate.
- Produces, in package `server`:

```go
const AccessCookieName = memql.AccessCookieName
const AccessStampSiteHeader, AccessStampOwnerHeader = "X-Memql-Access-Site", "X-Memql-Access-Owner"
func AccessSurfacePaths() []string // the /access/ base; verify nothing already mounts it
type AccessSite struct { ID, OwnerUserID, AccountID, DeployableName string; PackageDomains []string }
type AccessCode struct { ID, SiteID, AccountID, Gate, Subject string; Generation int; ExpiresAt time.Time; Revoked bool }
type AccessSession struct { ID, SiteID, CodeID string; Generation int; IssuedAt time.Time; Ended bool }
type AccessStore interface {
	AccessSite(ctx context.Context, siteID, owner string) (*AccessSite, error)
	CodeByHash(ctx context.Context, siteID, codeHash string) (*AccessCode, error)
	CodeByID(ctx context.Context, codeID string) (*AccessCode, error)
	SessionByTokenHash(ctx context.Context, tokenHash string) (*AccessSession, error)
	CreateSession(ctx context.Context, owner string, in AccessSession, tokenHash string) error
	EndSession(ctx context.Context, sessionID string, at time.Time) error
}
func NewAccessHandler(engine MemQLExecutor, store AccessStore, logger *slog.Logger, now func() time.Time) *AccessHandler
```

The wire is exactly spec section 6. `remainingMs` is `AccessSessionEnd(session.IssuedAt, code.ExpiresAt) - now`. A request with no stamp is `404`, and a stamp naming a site its owner cannot read is `404`.

- [ ] **Step 1: Write the failing tests** against fakes for the store and engine, with an injected clock:

```go
func TestUnlockSetsTheCookieAndAnswersRemaining(t *testing.T) // every attribute in Global Constraints; 200 body fields; session row written
func TestUnlockForgivesTyping(t *testing.T)                   // lowercase, whitespace, O/I/L
func TestUnlockInvalidIsOneAnswer(t *testing.T)               // unknown, other site's, revoked, superseded: identical 401 bodies, nothing written
func TestACodeWhoseAccountDiffersFromItsSiteOpensNothing(t *testing.T) // unlock answers invalid, a live session ends: a move racing a mint leaves a harmless code
func TestUnlockExpired(t *testing.T)                          // 403 expired, no cookie, nothing written
func TestSessionIssuedAtIsWholeSecondUTC(t *testing.T)        // the latest-session sort compares datetimes as text
func TestSessionEndsAtTheInstant(t *testing.T)                // clock at expiry-1ms ok; at expiry 401 ended, cookie cleared; no leeway
func TestSessionCapIs24Hours(t *testing.T)
func TestSessionEndedByRegenerateRemoveAndLeave(t *testing.T)
func TestExtendingAnExpiryMovesRemaining(t *testing.T)
func TestLeavingEndsThisSessionOnly(t *testing.T)             // two sessions on one code
func TestLeaveIsIdempotent(t *testing.T)
func TestNoCookieAnswersNone(t *testing.T)                    // and a stranger learns no gate name
func TestCallRunsTheDeclaredConstructUnderTheOwner(t *testing.T) // recorded query text and actor == owner; never internal origin
func TestCallStampsReservedArgumentsLast(t *testing.T)
func TestCallReturnsBareIds(t *testing.T)                     // a row's id leaves the handler as the bare short id, as every other wire does: clients never see v1: ids
func TestAGatedMutationAnswersOnlyItsBareId(t *testing.T)     // a mutation has no shape clause, so its node would hand over payload fields, created_by and the concept's description, including another code's on a repeat of a known id
func TestCallRefusesForgedReservedArguments(t *testing.T)     // each of the four: 400 invalid_args, engine not called
func TestCallRefusesUndeclaredArgumentNames(t *testing.T)     // including `x: 1) mutation evil(` as a key
func TestCallNotFoundIsOneAnswer(t *testing.T)                // unknown, other gate, other deployable, a domain the site's package does not ship, no recorded domains, undeclared gate: identical 404 bodies
func TestCallAfterExpiryNeverReachesTheEngine(t *testing.T)
func TestRefusalIsNotLoggedWithArguments(t *testing.T)        // captured slog holds no code, token or argument value
func TestRepliesAreNoStore(t *testing.T)
func TestUnstampedRequestIs404(t *testing.T)
func TestForgedOwnerReadsNothing(t *testing.T)                // including a forged owner who is a member of the site's organization, whom the tier's account arm admits: compare the stamp with the site's ownerUserId
```

- [ ] **Step 2: Run to verify failure**

Run: `go test github.com/znasllc-io/memql/component/server -run Access -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement the handler, the surface declarations and the bff mount.** `app/transport_access_surface.go` adapts `siteaccess.GraphStore` to `AccessStore` and resolves `AccessSite` with `query siteById` under the owner's actor, as `shopperSiteReader` does, requiring a non-empty `packageDeployableName`, and reads `PackageDomains` from the site's `packageId` row. Discarding the read error would open nothing, since an empty list misses; log it. Mount `GET` and `POST` on the same paths, letting the handler answer `404`, for the reason written on `mountShopperSurface`.

- [ ] **Step 4: Run to verify pass**

Run: `go test github.com/znasllc-io/memql/component/server/... -count=1 && go test github.com/znasllc-io/memql/cmd/frontdoorpaths/... -count=1 && go test -tags bff github.com/znasllc-io/memql/app/... -run 'Surface|Route' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit** `feat(server): the access surface's bff handler`.

### Task 6: The edge route

**Files:**
- Create: `component/edge/access.go`, `component/edge/access_test.go`
- Modify: `component/edge/handler.go` (a branch beside the shopper surface at line 296, above the `apiPrefix` proxy), `component/edge/doc.go` (the route list)

**Interfaces:**
- Consumes: `server.AccessStamp*` header names as literal constants; the edge does not import `component/server`, so repeat them with a test pinning equality, as `shopperStampHeaders` does.
- Produces: `func (h *Handler) serveAccessSurface(w http.ResponseWriter, r *http.Request, site *Site)`.

| Inbound | Forwarded path |
| --- | --- |
| `POST /_memql/access` | `/access/unlock` |
| `GET /_memql/access` | `/access/session` |
| `POST /_memql/access/leave` | `/access/leave` |
| `POST /_memql/access/call` | `/access/call` |

- [ ] **Step 1: Write the failing tests** against a fake upstream, as `shopper` tests do:

```go
func TestAccessRoutesFourPairsAndNothingElse(t *testing.T) // table; also DELETE, /_memql/access/x, and /_memql/accessories, which must reach serveAPI
func TestAccessIgnoresAPIProxy(t *testing.T)               // works with apiProxy false
func TestAccessStripsThenStamps(t *testing.T)              // inbound X-Memql-Access-* never survive
func TestAccessRefusesASiteWithNoOwner(t *testing.T)       // 404, nothing forwarded
func TestAccessRefusesAForeignOrigin(t *testing.T)         // POST with another host's Origin; an absent Origin is allowed; a custom-domain Host that matches is allowed
func TestAccessRequiresJSONOnPost(t *testing.T)
func TestAccessCapsBodies(t *testing.T)                    // 1 KiB unlock and leave, 64 KiB call, 413 before forwarding
func TestAccessRateLimitsUnlockPerAddressAndPerSite(t *testing.T) // 10 per address; 60 per site with spoofed addresses; 429 with Retry-After
func TestAccessRateLimitsOtherOperationsSeparately(t *testing.T)  // a burst of status polls does not spend unlock's budget
func TestAccessRepliesCarryNoStore(t *testing.T)
func TestAccessSiteOfflineAnswersBeforeForwarding(t *testing.T)   // disabled 503, draft 404, nothing forwarded
func TestAccessCookiePassesThrough(t *testing.T)           // the visitor's cookie reaches the bff; Set-Cookie reaches the visitor
```

- [ ] **Step 2: Run to verify failure**

Run: `go test github.com/znasllc-io/memql/component/edge -run Access -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement**, reusing `newBFFProxy` and the shopper bucket type with separate instances. The edge never reads the cookie's value and never decides access; a comment says so, citing `app/auth_mode_edge.go`.

- [ ] **Step 4: Run to verify pass**

Run: `go test github.com/znasllc-io/memql/component/edge/... -count=1`
Expected: PASS.

- [ ] **Step 5: Commit** `feat(edge): route the access surface on a deployable's own origin`.

### Task 7: The Access codes panel in MemQL OS

Read `.agents/skills/memql-ui-design/SKILL.md`, `clients/os/DESIGN.md` and `clients/os/SUPERVISED-VISUAL-COMPOSITION.md` first, as `CLAUDE.md` requires. Use the frontend-design skill within that brief.

**Files:**
- Create: `clients/os/src/apps/deployables/access/useAccessCodes.ts`, `clients/os/src/apps/deployables/access/AccessCodesPanel.tsx`, `clients/os/src/apps/deployables/access/expiry.ts`, `clients/os/test/deployables/accessCodes.test.tsx`
- Modify: `clients/os/src/apps/deployables/page/DeployablePage.tsx` (Head at line 431, the dialog branch at line 508, `detailTitle` at line 659), `clients/os/src/apps/deployables/page/DeployableWorkspace.tsx` (`WorkspaceDetail` at line 24), `clients/os/test/deployables/harness.tsx` (call branches beside line 586), `clients/os/qa/main.tsx` (views `access`, `access-empty`, `access-minted`, `access-no-gates`)

**Interfaces:**
- Consumes: Task 3's generated builders on `connection.query`: `siteAccessCodesForSite`, `siteAccessGates`, `siteAccessCodeCreate`, `siteAccessCodeRegenerate`, `siteAccessCodeSetExpiry`, `siteAccessCodeRemove`. The part name `access` in `parts.ts`.
- Produces: `<AccessCodesPanel site={site} />`, and from `expiry.ts`:
  - `EXPIRY_PRESETS`, the five in spec D8, in order
  - `expiryWord(expiresAt: string, now: number): string` for "Expires in 8 minutes" and "Expired"
  - `expiryFromPreset(preset, now: number): string`

Controls, copy and states are specified in spec section 5. `useAccessCodes` reads through `useReading` and re-reads after each write, holds the minted code in state only, and drops it on the next act. The Head control is `<IconButton label="Access codes">` with lucide `Lock`, after Traffic, rendered only when `can.access && !site.systemOwned && site.packageDeployableName`.

- [ ] **Step 1: Write the failing tests** on the shared harness:

```tsx
test("the lock sits after Traffic and is absent without the part, on a system-owned site, and with no package deployable")
test("the list shows label, what it unlocks, hint, expiry word and last opened, and never a code")
test("a failed read does not look empty; loading is a skeleton; no gates explains @accessGate and has no Add control")
test("Add preselects a single gate, asks for a subject only when the gate uses one, and sends the expiry the preset names")
test("custom expiry names the time zone and a server refusal renders in place with its sentence, draft kept")
test("the new code appears once with a warning and copy, and Done discards it")
test("the minted code is gone after any other act and is not in any row or storage")
test("a double click on Create sends one call")
test("Regenerate and Remove ask first, say what stops working, and Keep it writes nothing and returns focus")
test("a code whose gate is no longer declared still lists and says so")
test("a removed code is not listed, and the list is in label order")
test("lastOpenedAt reads as Last opened; its absence reads Not opened yet within 30 days of issuedAt, and Not opened in the last 30 days after, since sessions are retired at 30 days")
```

- [ ] **Step 2: Run to verify failure**

Run: `cd clients/os && npm test -- test/deployables/accessCodes.test.tsx`
Expected: FAIL.

- [ ] **Step 3: Implement.** Keep the code in `useState` only; add no field for it to a row type. Use `Button busy`, `InPlaceAsk`, `CopyValue`, `Notice`, `RecordList`, `AddButton`, `DateTimeInput`.

- [ ] **Step 4: Run to verify pass**

Run: `cd clients/os && npm test -- test/deployables && make -C ../.. os-typecheck`
Expected: PASS. The existing assertion `logs.nextElementSibling === traffic` still holds.

- [ ] **Step 5: Render and critique.** Run `npx vite --config qa/vite.config.ts --port 5199 --strictPort` from `clients/os`, capture each new view at `mode=light` and `mode=dark`, plus a narrow width, read the images and fix what looks wrong. No live credentials: fixtures only.

- [ ] **Step 6: Commit** `feat(os): manage a deployable's access codes`.

### Task 8: Documentation and the PR

**Files:**
- Create: `docs/public/operate/site-access-codes.md`
- Modify: `CLAUDE.md` (a row in Allowed HTTP Exceptions, owner-approved 2026-10-29, and the Deployables part list), `docs/public/operate/site-hosting.md`, `docs/public/operate/deployables.md`, `docs/public/build/building-a-pack.md`, `clients/os/README.md`, `component/language/annotations/registry.go` (the long doc, if Task 2 left a short one)

**Interfaces:**
- Consumes: everything above. The doc states the wire exactly as Task 5 built it, and the client's three obligations from spec section 8.

- [ ] **Step 1: Write the docs.** `site-access-codes.md` covers what a code opens and does not (nothing in a public bundle is protected), `@accessGate` with one worked example, the reserved arguments and how they isolate data, the wire, the client's obligations, and the limits stated plainly (idle tab within 5 seconds, per-replica rate limits, borrowed authority not re-checking the owner). Follow the frontmatter of its neighbours.

- [ ] **Step 2: Run the whole gate**

Run: `make test && make sdk-gen-check && make platform-graph-check && make dsl-lint && make os-test && make os-typecheck`, then the db-gated trees with `MEMQL_REQUIRE_DB=1`: `scripts/ci/db-gated-packages.sh --trees` names them.
Expected: PASS. Report anything skipped.

- [ ] **Step 3: Commit** `docs: access codes for deployables`.

- [ ] **Step 4: Push and open the PR**

```bash
git push -u origin claude/deployable-access-codes
gh pr create --repo znasllc-io/memql --title "Deployable access codes" --body-file <body>
```

The body names the owner approvals from the spec, calls out the new HTTP family and the new gRPC-visible builtins for the frontend ping `CLAUDE.md` asks for, and ends with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`. Use `ccd_pr` tools to read CI. Stop at green `MemQL / memql`: a session never merges.
