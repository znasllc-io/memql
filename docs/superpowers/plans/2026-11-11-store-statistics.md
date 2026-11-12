# Store statistics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Store statistics page on a Shopify storefront deployable shows the store's own sales figures for a chosen period, computed with exact arithmetic from fact tables the engine fills from the orders, lines and refunds it mirrors, for the people who hold a new sales part.

**Architecture:** The mirror gains each refund's detail and each store's shop row. A new component, `component/storestats`, walks every store's mirrored orders, lines and refunds through server-only queries each minute and writes facts to its own tables; a retired row is kept as a fact marked deleted, so no older version can bring it back. An hourly check walks the rows again, recomputes each fact from the row in hand and repairs what differs in version or in content, which is also how a customer's erasure reaches the facts. One client-callable builtin, gated by the part `app:deployables/sales` and by the site's binding, answers the figures with SQL over those tables. MemQL OS draws the page from the statistics kit, which gains a money formatter, time-zone labels and a value axis that reaches below zero.

**Tech Stack:** Go (`cmd/shopifyschema`, `integrations/shopify`, `core/statsperiod`, `core/money`, `component/storestats`, `component/auth`, `component/memql`), SQL migrations, MemQL DSL (`dsl/shopify/overlay`, `dsl/rbac`, `packs/wholesalepack/dsl`), React and TypeScript (`clients/os`), Vitest.

**Spec:** `docs/superpowers/specs/2026-11-11-store-statistics-design.md`, called "the record" below. Executors read both. The record's production walk (11.3), its live checks other than L11 and L16, and its rollout (13) are operator steps on the owner's instance and are not tasks here. The record's live server test (11.1, an order and its refund reaching the facts within two minutes) is held in two halves: Task 2's live tests hold what Shopify answers for a refund, and steps P4 and P5 of the production walk hold the whole chain.

## For the owner

Nothing here changes the plan. Each is a consequence of a decision already made, said once so it is decided knowingly.

