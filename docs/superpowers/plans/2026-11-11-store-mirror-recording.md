# Store mirror recording Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every purchase on a connected Shopify store is recorded with nobody watching, each store keeps its own sync state, and the storefront's Store page says whether a store is recording, why not and what fixes it, with an email to the store's owner when it stops.

**Architecture:** The managed connection asks for a commerce scope set, and a domain outside a store's connection is "not requested", never an error. The sync runtime gains a partition (the store) on sync state through one optional connector interface, queues and continues backfills itself, retries a failed delivery three times, and moves the reconcile watermark to the sweep's start. The Shopify integration computes one verdict for each store from the store row, its sync state and its given-up deliveries; MemQL OS shows it on the deployable's Store slot and Store page and groups Cluster > Data by store.

**Tech Stack:** Go (`component/memql/sync`, `component/datasync`, `integrations/shopify`, `cmd/shopifyschema`), MemQL DSL (`dsl/platform`, `dsl/common`, `dsl/shopify/overlay`), React and TypeScript (`clients/os`), Vitest.

**Spec:** `docs/superpowers/specs/2026-11-11-store-mirror-recording-design.md`. Executors read both. The spec's rollout (section 12) and walk (section 10) are operator steps on the owner's instance and are not tasks here.

## Global Constraints

- Work only in `/Users/znas/projects/memql/.claude/worktrees/store-mirror`, branch `feat/store-mirror`, by absolute path. Never touch `/Users/znas/projects/memql` itself or another worktree. Never stash, reset, switch branch or push.
- Every task's tests fail first (record the command and the failing output), then pass.
- Every `go test` carries `MEMQL_DATABASE_DSN="$MIRROR_DSN"`, where Task 1 exports `MIRROR_DSN='postgres://memql:memql_dev@127.0.0.1:55432/memql_mirror_a?sslmode=disable'`. Database lanes add `MEMQL_REQUIRE_DB=1` and `-p 1`. Never port 5432. Name packages; a bare `go test ./...` at the root misses the engine and is not a verification. The whole tree is `make test`.
- Generated files are regenerated, never hand-edited (`sdk/go/client/generated_*`, `sdk/ts/src/client/generated_*`, `integrations/shopify/generated/*`, `dsl/shopify/generated/*`, the concept snapshot, the platform graph). After any `.memql` change: `make sdk-gen`; `make concept-snapshot` when a concept gained a field or an enum value; `make platform-graph` when a concept or an automation changed. Then `make sdk-gen-check`, `MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -count=1 -run 'TestConceptFieldSnapshotIsNotStale|TestPlatformGraph' .` and `MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -count=1 ./test/dslconformance/...`. If a root gate names another stale file, run the make target its message names.
- Fields are added, never removed or renamed (memql#5199): `syncState.partition`, the `backfillStatus` value `queued`, `inboundRequest.attempts`, `inboundRequest.nextAttemptAt`.
- The commerce scope set, in this order: `unauthenticated_read_product_listings`, `unauthenticated_read_checkouts`, `unauthenticated_write_checkouts`, `unauthenticated_read_customers`, `write_products`, `read_inventory`, `read_locations`, `read_orders`, `read_customers`, `write_customers`, `read_discounts`, `read_returns`. `read_all_orders` is not requested.
- A delivery has four tries at most: the first, then retries 1 minute, 5 minutes and 30 minutes after the try before.
- The reconcile watermark is the sweep's start less two minutes.
- The alert wait is 30 minutes when unset and may be set from 1 to 1440 minutes, in the global variable `SHOPIFY_STORE_ALERT_MINUTES`.
- Schedules (six-field cron, as the tree uses): continue backfills `0 */5 * * * *`, retry deliveries `0 * * * * *`, watch stores `0 */5 * * * *`.
- Verdict states, exact strings: `recording`, `starting`, `paused`, `needs attention`, `not connected`. Words drawn in the OS: Recording, Starting, Paused, Needs attention. "Uninstalled" and "Data removed" stay as they are.
- Reason codes and their sentences, verbatim (`{domains}` and `{domain}` are plain words from the table below, `{n}` a count):
  - `data_not_declared`: "Shopify refuses {domains} for this app, because the app has not declared the customer data it uses."
  - `approval_outdated`: "This store approved an older list of permissions." Act label: "Open MemQL OS from the store's Shopify admin and approve".
  - `grant_lost`: "MemQL can no longer reach this store." Act: reconnect in Settings.
  - `events_not_registered`: "Shopify is not sending every change." Act label: "Register again".
  - `check_failing`: "The last check of {domain} failed", then the engine's reason. No act.
  - `changes_not_recorded`: "{n} changes could not be recorded." (one: "1 change could not be recorded."). Act label: "Try again".
- A domain is named in plain words, never by concept id: order "orders", customer "customers", product "products", productVariant "product variants", collection "collections", discountNode "discounts", inventoryItem "inventory", location "locations", abandonedCheckout "abandoned checkouts", tenderTransaction "payments", segment "customer segments", company "companies", companyLocation "company locations", priceList "price lists", sellingPlanGroup "selling plans", inventoryShipment "inventory shipments", inventoryTransfer "inventory transfers". Any other concept: its camel-case name split into lower-case words.
- Kinds on the Store page and the domain each reads its two times from: catalog (product), orders (order), customers (customer), discounts (discountNode), returns (order, shown only when the grant holds `read_returns`).
- No model or agent is called on any of these paths.
- Multi-node: the three scheduled passes run as automations under the cron leader; the connect callback can run on any node and only queues. Nothing new is kept in memory across requests.
- Go renders a named call through `call(...)` (datasync) or `renderCall(...)` (shopify), never `%q`. A number read from a payload is narrowed through `core/num`. A builtin that only an automation may call refuses a client call in its handler (`requireInternalOrigin`), because a builtin takes no `@serverOnly`.
- OS UI follows `clients/os/DESIGN.md`: kit components only, no dividers between rows, a state in plain text with no pill, badge or chip in a row, `Notice` and never a toast, a control whose part is not held is absent and not disabled. Each screen is checked in both themes and at both widths.
- Vocabulary: component, integration, pack; never "plugin". No emojis. The repository is public: no client, person, store domain or instance hostname in code, tests, docs or commits. Write "a development store" and "the owner's instance"; fixtures use the harness's `acme-widgets.myshopify.com`.
- Commits are `<area>: <sentence>`, staged by explicit path (never `git add -A`), ending with `Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>`.

## Review Focus

- A retried delivery is swallowed as a duplicate: the connector records a webhook id before it fetches, for 15 minutes, so the retries at 1 and 5 minutes would be stamped `processed` with nothing applied. A person expects the retry to fetch again. Test in Task 7.
- Two bulk operations on one store, or a tick that polls a running one hundreds of times: Shopify runs one bulk operation for a store at a time, and a poll that made no progress returns the cursor it was given. A person expects one domain of a store at a time and one poll a tick. Tests in Task 6.
- A domain Shopify refuses for a scope the connection never asks for (the generated scope for inventory shipments and transfers is `read_inventory`, and Shopify asks for `read_inventory_shipments` and `read_inventory_transfers`). The same holds for a domain the store's plan does not have. A person expects "not part of this connection", not a store that never reads Recording. Tests in Task 2 and Task 8.
- A paused store whose watermark moves while the other stores are swept, so the order placed during the pause is never read. A person expects the first sweep after Resume to start where that store stopped. Tests in Task 3 and Task 4.
- A sync-state row written before this version, with no partition, drawn as a store's state or continued as a backfill forever. A person expects it left as history. Tests in Task 6 and Task 11.

---

### Task 1: Workspace and baseline

**Files:** none changed.

- [ ] **Step 1:** Copy the git-ignored identity bundle: `cp -R /Users/znas/projects/memql/component/identity/web/static/. /Users/znas/projects/memql/.claude/worktrees/store-mirror/component/identity/web/static/`.
- [ ] **Step 2:** In `sdk/ts` run `npm ci && npm run build`; in `clients/os` run `npm ci`.
- [ ] **Step 3:** `docker exec memql-testdb createdb -U memql memql_mirror_a`, then `export MIRROR_DSN='postgres://memql:memql_dev@127.0.0.1:55432/memql_mirror_a?sslmode=disable'`.
- [ ] **Step 4:** Baseline, all expected PASS: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -p 1 -count=1 ./component/memql/sync/ ./component/datasync/ ./integrations/shopify/ ./cmd/shopifyschema/`; in `clients/os`, `npm run typecheck` and `npx vitest run test/deployables test/cluster`. Record any failure that exists before a change and stop: a dirty baseline is the controller's call.

### Task 2: Coverage (U1)

**Files:**
- Modify: `integrations/shopify/connections.go`, `integrations/shopify/store.go`, `integrations/shopify/reconcile.go`, `integrations/shopify/capabilities.go`, `integrations/shopify/offline_token.go`, `integrations/shopify/uninstall.go`, `component/memql/sync/notgranted.go`, `component/datasync/runners.go`, `docs/public/operate/shopify-connect.md`
- Test: `integrations/shopify/coverage_test.go` (new), `integrations/shopify/reconcile_refusal_test.go`, `component/memql/sync/connector_test.go`, `component/datasync/runners_refusal_test.go`

**Interfaces:**
- Produces, in `component/memql/sync`: `var ErrNotRequested = errors.New("connector: not requested")`; `func NotRequested(connector string) error`, whose text is `connector: not requested: <connector>'s connection does not ask for this domain`; `func IsNotRequested(err error) bool`.
- Produces, in `integrations/shopify`: `managedConnectScopes()` returning exactly the commerce set; `func (s Store) Managed() bool` (its `AdminTokenRef` ends in `_OFFLINE_GRANT`; the two inline checks in `offline_token.go` and `uninstall.go` call it); `func connectionScopes(store Store) []string` (`managedConnectScopes()` when managed, else `ConnectScopes()`); `func scopesNotApproved(store Store) []string` (asked scopes the grant does not reach through `grantIncludes`, sorted, nil when nothing is recorded as granted); `func domainInConnection(store Store, spec *generated.TypeSpec) bool`; `func requiredScopeOf(message string) string` (the scope in Shopify's "Required access: `<scope>` access scope", or ""); `func protectedDataRefusal(message string) bool` (the message says "is not approved to access" or links `protected-customer-data`).
- Changes, on each store of `shopifyStoreHealth`: `scopesNeeded` is what the store's own connection asks for and `scopesMissing` what of that the grant lacks (`missingScopes` in `capabilities.go`, which no test names, gives way to `scopesNotApproved`); `domainsNotGranted` lists only domains inside the connection; `domainsOutside` is added (concept ids of swept domains outside it). `phaseOf` answers `not requested` for a row whose `lastError` starts with the not-requested text.

Decisions:
- `Connector.Reconcile` asks a store about a domain in this order: outside its connection, so not requested and no Admin call; a scope its connection asks for is missing from the grant, so `NotGranted` naming it, as today. Then Shopify's own answer to a root `ACCESS_DENIED`: the protected-data refusal stays `ErrNotGranted` carrying Shopify's message; one whose `requiredScopeOf` is a scope the connection asks for stays `ErrNotGranted`; every other root refusal (a scope the connection never asks for, a feature the store's plan does not have) is not requested. Only the first two are something a person can fix.
- `Runner.Reconcile` treats `IsNotRequested` exactly as `IsNotGranted`: skipped, recorded once, re-stated at most every `notGrantedRecheck`.
- The sentence in `shopify-connect.md` beginning "The shared storefront flow requests" lists the twelve scopes in the Global Constraints order and still ends "Register those scopes on the app." `TestTheConnectDocListsTheScopesTheManagedFlowRequests` holds it to the code.

- [ ] **Step 1: Write the failing tests**

```go
func TestTheManagedConnectionAsksForTheCommerceSet(t *testing.T)                // equals the twelve, in order
func TestAStoreGrantedTheOldListLacksExactlyTheNewScopes(t *testing.T)          // read_customers, read_discounts, read_returns, write_customers
func TestAWriteScopeCountsAsItsRead(t *testing.T)                               // granted write_customers: read_customers is not missing
func TestADomainOutsideTheConnectionIsNotRequestedAndNotAsked(t *testing.T)     // blog on a managed store: IsNotRequested, zero Admin calls
func TestARefusalNamingAnUnaskedScopeIsNotPartOfTheConnection(t *testing.T)     // root denied, "Required access: `read_inventory_shipments` access scope.": IsNotRequested
func TestARootRefusalThatNamesNoScopeAndNoCustomerDataIsNotPartOfTheConnection(t *testing.T) // a plan-gated domain: IsNotRequested
func TestAProtectedDataRefusalStaysNotGranted(t *testing.T)                     // "This app is not approved to access the Order object...": IsNotGranted, not IsNotRequested
func TestADomainOutsideTheConnectionIsNeverInScopesMissing(t *testing.T)        // scopesNeeded is the connection's list; domainsOutside names the rest
func TestANotRequestedDomainIsRecordedOnceAndIsNotAFailure(t *testing.T)        // datasync: Skipped, nil error, lastError carries the text, no second write inside notGrantedRecheck
func TestNotRequestedIsNotNotGranted(t *testing.T)                              // sync: the two sentinels do not match each other
```

- [ ] **Step 2:** Run `MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -count=1 ./component/memql/sync/ ./component/datasync/ ./integrations/shopify/ -run 'CommerceSet|OldList|WriteScopeCounts|OutsideTheConnection|UnaskedScope|NamesNoScope|ProtectedDataRefusal|NotRequested|ConnectDocLists'`. Expected: FAIL (undefined names; the doc test fails once the list grows).
- [ ] **Step 3:** Implement, and edit the doc sentence.
- [ ] **Step 4:** Run the same command, then the three packages whole. Expected: PASS.
- [ ] **Step 5: Commit** `shopify: a managed connection asks for the commerce set, and what it does not ask for is not an error`.

### Task 3: Partitions in the contract and the runtime (U2)

**Files:**
- Create: `component/memql/sync/partitioned.go`, `component/datasync/partitions_db_test.go`
- Modify: `component/memql/sync/connector.go`, `component/datasync/store.go`, `component/datasync/runners.go`, `component/datasync/inbound.go`, `component/datasync/capabilities.go`, `dsl/platform/concepts.memql` (`syncState`), `dsl/platform/shapes.memql` (`syncStateFull`), `dsl/platform/queries.memql`, `dsl/platform/mutations.memql` (`upsertSyncState`), `dsl/common/builtins.memql`
- Regenerate: the SDKs, the concept snapshot, the platform graph
- Test: `component/datasync/syncstate_id_test.go`, `component/datasync/runners_test.go`, `component/datasync/inbound_test.go`, `component/datasync/harness_test.go`

**Interfaces:**
- Produces, in `component/memql/sync`:

```go
type Partitioned interface {
	Partitions(ctx context.Context) ([]string, error)
	BackfillPartition(ctx context.Context, partition, conceptName, cursor string) (BackfillPage, error)
	ReconcilePartition(ctx context.Context, partition, conceptName string, since time.Time) (ReconcileReport, error)
}
```

  and `Partition string` on `MirrorWrite`.
- Produces, in `component/datasync`: `Partition string` on `SyncState`; `func SyncStateID(conceptID, connector, direction, partition string) string`; `func (s *Store) SyncStateFor(ctx context.Context, conceptID, connector, direction, partition string) (SyncState, error)`; `func (s *Store) SyncStates(ctx context.Context, connector string) ([]SyncState, error)` (every page); and on `Runner`: `StartBackfill(ctx, connectorName, conceptID, partition string)`, `Reconcile(ctx, connectorName, conceptID, partition string)`, `ReconcileDue(ctx, connectorName string, spec memqlsync.DomainSpec, partition string) bool`, `SetPaused(ctx, connectorName, conceptID, partition string, paused bool) error`.
- Produces, in the DSL: `syncState.partition string`; the `backfillStatus` value `queued`; `partition` in `syncStateFull`; `upsertSyncState` takes and accepts `partition string`; `query syncState syncStateById { args { stateId string! } }` (cluster owner, `@unbounded`, one row); `syncStatesAll` takes `partition string` and filters on it when given; `datasyncStartBackfill` and `datasyncSetSyncPaused` take `partition string`.

Decisions:
- An empty partition derives the id from the same three-part string as today, so every existing row keeps its id; a partition is appended as a fourth part.
- The runtime reads one state by its id (`syncStateById`), not by its fields: a row written before this version has no `partition` field, and matching an absent field against "" is not a rule to rely on. `syncStateFor` stays in the DSL, unused by the runtime.
- `SyncStates` pages with `memql.ContextWithCursor` and `memql.ResultPageCursor`, as `component/campaigns` does.
- `handleReconcileDomains`: a connector implementing `Partitioned` is swept one partition at a time, through `ReconcilePartition`; a partition's failure is logged and the next partition still runs. Any other connector is swept with partition "". With a partition, `Runner` calls `BackfillPartition` and `ReconcilePartition`, and refuses a connector that does not implement the interface.
- `recordInboundHealth` writes the row of each write's own concept and partition.
- `fakePartitioned` in `harness_test.go` embeds `fakeConnector` and scripts a report, an error and the recorded `since` values for each partition.

- [ ] **Step 1: Write the failing tests**

```go
func TestAnEmptyPartitionKeepsTheRowIdItAlwaysHad(t *testing.T)           // equals the id derived from "concept|connector|direction"
func TestTwoPartitionsOfOneDomainAreTwoRows(t *testing.T)
func TestAPartitionedConnectorIsSweptOnePartitionAtATime(t *testing.T)    // each partition gets its own since
func TestOnePartitionsRefusalLeavesTheOthersSweepClean(t *testing.T)
func TestAPartitionTheConnectorLeavesOutKeepsItsWatermark(t *testing.T)   // Partitions answers only "b": "a"'s row is not written, its lastReconcileAt unchanged
func TestTheRunnerNeverCallsAPartitionedConnectorsSharedSweep(t *testing.T)
func TestInboundHealthLandsOnTheWritesOwnPartition(t *testing.T)
func TestAPauseIsOnePartitionsOwn(t *testing.T)
// partitions_db_test.go, database lane
func TestTwoStoresKeepTheirOwnCursorWatermarkAndError(t *testing.T)       // write both and the unpartitioned row, read each back whole
func TestAConnectorWithNoPartitionsKeepsItsRows(t *testing.T)             // a row written by the old three-part id reads back through SyncStateFor(..., "")
func TestSyncStatesReadsPastTheFirstPage(t *testing.T)                    // 120 rows of one connector come back
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -p 1 -count=1 ./component/memql/sync/ ./component/datasync/`. Expected: FAIL to compile, then FAIL.
- [ ] **Step 3:** Implement the Go and the DSL, then regenerate as the Global Constraints say.
- [ ] **Step 4:** Run the same command and the generated-file gates of the Global Constraints. Expected: PASS. `./integrations/shopify/` must still compile: it does not call the changed functions.
- [ ] **Step 5: Commit** `datasync: sync state is kept for each partition of a connector`.

### Task 4: Each store is a partition (U2)

**Files:**
- Create: `integrations/shopify/partitions.go`, `integrations/shopify/partitions_test.go`, `component/datasync/shopify_partitions_db_test.go`
- Modify: `integrations/shopify/reconcile.go`, `integrations/shopify/backfill.go`, `integrations/shopify/mapper.go`, `integrations/shopify/capabilities.go`

**Interfaces:**
- Consumes: Task 3's `Partitioned`, `MirrorWrite.Partition`, `syncStatesAll(connector, partition)`; Task 2's classification.
- Produces: `func (c *Connector) Partitions(ctx context.Context) ([]string, error)` (ids of the stores that ingest, sorted); `BackfillPartition` (the store's `BackfillStore`; a store that does not ingest is an error naming it); `ReconcilePartition` (Task 2's order for that one store, then `reconcileStore`). `mapObject` and `tombstone` stamp `Partition` with the store id. `handleStoreHealth` gives each store the domains of its own partition, read with every page.

Decisions:
- `Connector.Backfill` and `Connector.Reconcile` stay, as loops over `Partitions`, for the contract and the unit tests that call them. The runner never calls them for this connector.
- `phaseOf` answers `backfilling` for `queued` as for `running`.
- The database test follows `shopify_propagation_db_test.go`: a real engine, the real connector, a fake Shopify transport.

- [ ] **Step 1: Write the failing tests**

```go
func TestEveryMirrorWriteCarriesItsStore(t *testing.T)              // parent, child and tombstone
func TestAStoreThatDoesNotIngestIsNotAPartition(t *testing.T)       // paused, uninstalled
func TestOneStoresRefusalIsItsOwn(t *testing.T)                     // A's root denied: NotGranted for A, a clean report for B
func TestStoreHealthShowsEachStoreItsOwnDomains(t *testing.T)
// component/datasync/shopify_partitions_db_test.go, database lane
func TestTheSecondStoreBackfillsAfterTheFirstIsComplete(t *testing.T)
func TestAPausedStoreIsSweptFromWhereItStopped(t *testing.T)        // pause A, sweep B twice, resume A: A's since is its own last sweep
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -p 1 -count=1 ./integrations/shopify/ ./component/datasync/ -run 'CarriesItsStore|NotAPartition|RefusalIsItsOwn|ItsOwnDomains|SecondStoreBackfills|FromWhereItStopped'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, then both packages whole. Expected: PASS.
- [ ] **Step 5: Commit** `shopify: each store is its own partition of the mirror`.

### Task 5: The watermark is when the sweep started (U5)

**Files:**
- Modify: `component/datasync/runners.go`, `dsl/platform/concepts.memql` (the description of `lastReconcileAt` only)
- Test: `component/datasync/runners_test.go`

**Interfaces:**
- Produces: `const reconcileOverlap = 2 * time.Minute`. A clean sweep writes `LastReconcileAt` as the time it started less `reconcileOverlap`, and `LastAttemptAt` as the time it ended.

- [ ] **Step 1: Write the failing tests**

```go
func TestTheWatermarkIsWhenTheSweepStartedLessTwoMinutes(t *testing.T)   // clock moves ten minutes inside the connector: the next since is start minus 2m
func TestARowChangedDuringASweepIsReadByTheNext(t *testing.T)            // a change stamped start+3m is at or after the next since
func TestTheCadenceStillRunsFromWhenTheSweepEnded(t *testing.T)          // ReconcileDue measures from LastAttemptAt
```

- [ ] **Step 2:** Run `MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -count=1 ./component/datasync/ -run 'Watermark|ChangedDuringASweep|CadenceStillRuns'`. Expected: FAIL.
- [ ] **Step 3:** Implement; correct any existing test that pinned the end time.
- [ ] **Step 4:** Run the package whole, then regenerate (the description changed) and run the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `datasync: a sweep's watermark is when it started, less two minutes`.

### Task 6: A backfill starts and continues by itself (U3)

**Files:**
- Modify: `component/datasync/runners.go`, `component/datasync/capabilities.go`, `component/datasync/store.go`, `dsl/common/builtins.memql`, `dsl/platform/queries.memql`, `dsl/platform/automations.memql`, `integrations/shopify/connect_write.go`, `integrations/shopify/plugin.go` (`Bootstrap`)
- Test: `component/datasync/runners_test.go`, `component/datasync/partitions_db_test.go`, `integrations/shopify/connect_write_test.go`, `integrations/shopify/partitions_test.go`

**Interfaces:**
- Consumes: Task 3's partitioned runner and the `queued` status; Task 4's partitions; Task 2's `domainInConnection` and `scopesMissingFor`.
- Produces, in `component/datasync`: `func (r *Runner) QueueBackfill(ctx context.Context, connectorName, conceptID, partition string) error`; `type ContinueResult struct{ Started, Advanced, Completed int }`; `func (r *Runner) ContinueBackfills(ctx context.Context) (ContinueResult, error)`; capabilities `queueBackfill` and `continueBackfills`.
- Produces, in the DSL: `builtin datasyncQueueBackfill { connector string!, conceptId string!, partition string }` and `builtin datasyncContinueBackfills { }`, both refusing a call without internal origin; `query syncState syncStatesBackfilling { args { connector string! } }` (status `queued` or `running`, oldest first, paginated); `automation continueQueuedBackfills` on `0 */5 * * * *`.
- Produces, in `integrations/shopify`: `func (c *Connector) queueConnectBackfills(ctx context.Context, prior Store, found bool, now Store)`, called by `writeConnect` once the store row is written and before the webhook pass; `func (c *Connector) queueUnstartedBackfills(ctx context.Context)`, called by `Bootstrap` after the subscription pass; the store health key `ordersFrom`.

Decisions:
- `QueueBackfill` writes `queued` and an empty cursor. A `running` backfill is left as it is. A `complete` one is queued again: a reconnect reads the store again, and every write is idempotent on the row id.
- `ContinueBackfills` takes, for each partition of each bound connector, at most one domain a call: the one that is `running`, else the first `queued` in `Domains()` order. It never starts a second domain of a partition while one is `running`, and leaves a paused domain alone. A row with no partition is ignored for a connector that implements `Partitioned`.
- `StartBackfill` stops an invocation, leaving `running`, when a page brought no writes and the cursor it was given back: a poll of an operation still running.
- A clean sweep of a domain whose row carried a not-granted or not-requested `lastError` queues that domain's backfill.
- `queueConnectBackfills` queues every bulk domain in the store's connection, in `generated.ApplyOrder`, when the store is new, was uninstalled or purged, or had no Admin token. Otherwise it queues the bulk domains the prior grant lacked a scope for and the new grant reaches. It queues through `builtin datasyncQueueBackfill` executed on the engine under `operatorContext`, because this integration does not import the runtime. When it queues the order domain, the grant lacks `read_all_orders` and `ordersFrom` is absent, it stamps `ordersFrom` with the day 60 days before now.
- `queueUnstartedBackfills` is how a store connected before this version gets its past (the spec's section 5, "The existing rows"): for each store that ingests, it queues every bulk domain of the connection that has no sync-state row of that store's own. A domain that has its row is left alone, so a second boot and a second node queue nothing more.

- [ ] **Step 1: Write the failing tests**

```go
func TestAQueuedBackfillIsStartedByTheContinuingPass(t *testing.T)
func TestABackfillThatUsedItsPageBudgetIsContinuedToComplete(t *testing.T)     // maxPages 2, five pages: three calls, then complete
func TestOneStoreRunsOneBackfillAtATime(t *testing.T)                          // two queued domains: the second starts only after the first is complete
func TestAPollThatMadeNoProgressYieldsUntilTheNextTick(t *testing.T)           // one connector call in the tick
func TestAPausedDomainIsNotStarted(t *testing.T)
func TestQueueingLeavesARunningBackfillAlone(t *testing.T)
func TestTheFirstCleanSweepOfARefusedDomainQueuesItsBackfill(t *testing.T)
func TestAnUnpartitionedRowOfAPartitionedConnectorIsNotContinued(t *testing.T)
func TestQueueAndContinueRefuseACallWithoutInternalOrigin(t *testing.T)
func TestAQueuedBackfillRunsToCompleteAgainstARealEngine(t *testing.T)         // database lane
// integrations/shopify/connect_write_test.go
func TestAFirstConnectQueuesEveryBulkDomainOfTheConnection(t *testing.T)       // none for blog
func TestAWiderGrantQueuesOnlyWhatItUnlocked(t *testing.T)                     // old list to commerce set: company, companyLocation, customer, discountNode
func TestAReconnectAfterAnUninstallQueuesEverything(t *testing.T)
func TestOrdersFromIsStampedOnceSixtyDaysBack(t *testing.T)                    // a second connect does not move it
// integrations/shopify/partitions_test.go
func TestAStoreConnectedBeforeThisVersionHasItsBackfillsQueuedAtBoot(t *testing.T)
func TestBootQueuesNothingForADomainThatHasItsRow(t *testing.T)
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -p 1 -count=1 ./component/datasync/ ./integrations/shopify/ -run 'Queue|Continu|OneBackfillAtATime|NoProgress|PausedDomainIsNot|RefusedDomainQueues|UnpartitionedRow|WiderGrant|OrdersFrom|AfterAnUninstall|AtBoot'`. Expected: FAIL.
- [ ] **Step 3:** Implement and regenerate.
- [ ] **Step 4:** Run the same command, both packages whole and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `datasync: a backfill is queued by the engine and continued until it is done`.

### Task 7: A failed delivery is tried again (U4)

**Files:**
- Modify: `component/datasync/inbound.go`, `component/datasync/store.go`, `component/datasync/capabilities.go`, `dsl/platform/concepts.memql` (`inboundRequest`), `dsl/platform/shapes.memql`, `dsl/platform/queries.memql`, `dsl/platform/mutations.memql`, `dsl/common/builtins.memql`, `dsl/platform/automations.memql`, `integrations/shopify/apply.go`, `integrations/shopify/connector.go`
- Test: `component/datasync/inbound_test.go`, `component/datasync/partitions_db_test.go`, `integrations/shopify/apply_test.go`

**Interfaces:**
- Produces, in the DSL: `inboundRequest.attempts int` and `inboundRequest.nextAttemptAt datetime`, both in `inboundRequestFull`; `shape inboundRequest inboundRequestBrief` (`row.id`, `source`, `status`, `attempts`, `nextAttemptAt`, `lastError`, `receivedAt`, `row.createdAt`, and no body); `updateInboundRequestStatus` takes and accepts `attempts int` and `nextAttemptAt datetime`; `@serverOnly mutation inboundRequest requeueInboundRequest { args { requestId string!, nextAttemptAt datetime! } }` setting `attempts` to 0; `query inboundRequest inboundRequestsDueForRetry { args { asOf datetime! } }` (cluster owner, `status == "failed"`, `attempts < 4`, `nextAttemptAt <= asOf`, oldest first, `paginate 50`, `inboundRequestFull`, since a retry needs the body); `query inboundRequest inboundRequestsGivenUp { args { source string } }` (cluster owner, `status == "failed"`, `attempts >= 4`, the source when given, newest first, `paginate 50`, `inboundRequestBrief`); `builtin datasyncRetryInbound { }` (internal origin only); `@sdk builtin datasyncRequeueInbound { source string! }`; `automation retryFailedInboundDeliveries` on `0 * * * * *`.
- Produces, in `component/datasync`: `const maxInboundAttempts = 4`; `var inboundRetryAfter = [3]time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}`; `Attempts int` on `StagedRequest`; `type RetryResult struct{ Tried, Recovered int }`; `func (d *Dispatcher) RetryDue(ctx context.Context) (RetryResult, error)`; `func (d *Dispatcher) Requeue(ctx context.Context, source string) (int, error)`; capabilities `retryInbound` and `requeueInbound`.
- Produces, in `integrations/shopify`: `func (a *applied) forget(id string)`.

Decisions:
- `attempts` counts tries made. A failed try stamps `attempts` plus one. It also stamps `nextAttemptAt` as now plus `inboundRetryAfter[attempts-1]` when the failure is worth another try and fewer than four tries were made. A row never worth another try keeps no `nextAttemptAt`, so the due query never answers it.
- Not worth another try: `IsPermanent`, `IsNotGranted`, `IsNotRequested`, `IsNotImplemented`, a delivery another tier verified or nobody verified, staged metadata that does not parse. Everything else is.
- `RetryDue` reads one page and dispatches each row through `DispatchStaged`, so a retry passes the same judgement as a first try.
- `Requeue` sets `attempts` to 0 and `nextAttemptAt` to now on the given-up rows of one source. `datasyncRequeueInbound` admits internal origin or a cluster owner and refuses anybody else in the handler.
- The Shopify connector forgets a webhook id when `Apply` returns an error, so its 15-minute short-circuit records only deliveries it applied. Its refusal of a store pinned to another Admin API version becomes `memqlsync.Permanentf`.

- [ ] **Step 1: Write the failing tests**

```go
func TestAFailureWorthAnotherTryIsStampedWithItsNextTime(t *testing.T)      // attempts 1, next is now plus 1m
func TestTheThreeRetriesComeAtOneFiveAndThirtyMinutes(t *testing.T)
func TestAfterTheFourthTryTheRowStaysFailed(t *testing.T)                   // attempts 4, never due again
func TestARefusalThatWillNotChangeIsNeverRetried(t *testing.T)              // permanent, not granted, not requested, wrong tier, unsigned, unparseable
func TestARetryThatSucceedsStampsProcessed(t *testing.T)
func TestTheRetryPassTakesOneBoundedPage(t *testing.T)
func TestRetryInboundRefusesACallWithoutInternalOrigin(t *testing.T)
func TestRequeueReturnsTheGivenUpRowsOfOneSourceOnly(t *testing.T)
func TestRequeueRefusesACallerWhoIsNotAClusterOwner(t *testing.T)
func TestAFailedDeliveryIsDueOnlyWhenItsTimeHasCome(t *testing.T)           // database lane: a row with no time, one at four tries and one not yet due are not answered
// integrations/shopify/apply_test.go
func TestARetriedDeliveryIsFetchedAgainNotSwallowedAsADuplicate(t *testing.T)   // first fetch fails, the same webhook id fetches again
func TestAStorePinnedToAnotherVersionIsAPermanentRefusal(t *testing.T)
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -p 1 -count=1 ./component/datasync/ ./integrations/shopify/ -run 'AnotherTry|ThreeRetries|FourthTry|WillNotChange|RetryThatSucceeds|BoundedPage|RetryInbound|Requeue|ItsTimeHasCome|FetchedAgain|AnotherVersion'`. Expected: FAIL.
- [ ] **Step 3:** Implement and regenerate.
- [ ] **Step 4:** Run the same command, both packages whole, the generated-file gates and `MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -count=1 ./component/inbound/... ./component/campaigns/ -run 'Inbound'` (other writers of the same mutation). Expected: PASS.
- [ ] **Step 5: Commit** `datasync: a delivery that failed is tried three more times`.

### Task 8: The verdict (U6)

**Files:**
- Create: `integrations/shopify/verdict.go`, `integrations/shopify/verdict_test.go`
- Modify: `integrations/shopify/capabilities.go`, `integrations/shopify/subscriptions.go`, `dsl/shopify/overlay/builtins.memql`

**Interfaces:**
- Consumes: Task 2's coverage helpers; Task 4's per-store states; Task 6's `queued` and `ordersFrom`; Task 7's `inboundRequestsGivenUp` and `datasyncRequeueInbound`.
- Produces:

```go
type VerdictReason struct {
	Code, Domain, Sentence, Detail string
	Count                          int
	Act, ActLabel, ActURL          string // Act: "", "approve", "reconnect", "register", "retry", "declare"
}
type RecordedKind struct{ Kind, LastChangeAt, LastCheckedAt string }
type Verdict struct {
	StoreID, Domain, State string
	Since                  time.Time
	Reasons                []VerdictReason
	Recorded               []RecordedKind
	Outside, Withheld      []string
	OrdersFrom             string
}
type verdictInput struct {
	Store     Store
	States    []map[string]any          // this store's sync-state rows
	Intervals map[string]time.Duration  // concept id to its sweep interval
	GivenUp   []map[string]any          // this store's given-up deliveries
	Denied    map[string][]string       // fieldsDenied
	Previous  map[string]any            // health.verdict as last recorded
	Now       time.Time
}
func computeVerdict(in verdictInput) Verdict
func plainDomain(concept string) string
func (c *Connector) mayActOnStore(ctx context.Context, store Store) bool   // the store's owner, or a cluster owner
```

- Produces, in the DSL, each `@sdk` with `@requiresCapability("execute", "app:deployables/store")`: `builtin shopifyStoreVerdict { storeId string! }`, `builtin shopifyStoreRegisterEvents { storeId string! }`, `builtin shopifyStoreRetryDeliveries { storeId string! }`; capabilities `storeVerdict`, `storeRegisterEvents`, `storeRetryDeliveries`.
- The reply of `shopifyStoreVerdict` is one node with `storeId`, `domain`, `state`, `since`, `reasons` (each `code`, `domain`, `sentence`, `detail`, `count`, `act`, `actLabel`, `actUrl`), `recorded` (each `kind`, `lastChangeAt`, `lastCheckedAt`), `outside`, `withheld`, `ordersFrom`, `alert` (`state`, `reason`; Task 9 fills it) and `may` (`act`).

Decisions:
- State, first match: `uninstalledAt` or `redactedAt` set is `not connected`; status `paused` is `paused`; any reason is `needs attention`; a domain of the connection `queued` or `running`, or a swept domain of it never attempted, is `starting`; else `recording`. A paused or disconnected store carries no reasons.
- `Since` is kept from `Previous` while the state is the same, else `Now`.
- Only swept domains inside the connection are asked. A child domain, a domain outside the connection and a not-requested row are never a reason.
- `data_not_declared`: a domain's `lastError` is not-granted and `protectedDataRefusal`. One reason naming every such domain, with act `declare` and `ActURL` `https://shopify.dev/docs/apps/launch/protected-customer-data`.
- `approval_outdated`: `scopesNotApproved` is not empty. `Detail` lists the scopes; `ActURL` is `https://<store domain>/admin/apps`.
- `grant_lost`: a `lastError` containing "reconnect this store", "could not be renewed" or "HTTP 401".
- `events_not_registered`: `health.subscriptions.failed` is not empty, or the last pass left fewer subscriptions than it wanted (`existing` plus those created, less those removed, is below `desired`).
- `check_failing`: a domain with no clean sweep within twice its interval, whose row was attempted. This stands for "failed its last two sweeps" with no new counter. `Detail` is the `lastError` when there is one, else "No check has run since <time>", and names the time of the next sweep.
- `changes_not_recorded`: given-up deliveries received after the last clean sweep of the domain their `x-shopify-topic` header routes to. `Count` is how many.
- `Withheld` is the denied fields in plain words; a withheld field is never a reason (D12).
- `shopifyStoreRegisterEvents` runs `EnsureSubscriptionsForStore` and records it. `shopifyStoreRetryDeliveries` executes `builtin datasyncRequeueInbound` for the source `shopify-<storeId>` under `operatorContext`. Both refuse a caller for whom `mayActOnStore` is false.
- The daily `EnsureSubscriptions` records a pass it could not read as failed on the store's health, as `registerConnectWebhooks` already does. Today it only logs.

- [ ] **Step 1: Write the failing tests**

```go
func TestAStoreSweptCleanIsRecording(t *testing.T)
func TestABackfillQueuedOrRunningIsStarting(t *testing.T)
func TestAPausedStoreIsPausedAndCarriesNoReasons(t *testing.T)
func TestAnUninstalledOrPurgedStoreIsNotConnected(t *testing.T)
func TestAProtectedDataRefusalIsDataNotDeclared(t *testing.T)                 // the sentence names "orders"
func TestAnOlderGrantIsApprovalOutdatedAndNamesWhatItLacks(t *testing.T)
func TestARenewalThatFailedIsGrantLost(t *testing.T)
func TestAFailedSubscriptionPassIsEventsNotRegistered(t *testing.T)
func TestADomainWithNoCleanSweepInTwoIntervalsIsCheckFailing(t *testing.T)
func TestADeliveryThatUsedItsTriesCountsUntilItsDomainIsSweptClean(t *testing.T)   // "1 change" and "3 changes"
func TestADomainOutsideTheConnectionIsNeverAReason(t *testing.T)              // includes a row refused for an unasked scope
func TestAChildDomainIsNeverAsked(t *testing.T)
func TestADomainIsNamedInPlainWords(t *testing.T)                             // the table, and the camel-case fallback
func TestSinceIsKeptWhileTheStateHolds(t *testing.T)
func TestTheDailyPassRecordsAFailedSubscriptionRead(t *testing.T)
func TestAStoreActIsRefusedForSomebodyWhoIsNeitherItsOwnerNorAClusterOwner(t *testing.T)
```

- [ ] **Step 2:** Run `MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -count=1 ./integrations/shopify/ -run 'IsRecording|IsStarting|PausedStoreIsPaused|IsNotConnected|DataNotDeclared|ApprovalOutdated|GrantLost|EventsNotRegistered|CheckFailing|UsedItsTries|NeverAReason|NeverAsked|PlainWords|SinceIsKept|DailyPassRecords|StoreActIsRefused'`. Expected: FAIL.
- [ ] **Step 3:** Implement and regenerate the SDKs.
- [ ] **Step 4:** Run the same command, the package whole, and `make sdk-gen-check`. Expected: PASS.
- [ ] **Step 5: Commit** `shopify: one verdict for each store, with its reasons and what fixes each`.

### Task 9: The watch and the two emails (U6)

**Files:**
- Create: `integrations/shopify/watch.go`, `integrations/shopify/watch_test.go`
- Modify: `integrations/shopify/capabilities.go`, `dsl/shopify/overlay/builtins.memql`, `dsl/shopify/overlay/automations.memql`

**Interfaces:**
- Consumes: Task 8's `computeVerdict`.
- Produces: `const storeAlertWaitVariable = "SHOPIFY_STORE_ALERT_MINUTES"`; `const defaultStoreAlertWait = 30 * time.Minute`; `func validAlertMinutes(n int) bool` (1 to 1440); `func (c *Connector) storeAlertWait(ctx context.Context) time.Duration`; `func (c *Connector) WatchStores(ctx context.Context) error`; capabilities `watchStores`, `storeAlertWait`, `storeAlertWaitSet`.
- Produces, in the DSL: `builtin shopifyWatchStores { }` (internal origin only); `@sdk builtin shopifyStoreAlertWait { }` with `@requiresCapability("read", "app:settings/connections")`, replying `minutes`, `isDefault` and `maySet`; `@sdk builtin shopifyStoreAlertWaitSet { minutes int! }`, a cluster owner's; `automation watchShopifyStores` on `0 */5 * * * *`.
- Produces, on the store row: `health.verdict` with `state`, `reasons` (codes), `since`, `mailedFor`, `recoveredFor` and `alert` (`state` of `none`, `staged`, `sent` or `failed`, and `reason`).

Decisions:
- `WatchStores` computes every store's verdict and writes `health.verdict` through `recordStoreHealth` only when it changed.
- An episode is a `since`. One alert for an episode: when the state is `needs attention` or `not connected`, has been for the alert wait, and `mailedFor` is not this `since`. `paused` and `starting` never alert. One recovery mail: when the state is `recording`, an alert was mailed (`mailedFor` is set) and `recoveredFor` is not that same episode. A store that passes through `starting` on its way back is still told.
- Both go through `stageOutboundRequest`, medium `email`, to the address of the store's owner (`identity.Store.LookupUserById` on `ownerUserId`), `requestedBy` `shopify:storeWatch`, with `requestId` and `dedupeKey` `shopify-store-alert-<storeId>-<since as 20060102T150405Z>` and `shopify-store-recovered-<storeId>-<the mailed episode's stamp>`.
- Exact copy. Alert subject: `<store domain> is not being recorded`. Alert body: "MemQL has not been recording <store domain> since <since, RFC 1123, UTC>.", a blank line, each reason's sentence on its own line, a blank line, "Open the storefront's Store page in MemQL OS to see what fixes it: <the OS address>/", a blank line, "This email carries no order and nothing about a customer." Recovery subject: `<store domain> is being recorded again`. Recovery body: "MemQL is recording <store domain> again. It was not recording from <since> to <now>."
- A store with no owner, or an owner with no address, sets `alert` to `failed` with "This store names no owner to tell." On a later pass the staged request is read back by `outboundRequestById`: delivered sets `sent`; failed sets `failed` with the outbox's own reason, which is how an address off the allowlist is named.
- `storeAlertWait` answers 30 minutes for an absent, unreadable or out-of-range value. `shopifyStoreAlertWaitSet` refuses anything outside 1 to 1440 and anybody but a cluster owner, and writes through `setGlobalVariable`.

- [ ] **Step 1: Write the failing tests**

```go
func TestTheAlertIsStagedOnceAfterTheWait(t *testing.T)
func TestNothingIsStagedBeforeTheWait(t *testing.T)
func TestAStoreThatRecoversBeforeTheWaitIsNeverMailed(t *testing.T)
func TestTheSameEpisodeIsNeverMailedTwice(t *testing.T)
func TestRecoveryIsMailedOnceAndOnlyAfterAnAlert(t *testing.T)
func TestRecoveryStillComesWhenTheStorePassesThroughStarting(t *testing.T)
func TestAPausedOrStartingStoreIsNeverMailed(t *testing.T)
func TestTheWaitIsThirtyMinutesWhenNothingIsSet(t *testing.T)
func TestAWaitOfFiveMinutesIsHonoured(t *testing.T)
func TestZeroAndTwentyFiveHoursAreRefusedWhenSaved(t *testing.T)
func TestOnlyAClusterOwnerSetsTheWait(t *testing.T)
func TestAnAlertTheOutboxRefusedIsNamedOnTheVerdict(t *testing.T)
func TestAStoreWithNoOwnerSaysNobodyWasTold(t *testing.T)
func TestTheAlertCarriesNoOrderAndNoCustomer(t *testing.T)        // subject and body are exactly the copy above
func TestWatchStoresRefusesACallWithoutInternalOrigin(t *testing.T)
func TestAnUnchangedVerdictIsNotWrittenAgain(t *testing.T)
```

- [ ] **Step 2:** Run `MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -count=1 ./integrations/shopify/ -run 'Alert|Wait|Mailed|Recovery|TwentyFiveHours|NoOwner|WatchStores|UnchangedVerdict'`. Expected: FAIL.
- [ ] **Step 3:** Implement and regenerate (builtins and an automation changed).
- [ ] **Step 4:** Run the same command, the package whole and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `shopify: tell a store's owner when it stops being recorded, and when it is back`.

### Task 10: The Store slot and the Store page (U7)

**Files:**
- Create: `clients/os/src/apps/deployables/store/verdict.ts`, `clients/os/test/deployables/verdict.test.ts`, `clients/os/test/deployables/storeVerdict.test.tsx`
- Modify: `clients/os/src/apps/deployables/store/useStore.ts`, `clients/os/src/apps/deployables/store/ShopifyStorePanel.tsx`, `clients/os/src/apps/deployables/page/DeployableWorkspace.tsx` (`StorePiece`), `clients/os/src/modules/connections/ShopifyConnections.tsx`, `clients/os/test/deployables/harness.tsx`, `clients/os/test/deployables/shopifyConnect.test.tsx`

**Interfaces:**
- Consumes: the reply of `shopifyStoreVerdict`, `shopifyStoreRegisterEvents`, `shopifyStoreRetryDeliveries`, `shopifyStoreAlertWait`, `shopifyStoreAlertWaitSet`, and `setStoreStatus`, which exists.
- Produces, in `verdict.ts`: `type VerdictState = "recording" | "starting" | "paused" | "needs attention" | "not connected"`; `interface VerdictReason`, `interface RecordedKind` and `interface StoreVerdict` mirroring the reply; `function readStoreVerdict(rows: readonly Row[]): StoreVerdict | null` (pure; an absent time stays absent, never a zero); `function verdictWord(state: VerdictState): string` ("" for `not connected`).
- Produces, in `useStore.ts`: `function useStoreVerdict(storeId: string): Reading<StoreVerdict | null> & { verdict: StoreVerdict | null }`; `registerEvents(storeId: string)` and `retryDeliveries(storeId: string)` on `StoreWrites`.
- Produces, in `harness.tsx`: `storeVerdict?: Record<string, Row>` and `storeAlertWait?: Row` on `FakeSeed`, answered for the three reads and recorded for the writes.

Decisions:
- `storeConnection` still speaks first: uninstalled, purged, a misnamed token and missing setup keep their words and notices. Where it answers `connected`, the verdict's word replaces "Connected", on the slot and on each connection row. While the verdict is read, an `InlineSkeleton`; when it cannot be read, "Status unknown" and a `Notice` with "Try again".
- A store that needs attention shows each reason as a `Notice` (tone `warn`) carrying its sentence, its `detail` as a caption and its one act: a link for `approve` and `declare` (to `actUrl`, opened in a new tab), "Reconnect in Settings" for `reconnect`, a button for `register` and `retry`. One store bound to both destinations shows a reason once.
- Acts are drawn only when `may.act` is true: "Pause recording" when the state is not `paused` and "Resume" when it is, through the existing `setStatus` with `paused` and `live`; never both, never disabled.
- "What is recorded" is a `Panel` under the connections: one line for each `recorded` kind with "Last change" and "Last checked" as `Facts`; "Orders since <date>" when `ordersFrom` is present; "Shopify withholds: ..." when `withheld` is not empty; and, when `outside` is not empty, one closed line "Not part of this connection" that opens to the list, drawn with a native `details` element as `kit/CostEstimate.tsx` does. No kit piece is added.
- When `alert.state` is `failed`, a `Notice` says "The alert could not be sent." with the reason.
- Settings > Shopify gains one panel, drawn when `maySet` is true: "Email a store's owner after [minutes] minutes without recording", with Save. A refusal is shown in the engine's words.

- [ ] **Step 1: Write the failing tests**

```ts
// verdict.test.ts
it("keeps an absent time absent")
it("answers null for a reply that names no state")
it("has no word for a store that is not connected")
// storeVerdict.test.tsx
it("draws Recording where the page said Connected, on the slot and on the row")
it("draws Starting while a backfill runs")
it("draws Paused and offers Resume, and only Resume")
it("draws each reason once for a store bound to both destinations")
it("gives approval_outdated a link to the store's Shopify admin")
it("gives register and retry a button, and runs the builtin once")
it("draws no act for somebody who may not act on the store")
it("lists what is recorded, with orders since")
it("keeps what is outside the connection behind one closed line, never as an error")
it("names a field Shopify withholds without calling it an error")
it("says the alert could not be sent, and why")
it("keeps Uninstalled and its Reconnect notice")
it("draws nothing of the store for somebody without the store part")
// shopifyConnect.test.tsx
it("shows the alert wait to a cluster owner and saves five")
it("draws no alert wait for somebody who may not set it")
it("shows the engine's refusal for a wait of zero")
```

- [ ] **Step 2:** In `clients/os` run `npx vitest run test/deployables/verdict.test.ts test/deployables/storeVerdict.test.tsx test/deployables/shopifyConnect.test.tsx`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, then `npm run typecheck` and `npx vitest run test/deployables test/settings`. Expected: PASS. Check the Store page for each state in both themes and at both widths.
- [ ] **Step 5: Commit** `os: the Store page says whether a store is being recorded`.

### Task 11: Cluster > Data, by store (U7)

**Files:**
- Modify: `clients/os/src/apps/cluster/origins/rows.ts`, `clients/os/src/apps/cluster/origins/OriginsSection.tsx`
- Test: `clients/os/test/cluster/origins.test.tsx`, `clients/os/test/cluster/harness.tsx`

**Interfaces:**
- Consumes: `syncStatesAll` rows carrying `partition`; `inboundRequestsGivenUp`; `datasyncRequeueInbound`; `datasyncStartBackfill` and `datasyncSetSyncPaused` with `partition`.
- Produces, in `rows.ts`: `partition: string` and `notRequested: boolean` on `OriginRow`; the row key gains the partition; `function groupByPartition(rows: readonly OriginRow[]): { partition: string; rows: OriginRow[]; outside: OriginRow[] }[]`; `function givenUpBySource(rows: readonly Row[]): { source: string; count: number; lastError: string }[]`.

Decisions:
- Health is read to its last page, with the cursor each page hands back.
- A row is `notRequested` when its `lastError` starts with `connector: not requested`. Such rows sit in the group's `outside`, drawn as one closed line "Not part of this connection", and are never counted in a connector's `withError`.
- Where a connector has any row with a partition, its rows with none are history and are not drawn.
- Each store's group is headed by its partition in plain text. Backfill, Pause and Resume send the row's partition.
- Deliveries are a store's, not a domain's, so "Try again" sits beside the existing dead letters, in a section read on demand: "Changes that could not be recorded", one line for each source with its count and last reason. The act runs `datasyncRequeueInbound` for that source and reads the section again.

- [ ] **Step 1: Write the failing tests**

```ts
it("groups a connector's domains by store")
it("reads health past the first page")
it("keeps what a connection never asked for behind one closed line, and out of the error count")
it("does not draw a shared row of a connector that now keeps its stores apart")
it("sends the store with Backfill, Pause and Resume")
it("does not read given-up deliveries until somebody asks")
it("lists given-up deliveries by source and tries one source again")
```

- [ ] **Step 2:** In `clients/os` run `npx vitest run test/cluster/origins.test.tsx`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, `npm run typecheck` and `npx vitest run test/cluster`. Expected: PASS. Check the page in both themes and at both widths.
- [ ] **Step 5: Commit** `os: Cluster > Data shows each store's domains, and what its connection never asked for`.

### Task 12: An order's lines past the first hundred (U8, F9)

**Files:**
- Test first: `integrations/shopify/children_test.go` (new)
- If the test fails: `cmd/shopifyschema/emit_graphql.go`, `cmd/shopifyschema/emit_go.go`, `cmd/shopifyschema/mapping.go`, `integrations/shopify/children.go` (new), `integrations/shopify/apply.go`, `integrations/shopify/reconcile.go`, then regenerate `integrations/shopify/generated/*` with `go run ./cmd/shopifyschema`

**Interfaces:**
- Produces, only if needed: `PageOp string` on the generated `ChildSpec`; `func (c *Connector) completeChildren(ctx context.Context, store Store, spec *generated.TypeSpec, obj map[string]any) error`.

- [ ] **Step 1: Write the test**

```go
func TestAnOrderWithMoreThanAHundredLinesKeepsThemAll(t *testing.T)
// the fetch answers 100 line items and says more follow; the mirror writes for the order carry 101 orderLineItem rows
```

- [ ] **Step 2:** Run `MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -count=1 ./integrations/shopify/ -run 'MoreThanAHundredLines'`. If it PASSES, record that in the commit, strike F9 from the spec's section 2 and stop.
- [ ] **Step 3, only if it failed:** The generator selects `pageInfo { hasNextPage endCursor }` on every paged child connection of a fetch document and emits, for each, an operation that reads one further page of that connection by the parent's id and an `after` cursor, named on `ChildSpec.PageOp`. Bulk documents are unchanged. `completeChildren` follows `hasNextPage` for each child, at most 50 pages, and appends the nodes to the object before it is mapped. `fetchAndMap`, `reconcileByUpdatedAt` and `relist` call it. Add `TestAChildConnectionStopsAtItsPageCeiling` and `TestABulkDocumentCarriesNoPageInfo`.
- [ ] **Step 4:** Run `MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -count=1 ./cmd/shopifyschema/ ./integrations/shopify/` (`TestGeneratedTreeIsNotStale` included). Expected: PASS.
- [ ] **Step 5: Commit** `shopify: an order keeps every line, past the first hundred`, or `shopify: an order's lines past a hundred are already kept; a test now says so`.

### Task 13: The connector reading the rows it wrote (U8, F10)

**Files:**
- Test first: `integrations/shopify/connector_reads_db_test.go` (new, database lane)
- If the test fails: `integrations/shopify/reconcile.go` (`liveRowsAbsentFrom`), `integrations/shopify/analytics.go` (`walk`), `integrations/shopify/capabilities.go` (the product fetch)

- [ ] **Step 1: Write the test**, with `authorityEngine` and `seedStore` from `store_authority_db_test.go`:

```go
func TestTheConnectorReadsBackTheMirrorRowsItWrote(t *testing.T)
// write one product through the mirror write path, then read spec.ForStoreFn for the store under connectorContext: one row
func TestAFullRelistTombstonesWhatTheOriginNoLongerHas(t *testing.T)
// two mirrored gift cards, the origin lists one: the other is tombstoned, against a real engine
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -p 1 -count=1 ./integrations/shopify/ -run 'ReadsBackTheMirrorRows|TombstonesWhatTheOrigin'`. If both PASS, record that in the commit, strike F10 from the spec and stop.
- [ ] **Step 3, only if one failed:** Move the three reads to `operatorContext`, as the store list, the privacy queue and the sync health already were (`TestTheConnectorsOwnConceptsAreReadableAndWritable` states why), and add those reads to that test's table.
- [ ] **Step 4:** Run the package whole in the database lane. Expected: PASS.
- [ ] **Step 5: Commit** `shopify: the connector's own reads of the mirror answer the rows it wrote`.

### Task 14: Documentation

**Files:**
- Modify: `docs/public/operate/shopify-connector.md`, `docs/public/operate/deployables.md`

Content, each under the page's existing headings where one fits:
- `shopify-connector.md`: what the managed connection asks for and why, including that `write_customers` is used only for the wholesale tag and for creating a customer. "The data declaration", a numbered runbook: in the app's settings at Shopify, request protected customer data with the name, email and address fields and not phone; a development store needs no review; a merchant's live store needs Shopify's review, so the request goes in early. Registering the four new scopes on the app, and approving again from the store's Shopify admin after the list grows. "The storefront's Store panel" rewritten as the verdict: the five states, each reason with its sentence and its act. "What the numbers mean" gains `not requested`, `domainsOutside` and `ordersFrom`, and says that `scopesNeeded`, `scopesMissing` and `domainsNotGranted` are now measured against the store's own connection. A store's history begins 60 days before it connects. The delivery retry (four tries; 1, 5 and 30 minutes) and "Try again". The email to the store's owner, the alert wait and its global variable. "Pausing a store" names the Store page's acts. In "Unverified": whether an order's fulfilments are readable under `read_orders` alone, until Task 15 answers it.
- `deployables.md`, "The Shopify storefront": the Store slot's words and the Store page's three parts.

- [ ] **Step 1:** Run `MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -count=1 -run 'TestDocs|TestRetiredVocabulary|TestPluginMeansPackVocabulary|TestMakeTargetCitationsNameRealTargets|TestEngineIsProductNeutral' .` before the edit. Expected: PASS.
- [ ] **Step 2:** Write the pages.
- [ ] **Step 3:** Run the same command and `MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -count=1 ./integrations/shopify/ -run 'ConnectDoc'`. Expected: PASS.
- [ ] **Step 4: Commit** `docs: what a managed store connection records, and what its Store page says`.

### Task 15: A live test against a development store

**Files:**
- Create: `integrations/shopify/mirror_live_test.go`

**Interfaces:**
- Consumes: `NewStoreRegistry`, `NewAdminClient`, `NewConnector`, as `newHarness` wires them, with the real Admin endpoint.
- Reads four test-only variables, registered in no manifest, as `integrations/openai/asr_live_test.go` reads its own: `MEMQL_TEST_SHOPIFY_STORE_DOMAIN`, `MEMQL_TEST_SHOPIFY_ADMIN_TOKEN`, and for the two-store case `MEMQL_TEST_SHOPIFY_STORE_DOMAIN_2`, `MEMQL_TEST_SHOPIFY_ADMIN_TOKEN_2`. Without the first two every test in the file skips, naming the variable.

Decisions:
- The test creates what it reads, through Admin GraphQL with the test's own token, and deletes it afterwards: a draft order completed as paid, then edited, one line refunded, then cancelled. It never touches an order it did not create.
- It needs a token whose app holds the commerce set and has made the data declaration; a refusal for either is reported as that, not as a failure of the mirror. For its own setup the token also needs `write_draft_orders` and `write_orders`, which are the test's and not the connection's.

- [ ] **Step 1: Write the tests**

```go
func TestLiveTheGrantHoldsTheCommerceSet(t *testing.T)                 // scopesNotApproved is empty
func TestLiveACreatedOrderIsMirroredWithLinesPaymentAndBuyer(t *testing.T)
func TestLiveAnEditARefundAndACancellationFollow(t *testing.T)
func TestLiveAnOrdersFulfilmentsAreReadUnderTheCommerceSet(t *testing.T)   // reports, for Task 14's Unverified entry
func TestLiveASecondStoreKeepsItsOwnState(t *testing.T)                // skips without the second pair
```

- [ ] **Step 2:** Run `MEMQL_DATABASE_DSN="$MIRROR_DSN" go test -count=1 ./integrations/shopify/ -run 'TestLive' -v`. Expected without credentials: SKIP, each naming its variable. With a development store's credentials: PASS.
- [ ] **Step 3:** Update the Unverified entry in `shopify-connector.md` with what the fulfilment test found.
- [ ] **Step 4: Commit** `shopify: a live test of the mirror against a development store`.

### Task 16: The whole tree

**Files:** none changed, unless a gate names one.

- [ ] **Step 1:** `docker exec memql-testdb createdb -U memql memql_mirror_final`.
- [ ] **Step 2:** `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN='postgres://memql:memql_dev@127.0.0.1:55432/memql_mirror_final?sslmode=disable' make test`. Expected: PASS. The two known flakes on a busy machine are `integrations/email` `TestAzureFeedbackDiscoversPreviouslySavedOrganization` and `component/packages` `TestSynchronousConfirmationSurvivesItsCallerAndTheSweep`; run either again alone before calling it a failure.
- [ ] **Step 3:** For each tree `scripts/ci/db-gated-packages.sh --trees` names that this plan touched (`component/datasync`, `integrations/shopify`, `component/memql`), run it in the database lane with `-p 1` on the same fresh database. Expected: PASS.
- [ ] **Step 4:** `make sdk-gen-check`, `make concept-snapshot-check`, `make platform-graph-check`. Expected: PASS.
- [ ] **Step 5:** In `clients/os`: `npm run typecheck`, `npm test`, `npm run build`. Expected: PASS.
- [ ] **Step 6:** Commit anything a gate made you regenerate, as `build: regenerate after the store mirror change`.
