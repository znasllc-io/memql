# Store statistics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Store statistics page on a Shopify storefront deployable shows the store's own sales figures for a chosen period, computed with exact arithmetic from fact tables the engine fills from the orders, lines and refunds it mirrors, for the people who hold a new sales part.

**Architecture:** The mirror gains each refund's detail and each store's shop row. A new component, `component/storestats`, walks every store's mirrored orders, lines and refunds through server-only queries each minute and writes facts to its own tables; an hourly check walks them again and repairs what differs. One client-callable builtin, gated by the part `app:deployables/sales` and by the site's binding, answers the figures with SQL over those tables. MemQL OS draws the page from the statistics kit, which gains a money formatter, time-zone labels and a value axis that reaches below zero.

**Tech Stack:** Go (`cmd/shopifyschema`, `integrations/shopify`, `core/statsperiod`, `core/money`, `component/storestats`, `component/auth`, `component/memql`), SQL migrations, MemQL DSL (`dsl/shopify/overlay`, `dsl/rbac`, `packs/wholesalepack/dsl`), React and TypeScript (`clients/os`), Vitest.

**Spec:** `docs/superpowers/specs/2026-11-11-store-statistics-design.md`, called "the record" below. Executors read both. The record's production walk (11.3), its live checks other than L11, and its rollout (13) are operator steps on the owner's instance and are not tasks here. The record's live server test (11.1, an order and its refund reaching the facts within two minutes) is held in two halves: Task 2's live tests hold what Shopify answers for a refund, and steps P4 and P5 of the production walk hold the whole chain.

## Amendments to the record

Made in the same commit as this plan. A1 to A4 are what changed on the mirror's branch after the record was written. A5 is what the record left to the plan. A6 to A13 are where the code said otherwise than the record. No decision of the owner's (S1 to S14) changed.

| # | Where in the record | What it says now |
| --- | --- | --- |
| A1 | F8, F22, 6.1, section 10 ("An order with more lines than the mirror fetches", "The verdict cannot be read") | The mirror reads an order's lines past the first hundred (`integrations/shopify/children.go`) and the verdict builtin exists (`integrations/shopify/verdict_capabilities.go`). The `lines_complete` guard stays. An inlined list on a refund is read one page deep and is not paged. The row about a verdict "not yet available" is now about a verdict the walk could not read. |
| A2 | 6.2, 6.3, 6.5, section 8, section 10 (new row "An order placed more than sixty days ago") | The app does not hold `read_all_orders`: a refund or edit of an order placed more than 60 days ago is not recorded. The answer carries `olderOrderChangesNotRecorded`, kept as `all_orders_readable` on `store_sales_store` from the store row's `scopesGranted`; the page says "Refunds and edits of orders placed more than 60 days ago are not recorded." |
| A3 | 6.2, 6.3, section 9 (U7, U8), section 10 ("Several engine nodes", new row "A tombstoned order") | A fact's version is its mirror row's own write time, not the row's `updatedAt`; a tombstone is recognised by `deleted` alone; an order's tombstone does not retire its lines or refunds, whose facts stay and count nowhere. |
| A4 | Section 10 (new row "A paused store") | A paused store keeps the deliveries that arrive while it is paused and applies them on resume. |
| A5 | Section 14: O3, O4, O6 | O6 settled: the component asks `PackDomainDisabled` in package `dsl`. O3 and O4 answered from the code, each still held by its test. |
| A6 | F12, D22, 6.1, section 10 ("A refund whose detail is not fetched") | A list a document did not select is written empty by the mapper, not left absent, and only a store's first sweep reads every order. So an inlined connection is a field kind of its own that the mapper leaves out when it was not read, and a store's first order sweep under this version reads from the start. A refund of an order Shopify no longer shows never gets its detail. |
| A7 | F13, section 3, 6.1, section 10 (new row "A store whose shop never changed") | The shop row is written only when a `SHOP_UPDATE` delivery arrives. The mirror now reads it at connect, at boot when missing, and daily. |
| A8 | 6.2 | New columns: `checked_at` on the three walked fact tables; `store_domain`, `store_name`, `taxes_included` and `all_orders_readable` on `store_sales_store`; `window_seconds` and `walked_through` on `store_sales_watermark`, which has no `entitlement` source. `verdict_since` is when the store left `recording`. `tax_refunded` is stored as Shopify wrote it. The check's cursors are instants. Derived columns are recomputed in the page's own transaction, a line's in Go with U3. |
| A9 | 6.3, 6.4 | Every walked shape projects `row.id` and `row.createdAt`; three shapes are added for the buyers, the shop and the store. The check reads the three `...Changed` queries from the start: the version shape and the three `...Versions` queries are gone. The wholesale reads take every form of a store id, the settings read is a query of its own, and `wholesaleStatisticsChanged` is gone. A binding's store id is compared bare. |
| A10 | 6.5, section 8, section 9 (U12), section 10 ("A refund with more than 50 lines") | The answer gains `storeName`, `storeDomain` and `incompleteRefunds`; a refund holding a full page of lines makes Units sold absent, not a floor. The chart draws a value below zero. Money is written in `en-US`, the locale `formatCount` writes in. Buckets are cut from the bounds Go computes, not with `AT TIME ZONE`. New copy: "The store's time zone has not been recorded yet.", "The Store page of this deployable says what fixes it.", "1 refund has more lines than were recorded, so units are not exact."; the freshness example reads as `formatFreshness` writes it. |
| A11 | 6.2, section 9 (U7), section 10 (new row "A long history") | The past is read in bounded windows, each as of its end, halved when a page outlasts a read. |
| A12 | D20 | The OS words a part by the last segment of its resource ("sales"); "sales figures" is the seeds' description. |
| A13 | Approvals, section 9 | A paragraph naming what was amended, a note that this plan splits and reorders the units, and the unit rows U1, U2, U4, U5, U6, U7, U8, U9 and U12 brought in line. |

## How the tasks follow the record's units

| Record | Tasks | Why it departs, where it does |
| --- | --- | --- |
| U1 | 2, 3, 4 | Three reviewable changes to the mirror: the refund's detail, the detail of refunds mirrored before it, and the shop row the figures cannot be cut without. |
| U2, U3, U4, U5 | 5, 6, 7, 8 | As the record. U4's test stands beside the other migration round trips, so it shares no package with the component. |
| U6 | 9, and the builtins in 11, 14, 15 | Each builtin, automation and principal is declared in the task that gives it a body, so no task leaves a builtin whose handler does nothing. |
| U7 | 10, 11, 12, 13 | The pure rules that turn a row into a fact, the walk that reads and writes them, what the walk keeps about each store, and the wholesale judgement each carry their own test cycle; a reviewer can reject one and accept its neighbour. |
| U8, U9, U10 | 14, 15, 16 | As the record. |
| U11 | 15 | `make sdk-gen-check` fails the moment an `@sdk` builtin exists without its builders, so they land in one commit. |
| U12, U13, U14 | 17, 18, 19 | As the record. |

## Global Constraints

- Work only in `/Users/znas/projects/memql/.claude/worktrees/store-statistics`, branch `feat/store-statistics`, by absolute path. Never touch `/Users/znas/projects/memql` itself or another worktree. Task 1 rebases once; after it, never rebase, stash, reset, switch branch or push.
- Every task's tests fail first (record the command and the failing output), then pass.
- The mirror's code is cited as it stood at commit 94f55880a of its branch, and that branch moved on before it landed. Where a function this plan names differs on main, main is right: do what the task intends against the code as it stands, and report the difference.
- Every `go test` carries `MEMQL_DATABASE_DSN="postgres://memql:memql_dev@127.0.0.1:55432/memql_stats_a?sslmode=disable"`, written `$STATS_DSN` below (Task 1 exports it). Database-lane tests add `MEMQL_REQUIRE_DB=1` and `-p 1`. Never port 5432. Tests under `./app/` need `-tags bff`. Name packages; a bare `go test ./...` at the root misses the engine and is not a verification. The whole tree is `make test`.
- Generated files are regenerated, never hand-edited: `sdk/go/client/generated_*`, `sdk/ts/src/client/generated_*`, `integrations/shopify/generated/*`, `dsl/shopify/generated/*`, the concept snapshot, the platform graph, the automation corpus. After any `.memql` change: `make sdk-gen`; `make concept-snapshot` when a concept gained a field; `make platform-graph` when a concept or an automation changed. Then `make sdk-gen-check`, `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 -run 'TestConceptFieldSnapshotIsNotStale|TestPlatformGraph' .`, `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 ./test/dslconformance/...` and `make dsl-lint`. If a gate names another stale file, run the make target or the `-update` its message names.
- The docs gates: `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 -run 'TestDocs|TestRetiredVocabulary|TestPluginMeansPackVocabulary|TestMakeTargetCitationsNameRealTargets|TestEngineIsProductNeutral|TestOnlyAllowlistedPackagesStampInternalOrigin' .`
- A tracked Markdown file must not name a registration function in its qualified form (a package or receiver name, a dot, then a name that begins with Register) unless Go declares that name: a docs gate fails on it. Write a new registration in prose. A backtick-quoted make command must name a real target.
- The repository is public. No client, person, real store domain, instance hostname or a client's trade in code, tests, fixtures, docs or commits. Write "a development store" and "the owner's instance". Where a `myshopify.com` domain is needed, use the tree's `acme-widgets.myshopify.com`; everything else a fixture invents is a bicycle shop's (a city bike, an inner tube, a bell), and a buyer is `buyer-1@example.test`.
- The engine has exactly three extension words: component, integration, pack. Never "plugin" in prose. No emojis.
- Engine facts, established on the other branches:
  - A builtin with no gate of its own can be called by any signed-in user. `@sdk` only marks it for the generated clients, and there is no default rank floor. Every builtin here checks its caller in Go before it reads anything.
  - Internal origin escapes the row-authorization write guard, `@serverOnly` and `@requiresCapability`. It is stamped in one place in `component/storestats` and only for the two scheduled passes.
  - An automation's system actor is a reader. A pass that must read every store runs under its own maintenance principal.
  - A raw insert read-merges: a field a write omits keeps its stored value.
  - `createdAt` comes back from the engine as a time value, not a string. Read it through `instantOf`.
  - Scheduled automations can overlap across nodes (`@mode(single)` is process-local). Every pass is safe to run twice and at once.
  - A schema refuses unknown stored keys on a row's next write, so a release that adds a concept field is roll-forward only.
- Money is exact. An amount is a `money.Amount` in Go and `NUMERIC` in SQL, passed as text and cast with `::numeric`; never a `float64`. Shop money only: `presentmentMoney` is never read.
- Every id in a `store_sales_*` table is bare. A store id is compared through `memql.BareShortId`; a wholesale row may hold either `<id>` or `v1:shopify:store:<id>`, and a read that asks one form misses rows.
- Facts hold no email, name, address or phone. An order's `email` is read in memory for the wholesale judgement and written nowhere.
- Exact values: the part `app:deployables/sales`; the provider name `storeStats`; the principals `system:maintenance:storeStatisticsRollupSweep` and `system:maintenance:storeStatisticsCheckSweep`; schedules (six-field cron) walk `15 * * * * *`, check `0 40 * * * *`; a walk page is 250 rows, at most 40 pages of a source for a store in a run, read from its watermark less 5 minutes; a refund's inlined pages are 50, 10 and 10; a top list carries 25 rows; `storeStatsDefinition` is 1.
- The read's one refusal sentence, for a site that does not read, is not a storefront, is system-owned or does not serve the store: "This deployable does not serve that store for you."
- Go renders a named call's string arguments with `langparser.QuoteString`, never `%q`. A number read from a row is narrowed through `core/num`.
- No model or agent is called on any of these paths, and nothing is sent to anyone.
- OS UI follows `clients/os/DESIGN.md`: kit components only, no dividers between rows, no pill, badge or chip in a row, `Notice` and never a toast, a control whose part is not held is absent and not disabled, an unmeasured value is a dash with its reason and never a zero. Each screen is checked in both themes and at both widths.
- Commits are `<area>: <sentence>`, staged by explicit path (never `git add -A`), ending with `Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>`.

## Review Focus

- A connected store whose shop settings never changed since it connected has no mirrored shop row, so no currency and no time zone: every figure would be a dash for good. A person expects figures once the store is connected. Tests in Task 4 and Task 15.
- A day on which more was refunded than sold, or a refund in a period with no order: net sales, total sales and units are below zero, and the kit's chart scales from zero up. A person expects the day drawn below the zero line and the average shown as a dash. Tests in Task 17 and Task 15.
- A refund the mirror wrote without its detail (by a bulk line, or before this version): its split into items, shipping and tax is unknown, and a bulk line must never blank detail a fetch recorded. A person expects no wrong Refunds figure and a notice that clears by itself. Tests in Task 2, Task 3 and Task 11.
- A store in a zone whose clocks change, above all one whose clocks skip midnight (Santiago): Go's own date constructor answers the evening before for such a day. A person expects each sale on its own local day, once. Tests in Task 5 and Task 15.
- One store id written two ways, bare in one row and `v1:shopify:store:<id>` in another: in a site's binding and in the wholesale pack's rows. A person expects the page to open and a wholesale buyer to be found whichever form each row holds. Tests in Task 13 and Task 15.

A task's **Independent of** line names the tasks it shares no file and no Go package with and whose code it does not need, in either direction. Two such tasks can be done in either order. Doing two at once is the controller's call: they share one working tree and one test database.

---

### Task 1: Workspace and baseline

**Files:** none changed.

- [ ] **Step 1:** Confirm the store mirror has landed: `git -C /Users/znas/projects/memql/.claude/worktrees/store-statistics fetch origin main`, then `git cat-file -e origin/main:integrations/shopify/verdict_capabilities.go` and `git cat-file -e origin/main:component/memql/sync/version.go`. If either is missing, stop: this plan builds on that branch and the controller decides.
- [ ] **Step 2:** `git rebase origin/main` on `feat/store-statistics`. The branch carries only documents, so a conflict is a document's; resolve it by keeping both texts and stop if anything else conflicts.
- [ ] **Step 3:** Record three facts for later tasks. The newest file in `component/database/memory-nodes/migrations/` and the newest dated `.go` migration in `component/database/`: if either is dated after `20261112090000`, Task 7 names its files with the first free minute after the newest. Whether `packs/wholesalepack/dsl/concepts.memql` declares `accountId` on `entitlement` (the wholesale branch has landed): either way the fields this plan reads (`entitlement.storeId`, `buyerEmail`, `state`, and `wholesaleSettings.storeId`) are there. And whether main carries the mirror's rule that an update notice whose fetch answers nothing does not retire an order when the store's grant lacks `read_all_orders`: search `integrations/shopify/apply.go` for `read_all_orders`. The copy of the mirror this plan was written against (commit 94f55880a) retires such an order after two empty answers and has no such test. If main has none either, tell the controller before Task 11: an order older than 60 days that Shopify sends a notice for would then be retired, its fact deleted and its sale gone from every figure, which is more than the page's sixty-day line says.
- [ ] **Step 4:** Copy the git-ignored identity bundle: `cp -R /Users/znas/projects/memql/component/identity/web/static/. /Users/znas/projects/memql/.claude/worktrees/store-statistics/component/identity/web/static/`. In `sdk/ts` run `npm ci && npm run build`; in `clients/os` run `npm ci`.
- [ ] **Step 5:** `docker exec memql-testdb createdb -U memql memql_stats_a`, then `export STATS_DSN='postgres://memql:memql_dev@127.0.0.1:55432/memql_stats_a?sslmode=disable'`.
- [ ] **Step 6:** Baseline, all expected PASS: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./cmd/shopifyschema/ ./integrations/shopify/ ./component/datasync/ ./component/emailstats/ ./core/statsperiod/ ./packs/wholesalepack/ ./component/database/memory-nodes/`; the generated-file gates and the docs gates of the Global Constraints; in `clients/os`, `npm run typecheck` and `npx vitest run test/deployables test/kit`. Record any failure that exists before a change and stop: a dirty baseline is the controller's call.

### Task 2: Refund detail in the mirror (U1)

**Independent of:** Tasks 5, 6, 7, 8, 10, 17.

**Files:**
- Modify: `cmd/shopifyschema/allowlist.go`, `cmd/shopifyschema/allowlist.yaml` (the header comment and the `Refund` entry), `cmd/shopifyschema/mapping.go`, `cmd/shopifyschema/emit_go.go` (the kind constants it writes into the model), `integrations/shopify/mapper.go`, `integrations/shopify/admin.go`
- Regenerate with `go run ./cmd/shopifyschema`: `integrations/shopify/generated/model.go`, `dsl/shopify/generated/refund.memql`, `dsl/shopify/generated/selections/Order.graphql`, `dsl/shopify/generated/selections/Refund.graphql`; then the concept snapshot and whatever `make sdk-gen` and `make platform-graph` change
- Test: `cmd/shopifyschema/mapping_test.go`, `cmd/shopifyschema/emit_graphql_test.go`, `integrations/shopify/mapper_inline_test.go` (new), `component/datasync/shopify_refund_detail_db_test.go` (new, database lane), `integrations/shopify/mirror_live_test.go`

**Interfaces:**
- Produces, in the generator: `type Inline struct { Connection string; Page int }` (yaml `connection`, `page`) and `Inline []Inline` (yaml `inline`) on `Entry`; `const KindInlineList = "inlineList"`; `planField(f *Field, isRef bool, inline *Inline) (FieldPlan, bool, error)`. An inlined connection plans as `FieldPlan{Kind: KindInlineList, DSLType: "[]object", Required: false, OmitFromBulk: true, Selection: "<connection>(first: <page>) { nodes <the node's object selection> }", Extract: "<connection>.nodes[]"}`.
- Produces, in the allowlist:

```yaml
  - type: Refund
    concept: refund
    scopes: [read_orders]
    reconcile: none
    bulk: false
    inline:
      - {connection: refundLineItems, page: 50}
      - {connection: refundShippingLines, page: 10}
      - {connection: orderAdjustments, page: 10}
