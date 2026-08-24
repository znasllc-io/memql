---
title: Deployables
audience: public
status: stable
area: operate
sinceVersion: 0.18.0
owner: znas
---

# Deployables

A deployable is something this cluster hosts for someone. It is the surface
that used to be called Sites, widened so that a person -- not only an operator
-- can put a thing on the internet.

The row behind it is still `v1:platform:site`, and the edge still resolves a
request's `Host` header to one of those rows and serves its bundle. What
changed is who may own one, where it may live, and where its bytes may come
from.

---

## Ownership

`v1:platform:site` declares `@rowAuthz(owner="ownerUserId", clusterOwner)`:
the owner, or a cluster owner. A cluster owner still sees every site, which is
the capability the old `clusterOwner`-only tier existed to protect -- the
composite keeps "list every site in this cluster" expressible while adding
per-user ownership underneath it.

`ownerUserId` is **stamped from the actor** by `createSite`; there is no
`ownerUserId` argument. Go only ever NARROWS it: a privileged writer (cluster
owner, internal origin, system actor) has the stamp removed, and only when the
stamped value is the caller's own id. The outcome is therefore "the caller owns
it" or "nobody does" -- it can never name a third party.

An **empty `ownerUserId` means cluster-owned.** The seeded portal row keeps it
empty and lands cluster-owned on every boot.

Consequences worth stating plainly, because they are surprising:

- A cluster owner cannot personally own a site. Theirs are cluster-owned.
- Handing a site over, or taking one back, is re-running `createSite` on the
  same id. There is no owner-assignment mutation: one writing the owner from an
  argument could not exist without failing the owner-stamp gate or lying about
  why it was exempt.

---

## The hostname policy

A user's site is `<slug>.<domain>`, where `<domain>` is the one this cluster
serves (derived from `MEMQL_DOMAIN` through `component/frontdoor`, the same
derivation the portal host uses).

- The slug matches `[a-z0-9-]{3,40}`.
- It must be a **single label** -- one label, because the front door's
  `*.<domain>` Ingress wildcard matches exactly one.
- It must be unique across sites.
- It must not be reserved.

**The reserved set is derived, not listed.** It is `frontdoor.Roles()` (`api`,
`identity`, `mcp`) plus the platform's own `portal`, plus `www`, `admin` and
`mail`. Deriving it means a new role can never become claimable by forgetting
to add it here. The last three are different in kind: nothing serves them
today, and that is precisely why they are held -- they are the labels a person
reads as the organisation's rather than a tenant's, and `mail` is where a mail
host would land if one is ever added.

Any other hostname -- a custom apex, a different domain -- stays
**cluster-owner-only** and hand-certified, as before.

---

## Kinds

Three kinds are live, and the enum holds exactly these three:

| Kind | What it is |
|---|---|
| `spa` | A single-page app bundle. |
| `static` | A plain website. |
| `shopify_storefront` | A SPA bundle with a typed binding to a Shopify store. |

Android, iOS and macOS appear in the portal as disabled "coming soon" entries
and have **no schema at all**. That is deliberate: those are artifact
*distribution* (stores, TestFlight, notarisation), not hostname-resolved web
surfaces, and an enum value that never resolves would be the wrong kind of
additive. The portal renders them from its own list, because there is nothing
in the schema to read. `TestSiteKindEnumIsExactlyThreeValues` pins the enum.

---

## Deploying from the Library

`sitePublishFromArtifact(siteId, artifactId)` publishes a zip artifact from the
person's Library as the site's new bundle. It runs entirely under the caller's
actor: the site, the artifact and the backing file are each read through
owner-scoped queries, so a cross-user call returns zero rows and is refused by
name rather than by a special case.

The bundle is validated before anything is written: it must be a zip, it must
carry `index.html` at the ROOT for `spa` and `shopify_storefront`, and it is
held to the same per-file (25 MB), total (500 MB) and file-count (20000) limits
the CI bundle route enforces. Path traversal is refused, not sanitised.

Atomicity is the publisher's: a new content-addressed version is written first,
and only then does the row's `bundleRef` flip. `artifactId` is stamped on the
row so a site can say which bundle it came from.

