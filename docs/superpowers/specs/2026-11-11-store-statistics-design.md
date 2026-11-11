---
title: A storefront deployable's own sales figures, computed from the orders the engine mirrors
audience: internal
status: draft
area: operate
sinceVersion: 0.31.34
owner: znas
---

# Store statistics: the sales of a storefront's store

Owner brief, 2026-11-11. On a deployable that is a Shopify storefront, and only on that
kind, beside the website Statistics button, a second button opens a page of the store's
own numbers: purchases, revenue and the like. They are computed from the orders the
engine already mirrors from Shopify. Shopify stays the source of truth; the engine keeps
the copy and reports on it.

This record reads what the store mirror records
(`docs/superpowers/specs/2026-11-11-store-mirror-recording-design.md`, called "the mirror
record" below). That record gives every connected store its own sync state, an automatic
backfill, a retry for failed deliveries and one verdict per store (`recording`,
`starting`, `paused`, `needs attention`, `not connected`). It lands first, and this one
depends on it (section 9).

Terms used throughout:

- **Mirrored order**: a `v1:shopify:order` row, with its `v1:shopify:orderLineItem` rows.
- **Fact**: one row in a table this design adds, holding what one order or one order line
  contributes to the figures. The figures are sums over facts.
- **Counted order**: an order whose facts enter the figures (D3).
- **Destination**: one of a deployable's two placements, Production and Testing, each bound
  to its own store (`site.binding.storeId` and `site.previewBinding.storeId`).
- **Store day**: a calendar day in the store's own time zone.

**Approvals.** S1 to S11 below are the owner's, given in the brief. Every other decision is
marked *Made here* and waits for the owner's review of this record.

## 1. What the owner asked for

| # | Requirement, in the owner's terms |
| --- | --- |
| R1 | A "Store statistics" button on a storefront deployable, beside Statistics, for people allowed to see sales. |
| R2 | Purchases, revenue and the like, for a chosen period, from the engine's copy of the store's orders. |
| R3 | The numbers agree with the store's own admin, and say so when they cannot be current. |
| R4 | No model, no agent, nothing sent to anyone. |

## 2. What exists, and what is missing

Read at `3d4fe13e9` (v0.31.33).

| # | Finding | Where |
| --- | --- | --- |
| F1 | Mirrored orders are readable by a cluster owner only. The concept is cluster-owner tier and every order query also requires `actor.isClusterOwner == true`. A client's own people cannot read an order through any query. | `dsl/shopify/generated/order.memql:22-23`; `dsl/shopify/overlay/queries.memql:141-155` |
| F2 | Nothing sums mirrored rows. The query language counts and nothing else; `sum` and `groupBy` run in process over rows already read. | `integrations/shopify/analytics.go:13-15`; `docs/public/language/functions.md:274-303` |
| F3 | The one walk over orders, `CommerceSold`, stops at 10 pages of 250 orders, reads each order's lines one query at a time, and sums no money. It is an agent tool, not a client read. | `integrations/shopify/analytics.go:33-37`, `:61`; `dsl/shopify/overlay/tools.memql:17-50` |
| F4 | No sales fact table, rollup or materialization exists. | searched `order_stat`, `orderFact`, `salesStat`, `storeSales`, `storeStatistics`: none |
| F5 | The email statistics are the pattern: a fact table filled by a scheduled walk through the engine under a maintenance principal, with `@serverOnly` queries, a watermark less five minutes, a fresh, roots-only read, and a read that groups the facts with SQL. | `component/emailstats/doc.go:9-58`; `component/emailstats/rollup.go:454`; `migrations/20261108090300_email_stat_facts.up.sql:27-50`; `dsl/campaigns/automations.memql:191-194` |
| F6 | The maintenance principal is a named synthetic cluster owner, granted only to automations on a compiled list. | `component/auth/maintenance_actor.go:8-60`, `:108-121`, `:268-313` |
| F7 | Money arrives as a decimal string inside a `MoneyBag`: `{shopMoney: {amount, currencyCode}, presentmentMoney: {...}}`. No decimal type exists in the engine. | `cmd/shopifyschema/mapping.go:721-735`; `dsl/shopify/generated/order.memql:63-65`; `go.mod` (no decimal module) |
| F8 | A line carries `quantity`, `currentQuantity`, `isGiftCard`, `originalTotalSet`, `discountAllocations` (each with `allocatedAmountSet`), `productGid`, `title`. Lines are fetched `first: 100` per order. | `dsl/shopify/generated/orderLineItem.memql:43-122`; `integrations/shopify/generated/model.go:4260-4300` |
| F9 | An order carries `test`, `cancelledAt`, `processedAt`, `sourceCreatedAt`, `currencyCode`, `customerGid`, `email`, `taxesIncluded`, `subtotalLineItemsQuantity`, `currentSubtotalPriceSet`, `totalDiscountsSet`, `totalRefundedSet`, `updatedAt` (Shopify's version) and `syncedAt` (the mirror's write time). | `dsl/shopify/generated/order.memql:31-34`, `:60`, `:82`, `:93`, `:118`, `:204`, `:237`, `:247`, `:260`, `:262`, `:270`, `:282` |
| F10 | The mirrored shop row holds `currencyCode`, `ianaTimezone` and `plan`; the mirror selects `plan { partnerDevelopment ... }`. | `dsl/shopify/generated/shop.memql:51`, `:72`, `:94`; `integrations/shopify/generated/model.go:6641` |
| F11 | The store row's `isDevelopment` is set at connect from Shopify's `plan.partnerDevelopment`. | `integrations/shopify/connect_write.go:46`, `:131-139`, `:253-275`; `integrations/shopify/store.go:43-49` |
| F12 | Statistics periods are a fixed list; every bound is a UTC instant. No time zone variant exists. | `core/statsperiod/statsperiod.go:11-15`, `:117-140` |
| F13 | The OS kit has the figure row, one chart, ranked lists, a period control and floors ("5,000 or more"), but no money formatter, and bucket labels are UTC only. | `clients/os/src/kit/statistics/StatFigures.tsx`; `kit/statistics/period.ts:62-77`; `kit/measure.ts:74-113`; `kit/format.ts` |
| F14 | Reviews is the exact gate wanted: only a `shopify_storefront`, not system-owned, with a bound store, for a holder of the part; it takes the pane. | `clients/os/src/apps/deployables/page/DeployablePage.tsx:449-460`, `:552` |
| F15 | A per-organization part is declared in six places: role defaults, seeds, the construct, the organization gate, the parity test and the OS part list. | `component/auth/rbac_model.go:254-307`; `dsl/rbac/seeds.memql:745-758`; `packs/reviewspack/dsl/builtins.memql:74-85`; `component/memql/organization_action_capabilities.go:13-85`; `component/memql/app_resource_os_parity_test.go:148`; `clients/os/src/apps/deployables/parts.ts:72-114` |
| F16 | Binding a site to a store requires that the caller can read the store, so a site's binding is an authorized link between the two. | `component/memql/platform_site_binding_guard.go:12-60` |
| F17 | The reviews pack checks that a named site serves a named store through `binding` or `previewBinding`. | `packs/reviewspack/request.go:433-441`; `packs/reviewspack/settings.go:329-349` |
| F18 | The wholesale pack keeps an entitlement per approved application, keyed by store and buyer email, in state `pending`, `granted`, `failed` or `revoked`. It ships disabled; a `v1:platform:packState` row enables it. | `packs/wholesalepack/dsl/concepts.memql:100-108`, `:115-120`; `packs/wholesalepack/pack.go` (`DefaultEnabled = false`) |
| F19 | The query path has a per-node result cache; a fresh read neither consults nor fills it. | `component/memql/engine.go:1189-1242`; `component/memql/fresh_read.go:36-45` |
| F20 | The deployables settings document remembers one period for every deployable, against the website periods. | `clients/os/src/apps/deployables/settings.ts:30-40`, `:69-77` |
| F21 | The mirror record's verdict builtin (`shopifyStoreVerdict`) and the lines fix (F9 there) are not on `main` yet. | mirror record sections 6 and 8 (U6, U8) |

## 3. Shape of the solution

One component, `component/storestats`, in the manner of `component/emailstats`. It is a
component because it reads this cluster's own rows and writes its own tables, and calls
no outside service.

**Fill.** Every minute a walk reads, through the engine, the orders and lines of each
connected store that changed since its watermark, and writes one fact per order and one
per line. A changed order replaces its facts, so a refund, an edit or a cancellation
changes them.

**Repair.** Every hour a check compares, within a page budget, every mirrored order and
line of a store with its facts by version, and recomputes whatever differs. A rebuild
deletes a store's facts and lets the walk write them again from the start, then the check
proves them.

**Read.** One client-callable builtin, `storeStatistics`, gated by a new part judged per
organization, checks that the named site serves the named store and answers the figures
with plain SQL over the fact tables. It reads no order row, so it does not need the
caller to be a cluster owner.

**Page.** A Store statistics page on the deployable, built from the statistics kit, with a
money formatter and time-zone labels added to the kit.

No model or agent is involved anywhere, and nothing is sent to anyone (S10).

## 4. Decisions

### 4.1 The owner's decisions

| # | Decision | Why |
| --- | --- | --- |
| S1 | *Owner.* The button "Store statistics" sits on the deployable's page beside Statistics. It is shown only for a `shopify_storefront` deployable with a bound store, and only to a holder of the sales part (S6). | R1. Revenue belongs where the storefront is managed. |
| S2 | *Owner.* For the chosen period: orders, gross sales, discounts, refunds, net sales, average order value, units sold; a sales-by-day line; top products by net sales and by units; new against returning buyers; wholesale against retail, only when the wholesale pack is enabled for the store, where an order is wholesale when its buyer holds granted wholesale access in that pack. | R2. |
| S3 | *Owner.* The periods are the website Statistics list: 24 hours, 7, 30 and 90 days, 12 months. No custom range. | One vocabulary across the app. |
| S4 | *Owner.* Money is in the store's own currency, summed with exact decimal arithmetic from Shopify's amounts, never floating point. Currency and time zone come from the mirrored shop row; a day is a store day. | R3. A merchant's day is their own. |
| S5 | *Owner.* The numbers come from a fact table the engine fills as orders are recorded and changed, in the manner of the email statistics. A refund, edit or cancellation changes the order's facts. The design states what fills, repairs and rebuilds the facts. | The page must not walk thousands of rows on every open, nor depend on the reader being the cluster owner (F1, F3). |
| S6 | *Owner.* Access is a new organization part for sales figures, separate from website Statistics, declared everywhere `reviews` is and granted to a group the same way. The server checks the part and that the site and store named belong together. | Revenue is more sensitive than visits. |
| S7 | *Owner.* The page shows the store of the destination being looked at and lets the reader switch. | Production and Testing are different stores. |
| S8 | *Owner.* Cancelled orders are not sales. On a live store, Shopify's test orders are left out. On a development store every order is a test order, so they are counted, and the page says so in one line. | A development store would otherwise read zero forever. |
| S9 | *Owner.* The page states when the last order change was recorded, and when the mirror is not recording it shows that, with the mirror's reason, rather than stale numbers presented as current. | R3. |
| S10 | *Owner.* No model or agent. Nothing is sent to anyone. | R4. |
| S11 | *Owner.* A value that cannot be known is shown as absent, never as zero. | The house rule (`clients/os/DESIGN.md`, "Not measured is a dash"). |

### 4.2 Decisions made here

| # | Decision | Why |
| --- | --- | --- |
| D1 | *Made here.* **Every money figure is computed from the order's lines, not its order-level totals.** Section 5 gives the formulas. | Only lines separate gift cards (which Shopify's sales reports leave out), give units and products, and make the top products add up exactly to net sales. Order-level totals are kept on the fact only to cross-check (D14). |
| D2 | *Made here.* **An order is dated by `processedAt`, falling back to `sourceCreatedAt`, and the whole order, refunds and edits included, stays on that date.** | Facts per order, replaced on change, are what S5 asks for. Shopify's own reports date a return on the day it happens; this page does not, and its help says so (5.3, live check L5). |
| D3 | *Made here.* **A counted order** is one whose mirrored row is not deleted, whose `cancelledAt` is empty, and which is not a test order, unless the store is a development store, where test orders count. | S8. |
| D4 | *Made here.* **A store is a development store when its mirrored shop row says `plan.partnerDevelopment = true`; when no shop row is mirrored yet, when the store row's `isDevelopment` says so.** | The shop row is Shopify's current answer, refreshed by the mirror (F10). The store row's flag is the same answer taken at connect (F11), so it is the fallback. A store an operator marked `isDevelopment` that Shopify bills as a live store keeps excluding test orders, because its orders are not all test orders. |
| D5 | *Made here.* **A fact carries the order's instant (`ordered_at`), never a precomputed day.** The read cuts store days with the time zone it holds now. | A time zone change then re-buckets history in the new zone with no rebuild, which is also how a merchant reads their own admin after changing it. |
| D6 | *Made here.* **Money is summed only for orders in the store's current currency.** Orders in another currency are counted in Orders and Units, and the page says in one line how many there are and in which currency. | S4 forbids adding two currencies. No conversion rate is known to the engine. |
| D7 | *Made here.* **Shop money only.** `presentmentMoney` (the buyer's currency) is never read. | It is summable only per buyer currency, and Shopify reports in shop money. |
| D8 | *Made here.* **The walk runs every minute** (`15 * * * * *`), each store at most 40 pages of each source per run. | "As orders are recorded" (S5). A mirrored order is on the page within about two minutes of reaching the mirror. |
| D9 | *Made here.* **Wholesale is judged now, not at order time.** An order is wholesale when its `email`, case-folded and trimmed, equals the `buyerEmail` of an entitlement of the same store in state `granted`. A change to a store's granted set recomputes the flag on all its orders. | The pack keeps the current state, not a history the engine can join on (F18). A revoked buyer's past orders move to retail; the help says so. The email is read in memory and never stored in a fact. |
| D10 | *Made here.* **"Wholesale enabled for the store"** means the wholesale pack is enabled on the cluster and a `wholesaleSettings` row exists for that store. | A pack that is on but never set up for a store has no wholesale buyers to tell apart. |
| D11 | *Made here.* **A buyer is the order's `customerGid`.** In a period, a buyer is new when their first counted order in the store's facts falls inside the period, returning when they have a counted order before it. Orders with no customer are counted apart as "No customer recorded". | No personal data is needed. The history begins where the mirror's does (60 days before connect, mirror record D11), so "new" means new since then, and the page says since when. |
| D12 | *Made here.* **The page shows seven figures in one row**: Net sales, Orders, Average order value, Units sold, then Gross sales, Discounts, Refunds. All but the average are chartable. | S2 names seven. One figure row, as the statistics rule asks; the kit's grid already draws seven as four then three, and `clients/os/DESIGN.md` gains that sentence. |
| D13 | *Made here.* **The period is the same remembered choice as website Statistics** (`statisticsPeriod`). The destination is not remembered; the page opens on Production when it has a store, else Testing. | Same list (S3); a person comparing traffic and sales asks about the same period. A destination choice that survived would open a Testing store's sales when someone came for Production's. |
| D14 | *Made here.* **The facts keep Shopify's order-level totals beside the line sums**, and the check (U7) counts orders where they disagree. Nothing on the page draws them. | A cheap, permanent test of D1 against Shopify's own arithmetic on live data, and the place a wrong reading of a field shows first. |
| D15 | *Made here.* **The button is shown when either destination has a store**; Reviews shows only for Production's. | S7: a storefront exercised only against a development store still has sales to read. |
| D16 | *Made here.* **When the store's verdict is not `recording`**, the figures are drawn as of the time the verdict stopped being `recording` (its `since`), with the verdict's sentence in a `Notice`; buckets after that instant are gaps. During `starting` and during a rebuild every figure is absent. | S9 and S11: what was recorded before the break is true as of then; what came after is not known. While history is still arriving no total is known. |
| D17 | *Made here.* **Facts hold no personal data**: no email, name, address or phone. A buyer is an opaque Shopify id. When the store's data is removed (`redactedAt` set), the store's facts are deleted. | The walk reads every store's orders; the figures need none of it. |
| D18 | *Made here.* **Facts are kept as long as their order is mirrored.** No 400-day retention. | New against returning needs the whole history, and one row per order is small. |
| D19 | *Made here.* **A fact definition version** (`storeStatsDefinition`, an integer constant) is stored per store. A release that changes a formula raises it, and the walk rebuilds each store whose stored version is lower. | A changed definition must never mix with facts computed the old way. |
| D20 | *Made here.* **The part is `app:deployables/sales`**, labelled "Sales figures", on owner and developer by default, like `reviews`. | S6. A short resource name in the existing family. |

## 5. The figures

### 5.1 Per line

For a line L of a counted order, gift-card lines excluded (`isGiftCard = true`):

| Name | Formula | Source |
| --- | --- | --- |
| q0 | `quantity` | units ordered, edits included |
| q1 | `currentQuantity` | units still on the order after removals and refunds (`analytics.go:101-104` already uses it for units) |
| G(L) | `originalTotalSet.shopMoney.amount` | the line's price before discounts, times q0 |
| D(L) | sum of `discountAllocations[].allocatedAmountSet.shopMoney.amount` | every discount allocated to the line: line, order and code discounts |
| R(L) | `(G(L) - D(L)) * (q0 - q1) / q0`, rounded half to even to the scale of G(L); 0 when q0 is 0 | the discounted value of the units no longer on the order |
| N(L) | `G(L) - D(L) - R(L)` | the line's net sales |

The scale of an amount is the number of decimals Shopify wrote in it ("42.00" has two), so
rounding needs no table of currencies. R(L) is the only division in the design.

### 5.2 Per period

Over the counted orders whose `ordered_at` falls in the period, in the store's currency (D6):

| Figure | Definition | Shipping, tax, duties, tips |
| --- | --- | --- |
| Orders | count of counted orders | none |
| Gross sales | sum of G(L) | excluded |
| Discounts | sum of D(L), shown as a positive amount | shipping discounts excluded, since they are allocated to shipping lines, not to items |
| Refunds | sum of R(L) | excluded |
| Net sales | sum of N(L), which equals Gross minus Discounts minus Refunds | excluded |
| Average order value | (Gross sales minus Discounts) / Orders, to four decimals; absent when Orders is 0 | excluded |
| Units sold | sum of q1 | none |
| Top products by net sales | sum of N(L) grouped by `productGid`; a line with no product is grouped by its title | excluded |
| Top products by units | sum of q1 by the same key | none |
| New and returning buyers | distinct `customerGid` by D11 | none |
| Wholesale and retail | Orders and Net sales split by D9 | excluded |
| Sales by day | Orders, Gross, Discounts, Refunds, Net and Units per bucket: hour for 24 hours, store day for 7 to 90 days, store month for 12 months | |

A product's label is the title of its newest line in the period.

### 5.3 Against Shopify's own reports

What this repository establishes is the shape of the fields, not Shopify's meaning: the
generated comments say only "Admin GraphQL <field>" (F8). The definitions follow
Shopify's published meaning of each field as this record understands it, and each point
that comparison depends on is a live check, run in the production walk (section 11.3)
before the page is announced.

| Point | This page | Shopify's sales report, as understood | Live check |
| --- | --- | --- | --- |
| Gross sales | item price times quantity ordered, gift cards out | the same | L1: `originalTotalSet` equals unit price times `quantity`, also after an edit that adds a line |
| Discounts | allocations on item lines | item and order discounts | L2: the sum of a line's `discountAllocations` over all lines equals `totalDiscountsSet` when no shipping discount applies |
| Refunds | discounted value of units removed, on the order's day | "Returns", on the day of the return | L3: `currentQuantity` drops on a refund with and without restock, and on an edit that removes a unit. L5: the date the report puts an order on (`processedAt` or `createdAt`) |
| A refund of an amount with no line | not in Refunds | in Returns or adjustments | L4: refund a fixed amount and compare. Carried as a known difference (section 10) |
| Cancellation | the order leaves every figure | a sale on its day and a return on the cancel day | none: S8 decides |
| Tax-inclusive prices | amounts as Shopify gives them, so tax inside the price is inside Gross | thought to exclude tax | L6: on a tax-inclusive store, compare Gross sales |
| Average order value | (Gross minus Discounts) / Orders | thought to be the same | L9 |
| Edits | change the order's own day | dated on the edit's day | none: D2 |

When a live check disagrees with this table, the formula changes, `storeStatsDefinition`
is raised (D19), and this table is corrected in the same change.

## 6. Data

### 6.1 Tables

One migration, `component/database/memory-nodes/migrations/20261112090000_store_sales_facts.up.sql`
with its `.down.sql`. Every id is bare. Every money column is `NUMERIC` with no scale, so a
sum is exact in the database.

`store_sales_order_fact`, one row per mirrored order:

| Column | Type | Meaning |
| --- | --- | --- |
| `store_id` | `TEXT NOT NULL` | the store |
| `order_gid` | `TEXT NOT NULL` | Shopify's id |
| `order_version` | `TIMESTAMPTZ NOT NULL` | the order's `updatedAt` the fact was computed from |
| `recorded_at` | `TIMESTAMPTZ NOT NULL` | the order's `syncedAt` |
| `ordered_at` | `TIMESTAMPTZ NOT NULL` | D2 |
| `currency` | `TEXT NOT NULL` | `currencyCode` |
| `cancelled` | `BOOLEAN NOT NULL` | `cancelledAt` is not empty |
| `test` | `BOOLEAN NOT NULL` | `test` |
| `taxes_included` | `BOOLEAN NOT NULL` | `taxesIncluded` |
| `buyer_key` | `TEXT NOT NULL DEFAULT ''` | `customerGid`, empty when none |
| `wholesale` | `BOOLEAN NOT NULL DEFAULT false` | D9 |
| `expected_quantity` | `INTEGER NOT NULL` | `subtotalLineItemsQuantity` |
| `shopify_current_subtotal` | `NUMERIC` | `currentSubtotalPriceSet.shopMoney.amount`, for D14 |
| `shopify_total_discounts` | `NUMERIC` | `totalDiscountsSet.shopMoney.amount`, for D14 |
| `shopify_total_refunded` | `NUMERIC` | `totalRefundedSet.shopMoney.amount`, for D14 and L4 |
| `gross`, `discounts`, `refunds`, `net` | `NUMERIC NOT NULL` | sums of the order's line facts |
| `units` | `INTEGER NOT NULL` | sum of q1 |
| `line_count` | `INTEGER NOT NULL` | line facts written, gift cards included |
| `lines_version` | `TIMESTAMPTZ` | newest `updatedAt` among its lines |
| `lines_complete` | `BOOLEAN NOT NULL` | the sum of `quantity` over all its lines is at least `expected_quantity` (L8) |

Primary key `(store_id, order_gid)`; indexes `(store_id, ordered_at)` and
`(store_id, buyer_key, ordered_at)`.

`store_sales_line_fact`, one row per mirrored line:

| Column | Type | Meaning |
| --- | --- | --- |
| `store_id`, `order_gid`, `line_gid` | `TEXT NOT NULL` | primary key |
| `line_version` | `TIMESTAMPTZ NOT NULL` | the line's `updatedAt` |
| `gift_card` | `BOOLEAN NOT NULL` | `isGiftCard` |
| `product_key` | `TEXT NOT NULL` | `productGid`, or `title:` and the title |
| `title` | `TEXT NOT NULL` | the line's `title` |
| `quantity` | `INTEGER NOT NULL` | q0 |
| `units` | `INTEGER NOT NULL` | q1 |
| `gross`, `discounts`, `refunds`, `net` | `NUMERIC NOT NULL` | 5.1 |

Index `(store_id, product_key)`.

`store_sales_store`, one row per store the walk has seen:

| Column | Type | Meaning |
| --- | --- | --- |
| `store_id` | `TEXT PRIMARY KEY` | |
| `currency`, `timezone` | `TEXT NOT NULL DEFAULT ''` | from the shop row; empty means not known |
| `development` | `BOOLEAN` | D4; null means not known |
| `history_since` | `TIMESTAMPTZ` | "Orders since" from the store's health (mirror record, section 13), else the oldest fact |
| `wholesale_enabled` | `BOOLEAN NOT NULL DEFAULT false` | D10 |
| `wholesale_digest` | `TEXT NOT NULL DEFAULT ''` | SHA-256 of the sorted, case-folded granted emails; never the emails |
| `wholesale_cursor` | `TEXT NOT NULL DEFAULT ''` | where a wholesale pass resumes; empty when none runs |
| `verdict_state` | `TEXT NOT NULL DEFAULT ''` | copied from `shopifyStoreVerdict`; empty means not known |
| `verdict_reasons` | `JSONB NOT NULL DEFAULT '[]'` | `[{code, sentence}]`, the mirror's own words |
| `verdict_since` | `TIMESTAMPTZ` | |
| `last_change_at` | `TIMESTAMPTZ` | newest `recorded_at` folded into facts |
| `walked_through` | `TIMESTAMPTZ` | how far both walks have read, never past the run's clock |
| `rebuilding_since` | `TIMESTAMPTZ` | set by a rebuild, cleared when the walk catches up |
| `definition` | `INTEGER NOT NULL DEFAULT 0` | D19 |

`store_sales_watermark`: `store_id TEXT`, `source TEXT` (`order`, `line`, `entitlement`),
`through TIMESTAMPTZ NOT NULL`; primary key `(store_id, source)`.

`store_sales_check`: `store_id TEXT PRIMARY KEY`, `started_at TIMESTAMPTZ`,
`order_cursor TEXT`, `line_cursor TEXT`, `finished_at TIMESTAMPTZ`, `mirror_orders INTEGER`,
`fact_orders INTEGER`, `mismatched INTEGER`, `repaired INTEGER`, `disagreeing INTEGER`
(D14), `clean_at TIMESTAMPTZ`.

None of these tables is a concept; no row admission sees them, and only
`component/storestats` reads or writes them.

### 6.2 Constructs

All in `dsl/shopify/overlay/` unless named otherwise. Every query is `@serverOnly`, has
`actor.isClusterOwner == true` in its filter, as the email walk's do
(`dsl/campaigns/queries.memql:1089-1121`), and is asked only by `component/storestats`.

| Kind | Name | What |
| --- | --- | --- |
| shape | `storeStatisticsOrder` | `storeId, gid, updatedAt, syncedAt, deleted, processedAt, sourceCreatedAt, cancelledAt, test, taxesIncluded, currencyCode, customerGid, email, subtotalLineItemsQuantity, currentSubtotalPriceSet, totalDiscountsSet, totalRefundedSet`. `email` is used only by the wholesale judgement (D9). |
| shape | `storeStatisticsLine` | `storeId, gid, parentGid, updatedAt, deleted, isGiftCard, productGid, title, quantity, currentQuantity, originalTotalSet, discountAllocations` |
| shape | `storeStatisticsVersion` | `gid, parentGid, updatedAt, deleted` |
| query | `storeStatisticsOrdersChanged(storeId, since)` | orders with `row.createdAt >= since`, deleted ones included, `sort "row.createdAt", "asc"`, `paginate 250` |
| query | `storeStatisticsLinesChanged(storeId, since)` | the same over lines |
| query | `storeStatisticsOrderVersions(storeId)` and `storeStatisticsLineVersions(storeId)` | every row, keyset-paged by id, for the check |
| query | `storeStatisticsOrderBuyers(storeId)` | `gid, email, deleted`, keyset-paged, for the wholesale pass |
| query | `storeStatisticsShop(storeId)` | `currencyCode, ianaTimezone, plan` |
| query | `storeStatisticsStores()` | store rows: `storeId, isDevelopment, uninstalledAt, redactedAt`, and the "Orders since" on `health` |
| query (wholesale pack, `packs/wholesalepack/dsl/queries.memql`) | `wholesaleStatisticsGranted(storeId)` | `buyerEmail` of entitlements in state `granted`, and whether a `wholesaleSettings` row exists |
| query (wholesale pack) | `wholesaleStatisticsChanged(since)` | entitlements with `row.createdAt >= since`, `storeId` and `updatedAt` only |
| builtin | `storeStatisticsRollup {}` | executor `integration.storeStats.rollup`; no `@sdk`; admits the principal `system:maintenance:storeStatisticsRollupSweep` and no other caller, a cluster owner included |
| builtin | `storeStatisticsCheck {}` | executor `integration.storeStats.check`; no `@sdk`; admits `system:maintenance:storeStatisticsCheckSweep` only |
| builtin | `storeStatisticsRebuild { storeId string! }` | executor `integration.storeStats.rebuild`; no `@sdk`; admits a cluster owner only, checked in Go |
| builtin | `storeStatistics { siteId string!, storeId string!, period string! }` | executor `integration.storeStats.statistics`; `@sdk`, `@args(profile="object")`, `@requiresCapability("execute", "app:deployables/sales")` |
| automation | `storeStatisticsRollupSweep` | `@trigger(schedule="15 * * * * *")`, calls the rollup |
| automation | `storeStatisticsCheckSweep` | `@trigger(schedule="0 40 * * * *")`, calls the check |
| maintenance principal | `storeStatisticsRollupSweep`, `storeStatisticsCheckSweep` | added to the compiled list in `component/auth/maintenance_actor.go` with their reasons |
| part | `app:deployables/sales` | section 7 |

The wholesale queries sit in the pack because the rows are the pack's. When the pack is
disabled the component does not ask them and treats the store as D10's "not enabled".

`component/storestats` registers as the `storeStats` integration provider on every node
type, the way `component/sitetraffic/integration.go` registers `siteTraffic`, and is linked
in by the same blank-import list in `app/` that links `component/emailstats`.

### 6.3 What `storeStatistics` requires

1. The caller holds `execute app:deployables/sales` in the organization of the named site.
   The engine's organization gate judges it before the handler runs: `app:deployables/sales`
   joins `organizationDeployableCapability`, and `storeStatistics` maps to
   `v1:platform:site` / `siteId` in `organizationCapabilityTarget`.
2. The site reads for the caller under their own actor (`siteById`), is of kind
   `shopify_storefront` and is not system-owned.
3. Its `binding.storeId` or `previewBinding.storeId` equals `storeId`. A binding is an
   authorized link (F16), so this is what ties the store to the organization.
4. `period` is one of `statsperiod.All()`.

Each refusal is one sentence and names nothing the caller could not already read: a site
that does not read and a site that does not serve the store answer the same sentence,
"This deployable does not serve that store for you."

The handler then reads only `store_sales_*` with SQL, in one read-only transaction.

### 6.4 The answer

One synthetic node, as the website and email reads answer. Money is a decimal string;
everything absent is `null`.

```
{
  storeId, siteId, destination: "production" | "testing" | "both",
  currency, timezone, development, period, bucket, start, end, prevStart,
  historySince, lastChangeAt, asOf,
  recording: { state, reasons: [{ code, sentence }], since },
  rebuilding,
  figures: { orders, grossSales, discounts, refunds, netSales, averageOrderValue, unitsSold },
  previous: { ...the same, or null when not comparable },
  floors: { unitsSold, grossSales, discounts, refunds, netSales },
  series: [{ at, orders, grossSales, discounts, refunds, netSales, unitsSold }],
  productsBySales: [{ key, title, value }], productsByUnits: [{ key, title, value }],
  productCount, incompleteOrders,
  buyers: { new, returning, unknown },
  wholesale: null | { wholesale: { orders, netSales }, retail: { orders, netSales } },
  otherCurrencies: [{ currency, orders }]
}
```

`series[].at` is the bucket's start as an instant with the store's offset. `floors` says
which figures are floors because `incompleteOrders` is not zero. Top lists carry 25 rows.

## 7. Access

The part is declared everywhere `reviews` is:

1. `component/auth/rbac_model.go`, `appPartGrants`: `"app:deployables/sales": {RoleOwner, RoleDeveloper}`, with a comment giving S6's reason.
2. `dsl/rbac/seeds.memql`: `cap-owner-execute-app-deployables-sales` and `cap-developer-execute-app-deployables-sales`.
3. The construct: `@requiresCapability("execute", "app:deployables/sales")` on `storeStatistics`.
4. `component/memql/organization_action_capabilities.go`: the resource in `organizationDeployableCapability`, the builtin in `organizationCapabilityTarget`, with a test beside `reviews_capability_test.go`.
5. `component/memql/app_resource_os_parity_test.go`: the part in its list and the builtin in the construct table.
6. `clients/os/src/apps/deployables/parts.ts`: `"sales"` in `DEPLOYABLE_PARTS`, `NO_PARTS` and `ALL_PARTS`, so `partsForOrganization` judges it per organization.

A client's people receive it the way they receive reviews: their organization's group is
granted `allow execute app:deployables/sales`, beside `allow read app:deployables`. Settings
and Users list the part from the engine's grants, so no list there changes.

## 8. The page

`clients/os/src/apps/deployables/storeStatistics/`, taking the pane as Reviews does
(`DeployablePage.tsx:449-460`), because it carries its own controls.

**The button.** In the Head, after Statistics: `IconButton label="Store statistics"` with
the `Receipt` icon. Present only when `can.sales`, the site is a `shopify_storefront`, not
system-owned, and `boundStoreId(site)` or `previewStoreId(site)` is not empty (D15). Absent
otherwise, never disabled (`clients/os/DESIGN.md:170-175`, rule 12). `WorkspaceDetail` gains
`"storeStatistics"`; `detailTitle` gains "Store statistics".

**The Head.** Trail: back label, deployable name, "Store statistics", through
`breadcrumbs` and `back` (`DESIGN.md:722-745`). At its right, quiet (`DESIGN.md:561-572`):

- the **destination control**, a `Select tone="quiet"` reading "Production" or "Testing",
  drawn only when both destinations name stores and the two differ. When both name the
  same store there is no control and the context line says "Production and Testing".
  It is local to the deployables app, `DestinationControl.tsx`, since no other app has
  destinations.
- `PeriodControl` over `SITE_PERIODS`, the remembered `statisticsPeriod` (D13).

**Blocks, top to bottom.** No dividers between rows; spacing only (`DESIGN.md:118-126`).

1. **Context line**, a `Caption`: the store's name or domain, "Prices in EUR", "Days in
   Europe/Madrid", "Orders since Sep 12", "Last order change recorded 4 minutes ago"
   (`formatFreshness`). On a development store a second line: "This is a development
   store: every order on it is a test order, and they are counted." On a tax-inclusive
   store: "Prices on this store include tax."
2. **Recording notice**, a `Notice`, only when the verdict is not `recording`, a rebuild runs,
   or the verdict is not known: the mirror's sentence, then "These figures are as of
   <moment>." (D16). For `starting` and a rebuild: "The store's orders are still being
   brought in." and every figure absent. The notice names the Store page, where the act
   that fixes it lives, and holds no act of its own.
3. **Figure row**, `StatFigures`, seven figures (D12), money with `formatMoney`, counts with
   `formatCount`, each with its previous period only when `previousComparable(prevStart,
   historySince)` (`DESIGN.md:577-589`). A floor reads "or more" and has no change.
4. **Chart**, `TimeChart` of the chosen figure, labels in the store's time zone. Buckets before
   `historySince` or after `asOf` are gaps (`DESIGN.md:590-594`).
5. **Products**, `RankedList` titled "Products", with a `LocalTabs` control under its heading
   (the Devices pattern, `DESIGN.md:601-604`): "Net sales" or "Units". Eight rows; when
   `productCount` is larger, its footer says "The 8 best-selling of 31 products." and
   opens "All products" as a page under the trail (`DESIGN.md:605-612`). When
   `incompleteOrders` is not zero, a footer line: "3 orders have more lines than were
   recorded, so these are floors."
6. **Buyers**, `RankedList` with rows New, Returning, No customer recorded; values are counts.
7. **Wholesale and retail**, `RankedList`, only when `wholesale` is not null: two rows,
   Wholesale and Retail, with a `LocalTabs` control "Net sales" or "Orders".
8. **Other currencies**, a `Caption`, only when `otherCurrencies` is not empty: "2 orders in
   USD are not in these sums."

Rows carry their label and value only: no badge, no pill, no toast in a row. The figure
definitions of section 5, and the known differences from Shopify's reports, are in the
page's shared help control, not on the page.

**States.**

| State | What is drawn |
| --- | --- |
| Loading | `TimeChartSkeleton`, `loading` on `StatFigures` and each `RankedList`, the context line as an `InlineSkeleton` (`DESIGN.md:458-482`, `:595-600`). A refresh keeps what it has. |
| Refused or failed | A `Notice` with the engine's sentence, kept while the read is tried again; no shape over it. |
| No part | The button is absent. A part withdrawn while the page is open gets the server's refusal as above. |
| Empty | After a successful read with no counted orders: figures are a measured 0 where history covers the period, the average is a dash ("No orders in this period."), each list says "No orders in this period." |
| Not recording | Block 2, and D16. |
| Currency or time zone not known | Money figures are dashes, "The store's currency has not been recorded yet."; with no time zone every figure is a dash, because no day can be cut. |
| History shorter than the period | Buckets before `historySince` are gaps; no change is drawn. |

**Cadence.** `AutoRefresh` at the website Statistics' cadence (`statisticsRefreshMs`: five
minutes for 24 hours, fifteen otherwise). A rollup table publishes no event, so nothing
pushes.