```

- Produces, generated: `generated.KindInlineList`; on `v1:shopify:refund` the fields `refundLineItems []object`, `refundShippingLines []object`, `orderAdjustments []object`. Each element is the node as the generator's object rule selects it. The statistics read, per refund line, `lineItem.id`, `quantity`, `subtotalSet.shopMoney.amount`, `totalTaxSet.shopMoney.amount`; per shipping line `subtotalAmountSet` and `taxAmountSet`; per adjustment `amountSet`, `taxAmountSet` and `reason`.
- Produces, in the connector: `mapObjectUnder` leaves a `KindInlineList` field out of the payload when its extracted value is nil; `RequestedCost float64` on `AdminResponse`, Shopify's `requestedQueryCost`.

Decisions:
- Validation in `Allowlist.validate` and the planner: a connection is named at most once across one entry's `children`, `references` and `inline`; `page` is 1 to 250, and 25 when omitted; the field is a connection; its node type is not an allowlist entry (a mirrored node is a child or a reference, never a second copy inline). Each refusal names the entry and the connection.
- A list the document did not select is "not read", never "empty". Today the mapper writes `[]` for every list it finds nothing for, so a bulk line would blank a refund's detail and a reader could not tell an unread refund from one with nothing on it. With the rule, a bulk line leaves the three keys out: on a new row they are absent, and on an existing row the engine's read-merge keeps what a fetch wrote. Every other list keeps the `[]` it gets today.
- Whether the refund's lists need `completeChildren`: no, and it cannot reach them. It pages only the entries of the fetched object's own `spec.Children` that carry a `PageOp`, through an operation keyed by that object's id. `refunds` is a plain list (`List: true`, no `PageOp`), which Shopify answers whole, and a refund's inlined connection is a field of the child, in no `Children` list. So it is read one page deep: a refund holding 50 refund lines is treated as possibly longer (`lines_complete` on its fact, Task 10), and Refunds stays exact because it starts from `totalRefundedSet`.
- O3, as the code has it: only a store's first sweep (no watermark) lists every order; a later sweep reads what changed since its watermark. The detail therefore arrives through the first sweep, an order's own fetch, or Task 3, and a bulk line never removes it.
- The three fields make this release roll-forward only: a refund row that holds them is refused by an older schema on its next write.
- O2 and L11: the requested cost of `ShopifyFetchOrder` is a property of the document, the same for every order. The live test measures it once. Past Shopify's 1,000 points every order fetch would be refused and recording would stop, so the task is not done until the test has run against a development store. If it fails, lower `refundLineItems` to 25, then 10, regenerate, set `refundLinesPage` in Task 10 to the same number, and write the measured cost and the page into the record's 6.1.
- The `allowlist.yaml` header gains a paragraph for `inline`, beside children and references.

- [ ] **Step 1: Write the failing tests**

```go
// cmd/shopifyschema/mapping_test.go
func TestAnInlinedConnectionCarriesItsNodesAsObjects(t *testing.T)        // Refund.refundLineItems: KindInlineList, "[]object", OmitFromBulk, Extract "refundLineItems.nodes[]", Selection starts "refundLineItems(first: 50) { nodes { id lineItem { id }"
func TestAnInlinedConnectionWhoseNodeIsMirroredIsRefused(t *testing.T)    // inline lineItems on Order: the error names Order.lineItems and LineItem
func TestAConnectionNamedTwiceOnOneEntryIsRefused(t *testing.T)           // references and inline both naming it
func TestAnInlinePageOutsideOneToTwoHundredFiftyIsRefused(t *testing.T)   // 0 is the default 25; -1 and 251 are refused
// cmd/shopifyschema/emit_graphql_test.go
func TestAnOrdersFetchSelectsEachRefundsDetailAndItsBulkDocumentDoesNot(t *testing.T) // the recorded Order.graphql: ShopifyFetchOrder and ShopifyListOrder hold the three connections inside refunds { }, the bulk documents none
// integrations/shopify/mapper_inline_test.go
func TestAnInlinedListTheDocumentDidNotSelectIsLeftOutOfTheWrite(t *testing.T) // a bulk line's refund: no refundLineItems key in the payload; duties, a plain list, is still []
func TestAnInlinedListThatWasReadAndIsEmptyIsWrittenEmpty(t *testing.T)        // {"refundLineItems":{"nodes":[]}} writes []
func TestARefundsDetailIsWrittenWithItsOrder(t *testing.T)                     // a fetched order with one refund: quantity 1, subtotalSet 36.00, shipping 5.00 on the refund's write
// component/datasync/shopify_refund_detail_db_test.go, database lane
func TestARefundRowHoldsItsLinesItsShippingAndItsAdjustments(t *testing.T)     // an order with a line refund, a shipping refund and an amount-only refund, swept: the three lists on the three rows
func TestRefundDetailArrivesWhicheverOfTheBackfillAndTheFirstSweepRunsFirst(t *testing.T) // O3, both orders
func TestABulkLineDoesNotEraseDetailAFetchRecorded(t *testing.T)               // same updatedAt, later export: the lists stay
// integrations/shopify/mirror_live_test.go
func TestLiveTheOrderFetchStaysUnderTheQueryCostCeiling(t *testing.T)          // O2, L11: creates an order and a refund as the file's other tests do, fetches it, RequestedCost <= 1000, logs the number
func TestLiveARefundIsMirroredWithItsLinesAndItsShipping(t *testing.T)         // the refund that flow makes is written with the refunded line's quantity and subtotal: Shopify answers the inlined selection as generated
```

The database test composes a real engine, the real connector and its own fake Shopify transport, as `component/datasync/shopify_partitions_db_test.go` does; its fake answers `ShopifyListOrder`, `ShopifyBulkRun`, `ShopifyBulkStatus` and the export's download with JSON lines.

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./cmd/shopifyschema/ ./integrations/shopify/ ./component/datasync/ -run 'Inlin|NamedTwice|RefundsDetail|RefundRowHolds|RefundDetailArrives|BulkLineDoesNotErase'`. Expected: FAIL to compile, then FAIL.
- [ ] **Step 3:** Implement, regenerate with `go run ./cmd/shopifyschema`, then `make concept-snapshot`, `make sdk-gen`, `make platform-graph`.
- [ ] **Step 4:** Run the same command, the three packages whole in the database lane (`TestGeneratedTreeIsNotStale` included), and the generated-file gates. Expected: PASS.
- [ ] **Step 5:** Run `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 ./integrations/shopify/ -run 'TestLiveTheOrderFetchStaysUnderTheQueryCostCeiling|TestLiveARefundIsMirroredWithItsLinesAndItsShipping' -v` with a development store's `MEMQL_TEST_SHOPIFY_STORE_DOMAIN` and `MEMQL_TEST_SHOPIFY_ADMIN_TOKEN`. Expected: PASS, with the cost logged. Without credentials both skip: report that to the controller, who runs them before this task is accepted.
- [ ] **Step 6: Commit** `shopify: a refund is mirrored with its lines, its shipping and its adjustments`.

### Task 3: A store's first order sweep under this version reads from the start

**Independent of:** Tasks 5, 6, 7, 8, 10, 17.

**Files:**
- Modify: `integrations/shopify/partitions.go`
- Test: `integrations/shopify/partitions_test.go`, `component/datasync/shopify_refund_detail_db_test.go`

**Interfaces:**
- Consumes: Task 2's inlined fields; `recordStoreHealthKey(ctx, storeID, key string, value any, extra map[string]any) error`; `reconcileByUpdatedAt`, which lists unfiltered when `since` is zero.
- Produces: `const refundDetailKey = "refundDetailFrom"`, a key of the store row's `health`. `ReconcilePartition` asks the order domain's sweep with a zero `since` while the store's health holds no such key, and stamps it with the sweep's start (RFC3339, UTC) once the sweep returns no error.

Decisions:
- Why: a refund row written before Task 2, or by a bulk line, has no detail, and an hourly sweep that reads only what changed never reads its order again. One unfiltered sweep reads every order Shopify still shows, with the detail. It rides the store's next scheduled order sweep, so nothing new is scheduled and nothing runs at boot.
- Only the order concept. Every other domain keeps the `since` it is handed.
- The stamp is written only after a sweep that read every page and wrote every row. Read `ReconcilePartition` and `sweepStore` on main before the change: if a sweep can report writes that failed without returning an error, such a sweep is not stamped either. A stamp written after a sweep that left rows unwritten would spend the one read from the start and leave those refunds without detail for good.
- A stamp that cannot be written is logged, and the next sweep reads from the start again: reading twice is harmless, since the write is guarded and skips an unchanged row.
- What it cannot reach, and stays without detail: an order Shopify no longer shows (older than 60 days, without `read_all_orders`) and an order past the 20,000 rows one sweep reads (`relistRowCap`). The record's section 10 says so.

- [ ] **Step 1: Write the failing tests**

```go
// integrations/shopify/partitions_test.go
func TestAStoresFirstOrderSweepUnderThisVersionReadsFromTheStart(t *testing.T) // a watermark is handed in, health has no refundDetailFrom: ShopifyListOrder carries no query variable, and the key is stamped
func TestTheSweepAfterItReadsOnlyWhatChanged(t *testing.T)                     // stamped: the query is updated_at:>'<since>'
func TestOnlyTheOrderDomainIsReadFromTheStart(t *testing.T)                    // a product sweep with a watermark keeps its filter
func TestASweepThatFailedIsReadFromTheStartAgain(t *testing.T)                 // an error, and a sweep one of whose writes failed: nothing stamped
// component/datasync/shopify_refund_detail_db_test.go, database lane
func TestARefundMirroredBeforeItsDetailGetsItAtTheNextSweep(t *testing.T)      // a refund row with no lists and a watermark already set: one sweep later the lists are there
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./integrations/shopify/ ./component/datasync/ -run 'FromTheStart|OnlyWhatChanged|MirroredBeforeItsDetail'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, then both packages whole in the database lane. Expected: PASS.
- [ ] **Step 5: Commit** `shopify: a store's first order sweep under this version reads every order, so older refunds get their detail`.

### Task 4: Every connected store has its shop row

**Independent of:** Tasks 5, 6, 7, 8, 10, 17.

**Files:**
- Create: `integrations/shopify/shop.go`, `integrations/shopify/shop_test.go`, `integrations/shopify/shop_db_test.go`
- Modify: `integrations/shopify/plugin.go` (`Bootstrap`), `integrations/shopify/connect_write.go` (`writeConnect`), `integrations/shopify/capabilities.go` (`handleEnsureSubscriptions`)

**Interfaces:**
- Consumes: `fetchAndMap(ctx, store, spec, gid)`, `heal(ctx, writes)`, `domainInConnection`, `scopesMissingFor`, `operatorContext`, the generated `shopifyShopForStore` read.
- Produces: `func (c *Connector) refreshShop(ctx context.Context, store Store) error` (reads the shop singleton with the generated `ShopifyFetchShop` and heals its row); `func (c *Connector) refreshShops(ctx context.Context, onlyMissing bool)` (every store that ingests; one store's failure is logged and the next is still read).

Decisions:
- Why: `v1:shopify:shop` is a singleton with no listing and no backfill, written only when a `SHOP_UPDATE` delivery arrives. A store whose settings never change has no row, so no `currencyCode` and no `ianaTimezone`.
- When: in `writeConnect`, after `queueConnectBackfills`, for the connected store; in `Bootstrap`, after `queueUnstartedBackfills`, for each ingesting store with no live shop row (`onlyMissing`); in `handleEnsureSubscriptions`, after the subscription pass, for every ingesting store, so a lost `SHOP_UPDATE` is healed within a day. `SHOP_UPDATE` stays as it is.
- A store whose connection does not ask for the shop's scope, or whose grant holds none of them, is not asked. A clean answer with no shop retires nothing. No failure here fails a connect, a boot or the subscription pass.
- The write is `heal`'s: guarded by the mirror's own rule and skipped when nothing changed, so a boot on every node writes once.

- [ ] **Step 1: Write the failing tests**

```go
// integrations/shopify/shop_test.go
func TestAConnectReadsTheStoresShop(t *testing.T)
func TestBootReadsTheShopOfAStoreThatHasNone(t *testing.T)                 // and makes no Admin call for a store that has its row
func TestTheDailyPassReadsEveryStoresShopAgain(t *testing.T)
func TestAShopThatCannotBeReadStopsNothing(t *testing.T)                   // the connect still succeeds; the next store is still read
func TestAStoreWithoutTheShopsScopeIsNotAsked(t *testing.T)                // zero Admin calls
// integrations/shopify/shop_db_test.go, database lane
func TestAConnectedStoreHasItsCurrencyAndTimeZoneWithoutAShopUpdate(t *testing.T) // Review Focus: the row reads back with currencyCode, ianaTimezone and plan
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./integrations/shopify/ -run 'StoresShop|ShopOfAStore|ShopThatCannot|ShopsScope|WithoutAShopUpdate'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, then the package whole in the database lane. Expected: PASS.
- [ ] **Step 5: Commit** `shopify: every connected store has its shop row, read at connect, at boot and daily`.

### Task 5: Periods in a zone (U2)

**Independent of:** Tasks 2, 3, 4, 6, 7, 8, 9, 10, 17.

**Files:**
- Modify: `core/statsperiod/statsperiod.go`
- Test: `core/statsperiod/statsperiod_test.go`

**Interfaces:**
- Produces: `func ForIn(p Period, now time.Time, loc *time.Location) Bounds`; `For(p, now)` is `ForIn(p, now, time.UTC)`. Every time in the answer is in `loc`. `Bounds.Next`, `Starts` and `PreviousStarts` keep their signatures.

Decisions:
- A nil `loc` is UTC.
- 24 hours: the hour under way starts at `now` less its local minutes, seconds and nanoseconds (never `Truncate(time.Hour)`, which cuts on UTC hours and is half an hour off in a +05:30 zone). `End` is that plus one hour, `Start` is `End` less 24 hours, the previous period the 24 hours before. Twenty-four buckets of one hour each.
- Days and months: a day starts at the first instant whose local date is that day. `time.Date(y, m, d, 0, 0, 0, 0, loc)` is that instant except where the clocks skip midnight, where it answers an instant on the day before or an hour into the day; then the start is the change itself, from `Time.ZoneBounds`. One unexported `startOfDay(y int, m time.Month, d int, loc *time.Location) time.Time` holds the rule.
- `End` is the start of tomorrow; `Start`, `PrevStart` and each bucket are reached by calendar days through `startOfDay`, so a bucket is 23, 24 or 25 hours long. `Bounds.Next` steps a day or a month through `startOfDay` on `t`'s own location.
- Twelve months: thirteen local months, no previous period, as before.

- [ ] **Step 1: Write the failing tests**

```go
func TestForAnswersWhatItAnsweredBefore(t *testing.T) {
	for _, p := range All() {
		if got, want := ForIn(p, at, time.UTC), For(p, at); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: ForIn(UTC) = %+v, For = %+v", p, got, want)
		}
	}
}

func TestASpringForwardDayHasTwentyThreeHoursAndAFallBackDayTwentyFive(t *testing.T) {
	madrid, _ := time.LoadLocation("Europe/Madrid")
	spring := ForIn(Last7Days, time.Date(2025, 3, 30, 12, 0, 0, 0, madrid), madrid).Starts()
	if d := spring[6].Sub(spring[5]); d != 24*time.Hour { t.Errorf("the 29th is %s long", d) }
	b := ForIn(Last7Days, time.Date(2025, 3, 30, 12, 0, 0, 0, madrid), madrid)
	if d := b.End.Sub(spring[6]); d != 23*time.Hour { t.Errorf("the 30th is %s long, want 23h", d) }
	fall := ForIn(Last7Days, time.Date(2025, 10, 26, 12, 0, 0, 0, madrid), madrid)
	if d := fall.End.Sub(fall.Starts()[6]); d != 25*time.Hour { t.Errorf("the 26th is %s long, want 25h", d) }
}

func TestTwentyFourHoursInAHalfHourZoneStartOnItsOwnHour(t *testing.T) {
	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	b := ForIn(Last24Hours, time.Date(2025, 11, 8, 10, 17, 0, 0, kolkata), kolkata)
	if want := time.Date(2025, 11, 8, 5, 30, 0, 0, time.UTC); !b.End.Equal(want) { t.Errorf("End = %s, want 11:00 +05:30", b.End) }
}

func TestADayWhoseMidnightTheClocksSkipStartsAtTheChange(t *testing.T) {
	santiago, _ := time.LoadLocation("America/Santiago")
	b := ForIn(Last7Days, time.Date(2023, 9, 3, 12, 0, 0, 0, santiago), santiago)
	day := b.Starts()[6]
	if want := time.Date(2023, 9, 3, 4, 0, 0, 0, time.UTC); !day.Equal(want) { t.Errorf("the 3rd starts %s, want 01:00 -03", day.UTC()) }
	if d := b.End.Sub(day); d != 23*time.Hour { t.Errorf("the 3rd is %s long, want 23h", d) }
	if got := day.In(santiago).Day(); got != 3 { t.Errorf("the bucket's first instant is on day %d", got) }
}

