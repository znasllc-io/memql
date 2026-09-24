---
title: Storefront preview
audience: public
status: stable
area: operate
sinceVersion: 0.22.8
owner: znas
---

# Storefront preview

A storefront is a deployable whose bundle talks to a Shopify store. Before a
version of it reaches shoppers, somebody has to walk it: read the catalog, put
something in a cart, open the checkout, pay, and see the order come back. That
walk has to happen against a store that is not the merchant's, and it has to
happen on the storefront's own address, because a cart and a hosted checkout
both assume that address.

This page is how. Its other halves are [Deployables](deployables.md), which is
the deployable itself, and [The Shopify connector](shopify-connector.md), which
is the store row a storefront is bound to.

Design record:
[Shopify storefronts on MemQL](../../superpowers/specs/2026-09-20-shopify-storefront-program-design.md),
decisions D7 and D8.

---

## The five words

Everything below is said in these five, and they are not cosmetic -- two of the
words an operator would reach for instead are ones this repository refuses to
compile.

| Word | What it is |
|---|---|
| **serving version** | the bundle version the public is served. The `bundleRef` field on `v1:platform:site` |
| **candidate version** | a published bundle version this deployable is NOT serving. The `candidateRef` field. Empty is the ordinary state |
| **binding** | the Shopify store shoppers reach: `binding`, `{storeId}` naming a `v1:shopify:store` row |
| **preview binding** | the development store a candidate is exercised against: `previewBinding`, the same one-key shape |
| **preview** | one operator's short-lived, authenticated view of the candidate version, served on the deployable's own address |

### Preview is not an environment

MemQL ships one installation shape. There is no second tier inside the product,
and preview does not add one. Under preview there is still **one cluster, one
site row, one hostname, one database**; nothing is duplicated and nothing is
copied.

What differs is only two things: **which bundle version is served to whom**, and
**which external store that version is pointed at**. The second is a fact about
somebody else's system, the way a payment provider's test mode is -- MemQL
removed environments of MemQL, which says nothing about Shopify's.

The engine holds itself to that literally. Nothing in it branches on "this is a
preview" to change what the engine *does*; a request carrying a valid grant is
served from a copy of the resolved site with two fields swapped, and every layer
below -- the bundle opener, the content security policy, the runtime-config
document -- runs unchanged and has no idea a preview is happening. That is
design decision D7, confirmed by the owner on 2026-09-20, and
`TestNoEnvironmentBranchingInEngineCode` still passes with an empty exemption
map, which is the assertion that it was honoured.

---

## 1. Attach a development store

A development store is an ordinary second `v1:shopify:store` row. It is not
something MemQL provisions, and specifically not a Shopify store created with
generated test data -- such a store cannot be transferred to a merchant, which
is why the design refuses that shape. Create it on Shopify, on a plan that
supports whatever the storefront needs, and register it the way any store is
registered:

```memql fragment
mutation createStore(
  storeId:              "acme-dev",
  domain:               "acme-dev.myshopify.com",
  name:                 "Acme, development",
  storefrontTokenRef:   "acme_dev_storefront_token",
  isDevelopment:        true,
  developmentOfStoreId: "acme"
)
```

Two fields matter here and nowhere else:

- **`isDevelopment`** is what every guard on this page asks. It lives on the
  store rather than on the site because what makes a store a development store
  is a fact about somebody else's system, and because the guard has to be able
  to ask it of the store a site *names*.
- **`developmentOfStoreId`** pairs it with the live store. It is a record, not a
  gate: without it the two rows sit in one list with nothing saying which
  belongs to which. `developmentStoresFor` is the read behind that pairing.

A development store is registered and read like any other store: developers
and cluster owners read store rows (design decision D3), and only a cluster
owner -- or server code, which is how Connect Shopify writes them -- registers
or changes one (D15). The token arguments are
references to `v1:platform:globalSecret` rows, never tokens -- see
[The Shopify connector](shopify-connector.md).

## 2. Point the preview binding at it

```memql fragment
mutation updateSitePreviewBinding(siteId: "<site>", storeId: "acme-dev")
```

The binding is written whole as `{storeId}`, never merged. An empty `storeId`
writes the unbound state, which is how a development store is detached.

