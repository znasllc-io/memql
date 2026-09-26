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

`sitePreviewReadiness(siteId)` reports the Testing URL and availability.
Non-storefront candidate previews retain their existing behavior. Storefronts
use the shared published build instead of `candidateRef`.

Site runtime settings are shared between destinations. Store-specific Customer
Account API settings still require separate work; independent catalog bindings
do not imply independent account-client settings.

## Store connection state

For storefronts, `runtime-config.json` includes `storefront.connectionState`:
`unbound` means no store is assigned and the website may offer a clearly marked
local design-preview catalog/cart with checkout disabled. `unavailable` means a
store is assigned but its configuration could not be read; display a connection
error, never substitute demonstration products. `connected` means the domain and
public Storefront credential are available; Shopify API failures still remain
errors. This state is independent for Testing and Production. It exposes no
backend error details or private credentials.