func TestAStoreDayIsNotAUTCDay(t *testing.T)        // America/Los_Angeles at 2025-11-08T03:00Z: 7d ends 2025-11-08T08:00Z, not the UTC midnight
func TestTwelveMonthsStartOnLocalFirsts(t *testing.T) // Europe/Madrid in November 2025: Start 2024-10-31T23:00Z, thirteen buckets
func TestANilZoneIsUTC(t *testing.T)
```

- [ ] **Step 2:** Run `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 ./core/statsperiod/`. Expected: FAIL (undefined `ForIn`).
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, then `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/emailstats/ ./component/sitetraffic/ -run 'Statistic|Period'` (the two callers of `For`). Expected: PASS.
- [ ] **Step 5: Commit** `statsperiod: a period can be cut in a store's own time zone`.

### Task 6: Exact money (U3)

**Independent of:** Tasks 2, 3, 4, 5, 7, 8, 9, 17.

**Files:**
- Create: `core/money/money.go`, `core/money/money_test.go`

**Interfaces:**
- Produces, package `money` (`github.com/znasllc-io/memql/core/money`), on `math/big` and nothing else:

```go
type Amount struct{ /* an unscaled big integer and a scale */ }

func Parse(s string) (Amount, error)                         // only -?digits[.digits], ASCII digits
func (a Amount) Add(b Amount) Amount                         // at the wider of the two scales
func (a Amount) Sub(b Amount) Amount
func (a Amount) Mul(n int64) Amount                          // keeps a's scale
func (a Amount) MulRatio(num, den int64) (Amount, error)     // a * num / den, rounded half to even at a's scale; den 0 is an error
func (a Amount) Scale() int
func (a Amount) Sign() int
func (a Amount) String() string                              // every digit of its scale: "42.00" stays "42.00"
```

Decisions:
- The zero value is 0 at scale 0 and usable. A value is immutable: no method changes its receiver.
- `Parse` refuses anything else, the empty string included, and its error names no input longer than 32 characters. A negative zero prints as zero.
- A sum's scale is the wider operand's, so adding "1" to "42.00" gives "43.00": the scale is the number of decimals Shopify wrote, and no table of currencies is kept.

- [ ] **Step 1: Write the failing tests**

```go
func TestParseAcceptsOnlyPlainDecimals(t *testing.T) {
	for _, ok := range []string{"0", "42", "42.00", "-0.10", "007.5", "1200", "1.250"} {
		if _, err := Parse(ok); err != nil { t.Errorf("Parse(%q): %v", ok, err) }
	}
	// the last is two Arabic-Indic digits: only ASCII digits are an amount
	for _, bad := range []string{"", " 1", "1 ", "1e3", "1E3", ".5", "5.", "+5", "1,000", "NaN", "Inf", "0x10", "1.2.3", "-", "--1", "\u0661\u0662"} {
		if _, err := Parse(bad); err == nil { t.Errorf("Parse(%q) was accepted", bad) }
	}
}

func TestOneTenthPlusTwoTenthsIsThreeTenths(t *testing.T) // Parse("0.1").Add(Parse("0.2")).String() == "0.3"

func TestASumKeepsTheWiderScale(t *testing.T)             // "1.5"+"2.25" = "3.75"; "42.00"+"1" = "43.00"; "5.00"-"5" = "0.00"

func TestMulRatioRoundsHalfToEvenAtTheOperandsScale(t *testing.T) {
	for _, c := range []struct{ a string; num, den int64; want string }{
		{"0.05", 1, 2, "0.02"}, {"0.15", 1, 2, "0.08"}, {"-0.05", 1, 2, "-0.02"},
		{"10.00", 1, 3, "3.33"}, {"10.00", 2, 3, "6.67"}, {"1.00", 2, 3, "0.67"},
		{"1200", 1, 7, "171"}, {"3.00", 2, 3, "2.00"}, {"8.00", 2, 2, "8.00"},
	} { /* MulRatio(c.num, c.den).String() == c.want */ }
}

func TestMulRatioByZeroIsRefused(t *testing.T)
func TestMulKeepsTheScale(t *testing.T)                   // "5.00".Mul(0) = "0.00"; "9.99".Mul(2) = "19.98"
func TestAMillionRandomSumsEqualBigRat(t *testing.T)      // a seeded generator of amounts at scales 0 to 4, signs both ways; the running Amount equals the running big.Rat at every step
func TestAZeroDecimalAndAThreeDecimalCurrencySumExactly(t *testing.T) // "1200"+"800" = "2000"; "1.250"+"0.005" = "1.255"
func TestTheZeroValueIsZeroAndNegativeZeroPrintsAsZero(t *testing.T)   // Amount{}.String() == "0"; Parse("-0.00").String() == "0.00"
func TestNoMethodChangesItsReceiver(t *testing.T)
```

- [ ] **Step 2:** Run `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 ./core/money/`. Expected: FAIL (the package does not exist).
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, and from `core/` run `GOWORK=off MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 ./money/` (the module needs nothing outside itself). Expected: PASS.
- [ ] **Step 5: Commit** `money: exact decimal amounts, with half-to-even rounding in one place`.

### Task 7: Tables (U4)

**Independent of:** Tasks 2, 3, 4, 5, 6, 8, 9, 10, 17.

**Files:**
- Create: `component/database/memory-nodes/migrations/20261112090000_store_sales_facts.up.sql`, `component/database/memory-nodes/migrations/20261112090000_store_sales_facts.down.sql` (a later minute if Task 1 found a newer migration), `component/database/memory-nodes/store_sales_facts_migration_test.go`

**Interfaces:**
- Produces seven tables. Statements are separated by `--bun:split`; every table is `CREATE TABLE IF NOT EXISTS` and every index `CREATE INDEX IF NOT EXISTS`. The file opens with a comment in the manner of `20261108090300_email_stat_facts.up.sql`: side tables and not graph rows, every id bare, no personal data, what a version is.

```sql
CREATE TABLE IF NOT EXISTS store_sales_order_fact (
    store_id                 TEXT NOT NULL,
    order_gid                TEXT NOT NULL,
    order_version            TIMESTAMPTZ NOT NULL,   -- the mirror row's own write time (row.createdAt)
    recorded_at              TIMESTAMPTZ NOT NULL,   -- the row's syncedAt
    ordered_at               TIMESTAMPTZ NOT NULL,
    currency                 TEXT NOT NULL,
    cancelled                BOOLEAN NOT NULL,
    test                     BOOLEAN NOT NULL,
    taxes_included           BOOLEAN NOT NULL,
    buyer_key                TEXT NOT NULL DEFAULT '',
    wholesale                BOOLEAN NOT NULL DEFAULT false,
    shipping                 NUMERIC NOT NULL,
    tax                      NUMERIC NOT NULL,
    duties                   NUMERIC NOT NULL,
    expected_quantity        INTEGER NOT NULL,
    shopify_current_subtotal NUMERIC,
    shopify_total_discounts  NUMERIC,
    shopify_total_refunded   NUMERIC,
    gross                    NUMERIC NOT NULL DEFAULT 0,      -- derived
    discounts                NUMERIC NOT NULL DEFAULT 0,      -- derived
    units                    INTEGER NOT NULL DEFAULT 0,      -- derived
    line_count               INTEGER NOT NULL DEFAULT 0,      -- derived
    lines_version            TIMESTAMPTZ,                     -- derived
    lines_complete           BOOLEAN NOT NULL DEFAULT false,  -- derived
    checked_at               TIMESTAMPTZ,
    PRIMARY KEY (store_id, order_gid)
);
CREATE INDEX IF NOT EXISTS store_sales_order_fact_ordered_idx ON store_sales_order_fact (store_id, ordered_at);
CREATE INDEX IF NOT EXISTS store_sales_order_fact_buyer_idx ON store_sales_order_fact (store_id, buyer_key, ordered_at);

CREATE TABLE IF NOT EXISTS store_sales_line_fact (
    store_id            TEXT NOT NULL,
    order_gid           TEXT NOT NULL,
    line_gid            TEXT NOT NULL,
    line_version        TIMESTAMPTZ NOT NULL,
    gift_card           BOOLEAN NOT NULL,
    product_key         TEXT NOT NULL,
    title               TEXT NOT NULL,
    quantity            INTEGER NOT NULL,
    current_quantity    INTEGER NOT NULL,
    unit_price          NUMERIC NOT NULL,
    allocated_discounts NUMERIC NOT NULL,
    refunded_quantity   INTEGER NOT NULL DEFAULT 0,  -- derived
    sold_quantity       INTEGER NOT NULL DEFAULT 0,  -- derived
    gross               NUMERIC NOT NULL DEFAULT 0,  -- derived
    discounts           NUMERIC NOT NULL DEFAULT 0,  -- derived
    checked_at          TIMESTAMPTZ,
    PRIMARY KEY (store_id, order_gid, line_gid)
);
CREATE INDEX IF NOT EXISTS store_sales_line_fact_product_idx ON store_sales_line_fact (store_id, product_key);

CREATE TABLE IF NOT EXISTS store_sales_refund_fact (
    store_id           TEXT NOT NULL,
    refund_gid         TEXT NOT NULL,
    order_gid          TEXT NOT NULL,
    refund_version     TIMESTAMPTZ NOT NULL,
    recorded_at        TIMESTAMPTZ NOT NULL,
    refunded_at        TIMESTAMPTZ NOT NULL,
    money_refunded     NUMERIC NOT NULL,
    shipping_refunded  NUMERIC,              -- null while the detail is not read
    tax_refunded       NUMERIC,              -- as Shopify wrote it; a figure reads it as 0 for an order whose prices include tax
    duties_refunded    NUMERIC,
    detail_fetched     BOOLEAN NOT NULL,
    lines_complete     BOOLEAN NOT NULL,
    gift_card_refunded NUMERIC,              -- derived
    item_refunds       NUMERIC,              -- derived
    units_returned     INTEGER,              -- derived
    checked_at         TIMESTAMPTZ,
    PRIMARY KEY (store_id, refund_gid)
);
CREATE INDEX IF NOT EXISTS store_sales_refund_fact_refunded_idx ON store_sales_refund_fact (store_id, refunded_at);
CREATE INDEX IF NOT EXISTS store_sales_refund_fact_order_idx ON store_sales_refund_fact (store_id, order_gid);

CREATE TABLE IF NOT EXISTS store_sales_refund_line_fact (
    store_id   TEXT NOT NULL,
    refund_gid TEXT NOT NULL,
    position   INTEGER NOT NULL,
    order_gid  TEXT NOT NULL,
    line_gid   TEXT NOT NULL,
    quantity   INTEGER NOT NULL,
    subtotal   NUMERIC NOT NULL,
    tax        NUMERIC NOT NULL,
    PRIMARY KEY (store_id, refund_gid, position)
);
CREATE INDEX IF NOT EXISTS store_sales_refund_line_fact_line_idx ON store_sales_refund_line_fact (store_id, order_gid, line_gid);

CREATE TABLE IF NOT EXISTS store_sales_store (
    store_id            TEXT PRIMARY KEY,
    store_domain        TEXT NOT NULL DEFAULT '',
    store_name          TEXT NOT NULL DEFAULT '',
    currency            TEXT NOT NULL DEFAULT '',
    timezone            TEXT NOT NULL DEFAULT '',
    taxes_included      BOOLEAN,
    development         BOOLEAN,
    all_orders_readable BOOLEAN NOT NULL DEFAULT false,
    history_since       TIMESTAMPTZ,
    wholesale_enabled   BOOLEAN NOT NULL DEFAULT false,
    wholesale_digest    TEXT NOT NULL DEFAULT '',
    wholesale_cursor    TEXT NOT NULL DEFAULT '',
    verdict_state       TEXT NOT NULL DEFAULT '',
    verdict_reasons     JSONB NOT NULL DEFAULT '[]',
    verdict_since       TIMESTAMPTZ,
    last_change_at      TIMESTAMPTZ,
    walked_through      TIMESTAMPTZ,
    rebuilding_since    TIMESTAMPTZ,
    definition          INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS store_sales_watermark (
    store_id       TEXT NOT NULL,
    source         TEXT NOT NULL,                    -- order, line or refund
    through        TIMESTAMPTZ NOT NULL,
    window_seconds INTEGER NOT NULL DEFAULT 86400,
    walked_through TIMESTAMPTZ,
    PRIMARY KEY (store_id, source)
);

CREATE TABLE IF NOT EXISTS store_sales_check (
    store_id      TEXT PRIMARY KEY,
    started_at    TIMESTAMPTZ,
    order_cursor  TEXT NOT NULL DEFAULT '',
    line_cursor   TEXT NOT NULL DEFAULT '',
    refund_cursor TEXT NOT NULL DEFAULT '',
    finished_at   TIMESTAMPTZ,
    mirror_rows   INTEGER NOT NULL DEFAULT 0,
    fact_rows     INTEGER NOT NULL DEFAULT 0,
    mismatched    INTEGER NOT NULL DEFAULT 0,
    repaired      INTEGER NOT NULL DEFAULT 0,
    disagreeing   INTEGER NOT NULL DEFAULT 0,
    clean_at      TIMESTAMPTZ
);
```

Decisions:
- The down file drops the seven tables, the check first and the order fact last.
- The round trip stands beside the other migration round trips (`role_test_session_account_migration_test.go`), not in the component: it reads the two files, rewrites `CREATE TABLE` to `CREATE TEMP TABLE`, and runs up, down, up inside one transaction it rolls back.
- "On Postgres and TimescaleDB" is held two ways: the lane's database is TimescaleDB, and the migration uses nothing of it, which the third test reads off the text.

- [ ] **Step 1: Write the failing tests**

```go
func TestStoreSalesFactsMigrationRoundTrips(t *testing.T)               // up, down, up: after the first and the third step the seven tables, their primary keys and the six named indexes exist; after the second none does
func TestStoreSalesMoneyColumnsAddExactly(t *testing.T)                 // two order facts with shipping 0.1 and 0.2: SUM(shipping)::text is "0.3"; 1234567890123.456789 reads back digit for digit
func TestStoreSalesFactsMigrationUsesNothingOfTimescale(t *testing.T)   // neither file names create_hypertable, timescaledb or a continuous aggregate
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/database/memory-nodes/ -run 'StoreSales'`. Expected: FAIL (no such file).
- [ ] **Step 3:** Write the two files.
- [ ] **Step 4:** Run the same command, then `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/database/...`. Expected: PASS.
- [ ] **Step 5: Commit** `database: the store sales fact tables`.

### Task 8: The sales part (U5)

**Independent of:** Tasks 2, 3, 4, 5, 6, 7, 9, 10, 17.

**Files:**
- Modify: `component/auth/rbac_model.go` (`appPartGrants`), `dsl/rbac/seeds.memql`, `component/memql/organization_action_capabilities.go`, `component/memql/app_resource_os_parity_test.go` (the part list of `assertKnownResourcesNameEveryApp`)
- Create: `component/memql/sales_capability_test.go`

**Interfaces:**
- Produces: `"app:deployables/sales": {RoleOwner, RoleDeveloper}` in `appPartGrants`; the seeds `cap-owner-execute-app-deployables-sales` and `cap-developer-execute-app-deployables-sales` (`verb: "execute"`, `resourceType: "app:deployables/sales"`, `predefined: true`); `organizationDeployableCapability("app:deployables/sales")` is true; `organizationCapabilityTarget` answers `("v1:platform:site", "siteId")` for a builtin named `storeStatistics`.

Decisions:
- The seed descriptions, in the form the reviews seeds use: "owner: the sales part of the Deployables app -- a storefront's sales figures, on its Store statistics page." and the same for developer. The comment above them and in `rbac_model.go` gives S6's reason: revenue is more sensitive than visits, so it is a part of its own, granted to a client's group beside `read app:deployables`.
- The builtin is declared in Task 15, and its row in `TestTheDeployablesPartsAreDeclaredOnTheirConstructs` lands there too: that test loads the shipped tree. So does the record's database proof that a holder in one organization is refused a site of another: the gate judges a loaded builtin against a stored site.

- [ ] **Step 1: Write the failing tests**

```go
// component/memql/sales_capability_test.go
func TestTheSalesBuiltinNamesItsOrganizationTarget(t *testing.T) {
	if !organizationDeployableCapability("app:deployables/sales") {
		t.Fatal("app:deployables/sales is not an organization deployable capability, so the read is judged against the caller's global capability alone")
	}
	fn := &Function{Name: "storeStatistics", FunctionKind: "builtin",
		RequiresCapability: CapabilityRequirement{Verb: auth.VerbExecute, Resource: "app:deployables/sales"}}
	if concept, argument := organizationCapabilityTarget(fn); concept != "v1:platform:site" || argument != "siteId" {
		t.Errorf("storeStatistics resolves its organization target to (%q, %q)", concept, argument)
	}
}
func TestOwnerAndDeveloperHoldTheSalesPartAndNobodyElse(t *testing.T) // through the seeded catalog: owner and developer hold execute on it; admin, writer and reader do not
```

  and add `"sales"` to the part list in `assertKnownResourcesNameEveryApp`.

