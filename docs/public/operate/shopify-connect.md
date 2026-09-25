---
title: Connect Shopify through shared Connections
audience: public
status: draft
area: operate
sinceVersion: 0.23.0
owner: znas
---

# Connect Shopify

A Shopify storefront can go live for design review before a store is connected.
Its catalog, cart and checkout become available after connecting a store. An amber
marker on the Store icon indicates that setup remains.

## Connect and select a store

1. Open **Settings → Connections → Shopify**, or **Deployables → Settings → Shopify**.
   These surfaces use the same personal connections and controls.
2. Use the plus button and continue to Shopify. Select the store and approve
   access on Shopify. MemQL verifies Shopify's signed launch, completes
   authorization, and returns to the Settings surface that began the flow.
   Installing directly from Shopify opens global Settings → Connections.
3. Open the storefront's **Store** icon beside Traffic. Select a saved store,
   review the selection, and connect it to the storefront.

Shopify authorizes an app separately for each store. Connecting one store does not
list every store belonging to a Shopify identity. Add each store you need.

Users never enter app secrets or API tokens. MemQL seals the Admin authorization,
automatically obtains a Storefront token, and registers the connector's webhooks.
Connected storefronts also inherit the store's pinned API version in
`settings.storefrontApiVersion` of their runtime configuration. A nonblank site
setting can override that version; normal connection setup needs no manual entry.
A connection is separate from a deployment: removing a personal saved connection
keeps existing store bindings, credentials and deployed websites. Uninstalling the
app at Shopify is a different action and can revoke access there.

## Sandbox first, client store later

Connect the internal development store and the client's production store separately.
They can belong to different Shopify accounts and organizations. Each store's staff
must authorize the app; access to the internal sandbox grants no access to the client.
MemQL derives the **Sandbox** label from Shopify's `shop.plan.partnerDevelopment`,
not from a store's name or the email used to sign in.

A storefront has one active store binding. Start with the sandbox. When ready,
connect the client's store, open **Store → Change store**, and review the replacement
before confirming. Authorizing a new store never changes the active binding by
itself. Store selection does not copy products, customers, orders or credentials
between stores. Product handles and collections needed by the storefront must exist
in the selected store. Fylo keeps carts separate by store domain.

A production-store label describes the store type, not payment readiness. Verify
its products, payment configuration and checkout in Shopify before accepting orders.
Keep the internal sandbox for testing rather than transferring its generated data
to the client. Separate storefront deployments can keep sandbox testing available
alongside the client's live storefront.

## Operator prerequisites

Register the Shopify app before enabling merchant authorization. For a shared MemQL app used by unrelated merchants, use public distribution and
complete Shopify review. Custom distribution is restricted to one store or stores
within one Plus organization; it is not a substitute for multi-client distribution.
A development app and its Dev Dashboard install link are for sandbox testing only.

Use a Shopify-owned installation entry, with the app's **App URL** pointing to
`https://os.<domain>/`. MemQL does not request a manually entered shop domain.
Register the production application's own credentials and approved listing URL on
the production MemQL installation; do not rotate a working sandbox app into an
unrelated production registration. Shopify app distribution, approved permissions,
protected customer data requirements and public webhook reachability must all fit
the installation. The investigation and
expected deliverables are tracked in
[MemQL #5638](https://github.com/znasllc-io/memql/issues/5638).

The shared storefront flow requests `unauthenticated_read_product_listings`,
`unauthenticated_read_checkouts`, `unauthenticated_write_checkouts`,
`unauthenticated_read_customers`, `read_products`, `read_inventory`,
`read_locations`, and `read_orders`. Register those scopes on the app.
The complete Shopify mirror has additional optional scope requirements; connecting
a storefront does not request finance, staff, payment-method, or historical-order
access for that wider mirror.

The shared connector reads these operator-managed records:

| Record | Name | Value |
|---|---|---|
| Global variable | `SHOPIFY_CONNECT_CLIENT_ID` | Registered app's client ID |
| Global variable | `SHOPIFY_CONNECT_INSTALL_URL` | Reviewed Shopify App Store listing URL; for development only, the exact Install app link copied from the Dev Dashboard |
| Sealed global secret | `SHOPIFY_CONNECT_CLIENT_SECRET` | Registered app's client secret |

Use the installation's secret provisioning process. Never paste credentials into
GitHub issues, screenshots, repository files or browser settings. Each independent
installation must resolve its own authorized app configuration; published client
code and images contain no shared app secret.

Register `https://identity.<domain>/auth/shopify/callback` as the redirect URL.
Identity relays the unchanged signed query to the OS completion route so the
originating browser session can be checked. Local testing uses the equivalent
`identity.memql.localhost` and `os.memql.localhost` hosts. Shopify's server-to-server
webhooks additionally require a reachable HTTPS endpoint; local DNS alone does not
provide one. Complete the investigation before exposing a local tunnel or enabling
an app for live merchants.

## Authorization and refresh

`app:settings/connections` controls shared connection reads and management.
Developer and owner roles have it by default. Records are scoped to the current
MemQL user. Connecting an existing store refuses to replace another user's grant
or an app registered with a different client ID. The storefront's separate
`app:deployables/store` permission and row authorization still govern binding.

OAuth state is stored centrally, single-use and bound to the originating user and
browser session. Before creating OAuth state, the receiving node validates the full signed App URL
query and a five-minute timestamp window. It rejects duplicate parameters, unsigned
shop names and OAuth callbacks presented as app launches. The completion node
verifies Shopify's HMAC and shop again, and checks
current user permissions before saving anything. Settings callbacks never change
a storefront binding automatically.

The shared flow requests expiring offline tokens. The access token, refresh token,
app credentials and expiry times are sealed together in one secret record. Refresh
and reauthorization serialize through a PostgreSQL advisory lock across replicas.
A failed refresh stops Admin API use and requires recovery; it never reports the
failed renewal as success or exposes token response bodies.

See Shopify's [standalone authentication flow](https://shopify.dev/docs/apps/build/authentication-authorization/authenticate-standalone-apps)
and [expiring offline token guidance](https://shopify.dev/docs/apps/build/authentication-authorization/migrate-to-expiring-offline-access-tokens).

Production registration must follow Shopify's [distribution rules](https://shopify.dev/docs/apps/launch/distribution)
and [installation requirements](https://shopify.dev/docs/apps/launch/shopify-app-store/app-store-requirements).
Development-store detection uses [ShopPlan](https://shopify.dev/docs/api/admin-graphql/latest/objects/shopplan).
Local verification does not constitute Shopify approval or production readiness.
