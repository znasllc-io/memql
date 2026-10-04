---
title: Shopify connection investigation -- registration, distribution, approvals and handoff (memql#5638)
audience: ops
status: stable
area: ops
sinceVersion: 0.22.10
owner: znas
description: What a shared MemQL Shopify app needs from Shopify and from MemQL before unrelated merchants can connect stores, checked against Shopify's documentation on 2026-10-04.
---

# Shopify connection investigation (memql#5638)

Research and preparation for
[memql#5638](https://github.com/znasllc-io/memql/issues/5638). It covers what
Shopify requires before unrelated merchants can connect stores through MemQL's
shared Connections flow (shipped in v0.22.10), what MemQL already does, and
what is still missing. Nothing was registered, published, installed or exposed
to write it: the work was read-only.

The flow it describes is the one an ordinary user sees today:
**Settings > Connections > Shopify > + > Continue to Shopify** (also under
**Deployables > Settings**), store selection and approval on Shopify, and a
return to the surface that began it. Binding a storefront to a connected store
is a separate step on the storefront's **Store** panel. The operator side is
documented in [Connect Shopify](../../public/operate/shopify-connect.md) and
[The Shopify connector](../../public/operate/shopify-connector.md).

**Evidence labels used throughout.**

| Label | Meaning |
|---|---|
| `[Sn]` | Stated on the official Shopify page numbered `n` in [Sources](#sources). Every page was fetched on **2026-10-04**. Quoted text is verbatim. |
| `[code]` | Read in this repository at origin/main `37959fe6d`; the file is named beside the claim. |
| `[test]` | Held by a named test in this repository. |
| `[live 2026-09-25]` | Observed on the local sandbox and reported in the issue's comments. Not re-run for this document. |
| `[needs live exercise]` | Not established by any of the above. |

Where a Shopify page was silent on a question, this document says so. It does
not fill the gap with a guess.

---

## 1. Recommendation

1. **Register one public-distribution app for unrelated merchants, owned by the
   MemQL publisher's own Shopify organization.** It should be neither the
   sandbox organization nor a client's. Custom distribution cannot serve
   unrelated merchants. It installs on "a single Shopify store, on multiple
   stores that belong to the same Plus organization, or on transfer-disabled
   development stores", and "You can't change the distribution method after
   you select it" [S7]. A limited-visibility listing keeps the app
   installable by link without appearing in App Store search [S8].
2. **Configure it as MemQL already behaves.** That means a standalone app
   (`embedded = false`) on Shopify managed installation, using the
   authorization code grant with expiring offline tokens. The App URL is
   `https://os.<domain>/`, the one redirect URL is
   `https://identity.<domain>/auth/shopify/callback`, and the three
   compliance topics go to `https://api.<domain>/inbound/shopify`. The
   template is in [section 9](#9-app-configuration-template-shopifyapptoml).
3. **Keep the sandbox app as a separate registration** used only by sandbox
   installations against development stores. Each MemQL installation names
   exactly one registration through its three operator records. Never copy
   sandbox credentials or the sandbox's Dev Dashboard install link into the
   installation that serves merchants.
4. **Close the blocking MemQL gaps before submitting for review**
   ([section 13](#13-requires-memql-implementation-gaps-with-file-pointers)).
   They are: `app/uninstalled` handling, client-secret rotation, webhook
   subscriptions scoped to the grant, reinstall after `shop/redact`, and a
   supported way to provision the operator records. Also decide how MemQL's
   sign-in-before-OAuth step meets App Store requirement 2.3.2.
5. **Decide whether order sync ships at launch before submitting.** The
   submission should carry the eight scopes the code requests, which include
   `write_products` (the public doc still says `read_products`). `read_orders`
   reaches Orders, which Shopify classifies as protected customer data, so
   keeping it means a protected-customer-data request and meeting its
   requirements [S10]. Deferring it means moving it to `optional_scopes`,
   which needs MemQL work.

A single public app shared by *independent* MemQL installations is not
recommended. [Section 7](#7-who-holds-the-app-credentials) explains why: it
would need a hosted broker that holds every merchant's tokens and performs
every hourly refresh.

---

## 2. Status at a glance

| Item | State |
|---|---|
| Shared Connections flow, with no developer forms in Settings or Deployables | Shipped in v0.22.10 `[code]` `[test]`; exercised end to end on the sandbox through a simulated checkout `[live 2026-09-25]` |
| Expiring offline tokens: sealed pair, refresh, cross-replica serialization | Implemented `[code]` `[test]`; a refresh against Shopify itself is not recorded `[needs live exercise]` |
| Automatic Storefront token (`storefrontAccessTokenCreate`) | Implemented `[code]` `[test]` `[live 2026-09-25]` |
| Compliance webhooks on the app-level endpoint | Implemented `[code]` `[test]`; not registered on any merchant-facing app yet |
| `app/uninstalled` | **Not handled** `[code]` (gap G1) |
| Client-secret rotation | **Not supported without reconnecting every store** `[code]` (gap G2) |
| Public app registration, listing, review | Pending (Owner, Shopify) |
| Protected customer data request | Pending, and only if `read_orders` ships (Owner, Shopify) |
| Operator records on the merchant-facing installation | None set, per the issue comment of 2026-09-25 (Cluster operator) |
| Client store authorization, catalog, payments and checkout validation | Pending (Merchant staff) |

---

## 3. Verified findings

### 3.1 Authenticating a standalone app

- "Standalone and API-only apps run outside the Shopify admin, unlike embedded
  apps." and "Because they can't use ID tokens, they authenticate using the
  OAuth authorization code grant flow." [S1]. MemQL OS is standalone, and
  `shopifyAccountConnectBegin` answers an authorization-code URL
  (`integrations/shopify/connections.go`, `integrations/shopify/connect.go`
  `beginConnect`) `[code]`.
- The authorize URL carries `client_id`, `scope` ("A comma-separated list of
  access scopes your app needs"), `redirect_uri` and `state`. The redirect URI
  "Must exactly match a redirect URI you've configured in the Dev Dashboard or
  your app's configuration TOML file" [S1]. `&grant_options[]=per-user`
  requests an online token [S1]. MemQL sends no `grant_options`, so its token
  is an offline one (`connect.go` `connectAuthorizeURLWithScopes`) `[code]`.
- The callback checks are a `state` comparison, a shop matching
  `^[a-zA-Z0-9][a-zA-Z0-9\-]*\.myshopify\.com$`, and the HMAC: "Remove the
  `hmac` parameter from the query string, sort the remaining parameters
  alphabetically, and compute an HMAC-SHA256 hash using your client secret",
  compared in constant time [S1]. MemQL stores only the state's SHA-256 digest
  on a single-use row bound to the person and their browser session, with a
  ten-minute lifetime (`connect.go` `beginConnect`). It verifies the HMAC in
  `connect_callback.go` `validCallbackHMAC` and requires the signed `shop` to
  equal the state's shop `[code]` `[test]`
  (`TestTheCallbackSignatureMatchesShopifysWorkedExample`).
- The code is exchanged by `POST https://{shop}/admin/oauth/access_token` with
  `client_id`, `client_secret`, `code` and `expiring: '1'`. The expiring
  response carries `access_token`, `scope`, `expires_in`, `refresh_token` and
  `refresh_token_expires_in` [S1]. MemQL sends the same body and refuses a reply
  missing either token or either lifetime (`offline_token.go`
  `exchangeOffline`) `[code]` `[test]`
  (`TestOfflineExchangeRequiresCompleteExpiringPair`).
- **Not documented on the current page:** the request Shopify makes when it
  opens the App URL after an install. MemQL depends on that launch carrying
  `shop`, `hmac` and `timestamp` signed with the client secret, and admits it
  only within a five-minute window (`connections.go` `verifiedShopifyLaunch`)
  `[test]` (`TestShopifyInstallationRequiresRecentSignedLaunch`). It works
  `[live 2026-09-25]`, but none of the pages fetched for this document states
  it as a contract (limitation L3).

### 3.2 Managed installation and scopes

- `use_legacy_install_flow`: "When `true`, the legacy installation flow
  requests scopes through a URL parameter during the OAuth flow." When it is
  omitted or false, "scopes are saved in your app's configuration, and are
  automatically requested when the app is installed on a store." The page does
  not require an embedded app for this [S5].
- `optional_scopes` are "Any access scopes that your app can request
  dynamically after installation." [S5]
- A scope change needs a new released version: "When you update scopes in a
  new version of your app, updates aren't applied automatically to the stores
  your app is installed on." [S6]. Also: "If you add scopes later, merchants who
  already have your app installed approve the new ones the next time they open
  it." [S4]
- **Not found:** how Shopify treats an authorize-URL `scope` list that differs
  from the configured list under managed installation. Keep the two identical
  (question Q6).
- "Any scope that writes a resource also grants read access to it, so request
  the write scope only when your app needs both." [S11]
- `read_orders`: "These scopes cover orders created within the last 60 days.
  For older orders, add read_all_orders." [S11]. App Store requirements 3.2.1
  to 3.2.5 single out high-risk scopes, `read_all_orders` among them [S9].
  MemQL's managed list does not request `read_all_orders` `[code]`.

### 3.3 Expiring offline tokens

- "Starting April 1, 2026, all new public apps must request and use expiring
  offline access tokens." That change does not affect public apps created
  before that date or custom apps created at any time [S12]. Existing public
  apps follow later: "Public apps must use expiring offline access tokens for
  GraphQL Admin API requests by January 1, 2027." [S2], after which "Shopify
  rejects a GraphQL Admin API request that presents a non-expiring offline
  token." [S3]
- Lifetimes: the access token lasts 1 hour (`expires_in` is `3600`). The
  refresh token lasts 90 days (`refresh_token_expires_in` is `7776000`) "when
  issued" [S2].
- Rotation: "Every refresh returns a new access token and a new refresh token.
  Store both securely, and use the new refresh token in the next refresh
  request." and "Retired access tokens stay valid until their `expires_in`
  duration ends so in-progress requests can complete." [S2]
- Grace rule: the presented refresh token stays usable until the earliest of
  four events. Those are the app using the newer refresh token, the app
  acquiring a new token, "30 days after your app first used that token to
  refresh", and the original 90-day expiry [S2] [S13].
- Concurrency: "Don't do the two at the same time for the same store. Acquiring
  a token and refreshing one each retire the other's result, so one of the two
  tokens your app receives is already invalid when it arrives." [S2]. Also:
  "Serialize refresh operations for each shop. Persist each returned
  access-token and refresh-token pair atomically." [S13]
- Failure: "if a refresh token is expired, already replaced by one your app has
  since used, or otherwise invalid, Shopify returns `401 Unauthorized`." [S3]
- **The refresh request body is not printed on any page fetched.** The pages
  give the endpoint ("All grants exchange their inputs for an access token at
  the same OAuth token endpoint") and the grant type `grant_type=refresh_token`
  [S2] [S15]. MemQL sends `client_id`, `client_secret`,
  `grant_type=refresh_token` and `refresh_token` (`offline_token.go`
  `exchangeOffline`). That fits [S15]'s statement that a token "is pinned to
  the new secret when it refreshes", but it has not been exercised against
  Shopify `[needs live exercise]`.

**How MemQL meets this** `[code]` `[test]`:

- The whole pair is sealed as one secret row, `SHOPIFY_<ID>_OFFLINE_GRANT`:
  access token, refresh token, client id, client secret, scope and both
  deadlines (`offline_token.go` `offlineGrant`, `connect_write.go`). A reader
  can never see a new access token beside an old refresh token, which meets the
  "persist atomically" rule.
- Acquiring and refreshing take the same per-store PostgreSQL
  `pg_advisory_xact_lock` on every replica (`connections.go`
  `writeManagedConnection`, `offline_token.go` `adminToken`, `connect_gate.go`).
  That meets both concurrency rules. `TestSharedShopifyConnectionAcrossNodes`
  begins the flow on one engine, finishes it on another, then refreshes from
  both at once and asserts exactly one rotation.
- MemQL refreshes when less than one minute remains and asks for a reconnect
  once the refresh token has expired (`offline_token.go` `adminToken`). A seal
  that fails after Shopify has already refreshed is recoverable under the grace
  rule: the stored refresh token stays usable.

### 3.4 Client-secret rotation

From [S15]:

- The steps are "Generate a new secret", "Update webhook validation", "Update
  your app to use the new secret", "Replace access tokens minted under the old
  secret" and "Revoke the old secret". "Your old secret stays active until you
  revoke it."
- "An access token stays pinned to the secret that minted it". For expiring
  tokens: "Each store's token is pinned to the new secret when it refreshes. Let
  the refresh cycle run, or send a `grant_type=refresh_token` request for each
  store's stored refresh token to move every store at once."
- "Shopify signs webhooks with your app's oldest unrevoked client secret, so
  during rotation your webhooks keep being signed with the old secret until you
  revoke it." After revocation, "Any access tokens still pinned to the old
  secret stop working."
- In an emergency, "revoke the exposed secret immediately, before you generate
  and roll out a new one."
- Storage: "Never commit credentials to version control. Use environment
  variables or a secrets manager." and "Your client secret must never be exposed
  to the browser or included in client-side code."
- **Not stated:** which secret signs the OAuth callback and the App URL launch
  while two secrets are active (question Q5).

MemQL cannot follow this procedure today. See gap G2 and
[section 15](#15-secure-delivery-and-rotation-today).

### 3.5 Distribution and listing visibility

- Public distribution "Can be installed on multiple Shopify stores" and
  requires approval. Custom distribution is "Installed on a single Shopify
  store, on multiple stores that belong to the same Plus organization, or on
  transfer-disabled development stores", needs no approval, and "Can't charge
  merchants through Shopify's app billing system". "You can't change the
  distribution method after you select it." [S7]
- "All apps distributed through the Shopify App Store must have an app listing
  page on the Shopify App Store". Only fully visible apps are indexed in search,
  but "Merchants can install both fully visible and limited visibility apps from
  an app listing page that uses a Shopify App Store URL", and "You can change
  your app's visibility at any time." [S8]. MemQL's install-URL check accepts an
  `https://apps.shopify.com/<handle>` listing URL (`connections.go`
  `validManagedInstallURL`) `[test]`
  (`TestShopifyInstallationDestinationIsShopifyOwned`).
- **Not on the distribution page:** whether a public-distribution app can be
  installed on a live store before approval (question Q8). Before approval,
  test on development stores, where protected customer data is also reachable
  without review [S10].

### 3.6 App Store requirements that bear on MemQL

From [S9] unless noted:

- **2.3.1** "Apps must be installed and initiated only on Shopify services. Your
  app must not request the manual entry of a myshopify.com URL or a shop's
  domain during the installation or configuration flow." MemQL meets this. Its
  add button goes only to a Shopify-owned install URL, and the shop comes only
  from Shopify's signed launch `[code]` `[test]`.
- **2.3.2** "Your app must immediately authenticate using OAuth before any other
  steps occur. Merchants should not be able to interact with the user interface
  (UI) before OAuth." **This is an open risk.** MemQL requires a MemQL sign-in,
  plus the `app:settings/connections` capability, before it begins OAuth
  (`connections.go` `handleAccountConnectBegin`) (question Q1).
- **2.3.3** "Your app must redirect merchants to the user interface (UI) after
  they accept permissions access on the OAuth handshake page." MemQL meets this:
  the callback returns to the Settings surface that began the flow.
- **2.3.4** OAuth again "even if the merchant has previously installed and then
  uninstalled your app." This is partial. The flow repeats, but MemQL never
  learns of the uninstall (G1) and does not clear a redacted store (G4).
- **3.1.1** "Your app must have a valid TLS/SSL certificate without any errors."
  A cloud installation's front door issues real certificates. A local mkcert
  installation does not qualify.
- **3.2** "Your app must request only the access scopes that are necessary for
  your app to function properly." See the scope matrix in
  [section 4](#4-scope-matrix).
- **2.1.1 / 2.1.2, 4.5.4, 4.5.5**: the app must let review complete, and "If
  your app requires login credentials, then the credentials you provide for
  review must be valid, and grant full access to the app's complete feature
  set." Review therefore needs a MemQL account on the merchant-facing
  installation (question Q1).
- **4.5.1** "If your app meets Shopify's definition of a Sales Channel, turn your
  app into a Sales Channel and comply with all requirements before submitting
  your app for review." See section 3.9 and question Q2.
- Compliance: "Any app that you distribute through the Shopify App Store must
  respond to data subject requests, regardless of whether the app collects
  personal data." Missing URLs mean the app "will be rejected" [S16]. "Apps
  that are distributed through the Shopify App Store must subscribe to
  compliance webhooks." A submission needs a primary language, at least one
  listing, and an emergency developer email and phone number [S17].

### 3.7 Protected customer data

- Level 1 is "Customer data excluding name, address, phone, and email fields".
  Level 2 is "Customer data including name, address, phone, or email fields".
  Protected data explicitly includes "Orders, draft orders, abandoned checkouts,
  refunds, transactions, and other data that relate to a single customer." [S10]
- "Public apps request access to protected customer data and protected customer
  fields through the Partner Dashboard." For custom apps access is "Always
  available". "If your app is for testing or installed only on a development
  store, you can access customer data in development after Step 5. You don't
  need to submit for review." [S10]. The page names the Partner Dashboard, not
  the Dev Dashboard (question Q7).
- The request runs: Apps, choose the app, **API access requests**, **Protected
  customer data access**, **Request access**, select data and reasons, select
  fields, **Data protection details**, then submit for review [S10].
- Level 1 requirements include processing only the minimum data, informing
  merchants, honouring consent and opt-outs, retention periods, and "Encrypt
  data at rest and in transit". Level 2 adds encrypted backups, "Keep test and
  production data separate", data-loss prevention, limited staff access, strong
  passwords, an access log, and a security incident response policy [S10].
- Without approval: "GraphQL requests to unapproved types will return an HTTP
  200 Ok response with an error message in the errors hash." [S10]. The page
  says nothing about webhooks.

### 3.8 Storefront API access for an app

- `storefrontAccessTokenCreate` "Creates a storefront access token that
  delegates unauthenticated access scopes to clients using the Storefront API."
  and "Each shop can have up to 100 active StorefrontAccessToken objects."
  (2026-10) [S18].
- "A storefront access token inherits all of the unauthenticated access scopes
  from the app that creates it. If the app has not been granted any
  unauthenticated access scopes, then creating the storefront access token will
  fail." and "an application can have a maximum of 100 active Storefront access
  tokens per shop." [S19]
- The Storefront API's own authentication section lists four ways to get
  access: the Headless channel, a delegate token for a custom app, requesting
  unauthenticated scopes on an existing access token, and "Create a public
  access token with the storefrontAccessTokenCreate mutation on the GraphQL
  Admin API". Public tokens are "Used to query the API from a browser or mobile
  app, where the token is visible to buyers" (header
  `X-Shopify-Storefront-Access-Token`), and "Public access capacity scales with
  the number of buyers, based on their IP address." [S20]
- MemQL mints a token titled "MemQL storefront" on the first connect of a store
  with none, seals it, and points the store row at it (`connect_write.go`
  `mintStorefrontToken`) `[code]` `[test]`. The user never copies a token.
  memql-fylo's storefront sends it from the shopper's browser in
  `X-Shopify-Storefront-Access-Token` (memql-fylo `clients/storefront/src/lib/storefront.js`
  at main `225519f`).

### 3.9 Product publication and the sales-channel question

- A `Publication` is "A group of products and collections that are published to
  an app." and "Each publication manages which products and collections display
  on its associated Channel" [S21].
- `publishablePublishToCurrentChannel` publishes to "the current Channel
  associated with the requesting app. The system determines the current channel
  by the app's API client ID." It "Requires `write_publications` access scope"
  and is deprecated in favour of `publishablePublish` [S22]. Publishing to an
  app's own channel needs "the read_publications and write_publications access
  scopes" [S23]. MemQL requests neither, so it cannot publish products itself
  `[code]`.
- Storefront `product` query: "The Storefront API will automatically limit your
  query to products that are published in any applicable catalogs." and "If your
  app is a sales channel to which products can be published, then the Storefront
  API will only return products that are published both to your sales channel
  and the market you're querying for." [S24]
- Sales channels: "Only create a sales channel if you own and operate, or are
  contracted to represent, a distinct surface where merchants can promote or
  sell products outside Shopify." [S25]. A MemQL-hosted storefront is the
  merchant's own storefront rather than a surface MemQL operates, which argues
  against sales-channel classification. Confirm with Shopify before submitting
  (question Q2).
- Help Center: "When you add a new sales channel, all existing products are
  automatically made available to that channel." The admin activity log also
  records "Whether you activated the Storefront API for a custom app" [S26].
- **Not documented on any page fetched:** how a product becomes visible through
  a storefront token minted by a *non-sales-channel* public app. The sandbox
  catalog loaded through the minted token `[live 2026-09-25]`, but how those
  products were made available was not recorded (question Q3).

### 3.10 Customer accounts for headless storefronts

- "To start accessing the Storefront API and the Customer Account API, you need
  to install the Headless channel from the Shopify App Store." [S27]
- The Customer Account API needs "the Headless or Hydrogen sales channel" and
  "Shopify's customer accounts". It is configured in the Headless channel's
  Customer Account API settings, with callback URLs ("must be in the HTTPS
  scheme") and JavaScript origins. "Shopify doesn't support the use of localhost
  or any http based URL due to security concerns." [S28]
- "B2B only works with customer accounts." Contextualized B2B responses must not
  be cached [S29]. The Storefront `CustomerAccessToken` object "Requires
  `unauthenticated_read_customers` access scope." [S30]. `BuyerInput`, which
  carries the Customer Account API token into `@inContext`, states no scope
  requirement [S31] (question Q9).
- memql-fylo's trade sign-in runs a PKCE public client with scope
  `openid email customer-account-api:full`. Its client id is the site setting
  `customerAccountClientId` (memql-fylo `clients/storefront/src/lib/customer.js`,
  `config.js`). That client belongs to the merchant's Headless channel, not to
  MemQL's app (limitation L7).

### 3.11 Webhooks: compliance, uninstall, verification and delivery

- **Verification.** "Each HTTPS delivery includes a base64-encoded HMAC
  signature in the `X-Shopify-Hmac-SHA256` header". To check it, "compute
  HMAC-SHA256 of the raw request body using your app's client secret as the
  key" and compare in constant time [S32]. MemQL's receiver does exactly this
  under the `hmac-sha256-base64` scheme and answers `401` on any mismatch
  (`component/inbound/verify.go`, `component/inbound/handler.go`) `[code]`
  `[test]`.
- **Delivery.** "Shopify has a one-second connection timeout and a five-second
  timeout for the entire request". "Any response outside the 200 range,
  including 3XX codes, is treated as an error". "it retries 8 times over the
  next 4 hours". "After 8 consecutive failures, the subscription is
  automatically deleted if it was configured using the Admin API". Warning
  emails go to the app's emergency developer email. Deduplicate on
  `X-Shopify-Webhook-Id` [S32]. MemQL stages and answers `202` at once, keys the
  staged row on signed material, and recreates deleted API subscriptions in the
  daily reconcile `[code]`.
- **App-specific and shop-specific subscriptions.** App-specific subscriptions
  are "Defined in `shopify.app.toml` and applied uniformly across every shop that
  installs your app". They show as "config-managed" with "No ID", outside the
  Admin API. Compliance topics "Can be configured in your app configuration
  file. **Cannot** be subscribed to using the Admin API." Shopify recommends
  app-specific subscriptions "unless your topics, delivery URIs, or filters need
  to vary between shops." [S33]. MemQL's per-store mirror subscriptions
  legitimately vary by shop, because each store has its own delivery URL.
- **Compliance topics.** `customers/data_request` payloads carry `shop_id`,
  `shop_domain`, `orders_requested`, `customer` and `data_request`.
  `customers/redact` is sent "10 days after the deletion request" when the
  customer has no order in six months. `shop/redact` arrives "48 hours after a
  store owner uninstalls your app". "Complete the action within 30 days of
  receiving the request." The app must acknowledge "with a `200` series status
  code" and "must return a `401 Unauthorized` HTTP status" for an invalid HMAC
  [S16].
- **`app/uninstalled`**: "Occurs whenever a shop has uninstalled the app."
  (2026-10) [S34]. Uninstalling "triggers cleanup tasks in Shopify, including
  deleting any registered webhooks, script tags, and Shopify admin links" [S35],
  and "A merchant uninstalling your app, or you revoking the client secret, ends
  all of a token's access." [S2]. **Not stated on any page fetched:** whether an
  app-minted Storefront token survives an uninstall (question Q4).
- **Topic scopes.** "`orders/create` ... Requires at least one of the following
  scopes: read_orders, read_marketplace_orders." `customers/create` "Requires
  the `read_customers` scope." `inventory_levels/update` "Requires the
  `read_inventory` scope." [S34]

### 3.12 Local development networking

- `shopify app dev` uses Cloudflare Quick Tunnels by default. `--tunnel-url`
  names your own tunnel, and `--use-localhost` serves a local HTTPS certificate
  from `mkcert`. Localhost "isn't compatible with the Shopify features that
  directly invoke your app, such as Webhooks, App proxy, and Flow actions"
  [S36].
- `[build] automatically_update_urls_on_dev`: "When `true`, your app URL and
  redirect URLs will be automatically updated on `dev`." [S5]
- Separate apps from one codebase: "a file `shopify.app.{your-config-name}.toml`
  is generated". `shopify app config use` switches the default, `--config`
  overrides it, and "you can run the `deploy` command to create and release a
  new app version." [S37]
- What this means for MemQL. MemQL is not a CLI-scaffolded app and does not use
  `shopify app dev`. The App URL launch and the OAuth callback are *browser*
  redirects, so `.localhost` hosts work for them `[live 2026-09-25]`. Only
  Shopify's server-to-server webhooks need a public HTTPS endpoint, on these
  two paths:
  - `https://api.<domain>/inbound/shopify`: the app-level endpoint (compliance
    topics, and `app/uninstalled` once G1 lands).
  - `https://api.<domain>/inbound/shopify-<storeId>`: per-store mirror
    deliveries.

  MemQL composes both from `MEMQL_DOMAIN` (`integrations/shopify/connector.go`
  `deliveryURL`, `privacy_refusal.go` `appLevelDeliveryURL`). A tunnel in front
  of a `.localhost` installation therefore does not help on its own: the
  subscriptions MemQL registers would still name `api.<local domain>` (limitation
  L2). The supported arrangement is a sandbox installation on a real domain
  (`make up DOMAIN=<domain>`) whose `api.<domain>` front door is publicly
  reachable for `/inbound/*`. No tunnel was created for this investigation.

### 3.13 Store discovery

No page fetched documents an API that lets a third-party app list the stores a
person can reach. Authorization and tokens are per shop: the token endpoint is
`https://{shop}.myshopify.com/admin/oauth/access_token` [S2]. Store selection
happens on Shopify during install; the sandbox flow opened "Shopify's own store
picker, without manual domain entry" `[live 2026-09-25]`. MemQL should not build
a GitHub-style account grant. Each store is authorized and listed separately,
which is what the code does: one store row per domain-derived id, one personal
connection per store.

### 3.14 Development stores

`ShopPlan.partnerDevelopment` is "Whether the shop is a partner development shop
for testing purposes." (2026-10) [S38]. MemQL reads it at connect time and
stores it as `isDevelopment` (`connect_write.go` `shopPlan`) `[code]` `[test]`.
That shows a **Sandbox** label in the Store panel. A live-store label says
nothing about payment readiness.

---

## 4. Scope matrix

What the shared connection requests today is
`managedConnectScopes()` in `integrations/shopify/connections.go` `[code]`
`[test]` (`TestSharedShopifyConnectionAcrossNodes` pins the exact string). The
storefront's actual API use is from memql-fylo `clients/storefront` at main
`225519f`.

| Feature | Scope | Requested today | Need | Approval |
|---|---|---|---|---|
| Catalog, collections, search, predictive search (storefront) | `unauthenticated_read_product_listings` | yes | required | none |
| Cart and the checkout hand-off (`cartCreate`, `cartLines*`, `cartBuyerIdentityUpdate`) | `unauthenticated_read_checkouts`, `unauthenticated_write_checkouts` | yes | required | none |
| Trade (B2B) buyer context with a Customer Account API token | `unauthenticated_read_customers` | yes | conditional: B2B stores only; Q9 | none stated |
| Inventory counts on the storefront (`quantityAvailable`) | `unauthenticated_read_product_inventory` [S11] | no | not needed; the storefront reads `availableForSale` | none |
| Product content delivery (MemQL writes product metafields, namespace `memql`) | `write_products` (implies `read_products` [S11]) | yes, since 0.23.7 (`53b46ece8`) | required for content delivery | none |
| Inventory and location mirror | `read_inventory`, `read_locations` | yes | optional: no storefront call needs them | none |
| Order synchronization, last 60 days | `read_orders` | yes | optional: decide (Q10) | protected customer data, Level 1; Level 2 for names, emails, phones and addresses [S10] |
| Orders older than 60 days | `read_all_orders` | no | not requested | Shopify approval, plus requirement 3.2.x scrutiny [S9] [S11] |
| Customer records mirror | `read_customers` | no (full mirror only) | not part of storefront setup | protected customer data |
| Publishing products to the app | `read_publications`, `write_publications` [S23] | no | not requested; the merchant manages availability (Q3) | none |
| Compliance webhooks, `app/uninstalled` | none stated; declared in the app configuration | n/a | required | none |
| Webhook topics of the mirror | follow each topic's scope [S34] | all 163 topics registered | must follow the grant (G3) | as the topic's scope |

The complete mirror's 33-scope list (`generated.Scopes`,
[The Shopify connector](../../public/operate/shopify-connector.md), Step 2) is
not part of ordinary storefront setup and should not be in the public app's
scope list.

---

## 5. MemQL today versus what Shopify requires

| Shopify requirement | Source | MemQL today | Verdict |
|---|---|---|---|
| Authorization code grant for a standalone app | S1 | `connections.go`, `connect.go` | meets |
| Redirect URI registered exactly | S1 | `MEMQL_IDENTITY_BASE_URL` + `/auth/shopify/callback`; identity relays the signed query to the OS completion route (`component/identity/http/shopify_callback.go` `handleShopifyReturn`, `component/edge/identity_proxy.go`) | meets: register exactly this URL |
| State nonce, HMAC, shop validation | S1 | hashed single-use state bound to person and session; `validCallbackHMAC`; `NormalizeShopDomain` | meets |
| Request expiring offline tokens | S12, S1 | `expiring=1` (`offline_token.go`) | meets |
| Store both tokens, refresh before expiry; an invalid refresh token answers `401` | S2, S3 | one sealed row; refresh when under one minute remains; a failed refresh stops Admin calls, and an expired refresh token asks for a reconnect | meets |
| Serialize per shop; persist pair atomically; never acquire and refresh at once | S13, S2 | one per-store advisory lock for both | meets |
| Rotate the secret by refreshing with the new one; webhooks signed with the oldest unrevoked secret | S15 | the grant carries its own copy of the connect-time secret; the per-store webhook secret is another copy; the app-level endpoint verifies one secret | **gap G2** |
| Secret never in the browser | S15 | sealed `globalSecret`; `shopifyConnectionProviderStatus` answers only `configured` and `installUrl` | meets |
| Install starts on Shopify; no shop entry | S9 2.3.1 | install URL limited to Shopify hosts; shop only from the signed launch | meets |
| OAuth before any other step | S9 2.3.2 | MemQL sign-in precedes OAuth | **open: Q1** |
| Back to the app UI after OAuth | S9 2.3.3 | returns to the originating Settings surface | meets |
| OAuth again after reinstall | S9 2.3.4 | flow repeats; uninstall unseen; redacted store not cleared | **gaps G1, G4** |
| Request only necessary scopes | S9 3.2 | eight scopes, `read_orders` among them | decision: Q10 |
| Compliance webhooks: 200 ack, 401 on bad HMAC, done within 30 days | S16 | app-level endpoint; `202`; `401`; jobs (data request on receipt, redact after 24 h, shop purge after a 48 h hold) | meets; registration pending |
| Answer within 5 s; deduplicate | S32 | stage, then `202`; row id from signed material | meets |
| API subscriptions deleted after 8 failures | S32 | daily reconcile recreates them | meets |
| Subscribe only to topics the grant covers | S34 | all 163 generated topics for every store | **gap G3** |
| Handle `app/uninstalled` | S34, S35 | ignored as an unmirrored topic | **gap G1** |
| Protected customer data for orders | S10 | not requested | approval pending, or defer `read_orders` |
| Storefront token without a manual step | S18, S19 | minted on first connect; kept, not re-checked, on reconnect | meets; **gap G5** |
| Products available to the app | S21 to S26 | MemQL cannot publish (no publications scopes) | **open: Q3** |

---

## 6. Known limitations

- **L1. One App URL per app version.** `application_url` is a single value, "The
  URL of your app." [S5]. A public app can send newly installed merchants to one
  MemQL installation only (section 7).
- **L2. Webhooks cannot reach a `.localhost` installation, and a tunnel alone
  does not fix that.** Webhook URLs are derived from `MEMQL_DOMAIN`
  (section 3.12). A local sandbox can exercise install, authorization, refresh,
  Storefront tokens and catalog/cart, but not webhook delivery.
- **L3. The App URL launch signature is undocumented** on the pages fetched
  (section 3.1). If Shopify changes it, Shopify-initiated installs stop working
  while every unit test stays green. Keep a live check in the release checklist.
- **L4. No store discovery.** Each store is installed and authorized on its own
  (section 3.13).
- **L5. Refresh happens on demand.** A store with no Admin API call for 90 days
  must reconnect. The daily subscription reconcile ("On boot and daily at
  03:15", [The Shopify connector](../../public/operate/shopify-connector.md),
  Step 4) touches every *ingesting* store. A paused store is skipped
  (`subscriptions.go` `EnsureSubscriptions`), so its refresh token can lapse.
- **L6. Every Admin call for a managed store takes the per-store advisory lock**
  (`offline_token.go` `adminToken`), even when no refresh is due. That is
  correct, and it serializes a store's Admin calls across replicas at the cost
  of one database transaction per call.
- **L7. Trade sign-in is outside the app's authorization.** It needs the
  merchant's Headless channel, new customer accounts, and a Customer Account API
  client whose id is entered as the site setting `customerAccountClientId`
  [S27] [S28]. That is not a secret, but it is still a manual setup step.
- **L8. Installation from the listing needs an existing MemQL account.** A
  merchant arriving from the App Store lands on MemQL sign-in. Someone with no
  account on the installation, or without `app:settings/connections`, cannot
  finish. Invite first, then install, and prefer a limited-visibility listing.
- **L9. The earlier per-storefront path is still callable.**
  `shopifyStoreAppSave` takes a client id and secret, and
  `shopifyStorefrontTokenSet` takes a pasted token. Both remain declared under
  `execute app:deployables/store` (`dsl/shopify/overlay/builtins.memql`). The OS
  no longer calls either, so they are outside the ordinary flow, but they are not
  retired (G9).
- **L10. A store belongs to the person who connected it.** If a second MemQL
  user authorizes the same store at Shopify, MemQL refuses to replace the first
  person's grant (`connections.go` `managedTarget`, answered as
  `permission_lost`). That is deliberate, since an OAuth proof does not
  authorize changing another user's deployment credentials. It also means a
  teammate reuses that store only through deployables the first person binds.

---

## 7. Who holds the app credentials

**Today: one operator-configured app per installation** `[code]`. The three
records are `v1:platform:globalVariable` and `v1:platform:globalSecret` rows,
read by name on the node that handles each request (`connections.go`
`managedApp`). The secret is sealed under `MEMQL_MASTER_KEY` and only ever read
server-side. The callback is the installation's own identity host, and the
completion route is proxied on the OS origin, so the host-only session cookie
binds the browser that began the flow.

| Record | Kind | Value |
|---|---|---|
| `SHOPIFY_CONNECT_CLIENT_ID` | global variable | the registered app's client id |
| `SHOPIFY_CONNECT_CLIENT_SECRET` | sealed global secret | the registered app's client secret |
| `SHOPIFY_CONNECT_INSTALL_URL` | global variable | the reviewed `https://apps.shopify.com/<handle>` listing URL; on a sandbox installation only, the exact Dev Dashboard install link |

**Why not one shared app behind a hosted broker.** Three facts decide it. An app
has one App URL [S5]. A token is pinned to the secret it was minted or refreshed
under [S15], so whoever refreshes it needs the app secret. Every webhook is
signed with the app secret [S32]. A shared
app serving independent installations would therefore need a broker, operated
by the publisher, that would:

- receive every install and callback;
- hold every merchant's token pair, and refresh each one hourly;
- verify every webhook, and relay grants and deliveries to the right
  installation.

That is a new and high-value service, and it moves token custody out of the
installations. It is not recommended now.

**Recommended.** The publisher's own installation uses the publisher's public
app. Any other independent installation registers its own app and enters it
through its own operator records. That app uses custom distribution when it
serves one store or one Plus organization [S7], and public distribution
otherwise. A broker is worth revisiting only if many independent installations
need the publisher's listing.

**Return context** `[code]` `[test]`. The state row carries the return path
(validated by `SafeRelativeRedirect`), a ten-minute expiry, and the person and
session it was begun by (`connect.go` `beginConnect`). The OS remembers the
originating surface (Settings or Deployables) for 30 minutes. A
Shopify-initiated install opens global Connections
(`clients/os/src/modules/connections/shopifyInstallation.ts`). Connecting a
store never changes a storefront binding.

---

## 8. Registration and approval steps

Who performs each step:

| Role | Who |
|---|---|
| **Owner** | administers the MemQL publisher's Shopify organization (today: znas) |
| **Cluster operator** | an owner or developer on the MemQL installation concerned |
| **MemQL engineering** | changes in this repository |
| **Shopify** | App Review and protected-customer-data review |
| **Merchant staff** | may install apps on the store |
| **MemQL user** | a signed-in person holding `app:settings/connections` (developer and owner roles by default) |

**A. Sandbox registration (exists).**

1. **Owner.** Release a new version of the sandbox app whose scopes include
   `write_products`. The code has requested it since `53b46ece8` (0.23.7), and
   the registered version `memql-local-sandbox-1` predates that. Scope updates
   "aren't applied automatically to the stores your app is installed on" [S6].
2. **Merchant staff** (the owner, for the sandbox store). Approve the new scope
   when Shopify asks [S4]. Then confirm the store row's `scopesGranted` includes
   `write_products`.
3. **Cluster operator.** Keep the sandbox's Dev Dashboard install link only in
   sandbox installations' `SHOPIFY_CONNECT_INSTALL_URL`.

**B. Public registration (pending).**

1. **Owner.** Choose the owning organization: the publisher's. Make sure the
   account holds app development permissions [S6] [S40].
2. **Owner.** In the Dev Dashboard, go to **Apps**, **Create app**, **Start from
   Dev Dashboard**, and name the app [S6].
3. **Owner.** Configure and release the first version from the
   [section 9](#9-app-configuration-template-shopifyapptoml) template. Either
   use Shopify CLI (`shopify app config link`, then `shopify app deploy`, which
   creates "and release[s] a new app version" [S37]), or the Dev Dashboard
   version form (App URL, webhooks API version, scopes [S6]). Keep the filled
   file with the installation that owns `<domain>`, never in this repository.
4. **MemQL engineering.** Close the gaps marked *blocks review* in section 13,
   and settle Q1 and Q2.
5. **Cluster operator.** On the merchant-facing installation, set
   `SHOPIFY_CONNECT_CLIENT_ID` and `SHOPIFY_CONNECT_CLIENT_SECRET` through the
   approved secret path (G6, section 15). These two are enough for the
   compliance endpoint and for Shopify-initiated installs. The in-product +
   button stays hidden until step 10.
6. **Owner.** Choose public distribution, which cannot be changed afterwards
   [S7], and the listing visibility [S8].
7. **Owner.** Prepare the listing and submission: primary language, a listing,
   an emergency developer email and phone [S17], a screencast, and review
   credentials (4.5.3 to 4.5.5 [S9]). For the credentials, prefer a per-user
   grant of `app:settings/connections` over a developer role (Q1).
8. **Owner.** If `read_orders` ships, request protected customer data access
   (section 3.7) [S10].
9. **Owner** submits through the automated checks [S17]; **Shopify** reviews
   [S7].
10. **Cluster operator.** After approval, set `SHOPIFY_CONNECT_INSTALL_URL` to
    the listing URL. **Settings > Connections > Shopify > +** then works.
11. **Merchant staff.** Install from the listing, select the store and approve.
12. **MemQL user.** Connect the store from Settings, then bind it on the
    storefront's **Store** panel (Testing or Production, then **Connect
    store**).
13. **Merchant staff.** Make products available to the app (Q3), configure
    payments, and validate checkout in Shopify. For trade sign-in, also set up
    the Headless channel and the Customer Account API client, with callback
    `https://<storefront host>/account/callback`, and hand the client id to the
    deployable's `customerAccountClientId` setting (L7).

---

## 9. App configuration template (`shopify.app.toml`)

Sanitized. Angle-bracket values are per registration, and no value here is a
credential. The client secret is never in this file.

```toml
# shopify.app.toml -- sanitized template for memql#5638. Angle-bracket values are per registration.
# Keep the filled copy with the installation that owns <domain>, never in the engine repository.

client_id = "<client ID from the Dev Dashboard, App settings>"
name = "<app name merchants see>"
application_url = "https://os.<domain>/"
embedded = false

[access_scopes]
# Shopify managed installation: these scopes are requested when a store installs the app.
scopes = "unauthenticated_read_product_listings,unauthenticated_read_checkouts,unauthenticated_write_checkouts,unauthenticated_read_customers,write_products,read_inventory,read_locations,read_orders"
use_legacy_install_flow = false

[auth]
redirect_urls = [ "https://identity.<domain>/auth/shopify/callback" ]

[webhooks]
# Follow the Admin API version the mirror is generated from (integrations/shopify/generated).
api_version = "2026-07"

  [[webhooks.subscriptions]]
  compliance_topics = [ "customers/data_request", "customers/redact", "shop/redact" ]
  uri = "https://api.<domain>/inbound/shopify"

# Add only once MemQL handles the topic on the app-level endpoint (gap G1):
#  [[webhooks.subscriptions]]
#  topics = [ "app/uninstalled" ]
#  uri = "https://api.<domain>/inbound/shopify"

[build]
# Stops `shopify app dev` from rewriting the registered URLs to a tunnel.
automatically_update_urls_on_dev = false
```

| Field | Same for every registration? | Notes |
|---|---|---|
| `client_id` | no | One per registration. Each MemQL installation's `SHOPIFY_CONNECT_CLIENT_ID` must equal it. |
| `name` | no | e.g. a sandbox name versus the listed name. |
| `application_url` | no | The installation's OS host, `https://os.<domain>/`. The OS captures the signed launch query when it loads (`clients/os/src/main.tsx`). |
| `embedded` | yes, `false` | MemQL OS runs outside the Shopify admin. |
| `scopes` | yes | Identical to `managedConnectScopes()`. The authorize URL sends the same list (Q6). If `read_orders` is deferred, move it to `optional_scopes`, which needs MemQL work (Q10). |
| `use_legacy_install_flow` | yes, `false` | Shopify managed installation [S5]. |
| `redirect_urls` | no | Exactly `https://identity.<domain>/auth/shopify/callback`. The OS completion route `/auth/shopify/complete` is internal and is **not** registered. |
| `[webhooks] api_version` | yes | Moves with the quarterly regeneration of the mirror. |
| compliance `uri` | no | `https://api.<domain>/inbound/shopify`, the app-level endpoint, never a per-store `/inbound/shopify-<storeId>`. `apply.go` refuses privacy topics on a managed store's per-store URL, and `privacy_refusal.go` reports each refusal. Must be publicly reachable over HTTPS. |
| per-store mirror webhooks | not in the file | MemQL registers them through the Admin API at `https://api.<domain>/inbound/shopify-<storeId>`, because the URL differs per store [S33]. |

---

## 10. Setup matrix

| Item | Sandbox registration | Public registration |
|---|---|---|
| Purpose | Testing against development stores only | Unrelated merchants' live stores |
| Owning organization | Dev organization `230894035` (existing) | The MemQL publisher's organization (pending; Owner) |
| App and distribution type | Standalone Dev Dashboard app, installed through the Dev Dashboard install link | Standalone, public distribution, App Store listing (limited visibility allowed) [S7] [S8] |
| MemQL installation | A sandbox installation (today the local k3d cluster) | The merchant-facing installation only |
| App URL | `https://os.<sandbox domain>/` (today `https://os.memql.localhost/`) | `https://os.<domain>/` |
| OAuth redirect URL | `https://identity.<sandbox domain>/auth/shopify/callback` | `https://identity.<domain>/auth/shopify/callback` |
| Completion route (internal, not registered) | `https://os.<sandbox domain>/auth/shopify/complete` | `https://os.<domain>/auth/shopify/complete` |
| Compliance webhooks | Need a public `api.<sandbox domain>`; unreachable on `.localhost` (L2) | `https://api.<domain>/inbound/shopify` |
| `app/uninstalled` | After G1, same endpoint | After G1, `https://api.<domain>/inbound/shopify` |
| Per-store mirror webhooks | Registered by MemQL; delivered only on a public sandbox domain | Registered by MemQL at `https://api.<domain>/inbound/shopify-<storeId>` |
| Storefront API | Token minted by Connect from the app's unauthenticated scopes; no Headless channel needed for catalog and cart | Same. Headless channel only for trade sign-in (L7) |
| Scopes | Same eight; the current version needs `write_products` (step A1) | Same eight, subject to Q10 |
| `SHOPIFY_CONNECT_INSTALL_URL` | The exact Dev Dashboard install link (`admin.shopify.com/?organization_id=...&no_redirect=true&redirect=...`) | `https://apps.shopify.com/<handle>` after approval |
| Credentials | Sealed on the sandbox installation | Set on the merchant-facing installation (Cluster operator; G6) |
| Required approvals | None. Protected customer data reachable in development [S10] | App Review [S7]; protected customer data if `read_orders` ships [S10] |
| Who performs | Owner (registration), Cluster operator (records), Owner as sandbox merchant | Owner, Cluster operator, MemQL engineering, Shopify, Merchant staff (section 8) |

---

## 11. Sandbox identifiers versus what is pending

Non-secret identifiers only, as recorded in the issue's comments on 2026-09-25.

| Identifier | Sandbox (exists) | Public (pending) |
|---|---|---|
| Shopify organization | Dev organization `230894035` ("My Store") | Publisher's organization |
| App | **Fylo Sandbox**, app id `427932057601` | not created |
| Dev Dashboard | https://dev.shopify.com/dashboard/230894035/apps/427932057601 | n/a |
| Client id (public) | Not recorded in the issue. It is in the Dev Dashboard's App settings and in the sandbox installation's `SHOPIFY_CONNECT_CLIENT_ID` | n/a |
| Active version | `memql-local-sandbox-1`: standalone, local OS App URL, local identity callback, scopes registered with `read_products` | n/a |
| Development store | `fylo-sandbox.myshopify.com` (generated test data, not a client's store) | The client's store: pending merchant authorization |
| Distribution | Not recorded; installed through the Dev Dashboard install link on a development store | Public, pending review |
| Verified on it | Install from the + button, return to the same Settings surface, storefront binding, reconnect updating one connection, catalog of 8 products and 17 variants, cart, and test order #1001 with no real payment | none |
| Not verified on it | Webhook delivery, refresh against Shopify, uninstall and reinstall, a second store from another account | none |

---

## 12. Handoff checklist

**Confirmed**

- [x] Standalone app, authorization code grant, offline token (section 3.1) `[code]` `[test]`
- [x] Expiring offline tokens requested; pair sealed in one row; refresh and acquire serialized per store across replicas `[code]` `[test]`
- [x] Ordinary users enter no client id, secret, token, scope, callback or shop domain (section 14.1) `[code]` `[test]` `[live 2026-09-25]`
- [x] Storefront token minted automatically `[code]` `[test]` `[live 2026-09-25]`
- [x] Compliance topics implemented on `https://api.<domain>/inbound/shopify`, with the signed `shop_domain` binding the store and `401` on a bad HMAC `[code]` `[test]`
- [x] Webhook HMAC over the raw body, base64, constant time `[code]` `[test]`
- [x] Custom distribution cannot serve unrelated merchants; distribution cannot be changed later [S7]
- [x] Callback `https://identity.<domain>/auth/shopify/callback` relayed to the OS completion route; App URL `https://os.<domain>/`

**Requires registration** (Owner, Cluster operator)

- [ ] Sandbox app: new version with `write_products`; re-approve on the sandbox store (steps A1, A2)
- [ ] Public app in the publisher's organization, configured from section 9 (B1 to B3)
- [ ] Public distribution, listing visibility, listing, emergency contact, review credentials (B6, B7)
- [ ] Operator records on the merchant-facing installation (B5, B10)
- [ ] Where the filled `shopify.app.toml` files live (Q11)

**Requires Shopify approval** (Shopify)

- [ ] App Store review of the public app [S7]
- [ ] Protected customer data, Level 1 (and Level 2 if names, emails, phones or addresses are read), if `read_orders` ships [S10]
- [ ] Confirmation that MemQL's app is not a sales channel (Q2), and of its review sign-in path (Q1)

**Requires MemQL implementation** (MemQL engineering; details in section 13)

- [ ] G1 `app/uninstalled` *(blocks review)*
- [ ] G2 client-secret rotation *(blocks review)*
- [ ] G3 subscriptions scoped to the grant *(blocks review)*
- [ ] G4 reinstall after `shop/redact` *(blocks review)*
- [ ] G5 Storefront token re-check on reconnect
- [ ] G6 a supported path to provision and rotate the operator records *(blocks launch)*
- [ ] G7 public doc and sandbox scope drift (`read_products` versus `write_products`)
- [ ] G8 decision on 2.3.2 (sign-in before OAuth)
- [ ] G9 retire or restrict the legacy per-storefront builtins
- [ ] G10 `customers/redact` coverage of MemQL-origin rows and earlier exports

**Requires merchant authorization** (Merchant staff)

- [ ] Each store's staff install the app and approve its scopes. Every store is authorized separately; a sandbox authorization grants nothing on a client's store
- [ ] Products available to the app (Q3); payments configured; checkout validated in Shopify
- [ ] For trade sign-in: Headless channel, new customer accounts, a Customer Account API public client with the storefront's callback and origin (L7)
- [ ] Storefront binding confirmed by a MemQL user on the Store panel (Testing or Production)

---

## 13. Requires MemQL implementation: gaps with file pointers

Found by reading the code at `37959fe6d`. None is implemented here. Each one
needs a failing test first.

- **G1. `app/uninstalled` is not handled.** *Blocks review.*
  - `integrations/shopify/apply.go` `Apply` drops any topic missing from
    `generated.Topics`, and `apply_test.go`
    (`TestApplyIsANoOpForAnUnknownStoreAndAnUnmirroredTopic`) pins
    `app/uninstalled` as that no-op.
  - The app-level endpoint binds a store only for compliance topics
    (`managed_webhooks.go` `managedComplianceStore`). An `app/uninstalled`
    declared in the TOML today would be staged, then dropped as "a delivery for
    an unknown store".
  - After an uninstall, the store row keeps a dead grant and a Storefront token
    reference. The edge keeps answering `connectionState: "connected"`, because
    it checks only the domain and token reference
    (`component/edge/runtimeconfig.go` `storefrontForSite`). Shoppers then meet
    Shopify's refusal ("The store refused this storefront's token" in
    memql-fylo), and the personal connection still reads **Connected**.
  - Needed: accept `app/uninstalled` on the app-level endpoint, bound by the
    signed body's shop domain and the store's `appClientId` as compliance
    deliveries already are. Then mark the store disconnected so the edge reports
    `unavailable` and the Store panel says why, audit it, and leave
    `shop/redact` to purge.
- **G2. Client-secret rotation forces every store to reconnect.** *Blocks review.*
  - `offline_token.go` `offlineGrant` embeds `ClientSecret`, and `adminToken`
    refreshes with that embedded copy. Refreshing never re-pins a token to a new
    secret [S15].
  - `connect_write.go` (`promoted` covers the managed source) seals another copy
    as `SHOPIFY_<ID>_WEBHOOK_SECRET`. `store.go` `InboundSourceFor` verifies
    per-store deliveries against that copy, and `managed_webhooks.go`
    `managedInboundSource` verifies app-level deliveries against
    `SHOPIFY_CONNECT_CLIENT_SECRET` alone.
  - Needed: refresh managed grants with the *current* app secret, with the
    embedded one as fallback. Verify deliveries against both the current and
    the previous secret during a rotation window. Resolve managed stores'
    per-store verification from the app secret rather than a frozen copy, or
    re-seal the copies on rotation.
- **G3. Webhook subscriptions ignore the grant.** *Blocks review.*
  - `subscriptions.go` `EnsureSubscriptionsForStore` registers all 163
    `generated.SubscribedTopics` for every store, on connect, on boot and daily.
  - For a store holding the eight managed scopes, 97 of the 163 route to
    concepts whose generated scope list the grant does not cover: `read_customers`
    28, fulfillment orders 17, `read_returns` 9, subscription contracts 7,
    discounts 5, and others. That figure is measured from
    `integrations/shopify/generated/`. A topic's own scope can differ from its
    concept's, so treat it as approximate.
  - Those creations should be refused [S34], and each refusal lands on the
    store's `health.subscriptions.failed` and repeats daily `[needs live
    exercise]`: read the sandbox store's health.
  - `reconcile.go` `scopesMissingFor` already makes this decision for
    reconciliation. Reuse it for subscriptions.
- **G4. Reinstall after `shop/redact` leaves the store paused and redacted.**
  *Blocks review.*
  - `markStoreRedacted` sets `status: "paused"` and `redactedAt`
    (`dsl/shopify/overlay/mutations.memql`).
  - `connections.go` `managedTarget` refuses only `prior.Status == "redacted"`,
    a value the status enum does not contain (`dsl/shopify/overlay/concepts.memql`).
    That guard can never fire.
  - A managed reconnect of a purged store therefore writes fresh credentials
    through `updateStore`, which touches neither field, and reports `connected`
    while the store stays paused and redacted. The legacy resolver would refuse
    it as `store_redacted`.
  - Needed: decide between a named refusal and clearing the redaction (status
    back to `configured`) on a verified reinstall, then fix the guard.
- **G5. A reconnect keeps the old Storefront token unchecked.**
  - `connect_write.go` step 13 mints only when `StorefrontTokenRef` is empty.
  - If an uninstall revoked the app-minted token (Q4), the storefront stays
    broken after a reinstall.
  - Needed: re-check the token (`storefrontTokenWorks`) on reconnect, and
    re-mint when Shopify refuses it, within the 100-token cap [S18].
- **G6. No supported way to provision or rotate the three operator records.**
  *Blocks launch.*
  - They are absent from `scripts/secrets/manifest.yaml`, so `go run
    ./scripts/secrets seed` ignores them.
  - There is no owner-or-developer configure surface for them like email's
    `integrationConfigure` (`integrations/email/configure.go`).
  - The OS tells users "Your cluster administrator needs to finish Shopify app
    registration." (`clients/os/src/modules/connections/ShopifyConnections.tsx`),
    but the administrator has no in-product path. A secret must be sealed
    server-side under `MEMQL_MASTER_KEY`.
  - Needed: manifest entries (global; the secret as a secret), or a configure
    builtin with server-side sealing, plus the operator doc.
- **G7. Scope drift.**
  - `docs/public/operate/shopify-connect.md` lists `read_products`. The code
    has requested `write_products` since `53b46ece8`.
  - The sandbox version predates the change (step A1).
  - A managed connection refuses only a missing *Storefront* scope
    (`connections.go` `writeManagedConnection`). A grant without
    `write_products` connects, and product-content delivery fails later.
- **G8. Sign-in before OAuth versus requirement 2.3.2.**
  - `handleAccountConnectBegin` requires a signed-in caller holding
    `app:settings/connections` and a browser session before any OAuth step. A
    reviewer installing from the listing lands on MemQL sign-in.
  - Either justify it with review credentials, or redesign so OAuth runs first
    and a signed-in user claims the grant within a bounded window. The redesign
    is a security design question about binding, not a small change.
- **G9. The legacy per-storefront builtins are still declared**
  (`dsl/shopify/overlay/builtins.memql`: `shopifyStoreAppSave`,
  `shopifyConnectBegin`, `shopifyStorefrontTokenSet`). They accept developer
  credentials and pasted tokens. Retire them or restrict them to the cluster
  owner.
- **G10. `customers/redact` covers the generated mirror only.**
  - `compliance.go` `RedactCustomer` walks `generated.ApplyOrder`.
  - MemQL-origin concepts the connector pushes (`connector.go` `originDomains`,
    for example `v1:commerce:customerNote`) are outside that walk.
  - Earlier `customers/data_request` exports filed in the Library are personal
    data with no stated retention.
  - Check whether those rows hold the customer's personal data, and give
    exports a retention period [S10].

---

## 14. Acceptance evidence

### 14.1 The ordinary-user flow needs no developer credentials or pasted tokens

- **The UI offers nothing to type.** The add flow is a single **Continue to
  Shopify** action that navigates to the operator-configured install URL
  (`clients/os/src/modules/connections/ShopifyConnections.tsx`
  `AddShopifyConnection`). Tests in `clients/os/test/deployables/shopifyConnect.test.tsx`:
  "opens Shopify installation without requesting a store address or
  credentials", and "shows operator setup as unavailable without a developer
  credential form".
- **The server takes no credential from the caller.** `shopifyAccountConnectBegin`
  accepts only `signedQuery` (Shopify's launch) and `returnPath`. The shop comes
  only from a verified, recent launch (`connections.go` `verifiedShopifyLaunch`;
  `TestShopifyInstallationRequiresRecentSignedLaunch` rejects unsigned, forged,
  duplicated, stale, future-dated and callback-shaped queries). The app's client
  id and secret are read server-side from operator records (`managedApp`), and
  the readiness call answers only `configured` and `installUrl`.
- **The OS forwards the launch rather than trusting it.**
  `clients/os/test/settings/shopifyInstallation.test.ts`: "sends the signed
  launch to the server instead of trusting the selected shop", "refuses a forged
  authorization destination", and "does not intercept OAuth callbacks".
- **Tokens are obtained and sealed without the user.** The code is exchanged,
  the Storefront token minted, and the personal connection recorded in one
  server-side callback (`connect_write.go`, `connections.go`
  `writeManagedConnection`). `recordExternalConnection` is `@serverOnly`, and a
  client call to it or a raw insert is refused in
  `TestSharedShopifyConnectionAcrossNodes`.
- **Cross-node.** `TestSharedShopifyConnectionAcrossNodes` begins on one engine,
  verifies and writes on another, and refreshes concurrently from both with a
  single rotation. `TestConnectShopifyBeginsOnOneEngineAndFinishesOnAnother`
  covers the hop for the per-storefront path.
- **Session binding.** `TestShopifyReturnUsesTheOSSessionAndPreservesTheSignedQuery`,
  `TestACallbackFromAnotherBrowserSpendsNothing` and
  `TestABadSignatureLeavesTheStateUnspent`.
- **Live.** "MemQL Deployables Settings > Shopify > + opens Shopify's own store
  picker, without manual domain entry" `[live 2026-09-25]`.

### 14.2 One authorized store serves several permitted apps and deployables

- **One record set for every surface.** The connection is a platform-level
  personal record (`v1:platform:externalConnection`, `@rowAuthz(owner=...)`), read
  through `externalConnectionsMine` under `app:settings/connections`.
  **Settings > Connections** and **Deployables > Settings** render the same
  `ShopifyConnections` component over the same records. Removing a personal
  connection changes no store credential and no binding: the final assertion of
  `TestSharedShopifyConnectionAcrossNodes`, and the OS test "disconnects a
  personal selection without changing a store or site".
- **No one-site-per-store limit.** The store is its own row, keyed by its
  domain, and a site's binding is a reference to it. `updateSiteStoreBinding`
  sets `binding.storeId` with no uniqueness check, and `sitesBoundToStore`
  returns every site whose serving or preview binding names the store
  (`dsl/platform/mutations.memql`, `dsl/platform/queries.memql`).
  `TestAClusterOwnerMayChangeTheTokenOfAStoreInUse` uses two sites bound to one
  store. The OS tests "can use the same sandbox for Testing while retaining
  Production" and "offers a sandbox on Production without changing Testing".
- **Still to be exercised live:** two *different* deployables bound to one store
  `[needs live exercise]`. Reuse is per person: a second MemQL user cannot add a
  store someone else connected (L10).

### 14.3 Test checklist

| Case | Covered by | Status |
|---|---|---|
| Approval (happy path) | `TestSharedShopifyConnectionAcrossNodes` | `[test]` with fake Shopify; `[live 2026-09-25]` |
| Merchant declines on Shopify | none; Shopify's behaviour on decline is not documented on the pages fetched | `[needs live exercise]`. Expected: the state expires after ten minutes and nothing is written |
| Expired or spent state | `TestAShopifyStateThatCannotFinishIsRefusedAndWritesNothing` (state_expired, consumed, unknown) | `[test]` |
| Callback from another browser, or an expired session | `TestACallbackFromAnotherBrowserSpendsNothing` | `[test]` |
| Forged or stale App URL launch | `TestShopifyInstallationRequiresRecentSignedLaunch` | `[test]` |
| Token refresh, two replicas | `TestSharedShopifyConnectionAcrossNodes` | `[test]`; against Shopify `[needs live exercise]` |
| Refresh token expired | `offline_token.go` asks for a reconnect; no test found | `[needs live exercise]` and a unit test |
| Reconnect of the same store | `TestAReconnectUpdatesTheStoreInPlace` (per-storefront path) | `[test]`; managed path `[live 2026-09-25]` ("exactly one active saved selection remains") |
| Insufficient Storefront scopes | `TestOnlyAMissingStorefrontScopeRefuses` | `[test]` |
| Person lacks `app:settings/connections`, or the store is another person's | `managedTarget`, `managedPerson`; no managed-path test found | add a test |
| Multiple stores from different accounts | none | `[needs live exercise]` |
| Uninstall, then reinstall | not handled (G1, G4, G5) | needs implementation, then a live exercise |
| Storefront changes its selected store | OS tests in `shopifyConnect.test.tsx` (keep binding on failure, clear old store display); binding guard tests in `component/memql` | `[test]`; switch to a client store `[needs live exercise]` |
| Webhook delivery and compliance requests end to end | `managed_webhooks_test.go`, `compliance_test.go` | `[test]`; delivery from Shopify `[needs live exercise]` (needs a public domain, L2) |

---

## 15. Secure delivery and rotation today

- **Never** put a client secret, an access or refresh token, an authorization
  code, a cookie or a populated environment file in an issue, a screenshot, a
  repository or the browser. Shopify says the same [S15].
- **Source of truth.** The owner's secret manager holds the client secret.
  Naming it is the owner's call (Q12). A cluster operator reads the secret there
  and writes it once to the installation, where it is sealed under
  `MEMQL_MASTER_KEY` as `SHOPIFY_CONNECT_CLIENT_SECRET`. The client id and
  install URL are not secrets.
- **Delivery mechanism.** None is supported in the product today (G6). Until G6
  lands, provisioning means writing the sealed row server-side, as was done on
  the sandbox installation. Record who did it and when.
- **Rotation today.** Because of G2, the procedure Shopify describes ([S15],
  section 3.4) cannot keep stores connected. Updating MemQL to a new secret
  before revoking the old one makes app-level privacy deliveries fail, since
  Shopify keeps signing with the oldest unrevoked secret. Revoking the old one
  kills every token pinned to it. Until G2 lands, every rotation disconnects
  every store:
  1. **Planned rotation.** The **Owner** generates the new secret at Shopify.
     The **Cluster operator** sets it on the installation. The **Owner** then
     revokes the old secret at once. **Leaked secret:** revoke first, as Shopify
     directs [S15], then generate and set.
  2. Every connected store's user reconnects (**MemQL user**). Each reconnect
     re-seals that store's grant and its webhook secret copy, and re-registers
     its subscriptions.
  3. Deliveries refused in the gap are retried by Shopify, 8 times over 4 hours
     [S32]. A per-store subscription that keeps failing is deleted after 8
     consecutive failures and comes back when the store reconnects.

---

## 16. Open questions

| # | Question | Who answers |
|---|---|---|
| Q1 | Does App Review accept a MemQL sign-in before OAuth (2.3.2), with review credentials holding a per-user `app:settings/connections` grant? Or must OAuth come first (G8)? | Owner, with Shopify |
| Q2 | Is MemQL's app a "sales channel" under 4.5.1? The definition [S25] suggests not. | Owner, with Shopify |
| Q3 | How do products become visible through a storefront token minted by a non-sales-channel app? Check on the sandbox: the product's **Sales channels and apps** list, and a product created after install. | Owner (sandbox) |
| Q4 | Does an app-minted Storefront token survive uninstall and reinstall? | Owner (sandbox) |
| Q5 | Which client secret signs the OAuth callback and the App URL launch while two secrets are active? | Shopify docs or support |
| Q6 | Under managed installation, what does Shopify do with an authorize-URL `scope` that differs from the configured scopes? | Shopify docs or support |
| Q7 | Where does a Dev Dashboard app request protected customer data? [S10] names the Partner Dashboard. | Owner |
| Q8 | Can a public-distribution app be installed on a live store before approval? | Shopify docs or support |
| Q9 | Does the B2B buyer context need `unauthenticated_read_customers`? `BuyerInput` states no scope [S31]. | Live check with a B2B store |
| Q10 | Ship order sync at launch (`read_orders` plus protected customer data), or defer it to `optional_scopes` (needs MemQL support for dynamic scope requests)? | Owner |
| Q11 | Which Shopify organization owns the public app, and where do the filled `shopify.app.toml` files live (the installation's repository, not this one)? | Owner |
| Q12 | Which secret manager is the source of truth for the client secret, and who may read it? | Owner |

---

## Sources

All pages were accessed on **2026-10-04**. Where a requested URL redirected, the
page actually read is listed.

| # | Page | URL |
|---|---|---|
| S1 | Authenticate a standalone or API-only app (the authorization-code-grant URL redirects here) | https://shopify.dev/docs/apps/build/authentication-authorization/authenticate-standalone-apps |
| S2 | Access tokens | https://shopify.dev/docs/apps/build/authentication-authorization/access-tokens |
| S3 | Migrate to expiring offline access tokens | https://shopify.dev/docs/apps/build/authentication-authorization/migrate-to-expiring-offline-access-tokens |
| S4 | Authentication for apps built with Shopify CLI (the app-installation URL redirects here) | https://shopify.dev/docs/apps/build/authentication-authorization/cli-app-authentication |
| S5 | App configuration (`shopify.app.toml` reference) | https://shopify.dev/docs/apps/build/cli-for-apps/app-configuration |
| S6 | Create apps using the Dev Dashboard | https://shopify.dev/docs/apps/build/dev-dashboard/create-apps-using-dev-dashboard |
| S7 | About app distribution | https://shopify.dev/docs/apps/launch/distribution |
| S8 | App listing visibility | https://shopify.dev/docs/apps/launch/distribution/visibility |
| S9 | App Store requirements | https://shopify.dev/docs/apps/launch/shopify-app-store/app-store-requirements |
| S10 | Work with protected customer data | https://shopify.dev/docs/apps/launch/protected-customer-data |
| S11 | Access scopes | https://shopify.dev/docs/api/usage/access-scopes |
| S12 | Changelog, 2026-03-20: Expiring offline access tokens required for new public apps as of April 1, 2026 | https://shopify.dev/changelog/expiring-offline-access-tokens-required-for-public-apps-april-1-2026 |
| S13 | Changelog, 2026-08-28: More resilient refreshes for expiring offline access tokens | https://shopify.dev/changelog/more-resilient-refreshes-for-expiring-offline-access-tokens |
| S14 | Changelog, 2025-12-10: Offline access tokens now support expiry and refresh | https://shopify.dev/changelog/posts/offline-access-tokens-now-support-expiry-and-refresh |
| S15 | Manage your app credentials (the rotate-revoke URL redirects here) | https://shopify.dev/docs/apps/build/authentication-authorization/manage-credentials |
| S16 | Privacy law compliance | https://shopify.dev/docs/apps/build/compliance/privacy-law-compliance |
| S17 | Submit your app for review | https://shopify.dev/docs/apps/launch/app-store-review/submit-app-for-review |
| S18 | `storefrontAccessTokenCreate` (GraphQL Admin, 2026-10) | https://shopify.dev/docs/api/admin-graphql/latest/mutations/storefrontAccessTokenCreate |
| S19 | StorefrontAccessToken (REST Admin, legacy) | https://shopify.dev/docs/api/admin-rest/latest/resources/storefrontaccesstoken |
| S20 | Storefront API reference, Authentication (2026-10) | https://shopify.dev/docs/api/storefront |
| S21 | `Publication` object (GraphQL Admin, 2026-10) | https://shopify.dev/docs/api/admin-graphql/latest/objects/Publication |
| S22 | `publishablePublishToCurrentChannel` (GraphQL Admin, 2026-10) | https://shopify.dev/docs/api/admin-graphql/latest/mutations/publishablePublishToCurrentChannel |
| S23 | Product and variant publishing | https://shopify.dev/docs/apps/build/sales-channels/product-publishing |
| S24 | Storefront `product` query (2026-10) | https://shopify.dev/docs/api/storefront/latest/queries/product |
| S25 | Apps as sales channels | https://shopify.dev/docs/apps/build/sales-channels |
| S26 | Help Center: Activity logs | https://help.shopify.com/en/manual/shopify-admin/activity-logs |
| S27 | Bring your own headless stack | https://shopify.dev/docs/storefronts/headless/bring-your-own-stack |
| S28 | Getting started with the Customer Account API | https://shopify.dev/docs/storefronts/headless/building-with-the-customer-account-api/getting-started |
| S29 | Headless with B2B | https://shopify.dev/docs/storefronts/headless/bring-your-own-stack/b2b |
| S30 | `CustomerAccessToken` (Storefront, 2026-10) | https://shopify.dev/docs/api/storefront/latest/objects/CustomerAccessToken |
| S31 | `BuyerInput` (Storefront, 2026-10) | https://shopify.dev/docs/api/storefront/latest/input-objects/BuyerInput |
| S32 | Verify webhook deliveries (the HTTPS-delivery URL redirects here) | https://shopify.dev/docs/apps/build/webhooks/verify-deliveries |
| S33 | Manage webhook subscriptions | https://shopify.dev/docs/apps/build/webhooks/subscribe |
| S34 | `WebhookSubscriptionTopic` enum (GraphQL Admin, 2026-10) | https://shopify.dev/docs/api/admin-graphql/latest/enums/WebhookSubscriptionTopic |
| S35 | Sunsetting your app (the uninstall-app URL redirects here) | https://shopify.dev/docs/apps/launch/distribution/sunsetting-your-app |
| S36 | Select a networking option for local development | https://shopify.dev/docs/apps/build/cli-for-apps/networking-options |
| S37 | Manage app config files | https://shopify.dev/docs/apps/build/cli-for-apps/manage-app-config-files |
| S38 | `ShopPlan` object (GraphQL Admin, 2026-10) | https://shopify.dev/docs/api/admin-graphql/latest/objects/ShopPlan |
| S39 | Getting started with the Storefront API | https://shopify.dev/docs/storefronts/headless/building-with-the-storefront-api/getting-started |
| S40 | Dev Dashboard | https://shopify.dev/docs/apps/build/dev-dashboard |

[S39] was read and documents the Headless channel route only: "Install the
Headless channel from the Shopify App Store. On installation, click **Create
storefront** to generate public and private access tokens". It does not cover
app-minted tokens. That is why section 3.8 relies on [S18] to [S20].

[S1]: https://shopify.dev/docs/apps/build/authentication-authorization/authenticate-standalone-apps
[S2]: https://shopify.dev/docs/apps/build/authentication-authorization/access-tokens
[S3]: https://shopify.dev/docs/apps/build/authentication-authorization/migrate-to-expiring-offline-access-tokens
[S4]: https://shopify.dev/docs/apps/build/authentication-authorization/cli-app-authentication
[S5]: https://shopify.dev/docs/apps/build/cli-for-apps/app-configuration
[S6]: https://shopify.dev/docs/apps/build/dev-dashboard/create-apps-using-dev-dashboard
[S7]: https://shopify.dev/docs/apps/launch/distribution
[S8]: https://shopify.dev/docs/apps/launch/distribution/visibility
[S9]: https://shopify.dev/docs/apps/launch/shopify-app-store/app-store-requirements
[S10]: https://shopify.dev/docs/apps/launch/protected-customer-data
[S11]: https://shopify.dev/docs/api/usage/access-scopes
[S12]: https://shopify.dev/changelog/expiring-offline-access-tokens-required-for-public-apps-april-1-2026
[S13]: https://shopify.dev/changelog/more-resilient-refreshes-for-expiring-offline-access-tokens
[S14]: https://shopify.dev/changelog/posts/offline-access-tokens-now-support-expiry-and-refresh
[S15]: https://shopify.dev/docs/apps/build/authentication-authorization/manage-credentials
[S16]: https://shopify.dev/docs/apps/build/compliance/privacy-law-compliance
[S17]: https://shopify.dev/docs/apps/launch/app-store-review/submit-app-for-review
[S18]: https://shopify.dev/docs/api/admin-graphql/latest/mutations/storefrontAccessTokenCreate
[S19]: https://shopify.dev/docs/api/admin-rest/latest/resources/storefrontaccesstoken
[S20]: https://shopify.dev/docs/api/storefront
[S21]: https://shopify.dev/docs/api/admin-graphql/latest/objects/Publication
[S22]: https://shopify.dev/docs/api/admin-graphql/latest/mutations/publishablePublishToCurrentChannel
[S23]: https://shopify.dev/docs/apps/build/sales-channels/product-publishing
[S24]: https://shopify.dev/docs/api/storefront/latest/queries/product
[S25]: https://shopify.dev/docs/apps/build/sales-channels
[S26]: https://help.shopify.com/en/manual/shopify-admin/activity-logs
[S27]: https://shopify.dev/docs/storefronts/headless/bring-your-own-stack
[S28]: https://shopify.dev/docs/storefronts/headless/building-with-the-customer-account-api/getting-started
[S29]: https://shopify.dev/docs/storefronts/headless/bring-your-own-stack/b2b
[S30]: https://shopify.dev/docs/api/storefront/latest/objects/CustomerAccessToken
[S31]: https://shopify.dev/docs/api/storefront/latest/input-objects/BuyerInput
[S32]: https://shopify.dev/docs/apps/build/webhooks/verify-deliveries
[S33]: https://shopify.dev/docs/apps/build/webhooks/subscribe
[S34]: https://shopify.dev/docs/api/admin-graphql/latest/enums/WebhookSubscriptionTopic
[S35]: https://shopify.dev/docs/apps/launch/distribution/sunsetting-your-app
[S36]: https://shopify.dev/docs/apps/build/cli-for-apps/networking-options
[S37]: https://shopify.dev/docs/apps/build/cli-for-apps/manage-app-config-files
[S38]: https://shopify.dev/docs/api/admin-graphql/latest/objects/ShopPlan
[S39]: https://shopify.dev/docs/storefronts/headless/building-with-the-storefront-api/getting-started
[S40]: https://shopify.dev/docs/apps/build/dev-dashboard
