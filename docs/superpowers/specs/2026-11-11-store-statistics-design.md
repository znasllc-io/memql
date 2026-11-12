---
title: A storefront deployable's own sales figures, computed from the orders the engine mirrors
audience: internal
status: draft
area: operate
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
builds on it (section 9).

No release number is given here: this ships in the first release after the mirror
record's, whatever its number turns out to be.

Terms used throughout:

- **Mirrored order**: a `v1:shopify:order` row, with its `v1:shopify:orderLineItem` and
  `v1:shopify:refund` rows.
- **Refund record**: a mirrored `v1:shopify:refund` row, which is Shopify's own record of
  one refund: when it was processed, how much money went back, and for which lines.
- **Fact**: one row in a table this design adds, holding what one order, order line,
  refund or refund line contributes to the figures. The figures are sums over facts.
- **Counted order**: an order whose facts enter the figures (D3). A **counted refund** is
  a refund of a counted order.
- **Destination**: one of a deployable's two placements, Production and Testing, each bound
  to its own store (`site.binding.storeId` and `site.previewBinding.storeId`).
- **Store day**: a calendar day in the store's own time zone.

**Approvals.** S1 to S11 below are the owner's, given in the brief. Total sales, refunds
from Shopify's refund records on their own day, and the single release are the owner's
rulings on the first draft (2026-11-11). Every other decision is marked *Made here* and
waits for the owner's review of this record.

**Amended with the plan (2026-11-11).** The plan
(`docs/superpowers/plans/2026-11-11-store-statistics.md`) was written against the store
mirror's branch as it then stood and against the code. Where either said otherwise than
this record, the record was corrected: F8, F12, F13, F22, D20, D22, sections 3, 6.1 to
6.5, 8, 9, 10 and 14. The plan lists each amendment under "Amendments to the record".
No decision of the owner's (S1 to S14) changed.

**Amended after the plan's review (2026-11-11).** The plan was reviewed against this
record, its method and the mirror's branch as it stood when its two halves were joined, and
the rulings on that review changed the design in these places: D17, D21, sections 3, 5.1 to
5.3, 5.5, 6.1 to 6.3, 6.5, 8, 9, 10, 11.1, 11.3 and 13. The plan lists each as A14 to A25.
No decision of the owner's changed.

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
| F8 | A line carries `quantity`, `currentQuantity`, `isGiftCard`, `originalUnitPriceSet`, `originalTotalSet`, `discountAllocations` (each with `allocatedAmountSet`), `productGid`, `title`. Lines are fetched `first: 100` per order, and on the mirror's branch the pages after the first are read too, up to fifty pages (`integrations/shopify/children.go`, `completeChildren`). | `dsl/shopify/generated/orderLineItem.memql:43-122`; `integrations/shopify/generated/model.go:4260-4300` |
| F9 | An order carries `test`, `cancelledAt`, `processedAt`, `sourceCreatedAt`, `currencyCode`, `customerGid`, `email`, `taxesIncluded`, `subtotalLineItemsQuantity`, `currentSubtotalPriceSet`, `totalDiscountsSet`, `totalShippingPriceSet`, `totalTaxSet`, `originalTotalDutiesSet`, `totalRefundedSet`, `updatedAt` (Shopify's version) and `syncedAt` (the mirror's write time). | `dsl/shopify/generated/order.memql:31-34`, `:60`, `:82`, `:93`, `:118`, `:186`, `:204`, `:237`, `:247`, `:260`, `:262`, `:270`, `:282`, `:288`, `:291` |
| F10 | Refunds are already mirrored as a child of the order (`refunds`, a plain list, so it is part of the order's own fetch and of its bulk line). The row holds `processedAt`, `sourceCreatedAt`, `totalRefundedSet`, `duties` and `orderGid`, and nothing about which lines were refunded. No refund topic is subscribed: a refund moves the order's `updatedAt`, so `ORDERS_UPDATED` carries it. | `cmd/shopifyschema/allowlist.yaml:13-19`, `:355-401`; `dsl/shopify/generated/refund.memql:30-62`; `integrations/shopify/generated/model.go:4335-4346` |
| F11 | In the pinned Admin schema, `Refund` has three connections the mirror drops: `refundLineItems` (`lineItem`, `quantity`, `subtotalSet`, `totalTaxSet`, `restockType`, `restocked`), `refundShippingLines` (`subtotalAmountSet`, `taxAmountSet`) and `orderAdjustments` (`amountSet`, `taxAmountSet`, `reason`, one of `RESTOCK`, `DAMAGE`, `CUSTOMER`, `REFUND_DISCREPANCY`, `FULL_RETURN_BALANCING_ADJUSTMENT`, `PENDING_REFUND_DISCREPANCY`). `Refund.updatedAt` exists. | `cmd/shopifyschema/testdata/schema-2026-07.json`, types `Refund`, `RefundLineItem`, `RefundShippingLine`, `OrderAdjustment`, `OrderAdjustmentDiscrepancyReason` |
| F12 | The generator keeps a connection that is not a declared child as GIDs only when its node type has an id, and inline as objects only when it has none. All three refund connections have ids, so listing them as references would keep ids and no amounts. A child's own children are not selected in its parent's document (`selectionLines(child, ..., false, ...)`), so making them child concepts of `Refund` would fetch them only through a refund's own fetch, which nothing triggers. Reference connections are left out of bulk documents. The mapper writes a list it finds nothing for as an empty list, not as an absent field, so a bulk line leaves a reference list empty (`integrations/shopify/mapper.go`, `mapObjectUnder`). Only a store's first sweep, which has no watermark, lists every row again; a later sweep reads what changed since its watermark (`component/datasync/runners.go`, `reconcileOne`; `integrations/shopify/reconcile.go`, `reconcileByUpdatedAt`). | `cmd/shopifyschema/allowlist.go:83-95`; `cmd/shopifyschema/mapping.go:94-102`, `:430-475`; `cmd/shopifyschema/emit_graphql.go`, `selectionLines` and `EmitBulkDocuments` |
| F13 | The mirrored shop row holds `currencyCode`, `ianaTimezone` and `plan`; the mirror selects `plan { partnerDevelopment ... }`. The store row's `isDevelopment` is set at connect from the same answer. The shop row is a singleton with no listing and no backfill: it is written only when a `SHOP_UPDATE` delivery arrives, so a store whose settings never changed has none (the one fetch of the singleton is a delivery's, `integrations/shopify/apply.go`, `fetchAndMap`). U1 makes the mirror read it at connect, at boot and daily. | `dsl/shopify/generated/shop.memql:51`, `:72`, `:94`; `integrations/shopify/generated/model.go:6641`; `integrations/shopify/connect_write.go:46`, `:131-139`, `:253-275`; `integrations/shopify/store.go:43-49` |
| F14 | Statistics periods are a fixed list; every bound is a UTC instant. No time zone variant exists. | `core/statsperiod/statsperiod.go:11-15`, `:117-140` |
| F15 | The OS kit has the figure row, one chart, ranked lists, a period control and floors ("5,000 or more"), but no money formatter, and bucket labels are UTC only. The figure grid is four columns unless there are exactly five figures. | `clients/os/src/kit/statistics/StatFigures.tsx`; `kit/statistics/period.ts:62-77`; `kit/measure.ts:74-113`; `kit/format.ts`; `clients/os/src/styles/index.css:14618-14621` |
| F16 | Reviews is the exact gate wanted: only a `shopify_storefront`, not system-owned, with a bound store, for a holder of the part; it takes the pane. | `clients/os/src/apps/deployables/page/DeployablePage.tsx:449-460`, `:552` |
| F17 | A per-organization part is declared in six places: role defaults, seeds, the construct, the organization gate, the parity test and the OS part list. | `component/auth/rbac_model.go:254-307`; `dsl/rbac/seeds.memql:745-758`; `packs/reviewspack/dsl/builtins.memql:74-85`; `component/memql/organization_action_capabilities.go:13-85`; `component/memql/app_resource_os_parity_test.go:148`; `clients/os/src/apps/deployables/parts.ts:72-114` |
| F18 | Binding a site to a store requires that the caller can read the store, so a site's binding is an authorized link between the two. The reviews pack checks a named site serves a named store through `binding` or `previewBinding`. | `component/memql/platform_site_binding_guard.go:12-60`; `packs/reviewspack/request.go:433-441`; `packs/reviewspack/settings.go:329-349` |
| F19 | The wholesale pack keeps an entitlement per approved application, keyed by store and buyer email, in state `pending`, `granted`, `failed` or `revoked`. It ships disabled; a `v1:platform:packState` row enables it. | `packs/wholesalepack/dsl/concepts.memql:100-108`, `:115-120`; `packs/wholesalepack/pack.go` (`DefaultEnabled = false`) |
| F20 | The query path has a per-node result cache; a fresh read neither consults nor fills it. | `component/memql/engine.go:1189-1242`; `component/memql/fresh_read.go:36-45` |
| F21 | The deployables settings document remembers one period for every deployable, against the website periods. | `clients/os/src/apps/deployables/settings.ts:30-40`, `:69-77` |
| F22 | On the mirror's branch the verdict builtin exists (`shopifyStoreVerdict`, for the store's owner, a cluster owner or a caller who may read stores) and an order's lines past the first hundred are read through a generated page operation (`ChildSpec.PageOp`). Both land with the mirror, before this work. Every mirror row there carries `versionSource`, and the rule that orders two writes is `component/memql/sync/version.go`. | `integrations/shopify/verdict_capabilities.go`; `integrations/shopify/children.go`; `component/memql/sync/version.go` |

## 3. Shape of the solution

**The mirror records refunds in full.** Each mirrored refund gains, inline, its refund
lines, its refunded shipping and its order adjustments, so the engine holds Shopify's own
account of every refund (U1). The mirror also reads each store's shop row without waiting
for Shopify to announce a change to it, because the store's currency and time zone are
there and nowhere else (F13).

**One component, `component/storestats`**, in the manner of `component/emailstats`. It is a
component because it reads this cluster's own rows and writes its own tables, and calls
no outside service.

**Fill.** Every minute a walk reads, through the engine, the orders, lines and refunds of
each connected store that changed since its watermark, and writes facts for each. Sales
are dated on the order's day; refunds on the refund's own day. A changed row replaces its
facts, and a row the mirror retired keeps its fact, marked deleted, so that no older version
of it can be written back.