**A preview binding that does not name a development store is refused**
(`preview_binding_is_not_development_store`), and that refusal is the whole
reason this field is separate from `binding`. A preview pointed at the store
shoppers reach looks exactly like a preview -- the catalog loads, the cart
works, the checkout opens -- right up to the moment a test payment lands in the
merchant's real orders, and by then it has happened.

Setting the preview binding needs `execute` on `app:deployables/store`, the same
capability that binds the serving store, and the store it names must be one the
caller can read. Both are asked of `updateSitePreviewBinding` and of any other
write that changes the store `previewBinding` names, a raw `insert()` included,
so a person an owner has denied the part by name points no preview anywhere.
They are judged against the binding the row already carries: a write that keeps
it asks neither, and one that clears it names no store to read. Changing either
binding, clearing included, also needs the store capability in the site's
organization. Preparing a version is one thing; choosing which of the cluster's
stores it talks to is another.

Developers hold the part (Connect Shopify, D3), so a developer may point the
preview of ANY storefront they can write at any development store on the
cluster -- every account-tied client storefront included, live ones too, the
same reach that lets a developer pause, archive or delete those sites.

One limitation remains on the SERVING store: its readability is still asked on
every write to a bound storefront, not only on a change, so a site owner who
cannot read the store a cluster owner bound cannot edit the site until that is
fixed.

## 3. Publish a candidate version

Publish the bundle the ordinary way -- from a Library zip, from your own CI
through `POST /sites/{id}/bundles`, or from a package deploy -- and then name
that version as the candidate:

```memql fragment
mutation setSiteCandidate(siteId: "<site>", candidateRef: "blob://sites/<site>/v/<version>/")
```

Setting a candidate changes **nothing** about what the public is served. The
edge goes on serving `bundleRef` to every request that does not carry a preview
grant, on a draft deployable and on a live one alike.

`clearSiteCandidate` is the plain inverse, and it is a deliberate act rather
than an omitted argument. It touches neither the serving version nor the preview
binding: withdrawing a candidate is not detaching a development store, and
whoever withdraws one usually means to publish another against the same store.

**Withdrawing a candidate ends every open preview of it**, because a grant names
the version it was issued for and the edge compares the two. That is also the
fastest way to close every preview URL at once.

## 4. Open a preview

```memql fragment
sitePreviewOpen(siteId: "<site>")
```

The reply is
`{grantId, siteId, hostname, candidateRef, previewStoreId, previewStoreDomain, expiresAt, ttlMinutes, url}`.

**The URL comes back once.** Only the SHA-256 of its token is stored, so nothing
can ever read it back -- not an operator, not a support session, not a backup.
Four gates run before anything is minted, in the order a person would ask them:
is there a deployable I can see, is it one this applies to, is there a candidate
to preview, and is there a development store to preview it against.

Opening the link lands on `https://<hostname>/_memql/preview?grant=<token>`,
which sets a cookie and answers `303` to `/`. The token is in the address bar
for exactly one navigation and is a cookie thereafter, so it never reaches a
`Referer` header the page's own assets send, never lands in a shared history
entry, and never appears in a screenshot of a working preview.

`https://<hostname>/_memql/preview/leave` clears the cookie in that browser and
sends the person back to the public site. **It is not revocation**: the grant row
is untouched and a link somebody else holds goes on working. Ending a preview
everywhere is `revokeSitePreviewGrant`, which is a soft end -- the row stays as
the record that a preview was opened, by whom, against which version and which
store, which is the question asked after something unexpected turns up in a
development store.

### What a request gets

| The request | What it is served |
|---|---|
| no cookie, draft deployable | `404`. To the internet that deployable does not exist |
| no cookie, live deployable with a candidate | the **serving** version, under the ordinary binding. Nothing about the candidate reaches it |
| a valid grant, draft deployable | the **candidate**, under the preview binding |
| a valid grant, live deployable | the **candidate**, under the preview binding, WHILE the serving version goes on serving the public |

The fourth row is the requirement that shapes everything: preview is per bundle
version, not per deployable status. After a storefront goes live, its owner
keeps working, and exercises version N+1 the same way while N serves shoppers.
A design that previewed only a draft deployable would deliver this once and
never again.