- [ ] **Step 2:** Run `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 ./component/memql/ ./component/auth/ -run 'Sales|KnownResources|Seed|Capabilit'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 ./component/auth/ ./integrations/rbac/`, and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `rbac: a sales part of Deployables, judged in the organization of the site`.

### Task 9: The walk's shapes and queries (U6)

**Independent of:** Tasks 5, 6, 7, 8, 10, 17.

**Files:**
- Modify: `dsl/shopify/overlay/shapes.memql`, `dsl/shopify/overlay/queries.memql`, `packs/wholesalepack/dsl/shapes.memql`, `packs/wholesalepack/dsl/queries.memql`, `test/dslconformance/server_only_parsed_test.go`
- Create: `integrations/shopify/statistics_reads_authz_db_test.go`, `packs/wholesalepack/statistics_reads_db_test.go`

**Interfaces:**
- Consumes: Task 2's three fields on `v1:shopify:refund`, each `[]object`: `refundLineItems`, `refundShippingLines`, `orderAdjustments`.
- Produces six `@row` shapes in `dsl/shopify/overlay/shapes.memql`:

| Shape | Concept | Fields, in order |
| --- | --- | --- |
| `storeStatisticsOrder` | `order` | `row.id`, `row.createdAt`, `storeId`, `gid`, `syncedAt`, `deleted`, `processedAt`, `sourceCreatedAt`, `cancelledAt`, `test`, `taxesIncluded`, `currencyCode`, `customerGid`, `email`, `subtotalLineItemsQuantity`, `totalShippingPriceSet`, `totalTaxSet`, `originalTotalDutiesSet`, `currentSubtotalPriceSet`, `totalDiscountsSet`, `totalRefundedSet` |
| `storeStatisticsLine` | `orderLineItem` | `row.id`, `row.createdAt`, `storeId`, `gid`, `parentGid`, `deleted`, `isGiftCard`, `productGid`, `title`, `quantity`, `currentQuantity`, `originalUnitPriceSet`, `discountAllocations` |
| `storeStatisticsRefund` | `refund` | `row.id`, `row.createdAt`, `storeId`, `gid`, `parentGid`, `syncedAt`, `deleted`, `processedAt`, `sourceCreatedAt`, `totalRefundedSet`, `duties`, `refundLineItems`, `refundShippingLines`, `orderAdjustments` |
| `storeStatisticsBuyer` | `order` | `row.id`, `row.createdAt`, `storeId`, `gid`, `deleted`, `email` |
| `storeStatisticsShop` | `shop` | `row.id`, `row.createdAt`, `storeId`, `deleted`, `name`, `currencyCode`, `ianaTimezone`, `taxesIncluded`, `plan` |
| `storeStatisticsStore` | `store` | `row.id`, `domain`, `name`, `isDevelopment`, `scopesGranted`, `uninstalledAt`, `redactedAt`, `health` |

- Produces six queries in `dsl/shopify/overlay/queries.memql`, each `@actor`, `@serverOnly`, `@cache(0)`. The four walks have this form, tombstones included:

```memql
@actor
@serverOnly
@cache(0)
query order storeStatisticsOrdersChanged {
  args {
    storeId  string!
    since    datetime!
  }
  filter    row => actor.isClusterOwner == true && row.storeId == args.storeId && row.createdAt >= args.since
  sort      "row.createdAt", "asc"
  paginate  250
  shape     storeStatisticsOrder
}
```

  `storeStatisticsLinesChanged` (concept `orderLineItem`, shape `storeStatisticsLine`), `storeStatisticsRefundsChanged` (`refund`, `storeStatisticsRefund`) and `storeStatisticsOrderBuyers` (`order`, `storeStatisticsBuyer`) are the same with their concept and shape. `storeStatisticsShop { storeId string! }` filters `actor.isClusterOwner == true && row.storeId == args.storeId`, sorts `"row.createdAt", "desc"`, `paginate 1`. `storeStatisticsStores` takes no argument, filters `actor.isClusterOwner == true`, sorts `"row.createdAt", "asc"`, `paginate 100`.
- Produces, in the wholesale pack: the `@row` shapes `entitlementStatisticsRef` (`row.id`, `storeId`, `buyerEmail`, `state`) and `wholesaleSettingsStatisticsRef` (`row.id`, `storeId`); and two `@serverOnly`, `@actor` queries, each with an `@description`: `wholesaleStatisticsGranted { storeIds []string! }` on `entitlement`, filter `actor.isClusterOwner == true && row.storeId in args.storeIds && row.state == "granted"`, and `wholesaleStatisticsSettings { storeIds []string! }` on `wholesaleSettings`, filter `actor.isClusterOwner == true && row.storeId in args.storeIds`; both sort `"row.createdAt", "asc"` and `paginate 250`.

Decisions:
- `row.createdAt >= args.since` stays a top-level `&&` conjunct: only that shape lets the engine collapse the versions written since, so a page costs what was written since and not the concept's history (`component/memql/latest_scan_keys.go`). The existing walks in `dsl/campaigns/queries.memql` are the pattern, their `///` line included: each query here carries one that says why `actor.userId` cannot scope it and that the cluster-owner predicate and internal origin are both mandatory.
- The pack's reads take a list because the pack's rows hold a store id in either form and the engine does not join the two: the caller passes `<id>` and `v1:shopify:store:<id>`. The settings read is a query of its own because a query is bound to one concept.
- The pack's files are appended to and nothing in them is reordered, so a rebase over the wholesale branch is a textual merge.
- Each overlay query is added to the map in `TestServerOnlyParsedSetMatchesTheTree` with a comment giving its reason: a mirror row has no owner to scope an actor against, and the one reader is the statistics walk under its maintenance principal.
- O4, from `dsl/pack_enablement.go`: a disabled pack keeps its concepts and loads none of its queries, so asking one on a node where the pack is off is an unknown call. The pack's test holds that; Task 13 asks the pack's state before it asks a query.

- [ ] **Step 1: Write the failing tests**

```go
// integrations/shopify/statistics_reads_authz_db_test.go, database lane, with authorityEngine and seedStore
func TestTheStatisticsReadsAnswerNothingToAReaderAndRowsToAClusterOwner(t *testing.T) // each of the six: a reader with internal origin gets zero rows; a cluster owner with client origin is refused as server-only; a cluster owner with internal origin gets the seeded row
func TestAChangedReadAnswersATombstoneToo(t *testing.T)                               // a row with deleted true comes back, deleted true
func TestTheStatisticsShapesProjectExactlyTheirFields(t *testing.T)                   // the loaded tree's six shapes hold the lists above and nothing else: no name, address or phone, and email on the two order shapes only
// packs/wholesalepack/statistics_reads_db_test.go, database lane
func TestTheGrantedReadFindsABuyerUnderEitherFormOfTheStoreId(t *testing.T)           // one entitlement stored with the bare id, one with v1:shopify:store:<id>: asked both forms, both; asked one, one
func TestOnlyGrantedEntitlementsAreAnswered(t *testing.T)                             // pending, failed and revoked are not
func TestTheSettingsReadSaysWhichStoresAreSetUp(t *testing.T)
func TestThePacksStatisticsReadsAreForAClusterOwnerWithInternalOrigin(t *testing.T)
func TestWithThePackDisabledItsStatisticsQueriesAreNotLoaded(t *testing.T)            // O4
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./integrations/shopify/ ./packs/wholesalepack/ -run 'StatisticsRead|ChangedRead|StatisticsShapes|GrantedRead|OnlyGranted|SettingsRead|StatisticsQueries'`. Expected: FAIL.
- [ ] **Step 3:** Write the constructs, then `make sdk-gen`.
- [ ] **Step 4:** Run the same command, `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./packs/wholesalepack/`, and the generated-file gates. Expected: PASS, and `make sdk-gen-check` shows no builder for any of the eight.
- [ ] **Step 5: Commit** `shopify: the server-only reads the store statistics walk asks`.

### Task 10: The rules that turn a mirrored row into a fact (U7)

**Independent of:** Tasks 2, 3, 4, 5, 7, 8, 9, 17.

**Files:**
- Create: `component/storestats/doc.go`, `component/storestats/facts.go`, `component/storestats/facts_test.go`

**Interfaces:**
- Consumes: Task 6's `money.Amount`, with `money.Parse(s string) (Amount, error)`, `Add(b Amount) Amount`, `Sub(b Amount) Amount`, `Mul(n int64) Amount`, `MulRatio(num, den int64) (Amount, error)` and `Scale() int`; a mirrored row as a `map[string]any`, with the keys of the table below.
- Produces:

```go
const refundLinesPage = 50 // the page Task 2 inlines refundLineItems with

type OrderFact struct {
	StoreID, OrderGID            string
	Version, RecordedAt          time.Time // row.createdAt; syncedAt
	OrderedAt                    time.Time
	Currency, BuyerKey           string
	Cancelled, Test, TaxIncluded bool
	Shipping, Tax, Duties        money.Amount
	ExpectedQuantity             int
	ShopifyCurrentSubtotal, ShopifyTotalDiscounts, ShopifyTotalRefunded *money.Amount
	Email                        string // read in memory for the wholesale judgement; never written
}
type LineFact struct {
	StoreID, OrderGID, LineGID    string
	Version                       time.Time
	GiftCard                      bool
	ProductKey, Title             string
	Quantity, CurrentQuantity     int
	UnitPrice, AllocatedDiscounts money.Amount
}
type RefundLineFact struct {
	Position      int
	LineGID       string
	Quantity      int
	Subtotal, Tax money.Amount
}
type RefundFact struct {
	StoreID, RefundGID, OrderGID                    string
	Version, RecordedAt, RefundedAt                 time.Time
	MoneyRefunded                                   money.Amount
	DetailFetched, LinesComplete                    bool
	ShippingRefunded, TaxRefunded, DutiesRefunded   *money.Amount // nil while the detail is not read
	Lines                                           []RefundLineFact
}

type rowKind int
const (
	rowLive       rowKind = iota // write the fact
	rowGone                      // a tombstone: delete the fact
	rowUnreadable                // no fact can be made; counted and logged, never guessed
)

func orderFactOf(row map[string]any) (OrderFact, rowKind, error)
func lineFactOf(row map[string]any) (LineFact, rowKind, error)
func refundFactOf(row map[string]any) (RefundFact, rowKind, error)
func soldOf(quantity, currentQuantity, refundedQuantity int, unitPrice, allocated money.Amount) (sold int, gross, discounts money.Amount)
func instantOf(v any) (time.Time, bool) // a time.Time, or an RFC3339 string
```

- Consumes, key by key. A row is what a shape of Task 9 answers: `row.id` under `id`, `row.createdAt` under `createdAt`, every other field under its own name. A money value is an object, and its amount is the decimal string at `shopMoney.amount`.

| Fact | Fields | Read from the row |
| --- | --- | --- |
| order | `StoreID`, `OrderGID`, `Version`, `RecordedAt` | `storeId`, `gid`, `createdAt`, `syncedAt` |
| order | `OrderedAt` | `processedAt`, else `sourceCreatedAt` |
| order | `Currency`, `BuyerKey`, `Email` | `currencyCode`, `customerGid`, `email` |
| order | `Cancelled`, `Test`, `TaxIncluded` | `cancelledAt` is not empty; `test`; `taxesIncluded` |
| order | `Shipping`, `Tax`, `Duties` | `totalShippingPriceSet`, `totalTaxSet`, `originalTotalDutiesSet` |
| order | `ExpectedQuantity` | `subtotalLineItemsQuantity` |
| order | `ShopifyCurrentSubtotal`, `ShopifyTotalDiscounts`, `ShopifyTotalRefunded` | `currentSubtotalPriceSet`, `totalDiscountsSet`, `totalRefundedSet`; nil when the row holds no such object |
| line | `StoreID`, `OrderGID`, `LineGID`, `Version` | `storeId`, `parentGid`, `gid`, `createdAt` |
| line | `GiftCard`, `Title`, `Quantity`, `CurrentQuantity`, `UnitPrice` | `isGiftCard`, `title`, `quantity`, `currentQuantity`, `originalUnitPriceSet` |
| line | `ProductKey` | `productGid`, else `title:` and the title |
| line | `AllocatedDiscounts` | the sum of `allocatedAmountSet` over `discountAllocations` |
| refund | `StoreID`, `RefundGID`, `OrderGID`, `Version`, `RecordedAt` | `storeId`, `gid`, `parentGid`, `createdAt`, `syncedAt` |
| refund | `RefundedAt`, `MoneyRefunded` | `processedAt`, else `sourceCreatedAt`; `totalRefundedSet` |
| refund | `DutiesRefunded` | the sum of `amountSet` over `duties` |
| refund | `ShippingRefunded` | the sum of `subtotalAmountSet` over `refundShippingLines` |
| refund | `TaxRefunded` | the sum of `totalTaxSet` over `refundLineItems`, of `taxAmountSet` over `refundShippingLines` and of `taxAmountSet` over `orderAdjustments` |
| refund | `Lines` | `refundLineItems` in order: `lineItem.id`, `quantity`, `subtotalSet`, `totalTaxSet` |
| any | `rowGone` | `deleted` is true |

Decisions:
- How a tombstone is recognised: by `deleted == true` and nothing else. The mirror retires a row with a raw insert that carries only `storeId`, `gid`, `updatedAt`, `versionSource`, `syncedAt`, `versionReadAt` and `deleted: true` (`integrations/shopify/mapper.go`, `tombstone`; `component/datasync/mirror_writer.go`, `retireFieldsFor`), and the engine's read-merge keeps every other field the row held. So a tombstone still shows its last amounts, its `updatedAt` is the time MemQL read Shopify and not Shopify's version, and its `versionSource` is `read` or `announced`. None of those is read: a row with `deleted` true is `rowGone`.
- A fact's `Version` is the row's own `createdAt`, the instant the mirror wrote that version of the row. It is how the engine itself orders a row's versions, and it moves forward on every write, a tombstone and a revival included; the row's `updatedAt` does not.
- 5.1 and 5.2 of the record are the formulas, and the table above is how they read a row. `Tax` is zero at `totalTaxSet`'s scale when `taxesIncluded`. An absent money object is zero, except the three Shopify totals, which are nil. A `rowGone` answer carries its ids and its `Version` and nothing else is read: `deleted` is judged first, and a tombstone needs only `storeId`, `gid` and `createdAt`.
- A refund's `TaxRefunded` is kept as Shopify wrote it (the order's `taxesIncluded` is applied where a figure reads it). `Lines` are the refund lines in order, `Position` from 0, `LineGID` from `lineItem.id`.
- `DetailFetched` is true when all three inlined values are lists, empty or not, and false when any is nil: a shape projects an absent field as nil. Then the three parts are nil and there are no lines. `LinesComplete` is `len(refundLineItems) < refundLinesPage`.
- `soldOf` is 5.1: `sold = min(quantity, currentQuantity + refundedQuantity)`; `gross = unitPrice.Mul(sold)`; `discounts` is zero at `allocated`'s scale when `quantity` is 0, else `allocated` when `sold == quantity`, else `allocated.MulRatio(sold, quantity)`, whose error cannot occur there because a zero quantity was answered first.
- A row is `rowUnreadable`, with an error that names the field and never a value, when it has no `gid`, `storeId` or `createdAt`, no date, an amount `money.Parse` refuses, or a quantity that is not a whole number. Quantities are narrowed through `core/num`.

- [ ] **Step 1: Write the failing tests**

```go
func TestSoldOfIsFivePointOne(t *testing.T) {
	for _, c := range []struct {
		q0, q1, qr           int
		price, allocated     string
		sold                 int
		gross, discounts     string
	}{
		{2, 2, 0, "40.00", "8.00", 2, "80.00", "8.00"},
		{3, 1, 1, "10.00", "3.00", 2, "20.00", "2.00"}, // one refunded, one removed by an edit
		{3, 2, 0, "9.99", "1.00", 2, "19.98", "0.67"},
		{2, 1, 0, "5.00", "0.05", 1, "5.00", "0.02"},   // half to even
		{2, 0, 2, "5.00", "1.00", 2, "10.00", "1.00"},  // refunded in full: still sold
		{2, 0, 0, "5.00", "1.00", 0, "0.00", "0.00"},   // removed by an edit
		{0, 0, 0, "5.00", "1.00", 0, "0.00", "0.00"},   // no units: nothing is divided, and no discount is counted
		{1, 1, 1, "5.00", "0.00", 1, "5.00", "0.00"},   // never more than were ordered
	} { /* soldOf(...) equals sold, gross, discounts as strings */ }
}

func TestAnOrderRowBecomesItsFact(t *testing.T)                    // shipping "12.50", tax "4.20", no duties ("0"), buyer key, currency, the three Shopify totals, Email kept
func TestTaxIncludedInPricesIsNotAddedAgain(t *testing.T)          // taxesIncluded true: Tax is "0.00"
func TestAnOrderIsDatedByProcessedAtElseItsCreation(t *testing.T)
func TestARowWithNoDateOrAnAmountThatDoesNotParseIsUnreadable(t *testing.T) // rowUnreadable; the error names the field and holds no "12,50"
func TestATombstoneIsRecognisedByDeletedAlone(t *testing.T)        // deleted true, versionSource "read", every amount still set: rowGone
func TestALineRowBecomesItsFact(t *testing.T)                      // productGid as the key; "title:Inner tube" without one; two allocations of 5.00 and 3.00 are 8.00
func TestARefundRowBecomesItsFactAndItsLines(t *testing.T)         // M 48.56, shipping 5.00, tax 7.56, duties 0, one line: position 0, quantity 1, subtotal 36.00
func TestARefundWhoseDetailWasNotReadSaysSo(t *testing.T)          // the three values nil: DetailFetched false, the three parts nil, no lines
func TestARefundReadWithNothingOnItIsFetched(t *testing.T)         // three empty lists: DetailFetched true, the three parts "0"
func TestARefundWithFiftyLinesIsNotKnownToBeComplete(t *testing.T) // 49 lines: complete; 50: not
func TestAQuantityIsAWholeNumberOrTheRowIsUnreadable(t *testing.T) // 2.0 and json.Number("2") read 2; 2.5 does not
func TestTheVersionOfAFactIsTheRowsOwnWriteTime(t *testing.T)      // createdAt as a time.Time and as an RFC3339Nano string
```