**Kit additions** (`clients/os/src/kit`):

- `formatMoney(currency: string): (value: number) => string` in `kit/format.ts`, over
  `Intl.NumberFormat(undefined, {style: "currency", currency})`, the currency's own
  decimals; a non-finite value renders "--". The value is the server's exact decimal
  string converted with `Number()`. Every sum the page can show has at most 15 significant
  digits (under ten trillion with cents), which a double holds and prints back exactly, so
  the display loses nothing; the exact arithmetic is the server's.
- `bucketLabel` and `bucketMoment` take an optional IANA `timeZone`, through
  `Intl.DateTimeFormat`; `TimeChart` passes a new optional `timeZone` prop to them. Without
  it, both stay UTC, so the existing pages do not change.
- `clients/os/DESIGN.md`, "App overviews": "seven stand as four then three"; "Statistics":
  `formatMoney` joins the formatters, and a store's buckets are labelled in its time zone.

## 9. Units of work

Ordered so the server side is proved before the page exists.

| Unit | What | Files | Interfaces | Depends on | Proved by |
| --- | --- | --- | --- | --- | --- |
| U1 Periods in a zone | `ForIn(p Period, now time.Time, loc *time.Location) Bounds`; `For` becomes `ForIn(p, now, time.UTC)`. Day and month starts are local midnights; hours are local hours, so a zone offset by 30 minutes cuts at its own hour. | `core/statsperiod/statsperiod.go`, `statsperiod_test.go` | `ForIn` | none | a spring-forward day has 23 hours and a fall-back day 25; a +05:30 zone's 24 hours start on its own hour; `For` answers what it answered before |
| U2 Exact money | `core/money`: `Parse(string) (Amount, error)` accepting only `-?digits[.digits]`; `Add`, `Sub`, `MulRatio(num, den int64)` rounding half to even to the operand's scale; `Scale`; `String`. Built on `math/big`. | `core/money/money.go`, `money_test.go` | `Amount` | none | refuses exponents, spaces, empty strings and floats; a million random sums equal `big.Rat` sums; `0.1 + 0.2` is `0.3` |
| U3 Tables | The migration of 6.1. | `component/database/memory-nodes/migrations/20261112090000_store_sales_facts.{up,down}.sql`; `component/storestats/main_dbschema_test.go` | | none | up, down, up again on Postgres and TimescaleDB |
| U4 Part | Section 7, steps 1, 2, 4 and 5 (the construct lands in U5). | as listed in section 7 | | none | the parity test; an organization-gate test: a holder in one organization is refused a site of another |
| U5 Constructs | Shapes, queries, builtins and automations of 6.2; the two maintenance principals. | `dsl/shopify/overlay/{shapes,queries,builtins,automations}.memql`; `packs/wholesalepack/dsl/queries.memql`; `component/auth/maintenance_actor.go` | 6.2 | U4 | DSL validation and the edition gate; each query answers nothing to a reader actor and rows to the principal |
| U6 Fill | The walk: per store, read changed orders and lines from the watermark less five minutes under the principal, internal origin and a fresh, roots-only read (as `component/emailstats/rollup.go:454` does); compute 5.1 with U2; upsert line facts, then re-derive each touched order's sums from its line facts in SQL; delete facts of tombstoned rows; copy the shop row, D4 and the verdict into `store_sales_store`; run a wholesale pass when the granted digest changed; rebuild on a lower `definition`. Writes are guarded so an older version never replaces a newer one: `ON CONFLICT ... DO UPDATE ... WHERE excluded.order_version >= fact.order_version` (and the same on `line_version`). | `component/storestats/{doc,integration,facts,rollup,wholesale}.go` and tests | `handleRollup` | U1 to U5 | database tests in section 11.1 |
| U7 Check and rebuild | The check: per store, page through `storeStatisticsOrderVersions` and `storeStatisticsLineVersions` (40 pages a run, resuming), compare with the facts by gid and version, recompute or delete what differs, count D14 disagreements, record `store_sales_check`. The rebuild: in one transaction delete the store's facts, watermarks and check row and set `rebuilding_since`; the walk refills; when `walked_through` reaches the clock less five minutes, `rebuilding_since` clears and a check starts at once. A rebuild is proven when that check finishes with `mismatched = 0`. | `component/storestats/{check,rebuild}.go` and tests | `handleCheck`, `handleRebuild` | U6 | section 11.1 |
| U8 Read | The gate of 6.3; the SQL of 5.2 with store days cut by `AT TIME ZONE`; previous period, floors, D6, D11, D16; the answer of 6.4. | `component/storestats/{statistics,statistics_payload}.go` and tests | `handleStatistics` | U6 | section 11.1, and `statistics_scale_test.go` |
| U9 Independent totals | A fixture store of Shopify-shaped orders (discount codes, an order-level discount, a gift card, a refund with and without restock, an edit, a cancellation, a test order, two currencies, a buyer with three orders, a wholesale buyer, an order across a local midnight, one order with 120 lines) applied through the mirror's own write path; then the walk; then `storeStatistics`. A reference calculator in the test, written from section 5 over the fixture JSON and sharing no code with U6 or U8, must give the same figures to the cent for every period. | `component/storestats/reference_db_test.go`, `component/storestats/testdata/fixture-store/` | | U8 | the test |
| U10 SDK | Regenerate the client builders so `query.storeStatistics` exists. | `sdk/go/client/generated_builtins.go`, the TS SDK | | U5 | the generators' drift tests |
| U11 Kit | Section 8, kit additions. | `clients/os/src/kit/format.ts`, `kit/statistics/{period.ts,TimeChart.tsx}`, `clients/os/DESIGN.md` | `formatMoney`, `timeZone` | none | unit tests: three currencies with 0, 2 and 3 decimals; a bucket at 23:30 UTC labelled as the next day in a +01:00 zone |
| U12 Page | Part, button, page, controls and states of section 8. | `clients/os/src/apps/deployables/{parts.ts,page/DeployablePage.tsx,page/DeployableWorkspace.tsx}`, `apps/deployables/storeStatistics/{read.ts,useStoreStatistics.ts,StoreStatisticsSection.tsx,DestinationControl.tsx,ProductsListPage.tsx}` | | U10, U11 | section 11.2 |
| U13 Docs | Section 12. | | | U8, U12 | the docs gates |

