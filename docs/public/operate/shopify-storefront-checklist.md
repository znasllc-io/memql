---
title: The Shopify storefront completeness checklist
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# The Shopify storefront completeness checklist

What a headless storefront on MemQL has to build to lose nothing a Liquid
theme gave the store, and what it cannot have at all.

This is a checklist rather than a guide because the failure mode it exists
to prevent is a specific one: a headless build that is 90% complete on launch
day and discovers the last 10% one merchant complaint at a time. Work it
section by section against the store, and write a line for every app the
store has installed -- an integration plan, or an explicit "not carried".

The connector's side is [the Shopify connector](shopify-connector.md).

---

## 1. Channel and tokens

- [ ] The **Headless channel** is installed on the store. It is what defines
      the Storefront API scopes, and there is one per storefront.
- [ ] The **public** Storefront token is the one the browser uses. It is
      public by design and rate-limited per IP.
- [ ] The **private** Storefront token is for server-side calls, and it must
      be sent with the buyer's IP in `Shopify-Storefront-Buyer-IP` -- without
      it, every server-rendered request shares one rate-limit bucket and the
      storefront throttles itself under load.
- [ ] Both tokens are `globalSecret` rows; the store row references them and
      the site binding reads them from there.
- [ ] The Storefront API version is pinned and bumped deliberately, on the
      same quarterly rhythm as the mirror.

## 2. Catalog, search and cart

- [ ] Products, variants, collections, and the media Shopify holds for them.
- [ ] **Search** and **predictive search** through the Storefront API. The
      store's Search & Discovery settings -- synonyms, boosts, filters --
      apply to these endpoints, so a merchant's tuning carries over. A
      client-side filter over a product list does not, and losing it is one
      of the quieter regressions a headless rebuild makes.
- [ ] `@inContext(country:, language:)` on every catalog query, from the
      market the visitor resolved to. Prices, availability and translations
      are all context-dependent, and a query without it silently answers for
      the primary market.
- [ ] The **Cart API**: create, add, update, remove, note, attributes,
      discount codes, delivery options, and buyer identity.
- [ ] The hand-off is `cart.checkoutUrl`. **Checkout runs on Shopify,
      always.** There is no Checkout object and no way to build one.
- [ ] Cart state survives a reload and a device change to the extent the
      store expects (cart id in a cookie; a logged-in buyer's cart
      associated with their customer).

## 3. Customers

- [ ] The **Customer Account API**, not the legacy customer accounts:
      classic accounts were deprecated on 2026-02-26, and the Customer
      Account API requires new customer accounts to be enabled on the store.
- [ ] OAuth 2.0 / OIDC with **PKCE**, driven from the published discovery
      document rather than hard-coded endpoints.
- [ ] Scope `customer-account-api:full`.
- [ ] Profile, addresses, order history, returns, subscriptions and company
      contacts all come from that API. None of them come from the Storefront
      API, and none of them should come from the mirror -- the mirror is
      cluster-owner tier and a storefront request is a buyer's.
- [ ] Sign-in, sign-out and token refresh, including the case where a session
      expires mid-checkout.

## 4. B2B

- [ ] `customer.companyContacts → locations` drives a **location selector**.
      A buyer with two locations sees two sets of prices, and picking the
      wrong one is a wrong quote rather than a cosmetic error.
- [ ] `@inContext(buyer: {customerAccessToken, companyLocationId})` on
      **every** product, collection and search query once a location is
      chosen. This is the whole B2B pricing mechanism; a query that omits it
      returns retail prices to a wholesale buyer.
- [ ] `cartBuyerIdentityUpdate(companyLocationId:)` on the cart, before
      anything is added.
- [ ] The hosted **B2B checkout** carries PO number, payment terms, deposits,
      vaulted cards and "submit for approval". None of it is rebuildable.
- [ ] **Never cache a buyer-contextual response.** A CDN or an in-process
      cache keyed without the buyer context will serve one company's prices
      to another, and it will do it intermittently.

### What the storefront MAY cache

The prohibition above is the important half, and stated alone it reads as
"do not cache", which is not the policy and costs a storefront real
latency. The line is **buyer context**, not freshness.

**May cache, client-side, with a short TTL:** catalog, collection, search
and metaobject responses -- the reads that are the same for every visitor.
Key them on the query and its variables **and nothing else**: no customer
access token, no `companyLocationId`, no country or language from
`@inContext`.