- [ ] **Step 2:** Run `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 ./component/storestats/`. Expected: FAIL (the package does not exist).
- [ ] **Step 3:** Implement. `doc.go` is the package comment, in the manner of `component/emailstats/doc.go`: why a side table, why through the engine, what a version is, why no personal data.
- [ ] **Step 4:** Run the same command and `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 -run 'TestEveryPayloadNarrowingCarriesAnAnswer' .`. Expected: PASS.
- [ ] **Step 5: Commit** `storestats: the rules that turn a mirrored order, line and refund into a fact`.

### Task 11: The walk (U7)

**Independent of:** Task 17.

**Files:**
- Create: `component/storestats/integration.go`, `component/storestats/rollup.go`, `component/storestats/derive.go`, `component/storestats/main_dbschema_test.go`, `component/storestats/harness_db_test.go`, `component/storestats/rollup_db_test.go`
- Modify: `dsl/shopify/overlay/builtins.memql`, `dsl/shopify/overlay/automations.memql`, `component/auth/maintenance_actor.go`, `component/automations/maintenance_actor_gate_test.go`, `component/automations/strict_automation_boot_test.go` (`shippedAutomationCount`, plus one, with its comment), `app/plugins_core.go` (the blank import, beside `component/emailstats`), `scripts/ci/db-gated-packages.sh` (the tree and a paragraph for it), `test/fixtures/pipelines/delivery-after-bootstrap.yaml` (the same list of trees)
- Modify, at the repository root: `module_taxonomy_test.go` (`"storeStats": kindComponent`, with its reason), `call_origin_conformance_test.go` (the allowlist entry for `component/storestats`), `memql-package.yaml` (`select.dbGated`, the same list of trees)
- Regenerate: the platform graph; the automation corpus file `component/automations/steps/testdata/automation_corpus/shopify/storeStatisticsRollupSweep.json` with `go test ./component/automations/steps/ -run TestAutomationCorpusRuns -update`

**Interfaces:**
- Consumes: Task 7's seven `store_sales_*` tables; Task 9's queries `storeStatisticsStores()`, `storeStatisticsOrdersChanged(storeId, since)`, `storeStatisticsLinesChanged(storeId, since)` and `storeStatisticsRefundsChanged(storeId, since)`; Task 10's `orderFactOf(row map[string]any) (OrderFact, rowKind, error)`, `lineFactOf(row map[string]any) (LineFact, rowKind, error)`, `refundFactOf(row map[string]any) (RefundFact, rowKind, error)`, `soldOf(quantity, currentQuantity, refundedQuantity int, unitPrice, allocated money.Amount) (sold int, gross, discounts money.Amount)`, `instantOf(v any) (time.Time, bool)` and the kinds `rowLive`, `rowGone` and `rowUnreadable`.
- Produces, in the DSL: `builtin storeStatisticsRollup { }` with `@executor("integration.storeStats.rollup")`, an `@description`, and no `@sdk`; `automation storeStatisticsRollupSweep` with `@trigger(schedule="15 * * * * *")` whose one step is `roll := builtin storeStatisticsRollup()`; `storeStatisticsRollupSweep` in `maintenanceAutomations` with its reason.
- Produces, in Go:

```go
const (
	sourceOrder  = "order"
	sourceLine   = "line"
	sourceRefund = "refund"
)

const (
	watermarkOverlap = 5 * time.Minute
	walkPageSize     = 250
	maxPagesPerRun   = 40
	maxReadsPerRun   = 400
	defaultWindow    = 24 * time.Hour
	maxWindow        = 32 * 24 * time.Hour
	minWindow        = time.Minute
)

// No mirror row is older than the mirror itself. A variable, so a test can move it.
var historyFloor = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

type Engine interface {
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
}

// Integration carries the engine, the database getter, the logger, and three nil-safe seams:
//   now          func() time.Time
//   storeState   func(ctx context.Context, store map[string]any, now time.Time) (outcome string, err error) // Task 12; nil keeps every store
//   wholesaleFor func(ctx context.Context, storeIDs []string) (map[string]wholesaleState, error)            // Task 13; nil means no store is enabled
type Integration struct{ /* ... */ }

func NewIntegration(engine Engine, bunDB func() *bun.DB, logger *slog.Logger) *Integration
func (i *Integration) IntegrationName() string                     // "storeStats"
func (i *Integration) Capabilities() []memql.IntegrationCapability // "rollup" here; Tasks 14 and 15 add theirs

type wholesaleState struct {
	Enabled bool
	Granted map[string]bool // folded emails
	Digest  string
}

type RollupReport struct {
	Stores     int            `json:"stores"`
	Rows       map[string]int `json:"rows"`
	Facts      int            `json:"facts"`
	Unreadable int            `json:"unreadable"`
	Behind     []string       `json:"behind,omitempty"`  // "<store>/<source>"
	Rebuilt    []string       `json:"rebuilt,omitempty"` // Task 12
	Removed    []string       `json:"removed,omitempty"` // Task 12
}

func walkContext(ctx context.Context) context.Context // the package's one internal-origin stamp, with a fresh, roots-only read
func lockWrites(ctx context.Context, tx bun.Tx) error // LOCK TABLE store_sales_store IN SHARE ROW EXCLUSIVE MODE
func runRollup(ctx context.Context, i *Integration, now time.Time) (RollupReport, error)

type walkWindow struct {
	Through time.Time     // rows written up to here are folded; zero is never walked
	Width   time.Duration
}

// walkSource reads one source of one store in creation order from w, in bounded windows, at most budget
// pages that held rows, handing each page to visit. caughtUp is the clock the last page of a read to the
// clock was asked by, and zero when the read stopped behind.
func walkSource(ctx context.Context, engine Engine, query string, storeID string, w walkWindow, clock func() time.Time, budget int,
	visit func(rows []map[string]any, through time.Time) error) (next walkWindow, caughtUp time.Time, err error)

type pageResult struct {
	Facts, Unreadable int
	Orders            []string  // the order gids the page touched
	LastRecorded      time.Time
}

// foldPage writes one page's facts inside tx: guarded upserts, deletes for tombstones, then deriveOrders.
func foldPage(ctx context.Context, tx bun.Tx, storeID, source string, rows []map[string]any, wholesale func(email string) bool) (pageResult, error)
func deriveOrders(ctx context.Context, tx bun.Tx, storeID string, orderGIDs []string) error
```

Decisions:
- Registration: the package registers itself with the engine under the name `storeStats` from its `init`, the way `component/emailstats/plugin.go` does, with the name as a string literal there so the taxonomy gate sees it. It is linked on every node type, because the builtin is declared in a tree every binary loads.
- `handleRollup` admits one caller: `actor.UserId == "system:maintenance:storeStatisticsRollupSweep"` and `actor.IsClusterOwner()`, checked before anything is read; everybody else, a cluster owner included, gets "store statistics rollup requires the engine maintenance principal". One run per process (an atomic flag, as the email rollup keeps); a second answers nothing.
- The run, for each store `storeStatisticsStores` answers (every page, each store once, in bare-id order): make sure its `store_sales_store` row exists; call `storeState` when set, add the store to `Rebuilt` or `Removed` as it answers, and leave a removed store; walk order, line and refund with `walkSource`, each from its `store_sales_watermark` row, 40 pages each; set the store's `walked_through` to the least of its three sources' `walked_through`, null while any is null. One store's failure is logged with its id and the next store still runs; the run's error joins them.
- `walkSource`, with the constants above and `historyFloor` (the connector that writes mirror rows first shipped later in August 2026, so no row is older):
  - `since` is `Through` less the overlap, or `historyFloor` when `Through` is zero. `until` is `since` plus `Width`.
  - When `until` is within the overlap of the clock, the read is to the clock: `paginate(query <name>(storeId: <q>, since: <q>), 250)`. Its pages move `Through` to the newest `createdAt` read, never past the clock; its last page is caught up at the clock that page was asked by.
  - Otherwise the read is of a past window, as of its end: the same call wrapped as `asOf(<call>, <until>)`, the form `walkQuery` in `component/emailstats/rollup.go` writes. Its last page sets `Through` to `until`, doubles `Width` up to `maxWindow`, and the walk goes on to the next window in the same call.
  - A page that outlasts a read halves `Width` down to `minWindow`, saves it, and ends this source for the run; at `minWindow` it is the source's error. A read to the clock that times out sets `Width` to half the span it tried. The test for "outlasts a read" is the email walk's `isPageTimeout`, copied here with its test: the tree keeps this reading beside each walk it serves (`component/retention` has its own).
  - Only pages that held rows count against `budget`; every read counts against `maxReadsPerRun`. A cursor the engine hands back unchanged is an error, never a loop.
  Why windows: a read costs every version of the concept written since `since`, on the whole cluster (`component/memql/latest_scan_keys.go`), and a first fill, a rebuild and the check all start at the beginning. Without a bound a large mirror's first page outlasts the statement timeout on every run, as the email walk's did before it was bounded.
- Each page is one transaction: `lockWrites`, `foldPage`, the watermark (`through` by `GREATEST`, `window_seconds`, `walked_through`), and the store's `last_change_at` by `GREATEST` with the page's `LastRecorded`. The lock is the one `component/sitetraffic/rollup.go` takes on its own table: it serialises two nodes' writers and blocks no reader.
- `foldPage`: a live row is upserted with `ON CONFLICT ... DO UPDATE ... WHERE excluded.<row>_version >= <table>.<row>_version`; an upsert never touches a derived column or `checked_at`. A tombstone deletes its fact when the fact's version is not newer (a refund's lines with it); a line's is found by `(store_id, line_gid)` and a refund's by `(store_id, refund_gid)`, since a tombstone need not carry its parent, and the order the deleted fact named is among the orders the page touched. A refund's lines are replaced whole with the refund. An unreadable row writes nothing, is counted, and is logged once with the store, the source, the gid and the field. An order's `wholesale` is `wholesale(fact.Email)`, false when the function is nil.
- Facts follow rows one for one. The mirror retires only the order (a delete notice, or two clean empty fetches); `orderLineItem` and `refund` have no notice and no sweep of their own, so a tombstoned order's lines and refunds stay live rows, and their facts stay. They count nowhere, because every figure reaches a line or a refund through its order's fact (Task 15).
- `deriveOrders`, for the orders a page touched, in the page's transaction: each line's `refunded_quantity` is the sum of the refund-line quantities naming it; `sold_quantity`, `gross` and `discounts` come from `soldOf`, computed in Go and written back (the rounding is `money`'s, never SQL's); the order's `gross`, `discounts` and `units` sum its lines that are not gift cards, `line_count` and `lines_version` are over all its lines, and `lines_complete` is the sum of `quantity` over all its lines being at least `expected_quantity`; a fetched refund's `gift_card_refunded` sums the subtotals of its lines that name a gift-card line, `units_returned` the quantities of the others, and `item_refunds` is `money_refunded - shipping_refunded - duties_refunded - gift_card_refunded`, less `tax_refunded` unless the order's `taxes_included`; all three are null for a refund whose detail is not read.
- The allowlist entry in `call_origin_conformance_test.go`: server-initiated, under the two maintenance principals, each checked before its walk; one stamp, in `walkContext`, for the `@serverOnly` reads and the verdict; the package writes only its own tables.
- The package runs in the database lane's default class.

- [ ] **Step 1: Write the failing tests** (database lane, unless marked)

`harness_db_test.go` boots one engine per process as `component/emailstats/rollup_db_test.go` does, registers this package's provider on it, and offers: `principalCtx(t, "storeStatisticsRollupSweep")`; `newStore(t) string` (a fresh store id and its store row, written by `createStore` under an owner); `mirror(t, concept, storeID, gid string, payload map[string]any)` (a raw insert under `auth.ContextWithConnectorActor(ctx, "shopify")`, at the id `shopify.MirrorRowID(storeID, gid)`, with `deleted: false` unless given); and reads of the store's rows in each `store_sales_*` table. Every assertion is about the store the test made.

```go
func TestAnOrderWithThreeLinesWritesItsFacts(t *testing.T)                  // 2 x 40.00 less 8.00, 1 x 19.99, a 25.00 gift card, subtotalLineItemsQuantity 4: gross "99.99", discounts "8.00", units 3, line_count 3, lines_complete true
func TestALineRefundIsWrittenOnItsOwnDayAndTheOrdersDayKeepsItsSale(t *testing.T) // refunded_at is the refund's; the line's refunded_quantity 1, sold_quantity still 2; item_refunds "36.00"
func TestAShippingRefundIsNoRefundOfItems(t *testing.T)                     // shipping_refunded "5.00", item_refunds "0.00"
func TestAnAmountOnlyRefundCountsItsAmountLessItsTax(t *testing.T)          // M 12.10, an adjustment whose tax is 2.10, no line: item_refunds "10.00"
func TestARefundOfAnOrderWhosePricesIncludeTaxKeepsItsTax(t *testing.T)     // taxes_included: item_refunds is M less shipping only
func TestAGiftCardRefundIsNoRefundOfItems(t *testing.T)                     // gift_card_refunded "25.00", units_returned 0
func TestAnEditThatRemovesAUnitLowersGrossAndWritesNoRefund(t *testing.T)   // currentQuantity 1 of 2, no refund line: sold 1, gross "40.00"
func TestAnEditThatAddsALineRaisesGross(t *testing.T)
func TestACancelledOrderIsMarkedAndKeepsItsFact(t *testing.T)
func TestATombstoneDeletesItsFact(t *testing.T)                             // order; line; refund with its lines
func TestTheLinesOfATombstonedOrderKeepTheirFacts(t *testing.T)             // and the order has none
func TestAnOrderThatComesBackAfterATombstoneIsWrittenAgain(t *testing.T)
func TestAnOlderVersionArrivingAfterANewerOneChangesNothing(t *testing.T)   // foldPage with a row whose createdAt is earlier than the fact's version
func TestARowCommittedBehindTheWatermarkIsReadByTheNextRun(t *testing.T)    // O7: a row whose createdAt is two minutes before the watermark
func TestAWalkPagesEveryMirroredRowOnceInCreationOrder(t *testing.T)        // O7: 600 orders, three pages, 600 facts, no cursor repeated
func TestTwoRunsAtOnceWriteTheSameFacts(t *testing.T)
func TestAPastWindowIsReadAsOfItsEndAndTheNextStartsThere(t *testing.T)     // a row rewritten after the window: the window's read writes its older version, a later one the newer
func TestAPageThatOutlastsAReadHalvesTheWindow(t *testing.T)                // an engine double answering the statement timeout: window_seconds 43200, no watermark moved
func TestAnEmptyStretchOfHistoryIsCrossedInWidenedWindows(t *testing.T)     // historyFloor a year back, one row today: found in one run, within maxReadsPerRun
func TestAStoreIsNotWalkedThroughUntilEverySourceReachedTheClock(t *testing.T)
func TestARefundWrittenByABulkLineHasNoDetailAndOneReadEmptyHasIt(t *testing.T) // Review Focus, through the real shape: an absent key reads nil, [] reads empty
func TestARowThatCannotBeReadIsCountedAndTheWalkGoesOn(t *testing.T)
func TestOneStoresFailureLeavesTheNextStoreWalked(t *testing.T)
func TestOnlyTheRollupsPrincipalRunsIt(t *testing.T)                        // a cluster owner, an automation's reader actor and another maintenance principal are refused; no query is asked
func TestTheWalkReadsNoFieldOutsideItsShapes(t *testing.T)                  // every key foldPage reads is a field of its shape
func TestTheProviderOffersWhatItsBuiltinsName(t *testing.T)                 // not database: the registered name is storeStats and every integration.storeStats.* executor in the tree has its capability
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/storestats/`. Expected: FAIL to compile, then FAIL.
- [ ] **Step 3:** Implement, then `make sdk-gen`, `make platform-graph`, and write the corpus file with `-update`.
- [ ] **Step 4:** Run the same command; `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/auth/ ./component/automations/... ./scripts/cidb/ ./scripts/ci/`; `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -tags bff -p 1 -count=1 ./app/`; `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 -run 'TestOnlyAllowlistedPackagesStampInternalOrigin|TestEveryRegisteredPluginIsClassified|TestEveryPayloadNarrowingCarriesAnAnswer|TestDSLCallStringsDoNotUseGoQuoting|TestEngineManifest|TestActivatedDeliveryManifest' .`; the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `storestats: a walk fills the sales facts of every store each minute`.

### Task 12: What the walk keeps about each store (U7)

**Independent of:** Task 17.