## 10. Failure cases and limits

| Situation | What happens |
| --- | --- |
| A store with many thousands of orders | The read is grouped SQL over two indexed tables; the scale test holds 200,000 orders and 600,000 lines under 500 ms for 90 days. The first fill reads at most 40 pages of 250 of each source per minute per store, about 10,000 orders a minute, with `walked_through` and D16 keeping figures absent until it catches up. |
| An order with more lines than the mirror fetches | Today lines are fetched `first: 100`; the mirror record's U8 fixes that. Until then, and for any order where the recorded quantities fall short of `subtotalLineItemsQuantity`, `lines_complete` is false, every line-derived figure for the period is a floor ("or more"), and the products list says how many orders are short. |
| Several currencies over time | D6. A store that changed currency shows its old orders' count and the line naming them. |
| A store whose time zone changes | D5: the next read cuts every day in the new zone. Comparisons across the change are in the new zone too. |
| An order recorded late | The walk reads by the mirror's write time, so a late order is folded within two minutes of being written, onto its own past day. A past day's figures can therefore grow; "Last order change recorded" says when. |
| A write the walk missed | The five-minute overlap catches a row committed behind the watermark; the hourly check catches anything else within a day, by version, and repairs it. |
| A refund of an amount with no line | Not in Refunds (5.3, L4). The fact keeps `shopify_total_refunded`, so the check can count how often it happens. |
| Several engine nodes | The rollup and the check run on the cron leader, one run at a time per process, under a table lock across replicas, as `component/sitetraffic/rollup.go` does. Every write is guarded by version, so two runs give one answer. Reads are pure reads of shared tables and any node answers. The walk's reads are fresh reads, so a node's result cache never hands it an old page (F19). |
| The verdict is not yet available | Until the mirror record's `shopifyStoreVerdict` is on the cluster, `verdict_state` is empty and the page says "Whether this store is recording is not known." above the figures. |
| The wholesale pack is turned off | `wholesale` is null and the block is absent; the stored flags are left and recomputed when it is turned on again. |
| A customer's data is redacted | The order's email is overwritten, so at the next wholesale pass the order is retail. The fact held no personal data to redact. |
| The store's data is removed | D17: the store's facts and its `store_sales_store` row are deleted. |

