---
title: A connected store's purchases are recorded, and its Store page says when they are not
audience: internal
status: approved
area: operate
sinceVersion: 0.31.33
owner: znas
---

# Store mirror: recording every purchase, and saying so when it stops

Owner brief, 2026-11-11. On the owner's instance a storefront deployable is connected
to a development store while the storefront is tested. Its store read "Connected", yet
Cluster > Data listed 34 of the 69 Shopify mirrors with an error and the mirror held no
orders, while the store itself had orders. The owner wants the mirror to "work excellent
100% of the time" and to record "all the info that we can gather from purchases". Shopify
stays the source of truth; MemQL keeps a copy and has to say so when it cannot.

A statistics screen for a storefront's purchases is a separate record. It reads what
this one records, so this one comes first.

**Approvals.** The owner, in chat, on 2026-11-11: chose "the settings first, then one
engine release" over settings alone and over a private app for each store; approved
this design ("Yes, write it up"); and approved this record ("Yes"), asking in the same
breath that any wait in the way of a test can be shortened to minutes, which is why the
alert wait of D10 is a value and not a constant.

## 1. What the owner asked for

| # | Requirement, in the owner's terms |
| --- | --- |
| M1 | Every purchase on a connected store is recorded, with everything the store gives about it. |
| M2 | It keeps working with nobody watching: a missed change is caught, and the past arrives by itself. |
| M3 | The storefront's Store page tells the truth: recording or not, why, and the one thing that fixes it. |
| M4 | The owner is told when recording stops. |
| M5 | Generic: any storefront and any store, a development store now and a merchant's live store later. |

## 2. What is wrong at v0.31.32

Checked in the code at `6a43368be` and on the owner's instance.

| # | Finding | Where |
| --- | --- | --- |
| F1 | Shopify refuses the order mirror: "This app is not approved to access the Order object". Orders, customers, draft orders, abandoned checkouts, refunds and transactions are protected customer data, and the managed app has not declared that it uses it. On a development store the declaration needs no review, only the selection. | Shopify's rule; the app's settings in the operator's Shopify partner organization |
| F2 | The managed connection never asks for customers, discounts or returns, so those domains are not granted by design. Health still measures a store against the complete mirror's list, and Cluster > Data prints the standing reason where an error goes. | `managedConnectScopes` (`integrations/shopify/connections.go:27`); `scopesNeeded` (`capabilities.go:350`) |
| F3 | A store that approved the app before the list grew keeps its older grant, and nothing says so. `write_products` and `read_orders` joined on 2026-09-30 (`53b46ece8`); a store approved five days earlier still dead-letters every product-content push. | `writeManagedConnection` records the grant once |
| F4 | "Connected" means the tokens exist and the edge serves the Storefront token. It never asks whether anything is being recorded. The panel that listed the mirrored domains is no longer drawn. | `storeConnection` (`clients/os/src/apps/deployables/store/rows.ts:137`); `ShopifyStorePanel.tsx` |
| F5 | A backfill only starts when an operator presses it, and nothing continues one that used up its page budget. | `component/datasync/capabilities.go:26`; `Runner.StartBackfill` |
| F6 | Sync state is one row for a concept, a connector and a direction. Every store shares the backfill cursor, the reconcile watermark and the last error. A second store is never backfilled once the first is `complete`; one store's failure ends the sweep for the stores after it and is reported for all of them. | `SyncStateID` (`component/datasync/store.go`); `Connector.Backfill` (`integrations/shopify/backfill.go:108`); `Connector.Reconcile` (`reconcile.go:91`) |
| F7 | A delivery whose apply fails is stamped `failed` and never tried again. Shopify already had its `202`, so it does not resend. Only the hourly sweep recovers the change. | `Dispatcher.dispatchTo` (`component/datasync/inbound.go`) |
| F8 | The reconcile watermark is the time the sweep ended. A row that changed while the sweep ran, behind the page already read, is older than the next sweep's `since` and is skipped. | `Runner.Reconcile` (`component/datasync/runners.go`) |
| F9 | An order selects `lineItems(first: 100)` and nothing paged a child connection, so an order with more lines lost the rest. Confirmed by `TestAnOrderWithMoreThanAHundredLinesKeepsThemAll`, and fixed: a fetch or a sweep now reads each paged child connection to its end, fifty pages at most. | `integrations/shopify/generated/model.go` |
| F10 | The generated reads require `actor.isClusterOwner`, and the connector's actor has the role `connector`, so the connector's own reads of the mirror answered nothing: no tombstone on a full re-list, empty `commerceSold`. Confirmed by `TestTheConnectorReadsBackTheMirrorRowsItWrote` and `TestAFullRelistTombstonesWhatTheOriginNoLongerHas`, and fixed: the tombstone pass, the analytics walk and the product capability read under `operatorContext`. | `dsl/shopify/generated/order.memql:317`; `component/auth/connector_actor.go` |
| F11 | Nobody is told. A broken mirror is a row in Cluster > Data. | |
| F12 | No webhook was ever registered. Every creation carried `apiVersion` inside `WebhookSubscriptionInput`, which the type does not define, and Shopify refused the whole call. The store's health read 66 desired, none existing, 66 failed; the mirror moved only when a sweep ran, so a new order waited up to an hour. Found on 2026-11-11 by placing an order and watching it not arrive. **Fixed on `main` in `177cf52bb` (0.31.33)**, ahead of the rest of this record. | `subscriptionInput` (`integrations/shopify/subscriptions.go`) |
| F13 | Known, not fixed by this record: a connection stored on the parent row as a field is still read one page deep, so an order keeps only its first 25 discount applications, shipping lines and fulfillment orders, and any row only its first 50 metafields. | `metafieldPageSize` and `refsPageSize` (`cmd/shopifyschema/mapping.go`); the order selection in `integrations/shopify/generated/model.go` |