**Repair.** Every hour a check recomputes, within a page budget, the fact of every mirrored
order, line and refund of a store from the row in hand, compares it with the stored one by
version and by content, and writes again whatever differs. A rebuild deletes a store's
facts and lets the walk write them again, then the check proves them.

**Read.** One client-callable builtin, `storeStatistics`, gated by a new part judged per
organization, checks that the named site serves the named store and answers the figures
with plain SQL over the fact tables. It reads no order row, so it does not need the caller
to be a cluster owner.

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
| S5 | *Owner.* The numbers come from a fact table the engine fills as orders are recorded and changed, in the manner of the email statistics. A refund, edit or cancellation changes the facts. The design states what fills, repairs and rebuilds them. | The page must not walk thousands of rows on every open, nor depend on the reader being the cluster owner (F1, F3). |
| S6 | *Owner.* Access is a new organization part for sales figures, separate from website Statistics, declared everywhere `reviews` is and granted to a group the same way. The server checks the part and that the site and store named belong together. | Revenue is more sensitive than visits. |
| S7 | *Owner.* The page shows the store of the destination being looked at and lets the reader switch. | Production and Testing are different stores. |
| S8 | *Owner.* Cancelled orders are not sales. On a live store, Shopify's test orders are left out. On a development store every order is a test order, so they are counted, and the page says so in one line. | A development store would otherwise read zero forever. |
| S9 | *Owner.* The page states when the last order change was recorded, and when the mirror is not recording it shows that, with the mirror's reason, rather than stale numbers presented as current. | R3. |
| S10 | *Owner.* No model or agent. Nothing is sent to anyone. | R4. |
| S11 | *Owner.* A value that cannot be known is shown as absent, never as zero. | The house rule (`clients/os/DESIGN.md`, "Not measured is a dash"). |
| S12 | *Owner.* **Total sales** is a figure: net sales plus shipping, tax and duties, Shopify's own "Total sales". | The figure a merchant reconciles against money received. |
| S13 | *Owner.* **Refunds come from Shopify's refund records, on the refund's own day.** The mirror gains what that needs. | A page whose Refunds and Net sales disagree with the store's own admin will not be trusted. |
| S14 | *Owner.* **One release.** The part is granted to a client's group only after the check has run clean once on the store and the production walk has passed. | |

### 4.2 Decisions made here