## 11. Testing

### 11.1 Server

- **U1, U2**: as in section 9.
- **U6 (database)**: an order with three lines writes four facts with 5.1's numbers; a
  refund of one unit lowers Refunds and Net on the order's day only; an edit that adds a
  line raises Gross; a cancellation removes the order from every figure; a tombstone deletes
  its facts; an older version arriving after a newer one changes nothing; a row committed
  behind the watermark is read by the next run; two concurrent runs write the same facts;
  a development store counts its test orders and a live store does not; a granted buyer's
  orders become wholesale within one run of the grant, and retail after a revoke; a lower
  `definition` rebuilds; the walk reads no field outside the two shapes.
- **U7 (database)**: a fact deleted by hand is restored by the check; a fact whose version
  was set back is recomputed; a fact with no mirrored order is deleted; a rebuild ends with
  the same facts as before it and a clean check.
- **U8 (database)**: each refusal of 6.3; a holder of the part in another organization is
  refused; a site bound only through `previewBinding` is answered as Testing; store days in
  a zone west and east of UTC; a spring-forward day; previous period null when it starts
  before `historySince`; the average null with no orders; other currencies listed and not
  summed; floors with an incomplete order; buyers new, returning and unknown; D16 for each
  verdict state; a caller who is not a cluster owner gets figures.