**Files:**
- Create: `component/storestats/store.go`, `component/storestats/store_db_test.go`
- Modify: `component/storestats/integration.go` (`NewIntegration` sets `storeState`, and the `verdictOf` seam), `component/storestats/rollup.go` (the run clears `rebuilding_since`), `component/storestats/harness_db_test.go`

**Interfaces:**
- Consumes: Task 11's `Integration` and its seam `storeState func(ctx context.Context, store map[string]any, now time.Time) (outcome string, err error)`, `walkContext(ctx context.Context) context.Context`, `lockWrites(ctx context.Context, tx bun.Tx) error` and the `Rebuilt` and `Removed` lists of `RollupReport`; a store row as Task 9's `storeStatisticsStores()` answers it (`id`, `domain`, `name`, `isDevelopment`, `scopesGranted`, `uninstalledAt`, `redactedAt`, `health`); Task 9's `storeStatisticsShop(storeId)`, whose one row holds `id`, `createdAt`, `storeId`, `deleted`, `name`, `currencyCode`, `ianaTimezone`, `taxesIncluded` and `plan`; Task 10's `instantOf(v any) (time.Time, bool)`; the mirror's `builtin shopifyStoreVerdict(storeId)`, whose one row holds `state`, `since` (RFC3339) and `reasons` (each with `code` and `sentence`).
- Produces:

```go
const storeStatsDefinition = 1

type verdictReason struct{ Code, Sentence string }
type storeVerdict struct {
	State   string
	Since   time.Time
	Reasons []verdictReason
}
// a seam on Integration: verdictOf func(ctx context.Context, storeID string) (storeVerdict, error); its default asks the builtin

const (
	outcomeRebuilt = "rebuilt"
	outcomeRemoved = "removed" // the walk leaves the store
)

func (i *Integration) keepStore(ctx context.Context, store map[string]any, now time.Time) (outcome string, err error) // what storeState is set to
func rebuildStore(ctx context.Context, tx bun.Tx, storeID string, now time.Time) error
```

Decisions:
- `keepStore` reads first (the shop row, the verdict) and writes after, in one transaction of its own that begins with `lockWrites`: no engine call is made while the lock is held.
- A store with `redactedAt` set: its rows are deleted from all seven tables and the answer is `outcomeRemoved` (D17).
- Otherwise `store_sales_store` is written from the store row and the shop row (`storeStatisticsShop`): `store_domain`; `store_name` (the store row's `name`, else the shop row's); `currency`, `timezone` and `taxes_included` from a live shop row, left as they are when there is none; `development` is the shop row's `plan.partnerDevelopment` when there is a shop row, else the store row's `isDevelopment`, else null (D4); `all_orders_readable` is whether `scopesGranted` holds `read_all_orders`, the test the mirror makes in `stampOrdersFrom`; `history_since` is `health.ordersFrom`, a day read as its UTC midnight, else the store's oldest `ordered_at`.
- The verdict, from `verdictOf`: `verdict_state` and `verdict_reasons` (code and sentence only) are copied. `verdict_since` is when the store left `recording`: set to the reply's `since` when the stored state was `recording` or empty and the new one is not, kept while the state stays other than `recording`, and set to the reply's `since` when it returns to `recording`. A verdict that cannot be read writes an empty state and logs once a run.
- `rebuildStore`, in the caller's transaction: delete the store's rows in the four fact tables, its watermarks and its check row; set `rebuilding_since` to `now`, `walked_through` to null and `definition` to `storeStatsDefinition`. `keepStore` calls it when the stored `definition` is below `storeStatsDefinition` and answers `outcomeRebuilt`. A new row has `definition` 0, so a store's first fill is a rebuild.
- The run clears `rebuilding_since` once the store's `walked_through` is within 5 minutes of the clock.

- [ ] **Step 1: Write the failing tests** (database lane). The harness gains `shopRow(t, storeID, currency, zone string, development bool)` (a raw insert of a `v1:shopify:shop` row under the connector actor) and a `verdictOf` stub.

```go
func TestTheShopsCurrencyZoneAndTaxSettingAreCopied(t *testing.T)
func TestAStoreWithNoShopRowKeepsNoCurrencyAndNoZone(t *testing.T)          // and keeps the ones it had when the shop row is retired
func TestADevelopmentStoreIsToldFromALiveOne(t *testing.T)                  // the shop row's plan; with no shop row, the store row's flag; with neither, null
func TestTheStoresNameAndDomainAreCopied(t *testing.T)                      // the store row's name, else the shop's
func TestAGrantWithoutAllOrdersIsRecorded(t *testing.T)                     // scopesGranted without read_all_orders: all_orders_readable false; with it, true
func TestOrdersSinceIsTheStoresOwnOrTheOldestOrder(t *testing.T)            // health.ordersFrom "2025-09-12" reads 2025-09-12T00:00:00Z
func TestTheVerdictIsCopiedAndWhenTheStoreLeftRecordingIsKept(t *testing.T) // recording, paused at t1, needs attention at t2: verdict_since stays t1; recording again at t3: t3
func TestAVerdictThatCannotBeReadIsNotKnown(t *testing.T)
func TestALowerDefinitionRebuilds(t *testing.T)                             // definition 0: rebuilding_since set, cleared once the walk reaches the clock, and the facts are the same as before
func TestAStoreWhoseDataWasRemovedLosesItsRows(t *testing.T)                // redactedAt: nothing of it in any of the seven tables, and it is not walked
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/storestats/ -run 'Shop|Development|NameAndDomain|AllOrders|OrdersSince|Verdict|Definition|DataWasRemoved'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, then the package whole in the database lane. Expected: PASS.
- [ ] **Step 5: Commit** `storestats: the walk keeps each store's currency, time zone, verdict and definition`.

### Task 13: Wholesale and retail (U7)

**Independent of:** Task 17.

**Files:**
- Create: `component/storestats/wholesale.go`, `component/storestats/wholesale_db_test.go`
- Modify: `component/storestats/integration.go` (`NewIntegration` sets `wholesaleFor`), `component/storestats/rollup.go` (the run asks it once and continues a pass), `component/storestats/harness_db_test.go`

**Interfaces:**
- Consumes: Task 9's `wholesaleStatisticsGranted(storeIds)` (a row holds `id`, `storeId`, `buyerEmail`, `state`), `wholesaleStatisticsSettings(storeIds)` (a row holds `id`, `storeId`) and `storeStatisticsOrderBuyers(storeId, since)` (a row holds `id`, `createdAt`, `storeId`, `gid`, `deleted`, `email`); Task 11's `wholesaleState`, its seam `wholesaleFor func(ctx context.Context, storeIDs []string) (map[string]wholesaleState, error)`, `walkSource(ctx, engine, query, storeID, w walkWindow, clock, budget, visit) (next walkWindow, caughtUp time.Time, err error)`, `lockWrites(ctx context.Context, tx bun.Tx) error`, `walkContext(ctx context.Context) context.Context`, `historyFloor`, and the `wholesale func(email string) bool` argument of `foldPage`; `PackDomainDisabled(domain string) bool` in package `github.com/znasllc-io/memql/dsl`.
- Produces:

```go
const wholesaleDomain = "wholesale"

func foldEmail(s string) string                     // lower case, trimmed
func grantedDigest(granted map[string]bool) string  // hex SHA-256 of the sorted folded emails joined by "\n"; never empty
func (i *Integration) wholesaleOf(ctx context.Context, storeIDs []string) (map[string]wholesaleState, error)
func wholesalePass(ctx context.Context, i *Integration, storeID string, st wholesaleState, clock func() time.Time, budget int) error
```

Decisions:
- O6: the component asks `PackDomainDisabled("wholesale")`. It is the loaders' own answer for this node, set at boot from the `v1:platform:packState` rows over the pack's declared default, and it is what `component/memql/module_registry.go` reports as loaded or mounted inert. The rows themselves (`ReadPackStates`) are the cluster's wish and can differ from a node until it restarts; the question here is whether the pack's queries exist on the node that runs the pass. When it answers true, no pack query is asked, every store's `wholesale_enabled` is false, and its digest, cursor and stored flags are left as they are.
- Otherwise, once a run: both reads are asked with both forms of every store's id (`<id>` and `v1:shopify:store:<id>`), paged to their end, and grouped by the bare id. A store is enabled when it has a settings row (D10). Its granted set is the folded `buyerEmail` of its granted entitlements.
- An order is wholesale when its store is enabled and `foldEmail(email)` is not empty and is in the granted set. An empty email matches nothing, an entitlement with an empty address included. This is `foldPage`'s `wholesale` function for the run, so a new order by a granted buyer is wholesale when it is written.
- A pass starts when an enabled store's digest differs from the stored one, and when a store becomes enabled (its stored `wholesale_enabled` was false), whatever its digest: orders written while it was not enabled were written retail. Starting is one transaction that stores the digest, sets `wholesale_enabled` and sets `wholesale_cursor` to `historyFloor` as RFC3339Nano. While the cursor is not empty the run continues a pass: `storeStatisticsOrderBuyers` through `walkSource` from the cursor, at most 40 pages, each page one transaction that sets `wholesale` true for its granted orders and false for the rest (tombstones skipped) and moves the cursor; the last page empties it. A set that changes mid-pass starts the pass again.
- The email is compared in memory. No address, and nothing derived from one but the digest, reaches a table or a log.
- A store that stops being enabled keeps its flags; the read answers `wholesale: null` for it (Task 15).

- [ ] **Step 1: Write the failing tests** (database lane; the pack is linked into the test engine through `packs/anchor`, once per process, before it boots)