Five things are checked on every request, and a failure at any of them is not an
error and not a redirect -- the request falls through to the ordinary public
path and gets precisely what it would have got with no cookie at all:

1. the token is well formed;
2. a grant exists for its digest;
3. the grant is for **this** deployable (a cookie carried to a sibling hostname
   gets the public answer);
4. the grant names the candidate the site is **currently** carrying;
5. the grant is neither expired nor revoked.

A preview is refused outright for a deployable that is `disabled` or `archived`.
A pause and a decommission are deliberate acts, and a preview that served
through either would be a way to bring a deployable back that does not touch its
status.

## 5. Walk the storefront

This is the part the engine does not do for you. Open the preview URL, read the
catalog, add a line to a cart, follow the checkout to Shopify's hosted checkout,
pay with a test card, and watch the order arrive back in the mirror under the
development store's `storeId`.

It runs on the deployable's **own** address, which is a requirement rather than
a convenience: a storefront's cookies, its cart and Shopify's checkout return
all assume that origin, and a preview served from anywhere else would fail in
ways that say nothing about the candidate.

## 6. Run the probe, and read the observations

```memql fragment
sitePreviewProbe(grantId: "<grant>")
```

The probe asks the development store the Storefront API questions the walk asks,
and records each as a `v1:platform:sitePreviewObservation` row on the grant. It
**creates a cart** -- that is the thing being observed, not a side effect, and it
is why the preview binding is held to a development store before this can run.
Nothing is paid for and no order is placed.

Four observations, and the closed set is the decision:

| `kind` | What it means | How it arrives |
|---|---|---|
| `catalog_read` | the development store answers a catalog read | active: the probe asks |
| `cart_accepted` | a cart accepts a line | active: the probe asks |
| `checkout_url` | a `checkoutUrl` comes back | active: the probe asks |
| `order_mirrored` | an order arrived in the mirror under that store's `storeId` | passive: the connector doing its ordinary job |

Three rules about reading them:

- **A failure is a result.** `ok: false` with `failure` saying why is an answer.
- **A step nobody ran has no row at all.** That third state is what a surface
  draws as *unmeasured*, and it is not a zero and not a failure. Writing a row
  to say "we did not look" would make "nobody asked" and "we asked and got
  nothing" the same shape on the screen.
- **`detail` never carries the checkout URL and never carries a token.** An
  observation row is readable by every operator the composite tier admits, and a
  checkout URL is a live cart somebody else could pay from. The checkout **host**
  is recorded instead, which is what an operator actually checks.

`durationMs` is zero on `order_mirrored`, and that is honest rather than
missing: nothing timed the order's journey, and a duration invented for it would
be a measurement of the mirror's lag dressed up as the storefront's.

Read them back with `sitePreviewObservationsForGrant` (one exercise, oldest
first, which is the order the steps happen in) or
`sitePreviewObservationsForSite` (everything ever observed of this deployable,
newest first). An empty answer is "nobody has exercised this".

## 7. Promote

```memql fragment
mutation promoteSiteCandidate(siteId: "<site>", candidateRef: "<the version you just read>")
```

Promotion is the row flip `bundleRef` already was: the candidate becomes the
serving version and the candidate is cleared, in one write.

**The caller names the version it is promoting, and that argument is the whole
safety of this mutation.** A mutation body cannot read the row's own stored
field, so the candidate arrives as an argument; an argument nobody checked would
let a candidate republished between the reading and the click be promoted by
surprise. A promotion naming a version that is not the stored candidate is
refused (`candidate_moved`).

Promotion needs `execute` on `app:deployables/publish`, not `preview` -- it sits
with go live, pause and roll back, because it is the act that changes what the
public is served. A person may hold the whole of preparing and proving a
storefront without being able to put a version in front of a shopper.

## 8. Roll back

Rollback is `updateSiteBundle` pointed back at the previous version. **One row
write, unchanged by any of this**, and deliberately given no ceremony of its own:

```memql fragment
mutation updateSiteBundle(siteId: "<site>", bundleRef: "blob://sites/<site>/v/<the previous version>/")
```

That works because every version is stored under its own content-addressed
prefix rather than overwritten, so the version a promotion replaced is still
sitting exactly where it was. Publish and roll back are the same write in
opposite directions.