| # | Decision | Why |
| --- | --- | --- |
| D1 | *Made here.* **Sales are computed from the order's lines; refunds from its refund records.** Gross sales, discounts and units come from lines on the order's day; refunds from `v1:shopify:refund` rows on the refund's day. Order-level totals are kept on the fact only to cross-check (D14). Section 5 gives every formula. | Only lines separate gift cards (Shopify's sales reports leave them out) and give units and products; only refund records say when money went back and how much of it was shipping, tax or duties (S13). |
| D2 | *Made here.* **An order's sale is dated by its `processedAt`, falling back to `sourceCreatedAt`; a refund by its own `processedAt`, falling back to its `sourceCreatedAt`.** An edit stays on the order's day. | S13. An edit has no record of its own with a time in the mirror, so it can only change the order's day; 5.4 says how it shows. |
| D3 | *Made here.* **A counted order** is one whose mirrored row is not deleted, whose `cancelledAt` is empty, and which is not a test order, unless the store is a development store, where test orders count. A refund counts when its order counts and its row is not deleted. | S8. A cancellation removes the order and its refunds from every period, which is what "not a sale" means. |
| D4 | *Made here.* **A store is a development store when its mirrored shop row says `plan.partnerDevelopment = true`; when no shop row is mirrored yet, when the store row's `isDevelopment` says so.** | The shop row is Shopify's current answer, refreshed by the mirror; the store row's flag is the same answer taken at connect (F13). A store an operator marked `isDevelopment` that Shopify bills as a live store keeps excluding test orders, because its orders are not all test orders. |
| D5 | *Made here.* **A fact carries an instant (`ordered_at`, `refunded_at`), never a precomputed day.** The read cuts store days with the time zone it holds now. | A time zone change then re-buckets history in the new zone with no rebuild, which is also how a merchant reads their own admin after changing it. |
| D6 | *Made here.* **Money is summed only for orders, and refunds of orders, in the store's current currency.** Orders in another currency are counted in Orders and Units, and the page says in one line how many there are and in which currency. | S4 forbids adding two currencies. No conversion rate is known to the engine. |
| D7 | *Made here.* **Shop money only.** `presentmentMoney` (the buyer's currency) is never read. | It is summable only per buyer currency, and Shopify reports in shop money. |
| D8 | *Made here.* **The walk runs every minute** (`15 * * * * *`), each store at most 40 pages of each source per run. | "As orders are recorded" (S5). A mirrored change is on the page within about two minutes of reaching the mirror. |
| D9 | *Made here.* **Wholesale is judged now, not at order time.** An order is wholesale when its `email`, case-folded and trimmed, equals the `buyerEmail` of an entitlement of the same store in state `granted`. A change to a store's granted set recomputes the flag on all its orders. A refund takes its order's flag. | The pack keeps the current state, not a history the engine can join on (F19). A revoked buyer's past orders move to retail; the help says so. The email is read in memory and never stored in a fact. |
| D10 | *Made here.* **"Wholesale enabled for the store"** means the wholesale pack is enabled on the cluster and a `wholesaleSettings` row exists for that store. | A pack that is on but never set up for a store has no wholesale buyers to tell apart. |
| D11 | *Made here.* **A buyer is the order's `customerGid`.** In a period, a buyer is new when their first counted order in the store's facts falls inside the period, returning when they have a counted order before it. Orders with no customer are counted apart as "No customer recorded". | No personal data is needed. The history begins where the mirror's does (60 days before connect, mirror record D11), so "new" means new since then, and the page says since when. |
| D12 | *Made here.* **Eight figures in one row**, in this order: Total sales, Net sales, Orders, Average order value; then Gross sales, Discounts, Refunds, Units sold. All but the average are chartable. The kit's grid draws eight as two rows of four, and as four rows of two at the narrowest width; no CSS changes. | The first four are what a store owner reads first: money taken, sales after returns, how many orders, and their size. The second row explains how Net sales is made. One figure row, as the statistics rule asks (F15). |
| D13 | *Made here.* **The period is the same remembered choice as website Statistics** (`statisticsPeriod`). The destination is not remembered; the page opens on Production when it has a store, else Testing. | Same list (S3); a person comparing traffic and sales asks about the same period. A destination choice that survived would open a Testing store's sales when someone came for Production's. |
| D14 | *Made here.* **The facts keep Shopify's order-level totals beside the line sums**, and the check counts orders where they disagree. Nothing on the page draws them. | A permanent test of the formulas against Shopify's own arithmetic on live data, and the place a wrong reading of a field shows first. |
| D15 | *Made here.* **The button is shown when either destination has a store**; Reviews shows only for Production's. | S7: a storefront exercised only against a development store still has sales to read. |
| D16 | *Made here.* **When the store's verdict is not `recording`**, the figures are drawn as of the time the verdict stopped being `recording` (its `since`), with the verdict's sentence in a `Notice`; buckets after that instant are gaps. During `starting` and during a rebuild every figure is absent. | S9 and S11: what was recorded before the break is true as of then; what came after is not known. While history is still arriving no total is known. |
| D17 | *Made here.* **Facts hold no name, email, address or phone.** What a fact holds of a buyer is their Shopify customer id (`buyer_key`); a line fact also holds the line's title. After a customer's erasure the mirror keeps the order's customer reference (an erasure keeps references and scrubs the person) and the customer's own row is an empty shell, so the id names nobody; a custom line item, one with no product, has its title scrubbed, since a person typed it. The erasure rewrites rows in place, where the walk does not look, so the next check pass recomputes the facts of the rows it scrubbed (U8): a scrubbed title leaves the facts, and an order whose email was scrubbed is retail. When the store's data is removed (`redactedAt` set), the store's facts are deleted. | The walk reads every store's orders; the figures need a buyer only as an opaque id, and a product only by its name. |
| D18 | *Made here.* **Facts are kept as long as their order is mirrored.** No 400-day retention. | New against returning needs the whole history, and one row per order is small. |
| D19 | *Made here.* **A fact definition version** (`storeStatsDefinition`, an integer constant) is stored per store. A release that changes a formula raises it, and the walk rebuilds each store whose stored version is lower. | A changed definition must never mix with facts computed the old way. It is also why no Rebuild act is needed on the Store page. |
| D20 | *Made here.* **The part is `app:deployables/sales`**, on owner and developer by default, like `reviews`. The OS words a part by the last segment of its resource ("sales"), as it does for every part; "sales figures" is what the seeds' descriptions call it. | S6. A short resource name in the existing family. No table of part labels exists to hold another wording. |
| D21 | *Made here.* **The refund's detail is mirrored inline on the refund row**, as three object-list fields, through a new generator option that names the fields it keeps, not as three child concepts. U1 states the choice and its cost. | F12: child concepts of a child are never fetched with the order, and a reference keeps ids without amounts. Inline fields ride in the order's own fetch, add no rows, no topics and no new read path. |
| D22 | *Made here.* **A refund whose detail has not been fetched yet** (its three inline fields are absent: the row was written by a bulk line, or before the detail was mirrored, 6.1) makes Refunds, Net sales, Total sales and Units sold absent for the periods it falls in, with the reason "N refunds are still being read from Shopify." | S11. Its money total is known, but not how much of it was shipping or tax, so no figure that subtracts it can be stated. |

## 5. The figures

### 5.1 The sale, per line, on the order's day

For a line L of a counted order, gift-card lines excluded (`isGiftCard = true`):

| Name | Formula | Source |
| --- | --- | --- |
| q0 | `quantity` | units on the line when ordered or added by an edit |
| q1 | `currentQuantity` | units still on the order after refunds and edits |
| qr | sum of `quantity` over the refund lines of this order naming L (`refundLineItems[].lineItem.id`) | units refunded, from the refund records |
| qs | `min(q0, q1 + qr)` | units sold: what was not removed by an edit |
| P | `originalUnitPriceSet.shopMoney.amount` | the unit price before discounts |
| G(L) | `P * qs` | the line's gross sales |
| A | sum of `discountAllocations[].allocatedAmountSet.shopMoney.amount` | every discount allocated to the line, for q0 units |
| D(L) | `A * qs / q0`, rounded half to even at the larger of A's own scale and the currency's minor units; `A` itself when `qs = q0`; 0 when q0 is 0 | the discounts on the units sold |

The scale of an amount is the number of decimals Shopify wrote in it, and that is not the
currency's: Shopify writes a whole amount with one decimal ("35.0"), so a share rounded at
the written scale alone would come out in tenths. A computed amount is therefore rounded
half to even at the larger of its operand's own scale and the currency's minor units, read
from a table compiled into `core/money` from the minor units of ISO 4217, for the codes of
the Admin API's `CurrencyCode` enum: 0 for `BIF`, `CLP`, `DJF`, `GNF`, `ISK`, `JPY`, `KMF`,
`KRW`, `PYG`, `RWF`, `UGX`, `VND`, `VUV`, `XAF`, `XOF` and `XPF`; 3 for `BHD`, `IQD`, `JOD`,
`KWD`, `LYD`, `OMR` and `TND`; 2 for every other code. A discount written "1.0" over two of
three units is "0.67". D(L) and the average order value are the only divisions in the
design, and D(L) divides only when an edit removed units. L16 records the scale Shopify
writes.

For the order O, on its day, from the order row:

| Name | Field | Live check |
| --- | --- | --- |
| S(O) | `totalShippingPriceSet.shopMoney.amount` | L13: shipping charged after shipping discounts, before refunds |
| T(O) | `totalTaxSet.shopMoney.amount`, or 0 when `taxesIncluded` is true | L13: tax charged before refunds. When tax is included in prices it is already inside G, so it is not added twice |
| U(O) | `originalTotalDutiesSet.shopMoney.amount`, 0 when absent | L13 |

### 5.2 The refund, on the refund's day

For a counted refund F, from its refund record (U1 adds the three inline lists):

| Name | Formula |
| --- | --- |
| M(F) | `totalRefundedSet.shopMoney.amount`: all money that went back |
| RS(F) | sum of `refundShippingLines[].subtotalAmountSet` |
| RT(F) | sum of `refundLineItems[].totalTaxSet` + `refundShippingLines[].taxAmountSet` + `orderAdjustments[].taxAmountSet`; 0 when the order's `taxesIncluded` is true |
| RU(F) | sum of `duties[].amountSet` |
| RG(F) | sum of `refundLineItems[].subtotalSet` over refund lines whose line is a gift card |
| R(F) | `M(F) - RS(F) - RT(F) - RU(F) - RG(F)`: the refund of items, Shopify's "Returns" |
| RQ(F) | sum of `refundLineItems[].quantity` over refund lines whose line is not a gift card |
| RP(F, product) | sum of `refundLineItems[].subtotalSet` over its non-gift refund lines of that product |

R(F) starts from the money that went back, so a refund of an amount with no line on it is
counted in full: Shopify records the difference between the lines and the money as an
order adjustment, whose tax part `taxAmountSet` is taken out like any other tax. A
restocking or damage fee (an adjustment with reason `RESTOCK` or `DAMAGE`) lowers M(F) and
so lowers R(F): the page counts a smaller return where Shopify shows the full return and a
return fee; both give the same Total sales (L12).

These formulas assume two things of Shopify, and each is a live check (11.3) before the
part is granted. On a store whose prices include tax, a refunded shipping line's
`subtotalAmountSet` holds its tax, so taking RS out takes that tax with it and RT is 0
(L13). An order adjustment's `amountSet` and `taxAmountSet` carry the sign Shopify wrote
and can be negative; `taxAmountSet` is summed into RT with that sign, a positive tax being
tax that went back to the buyer (L4, L12). If either is found otherwise the formula changes
as 5.5 says, and the plan names the tests and the fixture orders that change with it.

### 5.3 Per period

Over the counted orders whose `ordered_at` falls in the period (the period's orders) and
the counted refunds whose `refunded_at` falls in it (the period's refunds), in the store's
currency (D6). Each figure is defined here once.

| Figure | Definition |
| --- | --- |
| Orders | count of the period's orders |
| Gross sales | sum of G(L) over the period's orders |
| Discounts | sum of D(L) over the period's orders, shown as a positive amount |
| Refunds | sum of R(F) over the period's refunds, shown as a positive amount |
| Net sales | Gross sales minus Discounts minus Refunds |
| Total sales | Net sales plus the sum of S(O) + T(O) + U(O) over the period's orders, minus the sum of RS(F) + RT(F) + RU(F) over the period's refunds. Equivalently, the sales of the period's orders with shipping, tax and duties, less `M(F) - RG(F)` of the period's refunds. Tips and additional fees are left out. |
| Average order value | (Gross sales minus Discounts) / the period's orders in the store's currency, which are the orders those two sums were taken over (Orders counts every currency, D6). Rounded half to even at the currency's minor units and answered so, so the page rounds nothing a second time. Absent when the period has no such order. Refunds do not enter it. |
| Units sold | sum of qs over the period's orders, minus sum of RQ(F) over the period's refunds. It can be negative in a period with more returns than sales, as in Shopify's "net items sold". |
| Top products by net sales | per product, sum of `G(L) - D(L)` over the period's orders minus sum of RP(F, product) over the period's refunds. A line with no product is grouped by its title. |
| Top products by units | per product, sum of qs minus refunded units of that product, by the same key |
| New and returning buyers | distinct `customerGid` of the period's orders, by D11 |
| Wholesale and retail | Orders and Net sales split by D9; a refund on the side of its order |
| Sales by day | each figure but the average per bucket: hour for 24 hours, store day for 7 to 90 days, store month for 12 months |

A product's label is the title of its newest line in the period. The part of Refunds that
names no line (`R(F)` minus its RP over every product) is in Net sales and not in any
product; the products list says so in its footer when it is not zero.

### 5.4 Edits and cancellations

- **A line removed by an edit without a refund** lowers `currentQuantity` and has no refund
  line, so `qs` drops: those units leave Gross sales, Discounts, Units sold and the
  product's figures on the order's own day. Nothing shows on the edit's day, and no
  Refunds figure moves.
- **A line added by an edit** is a new line with its own `quantity`, counted on the order's
  day.
- **A cancelled order** leaves every figure, its refunds with it, in every period (D3). A
  cancellation in a later period therefore lowers the figures of the earlier one.

### 5.5 Against Shopify's own reports

This repository establishes the fields' shape, not Shopify's meaning: the generated
comments say only "Admin GraphQL <field>". Each point the comparison depends on is a live
check, settled in the production walk (11.3) before the part is granted.

| Point | This page | Shopify's sales report, as understood | Live check |
| --- | --- | --- | --- |
| Gross sales | unit price times units not removed by an edit, gift cards out | the same, with an edit's removal on the edit's day | L1: `originalUnitPriceSet * qs` reproduces the admin's gross after an edit that removes a unit |
| Discounts | allocations on item lines, scaled to units sold | item and order discounts | L2: the allocations over all lines equal `totalDiscountsSet` when no shipping discount applies |
| Refunds | R(F) on the refund's day | "Returns" on the day of the return | L3: `currentQuantity` drops by the refunded quantity, with and without restock, and on an edit removal. L4: `totalRefundedSet` is all money refunded, and an amount-only refund appears as an order adjustment. L5: the report dates an order by `processedAt` and a return by the refund's `processedAt` |
| Return fees | lower Refunds | a separate line | L12 |
| Net sales | Gross minus Discounts minus the period's Refunds | the same | L15: in a period holding a refund of an order from an earlier period, Net sales equals the admin's |
| Total sales | Net plus shipping, tax and duties charged, less those refunded | the same, with tips and fees | L13: the meaning of `totalShippingPriceSet`, `totalTaxSet`, `originalTotalDutiesSet`. L14: Total sales equals the admin's for the same period |
| Cancellation | the order and its refunds leave every figure | a sale on its day and a return on the cancel day | none: S8 decides |
| Tax-inclusive prices | tax inside Gross, not added again in Total sales | thought to show Gross without tax | L6: on a store that includes tax in prices, compare Gross sales |
| Average order value | (Gross minus Discounts) / the orders in the store's currency | thought to be the same | L9 |
| Edits | change the order's own day | dated on the edit's day | none: D2 |

When a live check disagrees, the formula changes, `storeStatsDefinition` is raised (D19),
and this table is corrected in the same change.

## 6. Data

### 6.1 The mirror (U1)

`cmd/shopifyschema/allowlist.yaml`, the `Refund` entry gains a new key:

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

`inline` (new, `cmd/shopifyschema/allowlist.go`, `Entry.Inline []Inline`) names connections
carried on the row as `[]object`, `first: page`, each element holding the fields the entry
names and no other. It never takes a node's full selection: the generator's own selection
of a refund's shipping line carries `shippingLine`, whose `phone` is the number at the
buyer's delivery address, and that would be copied into every refund row. Only what cannot
be a person's words may be named: a scalar that is a figure or a moment, an enum, a money
object, or a reference to a mirrored type, kept as its id. The validation refuses anything
else. Only a connection whose node type is not itself mirrored may be inlined, which the
allowlist validation enforces; that is what keeps it from being "two versions of the same
fact" (`mapping.go:437-441`). Like references, an inline connection is left out of bulk
documents (`OmitFromBulk`). It is a field kind of its own (`inlineList`), because the
mapper must treat it differently from every other list: a list the mapper finds nothing for
is written empty (F12), and an inlined connection the document did not select is instead
left out of the write. On a new row the field is then absent, and on an existing row the
engine's read-merge keeps what a fetch wrote, so a bulk line never blanks a refund's detail
and "not read" can be told from "read, and empty". The page a connection is read with is
carried on the generated field, its one owner, and the statistics read it there.

Regenerated, `v1:shopify:refund` gains three fields, added and never renamed:

| Field | Type | Holds, per element |
| --- | --- | --- |
| `refundLineItems` | `[]object` | `lineItem { id }`, `quantity`, `subtotalSet`, `totalTaxSet` |
| `refundShippingLines` | `[]object` | `subtotalAmountSet`, `taxAmountSet` |
| `orderAdjustments` | `[]object` | `amountSet`, `taxAmountSet`, `reason` |

The three fields make the release that carries them roll-forward only: a refund row that
holds them is refused by an older schema on its next write.

**The erasure.** A customer's erasure scrubs every list of objects that is not on its
keep-list (`integrations/shopify/erasure_fields.go`), and a refund scrubbed to three empty
lists would read as "detail read, nothing on it" and count its whole amount as a refund of
items. The three fields hold nothing about a person, so they go on the keep-list with
their reasons, the erasure's golden file is regenerated with them, and the generator sets
each field's Admin type and the references it holds, which the erasure's own tests ask of
every field. A test fails if an inlined list ever holds a field the erasure would scrub.

**What it costs.** No new concept, row, topic, webhook or read path: the three lists ride in
the order's existing fetch, inside its `refunds { ... }` selection, and in
`ShopifyFetchRefund`. Each refund row grows by one object per refunded line. The order
fetch's requested query cost grows with the number of refunds times 70 nodes at most;
Shopify refuses a single query whose requested cost passes its limit, so the measured cost
of `ShopifyFetchOrder` on an order with refunds is a gate of U1 (L11). A refund with more
lines than 50, which the mirror cannot tell from exactly 50, marks its facts incomplete
(6.2). The mirror's paging of an order's lines (`completeChildren`, F22) does not reach
these lists and is not extended to them: it pages only the fetched object's own child
connections that carry a page operation, by that object's id; `refunds` is a plain list,
which Shopify answers whole, and a refund's inlined connection is a field of the child.

**When the detail arrives.** With an order's own fetch (every delivery that names it), and
with a sweep's list page. A bulk line never carries it and never removes it. Only a
store's first sweep lists every order (F12), so a refund mirrored before this version, or
written only by a bulk line, would stay without detail for good. So every order of a store
is read once more: while the store's health holds no `refundDetailFrom`, an order sweep
reads the store's whole order listing, whatever watermark it was handed. The read is
stamped only when it ended. A sweep in which a write failed is not stamped. A read cut at
the sweep's row budget is not stamped either: it keeps its cursor on the store's health
(`refundDetailAfter`), and the next sweep, after its ordinary check, goes on from there, so
a store with more orders than one sweep reads is read to its end over several sweeps
instead of from the start at every one. A paused store is not swept, so nothing is stamped
and the read begins, or goes on, after the resume. What stays without detail is a refund
of an order Shopify no longer shows (section 10, "An order placed more than sixty days
ago"); D22 holds for it.

**The shop row.** The mirror reads each store's shop singleton at connect, at boot when the
store has no shop row, in the daily subscription pass and when a store is resumed, through
the same generated fetch and the same guarded write a `SHOP_UPDATE` delivery uses (F13).

### 6.2 Tables

One migration, `component/database/memory-nodes/migrations/20261112090000_store_sales_facts.up.sql`
with its `.down.sql`. Every id is bare. Every money column is `NUMERIC` with no scale, so a
sum is exact in the database. Columns marked *derived* are recomputed from the other fact
tables for every order a page of the walk touched, in the transaction that wrote the page,
so a run that stops between two pages leaves no derived column stale. A line's sold
quantity, gross and discounts are computed in Go with U3, where the one rounding rule
lives; everything that is a sum is SQL.

**A fact's version** (`order_version`, `line_version`, `refund_version`) is its mirror row's
own write time, the row's `createdAt`: the instant the mirror wrote that version of the
row. It is how the engine itself orders a row's versions, and it moves forward on every
write. The row's `updatedAt` cannot serve: on the mirror's branch it is Shopify's version
only when `versionSource` is `origin`; a tombstone carries the time MemQL read Shopify
there, and so does a row written before the mirror kept the origin's version, so a later
state can hold an earlier `updatedAt`. `checked_at`, on the three walked fact tables, is
the start of the last check pass that found the fact's row (U8).

**A retired row's fact is kept.** When the mirror retires a row (a tombstone, `deleted`
true), its fact is not removed: it is marked `deleted` and takes the tombstone's version.
The writes are guarded by version, and a guard needs a row to compare with. Without one,
the older live version of a deleted order, which a later window of a first fill, another
node and every pass of the check read again, would be written back as if the order were
new, and the check would find something to repair on every pass. Every figure, every join
and the derivation leave `deleted` facts out. A deleted refund's lines are removed with
it, since they have no version of their own.

**Every index names its reader** in the migration, and none is kept that no statement
uses.

`store_sales_order_fact`, one row per mirrored order, primary key `(store_id, order_gid)`,
indexes `(store_id, ordered_at)` and `(store_id, buyer_key, ordered_at)`:

| Column | Type | Meaning |
| --- | --- | --- |
| `store_id`, `order_gid` | `TEXT NOT NULL` | |
| `order_version` | `TIMESTAMPTZ NOT NULL` | the order row's `createdAt` (above) |
| `deleted` | `BOOLEAN NOT NULL DEFAULT false` | the mirror retired the row (above) |
| `recorded_at` | `TIMESTAMPTZ NOT NULL` | the order's `syncedAt` |
| `ordered_at` | `TIMESTAMPTZ NOT NULL` | D2 |
| `currency` | `TEXT NOT NULL` | `currencyCode` |
| `cancelled`, `test`, `taxes_included` | `BOOLEAN NOT NULL` | `cancelledAt` not empty; `test`; `taxesIncluded` |
| `buyer_key` | `TEXT NOT NULL DEFAULT ''` | `customerGid` |
| `wholesale` | `BOOLEAN NOT NULL DEFAULT false` | D9 |
| `shipping`, `tax`, `duties` | `NUMERIC NOT NULL` | S(O), T(O), U(O) |
| `expected_quantity` | `INTEGER NOT NULL` | `subtotalLineItemsQuantity` |
| `shopify_current_subtotal`, `shopify_total_discounts`, `shopify_total_refunded` | `NUMERIC` | for D14 |
| `gross`, `discounts` | `NUMERIC NOT NULL` | *derived*: sums of G(L), D(L) |
| `units` | `INTEGER NOT NULL` | *derived*: sum of qs |
| `line_count` | `INTEGER NOT NULL` | *derived* |
| `lines_version` | `TIMESTAMPTZ` | *derived*: newest line version |
| `lines_complete` | `BOOLEAN NOT NULL` | *derived*: the sum of `quantity` over its lines is at least `expected_quantity` (L8) |
| `checked_at` | `TIMESTAMPTZ` | U8 |

`store_sales_line_fact`, primary key `(store_id, line_gid)`, index `(store_id, order_gid)`.
The key is the store and the line, because a tombstone carries a row's identity and may
carry no parent, and it must still find its fact. The index serves every read that goes
from an order to its lines; no read looks a line up by its product, so no index is kept
by product:

| Column | Type | Meaning |
| --- | --- | --- |
| `store_id`, `order_gid`, `line_gid` | `TEXT NOT NULL` | |
| `line_version` | `TIMESTAMPTZ NOT NULL` | the line row's `createdAt` |
| `deleted` | `BOOLEAN NOT NULL DEFAULT false` | the mirror retired the row |
| `currency` | `TEXT NOT NULL DEFAULT ''` | of its unit price; it picks the scale D(L) is rounded at (5.1) |
| `gift_card` | `BOOLEAN NOT NULL` | |
| `product_key`, `title` | `TEXT NOT NULL` | `productGid` or `title:` and the title; the line's title |
| `quantity`, `current_quantity` | `INTEGER NOT NULL` | q0, q1 |
| `unit_price`, `allocated_discounts` | `NUMERIC NOT NULL` | P, A |
| `refunded_quantity`, `sold_quantity` | `INTEGER NOT NULL` | *derived*: qr, qs |
| `gross`, `discounts` | `NUMERIC NOT NULL` | *derived*: G(L), D(L) |
| `checked_at` | `TIMESTAMPTZ` | U8 |

`store_sales_refund_fact`, one row per mirrored refund, primary key `(store_id, refund_gid)`,
indexes `(store_id, refunded_at)` and `(store_id, order_gid)`:

| Column | Type | Meaning |
| --- | --- | --- |
| `store_id`, `refund_gid`, `order_gid` | `TEXT NOT NULL` | |
| `refund_version` | `TIMESTAMPTZ NOT NULL` | the refund row's `createdAt` |
| `deleted` | `BOOLEAN NOT NULL DEFAULT false` | the mirror retired the row |
| `recorded_at` | `TIMESTAMPTZ NOT NULL` | the refund's `syncedAt` |
| `refunded_at` | `TIMESTAMPTZ NOT NULL` | D2 |
| `money_refunded` | `NUMERIC NOT NULL` | M(F) |
| `shipping_refunded`, `tax_refunded`, `duties_refunded` | `NUMERIC` | RS, RT, RU; null when the detail is not fetched (D22). `tax_refunded` is the sum as Shopify wrote it; a figure reads it as 0 for an order whose prices include tax, which the refund row does not know |
| `detail_fetched` | `BOOLEAN NOT NULL` | the three inline lists were present on the row, empty or not (6.1) |
| `lines_complete` | `BOOLEAN NOT NULL` | `refundLineItems` held fewer than its page of 50 |
| `gift_card_refunded`, `item_refunds` | `NUMERIC` | *derived*: RG, R(F); null when not fetched |
| `units_returned` | `INTEGER` | *derived*: RQ |
| `checked_at` | `TIMESTAMPTZ` | U8 |

`store_sales_refund_line_fact`, primary key `(store_id, refund_gid, position)`, index
`(store_id, order_gid, line_gid)`: `store_id`, `refund_gid`, `order_gid`, `line_gid`
(`lineItem.id`) `TEXT NOT NULL`; `position INTEGER NOT NULL` (its place in the list, since
a refund line's own id may be null); `quantity INTEGER NOT NULL`; `subtotal NUMERIC NOT
NULL`; `tax NUMERIC NOT NULL`. Gift card and product come from the line fact it names.

`store_sales_store`, one row per store the walk has seen:

| Column | Type | Meaning |
| --- | --- | --- |
| `store_id` | `TEXT PRIMARY KEY` | |
| `generation` | `BIGINT NOT NULL DEFAULT 0` | raised by a rebuild and by a removal; a page or check transaction read under an older one writes nothing (section 10) |
| `purging` | `BOOLEAN NOT NULL DEFAULT false` | the store's facts are being deleted in batches; nothing is folded for it meanwhile |
| `store_domain`, `store_name` | `TEXT NOT NULL DEFAULT ''` | from the store row, for the page's context line: a holder of the part cannot read the store row itself |
| `currency`, `timezone` | `TEXT NOT NULL DEFAULT ''` | from the shop row; empty means not known |
| `taxes_included` | `BOOLEAN` | the shop row's `taxesIncluded`; null means not known |
| `development` | `BOOLEAN` | D4; null means not known |
| `all_orders_readable` | `BOOLEAN NOT NULL DEFAULT false` | the store's recorded grant holds `read_all_orders` (section 10) |
| `history_floor` | `TIMESTAMPTZ` | an instant before the store row was first written: where every walk of the store begins (section 10, "A long history") |
| `history_since` | `TIMESTAMPTZ` | the store-local midnight of the first recorded day: the day of "Orders since" in the store's health (mirror record, section 13) cut in the store's zone, else the day of the oldest fact |
| `wholesale_enabled` | `BOOLEAN NOT NULL DEFAULT false` | D10 |
| `wholesale_digest` | `TEXT NOT NULL DEFAULT ''` | SHA-256 of the sorted, case-folded granted emails; never the emails |
| `wholesale_cursor` | `TEXT NOT NULL DEFAULT ''` | where a wholesale pass resumes, an instant; empty when none runs |
| `wholesale_window_seconds` | `INTEGER NOT NULL DEFAULT 86400` | the window width the pass learned, kept from one run to the next |
| `verdict_state` | `TEXT NOT NULL DEFAULT ''` | copied from `shopifyStoreVerdict`; empty means not known |
| `verdict_reasons` | `JSONB NOT NULL DEFAULT '[]'` | `[{code, sentence}]`, the mirror's own words |
| `verdict_since` | `TIMESTAMPTZ` | when the store left `recording`: the verdict's `since` at that moment, kept while the state stays other than `recording`, so a move from `paused` to `needs attention` does not move D16's "as of" |
| `last_change_at` | `TIMESTAMPTZ` | newest `recorded_at` folded into facts |
| `walked_through` | `TIMESTAMPTZ` | how far every walk has read, never past the run's clock |
| `rebuilding_since` | `TIMESTAMPTZ` | set by a rebuild, cleared when the walk catches up |
| `definition` | `INTEGER NOT NULL DEFAULT 0` | D19 |

`store_sales_watermark`: `store_id TEXT`, `source TEXT` (`order`, `line`, `refund`),
`through TIMESTAMPTZ NOT NULL`, `window_seconds INTEGER NOT NULL DEFAULT 86400` (how far
past `through` one read of the past may reach, section 10), `walked_through TIMESTAMPTZ`
(how far this source is walked; the store's is the least of its three); primary key
`(store_id, source)`.

`store_sales_check`: `store_id TEXT PRIMARY KEY`, and two groups of columns, each with a
writer. The pass under way: `pass_started_at TIMESTAMPTZ` (null when none is), and for each
of the three sources a cursor (`order_cursor`, `line_cursor`, `refund_cursor` `TEXT`: empty,
an instant to go on from, or `done`) with the window width the walk learned for it
(`order_window_seconds` and its two fellows), so a run that follows one that halved a
window does not start again at a day; and the running counts `compared`, `mismatched` and
`repaired`. The last pass that ended: `last_started_at`, `last_finished_at`,
`last_compared`, `last_mismatched`, `last_repaired`, `last_disagreeing` (D14). And
`clean_at TIMESTAMPTZ`, when a pass last ended with nothing mismatched.

None of these tables is a concept; no row admission sees them, and only
`component/storestats` reads or writes them.

### 6.3 Constructs

All in `dsl/shopify/overlay/` unless named otherwise. Every query is `@serverOnly`, has
`actor.isClusterOwner == true` in its filter, as the email walk's do
(`dsl/campaigns/queries.memql:1089-1121`), and is asked only by `component/storestats`.

| Kind | Name | What |
| --- | --- | --- |
| shape | `storeStatisticsOrder` | `row.id, row.createdAt, storeId, gid, syncedAt, deleted, processedAt, sourceCreatedAt, cancelledAt, test, taxesIncluded, currencyCode, customerGid, email, subtotalLineItemsQuantity, totalShippingPriceSet, totalTaxSet, originalTotalDutiesSet, currentSubtotalPriceSet, totalDiscountsSet, totalRefundedSet`. `email` is used only by the wholesale judgement (D9). `row.createdAt` is the fact's version (6.2); `updatedAt` is not read. |
| shape | `storeStatisticsLine` | `row.id, row.createdAt, storeId, gid, parentGid, deleted, isGiftCard, productGid, title, quantity, currentQuantity, originalUnitPriceSet, discountAllocations` |
| shape | `storeStatisticsRefund` | `row.id, row.createdAt, storeId, gid, parentGid, syncedAt, deleted, processedAt, sourceCreatedAt, totalRefundedSet, duties, refundLineItems, refundShippingLines, orderAdjustments` |
| shape | `storeStatisticsBuyer`, `storeStatisticsShop`, `storeStatisticsStore` | `row.id, row.createdAt, storeId, gid, deleted, email`; `row.id, row.createdAt, storeId, deleted, name, currencyCode, ianaTimezone, taxesIncluded, plan`; `row.id, domain, name, isDevelopment, scopesGranted, uninstalledAt, redactedAt, health` |
| query | `storeStatisticsOrdersChanged(storeId, since)`, `storeStatisticsLinesChanged(storeId, since)`, `storeStatisticsRefundsChanged(storeId, since)` | rows with `row.createdAt >= since`, deleted ones included, `sort "row.createdAt", "asc"`, `paginate 250`. The walk asks them from each watermark; the check asks the same three from the start. The engine continues a page by keyset over creation order or a schema-bound text field, not over the row id, and a version is a write time these already order by and project, so the check needs no shape or query of its own and recomputes from the row in hand. |
| query | `storeStatisticsOrderBuyers(storeId, since)` | the same walk over `storeStatisticsBuyer`, for the wholesale pass |
| query | `storeStatisticsShop(storeId)` | the store's shop row, newest first, one row |
| query | `storeStatisticsStores()` | every store row; "Orders since" is `health.ordersFrom`, and `scopesGranted` says whether the grant holds `read_all_orders` |
| query | `storeStatisticsStoreAt(storeId, at)` | the store row as it stood at an instant, and nothing for an instant before it was first written. The walk asks it to find where a store's history begins (`history_floor`, 6.2) |
| query (wholesale pack, `packs/wholesalepack/dsl/queries.memql`) | `wholesaleStatisticsGranted(storeIds)` | `storeId` and `buyerEmail` of entitlements in state `granted`, for every form of a store id named: the pack's rows hold a store id bare or as `v1:shopify:store:<id>`, and the engine does not join the two |
| query (wholesale pack) | `wholesaleStatisticsSettings(storeIds)` | the `storeId` of each `wholesaleSettings` row among the forms named. A query of its own, because a query is bound to one concept |
| builtin | `storeStatisticsRollup {}` | executor `integration.storeStats.rollup`; no `@sdk`; admits the principal `system:maintenance:storeStatisticsRollupSweep` and no other caller, a cluster owner included |
| builtin | `storeStatisticsCheck {}` | executor `integration.storeStats.check`; no `@sdk`; admits `system:maintenance:storeStatisticsCheckSweep` only |
| builtin | `storeStatisticsRebuild { storeId string! }` | executor `integration.storeStats.rebuild`; no `@sdk`; for a person who is a cluster owner, checked in Go: a maintenance principal is refused. The store id is compared bare, and a store the component holds no row for is refused |
| builtin | `storeStatisticsCheckState { storeId string! }` | executor `integration.storeStats.checkState`; no `@sdk`; for a person who is a cluster owner. Answers when the last check pass began and ended, when one last ended clean, and its counts: how S14's "the check has run clean once" is seen |
| builtin | `storeStatistics { siteId string!, storeId string!, period string! }` | executor `integration.storeStats.statistics`; `@sdk`, `@args(profile="object")`, `@requiresCapability("execute", "app:deployables/sales")` |
| automation | `storeStatisticsRollupSweep` | `@trigger(schedule="15 * * * * *")`, calls the rollup |
| automation | `storeStatisticsCheckSweep` | `@trigger(schedule="0 40 * * * *")`, calls the check |
| maintenance principal | `storeStatisticsRollupSweep`, `storeStatisticsCheckSweep` | added to the compiled list in `component/auth/maintenance_actor.go` with their reasons |
| part | `app:deployables/sales` | section 7 |

The wholesale queries sit in the pack because the rows are the pack's. When the pack is
disabled the component does not ask them and treats the store as D10's "not enabled". The
walk reads a store's granted set once a run, because it must hold the set to judge each
order it writes; it compares the set's digest there, so no read of changed entitlements
and no watermark for them is kept.

`component/storestats` registers as the `storeStats` integration provider on every node
type, the way `component/sitetraffic/integration.go` registers `siteTraffic`, and is linked
in by the same blank-import list in `app/` that links `component/emailstats`.

### 6.4 What `storeStatistics` requires

1. The caller holds `execute app:deployables/sales` in the organization of the named site.
   The engine's organization gate judges it before the handler runs: `app:deployables/sales`
   joins `organizationDeployableCapability`, and `storeStatistics` maps to
   `v1:platform:site` / `siteId` in `organizationCapabilityTarget`.
2. The site reads for the caller under their own actor (`siteById`), is of kind
   `shopify_storefront` and is not system-owned.
3. Its `binding.storeId` or `previewBinding.storeId` equals `storeId`, each compared as a
   bare id: a binding may hold `v1:shopify:store:<id>`. A binding is an authorized link
   (F18), so this is what ties the store to the organization.
4. `period` is one of `statsperiod.All()`.

Each refusal is one sentence and names nothing the caller could not already read: a site
that does not read and a site that does not serve the store answer the same sentence,
"This deployable does not serve that store for you."

The handler then reads only `store_sales_*` with SQL, in one read-only transaction.

### 6.5 The answer

One synthetic node, as the website and email reads answer. Money is a decimal string;
everything absent is `null`.

```
{
  storeId, siteId, storeName, storeDomain, destination: "production" | "testing" | "both",
  currency, timezone, development, taxesIncluded, olderOrderChangesNotRecorded,
  period, bucket, start, end, prevStart, historySince, lastChangeAt, asOf,
  recording: { state, reasons: [{ code, sentence }], since },
  rebuilding,
  figures: { totalSales, netSales, orders, averageOrderValue,
             grossSales, discounts, refunds, unitsSold },
  previous: { ...the same, or null when not comparable },
  floors: { ...the figures that are floors },
  series: [{ at, totalSales, netSales, orders, grossSales, discounts, refunds, unitsSold }],
  productsBySales: [{ key, title, value }], productsByUnits: [{ key, title, value }],
  productCount, refundsWithoutItem, incompleteOrders, incompleteRefunds, refundsNotRead,
  buyers: { new, returning, unknown },
  wholesale: null | { wholesale: { orders, netSales }, retail: { orders, netSales } },
  otherCurrencies: [{ currency, orders }]
}
```

`series[].at` is the bucket's start as an instant with the store's offset. Top lists carry
25 rows. `floors` names the line-derived figures that are floors because an order's lines
are incomplete; `refundsNotRead` counts D22's refunds. `incompleteRefunds` counts the
period's refunds that hold a full page of refund lines: a figure is not a floor then (a
missing refund line would lower Units sold, not raise it), so Units sold is absent for the
period. `storeName` and `storeDomain` are there because a holder of the part cannot read
the store row. `olderOrderChangesNotRecorded` is true when the store's recorded grant does
not hold `read_all_orders` (section 10, "An order placed more than sixty days ago").
`averageOrderValue` is answered already rounded at the currency's minor units (5.3).

A bucket is cut from the bounds U2 computes for the store's zone, compared as instants in
SQL (`width_bucket` over the bucket starts), not with `AT TIME ZONE`: the period's own
start and end come from Go, and two time zone databases, Go's and the database server's,
can disagree on a rule or a name.

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
   Europe/Madrid", "Orders since Sep 12", "Last order change recorded 4m ago"
   (`formatFreshness`). On a development store a second line: "This is a development
   store: every order on it is a test order, and they are counted." On a tax-inclusive
   store: "Prices on this store include tax." When the answer's
   `olderOrderChangesNotRecorded` is true, a line of its own: "Refunds, edits and
   cancellations of orders placed more than 60 days ago are not recorded."
2. **Recording notice**, a `Notice`, only when the verdict is not `recording`, a rebuild runs,
   the verdict is not known, or refunds are still being read (D22): the mirror's sentence,
   then "These figures are as of <moment>." (D16). For `starting` and a rebuild: "The
   store's orders are still being brought in." and every figure absent. For a verdict
   that is not known: "Whether this store is recording is not known." The notice names
   the Store page, where the act that fixes it lives ("The Store page of this deployable
   says what fixes it."), and holds no act of its own.
3. **Figure row**, `StatFigures`, eight figures in D12's order, money with `formatMoney`,
   counts with `formatCount`, each with its previous period only when
   `previousComparable(prevStart, historySince)` (`DESIGN.md:577-589`). A floor reads "or
   more" and has no change.
4. **Chart**, `TimeChart` of the chosen figure, labels in the store's time zone. Buckets before
   `historySince` or after `asOf` are gaps (`DESIGN.md:590-594`).
5. **Products**, `RankedList` titled "Products", with a `LocalTabs` control under its heading
   (the Devices pattern, `DESIGN.md:601-604`): "Net sales" or "Units". Eight rows; when
   `productCount` is larger, its footer says "The 8 best-selling of 31 products." and
   opens "All products" as a page under the trail (`DESIGN.md:605-612`). Further footer
   lines, only when they apply: "Refunds of €12.00 name no product, so they are in Net
   sales and not here." (the amount through `formatMoney`, as every amount the page
   writes), "3 orders have more lines than were recorded, so these are floors." and "1
   refund has more lines than were recorded, so units are not exact."
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
| Empty | After a successful read with no counted orders or refunds: figures are a measured 0 where history covers the period, the average is a dash ("No orders in this period."), each list says "No orders in this period." |
| Not recording | Block 2, and D16. |
| Refunds not yet read | Block 2; the four figures D22 names are dashes with its reason. |
| Currency or time zone not known | Money figures are dashes, "The store's currency has not been recorded yet."; with no time zone every figure is a dash, because no day can be cut: "The store's time zone has not been recorded yet." U1's read of the shop row is what keeps a connected store out of this state (F13). |
| A day below zero | A day with more refunded than sold has Net sales, Total sales and Units sold below zero. The figure shows the amount with its sign and the chart draws the day under the zero rule. |
| History shorter than the period | Buckets before `historySince` are gaps; no change is drawn. |

**Cadence.** `AutoRefresh` at the website Statistics' cadence (`statisticsRefreshMs`: five
minutes for 24 hours, fifteen otherwise). A rollup table publishes no event, so nothing
pushes.

**Kit additions** (`clients/os/src/kit`):

- `formatMoney(currency: string): (value: number) => string` in `kit/format.ts`, over
  `Intl.NumberFormat("en-US", {style: "currency", currency})`, the one locale
  `formatCount` writes in, so a figure and the axis beside it agree; the currency's own
  decimals; a non-finite value renders "--". The value is the server's exact decimal
  string converted with `Number()`. Every sum the page can show has at most 15 significant
  digits (under ten trillion with cents), which a double holds and prints back exactly, so
  the display loses nothing; the exact arithmetic is the server's.
- `bucketLabel` and `bucketMoment` take an optional IANA `timeZone`, through
  `Intl.DateTimeFormat`; `TimeChart` passes a new optional `timeZone` prop to them. Without
  it, both stay UTC, so the existing pages do not change.
- `TimeChart` scales from zero up today, and a value below zero would be drawn off the
  plot. It gains a floor below zero when a series has a value there: the value axis gains a
  step below zero and the fill runs to the zero rule. A series with nothing below zero is
  drawn exactly as before.
- `clients/os/DESIGN.md`, "App overviews": "eight stand as two rows of four"; "Statistics":
  `formatMoney` joins the formatters, a store's buckets are labelled in its time zone, and
  a figure that can be below zero is drawn below the zero rule.

## 9. Units of work

Ordered so the mirror comes first and the server side is proved before the page exists.
U1 builds on the mirror record's branch, which will have landed.

The plan (`docs/superpowers/plans/2026-11-11-store-statistics.md`) splits and reorders these
units, and says why where it does: U1 is three tasks; U6's builtins, automations and
principals are declared with the tasks that give them bodies; U7 is six tasks (the rules
that make a fact, the provider, the walk in windows, the fold, the store's state, the
wholesale judgement); U9 is two, the first with U11; U13 is two.

| Unit | What | Files | Interfaces | Depends on | Proved by |
| --- | --- | --- | --- | --- | --- |
| U1 Refund detail in the mirror | The `inline` allowlist key and its validation; the planner renders an inlined connection as its own kind, `inlineList`, a `[]object` holding the fields the entry names and `first: page`, left out of bulk; the mapper leaves it out of a write when the document did not select it; the `Refund` entry inlines its three connections; regenerate. The three lists go on the erasure's keep-list and into its golden file. Every order of a store is read once more, carried over as many sweeps as it takes (6.1). The mirror reads each store's shop row at connect, at boot, daily and on resume. | `cmd/shopifyschema/{allowlist.go,allowlist.yaml,mapping.go,emit_go.go}` and tests; `integrations/shopify/{mapper.go,admin.go,erasure_fields.go,reconcile.go,refund_detail.go,shop.go,plugin.go,connect_write.go,capabilities.go,resume.go}` and the erasure's golden file; regenerated `dsl/shopify/generated/refund.memql`, `integrations/shopify/generated/model.go`, `dsl/shopify/generated/selections/` | 6.1 | the mirror record | the drift test; a planner test refusing an inline connection whose node is mirrored, and one refusing a named field that is text or an object; no inlined list holds a field the erasure would scrub; a read cut at its row budget is not stamped and goes on from its cursor; a database test that applies a fixture order with a line refund, a shipping refund and an amount-only refund and finds the three lists on the refund rows; O3's test, in both orders; a refund mirrored before its detail gets it at the next sweep; a connected store has its currency and time zone without a `SHOP_UPDATE`; L11 (requested cost of `ShopifyFetchOrder`) on a development store |
| U2 Periods in a zone | `ForIn(p Period, now time.Time, loc *time.Location) Bounds`; `For` becomes `ForIn(p, now, time.UTC)`. Day and month starts are local midnights, or the change itself on a day whose midnight the clocks skip; hours are local hours, so a zone offset by 30 minutes cuts at its own hour. | `core/statsperiod/statsperiod.go`, `statsperiod_test.go` | `ForIn` | none | a spring-forward day has 23 hours and a fall-back day 25; a +05:30 zone's 24 hours start on its own hour; a day whose midnight is skipped starts at the change; `For` answers what it answered before |
| U3 Exact money | `core/money`: `Parse(string) (Amount, error)` accepting only `-?digits[.digits]`; `Add`, `Sub`, `Mul(int64)`, `MulRatio(num, den int64, scale int)` rounding half to even at the scale it is given; `MinorUnits(currency)` from a compiled table and `RoundingScale(amount, currency)`, the larger of the amount's own scale and the currency's minor units (5.1); `Scale`; `Equal`; `String`. Built on `math/big`. | `core/money/money.go`, `money_test.go` | `Amount` | none | refuses exponents, spaces, empty strings and floats; a million random sums equal `big.Rat` sums; `0.1 + 0.2` is `0.3`; two thirds of "1.0" in a currency counted in cents is "0.67" |
| U4 Tables | The migration of 6.2. | `component/database/memory-nodes/migrations/20261112090000_store_sales_facts.{up,down}.sql`; the round trip beside the other migration round trips, `component/database/memory-nodes/store_sales_facts_migration_test.go` | 6.2 | none | up, down, up again on the lane's TimescaleDB; the migration uses nothing of it, so the same statements run on plain Postgres |
| U5 Part | Section 7, steps 1, 2, 4 and 5 (the construct lands with U9, and its row in the construct table and the database proof with it). | as listed in section 7 | | none | the parity test; an organization-gate test: a holder in one organization is refused a site of another |
| U6 Constructs | Shapes, queries, builtins and automations of 6.3; the two maintenance principals. | `dsl/shopify/overlay/{shapes,queries,builtins,automations}.memql`; `packs/wholesalepack/dsl/{shapes,queries}.memql`; `component/auth/maintenance_actor.go` | 6.3 | U1, U5 | DSL validation and the edition gate; each query answers nothing to a reader actor and rows to the principal; with the pack disabled its queries are not loaded (O4) |
| U7 Fill | The walk: per store, read changed orders, lines and refunds from each watermark less five minutes under the principal, internal origin and a fresh, roots-only read (as `component/emailstats/rollup.go:454` does), the past in bounded windows read as of their end (section 10); write order, line, refund and refund-line facts with U3; re-derive the derived columns of 6.2 for every order a page touched, in the page's transaction; keep the fact of a tombstoned row, recognised by `deleted` alone, marked deleted at the tombstone's version (6.2); copy the store row, the shop row, D4 and the verdict into `store_sales_store`; run a wholesale pass when the granted digest changed; rebuild on a lower `definition`. Writes are guarded so an older version never replaces a newer one: `ON CONFLICT ... DO UPDATE ... WHERE excluded.<row>_version >= fact.<row>_version`, where a version is the mirror row's own write time (6.2). Every writing transaction takes the one write lock and reads the store's generation under it (section 10). A store's history begins at its `history_floor`. | `component/storestats/{doc,integration,facts,rollup,derive,store,wholesale}.go` and tests | `handleRollup` | U2, U3, U4, U6 | 11.1 |
| U8 Check and rebuild | The check: per store, walk the three `...Changed` queries again from the start (40 pages a run, resuming), for each row written before the pass began recompute its fact from the row in hand and compare it with the stored one by version and by content, write again what differs, re-derive, mark each fact it found a row for and at the end of the pass delete, in batches, the facts no row answered for, deleted facts apart, count D14 disagreements, record `store_sales_check`. A row written after the pass began is the walk's to fold and is not a mismatch. The rebuild, for a person who is a cluster owner: in one transaction raise the store's generation, set `rebuilding_since` and delete its watermarks and check row; the walk deletes the facts in batches and refills; when `walked_through` reaches the clock less five minutes, `rebuilding_since` clears and a check starts at once. A rebuild is proven when that check finishes with `mismatched = 0`. `storeStatisticsCheckState` answers when the last pass began and ended and when one last ended clean. | `component/storestats/check.go` and tests | `handleCheck`, `handleRebuild` | U7 | 11.1 |
| U9 Read | The gate of 6.4; the SQL of 5.3 with store days cut from U2's bounds (6.5); previous period, floors, D6, D11, D16, D22; the answer of 6.5. | `component/storestats/{statistics,statistics_payload}.go` and tests | `handleStatistics` | U7 | 11.1, and `statistics_scale_test.go` |
| U10 Independent totals | A fixture store of Shopify-shaped orders and refunds (discount codes, an order-level discount, a gift card, a line refund with and without restock, a shipping refund, an amount-only refund, a restocking fee, a refund in a later period than its order, an edit that removes a unit, an edit that adds a line, a cancellation with a refund, a test order, two currencies, a buyer with three orders, a wholesale buyer, an order and a refund across a local midnight, one order with 120 lines, a discount Shopify wrote with one decimal, a tax-inclusive order with its shipping refunded, an order deleted after it was read) applied through the mirror's own write path; then the walk; then `storeStatistics`. A reference calculator in the test, written from section 5 over the fixture JSON and sharing no code with U7 or U9, must give the same figures to the cent for every period. | `component/storestats/reference_db_test.go`, `component/storestats/testdata/fixture-store/` | | U9 | the test |
| U11 SDK | Regenerate the client builders so `query.storeStatistics` exists. | `sdk/go/client/generated_builtins.go`, the TS SDK | | U6 | the generators' drift tests |
| U12 Kit | Section 8, kit additions. | `clients/os/src/kit/format.ts`, `kit/statistics/{period.ts,TimeChart.tsx,scale.ts}`, `clients/os/DESIGN.md` | `formatMoney`, `timeZone` | none | unit tests: three currencies with 0, 2 and 3 decimals; a bucket at 23:30 UTC labelled as the next day in a +01:00 zone; a bucket below zero drawn below the zero rule |
| U13 Page | Part, button, page, controls and states of section 8. | `clients/os/src/apps/deployables/{parts.ts,page/DeployablePage.tsx,page/DeployableWorkspace.tsx}`, `apps/deployables/storeStatistics/{read.ts,useStoreStatistics.ts,StoreStatisticsSection.tsx,DestinationControl.tsx,ProductsListPage.tsx}` | 6.5 | U11, U12 | 11.2 |
| U14 Docs | Section 12. | | | U9, U13 | the docs gates |

## 10. Failure cases and limits

| Situation | What happens |
| --- | --- |
| A store with many thousands of orders | The read is grouped SQL over indexed tables; the scale test holds 200,000 orders, 600,000 lines and 20,000 refunds under 500 ms for 90 days when it is run on demand, and a tenth of that in the lane on every run, where it catches a read that asks once an order; only the full size proves the indexes. The first fill reads at most 40 pages of 250 of each source per minute per store, about 10,000 orders a minute, with `walked_through` and D16 keeping figures absent until it catches up. |
| A long history | A read of rows written since an instant costs every version of the concept written since then, on the whole cluster (`component/memql/latest_scan_keys.go`), and a first fill, a rebuild and the check all start at the beginning; a first page over a large mirror would outlast the statement timeout on every run, as the email walk's once did. So the past is read in bounded windows, each as of its end: a day wide at first, halved when a page outlasts a read, doubled after a window that read cleanly. Only a read that reaches the clock is unbounded. Where the past begins is not a compiled date: it is an instant before the store row was first written, found once by asking the engine for the store row as of earlier and earlier instants and kept as `history_floor`. A row's own `createdAt` cannot say it, being the write time of the row's newest version; the probe holds as long as no retention prunes the store row's versions, which a test pins. The check's "within a day" holds for a store whose three sources each fit 24 runs of 40 pages; a larger one takes proportionally longer. |
| An order with more lines than the mirror fetches | On the mirror's branch an order's lines past the first hundred are read, up to fifty pages (F22). The `lines_complete` guard stays for what that cannot reach: for any order whose recorded quantities fall short of `subtotalLineItemsQuantity`, `lines_complete` is false, every line-derived figure for its period is a floor ("or more"), and the products list says how many orders are short. |
| A refund with more than 50 lines | `lines_complete` is false on its fact; Refunds stays exact, because it starts from `totalRefundedSet`. A missing refund line would lower Units sold, so the figure is not a floor: Units sold is absent for the period, `incompleteRefunds` counts such refunds, and the products footer says so. |
| A refund whose detail is not fetched | D22, until its order is read again by a fetch or a sweep's list page (6.1). A refund of an order Shopify no longer shows is never read again: D22 holds for its periods for good. A store with more orders than one sweep reads has them read over several sweeps, and D22 holds meanwhile for the refunds not reached yet. A store one of whose orders can never be written repeats the read at every sweep until it can, as its ordinary sweep already re-reads from a watermark that does not move. |
| An order placed more than sixty days ago | The app does not hold Shopify's `read_all_orders`, so Shopify hides an order once it is more than sixty days old. The mirror keeps such an order as it was last recorded: an answer of nothing never retires an order, or what hangs from one, under a grant without `read_all_orders` (`emptyAnswerRetires` and `readUnderOrders`, `integrations/shopify/apply.go`); only Shopify's own delete notice does. A refund, an edit or a cancellation made to it after that is therefore not recorded, and Refunds can be lower than the store's own admin shows. The answer says so with `olderOrderChangesNotRecorded`, true when the store's recorded grant lacks `read_all_orders` (the test the mirror makes on `scopesGranted`), and the page says it in one line (section 8). |
| A paused store | The mirror keeps the deliveries that arrive while a store is paused and applies them on resume, so nothing is lost and the "as of" of D16 stays true for the pause. A paused store is not swept either, so its one read of every order (6.1) waits for the resume, and its shop row is read again then. |
| A store whose shop never changed | Without U1's read of the shop row such a store would have no currency and no time zone, and every figure would be absent (F13). |
| A tombstoned order | The mirror retires only the order row (Shopify's delete notice, or, under a grant that reads every order, two clean empty answers); its lines and refunds have no notice and no sweep of their own, so their rows stay live and their facts stay. The order's fact is kept, marked deleted (6.2), so no older version of the order can be written back and the check finds nothing to repair. Deleted facts count nowhere, and neither do the lines and refunds of a deleted order: every figure reaches a line or a refund through its order's fact. |
| A refund recorded late | It is folded within two minutes of being written, onto its own past day; that period's Refunds, Net sales and Total sales change. |
| An order cancelled after its period | D3 and 5.4: the earlier period's figures drop. "Last order change recorded" says when. |
| Several currencies over time | D6. A store that changed currency shows its old orders' count and the line naming them. |
| A store whose time zone changes | D5: the next read cuts every day in the new zone. Comparisons across the change are in the new zone too. |
| An order recorded late | The walk reads by the mirror's write time, so a late order is folded within two minutes of being written, onto its own past day. |
| A write the walk missed | The five-minute overlap catches a row committed behind the watermark; the hourly check catches anything else within a day, by version and by content, and repairs it. A change made in place, which keeps a row's write time, is one the walk never sees and only the check does. |
| Several engine nodes | The rollup and the check run on the cron leader, one run at a time per process, and every write transaction takes a table lock across replicas, as `component/sitetraffic/rollup.go` does; a scheduled automation can still overlap across nodes, so every pass is safe to run twice and at once. Every write is guarded by version and the derivation is recomputed from facts, so two runs give one answer. Every transaction that writes a fact table, the wholesale pass's and the check's included, begins by taking that one lock. The version is the mirror row's own write time (6.2), not its `updatedAt`: on the mirror's branch every mirror row carries `versionSource`, the rule that orders two mirror writes is `component/memql/sync/version.go`, and a row's `updatedAt` is Shopify's version only when `versionSource` is `origin`. A retired row is a tombstone the mirror keeps: the same row with `deleted` true, every other field as it last was, `updatedAt` the time MemQL read Shopify and `versionSource` `read` or `announced`. A read recognises it by `deleted` alone. Reads are pure reads of shared tables and any node answers. The walk's reads are fresh reads, so a node's result cache never hands it an old page (F20). |
| A rebuild while a walk or a check runs | A page read before a rebuild and written after it would write old facts over a store whose watermarks the rebuild had set back, and the rows between would never be read again. `store_sales_store` carries a `generation` the rebuild raises: every page and check transaction reads it under the write lock and writes nothing when it is not the one read before its page was asked. A rebuild's delete, a removed store's delete and the check's end-of-pass delete run in batches, so no one transaction holds the write lock for the length of a store, and nothing is folded for a store while its facts are being deleted. |
| The order fetch costs more | U1 adds up to 70 nodes per refund to the order fetch; L11 measures it before release. If it passes Shopify's limit on a real order, the page sizes of 6.1 are lowered, and a refund past them is incomplete as above. |
| The verdict cannot be read | `shopifyStoreVerdict` lands with the mirror. When the walk's call of it fails, `verdict_state` is empty and the page says "Whether this store is recording is not known." above the figures. |
| The wholesale pack is turned off | `wholesale` is null and the block is absent; the stored flags are left and recomputed when it is turned on again. |
| A customer's data is redacted | A customer's erasure rewrites the customer's rows in the mirror in place and keeps their write times, so the walk does not see it. The next check pass does, because it compares each fact's content with what its row gives now (U8). What a fact held of the buyer is their Shopify customer id, which the erasure keeps on the order as it keeps every reference while it empties the customer's own row; and, on a custom line item (one with no product), a title a person typed, which the erasure scrubs. After that pass the line's title and product key are the erasure's marker, and the order, whose email was scrubbed, is retail. The order's money stays in every figure. |
| The store's data is removed | D17: the store's facts and its `store_sales_store` row are deleted, in batches. |

## 11. Testing

### 11.1 Server

- **U1, U2, U3**: as in section 9.
- **U7 (database)**: an order with three lines writes its order and line facts with 5.1's
  numbers; a line refund writes a refund fact on the refund's day and leaves the order's
  day unchanged except for `refunded_quantity`; a shipping refund lowers Total sales and
  not Net sales; an amount-only refund lowers Net sales by its amount less its tax; an
  edit that removes a unit lowers Gross on the order's day and moves no Refunds; an edit
  that adds a line raises Gross; a cancellation removes the order and its refunds from
  every figure; a tombstone marks its fact deleted and keeps it; a tombstone met in a later
  window than the live version, and an older live version arriving after it from another
  node, leave it deleted; an older version arriving after a newer one changes nothing; a
  rebuild landing between a page's read and its write ends with the full facts; a row committed behind a watermark is read by the next run; two
  concurrent runs write the same facts; a development store counts its test orders and a
  live store does not; a granted buyer's orders become wholesale within one run of the
  grant, and retail after a revoke; a lower `definition` rebuilds; the walk reads no field
  outside the shapes.
- **U8 (database)**: a fact deleted by hand is restored by the check, for each of the four
  fact tables; a fact whose version was set back is recomputed; a fact whose row was changed
  in place is recomputed; a fact with no mirrored row is deleted; a pass over a store with a
  deleted order ends with nothing mismatched; after a customer's erasure and one pass no
  fact holds a value the erasure removed and the figures still hold the order's money; a
  rebuild ends with the same facts as before it and a clean check.
- **U9 (database)**: each refusal of 6.4; a holder of the part in another organization is
  refused; a site bound only through `previewBinding` is answered as Testing; store days in
  a zone west and east of UTC; a spring-forward day; previous period null when it starts
  before `historySince`; the average null with no orders; other currencies listed and not
  summed; floors with an incomplete order; D22's dashes; buyers new, returning and unknown;
  D16 for each verdict state; a caller who is not a cluster owner gets figures.
- **U10**: the independent totals check.
- **Live, against a development store** (skipped without credentials): an order placed
  through the storefront, then refunded, appears in the facts within two minutes of the
  mirror recording each.

### 11.2 Page

`clients/os/test/deployables/storeStatistics.test.tsx`: the button's presence for each
gate of S1 and D15; the destination control only with two different stores; each state of
section 8; eight figures in D12's order with money in the store's currency; chart gaps
before `historySince` and after `asOf`; the development and tax lines; the products
footer lines; no dividers between rows; both themes and both widths.

### 11.3 The production walk

On the owner's instance, on a development store bound to a storefront deployable, after
the release. Each step is done in the OS and in the store's Shopify admin (Analytics,
Reports: "Sales over time" and "Sales by product").

| # | Step | Passes when |
| --- | --- | --- |
| P1 | As a cluster owner, call `storeStatisticsCheckState` for the store, then open the deployable. | The answer says a pass has ended clean: `cleanAt` is set and `mismatched` is 0. "Store statistics" is beside Statistics. |
| P2 | Open Store statistics. | The context line names the store, its currency and time zone, "Orders since", a recent last change, and the development-store line. No notice. |
| P3 | Choose "Last 30 days". Compare all eight figures with the admin's report for the same 30 days in the store's time zone. | Equal, or each difference is one named in 5.5 and explained by a live check below. |
| P4 | Place an order through the storefront with a discount code and shipping, using the store's test gateway. | Within two minutes of the mirror recording it, Orders rises by one, Discounts by the code's amount, and Total sales by the order's total. |
| P5 | In the admin, refund one unit of that order without restocking. | On today's bucket, Refunds rises by that unit's discounted price and Net sales falls by it (L3, L15). |
| P6 | Refund its shipping. | Total sales falls by it; Net sales does not move (L13). |
| P7 | Refund a fixed amount on another order with no line chosen. | Refunds rises by the amount less its tax, on today (L4). |
| P8 | Edit a third order to remove a unit without refunding it, and a fourth to add a line. | Gross sales on each order's own day falls by the removed unit and rises by the added line; Refunds does not move (L1). |
| P9 | Cancel a fifth order that has a refund. | It and its refund leave every figure. |
| P10 | Switch the destination to the other store, then back. | The figures are the other store's, then the first again. |
| P11 | Pause recording for the store on its Store page; reopen Store statistics; resume. | While paused, the notice gives the mirror's sentence and "as of"; after the resume it goes. |
| P12 | Choose "Last 24 hours". | Hours are labelled in the store's time zone; P4's order and P5's refund are in their own hours. |
| P13 | Compare the Products list by net sales and by units with "Sales by product". | The same leaders and amounts. |
| P14 | With the wholesale pack enabled and set up for the store, approve a test buyer, then place an order as that buyer. | The Wholesale and retail block appears and the order is Wholesale. |
| P15 | As a cluster owner, run `storeStatisticsRebuild` for the store. | The page says the orders are being brought in, then shows the same figures as before, and the check that follows is clean (`storeStatisticsCheckState`). |
| P16 | Note what L1 to L16 found. | Each check of 5.5 is answered and this record is corrected where it was wrong. |

**Live checks** settled in the walk: L1 `originalUnitPriceSet * qs` after edits; L2 the sum
of discount allocations against `totalDiscountsSet`; L3 `currentQuantity` after a refund
with and without restock and after an edit removal; L4 `totalRefundedSet` as all money
refunded, and the order adjustment of an amount-only refund, with the sign of its
`amountSet` and `taxAmountSet`; L5 the dates the report uses
for an order and for a return; L6 tax-inclusive Gross (on a store set to include tax in
prices); L7 that every order on a development store has `test = true`; L8 that the sum of
line quantities equals `subtotalLineItemsQuantity` on a complete order; L9 Shopify's average
order value; L10 whether Shopify writes an order's `email` in the case the buyer typed; L11
the requested query cost of `ShopifyFetchOrder` on an order with refunds, against Shopify's
limit (also a gate of U1); L12 a refund with a restocking fee, and the sign of its
adjustment; L13 the meaning of `totalShippingPriceSet`, `totalTaxSet` and
`originalTotalDutiesSet`, and whether a refunded shipping line's `subtotalAmountSet` holds
its tax on a store that includes tax in prices; L14 Total sales against
the admin's for the same period; L15 Net sales in a period holding a refund of an earlier
order; L16 the scale Shopify writes an amount in, a whole amount with one decimal, which is
why a share is rounded at the currency's minor units (5.1; a live test of U1 logs it).

## 12. Documentation

- `docs/public/operate/deployables.md`: the Store statistics page, the sales part and how
  to grant it, the destination switch, what "as of" means.
- `docs/public/operate/shopify-connector.md`: the refund detail the mirror now records;
  what is computed from the mirror, the figure definitions of section 5, the known
  differences from Shopify's reports, the check, how to run a rebuild.
- `cmd/shopifyschema/allowlist.yaml` header: the `inline` key, beside children and references.
- `clients/os/DESIGN.md`: section 8's kit additions.
- This record, corrected by the live checks.

## 13. Rollout

1. The mirror record's release has landed.
2. **One release** carries U1 to U14. On start, the walk fills every connected store by
   itself; no group holds the part, so only cluster owners and developers see the button.
3. On the development store, the check runs once and finishes clean
   (`storeStatisticsCheckState` answers `cleanAt` set and `mismatched` 0). Being hourly, it
   does so within the first hours.
4. The production walk (11.3) passes on that store.
5. Only then is the part granted to a client's group.

## 14. Unsettled

| # | What | How it is settled |
| --- | --- | --- |
| O1 | Shopify's meaning of the fields in 5.5 | L1 to L16 in the walk |
| O2 | Whether the order fetch stays under Shopify's query cost limit with the refund detail inlined | L11, a gate of U1 |
| O3 | That the first reconcile after a bulk backfill re-reads every order, so inlined refund detail arrives (F12 states it for references) | Read in the code: only a store's first sweep, which has no watermark, lists every order; a later one reads what changed. So the detail arrives with the first sweep, with an order's own fetch, or with the one sweep from the start of 6.1, and a bulk line never removes it. Held by a U1 database test in both orders: backfill then first sweep, and first sweep then backfill, the lists are present |
| O4 | Whether a disabled pack's queries are still loaded; the component asks the pack state first either way | Read in the code: they are not. A disabled pack is mounted inert: its concepts load and every query, mutation, builtin and shape is skipped (`dsl/pack_enablement.go`, `SkipsBehavioralLoad`), so asking one is an unknown call. Held by a test in U6 with the pack disabled |
| O5 | Whether a top-level builtin's answer is ever served from the result cache (`useSiteStatistics.ts:15-17` claims it is not; this record did not find the code) | a U9 test that reads twice around a fact write on one node and sees the change |
| O6 | The exact name of the pack-state read the component calls | Settled: `PackDomainDisabled(domain string) bool` in package `dsl` (`dsl/pack_enablement.go`), asked with `wholesale`. It is the loaders' own answer for the node, set at boot from the `v1:platform:packState` rows over the pack's declared default, and what `component/memql/module_registry.go` reports as loaded or mounted inert. `ReadPackStates` (`component/memql/pack_state.go`) reads the rows, the cluster's wish, which can differ from a node until it restarts; the question is whether the pack's queries exist on the node that runs the pass |
| O7 | That `row.createdAt >= since` with `sort "row.createdAt"` pages correctly over mirrored rows as it does over campaign rows | the U7 overlap test |

## 15. Proposed additions

None at present. Anything beyond the decided design is listed here for the owner first.

## 16. Not in this version

- A custom date range (S3).
- Conversion between currencies, and presentment-currency figures.
- Orders older than the mirror's history (mirror record D11).
- Shipping, tax, duties, tips and fees as figures of their own (they enter Total sales only).
- Sales by channel, location, discount code, customer or region.
- A Rebuild act on the Store page: the definition version (D19) and the hourly check cover
  what it would do; the builtin stays for a cluster owner.
- Filling facts on write through an event trigger: the minute walk is enough.
- Exports, emails, alerts or any message about sales.
- Telling apart the orders of two destinations bound to the same store.
- Any model or agent.