- **U9**: the independent totals check.
- **Live, against a development store** (skipped without credentials): an order placed
  through the storefront appears in the facts within two minutes of the mirror recording it.

### 11.2 Page

`clients/os/test/deployables/storeStatistics.test.tsx`: the button's presence for each
gate of S1 and D15; the destination control only with two different stores; each state of
section 8; seven figures with money in the store's currency; chart gaps before
`historySince` and after `asOf`; the development and tax lines; no dividers between rows;
both themes and both widths.

### 11.3 The production walk

On the owner's instance, on a development store bound to a storefront deployable, after
the mirror record's walk passes. Each step is done in the OS and in the store's Shopify
admin (Analytics, Reports: "Sales over time" and "Sales by product").

| # | Step | Passes when |
| --- | --- | --- |
| P1 | In Settings, grant the client's group `execute app:deployables/sales`. Sign in as a member of that group. | The deployable shows "Store statistics" beside Statistics. A member of a group without the part sees no such button. |
| P2 | Open Store statistics. | The context line names the store, its currency and time zone, "Orders since", a recent last change, and the development-store line. No notice. |
| P3 | Choose "Last 30 days". Compare Orders, Gross sales, Discounts, Net sales with the admin's report for the same 30 days in the store's time zone. | Equal, or each difference is one named in 5.3 and explained by a live check below. |
| P4 | Place an order through the storefront with a discount code, using the store's test gateway. | Within two minutes of the mirror recording it, Orders rises by one and Discounts by the code's amount. |
| P5 | In the admin, refund one unit of that order without restocking. | Refunds rises by that unit's discounted price on the order's day (L3). |
| P6 | Edit another order to add a line. | Gross sales rises by the line's price (L1). |
| P7 | Cancel a third order. | It leaves every figure. |
| P8 | Refund a fixed amount on an order with no line chosen. | Refunds does not change (L4); the difference from the admin is that amount. |
| P9 | Switch the destination to the other store, then back. | The figures are the other store's, then the first again. |
| P10 | Pause recording for the store on its Store page; reopen Store statistics; resume. | While paused, the notice gives the mirror's sentence and "as of"; after the resume it goes. |
| P11 | Choose "Last 24 hours". | Hours are labelled in the store's time zone; P4's order is in its hour. |
| P12 | Compare the Products list by net sales and by units with "Sales by product". | The same leaders and amounts. |
| P13 | With the wholesale pack enabled and set up for the store, approve a test buyer, then place an order as that buyer. | The Wholesale and retail block appears and the order is Wholesale. |
| P14 | As a cluster owner, run `storeStatisticsRebuild` for the store. | The page says the orders are being brought in, then shows the same figures as before, and the check that follows is clean. |
| P15 | Note what L1 to L10 found. | Each check of 5.3 is answered and this record is corrected where it was wrong. |