```go
func TestAGrantedBuyersOrdersBecomeWholesaleWithinOneRun(t *testing.T)
func TestARevokedBuyersOrdersGoBackToRetail(t *testing.T)
func TestANewOrderByAGrantedBuyerIsWholesaleWhenItIsWritten(t *testing.T)
func TestABuyerIsMatchedWhateverCaseAndSpacesTheEmailCarries(t *testing.T) // " Buyer-1@Example.TEST " on the order, buyer-1@example.test on the entitlement
func TestAnOrderWithNoEmailIsNeverWholesale(t *testing.T)                  // even beside an entitlement whose buyerEmail is empty
func TestGrantedBuyersAreFoundUnderEitherFormOfTheStoreId(t *testing.T)    // Review Focus: one entitlement under each form, both buyers wholesale
func TestAStoreWithNoSettingsRowIsNotEnabled(t *testing.T)
func TestWithThePackOffNoPackQueryIsAskedAndTheFlagsAreLeft(t *testing.T)  // a recording engine: no query whose name starts wholesaleStatistics
func TestAStoreEnabledAgainJudgesTheOrdersWrittenMeanwhile(t *testing.T)   // pack off, a granted buyer's order is written retail, pack on with the same granted set: the order is wholesale after the pass
func TestAnOrderWhoseEmailWasRemovedGoesBackToRetail(t *testing.T)         // the order row is rewritten with another email: its fact is retail on the next run, with no pass
func TestAPassThatStoppedAtItsBudgetCarriesOnFromItsCursor(t *testing.T)   // budget 1, 600 orders: three runs, then the cursor is empty
func TestAGrantedSetThatChangesMidPassStartsThePassAgain(t *testing.T)
func TestNoEmailReachesATable(t *testing.T)                                // after a run no text column of any store_sales_ table holds an "@"
func TestTheDigestOfNoBuyersIsNotTheDigestNeverComputed(t *testing.T)      // not database
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/storestats/ -run 'Wholesale|Granted|Retail|Email|PackOff|Digest|SettingsRow|ItsCursor|EnabledAgain'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, then the package whole in the database lane. Expected: PASS.
- [ ] **Step 5: Commit** `storestats: an order is wholesale when its buyer holds granted access now`.

### Task 14: The check and the rebuild (U8)

**Independent of:** Task 17.

**Files:**
- Create: `component/storestats/check.go`, `component/storestats/check_db_test.go`
- Modify: `component/storestats/integration.go` (two capabilities), `component/storestats/rollup.go` (a check starts when a rebuild's walk has caught up), `dsl/shopify/overlay/builtins.memql`, `dsl/shopify/overlay/automations.memql`, `component/auth/maintenance_actor.go`, `component/automations/maintenance_actor_gate_test.go`, `component/automations/strict_automation_boot_test.go` (plus one)
- Regenerate: the platform graph; the corpus file `.../automation_corpus/shopify/storeStatisticsCheckSweep.json`

**Interfaces:**
- Consumes: Task 11's `walkSource(ctx, engine, query, storeID, w walkWindow, clock, budget, visit) (next walkWindow, caughtUp time.Time, err error)`, `foldPage(ctx context.Context, tx bun.Tx, storeID, source string, rows []map[string]any, wholesale func(email string) bool) (pageResult, error)`, `deriveOrders(ctx context.Context, tx bun.Tx, storeID string, orderGIDs []string) error`, `lockWrites(ctx context.Context, tx bun.Tx) error`, `walkContext(ctx context.Context) context.Context`, `historyFloor` and the seam `wholesaleFor`; Task 9's `storeStatisticsOrdersChanged`, `storeStatisticsLinesChanged` and `storeStatisticsRefundsChanged`, each `(storeId, since)`; Task 12's `rebuildStore(ctx context.Context, tx bun.Tx, storeID string, now time.Time) error`; Task 10's `orderFactOf`, `lineFactOf` and `refundFactOf`, each from a `map[string]any` to its fact, a `rowKind` and an error.
- Produces, in the DSL: `builtin storeStatisticsCheck { }` (`@executor("integration.storeStats.check")`, no `@sdk`); `builtin storeStatisticsRebuild { storeId string! }` (`@executor("integration.storeStats.rebuild")`, no `@sdk`); `automation storeStatisticsCheckSweep` with `@trigger(schedule="0 40 * * * *")`; `storeStatisticsCheckSweep` in `maintenanceAutomations`.
- Produces, in Go:

```go
type CheckReport struct {
	Stores      int      `json:"stores"`
	Compared    int      `json:"compared"`
	Mismatched  int      `json:"mismatched"`
	Repaired    int      `json:"repaired"`
	Disagreeing int      `json:"disagreeing"`
	Finished    []string `json:"finished,omitempty"`
}
func runCheck(ctx context.Context, i *Integration, now time.Time) (CheckReport, error)
// checkStore carries one store's pass on by budget pages of each source; finished is true when the pass ended in this call.
func checkStore(ctx context.Context, i *Integration, storeID string, now time.Time, budget int) (finished bool, err error)
```

Decisions:
- `handleCheck` admits `system:maintenance:storeStatisticsCheckSweep` with the cluster-owner role and nobody else: "store statistics check requires the engine maintenance principal". `handleRebuild` admits a caller for whom `actor.IsClusterOwner()` and nobody else, checked before the store id is read: "store statistics rebuild is for a cluster owner". Neither is in the generated clients; both are callable by name by any signed-in user, which is why each check is in Go.
- A pass is one walk of the store's three sources from `historyFloor`, in the walk's windows, comparing instead of trusting. Its state is the store's `store_sales_check` row: `started_at`, and for each source a cursor that is empty (not begun), an RFC3339Nano instant (resume there) or `done`. A run carries each store's pass on by 40 pages a source. A pass that finished starts again at the next run.
- A store is checked only when its walk has caught up (`walked_through` within 10 minutes of the clock) and it is not rebuilding; otherwise the run leaves it.
- The cut is `started_at` less 5 minutes. A row written after the cut is the walk's to fold, not a mismatch: its fact, if any, is marked seen and not compared.
- For a row at or before the cut: a live row with no fact, or with a fact whose version is older than the row's `createdAt`, is a mismatch and is recomputed from the row in hand through `foldPage` (the guard still holds, so a newer fact is never replaced); a tombstone with a fact is a mismatch and the fact is deleted; a refund whose refund-line facts are not as many as its `refundLineItems` is a mismatch and is rewritten. Every fact a page names gets `checked_at = started_at`. A recomputed order is judged wholesale or retail with the store's granted set, asked once a run through `wholesaleFor` as the walk asks it.
- Every order page re-derives its orders, so a derived column is recomputed from its facts once a pass whether or not anything differed.
- When all three cursors are `done`: delete, and count as mismatched and repaired, every fact of the store with `checked_at` null or older than `started_at` and a version at or before the cut (a fact no mirrored row answers for); count `disagreeing`; write `finished_at`, the counts, and `clean_at` when `mismatched` is 0.
- `disagreeing` (D14) counts the store's counted orders with complete lines for which any of these holds: `shopify_total_refunded` is not the sum of `money_refunded` over its refund facts; `shopify_total_discounts` is not the sum of `allocated_discounts` over all its lines; no line has `current_quantity` strictly between 0 and `quantity`, and `shopify_current_subtotal` is not the sum over its lines with `current_quantity = quantity` of `unit_price * quantity - allocated_discounts`. Nothing reads it but a person; the live checks L1 to L4 decide whether a predicate was read wrongly.
- `handleRebuild`: one transaction, `lockWrites` then `rebuildStore`. The walk refills; when it clears `rebuilding_since` it calls `checkStore` for that store in the same run, and a rebuild is proven when that pass finishes with `mismatched` 0.

- [ ] **Step 1: Write the failing tests** (database lane)

```go
func TestAFactDeletedByHandIsRestored(t *testing.T)                        // each of the four tables in turn; mismatched and repaired count it
func TestAFactWhoseVersionWasSetBackIsRecomputed(t *testing.T)
func TestAFactWithNoMirroredRowIsDeleted(t *testing.T)                     // an order fact, a line fact and a refund fact with its lines
func TestATombstonesFactIsDeleted(t *testing.T)
func TestADerivedColumnChangedByHandIsDerivedAgain(t *testing.T)           // gross set to 1.00 on an order fact
func TestARowWrittenAfterThePassBeganIsNotAMismatch(t *testing.T)          // an order written a second ago, no fact yet: mismatched 0, clean_at set
func TestAStoreWhoseWalkIsBehindIsNotChecked(t *testing.T)
func TestAPassThatStoppedAtItsBudgetCarriesOnAndFinishes(t *testing.T)     // budget 1 and 600 orders: no orphan delete before the last page
func TestACleanPassSaysSoAndADirtyOneDoesNot(t *testing.T)                 // clean_at set only when mismatched is 0, and kept from an earlier clean pass
func TestAnOrderWhoseRefundsDoNotAddUpToShopifysTotalDisagrees(t *testing.T)
func TestARebuildEndsWithTheSameFactsAndACleanCheck(t *testing.T)          // every fact row equal before and after but checked_at; clean_at set by the pass the walk started
func TestOnlyTheChecksPrincipalRunsIt(t *testing.T)
func TestOnlyAClusterOwnerRebuilds(t *testing.T)                           // a developer and a writer are refused, nothing deleted
func TestTwoChecksAtOnceLeaveOneAnswer(t *testing.T)
func TestARecomputedOrderKeepsItsWholesaleSide(t *testing.T)               // a granted buyer's order fact deleted by hand comes back wholesale
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/storestats/ -run 'Fact|Tombstones|Derived|PassBegan|Behind|Budget|CleanPass|Disagrees|Rebuild|Principal|ChecksAtOnce|Recomputed'`. Expected: FAIL.
- [ ] **Step 3:** Implement, then `make sdk-gen`, `make platform-graph`, and write the corpus file with `-update`.
- [ ] **Step 4:** Run the same command, the package whole in the database lane, `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/auth/ ./component/automations/...`, and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `storestats: an hourly check proves the facts against the mirror, and a rebuild refills them`.

### Task 15: The read (U9, U11)

**Independent of:** Task 17.

**Files:**
- Create: `component/storestats/statistics.go`, `component/storestats/statistics_payload.go`, `component/storestats/statistics_db_test.go`, `component/storestats/statistics_scale_test.go`
- Modify: `component/storestats/integration.go` (the `statistics` capability), `dsl/shopify/overlay/builtins.memql`, `component/memql/app_resource_os_parity_test.go` (`"storeStatistics": "app:deployables/sales"` in `TestTheDeployablesPartsAreDeclaredOnTheirConstructs`)
- Regenerate: `sdk/go/client/generated_builtins.go`, `sdk/ts/src/client/generated_builtins.ts`

**Interfaces:**
- Consumes: Task 5's `statsperiod.ForIn(p Period, now time.Time, loc *time.Location) Bounds` and the existing `statsperiod.Parse(s string, allowed ...Period) (Period, error)`; Task 7's tables, as Tasks 11 to 14 fill them; Task 8's part `app:deployables/sales`, which the engine judges in the organization of the site `siteId` names for a builtin called `storeStatistics`; Task 6's `money.Amount`; Task 11's `Integration` and its `Capabilities()`; the site read `query siteById(siteId: <q>)` under the caller's own context, whose row holds `kind`, `systemOwned`, `binding` and `previewBinding` (each `{storeId}`).
- Produces, in the DSL:

```memql
@sdk
@executor("integration.storeStats.statistics")
@args(profile="object")
@requiresCapability("execute", "app:deployables/sales")
@description(<what the answer holds, that a figure not known is null and never zero, and who may ask>)
builtin storeStatistics {
  siteId   string!  @description("The storefront deployable.")
  storeId  string!  @description("The store of one of its destinations, as a bare id.")
  period   string!  @description("24h, 7d, 30d, 90d or 12m.")
}
```

- Produces, in Go: `const statisticsConcept = "integration:storeStats:statistics"`; `type statisticsRequest struct { SiteID, StoreID, Destination string; Period statsperiod.Period }` (`StoreID` bare, `Destination` one of `production`, `testing`, `both`); `func readStatistics(ctx context.Context, db *bun.DB, req statisticsRequest, now time.Time) (Statistics, error)`, which reads only `store_sales_*`; `func (s Statistics) Payload() map[string]any`; `handleStatistics`, which makes the gate's checks, builds the request and answers one node with id `statistics`.
- Produces the answer, one row. Money is a decimal string; everything not known is `null`:

```
{
  storeId, siteId, storeName, storeDomain, destination: "production" | "testing" | "both",
  currency, timezone, development, taxesIncluded, olderOrderChangesNotRecorded,
  period, bucket, start, end, prevStart, historySince, lastChangeAt, asOf,
  recording: { state, reasons: [{ code, sentence }], since },
  rebuilding,
  figures:  { totalSales, netSales, orders, averageOrderValue, grossSales, discounts, refunds, unitsSold },
  previous: { ...the same eight } | null,
  floors:   { <figure>: true, ... },
  series:   [{ at, totalSales, netSales, orders, grossSales, discounts, refunds, unitsSold }],
  productsBySales: [{ key, title, value }], productsByUnits: [{ key, title, value }],
  productCount, refundsWithoutItem, incompleteOrders, incompleteRefunds, refundsNotRead,
  buyers: { new, returning, unknown },
  wholesale: null | { wholesale: { orders, netSales }, retail: { orders, netSales } },
  otherCurrencies: [{ currency, orders }]
}
```

Decisions:
- The gate, in this order. The engine has already judged `execute app:deployables/sales` in the organization of the named site. Then the handler: (1) `statsperiod.Parse(period)`, whose own sentence refuses a bad period, before anything is read; (2) the site read under the caller's own actor; (3) it is a `shopify_storefront` and not `systemOwned`; (4) `memql.BareShortId` of its `binding.storeId` or of its `previewBinding.storeId` equals `memql.BareShortId(storeId)`. Any of (2) to (4) failing answers the one sentence of the Global Constraints. `destination` says which binding matched. Only then is `store_sales_*` read, in one read-only transaction, by the bare store id. No order row is read, so the caller need not be a cluster owner.
- 5.3 of the record defines every figure and is implemented as written. A counted order is one with `cancelled` false and, unless the store's `development` is true, `test` false. A counted refund is a refund fact whose order fact exists and is counted. Money is summed only over orders, and refunds of orders, whose `currency` is the store's; the others are counted in Orders and Units and listed in `otherCurrencies`.
- The store's zone is loaded in Go (`time.LoadLocation`; the engine already embeds the zone database). The period is `statsperiod.ForIn(period, now, loc)`. SQL cuts buckets with `width_bucket(<instant>, <the bucket starts as a timestamptz array>)`, never with `AT TIME ZONE`: every boundary then comes from one zone database, the one that cut the period. `series[].at`, `start`, `end` and `prevStart` are RFC3339 with the store's offset.
- `asOf` is `verdict_since` when the state is `paused`, `needs attention` or `not connected`, else `walked_through`. A period's sums cover `[start, min(end, asOf))`. A bucket that starts after `asOf`, or ends at or before `historySince`, is null in every figure.
- Null, never zero:
  - no zone, or one that does not load: every figure, every bucket, every list; the frame is cut in UTC;
  - `rebuilding_since` set, state `starting`, or `walked_through` null: every figure and bucket;
  - no currency: every money figure; `orders` and `unitsSold` stay;
  - a counted refund in the period with `detail_fetched` false: `refunds`, `netSales`, `totalSales`, `unitsSold`, for the period and for the bucket it falls in; `refundsNotRead` counts them (D22);
  - a counted refund in the period with `lines_complete` false: `unitsSold`; `incompleteRefunds` counts them;
  - no order in the period: `averageOrderValue`.
- `floors` names `grossSales`, `discounts`, `netSales`, `totalSales`, `averageOrderValue` and `unitsSold` when a counted order in the period has `lines_complete` false; `incompleteOrders` counts them.
- `previous` is null for 12 months, when `prevStart` is before `historySince`, and whenever every figure is null; otherwise it is the same eight figures over the previous period, with the same null rules.
- `averageOrderValue` is rounded to four decimals. A product's title is that of its newest line in the period. `refundsWithoutItem` is the period's `item_refunds` less its refund lines' subtotals on lines that are not gift cards. `buyers.unknown` counts the period's orders with no `buyer_key`; a buyer is new when their first counted order in the store's facts is inside the period.
- `wholesale` is null unless the store's `wholesale_enabled`; a refund is on its order's side.
- `olderOrderChangesNotRecorded` is `not all_orders_readable`.
- O5: the handler runs on every call. The test below holds it.
- `statistics_scale_test.go` is opt-in, in the way of `component/emailstats/statistics_scale_test.go`: with `MEMQL_STORESTATS_SCALE_EVIDENCE_FILE` set it writes 200,000 orders, 600,000 lines and 20,000 refunds for one store and requires the 90-day read under 500 ms.

- [ ] **Step 1: Write the failing tests** (database lane)

```go
func TestAWeekOfSalesReadsToTheCent(t *testing.T) {
	// Europe/Madrid, EUR, now 2025-11-08T10:17Z, period 7d.
	// Order A, Nov 5 12:00 local: 2 x 40.00 less 8.00, 1 x 19.99; shipping 5.00, tax 21.00; prices do not include tax.
	// Order B, Nov 6: 1 x 10.00; tax 2.10.
	// A refund of A on Nov 7: one unit of the first line (subtotal 36.00, tax 7.56) and its shipping (5.00): 48.56.
	// After it the first line's currentQuantity is 1 of 2.
	want := map[string]any{"totalSales": "81.53", "netSales": "65.99", "orders": 2, "averageOrderValue": "50.9950",
		"grossSales": "109.99", "discounts": "8.00", "refunds": "36.00", "unitsSold": 3}
	// figures equals want; the Nov 7 bucket has refunds "36.00", netSales "-36.00", unitsSold -1, orders 0 (Review Focus: below zero, never absent)
}

func TestEachRefusalIsTheOneSentence(t *testing.T)                          // a site that does not read, a static site, a system-owned one, a store it does not serve: the same sentence; a bad period: the period's own
func TestAHolderOfThePartInAnotherOrganizationIsRefused(t *testing.T)       // U5's proof: capability_not_held, nothing read
func TestACallerWithoutThePartIsRefused(t *testing.T)
func TestACallerWhoIsNotAClusterOwnerGetsFigures(t *testing.T)              // a member of the site's organization granted the part
func TestASiteBoundOnlyThroughItsPreviewBindingIsAnsweredAsTesting(t *testing.T) // and "both" when the two name one store
func TestASiteBoundUnderTheCanonicalFormOfTheStoreIdStillServesIt(t *testing.T)  // Review Focus: binding v1:shopify:store:<id>, asked with <id>, and the reverse
func TestASaleLandsOnItsStoreDayWestAndEastOfUTC(t *testing.T)              // 2025-11-08T03:00Z is the 7th in Los Angeles and the 8th in Kolkata
func TestSalesOnADaylightSavingDayLandInThatDay(t *testing.T)               // Review Focus: Santiago 2023-09-03 01:30 local and 23:30 local are one bucket; 2023-09-02 23:30 local is the day before
func TestThePreviousPeriodIsNullWhenItStartsBeforeHistory(t *testing.T)
func TestTheAverageIsNullWithNoOrders(t *testing.T)                         // a period holding only a refund: orders 0, averageOrderValue null, netSales below zero
func TestOtherCurrenciesAreListedAndNotSummed(t *testing.T)
func TestAnIncompleteOrderMakesFloors(t *testing.T)
func TestARefundNotYetReadBlanksTheFourFiguresThatSubtractIt(t *testing.T)  // D22: the four null, refundsNotRead 1, orders and grossSales still measured
func TestBuyersAreNewReturningOrUnknown(t *testing.T)                       // a buyer with three orders, one before the period: returning; a guest order: unknown
func TestWholesaleIsNullUnlessTheStoreIsEnabledAndARefundIsOnItsOrdersSide(t *testing.T)
func TestACancelledOrderAndItsRefundLeaveEveryFigure(t *testing.T)          // in the period of the order and of the refund
func TestAShippingRefundLowersTotalSalesAndNotNetSales(t *testing.T)
func TestAnEditThatRemovesAUnitChangesTheOrdersOwnDayAndNoRefund(t *testing.T)
func TestATestOrderCountsOnlyOnADevelopmentStore(t *testing.T)
func TestAStoreNotRecordingIsAnsweredAsOfWhenItStopped(t *testing.T)        // paused and needs attention: asOf is verdict_since, later buckets null; starting and rebuilding: every figure null; not known: figures, state ""
func TestAStoreWithNoZoneAnswersNoFigureAndOneWithNoCurrencyNoMoney(t *testing.T) // Review Focus
func TestTheLinesOfATombstonedOrderCountNowhere(t *testing.T)
func TestTopProductsCarryTwentyFiveRowsAndSayHowManyThereAre(t *testing.T)
func TestAGrantWithoutAllOrdersSaysOlderChangesAreNotRecorded(t *testing.T)
func TestAReadAfterAFactWriteOnTheSameNodeSeesIt(t *testing.T)              // O5: read, write an order fact, read again through the builtin: orders rose by one
func TestTheAnswerHoldsNoEmail(t *testing.T)
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/storestats/ -run 'Reads|Refusal|Holder|Caller|Bound|StoreDay|DaylightSaving|PreviousPeriod|Average|Currenc|Floors|NotYetRead|Buyers|Wholesale|Cancelled|ShippingRefund|RemovesAUnit|TestOrder|NotRecording|NoZone|Tombstoned|TopProducts|AllOrders|SameNode|NoEmail'` and `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/memql/ -run 'TestTheDeployablesPartsAreDeclaredOnTheirConstructs'`. Expected: FAIL.
- [ ] **Step 3:** Implement, then `make sdk-gen`; in `sdk/ts` run `npm run build`.
- [ ] **Step 4:** Run the same two commands, the package whole in the database lane, `make sdk-gen-check` (both clients now carry `storeStatistics`), and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `storestats: a period of a store's sales, for a holder of the sales part on a site that serves the store`.

### Task 16: Independent totals (U10)

**Independent of:** Task 17.

**Files:**
- Create: `component/storestats/reference_calc_test.go`, `component/storestats/reference_db_test.go`, `component/storestats/testdata/fixture-store/orders.json`, `component/storestats/testdata/fixture-store/README.md`

**Interfaces:**
- Consumes: the builtins `storeStatisticsRollup()` (Task 11) and `storeStatistics(siteId, storeId, period)` (Task 15), and the harness of Tasks 11 to 15; from `integrations/shopify`, only what it exports: `NewStoreRegistry(engine, secrets) *StoreRegistry`, `NewAdminClient() *AdminClient`, `NewConnector(engine, logger, stores, admin) *Connector`, `(*Connector).WithDatabase(db func() *sql.DB) *Connector` and `(*Connector).ReconcilePartition(ctx context.Context, partition, conceptName string, since time.Time) (memqlsync.ReconcileReport, error)`, asked with the store id as the partition and `generated.ConceptID("order")` as the concept.
- Produces, in `reference_calc_test.go`: `func referenceFigures(fixture []byte, zone *time.Location, currency string, development bool, granted []string, start, end time.Time, bucket string) referenceAnswer`, where `referenceAnswer` holds the eight figures, the series, both product lists, the buyers and the wholesale split as strings.