- The sales part is on the cluster's owner and developer roles by default, as the reviews part is. The cluster's own developers can therefore see every client's sales.
- A holder of the sales part reads, on the page, the mirror's own sentences for why a store is not recording, without holding the store part.
- Money is written in the `en-US` manner (grouping by commas, a point for decimals, the currency's symbol or code first) for every store, whatever the store's country.
- A store that changed its currency shows the money of the orders in its present currency only; the older orders are counted and named, never summed. A figure in each order's own currency is out of this version.
- The OS draws a part's control only for a person who, in the site's organization, also holds `read data`, `read app:deployables` and `update data`: `partsForOrganization` judges every part of the Deployables app that way, the reviews part included. A client's group that may only read would be answered by the server and shown no button.
- The sentences this plan invents, for wording:
  - "The store's time zone has not been recorded yet."
  - "The Store page of this deployable says what fixes it."
  - "1 refund is still being read from Shopify." (the singular of the record's "N refunds are still being read from Shopify.")
  - "A refund in this period has more lines than were recorded."
  - "1 refund has more lines than were recorded, so units are not exact." and "N refunds have more lines than were recorded, so units are not exact."
  - "Refunds, edits and cancellations of orders placed more than 60 days ago are not recorded."
  - "Refunds of €12.00 name no product, so they are in Net sales and not here." (the amount written as every other amount on the page)
  - The refusals: "store statistics rollup requires the engine maintenance principal", "store statistics check requires the engine maintenance principal", "store statistics rebuild is for a cluster owner", "store statistics check state is for a cluster owner", "Store statistics has no store with that id."
  - The seeds' description of the part: "the sales part of the Deployables app -- a storefront's sales figures, on its Store statistics page."

## Amendments to the record

A1 to A13 were made with the first commit of this plan. A14 to A25 were made after the plan's review, in the commit that corrected it; where a later amendment changes an earlier one, the earlier row says what holds now. A26 to A33 were made when the plan was checked against the tree it will be built on: the finished store mirror with engine main merged in (`feat/store-mirror` at `f5df0fb3f`, main at `24e8dab2b`, engine 0.31.35). No decision of the owner's (S1 to S14) changed.

| # | Where in the record | What it says now |
| --- | --- | --- |
| A1 | F8, F22, 6.1, section 10 ("An order with more lines than the mirror fetches", "The verdict cannot be read") | The mirror reads an order's lines past the first hundred (`integrations/shopify/children.go`) and the verdict builtin exists (`integrations/shopify/verdict_capabilities.go`). The `lines_complete` guard stays. An inlined list on a refund is read one page deep and is not paged. The row about a verdict "not yet available" is now about a verdict the walk could not read. |
| A2 | 6.2, 6.3, 6.5, section 8, section 10 ("An order placed more than sixty days ago") | The app does not hold `read_all_orders`: a refund, an edit or a cancellation of an order placed more than 60 days ago is not recorded. The answer carries `olderOrderChangesNotRecorded`, kept as `all_orders_readable` on `store_sales_store` from the store row's `scopesGranted`; the page's line is A22's. |
| A3 | 6.2, 6.3, section 9 (U7, U8), section 10 ("Several engine nodes", "A tombstoned order") | A fact's version is its mirror row's own write time, not the row's `updatedAt`; a tombstone is recognised by `deleted` alone; an order's tombstone does not retire its lines or refunds, whose facts stay and count nowhere. What a tombstone does to its own fact is A14's. |
| A4 | Section 10 ("A paused store") | A paused store keeps the deliveries that arrive while it is paused and applies them on resume. |
| A5 | Section 14: O3, O4, O6 | O6 settled: the component asks `PackDomainDisabled` in package `dsl`. O3 and O4 answered from the code, each still held by its test. |
| A6 | F12, D22, 6.1, section 10 ("A refund whose detail is not fetched") | A list a document did not select is written empty by the mapper, not left absent, and only a store's first sweep reads every order. So an inlined connection is a field kind of its own that the mapper leaves out when it was not read, and every order is read once more for the refunds mirrored before (A23). A refund of an order Shopify no longer shows never gets its detail. |
| A7 | F13, section 3, 6.1, section 10 ("A store whose shop never changed") | The shop row is written only when a `SHOP_UPDATE` delivery arrives. The mirror now reads it at connect, at boot when missing, daily, and when a store is resumed. |
| A8 | 6.2 | New columns: `checked_at` on the three walked fact tables; `store_domain`, `store_name`, `taxes_included` and `all_orders_readable` on `store_sales_store`; `window_seconds` and `walked_through` on `store_sales_watermark`, which has no `entitlement` source. `verdict_since` is when the store left `recording`. `tax_refunded` is stored as Shopify wrote it. Derived columns are recomputed in the page's own transaction, a line's in Go with U3. A24 adds to these. |
| A9 | 6.3, 6.4 | Every walked shape projects `row.id` and `row.createdAt`; three shapes are added for the buyers, the shop and the store. The check reads the three `...Changed` queries from the start: the version shape and the three `...Versions` queries are gone. The wholesale reads take every form of a store id, the settings read is a query of its own, and `wholesaleStatisticsChanged` is gone. A binding's store id is compared bare. |
| A10 | 6.5, section 8, section 9 (U12), section 10 ("A refund with more than 50 lines") | The answer gains `storeName`, `storeDomain` and `incompleteRefunds`; a refund holding a full page of lines makes Units sold absent, not a floor. The chart draws a value below zero. Money is written in `en-US`, the locale `formatCount` writes in. Buckets are cut from the bounds Go computes, not with `AT TIME ZONE`. New copy: "The store's time zone has not been recorded yet.", "The Store page of this deployable says what fixes it.", "1 refund has more lines than were recorded, so units are not exact."; the freshness example reads as `formatFreshness` writes it. |
| A11 | 6.2, section 9 (U7), section 10 ("A long history") | The past is read in bounded windows, each as of its end, halved when a page outlasts a read. Where the past begins is A19's. |
| A12 | D20 | The OS words a part by the last segment of its resource ("sales"); "sales figures" is the seeds' description. |
| A13 | Approvals, section 9 | A paragraph naming what was amended, a note that this plan splits and reorders the units, and the unit rows brought in line. |
| A14 | 6.2, section 9 (U7, U8), section 10 ("A tombstoned order", "Several engine nodes"), 11.1 | A retired row's fact is kept, marked `deleted`, with the tombstone's version: the guarded write then refuses every older version, whichever node or window brings it, and a check pass over a deleted order finds nothing to repair. Every figure and every join leaves deleted facts out; the end-of-pass delete of facts no row answers for leaves them alone. A deleted refund's lines are removed with it, since they have no version of their own. The line fact's key is the store and the line, so a tombstone that carries no parent still finds its fact. |
| A15 | 5.1, 5.3, 5.5, section 9 (U3), 11.3 (L16) | A computed amount is rounded half to even at the larger of the operand's own scale and the currency's minor units, from a table of exponents compiled into `core/money`: Shopify writes a whole amount with one decimal, and the written scale alone would round a share to tenths. The average order value divides the store-currency sales by the store-currency orders and is answered already rounded at the currency's minor units. A live check, L16, records the scale Shopify writes. |
| A16 | D21, 6.1, section 9 (U1), section 12 | The `inline` option names the fields it keeps and never takes a node's full selection: only amounts, counts, moments, enums and references can be named. The refund's shipping lines keep their two amounts and no nested shipping line, which would have copied the buyer's phone number at the delivery address. The three lists go on the erasure's keep-list with their reasons, the erasure's golden file is regenerated, and the generator sets each new field's Admin type and the references it holds. The page an inlined connection is read with is carried on the generated field, its one owner. |
| A17 | D17, section 9 (U8), section 10 ("A customer's data is redacted", "A write the walk missed"), 11.1 | An erasure rewrites a row in place and keeps its write time, so the walk never sees it. The check therefore recomputes each fact from the row in hand and compares its content, not only its version; that is how an erasure, and any other change in place, reaches the facts within one check pass. D17 says what a fact holds (the buyer's Shopify customer id, a line's title) and what an erasure does to each. A gate in `component/storestats` fails when a field the facts read is one the erasure would scrub, the email apart. |
| A18 | 6.3, 11.3 (P1), section 13 | A builtin for a cluster owner, `storeStatisticsCheckState`, answers when the last check pass began and ended, when one last ended clean, and its counts. The production walk's first step and the rollout's third read it. |
| A19 | 6.2, 6.3, section 10 ("A long history") | No date is compiled in. A store's history begins at an instant before its store row was first written, found once by asking the engine for the store row as of earlier and earlier instants (`storeStatisticsStoreAt`), and kept as `history_floor`. A row's own `createdAt` cannot say it: it is the write time of the row's newest version. |
| A20 | 6.2, 6.3, section 9 (U7, U8), section 10 ("Several engine nodes", "A rebuild while a walk or a check runs"), 11.3 (P15) | The rebuild is run by a person who is a cluster owner (a maintenance principal is refused), compares the store id bare and refuses a store it holds no row for. `store_sales_store` carries a `generation` the rebuild raises: every page and check transaction reads it under the lock and writes nothing when it is not the one read before its page was asked. Every transaction that writes a `store_sales_*` table begins by taking the one write lock. A rebuild's delete, a removed store's delete and the check's end-of-pass delete run in batches. |
| A21 | 5.2, 11.3 (L4, L12, L13) | The refund formulas state what they assume: on a store whose prices include tax a refunded shipping line's subtotal holds its tax, and an order adjustment's amounts carry their sign and can be negative. Both are live checks, and the plan names the fixture orders and the tests that change if either is wrong. |
| A22 | Section 8, section 10 ("An order placed more than sixty days ago") | The page's line reads "Refunds, edits and cancellations of orders placed more than 60 days ago are not recorded." Every amount the page writes goes through `formatMoney`, the products footer included: "Refunds of €12.00 name no product, so they are in Net sales and not here." |
| A23 | 6.1, section 9 (U1), section 10 ("A refund whose detail is not fetched", "An order placed more than sixty days ago", "A paused store") | The mirror's rule is named: `emptyAnswerRetires` and `readUnderOrders` in `integrations/shopify/apply.go` keep an answer of nothing from retiring an order, or what hangs from one, under a grant without `read_all_orders`. The one read of every order is stamped only when it ended: a sweep in which a write failed is not stamped, a read cut at its row budget is not stamped and goes on from its cursor at the next sweep, and a paused store's sweep is skipped with nothing stamped. |
| A24 | 6.2, 6.5 | `store_sales_check` holds the pass under way (its start, a cursor and a learned window width for each source, running counts) and the last pass that ended (its start, its end, its counts) and no column nothing writes. The wholesale pass keeps its learned width too. `history_since` is the store-local midnight of the first recorded day. Each index names its reader: the index by product is gone and one by order serves the lines of an order. The scale test runs in the lane at a tenth of its size and on demand in full. |
| A25 | Section 9 | The note on how the plan splits the units is brought in line: the walk is five tasks, the read two and the page two. |
| A26 | 6.2, section 9 (U4) | The migration is `20261112110100_store_sales_facts`. Main now holds `20261112100000_account_ownership` (`component/database/memory-nodes/migrations/`) and the Go migration `20261112110000_account_index` (`component/database/`, registered in `timescaledb.go`), both after the minute the record named, and the plan's own rule (Task 1) takes the first free minute after the newest. Record corrected. |
| A27 | 6.2 (last paragraph), 6.3 | Every row now records the account it belongs to (accounts design record 2026-11-12). Every concept declares `@belongsTo`, a build gate holds `dsl/` and `packs/` to it, and the engine fills `accountId` on an engine-managed concept's rows. This work adds NO concept: the seven `store_sales_*` tables are side tables outside the row table, as the email statistics' are, and the answer rides a synthetic `integration:storeStats:statistics` node. So nothing here declares `@belongsTo`, the stamp never reaches a fact, and the Accounts app's inventory, which counts node-table rows of concepts that declare an account form and words them by namespace, gains no row and no area. A fact's account is its store's: every fact is keyed by `store_id`, and the mirror rows it is computed from declare `@belongsTo(account, via="storeId", parent="v1:shopify:store")` (the accounts record's section I, "all 65 mirror concepts"). An export of one account classifies data held outside the row table in its own piece (the accounts record's section N). Whether the store's account is also copied onto `store_sales_store` is open (pre-flight, "Needs a ruling"). The refund's three new fields join a concept that already declares its form. Record corrected. |
| A28 | F5, F16, F22 | The email facts' migration is under `component/database/memory-nodes/migrations/`. The Deployables page's Reviews pane and button moved to `DeployablePage.tsx:456` and `:546`, and Statistics is drawn there only for a site that is not a draft. The verdict builtin is gated by `@requiresCapability("execute", "app:deployables/store")` as well as its handler's check; the walk's internal origin passes the first and its maintenance principal, a cluster owner, the second. Record corrected. |
| A29 | D17, section 10 ("A customer's data is redacted") | Read on the finished erasure. Its exported read is `ErasureDecision(concept, field string) string`, answering `kept by kind`, `kept by the list`, `kept, scrubbed on a custom item`, `kept as structure`, `scrubbed` or `no such field`; the structural fields are `storeId`, `gid`, `updatedAt`, `versionSource`, `syncedAt`, `versionReadAt`, `deleted`, `parentGid` and now `accountId`. Task 10's gate asks that read instead of keeping a list of its own, and admits a line's `title` as `kept, scrubbed on a custom item`, which the check handles. The connector's erasure is `RedactCustomer(ctx, storeID, customerGID string, orderGIDs []string) (ErasureResult, error)` and runs only on the engine's maintenance session (`WithMaintenanceSession`), so Task 18's test gives it one; the erasure's marker is the exported `RedactionMarker`. The plan's earlier statement that the erasure was still being written at `caaecb486` is withdrawn. |
| A30 | none (the plan's own) | The plan said `component/storestats` runs in the database lane's default class without saying why. CI gives every database-gated tree a per-package alarm (`scripts/ci/db-test-budgets.yaml`): the default class is 180 seconds, on a runner about 1.9 times slower than a laptop. Task 19 now measures the whole package with the lane's flags and gives it a class of its own when the default would not hold it with the headroom the file's other classes keep, and Task 24 measures the touched trees that already have classes. |
| A31 | none (the plan's own) | Names read on the branch: a paused store's sweep answers `memqlsync.Busy(...)`, which wraps `memqlsync.ErrBusy` and is recognised by `memqlsync.IsBusy` (Task 3). Task 1's records are now known and written in (Task 1, Step 4), each still checked. |
| A32 | none (the plan's own) | Disagreements inside the plan, fixed: Task 16 adds to the test harness and now lists it among its files, and no gate reads a test file's hand-written statement (`staged_read_site_classification_test.go` does not sweep `_test.go` files); Task 9's shape test no longer says "no name" of shapes that project the shop's and the store's names; Task 22's week fixture carries the units of the week it says it is. |
| A33 | D16, section 10 ("The verdict cannot be read") | Not changed, and open: on the finished mirror a store reads `needs attention` for any reason, a privacy request failing for more than a day among them (`privacy_request_failing`, which the mirror says "is not about recording"), and for reasons about domains that do not feed the figures. D16 draws the figures as of when the store left `recording` in every such case. Whether it should is a ruling (pre-flight, "Needs a ruling"); until it is given, Tasks 14, 17 and 22 do what D16 says. |

## How the tasks follow the record's units

| Record | Tasks | Why it departs, where it does |
| --- | --- | --- |
| U1 | 2, 3, 4 | Three reviewable changes to the mirror: the refund's detail, the detail of refunds mirrored before it, and the shop row the figures cannot be cut without. |
| U2, U3, U4, U5 | 5, 6, 7, 8 | As the record. U4's test stands beside the other migration round trips, so it shares no package with the component. |
| U6 | 9, and the builtins in 11, 16, 17 | Each builtin, automation and principal is declared in the task that gives it a body, so no task leaves a builtin whose handler does nothing. |
| U7 | 10, 11, 12, 13, 14, 15 | Too large for one review as one task or as three: the pure rules that turn a row into a fact; the provider with its builtin, automation, principal and gates; the windowed walk; the fold and the derivation; what the walk keeps about each store; the wholesale judgement. A reviewer can reject one and accept its neighbour. |
| U8 | 16 | As the record, with the builtin that says whether a pass ended clean. |
| U9, U11 | 17, 18 | The gate, the answer's frame and the SDK first, since `make sdk-gen-check` fails the moment an `@sdk` builtin exists without its builders; then the figures. |
| U10 | 19 | As the record. |
| U12 | 20 | As the record. |
| U13 | 21, 22 | The part, the button and the read first; then the figures, the chart, the lists and the states. |
| U14 | 23 | As the record. |

## Global Constraints

- Work only in this worktree, on branch `feat/store-statistics`. Every path in this plan is relative to the repository root. Never touch the main checkout or another worktree. Task 1 rebases once; after it, never rebase, stash, reset, switch branch or push.
- Every task's tests fail first (record the command and the failing output), then pass.
- The mirror's code is cited as it stands on its finished branch with engine main merged in (`feat/store-mirror` at `f5df0fb3f`, main at `24e8dab2b`), the customer's erasure included (`integrations/shopify/erasure_fields.go`, its golden file `integrations/shopify/testdata/erasure-fields.golden`, and the generator's `AdminType` and `GidsInside` on every field). Where a function this plan names differs on main when this work begins, main is right: do what the task intends against the code as it stands, and report the difference. Task 1 checks each name this plan consumes.
- Every `go test` carries `MEMQL_DATABASE_DSN="postgres://memql:memql_dev@127.0.0.1:55432/memql_stats_a?sslmode=disable"`, written `$STATS_DSN` below (Task 1 exports it). Database-lane tests add `MEMQL_REQUIRE_DB=1` and `-p 1`. Never port 5432. Tests under `./app/` need `-tags bff`. Name packages; a bare `go test ./...` at the root misses the engine and is not a verification. The whole tree is `make test`.
- No file under `integrations/` imports `component/datasync`: the integrations module cannot see the root module. A test of the connector that needs the sync runtime lives in `component/datasync`. `scripts/ci/module-boundaries.sh integrations` holds it.
- Generated files are regenerated, never hand-edited: `sdk/go/client/generated_*`, `sdk/ts/src/client/generated_*`, `integrations/shopify/generated/*`, `dsl/shopify/generated/*`, the concept snapshot, the platform graph, the automation corpus, the erasure's golden file. After any `.memql` change: `make sdk-gen`; `make concept-snapshot` when a concept gained a field; `make platform-graph` when a concept or an automation changed. Then `make sdk-gen-check`, `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 -run 'TestConceptFieldSnapshotIsNotStale|TestPlatformGraph' .`, `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 ./test/dslconformance/...` and `make dsl-lint`. If a gate names another stale file, run the make target or the `-update` its message names.
- The docs gates: `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 -run 'TestDocs|TestRetiredVocabulary|TestPluginMeansPackVocabulary|TestMakeTargetCitationsNameRealTargets|TestEngineIsProductNeutral|TestOnlyAllowlistedPackagesStampInternalOrigin' .`
- A tracked Markdown file must not name a registration function in its qualified form (a package or receiver name, a dot, then a name that begins with Register) unless Go declares that name: a docs gate fails on it. Write a new registration in prose. A backtick-quoted make command must name a real target.
- The repository is public. No client, person, real store domain, instance hostname or a client's trade in code, tests, fixtures, docs or commits. Write "a development store" and "the owner's instance". Where a `myshopify.com` domain is needed, use the tree's `acme-widgets.myshopify.com`; everything else a fixture invents is a bicycle shop's (a city bike, an inner tube, a bell), and a buyer is `buyer-1@example.test`.
- The engine has exactly three extension words: component, integration, pack. Never "plugin" in prose. No emojis.
- Engine facts, established on the other branches:
  - A builtin with no gate of its own can be called by any signed-in user. `@sdk` only marks it for the generated clients, and there is no default rank floor. Every builtin here checks its caller in Go before it reads anything.
  - Internal origin escapes the row-authorization write guard, `@serverOnly` and `@requiresCapability`. It is stamped in one place in `component/storestats` and only for the two scheduled passes.
  - An automation's system actor is a reader. A pass that must read every store runs under its own maintenance principal.
  - A raw insert read-merges: a field a write omits keeps its stored value.
  - A row is stored as versions, and a read answers the newest. `row.createdAt` is that version's write time: it moves on every write, and nothing on a row says when the row was first written. It comes back from the engine as a time value, not a string; read it through `instantOf`.
  - A customer's erasure rewrites every version of a row in place and keeps each version's write time. No read that asks for "what was written since" sees it.
  - Scheduled automations can overlap across nodes (`@mode(single)` is process-local). Every pass is safe to run twice and at once.
  - A schema refuses unknown stored keys on a row's next write, so a release that adds a concept field is roll-forward only.
  - Every concept declares which account its rows belong to (`@belongsTo`; `TestEveryShippedConceptDeclaresBelongsTo` in `test/dslconformance` fails the build on one in `dsl/` or `packs/` that does not), and the engine fills `accountId` on an engine-managed concept's rows behind a one-way switch that tests run with off (`SetAccountStampingEnabled(true)` turns it on in a test that wants it). A mirror row's `accountId` is its store's, filled by the engine; the connector never writes it, and an erasure keeps it as structure. This plan adds no concept (A27): its tables are side tables, its shapes and queries sit on concepts that already declare (the mirror's and the wholesale pack's), its builtins and automations are not concepts, and nothing here reads or writes `accountId`.
- Money is exact. An amount is a `money.Amount` in Go and `NUMERIC` in SQL, passed as text and cast with `::numeric`; never a `float64`. Shop money only: `presentmentMoney` is never read. A computed amount is rounded half to even at the larger of its operand's own scale and the currency's minor units (`money.RoundingScale`), never at the written scale alone: Shopify writes a whole amount as `35.0`.
- A fact is never removed because its mirror row was retired: it is kept, marked `deleted`, with the tombstone's version. Every statement that reads facts for a figure, a join or a derivation leaves `deleted` facts out.
- Every transaction that writes a `store_sales_*` table is opened by `writeTx`, which takes the write lock first and then compares the store's generation. Nothing else in `component/storestats` opens a writing transaction.
- Every id in a `store_sales_*` table is bare. A store id is compared through `memql.BareShortId`; a wholesale row may hold either `<id>` or `v1:shopify:store:<id>`, and a read that asks one form misses rows.
- Facts hold no name, email, address or phone. An order's `email` is read in memory for the wholesale judgement and written nowhere, a table's JSON column and a log line included.
- Exact values: the part `app:deployables/sales`; the provider name `storeStats`; the principals `system:maintenance:storeStatisticsRollupSweep` and `system:maintenance:storeStatisticsCheckSweep`; schedules (six-field cron) walk `15 * * * * *`, check `0 40 * * * *`; a walk page is 250 rows, at most 40 pages of a source for a store in a run, read from its watermark less 5 minutes; a refund's inlined pages are 50, 10 and 10, owned by the allowlist; a top list carries 25 rows; `storeStatsDefinition` is 1; a batched delete takes 5,000 rows a transaction.
- The read's one refusal sentence, for a site that does not read, is not a storefront, is system-owned or does not serve the store: "This deployable does not serve that store for you."
- The page's line about older orders, exactly: `Refunds, edits and cancellations of orders placed more than 60 days ago are not recorded.`
- Go renders a named call's string arguments with `langparser.QuoteString`, never `%q`. A number read from a row is narrowed through `core/num`.
- No model or agent is called on any of these paths, and nothing is sent to anyone.
- OS UI follows `clients/os/DESIGN.md`: kit components only, no dividers between rows, no pill, badge or chip in a row, `Notice` and never a toast, a control whose part is not held is absent and not disabled, an unmeasured value is a dash with its reason and never a zero. Each screen is checked in both themes and at both widths. Every amount the page writes goes through `formatMoney`.
- Commits are `<area>: <sentence>`, staged by explicit path (never `git add -A`), ending with `Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>`.

## Review Focus

- A connected store whose shop settings never changed since it connected has no mirrored shop row, so no currency and no time zone: every figure would be a dash for good. A person expects figures once the store is connected. Tests in Task 4 and Task 18.
- A day on which more was refunded than sold, or a refund in a period with no order: net sales, total sales and units are below zero, and the kit's chart scales from zero up. A person expects the day drawn below the zero line and the average shown as a dash. Tests in Task 20 and Task 18.
- A refund the mirror wrote without its detail (by a bulk line, or before this version): its split into items, shipping and tax is unknown, and a bulk line must never blank detail a fetch recorded. A person expects no wrong Refunds figure and a notice that clears by itself. Tests in Task 2, Task 3 and Task 13.
- A store in a zone whose clocks change, above all one whose clocks skip midnight (Santiago) or repeat an hour: Go's own date constructor answers the evening before for a skipped midnight. A person expects each sale on its own local day, once, and both of a repeated hour's buckets. Tests in Task 5, Task 18 and Task 20.
- One store id written two ways, bare in one row and `v1:shopify:store:<id>` in another: in a site's binding and in the wholesale pack's rows. A person expects the page to open and a wholesale buyer to be found whichever form each row holds. Tests in Task 15 and Task 17.
- An order deleted in the store. Its row is retired in the mirror, and every older version of it is still there to be read again by a later window, another node or the next check. A person expects the order gone from every figure for good and the check to end clean. Tests in Task 13, Task 16 and Task 18.
- An amount Shopify writes with one decimal (`1.0`), shared out over the units an edit left. A person expects cents, not tenths. Tests in Task 6, Task 10, Task 13 and Task 19.
- A customer who asked to be erased. The mirror scrubs their rows in place, where no walk looks. A person expects nothing the erasure removed to remain in a fact after the next check, and the order's money to stay in the figures. Tests in Task 2, Task 10, Task 16 and Task 18.

A task's **Independent of** line names the tasks it shares no file and no Go package with and whose code it does not need, in either direction. Two such tasks can be done in either order. Doing two at once is the controller's call: they share one working tree and one test database.

---

### Task 1: Workspace and baseline

**Files:** none changed.

- [ ] **Step 1:** Confirm the store mirror and its erasure have landed: `git fetch origin main`, then `git cat-file -e` for each of `origin/main:integrations/shopify/verdict_capabilities.go`, `origin/main:component/memql/sync/version.go`, `origin/main:integrations/shopify/resume.go`, `origin/main:integrations/shopify/erasure_fields.go`, `origin/main:integrations/shopify/testdata/erasure-fields.golden` and `origin/main:test/dslconformance/belongs_to_inventory_test.go` (the accounts piece, which this plan was checked against). If any is missing, stop: this plan builds on that branch and the controller decides.
- [ ] **Step 2:** Confirm on main, by name, the rule that an answer of nothing never retires an order, or what hangs from one, under a grant without `read_all_orders`: `git grep -nE 'func (emptyAnswerRetires|readUnderOrders)\(' origin/main -- integrations/shopify/apply.go` names both functions. If it does not, stop: main does not hold the mirror this plan was written against.
- [ ] **Step 3:** `git rebase origin/main` on `feat/store-statistics`. The branch carries only documents, so a conflict is a document's; resolve it by keeping both texts and stop if anything else conflicts.
- [ ] **Step 4:** Check on main, and record in the controller's ledger, each of these. The value given is what the store mirror's finished branch holds (A26 to A31); a difference is reported and main is right.
  1. The newest file in `component/database/memory-nodes/migrations/` and the newest dated `.go` migration in `component/database/`. On the branch they are `20261112100000_account_ownership.up.sql` and `20261112110000_account_index.go`, so Task 7's files are `20261112110100_store_sales_facts.{up,down}.sql`; if a newer one has landed, Task 7 takes the first free minute after it.
  2. The erasure's exported, read-only decision for a concept and a field (`git grep -nE '^func [A-Z]' origin/main -- integrations/shopify/erasure_fields.go`). On the branch: `ErasureDecision(concept, field string) string`, which takes a concept by name or id and answers `kept by kind`, `kept by the list`, `kept, scrubbed on a custom item`, `kept as structure`, `scrubbed`, or `no such field` for a concept or field the generated model does not hold (the store and the shop's own rows are outside Task 10's gate by name, so that answer is never what the gate relies on). Task 10's gate calls it. If there is none, stop: that gate cannot be written without it.
  3. The connector's exported erasure of a customer and its signature, which Task 18's database test runs: on the branch `(*Connector).RedactCustomer(ctx context.Context, storeID, customerGID string, orderGIDs []string) (ErasureResult, error)`, which refuses to run without the engine's maintenance session (`(*Connector).WithMaintenanceSession(open func(context.Context) (*sql.Conn, func(), error))`; a test opens one with `database.OpenMaintenanceSessionDSN(ctx, dsn)`), and the marker it leaves in free text, the exported `RedactionMarker` (`"[redacted]"`). And the names of the erasure's keep-list and of its rule for a field's kind and Admin type, which Task 2 adds to and tests against: `erasureKeepList` and `erasureKeptByKind(f generated.FieldSpec) (string, bool)`.
  4. The generator's signatures, which Task 2 adds to: `func (p *Planner) planField(f *Field, isRef bool) (FieldPlan, bool, error)` and `func (p *Planner) objectSelection(t *Type, depth int, kept map[string]bool) string`; `FieldPlan` and the generated `FieldSpec` carry `AdminType` and `GidsInside`.
  5. The functions of `integrations/shopify/resume.go` that run when a store is resumed, which Task 4 adds to: `resumeStore(ctx context.Context, store Store)`, which the Store page's Resume act calls, and `resumeSeenByWatch(ctx context.Context, store Store, previous map[string]any, v Verdict) error`, which the store watch calls when it sees a store leave its pause.
  6. `packs/wholesalepack/dsl/concepts.memql`: `entitlement` declares `accountId` and `@belongsTo(account, via="applicationId", parent="v1:wholesale:application")`, and `wholesaleSettings` `@belongsTo(account, via="storeId", parent="v1:shopify:store")`. The fields this plan reads (`entitlement.storeId`, `buyerEmail`, `state`, and `wholesaleSettings.storeId`) are there either way.
- [ ] **Step 5:** Build what git ignores: `make identity-tailwind`; in `sdk/ts` run `npm ci && npm run build`; in `clients/os` run `npm ci`.
- [ ] **Step 6:** `docker exec memql-testdb createdb -U memql memql_stats_a`, then `export STATS_DSN='postgres://memql:memql_dev@127.0.0.1:55432/memql_stats_a?sslmode=disable'`.
- [ ] **Step 7:** Baseline, all expected PASS: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./cmd/shopifyschema/ ./integrations/shopify/ ./component/datasync/ ./component/emailstats/ ./core/statsperiod/ ./packs/wholesalepack/ ./component/database/memory-nodes/`; `scripts/ci/module-boundaries.sh integrations`; the generated-file gates and the docs gates of the Global Constraints; in `clients/os`, `npm run typecheck` and `npx vitest run test/deployables test/kit`. Record any failure that exists before a change and stop: a dirty baseline is the controller's call.

### Task 2: Refund detail in the mirror (U1)

**Independent of:** Tasks 5, 6, 7, 8, 20.

**Files:**
- Modify: `cmd/shopifyschema/allowlist.go`, `cmd/shopifyschema/allowlist.yaml` (the header comment and the `Refund` entry), `cmd/shopifyschema/mapping.go`, `cmd/shopifyschema/emit_go.go` (the kind constants and the field literal it writes into the model), `integrations/shopify/mapper.go`, `integrations/shopify/admin.go`, `integrations/shopify/erasure_fields.go` (three entries on the keep-list)
- Regenerate with `go run ./cmd/shopifyschema`: `integrations/shopify/generated/model.go`, `dsl/shopify/generated/refund.memql`, `dsl/shopify/generated/selections/Order.graphql`, `dsl/shopify/generated/selections/Refund.graphql`; then the concept snapshot and whatever `make sdk-gen` and `make platform-graph` change; then `integrations/shopify/testdata/erasure-fields.golden`, with `SHOPIFY_ERASURE_UPDATE_GOLDEN=1 go test ./integrations/shopify/ -run TestTheErasurePolicyMatchesItsGolden`, reading its diff
- Test: `cmd/shopifyschema/mapping_test.go`, `cmd/shopifyschema/emit_graphql_test.go`, `integrations/shopify/mapper_inline_test.go` (new), `integrations/shopify/erasure_fields_test.go`, `component/datasync/shopify_refund_detail_db_test.go` (new, database lane), `integrations/shopify/mirror_live_test.go`

**Interfaces:**
- Consumes: the generator's `planField(f *Field, isRef bool) (FieldPlan, bool, error)` and `objectSelection(t *Type, depth int, kept map[string]bool) string`, the fields `AdminType` and `GidsInside` on `FieldPlan` and `generated.FieldSpec`, the erasure's keep-list `erasureKeepList`, its rule `erasureKeptByKind(f generated.FieldSpec) (string, bool)` and its golden test, as Task 1 checked them.
- Produces, in the generator:

```go
// cmd/shopifyschema/allowlist.go
type Inline struct {
	Connection string   `yaml:"connection"`
	Page       int      `yaml:"page"`
	Fields     []string `yaml:"fields"`
}
// on Entry: Inline []Inline `yaml:"inline,omitempty"`

// cmd/shopifyschema/mapping.go
const KindInlineList = "inlineList"

// HeldField is one field an inlined connection keeps of each node.
type HeldField struct{ Name, Kind, AdminType string }

// on FieldPlan: Page int, the page an inlined connection is read with; Holds []HeldField, what it keeps of each node.
// planField gains the inline option as its last parameter:
func (p *Planner) planField(f *Field, isRef bool, inline *Inline) (FieldPlan, bool, error)
```

  An inlined connection plans as `FieldPlan{Kind: KindInlineList, DSLType: "[]object", Required: false, OmitFromBulk: true, Selection: "<connection>(first: <page>) { nodes { <the named fields> } }", Extract: "<connection>.nodes[]", AdminType: "<the node's type>", GidsInside: <the mirrored types its references name, sorted>, Page: <page>, Holds: <one entry a named field>}`.
- Produces, in the allowlist:

```yaml
  - type: Refund
    concept: refund
    scopes: [read_orders]
    reconcile: none
    bulk: false
    inline:
      - {connection: refundLineItems, page: 50, fields: [lineItem, quantity, subtotalSet, totalTaxSet]}
      - {connection: refundShippingLines, page: 10, fields: [subtotalAmountSet, taxAmountSet]}
      - {connection: orderAdjustments, page: 10, fields: [amountSet, taxAmountSet, reason]}
```

- Produces, generated: `generated.KindInlineList`; `generated.HeldField`; `Page int` and `Holds []HeldField` on `generated.FieldSpec`, written into a field's literal only when set; on `v1:shopify:refund` the fields `refundLineItems []object`, `refundShippingLines []object`, `orderAdjustments []object`. Each element holds the named fields and nothing else: a refund line `lineItem { id }`, `quantity`, `subtotalSet`, `totalTaxSet`; a shipping line `subtotalAmountSet`, `taxAmountSet`; an adjustment `amountSet`, `taxAmountSet`, `reason`. A money field is the object the generator selects for every money field (`shopMoney { amount currencyCode }` and `presentmentMoney`).
- Produces, in the connector: `mapObjectUnder` leaves a `KindInlineList` field out of the payload when its extracted value is nil; `RequestedCost float64` on `AdminResponse`, Shopify's `requestedQueryCost`; on the erasure's keep-list, under `refund`: `refundLineItems` ("the refunded lines: which line, how many, and their amounts"), `refundShippingLines` ("the refunded shipping: its amounts and nothing else") and `orderAdjustments` ("the refund's adjustments: their amounts and a reason that is an enum").

Decisions:
- An inlined connection names the fields it keeps and never takes its node's full selection. `Refund.refundShippingLines` shows why: the node's own selection would carry `shippingLine`, an object whose `phone` is the number at the buyer's delivery address, into every refund row. The figures read the two amounts, so the two amounts are what is kept.
- What may be named, enforced where the allowlist is validated and where a field is planned. `fields` is required and not empty. Each name is a field of the node type, and its type is one of: a scalar that is a figure or a moment (`Boolean`, `Int`, `BigInt`, `Float`, `Decimal`, `Money`, `UnsignedInt64`, `Date`, `DateTime`, `ID`, the scalars the erasure keeps by kind); an enum; a money object (`MoneyBag` or `MoneyV2`); or a reference to a mirrored type, kept as `{ id }`. A `String`, a `URL`, a `JSON`, and every other object or list is refused, and the refusal names the entry, the connection and the field and says an inlined connection holds amounts, counts, moments, enums and references and nothing a person typed. `Holds` records each named field's kind (`scalar`, `enum`, `object`, `gid`) and Admin type.
- The rest of the validation: a connection is named at most once across one entry's `children`, `references` and `inline`; `page` is 1 to 250, and 25 when omitted; the field is a connection; its node type is not an allowlist entry (a mirrored node is a child or a reference, never a second copy inline). Each refusal names the entry and the connection.
- A list the document did not select is "not read", never "empty". Today the mapper writes `[]` for every list it finds nothing for, so a bulk line would blank a refund's detail and a reader could not tell an unread refund from one with nothing on it. With the rule, a bulk line leaves the three keys out: on a new row they are absent, and on an existing row the engine's read-merge keeps what a fetch wrote. Every other list keeps the `[]` it gets today.
- The erasure. A list of objects is scrubbed to `[]` unless its field is on the erasure's keep-list, and a refund scrubbed to three empty lists would read as "detail read, nothing on it" and count its whole amount as a refund of items. So the three fields go on the keep-list, each with the reason above, and the golden file is regenerated and its diff read: it prints what each kept list holds. The generator sets the metadata the erasure's tests ask of every field: `AdminType` is the node's type and `GidsInside` the mirrored types its references name (`LineItem` for the refund lines, none for the other two).
- Whether the refund's lists need `completeChildren`: no, and it cannot reach them. It pages only the entries of the fetched object's own `spec.Children` that carry a `PageOp`, through an operation keyed by that object's id. `refunds` is a plain list (`List: true`, no `PageOp`), which Shopify answers whole, and a refund's inlined connection is a field of the child, in no `Children` list. So it is read one page deep: a refund holding a full page of refund lines is treated as possibly longer (`lines_complete` on its fact, Task 10), and Refunds stays exact because it starts from `totalRefundedSet`.
- The page an inlined connection is read with has one owner: the allowlist, carried to the generated field's `Page`. Task 10 reads it there and writes no number of its own.
- O3, as the code has it: only a store's first sweep (no watermark) lists every order; a later sweep reads what changed since its watermark. The detail therefore arrives through the first sweep, an order's own fetch, or Task 3, and a bulk line never removes it.
- The three fields make this release roll-forward only: a refund row that holds them is refused by an older schema on its next write.
- The account. `v1:shopify:refund` is generated with `accountId` and `@belongsTo(account, via="storeId", parent="v1:shopify:store")` (`cmd/shopifyschema/emit_dsl.go`); the regeneration keeps both and the three fields change neither. An erasure keeps `accountId` as structure. No new concept, so no new declaration and nothing for the Accounts app's inventory (A27).
- O2 and L11: the requested cost of `ShopifyFetchOrder` is a property of the document, the same for every order. The live test measures it once. Past Shopify's 1,000 points every order fetch would be refused and recording would stop, so the task is not done until the test has run against a development store. If it fails, lower the page of `refundLineItems` in the allowlist to 25, then 10, regenerate, and write the measured cost and the page into the record's 6.1.
- The `allowlist.yaml` header gains a paragraph for `inline`, beside children and references, that says what may be named and why.

- [ ] **Step 1: Write the failing tests**

```go
// cmd/shopifyschema/mapping_test.go
func TestAnInlinedConnectionCarriesOnlyTheFieldsItNames(t *testing.T)     // Refund.refundShippingLines: KindInlineList, "[]object", OmitFromBulk, Page 10, Extract "refundShippingLines.nodes[]", Selection "refundShippingLines(first: 10) { nodes { subtotalAmountSet { ... } taxAmountSet { ... } } }": no "shippingLine", no "phone"
func TestAnInlinedConnectionSaysWhatItHolds(t *testing.T)                 // Refund.refundLineItems: AdminType "RefundLineItem", GidsInside ["LineItem"], Holds lineItem gid LineItem, quantity scalar Int, subtotalSet object MoneyBag, totalTaxSet object MoneyBag
func TestAnInlinedFieldThatIsTextOrAnObjectIsRefused(t *testing.T)        // fields [shippingLine] on refundShippingLines, and a String field: each error names Refund, the connection and the field
func TestAnInlineWithNoFieldsIsRefused(t *testing.T)
func TestAnInlinedConnectionWhoseNodeIsMirroredIsRefused(t *testing.T)    // inline lineItems on Order: the error names Order.lineItems and LineItem
func TestAConnectionNamedTwiceOnOneEntryIsRefused(t *testing.T)           // references and inline both naming it
func TestAnInlinePageOutsideOneToTwoHundredFiftyIsRefused(t *testing.T)   // 0 is the default 25; -1 and 251 are refused
// cmd/shopifyschema/emit_graphql_test.go
func TestAnOrdersFetchSelectsEachRefundsDetailAndItsBulkDocumentDoesNot(t *testing.T) // the recorded Order.graphql: ShopifyFetchOrder and ShopifyListOrder hold the three connections inside refunds { }, the bulk documents none
// integrations/shopify/mapper_inline_test.go
func TestAnInlinedListTheDocumentDidNotSelectIsLeftOutOfTheWrite(t *testing.T) // a bulk line's refund: no refundLineItems key in the payload; duties, a plain list, is still []
func TestAnInlinedListThatWasReadAndIsEmptyIsWrittenEmpty(t *testing.T)        // {"refundLineItems":{"nodes":[]}} writes []
func TestARefundsDetailIsWrittenWithItsOrder(t *testing.T)                     // a fetched order with one refund: quantity 1, subtotalSet 36.00, shipping 5.00 on the refund's write
// integrations/shopify/erasure_fields_test.go
func TestNoInlinedListHoldsAFieldAnErasureWouldScrub(t *testing.T)             // every generated field of kind inlineList: the erasure keeps the field (by its list, with a reason), and every entry of Holds is kept by the erasure's own rule for a kind and an Admin type, or is a money object. A field renamed "note" of type String in a copy of the spec fails it
func TestAnErasedRefundKeepsItsDetail(t *testing.T)                            // the erasure's plan for refund keeps refundLineItems, refundShippingLines and orderAdjustments and still marks note
// component/datasync/shopify_refund_detail_db_test.go, database lane
func TestARefundRowHoldsItsLinesItsShippingAndItsAdjustments(t *testing.T)     // an order with a line refund, a shipping refund and an amount-only refund, swept: the three lists on the three rows, and no key named shippingLine or phone anywhere in a refund row
func TestRefundDetailArrivesWhicheverOfTheBackfillAndTheFirstSweepRunsFirst(t *testing.T) // O3, both orders
func TestABulkLineDoesNotEraseDetailAFetchRecorded(t *testing.T)               // same updatedAt, later export: the lists stay
// integrations/shopify/mirror_live_test.go
func TestLiveTheOrderFetchStaysUnderTheQueryCostCeiling(t *testing.T)          // O2, L11: creates an order and a refund as TestLiveAnEditARefundAndACancellationFollow does, fetches it, RequestedCost <= 1000, logs the number
func TestLiveARefundIsMirroredWithItsLinesAndItsShipping(t *testing.T)         // the refund that flow makes is written with the refunded line's quantity and subtotal and with nothing but the named fields: Shopify answers the inlined selection as generated
func TestLiveAnAmountOnlyRefundSaysHowAnAdjustmentIsSigned(t *testing.T)       // L4, L12: a refund of an amount with no line. Fails unless orderAdjustments holds one element; logs amountSet and taxAmountSet as written and fails if taxAmountSet is below zero, which Task 10 assumes it is not. On a store whose shop row says taxesIncluded it also refunds shipping and fails unless subtotalAmountSet is the shipping charged with its tax (L13); on any other store it logs that this store cannot answer that
func TestLiveShopifyWritesAmountsAsPlainDecimals(t *testing.T)                 // L16: every amount of the fetched order and its refund matches ^-?[0-9]+(\.[0-9]+)?$, and the test logs each amount's text and its scale
```

The database test composes a real engine, the real connector and its own fake Shopify transport, as `component/datasync/shopify_partitions_db_test.go` does; its fake answers `ShopifyListOrder`, `ShopifyBulkRun`, `ShopifyBulkStatus` and the export's download with JSON lines.

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./cmd/shopifyschema/ ./integrations/shopify/ ./component/datasync/ -run 'Inlin|NamedTwice|RefundsDetail|RefundRowHolds|RefundDetailArrives|BulkLineDoesNotErase|ErasedRefund'`. Expected: FAIL to compile, then FAIL.
- [ ] **Step 3:** Implement, regenerate with `go run ./cmd/shopifyschema`, then `make concept-snapshot`, `make sdk-gen`, `make platform-graph`, then the erasure's golden file.
- [ ] **Step 4:** Run the same command, the three packages whole in the database lane (`TestGeneratedTreeIsNotStale`, `TestTheErasurePolicyMatchesItsGolden`, `TestTheKeepListNamesOnlyFieldsAnErasureWouldOtherwiseScrub` and `TestEveryFieldOfTheModelCarriesItsAdminType` included), `scripts/ci/module-boundaries.sh integrations`, and the generated-file gates (`TestEveryShippedConceptDeclaresBelongsTo` among them, through `./test/dslconformance/...`). Expected: PASS.
- [ ] **Step 5:** Run `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 ./integrations/shopify/ -run 'TestLiveTheOrderFetchStaysUnderTheQueryCostCeiling|TestLiveARefundIsMirroredWithItsLinesAndItsShipping|TestLiveAnAmountOnlyRefundSaysHowAnAdjustmentIsSigned|TestLiveShopifyWritesAmountsAsPlainDecimals' -v` with a development store's `MEMQL_TEST_SHOPIFY_STORE_DOMAIN` and `MEMQL_TEST_SHOPIFY_ADMIN_TOKEN`. Expected: PASS, with the cost, the adjustment's amounts and each amount's scale logged. Without credentials all four skip: report that to the controller, who runs them before this task is accepted and writes what they logged into the record's live checks.
- [ ] **Step 6: Commit** `shopify: a refund is mirrored with its lines, its shipping and its adjustments, and with nothing a person typed`.

### Task 3: Every order is read once more, so older refunds get their detail

**Independent of:** Tasks 5, 6, 7, 8, 10, 20.

**Files:**
- Modify: `integrations/shopify/reconcile.go` (the listing loop of `reconcileByUpdatedAt` becomes `sweepListing`; `reconcileStore` asks the read for an order sweep that owes it)
- Create: `integrations/shopify/refund_detail.go`, `integrations/shopify/refund_detail_test.go`
- Test: `component/datasync/shopify_refund_detail_db_test.go`

**Interfaces:**
- Consumes: Task 2's inlined fields; `reconcileByUpdatedAt(ctx context.Context, store Store, spec *generated.TypeSpec, since time.Time) (memqlsync.ReconcileReport, error)`, which answers an error when a row it read could not be written (`unwritten`) and no error when it stops at `relistRowCap`; `reconcileStore(ctx, store, spec, since)`, which `sweepStore` calls after its two checks; `recordStoreHealthKey(ctx context.Context, storeID, key string, value any, extra map[string]any) error`; `storeByID(ctx context.Context, storeID string) (Store, bool, error)`; `ingestingStore(ctx context.Context, partition string) (Store, error)`, which `ReconcilePartition` asks first and which answers `memqlsync.Busy(ConnectorName, <reason>)`, an error wrapping `memqlsync.ErrBusy` and recognised by `memqlsync.IsBusy`, for a store that does not ingest, before any sweep runs.
- Produces:

```go
const (
	refundDetailFromKey  = "refundDetailFrom"  // health: when the read of every order began, written once it has ended (RFC3339, UTC)
	refundDetailAfterKey = "refundDetailAfter" // health: {"cursor", "began"} of a read under way; absent otherwise
)

var refundDetailRowBudget = relistRowCap // a variable so a test can lower it

// sweepListing is the loop reconcileByUpdatedAt runs today, with a place to start and a place to stop: it pages spec's
// listing under filter from after, completes and heals each row, and stops after budget rows. cut is true when it
// stopped there with more to read, and next is then the cursor to go on from.
func (c *Connector) sweepListing(ctx context.Context, store Store, spec *generated.TypeSpec, filter, after string, budget int) (report memqlsync.ReconcileReport, next string, cut bool, err error)

// refundDetailOwed reads the store row fresh and says whether its health holds no refundDetailFrom, and where a read under way stands.
func (c *Connector) refundDetailOwed(ctx context.Context, store Store) (owed bool, cursor string, began time.Time)

// reconcileRefundDetail is an order sweep of a store that owes the read.
func (c *Connector) reconcileRefundDetail(ctx context.Context, store Store, spec *generated.TypeSpec, since time.Time, cursor string, began time.Time) (memqlsync.ReconcileReport, error)
```

Decisions:
- Why: a refund row written before Task 2, or by a bulk line, has no detail, and an hourly sweep that reads only what changed never reads its order again. One read of the store's whole order listing reads every order Shopify still shows, with the detail. It rides the store's scheduled order sweeps, so nothing new is scheduled and nothing runs at boot.
- Only the order concept, and only while the store's health, read fresh, holds no `refundDetailFrom`. Every other sweep is `reconcileByUpdatedAt` as today, which now calls `sweepListing` with an empty cursor and `relistRowCap` and behaves as it did.
- With no read under way the sweep is the read: `sweepListing` with no filter and an empty cursor, whatever `since` it was handed. With a read under way the sweep runs first as it was handed, so the store's ordinary check is never held back by the read, and the read then goes on from its cursor.
- The read is stamped only when it ended. Ended, with no error and not cut: `refundDetailFrom` is written with when the read began and `refundDetailAfter` is cleared. Cut at its row budget: nothing is stamped and `refundDetailAfter` keeps the cursor and when the read began, so the next sweep goes on from there. Without the cursor a store with more orders than one sweep reads would read the same first orders at every sweep for good and never reach the rest, because the listing starts at the beginning on every call. An error (a page failed, or a row could not be written): nothing is stamped, the cursor stays where it was and the error is the sweep's, as it is today. A read that fails on its first page from a stored cursor drops the cursor, so a cursor Shopify no longer takes starts the read again.
- A paused store is not swept: `ingestingStore` answers busy before `reconcileStore` is reached, the runtime skips the sweep, nothing is read and nothing is stamped. After the resume the read begins, or goes on from its cursor.
- A health key that cannot be written is logged, and the next sweep does the same work again: reading twice is harmless, since a write is guarded and skips an unchanged row.
- What it cannot reach, and stays without detail: an order Shopify no longer shows (older than 60 days, without `read_all_orders`). A store one of whose orders can never be written repeats the read at every sweep until it can, as its ordinary sweep already re-reads from a watermark that does not move. The record's section 10 says both.

- [ ] **Step 1: Write the failing tests**

```go
// integrations/shopify/refund_detail_test.go (a fake Admin transport and a fake engine, as partitions_test.go has them)
func TestAStoresFirstOrderSweepUnderThisVersionReadsFromTheStart(t *testing.T) // a watermark is handed in, health has no refundDetailFrom: ShopifyListOrder carries no query variable, and the key is stamped with when the read began
func TestTheSweepAfterItReadsOnlyWhatChanged(t *testing.T)                     // stamped: the query is updated_at:>'<since>'
func TestOnlyTheOrderDomainIsReadFromTheStart(t *testing.T)                    // a product sweep with a watermark keeps its filter
func TestASweepInWhichAWriteFailedIsNotStamped(t *testing.T)                   // one row's write is refused: the sweep answers its error, no refundDetailFrom, no refundDetailAfter
func TestAReadCutAtItsRowBudgetIsNotStampedAndGoesOnFromItsCursor(t *testing.T) // refundDetailRowBudget 3, five orders: cut, no stamp, refundDetailAfter holds the cursor; the next sweep asks its handed filter first, then the listing after that cursor, and stamps when it ends with the first read's start
func TestAPausedStoresSweepIsSkippedAndNothingIsStamped(t *testing.T)          // ReconcilePartition answers busy (memqlsync.IsBusy), no Admin call, no health write; resumed, the next sweep reads from the start
func TestACursorTheStoreRefusesStartsTheReadAgain(t *testing.T)                // the first page after a stored cursor fails: the cursor is cleared
// component/datasync/shopify_refund_detail_db_test.go, database lane
func TestARefundMirroredBeforeItsDetailGetsItAtTheNextSweep(t *testing.T)      // a refund row with no lists and a watermark already set: one sweep later the lists are there
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./integrations/shopify/ ./component/datasync/ -run 'FromTheStart|OnlyWhatChanged|NotStamped|IsSkipped|RowBudget|StartsTheReadAgain|MirroredBeforeItsDetail'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, then both packages whole in the database lane and `scripts/ci/module-boundaries.sh integrations`. Expected: PASS.
- [ ] **Step 5: Commit** `shopify: every order is read once more, so a refund mirrored before its detail gets it`.

### Task 4: Every connected store has its shop row

**Independent of:** Tasks 5, 6, 7, 8, 10, 20.

**Files:**
- Create: `integrations/shopify/shop.go`, `integrations/shopify/shop_test.go`, `integrations/shopify/shop_db_test.go`
- Modify: `integrations/shopify/plugin.go` (`Bootstrap`), `integrations/shopify/connect_write.go` (`writeConnect`), `integrations/shopify/capabilities.go` (`handleEnsureSubscriptions`), `integrations/shopify/resume.go` (the two functions Task 1 recorded)

**Interfaces:**
- Consumes: `fetchAndMap(ctx context.Context, store Store, spec *generated.TypeSpec, gid string) (writes []memqlsync.MirrorWrite, gone bool, readAt time.Time, err error)`, which reads a singleton through its own query and ignores `gid` for one; `heal(ctx context.Context, writes []memqlsync.MirrorWrite) (healed int, failed int)`; `domainInConnection(store Store, spec *generated.TypeSpec) bool`; `scopesMissingFor(store Store, spec *generated.TypeSpec) []string`; `operatorContext(ctx context.Context) context.Context`; the generated read `shopifyShopForStore(storeId)`.
- Produces: `func (c *Connector) refreshShop(ctx context.Context, store Store) error` (reads the shop singleton with the generated `ShopifyFetchShop` and heals its row); `func (c *Connector) refreshShops(ctx context.Context, onlyMissing bool)` (every store that ingests; one store's failure is logged and the next is still read).

Decisions:
- Why: `v1:shopify:shop` is a singleton with no listing and no backfill, written only when a `SHOP_UPDATE` delivery arrives. A store whose settings never change has no row, so no `currencyCode` and no `ianaTimezone`.
- When: in `writeConnect`, after `queueConnectBackfills`, for the connected store; in `Bootstrap`, after `queueUnstartedBackfills`, for each ingesting store with no live shop row (`onlyMissing`); in `handleEnsureSubscriptions`, after the subscription pass, for every ingesting store, so a lost `SHOP_UPDATE` is healed within a day; and when a store is resumed, in the function the Resume act calls and in the one the store watch calls when it sees a store leave its pause, so a store resumed by either way reads its shop at once. `SHOP_UPDATE` stays as it is.
- A store whose connection does not ask for the shop's scope, or whose grant holds none of them, is not asked. A clean answer with no shop retires nothing. No failure here fails a connect, a boot, a resume or the subscription pass: it is logged with the store's id.
- The write is `heal`'s: guarded by the mirror's own rule, fenced on the row it was judged against and skipped when nothing changed, so a boot on every node, or a resume seen by both the act and the watch, writes once.

- [ ] **Step 1: Write the failing tests**

```go
// integrations/shopify/shop_test.go
func TestAConnectReadsTheStoresShop(t *testing.T)
func TestBootReadsTheShopOfAStoreThatHasNone(t *testing.T)                 // and makes no Admin call for a store that has its row
func TestTheDailyPassReadsEveryStoresShopAgain(t *testing.T)
func TestAResumeReadsTheStoresShop(t *testing.T)                           // through the act, and through the watch's pass that sees the store leave its pause
func TestAShopThatCannotBeReadStopsNothing(t *testing.T)                   // the connect and the resume still succeed; the next store is still read
func TestAStoreWithoutTheShopsScopeIsNotAsked(t *testing.T)                // zero Admin calls
// integrations/shopify/shop_db_test.go, database lane
func TestAConnectedStoreHasItsCurrencyAndTimeZoneWithoutAShopUpdate(t *testing.T) // Review Focus: the row reads back with currencyCode, ianaTimezone and plan
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./integrations/shopify/ -run 'StoresShop|ShopOfAStore|ShopThatCannot|ShopsScope|WithoutAShopUpdate'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, then the package whole in the database lane and `scripts/ci/module-boundaries.sh integrations`. Expected: PASS.
- [ ] **Step 5: Commit** `shopify: every connected store has its shop row, read at connect, at boot, daily and on resume`.

### Task 5: Periods in a zone (U2)

**Independent of:** Tasks 2, 3, 4, 6, 7, 8, 9, 10, 20.

**Files:**
- Modify: `core/statsperiod/statsperiod.go`
- Test: `core/statsperiod/statsperiod_test.go`

**Interfaces:**
- Produces: `func ForIn(p Period, now time.Time, loc *time.Location) Bounds`; `For(p, now)` is `ForIn(p, now, time.UTC)`; `func StartOfDay(y int, m time.Month, d int, loc *time.Location) time.Time`, the first instant whose date in `loc` is that day. Every time in an answer of `ForIn` is in `loc`. `Bounds.Next`, `Starts` and `PreviousStarts` keep their signatures.

Decisions:
- A nil `loc` is UTC.
- 24 hours: the hour under way starts at `now` less its local minutes, seconds and nanoseconds (never `Truncate(time.Hour)`, which cuts on UTC hours and is half an hour off in a +05:30 zone). `End` is that plus one hour, `Start` is `End` less 24 hours, the previous period the 24 hours before. Twenty-four buckets of one hour of elapsed time each, so on a day the clocks go back two of them carry the same local hour.
- Days and months: a day starts at the first instant whose local date is that day. `time.Date(y, m, d, 0, 0, 0, 0, loc)` is that instant except where the clocks skip midnight, where it answers an instant on the day before or an hour into the day; then the start is the change itself, from `Time.ZoneBounds`. `StartOfDay` holds the rule and is exported because Task 14 cuts a store's first recorded day with it.
- `End` is the start of tomorrow; `Start`, `PrevStart` and each bucket are reached by calendar days through `StartOfDay`, so a bucket is 23, 24 or 25 hours long. `Bounds.Next` steps a day or a month through `StartOfDay` on `t`'s own location.
- Twelve months: thirteen local months, no previous period, as before.

- [ ] **Step 1: Write the failing tests**

```go
// The expected values are what For answered at this instant before this change, written down as literals:
// a comparison of ForIn against For would compare a function with itself.
func TestForAndForInUTCAnswerWhatForAnswered(t *testing.T) {
	at := time.Date(2025, 11, 8, 10, 17, 42, 0, time.UTC)
	when := func(s string) time.Time {
		if s == "" {
			return time.Time{}
		}
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		p                              Period
		start, end, prevStart, prevEnd string
		bucket                         string
		buckets                        int
		hasPrevious                    bool
	}{
		{Last24Hours, "2025-11-07T11:00:00Z", "2025-11-08T11:00:00Z", "2025-11-06T11:00:00Z", "2025-11-07T11:00:00Z", BucketHour, 24, true},
		{Last7Days, "2025-11-02T00:00:00Z", "2025-11-09T00:00:00Z", "2025-10-26T00:00:00Z", "2025-11-02T00:00:00Z", BucketDay, 7, true},
		{Last30Days, "2025-10-10T00:00:00Z", "2025-11-09T00:00:00Z", "2025-09-10T00:00:00Z", "2025-10-10T00:00:00Z", BucketDay, 30, true},
		{Last90Days, "2025-08-11T00:00:00Z", "2025-11-09T00:00:00Z", "2025-05-13T00:00:00Z", "2025-08-11T00:00:00Z", BucketDay, 90, true},
		{Last12Months, "2024-11-01T00:00:00Z", "2025-12-01T00:00:00Z", "", "", BucketMonth, 13, false},
	} {
		for name, got := range map[string]Bounds{"For": For(c.p, at), "ForIn(UTC)": ForIn(c.p, at, time.UTC)} {
			if !got.Start.Equal(when(c.start)) || !got.End.Equal(when(c.end)) || !got.PrevStart.Equal(when(c.prevStart)) || !got.PrevEnd.Equal(when(c.prevEnd)) ||
				got.Bucket != c.bucket || got.Buckets != c.buckets || got.HasPrevious != c.hasPrevious || len(got.Starts()) != c.buckets {
				t.Errorf("%s(%s) = %+v", name, c.p, got)
			}
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

func TestTheTwentyFourHoursOfAFallBackDayHoldTheRepeatedHourTwice(t *testing.T) {
	newYork, _ := time.LoadLocation("America/New_York")
	starts := ForIn(Last24Hours, time.Date(2025, 11, 2, 12, 30, 0, 0, newYork), newYork).Starts()
	if len(starts) != 24 { t.Fatalf("%d buckets", len(starts)) }
	ones := 0
	for i, s := range starts {
		if i > 0 && s.Sub(starts[i-1]) != time.Hour { t.Errorf("bucket %d starts %s after the one before", i, s.Sub(starts[i-1])) }
		if s.In(newYork).Day() == 2 && s.In(newYork).Hour() == 1 { ones++ }
	}
	if ones != 2 { t.Errorf("%d buckets start at 01:00 on the 2nd, want the hour before and the hour after the clocks went back", ones) }
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
	if got := StartOfDay(2023, 9, 3, santiago); !got.Equal(day) { t.Errorf("StartOfDay = %s", got) }
}

func TestAStoreDayIsNotAUTCDay(t *testing.T)        // America/Los_Angeles at 2025-11-08T03:00Z: 7d ends 2025-11-08T08:00Z, not the UTC midnight
func TestTwelveMonthsStartOnLocalFirsts(t *testing.T) // Europe/Madrid in November 2025: Start 2024-10-31T23:00Z, thirteen buckets
func TestANilZoneIsUTC(t *testing.T)                // ForIn(p, at, nil) equals ForIn(p, at, time.UTC) field by field, and StartOfDay(2025, 11, 8, nil) is 2025-11-08T00:00:00Z
```

- [ ] **Step 2:** Run `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 ./core/statsperiod/`. Expected: FAIL (undefined `ForIn`).
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, then `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/emailstats/ ./component/sitetraffic/ -run 'Statistic|Period'` (the two callers of `For`). Expected: PASS.
- [ ] **Step 5: Commit** `statsperiod: a period can be cut in a store's own time zone`.

### Task 6: Exact money (U3)

**Independent of:** Tasks 2, 3, 4, 5, 7, 8, 9, 20.

**Files:**
- Create: `core/money/money.go`, `core/money/currency.go`, `core/money/money_test.go`

**Interfaces:**
- Produces, package `money` (`github.com/znasllc-io/memql/core/money`), on `math/big` and nothing else:

```go
type Amount struct{ /* an unscaled big integer and a scale */ }

func Parse(s string) (Amount, error)                              // only -?digits[.digits], ASCII digits
func (a Amount) Add(b Amount) Amount                              // at the wider of the two scales
func (a Amount) Sub(b Amount) Amount
func (a Amount) Mul(n int64) Amount                               // keeps a's scale
func (a Amount) MulRatio(num, den int64, scale int) (Amount, error) // a * num / den, rounded half to even at scale decimals; den 0 or a scale below 0 is an error
func (a Amount) Scale() int
func (a Amount) Sign() int
func (a Amount) Equal(b Amount) bool                              // the same value whatever the two scales: "36.0" equals "36.00"
func (a Amount) String() string                                   // every digit of its scale: "42.00" stays "42.00"

func MinorUnits(currency string) int                 // 0, 3 or 2: the decimals a currency is counted in
func RoundingScale(a Amount, currency string) int    // the larger of a's own scale and the currency's minor units
```

Decisions:
- The zero value is 0 at scale 0 and usable. A value is immutable: no method changes its receiver.
- `Parse` refuses anything else, the empty string included, and its error names no input longer than 32 characters. A negative zero prints as zero.
- A sum's scale is the wider operand's, so adding "1" to "42.00" gives "43.00".
- The one rounding rule. A computed amount is rounded half to even at `RoundingScale`: the larger of the operand's own scale and the currency's minor units. The written scale alone is not enough, because the scale is whatever Shopify wrote and it writes a whole amount with one decimal: a third of "1.0" would come out "0.3" in a currency counted in cents. The caller passes the scale to `MulRatio`, so the rule is in one function and is seen at every call.
- `MinorUnits` reads a table compiled into `currency.go`, taken from the list of minor units in ISO 4217 for the codes of the Admin API's `CurrencyCode` enum as recorded in `cmd/shopifyschema/testdata/schema-2026-07.json`:
  - 0: `BIF`, `CLP`, `DJF`, `GNF`, `ISK`, `JPY`, `KMF`, `KRW`, `PYG`, `RWF`, `UGX`, `VND`, `VUV`, `XAF`, `XOF`, `XPF`.
  - 3: `BHD`, `IQD`, `JOD`, `KWD`, `LYD`, `OMR`, `TND`.
  - 2: every other code, a code the table has never seen and the empty string included. The code is matched in upper case. A larger exponent than a currency has only keeps more decimals, so an unknown code is never rounded too coarsely.

- [ ] **Step 1: Write the failing tests**

```go
func TestParseAcceptsOnlyPlainDecimals(t *testing.T) {
	for _, ok := range []string{"0", "42", "42.00", "-0.10", "007.5", "1200", "1.250", "35.0"} {
		if _, err := Parse(ok); err != nil { t.Errorf("Parse(%q): %v", ok, err) }
	}
	// the last is two Arabic-Indic digits: only ASCII digits are an amount
	for _, bad := range []string{"", " 1", "1 ", "1e3", "1E3", ".5", "5.", "+5", "1,000", "NaN", "Inf", "0x10", "1.2.3", "-", "--1", "\u0661\u0662"} {
		if _, err := Parse(bad); err == nil { t.Errorf("Parse(%q) was accepted", bad) }
	}
}

func TestOneTenthPlusTwoTenthsIsThreeTenths(t *testing.T) // Parse("0.1").Add(Parse("0.2")).String() == "0.3"

func TestASumKeepsTheWiderScale(t *testing.T)             // "1.5"+"2.25" = "3.75"; "42.00"+"1" = "43.00"; "5.00"-"5" = "0.00"

func TestMulRatioRoundsHalfToEvenAtTheScaleItIsGiven(t *testing.T) {
	for _, c := range []struct{ a string; num, den int64; scale int; want string }{
		{"0.05", 1, 2, 2, "0.02"}, {"0.15", 1, 2, 2, "0.08"}, {"-0.05", 1, 2, 2, "-0.02"},
		{"10.00", 1, 3, 2, "3.33"}, {"10.00", 2, 3, 2, "6.67"}, {"1.00", 2, 3, 2, "0.67"},
		{"1200", 1, 7, 0, "171"}, {"3.00", 2, 3, 2, "2.00"}, {"8.00", 2, 2, 2, "8.00"},
		{"1.0", 2, 3, 2, "0.67"},   // three units, two kept, a discount written with one decimal
		{"1.0", 2, 3, 1, "0.7"},    // what the written scale alone would give
		{"101.99", 1, 2, 2, "51.00"}, // 50.995 is a tie: to the even cent
		{"35.0", 1, 1, 2, "35.00"}, // a wider scale pads
	} { /* MulRatio(c.num, c.den, c.scale).String() == c.want */ }
}

func TestAShareIsRoundedAtTheCurrencysMinorUnitsWhenTheAmountWasWrittenShorter(t *testing.T) {
	a, _ := Parse("1.0")
	for currency, want := range map[string]string{"EUR": "0.67", "JPY": "0.7", "KWD": "0.667", "": "0.67", "eur": "0.67"} {
		got, err := a.MulRatio(2, 3, RoundingScale(a, currency))
		if err != nil || got.String() != want { t.Errorf("%q: %s, %v; want %s", currency, got, err, want) }
	}
	// and never coarser than the amount was written: "1.2345" in EUR keeps four decimals
	b, _ := Parse("1.2345")
	if RoundingScale(b, "EUR") != 4 { t.Error("an amount written with four decimals is rounded at fewer") }
}

func TestTheCurrenciesCountedWithoutDecimalsAndWithThree(t *testing.T) {
	for _, c := range []string{"BIF", "CLP", "DJF", "GNF", "ISK", "JPY", "KMF", "KRW", "PYG", "RWF", "UGX", "VND", "VUV", "XAF", "XOF", "XPF"} {
		if MinorUnits(c) != 0 { t.Errorf("%s: %d", c, MinorUnits(c)) }
	}
	for _, c := range []string{"BHD", "IQD", "JOD", "KWD", "LYD", "OMR", "TND"} {
		if MinorUnits(c) != 3 { t.Errorf("%s: %d", c, MinorUnits(c)) }
	}
	for _, c := range []string{"EUR", "USD", "HUF", "TWD", "MGA", "XXX", "not a code", ""} {
		if MinorUnits(c) != 2 { t.Errorf("%q: %d", c, MinorUnits(c)) }
	}
}

func TestMulRatioByZeroOrAtANegativeScaleIsRefused(t *testing.T)
func TestMulKeepsTheScale(t *testing.T)                   // "5.00".Mul(0) = "0.00"; "9.99".Mul(2) = "19.98"
func TestEqualIsByValue(t *testing.T)                     // "36.0" equals "36.00"; "36.01" does not; the zero value equals "0.00"
func TestAMillionRandomSumsEqualBigRat(t *testing.T)      // a seeded generator of amounts at scales 0 to 4, signs both ways; the running Amount equals the running big.Rat at every step
func TestAZeroDecimalAndAThreeDecimalCurrencySumExactly(t *testing.T) // "1200"+"800" = "2000"; "1.250"+"0.005" = "1.255"
func TestTheZeroValueIsZeroAndNegativeZeroPrintsAsZero(t *testing.T)   // Amount{}.String() == "0"; Parse("-0.00").String() == "0.00"
func TestNoMethodChangesItsReceiver(t *testing.T)
```

- [ ] **Step 2:** Run `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 ./core/money/`. Expected: FAIL (the package does not exist).
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, and from `core/` run `GOWORK=off MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 ./money/` (the module needs nothing outside itself). Expected: PASS.
- [ ] **Step 5: Commit** `money: exact decimal amounts, rounded half to even at a currency's own decimals`.

### Task 7: Tables (U4)

**Independent of:** Tasks 2, 3, 4, 5, 6, 8, 9, 10, 20.

**Files:**
- Create: `component/database/memory-nodes/migrations/20261112110100_store_sales_facts.up.sql`, `component/database/memory-nodes/migrations/20261112110100_store_sales_facts.down.sql` (the first free minute after `20261112110000_account_index`, the newest migration on the branch; a later minute if Task 1 found a newer one), `component/database/memory-nodes/store_sales_facts_migration_test.go`

**Interfaces:**
- Produces seven tables. Statements are separated by `--bun:split`; every table is `CREATE TABLE IF NOT EXISTS` and every index `CREATE INDEX IF NOT EXISTS`, each index under a comment line that begins `-- read by:` and names the statement that uses it. The file opens with a comment in the manner of `20261108090300_email_stat_facts.up.sql`: side tables and not graph rows, every id bare, no personal data, what a version is, why a retired row's fact is kept.

```sql
CREATE TABLE IF NOT EXISTS store_sales_order_fact (
    store_id                 TEXT NOT NULL,
    order_gid                TEXT NOT NULL,
    order_version            TIMESTAMPTZ NOT NULL,            -- the mirror row's own write time (row.createdAt)
    deleted                  BOOLEAN NOT NULL DEFAULT false,  -- the mirror retired the row: the fact stays, counts nowhere and refuses every older version
    recorded_at              TIMESTAMPTZ NOT NULL,            -- the row's syncedAt
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
-- read by: the orders of a period (the read's figures and series)
CREATE INDEX IF NOT EXISTS store_sales_order_fact_ordered_idx ON store_sales_order_fact (store_id, ordered_at);
-- read by: a buyer's first counted order, for new against returning
CREATE INDEX IF NOT EXISTS store_sales_order_fact_buyer_idx ON store_sales_order_fact (store_id, buyer_key, ordered_at);

CREATE TABLE IF NOT EXISTS store_sales_line_fact (
    store_id            TEXT NOT NULL,
    line_gid            TEXT NOT NULL,
    order_gid           TEXT NOT NULL DEFAULT '',        -- '' only on a deleted fact whose tombstone carried no parent
    line_version        TIMESTAMPTZ NOT NULL,
    deleted             BOOLEAN NOT NULL DEFAULT false,
    currency            TEXT NOT NULL DEFAULT '',        -- of its unit price: it picks the scale a share is rounded at
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
    PRIMARY KEY (store_id, line_gid)
);
-- read by: the lines of an order, in the derivation and in every figure that reaches a line through its order
CREATE INDEX IF NOT EXISTS store_sales_line_fact_order_idx ON store_sales_line_fact (store_id, order_gid);

CREATE TABLE IF NOT EXISTS store_sales_refund_fact (
    store_id           TEXT NOT NULL,
    refund_gid         TEXT NOT NULL,
    order_gid          TEXT NOT NULL DEFAULT '',
    refund_version     TIMESTAMPTZ NOT NULL,
    deleted            BOOLEAN NOT NULL DEFAULT false,
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
-- read by: the refunds of a period (the read's figures and series)
CREATE INDEX IF NOT EXISTS store_sales_refund_fact_refunded_idx ON store_sales_refund_fact (store_id, refunded_at);
-- read by: the refunds of an order, in the derivation
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
-- read by: the units refunded of each line of an order, in the derivation
CREATE INDEX IF NOT EXISTS store_sales_refund_line_fact_line_idx ON store_sales_refund_line_fact (store_id, order_gid, line_gid);

CREATE TABLE IF NOT EXISTS store_sales_store (
    store_id                 TEXT PRIMARY KEY,
    generation               BIGINT NOT NULL DEFAULT 0,       -- raised by a rebuild and by a removal: a page read under an older one writes nothing
    purging                  BOOLEAN NOT NULL DEFAULT false,  -- the store's facts are being deleted in batches; nothing is folded meanwhile
    store_domain             TEXT NOT NULL DEFAULT '',
    store_name               TEXT NOT NULL DEFAULT '',
    currency                 TEXT NOT NULL DEFAULT '',
    timezone                 TEXT NOT NULL DEFAULT '',
    taxes_included           BOOLEAN,
    development              BOOLEAN,
    all_orders_readable      BOOLEAN NOT NULL DEFAULT false,
    history_floor            TIMESTAMPTZ,                     -- an instant before the store row was first written: where every walk of the store begins
    history_since            TIMESTAMPTZ,                     -- the store-local midnight of the first recorded day
    wholesale_enabled        BOOLEAN NOT NULL DEFAULT false,
    wholesale_digest         TEXT NOT NULL DEFAULT '',
    wholesale_cursor         TEXT NOT NULL DEFAULT '',
    wholesale_window_seconds INTEGER NOT NULL DEFAULT 86400,
    verdict_state            TEXT NOT NULL DEFAULT '',
    verdict_reasons          JSONB NOT NULL DEFAULT '[]',
    verdict_since            TIMESTAMPTZ,
    last_change_at           TIMESTAMPTZ,
    walked_through           TIMESTAMPTZ,
    rebuilding_since         TIMESTAMPTZ,
    definition               INTEGER NOT NULL DEFAULT 0
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
    store_id              TEXT PRIMARY KEY,
    -- the pass under way
    pass_started_at       TIMESTAMPTZ,                     -- null when none is under way
    order_cursor          TEXT NOT NULL DEFAULT '',        -- '', an instant to go on from, or 'done'
    order_window_seconds  INTEGER NOT NULL DEFAULT 86400,
    line_cursor           TEXT NOT NULL DEFAULT '',
    line_window_seconds   INTEGER NOT NULL DEFAULT 86400,
    refund_cursor         TEXT NOT NULL DEFAULT '',
    refund_window_seconds INTEGER NOT NULL DEFAULT 86400,
    compared              INTEGER NOT NULL DEFAULT 0,
    mismatched            INTEGER NOT NULL DEFAULT 0,
    repaired              INTEGER NOT NULL DEFAULT 0,
    -- the last pass that ended
    last_started_at       TIMESTAMPTZ,
    last_finished_at      TIMESTAMPTZ,
    last_compared         INTEGER NOT NULL DEFAULT 0,
    last_mismatched       INTEGER NOT NULL DEFAULT 0,
    last_repaired         INTEGER NOT NULL DEFAULT 0,
    last_disagreeing      INTEGER NOT NULL DEFAULT 0,
    clean_at              TIMESTAMPTZ                      -- when a pass last ended with nothing mismatched
);
```

Decisions:
- The down file drops the seven tables, the check first and the order fact last.
- The line fact is keyed by the store and the line, not by its order: a tombstone carries a row's identity and may carry no parent, and it must still find the fact it retires. An index by order serves every read that goes from an order to its lines. No index is kept by product: the products of a period are grouped after the join from the period's orders, and nothing looks a line up by its product.
- Every column has a writer, named in the task that writes it: Task 11 a store's row; Task 12 `history_floor`; Task 13 the fact tables, the watermarks and the store's `last_change_at` and `walked_through`; Task 14 the rest of `store_sales_store`, `generation` and `purging` included, but the wholesale columns; Task 15 those; Task 16 `checked_at` and `store_sales_check`.
- The round trip stands beside the other migration round trips (`role_test_session_account_migration_test.go`), not in the component: it reads the two files, rewrites `CREATE TABLE` to `CREATE TEMP TABLE`, and runs up, down, up inside one transaction it rolls back.
- "On Postgres and TimescaleDB" is held two ways: the lane's database is TimescaleDB, and the migration uses nothing of it, which a test reads off the text.
- Whose rows these are (A27). None of the seven is a concept and none is in the row table, so the accounts piece's rules do not reach them: no `@belongsTo` is declared (the rule is for a concept, and `TestEveryShippedConceptDeclaresBelongsTo` walks concepts), the engine's account stamp runs in the write path of the row table and never sees a `store_sales_*` statement, and the Accounts app's inventory (`integrations/accounts/inventory.go`), which counts node-table rows of the concepts that declare an account form, neither counts them nor needs a new area. A fact's account is its store's, reached through `store_id`: the mirror rows a fact is computed from follow the same store (`@belongsTo(account, via="storeId", parent="v1:shopify:store")`), and a fact is derived and can be rebuilt from them. No column holds an account; whether `store_sales_store` should copy the store's is open (pre-flight, "Needs a ruling"). The opening comment of the up file says this in a sentence: side tables, keyed by store, whose account is the store's.

- [ ] **Step 1: Write the failing tests**

```go
func TestStoreSalesFactsMigrationRoundTrips(t *testing.T)               // up, down, up: after the first and the third step the seven tables, their primary keys and the six named indexes exist; after the second none does
func TestStoreSalesMoneyColumnsAddExactly(t *testing.T)                 // two order facts with shipping 0.1 and 0.2: SUM(shipping)::text is "0.3"; 1234567890123.456789 reads back digit for digit
func TestStoreSalesFactsMigrationUsesNothingOfTimescale(t *testing.T)   // neither file names create_hypertable, timescaledb or a continuous aggregate
func TestEveryStoreSalesIndexNamesItsReader(t *testing.T)               // every CREATE INDEX line of the up file follows a line that begins "-- read by:"
func TestALineFactIsFoundByItsStoreAndItsLineAlone(t *testing.T)        // the line fact's primary key is (store_id, line_gid), read from the catalog after the up step
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/database/memory-nodes/ -run 'StoreSales|LineFactIsFound'`. Expected: FAIL (no such file).
- [ ] **Step 3:** Write the two files.
- [ ] **Step 4:** Run the same command, then `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/database/...`. Expected: PASS.
- [ ] **Step 5: Commit** `database: the store sales fact tables`.

### Task 8: The sales part (U5)

**Independent of:** Tasks 2, 3, 4, 5, 6, 7, 9, 10, 20.

**Files:**
- Modify: `component/auth/rbac_model.go` (`appPartGrants`), `dsl/rbac/seeds.memql`, `component/memql/organization_action_capabilities.go`, `component/memql/app_resource_os_parity_test.go` (the part list of `assertKnownResourcesNameEveryApp`)
- Create: `component/memql/sales_capability_test.go`

**Interfaces:**
- Produces: `"app:deployables/sales": {RoleOwner, RoleDeveloper}` in `appPartGrants`; the seeds `cap-owner-execute-app-deployables-sales` and `cap-developer-execute-app-deployables-sales` (`verb: "execute"`, `resourceType: "app:deployables/sales"`, `predefined: true`); `organizationDeployableCapability("app:deployables/sales")` is true; `organizationCapabilityTarget` answers `("v1:platform:site", "siteId")` for a builtin named `storeStatistics`.

Decisions:
- The seed descriptions, in the form the reviews seeds use: "owner: the sales part of the Deployables app -- a storefront's sales figures, on its Store statistics page." and the same for developer. The comment above them and in `rbac_model.go` gives S6's reason: revenue is more sensitive than visits, so it is a part of its own, granted to a client's group beside `read app:deployables`.
- The builtin is declared in Task 17, and its row in `TestTheDeployablesPartsAreDeclaredOnTheirConstructs` lands there too: that test loads the shipped tree. So does the record's database proof that a holder in one organization is refused a site of another: the gate judges a loaded builtin against a stored site.

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

**Independent of:** Tasks 5, 6, 7, 8, 20.

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

- Produces seven queries in `dsl/shopify/overlay/queries.memql`, each `@actor`, `@serverOnly`, `@cache(0)`. The four walks have this form, tombstones included:

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

  `storeStatisticsLinesChanged` (concept `orderLineItem`, shape `storeStatisticsLine`), `storeStatisticsRefundsChanged` (`refund`, `storeStatisticsRefund`) and `storeStatisticsOrderBuyers` (`order`, `storeStatisticsBuyer`) are the same with their concept and shape. `storeStatisticsShop { storeId string! }` filters `actor.isClusterOwner == true && row.storeId == args.storeId`, sorts `"row.createdAt", "desc"`, `paginate 1`. `storeStatisticsStores` takes no argument, filters `actor.isClusterOwner == true`, sorts `"row.createdAt", "asc"`, `paginate 100`. `storeStatisticsStoreAt { storeId string!, at datetime! }` is on `store` with shape `storeStatisticsStore`: filter `actor.isClusterOwner == true && row.id == args.storeId`, `asOf args.at ?? latest` (the clause `storesForConnector` has), `paginate 1`. It answers the store row as it stood at an instant, and nothing for an instant before the row was first written.
- Produces, in the wholesale pack: the `@row` shapes `entitlementStatisticsRef` (`row.id`, `storeId`, `buyerEmail`, `state`) and `wholesaleSettingsStatisticsRef` (`row.id`, `storeId`); and two `@serverOnly`, `@actor` queries, each with an `@description`: `wholesaleStatisticsGranted { storeIds []string! }` on `entitlement`, filter `actor.isClusterOwner == true && row.storeId in args.storeIds && row.state == "granted"`, and `wholesaleStatisticsSettings { storeIds []string! }` on `wholesaleSettings`, filter `actor.isClusterOwner == true && row.storeId in args.storeIds`; both sort `"row.createdAt", "asc"` and `paginate 250`.

Decisions:
- `row.createdAt >= args.since` stays a top-level `&&` conjunct: only that shape lets the engine collapse the versions written since, so a page costs what was written since and not the concept's history (`component/memql/latest_scan_keys.go`). The existing walks in `dsl/campaigns/queries.memql` are the pattern, their `///` line included: each query here carries one that says why `actor.userId` cannot scope it and that the cluster-owner predicate and internal origin are both mandatory.
- `storeStatisticsStoreAt` exists because nothing on a row says when the row was first written: `row.createdAt` is the write time of its newest version, and a store row is rewritten at least daily. The walk asks for the store row as of earlier and earlier instants to find one before it existed (Task 12).
- The pack's reads take a list because the pack's rows hold a store id in either form and the engine does not join the two: the caller passes `<id>` and `v1:shopify:store:<id>`. The settings read is a query of its own because a query is bound to one concept.
- The pack's files are appended to and nothing in them is reordered, so a rebase over the wholesale branch is a textual merge.
- Each overlay query is added to the map in `TestServerOnlyParsedSetMatchesTheTree` with a comment giving its reason: a mirror row has no owner to scope an actor against, and the one reader is the statistics walk under its maintenance principal.
- O4, from `dsl/pack_enablement.go`: a disabled pack keeps its concepts and loads none of its queries, so asking one on a node where the pack is off is an unknown call. The pack's test holds that; Task 15 asks the pack's state before it asks a query.
- No concept is added (A27). Each shape and query is on a concept that already declares its account: the mirror's `order`, `orderLineItem`, `refund` and `shop` follow their store (`via="storeId", parent="v1:shopify:store"`), `store` is `@belongsTo(account)`, the pack's `entitlement` follows its application and `wholesaleSettings` its store. No shape projects `accountId`: the walk reads every store's rows under the cluster-owner predicate and keys them by store, and the account plays no part in a figure.

- [ ] **Step 1: Write the failing tests**

```go
// integrations/shopify/statistics_reads_authz_db_test.go, database lane, with authorityEngine and seedStore
func TestTheStatisticsReadsAnswerNothingToAReaderAndRowsToAClusterOwner(t *testing.T) // each of the seven: a reader with internal origin gets zero rows; a cluster owner with client origin is refused as server-only; a cluster owner with internal origin gets the seeded row
func TestAChangedReadAnswersATombstoneToo(t *testing.T)                               // a row with deleted true comes back, deleted true
func TestTheStatisticsShapesProjectExactlyTheirFields(t *testing.T)                   // the loaded tree's six shapes hold the lists above and nothing else: no person's name, no address and no phone (the one name projected is the shop's and the store's own, on storeStatisticsShop and storeStatisticsStore), no accountId, and email on the two order shapes only
func TestTheStoreRowIsAnsweredAsOfAnInstantAndNotBeforeItWasWritten(t *testing.T)     // storeStatisticsStoreAt: an instant after the store's first write answers the row as it stood then (a name changed later is the older name); an instant before it answers no row
// packs/wholesalepack/statistics_reads_db_test.go, database lane
func TestTheGrantedReadFindsABuyerUnderEitherFormOfTheStoreId(t *testing.T)           // one entitlement stored with the bare id, one with v1:shopify:store:<id>: asked both forms, both; asked one, one
func TestOnlyGrantedEntitlementsAreAnswered(t *testing.T)                             // pending, failed and revoked are not
func TestTheSettingsReadSaysWhichStoresAreSetUp(t *testing.T)
func TestThePacksStatisticsReadsAreForAClusterOwnerWithInternalOrigin(t *testing.T)
func TestWithThePackDisabledItsStatisticsQueriesAreNotLoaded(t *testing.T)            // O4
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./integrations/shopify/ ./packs/wholesalepack/ -run 'StatisticsRead|ChangedRead|StatisticsShapes|AsOfAnInstant|GrantedRead|OnlyGranted|SettingsRead|StatisticsQueries'`. Expected: FAIL.
- [ ] **Step 3:** Write the constructs, then `make sdk-gen`.
- [ ] **Step 4:** Run the same command, `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./packs/wholesalepack/`, `scripts/ci/module-boundaries.sh integrations`, and the generated-file gates. Expected: PASS, and `make sdk-gen-check` shows no builder for any of the nine.
- [ ] **Step 5: Commit** `shopify: the server-only reads the store statistics walk asks`.

### Task 10: The rules that turn a mirrored row into a fact (U7)

**Independent of:** Tasks 3, 4, 5, 7, 8, 20.

**Files:**
- Create: `component/storestats/doc.go`, `component/storestats/facts.go`, `component/storestats/facts_test.go`, `component/storestats/erasure_gate_test.go`

**Interfaces:**
- Consumes: Task 6's `money.Amount`, with `money.Parse(s string) (Amount, error)`, `Add(b Amount) Amount`, `Sub(b Amount) Amount`, `Mul(n int64) Amount`, `MulRatio(num, den int64, scale int) (Amount, error)`, `Scale() int`, `Equal(b Amount) bool` and `money.RoundingScale(a Amount, currency string) int`; Task 2's `Page` on the generated field `refundLineItems` of `generated.Types["refund"]` (package `integrations/shopify/generated`); Task 9's shapes `storeStatisticsOrder`, `storeStatisticsLine`, `storeStatisticsRefund` and `storeStatisticsBuyer` as `dsl/shopify/overlay/shapes.memql` declares them; the erasure's exported decision `shopify.ErasureDecision(concept, field string) string` (`integrations/shopify/erasure_fields.go`), as Task 1 checked it; a mirrored row as a `map[string]any`, with the keys of the table below.
- Produces:

```go
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
	Currency                      string // of its unit price
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
	rowGone                      // a tombstone: keep the fact, marked deleted, at this version
	rowUnreadable                // no fact can be made; counted and logged, never guessed
)

func orderFactOf(row map[string]any) (OrderFact, rowKind, error)
func lineFactOf(row map[string]any) (LineFact, rowKind, error)
func refundFactOf(row map[string]any, linesPage int) (RefundFact, rowKind, error)
func refundLinesPage() int // the Page of refund.refundLineItems in the generated model; 0 when the model has none
func soldOf(quantity, currentQuantity, refundedQuantity int, unitPrice, allocated money.Amount, currency string) (sold int, gross, discounts money.Amount)
func instantOf(v any) (time.Time, bool) // a time.Time, or an RFC3339 string

// same reports whether two facts made from a row hold the same content: every field the fold writes,
// instants by Equal, amounts by value, a nil amount only with a nil one. Email is left out; the check judges wholesale beside it.
func (f OrderFact) same(g OrderFact) bool
func (f LineFact) same(g LineFact) bool
func (f RefundFact) same(g RefundFact) bool // its lines too, in order
```

- Consumes, key by key. A row is what a shape of Task 9 answers: `row.id` under `id`, `row.createdAt` under `createdAt`, every other field under its own name. A money value is an object; its amount is the decimal string at `shopMoney.amount` and its currency the code at `shopMoney.currencyCode`.

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
| line | `GiftCard`, `Title`, `Quantity`, `CurrentQuantity`, `UnitPrice`, `Currency` | `isGiftCard`, `title`, `quantity`, `currentQuantity`, `originalUnitPriceSet` (its amount, and its currency code) |
| line | `ProductKey` | `productGid`, else `title:` and the title |
| line | `AllocatedDiscounts` | the sum of `allocatedAmountSet` over `discountAllocations` |
| refund | `StoreID`, `RefundGID`, `OrderGID`, `Version`, `RecordedAt` | `storeId`, `gid`, `parentGid`, `createdAt`, `syncedAt` |
| refund | `RefundedAt`, `MoneyRefunded` | `processedAt`, else `sourceCreatedAt`; `totalRefundedSet` |
| refund | `DutiesRefunded` | the sum of `amountSet` over `duties` |
| refund | `ShippingRefunded` | the sum of `subtotalAmountSet` over `refundShippingLines` |
| refund | `TaxRefunded` | the sum of `totalTaxSet` over `refundLineItems`, of `taxAmountSet` over `refundShippingLines` and of `taxAmountSet` over `orderAdjustments`, each with the sign it was written with |
| refund | `Lines` | `refundLineItems` in order: `lineItem.id`, `quantity`, `subtotalSet`, `totalTaxSet` |
| any | `rowGone` | `deleted` is true |

Decisions:
- How a tombstone is recognised: by `deleted == true` and nothing else. The mirror retires a row with a raw insert that carries only `storeId`, `gid`, `updatedAt`, `versionSource`, `syncedAt`, `versionReadAt` and `deleted: true` (`integrations/shopify/mapper.go`, `tombstone`; `component/datasync/mirror_writer.go`, `retireFieldsFor`), and the engine's read-merge keeps every other field the row held. So a tombstone still shows its last amounts, its `updatedAt` is the time MemQL read Shopify and not Shopify's version, and its `versionSource` is `read` or `announced`. None of those is read: a row with `deleted` true is `rowGone`. Its answer carries its ids, its `Version` and, when the row holds one, its `RecordedAt`; `deleted` is judged first, and a tombstone needs only `storeId`, `gid` and `createdAt`.
- A fact's `Version` is the row's own `createdAt`, the instant the mirror wrote that version of the row. It is how the engine itself orders a row's versions, and it moves forward on every write, a tombstone and a revival included; the row's `updatedAt` does not.
- 5.1 and 5.2 of the record are the formulas, and the table above is how they read a row. `Tax` is zero at `totalTaxSet`'s scale when `taxesIncluded`. An absent money object is zero, except the three Shopify totals, which are nil.
- `soldOf` is 5.1: `sold = min(quantity, currentQuantity + refundedQuantity)`; `gross = unitPrice.Mul(sold)`; `discounts` is zero at `allocated`'s scale when `quantity` is 0, else `allocated` when `sold == quantity`, else `allocated.MulRatio(sold, quantity, money.RoundingScale(allocated, currency))`, whose error cannot occur there because a zero quantity was answered first. The share is the one amount this component computes by division, and it is rounded at the larger of the scale Shopify wrote and the currency's minor units: a discount written "1.0" over two of three units is "0.67", not "0.7".
- `DetailFetched` is true when all three inlined values are lists, empty or not, and false when any is nil: a shape projects an absent field as nil. Then the three parts are nil and there are no lines. `LinesComplete` is `len(refundLineItems) < linesPage`, and false when `linesPage` is not above zero. The page is the generated field's own (`refundLinesPage`); no number is written here.
- What the refund formulas assume of Shopify, each a live check before the part is granted (the record's L4, L12 and L13; Task 2's live tests log both):
  1. On a store whose prices include tax, a refunded shipping line's `subtotalAmountSet` holds its tax. So taking the shipping out of the money refunded takes its tax with it, and a figure reads `TaxRefunded` as zero for such an order.
  2. An order adjustment's `amountSet` and `taxAmountSet` carry the sign Shopify wrote and can be negative; `taxAmountSet` is summed into `TaxRefunded` with that sign, taking a positive tax to be tax that went back to the buyer.
  If a live check says otherwise, what changes is `refundFactOf` and the read of `tax_refunded` in the derivation, `storeStatsDefinition` (raised, so every store is rebuilt), the tests `TestARefundRowBecomesItsFactAndItsLines` here, `TestAnAmountOnlyRefundCountsItsAmountLessItsTax` and `TestARefundOfAnOrderWhosePricesIncludeTaxKeepsItsTax` in Task 13, and the fixture orders `amount-only-refund`, `restocking-fee` and `tax-inclusive-shipping-refund` of Task 19 with the reference calculator's reading of them.
- A row is `rowUnreadable`, with an error that names the field and never a value, when it has no `gid`, `storeId` or `createdAt`, no date, an amount `money.Parse` refuses, or a quantity that is not a whole number. Quantities are narrowed through `core/num`.
- The gate. Every field the four shapes project from a row an erasure reaches (`order`, `orderLineItem`, `refund`) must be one the erasure keeps, by its own answer: `shopify.ErasureDecision(concept, field)` is `kept as structure` (the fields a mirror row carries for itself: `storeId`, `gid`, `updatedAt`, `versionSource`, `syncedAt`, `versionReadAt`, `deleted`, `parentGid` on a child, and `accountId`), `kept by kind` or `kept by the list`. `row.id` and `row.createdAt` are the row's own and are not asked. One answer is admitted by name for one field: `kept, scrubbed on a custom item` for the line's `title`, the one field the facts read that a custom item loses, which the check repairs (below). One field is excepted, by name: `email`, which is read to decide wholesale and never stored. Any other answer fails the gate, `scrubbed` and `no such field` included. A field the erasure would scrub cannot be folded into a fact, because nothing would take it out again but the check. The shop and the store rows are outside the gate: a customer's erasure reaches neither.
- What an erasure does that the gate cannot see is said here and handled by the check (Task 16): an erased order's `email` becomes the erasure's marker, so the order is retail; a line with no product is a custom item, whose title the erasure scrubs whatever its keep-list says, so the line's title and product key become the marker's.

- [ ] **Step 1: Write the failing tests**

```go
func TestSoldOfIsFivePointOne(t *testing.T) {
	for _, c := range []struct {
		q0, q1, qr           int
		price, allocated     string
		currency             string
		sold                 int
		gross, discounts     string
	}{
		{2, 2, 0, "40.00", "8.00", "EUR", 2, "80.00", "8.00"},
		{3, 1, 1, "10.00", "3.00", "EUR", 2, "20.00", "2.00"}, // one refunded, one removed by an edit
		{3, 2, 0, "9.99", "1.00", "EUR", 2, "19.98", "0.67"},
		{3, 2, 0, "10.0", "1.0", "EUR", 2, "20.0", "0.67"},    // three units, two kept, a discount written with one decimal: cents, not "0.7"
		{3, 2, 0, "1000", "100", "JPY", 2, "2000", "67"},      // no decimals to round at
		{3, 2, 0, "1.000", "0.100", "KWD", 2, "2.000", "0.067"},
		{2, 1, 0, "5.00", "0.05", "EUR", 1, "5.00", "0.02"},   // half to even
		{2, 0, 2, "5.00", "1.00", "EUR", 2, "10.00", "1.00"},  // refunded in full: still sold
		{2, 0, 0, "5.00", "1.00", "EUR", 0, "0.00", "0.00"},   // removed by an edit
		{0, 0, 0, "5.00", "1.00", "EUR", 0, "0.00", "0.00"},   // no units: nothing is divided, and no discount is counted
		{1, 1, 1, "5.00", "0.00", "EUR", 1, "5.00", "0.00"},   // never more than were ordered
	} { /* soldOf(...) equals sold, gross, discounts as strings */ }
}

func TestAnOrderRowBecomesItsFact(t *testing.T)                    // shipping "12.50", tax "4.20", no duties ("0"), buyer key, currency, the three Shopify totals, Email kept
func TestTaxIncludedInPricesIsNotAddedAgain(t *testing.T)          // taxesIncluded true: Tax is "0.00"
func TestAnOrderIsDatedByProcessedAtElseItsCreation(t *testing.T)
func TestARowWithNoDateOrAnAmountThatDoesNotParseIsUnreadable(t *testing.T) // rowUnreadable; the error names the field and holds no "12,50"
func TestATombstoneIsRecognisedByDeletedAlone(t *testing.T)        // deleted true, versionSource "read", every amount still set: rowGone, with its ids and its Version
func TestATombstoneThatCarriesOnlyItsIdentityIsStillATombstone(t *testing.T) // storeId, gid, createdAt, deleted and nothing else, for each of the three: rowGone, never rowUnreadable
func TestALineRowBecomesItsFact(t *testing.T)                      // productGid as the key; "title:Inner tube" without one; two allocations of 5.00 and 3.00 are 8.00; Currency "EUR" from the unit price
func TestARefundRowBecomesItsFactAndItsLines(t *testing.T)         // M 48.56, shipping 5.00, tax 7.56, duties 0, one line: position 0, quantity 1, subtotal 36.00
func TestAnAdjustmentsTaxIsSummedWithItsSign(t *testing.T)         // an adjustment whose taxAmountSet is "-0.42" beside a line tax of "7.56": TaxRefunded "7.14"
func TestARefundWhoseDetailWasNotReadSaysSo(t *testing.T)          // the three values nil: DetailFetched false, the three parts nil, no lines
func TestARefundReadWithNothingOnItIsFetched(t *testing.T)         // three empty lists: DetailFetched true, the three parts "0"
func TestARefundWithAFullPageOfLinesIsNotKnownToBeComplete(t *testing.T) // linesPage 50: 49 lines complete, 50 not; linesPage 0: never complete
func TestTheRefundLinesPageIsTheGeneratedOne(t *testing.T)         // refundLinesPage() is above zero and equals the Page of refundLineItems in generated.Types["refund"]
func TestAQuantityIsAWholeNumberOrTheRowIsUnreadable(t *testing.T) // 2.0 and json.Number("2") read 2; 2.5 does not
func TestTheVersionOfAFactIsTheRowsOwnWriteTime(t *testing.T)      // createdAt as a time.Time and as an RFC3339Nano string
func TestTwoFactsAreTheSameByContentNotByText(t *testing.T)        // "36.0" and "36.00" are the same; a title, a quantity, a refund line or a nil part that differs is not; Email is not compared

// erasure_gate_test.go
func TestEveryFieldTheFactsReadSurvivesAnErasure(t *testing.T)     // the fields of the four shapes, read from dsl/shopify/overlay/shapes.memql (the test fails when it finds a shape with no field): each but email and row.* is kept as structure, by kind or by the list in ErasureDecision's answer, and orderLineItem.title is "kept, scrubbed on a custom item"; a failure names the shape, the field and the erasure's answer
func TestTheGateFailsOnAFieldAnErasureScrubs(t *testing.T)         // the same check over a copy of the order shape's list with "note" added fails and names note ("scrubbed"), and one with "noSuchField" added fails and names it ("no such field")
func TestTheFactFunctionsReadNoKeyOutsideTheirShapes(t *testing.T) // a recording map: every key orderFactOf, lineFactOf and refundFactOf read is a field of its shape
```

- [ ] **Step 2:** Run `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 ./component/storestats/`. Expected: FAIL (the package does not exist).
- [ ] **Step 3:** Implement. `doc.go` is the package comment, in the manner of `component/emailstats/doc.go`: why a side table, why through the engine, what a version is, why a retired row's fact is kept, what of a buyer a fact holds and what an erasure does to it.
- [ ] **Step 4:** Run the same command and `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 -run 'TestEveryPayloadNarrowingCarriesAnAnswer' .`. Expected: PASS.
- [ ] **Step 5: Commit** `storestats: the rules that turn a mirrored order, line and refund into a fact`.

### Task 11: The provider, its rollup builtin and their gates (U7)

**Independent of:** Task 20.

**Files:**
- Create: `component/storestats/provider.go`, `component/storestats/integration.go`, `component/storestats/rollup.go`, `component/storestats/tx.go`, `component/storestats/tx_test.go`, `component/storestats/main_dbschema_test.go`, `component/storestats/harness_db_test.go`, `component/storestats/provider_db_test.go`
- Modify: `dsl/shopify/overlay/builtins.memql`, `dsl/shopify/overlay/automations.memql`, `component/auth/maintenance_actor.go`, `component/automations/maintenance_actor_gate_test.go`, `component/automations/strict_automation_boot_test.go` (`shippedAutomationCount`, plus one, with its comment), `app/plugins_core.go` (the blank import, beside `component/emailstats`), `scripts/ci/db-gated-packages.sh` (the tree and a paragraph for it), `test/fixtures/pipelines/delivery-after-bootstrap.yaml` (the same list of trees)
- Modify, at the repository root: `module_taxonomy_test.go` (`"storeStats": kindComponent`, with its reason), `call_origin_conformance_test.go` (the allowlist entry for `component/storestats`), `memql-package.yaml` (`select.dbGated`, the same list of trees)
- Regenerate: the platform graph; the automation corpus file `component/automations/steps/testdata/automation_corpus/shopify/storeStatisticsRollupSweep.json` with `go test ./component/automations/steps/ -run TestAutomationCorpusRuns -update`

**Interfaces:**
- Consumes: Task 7's `store_sales_store`; Task 9's query `storeStatisticsStores()`, whose rows hold `id`, `domain`, `name`, `isDevelopment`, `scopesGranted`, `uninstalledAt`, `redactedAt`, `health`.
- Produces, in the DSL: `builtin storeStatisticsRollup { }` with `@executor("integration.storeStats.rollup")`, an `@description`, and no `@sdk`; `automation storeStatisticsRollupSweep` with `@trigger(schedule="15 * * * * *")` whose one step is `roll := builtin storeStatisticsRollup()`; `storeStatisticsRollupSweep` in `maintenanceAutomations` with its reason.
- Produces, in Go:

```go
type Engine interface {
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
}

// Integration carries the engine, the database getter, the logger, and nil-safe seams later tasks set in NewIntegration:
//   now          func() time.Time
//   storeState   func(ctx context.Context, store map[string]any, now time.Time) (outcome string, err error)                         // Task 14; nil keeps every store
//   walkStore    func(ctx context.Context, store map[string]any, generation int64, now time.Time, report *RollupReport) error     // Task 13; nil walks nothing
//   wholesaleFor func(ctx context.Context, storeIDs []string) (map[string]wholesaleState, error)                                  // Task 15; nil means no store is enabled
type Integration struct{ /* ... */ }

func NewIntegration(engine Engine, bunDB func() *bun.DB, logger *slog.Logger) *Integration
func (i *Integration) IntegrationName() string                     // "storeStats"
func (i *Integration) Capabilities() []memql.IntegrationCapability // "rollup" here; Tasks 16 and 17 add theirs

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
	Rebuilt    []string       `json:"rebuilt,omitempty"` // Task 14
	Removed    []string       `json:"removed,omitempty"` // Task 14
}

func walkContext(ctx context.Context) context.Context // the package's one internal-origin stamp, with a fresh, roots-only read
func runRollup(ctx context.Context, i *Integration, now time.Time) (RollupReport, error)

// tx.go: the one way this package writes.
const anyGeneration int64 = -1 // for a write that is right whatever the store's generation: its own row, a rebuild, a batch of a delete

var errRebuilt = errors.New("storestats: the store was rebuilt after this page was read")

func lockWrites(ctx context.Context, tx bun.Tx) error // LOCK TABLE store_sales_store IN SHARE ROW EXCLUSIVE MODE
func storeGeneration(ctx context.Context, db bun.IDB, storeID string) (int64, error)

// writeTx runs fn in one transaction that begins with lockWrites and then reads the store's generation under the lock.
// When generation is not anyGeneration and is not the one stored, fn is not run, nothing is written and errRebuilt is answered.
func writeTx(ctx context.Context, db *bun.DB, storeID string, generation int64, fn func(ctx context.Context, tx bun.Tx) error) error

// readTx runs fn in one read-only transaction. It takes no lock.
func readTx(ctx context.Context, db *bun.DB, fn func(ctx context.Context, tx bun.Tx) error) error
```

Decisions:
- Registration: the package registers itself with the engine under the name `storeStats` from its `init` in `provider.go`, the way `component/emailstats` registers, with the name as a string literal there so the taxonomy gate sees it. It is linked on every node type, because the builtin is declared in a tree every binary loads.
- `handleRollup` admits one caller: `actor.UserId == "system:maintenance:storeStatisticsRollupSweep"` and `actor.IsClusterOwner()`, checked before anything is read; everybody else, a cluster owner included, gets "store statistics rollup requires the engine maintenance principal". One run per process (an atomic flag, as the email rollup keeps); a second answers nothing.
- The run, in this task: every page of `storeStatisticsStores()` under `walkContext`, each store once, in bare-id order. For each: its `store_sales_store` row is made if there is none (`writeTx` with `anyGeneration`, an insert that does nothing on conflict, the writer `ensureStore`); `storeState` is called when set, the store is added to `Rebuilt` or `Removed` as it answers, and a removed store is left; then the store's generation is read, after `storeState` has answered, since `storeState` may itself start a rebuild, and before any page of the store is asked; then `walkStore` is called when set, with that generation. One store's failure is logged with its id and the next store still runs; the run's error joins them. `errRebuilt` is not a failure: the store is left for the next run.
- One lock rule, and one place that opens a writing transaction. `lockWrites` is the lock `component/sitetraffic/rollup.go` takes on its own table: it serialises two nodes' writers and blocks no reader. `writeTx` takes it first, whatever is written after, the wholesale pass's and the check's transactions included. No engine call is made inside `writeTx`: what a transaction needs from the engine is read before it begins. The generation closes a race the lock cannot: a page read before a rebuild and written after it would write old facts over a store whose watermarks the rebuild had set back, and nothing would read the rows between again.
- The allowlist entry in `call_origin_conformance_test.go`: server-initiated, under the two maintenance principals, each checked before its walk; one stamp, in `walkContext`, for the `@serverOnly` reads and the verdict; the package writes only its own tables.
- The CI budget (A30). Every database-gated tree runs under a per-package alarm from `scripts/ci/db-test-budgets.yaml`; a tree no class names falls in the default class, 180 seconds, on a runner the file measures at about 1.9 times a laptop's time. `component/storestats` starts there, because nothing can be measured before its suite exists; its suite boots a real engine for most cases, as the mirror's do, whose suites the file measures at 2.2 to 2.5 times a laptop. Task 19, the last task that adds to the package, measures it and decides whether it keeps the default class; this task changes neither the budgets file nor its pinned test (`scripts/ci/pipeline_db_tests_test.py`).

- [ ] **Step 1: Write the failing tests**

`harness_db_test.go` boots one engine per process as `component/emailstats/rollup_db_test.go` does, registers this package's provider on it, and offers: `principalCtx(t, "storeStatisticsRollupSweep")`; `newStore(t) string` (a fresh store id and its store row, written by `createStore` under an owner); `storeRow(t, storeID) map[string]any` (the store's `store_sales_store` row, or nil); and `storeSalesQueries(t) func() []string`, which adds a `bun.QueryHook` to the handle the provider reads through and answers the statements naming `store_sales_` since it was last asked. Every assertion is about the store the test made.

```go
// tx_test.go
func TestEveryWritingTransactionBeginsWithTheLock(t *testing.T) {
	// Not a database test. It parses this package's Go files that are not tests and requires:
	//   - RunInTx and BeginTx are named in writeTx and readTx and nowhere else;
	//   - the first statement writeTx runs in its transaction is lockWrites;
	//   - every function that calls NewInsert, NewUpdate, NewDelete or NewRaw takes a bun.Tx: none writes through a *bun.DB;
	//   - the functions it found writing are exactly the list written in this test. In this task: ensureStore.
	// A task that adds a writer adds its name here, which is the decision that it goes through writeTx.
}
// provider_db_test.go, database lane
func TestAWriterWaitsForTheLock(t *testing.T)                              // a transaction holds LOCK TABLE store_sales_store IN SHARE ROW EXCLUSIVE MODE: a writeTx started meanwhile runs its body only after that one ends, and a readTx does not wait
func TestAWriteUnderAnOlderGenerationWritesNothing(t *testing.T)           // the store's generation is raised between a read of it and a writeTx handed the old one: errRebuilt, the body is not run; anyGeneration runs it
func TestTheRunKeepsARowForEveryStoreAndListsEachOnce(t *testing.T)        // three new stores and the stores page lowered to two (storesPageSize, a variable): each of the three is handed to a recording walkStore once and has one store_sales_store row; a second run adds none
func TestOneStoresFailureLeavesTheNextStoreWalked(t *testing.T)            // a walkStore seam that fails for the first store: the second is still handed to it, and the run's error names the first
func TestOnlyTheRollupsPrincipalRunsIt(t *testing.T)                        // a cluster owner, an automation's reader actor and another maintenance principal are refused; no query is asked
func TestTwoRunsAtOnceInOneProcessAreOne(t *testing.T)
func TestTheProviderOffersWhatItsBuiltinsName(t *testing.T)                 // not database: the registered name is storeStats and every integration.storeStats.* executor in the tree has its capability
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/storestats/`. Expected: FAIL to compile, then FAIL.
- [ ] **Step 3:** Implement, then `make sdk-gen`, `make platform-graph`, and write the corpus file with `-update`.
- [ ] **Step 4:** Run the same command; `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/auth/ ./component/automations/... ./scripts/cidb/ ./scripts/ci/`; `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -tags bff -p 1 -count=1 ./app/`; `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 -run 'TestOnlyAllowlistedPackagesStampInternalOrigin|TestEveryRegisteredPluginIsClassified|TestEveryPayloadNarrowingCarriesAnAnswer|TestDSLCallStringsDoNotUseGoQuoting|TestEngineManifest|TestActivatedDeliveryManifest' .`; the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `storestats: a provider whose rollup lists every store each minute, and one way to write`.

### Task 12: The walk in windows (U7)

**Independent of:** Task 20.

**Files:**
- Create: `component/storestats/walk.go`, `component/storestats/walk_test.go`, `component/storestats/walk_db_test.go`
- Modify: `component/storestats/harness_db_test.go`, `component/storestats/tx_test.go` (the writer `keepFloor`)

**Interfaces:**
- Consumes: Task 11's `Engine`, `Integration`, `walkContext(ctx context.Context) context.Context` and `writeTx(ctx context.Context, db *bun.DB, storeID string, generation int64, fn func(ctx context.Context, tx bun.Tx) error) error` with `anyGeneration`; Task 7's `store_sales_store.history_floor`; Task 9's queries `storeStatisticsOrdersChanged(storeId, since)`, `storeStatisticsLinesChanged(storeId, since)`, `storeStatisticsRefundsChanged(storeId, since)` and `storeStatisticsStoreAt(storeId, at)`; Task 10's `instantOf(v any) (time.Time, bool)`.
- Produces:

```go
const (
	sourceOrder  = "order"
	sourceLine   = "line"
	sourceRefund = "refund"

	watermarkOverlap = 5 * time.Minute
	walkPageSize     = 250
	maxPagesPerRun   = 40
	maxReadsPerRun   = 400
	defaultWindow    = 24 * time.Hour
	maxWindow        = 32 * 24 * time.Hour
	minWindow        = time.Minute
	floorProbes      = 14 // the clock less 1, 2, 4, ... 8192 days
)

// floorForTests is zero outside tests. When a test sets it, it is every store's floor and no store row is asked for.
var floorForTests time.Time

type walkWindow struct {
	Through time.Time     // rows written up to here are read; zero is not walked yet
	Width   time.Duration
}

type walkPage struct {
	Rows     []map[string]any // empty for a window that held no row
	Through  time.Time        // where the walk stands once this page is written
	Width    time.Duration    // the width to keep with it
	CaughtUp time.Time        // not zero on the last page of a read to the clock: the clock that page was asked by
}

// floorOf answers an instant at which the store's row did not exist yet.
func floorOf(ctx context.Context, engine Engine, storeID string, now time.Time) (time.Time, error)

// ensureFloor answers the store's kept history_floor, or finds it with floorOf and keeps it.
func ensureFloor(ctx context.Context, i *Integration, storeID string, now time.Time) (time.Time, error)

// walkSource reads one source of one store in creation order from w, or from floor when w.Through is zero, in bounded
// windows, at most budget pages that held rows. visit is handed every page, and every window that held no row, so the
// caller can keep where the walk stands. next is where it stands when it returns; caughtUp is the clock the last page
// of a read to the clock was asked by, and zero when the read stopped behind.
func walkSource(ctx context.Context, engine Engine, query, storeID string, floor time.Time, w walkWindow, clock func() time.Time, budget int,
	visit func(page walkPage) error) (next walkWindow, caughtUp time.Time, err error)

func isPageTimeout(err error) bool
```

Decisions:
- Where a store's history begins. No date is compiled in. No mirror row of a store is older than its store row, so the floor is an instant before the store row was first written. A row does not say when that was: `row.createdAt` is the write time of its newest version, and a store row is rewritten at least daily. So `floorOf` asks the engine for the store row as of the clock less 1, 2, 4, 8 and so on days (`storeStatisticsStoreAt`), `floorProbes` times at most, and answers the first instant at which there was no row; if a row answers at every one, the last instant asked. That is at most twice the store's age too early, which costs a few reads of empty windows and loses nothing. `ensureFloor` does it once and keeps it in `history_floor` (`writeTx` with `anyGeneration`, the writer `keepFloor`); a rebuild leaves it.
- The floor would be wrong if old versions of the store row were ever pruned: the probe would find the oldest version kept, the floor would move forward, and rows written before it would never be walked. No retention is declared on `v1:shopify:store`, and a test fails when one is.
- `walkSource`:
  - `since` is `Through` less the overlap, or `floor` less the overlap when `Through` is zero. `until` is `since` plus `Width`.
  - When `until` is within the overlap of the clock, the read is to the clock: `paginate(query <name>(storeId: <q>, since: <q>), 250)`. Its pages move `Through` to the newest `createdAt` read, never past the clock; its last page is caught up at the clock that page was asked by.
  - Otherwise the read is of a past window, as of its end: the same call wrapped as `asOf(<call>, <until>)`, the form `walkQuery` in `component/emailstats/rollup.go` writes. Its last page sets `Through` to `until`, doubles `Width` up to `maxWindow`, and the walk goes on to the next window in the same call. A window that held no row is handed to `visit` as a page with no rows.
  - A page that outlasts a read halves `Width` down to `minWindow` and ends this source for the run, with no error: the halved width is in `next` and the caller keeps it. At `minWindow` it is the source's error. A read to the clock that times out sets `Width` to half the span it tried. The test for "outlasts a read" is the email walk's `isPageTimeout`, copied here with its test: the tree keeps this reading beside each walk it serves (`component/retention` has its own).
  - Only pages that held rows count against `budget`; every read counts against `maxReadsPerRun`. A cursor the engine hands back unchanged is an error, never a loop.
  Why windows: a read costs every version of the concept written since `since`, on the whole cluster (`component/memql/latest_scan_keys.go`), and a first fill, a rebuild and the check all start at the floor. Without a bound a large mirror's first page outlasts the statement timeout on every run, as the email walk's did before it was bounded.
- This task writes no fact and is not yet called by the run: Task 13 hands `walkSource` a `visit` that folds.

- [ ] **Step 1: Write the failing tests**

The harness gains `mirror(t, concept, storeID, gid string, payload map[string]any)`: a raw insert under `auth.ContextWithConnectorActor(ctx, "shopify")`, at the id `shopify.MirrorRowID(storeID, gid)`, with `deleted: false` unless given.

```go
// walk_test.go: a scripted engine that records each call and answers pages, timeouts and cursors as told
func TestAPageThatOutlastsAReadHalvesTheWindow(t *testing.T)                // the statement timeout on the first page of a past window: next.Width is half, Through has not moved, no error; at minWindow it is an error
func TestAReadToTheClockThatTimesOutHalvesTheSpanItTried(t *testing.T)
func TestACursorTheEngineHandsBackUnchangedIsAnError(t *testing.T)
func TestOnlyPagesThatHeldRowsCountAgainstTheBudget(t *testing.T)           // budget 1: ten empty windows and one page of rows are read; maxReadsPerRun bounds the empty ones
func TestAWindowThatHeldNoRowIsStillHandedOn(t *testing.T)                  // visit gets a page with no rows, the window's end as Through and the doubled Width
func TestTheFloorIsTheFirstInstantWithNoStoreRow(t *testing.T)              // a store first written 100 days ago, rewritten yesterday: the row answers as of 1 to 64 days back and not 128: the floor is the clock less 128 days. One written an hour ago: the clock less a day
func TestTheFloorOfAStoreOlderThanEveryProbeIsTheLastInstantAsked(t *testing.T)
func TestIsPageTimeoutReadsTheStatementTimeoutAndNothingElse(t *testing.T)  // the email walk's cases
func TestTheStoreConceptKeepsEveryVersion(t *testing.T)                     // dsl/shopify/overlay/concepts.memql: the store concept carries no @retention; the message says the walk's floor depends on it
// walk_db_test.go, database lane: a visit that records what it is handed
func TestAPastWindowIsReadAsOfItsEndAndTheNextStartsThere(t *testing.T)     // a row rewritten after the window: the window's read hands its older version, a later one the newer
func TestAnEmptyStretchOfHistoryIsCrossedInWidenedWindows(t *testing.T)     // floorForTests a year back, one row today: found in one call, within maxReadsPerRun
func TestARowCommittedBehindTheWatermarkIsReadByTheNextRun(t *testing.T)    // O7: a row whose createdAt is two minutes before w.Through
func TestAWalkPagesEveryMirroredRowOnceInCreationOrder(t *testing.T)        // O7: 600 orders: three pages, 600 rows, each gid once, createdAt never decreasing, no cursor repeated
func TestTheFloorIsFoundOnceAndKept(t *testing.T)                           // ensureFloor twice on a real store: history_floor is set, is before the store row's first write, and the second call asks no query
func TestAStoreIsWalkedFromItsFloorNotFromACompiledDate(t *testing.T)       // floorForTests unset, a store row and an order written through the harness: the floor comes from the store row, the order is handed to visit, and no file of the package that is not a test holds a date literal
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/storestats/ -run 'Window|Timeout|TimesOut|Cursor|Budget|Floor|EveryVersion|EmptyStretch|BehindTheWatermark|PagesEveryMirroredRow'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, then the package whole in the database lane. Expected: PASS.
- [ ] **Step 5: Commit** `storestats: a store's rows are read in bounded windows from before its store row was first written`.

### Task 13: The fold and the derivation (U7)

**Independent of:** Task 20.

**Files:**
- Create: `component/storestats/fold.go`, `component/storestats/derive.go`, `component/storestats/fold_db_test.go`
- Modify: `component/storestats/integration.go` (`NewIntegration` sets `walkStore`), `component/storestats/rollup.go`, `component/storestats/harness_db_test.go`, `component/storestats/tx_test.go` (the writers `foldPage`, `deriveOrders`, `keepWatermark`, `keepWalked`)

**Interfaces:**
- Consumes: Task 7's fact tables and `store_sales_watermark`; Task 10's `orderFactOf(row map[string]any) (OrderFact, rowKind, error)`, `lineFactOf(row map[string]any) (LineFact, rowKind, error)`, `refundFactOf(row map[string]any, linesPage int) (RefundFact, rowKind, error)`, `refundLinesPage() int`, `soldOf(quantity, currentQuantity, refundedQuantity int, unitPrice, allocated money.Amount, currency string) (sold int, gross, discounts money.Amount)` and the kinds `rowLive`, `rowGone`, `rowUnreadable`; Task 11's `Integration`, its seams `walkStore` and `wholesaleFor`, `RollupReport`, `writeTx`, `errRebuilt` and `walkContext`; Task 12's `walkSource(ctx, engine, query, storeID, floor, w walkWindow, clock, budget, visit func(page walkPage) error) (next walkWindow, caughtUp time.Time, err error)`, `ensureFloor(ctx context.Context, i *Integration, storeID string, now time.Time) (time.Time, error)`, `walkWindow`, `walkPage`, `maxPagesPerRun` and the three source names.
- Produces:

```go
type pageResult struct {
	Facts, Unreadable int
	Orders            []string  // the order gids the page touched
	LastRecorded      time.Time
}

// foldPage writes one page's facts inside tx: guarded upserts, a retired row kept as a fact marked deleted, then deriveOrders.
func foldPage(ctx context.Context, tx bun.Tx, storeID, source string, rows []map[string]any, wholesale func(email string) bool) (pageResult, error)
func deriveOrders(ctx context.Context, tx bun.Tx, storeID string, orderGIDs []string) error

// walkOneStore is what walkStore is set to: the store's three sources, each from its watermark.
func (i *Integration) walkOneStore(ctx context.Context, store map[string]any, generation int64, now time.Time, report *RollupReport) error
```

Decisions:
- `walkOneStore`: the store's floor (`ensureFloor`); then order, line and refund with `walkSource`, each from its `store_sales_watermark` row, 40 pages each; then the store's `walked_through` is set to the least of its three sources' `walked_through`, null while any is null. A store whose `purging` is set is not walked (Task 14 deletes first).
- Each page, and each empty window, is one `writeTx` handed the generation the run read before the store's first page was asked: `foldPage`, the watermark (`through` by `GREATEST`, `window_seconds`, `walked_through`), and the store's `last_change_at` by `GREATEST` with the page's `LastRecorded`. A rebuild that lands after the page was read raises the generation, so the page writes nothing, the watermark does not move and the store is left for the next run, which starts where the rebuild left it.
- `foldPage`, for a live row: an upsert with `ON CONFLICT ... DO UPDATE ... WHERE excluded.<row>_version >= <table>.<row>_version`, setting every column the fact holds and `deleted` false. It never touches a derived column or `checked_at`. A refund's lines are replaced whole with the refund, when the refund's own write took effect.
- `foldPage`, for a tombstone: the fact is not removed. It is written `deleted`, with the tombstone's version and `recorded_at`, under the same guard; a fact that exists keeps its other columns, and one that does not is inserted with zeros, empty strings and its version as its dates, none of which is ever read. The row then stands in the table as the newest version, and the guard refuses every older one: the live version a later window of a first fill or of the check reads again, or one another node folds after this. A deleted refund's lines are removed with it, since they have no version of their own and are only ever written with their refund. A line's tombstone finds its fact by `(store_id, line_gid)` and a refund's by `(store_id, refund_gid)`, the tables' keys; the order the fact named is among the orders the page touched.
- A row that comes back after a tombstone is a live row with a newer version: the upsert writes it and clears `deleted`.
- An unreadable row writes nothing, is counted, and is logged once with the store, the source, the gid and the field, never a value. An order's `wholesale` is `wholesale(fact.Email)`, false when the function is nil.
- Facts follow rows one for one. The mirror retires only the order (a delete notice, or, under a grant that reads every order, two clean empty answers); `orderLineItem` and `refund` have no notice and no sweep of their own, so a retired order's lines and refunds stay live rows, and their facts stay live. They count nowhere, because every figure reaches a line or a refund through its order's fact and leaves a deleted order out (Task 18). The fold treats a line's or a refund's tombstone the same way all the same.
- `deriveOrders`, for the orders a page touched, in the page's transaction, over facts that are not deleted: each line's `refunded_quantity` is the sum of the refund-line quantities naming it, over refunds that are not deleted; `sold_quantity`, `gross` and `discounts` come from `soldOf` with the line's `currency`, computed in Go and written back (the rounding is `money`'s, never SQL's); the order's `gross`, `discounts` and `units` sum its lines that are not gift cards, `line_count` and `lines_version` are over all its lines, and `lines_complete` is the sum of `quantity` over all its lines being at least `expected_quantity`; a fetched refund's `gift_card_refunded` sums the subtotals of its lines that name a gift-card line, `units_returned` the quantities of the others, and `item_refunds` is `money_refunded - shipping_refunded - duties_refunded - gift_card_refunded`, less `tax_refunded` unless the order's `taxes_included`; all three are null for a refund whose detail is not read.

- [ ] **Step 1: Write the failing tests** (database lane)

The harness gains reads of the store's rows in each `store_sales_*` table, and `run(t)`, one run of the rollup under its principal.

```go
func TestAnOrderWithThreeLinesWritesItsFacts(t *testing.T)                  // 2 x 40.00 less 8.00, 1 x 19.99, a 25.00 gift card, subtotalLineItemsQuantity 4: gross "99.99", discounts "8.00", units 3, line_count 3, lines_complete true
func TestALineRefundIsWrittenOnItsOwnDayAndTheOrdersDayKeepsItsSale(t *testing.T) // refunded_at is the refund's; the line's refunded_quantity 1, sold_quantity still 2; item_refunds "36.00"
func TestAShippingRefundIsNoRefundOfItems(t *testing.T)                     // shipping_refunded "5.00", item_refunds "0.00"
func TestAnAmountOnlyRefundCountsItsAmountLessItsTax(t *testing.T)          // M 12.10, an adjustment whose tax is 2.10, no line: item_refunds "10.00"
func TestARefundOfAnOrderWhosePricesIncludeTaxKeepsItsTax(t *testing.T)     // taxes_included: item_refunds is M less shipping only
func TestAGiftCardRefundIsNoRefundOfItems(t *testing.T)                     // gift_card_refunded "25.00", units_returned 0
func TestAnEditThatRemovesAUnitLowersGrossAndWritesNoRefund(t *testing.T)   // currentQuantity 1 of 2, no refund line: sold 1, gross "40.00"
func TestADiscountWrittenWithOneDecimalIsSharedInCents(t *testing.T)        // three units, two kept, allocated "1.0" in EUR: the line's discounts "0.67"
func TestAnEditThatAddsALineRaisesGross(t *testing.T)
func TestACancelledOrderIsMarkedAndKeepsItsFact(t *testing.T)
func TestATombstoneMarksItsFactDeletedAndKeepsIt(t *testing.T)              // order; line; refund, whose refund-line facts are gone: each fact is still there, deleted true, at the tombstone's version
func TestATombstoneMetInALaterWindowThanTheLiveVersionLeavesTheFactDeleted(t *testing.T) // a first fill in two windows: the first writes the order live, the second its tombstone; then the first window is read again: the fact is deleted and its version is the tombstone's
func TestAnOlderLiveVersionArrivingAfterTheTombstoneChangesNothing(t *testing.T) // foldPage with the live row whose createdAt is before the tombstone's, as another node would hand it: deleted stays true, no column moves
func TestATombstoneOfARowNeverFoldedIsKeptAsADeletedFact(t *testing.T)      // only identity fields: a fact is inserted deleted, and the live row arriving after it with an older version is refused
func TestTheLinesOfARetiredOrderKeepTheirFactsLive(t *testing.T)            // the order's fact is deleted; its lines' and refunds' are not
func TestADerivationLeavesDeletedFactsOut(t *testing.T)                     // a deleted line and a deleted refund of a live order: gross, units, line_count and refunded_quantity are over the others
func TestAnOrderThatComesBackAfterATombstoneIsWrittenAgain(t *testing.T)    // deleted is false again, at the newer version
func TestAnOlderVersionArrivingAfterANewerOneChangesNothing(t *testing.T)   // foldPage with a row whose createdAt is earlier than the fact's version
func TestTwoRunsAtOnceWriteTheSameFacts(t *testing.T)
func TestARebuildLandingBetweenAPagesReadAndItsWriteEndsWithTheFullFacts(t *testing.T) // 600 orders; a seam raises the generation and clears the watermarks after the second page was read and before it is written: that page writes nothing, the run leaves the store, and runs after it end with 600 order facts
func TestAStoreIsNotWalkedThroughUntilEverySourceReachedTheClock(t *testing.T)
func TestARefundWrittenByABulkLineHasNoDetailAndOneReadEmptyHasIt(t *testing.T) // Review Focus, through the real shape: an absent key reads nil, [] reads empty
func TestARowThatCannotBeReadIsCountedAndTheWalkGoesOn(t *testing.T)        // and the log line holds the gid and the field and no value
func TestTheWalkReadsNoFieldOutsideItsShapes(t *testing.T)                  // every key foldPage reads is a field of its shape
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/storestats/`. Expected: FAIL to compile, then FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command. Expected: PASS, `TestEveryWritingTransactionBeginsWithTheLock` with its longer list included.
- [ ] **Step 5: Commit** `storestats: a walk folds every store's orders, lines and refunds into facts, and keeps a retired row as a deleted fact`.

### Task 14: What the walk keeps about each store (U7)

**Independent of:** Task 20.

**Files:**
- Create: `component/storestats/store.go`, `component/storestats/store_db_test.go`
- Modify: `component/storestats/integration.go` (`NewIntegration` sets `storeState`, and the `verdictOf` seam), `component/storestats/rollup.go` (the run gives a removed store no new row, leaves for this run a store that answers `outcomePurging`, and clears `rebuilding_since`), `component/storestats/harness_db_test.go`, `component/storestats/tx_test.go` (the writers `keepStoreRow`, `rebuildStore`, `purgeBatch`, `forgetStore`, `clearRebuilding`)

**Interfaces:**
- Consumes: Task 11's `Integration` and its seam `storeState func(ctx context.Context, store map[string]any, now time.Time) (outcome string, err error)`, `walkContext(ctx context.Context) context.Context`, `writeTx(ctx context.Context, db *bun.DB, storeID string, generation int64, fn func(ctx context.Context, tx bun.Tx) error) error` with `anyGeneration`, and the `Rebuilt` and `Removed` lists of `RollupReport`; a store row as Task 9's `storeStatisticsStores()` answers it (`id`, `domain`, `name`, `isDevelopment`, `scopesGranted`, `uninstalledAt`, `redactedAt`, `health`); Task 9's `storeStatisticsShop(storeId)`, whose one row holds `id`, `createdAt`, `storeId`, `deleted`, `name`, `currencyCode`, `ianaTimezone`, `taxesIncluded` and `plan`; Task 5's `statsperiod.StartOfDay(y int, m time.Month, d int, loc *time.Location) time.Time`; Task 10's `instantOf(v any) (time.Time, bool)`; the mirror's `builtin shopifyStoreVerdict(storeId)`, whose one row holds `state`, `since` (RFC3339) and `reasons` (each with `code` and `sentence`).
- Produces:

```go
const storeStatsDefinition = 1

var deleteBatch = 5000 // rows a batched delete takes in one transaction; a variable so a test can lower it

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
	outcomePurging = "purging" // its facts are still being deleted; the walk leaves the store for this run
)

func (i *Integration) keepStore(ctx context.Context, store map[string]any, now time.Time) (outcome string, err error) // what storeState is set to

// rebuildStore marks a store for a rebuild inside tx: it raises generation, sets purging and rebuilding_since, clears walked_through,
// sets definition, and deletes the store's watermarks and its check row. It deletes no fact: purgeStore does, in batches.
func rebuildStore(ctx context.Context, tx bun.Tx, storeID string, now time.Time) error

// purgeStore deletes a purging store's facts, deleteBatch rows of each fact table a transaction, at most budget transactions.
// done is true when no fact is left and purging was cleared.
func purgeStore(ctx context.Context, i *Integration, storeID string, budget int) (done bool, err error)
```

Decisions:
- `keepStore` reads first (the shop row, the verdict) and writes after, in `writeTx` with `anyGeneration`: no engine call is made while the lock is held.
- A store with `redactedAt` set (D17): the run gives it no new `store_sales_store` row. One it still has is marked the way a rebuild marks it (`rebuildStore`, which raises the generation so no page read before it writes after it), its facts are deleted by `purgeStore`, and when none is left its row is deleted too (`forgetStore`). The answer is `outcomePurging` until then and `outcomeRemoved` after.
- A store whose `purging` is set has its facts deleted before anything else is done for it: `purgeStore` with a budget of 20 transactions a run, each one `writeTx` with `anyGeneration` that reads `purging` again under the lock and stops when another node has cleared it, then deletes up to `deleteBatch` rows from each of the four fact tables. A delete of a whole store in one transaction would hold the write lock, and every other store's walk, for as long as it took. Nothing is folded for the store while `purging` is set, so no batch can delete a fact written after the rebuild.
- Otherwise `store_sales_store` is written from the store row and the shop row (`storeStatisticsShop`): `store_domain`; `store_name` (the store row's `name`, else the shop row's); `currency`, `timezone` and `taxes_included` from a live shop row, left as they are when there is none; `development` is the shop row's `plan.partnerDevelopment` when there is a shop row, else the store row's `isDevelopment`, else null (D4); `all_orders_readable` is whether `scopesGranted` holds `read_all_orders`, the test the mirror makes in `stampOrdersFrom`.
- `history_since` is the store-local midnight of the first recorded day. `health.ordersFrom` is a day (`2025-09-12`): `history_since` is `statsperiod.StartOfDay` of that day in the store's zone. With no `ordersFrom` it is `StartOfDay`, in the store's zone, of the day of the store's oldest `ordered_at` among facts that are not deleted. With no zone, or one that does not load, the zone is UTC. It is written again on every run, so a zone that changes moves it.
- The verdict, from `verdictOf`: `verdict_state` and `verdict_reasons` (code and sentence only) are copied. `verdict_since` is when the store left `recording`: set to the reply's `since` when the stored state was `recording` or empty and the new one is not, kept while the state stays other than `recording`, and set to the reply's `since` when it returns to `recording`. A verdict that cannot be read writes an empty state and logs once a run.
- A rebuild by definition: `keepStore` calls `rebuildStore` in its transaction when the stored `definition` is below `storeStatsDefinition`, and answers `outcomeRebuilt`. A new row has `definition` 0, so a store's first fill is a rebuild with nothing to delete.
- The run clears `rebuilding_since` once the store's `walked_through` is within 5 minutes of the clock.

- [ ] **Step 1: Write the failing tests** (database lane). The harness gains `shopRow(t, storeID, currency, zone string, development bool)` (a raw insert of a `v1:shopify:shop` row under the connector actor) and a `verdictOf` stub.

```go
func TestTheShopsCurrencyZoneAndTaxSettingAreCopied(t *testing.T)
func TestAStoreWithNoShopRowKeepsNoCurrencyAndNoZone(t *testing.T)          // and keeps the ones it had when the shop row is retired
func TestADevelopmentStoreIsToldFromALiveOne(t *testing.T)                  // the shop row's plan; with no shop row, the store row's flag; with neither, null
func TestTheStoresNameAndDomainAreCopied(t *testing.T)                      // the store row's name, else the shop's
func TestAGrantWithoutAllOrdersIsRecorded(t *testing.T)                     // scopesGranted without read_all_orders: all_orders_readable false; with it, true
func TestOrdersSinceIsTheStoreLocalMidnightOfTheFirstRecordedDay(t *testing.T) // health.ordersFrom "2025-09-12": in Europe/Madrid 2025-09-11T22:00:00Z, in America/Los_Angeles 2025-09-12T07:00:00Z, with no zone 2025-09-12T00:00:00Z; with no ordersFrom, the local midnight of the oldest order's day, a deleted order left out
func TestTheVerdictIsCopiedAndWhenTheStoreLeftRecordingIsKept(t *testing.T) // recording, paused at t1, needs attention at t2: verdict_since stays t1; recording again at t3: t3
func TestAVerdictThatCannotBeReadIsNotKnown(t *testing.T)
func TestALowerDefinitionRebuilds(t *testing.T)                             // definition 0: rebuilding_since set, generation raised, cleared once the walk reaches the clock, and the facts are the same as before
func TestAStoresFactsAreDeletedInBatchesBeforeItIsWalkedAgain(t *testing.T) // deleteBatch 2, five order facts and their lines: purgeStore takes three transactions, no fact is written while purging is set, and the run after the last batch walks
func TestAPurgeAnotherNodeFinishedDeletesNothingMore(t *testing.T)          // purging cleared and a fact refilled between two batches: the next batch deletes nothing
func TestAStoreWhoseDataWasRemovedLosesItsRowsAndGetsNoNewOne(t *testing.T) // redactedAt: nothing of it in any of the seven tables, it is not walked, and the next run makes no row for it
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/storestats/ -run 'Shop|Development|NameAndDomain|AllOrders|OrdersSince|Verdict|Definition|Batches|Purge|DataWasRemoved'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, then the package whole in the database lane. Expected: PASS.
- [ ] **Step 5: Commit** `storestats: the walk keeps each store's currency, time zone, verdict and definition, and deletes a store's facts in batches`.

### Task 15: Wholesale and retail (U7)

**Independent of:** Task 20.

**Files:**
- Create: `component/storestats/wholesale.go`, `component/storestats/wholesale_db_test.go`
- Modify: `component/storestats/integration.go` (`NewIntegration` sets `wholesaleFor`), `component/storestats/rollup.go` (the run asks it once and continues a pass), `component/storestats/harness_db_test.go`, `component/storestats/tx_test.go` (the writers `startWholesalePass`, `wholesalePage`)

**Interfaces:**
- Consumes: Task 9's `wholesaleStatisticsGranted(storeIds)` (a row holds `id`, `storeId`, `buyerEmail`, `state`), `wholesaleStatisticsSettings(storeIds)` (a row holds `id`, `storeId`) and `storeStatisticsOrderBuyers(storeId, since)` (a row holds `id`, `createdAt`, `storeId`, `gid`, `deleted`, `email`); Task 11's `wholesaleState`, its seam `wholesaleFor func(ctx context.Context, storeIDs []string) (map[string]wholesaleState, error)`, `writeTx`, `errRebuilt` and `walkContext`; Task 12's `walkSource(ctx, engine, query, storeID, floor, w walkWindow, clock, budget, visit func(page walkPage) error) (next walkWindow, caughtUp time.Time, err error)` and `ensureFloor`; the `wholesale func(email string) bool` argument of Task 13's `foldPage`; `PackDomainDisabled(domain string) bool` in package `github.com/znasllc-io/memql/dsl`.
- Produces:

```go
const wholesaleDomain = "wholesale"

func foldEmail(s string) string                     // lower case, trimmed
func grantedDigest(granted map[string]bool) string  // hex SHA-256 of the sorted folded emails joined by "\n"; never empty
func (i *Integration) wholesaleOf(ctx context.Context, storeIDs []string) (map[string]wholesaleState, error)
func wholesalePass(ctx context.Context, i *Integration, storeID string, generation int64, st wholesaleState, clock func() time.Time, budget int) error
```

Decisions:
- O6: the component asks `PackDomainDisabled("wholesale")`. It is the loaders' own answer for this node, set at boot from the `v1:platform:packState` rows over the pack's declared default, and it is what `component/memql/module_registry.go` reports as loaded or mounted inert. The rows themselves (`ReadPackStates`) are the cluster's wish and can differ from a node until it restarts; the question here is whether the pack's queries exist on the node that runs the pass. When it answers true, no pack query is asked, every store's `wholesale_enabled` is false, and its digest, cursor and stored flags are left as they are.
- Otherwise, once a run: both reads are asked with both forms of every store's id (`<id>` and `v1:shopify:store:<id>`), paged to their end, and grouped by the bare id. A store is enabled when it has a settings row (D10). Its granted set is the folded `buyerEmail` of its granted entitlements.
- An order is wholesale when its store is enabled and `foldEmail(email)` is not empty and is in the granted set. An empty email matches nothing, an entitlement with an empty address included; neither does the marker an erasure leaves in an order's `email`, since no granted address is the marker. This is `foldPage`'s `wholesale` function for the run, so a new order by a granted buyer is wholesale when it is written.
- A pass starts when an enabled store's digest differs from the stored one, and when a store becomes enabled (its stored `wholesale_enabled` was false), whatever its digest: orders written while it was not enabled were written retail. Starting is one `writeTx` that stores the digest, sets `wholesale_enabled`, sets `wholesale_cursor` to the store's floor as RFC3339Nano and `wholesale_window_seconds` to a day. While the cursor is not empty the run continues a pass: `storeStatisticsOrderBuyers` through `walkSource` from the cursor at the kept width, at most 40 pages, each page one `writeTx` handed the store's generation, which sets `wholesale` true for its granted orders and false for the rest (a tombstone and a deleted fact are passed over), moves the cursor and keeps the width the walk learned; the last page empties the cursor. A set that changes mid-pass starts the pass again. A rebuild drops a pass with the watermarks: the facts it refills are judged as they are written.
- The email is compared in memory. No address, and nothing derived from one but the digest, reaches a table or a log.
- A store that stops being enabled keeps its flags; the read answers `wholesale: null` for it (Task 18).

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
func TestAPassKeepsTheWindowWidthItLearned(t *testing.T)                   // a page that outlasts a read halves wholesale_window_seconds, and the next run reads at that width
func TestAGrantedSetThatChangesMidPassStartsThePassAgain(t *testing.T)
func TestAPassPageReadBeforeARebuildWritesNothing(t *testing.T)            // the generation raised between a page's read and its write: no flag moves
func TestNoEmailReachesATableOrALog(t *testing.T)                          // after a run with a granted buyer and an unreadable row: no text or JSON column of any store_sales_ table, each cast to text, holds an "@", and neither does any line the walk's logger wrote
func TestTheDigestOfNoBuyersIsNotTheDigestNeverComputed(t *testing.T)      // not database
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/storestats/ -run 'Wholesale|Granted|Retail|Email|PackOff|Digest|SettingsRow|ItsCursor|EnabledAgain|APass'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, then the package whole in the database lane. Expected: PASS.
- [ ] **Step 5: Commit** `storestats: an order is wholesale when its buyer holds granted access now`.

### Task 16: The check, the rebuild and the check's state (U8)

**Independent of:** Task 20.

**Files:**
- Create: `component/storestats/check.go`, `component/storestats/check_db_test.go`
- Modify: `component/storestats/integration.go` (three capabilities), `component/storestats/rollup.go` (a check starts when a rebuild's walk has caught up), `component/storestats/harness_db_test.go` (`rewriteInPlace`), `component/storestats/tx_test.go` (the writers `startCheckPass`, `checkPage`, `orphanBatch`, `finishCheckPass`), `dsl/shopify/overlay/builtins.memql`, `dsl/shopify/overlay/automations.memql`, `component/auth/maintenance_actor.go`, `component/automations/maintenance_actor_gate_test.go`, `component/automations/strict_automation_boot_test.go` (plus one)
- Regenerate: the platform graph; the corpus file `component/automations/steps/testdata/automation_corpus/shopify/storeStatisticsCheckSweep.json`

**Interfaces:**
- Consumes: Task 12's `walkSource(ctx, engine, query, storeID, floor, w walkWindow, clock, budget, visit func(page walkPage) error) (next walkWindow, caughtUp time.Time, err error)` and `ensureFloor`; Task 13's `foldPage(ctx context.Context, tx bun.Tx, storeID, source string, rows []map[string]any, wholesale func(email string) bool) (pageResult, error)` and `deriveOrders(ctx context.Context, tx bun.Tx, storeID string, orderGIDs []string) error`; Task 11's `writeTx`, `readTx`, `storeGeneration`, `errRebuilt`, `walkContext` and the seam `wholesaleFor`; Task 9's `storeStatisticsOrdersChanged`, `storeStatisticsLinesChanged` and `storeStatisticsRefundsChanged`, each `(storeId, since)`; Task 14's `rebuildStore(ctx context.Context, tx bun.Tx, storeID string, now time.Time) error` and `deleteBatch`; Task 10's `orderFactOf`, `lineFactOf` and `refundFactOf`, and the `same` method of each fact.
- Produces, in the DSL: `builtin storeStatisticsCheck { }` (`@executor("integration.storeStats.check")`); `builtin storeStatisticsRebuild { storeId string! }` (`@executor("integration.storeStats.rebuild")`); `builtin storeStatisticsCheckState { storeId string! }` (`@executor("integration.storeStats.checkState")`); none carries `@sdk`; `automation storeStatisticsCheckSweep` with `@trigger(schedule="0 40 * * * *")`; `storeStatisticsCheckSweep` in `maintenanceAutomations`.
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

  `storeStatisticsCheckState` answers one node with id `checkState`: `{ storeId, passUnderWaySince, lastStartedAt, lastFinishedAt, cleanAt, compared, mismatched, repaired, disagreeing }`, the four counts being the last ended pass's, a time that never was being null.

Decisions:
- Who may call what, each checked in Go before the store id is read. `handleCheck` admits `system:maintenance:storeStatisticsCheckSweep` with the cluster-owner role and nobody else: "store statistics check requires the engine maintenance principal". `handleRebuild` and `handleCheckState` are for a person who is a cluster owner: a caller for whom `actor.IsClusterOwner()` and whose user id does not begin `system:maintenance:`. Anybody else, a maintenance principal included, gets "store statistics rebuild is for a cluster owner" or "store statistics check state is for a cluster owner". None of the three is in the generated clients; each is callable by name by any signed-in user, which is why each check is in Go.
- The rebuild compares the store id in its bare form (`memql.BareShortId`), so `<id>` and `v1:shopify:store:<id>` name one store, and refuses a store it holds no `store_sales_store` row for: "Store statistics has no store with that id." Then one `writeTx` with `anyGeneration`: `rebuildStore`. It answers `{ storeId, rebuildingSince }`. The walk deletes the facts in batches and refills (Task 14); when it clears `rebuilding_since` it calls `checkStore` for that store in the same run, and a rebuild is proven when that pass ends with `mismatched` 0.
- The check's state takes the store id the same way and refuses the same way. It reads `store_sales_check` in `readTx` and nothing else. It is how a cluster owner sees that a pass ended clean (S14), and the production walk's first step reads it.
- A pass is one walk of the store's three sources from its floor, in the walk's windows, comparing instead of trusting. Its state is the store's `store_sales_check` row: `pass_started_at`, and for each source a cursor that is empty (not begun), an RFC3339Nano instant (go on from there) or `done`, with the window width the walk learned for it, so a run that follows one that halved a window does not start again at a day and halve it again. A run carries each store's pass on by 40 pages a source. A pass that ended starts again at the next run.
- A store is checked only when its walk has caught up (`walked_through` within 10 minutes of the clock), it is neither rebuilding nor purging, and no wholesale pass is under way (`wholesale_cursor` empty); otherwise the run leaves it.
- Every transaction of a pass is a `writeTx` handed the generation read when the run began the store. A rebuild that lands meanwhile answers `errRebuilt`: the run leaves the store, and the rebuild has deleted the check row.
- The cut is `pass_started_at` less 5 minutes. A row written after the cut is the walk's to fold, not a mismatch: its fact, if any, is marked seen and not compared.
- For a row at or before the cut, the fact is recomputed from the row in hand (`orderFactOf`, `lineFactOf`, `refundFactOf`) and compared:
  - no fact, or a fact whose version is older than the row's `createdAt`: a mismatch, and the fact is written from the row through `foldPage`, live or deleted as the row is;
  - a fact whose version is newer than the row's: the row in hand is an older version than the one folded, read again because the window is in the past. Nothing is compared and nothing is written;
  - a fact at the row's own version: its content is compared with what the row gives now. For a live row that is `same`, `deleted` being false, and for an order the wholesale judgement with the store's granted set (asked once a run through `wholesaleFor`, as the walk asks it). For a tombstone it is `deleted` being true and nothing else: a deleted fact's other columns are whatever it last held and are never read. Any difference is a mismatch and the fact is written again from the row; the fold's guard admits an equal version.
  Every fact a page names gets `checked_at = pass_started_at`.
- Why content and not only version: a customer's erasure rewrites every version of a row in place and keeps each version's write time, so neither the walk nor a comparison of versions ever sees it. With the comparison of content, one pass after an erasure the facts of the rows it scrubbed are recomputed: an order whose `email` is the erasure's marker is retail, and a custom line whose title was scrubbed has the marker for its title and product key. Any other change in place is repaired the same way.
- A deleted order costs a pass nothing: its tombstone is at the fact's own version and both are deleted, and the older live version an earlier window reads is older than the fact, so nothing is compared.
- Every order page re-derives its orders, so a derived column is recomputed from its facts once a pass whether or not anything differed.
- When all three cursors are `done`, the facts no mirrored row answers for are deleted, in batches: each a `writeTx` that deletes up to `deleteBatch` facts of the store that are not `deleted`, have `checked_at` null or older than `pass_started_at`, and a version at or before the cut, counting each as mismatched and repaired; a refund's lines go with it. A fact marked `deleted` is left alone whatever its `checked_at`: it is the guard against the versions before it. When a batch deletes nothing the pass ends in one more `writeTx`: `disagreeing` is counted, the pass's start and counts are copied to `last_started_at`, `last_compared`, `last_mismatched`, `last_repaired` and `last_disagreeing`, `last_finished_at` is the clock, `clean_at` is the clock when `mismatched` is 0, and the pass's own columns are cleared.
- `disagreeing` (D14) counts the store's counted orders with complete lines for which any of these holds, over facts that are not deleted: `shopify_total_refunded` is not the sum of `money_refunded` over its refund facts; `shopify_total_discounts` is not the sum of `allocated_discounts` over all its lines; no line has `current_quantity` strictly between 0 and `quantity`, and `shopify_current_subtotal` is not the sum over its lines with `current_quantity = quantity` of `unit_price * quantity - allocated_discounts`. Amounts are compared by value. Nothing reads it but a person; the live checks L1 to L4 decide whether a predicate was read wrongly.

- [ ] **Step 1: Write the failing tests** (database lane)

The harness gains `rewriteInPlace(t, concept, storeID, gid string, change map[string]any)`, which changes keys of every stored version of a mirror row with one SQL statement and leaves each version's write time, as an erasure does. It is a test's stand-in for an erasure and says so in its comment. The root gate over hand-written statements on the node table (`staged_read_site_classification_test.go`) does not sweep `_test.go` files, so the helper needs no verdict line there. Task 18 runs the mirror's own erasure.

```go
func TestAFactDeletedByHandIsRestored(t *testing.T)                        // each of the four tables in turn; mismatched and repaired count it
func TestAFactWhoseVersionWasSetBackIsRecomputed(t *testing.T)
func TestAFactWithNoMirroredRowIsDeleted(t *testing.T)                     // an order fact, a line fact and a refund fact with its lines
func TestTheDeleteOfFactsNoRowAnswersForRunsInBatches(t *testing.T)        // deleteBatch 2 and five such facts: three batches, and the pass ends only after the one that deletes nothing
func TestACheckOverAStoreWithADeletedOrderEndsCleanWithNothingMismatched(t *testing.T) // Review Focus: an order folded live and then retired; two passes: mismatched 0 in each, clean_at set, the fact still deleted
func TestADeletedFactIsNeverDeletedAsAnOrphan(t *testing.T)                // a deleted fact whose mirror row was removed by hand: still there after a pass
func TestAFactWhoseRowWasChangedInPlaceIsRecomputed(t *testing.T)          // a line's title rewritten in place at the same createdAt: after one pass the fact holds the new title, mismatched 1, repaired 1
func TestAnOrderWhoseEmailWasScrubbedInPlaceIsRetailAfterOnePass(t *testing.T) // a granted buyer's order, its email rewritten in place to the erasure's marker: wholesale false after the pass
func TestACustomItemsScrubbedTitleLeavesTheFactAfterOnePass(t *testing.T)  // a line with no productGid, title "Engraving: Ada" rewritten in place to the marker: the fact's title and product_key hold no "Ada"
func TestAnOlderVersionReadByAnEarlierWindowIsNotAMismatch(t *testing.T)   // a row rewritten later: the pass's first window hands its older version; mismatched stays 0
func TestADerivedColumnChangedByHandIsDerivedAgain(t *testing.T)           // gross set to 1.00 on an order fact
func TestARowWrittenAfterThePassBeganIsNotAMismatch(t *testing.T)          // an order written a second ago, no fact yet: mismatched 0, clean_at set
func TestAStoreWhoseWalkIsBehindIsNotChecked(t *testing.T)                 // and neither is one that is rebuilding, purging or in a wholesale pass
func TestAPassThatStoppedAtItsBudgetCarriesOnAndFinishes(t *testing.T)     // budget 1 and 600 orders: no delete of unanswered facts before the last page
func TestAPassKeepsTheWindowWidthItLearnedForEachSource(t *testing.T)      // a page that outlasts a read halves order_window_seconds; the next run of the pass reads at that width, and line_window_seconds is untouched
func TestACleanPassSaysSoAndADirtyOneDoesNot(t *testing.T)                 // clean_at set only when mismatched is 0, and kept from an earlier clean pass; last_started_at, last_finished_at and the four last counts are the ended pass's
func TestAnOrderWhoseRefundsDoNotAddUpToShopifysTotalDisagrees(t *testing.T)
func TestARebuildDeletesInBatchesAndEndsWithTheSameFactsAndACleanCheck(t *testing.T) // deleteBatch 2: every fact row equal before and after but checked_at; clean_at set by the pass the walk started
func TestARebuildDuringAPassDropsThePass(t *testing.T)                     // the generation raised between a check page's read and its write: nothing is written, and the store has no check row until its walk has caught up again
func TestOnlyTheChecksPrincipalRunsIt(t *testing.T)
func TestOnlyAPersonWhoIsAClusterOwnerRebuilds(t *testing.T)               // a developer and a writer are refused, nothing marked
func TestAMaintenancePrincipalCannotRebuild(t *testing.T)                  // the rollup's and the check's principals, both cluster owners: refused with the rebuild's sentence, generation unchanged
func TestARebuildTakesEitherFormOfTheStoreId(t *testing.T)                 // "<id>" and "v1:shopify:store:<id>" mark the same row
func TestARebuildOfAStoreWithNoRowIsRefused(t *testing.T)                  // "Store statistics has no store with that id.", and no row is made
func TestTheCheckStateSaysWhenThePassBeganEndedAndEndedClean(t *testing.T) // before any pass: every time null; under way: passUnderWaySince; after a clean pass: lastStartedAt, lastFinishedAt, cleanAt and mismatched 0; after a dirty one: cleanAt is the earlier pass's
func TestOnlyAPersonWhoIsAClusterOwnerReadsTheCheckState(t *testing.T)     // a developer and a maintenance principal are refused; either form of the store id is taken; a store with no row is refused
func TestTwoChecksAtOnceLeaveOneAnswer(t *testing.T)
func TestARecomputedOrderKeepsItsWholesaleSide(t *testing.T)               // a granted buyer's order fact deleted by hand comes back wholesale
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/storestats/ -run 'Fact|Orphan|DeletedOrder|Scrubbed|EarlierWindow|Derived|PassBegan|Behind|Budget|WindowWidth|CleanPass|Disagrees|Rebuild|Principal|CheckState|ChecksAtOnce|Recomputed'`. Expected: FAIL.
- [ ] **Step 3:** Implement, then `make sdk-gen`, `make platform-graph`, and write the corpus file with `-update`.
- [ ] **Step 4:** Run the same command, the package whole in the database lane, `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/auth/ ./component/automations/...`, and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `storestats: an hourly check proves the facts against the mirror by version and by content, and a rebuild refills them`.

### Task 17: The read: its gate, the answer's frame and the SDK (U9, U11)

**Independent of:** Task 20.

**Files:**
- Create: `component/storestats/statistics.go`, `component/storestats/statistics_payload.go`, `component/storestats/statistics_gate_db_test.go`
- Modify: `component/storestats/integration.go` (the `statistics` capability), `component/storestats/harness_db_test.go`, `dsl/shopify/overlay/builtins.memql`, `component/memql/app_resource_os_parity_test.go` (`"storeStatistics": "app:deployables/sales"` in `TestTheDeployablesPartsAreDeclaredOnTheirConstructs`)
- Regenerate: `sdk/go/client/generated_builtins.go`, `sdk/ts/src/client/generated_builtins.ts`

**Interfaces:**
- Consumes: Task 5's `statsperiod.ForIn(p Period, now time.Time, loc *time.Location) Bounds` and the existing `statsperiod.Parse(s string, allowed ...Period) (Period, error)`; Task 7's `store_sales_store`, as Tasks 12, 14 and 15 fill it; Task 8's part `app:deployables/sales`, which the engine judges in the organization of the site `siteId` names for a builtin called `storeStatistics`; Task 11's `Integration`, its `Capabilities()` and `readTx(ctx context.Context, db *bun.DB, fn func(ctx context.Context, tx bun.Tx) error) error`; the site read `query siteById(siteId: <q>)` under the caller's own context, whose row holds `kind`, `systemOwned`, `binding` and `previewBinding` (each `{storeId}`).
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

- Produces, in Go:

```go
const statisticsConcept = "integration:storeStats:statistics"

type statisticsRequest struct {
	SiteID, StoreID, Destination string // StoreID bare; Destination one of production, testing, both
	Period                       statsperiod.Period
}

// Statistics is one answer. This task fills its frame; Task 18 its figures, which are null until then.
type Statistics struct{ /* the keys of the answer below */ }

func readFrame(ctx context.Context, tx bun.Tx, req statisticsRequest, now time.Time) (Statistics, error) // store_sales_store and the period's bounds
func readStatistics(ctx context.Context, db *bun.DB, req statisticsRequest, now time.Time) (Statistics, error) // one readTx: the frame, and from Task 18 the figures
func (s Statistics) Payload() map[string]any
```

  and `handleStatistics`, which makes the gate's checks, builds the request and answers one node with id `statistics`.
- Produces the answer, one row. Money is a decimal string; everything not known is `null`. The frame is every key above `figures`; in this task `figures` holds eight nulls, `previous` and `buyers` and `wholesale` are null, `floors` is empty, each list is empty, each count is null, and `series` holds one entry a bucket with its `at` and seven nulls:

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
- The gate, in this order. The engine has already judged `execute app:deployables/sales` in the organization of the named site. Then the handler: (1) `statsperiod.Parse(period)`, whose own sentence refuses a bad period, before anything is read; (2) the site read under the caller's own actor; (3) it is a `shopify_storefront` and not `systemOwned`; (4) `memql.BareShortId` of its `binding.storeId` or of its `previewBinding.storeId` equals `memql.BareShortId(storeId)`. Any of (2) to (4) failing answers the one sentence of the Global Constraints. `destination` says which binding matched. Only then is `store_sales_*` read, in one `readTx`, by the bare store id. No order row is read, so the caller need not be a cluster owner.
- The read never runs under the walk's context. `handleStatistics` uses the context it was handed for the site read and for the database, and nothing in `statistics.go` or `statistics_payload.go` calls `walkContext`: a read with internal origin would pass `@requiresCapability` and the site's own row authorization for whoever asked.
- The store's zone is loaded in Go (`time.LoadLocation`; the engine already embeds the zone database). The period is `statsperiod.ForIn(period, now, loc)`, in UTC when the store has no zone or one that does not load. `series[].at`, `start`, `end` and `prevStart` are RFC3339 with the store's offset. `prevStart` is null for twelve months.
- `asOf` is `verdict_since` when the state is `paused`, `needs attention` or `not connected`, else `walked_through`. This is D16 as written. On the finished mirror `needs attention` is also the state of a store whose orders record normally while a privacy request has failed for more than a day, or while a domain the figures do not read is behind (A33); whether those stores' figures are drawn as of `verdict_since` awaits the owner's ruling (pre-flight, "Needs a ruling"), and until it is given the rule stands as written. `rebuilding` is true while `rebuilding_since` is set. `recording` is the verdict as Task 14 kept it. `olderOrderChangesNotRecorded` is `not all_orders_readable`. `storeName` and `storeDomain` are there because a holder of the part cannot read the store row.
- A store the walk holds no row for yet is answered, not refused: the site serves it, so the frame says its ids and its destination, a `recording.state` of `starting` and every other key null.

- [ ] **Step 1: Write the failing tests** (database lane)

The harness gains `siteFor(t, storeID string, over map[string]any) string` (a `shopify_storefront` site in an organization of its own, bound to the store), `memberCtx(t, siteID, withPart bool)` (a member of the site's organization who is not a cluster owner, granted `execute app:deployables/sales` through a group when asked), and `storeFrame(t, storeID, columns map[string]any)`, which writes a `store_sales_store` row.

```go
func TestEachRefusalIsTheOneSentence(t *testing.T)                          // a site that does not read, a static site, a system-owned one, a store it does not serve: the same sentence; a bad period: the period's own
func TestACallerWithoutThePartIsRefusedAndNothingIsRead(t *testing.T)       // capability_not_held, and storeSalesQueries() recorded no statement during the call
func TestAHolderOfThePartInAnotherOrganizationIsRefusedAndNothingIsRead(t *testing.T) // U5's proof: capability_not_held, and no store_sales_ table was queried
func TestASiteThatDoesNotServeTheStoreIsRefusedBeforeAnyFactIsRead(t *testing.T) // the one sentence, and storeSalesQueries() is empty
func TestACallerWhoIsNotAClusterOwnerGetsAnAnswer(t *testing.T)             // a member of the site's organization granted the part
func TestASiteBoundOnlyThroughItsPreviewBindingIsAnsweredAsTesting(t *testing.T) // and "both" when the two name one store
func TestASiteBoundUnderTheCanonicalFormOfTheStoreIdStillServesIt(t *testing.T)  // Review Focus: binding v1:shopify:store:<id>, asked with <id>, and the reverse
func TestTheReadCarriesNoInternalOrigin(t *testing.T)                       // a recording engine in front of the site read: auth.OriginFromContext of the context the handler asks it with is not internal, and the actor is the caller's own
func TestTheFrameSaysTheStoreItsCurrencyItsZoneAndItsKind(t *testing.T)     // storeName, storeDomain, currency, timezone, development, taxesIncluded as the row holds them; null where it holds none
func TestThePeriodIsCutInTheStoresZone(t *testing.T)                        // Europe/Madrid, 7d at 2025-11-08T10:17Z: start 2025-11-02T00:00:00+01:00, end 2025-11-09T00:00:00+01:00, seven entries in series, each at a local midnight; 12m: prevStart null
func TestAStoreNotRecordingIsAnsweredAsOfWhenItStopped(t *testing.T)        // paused, needs attention and not connected: asOf is verdict_since; recording: walked_through; rebuilding_since set: rebuilding true
func TestAStoreWithNoZoneIsCutInUTC(t *testing.T)                           // timezone null in the answer, start a UTC midnight
func TestAGrantWithoutAllOrdersSaysOlderChangesAreNotRecorded(t *testing.T)
func TestAStoreTheWalkHasNotSeenYetIsAnsweredAsStarting(t *testing.T)
func TestAStartingStoresFiguresAreNullAndEveryKeyIsThere(t *testing.T)       // a store whose state is starting: the eight are null, never 0, and the payload holds every key of the answer
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/storestats/ -run 'Refus|NothingIsRead|IsRead|ClusterOwnerGets|Bound|InternalOrigin|Frame|CutIn|NotRecording|AllOrders|NotSeenYet|EveryKeyIsThere'` and `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/memql/ -run 'TestTheDeployablesPartsAreDeclaredOnTheirConstructs'`. Expected: FAIL.
- [ ] **Step 3:** Implement, then `make sdk-gen`; in `sdk/ts` run `npm run build`.
- [ ] **Step 4:** Run the same two commands, the package whole in the database lane, `make sdk-gen-check` (both clients now carry `storeStatistics`), and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `storestats: a holder of the sales part asks for a store a site serves, and gets its frame`.

### Task 18: The read: the figures (U9)

**Independent of:** Task 20.

**Files:**
- Create: `component/storestats/figures.go`, `component/storestats/figures_db_test.go`, `component/storestats/erasure_db_test.go`, `component/storestats/statistics_scale_test.go`
- Modify: `component/storestats/statistics.go` (`readStatistics` reads the figures after the frame), `component/storestats/statistics_payload.go`, `component/storestats/harness_db_test.go`

**Interfaces:**
- Consumes: Task 17's `Statistics`, `readFrame`, `readStatistics` and `Payload`; Task 7's fact tables, as Tasks 13 to 16 fill them; Task 6's `money.Amount`, `MulRatio(num, den int64, scale int) (Amount, error)` and `money.MinorUnits(currency string) int`; Task 5's `Bounds.Starts()`; the harness's `run(t)` and the builtins `storeStatisticsRollup()` and `storeStatisticsCheck()`; the connector's exported erasure of a customer, `(*shopify.Connector).RedactCustomer(ctx context.Context, storeID, customerGID string, orderGIDs []string) (shopify.ErasureResult, error)`, on a connector made with `shopify.NewConnector(engine, logger, stores, admin)` and given the engine's maintenance session with `WithMaintenanceSession(func(ctx context.Context) (*sql.Conn, func(), error) { return database.OpenMaintenanceSessionDSN(ctx, dsn) })` (the erasure refuses to run on the request pool), and the marker it leaves, `shopify.RedactionMarker`; Task 1 checked each.
- Produces: `func readFigures(ctx context.Context, tx bun.Tx, s *Statistics) error`, which fills every key of the answer from `figures` down and reads only `store_sales_*`.

Decisions:
- 5.3 of the record defines every figure and is implemented as written. A counted order is one whose fact is not `deleted`, with `cancelled` false and, unless the store's `development` is true, `test` false. A counted refund is a refund fact that is not `deleted` whose order fact exists and is counted. A line counts when it is not `deleted` and its order is counted. No statement here reads a fact without that filter.
- Money is summed only over orders, and refunds of orders, whose `currency` is the store's; the others are counted in Orders and Units and listed in `otherCurrencies`.
- The average order value divides what it sums by what it summed over: the gross sales less the discounts of the period's orders in the store's currency, by the number of those orders, not by Orders, which counts every currency. It is null when there is no such order. It is answered already rounded for display, half to even at the currency's minor units (`MulRatio(1, n, money.MinorUnits(currency))`), so the page's formatter rounds nothing a second time and by another rule.
- SQL cuts buckets with `width_bucket(<instant>, <the bucket starts as a timestamptz array>)`, never with `AT TIME ZONE`: every boundary then comes from one zone database, the one that cut the period.
- A period's sums cover `[start, min(end, asOf))`. A bucket that starts after `asOf`, or ends at or before `historySince`, is null in every figure.
- Null, never zero:
  - no zone, or one that does not load: every figure, every bucket, every list;
  - `rebuilding_since` set, state `starting`, or `walked_through` null: every figure and bucket;
  - no currency: every money figure; `orders` and `unitsSold` stay;
  - a counted refund in the period with `detail_fetched` false: `refunds`, `netSales`, `totalSales`, `unitsSold`, for the period and for the bucket it falls in; `refundsNotRead` counts them (D22);
  - a counted refund in the period with `lines_complete` false: `unitsSold`; `incompleteRefunds` counts them;
  - no order in the store's currency in the period: `averageOrderValue`.
- `floors` names `grossSales`, `discounts`, `netSales`, `totalSales`, `averageOrderValue` and `unitsSold` when a counted order in the period has `lines_complete` false; `incompleteOrders` counts them.
- `previous` is null for 12 months, when `prevStart` is before `historySince`, and whenever every figure is null; otherwise it is the same eight figures over the previous period, with the same null rules.
- A product's title is that of its newest line in the period. `refundsWithoutItem` is the period's `item_refunds` less its refund lines' subtotals on lines that are not gift cards. `buyers.unknown` counts the period's orders with no `buyer_key`; a buyer is new when their first counted order in the store's facts is inside the period.
- `wholesale` is null unless the store's `wholesale_enabled`; a refund is on its order's side.
- O5: the handler runs on every call. The test below holds it.
- `statistics_scale_test.go` runs at two sizes. In the lane it writes 20,000 orders, 60,000 lines and 2,000 refunds for one store among nine other stores of 1,000 orders each, requires the 90-day read under 500 ms, and requires the answer's Orders and Gross sales to equal a direct count and sum over what it wrote: at that size it catches a read that asks once an order or leaves a store's filter out, and a busy runner does not miss the bound. With `MEMQL_STORESTATS_SCALE_EVIDENCE_FILE` set it writes ten times that, requires the same bound and writes the timings to the file, in the way of `component/emailstats/statistics_scale_test.go`: only that size proves the indexes.

- [ ] **Step 1: Write the failing tests** (database lane)

```go
func TestAWeekOfSalesReadsToTheCent(t *testing.T) {
	// Europe/Madrid, EUR, now 2025-11-08T10:17Z, period 7d.
	// Order A, Nov 5 12:00 local: 2 x 40.00 less 8.00, 1 x 19.99; shipping 5.00, tax 21.00; prices do not include tax.
	// Order B, Nov 6: 1 x 10.00; tax 2.10.
	// A refund of A on Nov 7: one unit of the first line (subtotal 36.00, tax 7.56) and its shipping (5.00): 48.56.
	// After it the first line's currentQuantity is 1 of 2.
	want := map[string]any{"totalSales": "81.53", "netSales": "65.99", "orders": 2, "averageOrderValue": "51.00",
		"grossSales": "109.99", "discounts": "8.00", "refunds": "36.00", "unitsSold": 3}
	// figures equals want (the average is 101.99 / 2 = 50.995, a tie, to the even cent);
	// the Nov 7 bucket has refunds "36.00", netSales "-36.00", unitsSold -1, orders 0 (Review Focus: below zero, never absent)
}

func TestTheAverageDividesByTheOrdersInTheStoresCurrency(t *testing.T)      // two EUR orders of 30.00 and 50.00 and one USD order of 999.00: orders 3, grossSales "80.00", averageOrderValue "40.00", otherCurrencies [{USD, 1}]
func TestTheAverageIsAnsweredRoundedAtTheCurrencysDecimals(t *testing.T)    // three EUR orders summing 100.00: "33.33"; three JPY orders summing 100: "33"; three KWD orders summing 1.000: "0.333"
func TestTheAverageIsNullWithNoOrderInTheStoresCurrency(t *testing.T)       // a period holding only a refund, and one holding only a USD order: averageOrderValue null; in the first netSales is below zero
func TestOtherCurrenciesAreListedAndNotSummed(t *testing.T)
func TestAStoreWithNoOrdersReadsMeasuredZeros(t *testing.T)                 // recording, with history, no order in the period: orders 0, grossSales "0", netSales "0", averageOrderValue null
func TestADeletedOrderIsInNoFigure(t *testing.T)                            // Review Focus: an order folded and then retired, with its lines and a refund still live: orders, grossSales, refunds, unitsSold, the products, the buyers and the series are as if it never was
func TestADeletedLineAndADeletedRefundAreInNoFigure(t *testing.T)
func TestASaleLandsOnItsStoreDayWestAndEastOfUTC(t *testing.T)              // 2025-11-08T03:00Z is the 7th in Los Angeles and the 8th in Kolkata
func TestSalesOnADaylightSavingDayLandInThatDay(t *testing.T)               // Review Focus: Santiago 2023-09-03 01:30 local and 23:30 local are one bucket; 2023-09-02 23:30 local is the day before
func TestBothOfARepeatedHoursBucketsHoldTheirOwnSales(t *testing.T)         // America/New_York, 24h on 2025-11-02: an order at 05:30Z and one at 06:30Z are in two buckets, both starting at 01:00 local
func TestThePreviousPeriodIsNullWhenItStartsBeforeHistory(t *testing.T)
func TestAnIncompleteOrderMakesFloors(t *testing.T)
func TestARefundNotYetReadBlanksTheFourFiguresThatSubtractIt(t *testing.T)  // D22: the four null, refundsNotRead 1, orders and grossSales still measured
func TestARefundWithAFullPageOfLinesBlanksUnitsSold(t *testing.T)           // incompleteRefunds 1, unitsSold null, refunds still measured
func TestBuyersAreNewReturningOrUnknown(t *testing.T)                       // a buyer with three orders, one before the period: returning; a guest order: unknown
func TestWholesaleIsNullUnlessTheStoreIsEnabledAndARefundIsOnItsOrdersSide(t *testing.T)
func TestACancelledOrderAndItsRefundLeaveEveryFigure(t *testing.T)          // in the period of the order and of the refund
func TestAShippingRefundLowersTotalSalesAndNotNetSales(t *testing.T)
func TestAnEditThatRemovesAUnitChangesTheOrdersOwnDayAndNoRefund(t *testing.T)
func TestATestOrderCountsOnlyOnADevelopmentStore(t *testing.T)
func TestFiguresStopAtAsOfAndStartAtHistory(t *testing.T)                   // paused: buckets after verdict_since null; buckets ending before historySince null; starting and rebuilding: every figure null; a state not known: figures, state ""
func TestAStoreWithNoZoneAnswersNoFigureAndOneWithNoCurrencyNoMoney(t *testing.T) // Review Focus
func TestTheLinesOfARetiredOrderCountNowhere(t *testing.T)
func TestTopProductsCarryTwentyFiveRowsAndSayHowManyThereAre(t *testing.T)
func TestAReadAfterAFactWriteOnTheSameNodeSeesIt(t *testing.T)              // O5: read, write an order fact, read again through the builtin: orders rose by one
func TestTheAnswerHoldsNoEmail(t *testing.T)
func TestTheNinetyDayReadAtScale(t *testing.T)                              // statistics_scale_test.go, at the two sizes of the Decisions

// erasure_db_test.go: the mirror's own erasure, then one check pass
func TestAfterAnErasureAndOneCheckNoFactHoldsWhatTheErasureRemoved(t *testing.T) {
	// A store with a granted wholesale buyer "ada@example.test". Their order: one product line (2 x 40.00) and one custom
	// line with no product titled "Engraving: Ada Example" (1 x 10.00); shipping 5.00. Folded: the order is wholesale,
	// grossSales "90.00".
	// The connector's erasure of that customer runs (RedactCustomer, on a maintenance session), then the check under its principal.
	// Then: no text or JSON column of any store_sales_ table, cast to text, holds "Ada", "ada@example.test" or "Engraving";
	// the order's fact is retail and still holds its buyer_key; the custom line's title is shopify.RedactionMarker;
	// and storeStatistics for the period still answers orders 1, grossSales "90.00" and totalSales "95.00".
}
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/storestats/ -run 'Reads|Average|Currenc|Deleted|StoreDay|DaylightSaving|RepeatedHour|PreviousPeriod|Floors|NotYetRead|FullPage|Buyers|Wholesale|Cancelled|ShippingRefund|RemovesAUnit|TestOrder|AsOf|NoZone|Retired|TopProducts|SameNode|NoEmail|NoOrders|Erasure|Scale'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, the package whole in the database lane, and the generated-file gates. Expected: PASS.
- [ ] **Step 5: Commit** `storestats: a period of a store's sales, to the cent, with nothing counted that the store deleted`.

### Task 19: Independent totals (U10)

**Independent of:** Task 20.

**Files:**
- Create: `component/storestats/reference_calc_test.go`, `component/storestats/reference_db_test.go`, `component/storestats/testdata/fixture-store/orders.json`, `component/storestats/testdata/fixture-store/README.md`
- Modify, only when Step 5's measurement asks it: `scripts/ci/db-test-budgets.yaml` (a class for `component/storestats`, with its comment), `scripts/ci/pipeline_db_tests_test.py` (the class in `test_real_table_preserves_every_selected_package_and_budget`'s two lists)

**Interfaces:**
- Consumes: the builtins `storeStatisticsRollup()` (Task 11) and `storeStatistics(siteId, storeId, period)` (Task 17), and the harness of Tasks 11 to 18; from `integrations/shopify`, only what it exports: `NewStoreRegistry(engine, secrets) *StoreRegistry`, `NewAdminClient() *AdminClient`, `NewConnector(engine, logger, stores, admin) *Connector`, `(*Connector).WithDatabase(db func() *sql.DB) *Connector` and `(*Connector).ReconcilePartition(ctx context.Context, partition, conceptName string, since time.Time) (memqlsync.ReconcileReport, error)`, asked with the store id as the partition and `generated.ConceptID("order")` as the concept.
- Produces, in `reference_calc_test.go`: `func referenceFigures(fixture []byte, zone *time.Location, currency string, development bool, granted []string, start, end time.Time, bucket string) referenceAnswer`, where `referenceAnswer` holds the eight figures, the series, both product lists, the buyers and the wholesale split as strings.

Decisions:
- The fixture is one file of orders as `ShopifyListOrder` answers them (Admin GraphQL nodes with `lineItems.nodes`, `refunds` and each refund's three connections holding the fields Task 2 names and no other), for a bicycle shop in EUR and `Europe/Madrid`. Each order carries a `name` that says what it is for, and the test finds each case by that name. The cases: two discount codes; an order-level discount; a gift card; a line refund with and without restock; a shipping refund; an amount-only refund (`amount-only-refund`); a refund with a restocking fee (`restocking-fee`); a refund in a later period than its order; an edit that removes a unit; an edit that adds a line; a cancelled order with a refund; a test order; an order in USD; a buyer with three orders; a wholesale buyer; an order and a refund on either side of a local midnight; one order of 120 lines; a line of three units of which an edit left two, with a discount Shopify wrote as "1.0" (`discount-written-short`), whose share is 0.67; an order whose prices include tax with its shipping refunded (`tax-inclusive-shipping-refund`); an order an `ORDERS_DELETE` notice retires after it was read (`deleted-order`). The README says what each order is for, that nothing in it is a real shop's, and which three orders change, and how, if a live check finds an adjustment signed the other way or a tax-inclusive shipping refund written without its tax.
- The write path is the mirror's own: a fake transport on `http.DefaultTransport` answers `ShopifyListOrder` and `ShopifyPageOrderLineItems` from the file, and the connector's `ReconcilePartition` pages the 120 lines, maps and heals; the deleted order is then retired by a raw insert as the mirror's tombstone writes it. The shop row is a raw insert under the connector actor; the wholesale buyer's entitlement and the store's settings row are written as the pack's own tests write them. Then the rollup until it has caught up, then `storeStatistics` for every period at a fixed clock.
- The calculator is its own file. It reads the same JSON and implements section 5 of the record with `math/big` rationals, and cuts days with `time` in the store's zone. It rounds a share half to even at the larger of the digits after the point in the amount as the file writes it and two, the fixture's currency being the euro, and the average at two; it writes that rule itself, in its own words. It leaves out the order named `deleted-order`, as it leaves out the cancelled order and the test order. It uses the standard library and nothing else: no function, type or constant of this package, and not `core/money` or `core/statsperiod`. The period's start and end it is handed are computed in the test from the clock by plain date arithmetic, not by `statsperiod`.
- The comparison is exact strings for every figure of every period, every bucket of every series, both top lists, the buyers and the wholesale split.

- [ ] **Step 1: Write the failing tests**

```go
func TestTheFiguresEqualAnIndependentCalculationToTheCent(t *testing.T) // every period; a difference names the period, the figure, both values and the orders that fall in it
func TestTheReferenceSharesNoCodeWithTheComponent(t *testing.T)         // parses reference_calc_test.go: its imports are standard library only, and every identifier it does not declare itself is a Go builtin or a name of an imported package
func TestTheFixtureHoldsEveryCaseTheRecordNames(t *testing.T)           // twenty cases, each found by its order's name in the file
func TestTheFixturesRefundsHoldOnlyTheFieldsTheMirrorKeeps(t *testing.T) // no key of a refund's three connections but the ones Task 2 names: no shippingLine, no phone
func TestTheShortWrittenDiscountIsSharedInCentsByBoth(t *testing.T)     // discount-written-short: the component's line fact and the calculator both give 0.67
```

- [ ] **Step 2:** Run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -p 1 -count=1 ./component/storestats/ -run 'IndependentCalculation|ReferenceShares|Fixture|ShortWritten'`. Expected: FAIL.
- [ ] **Step 3:** Write the fixture and the calculator. A difference is a defect in the component or in the reading of section 5: settle which before changing either, and never make the two agree by copying one into the other.
- [ ] **Step 4:** Run the same command, then the package whole in the database lane, and the docs gates (the README is a tracked Markdown file). Expected: PASS.
- [ ] **Step 5: The package's CI budget (A30).** The suite is now whole. Time it three times with the lane's own flags on a fresh database: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 -p=1 -parallel=4 -timeout=600s ./component/storestats/`, and take the slowest `ok` line. The file puts the runner at about 1.9 times a laptop for most trees and 2.2 to 2.5 times for the mirror's suites, which boot a real engine as this one does: take 2.5. The file's classes keep about twice the expected runner time (`integrations/shopify`'s 600s for an expected 310s, `component/datasync`'s 300s for 135s). So when 2.5 times the slowest pass is under 90 seconds the package stays in the default class, and the report says so with the number. Otherwise it gets a class of its own, `db      storestats component/storestats <seconds>s serial    1`, at about twice the expected runner time rounded up to a multiple of 60, placed before the `default` row, under a comment in the file's manner: what the suite boots, the laptop time with the lane's flags, the expected runner time, and "keep every test and its operation deadlines". The class's name and timeout join both lists of `test_real_table_preserves_every_selected_package_and_budget`. Then `python3 -m unittest discover -s scripts/ci -p pipeline_db_tests_test.py` and `MEMQL_DATABASE_DSN="$STATS_DSN" go test -count=1 ./scripts/cidb/ ./scripts/ci/`. Expected: PASS.
- [ ] **Step 6: Commit** `storestats: the figures equal an independent calculation over a fixture store, to the cent`, and, when Step 5 gave the package a class, a separate commit `ci: the store statistics suite gets its own database budget`.

### Task 20: The kit (U12)

**Independent of:** Tasks 2 to 19.

**Files:**
- Modify: `clients/os/src/kit/format.ts`, `clients/os/src/kit/statistics/period.ts`, `clients/os/src/kit/statistics/TimeChart.tsx`, `clients/os/src/kit/statistics/scale.ts`, `clients/os/DESIGN.md`
- Test: `clients/os/test/kit/format.test.ts`, `clients/os/test/kit/statistics/period.test.ts`, `clients/os/test/kit/statistics/timeChart.test.tsx`, `clients/os/test/kit/statistics/scale.test.ts`

**Interfaces:**
- Produces: `formatMoney(currency: string): (value: number) => string` in `kit/format.ts`; `bucketLabel(at: string, bucket: Bucket, timeZone?: string): string` and `bucketMoment(at: string, bucket: Bucket, timeZone?: string): string`; an optional `timeZone?: string` prop on `TimeChart`, passed to both; `niceMin(min: number): number` in `scale.ts` (0 for a value at or above zero, else minus `niceMax` of its size).

Decisions:
- `formatMoney` writes through `Intl.NumberFormat("en-US", { style: "currency", currency })`, the locale `formatCount` writes in, with the currency's own decimals. A value that is not finite is "--". A code `Intl` refuses (one that is not three letters) never throws: the value is written as its count and the code as given ("1,204.5 EURO").
- With a `timeZone`, the label's parts are read with `Intl.DateTimeFormat("en-US", { timeZone, hourCycle: "h23", year: "numeric", month: "numeric", day: "numeric", hour: "numeric", minute: "numeric" }).formatToParts`, and the month is still written from the file's own `MONTHS`. A zone `Intl` refuses is read as UTC. Without a `timeZone` both functions answer exactly what they answer today. On a day the clocks go back, the two buckets of the repeated hour carry the same label: they are two hours of elapsed time, and the label is what the store's clock showed.
- `TimeChart` scales from `niceMin` of its smallest value to `niceMax` of its largest. With nothing below zero the drawing is unchanged. With a value below zero the value axis's steps are the floor, zero, half the top and the top (the half left out for whole numbers when it is not whole, as today); the fill's baseline is where zero falls instead of the plot's bottom; and the grid rule at zero carries `data-zero="true"`.
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
it("labels a month by the zone's own month", () => expect(bucketLabel("2025-10-31T23:00:00Z", "month", "Europe/Madrid")).toBe("Nov 2025"));
it("labels both of a fall-back day's repeated hours 01:00", () => {
  expect(bucketLabel("2025-11-02T05:00:00Z", "hour", "America/New_York")).toBe("01:00");
  expect(bucketLabel("2025-11-02T06:00:00Z", "hour", "America/New_York")).toBe("01:00");
  expect(bucketLabel("2025-11-02T07:00:00Z", "hour", "America/New_York")).toBe("02:00");
});
it("reads a zone it does not know as UTC", () => {
  expect(bucketLabel("2025-11-08T23:30:00Z", "day", "Not/AZone")).toBe("Nov 8");
  expect(bucketMoment("2025-11-08T23:30:00Z", "hour", "Not/AZone")).toBe("Nov 8, 23:30");
});

// scale.test.ts
it("answers zero for a floor at or above zero and a round number below it", () => {
  expect(niceMin(0)).toBe(0);
  expect(niceMin(12)).toBe(0);
  expect(niceMin(-36)).toBe(-50);
  expect(niceMin(-0.42)).toBe(-0.5);
});

// timeChart.test.tsx, with the file's own days(), seriesPath() and count
it("draws a bucket below zero below the zero rule, and a series with nothing below zero as before", () => {
  const below = render(<TimeChart label="Net sales" bucket="day" points={days([20, -36, 55])} format={count} />).container;
  // The axis runs from -50 to 100 over 160: zero falls at 106.67, 20 at 85.33, -36 at 145.07, 55 at 48.
  expect(seriesPath(below, "current")?.getAttribute("d")).toBe("M0 85.33 L300 145.07 L600 48");
  expect(below.querySelector('path[data-area="current"]')?.getAttribute("d")).toBe("M0 106.67 L0 85.33 L300 145.07 L600 48 L600 106.67 Z");
  expect(below.querySelector(".os-time-chart-values")?.textContent).toBe("100500-50");
  expect(below.querySelector('.os-time-chart-grid[data-zero="true"]')?.getAttribute("y1")).toBe("106.67");
  cleanup();
  const above = render(<TimeChart label="Visitors" bucket="day" points={days([3, 5, 4])} format={count} />).container;
  expect(seriesPath(above, "current")?.getAttribute("d")).toBe("M0 64 L300 0 L600 32");
  expect(above.querySelector('path[data-area="current"]')?.getAttribute("d")).toBe("M0 160 L0 64 L300 0 L600 32 L600 160 Z");
  expect(above.querySelector(".os-time-chart-values")?.textContent).toBe("50");
  expect(above.querySelector('.os-time-chart-grid[data-zero="true"]')).toBeNull();
});
it("labels its buckets, its readout and its summary in the time zone it is given", () => {
  const points = [{ at: "2025-11-07T23:00:00Z", value: 1 }, { at: "2025-11-08T23:00:00Z", value: 2 }];
  const { container } = render(<TimeChart label="Net sales" bucket="day" timeZone="Europe/Madrid" points={points} format={count} />);
  expect(container.querySelector(".os-time-chart-times")?.textContent).toBe("Nov 8Nov 9");
  expect(container.querySelector("figcaption")?.textContent).toBe("Net sales: 2 points from Nov 8 to Nov 9; highest 2 on Nov 9.");
  cleanup();
  const utc = render(<TimeChart label="Net sales" bucket="day" points={points} format={count} />).container;
  expect(utc.querySelector(".os-time-chart-times")?.textContent).toBe("Nov 7Nov 8");
});
```

- [ ] **Step 2:** In `clients/os` run `npx vitest run test/kit/format.test.ts test/kit/statistics`. Expected: FAIL (`formatMoney` and `niceMin` are not exported; the labels are UTC; the path runs to the plot's bottom).
- [ ] **Step 3:** Implement, and edit `DESIGN.md`.
- [ ] **Step 4:** Run the same command, `npm run typecheck`, and `npx vitest run test/kit test/deployables/statistics.test.tsx test/email` (the pages that draw the chart today). Expected: PASS.
- [ ] **Step 5: Commit** `os: the statistics kit writes money, labels a store's own days and draws below zero`.

### Task 21: The page: the part, the button and the read (U13)

**Files:**
- Create: `clients/os/src/apps/deployables/storeStatistics/read.ts`, `useStoreStatistics.ts`, `StoreStatisticsSection.tsx`, `DestinationControl.tsx` (same directory), `clients/os/test/deployables/storeStatistics.test.tsx`, `clients/os/test/deployables/storeStatisticsRead.test.ts`
- Modify: `clients/os/src/apps/deployables/parts.ts`, `clients/os/src/apps/deployables/page/DeployablePage.tsx`, `clients/os/src/apps/deployables/page/DeployableWorkspace.tsx` (`WorkspaceDetail`), `clients/os/test/deployables/harness.tsx`, `clients/os/test/deployables/parts.test.tsx`

**Interfaces:**
- Consumes: the generated `query.storeStatistics({ siteId, storeId, period }, { signal })` and the answer of Task 17; `siteFromRow(raw: Row): SiteRow`, `boundStoreId(site: SiteRow): string` and `previewStoreId(site: SiteRow): string` from `rows.ts`; `partsForOrganization` from `parts.ts`; the kit's `PeriodControl`, `Select`, `Caption`, `Notice`, `AutoRefresh`, `InlineSkeleton`; `formatFreshness` from `kit/format.ts`; `statisticsRefreshMs` from `statistics/useSiteStatistics.ts`.
- Produces, in `parts.ts`: `"sales"` in `DEPLOYABLE_PARTS`, `NO_PARTS` and `ALL_PARTS`, and in the header comment's list.
- Produces, in `read.ts`: `interface StoreStatistics` mirroring the answer, a money figure parsed with `Number()` and a null kept null; `parseStoreStatistics(row: Row, requested: Period): StoreStatistics`; `fetchStoreStatistics(query: QueryClient, args: { siteId: string; storeId: string; period: Period }, signal?: AbortSignal): Promise<StoreStatistics | null>`; `destinationsOf(site: SiteRow): { production: string; testing: string }` (each id as the site holds it, "" when unbound; the server compares both forms of an id, so the client parses none).
- Produces: `useStoreStatistics({ siteId, storeId, period, enabled }): StoreStatisticsReading`, with the states and the retry rule of `useSiteStatistics`; `StoreStatisticsSection({ site, name, trail, back })`, taking the pane as `ReviewsSection` does, a `region` named "Store statistics for " and the site's hostname; `DestinationControl({ value, onChange })`, a `Select` labelled "Destination" over `"production" | "testing"`; `WorkspaceDetail` gains `"storeStatistics"`.
- Produces, in `harness.tsx`: `storeStatistics?: Record<string, Row>` (keyed by the store id the call names) and `storeStatisticsError?: string` on `FakeSeed`, answered for `builtin storeStatistics(`; `storeStatisticsRow(over: Partial<Row> & { storeId: string }): Row`, a recording store in EUR and `Europe/Madrid` for the week of Nov 2 to Nov 8 2026 with the names of the harness's `STORE`, every figure null and every list empty unless named.

Decisions:
- The button: `IconButton label="Store statistics"` with the `Receipt` icon, after Statistics. Present only when the organization's parts hold `sales` (`partsForOrganization`, as every part of this app is judged), the site is a `shopify_storefront`, not system-owned, and either destination has a store (D15). Absent otherwise. `detailTitle` gains "Store statistics". Statistics itself is drawn only for a site that is not a draft (`DeployablePage.tsx:546`), and the tests' `SHOP` is live; on a draft storefront with a store this rule draws Store statistics with no Statistics beside it. That is S1 and D15 as written, and whether it should is open (pre-flight, "Needs a ruling").
- The page opens on Production when it has a store, else Testing; the choice is not remembered (D13). The destination control is drawn only when both destinations name stores and the answer's `destination` is not `both`; when the answer says `both` there is none, and the context line says "Production and Testing". The period is the remembered `statisticsPeriod`, through `PeriodControl` over `SITE_PERIODS`, at the Head's right after the destination control.
- This task draws the Head and block 1, the context, a `Caption`: the store's name, else its domain; "Prices in EUR"; "Days in Europe/Madrid"; "Orders since Sep 12"; "Last order change recorded " and `formatFreshness`. Then, each a `Caption` of its own and only when it applies: "This is a development store: every order on it is a test order, and they are counted."; "Prices on this store include tax."; and the line about older orders, exactly as the Global Constraints give it. Task 22 draws the rest.
- Loading draws an `InlineSkeleton` for the context line; a refresh keeps what it has; a refused or failed read is a `Notice` whose `sentence` is the engine's own, kept while it is tried again, with no shape over it. `AutoRefresh` at `statisticsRefreshMs(period)`.

- [ ] **Step 1: Write the failing tests**

The page tests share these, at the top of `storeStatistics.test.tsx`, with the connection mocked as `reviews.test.tsx` mocks it and `useFakeDate()` set to `2026-11-08T10:17:00Z`:

```tsx
const HOST = "shop.memql.example.com";
const OLDER = "Refunds, edits and cancellations of orders placed more than 60 days ago are not recorded.";
function seed(over: Partial<Row> = {}, more: Partial<FakeSeed> = {}): FakeSeed {
  return { sites: [SHOP], storeStatistics: { "store-example": storeStatisticsRow({ storeId: "store-example", ...over }) }, ...more };
}
async function openDeployable(s: FakeSeed, session: Parameters<typeof withSession>[1] = {}) { /* as reviews.test.tsx: render DeployablesApp, wait for the live list, click HOST; answers { connection, page } */ }
async function openStoreStatistics(s: FakeSeed = seed(), session: Parameters<typeof withSession>[1] = {}) {
  const { connection, page } = await openDeployable(s, session);
  await click(within(page).getByRole("button", { name: "Store statistics" }));
  return { connection, page: await screen.findByRole("region", { name: `Store statistics for ${HOST}` }) };
}
const captions = (page: HTMLElement) => [...page.querySelectorAll(".os-caption")].map((c) => c.textContent ?? "");
const lastCall = (connection: FakeConnection) => connection.callsNamed("storeStatistics").at(-1) ?? "";
```

```tsx
// storeStatisticsRead.test.ts
it("keeps a null figure null and reads a money string as its number", () => {
  const s = parseStoreStatistics(storeStatisticsRow({ storeId: "store-example", figures: { totalSales: null, netSales: "65.99", orders: 2, averageOrderValue: null, grossSales: "109.99", discounts: null, refunds: null, unitsSold: null } }), "7d");
  expect(s.figures.netSales).toBe(65.99);
  expect(s.figures.orders).toBe(2);
  expect(s.figures.totalSales).toBeNull();
  expect(s.figures.averageOrderValue).toBeNull();
});
it("answers null for a reply with no row", async () => {
  const connection = fakeConnection({ sites: [SHOP] });
  expect(await fetchStoreStatistics(connection.query, { siteId: "site-shop", storeId: "store-example", period: "7d" })).toBeNull();
});
it("reads each destination's store as the site holds it", () => {
  expect(destinationsOf(siteFromRow({ ...SHOP, previewBinding: { storeId: "store-example-dev" } }))).toEqual({ production: "store-example", testing: "store-example-dev" });
  expect(destinationsOf(siteFromRow(siteRow({ id: "site-plain", kind: "spa" })))).toEqual({ production: "", testing: "" });
});

// storeStatistics.test.tsx
it("draws the button after Statistics for a storefront with a store, to a holder of the sales part", async () => {
  const { page } = await openDeployable(seed());
  const buttons = within(page).getAllByRole("button");
  expect(buttons.indexOf(within(page).getByRole("button", { name: "Store statistics" }))).toBe(buttons.indexOf(within(page).getByRole("button", { name: "Statistics" })) + 1);
});
it("draws no button without the sales part", async () => {
  const { page } = await openDeployable(seed(), { role: "developer", capabilities: seededAccessWithout("developer", "app:deployables/sales") });
  expect(within(page).getByRole("button", { name: "Statistics" })).toBeTruthy();
  expect(within(page).queryByRole("button", { name: "Store statistics" })).toBeNull();
});
it("draws no button for a static site, for a system-owned one, and with no store on either destination", async () => {
  for (const site of [siteRow({ id: "site-shop", hostname: HOST, kind: "spa" }), { ...SHOP, systemOwned: true }, { ...SHOP, binding: {}, previewBinding: {} }]) {
    const { page } = await openDeployable({ sites: [site] });
    expect(within(page).queryByRole("button", { name: "Store statistics" })).toBeNull();
    cleanup();
  }
});
it("draws the button when only Testing has a store, and opens on it", async () => {
  const site = { ...SHOP, binding: {}, previewBinding: { storeId: "store-example-dev" } };
  const { connection, page } = await openStoreStatistics({ sites: [site], storeStatistics: { "store-example-dev": storeStatisticsRow({ storeId: "store-example-dev", destination: "testing" }) } });
  expect(lastCall(connection)).toContain('storeId: "store-example-dev"');
  expect(within(page).queryByLabelText("Destination")).toBeNull();
});
it("opens on Production under the deployable's trail, and offers Testing only when the two stores differ", async () => {
  const site = { ...SHOP, previewBinding: { storeId: "store-example-dev" } };
  const { connection, page } = await openStoreStatistics({ sites: [site], storeStatistics: { "store-example": storeStatisticsRow({ storeId: "store-example" }), "store-example-dev": storeStatisticsRow({ storeId: "store-example-dev", destination: "testing", storeName: "Example Shop (development)" }) } });
  expect(within(within(page).getByRole("navigation", { name: "Breadcrumbs" })).getAllByRole("listitem").map((item) => (item.textContent ?? "").trim())).toEqual(["Deployables", "Storefront", "Store statistics"]);
  expect(lastCall(connection)).toContain('storeId: "store-example"');
  chooseOption(within(page).getByLabelText("Destination"), "Testing");
  await waitFor(() => expect(lastCall(connection)).toContain('storeId: "store-example-dev"'));
  await waitFor(() => expect(captions(page).join(" ")).toContain("Example Shop (development)"));
});
it("says Production and Testing, with no control, when both name one store", async () => {
  const site = { ...SHOP, previewBinding: { storeId: "store-example" } };
  const { page } = await openStoreStatistics({ sites: [site], storeStatistics: { "store-example": storeStatisticsRow({ storeId: "store-example", destination: "both" }) } });
  await waitFor(() => expect(captions(page).join(" ")).toContain("Production and Testing"));
  expect(within(page).queryByLabelText("Destination")).toBeNull();
});
it("asks for the remembered period and asks again when it changes", async () => {
  const { connection, page } = await openStoreStatistics();
  expect(lastCall(connection)).toContain('period: "7d"');
  chooseOption(within(page).getByLabelText("Period"), "Last 30 days");
  await waitFor(() => expect(lastCall(connection)).toContain('period: "30d"'));
});
it("names the store, its currency, its days, since when and how fresh", async () => {
  const { page } = await openStoreStatistics(seed({ historySince: "2026-09-11T22:00:00Z", lastChangeAt: "2026-11-08T10:13:00Z" }));
  const line = await waitFor(() => { const all = captions(page).join(" | "); expect(all).toContain("Example Shop"); return all; });
  for (const part of ["Prices in EUR", "Days in Europe/Madrid", "Orders since Sep 12", "Last order change recorded 4m ago"]) expect(line).toContain(part);
});
it("draws the development line, the tax line and the line about older orders, each only when it applies", async () => {
  const plain = await openStoreStatistics();
  await waitFor(() => expect(captions(plain.page).join(" ")).toContain("Prices in EUR"));
  for (const text of ["This is a development store", "Prices on this store include tax.", OLDER]) expect(captions(plain.page).join(" ")).not.toContain(text);
  cleanup();
  const all = await openStoreStatistics(seed({ development: true, taxesIncluded: true, olderOrderChangesNotRecorded: true }));
  await waitFor(() => expect(captions(all.page)).toContain(OLDER));
  expect(captions(all.page)).toContain("This is a development store: every order on it is a test order, and they are counted.");
  expect(captions(all.page)).toContain("Prices on this store include tax.");
});
it("keeps a refusal standing while its retry is out, and draws no shape over it", async () => {
  const { page } = await openStoreStatistics({ sites: [SHOP], storeStatisticsError: "This deployable does not serve that store for you." });
  await waitFor(() => expect(page.querySelector(".os-notice-line")?.textContent).toBe("This deployable does not serve that store for you."));
  expect(page.querySelector('[aria-busy="true"]')).toBeNull();   // every kit skeleton is busy
});

// parts.test.tsx
it("judges the sales part per organization", () => {
  const allow = (accountId: string, verb: string, resource: string): OrganizationCapability => ({ accountId, verb, resource, effect: "allow" });
  setEffectiveCapabilities([], [
    ...["acme", "beta"].flatMap((id) => [allow(id, "read", "data"), allow(id, "read", "app:deployables"), allow(id, "update", "data")]),
    allow("acme", "execute", "app:deployables/sales"),
  ]);
  expect(partsForOrganization("acme", NO_PARTS).sales).toBe(true);
  expect(partsForOrganization("beta", NO_PARTS).sales).toBe(false);
});
```

- [ ] **Step 2:** In `clients/os` run `npx vitest run test/deployables/storeStatistics.test.tsx test/deployables/storeStatisticsRead.test.ts test/deployables/parts.test.tsx`. Expected: FAIL (the modules do not exist; `sales` is not a part).
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, `npm run typecheck` and `npx vitest run test/deployables`. Expected: PASS.
- [ ] **Step 5: Commit** `os: a storefront's Store statistics button and its read, for holders of the sales part`.

### Task 22: The page: figures, chart, lists and states (U13)

**Files:**
- Create: `clients/os/src/apps/deployables/storeStatistics/ProductsListPage.tsx`, `clients/os/qa/storeStatisticsScenes.tsx`
- Modify: `clients/os/src/apps/deployables/storeStatistics/StoreStatisticsSection.tsx`, `clients/os/test/deployables/storeStatistics.test.tsx`, `clients/os/qa/main.tsx`

**Interfaces:**
- Consumes: Task 21's `StoreStatisticsSection`, `useStoreStatistics`, `StoreStatistics`, the harness's `storeStatisticsRow` and the test file's `seed`, `openStoreStatistics` and `captions`; Task 20's `formatMoney` and the `timeZone` prop; the kit's `StatFigures`, `TimeChart`, `TimeChartSkeleton`, `RankedList`, `LocalTabs`, `Notice`, `Caption`, `PageHelp`; `figureOf` and `absent` from `kit/measure.ts`; `previousComparable` from `kit/statistics/period.ts`; `formatCount` and `formatMoment` from `kit/format.ts`.
- Produces: blocks 2 to 8 of the page; `ProductsListPage({ statistics, by, trail, back })`, the answer's whole top list (25 rows at most) as a page under the trail, which says "The 25 best-selling of 31 products." when the period has more.

Decisions:
- Blocks, top to bottom after the context, spacing only between them:
  2. A `Notice`, only when one applies: for `starting` or a rebuild, "The store's orders are still being brought in."; for `paused`, `needs attention` and `not connected`, the mirror's sentences, then "These figures are as of " and `formatMoment(asOf)` and a full stop; for a state not known, "Whether this store is recording is not known."; for refunds not read, "1 refund is still being read from Shopify." or "N refunds are still being read from Shopify." Its `next` is "The Store page of this deployable says what fixes it." for the three states the mirror gives a reason for, and nothing otherwise. It holds no act.
  3. `StatFigures`, labelled "Sales figures", eight, in this order and with these labels: Total sales, Net sales, Orders, Average order value, Gross sales, Discounts, Refunds, Units sold. Money through `formatMoney(currency)`, counts through `formatCount`. All but the average are chartable. A previous figure is given only when `previousComparable(prevStart, historySince)`. A figure in `floors` is a floor. An absent figure carries its reason as the figure's detail: "The store's currency has not been recorded yet.", "The store's time zone has not been recorded yet.", "The store's orders are still being brought in.", the refunds sentence above, "A refund in this period has more lines than were recorded.", or "No orders in this period." for the average. When more than one applies the reason is the first of these that does: no time zone; still being brought in; no currency, for a money figure; refunds still being read, for the four figures they blank; a refund with more lines than were recorded, for Units sold; no orders, for the average. When the store's state and unread refunds both call for the `Notice`, the state is said first and the refunds sentence after it.
  4. `TimeChart` of the chosen figure, labelled with the figure's label, with `timeZone`.
  5. `RankedList` "Products", with a `LocalTabs` control labelled "Products by", "Net sales" and "Units"; eight rows; when `productCount` is larger its footer says "The 8 best-selling of 31 products." and "All products" opens `ProductsListPage` under the trail. Further footer lines when they apply, every amount through `formatMoney(currency)`: "Refunds of €12.00 name no product, so they are in Net sales and not here."; "3 orders have more lines than were recorded, so these are floors."; "1 refund has more lines than were recorded, so units are not exact."
  6. `RankedList` "Buyers": New, Returning, No customer recorded.
  7. `RankedList` "Wholesale and retail", only when `wholesale` is not null: Wholesale, Retail, with a `LocalTabs` control labelled "Wholesale and retail by", "Net sales" and "Orders".
  8. A `Caption`, only when `otherCurrencies` is not empty: "2 orders in USD are not in these sums."
  Each empty list says "No orders in this period."
- The figure definitions of the record's section 5 and its known differences from Shopify's reports go in a `PageHelp`, not on the page.
- Loading draws shapes (`TimeChartSkeleton`, `loading` on the figures and lists); a refresh keeps what it has.
- The QA scenes, one view per state, registered in `qa/main.tsx`: `store-statistics`, `-development`, `-paused`, `-starting`, `-refunds-unread`, `-no-currency`, `-empty`, `-below-zero`, `-two-stores`, `-refused`.

- [ ] **Step 1: Write the failing tests**

Added to `storeStatistics.test.tsx`, with two more helpers and one fixture:

```tsx
const figures = (page: HTMLElement) => within(page).findByRole("radiogroup", { name: "Sales figures" });
function figure(group: HTMLElement, label: string) {
  const el = [...group.querySelectorAll(".os-stat-figure")].find((f) => f.querySelector(".os-stat-figure-label")?.textContent === label);
  return { value: el?.querySelector(".os-stat-figure-value")?.textContent ?? "", reason: el?.querySelector(".os-figure-absent")?.getAttribute("title") ?? "", change: el?.querySelector(".os-stat-figure-change")?.textContent ?? "" };
}
const list = (page: HTMLElement, title: string) => within(page).getByRole("region", { name: title });
const WEEK = { // the week Task 18's first test reads
  figures: { totalSales: "81.53", netSales: "65.99", orders: 2, averageOrderValue: "51.00", grossSales: "109.99", discounts: "8.00", refunds: "36.00", unitsSold: 3 },
  series: ["02", "03", "04", "05", "06", "07", "08"].map((day, i) => ({ at: `2026-11-${day}T00:00:00+01:00`, totalSales: null, orders: i === 3 || i === 4 ? 1 : 0, grossSales: null, discounts: null, refunds: null, unitsSold: i === 3 ? 3 : i === 4 ? 1 : i === 5 ? -1 : 0, netSales: i === 3 ? "91.99" : i === 4 ? "10.00" : i === 5 ? "-36.00" : "0.00" })),
  buyers: { new: 1, returning: 1, unknown: 0 },
};
```

```tsx
it("draws eight figures in order, money in the store's currency", async () => {
  const group = await figures((await openStoreStatistics(seed(WEEK))).page);
  expect([...group.querySelectorAll(".os-stat-figure-label")].map((l) => l.textContent)).toEqual(["Total sales", "Net sales", "Orders", "Average order value", "Gross sales", "Discounts", "Refunds", "Units sold"]);
  expect(figure(group, "Total sales").value).toBe("€81.53");
  expect(figure(group, "Average order value").value).toBe("€51.00");
  expect(figure(group, "Orders").value).toBe("2");
  expect(within(group).queryByRole("radio", { name: /^Average order value/ })).toBeNull();   // the average is not chartable
});
it("says the store is still being brought in, and draws every figure absent with that reason", async () => {
  const { page } = await openStoreStatistics(seed({ recording: { state: "starting", reasons: [], since: null } }));
  const group = await figures(page);
  expect(page.querySelector(".os-notice-line")?.textContent).toBe("The store's orders are still being brought in.");
  for (const label of ["Total sales", "Orders", "Units sold"]) expect(figure(group, label)).toMatchObject({ value: "--", reason: "The store's orders are still being brought in." });
});
it("gives the mirror's sentence and the moment the figures are as of, and names the Store page", async () => {
  const { page } = await openStoreStatistics(seed({ ...WEEK, asOf: "2026-11-07T09:00:00Z", recording: { state: "paused", reasons: [{ code: "paused", sentence: "Recording is paused for this store." }], since: "2026-11-07T09:00:00Z" } }));
  await figures(page);
  const notice = page.querySelector(".os-notice")?.textContent ?? "";
  expect(notice).toContain("Recording is paused for this store.");
  expect(notice).toContain(`These figures are as of ${formatMoment("2026-11-07T09:00:00Z")}.`);
  expect(notice).toContain("The Store page of this deployable says what fixes it.");
  expect(within(page.querySelector(".os-notice") as HTMLElement).queryByRole("button")).toBeNull();
});
it("says whether the store is recording is not known, and still draws the figures", async () => {
  const { page } = await openStoreStatistics(seed({ ...WEEK, recording: { state: "", reasons: [], since: null } }));
  const group = await figures(page);
  expect(page.querySelector(".os-notice-line")?.textContent).toBe("Whether this store is recording is not known.");
  expect(figure(group, "Net sales").value).toBe("€65.99");
});
it("draws the four figures a refund not yet read blanks as dashes with the reason", async () => {
  const { page } = await openStoreStatistics(seed({ ...WEEK, refundsNotRead: 1, figures: { ...WEEK.figures, refunds: null, netSales: null, totalSales: null, unitsSold: null } }));
  const group = await figures(page);
  for (const label of ["Refunds", "Net sales", "Total sales", "Units sold"]) expect(figure(group, label)).toMatchObject({ value: "--", reason: "1 refund is still being read from Shopify." });
  expect(figure(group, "Gross sales").value).toBe("€109.99");
  expect(page.querySelector(".os-notice-line")?.textContent).toBe("1 refund is still being read from Shopify.");
});
it("draws units as a dash with its own reason when a refund has more lines than were recorded", async () => {
  const group = await figures((await openStoreStatistics(seed({ ...WEEK, incompleteRefunds: 1, figures: { ...WEEK.figures, unitsSold: null } }))).page);
  expect(figure(group, "Units sold")).toMatchObject({ value: "--", reason: "A refund in this period has more lines than were recorded." });
});
it("draws money as dashes with the reason when the currency is not known, and every figure when the zone is not", async () => {
  const noCurrency = await figures((await openStoreStatistics(seed({ currency: null, figures: { ...WEEK.figures, totalSales: null, netSales: null, averageOrderValue: null, grossSales: null, discounts: null, refunds: null } }))).page);
  expect(figure(noCurrency, "Net sales")).toMatchObject({ value: "--", reason: "The store's currency has not been recorded yet." });
  expect(figure(noCurrency, "Orders").value).toBe("2");
  cleanup();
  const noZone = await figures((await openStoreStatistics(seed({ timezone: null }))).page);
  expect(figure(noZone, "Orders")).toMatchObject({ value: "--", reason: "The store's time zone has not been recorded yet." });
});
it("draws a floor as or more, with no change", async () => {
  const group = await figures((await openStoreStatistics(seed({ ...WEEK, floors: { grossSales: true }, incompleteOrders: 1, previous: { ...WEEK.figures, grossSales: "50.00" }, historySince: "2026-09-01T00:00:00Z" }))).page);
  expect(figure(group, "Gross sales").value).toBe("€109.99 or more");
  expect(figure(group, "Gross sales").change).toBe("");
});
it("draws a change against a period that can be compared, and none when it began before the orders did", async () => {
  const comparable = await figures((await openStoreStatistics(seed({ ...WEEK, previous: { ...WEEK.figures, orders: 1 }, historySince: "2026-09-01T00:00:00Z" }))).page);
  expect(figure(comparable, "Orders").change).toBe("+100%");
  cleanup();
  const tooEarly = await figures((await openStoreStatistics(seed({ ...WEEK, previous: { ...WEEK.figures, orders: 1 }, historySince: "2026-10-30T00:00:00Z" }))).page);
  expect(figure(tooEarly, "Orders").change).toBe("");
});
it("labels the chart's buckets in the store's time zone, draws the day below zero, and leaves gaps before the orders and after as-of", async () => {
  const series = WEEK.series.map((point, i) => (i === 0 || i === 6 ? { ...point, netSales: null } : point));
  const { page } = await openStoreStatistics(seed({ ...WEEK, series }));
  await click(within(await figures(page)).getByRole("radio", { name: /^Net sales/ }));
  const chart = page.querySelector(".os-time-chart") as HTMLElement;
  // "2026-11-02T00:00:00+01:00" is Nov 1 in UTC: the first label is the store's day.
  expect(chart.querySelector(".os-time-chart-times span")?.textContent).toBe("Nov 2");
  expect(chart.querySelector('.os-time-chart-grid[data-zero="true"]')).not.toBeNull();
  expect((chart.querySelector('path[data-series="current"]')?.getAttribute("d") ?? "").split("M").length - 1).toBe(1);   // one run of five days, a gap either side
  expect(chart.querySelector("figcaption")?.textContent).toContain("2 not measured");
});
it("says how many products there are and opens all of them as a page under the trail", async () => {
  const products = Array.from({ length: 25 }, (_, i) => ({ key: `gid://shopify/Product/${i + 1}`, title: `Inner tube ${i + 1}`, value: String(100 - i) }));
  const { page } = await openStoreStatistics(seed({ ...WEEK, productsBySales: products, productsByUnits: products, productCount: 31 }));
  await figures(page);
  expect(list(page, "Products").querySelectorAll(".os-ranked-label")).toHaveLength(8);
  expect(list(page, "Products").querySelector(".os-ranked-footer")?.textContent).toContain("The 8 best-selling of 31 products.");
  await click(within(list(page, "Products")).getByRole("button", { name: "All products" }));
  const all = await screen.findByRole("region", { name: `Store statistics for ${HOST}` });
  expect(within(within(all).getByRole("navigation", { name: "Breadcrumbs" })).getAllByRole("listitem").map((item) => (item.textContent ?? "").trim()).slice(-2)).toEqual(["Store statistics", "Products"]);
  expect(all.querySelectorAll(".os-ranked-label")).toHaveLength(25);
});
it("ranks products by units when Units is chosen", async () => {
  const { page } = await openStoreStatistics(seed({ ...WEEK, productsBySales: [{ key: "a", title: "City bike", value: "80.00" }, { key: "b", title: "Bell", value: "19.99" }], productsByUnits: [{ key: "b", title: "Bell", value: "5" }, { key: "a", title: "City bike", value: "2" }], productCount: 2 }));
  await figures(page);
  expect(list(page, "Products").querySelector(".os-ranked-label")?.textContent).toBe("City bike");
  await click(within(within(page).getByRole("navigation", { name: "Products by" })).getByRole("button", { name: "Units" }));
  expect(list(page, "Products").querySelector(".os-ranked-label")?.textContent).toBe("Bell");
});
it("writes the refunds that name no product as money, and says how many orders and refunds are short of lines", async () => {
  const { page } = await openStoreStatistics(seed({ ...WEEK, productsBySales: [{ key: "a", title: "City bike", value: "80.00" }], productCount: 1, refundsWithoutItem: "12.00", incompleteOrders: 3, incompleteRefunds: 1, floors: { grossSales: true } }));
  await figures(page);
  const footer = list(page, "Products").querySelector(".os-ranked-footer")?.textContent ?? "";
  expect(footer).toContain("Refunds of €12.00 name no product, so they are in Net sales and not here.");
  expect(footer).toContain("3 orders have more lines than were recorded, so these are floors.");
  expect(footer).toContain("1 refund has more lines than were recorded, so units are not exact.");
  expect(footer).not.toContain("12.00 EUR");
});
it("counts buyers as new, returning and with no customer recorded", async () => {
  const { page } = await openStoreStatistics(seed({ ...WEEK, buyers: { new: 4, returning: 2, unknown: 1 } }));
  await figures(page);
  expect([...list(page, "Buyers").querySelectorAll(".os-ranked-label")].map((l) => l.textContent)).toEqual(["New", "Returning", "No customer recorded"]);
});
it("draws the wholesale block only when the answer has one", async () => {
  const without = await openStoreStatistics(seed(WEEK));
  await figures(without.page);
  expect(within(without.page).queryByRole("region", { name: "Wholesale and retail" })).toBeNull();
  cleanup();
  const { page } = await openStoreStatistics(seed({ ...WEEK, wholesale: { wholesale: { orders: 1, netSales: "55.99" }, retail: { orders: 1, netSales: "10.00" } } }));
  await figures(page);
  expect([...list(page, "Wholesale and retail").querySelectorAll(".os-ranked-label")].map((l) => l.textContent)).toEqual(["Wholesale", "Retail"]);
  expect(list(page, "Wholesale and retail").textContent).toContain("€55.99");
});
it("says how many orders in another currency are not in the sums", async () => {
  const { page } = await openStoreStatistics(seed({ ...WEEK, otherCurrencies: [{ currency: "USD", orders: 2 }] }));
  await figures(page);
  expect(captions(page)).toContain("2 orders in USD are not in these sums.");
});
it("says no orders in this period, with a measured zero and a dash for the average", async () => {
  const zero = { totalSales: "0.00", netSales: "0.00", orders: 0, averageOrderValue: null, grossSales: "0.00", discounts: "0.00", refunds: "0.00", unitsSold: 0 };
  const { page } = await openStoreStatistics(seed({ figures: zero, buyers: { new: 0, returning: 0, unknown: 0 } }));
  const group = await figures(page);
  expect(figure(group, "Orders").value).toBe("0");
  expect(figure(group, "Net sales").value).toBe("€0.00");
  expect(figure(group, "Average order value")).toMatchObject({ value: "--", reason: "No orders in this period." });
  expect(list(page, "Products").textContent).toContain("No orders in this period.");
});
it("draws no divider between rows and no pill, badge or toast", async () => {
  const { page } = await openStoreStatistics(seed({ ...WEEK, productsBySales: [{ key: "a", title: "City bike", value: "80.00" }], productCount: 1 }));
  await figures(page);
  expect(page.querySelector("hr, .os-divider, .os-pill, .os-badge, .os-chip, .os-toast")).toBeNull();
});
```

- [ ] **Step 2:** In `clients/os` run `npx vitest run test/deployables/storeStatistics.test.tsx`. Expected: FAIL (the page draws its context and nothing after it).
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, `npm run typecheck` and `npx vitest run test/deployables`. Expected: PASS. Open every QA scene in both themes and at both widths (`clients/os/qa/README.md`) and look at each: no column collides, the eight figures stand as two rows of four and as four rows of two, the below-zero day is under the zero rule, every amount reads as `formatMoney` writes it.
- [ ] **Step 5: Commit** `os: a storefront's Store statistics page: its figures, its chart, its lists and its states`.

### Task 23: Documentation (U14)

**Files:**
- Modify: `docs/public/operate/deployables.md`, `docs/public/operate/shopify-connector.md`

Content, each under the page's existing headings where one fits:
- `deployables.md`, in "The Shopify storefront": the Store statistics page and who sees its button; the sales part, that owner and developer hold it by default, and how a client's group is granted it (`allow execute app:deployables/sales` beside `allow read app:deployables`); the destination switch and that it is not remembered; what the context lines say, the line about orders placed more than 60 days ago included, exactly; what "as of" means and when every figure is absent.
- `shopify-connector.md`: the refund detail the mirror now records, that an inlined list names the fields it keeps and why, and that it makes this release roll-forward only; that a store's shop row is read at connect, at boot, daily and on resume; that every order of a store is read once more under this version, carried over several sweeps for a large store; what is computed from the mirror, with the figure definitions of the record's section 5 in plain words, the rounding rule included; the known differences from Shopify's reports (5.5); the limits of section 10, among them refunds, edits and cancellations of orders older than 60 days, refunds whose detail cannot be read, and an order deleted in the store; what a customer's erasure does to the facts and that it reaches them at the next check; the walk and its minute; the check, what it compares, what `cleanAt` and `mismatched` mean, how a cluster owner reads them with `storeStatisticsCheckState`, and what `disagreeing` is for; how a cluster owner runs `storeStatisticsRebuild` and what the page shows meanwhile; that the part is granted to a client's group only after a clean check and the production walk.
- The `allowlist.yaml` header and `clients/os/DESIGN.md` were written in Tasks 2 and 20.

- [ ] **Step 1:** Run the docs gates of the Global Constraints before the edit. Expected: PASS.
- [ ] **Step 2:** Write the two pages. No registration function in its qualified form, no make command that is not a target, no store, person or hostname.
- [ ] **Step 3:** Run the docs gates again. Expected: PASS.
- [ ] **Step 4: Commit** `docs: the Store statistics page, the sales part, and what is computed from the mirror`.

### Task 24: The whole tree

**Files:** none changed, unless a gate names one or Step 3a raises a class in `scripts/ci/db-test-budgets.yaml` (and its pin in `scripts/ci/pipeline_db_tests_test.py`).

- [ ] **Step 1:** `docker exec memql-testdb createdb -U memql memql_stats_final`.
- [ ] **Step 2:** `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN='postgres://memql:memql_dev@127.0.0.1:55432/memql_stats_final?sslmode=disable' make test`. Expected: PASS. The two known flakes on a busy machine are `integrations/email` `TestAzureFeedbackDiscoversPreviouslySavedOrganization` and `component/packages` `TestSynchronousConfirmationSurvivesItsCallerAndTheSweep`; run either again alone before calling it a failure.
- [ ] **Step 3:** On the same fresh database, with `MEMQL_REQUIRE_DB=1` and `-p 1`, each database-gated tree this plan touched: `./component/storestats/`, `./integrations/shopify/`, `./component/datasync/`, `./component/memql/`, `./component/auth/`, `./component/automations/...`, `./component/database/...`, `./packs/wholesalepack/`, and `./app/` with `-tags bff`. Then `./core/statsperiod/ ./core/money/ ./cmd/shopifyschema/ ./test/dslconformance/... ./scripts/cidb/ ./scripts/ci/`, and `python3 -m unittest discover -s scripts/ci -p pipeline_db_tests_test.py`. Expected: PASS.
- [ ] **Step 3a: The budgets of the trees that already have a class (A30).** This plan adds engine-booting database tests to `integrations/shopify` (Tasks 2, 3, 4 and 9; class `shopify`, 600s, expected about 310s on the runner before them) and `component/datasync` (Tasks 2 and 3; class `datasync`, 300s, expected about 135s). Time each with the lane's flags (`go test -count=1 -p=1 -parallel=4 -timeout=900s <tree>`) on this branch and on main, side by side, as the file's comment for these two classes was measured. When this branch's time, at the factor that comment gives for these two trees (the runner took 2.2 to 2.5 times the laptop), passes about half its class's timeout, raise that class to about twice the expected runner time, with a comment in the file's manner naming the tasks that added the suites, and the same timeout in `pipeline_db_tests_test.py`; commit it as `ci: the mirror's suites keep their headroom with the store statistics tests`. `component/storestats` was measured in Task 19; `packs`, `component/database` and `component/memql` are in classes whose timeouts these few tests do not approach, and the report gives their times.
- [ ] **Step 4:** `make sdk-gen-check`, `make concept-snapshot-check`, `make platform-graph-check`, `make dsl-lint`, `scripts/ci/module-boundaries.sh`, and the docs gates. Expected: PASS.
- [ ] **Step 5:** In `clients/os`: `npm run typecheck`, `npm test`, `npm run build`. Expected: PASS.
- [ ] **Step 6:** Commit anything a gate made you regenerate, as `build: regenerate after the store statistics change`.
