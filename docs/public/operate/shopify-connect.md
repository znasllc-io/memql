---
title: Connect Shopify -- a storefront's store, connected from its Store panel
audience: public
status: stable
area: operate
sinceVersion: 0.23.0
owner: znas
---

# Connect Shopify

**Audience:** whoever sets up a client's Shopify store for a storefront deployed
on this cluster -- a developer or a cluster owner -- and anyone answering "why
does the Store panel say Not connected".
**Design:** `docs/superpowers/specs/2026-09-23-connect-shopify-design.md`,
section 12 and decisions D1-D15.

A storefront deployable (`kind: shopify_storefront`) is created as a draft with
no store, and it cannot go live until a store is connected. Connect Shopify is
how the store's credentials get into the cluster: the person saves the store's
Shopify app on the storefront's **Store** panel, presses **Connect Shopify**,
approves on Shopify, and comes back to a connected store. MemQL seals the keys,
writes the store record, mints a Storefront token, attaches the store to the
storefront and registers the webhooks -- nobody pastes an Admin token anywhere.

**Who can.** Anyone holding `execute` on `app:deployables/store` who can write
the storefront: developers and cluster owners, by default. The shop is never
taken from the browser. Every call names the storefront, and the server works
out the shop from the package run that last published it (its manifest's
`binding.store`), so a person who can write one storefront cannot point it, or
its credentials, at another shop.

**It is not a sign-in.** Nothing here changes who a person is in this cluster;
Shopify's approval is proof that the person is staff on that shop, and that is
all it is used for. The design is the same shape as
[GitHub Connect](github-connect.md), and rides the same single-use state row.

---

## Once per store, in Shopify

A client store gets its own **custom-distribution app**, created once in your
organization's Shopify Dev Dashboard. An app installed on many stores needs App
Store review, and a reviewed public app may not start installation outside
Shopify, so one app per store is the route that works.

1. In the Dev Dashboard, create an app with custom distribution for
   `<store>.myshopify.com`.
2. Set both its **App URL** and its **redirect URL** to
   `https://identity.<domain>/auth/shopify/callback`, where `<domain>` is this
   cluster's domain.
3. Turn **embedding off**.
4. Add the scopes below as **required** scopes.
5. Release the version, then copy the app's **client ID** and **client
   secret**.

The client secret is one secret with three uses: it signs Shopify's callback,
it is sent in the code exchange, and it signs every webhook the app delivers.

### The scopes

One list, in `integrations/shopify` (`ConnectScopes()`), returned to the Store
panel by `shopifyConnectStatus` and pinned by a test.

**Storefront scopes, required.** Without them Connect refuses with
`scopes_missing`, because the storefront's catalog and cart would fail:

| Scope | Covers |
|---|---|
| `unauthenticated_read_product_listings` | Products, collections, search, predictive search, product metafields. Also required for minting the Storefront token |
| `unauthenticated_read_checkouts` | Cart reads |
| `unauthenticated_write_checkouts` | Cart creation and every cart edit |
| `unauthenticated_read_customers` | The trade buyer-context reads (`@inContext(buyer: ...)`) |

