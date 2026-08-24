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
[the storefront completeness checklist](shopify-storefront-checklist.md).

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
  the rows a pass finds are counted as *drift* -- which is the only evidence
  anyone gets that deliveries are being lost.

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
`integrations/shopify/generated/model.go` and the portal shows the store's
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
  order history begins 60 days ago and nothing says so except a thin backfill.
- **Protected customer data** is a separate approval with two levels. Level 1
  covers customer records; Level 2 covers the fields marked as protected
  (name, email, phone, address). Below the level a field needs, Shopify
  returns `null` and the mirror stores `null` -- so the store row records the
  approved level and the portal shows it, because "the customer has no phone
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

---

## Step 3 -- enter the store in MemQL

Portal → Cluster → **Stores** → Add a store. It asks for:

| Field | What it is |
|---|---|
| Domain | `acme-widgets.myshopify.com`. The one identifier Shopify never changes. |
| App client id | From the Dev Dashboard. Recorded for the audit trail. |
| Admin token ref | The name of a `v1:platform:globalSecret` row holding the Admin API token. |
| Storefront token ref | Same, for the Headless channel's Storefront token. |
| Webhook secret ref | Same, for the app's webhook signing secret. |
| API version | Must match the version the mirror was generated from -- see "The quarterly bump". |
| Protected data level | `none`, `level1` or `level2`, as approved. |
| Owner | The MemQL user a `customers/data_request` export is filed under. |

**The three token fields are REFERENCES, not tokens.** The store row is read
by the portal and returned to a browser; a token on it would be a token on a
screen. Create the secret first (Portal → Cluster → Secrets, or
`memql env`), then name it here.

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
silently overrode a row would make the portal's view of a store a lie, and
would do it only on the nodes carrying the variable.

---

## Step 4 -- what happens next

**Subscriptions.** On boot and daily at 03:15, the connector registers every
mirrored topic for every ingesting store: HTTPS delivery to
`https://api.<your-domain>/inbound/shopify-<storeId>`, at the pinned API
version, with `includeFields` trimmed to `id`, `admin_graphql_api_id` and
`updated_at`. It updates subscriptions whose URL, version or fields have
drifted, and removes its own that the allowlist no longer wants. Another
app's subscriptions are not visible to this app and are never touched.

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
affordable at all. It is OPERATOR-DRIVEN -- nothing schedules it -- from the
portal's Data origins surface or with
`datasyncStartBackfill(connector: "shopify", conceptId: "v1:shopify:order")`.

Domains are applied **parents first** (`generated.ApplyOrder`), so a line
item never lands before its order.

**Live delivery.** A webhook arrives at the inbound receiver, is verified
against the store's own secret, and is staged. The runtime's dispatcher
automation hands the staged row to the connector, which dedupes on the
webhook id, fetches the object by GID with the generated selection set, and
returns the rows; the runtime applies them under the version guard, so a
write whose `updatedAt` is older than the stored row's is recorded as *stale*
and dropped.

**Reconciliation.** Every ten minutes the runtime sweeps the domains whose
own interval has elapsed. Domains whose root connection accepts a `query:`
filter are paged by `updated_at:>`; the rest -- gift cards, price lists and
catalogs, menus, pages, blogs, articles, policies, marketing, payouts,
redirects, carrier services, store credit -- are re-listed whole on their own
cadence and rows the origin no longer returns are tombstoned.

A full re-list that hits its page cap tombstones **nothing**. Tombstoning is
an argument from absence, and the argument is only valid if the walk was
complete.

---

## Step 5 -- the compliance topics

Three topics are declared in the app's configuration rather than created
through the API, because they are not members of `WebhookSubscriptionTopic`:

```
customers/data_request
customers/redact
shop/redact
```

Point all three at `https://api.<your-domain>/inbound/shopify-<storeId>`.
A bad HMAC on any of them answers **401**, which is Shopify's requirement.

What each one does:

The three arrive as ordinary webhooks and are turned into
`v1:shopify:complianceJob` rows -- the connector's own queue, deliberately
not the outbox: the outbox's drain hands every entry to `Propagate`, and a
privacy job handed to `Propagate` would try to write a customer's export into
Shopify. An hourly automation runs the ones whose hold has elapsed.

- **`customers/data_request`** collects every mirror row that mentions the
  customer's GID -- across every mirrored concept, at any nesting depth --
  into a JSON artifact in the Library, filed under the store's owner, with
  the Shopify request id recorded. Audited as
  `shopify_data_request_exported`. A store with no owner **fails** the export
  rather than filing one merchant's customer data in a stranger's library.
