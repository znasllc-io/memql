---
title: Storefront Testing and Production
audience: public
status: stable
area: operate
sinceVersion: 0.23.0
owner: znas
---

# Storefront Testing and Production

A Shopify storefront has one deployable, one published website build, and two
stable destinations. **Production** retains its existing hostname and store
binding. **Testing** uses `test--` before that hostname and an independent store
binding. A design deployment or rollback changes the build served by both.

| Destination | Example address | Store selection |
|---|---|---|
| Production website | `https://graceful-fjord.example.com` | Production in the Store panel |
| Testing website | `https://test--graceful-fjord.example.com` | Testing in the Store panel |

Either destination can use a sandbox or a live store, and both may select the
same store. An unconnected destination displays design preview. Store selection
does not copy data between stores and does not depend on the app developer's
own Shopify store. Merchants authorize the stores they control through
[Connections](shopify-connect.md).

## Connect stores and visit

1. Open the storefront's **Store** panel in Deployables.
2. Select a connected store independently for **Testing** and **Production**.
3. Choose **Visit → Testing** or **Visit → Production**.

MemQL handles Testing access automatically when it opens the website. There is
no candidate selection, exercise step, grant management, or promotion step in
the storefront workflow. Testing remains available on a draft or live deployable
that has a published build. Production requires the deployable to be live.

Testing is private: opening its address without an active authorized session
returns a message to open it through MemQL OS. It is served with private,
no-store caching and noindex headers. An expired session can be renewed by
choosing Visit → Testing again. Production remains public and always uses its
own store, even if a browser sends an old preview cookie.

## Declare the stores in a package

```yaml
formatVersion: 1
name: storefront-package
deployables:
  - name: storefront
    path: clients/storefront
    kind: shopify_storefront
    deployment:
      slug: graceful-fjord
    binding:
      store: merchant.myshopify.com
    testing:
      binding:
        store: sandbox.myshopify.com
```

`deployment.slug` and the existing `binding` continue to describe Production.
`testing.binding` is optional and applies only to `shopify_storefront`.
The Testing address is derived; it needs no second deployable, independent
build, hostname allocation, DNS record, or certificate under the cluster's
existing wildcard. The `test--` prefix is reserved for these aliases.

Package analysis carries both bindings through review. Changing the declared
Testing store changes the plan fingerprint. Publishing resolves and authorizes
each store independently. A missing or inaccessible declared store is reported;
an existing connection is retained. Omitting a binding leaves a selection made
in the Store panel intact. See [Packages](packages.md).

## Access and serving contract

The underlying site row keeps `binding.storeId` for Production and
`previewBinding.storeId` for Testing. Both point to an authorized Shopify store
row. Existing store-attachment and deployable permissions still apply; store
type does not grant access. A nonempty unreadable binding is refused, while an
empty binding permits design preview.

Visit uses `sitePreviewOpen(siteId)` to mint a short-lived site-scoped credential.
The returned entry URL establishes a Secure, HttpOnly, host-only cookie and
redirects to the stable Testing root. Tokens are not stored in readable rows;
only their digest is persisted. The edge checks the grant's site, expiry and
revocation, with its existing bounded 15-second grant cache. A missing or
invalid grant on Testing returns 401 and never falls through to Production.
Publishing a shared design does not invalidate an otherwise authorized Testing
session. Disabled and archived storefronts are not served through Testing.

The edge resolves the canonical site once per destination cache entry and keeps
the same `bundleRef`. On Testing it substitutes only the Testing store. Runtime
configuration, public Storefront token resolution, Shopify content-security
policy origins, and shopper-form store context follow that selected store.
Admin tokens and webhook secrets never enter the browser configuration.

`sitePreviewReadiness(siteId)` reports the Testing URL and availability, and
whether each destination's store names its own Storefront token
(`storeHasStorefrontToken`, `previewStoreHasStorefrontToken`) -- the one secret
the edge publishes for a store, so the console never treats a store as
connected while it serves nothing.

**Testing and Production serve ONE build, each against its own store.** That
is the storefront model, decided on 2026-09-25: there is no candidate version
of a storefront. Candidates -- a version published beside the serving one and
shown only under a preview -- are a `spa` and `static` feature
([Deployables](deployables.md#publishing-the-candidate-version-instead)). A
package deploy's `target: candidate` placement is refused for a storefront, and
`setSiteCandidate` and `POST /sites/{id}/bundles?target=candidate` refuse any
version but the one it already serves, with `storefront_has_no_candidate`.

Site runtime settings are shared between destinations. Values that belong to
ONE STORE -- the Customer Account API client of that store's Headless channel,
the wholesale adapter configured for it -- go in the deployable's
`storeSettings`, keyed by store id, and travel with the store each destination
selects (memql#5602): the edge merges the entry of the in-force binding's store
over `settings`, so Testing hands the testing store its own client and
Production hands the live store its own. The document keeps its shape: a bundle
reads `config.settings.customerAccountClientId` either way. See
[Deployables](deployables.md#settings-that-belong-to-one-store).

## Store connection state

For storefronts, `runtime-config.json` includes `storefront.connectionState`:
`unbound` means no store is assigned and the website may offer a clearly marked
local design-preview catalog/cart with checkout disabled. `unavailable` means a
store is assigned but its configuration could not be read; display a connection
error, never substitute demonstration products. `connected` means the domain and
public Storefront credential are available; Shopify API failures still remain
errors. This state is independent for Testing and Production. It exposes no
backend error details or private credentials.
