# The Shopify connector -- a complete, generated mirror of a store, and the origins a wholesale business needs

**Date:** 2026-08-22
**Status:** approved in the 2026-08-22 backlog brainstorm (sub-project J1, the connector half of the tenth sub-project)
**Owner:** `integrations/shopify` (the connector), `cmd/shopifyschema` (the generator), `dsl/shopify` (generated + overlay), `dsl/commerce` (the origins), `component/server` (inbound source rows), `clients/portal`, `docs/public/operate`

Sub-project J1 of the 2026-08-22 backlog brief. Built on J0 (epic #4378:
Mirror / Origin / Native, the connector contract, the outbox, the
runtime) and on F (epic #4339: the `shopify_storefront` site kind). The
client has a working, well-built Shopify store; MemQL will host a headless
storefront for it now and a wholesale web application later. The
requirement, in the owner's words: **lose no function the store has today,
mirror everything with full fidelity, and set the ownership model now so the
wholesale application can be built on MemQL without re-plumbing.**

Every Shopify fact below was verified on 2026-08-23 by live introspection of
Admin GraphQL **2026-07** (287 root queries, 483 mutations, 225 webhook
topics) and the pages cited in section 12; items no page states are marked
UNVERIFIED.

---

## 1. What "everything" is, and where it ends

### 1.1 Mirrorable, fully

About 45 root types, each with its children:

- **Products**: `Product`, `ProductVariant`, `ProductOption` / `ProductOptionValue`, media (`MediaImage`, `Video`, `ExternalVideo`, `Model3d`), `Collection`, `Publication` / `Channel` / `ResourcePublication`, `SellingPlanGroup` / `SellingPlan`, `ProductFeed`, taxonomy, bundles.
- **Pricing**: `DiscountNode` (code and automatic, every discount class), `GiftCard` (code masked by Shopify), `PriceList` / `PriceListPrice` / `QuantityRule` / `QuantityPriceBreak`, `Catalog`.
- **Inventory**: `InventoryItem`, `InventoryLevel` (named quantities), `Location`, `InventoryTransfer`, `InventoryShipment`.
- **Customers**: `Customer` (addresses, email/phone/consent through the current fields), `Segment` and membership, `CustomerPaymentMethod` (tokenised references), `StoreCreditAccount`.
- **B2B**: `Company`, `CompanyLocation` (addresses, `BuyerExperienceConfiguration`, tax settings, catalogs, staff assignments), `CompanyContact`, role assignments, `PaymentTermsTemplate`, `PaymentTerms` / `PaymentSchedule`.
- **Orders**: `Order` (152 fields), `LineItem`, `OrderTransaction`, `Refund`, `Fulfillment`, `FulfillmentOrder`, `Return`, `ReverseFulfillmentOrder` / `ReverseDelivery`, `OrderRiskSummary`, `DraftOrder`, `AbandonedCheckout`, `TenderTransaction`.
- **Shipping**: `DeliveryProfile` → location groups → zones → `DeliveryMethodDefinition`; `DeliveryCarrierService`; `DeliveryCustomization`.
- **Markets**: `Market`, `MarketWebPresence`, `Domain`, `ShopLocale`, translations.
- **Content**: `Page`, `Blog`, `Article`, `Comment`, `Menu` / `MenuItem`, `File` (generic, image, video), `UrlRedirect`, `Metaobject` / `MetaobjectDefinition`, `MetafieldDefinition`.
- **Shop**: `Shop`, `ShopPolicy`, `BusinessEntity`, `PrivacySettings`, checkout and accounts configuration.
- **Finance**: Shopify Payments payouts, balance transactions, disputes (only with Shopify Payments).
- **Marketing**: `MarketingEvent`, `MarketingActivity`. **Subscriptions**: the connector's own `SubscriptionContract`s. **Themes**: `OnlineStoreTheme` and its files. **Staff**: `StaffMember` (Plus only). **Timeline**: `Event`.

### 1.2 Not mirrorable by anyone -- the boundary, stated

- **Other apps' private data.** App-owned metafields and metaobjects lost public access on 2025-05-19 ("No other apps have access"), and data an app keeps in its own database never reaches Shopify. A reviews app, a bundle builder, a wishlist -- their data lives in the app. **"Lose no function" is met by integrating those apps in the storefront through their own APIs and embeds, not by mirroring them**, and the storefront checklist (section 8) lists them by category so nobody expects the mirror to carry them.
- **Other apps' subscription contracts** (`read_own_subscription_contracts` is own-app only).
- **Saved analytics reports.** Only ad-hoc ShopifyQL (`shopifyqlQuery`, `read_reports` + Level-2 approval); no report definitions. The mirror answers most questions itself; a pass-through builtin covers the rest.
- **Checkout internals.** There is no `Checkout` object; checkout runs only on Shopify; `AbandonedCheckout` and the `checkouts/*` / `carts/*` topics are what exists.
- **Gift-card codes** (masked), **raw payment instruments** (tokenised references only), orders older than 60 days without `read_all_orders`, staff without Plus, payouts without Shopify Payments, dispute evidence without approval.

### 1.3 B2B, and what Shopify does not model

Since 2026-04-02, companies, payment terms, volume pricing and up to three catalogs (via Markets) are available below Plus; Plus keeps unlimited catalogs, direct catalog assignment, deposits, order-review Functions. Shopify does **not** model: multi-step or buyer-side approval chains, quotes / RFQ, credit limits, per-company minimum order values, reps and territories, saved reorder lists. Those are the domains the wholesale application will need MemQL to be the **origin** of -- J0's third state used as intended (section 7).

---

## 2. What the tree already has

- **J0** (epic #4378): the `Connector` contract, the connector actor, the
  mirror write guard, the version-guarded apply, the outbox and drain
  worker, backfill and reconciliation runners, `syncState`, the Data
  origins page. `integrations/shopify` registers as the `shopify` connector
  with the product domain only.
- **The inbound receiver** (`POST /inbound/{source}`, memql#2957): HMAC
  with a configurable scheme and header (`X-Shopify-Hmac-Sha256`), a
  dedupe header (`X-Shopify-Webhook-Id`), staging rows, redelivery
  idempotency -- with the per-source secret in **env**
  (`MEMQL_INBOUND_SOURCE_SHOPIFY_SECRET`), which a multi-store connector
  cannot live with.
- **An Admin GraphQL client** for one store from env
  (`MEMQL_SHOPIFY_STORE_DOMAIN`, `MEMQL_SHOPIFY_ADMIN_TOKEN`,
  `MEMQL_SHOPIFY_STOREFRONT_TOKEN`, `MEMQL_SHOPIFY_API_VERSION`),
  `postGraphQL`, a hand-written product query.
- **F's storefront kind** (epic #4339): a site with a `binding` naming a
  store domain and a Storefront token reference, injected into the site's
  runtime config.
- **D's hard-delete precedent** (epic #4325): a Go-side deletion job for a
  concept the engine's own retention cannot touch.

---

## 3. Decisions

### D1 -- The model is generated from the Admin schema, pinned by version

`cmd/shopifyschema` introspects the token-free schema proxy Shopify's own
codegen downloads from (`https://shopify.dev/admin-graphql-direct-proxy/{version}`)
and emits the concept tree for an allowlist of root types. Chosen over
hand-curated concepts (a curated subset is the "limited functionality" the
owner refused, and it lags every quarter) and over generating everything
(the schema has hundreds of types that are inputs, payloads and
app-scoped objects with no mirror value). The allowlist is the explicit,
reviewed list; the generator is the fidelity.

### D2 -- A webhook is a trigger; the object is fetched

Webhook payloads are REST-shaped, lose fields (the 2025-01 customer
removals), truncate (100 variants), arrive unordered and duplicated, and
are not guaranteed. The connector dedupes on `X-Shopify-Webhook-Id`,
correlates on `X-Shopify-Event-Id`, then **fetches the resource by GID
through Admin GraphQL with the generated selection set** and applies it
under the `updatedAt` guard. One apply path for live, backfill and
reconciliation, at full fidelity. (Shopify's GraphQL-shaped "Next
Generation Events" are a developer preview; the connector adopts them when
they ship, as a transport detail.)

### D3 -- Backfill is Bulk Operations; reconciliation is `updated_at` plus full re-lists

Bulk queries are exempt from cost limits, stream JSONL with `__parentId`,
may run five at a time per shop, and must finish within ten days. The
generator emits the per-type bulk queries split to the two-level /
five-connection limit. Domains with no webhook topic (gift cards, price
lists and catalogs, menus, pages / blogs / articles, files, policies,
marketing, payouts, redirects, carrier services, store credit) are
reconciled on a schedule by full re-list; the rest by `updated_at:>`
queries. Webhook delivery is explicitly not guaranteed by Shopify, so
reconciliation is a requirement, not a backstop.

### D4 -- Many stores; credentials on the store row

`v1:shopify:store` carries each store's configuration and credential
references; the connector is one, the stores are many. The inbound
receiver learns to resolve a source's secret from a connector-owned row,
so each store has its own webhook secret and its own source.

### D5 -- Mirror rows are cluster-owner tier

Commerce data is business data. The storefront reads Shopify directly
through the Storefront API (F); the wholesale application reads the mirror
through named queries under the identity model it will define. Nothing
here is per-user.

### D6 -- The compliance topics are implemented regardless of app type

`customers/data_request`, `customers/redact`, `shop/redact` are mandatory
for App Store apps; no page exempts custom-distribution apps explicitly
(UNVERIFIED), and the client's own obligations do not depend on Shopify's
rules. Implemented as audited jobs.

### D7 -- The push channel ships proven on a harmless domain

`Propagate` implements Admin mutations, and the first MemQL-origin domain
mirrored to Shopify is **metafields in a `memql` custom namespace** on
products, customers and company locations -- additive, cannot break the
store, and the vehicle for agent-written content. B2B catalogs and price
lists are the next flip, when the wholesale application asks.

### D8 -- The wholesale foundation is a `commerce` namespace of MemQL-origin concepts

Quotes, approval chains, credit limits, reps and territories, reorder
lists: concepts now, with Shopify projections where Shopify can hold one
(a credit limit as a company-location metafield a checkout validation
reads). Generic wholesale shapes live in the engine as DSL-configurable
features; the client's workflows live in the product repository.

### D9 -- The storefront's completeness is a checklist, not a guess

What the Storefront API, the Customer Account API and B2B contexts give
a headless storefront, and what a Liquid theme delivers that headless must
rebuild, written down against this research so the SPA is built to the
list.

---

## 4. The generator and the model

### 4.1 `cmd/shopifyschema`

Inputs: `MEMQL_SHOPIFY_API_VERSION` (default `2026-07`) and the allowlist
(`cmd/shopifyschema/allowlist.yaml`: root type, child connections to
materialise, the bulk split, the topics, the scope, the reconciliation
mode and cadence). Output: `dsl/shopify/generated/<type>.memql`,
`dsl/shopify/generated/selections/<type>.graphql` (the fetch selection
set), `dsl/shopify/generated/bulk/<type>.graphql` (the bulk queries), and
`topics.go` (topic → concept routing). A drift gate
(`shopify_schema_drift_test.go`) regenerates in a temp dir and fails on a
difference; the quarterly bump is a regeneration and a reviewed diff,
recorded in the changelog.

### 4.2 Mapping rules

| Shopify | MemQL |
|---|---|
| object type on the allowlist | `@origin("shopify")` concept `v1:shopify:<type>`; fields `storeId!`, `gid!`, `updatedAt!` (the version), `syncedAt!`, `deleted bool`; `@displayCard` from the overlay |
| scalar, enum | the matching DSL type; enums closed to the schema's values |
| `Money` / `MoneyBag` / `MoneyV2` | `{amount, currencyCode}` (bag: shop and presentment) |
| nested object (non-connection) | `object` field with the nested selection |
| connection that carries data (line items, fulfilments, inventory levels, price-list prices, company locations, variants...) | a child concept with `@relationship(type="parent", field="parentGid", target=<parent>)`, materialised from `__parentId` on bulk and from the nested selection on fetch |
| connection of references only | `[]string` of GIDs |
| union / interface | `object` with `__typename` and the per-member selection |
| metafields | `metafields object` keyed `namespace.key` → `{type, value, jsonValue, compareDigest}`; only merchant-owned namespaces and the app's own are visible |
| deprecated field | omitted, with the replacement used (`media` not `images`, `addressesV2`, `defaultEmailAddress`, `discountNodes`, `Order.risk`) |

Every generated concept is a **mirror** by J0's rules: user and agent
writes refused, the `shopify` connector actor writes, `updatedAt` guards.

### 4.3 The overlay

`dsl/shopify/overlay/`: display cards; relationships across types
(`Order.customerGid → Customer`, `LineItem.variantGid → ProductVariant`,
`CompanyLocation.companyGid → Company`, `InventoryLevel.locationGid →
Location`, ...) declared with `as=` labels; `v1:shopify:store`
(section 5); the named analytics reads (section 9).

---

## 5. Stores, credentials, the app

`v1:shopify:store`: `domain!`, `name`, `appClientId`, `adminTokenRef`,
`storefrontTokenRef`, `webhookSecretRef` (names of `globalSecret` rows),
`apiVersion`, `scopesGranted []string`, `protectedDataLevel enum(none, level1, level2)`,
`plan`, `status enum(configured, backfilling, live, paused, error)`,
`health object`. Cluster-owner tier. The env-configured single store is
seeded into a row at boot when present and the env path is retired.

The app per store is a **custom-distribution app** created in the Dev
Dashboard (admin-created custom apps cannot be created since
2026-01-01), installed on the client's store, with the read scopes of
section 12's table, `read_all_orders` requested (approval for the >60-day
window; UNVERIFIED whether a custom-distribution app must request it),
`write_metafields`-class scopes for the push domain, and the Headless
channel's Storefront token for F's site binding. Expiring offline tokens
are required for public apps from 2027-01-01; the connector stores tokens
through references and refreshes where the grant allows.

Rate limits: a cost-aware client reads `extensions.cost.throttleStatus`
and paces on `currentlyAvailable`; single queries stay under 1,000 points;
bulk queries are exempt. Protected customer data: the store row records
the approval level; fields Shopify returns as `null` for an unapproved app
stay `null` and the Data origins page says why.

---

## 6. Ingestion

### 6.1 Subscriptions

`EnsureSubscriptions(store)` registers, through `webhookSubscriptionCreate`,
every topic of every allowlisted domain (about 150 of the 225) at the
pinned version, HTTPS delivery to `https://api.<domain>/inbound/shopify-<storeId>`,
`includeFields` trimmed to identity + `updated_at` (payloads are signals),
and reconciles the subscription list on boot and daily (Shopify deletes a
subscription after eight consecutive failures and retries eight times over
four hours; the receiver answers within the five-second budget by staging
only). The three compliance topics are declared on the app.

### 6.2 The inbound source row

`component/server`'s inbound receiver resolves a source named
`<connector>-<storeId>` through the connector (`Connector.InboundSource(id)
→ {scheme, header, dedupeHeader, secretRef}`) before falling back to env;
the store row's `webhookSecretRef` is the secret. Staged rows carry the
topic, the shop domain, the webhook id and the event id from the headers.

### 6.3 Apply

J0's dispatcher calls `Apply(req)`: dedupe on the webhook id (the staging
row's `dedupeKey`); route the topic through `topics.go`; for create /
update topics **fetch by GID** with the generated selection and return the
`MirrorWrite` (parent and children) stamped with `updatedAt`; for delete
topics return a tombstone (`deleted=true`); for `bulk_operations/finish`
hand the operation id to the backfill runner; for the compliance topics
enqueue the jobs of 6.5. A fetch that returns nothing (already deleted)
tombstones; a throttled fetch retries under the pacing client.

### 6.4 Backfill and reconciliation

`Backfill(concept, cursor)`: start (or resume) the generated bulk query
for the type, poll `bulkOperation(id)` (and accept the `finish` topic),
stream the JSONL from the signed URL (valid one week), emit parents and
`__parentId` children in order, persist the line offset as the cursor.
Five concurrent operations per shop at most; the runner orders domains so
parents precede children.

`Reconcile(concept, since)`: for domains with `updated_at` filters, page
the root query with `updated_at:>since` and apply; for the polling-only
set, a full re-list on the cadence in the allowlist (hourly for price lists
and gift cards, daily for content and policies), tombstoning rows absent
from the origin. Drift counted on `syncState`.

### 6.5 Compliance jobs

- `customers/data_request`: export every mirror row referencing the
  customer (orders, addresses, consent, metafields, company contact) as a
  JSON artifact in the Library (F's routes), delivered to the store owner
  with the request id; audited.
- `customers/redact`: scrub the PII fields of the customer's rows across
  **all versions** (a Go-side hard-delete-and-rewrite job, D's precedent),
  keep the opaque GID and the commercial facts (quantities, totals),
  record the redaction; audited; completed within 30 days.
- `shop/redact`: purge the store's entire mirror after the 48-hour hold;
  audited.

---

## 7. Origins: the push channel and the commerce namespace

### 7.1 `Propagate`

The `shopify` connector implements `Propagate(entry)` for a closed set of
MemQL-origin domains through Admin mutations (`metafieldsSet` first),
batching through `bulkOperationRunMutation` (staged JSONL, 100 MB, 24
hours) above a threshold, idempotent on the outbox key, with Shopify's
`userErrors` surfaced as typed failures that dead-letter rather than
retry.

### 7.2 First origin domain

`v1:commerce:productContent`, `customerNote`, `companyLocationNote`:
MemQL-origin concepts (`@origin("memql") @mirroredTo("shopify")`) whose
rows project to metafields in the `memql` namespace on the corresponding
Shopify object. Agents write them through tools; the storefront may read
them through the Storefront API once the definition's storefront access is
`public_read`. Proves the whole outbound path without risk to the store.

### 7.3 The wholesale foundation

`dsl/commerce/`: `quote` (lines, prices, validity, status `draft → sent →
accepted → expired`; acceptance creates a Shopify draft order through the
push channel with locked prices and the company's payment terms),
`approvalChain` (steps, approvers, thresholds; evaluated in MemQL before a
draft order is sent), `creditLimit` (`@mirroredTo("shopify")` as a
company-location metafield; the cart / checkout validation that reads it
is the product repository's Function), `salesRep` and `territory`
(assignments to companies and locations), `reorderList`. Concepts,
relationships to the mirror (`companyGid`, `companyLocationGid`), named
reads, and the projections. Cluster-owner tier now; the wholesale
application's identity model decides the per-user reads later.

---

## 8. The storefront checklist (for F's `shopify_storefront` kind)

`docs/public/operate/shopify-storefront-checklist.md`, written against the
research:

- **Channel and tokens**: the Headless channel per store (defines the
  Storefront scopes; public token for the browser, private token +
  buyer IP for server calls); the store row supplies F's binding.
- **Catalog and cart**: products, collections, search and predictive
  search (Search & Discovery settings apply), `@inContext(country,
  language, buyer)`, the Cart API with attributes, codes and delivery
  options, `cart.checkoutUrl` hand-off; checkout is Shopify-hosted, always.
- **Customers**: the **Customer Account API** (OAuth 2.0 / OIDC with PKCE,
  discovery documents, `customer-account-api:full`; requires new customer
  accounts -- legacy accounts deprecated 2026-02-26) for profile, addresses,
  orders, returns, subscriptions, company contacts.
- **B2B**: `customer.companyContacts → locations` selector,
  `@inContext(buyer: {customerAccessToken, companyLocationId})` on every
  product query, `cartBuyerIdentityUpdate(companyLocationId)`, the hosted
  B2B checkout (PO number, terms, deposits, vaulted cards, submit for
  approval); never cache buyer-contextual responses.
- **Platform plumbing**: the `checkout.<domain>` subdomain to Shopify,
  `sitemap` / `seo` fields, `UrlRedirect` rows re-applied by the SPA,
  consent through the Customer Privacy API and analytics without the
  Pixel Helper (not compatible with headless), metaobjects with
  `storefront: public_read`.
- **What headless loses from a Liquid theme and how each is replaced**:
  theme app extensions, app blocks and embeds, ScriptTags (no host in a
  SPA → the app's own API or embed); OS 2.0 sections and theme settings
  (rebuilt); the password page. **Kept**: checkout UI extensions,
  Functions, checkout branding and pixels, Flow, Search & Discovery
  configuration, policies / menus / content / metaobjects / SEO through the
  API. The app categories the client's store uses (reviews, bundles,
  wishlists, forms, email capture, page builders) are inventoried in the
  runbook's first step and each gets an integration line or an explicit
  "not carried".

---

## 9. Analytics reads, tools, portal, docs

- Named reads over the mirror (cluster-owner tier, `dsl/shopify/overlay/queries.memql`):
  `soldByProduct(storeId, from, to)`, `soldByVariant`, `neverSold(storeId, since)`,
  `stockBelow(storeId, locationGid, threshold)`, `repeatCustomers(storeId, window)`,
  `refundRate(storeId, window)`, `ordersByCompany(storeId, companyGid, window)`,
  `paymentTermsOutstanding(storeId)`, `abandonedCheckouts(storeId, since)`;
  agent tools `commerceSold`, `commerceStock`, `commerceCustomers`,
  `commerceCompany`; a `shopifyql` pass-through builtin (`read_reports`,
  Level 2) for what the mirror does not answer.
- Portal: a **Stores** page (Cluster group, cluster owners): add a store
  (client id, the three secret references, API version), the scope check
  against the allowlist's needs, the protected-data level, per-domain
  backfill / reconcile / pause through J0's Data origins page, the
  subscription list with last-delivery times.
- Docs: `docs/public/operate/shopify-connector.md` (app setup as a
  custom-distribution app, scopes, tokens and expiry, webhooks and the
  compliance topics, limits, the quarterly bump, what cannot be mirrored),
  the storefront checklist (section 8), the wholesale foundation
  (section 7.3), `integrations/CLAUDE.md` naming the connector as the
  reference implementation of J0's contract.

---

## 10. Testing

1. Generator: a recorded introspection fixture for 2026-07 regenerates
   byte-identically; the drift gate fails on a changed fixture; every
   mapping rule has a case (money, union, child connection, metafields,
   deprecated field).
2. Apply: a recorded webhook capture + a fake Admin endpoint →
   fetch-by-GID → the row; duplicates by webhook id ignored; an older
   `updatedAt` recorded stale; delete topics tombstone; a fetch of a
   deleted object tombstones.
3. Backfill: a recorded JSONL with `__parentId` children → parents and
   children; resume from a line offset after a simulated crash; five
   concurrent operations respected.
4. Reconciliation: `updated_at` paging; full re-list tombstones an absent
   row; drift counted.
5. Compliance: data request produces the artifact; redact scrubs across
   versions and keeps commercial facts; shop redact purges after the hold.
6. Push: `productContent` → `metafieldsSet` against the fake endpoint;
   `userErrors` dead-letter; bulk mutation above the threshold.
7. Stores: the inbound source resolves through the store row; two stores
   with different secrets verify independently; the env-seeded first row.
8. The cost-aware client paces on a recorded `throttleStatus` sequence.
9. Reads and tools on a fixture store; the Stores page.
10. A live dev-store smoke in the runbook: install, backfill, a webhook
    round-trip, a metafield push, the storefront checklist's first items.

---

## 11. Delivery

| PR | Contains | Depends on |
|---|---|---|
| 1 -- the model and the stores | `cmd/shopifyschema`, the generated tree and overlay, the drift gate, `v1:shopify:store`, the inbound source row, the env seed | J0 PR 1 (#4379-#4381) |
| 2 -- ingestion | subscriptions, fetch-apply, bulk backfill, reconciliation, the cost-aware client, the compliance jobs | PR 1; J0 PR 2 (#4382-#4383) |
| 3 -- origins, reads, surfaces | `Propagate` + the first origin domain, the `commerce` namespace, the analytics reads and tools, the Stores page, the runbook and the storefront checklist | PR 2 |

One `Closes #N` line per issue. Ten tasks.

---

## 12. References

- Admin API: versioning (`/docs/api/usage/versioning`), limits
  (`/docs/api/usage/limits`), access scopes (`/docs/api/usage/access-scopes`),
  protected customer data (`/docs/apps/launch/protected-customer-data`),
  bulk queries and imports (`/docs/api/usage/bulk-operations/queries`,
  `/imports`), custom data (`/docs/apps/build/custom-data`, the 2025-05-19
  changelog on app-owned metafield access), the schema proxy used by
  `@shopify/api-codegen-preset`, app distribution (`/docs/apps/launch/distribution`)
  and the 2026-01-01 custom-app changelog -- all under `https://shopify.dev`.
- Webhooks: subscribe, delivery structure, verify deliveries, ignore
  duplicates, HTTPS retry policy, the `WebhookSubscriptionTopic` enum
  (225 values in 2026-07), privacy-law compliance.
- Storefront and customers: the Storefront API reference, `@inContext`,
  the Cart object, bring-your-own-stack B2B, the Customer Account API
  getting-started, the legacy-customer-accounts deprecation (2026-02-26),
  customer privacy, Hydrogen migration notes.
- B2B: non-Plus B2B features (2026-04-02), manage catalogs, draft orders
  for companies, `BuyerExperienceConfiguration`, plan features.
- UNVERIFIED (carried from the research): authenticated-endpoint
  introspection as a documented statement; bucket sizes above Standard;
  bulk support for themes and payouts sub-connections; scope gating for
  menus, events, app installations, tender transactions; whether a
  custom-distribution app must request `read_all_orders`; a payload byte
  limit; an explicit compliance-topic exemption for custom apps; whether
  merchant pixels run on a non-Hydrogen SPA.
- Related records: J0 (`2026-08-22-data-origins-mirror-origin-native-design.md`),
  F (`2026-08-22-library-artifacts-and-deployables-design.md`), D (the
  hard-delete precedent).