**Live checks** settled in the walk: L1 `originalTotalSet` after an edit; L2 the sum of
discount allocations against `totalDiscountsSet`; L3 `currentQuantity` after a refund with
and without restock and after an edit removal; L4 an amount refund; L5 the date the report
uses; L6 tax-inclusive Gross (on a store set to include tax in prices); L7 that every
order on a development store has `test = true`; L8 that the sum of line quantities equals
`subtotalLineItemsQuantity` on a complete order; L9 Shopify's average order value; L10
whether Shopify writes an order's `email` in the case the buyer typed.

## 12. Documentation

- `docs/public/operate/deployables.md`: the Store statistics page, the sales part and how
  to grant it, the destination switch, what "as of" means.
- `docs/public/operate/shopify-connector.md`: what is computed from the mirror, the figure
  definitions of section 5, the known differences from Shopify's reports, the check, how to
  run a rebuild.
- `clients/os/DESIGN.md`: section 8's kit additions.
- This record, corrected by the live checks.

## 13. Rollout

1. The mirror record's release, with the verdict and the lines fix.
2. U1 to U10 in one release. The walk fills every connected store by itself; nothing is
   shown yet, because no group holds the part and the button is not built.
3. The check runs clean on every store for a day (`store_sales_check.clean_at`).
4. U11 and U12 in the next release.
5. The production walk (11.3) on a development store.
6. The part granted to a client's group.

