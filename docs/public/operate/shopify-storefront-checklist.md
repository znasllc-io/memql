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
- [ ] `UrlRedirect` rows are re-applied by the storefront. The merchant maintains
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

## 6. What the edge admits

A storefront bundle served by MemQL runs under a content security policy the
edge writes for it. Before memql#5534 that policy was the generic one every
site kind got, and it named no store at all -- so the browser's call to the
Storefront API and every Shopify-hosted image were refused by the page's own
policy before either reached the network, with a console violation naming an
origin nobody had configured anywhere. Nothing in the bundle could fix it.

A `shopify_storefront` site is now served this, and no other kind is:

| Directive | Gains | Where it comes from |
|---|---|---|
| `connect-src` | `https://<storeDomain>` | the site's own `binding.storeDomain` |
| | `https://cdn.shopify.com` | fixed; Shopify's asset CDN, which its client SDKs also fetch from |
| | `https://shopify.com` | fixed; where the Customer Account API's OAuth token exchange and GraphQL endpoints resolve |
| | `https://monorail-edge.shopifysvc.com` | fixed; Shopify's analytics beacon |
| `img-src` | `https://cdn.shopify.com`, `https://<storeDomain>` | product and collection images |
| `media-src` | `https://cdn.shopify.com`, `https://<storeDomain>` | Shopify-hosted video. The directive exists **only** on a storefront policy; every other kind falls back to `default-src 'self'` |

The three fixed hosts are Shopify's own, taken from Hydrogen's default
directives rather than guessed, and they are the same for every store. The
variable one is the bound store, and it is read from the SITE ROW -- never
from a header, a query parameter, or anything the bundle supplies. A bundle
that could name a host in its own policy could name any host, which is the
same as having no policy.

- [ ] The binding's `storeDomain` is the host the storefront actually calls.
      A malformed value is DROPPED from the policy rather than interpolated
      into it, so a typo presents as "the store is refused", not as a broken
      header.
- [ ] Nothing in the storefront needs a **script** from a third party.
      `script-src` stays `'self'` plus the document's own inline hashes and
      is not widened for a storefront: admitting a third party's script host
      is the one widening that runs somebody else's code inside the
      storefront's origin, and nothing in the binding requires it.
- [ ] Checkout is reached by NAVIGATING to `cart.checkoutUrl`. A navigation
      is governed by neither `connect-src` nor `frame-src`, so nothing has to
      be admitted for it -- and `frame-ancestors` stays `'none'`.
- [ ] Any host the storefront reaches that is not in the table above -- a
      review app's API, an email capture embed -- is a gap to close before
      launch, not at launch. It appears in section 10's inventory.

### The resolution tail

What the edge answers for a path matching no file in the bundle is the
site's own choice (memql#5535), declared as `resolutionTail` on the
deployable, or in `memql-package.yaml`:

| `resolutionTail` | The edge answers |
|---|---|
| omitted | whatever the `kind` says: `spa` and `shopify_storefront` fall back to `index.html`, `static` answers 404 |
| `fallback` | `index.html`, so client-side routes survive a hard reload |
| `not_found` | 404, so a mistyped path in a multi-page build is visible |

- [ ] A storefront that renders its catalog in the BROWSER wants `fallback`,
      or every product route 404s on a hard reload. That is the default, so
      it needs no declaration.
- [ ] A storefront that PRERENDERS its pages wants `not_found`, or a mistyped
      path silently renders the home page and a crawler sees one page however
      many the build emitted.

Declaring `kind: static` to get the 404 is the wrong instrument and used to
be the only one: it also gives up the store binding, the storefront block in
the runtime document, and the policy above.

## 7. Bound at runtime, never at build time

The whole point of the runtime document is that ONE bundle serves more than
one deployable. A storefront is exercised against a development store and
then promoted to serve the real one, and the bundle does not change between
those two events -- only the binding does.

That only works if nothing about the store is baked in. A build step that
prerenders the catalog from a store's products produces a bundle that is
about THAT store, so the copy promoted to production serves the development
store's products under the production store's name, and every page looks
right.

- [ ] The store's domain and Storefront token are read from
      `GET /runtime-config.json` at load, from the `storefront` block. They
      are never in the bundle, never in an environment variable baked at
      build time, and never in a committed config file.
- [ ] Products, collections and prices are fetched from the bound store at
      RUNTIME. If pages are prerendered, what is prerendered is the SHELL --
      the route, the layout, the parts that are the same for every store --
      and the store's data arrives in the browser.
- [ ] Anything else the storefront needs per-deployable is a `settings` key
      on the site row, which the same document carries. Public by
      construction: the document is served unauthenticated to every visitor,
      so a value there is a value anybody can read.
- [ ] Promotion is checked by pointing the SAME bundle at both stores and
      confirming each serves its own catalog. A bundle that passes against
      one store proves nothing about promotion.

The Storefront token in that document is PUBLIC by Shopify's design --
scoped to unauthenticated storefront operations, rate-limited per IP, and
embedded in shipped client code by Shopify's own SDKs. The **Admin** token
is the opposite kind of credential and never appears in any served byte;
that is asserted by a test that greps the served document, and again by the
cluster-e2e leg against a document a real cluster really served.

## 8. What headless loses from a Liquid theme

Each of these works only inside a theme. A headless storefront has no theme,
so each needs a replacement or an explicit decision to go without.

| Lost | What to do instead |
|---|---|
| Theme app extensions, app blocks, app embeds | Call the app's own API, or embed its script directly. Most apps document a headless path; the ones that do not are the ones to find now rather than later. |
| ScriptTags | There is no host page to inject into. Same answer: the app's API, or its embed. |
| OS 2.0 sections and theme settings | Rebuilt in the storefront. Merchandising the merchant did by dragging sections becomes a content model somebody has to design. |
| The password page | Rebuilt, if the store uses one. |
| Liquid-rendered content blocks | Metaobjects with storefront access, or `v1:commerce:productContent` through the connector's push channel. |

## 9. What headless KEEPS

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

## 10. The app inventory

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

## 11. What nobody can carry

Repeated here because the storefront is where it is felt:

- Other apps' private data, in any form.
- Other apps' subscription contracts.
- Saved analytics reports.
- Checkout internals -- there is no Checkout object and checkout is
  Shopify-hosted.
- Gift-card codes (masked) and raw payment instruments (tokenised).

See [the connector's boundary section](shopify-connector.md#what-cannot-be-mirrored----by-anyone)
for the full statement and what each one means.
