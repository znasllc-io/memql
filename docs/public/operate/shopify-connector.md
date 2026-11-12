---
title: The Shopify connector
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# The Shopify connector

A complete, generated mirror of a Shopify store, the origins a wholesale
business needs on top of it, and the boundary of what nobody can mirror.

This page is the operator's view: what to create in Shopify, what to enter in
MemQL, what happens next, and what to look at when it stops happening. The
design record is
[the connector design](../../superpowers/specs/2026-08-22-shopify-connector-complete-mirror-design.md);
the storefront's side is
[the storefront completeness checklist](shopify-storefront-checklist.md),
whose sections 6 and 7 are the SERVING half: which hosts this cluster's edge
admits for a storefront, where that list comes from, and why a storefront
bundle may not bake store data in at build time.

---

## What it is

Shopify owns the store's data. MemQL holds a **mirror** of it: 65 concepts
under `v1:shopify:*`, generated from the Admin GraphQL schema at a pinned
version, kept current by webhooks and repaired by reconciliation. Nothing in
the mirror is authored in MemQL and nothing writes to it but the connector.

Alongside the mirror, `v1:commerce:*` carries the things Shopify does not
model at all -- quotes, approval chains, credit limits, reps and territories,
reorder lists. MemQL is the **origin** of those, and a closed set of them is
pushed back into Shopify as metafields.

Three properties are worth knowing before anything else:

- **The model is generated.** `cmd/shopifyschema` reads Shopify's schema and
  emits the concepts, their reads and the GraphQL documents. Nothing under
  `dsl/shopify/generated/` or `integrations/shopify/generated/` is edited by
  hand; a hand edit fails the build and would be lost at the next
  regeneration anyway. No mirror WRITE is generated: the connector returns
  MirrorWrites and the data-origins runtime performs the write, behind the
  version guard and under the connector actor.
- **A webhook is a trigger, not a payload.** A delivery tells the connector
  which object changed. The object itself is then read back through the Admin
  API with the generated selection set. Webhook payloads lose fields,
  truncate, arrive out of order and are not guaranteed; the API does not.
- **Reconciliation is a requirement.** Shopify states that webhook delivery
  is not guaranteed. Every domain is therefore re-checked on a cadence, and
  the rows a pass finds different from the mirror are written and counted as
  *drift* -- which is the only evidence anyone gets that deliveries are being
  lost. A row the mirror already holds is read, compared and left alone, so a
  quiet store's passes write nothing.

---

## Step 1 -- inventory the store's apps

Do this first, before creating anything. Walk the store's installed apps and
write down what each one does, because the mirror will not carry any of them.