**Never cache:** anything issued under a `customerAccessToken`, anything
carrying `@inContext(buyer:)`, cart state, and any response whose price
could differ per buyer. When in doubt about a field, the test is not "is
this sensitive" but "could two visitors legitimately get different
answers" -- if yes, it is buyer-contextual whatever it is called.

**Drop the cache on any identity change:** sign-in, sign-out, and a
location change on a B2B account. A cache that survives a sign-in shows
retail prices to a buyer who has just proved they get wholesale ones --
which reads as a pricing bug rather than as a stale cache, and is the exact
failure the prohibition above describes arriving through the front door.

**Add no server-side tier.** Shopify's own CDN does the server-side half
already, and a storefront reads the Storefront API directly by design (the
mirror is `clusterOwner`-tier and buyer requests must not read it -- see
[shopify-connector.md](shopify-connector.md)). A second server-side cache
in the storefront's path would be a place for buyer context to leak into a
shared key, with no freshness left to win.

No new machinery: this is a policy about the fetch layer the storefront
already has, not a component to build.

## 5. Platform plumbing

- [ ] `checkout.<domain>` points at Shopify, so the checkout hand-off stays
      on a first-party-looking host.
- [ ] `sitemap` and the `seo` fields on every resource are rendered. A
      headless storefront that does not is invisible to search.
- [ ] `UrlRedirect` rows are re-applied by the SPA. The merchant maintains
      them in the Shopify admin and they are mirrored; nothing applies them
      automatically once the theme is gone.
- [ ] Consent through the **Customer Privacy API**, and analytics wired
      without the Pixel Helper -- which is not compatible with headless.
- [ ] Metaobjects the storefront reads have `storefront: public_read` on
      their definition. A metaobject without it is invisible to the
      Storefront API however correct the query is.
- [ ] The `memql` namespace metafields the connector pushes are readable:
      `memql.description`, `.summary`, `.keywords` and `.blocks` are created
      with `public_read` storefront access.

## 6. What headless loses from a Liquid theme

Each of these works only inside a theme. A headless storefront has no theme,
so each needs a replacement or an explicit decision to go without.

| Lost | What to do instead |
|---|---|
| Theme app extensions, app blocks, app embeds | Call the app's own API, or embed its script directly. Most apps document a headless path; the ones that do not are the ones to find now rather than later. |
| ScriptTags | There is no host page to inject into. Same answer: the app's API, or its embed. |
| OS 2.0 sections and theme settings | Rebuilt in the SPA. Merchandising the merchant did by dragging sections becomes a content model somebody has to design. |
| The password page | Rebuilt, if the store uses one. |
| Liquid-rendered content blocks | Metaobjects with storefront access, or `v1:commerce:productContent` through the connector's push channel. |

## 7. What headless KEEPS

Worth stating, because the list above reads worse than the situation is:

- Checkout UI extensions and checkout branding.
- Shopify Functions (discounts, delivery and payment customisation, cart and
  checkout validation, order routing).
- Web pixels and the merchant's own analytics configuration.
- Shopify Flow.
- Search & Discovery configuration, which applies to the Storefront API's
  search endpoints.
- Policies, menus, pages, blogs, articles, metaobjects and SEO fields -- all
  available through the API and all mirrored.

## 8. The app inventory

The connector cannot mirror another app's data. App-owned metafields lost
public access on 2025-05-19, and an app's own database was never exposed.

Fill this in for the store, one row per installed app, before the storefront
is built:

| App | What it provides | Headless plan |
|---|---|---|
| _(reviews)_ | Product ratings and review content | Its own API / embed, or **not carried** |
| _(bundles)_ | Bundle definitions and pricing | Its own API, or Shopify Functions |
| _(wishlist)_ | Per-customer saved items | Its own API, or rebuilt on MemQL |
| _(forms)_ | Contact / wholesale application forms | Its own API, or rebuilt on MemQL |
| _(email capture)_ | Pop-ups and list sign-up | Its own embed, or MemQL campaigns |
| _(page builder)_ | Marketing page content | Metaobjects, or rebuilt |

A row with no plan is the function that goes missing on launch day. "Not
carried" is an acceptable answer; silence is not.

## 9. What nobody can carry

Repeated here because the storefront is where it is felt:

- Other apps' private data, in any form.
- Other apps' subscription contracts.
- Saved analytics reports.
- Checkout internals -- there is no Checkout object and checkout is
  Shopify-hosted.
- Gift-card codes (masked) and raw payment instruments (tokenised).

See [the connector's boundary section](shopify-connector.md#what-cannot-be-mirrored----by-anyone)
for the full statement and what each one means.