---

## Every refusal, and the act that clears it

These are the codes in `component/memql/site_preview_rules.go`. They are one
closed set, answered identically by two callers: the **write guard** wired into
the engine's write path, which refuses the act however it arrives, and the
**readiness read** (`sitePreviewReadiness`), which lets a surface make an
illegal act *absent* rather than drawing it and having it fail. Both call the
same function, so they cannot disagree. For going live and promoting they also
judge the same facts: the serving store as the deployment reads it, not as the
caller does, so a person who may go live but cannot read the store is offered
what the guard would accept. The readiness read names the store's domain only
when the caller can read it.

| Code | What causes it | The act that clears it |
|---|---|---|
| `preview_binding_is_not_development_store` | the preview binding names the store shoppers reach | point the preview binding at a development store -- Shopify marks one on the store row as `isDevelopment` |
| `no_preview_binding` | a storefront has no development store on its preview binding, so there is nothing to exercise a candidate against | attach a development store to the preview binding |
| `storefront_not_connected` | going live, or promoting, while the **serving** binding names no store, or names a store with no Storefront token. A storefront's first deploy lands as a draft with no store when the manifest's store could not be attached, and taking it live would put a shop with no catalog in front of shoppers | connect Shopify on the Store panel |
| `serving_binding_is_development_store` | going live, or promoting, while the **serving** binding names a development store. It would serve a catalog nobody can buy from and take orders into a store that is not the merchant's -- and it would look like a successful launch while doing it | bind the storefront to the store shoppers reach. The development store stays on the preview binding |
| `bound_store_unreadable` | the store a binding names cannot be read here, so whether it is a development store cannot be answered | have an operator who can read the store check the binding, or re-bind to a store you can read |
| `candidate_is_serving_version` | the candidate named is the version already serving. There would be nothing to exercise, and promoting it would be a write that changes nothing while reading like a release | publish a new version and set that as the candidate |
| `no_candidate_version` | a promotion, or a preview, against a deployable with no candidate | publish a version and set it as the candidate first |
| `candidate_moved` | the stored candidate is not the one the promotion named -- it changed after the page read it | reload the deployable, look at the candidate that is there now, and promote that |
| `site_is_system_owned` | the platform's own site is exempt from this whole axis, as it is from the status axis and the settings axis | nothing. The platform's own site is deployed with the image and re-seeded at every boot, so a candidate written on it would silently undo itself |

An unbound storefront is refused on both sides, under two codes, because the
acts that clear them differ:

- **An unbound *serving* binding is refused going live** as
  `storefront_not_connected`. A storefront's first deploy is a draft with no
  store whenever the manifest's store could not be attached, so "unbound" is
  not a page nobody has wired up yet: it is a deploy that succeeded with no
  catalog behind it. The same code covers a bound store with no Storefront
  token, because the edge would serve an empty token and the catalog would not
  load. The token is judged last: an unreadable store and a development store
  are refusals about *which* store is bound, which a token does not clear.
- **An unbound *preview* binding is refused a preview** as `no_preview_binding`.
  A preview asks "is there a development store to exercise against", and for an
  unbound one the answer is no -- exercising it would fall back to no store at
  all and report four observations of nothing.

**An unreadable store refuses; it does not default.** `bound_store_unreadable`
is a third state, not a synonym for "not a development store". Reading an
unreadable store as "not a development store" would let a go-live through
precisely when the cluster had lost track of what the storefront is bound to.

---

## What the engine can and cannot observe

The engine observes four things, and there is no fifth value in the enum because
there is nothing honest a fifth row could say.

**The payment walk is not observed at all.** It happens in a browser on
Shopify's hosted checkout, and no request this cluster makes can witness it. A
`checkoutUrl` coming back is the last thing this cluster can honestly say. The
test that walks the payment is the product's
(`znasllc-io/memql-fylo#28`), not the engine's -- that repository is not
resolvable from this tree, which is why this is prose rather than a link.

A declared field nothing can write reads as a measurement that came back empty,
which is why the set was closed at four rather than at five.

---

## A preview URL is a bearer credential

Everything about the shape of a preview follows from that one sentence.