Decisions:
- The fixture is one file of orders as `ShopifyListOrder` answers them (Admin GraphQL nodes with `lineItems.nodes`, `refunds` and each refund's three connections), for a bicycle shop in EUR and `Europe/Madrid`. It holds every case the record's U10 names: two discount codes; an order-level discount; a gift card; a line refund with and without restock; a shipping refund; an amount-only refund; a refund with a restocking fee; a refund in a later period than its order; an edit that removes a unit; an edit that adds a line; a cancelled order with a refund; a test order; an order in USD; a buyer with three orders; a wholesale buyer; an order and a refund on either side of a local midnight; one order of 120 lines. The README says what each order is for and that nothing in it is a real shop's.
- The write path is the mirror's own: a fake transport on `http.DefaultTransport` answers `ShopifyListOrder` and `ShopifyPageOrderLineItems` from the file, and the connector's `ReconcilePartition` for the test's store and the order concept, with a zero `since`, pages the 120 lines, maps and heals. The shop row is a raw insert under the connector actor; the wholesale buyer's entitlement and the store's settings row are written as the pack's own tests write them. Then the rollup until it has caught up, then `storeStatistics` for every period at a fixed clock.
- The calculator is its own file. It reads the same JSON and implements section 5 of the record with `math/big` rationals, rounding half to even where 5.1 says so, and cuts days with `time` in the store's zone. It uses the standard library and nothing else: no function, type or constant of this package, and not `core/money` or `core/statsperiod`. The period's start and end it is handed are computed in the test from the clock by plain date arithmetic, not by `statsperiod`.
- The comparison is exact strings for every figure of every period, every bucket of every series, both top lists, the buyers and the wholesale split.

- [ ] **Step 1: Write the failing tests**

```go
func TestTheFiguresEqualAnIndependentCalculationToTheCent(t *testing.T) // every period; a difference names the period, the figure, both values and the orders that fall in it
func TestTheReferenceSharesNoCodeWithTheComponent(t *testing.T)         // parses reference_calc_test.go: its imports are standard library only, and every identifier it does not declare itself is a Go builtin or a name of an imported package
func TestTheFixtureHoldsEveryCaseTheRecordNames(t *testing.T)           // seventeen cases, each found by its marker in the file
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/storestats/ -run 'IndependentCalculation|ReferenceShares|FixtureHolds'`. Expected: FAIL.
- [ ] **Step 3:** Write the fixture and the calculator. A difference is a defect in the component or in the reading of section 5: settle which before changing either, and never make the two agree by copying one into the other.
- [ ] **Step 4:** Run the same command, then the package whole in the database lane, and the docs gates (the README is a tracked Markdown file). Expected: PASS.
- [ ] **Step 5: Commit** `storestats: the figures equal an independent calculation over a fixture store, to the cent`.

### Task 17: The kit (U12)

**Independent of:** Tasks 2 to 16.

**Files:**
- Modify: `clients/os/src/kit/format.ts`, `clients/os/src/kit/statistics/period.ts`, `clients/os/src/kit/statistics/TimeChart.tsx`, `clients/os/src/kit/statistics/scale.ts`, `clients/os/DESIGN.md`
- Test: `clients/os/test/kit/format.test.ts`, `clients/os/test/kit/statistics/period.test.ts`, `clients/os/test/kit/statistics/timeChart.test.tsx`, `clients/os/test/kit/statistics/scale.test.ts`

**Interfaces:**
- Produces: `formatMoney(currency: string): (value: number) => string` in `kit/format.ts`; `bucketLabel(at: string, bucket: Bucket, timeZone?: string): string` and `bucketMoment(at: string, bucket: Bucket, timeZone?: string): string`; an optional `timeZone?: string` prop on `TimeChart`, passed to both; `niceMin(min: number): number` in `scale.ts` (0 for a value at or above zero, else minus `niceMax` of its size).

Decisions:
- `formatMoney` writes through `Intl.NumberFormat("en-US", { style: "currency", currency })`, the locale `formatCount` writes in, with the currency's own decimals. A value that is not finite is "--". A code `Intl` refuses (one that is not three letters) never throws: the value is written as its count and the code as given ("1,204.5 EURO").
- With a `timeZone`, the label's parts are read with `Intl.DateTimeFormat("en-US", { timeZone, hourCycle: "h23", year: "numeric", month: "numeric", day: "numeric", hour: "numeric", minute: "numeric" }).formatToParts`, and the month is still written from the file's own `MONTHS`. A zone `Intl` refuses is read as UTC. Without a `timeZone` both functions answer exactly what they answer today.
- `TimeChart` scales from `niceMin` of its smallest value to `niceMax` of its largest. With nothing below zero the drawing is unchanged. With a value below zero the value axis gains a step below zero, the zero rule is drawn where zero falls, and the fill runs from the line to the zero rule.
- `DESIGN.md`: under "App overviews", eight figures stand as two rows of four; under "Statistics", `formatMoney` joins the formatters, a store's buckets are labelled in its time zone, and a figure that can be below zero is drawn below the zero rule.

- [ ] **Step 1: Write the failing tests**

```ts
// format.test.ts
it("writes a currency with no decimals, two and three", () => {
  expect(formatMoney("JPY")(1200)).toBe("\u00a51,200");          // the yen sign
  expect(formatMoney("EUR")(1204.5)).toBe("\u20ac1,204.50");     // the euro sign
  expect(formatMoney("KWD")(1.255)).toBe("KWD\u00a01.255");      // Intl puts a no-break space after a code
});
it("writes an amount below zero with its sign", () => expect(formatMoney("EUR")(-36)).toBe("-\u20ac36.00"));
it("writes a value that is not finite as two dashes", () => expect(formatMoney("EUR")(Number.NaN)).toBe("--"));
it("writes the count and the code for a code Intl refuses", () => {
  expect(formatMoney("EURO")(1204.5)).toBe("1,204.5 EURO");
  expect(() => formatMoney("")(1)).not.toThrow();
});
// period.test.ts
it("labels a bucket at 23:30 UTC as the next day in a +01:00 zone", () => {
  expect(bucketLabel("2025-11-08T23:30:00Z", "day", "Europe/Madrid")).toBe("Nov 9");
  expect(bucketLabel("2025-11-08T23:30:00Z", "day")).toBe("Nov 8");
});
it("labels an hour in the zone, half-hour offsets included", () => expect(bucketLabel("2025-11-08T05:30:00Z", "hour", "Asia/Kolkata")).toBe("11:00"));
it("names an hour with its day in the zone", () => expect(bucketMoment("2025-11-08T23:30:00Z", "hour", "Europe/Madrid")).toBe("Nov 9, 00:30"));
it("reads a zone it does not know as UTC");
// scale.test.ts
it("answers zero for a floor at or above zero and a round number below it", () => { expect(niceMin(0)).toBe(0); expect(niceMin(12)).toBe(0); expect(niceMin(-36)).toBe(-50); });
// timeChart.test.tsx
it("draws a bucket below zero below the zero rule, and labels a step below zero");
it("draws a series with nothing below zero exactly as before");
it("labels its buckets in the time zone it is given");
```

- [ ] **Step 2:** In `clients/os` run `npx vitest run test/kit/format.test.ts test/kit/statistics`. Expected: FAIL.
- [ ] **Step 3:** Implement, and edit `DESIGN.md`.
- [ ] **Step 4:** Run the same command, `npm run typecheck`, and `npx vitest run test/kit test/deployables/statistics.test.tsx test/email` (the pages that draw the chart today). Expected: PASS.
- [ ] **Step 5: Commit** `os: the statistics kit writes money, labels a store's own days and draws below zero`.

### Task 18: The page (U13)

**Files:**
- Create: `clients/os/src/apps/deployables/storeStatistics/read.ts`, `useStoreStatistics.ts`, `StoreStatisticsSection.tsx`, `DestinationControl.tsx`, `ProductsListPage.tsx` (same directory), `clients/os/test/deployables/storeStatistics.test.tsx`, `clients/os/test/deployables/storeStatisticsRead.test.ts`, `clients/os/qa/storeStatisticsScenes.tsx`
- Modify: `clients/os/src/apps/deployables/parts.ts`, `clients/os/src/apps/deployables/page/DeployablePage.tsx`, `clients/os/src/apps/deployables/page/DeployableWorkspace.tsx` (`WorkspaceDetail`), `clients/os/test/deployables/harness.tsx`, `clients/os/test/deployables/parts.test.tsx`, `clients/os/qa/main.tsx`

**Interfaces:**
- Consumes: the generated `query.storeStatistics({ siteId, storeId, period }, { signal })` and the answer of Task 15; Task 17's `formatMoney` and `timeZone`; `boundStoreId(site)` and `previewStoreId(site)` from `rows.ts`; the kit's `StatFigures`, `TimeChart`, `TimeChartSkeleton`, `RankedList`, `PeriodControl`, `LocalTabs`, `Select`, `Notice`, `Caption`, `AutoRefresh`, `PageHelp`; `statisticsRefreshMs` from `statistics/useSiteStatistics.ts`.
- Produces, in `parts.ts`: `"sales"` in `DEPLOYABLE_PARTS`, `NO_PARTS` and `ALL_PARTS`, and in the header comment's list.
- Produces, in `read.ts`: `interface StoreStatistics` mirroring the answer, a money figure parsed with `Number()` and a null kept null; `parseStoreStatistics(row: Row, requested: Period): StoreStatistics`; `fetchStoreStatistics(query: QueryClient, args: { siteId: string; storeId: string; period: Period }, signal?: AbortSignal): Promise<StoreStatistics | null>`; `destinationsOf(site: SiteRow): { production: string; testing: string }` (bare ids, "" when unbound).
- Produces: `useStoreStatistics({ siteId, storeId, period, enabled }): StoreStatisticsReading`, with the states and the retry rule of `useSiteStatistics`; `StoreStatisticsSection({ site, name, trail, back })`, taking the pane as `ReviewsSection` does; `DestinationControl({ value, onChange })` over `"production" | "testing"`; `WorkspaceDetail` gains `"storeStatistics"`.
- Produces, in `harness.tsx`: `storeStatistics?: Record<string, Row>` (keyed by store id) and `storeStatisticsError?: string` on `FakeSeed`, answered for `builtin storeStatistics(`; `storeStatisticsRow(over: Partial<Row> & { storeId: string }): Row`, every figure null unless named.

Decisions:
- The button: `IconButton label="Store statistics"` with the `Receipt` icon, after Statistics. Present only when `can.sales`, the site is a `shopify_storefront`, not system-owned, and either destination has a store (D15). Absent otherwise. `detailTitle` gains "Store statistics".
- The page opens on Production when it has a store, else Testing; the choice is not remembered (D13). The destination control is drawn only when both destinations name stores and the two differ; when they name one store there is none, and the context line says "Production and Testing". The period is the remembered `statisticsPeriod`, through `PeriodControl` over `SITE_PERIODS`, at the Head's right after the destination control.
- Blocks, top to bottom, spacing only between them:
  1. Context, a `Caption`: the store's name, else its domain; "Prices in EUR"; "Days in Europe/Madrid"; "Orders since Sep 12"; "Last order change recorded " and `formatFreshness`. Then, each on its own line and only when it applies: "This is a development store: every order on it is a test order, and they are counted."; "Prices on this store include tax."; "Refunds and edits of orders placed more than 60 days ago are not recorded."
  2. A `Notice`, only when one applies: for `starting` or a rebuild, "The store's orders are still being brought in."; for `paused`, `needs attention` and `not connected`, the mirror's sentences, then "These figures are as of " and `formatMoment(asOf)` and a full stop; for a state not known, "Whether this store is recording is not known."; for refunds not read, "1 refund is still being read from Shopify." or "N refunds are still being read from Shopify." Its `next` is "The Store page of this deployable says what fixes it." for the three states the mirror gives a reason for, and nothing otherwise. It holds no act.
  3. `StatFigures`, eight, in this order and with these labels: Total sales, Net sales, Orders, Average order value, Gross sales, Discounts, Refunds, Units sold. Money through `formatMoney(currency)`, counts through `formatCount`. All but the average are chartable. A previous figure is given only when `previousComparable(prevStart, historySince)`. A figure in `floors` is a floor. An absent figure carries its reason: "The store's currency has not been recorded yet.", "The store's time zone has not been recorded yet.", "The store's orders are still being brought in.", the refunds sentence above, "A refund in this period has more lines than were recorded.", or "No orders in this period." for the average.
  4. `TimeChart` of the chosen figure, with `timeZone`.
  5. `RankedList` "Products", with a `LocalTabs` control "Net sales" and "Units"; eight rows; when `productCount` is larger its footer says "The 8 best-selling of 31 products." and "All products" opens `ProductsListPage` under the trail. Further footer lines when they apply: "Refunds of 12.00 EUR name no product, so they are in Net sales and not here."; "3 orders have more lines than were recorded, so these are floors."; "1 refund has more lines than were recorded, so units are not exact."
  6. `RankedList` "Buyers": New, Returning, No customer recorded.
  7. `RankedList` "Wholesale and retail", only when `wholesale` is not null: Wholesale, Retail, with a `LocalTabs` control "Net sales" and "Orders".
  8. A `Caption`, only when `otherCurrencies` is not empty: "2 orders in USD are not in these sums."
  Each empty list says "No orders in this period."
- The figure definitions of the record's section 5 and its known differences from Shopify's reports go in a `PageHelp`, not on the page.
- Loading draws shapes (`TimeChartSkeleton`, `loading` on the figures and lists, an `InlineSkeleton` for the context line); a refresh keeps what it has; a refused or failed read is a `Notice` with the engine's sentence, kept while it is tried again, with no shape over it. `AutoRefresh` at `statisticsRefreshMs(period)`.
- The QA scenes, one view per state, registered in `qa/main.tsx`: `store-statistics`, `-development`, `-paused`, `-starting`, `-refunds-unread`, `-no-currency`, `-empty`, `-below-zero`, `-two-stores`, `-refused`.

- [ ] **Step 1: Write the failing tests**

```ts
// storeStatisticsRead.test.ts
it("keeps a null figure null and reads a money string as its number")
it("answers null for a reply with no row")
it("reads each destination's store as a bare id")
// storeStatistics.test.tsx
it("draws the button for a storefront with a store, to a holder of the sales part")
it("draws no button without the part, for a static site, for a system-owned one, and with no store on either destination")
it("draws the button when only Testing has a store, and opens on it")
it("opens on Production and offers Testing only when the two stores differ")
it("says Production and Testing when both name one store")
it("draws eight figures in order, money in the store's currency")
it("asks for the remembered period and asks again when it changes")
it("draws the development line, the tax line and the sixty-day line, each only when it applies")
it("says the store is still being brought in, and draws every figure absent")
it("gives the mirror's sentence and the moment the figures are as of, and names the Store page")
it("says whether the store is recording is not known, and still draws the figures")
it("draws the four figures a refund not yet read blanks as dashes with the reason")
it("draws money as dashes with the reason when the currency is not known")
it("draws a floor as or more, with no change")
it("draws no change when the previous period began before the orders did")
it("labels the chart's buckets in the store's time zone and leaves gaps before the orders and after as-of")
it("draws a day below zero")
it("says how many products there are and opens all of them as a page under the trail")
it("says what part of the refunds names no product, and how many orders are short of lines")
it("draws the wholesale block only when the answer has one")
it("says how many orders in another currency are not in the sums")
it("says no orders in this period, with a measured zero and a dash for the average")
it("keeps a refusal standing while its retry is out, and draws no shape over it")
it("draws no divider between rows and no pill, badge or toast")
// parts.test.tsx
it("judges the sales part per organization")
```

- [ ] **Step 2:** In `clients/os` run `npx vitest run test/deployables/storeStatistics.test.tsx test/deployables/storeStatisticsRead.test.ts test/deployables/parts.test.tsx`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, `npm run typecheck` and `npx vitest run test/deployables`. Expected: PASS. Open every QA scene in both themes and at both widths (`clients/os/qa/README.md`) and look at each: no column collides, the eight figures stand as two rows of four and as four rows of two, the below-zero day is under the zero rule.
- [ ] **Step 5: Commit** `os: a storefront's Store statistics page, for holders of the sales part`.

### Task 19: Documentation (U14)

**Files:**
- Modify: `docs/public/operate/deployables.md`, `docs/public/operate/shopify-connector.md`

Content, each under the page's existing headings where one fits:
- `deployables.md`, in "The Shopify storefront": the Store statistics page and who sees its button; the sales part, that owner and developer hold it by default, and how a client's group is granted it (`allow execute app:deployables/sales` beside `allow read app:deployables`); the destination switch and that it is not remembered; what the context lines say, the 60-day line included; what "as of" means and when every figure is absent.
- `shopify-connector.md`: the refund detail the mirror now records and that it makes this release roll-forward only; that a store's shop row is read at connect, at boot and daily; that a store's first order sweep under this version reads every order; what is computed from the mirror, with the figure definitions of the record's section 5 in plain words; the known differences from Shopify's reports (5.5); the limits of section 10, among them refunds and edits of orders older than 60 days and refunds whose detail cannot be read; the walk and its minute; the check, what `clean_at` and `mismatched` mean, and what `disagreeing` is for; how a cluster owner runs `storeStatisticsRebuild` and what the page shows meanwhile; that the part is granted to a client's group only after a clean check and the production walk.
- The `allowlist.yaml` header and `clients/os/DESIGN.md` were written in Tasks 2 and 17.

- [ ] **Step 1:** Run the docs gates of the Global Constraints before the edit. Expected: PASS.
- [ ] **Step 2:** Write the two pages. No registration function in its qualified form, no make command that is not a target, no store, person or hostname.
- [ ] **Step 3:** Run the docs gates again. Expected: PASS.
- [ ] **Step 4: Commit** `docs: the Store statistics page, the sales part, and what is computed from the mirror`.

### Task 20: The whole tree

**Files:** none changed, unless a gate names one.

- [ ] **Step 1:** `docker exec memql-testdb createdb -U memql memql_stats_final`.
- [ ] **Step 2:** `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN='postgres://memql:memql_dev@127.0.0.1:55432/memql_stats_final?sslmode=disable' make test`. Expected: PASS. The two known flakes on a busy machine are `integrations/email` `TestAzureFeedbackDiscoversPreviouslySavedOrganization` and `component/packages` `TestSynchronousConfirmationSurvivesItsCallerAndTheSweep`; run either again alone before calling it a failure.
- [ ] **Step 3:** On the same fresh database, with `MEMQL_REQUIRE_DB=1` and `-p 1`, each database-gated tree this plan touched: `./component/storestats/`, `./integrations/shopify/`, `./component/datasync/`, `./component/memql/`, `./component/auth/`, `./component/automations/...`, `./component/database/...`, `./packs/wholesalepack/`, and `./app/` with `-tags bff`. Then `./core/statsperiod/ ./core/money/ ./cmd/shopifyschema/ ./test/dslconformance/... ./scripts/cidb/ ./scripts/ci/`. Expected: PASS.
- [ ] **Step 4:** `make sdk-gen-check`, `make concept-snapshot-check`, `make platform-graph-check`, `make dsl-lint`, and the docs gates. Expected: PASS.
- [ ] **Step 5:** In `clients/os`: `npm run typecheck`, `npm test`, `npm run build`. Expected: PASS.
- [ ] **Step 6:** Commit anything a gate made you regenerate, as `build: regenerate after the store statistics change`.