- **`customers/redact`** rewrites the customer's PII fields to `[redacted]`
  across **every version** of every row that mentions them, keeping the
  opaque GID and the commercial facts -- quantities, totals, dates. The
  merchant's books are not the customer's personal data, and destroying them
  would be a different compliance failure. Audited as
  `shopify_customer_redacted`, and scheduled rather than immediate so a
  merchant's own grace period applies.
- **`shop/redact`** purges the store's whole mirror and its sync state after
  a 48-hour hold, then pauses the store row. Before purging it re-checks
  whether the store is reachable again: an uninstall an operator reverses
  within the hold must not cost the mirror. Audited as
  `shopify_shop_redacted` (or `shopify_shop_redact_skipped`).

Redaction is the **one write to a mirror that is not an apply**. Everything
else converges the mirror onto what the origin says; redaction deliberately
makes it differ, by rewriting rows the origin has already forgotten. It runs
under the connector actor and goes through raw SQL, because "every version"
is exactly what an append-only model will not let you touch.

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
transaction; the runtime's drain worker hands it to the connector, which maps
it to `metafieldsSet` (or, above 250 metafields, a staged
`bulkOperationRunMutation`).

**A Shopify `userError` dead-letters. It does not retry.** A validation
failure arrives inside a 200 response and will fail identically forever;
retrying it is how a queue stops draining while every individual attempt
looks transient -- so the connector returns it as `sync.Permanent` and the
drain dead-letters it immediately rather than spending its attempt budget.
Dead-lettered entries are in `outboxDeadLetters(connector: "shopify")` and on
the portal's Data origins surface.

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
- The store's current bucket is on the portal's Stores page.

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
5. Update every store row's `apiVersion` and let the daily reconcile
   re-register every subscription at the new version. A store pinned to a
   version the mirror was not generated from is **refused** rather than
   attempted: it would return fields the concepts do not declare and omit
   fields they require.

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

### The Stores page

Portal → Cluster → Stores. Per store: status, the granted scopes against the
allowlist's needs, the protected-data level, the last subscription reconcile
and what it changed, the cost bucket, and every domain's sync state with its
drift counters.

Backfilling, reconciling and pausing an individual DOMAIN are not here: they
belong to every connector, so the **Data origins** surface owns them and this
page does not repeat the same three buttons. What is here is what is
Shopify's alone -- the credentials, the scopes, the subscriptions, the cost
bucket, and the per-STORE pause, which is a different switch: it stops
ingestion for one merchant while their deliveries keep being staged.

### What the numbers mean

- **driftLast / driftTotal** -- rows a reconciliation pass wrote that live
  delivery should have carried. A steady non-zero number means deliveries are
  being lost; look at the subscription list and the store's health next.
- **staleWrites** -- writes refused because the stored row was newer. Ones
  and twos are normal (Shopify redelivers). A climbing number means something
  is replaying old deliveries.
- **tombstoned** -- rows the origin no longer has.
- **phase** -- `idle`, `backfilling`, `reconciling`, `paused`, `error`.

### Pausing a store

Set its status to `paused`. Deliveries are still **staged** and simply not
applied, so a pause loses telemetry rather than events and resuming does not
need a backfill.

### The dev-store smoke

Run this against a Shopify development store before pointing the connector at
a real one. It is the end-to-end proof, in the order things can break:

1. **Install.** Create the custom-distribution app on the dev store with the
   scopes above. Note the Admin token, the Storefront token and the webhook
   secret.
2. **Configure.** Seal the three secrets, add the store in the portal, and
   confirm the portal shows `configured` with no missing scopes.
3. **Subscriptions.** Run `shopifyEnsureSubscriptions()`. The store's health
   should show `created` equal to the desired count and `failed` empty.
   Cross-check in the Shopify admin that the subscriptions point at
   `https://api.<domain>/inbound/shopify-<storeId>` at the pinned version.
4. **Backfill.** Run
   `datasyncStartBackfill(connector: "shopify", conceptId: "v1:shopify:product")`
   to completion, then `v1:shopify:order`. Confirm the row counts match the
   store's own product and order counts.
5. **Webhook round trip.** Change a product title in the Shopify admin. Within
   seconds the mirror row's `title` should change and its `syncedAt` should
   move. If it does not, check that the store's front door is reachable from
   the internet -- a delivery Shopify cannot make is the most common failure
   and it is invisible from inside the cluster.
6. **Metafield push.** Write a `productContent` row for that product and wait
   for the drain. The metafield should appear on the product in the Shopify
   admin under the `memql` namespace, and the row's status should be `live`.
7. **Storefront.** Work the first section of
   [the storefront checklist](shopify-storefront-checklist.md): the Headless
   channel's tokens, a product query, a cart, and the hand-off to
   `cart.checkoutUrl`.

Record the results in the PR that changes the connector. A smoke nobody wrote
down is a smoke nobody ran.

---

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