**Admin read scopes, requested and recorded, never refused.** The mirror and
its webhooks read with these. They are `generated.Scopes` in
`integrations/shopify/generated/model.go`, listed in
[the connector's step 2](shopify-connector.md#step-2----create-the-app). A
missing one does not block Connect: the store row records what Shopify granted
(`scopesGranted`) and the Store panel shows what is missing.

No write scopes are requested.

---

## On the Store panel

MemQL OS -> Deployables -> the storefront -> **Store**. The panel reads its
state from `shopifyConnectStatus`, never from the binding:

| State | Meaning |
|---|---|
| Not connected | No app saved for the store, or saved and not yet approved |
| Connected | The store has an Admin token (`adminTokenRef` is set) |

### 1. Save the app

Enter the client ID and the client secret, then **Save**
(`shopifyStoreAppSave`). The secret is sealed on the server with
`MEMQL_MASTER_KEY` and neither value is ever returned.

**A save is PENDING, and changes no store (D12).** The client ID goes into the
`v1:platform:globalVariable` `SHOPIFY_<ID>_PENDING_CLIENT_ID` and the sealed
secret into the `v1:platform:globalSecret` `SHOPIFY_<ID>_PENDING_CLIENT_SECRET`
(`<ID>` is the store id, uppercased, hyphen kept: `acme-widgets.myshopify.com`
is `SHOPIFY_ACME-WIDGETS_...`). The store row, its live app and the webhook
secret it verifies deliveries with are untouched. Saving proves nothing about
the shop, so it can move nothing that verifies webhooks; only an approval by
the shop's own staff promotes a pending app.

### 2. Connect Shopify

**Connect Shopify** (`shopifyConnectBegin`) writes a single-use state bound to
you and to the browser session you pressed it from, good for ten minutes, and
sends the browser to Shopify's approve page for
the shop the server resolved. It asks Shopify to approve the pending app when
one is saved, otherwise the store's current app. It asks for an offline token,
which a custom-distribution app keeps without expiry, so there is nothing to
refresh.

The shop's staff approve, and Shopify sends the browser to
`GET /auth/shopify/callback` on the identity node. The callback checks, in this
order, and stops at the first that fails:

1. The request is HTTPS.
2. No code and no state is Shopify opening the App URL (the install link). The
   browser goes to MemQL OS with `shopify=installed`; nothing is read, written
   or echoed.
3. The state is looked up **without being spent**, and it must have been begun
   from this browser's signed-in session -- GitHub Connect's check, through the
   same HTTP-only refresh cookie. An approval finished in another browser is
   `connect_state_invalid` and spends nothing.
4. Shopify's signature verifies under the client secret the state names.
5. `shop` is the state's shop.
6. Only now is the state spent, exactly once, under a Postgres advisory lock --
   so a forged request cannot burn a Connect somebody began.
7. The storefront is still a storefront, and still publishes that shop.
8. You are judged again, now, under your account's real role: still active,
   still holding the store part (your own, and at the storefront's
   organization), the store readable, the storefront writable. On a first
   Connect there is no store row yet, so the store's read tier is asked
   instead: whoever could not read the store the connection creates is refused
   here, before anything is written.
9. The code is exchanged for the Admin token, in a POST body.
10. Shopify granted every Storefront scope.

Then the writes, every one keyed by the store id, so a failure between two of
them is repaired by pressing Connect again:

11. The Admin token is sealed as `SHOPIFY_<ID>_ADMIN_TOKEN`. On an approval of
    the pending app, its secret is sealed as `SHOPIFY_<ID>_WEBHOOK_SECRET` --
    the name the environment seed uses for the same value. That is the secret
    the code was exchanged with, never whatever the pending row holds by then,
    so a Save made while Shopify was approving cannot become the live secret.
12. The store row is created if there is none, otherwise updated in place:
    domain, `appClientId` (on a promotion), `adminTokenRef`,
    `webhookSecretRef`, `apiVersion` (the pinned version), `scopesGranted`,
    and `plan` (Shopify's plan name). `ownerUserId` -- whose Library receives
    `customers/data_request` exports -- is set to you only when the row names
    nobody, and is never overwritten. The pending pair is then cleared, if it
    is still the pair Shopify approved; a Save made since stays pending for its
    own approval.
13. If the store has no Storefront token, one is minted (titled
    `MemQL storefront`), sealed as `SHOPIFY_<ID>_STOREFRONT_TOKEN` and pointed
    at straight away.
14. The store is attached to the storefront, judged as you. A binding already
    naming the store is left alone, and the storefront stays a draft.
15. The webhooks are registered, after the browser has been sent back -- one
    Admin call per subscribed topic is too long to keep a person waiting --
    and the result lands on the store's subscription health, which the Store
    panel shows. A failure here does not undo the connection.

One audit event records the outcome: category `configuration`, target
`shopifyStore`, action `shopify_connected` or `shopify_reconnected` whenever the
store's credentials changed (steps 11 and 12 landed) -- a success even when a
later step could not finish, whose token rides the event's `reason` -- and
`shopify_connect_refused` with its reason when nothing was kept. The browser
returns to the Store panel with `?shopify=<result>&site=<storefront>`.

### 3. Optionally, paste a Storefront token

**Storefront token** (`shopifyStorefrontTokenSet`) replaces the minted token
with one you paste -- a Headless channel token, for instance. It is checked
with one Storefront API request before it is sealed. An empty token clears the
reference, so the next Connect mints one. It is refused as `store_in_use`
unless you are a cluster owner or can write every storefront bound to the
store, and an empty token is refused while a live storefront is bound.

### 4. Go live

**Go live** is offered once the storefront is attached to a store that has a
Storefront token. Before that it is refused as `storefront_not_connected`.

---

## Reconnecting

Press Connect Shopify again whenever a result below says to. It never creates a
second store: the row is updated in place.

- **Same app.** Nothing to save; the store's current app is asked to approve,
  and the Admin token is re-issued and re-sealed.
- **A new app** (a rotated secret, a recreated app). Save the new client ID and
  secret -- pending, so the live store keeps verifying with the old one --
  then Connect. The new app replaces the old only after the approval.
- **The Storefront token** is minted only when the store has none. To have
  Connect mint a fresh one, clear it on the panel first (an empty token).
- **The privacy owner** (`ownerUserId`) never moves on a reconnect. A cluster
  owner changes it.

---

## Results and what to do

These arrive as `?shopify=<result>` on the Store panel, with the copy MemQL OS
shows. They are catalogued in `component/packages/refusal.go`.

| Result | What happened | What to do |
|---|---|---|
| `connected` | The first connection of this store | Go live when ready |
| `reconnected` | A store that already had an Admin token was updated in place | Nothing |
| `installed` | Shopify opened the App URL -- the install link -- with no Connect behind it | Press Connect Shopify |
| `connect_state_invalid` | The link was unknown, already used, expired, or begun for something else; it was finished in a browser other than the one that began it; or the storefront no longer publishes that shop | Press Connect Shopify again, from the browser you are signed in with |
| `signature_invalid` | Shopify's reply did not verify under the saved client secret, or named another shop. Nothing was spent | Check the client secret on the Store panel, save it again, connect again |
| `permission_lost` | Judged when Shopify sent you back: your account is inactive, or you no longer hold the store part (yours, or at the storefront's organization), read the store or write the storefront | Ask a cluster owner |
| `exchange_failed` | Shopify did not trade the approval for a token, or the cluster could not keep it | Connect again; every write is keyed by the store id |
| `scopes_missing` | A required Storefront scope was not granted | Add it to the app as a required scope in Shopify, release, connect again |
| `storefront_token_failed` | Connected, and the Storefront token could not be minted (Shopify allows 100 per app per shop) | Connect again, which retries only the mint, or paste a token |

The Store panel's own calls answer these before anything reaches Shopify:

| Reason | Meaning |
|---|---|
| `site_not_writable` | You cannot write this storefront |
| `not_a_storefront` | The deployable is not `kind: shopify_storefront` |
| `store_not_named` | No package run that published this storefront names a shop; redeploy it |
| `store_redacted` | The shop was purged (`shop/redact`); it is not reconnected silently |
| `app_credentials_invalid` | A client ID or secret was empty or implausibly long |
| `secret_name_ambiguous` | More than one row carries a credential's name, so no write could say which it changed; a cluster owner removes the extra |
| `shopify_app_not_saved` | Neither a pending app nor a current one; save the app first |
| `store_not_connected` | A Storefront token needs a connected store |
| `store_in_use` | The store is bound to a storefront you cannot write |
| `storefront_token_required` | An empty token while a live storefront is bound |
| `storefront_token_invalid` | The pasted token did not answer the store's Storefront API |

No token, secret, code or Shopify response body appears in a URL, a log line,
an audit event or an error: failures are recorded as the fixed tokens above.

---

## Across nodes

Begin runs on the bff and the callback on the identity node. Both read the
store and the saved app directly from the database, never from a node's
30-second store cache, so a Save on one replica is seen by a callback on
another seconds later. Store rows broadcast, so every edge flushes on the
write. Another node's connector cache can hold the old row for up to 30
seconds; a webhook arriving in that window can be answered 404, and Shopify
retries it. A test runs begin and the callback on two engines over one
database (`integrations/shopify/connect_hop_db_test.go`).

---

## Locally

The callback is `https://identity.memql.localhost/auth/shopify/callback`, which
is only reachable from your own browser -- that is all the redirect needs. The
webhooks are not: they point at `api.memql.localhost`, which Shopify cannot
reach, so a failed subscription health on a local cluster is expected, and
step 15 is proven only on a cluster Shopify can reach.

---

## Unverified

Settled only by the first real connection, and stated so nobody mistakes them
for tested:

- Whether Shopify accepts a `.localhost` redirect URL. If not, the first real
  connection is made on a cloud cluster.
- Whether a direct approve link works for a custom-distribution app before its
  install link was opened. The fallback is opening the install link (which
  lands as `installed`), then Connect.
- How a Dev Dashboard app's managed installation treats the requested scopes.
- Whether the minted Storefront token sees the same products as the Headless
  channel's token. The fallback is pasting that token.
- Whether the trade buyer-context reads need `unauthenticated_read_customers`,
  or more.
- Whether a custom app needs Shopify's approval for `read_all_orders`.
- Webhook registration against a local cluster (above).

---

## Related

- [The Shopify connector](shopify-connector.md) -- the mirror a connected store
  feeds, and the store row's fields and who may change them.
- [The storefront completeness checklist](shopify-storefront-checklist.md) --
  what the storefront itself must build.
- [Storefront preview](storefront-preview.md) -- the development store and
  the candidate version.
- [GitHub Connect](github-connect.md) -- the same shape, for sources.