Every refusal carries a **stable machine-readable reason** rather than prose,
so the portal can explain it and an audit row can be searched on:
`site_not_found`, `artifact_not_found`, `artifact_archived`,
`artifact_not_a_file`, `file_not_found`, `file_archived`,
`artifact_not_a_zip`, `storage_not_configured`, `bundle_unreadable`,
`bundle_not_a_zip`, `bundle_path_invalid`, `bundle_too_many_files`,
`bundle_file_too_large`, `bundle_too_large`, `bundle_empty`,
`bundle_missing_index`, `publish_failed`, `missing_argument`.

Success and refusal are both audited to `v1:identity:auditEvent`
(`action: site_publish_from_artifact`), best-effort -- an audit write that
fails must never report a completed deploy as failed.

> **INFO: `POST /sites/{id}/bundles` is unchanged.** It remains the CI door,
> for service accounts only. No user-facing bundle upload route was added; the
> portal deploys from bytes the system already holds.

**A new deployable starts with a placeholder bundle.** `createSite` requires
`bundleRef` and the schema has no empty state, so the portal writes
`blob://sites/<siteId>/pending/` with `status: "draft"` -- the placeholder
convention [site-hosting.md](site-hosting.md) already describes. That is safe
because a draft site is refused *before* any file lookup happens, so the
placeholder is never dereferenced. The first real publish replaces it.

**Rollback** is unchanged: the detail page walks the site's history with an
`asOf` read and sets `bundleRef` back. Old versions stay under their own
content-addressed prefixes, so restoring one serves its original content.

---

## The Shopify storefront

A `shopify_storefront` site is a SPA that MemQL hosts at `<slug>.<domain>`,
with a typed binding to a store. Checkout is Shopify's own hosted checkout.

The binding is `{storeDomain, storefrontTokenRef}`, where `storefrontTokenRef`
**names** a `v1:platform:globalSecret` row. The token itself is never stored on
the site row.

At serve time the edge injects `{kind, storeDomain, storefrontToken}` into the
site's runtime-config document -- the same mechanism that already gives the
portal its config -- resolving the token from the named secret then, not at
publish time.

Two properties of that are load-bearing:

- **The KIND is the gate, not the presence of a binding.** A binding left on a
  `spa` row consults the secret store zero times.
- **The Admin token is never injected.** A Storefront API token is a
  client-side credential by Shopify's design; an Admin token is not, and a test
  greps the served document to keep it that way. An unresolvable token yields an
  empty value, never an error string in the document.

A storefront also gets the SPA fallback, because it *is* a SPA bundle -- without
it, reloading a deep link like `/products/x` would 404.

Orders, inventory and refunds are **not** this feature. They reach MemQL
through webhooks and reconciliation as sub-project J, so a sale from any
channel lands, not only one made through this storefront.

---

## TLS

A freshly deployed site is live over TLS with no operator step, provided the
cloud overlay declares the DNS-01 issuer: a wildcard `Certificate` for
`*.<domain>` covers every site hostname, and the edge Ingress's wildcard rule
carries it.

Without that issuer the pre-existing rule stands -- the front-door certificate
names exact hosts only, and a site routed by the wildcard terminates TLS with
the ingress controller's self-signed default until it has a `Certificate` and
an exact-host Ingress of its own.

See [front-door.md](front-door.md) for both regimes and
[azure-entry-install.md](azure-entry-install.md) for the DNS-01 prerequisites.

> **WARNING: locally the mkcert pair IS a wildcard.** A site that works over
> https on the local cluster is no evidence that it has a certificate in the
> cloud.

---

## In the portal

Deployables live in the **Library** rail group beside Artifacts. `/sites` and
everything under it redirect to `/deployables`, tail included, so an old
bookmark or a link in a runbook still lands.

Two things the page deliberately does not offer:

- **No custom-hostname field, even for a cluster owner.** An apex or a second
  domain needs its own DNS record and certificate, neither of which the portal
  can create; a form that accepted one would mint a row the cluster cannot
  serve. That path stays operator-side.
- **The storefront binding is read-only after creation.** There is no
  `updateSiteBinding` mutation -- `binding` is written by `createSite` and
  changed by re-running it on the same id -- so the page renders the binding
  rather than offering a field that would silently do nothing.

---

## Related

- [The Library](library.md) -- where the zip artifact you deploy comes from.
- [Site hosting](site-hosting.md) -- the request path and the per-site CSP.
- [Front door](front-door.md) -- hosts, certificates and the two TLS regimes.