- **The token** is `mql_prv_<43>`: 32 bytes from `crypto/rand`, base64url, the
  same 256 bits every other token in the family carries, and the same prefix
  convention (`mql_wkr_`, `mql_enr_`, `mql_rec_`, `mql_pat_`) that makes a token
  greppable in a leak scan and unmistakable in a support transcript.
- **Only the digest is stored.** The plain token goes back once in
  `sitePreviewOpen`'s reply and exists thereafter in the operator's URL and
  cookie and nowhere else.
- **The cookie is host-only, `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`.**
  Host-only so it belongs to this deployable's hostname and no sibling under the
  cluster domain. `HttpOnly` so no script on the previewed page can read the
  credential showing it -- and a candidate version is precisely the code nobody
  has reviewed in front of shoppers yet. `Lax` rather than `Strict` because
  Shopify's hosted checkout redirects **back** to this origin at the end of the
  walk, and `Strict` would drop the cookie on that top-level navigation, landing
  the person on the public site at the one moment the preview matters most.
- **Every previewed response carries `Cache-Control: no-store`,
  `X-Robots-Tag: noindex, nofollow` and `Vary: Cookie`.** `no-store` keeps a
  candidate version out of every shared cache between the cluster and the
  operator's browser -- without it a CDN or a corporate proxy could serve an
  unpublished storefront to the next person through. `noindex, nofollow` stops a
  crawler that somehow reached a preview from putting an unpublished version, and
  its development-store prices, into a search index. `Vary: Cookie` is the same
  statement to a cache that does pay attention.
- **`/_memql/preview` answers `303` to `/` whether or not the token is any
  good.** A distinguishable failure there would turn the path into an oracle for
  which tokens exist. A bad token simply sets no cookie, and the person lands on
  the public site -- which for a draft deployable is the 404 anybody else gets.
- **The lifetime is the bound.** `expiresAt` is the only limit a bearer
  credential has here, and a zero or unreadable expiry is read as **expired**,
  never as eternal.

`lastSeenAt` is the evidence a URL was used. It is throttled to at most one
write per grant per minute and written detached from the request, because a
preview loads a document and every asset under it and a write per asset would
put the graph in the serving path of a page. **An absent `lastSeenAt` means
nobody opened it**, never "a long time ago" -- an operator deciding whether a
preview URL escaped needs to be able to tell those apart.

### `MEMQL_SITE_PREVIEW_GRANT_TTL_MINUTES`

How long a minted preview lasts.

| | |
|---|---|
| Default | **30** minutes |
| Cap | **240** minutes (4 hours) |
| Unset, unparseable, zero or negative | the default. Never "no expiry" |
| Above the cap | clamped to the cap |

Half an hour is long enough for the walk in section 5 and short enough that a
leaked link is worth one walk. The cap exists because the failure it prevents is
one nobody notices: an operator raises the value once to finish a long
afternoon, and the cluster hands out half-day bearer tokens for unpublished
storefronts forever after. Somebody who needs longer opens another preview,
which is one click and leaves a record of itself.

---

## Who may do what

| Act | Capability |
|---|---|
| set or clear a candidate, open or end a preview, run the probe | `execute app:deployables/preview` (owner, developer) |
| attach the serving store, or point the preview binding at a store, on any storefront the caller can write | `execute app:deployables/store` (owner, developer), and a store the caller can read |
| promote a candidate, go live, pause, roll back | `execute app:deployables/publish` |
| read a store row | developer, cluster owner |
| register a store, or change, pause or resume one that exists | cluster owner |

That is the surface half. Underneath it, `v1:platform:site`,
`v1:platform:sitePreviewGrant` and `v1:platform:sitePreviewObservation` all
declare the composite owner tier -- the row's owner, or a cluster owner -- so
every read and write above runs against it **under the caller**. Somebody who
cannot read the deployable resolves zero rows and is refused by name, before
anything is minted. A capability names the surface; the tier decides the rows;
both must pass.

The edge is the one reader that is nobody. It resolves a grant under its own
synthetic cluster-owner actor, the same one it resolves a site and a store with
-- it holds no operator's credential and never learns who is asking. It holds a
**digest**, and the row it finds is the whole authorization decision, made
earlier by the mint under the caller's own actor.