## 14. Unsettled

| # | What | How it is settled |
| --- | --- | --- |
| O1 | Shopify's meaning of the fields in 5.3 | L1 to L10 in the walk |
| O2 | Whether a disabled pack's queries are still loaded; the component asks the pack state first either way | a test in U5 with the pack disabled |
| O3 | Whether a top-level builtin's answer is ever served from the result cache (`useSiteStatistics.ts:15-17` claims it is not; this record did not find the code) | a U8 test that reads twice around a fact write on one node and sees the change |
| O4 | The exact name of the pack-state read the component calls | settled in the plan, from `component/memql/module_registry.go` |
| O5 | That `row.createdAt >= since` with `sort "row.createdAt"` pages correctly over mirrored rows as it does over campaign rows | the U6 overlap test |

## 15. Proposed additions

Not part of the decided design; each needs the owner's yes.

- **Refunds on their own day, with a line.** Mirror Shopify's refund line items (the refund's
  `refundLineItems` with their subtotals), and date each refund on its own day. It removes the
  differences of L4 and L5 and makes the page agree with Shopify's Returns line. It changes
  the mirror's generator, so it belongs with the mirror.
- **Total sales.** Net sales plus shipping, tax and duties, Shopify's "Total sales", from the
  order's current totals. Easy from the facts; left out because S2 does not name it.
- **A Rebuild act for a cluster owner** on the Store page, instead of the builtin alone.
- **Fill on write.** An event trigger on a mirrored order's write could fold it within
  seconds instead of a minute. The minute walk is kept as the net either way.

## 16. Not in this version

- A custom date range (S3).
- Conversion between currencies, and presentment-currency figures.
- Orders older than the mirror's history (mirror record D11).
- Shipping, tax, duties, tips and fees as figures.
- Sales by channel, location, discount code, customer or region.
- Exports, emails, alerts or any message about sales.
- Telling apart the orders of two destinations bound to the same store.
- Any model or agent.