App-owned metafields lost public access on 2025-05-19 ("No other apps have
access"), and data an app keeps in its own database never reaches Shopify at
all. A reviews app, a bundle builder, a wishlist, a page builder, a form
tool -- their data lives with them.

"Lose no function" is met by **integrating those apps in the storefront
through their own APIs and embeds**, not by mirroring them. Each app gets a
line in the storefront checklist: an integration plan, or an explicit "not
carried". An app nobody wrote a line for is the one that goes missing on
launch day.

---

## Step 2 -- create the app

A **custom-distribution app**, created in the Shopify Dev Dashboard.
Admin-created custom apps cannot be created since 2026-01-01, so the Dev
Dashboard is the only route.

Request these scopes. The list is derived from the allowlist, so the
authoritative copy is `generated.Scopes` in
`integrations/shopify/generated/model.go` and the console shows the store's
grant against it:

```
read_all_orders                             read_locales
read_assigned_fulfillment_orders            read_locations
read_content                                read_markets
read_customer_payment_methods               read_marketing_events
read_customers                              read_merchant_managed_fulfillment_orders
read_discounts                              read_metaobject_definitions
read_draft_orders                           read_metaobjects
read_fulfillments                           read_online_store_navigation
read_gift_cards                             read_online_store_pages
read_inventory                              read_orders
read_legal_policies                         read_own_subscription_contracts
read_payment_terms                          read_returns
read_product_feeds                          read_shipping
read_products                               read_shopify_payments_disputes
read_publications                           read_shopify_payments_payouts
read_store_credit_accounts                  read_themes
read_users
```

Three of them need more than a checkbox:

- **`read_all_orders`** unlocks orders older than 60 days. It needs Shopify's
  approval, requested from the app's configuration. Without it the mirror's
  order history begins 60 days before the store connected. When a connect or
  the boot pass queues the store's orders, its health records that day as
  `ordersFrom`, and the Store page shows it as "Orders since"; a store whose
  orders were queued no other way has none.
- **Protected customer data** is a separate approval with two levels. Level 1
  covers customer records; Level 2 covers the fields marked as protected
  (name, email, phone, address). Below the level a field needs, Shopify
  returns `null` and the mirror stores `null` -- so the store row records the
  approved level and the console shows it, because "the customer has no phone
  number" and "we are not approved to see it" look identical in the data.
- **`read_reports`** plus Level 2 is what the `shopifyql` pass-through needs.
  The connector refuses below that with a message naming the level; Shopify
  would answer 403 naming nothing.

Also install the **Headless channel** on the store and take its Storefront
token. That is what the headless storefront uses, and it is a different
credential from the Admin token.

**Token expiry.** Offline access tokens for public apps become expiring from
2027-01-01. A custom-distribution app's token does not expire today; the
connector stores every token by reference so a rotation is a secret change
rather than a code change.

### What a managed connection asks for

The list above is for a store connected with its own app. A store connected
through Connect Shopify with the shared app -- a *managed* connection -- asks
for a smaller, fixed set, and everything about that store is measured against
this set rather than the list above: a scope it does not ask for is never
missing, and a domain it does not reach is never an error.

| Scope | What it is used for |
|---|---|
| `unauthenticated_read_product_listings`, `unauthenticated_read_checkouts`, `unauthenticated_write_checkouts`, `unauthenticated_read_customers` | The storefront: reading its catalog and holding a cart. Without them no Storefront token can be minted. |
| `write_products` | Products, variants and collections, and the `memql` metafields product-content delivery writes. It includes `read_products`. |
| `read_inventory` | Inventory items and levels, inventory transfers and inventory shipments. |
| `read_locations` | Locations. |
| `read_orders` | Orders with their lines, transactions and refunds; abandoned checkouts; payments. |
| `read_customers` | Customers, customer segments, companies, company locations and their contacts. |
| `write_customers` | Only the wholesale pack's customer tag and creating a customer. It is asked for now so that a store approves once rather than twice. |
| `read_discounts` | Discounts. |
| `read_returns` | Returns. |

`read_all_orders` is not asked for, so a managed store's orders begin 60 days
before it connected (see `ordersFrom`, above). The authoritative copy of the list
is `managedConnectScopes` in `integrations/shopify/connections.go`, and
[Connect Shopify](shopify-connect.md) prints it for registering on the app.

### The data declaration

Shopify treats orders, customers, draft orders, abandoned checkouts, refunds and
transactions as protected customer data. Until the app declares that it uses
it, Shopify refuses those domains, and the store's Store page says so, naming
them: "Shopify refuses orders for this app, because the app has not declared
the customer data it uses." MemQL cannot make the declaration. An operator
makes it once, for the app:

1. Open the app in the Shopify Dev Dashboard (or the Partner Dashboard, for an
   app created there) and find its protected customer data settings. The Store
   page's act, "Declare the customer data", links to Shopify's own page on it.
2. Request access to protected customer data.
3. Select the fields **name**, **email** and **address**. Do not select
   **phone**: nothing in MemQL uses a phone number, and Shopify approves only
   what an app needs.
4. For a **development store**, that is all: the selection needs no review.
5. For a **merchant's live store**, Shopify reviews the declaration before the
   app may read that store's orders, and the wait is Shopify's. Submit the
   request as soon as the declaration is made, well before the store is
   connected, so the wait runs while everything else is set up.

Declaring name, email and address is Shopify's Level 2, and its requirements
(encryption, retention, test data kept apart, an incident procedure) are the
operator's. The privacy topics already redact on request
([Step 5 -- the compliance topics](#step-5----the-compliance-topics)).

Nothing on the cluster changes. Each refused domain is asked again on its own
sweep interval, and the first clean sweep of a domain Shopify had refused
queues that domain's backfill, so the store's past arrives with nobody
pressing Backfill.

### When the list grows

A release can add scopes to what a managed connection asks for -- this one
added `read_customers`, `write_customers`, `read_discounts` and `read_returns`.
Then:

1. **Register the new scopes on the app.** Add them to the app's access scopes
   in its configuration at Shopify, beside the ones it already has.
2. **Each store approves again.** A store connected earlier approved the older
   list. Its Store page says "This store approved an older list of
   permissions.", with the scopes it lacks beneath, and -- for the store's
   owner or a cluster owner -- the act "Open MemQL OS from the store's Shopify
   admin and approve", which links to the store's apps page in its Shopify
   admin (`https://example-store.myshopify.com/admin/apps`). Somebody on the
   store's staff opens MemQL OS from there and approves the new list when
   Shopify asks.
3. **The rest is automatic.** The approval returns through the connect
   callback, which records the new grant and queues the backfill of each
   domain the new grant unlocked. The store reads "Starting" while those run,
   then "Recording".

---

## Step 3 -- attach the store to its storefront

**Connect Shopify does this step, in two places.**

1. **Connect the store.** Open **Settings → Connections → Shopify** (or
   **Deployables → Settings → Shopify**), press "Add Shopify store", then
   "Continue to Shopify". The shop's staff choose the store and approve on
   Shopify, and MemQL seals the credentials, writes the store row, mints the
   Storefront token and registers the webhooks. The store then reads
   "Connected" in the list of Shopify stores.
2. **Choose it for a destination.** Open the storefront's **Store** page and,
   under "Connections", the **Testing** or **Production** row. Pick one of your
   connected stores under "Which Shopify store" -- the bar reads "Choose a
   store" until you do -- press "Continue", check it under "Review", and press
   "Connect store". "Manage Shopify connections in Settings" goes back to the
   first place.

The whole flow, and every result it can come back with, is
[Connect Shopify](shopify-connect.md). The Store page registers no store and
seals nothing: it chooses among stores already connected.

A store whose credentials were sealed some other way -- the environment
seed's, below -- is registered by a cluster owner with `createStore`, which
takes:

| Argument | What it is |
|---|---|
| `storeId`, `domain` | The store's id, and `acme-widgets.myshopify.com`: the one identifier Shopify never changes. |
| `appClientId` | From the Dev Dashboard. Recorded for the audit trail. |
| `adminTokenRef` | The name of a `v1:platform:globalSecret` row holding the Admin API token. |
| `storefrontTokenRef` | Same, for the Headless channel's Storefront token. |
| `webhookSecretRef` | Same, for the app's webhook signing secret. |
| `apiVersion` | Must match the version the mirror was generated from -- see "The quarterly bump". |
| `protectedDataLevel` | `none`, `level1` or `level2`, as approved. |
| `ownerUserId` | The MemQL user a `customers/data_request` export is filed under, and who is emailed when the store stops being recorded. |

**The three token fields are REFERENCES, not tokens.** The store row is read
by the console and returned to a browser; a token on it would be a token on a
screen. No screen seals a token by hand: Connect Shopify seals them on the
server, and the environment seed below does for a cluster's first store. Name a
row one of them wrote.

**A store names its own Storefront token, and the edge publishes only that**
(memql#5626). A storefront's runtime document carries the Storefront token to
every visitor, so the token must be the store's OWN:
`SHOPIFY_<STOREID>_STOREFRONT_TOKEN` (the id upper-cased, hyphens kept) -- the
name Connect, a pasted token and the seed all seal it under. Three places hold
that line:

- **The write.** `createStore` and `updateStore` refuse a `storefrontTokenRef`
  naming any other secret, with a sentence naming the expected one. An empty
  reference is a store whose token is not set yet, and clearing is allowed.
  A new store's id must be one Connect derives -- lower-case letters, digits,
  `_` and `-`, starting with a letter or digit, at most 56 characters -- since
  `Acme` beside `acme` would share one secret name.
- **The edge.** A reference naming anything else -- the store's Admin token,
  another store's token, any other cluster secret -- is never looked up: the
  storefront reads `connectionState: "unavailable"`. A store written before
  the write rule keeps working as a row; the edge logs a warning when it
  resolves it, once an hour per site and store, naming the reference, the
  expected name and the repair: connect the store again in Settings, which
  seals the token under the right name. The Store page says so for such a
  store -- its Storefront token "is registered under a different name" --
  with "Reconnect in Settings".
- **The console.** `sitePreviewReadiness` reports `storeHasStorefrontToken`
  and `previewStoreHasStorefrontToken`: whether each destination's store names
  its own token. That, not a non-empty reference, is "connected".

The name is the check because nothing else marks a Storefront token: every
Shopify secret, the Admin token included, is sealed with kind `vendor_api_key`.

**A store row and a site binding are one record.** The storefront's binding
NAMES this row -- `binding: {storeId}` on `v1:platform:site` -- and does not
copy it. The edge resolves the myshopify.com domain and the Storefront token
reference through the store at serve time, so an edit here reaches every
storefront bound to it with no second write, and there is no second place to
keep in step. It used to carry its own `{storeDomain, storefrontTokenRef}`,
which meant one store was recorded twice, edited in two places, at two
authorization tiers.

### Who may read and change a store row

`v1:shopify:store` is `@rowAuthz(clusterOwner, rankFloor="developer")`
(design decision D3):

| Act | Who |
|---|---|
| read a store row (`storeById`, `storeByDomain`, `stores`, `developmentStoresFor`) | developer, cluster owner |
| register a store (`createStore`) | cluster owner, or server code (Connect Shopify) |
| change a store that exists (`updateStore`, or `setStoreStatus` called directly) | cluster owner |
| pause or resume recording a store (`shopifyStoreSetRecording`, which the Store page's "Pause recording" and "Resume" call) | the store's owner, cluster owner |
| attach a store to a storefront, or change which one it fronts (`updateSiteStoreBinding`) | developer, cluster owner: the store part, a store they can read, and a storefront they can write |

`shopifyStoreSetRecording` pauses a store that is being recorded -- whatever
its status says, `configured`, `backfilling` or `live` -- and resumes a paused
one to `live`. It writes through `setStoreStatus` as the deployment, so the
row's own gate is not widened, and records each pause and each resume in the
audit log (`shopify_store_paused`, `shopify_store_resumed`) under the person
who did it. Details are in [Pausing a store](#pausing-a-store).

**Who may call the store's builtins.** Each one that names a store asks who is
calling before it reads anything but the store row, and before it asks
Shopify. A store that exists and one that does not answer a refused caller
alike. A person names the store (`storeId`) for the verdict, its acts and the
wholesale builtins, and is refused with "shopify: <builtin> needs a storeId"
without one. The mirror reads, `shopifyql` and `storefrontProbe` may leave it
out: a cluster owner then gets the cluster's only store, and anybody else the
one store they own -- refused when they own none, and asked to name one when
they own several.

| Builtin | Who |
|---|---|
| `shopifyStoreVerdict` | the store's owner, a cluster owner, or a caller who may read store rows |
| `shopifyStoreRegisterEvents`, `shopifyStoreRetryDeliveries`, `shopifyStoreSetRecording` | the store's owner, cluster owner |
| `shopifyFetchProduct`, `commerceSold`, `commerceStock`, `commerceCustomers`, `commerceCompany` | the store's owner, cluster owner; anybody else is refused before the mirror is read |
| `shopifyql` | the store's owner, cluster owner; anybody else is refused before Shopify is asked |
| `storefrontProbe` | the store's owner, a cluster owner, or a caller who may read store rows |
| `shopifyStoreHealth` | answers every store for a cluster owner or a caller who may read store rows, their own store for a store's owner, and an empty list -- not a refusal -- for anybody else |
| `shopifyEnsureSubscriptions`, `shopifyRunComplianceJobs` | their scheduled automations, or a cluster owner running them by hand |
| `shopifyStoreAlertWaitSet` | cluster owner |
| `shopifyProvisionWholesale`, `shopifyRevokeWholesale`, `shopifyTagWholesaleCustomer`, `shopifyUntagWholesaleCustomer`, `shopifyWholesaleCatalogHeadroom` (the wholesale pack's calls) | the store's owner, cluster owner, or a step of a registered automation; anybody else is refused with "shopify: <builtin> is for the store's owner or a cluster owner" before Shopify is asked. A wholesale application approved by somebody who is neither keeps its entitlement `pending`, with that sentence as its error. |
| `datasyncStartBackfill`, `datasyncSetSyncPaused`, `datasyncRequeueInbound`, `datasyncRetryOutboxEntry`, `datasyncDiscardOutboxEntry` (the acts of [Cluster > Data](#cluster--data)), and `datasyncReturnHeldInbound` | cluster owner, or the engine itself; anybody else is refused with "datasync: <name> is for a cluster owner" |

`shopifyStoreRetryDeliveries` -- the Store page's "Try again" -- puts the
store's given-up deliveries back in line by making that requeue itself, as
the engine, so it works for the store's owner who is not a cluster owner.
Resuming a store calls `datasyncReturnHeldInbound` the same way
([Pausing a store](#pausing-a-store)).

Below developer a store read answers **zero rows, not an error**: the binding
guard and the deploy path read "not readable" off an empty result. The read
floor widens reads only, so a developer who can see a store still cannot
change it.

**Binding is a developer's or a cluster owner's act, and the rule is narrow.**
Attaching a store needs `execute` on `app:deployables/store`, which the owner
and developer roles hold; beside it, the engine refuses a binding that names a
store the caller cannot read. Binding a storefront to a store you may not read
would publish that store's Storefront token under that storefront's hostname.
Reading is not enough on its own: CHANGING which store a site is bound to -- a
create that binds, a rewrite that re-points, `updateSiteStoreBinding` -- also
needs the store part, and so does changing the store a preview binding names,
through `updateSitePreviewBinding` or a rewrite that re-points it. So an admin,
below the read floor, binds no store even when granted the part, and a
developer an owner has denied the part by name binds none either, on either
binding.

**The consequence, stated (D3).** A developer may bind ANY storefront they can
write to ANY store on the cluster, and that is not only the storefronts they
own. `v1:platform:site`'s account grant admits staff (developer and above) to
write every account-tied site, so every client storefront -- live ones
included -- can be re-pointed at any store by any developer holding the store
part. That is the same reach that already lets a developer pause, archive or
delete those sites. Every store row is registered by a cluster owner or by
server code (D15), so the token references a binding exposes are ones an
owner or Connect chose. An owner who wants one developer kept out denies them
`execute app:deployables/store` by name.

**What the Store page draws, by who you are.** The Store slot and the Store
page are drawn only for somebody holding `execute app:deployables/store`.
Among them:

- **Anybody holding the part** can open a destination's row and choose one of
  their own connected stores for it -- among those they may read.
- **A developer** (who may read store rows) also sees each bound store's state
  and reasons, but no act on them.
- **The store's owner and a cluster owner** also see each reason's act and the
  bar's "Pause recording" or "Resume".

The page draws no register form and no subscription reconcile for anybody: a
store is registered by connecting it in Settings, and a store's subscriptions
are registered again by "Register again", on that one store, when its verdict
asks for it.

### The environment seed

A cluster with no store row and these variables set seeds its first store at
boot:

```
MEMQL_SHOPIFY_STORE_DOMAIN         acme-widgets.myshopify.com
MEMQL_SHOPIFY_ADMIN_TOKEN          shpat_...
MEMQL_SHOPIFY_STOREFRONT_TOKEN     ...
MEMQL_SHOPIFY_WEBHOOK_SECRET       ...
MEMQL_SHOPIFY_API_VERSION          2026-07
MEMQL_SHOPIFY_APP_CLIENT_ID        ...
MEMQL_SHOPIFY_PROTECTED_DATA_LEVEL none | level1 | level2
```

The tokens are sealed into `globalSecret` rows and the store row references
them; the variables are then never read again. **Editing them later changes
nothing** -- the row is the configuration. That is deliberate: an env var that
silently overrode a row would make the console's view of a store a lie, and
would do it only on the nodes carrying the variable.

---

### The development store

A storefront is exercised against a **development store** before it serves
shoppers. It is connected exactly like any other store -- a second
`v1:shopify:store` row -- and mirrored like any other, so an order placed
against it lands in the mirror under that store's own `storeId` and is already
excluded from every read scoped to the live one.

Connect it in Settings with "Add Shopify store", as in
[Step 3](#step-3----attach-the-store-to-its-storefront). Connect Shopify asks
Shopify for the store's plan and records a development store as one
(`isDevelopment`); nobody marks it by hand. On the storefront's Store page,
choose it for the **Testing** row: the list of stores calls it "Sandbox
store", and the Store slot adds "Sandbox". The Store page does not record
which live store it stands in for; `developmentOfStoreId` is set only by a
cluster owner, through `createStore` or `updateStore`.

It is not a store this program provisions, and specifically not a Shopify store
created with generated test data -- such a store cannot be transferred to a
merchant.

---

## Step 4 -- what happens next

**Subscriptions.** On boot and daily at 03:15, the connector registers every
mirrored topic the store's grant covers, for every ingesting store: HTTPS
delivery to `https://api.<your-domain>/inbound/shopify-<storeId>`, with
`includeFields` trimmed to `id`, `admin_graphql_api_id` and `updated_at`. It
updates subscriptions whose URL or fields have drifted, and removes its own
that the allowlist or the grant no longer wants. Another app's subscriptions
are not visible to this app and are never touched.

**The version events are serialized at is the app's, not the subscription's.**
Shopify takes it from the app's configuration (its Webhooks API version) and a
subscription inherits it; a create or an update cannot name it, and Shopify
refuses the whole call if one tries. Set it to the pinned version in the app's
configuration at Shopify. When the two differ, the store's health carries the
version Shopify delivers at as `subscriptions.deliveredAtVersion`. The payload
is only a signal, since the object is fetched at the pinned version, so a
difference loses nothing by itself; a topic the app's version does not have
fails its creation and is listed under `subscriptions.failed`.

A topic is outside the grant when the store holds none of the scopes its
domain reads under -- the rule reconciliation uses to skip a domain. A store
connected through Connect Shopify holds the managed scopes
([What a managed connection asks for](#what-a-managed-connection-asks-for)),
so the topics of domains none of them reads -- fulfillment orders, gift cards,
draft orders and most of the rest -- are not asked for; Shopify would refuse
each one. They are listed on the store's health as
`subscriptions.notGranted`, a standing fact about the connection rather than a
failure. A store whose grant was never recorded is asked for every topic.

The daily pass is not tidiness. Shopify retries a failed delivery eight times
over four hours and then **deletes the subscription** after eight consecutive
failures. A deployment that was unreachable for an afternoon comes back with
its webhooks silently gone: the mirror simply stops changing, and nothing
anywhere says so. The daily reconcile is what brings them back, and the
store's health is where you see that it happened.

**Backfill.** Each domain's initial load is a Bulk Operation: Shopify runs the
query, writes a JSONL file, and the connector streams it in resumable
batches, persisting the line offset as the domain's cursor. Bulk queries are
exempt from the cost limit, which is why a hundred thousand orders are
affordable at all.

**A backfill starts by itself.** Nobody has to press anything for a store's
past to arrive. A domain's backfill is QUEUED:

- by a connect. A store that is new, was uninstalled, had its data removed by
  Shopify, or had no Admin token queues every bulk domain of its connection,
  and a backfill left running from before starts again from nothing. Any other
  connect queues only the domains the new grant unlocked.
- at boot, for a domain of an ingesting store that no backfill ever wrote.
- by the first clean sweep of a domain Shopify had refused (a wider grant, a
  data declaration made since).

The `continueQueuedBackfills` automation then carries every queued or running
backfill on. It starts **every five minutes** and works in rounds, starting
rounds for **four minutes**; a round under way when they are up finishes, so a
pass can run past four minutes. In each round every store with work left takes
one step, and when a domain is done the store's next queued domain starts in the next
round. A round in which every store only waited for Shopify to prepare an
export is followed by a ten-second pause. A store still runs one backfill at a
time, a store does not wait for another to finish, and a paused domain is left
alone. A domain whose step failed is left for the rest of that pass. While any
of a store's backfills is queued or running, its Store page reads "Starting":
for a store with little in it, a few minutes; for a large history, as long as
Shopify takes to prepare its exports and the passes to read them.

**When an export fails.** A history download is an export Shopify prepares,
and its link lasts a week. The link is never stored: the domain's row keeps
the export and the line reached, and each step asks Shopify for the link
again. When Shopify gives no link for an export that counted objects, refuses
the download, or no longer knows the export, the backfill starts that export
again by itself, from its first line. An export that counted none and gives no
link is a domain with nothing in it, and the domain's backfill is done. An export Shopify reports as failed, cancelled or expired is started
again the same way. All count toward one limit: **two restarts per backfill**. The next failure
after that leaves the backfill failed, and the Store page says "The last check
of orders failed" (naming the domain), with the reason beneath it.

A backfill can also be started by hand: "Backfill now" on that store's domain
in [Cluster > Data](#cluster--data). On a domain whose backfill failed it
starts again from the start; on any other it carries on from where it
stopped. It is refused while that backfill is running. The same call is
`datasyncStartBackfill(connector: "shopify", conceptId: "v1:shopify:order", partitionKey: "<storeId>")`,
with `restart: true` to start from the start. While a restarted backfill
runs, the Store page reads "Starting".

Its reply's `outcome` says what became of it. `waiting` leaves the backfill as
it was -- queued, or running at its cursor -- for the continuing pass, and its
detail says why: for a paused or
uninstalled store, `store "<storeId>" does not ingest now, so its mirror waits
for it`, and the pass carries it on once the store is recorded again.
`abandoned` means the domain's row was queued again, or another backfill of it
holds the row. A call whose progress could not be written fails with
`datasync: the backfill's progress could not be recorded`, and the next pass
continues from the last line that was recorded.

**Where a store's orders begin.** Without `read_all_orders`, Shopify lists
only a store's last 60 days of orders. When a connect or the boot pass queues
a store's orders, the day 60 days before then is recorded on its health as
`ordersFrom`, once. A store whose orders were queued another way, or before
this was recorded, has none, and its Store page shows no "Orders since". A
later connect does not move it. A store whose data Shopify removed is the
exception -- its mirrored orders are gone, so reconnecting it records the day
anew.

Domains are applied **parents first** (`generated.ApplyOrder`), so a line
item never lands before its order. An order with more than a hundred lines
is read whole: the lines after the first hundred are read a page at a time,
up to fifty pages in all, before the order is written. An order still
promising more lines at that ceiling is kept as far as it was read, and the
node's log says so. Its returns are read the same way. Lists kept on the order as a field
are read one page deep: an order's discount applications, shipping lines and
fulfillment orders past the first 25, and its metafields past the first 50,
are not mirrored.

**Live delivery.** A webhook arrives at the inbound receiver, is verified
against the store's own secret, and is staged. The runtime's dispatcher
automation hands the staged row to the connector, which dedupes on the
webhook id, fetches the object by GID with the generated selection set, and
returns the rows; the runtime applies them under the version guard, so a
write whose `updatedAt` is older than the stored row's is recorded as *stale*
and dropped.

**Changes are kept in order: two questions, two clocks.**

- **Which of two versions of a record is newer** is decided by Shopify's own
  last-changed time for it (`updatedAt`; a child with none of its own, such as
  an order's line, takes its parent's). A write older than the record held is
  refused on every path -- a delivery, the periodic check, a full re-list and
  a history download alike -- so an older snapshot that arrives late cannot
  replace a newer change. For a kind Shopify publishes no version for, the
  later read wins.
- **Whether a record still exists** is decided by when Shopify was last read
  for it, never by its last-changed time, which does not move when a record
  is deleted. A deletion stands against every read made before it, and a
  later read that finds the record brings it back. A history download counts
  as read when its export was started, so an export started before a deletion
  does not bring the deleted record back.

**How a deletion is believed.**

- **A deletion Shopify announces** (a delete notice) is believed at once.
  It is undone only by a read of the record made more than 60 seconds after
  the notice was received; the 60 seconds allow for the clocks of two nodes.
- **A deletion Shopify does not announce** -- a notice about a record that
  Shopify, when asked, no longer has -- is believed only when Shopify answers
  nothing for it **twice in a row** for the same notice. The first empty
  answer retires nothing: the delivery fails with `connector: the origin
  answered nothing for this record; it is asked once more before that answer
  is believed`, to be asked again on the delivery retry's waits -- one minute
  after the first try, five after the second, thirty after the third, so at
  most thirty minutes between two tries. If the tries run out without two
  empty answers in a row, the delivery is given up and the Store page says "1
  change could not be recorded."; its "Try again" asks again, and the count of
  empty answers starts again.
- **Order data is never retired by an empty answer on a store without
  `read_all_orders`** -- every store connected through the shared app. That
  is an order and everything read with it: its lines, transactions, refunds,
  fulfilments, returns, reverse fulfilment orders and their deliveries, and
  abandoned checkouts and payments. Shopify hides such a record from the app
  once it is older than sixty days and answers nothing for it, so an empty
  answer writes nothing and is not asked again. Only its delete notice retires
  it ([What the mirror cannot do](#what-the-mirror-cannot-do)).
- **A record the mirror does not hold yet** is asked once more too: a notice
  can arrive before its record is readable. The second empty answer writes
  nothing, and the record arrives with the next read that finds it. On the
  delivery's fourth and last try the first empty answer is taken as it is:
  the delivery is processed with nothing written, and is not counted among
  the changes that could not be recorded.
- **A record the mirror already holds as deleted** is left as it is: an empty
  answer for it writes nothing and is not asked again.
- **Nothing else retires a record.** An answer with any error, a refusal of
  protected customer data, and a partial answer (Shopify refused part of what
  was asked, and what is left has no record) are not deletions.
- **A record wrongly seen as gone** comes back on the next read that finds it.

**A failed delivery is tried again.** A delivery the connector could not work
-- the Admin API did not answer, the write failed -- gets **four tries at
most**: the first, then one minute after it, five minutes after the second,
and thirty minutes after the third. A refusal no retry changes -- a scope the
grant lacks, a domain the connection does not ask for -- is not tried again. A
delivery that used all four tries is *given up*: it stays `failed` and nothing
tries it again by itself. The domain's next clean sweep reads what it named,
and from then on it no longer counts against the store. Until then the Store
page says "3 changes could not be recorded." (or "1 change could not be
recorded."), and its act, "Try again", puts every given-up delivery of that
store back in line, with its tries counted from none. The retry pass runs
every minute, so they are tried within a minute. A delivery of a paused store
is *held* instead, and a delivery that arrived while the connector could not
read its list of stores fails with `shopify: could not tell which store a
delivery on source "..." belongs to; it is tried again` and is tried again
like any other. The full rules are in
[Inbound delivery](inbound-delivery.md#when-a-connectors-try-fails).

**Reconciliation.** Every ten minutes the runtime sweeps the domains whose
own interval has elapsed. The sweep runs on the cluster's schedule leader, and
one that is still running there when the next is due is not overlapped; while
the lead changes hands two nodes can each run one, and each write of a
domain's row is fenced on the version of the row it read, so neither writes
over what the other recorded. Orders, customers, products, product variants,
collections, companies, company locations, locations, inventory items, draft
orders and subscription contracts are paged by `updated_at:>`, the time each
last changed. The other swept kinds -- among them discounts, abandoned
checkouts, payments, customer segments, gift cards, price lists and catalogs,
markets, menus, pages, blogs and articles, themes, publications, redirects,
carrier and delivery settings, inventory transfers and shipments, disputes,
staff and marketing -- are re-listed whole on their own cadence and rows the
origin no longer returns are tombstoned. A kind read with its parent, such as
an order's lines, refunds, fulfilments and returns, is not swept on its own.
Metaobjects and metafield definitions are not swept at all:
`metaobjects(type:)` and `metafieldDefinitions(ownerType:)` each require an
argument a generic listing cannot supply, so those two domains are kept
current by their webhooks alone (listing them per type / per owner type is
future work).

A sweep in which a row could not be written is not a clean sweep: the rest of
the pass still runs, the domain's last error reads `shopify: 1 row read from
the store could not be written to the mirror; the next check reads it again`
(or "N rows ... reads them again"), and the next sweep starts from where this
one did. That next sweep comes at the domain's own interval -- an hour, six or
twenty-four -- not at the next ten-minute tick, and the store's notice says
when ("The next check is at" a time).

A full re-list that hits its page cap tombstones **nothing**. Tombstoning is
an argument from absence, and the argument is only valid if the walk was
complete.

So a record is marked deleted three ways: by a delete notice; by a notice
whose record Shopify answers nothing for twice in a row, except order data on
a store without `read_all_orders` (see "How a deletion is believed", above);
and by a complete full re-list that no longer lists it
-- never from a partial listing such as an `updated_at:>` page. A record read
after the re-list began is not marked deleted by it.
When the mirror holds more of a domain than one pass reads back, its oldest
records are kept that pass. Some kinds are never marked deleted by a re-list
at all, because their listing is not the whole set: fulfillment orders (open
ones only), abandoned checkouts and payments (recent order data only, without
`read_all_orders`), and selling plan groups, marketing activities and
marketing events (possibly only those this app created). A deletion reaches
those through a delete topic, or not at all.

---

## Step 5 -- the compliance topics

Three topics are declared in the app's configuration rather than created
through the API, because they are not members of `WebhookSubscriptionTopic`:

```
customers/data_request
customers/redact
shop/redact
```

Point all three at the **app-level** endpoint
`https://api.<your-domain>/inbound/shopify` -- the connector's own name,
with no store id -- and not at a store's `/inbound/shopify-<storeId>`. The
reason is when Shopify sends them: for a shop that never finished
installing, for a shop that uninstalled 48 hours ago, and before the first
store has connected at all. A per-store URL exists only while that store
row does; the app-level one exists as soon as the Connect flow has sealed
the app's client id and secret (`SHOPIFY_CONNECT_CLIENT_ID` /
`SHOPIFY_CONNECT_CLIENT_SECRET`), and every delivery to it is verified
against that secret. A bad HMAC on any of them answers **401**, which is
Shopify's requirement.

Because one secret covers every store, neither the URL nor a header can say
which store a privacy request is about. The connector reads the **signed**
`shop_domain` in the body, refuses a delivery whose `X-Shopify-Shop-Domain`
header disagrees with it, and queues a job only for a store this app
installed (`appClientId`); any other shop is dropped without a job. A
privacy delivery on a per-store URL is held to the same body check, and a
**paused** store still queues its privacy jobs -- a pause stops the mirror,
not the merchant's obligations.

**What each one does.** The three arrive as ordinary webhooks, and each is
turned into a `v1:shopify:complianceJob` row -- the connector's own queue,
deliberately not the outbox: the outbox's drain hands every entry to
`Propagate`, and a privacy job handed to `Propagate` would try to write a
customer's export into Shopify. The job rows are a cluster owner's to read;
each says where its request stands (`status`: `pending`, `done` or `failed`),
what its last run did (`outcome`) and, after a failed run, what failed
(`lastError`, one sentence that names no customer).

- **`customers/data_request`** is run at the next run of the privacy jobs. It
  collects the rows an erasure of the same customer reaches (below), each as
  its newest version, and files them as one JSON document in the Library of
  the store's owner, with Shopify's request id. What an erasure scrubs beyond
  those rows -- older versions of rows that moved to somebody else, a deleted
  customer's record scrubbed by itself, an entry taken out of a metaobject --
  is not in it. The document is the owner's, and only the owner can read it.
  Audited as `shopify_data_request_exported`. Three cases come first, in this
  order:
  - **the customer has been erased** in that store (her erasure has made its
    first pass): nothing is filed, any export of hers still in the owner's
    Library is removed, the request's deliveries are deleted, and the job ends
    with "the customer has been erased, so nothing is filed";
  - **the request was answered before**: the same request delivered again
    files nothing again;
  - **the store's row is gone**: the job ends with "the store is no longer
    configured; nothing to export".

  A store with no owner **fails** the request, and says so before a row of the
  person is read, rather than filing one merchant's customer data in a
  stranger's library.
- **`customers/redact`** is due a day after it arrives, so a merchant's own
  grace period applies, and scrubs the customer from the store's mirror, by
  the store's id even when the store's row is gone. Audited as
  `shopify_customer_redacted`, and its second pass as
  `shopify_customer_redacted_second_pass`.
- **`shop/redact`** is due 48 hours after it arrives, and purges the store:
  every mirror row of the store, its sync state and every staged delivery of
  its own source, every version, then pauses the store row and stamps
  `redactedAt`. Audited as `shopify_shop_redacted` (its second pass
  `shopify_shop_redacted_second_pass`), or, when the store answered,
  `shopify_shop_redact_skipped` (on the second pass
  `shopify_shop_redact_second_pass_skipped`). A purged store is served as unavailable, like an
  uninstalled one, until a verified reinstall clears it (see
  [app/uninstalled](#appuninstalled)).

**What a customer's erasure covers.** The customer is matched by her id as a
whole value, never as the start of a longer one, and only in the fields that
say whose a row is:

- **Her own customer record is emptied**: every field but its structure (the
  store, the account the row belongs to, the id, when the row changed and was
  read, whether it is deleted). Free text reads `[redacted]`, a list is empty,
  and anything else -- an address, a count, a date -- loses its value, so a
  typed read answers null.
- **The rows that are hers**: her orders (an order whose newest version names
  her as its customer or its buyer, and the orders the request names), her
  abandoned checkouts and draft orders, her own company contact record, her
  payment methods, store credit accounts and subscription contracts, and the
  gift cards she holds or was sent.
- **Everything under those orders**: their line items, transactions,
  fulfilments and fulfilment orders, refunds, returns with their reverse
  fulfilment orders and deliveries, payments and disputes.
- In each of those rows **what describes the sale is kept and what describes
  the person goes**. Amounts and currencies, dates, quantities, statuses, the
  order's number, product and variant titles and SKUs, tax lines and discount
  codes stay, so the merchant's books still add up. Names, email addresses,
  phone numbers, addresses, notes, custom attributes, the shopper's IP address,
  payment details, tracking, metafields and her consent to marketing as the
  order recorded it go. A **custom line item** -- one with no product -- loses
  its name, its title, its variant title and its SKU as well, because a person
  typed them.
- **A company is not emptied because one of its contacts was erased**: her own
  contact record is scrubbed, and the company, its other contacts and its
  locations stay as they were. A merchant's metaobject that refers to her
  loses the entry that names her and keeps everything else.
- **A row is decided on what it is now.** An order that has since moved to
  another customer -- including one the request names -- is that customer's:
  it keeps its current state and everything under it, loses only its older
  versions from when it was hers, and is not in her export. A deleted customer
  record whose last order is hers is scrubbed by itself, and nothing is reached
  from it.

Every version of every row in scope is rewritten. The precise statement,
concept by concept and field by field, with the reason for each field that
is kept, is the file `integrations/shopify/testdata/erasure-fields.golden` in
the engine's repository, which its tests hold the code to. What an erasure does
not reach is in [What the mirror cannot do](#what-the-mirror-cannot-do).

**What else an erasure removes.** A customer's erasure also blanks the content
of every processed staged delivery of the store's own source (the webhooks
Shopify sent for its orders and customers carry names and addresses), and
removes from the owner's Library every export a data request filed for the
same customer of that store: the document, its index entry, its history and
the comments on it -- not archived, and not restorable. The deliveries that carried a privacy
request (on whichever endpoint they arrived) are blanked at each pass and
deleted when the job is done. A shop's erasure deletes the store's staged
deliveries with its mirror.

**Every erasure is made twice.** A write already on its way when the scrub
runs -- the rest of an export's page, a change fetched a moment before --
lands after it. So the first pass leaves the job `pending`, with when it ran
(`scrubbedAt`) and a second pass due an hour later; the runner is hourly, so
the second pass comes one to two hours after the first, and only after it is
the job `done`. A shop's purge is made twice the same way. A pass that failed
is run again at every run until it succeeds; a failed first pass is a first
pass again. A second request for the same customer, or for the same shop
after a reinstall, opens the job again and runs both passes again; a request
that arrives while the job is waiting adds the orders it names, and does not
move the job's time.

**A shop's erasure asks first.** Before each purge -- the second as the first
-- the store's row is read fresh, and unless it already records the
uninstall, Shopify is asked whether the app is still installed. Only a clear
refusal counts as gone: the Admin API answering 401 or 404, or Shopify
refusing to renew the store's grant as one that no longer exists (401 with
`invalid_request`). A store that answers is not purged, and the store's
backfills that had not started are queued: the outcome is "skipped: the store
is reachable again", or on the second pass "second pass: the store is
reachable again and was not purged a second time; its reconnect reads
everything again", followed by "; queued N backfill(s) of the store that had
not started" when there were any. Anything else -- a timeout, a 5xx, a network error -- fails
the job with "the store did not say whether the app is still installed, so
nothing was deleted", and the next run asks again. A store that is connected
again while it is being purged is left connected, not marked redacted, and its
history is read again; audited as `shopify_shop_redact_overtaken`.

**The runner.** The `runShopifyComplianceJobs` automation runs the due jobs
once an hour, at minute 7, one run at a time. New requests go before an older
erasure being made again, and a run takes jobs for twenty minutes and finishes
the one it is on; the rest wait for the next run. Its statements run on the
engine's maintenance session
([the connection budget](db-connection-budget.md)).

**A request the connector has finished with is not kept as a delivery.** When
a job is done, the deliveries that asked for it are deleted, so the same
request arriving again is acted on again: a store reinstalled and removed
again is erased again, and a customer who asks twice is erased twice. The
delivery that carried a store's `app/uninstalled` is kept until the store is
connected again or purged.

**An export taken before the erasure.** A history download the store's
backfill took before an erasure would write the erased fields back as it is
read. So an erasure, right after its first scrub or purge, starts every
backfill of the store that is queued or under way again from nothing, and a
failed backfill loses the export its cursor named, so "Backfill now" starts a
new one; the job's outcome then says "; started N backfill(s) again from
nothing".
What a download already in flight writes after the first pass, the second
pass scrubs. When a download cannot be started again the job fails with "the
erasure ran, but a backfill in flight could not be started again", and is run
again.

**When a request keeps failing.** A job that fails is tried again at every
run, every hour. Once a job has failed and was due more than a day ago, the
store's Store page says "A privacy request for this store could not be completed."
([the reasons](#the-storefronts-store-page)). Three cases fail every time
until somebody looks, each with "the store did not say whether the app is
still installed, so nothing was deleted": a shop's erasure for a store whose
row records no uninstall and holds no Admin token; one for a store on the
shared app whose grant's renewal token has itself expired; and one for a
store on the shared app whose shop no longer exists at Shopify, whose answer
to a renewal is not documented and so is not read as a refusal.

Redaction is the **one write to a mirror that is not an apply**. Everything
else converges the mirror onto what the origin says; redaction deliberately
makes it differ, by rewriting rows the origin has already forgotten. It runs
under the connector actor and goes through raw SQL, because "every version"
is exactly what an append-only model will not let you touch.

### app/uninstalled

Declare `app/uninstalled` beside the three, at the same app-level endpoint
`https://api.<your-domain>/inbound/shopify`. It is not a privacy topic, but it
is the app's own, and it is bound the same way: the shop is the one the
**signed** body names (`myshopify_domain`, since the payload is the Shop
resource), the `X-Shopify-Shop-Domain` header may not disagree with it, and
the store must be one this app installed. On a per-store URL the topic is
ignored: a managed store's per-store deliveries are signed with the same
secret, so a captured one would replay there.

When it arrives the store is **disconnected**:

- `uninstalledAt` is stamped on the store row. The store ingests nothing while
  it is set, whatever its status says, and the edge serves its storefronts
  with `connectionState: "unavailable"` and no token;
- the Admin grant Shopify ended is dropped from the row (`adminTokenRef`
  cleared) and its sealed secret blanked;
- the store owner's saved connection to it reads `disconnected`, so it leaves
  the list of connected stores in Settings -> Connections;
- it is audited as `shopify_app_uninstalled`.

The mirror stays: purging is `shop/redact`'s, 48 hours later. Shopify retries
an undelivered webhook for four hours, so an `app/uninstalled` can arrive after
the shop has already reinstalled. The store's current grant is asked one
question first, under the lock a reinstall holds, and a grant that answers
means a reinstall superseded the uninstall: nothing changes, audited as
`shopify_app_uninstall_superseded`.

A **reinstall** through Connect Shopify is the shop's new consent. Once the
callback has the fresh grant on the row it clears `uninstalledAt` -- and, for a
store `shop/redact` purged, `redactedAt`, returning the store to `configured`
with nothing mirrored -- and audits it as `shopify_store_reinstalled`, naming
what it cleared. It also asks the store whether the Storefront token it kept
still works, and mints another only when the store refuses it.

### Upgrading from the per-store compliance URL

Earlier versions of this step pointed the three topics at the per-store
`/inbound/shopify-<storeId>`. For a store connected through **Connect
Shopify** (the managed app), that URL now **refuses** all three: the managed
app signs a store's per-store deliveries with the same secret as the
app-level ones, so a captured privacy delivery would replay onto it. A
cluster still configured the old way therefore refuses every privacy request
for those stores -- and Shopify, having had its `202` from the receiver, does
not send a refused one again.

You cannot miss it:

- `shopifyStoreHealth` reports `privacyDeliveries` per store -- how many
  were refused and the exact URL to configure -- to a cluster owner or a
  caller who may read store rows, and to a store's owner for their own store
  (see [What the numbers mean](#what-the-numbers-mean)). The Store page does
  not draw it;
- each refused delivery's staged `v1:platform:inboundRequest` row is
  `failed`, and its `lastError` names the app-level URL;
- each is audited as `shopify_privacy_delivery_refused` (outcome `blocked`,
  target the store) and logged at ERROR.

The fix is on Shopify's side only: in the app's configuration (Dev Dashboard,
or Partner Dashboard for an app created there), set `customers/data_request`,
`customers/redact` and `shop/redact` to
`https://api.<your-domain>/inbound/shopify`. Nothing on the cluster changes.
Every refusal counted before the fix is a request the automation did NOT
honour; its staged row keeps the signed body (the customer id and Shopify's
request id), which is what following it up by hand starts from.

A store connected with its **own** custom app, rather than through Connect,
is unaffected: its per-store URL keeps accepting the three topics, verified
by that store's own webhook secret.

---

## The push channel

The first thing MemQL writes into a live store is a **metafield in the
`memql` namespace**. Additive, invisible to shoppers unless a theme asks for
it, and impossible to break a price or an order with. B2B catalogs and price
lists are the next flip and get their own review.

Four MemQL-origin concepts project today:

| Concept | Metafield | Owner | Storefront |
|---|---|---|---|
| `v1:commerce:productContent` | `memql.description`, `.summary`, `.keywords`, `.blocks` | Product | `public_read` |
| `v1:commerce:customerNote` | `memql.note` | Customer | private |
| `v1:commerce:companyLocationNote` | `memql.note` | Company location | private |
| `v1:commerce:creditLimit` | `memql.creditLimit`, `.creditLimitStatus` | Company location | private |

A row change appends a `v1:platform:outboxEntry` in the write's own
transaction; the runtime's drain worker hands its row reference to the connector.
The connector reads the current source row under its own identity on every
attempt, including retries, and maps it to `metafieldsSet` (or, above 250 metafields, a staged
`bulkOperationRunMutation`).

**A Shopify `userError` dead-letters. It does not retry.** A validation
failure arrives inside a 200 response and will fail identically forever;
retrying it is how a queue stops draining while every individual attempt
looks transient -- so the connector returns it as `sync.Permanent` and the
drain dead-letters it immediately rather than spending its attempt budget.
Dead-lettered entries are in `outboxDeadLetters(target: "shopify")` and on
[Cluster > Data](#cluster--data), under "Dead letters" ("Check shopify").

Use the outbox entry's delivery outcome and the actual Shopify metafields as
delivery evidence. The current drain does not update `productContent.status`,
so a row may still say `draft` after delivery. For public product copy, also
verify that the Storefront API returns the new `memql` metafields; a successful
Admin write alone does not establish storefront visibility.

Accepting a quote is the other write: `draftOrderCreate` with the company as
`purchasingEntity`, the company's payment terms, a PO number, and
`priceOverride` on every line -- which is what makes a quote a quote rather
than a re-quote at today's prices.

---

## Rate limits and cost

The Admin API is limited by a leaky bucket of query-cost points, not by
requests. The connector reads `extensions.cost.throttleStatus` on every
response and **waits before** the call that would overdraw the bucket, rather
than after being refused. A 429 or a `THROTTLED` error still retries with the
vendor's own `Retry-After` where one is given.

- Single queries stay under 1,000 points; the generated selections are
  bounded to two levels of nesting and reference connections page at 25.
- Bulk queries are exempt from the bucket. At most five run per shop at once,
  they must finish within ten days, and the signed result URL is valid for
  one week -- past that the runner restarts the operation.
- The store's current bucket is `costBucket` in `shopifyStoreHealth`, which
  answers a cluster owner or a caller who may read store rows, and a store's
  owner for their own store. It is absent until an Admin call made by the
  node that answers has reported one. The Store page does not draw it.

---

## The quarterly bump

The mirror is pinned to one Admin API version. Bumping it is a reviewed
regeneration, never a configuration change:

1. Bump `apiVersion` in `cmd/shopifyschema/allowlist.yaml`.
2. `go run ./cmd/shopifyschema --record` -- re-records the introspection
   fixture from Shopify's public schema proxy. No token is needed.
3. Review the diff: new fields, removed fields, changed enums, renamed
   types. This is the whole point of the exercise.
4. `make sdk-gen && make arch-model`.
5. Update every store row's `apiVersion`, and set the app's Webhooks API
   version at Shopify to the new one: subscriptions inherit it from the app,
   and the daily reconcile cannot change it. A store pinned to a version the
   mirror was not generated from is **refused** rather than attempted: it
   would return fields the concepts do not declare and omit fields they
   require.

`cmd/shopifyschema/shopify_schema_drift_test.go` fails the build when the
checked-in tree and the generator disagree, which is what makes step 1 the
only hand edit.

---

## What cannot be mirrored -- by anyone

This is the boundary. It is not a limitation of this connector; none of it is
available to any app.

- **Other apps' private data.** App-owned metafields and metaobjects lost
  public access on 2025-05-19, and an app's own database was never exposed.
  Reviews, bundles, wishlists, form submissions, page-builder content: their
  data lives with them. Integrate them in the storefront.
- **Other apps' subscription contracts.**
  `read_own_subscription_contracts` is own-app only, so the mirror carries
  this connector's contracts and no one else's.
- **Saved analytics reports.** Only ad-hoc ShopifyQL exists
  (`shopifyqlQuery`); there is no API for saved report definitions. The
  mirror answers most analytics questions itself and the pass-through covers
  the rest.
- **Checkout internals.** There is no `Checkout` object and checkout runs
  only on Shopify. `AbandonedCheckout` and the `checkouts/*` / `carts/*`
  topics are what exists.
- **Gift-card codes** (masked by Shopify), **raw payment instruments**
  (tokenised references only).
- **Orders older than 60 days** without `read_all_orders`, **staff members**
  without Plus, **payouts and balance transactions** without Shopify
  Payments, **dispute evidence** without approval.

What "lose no function" means in light of that list: every function the store
has today is either mirrored, integrated through the owning app, or named
here as not carried. There is no fourth category and nothing is left to be
discovered on launch day.

---

## What the mirror cannot do

These are limits of this connector, as it is built today.

- **An order older than sixty days, without `read_all_orders`.** Shopify
  hides it from the app, so the mirror keeps it as last recorded and does not
  follow a later refund, edit or cancellation of it. Its delete notice still
  retires it. Every store connected through the shared app is in this case,
  and so is a store whose grant was never recorded: its order data is never
  retired by an empty answer either.
- **An abandoned checkout Shopify expires** is kept the same way, as last
  recorded, for the same reason.
- **A lost delete notice** is not found by the periodic check of a kind that
  is checked by the time each record last changed: orders, customers,
  products, product variants, collections, companies, company locations,
  locations, inventory items, draft orders and subscription contracts. A
  deletion changes no such time. The re-listed kinds
  ([Reconciliation](#step-4----what-happens-next)) find it at their next full
  re-list, except those whose listing is not the whole set.
- **Some kinds are never checked at all**: the shop itself, its policies and
  locales, business entities, payment terms templates, metaobjects, metafield
  definitions, and Shopify Payments payouts and balance transactions. They
  have no periodic check and are not read with a parent, so a lost delete
  notice of one is never found.
- **A delivery being dispatched in the instant a node stopped** is not
  dispatched again. The next periodic check of its kind reads the record's
  present state, and a deletion delivered in that instant is not applied.
- **After a long pause** the held deliveries are worked fifty a minute, so
  ten thousand take over three hours. The store reads Recording meanwhile: a
  held delivery is not a change that could not be recorded. The first periodic
  check after the resume brings the changed records of a kind current sooner.
- **A record the mirror cannot write** holds its kind's check back: the kind
  reads "The last check of ... failed" and the store Needs attention until the
  record can be written.
- **A periodic check that reaches its row budget** (20,000 rows) is recorded
  as run. Shopify does not list records in the order they changed, so the
  check cannot tell which changes it did not reach, and a record it did not
  reach is read again only when it next changes.
- **A store connected with its own app** that grants less than the full list
  of [Step 2](#step-2----create-the-app) reads Needs attention for as long as
  it does.
- **What a customer's erasure does not reach.** A record that carries the
  person only by an email address, with no customer on it -- a guest's
  abandoned checkout -- is not reached. The buyer's message inside a gift card
  another person holds stays: the card is its holder's. An order that moved to
  another customer keeps that customer's current state, and loses only the
  erased person's older versions of it. A line item whose product was deleted
  reads as a custom item, and loses its name, titles and SKU as one does.
- **A failing shop erasure on a store that is not connected sends no email of
  its own** ([the owner's email](#when-a-store-stops-being-recorded-the-owners-email)):
  it is seen on the Store page, as its "A privacy request for this store could
  not be completed." notice.
- **A frozen or locked store that was connected again** is not purged by a
  shop's erasure while Shopify answers neither that the app is installed nor
  that it is not: its job fails and is run again every hour.
- **The inbox keeps a processed delivery's content** until an erasure of its
  store: a customer's erasure blanks it, a shop's erasure deletes it. Nothing
  removes a staged delivery by its age.

---

## The wholesale foundation

`dsl/commerce/` carries what Shopify does not model. Since 2026-04-02
companies, payment terms, volume pricing and up to three catalogs are
available below Plus; approval chains, quotes, credit limits, reps,
territories and reorder lists are not available at all.

| Concept | What it is for | What Shopify holds of it |
|---|---|---|
| `quote` | Priced, time-boxed offer with locked prices | Nothing until accepted, then a draft order |
| `approvalChain` | Who approves, in order, above what value | Nothing. Shopify's order-review Functions run at checkout, after the buyer has been quoted |
| `creditLimit` | A company location's limit and outstanding balance | A `memql.creditLimit` metafield a checkout validation Function reads |
| `salesRep` / `territory` | Who covers which companies | Nothing. Staff members are Plus-only and a rep is not a staff member |
| `reorderList` | What a buyer buys again | Nothing |

**The credit-limit Function is not in this repository.** Shopify Functions
are authored and deployed in the product repository; what MemQL owns is the
number, its history and its projection. The Function reads the metafield.

All of `commerce` is cluster-owner tier today. The wholesale application will
define the per-user model -- which reps see which companies, which buyers see
which quotes -- and that record will re-tier these concepts. Declaring a
per-user tier before that model exists would be guessing at an authorization
boundary.

---

## Operating it

### The storefront's Store page

MemQL OS → Deployables → the storefront → **Store**. The page answers one
question for each store bound to the storefront: **is this store being
recorded, and if not, what fixes it?** The engine decides the answer -- the
store's *verdict*, `shopifyStoreVerdict(storeId)` -- and words every sentence
of it; the page draws what it is given. What the page shows besides is in
[Deployables](deployables.md#the-store-slot-and-the-store-page).

**The five states.** The first that holds is the store's state:

| State | The page says | When |
|---|---|---|
| `not connected` | Uninstalled, or Data removed ("Not connected" while the store's own row does not say which) | Shopify reported the app uninstalled from the store, or removed the store's data. |
| `paused` | Paused | Somebody paused recording for the store ([Pausing a store](#pausing-a-store)). |
| `needs attention` | Needs attention | At least one of the reasons below holds. |
| `starting` | Starting | A backfill of a domain in the store's connection is queued or running, or a domain has never been checked. A domain somebody paused in Cluster > Data does not count. |
| `recording` | Recording | None of the above: every domain in the connection has been checked, and no reason holds. |

A paused or not-connected store carries none of the reasons about its
recording: nothing they would say is something to act on while nobody is
recording it. It can carry one reason, `privacy_request_failing` (below), and
keeps its state when it does. When the verdict itself
could not be read, the store reads "Status unknown" and the page says
"Whether the store is being recorded could not be read.", with "Try again".

**The reasons.** Each reason a store needs attention is one notice: the
engine's sentence, its detail beneath, and at most one act. A store bound to
both destinations shows each reason once. A domain is named in plain words
("orders", "product variants"), never by its concept id.

| Reason | The sentence | Beneath it | The act |
|---|---|---|---|
| `data_not_declared` | "Shopify refuses orders for this app, because the app has not declared the customer data it uses." -- naming every refused domain, as in "orders and customers" | -- | "Declare the customer data": a link to Shopify's page on protected customer data. The steps are [The data declaration](#the-data-declaration). |
| `approval_outdated` | "This store approved an older list of permissions." -- or, for a store connected with its own app, "This store's own app does not allow everything MemQL records." | The scopes the store's grant lacks. | "Open MemQL OS from the store's Shopify admin and approve": a link to the store's apps page in its Shopify admin ([When the list grows](#when-the-list-grows)). None for a store on its own app: MemQL cannot widen that app's permissions, and the store reads Needs attention until they cover the list. |
| `grant_lost` | "MemQL can no longer reach this store." | The error the store's last check recorded. | "Reconnect in Settings", which opens the Shopify connections. |
| `events_not_registered` | "Shopify is not sending every change." | The subscriptions that failed, or "The last registration left 20 of the 24 subscriptions it wanted." | "Register again": the store's subscription pass, run now. It is refused within a minute of the last pass, and for a store that is not recording. |
| `check_failing` | "The last check of orders failed" | For a backfill that failed: its own reason, or "The backfill did not finish." Otherwise the sweep's last error, or "No check has run since" a time, then "The next check is at" a time, or "The next check is due now." | None. |
| `changes_not_recorded` | "3 changes could not be recorded." -- or "1 change could not be recorded.", and "At least 3 changes could not be recorded." when the count is a floor | -- | "Try again": every delivery of the store that used its four tries, back in line. |
| `privacy_request_failing` | "A privacy request for this store could not be completed." | How many and since when, as in "1 request, due since 2026-11-09T09:07:00Z. It is tried again at every run of the privacy jobs." | None. |

A check is *failing* when a domain's backfill failed, at once, or when twice
the domain's own sweep interval has passed with no clean sweep. That time is
counted from the latest of three: the domain's last clean sweep, the store's
connection, and the store's last return from paused or not connected. A store
with no recorded connection time counts a domain never swept clean from when
its verdict was first recorded (`health.verdict.firstRecordedAt`), so it has
the same grace as a store connected then. So a store resumed after a long
pause is not late until two of its intervals have passed. An error about the
store's grant is `grant_lost`, never this.

A privacy request is *failing* when one of the store's privacy jobs failed
and was due more than a day ago (the privacy jobs are in
[Step 5](#step-5----the-compliance-topics)). The reason is listed on a store in
any state. On a store that would read Recording or Starting it makes the store
Needs attention like any other reason; a paused or not-connected store keeps
its word and lists the reason beside it, because a shop's erasure is a job of
a store that is uninstalled.

A change is *not recorded* when a delivery used its four tries and its domain
has had no clean sweep since it arrived. Once that sweep runs, it has read what
the delivery named, and the delivery stops counting. A delivery held during a
pause is never counted ([Pausing a store](#pausing-a-store)).

**Who sees what.** The page is drawn for anyone holding `execute
app:deployables/store`, but the engine serves a store's verdict only to the
store's owner, a cluster owner, or a caller who may read store rows (a
developer). For anybody else holding the part, the verdict read is refused:
a store that does read back says "Status unknown", with the notice "Whether
the store is being recorded could not be read." and the engine's refusal
beneath it; a store whose row they may not read says "Store unavailable".
The acts are drawn only for the store's owner and a cluster owner; for a
developer a reason is its sentence and nothing to press.

**What the page does not repeat.** Each domain's own detail, and the acts on
one domain, are on [Cluster > Data](#cluster--data). What the Store page acts
on is the whole store.

### Cluster > Data

MemQL OS → Cluster → **Data** (the page is headed "Data origins") shows every
concept a connector carries, with its sync state. The section is drawn for those
holding `app:cluster/origins`. For Shopify:

- **Rows are grouped by store.** Each store is its own group, headed by the
  store's id; connectors that keep no stores apart share one group at the
  top. Each row is one domain of that store, with its lag, drift, outbox
  depth, dead letters and last error.
- **Who may act.** Every act on this page -- "Backfill now", "Pause",
  "Resume", "Try again" under "Changes that could not be recorded", and a dead
  letter's "Retry" and "Discard" -- is for a cluster owner, and only a cluster
  owner is shown them. Somebody else who holds the page's part sees every
  state, count and error, and no button to press. A store's owner retries that
  store's changes from its Store page instead, where the connector makes the
  call for them.
- **"Backfill now", "Pause" and "Resume" act on that one store's domain** --
  never on every store. "Backfill now" starts a failed backfill again from the
  start, and carries any other on from where it stopped; on a paused or
  uninstalled store it leaves the backfill queued, to carry on once the store
  is recorded again ([Step 4](#step-4----what-happens-next)). "Pause" stops
  the domain's backfill and its periodic check, and nothing else: the
  changes Shopify sends for it are still applied, and changes waiting to be
  pushed to Shopify are still pushed. When one of the three is refused or
  fails, the page says "The cluster did not finish this.", that the list is
  read again, and the cluster's reason: a backfill can fail part-way, and
  keeps what it applied. They are not drawn until the connector health has
  been read, and read to its end: while it is first read, when it could not be
  read, and when it stopped short -- the page then says "The connector health
  was not read to its end, so this list is incomplete." -- no act is drawn.
- **"Not part of this connection"** is one closed line under each store's
  group, which opens to the domains the store's connection never asked for.
  They are not rows, not errors, and not counted.
- **The counts.** "Connectors" gives, per connector, how many of its concepts
  have reported, out of those it carries, and says it per store ("of its 2
  stores"). A domain outside the connection is counted in none of them, and a
  connector whose concepts are all outside reads "None of its concepts is part
  of the connection of its store." The caption under the heading counts
  distinct concepts: "N of M declared concepts have a connector".
- **"Changes that could not be recorded"** is read only when you press
  "Check": one line per source, with how many deliveries used their four tries
  and "Try again" ([Inbound delivery](inbound-delivery.md#when-a-connectors-try-fails)).

### What the numbers mean

- **driftLast** -- rows the domain's last reconciliation pass wrote that live
  delivery should have carried. A pass compares each row it maps with the
  stored one and writes only those that differ, setting aside its own
  `syncedAt` stamp and, for a type Shopify publishes no version for, the read
  time that stands in for `updatedAt`; so zero is the healthy reading and a
  row's `syncedAt` is when it last changed here, not when a pass last looked.
  A steady non-zero number means deliveries are being lost; look at the
  subscription list and the store's health next. (There is no `driftTotal`:
  nothing counts a running total.)
- **lagSeconds** -- for the domain's last applied delivery, the seconds from
  the record's version to when MemQL applied it. For a record with Shopify's
  own last-changed time, that is how far behind Shopify the mirror was. For a
  kind Shopify publishes no version for, the version is the time it was read,
  and for a deletion it is when the notice was received or when Shopify was
  read and had nothing, so both read about zero. Cluster > Data shows it as "Lag".
- **outboxDepth** -- the pending and failed outbox entries waiting to be
  pushed to the store.
- **phase** -- `idle`, `backfilling` (a backfill queued or running),
  `paused`, `error`; `not granted` for a domain the store's connection asks
  for and its grant does not reach (a standing fact about the connection, not
  a fault; the reason is on the row); and `not requested` for a domain outside
  the store's connection -- never asked for, so there is nothing to grant --
  or one inside it that Shopify refuses for a reason nobody can grant, such as
  a feature the store's plan does not have. A
  `not requested` domain is never a reason and never an error: the Store page
  lists it under "Not part of this connection".
- **scopesNeeded, scopesMissing, domainsNotGranted** -- measured against the
  store's OWN connection: for a managed store, the
  [managed set](#what-a-managed-connection-asks-for); for a store connected
  with its own app, the full list of step 2. `scopesNeeded` is that list;
  `scopesMissing` is what it asks for and the recorded grant does not reach (a
  write scope reaches its read), and is empty when no grant was recorded;
  `domainsNotGranted` is the domains inside the connection the grant reaches
  none of the scopes for. A scope the connection does not ask for is never
  missing.
- **domainsOutside** -- the swept domains the store's connection does not ask
  for. Never requested, and not an error.
- **health.ordersFrom** -- the day (`YYYY-MM-DD`) the store's mirrored orders
  begin: 60 days before its orders were queued, by a connect or by the boot
  pass, under a grant without `read_all_orders`. Absent when the grant holds
  `read_all_orders`, and for a store whose orders no connect or boot pass has
  queued since this was recorded; the Store page then shows no "Orders
  since".
- **lastError** on a domain reading `not granted: shopify needs read_content`
  -- the store was connected without any scope that domain reads under. The
  sweep does not ask the origin about it (the answer is known here) and
  records the reason once; widening the grant and reconnecting is what
  changes it. The store's health lists these as **domainsNotGranted**.
- **fieldsDenied** -- per domain, the fields the grant does not cover
  (a variant's publication counts without `read_publications`). Shopify
  refuses such a field with ACCESS_DENIED and, because most of them are
  non-null, blanks the whole page with it; the connector removes the field
  from that store's copy of the document and asks again, so the rows land
  without it. Learned from the origin's answers since the node started, so
  it is empty until a sweep or a delivery has run, and each replica learns
  it once.
- **privacyDeliveries** -- `refused` is how many privacy deliveries arrived
  on the store's per-store URL and were refused because a managed-app store
  takes them only at the app-level URL; `lastRefusedAt` is the latest;
  `appLevelUrl` is the URL to configure instead. Counted from the store's
  audit trail (`shopify_privacy_delivery_refused`), so it is the same on
  every replica. `capped: true` means the walk stopped at its bound and
  `refused` is a floor. Anything above zero is a legal request Shopify will
  not resend -- see [Upgrading from the per-store compliance
  URL](#upgrading-from-the-per-store-compliance-url).
- **uninstalledAt** -- set while Shopify reports the app uninstalled from the
  store. The store ingests nothing whatever its status says, and its
  storefronts are served as unavailable, until a reinstall clears it -- see
  [app/uninstalled](#appuninstalled).
- **health.subscriptions.notGranted** -- the generated topics this store's
  grant cannot hold, so the daily pass does not ask for them. A standing fact
  about the connection, like a domain's `not granted` phase; widening the grant
  and reconnecting is what changes it.
- A domain whose generated query the origin rejects outright, or whose page
  costs more than the 1,000-point ceiling, is not retried on the next tick:
  a rejection is reported once and the domain waits its own cadence; a
  cost refusal shrinks the page to fit and remembers the size.

### Pausing a store

On the storefront's Store page, the bar carries one of two acts for the store
the page leads with (Production's, else Testing's): **"Pause recording"**
while it is recording, starting or needs attention, and **"Resume"** while it
is paused -- never both. A store that is not connected has neither; its act
is the reconnect its notice offers. The page draws them for the store's owner
and for a cluster owner, and both call `shopifyStoreSetRecording(storeId,
recording)`:

- **Pause** works on any store that is being recorded, and pauses it.
- **Resume** works on a paused store, and starts recording it again.
- **Asking for the state the store is already in** -- pausing a paused store,
  resuming one that is being recorded -- changes nothing and is not recorded.
- **A store that is not connected** -- uninstalled, its data removed by
  Shopify, or in error -- is refused with "shopify: a store that is not
  connected cannot be paused or resumed", and nothing is written.
- **Anybody but the store's owner or a cluster owner** is refused before
  anything but the store row is read.
- **Every pause and every resume is recorded in the audit log**, as
  `shopify_store_paused` or `shopify_store_resumed`, naming the person who did
  it, the store, and the status before and after.

The write runs as the deployment, so `setStoreStatus` called directly is still
a cluster owner's act (see
[Who may read and change a store row](#who-may-read-and-change-a-store-row)).
The next verdict reads the store as resumed from that moment (`resumedAt`),
and the next watch pass records it.

While a store is paused:

- Every delivery Shopify sends is **held**: staged, not applied, and kept for
  the resume. A deletion made in Shopify during the pause is held the same way
  and applied after it, so a pause loses nothing and resuming needs no
  backfill. A held delivery is `failed` with no next try and its tries
  uncounted, and its `lastError` reads `connector: recording is paused, so this
  delivery is held; it goes back in line when recording resumes`. It is never
  counted as a change that could not be recorded.
- Its state is Paused. It carries no reason but a failing privacy request,
  and its owner is not emailed.
- A backfill under way keeps its place and carries on after the resume. If
  the pause outlasted the week Shopify keeps a download, the backfill starts
  that export again by itself (see "When an export fails" in
  [Step 4](#step-4----what-happens-next)).
- "Try again" in Cluster > Data on the store's given-up deliveries puts them
  back in line, and their next try holds them like the rest.

"Resume" answers at once. Before it does, it puts up to fifty held
deliveries back in line and queues the backfill of every domain the store
never read. The store watch puts the rest back on its next passes, five thousand at
most at each, the first at most five minutes later; it does that on every pass for every connected store that is
not paused, so a store resumed by writing its status directly gets its held deliveries
back the same way. The retry pass then works them fifty a minute, so a long
pause takes a while to catch up
([What the mirror cannot do](#what-the-mirror-cannot-do)). A late check is
counted from the resume, so the store does not read as failing because it was
paused, and it reads Starting while a backfill it never ran is read.

This is the whole store. Pausing one domain of one store is "Pause" on that
domain in **Cluster → Data**: it stops that domain's backfill and its
periodic check; the changes Shopify sends for it are still applied, and changes
waiting to be pushed to Shopify are still pushed.

### When a store stops being recorded: the owner's email

A watch runs **every five minutes** (the `watchShopifyStores` automation, on
the cluster's schedule leader). For every store it computes the same verdict
the Store page shows, records it on the store's health as `verdict`, and
emails the store's owner at most twice for each *episode*.

**An episode** is one unbroken stretch in which the store is not recorded and
nobody chose that: needs attention and not connected, in any order. It starts
at the first of them; a move between the two does not start another. Starting,
paused and recording end it. Paused never sends an email (somebody chose it),
and neither does starting (the store is on its way).

**The first email** goes when an episode has lasted the *alert wait* and was
not mailed yet. It is worded for the state the store is in when it is sent,
so an episode that began as needs attention and is not connected by then is
mailed as not connected:

- A store that is **not connected**: subject `example-store.myshopify.com is
  not being recorded`; the body opens "MemQL has not been recording
  example-store.myshopify.com since" the episode's start.
- A store that **needs attention**: subject `example-store.myshopify.com needs
  attention`; the body opens "MemQL is not recording everything from
  example-store.myshopify.com, since" the episode's start.
  When a failing privacy request is the store's only reason, the store is
  being recorded in full, and the body opens instead
  "example-store.myshopify.com needs attention in MemQL, since" the episode's
  start; the subject is the same.

Either body goes on with each reason's sentence, one to a line, then "Open the
storefront's Store page in MemQL OS to see what fixes it:" with MemQL OS's
address, then "This email carries no order and nothing about a customer."
When the cluster names no domain, the whole "Open the storefront's Store
page..." sentence is left out, not only the address.

**The second email** goes when the store is recording again after an episode
that was mailed:

- Subject: `example-store.myshopify.com is being recorded again`
- Body: "MemQL is recording example-store.myshopify.com in full again. It
  needed attention from" the episode's start "to" now.

After an episode whose only reason, at every pass, was a failing privacy
request, the store was recorded all along, and the second email says so
instead: subject `example-store.myshopify.com no longer needs attention`, body
"example-store.myshopify.com no longer needs attention in MemQL. It needed
attention from" the episode's start "to" now.

A failing privacy request on a store that is **paused** or **not connected**
sends no email of its own: a paused store is never mailed, and a store that is
not connected is already in its episode, which is mailed once, after the wait,
if it is owed an email at all. The reason is seen on the Store page, by the
store's owner, a cluster owner, and anyone the cluster lets read stores,
whatever their role.

A store the watch sees for the first time already not connected -- uninstalled
or its data removed before the watch first looked at it -- is never mailed
about that episode. Connected again through Starting or Recording, it is not
mailed then either. Connected again straight into Needs attention, it begins a
new episode at its reconnection, and is mailed after the wait like any other.
A store first seen needing attention is mailed after the wait like any other.

The recovery email is sent even when the first could not be delivered. Times
are written in UTC. A reason's detail never travels: it is the engine's or
Shopify's text, and a mailbox is not the place for it.

**The alert wait** is 30 minutes unless set. A cluster owner sets it in
**Settings → Connections → Shopify**, under "Store alerts": "Email a store's
owner after" a number of "minutes without recording", then "Save". It is a
whole number from 1 to 1440 (one day), kept in the global variable
`SHOPIFY_STORE_ALERT_MINUTES`. A value that is missing, not a whole number,
out of range, or switched off reads as the 30-minute default. The field is
absent for anyone who is not a cluster owner.

**Who is told.** The store's owner: the MemQL user named as the store's owner,
at their primary address, read when the email is staged and recorded nowhere
else. The email is staged through the outbox
([Outbound delivery](outbound-delivery.md)), so the owner's address must be
admitted by the cluster's email allowlist.

**When the owner cannot be told**, the Store page says "The alert could not be
sent.", with why beneath it:

- "This store names no owner to tell." -- the store names no owner, or its
  owner is not found, deactivated, or has no address. No recovery email is
  sent for that episode either.
- The outbox's own reason, when it gave up on the email -- which is how an
  address the allowlist refuses is named. Every address in that reason is
  replaced by the words "the owner's address". "The outbox could not send the
  email." when it gave none.
- "The outbox holds no record of this email."

The notice is shown only while the store is still not recorded.

### The dev-store smoke

Run this against a Shopify development store before pointing the connector at
a real one. It is the end-to-end proof, in the order things can break:

1. **A development store.** Create a development store in Shopify, and make
   sure the cluster's Shopify app is registered ([Connect
   Shopify](shopify-connect.md)). No token is copied by hand: Connect seals
   the Admin token, the Storefront token and the webhook secret itself.
2. **Configure.** In Settings, press "Add Shopify store", then "Continue to
   Shopify", choose the development store and approve. On the storefront's
   Store page, open the **Testing** row and choose it: the list calls it
   "Sandbox store", which is how you know Connect recorded it as a development
   store ([The development store](#the-development-store)). Press "Continue",
   then "Connect store". The Store page should read "Starting", with no
   notice, and then "Recording". A notice names what to fix first.
3. **Subscriptions.** As a cluster owner, run `shopifyEnsureSubscriptions()`. The store's health
   should show `created` equal to the desired count and `failed` empty.
   Cross-check that the subscriptions point at
   `https://api.<domain>/inbound/shopify-<storeId>`, and that the health shows
   no `deliveredAtVersion`, which would mean the app's Webhooks API version
   at Shopify is not the pinned one.
4. **Backfill.** Press nothing: the connect queued every backfill, and the
   continuing pass carries them on every five minutes. Confirm that no domain
   of the store's health (`shopifyStoreHealth`) still reads `backfilling`, that
   the Store page has moved from "Starting" to "Recording", and that the row
   counts match the store's own product and order counts (orders from its
   "Orders since" day on).
5. **Webhook round trip.** Change a product title in the Shopify admin. Within
   seconds the mirror row's `title` should change and its `syncedAt` should
   move. If it does not, check that the store's front door is reachable from
   the internet -- a delivery Shopify cannot make is the most common failure
   and it is invisible from inside the cluster.
6. **Metafield push.** Write a `productContent` row for that product and wait
   for the drain. The metafield should appear on the product in the Shopify
   admin under the `memql` namespace, and its outbox entry should be `delivered`.
   Check the same value through the Storefront API before calling public copy
   live; `productContent.status` is not delivery evidence.
7. **Storefront.** Work the first section of
   [the storefront checklist](shopify-storefront-checklist.md): the Headless
   channel's tokens, a product query, a cart, and the hand-off to
   `cart.checkoutUrl`.
8. **Serving.** If the storefront is served BY this cluster as a
   `shopify_storefront` deployable, its content security policy is written
   from the site's binding and admits the bound store, Shopify's asset CDN
   and the Customer Account API host -- and nothing else. A storefront whose
   calls and images are all refused with no status is almost always a policy
   that does not name the store it is calling. The admitted set, and the rule
   that a storefront bundle binds at RUNTIME rather than at build time, are
   [What the edge admits](shopify-storefront-checklist.md#6-what-the-edge-admits)
   and [Bound at runtime](shopify-storefront-checklist.md#7-bound-at-runtime-never-at-build-time).

Record the results in the PR that changes the connector. A smoke nobody wrote
down is a smoke nobody ran.

---

## Upgrading to the release that records every store

The release that holds a paused store's deliveries, believes a deletion only
on the rules above, and asks a managed store for customers, discounts and
returns changes what an installation holds. Before you roll it out:

- **It is roll-forward only.** Once a node of this release has written, an
  earlier release cannot carry on from where it left off. It cannot write a
  mirror row that this release wrote: such a row carries fields the earlier
  release does not know (`versionSource`, `versionReadAt`), every write of a
  row is made over the stored row, and the concept's schema takes no field it
  does not declare, so the write is refused and that record stops changing
  (the same holds for a staged delivery this release tried, whose tries are
  such a field). It never puts back in line a delivery this release held for a
  paused store, because it does not know the held state. And a backfill this
  release left part-way names no download link, so an earlier release reads
  that export again from its first line. Do not pin the installation back to
  an earlier release after it.
- **Every store connected through the shared app needs attention once.** The
  permission list grew. First register the new permissions on the app at
  Shopify ([When the list grows](#when-the-list-grows), step 1): until then no
  store can approve them. Each such store reads Needs attention, with "This
  store approved an older list of permissions." and the scopes it lacks
  beneath, and its owner is emailed "<store> needs attention" after the alert
  wait. The store's owner (or a cluster owner) follows "Open MemQL OS from the
  store's Shopify admin and approve", somebody on the store's staff approves
  the new list in Shopify's admin, and only then does the store read
  Starting, and then Recording, by itself.
- **Each store's history is read again.** At the first boot, every domain of a
  store being recorded that no backfill read, or whose backfill completed
  under the earlier release, is queued; a backfill the earlier release left
  running begins again from the start at its next step. A history read under
  the earlier release may have put an older snapshot over a newer change,
  which is why each is read once more. The store reads Starting while that
  runs -- a store on the shared app once it has approved, above. What bounds it
  is the continuing pass ([Step 4](#step-4----what-happens-next)): rounds
  started for four minutes of every five, one domain of a store at a time, for
  as long as Shopify takes to prepare each export. The first periodic check
  that reads a record written under the earlier release rewrites it once, to
  add its `versionSource`, so `driftLast` can read high for that one pass. A
  store that is paused at that boot is read again when it is resumed.
- **An index is built at the rollout.** The retry pass reads the failed
  deliveries through a new index on the shared table of rows, and building it
  reads every row of that table once.
- **The first privacy run makes every earlier erasure again.** Every
  customer erasure an earlier release recorded as done is made again under the
  rules of [Step 5](#step-5----the-compliance-topics), twice as every erasure
  is: the earlier release left the customer's email and phone, never reached
  an order's transactions, fulfilments or refunds, and could leave rows the
  mirror cannot write. At the first pass of the first customer erasure made
  on each store after the upgrade, new or repeated, what the earlier release
  left is repaired, once, and the store records that it ran: those rows are made writable again, the
  kinds of the rows of other people it had wrongly scrubbed are read again
  from Shopify, and the store's older privacy and uninstall deliveries are
  deleted. Nothing is asked of you. Each job's `outcome` says what it did (for
  instance "; repaired 3 row(s) an earlier erasure left unwritable"), and new
  requests are worked first, twenty minutes a run, so an installation with many
  old erasures takes several runs.

---

## Wholesale customer access

The custom-app connection requests `write_customers` for the wholesale
customer-tag route. The route finds the applicant's customer by email, creates
a missing customer and applies the configured trade tag. The store must approve
the requested customer permissions and any required protected-customer-data
access. A refusal is recorded on the application's access result with the
next action; an approval alone is not evidence that the tag landed.

Removal acts on the customer id recorded by the grant, so a later email change
does not leave the old customer tagged. Configure the customer-conditioned
discount in the store; the tag alone does not change prices. See
[Consuming the wholesale pack](../build/consuming-the-wholesale-pack.md).

## Unverified

Carried from the research and not confirmed against a Shopify page. Each is
harmless if wrong in the direction assumed, and each is worth confirming
before it matters:

- Whether authenticated-endpoint introspection is documented as supported.
- Cost-bucket sizes above the Standard plan.
- Bulk-operation support for theme files and payout sub-connections.
- Scope gating for menus, events, app installations and tender transactions.
- Whether a custom-distribution app must request `read_all_orders` to reach
  orders older than 60 days, or whether the window does not apply to it.
- A hard byte limit on a webhook payload.
- Whether an explicit compliance-topic exemption exists for custom-
  distribution apps. The connector implements them either way.
- Whether merchant-configured pixels run on a non-Hydrogen SPA.
- Whether an order's fulfilments are readable under `read_orders` alone. A
  managed connection does not ask for `read_fulfillments`, which the
  allowlist names for fulfilments; until this is confirmed against a store,
  assume an order's fulfilments may be missing from a managed store's mirror.