F9 and F10 come from reading the code. Each gets a failing test before any fix, and
is dropped from this record if the test passes.

F12 is why the verdict's `events_not_registered` reason matters: the failure sat on
the store's health for weeks, and no screen draws that record.

## 3. Shape of the solution

Three moves.

**Ask for what a storefront needs, and say what was not asked for.** The managed
connection asks for the commerce set: the catalog, orders, customers, discounts and
returns. A domain outside a store's connection is a fact about that connection. It is
listed as such and never as an error.

**Make the runtime per store and self-healing.** Sync state is kept for each store. A
backfill starts when a store connects or gains a permission, and continues until it is
done. A failed delivery is tried again. The hourly sweep stays as the net under both.

**One verdict for each store, shown where the storefront is managed.** The engine
answers one question for a store: is it recording, and if not, why and what fixes it.
The deployable's Store slot and Store page show that answer. When the answer has been
bad for 30 minutes (a wait the operator can change) the store's owner gets an email,
and one more when it is good again.

Nothing here decides anything about a shopper or an order, and no model is involved.
Every step is a fixed rule.

## 4. Decisions

| # | Decision | Why |
| --- | --- | --- |
| D1 | The managed connection asks for the commerce set: today's list (the four Storefront scopes, `write_products`, `read_inventory`, `read_locations`, `read_orders`) plus `read_customers`, `write_customers`, `read_discounts` and `read_returns`. | M1. `write_customers` is what the wholesale pack's tag adapter needs (memql#6447); asking now means one approval by the store instead of two. |
| D2 | Coverage belongs to the connection. Health compares a store's grant with what its own connection asks for. Domains outside it are "not part of this connection". | F2. A standing fact printed as an error teaches people to ignore errors. |
| D3 | The app's data declaration at Shopify is operator setup, written into `shopify-connector.md`: protected customer data, with the name, email and address fields, and not phone. The engine cannot make it, but names it when Shopify refuses for that reason. | F1. Nothing uses a phone number, and Shopify approves only the minimum an app needs. |
| D4 | Sync state gains a partition, which for this connector is the store. The runtime stays generic: a connector that serves many tenants says so through one optional interface. | F6. The live store joining a cluster that already holds a development store must not inherit its cursor, its watermark or its error. |
| D5 | A backfill is queued by the engine: for every domain when a store connects or reconnects, for the domains a wider grant unlocked, and for a domain Shopify refused that now answers. A scheduled pass continues every queued or running backfill. The operator's button stays. | F5, M2. |
| D6 | A failed delivery is tried three more times when another try can change the outcome: 1 minute after it failed, then 5 minutes after that try, then 30 minutes after that one. A refusal that will not change (not granted, an unknown store, a body that does not parse, a version pin) is not retried. After the last try the row stays `failed` and is listed. | F7. The sweep already heals the data; the retry closes the hour in between. |
| D7 | The watermark is the time the sweep started, less two minutes. | F8. Apply is version-guarded, so reading a row twice writes nothing. |
| D8 | The verdict is computed in the engine, by one builtin, from the store row, its sync state and its deliveries. A reason is a code with one sentence and at most one act. | M3. The shell keeps no copy of the rules, as with `storeHasStorefrontToken`. |
| D9 | The storefront's Store slot and Store page show the verdict. Cluster > Data keeps the per-domain detail, now grouped by store. | F4. The person managing a storefront looks at the storefront. |
| D10 | When a store's verdict has needed attention for the alert wait, one email goes to the store's owner through the transactional outbox, and one more when it recovers. The alert wait is 30 minutes unless the operator sets another, from 1 minute to 24 hours. The request id is derived from the store and the episode, so nothing is sent twice. | M4. No new delivery lane: the address is a cluster user's and must be on the outbound allowlist, like every operational email. A tester sets five minutes to walk it and puts it back, as with a campaign's wait (the owner's direction, 2026-11-10). |
| D11 | `read_all_orders` is not requested. A store's history begins 60 days before it connects, and the Store page says since when. | It needs Shopify's approval. Whether a merchant's full history is wanted is decided with that merchant. |
| D12 | The mirror stores what Shopify returns. A field Shopify withholds is `null` and is named on the Store page as withheld, which is not an error. | `fieldsDenied` already tracks it. |

**Not chosen: a private app for each store.** Shopify gives a custom app protected
customer data with no review, so it would spare a merchant's store the review in
section 12. It also means an app to create and configure for each store and two kinds
of connection to keep working. It remains the fallback for a live store if the review
takes too long.

## 5. Data

Fields are added. None is removed or renamed (removing a field bricks stored rows,
memql#5199).

- **`v1:platform:syncState`** gains `partitionKey` (string, empty for a connector that has
  none; the engine reserves the name `partition`). The row id is derived from the concept,
  the connector, the direction and the partition, and is unchanged when the partition is
  empty. `backfillStatus` gains the
  value `queued`, the state a backfill waits in until the continuing pass picks it up.
- **`v1:platform:inboundRequest`** gains `attempts` (int) and `nextAttemptAt`
  (datetime). `status` keeps its four values.
- **`v1:shopify:store`** keeps its `health` object, which gains `verdict`: the state,
  the reason codes, `since`, and the episode the owner was last mailed for.
- **The alert wait** is one operator value for the cluster, a `v1:platform:globalVariable`
  row read on every pass of the watch, as the managed app's own configuration is. Absent
  means 30 minutes; a value outside 1 minute to 24 hours is refused when it is saved.

**The existing rows.** A Shopify sync-state row written before this version has no
partition. It is left as history. Each store's own rows start empty: the first sweep
reads from the beginning within its budget, and a backfill is queued for every domain
in the store's connection. Apply skips rows the mirror already holds (`516275348`), so
a store that was in sync re-reads its catalog once and writes nothing.

## 6. The verdict

`shopifyStoreVerdict(storeId)` answers one of five states.

| State | Meaning |
| --- | --- |
| `recording` | Every swept domain in the store's connection has had a clean sweep within twice its interval, events are registered, and no reason below holds. A child domain, which is only read with its parent, is not asked. |
| `starting` | Connected, and its first backfill is queued or running. |
| `paused` | An operator paused recording for this store. Deliveries are still staged, and the first sweep after a resume catches up. Nobody is mailed. |
| `needs attention` | At least one reason below. |
| `not connected` | Shopify reported the app uninstalled or the store's data removed. These two exist today and keep their words. |

| Reason | When | What the Store page says | The one act |
| --- | --- | --- | --- |
| `data_not_declared` | Shopify refuses a domain in the connection with its protected-customer-data answer. | "Shopify refuses orders for this app, because the app has not declared the customer data it uses." | For the operator: the declaration. The act links to Shopify's own page on protected customer data, since the OS has no helper for linking to the docs; the steps are in `shopify-connector.md`. |
| `approval_outdated` | The store's grant lacks a scope its connection asks for. | "This store approved an older list of permissions." | For the store's owner: "Open MemQL OS from the store's Shopify admin and approve", with a link to that admin's apps page. |
| `grant_lost` | The offline grant cannot be renewed, or Shopify answers 401. | "MemQL can no longer reach this store." | Reconnect in Settings. |
| `events_not_registered` | The last subscription pass failed, or a topic of a domain in the connection is missing. | "Shopify is not sending every change." | "Register again", which runs the subscription pass for this store. |
| `check_failing` | A domain in the connection failed its last two sweeps for a reason other than a grant. | "The last check of orders failed", then the engine's reason. | None. The next sweep is named with its time. |
| `changes_not_recorded` | A delivery used up its tries, and its domain has had no clean sweep since. | "3 changes could not be recorded." | "Try again", which dispatches them once more. |

The sentences name the domain concerned. A domain is named in plain words (orders,
customers), never by concept id.

Two facts sit beside the verdict and are not part of it. Whether the storefront can
read the store is a different question: a store can serve its catalog to shoppers
while nothing is recorded, and the page says both. And "Orders since" gives the date
the store's history begins (D11).

## 7. Operator surface

All of it lives in the Deployables app, on a storefront deployable, for a person
holding `execute app:deployables/store` as today.

- **Store slot on the deployable.** The verdict's word replaces "Connected":
  Recording, Starting, Paused or Needs attention. Uninstalled and Data removed stay.
- **Store page, each connection row.** The same word. A destination whose store needs
  attention shows each reason as a `Notice` with its sentence and its one act. The same
  store bound to both destinations shows a reason once. The two token repairs the page
  names today stay as they are.
- **Store page, acts.** "Pause recording" and "Resume", which the earlier panel had,
  and the acts of section 6. An act on a store is for the store's owner or a cluster
  owner, as connecting it is.
- **Store page, "What is recorded".** One line for each kind in the store's
  connection (catalog, orders, customers, discounts, returns) with when the last change
  arrived and when it was last checked, then "Orders since" and any field Shopify
  withholds. Kinds outside the connection are one closed line: "Not part of this
  connection", which opens to the list.
- **Cluster > Data.** Domains are grouped by store. "Not part of this connection" is its
  own group and never fills the error slot. A domain's row gains "Try again" for
  deliveries that used up their tries.

The interface rules of `clients/os/DESIGN.md` hold: kit components only, no dividers
between rows, a state in plain text with no pill or badge inside a row, `Notice` and
never a toast, a control whose part is not held is absent.

## 8. Runtime changes, by unit

Each unit has one job and can be tested alone.

| Unit | What changes | Where |
| --- | --- | --- |
| U1 Coverage | The managed scope list (D1). `connectionScopes(store)` answers what a store's connection asks for; health, the subscription pass and the sweep's "not granted" all read it. | `integrations/shopify/connections.go`, `capabilities.go`, `subscriptions.go`, `reconcile.go` |
| U2 Partitions | `Partition` on the sync state and on a mirror write. An optional `Partitioned` interface on a connector: its partitions, a backfill page for one, a reconcile of one. The runner, the due check, the pause switch and inbound health take a partition. The Shopify connector implements it with `BackfillStore` and `reconcileStore`, which exist. | `component/memql/sync`, `component/datasync`, `dsl/platform`, `integrations/shopify` |
| U3 Backfill by itself | The connect callback queues backfills from the difference between the prior grant and the new one. The first clean sweep of a domain Shopify had refused queues one. A new automation continues queued and running backfills every five minutes, one domain of a store at a time, and leaves a paused domain alone. | `integrations/shopify/connect_write.go`, `component/datasync/runners.go`, `dsl/platform/automations.memql` |
| U4 Delivery retry | The dispatcher classes a failure as worth another try or not, and stamps `attempts` and `nextAttemptAt`. A new automation dispatches due rows every minute, a bounded batch at a time. | `component/datasync/inbound.go`, `dsl/platform` |
| U5 Watermark | The sweep's start time, less two minutes, becomes `lastReconcileAt`. | `component/datasync/runners.go` |
| U6 Verdict and alert | `shopifyStoreVerdict`, and a watch automation that every five minutes computes each store's verdict, records it on the store row, and stages the two emails of D10. | `integrations/shopify/verdict.go` (new), `dsl/shopify/overlay` |
| U7 Screens | The Store slot, the Store page and Cluster > Data as in section 7. | `clients/os/src/apps/deployables/store`, `clients/os/src/apps/cluster` |
| U8 Two suspicions | A test for F9 and one for F10; a fix for each that fails. | `integrations/shopify` |

U2 comes first: U3 and U6 are wrong without it as soon as a cluster holds two stores.

## 9. What can go wrong

| Situation | What happens |
| --- | --- |
| Retries pile up behind a store that is down | A row has four tries at most, and a tick dispatches a bounded batch. Deliveries are still deduplicated on Shopify's webhook id. |
| Backfills compete for Shopify's budget | Shopify runs one bulk operation for a store at a time. A domain waits its turn; the continuing pass takes one domain of a store on each tick. |
| The alert cannot be delivered | The owner's address is not on the outbound allowlist. The Store page says the alert could not be sent and why. |
| A wider grant is stolen | `write_customers` adds what the connection can change. The connector uses it only for the wholesale tag and for creating a customer; this is stated in the connector's doc beside the scope. |
| More personal data is held | Declaring name, email and address is Shopify's Level 2. Its requirements (encryption, retention, test data kept apart, an incident procedure) are the operator's, and the privacy topics already redact on request. |
| Two nodes run the same pass | The continuing pass, the retry pass and the watch run under the cron leader, which is one node. The connect callback can run on any node, and it only queues. |
| An engine release widens the list | Every managed store reads `approval_outdated` until its owner approves again, and its owner is mailed after the alert wait. That is the intended signal, once for each store. |

## 10. Testing

- **U1.** The managed list is exactly the commerce set. A store granted the old list
  reads `approval_outdated` with the scopes it lacks. A domain outside the connection
  is never in `scopesMissing`.
- **U2 (database).** Two stores: each has its own cursor, watermark and error. The
  second store backfills after the first is complete. One store's refusal leaves the
  other's sweep clean. A connector with no partitions keeps its row ids.
- **U3 (database).** A connect queues every domain. A grant that gains
  `read_customers` queues customers and nothing else. A backfill that used its page
  budget is continued to `complete`. A paused domain is not started.
- **U4 (database).** A failure worth another try is dispatched again at 1, 5 and 30
  minutes and then stays `failed`. A refusal that will not change is never retried. A
  retry that succeeds stamps `processed`.
- **U5.** A row changed during a sweep, behind the page already read, is healed by
  the next sweep.
- **U6.** Each reason from a fixture of store, sync state and deliveries. The alert
  is staged once after the alert wait, not again in the same episode, and once on
  recovery. With no value set the wait is 30 minutes; a value of five is honoured; zero
  and 25 hours are refused.
- **U7.** The Store page for each state and reason, in both themes and at both
  widths; "not part of this connection" never draws as an error; a person without the
  store part sees none of it.
- **U8.** An order with 101 lines keeps all 101. A read as the connector answers the
  rows it wrote.
- **Live, against a development store.** Skipped without credentials. The grant holds
  the commerce set; a created order is mirrored with its lines, its transaction and its
  buyer; an edit, a refund and a cancellation follow; a second store keeps its own
  state.

**The walk, on the owner's instance.** Run after the rollout in section 12.

| # | Step | Passes when |
| --- | --- | --- |
| W1 | Read the Store page after the store approved the new list. | Starting, then Recording, with nobody pressing Backfill. |
| W2 | Compare the mirror with the store's admin for the last 60 days. | The same orders, and the same customers. |
| W3 | Place a test order. | It is in the mirror within a minute, with its lines, prices, discount, tax, shipping, payment and buyer. |
| W4 | Edit it, refund one line, then cancel it. | Each change follows within a minute. |
| W5 | Pause recording for the store, place an order, resume. | Paused while it lasts; the first sweep after the resume records the order. |
| W6 | Set the alert wait to five minutes. On a development store that no public address serves: uninstall the app. | Not connected at once; an email after five minutes. |
| W7 | Reinstall it from that store's admin, then put the alert wait back. | Starting, then Recording, with the past brought in; the recovery email arrives. |
| W8 | Read Cluster > Data. | Grouped by store; nothing outside the connection shown as an error; no dead letters. |

W6 and W7 cut a store off, so they never run against a store a public address serves.

## 11. Documentation

`docs/public/operate/shopify-connector.md`: what the managed connection asks for and
why; the data declaration as a numbered runbook; approving again after the list grows;
the verdict and each reason; a store's history beginning 60 days back; the delivery
retry. `docs/public/operate/deployables.md`: the Store page. `clients/os/DESIGN.md`
only if a kit piece is added.

## 12. Rollout

1. **Settings, with no release.** The operator makes the data declaration (D3). The
   development store's owner opens MemQL OS from the store's admin and approves, which
   also brings the grant up to today's list (F3). Orders arrive with the next hourly
   sweep, or at once with the operator's Backfill button. Done on the owner's instance
   on 2026-11-11: the store's 20 orders were recorded a minute after the approval, by
   the sweep, with no backfill pressed.
1a. **The webhook fix, released by itself** (F12, 0.31.33), so that a new order arrives
   within seconds while the rest is built.
2. **The release** carrying U1 to U8.
3. **The store approves once more**, for the scopes of D1. The backfill starts by
   itself.
4. **The walk** (section 10).
5. **A merchant's live store, later.** A public app needs Shopify's review of the data
   declaration before it may read a real store's orders, and the wait is Shopify's. The
   request is submitted as soon as the declaration is made, so the wait runs while the
   rest is built. If it is still open when a merchant is ready, the private app of
   section 4 is the fallback.

## 13. Settled while planning

The plan (`docs/superpowers/plans/2026-11-11-store-mirror-recording.md`) read the code
this record names and settled what it left open. Where the two differ, the plan holds.

- **A third standing answer.** Besides "not granted", a domain can be "not requested":
  outside the store's connection, or refused by Shopify for a scope the connection
  never asks for (inventory shipments and transfers, for one). Neither is an error, and
  neither keeps a store from reading Recording.
- **`check_failing`** means no clean sweep of the domain within twice its interval, the
  same threshold `recording` uses. Nothing counts failed sweeps, and no field is added
  to count them.
- **"Orders since"** is stamped on the store's health when the order backfill is first
  queued, 60 days back, because the store row holds no connect time.
- **A store connected before this version** gets its backfills queued at boot, for the
  domains it never backfilled.
- **A retried delivery fetches again.** The connector forgets a webhook id whose apply
  failed, or its own 15-minute duplicate guard would swallow the retries at 1 and 5
  minutes.
- **The continuing pass** runs one domain of a store on a tick and yields when a
  running bulk operation made no progress.
- **"Try again"** for deliveries sits beside the dead letters in Cluster > Data, one line
  for each store, because a delivery belongs to a store and not to a domain.
- **The alert wait** is read and set through two builtins and one field on Settings >
  Shopify, which is where a value outside 1 minute to 24 hours is refused.
- **F9's fix** is in the generator: the fetch document selects the first hundred lines
  with no page info, so the model is regenerated.
- **The scope list is also stated in `docs/public/operate/shopify-connect.md`**, and a
  test holds that sentence to the list. Both move together.
- **What a store approves is what Connect asks for.** The app's configuration at Shopify
  uses the legacy install flow, so the list in the authorization request decides: on
  2026-11-11 a store was offered "Edit products" although the app's own version listed
  only read access. The app version's list is kept in line for the record, and its
  Webhooks API version is the pinned one.

## 14. Not in this version

- The statistics screen for a storefront's purchases. It has its own record and reads
  what this one records.
- Orders older than 60 days (`read_all_orders`).
- Mirroring content, themes, staff, payouts, gift cards or markets through a managed
  connection.
- Telling apart the orders of two destinations bound to the same store.
- Counts on the Store page. The statistics screen is where numbers go.
- Any model or agent.
