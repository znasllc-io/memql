# Shopify storefronts on MemQL -- serving, preview, and the packs a storefront carries, a seven-epic program across two repositories

- **Date:** 2026-09-20
- **Status:** brainstormed with the owner on 2026-09-20; **not yet approved as a
  whole.** Section 4 records the answers the owner gave. Several were given
  BEFORE the facts in sections 3 and 6 were known: one of them (D3) is
  contradicted by the tree, and one derived decision (D7) reads the owner's own
  law and is his to confirm. Each is flagged where it appears and filed as a
  decision issue that blocks the work depending on it, rather than being
  written down as settled. Issues were filed on 2026-09-20; the table in
  section 12 names them.
- **What it is:** the design for a client's Shopify storefront as a MemQL
  deployable -- configured entirely in the Deployables app, previewed end to
  end against a Shopify development store before it serves, flipped live, and
  then edited, previewed and published again -- plus the two packs the first
  storefront carries (reviews, which exists, and a wholesale channel, which
  does not) and the product repository that consumes them.
- **Why it is pivotal:** `memql-fylo` is the first product on the
  product-agnostic engine, and the storefront is the first deployable with
  somebody else's system behind it. Almost every piece this needs was built
  -- the site kind, the runtime document, the mirror, the push channel, the
  commerce namespace, two packs -- and the pieces have never been joined. This
  record is mostly an account of the joins that are missing: one stops a
  storefront from working at all, and two more stop either pack from ever
  reaching a shopper.
- **Repositories:** `memql` (`component/edge`, `dsl/platform`,
  `dsl/shopify/overlay`, `component/packages`, `integrations/shopify`,
  `examples/reviewspack`, `examples/shopifypack`, `dsl/rbac`,
  `component/auth`, `component/server`, `docs/public/operate`);
  `memql-fylo` (`clients/storefront`, `memql-package.yaml`, `dsl/`).
  MemQL OS (`clients/os`) changes are named where backend work has a frontend
  counterpart and are **tracked separately**: that design was still being
  worked out live when this record was written.
- **What this record rests on:** every path and claim below was read at
  `origin/main` `cd10288d7` on 2026-09-20, not recalled. Shopify behaviour is
  cited from
  [the connector record](2026-08-22-shopify-connector-complete-mirror-design.md),
  which verified it by live introspection on 2026-08-23; anything that record
  does not state is marked UNVERIFIED here, by the same convention.

## 1. Problem

The client has a working Shopify store and a designed storefront: an Astro
site in `memql-fylo/clients/storefront` with URL parity to the live domain.
The owner wants to do, for every client, what was just done for this one --
prepare the whole storefront, then exercise it end to end against a sandbox
before it serves, including a real test payment; flip it live; and afterwards
keep editing, test the newer version the same way, and publish again. The
sandbox is to be storefront FUNCTIONALITY driven by settings in MemQL, per
client -- not an operations chore somebody performs by hand.

Today none of that is possible, and not for want of parts:

- The storefront is a mockup. Its catalog is a JavaScript file
  (`src/data/products.js`), it names no Shopify store anywhere, and its four
  forms post to themselves.
- A `shopify_storefront` site served by the edge **cannot reach Shopify at
  all**: the content security policy the edge writes for it admits neither the
  store's domain nor Shopify's image host (section 6, G1).
- A draft site answers 404 to everybody, its own operator included, and
  nothing anywhere previews one (G2).
- A shopper is nobody to MemQL. Both packs a storefront would carry can only
  be written by an authenticated MemQL user (G5).
- Neither pack can reach a running cluster: both sit under `examples/` behind
  build tags no published image sets (G6).

The risk this program exists to remove is the ordinary one for a storefront:
pointing it at real money before anybody has watched it work.

## 2. What the tree already has

### 2.1 The site row and the storefront kind

`v1:platform:site` (`dsl/platform/concepts.memql`) is the deployable.
`kind` is `enum("spa", "static", "shopify_storefront")`, the third added by
memql#4344. `binding` is a per-kind object and only this kind declares one:
`{storeDomain, storefrontTokenRef}`, the ref naming a
`v1:platform:globalSecret` row and never holding the token. `settings` is a
flat object of public string values the bundle reads at load (epic memql#4906),
"so ... one bundle serves two deployables against different endpoints without
a rebuild". `status` is `enum("draft", "live", "disabled", "archived")` with the
lifecycle `draft -> live <-> disabled -> archived`. `bundleRef` has three
usages; the one that matters here is `blob://sites/<id>/<version>/`, where
"deploy is an upload plus a row flip in seconds, rollback is one row write".
`accountId` ties a site to the client it is for, with no read effect.
`apiProxy` mounts `/_memql/*` on the site's own origin, forwarded to the bff.

A fourth kind, `server`, was designed in epic memql#3718 and **closed
unbuilt**; the concept's own description says "Do not read 'server' as queued
work". The edge serves bundles. It runs nothing.

### 2.2 The runtime-config document

`GET /runtime-config.json` (`component/edge/runtimeconfig.go`) is answered by
every hosted site ahead of the bundle lookup: `identityUrl`,
`identityApiBaseUrl`, `oauthClientId`, `authEnabled`, `domain`, `settings`,
and, **only** for a `shopify_storefront` site, a `storefront` block of
`{kind, storeDomain, storefrontToken}`. The token is resolved from the named
secret at serve time and is a public credential by Shopify's design.
`TestRuntimeConfigNeverCarriesTheShopifyAdminToken`
(`component/edge/runtimeconfig_test.go`) greps the SERVED document so the
Admin token cannot arrive there. The shape is additive-only.

This document is the whole of "driven by settings in MemQL". It is built.

### 2.3 The connector, the mirror and the store row

`integrations/shopify` is linked into every default binary by a blank import
at `app/plugins_core.go:100`, with no build tag. It is a complete mirror:
`backfill.go` (Bulk Operations), `subscriptions.go` (webhooks),
`reconcile.go`, `apply.go`, `propagate.go` (the push channel), `admin.go`
(a client paced on Shopify's cost bucket), `compliance.go`.
`dsl/shopify/generated/` holds **65** concepts generated from the Admin schema
by `cmd/shopifyschema`.

`v1:shopify:store` (`dsl/shopify/overlay/concepts.memql`) is MemQL's own record
of how to reach a store: `domain`, `adminTokenRef`, `storefrontTokenRef`,
`webhookSecretRef`, `apiVersion`, `scopesGranted`, `protectedDataLevel`,
`plan`, `status` (`configured | backfilling | live | paused | error`),
`health`. It is `@rowAuthz(clusterOwner)`. "One connector, many stores -- every
mirrored row carries the storeId of the row it came from, and every read is
scoped by it." Its `storefrontTokenRef` is described as the one "the headless
storefront's site binding consumes" -- and the site binding does not consume
it; it carries its own copy (G4).

### 2.4 The commerce namespace and the push channel

`dsl/commerce/concepts.memql` holds the MemQL-origin concepts for what Shopify
does not model: `productContent`, `customerNote`, `companyLocationNote`,
`creditLimit`, `quote`, `approvalChain`, `salesRep`, `territory`,
`reorderList`. `integrations/shopify/propagate.go` projects some of them into
Shopify metafields, four with `StorefrontAccess: "PUBLIC_READ"` (the
`productContent` fields) so a headless storefront reads MemQL-authored copy
THROUGH Shopify, and the rest with none, because "a note about a customer is
internal".

### 2.5 Packs: two exist, and neither ships

`examples/reviewspack` (memql#4139) declares `review`, `moderationAction`
(a closed six-value criterion, append-only) and `reviewSettings`, whose
`publicDisplay` is "data so a client can hide or show reviews without a deploy
or .memql edit". `examples/shopifypack` (memql#4138) is the portal projection of
a shop. Both register under a build tag of their own name, and
`register_shopifypack.go` says the tag "is never set in production binaries".
The published images are built with `BUILD_TAGS=<node type>` and nothing else
(`Dockerfile`, `scripts/k3d/dev.sh`).

Pack enablement exists and is sound (`dsl/pack_enablement.go`): one
`v1:platform:packState` row per pack, absence meaning enabled; a disabled pack
is **mounted-inert** -- its concepts still load so imports and relationships
resolve, every behavioural construct is skipped -- and a flip takes effect as
each node restarts. It governs packs that are linked in. None is.

### 2.6 The deploy pipeline, and what its words mean

`component/packages/pipeline.go` runs `analyzing -> awaiting_confirm ->
building -> staging_dsl -> rolling -> publishing`. **Its "stage" stages DSL.**
It has nothing to do with previewing a site, and the owner's word "staged" for
a storefront must not be read onto it.

The spelling `staging_dsl` is forced. `TestNoEnvironmentBranchingInEngineCode`
(`environment_branching_test.go`) fails the build on non-test Go that contains
the complete string literal `production`, `prod` or `staging`, and its
exemption map is empty by design. It serves the law in `CLAUDE.md`: MemQL ships
**one installation shape** (epic memql#3943); "there is no
staging-versus-production dimension inside the product". `development` and
`local` are deliberately allowed, because they name deploy targets. D7 below is
written against this.

A package that carries any DSL is refused at deploy start with
`dsl_requires_authoring` (`component/packages/refusal.go`) unless the caller
may author constructs, and a DSL deploy stages MemQL and then ROLLS -- "a roll
restarts the cluster onto staged MemQL".

### 2.7 What the connector record already decided

[The 2026-08-22 record](2026-08-22-shopify-connector-complete-mirror-design.md)
is approved and this program builds on it. Three of its decisions were
re-reached in the 2026-09-20 brainstorm without anybody knowing they existed,
which is recorded here so nobody re-litigates them a third time:

- **Its D5 is the read path.** "The storefront reads Shopify directly through
  the Storefront API; the wholesale application reads the mirror through named
  queries under the identity model it will define." The owner's "hybrid"
  answer (D2 below) is this.
- **Its D8 is the pack line.** "Generic wholesale shapes live in the engine as
  DSL-configurable features; the client's workflows live in the product
  repository." The owner's principle (D6 below) is this.
- **Its section 1.3 answers the plan question.** "Since 2026-04-02, companies,
  payment terms, volume pricing and up to three catalogs (via Markets) are
  available below Plus; Plus keeps unlimited catalogs, direct catalog
  assignment, deposits, order-review Functions."

It also left one thing open in so many words -- "Cluster-owner tier now; the
wholesale application's identity model decides the per-user reads later" --
and that comes due in this program (G5).

Its section 8 is `docs/public/operate/shopify-storefront-checklist.md`, which
exists, and which says the Customer Account API is "OAuth 2.0 / OIDC with
PKCE", that checkout is "Shopify-hosted, always", and that the app categories
the client's store uses -- reviews among them -- are "inventoried in the
runbook's first step and each gets an integration line or an explicit 'not
carried'".

### 2.8 The product repository today

`memql-fylo` ships no DSL: commit `413db4e` parked the generator's greeting
scaffold because a DSL-carrying package was refused for the storefront's own
developer (2.6). The scaffold, migrated to edition 2026, is at
`scripts/dev/testdata/fylo-parked/`, and `scripts/dev/edition-gate.sh`
(memql-fylo#18) holds any future DSL to the frozen language.

`clients/storefront` is Astro with `output: 'static'` and no adapter. It
prerenders its catalog at BUILD time: `src/pages/products/[handle].astro` and
`src/pages/collections/[band].astro` call `getStaticPaths()` over
`src/data/products.js`. `memql-package.yaml` declares the surface
`kind: static`, deliberately, so "a mistyped path must 404 rather than fall
back to index.html". The wholesale form
(`src/pages/pages/wholesale-application.astro`) collects an EIN and is
`method="post"` on purpose: a GET would put a federal tax identifier in the
query string, the browser history and the access log, and the comment adds
that the form "must NOT be downgraded to a JS submit handler" because the
served policy is `script-src 'self'`.

## 3. What the brainstorm got wrong

Stated because a later reader will find the session's claims in issue threads
and should know which did not survive contact with the tree.

- **"A static site cannot hold a customer session, so wholesale needs
  server rendering."** The checklist the engine already ships says the Customer
  Account API is OAuth 2.0 with PKCE, which is the public-client flow a bundle
  runs in the browser. The owner chose server rendering on this premise (D3).
  Whether a public client is accepted for THIS store's Customer Account API
  configuration is UNVERIFIED and is the first thing Q1 settles.
- **"Native B2B is probably Plus-only."** It is not, since 2026-04-02 (2.7).
  The owner's answer (D4) stands for a better reason: below Plus there are at
  most three catalogs.
- **"The second record of a store is the `shop` concept in
  `examples/shopifypack`."** That pack is not in any image. The real second
  record is `v1:shopify:store` in the core overlay (2.3), which is what the
  Stores app manages.
- **"The engine's deploy stages include a stage step, so staging exists."** That
  step stages DSL (2.6).
- **"Same bundle, different binding" was offered as free.** It is not free for
  this storefront: see G3.

## 4. Decisions

D1 to D6 are the owner's, recorded in substance. D7 to D9 are derived and are
marked so. Where a decision needs re-confirmation it says why.

### D1 -- Both channels: consumers and a wholesale channel

A public storefront plus a gated trade channel people apply for. The owner
accepts the application, and acceptance grants wholesale prices.

### D2 -- The read path is hybrid

Catalog, cart and checkout from Shopify's Storefront API; from MemQL, only what
Shopify cannot model. This is the connector record's D5 (2.7).

### D3 -- "Headless, hybrid rendering" -- NEEDS RE-CONFIRMATION (Q1)

The owner chose to keep Astro and add a server adapter for cart, account and
wholesale routes. It was chosen before two facts were known: the edge runs
nothing (2.1), so server routes have no runtime to run on; and the customer
session does not obviously need a server (section 3). The likely resolution is
a static bundle, the customer session held by the browser under PKCE, and
`site.apiProxy` for the calls that must be same-origin with MemQL. That
reverses the owner's answer and is his to make.

### D4 -- Wholesale entitlement is plan-independent; native B2B is one adapter

The pack owns the application, the decision and the entitlement STATE. How
prices actually apply is a named seam the client configures: Shopify's native
B2B (company, location, contact, catalog, price list -- all mirrored) is one
adapter; customer tags with price rules or draft orders is another.
`v1:shopify:store.plan` already records what the bound store can do.

### D5 -- Everything about a storefront is configured in Deployables

When `kind` is `shopify_storefront`, the store connection, the preview, the
reviews settings and the wholesale settings all live on that deployable. The
Stores app is deleted and its function re-homed there. The per-domain acts --
backfill, pause, retry, discard -- stay in Cluster > Data origins: they belong
to every connector, and `clients/os/src/apps/stores/StorePage.tsx` already
says so in writing. An `spa` or `static` deployable gains none of this. *OS
surface tracked separately.*

### D6 -- A pack is nouns and invariants; the client's DSL is the process

Packs are client-agnostic. Each client lays out its own process -- who
approves, in what order, what is notified -- in its own repository's DSL, over
the pack's concepts. Reviews is an existing pack the product consumes.
This is the connector record's D8 (2.7). Section 7 draws the line.

### D7 (derived) -- Preview is not an environment -- NEEDS THE OWNER'S CONFIRMATION

The owner's sandbox has to live inside a product whose law is that it has no
environments (2.6). It can, on this argument, and only on this argument:

- There is one cluster, one site row, one hostname, one database. Nothing is
  duplicated.
- What differs under preview is **which bundle version is served to whom**, and
  **which external store that version is pointed at**. The second is a fact
  about somebody else's system, in the way a payment provider's test mode is.
  memql#3943 removed environments of MemQL; it says nothing about Shopify's.
- Nothing in the engine may branch on "this is a preview" to change what the
  ENGINE does. It may only choose a bundle and a binding.

Vocabulary follows, and is not cosmetic, because two of the owner's words are
build failures: a site has a **serving version** and may have a **candidate
version**; it has a **binding** (the store shoppers reach) and may have a
**preview binding** (the development store); a **preview** is an authenticated
operator's view of the candidate. `staging` and `production` are not used as
identifiers anywhere in this program.

### D8 (derived) -- The development store is what the preview binding points at

Not a thing the program provisions, and specifically **not** a Shopify store
created with generated test data: the owner rejected that on learning such a
store "cannot be transferred to a merchant". A development store is attached
like any other store -- a second `v1:shopify:store` row -- and mirrored like
any other, so an order placed under preview lands in the mirror under that
store's `storeId` and is already excluded from every read scoped to the live
one. A development store is created on a chosen plan, which is what lets both
entitlement adapters of D4 be exercised.

### D9 (derived) -- Every row a shopper writes carries the store it was written against

The mirror is scoped by `storeId` and so is safe under preview (D8). Pack rows
are MemQL-origin and are not: a review or an application submitted while
previewing would be a real row in the one database. So every shopper-written
concept in this program carries the `storeId` of the binding it was written
through, and every storefront read filters on it.

## 5. The staging model

In the owner's terms, then in the tree's.

1. **Prepare.** The storefront is built and published to a `shopify_storefront`
   site that is not serving. *Tree: `status: draft`, a candidate version, a
   preview binding naming the development store.*
2. **Exercise it, for real.** Catalog, cart, Shopify-hosted checkout, a test
   payment, and the resulting order arriving back in MemQL. *Tree: an
   authenticated operator is served the candidate, its runtime document
   carrying the preview binding. The engine can observe the catalog read, the
   cart, the `checkoutUrl`, and the mirrored order; the payment walk happens in
   a browser and is the product's test, not the engine's.*
3. **Flip it.** *Tree: `status: live` and the candidate promoted -- refused
   while the serving binding still names a development store (G2's guard).*
4. **Keep working.** A newer version is published, exercised the same way
   against the development store WHILE the previous one keeps serving the
   public, and then promoted.

Step 4 is the requirement that shapes the design: **preview is per bundle
version, not per site status.** A site whose status is `live` and whose serving
version is N must be able to show version N+1, under the preview binding, to
its operator and nobody else. A design that previews only a `draft` site
delivers step 2 once and never again.

## 6. Gaps

| | Gap | Evidence | Blocks |
|---|---|---|---|
| G1 | **CLOSED, memql#5534.** A storefront cannot reach Shopify. The policy the edge writes is the generic one for every kind: `connect-src 'self'` plus the site and identity origins; `img-src 'self' data: blob:`. A browser call to the Storefront API and every Shopify-hosted image are refused. Nothing in `component/edge/csp.go` or its tests names a store. | `policyForSite`, `component/edge/csp.go` | the whole product epic |
| G2 | No preview, and no go-live guard. A draft site is `http.NotFound` for everybody; `status` and `binding` are written independently, so a site can go live bound to test data, or be exercised against the real store and take real orders. | the `switch` in `component/edge/handler.go`; no occurrence of "preview" in `component/edge` | section 5 entirely |
| G3 | **Engine half CLOSED, memql#5535** -- the tail is the site's own choice, so a product may keep a multi-page bundle AND the storefront kind; the product half is memql-fylo#21. Runtime binding and build-time catalog are incompatible. The storefront bakes its catalog into prerendered pages; a bundle exercised against the development store and then promoted would serve the development store's products. The engine resolved this in its own design by making the kind an SPA -- `handler.go` gives it the `index.html` fallback because "a shopify_storefront IS a spa bundle" -- and the product declared `kind: static` to get the opposite behaviour. | `getStaticPaths` in `clients/storefront/src/pages/products/[handle].astro`; the resolution tail in `component/edge/handler.go` | Q1, and the product's catalog work |
| G4 | Two records of one store. `site.binding` and `v1:shopify:store` both hold the domain and the Storefront token reference, edited in two places, at two authorization tiers (composite owner tier; cluster owner). | 2.1, 2.3 | D5, and the guard in G2 |
| G5 | A shopper is nobody to MemQL. `createReview` is `@actor` and stamps `ownerUserId: actor.userId`; reviewspack ships no query, so nothing lists published reviews, and `@rowAuthz(owner=...)` would hide them from everybody but their author. OIDC federation is for operators. Unauthenticated routes are a declared allowlist (`component/server/unauthenticated_surface.go`). An anonymous wholesale applicant meets the same wall, and must be able to submit with NO JavaScript (2.8). | `examples/reviewspack/dsl/mutations.memql`; `docs/public/operate/auth/access-model.md` | both packs |
| G6 | Packs have no delivery path. A pack's Go half "is compiled in via build tags ... there is no runtime loading of the Go half, by design" (`docs/public/build/building-a-pack.md`), and no image sets a pack's tag. The carrier route that once did is retiring (memql#2472). | 2.5 | both packs |
| G7 | No application, enrollment or wholesale concept exists anywhere in the engine. Every primitive on both sides does: `company`, `companyContact`, `companyLocation`, `catalog`, `priceList`, `priceListPrice`, `quantityRule`, `draftOrder` mirrored; `approvalChain`, `creditLimit`, `quote`, `reorderList` native. The bridge between an applicant and an entitlement is what is missing. | `dsl/shopify/generated/`, `dsl/commerce/concepts.memql` | the wholesale channel |

## 7. The wholesale pack: the line, and the test for it

The owner's concern is a pack too rigid for a client to lay its own process
over. The test, applied to every concept before it lands:

> Could two clients with different approval processes both express theirs
> without editing the pack?

**The pack owns:** the application record and its states; a settings row
(applications open or closed, display) in the manner of `reviewSettings`; the
state-transition mutations with their authorization invariants; the builtin
that provisions an entitlement through the adapter of D4; and the `storeId` of
D9.

**The pack does not own:** who approves, or in what order; what is notified;
any form field beyond the minimum an application needs to be one; any surface.

**Client-specific fields are a related concept, not a blob.** This client
collects an EIN and the next will not. The client declares its own concept in
its own domain with an `@relationship` to the pack's application: typed,
queryable, and it keeps the pack's schema from drifting toward one client.

The storefront packs that exist set the convention. `reviewspack` and
`shopifypack` ship concepts, mutations, tools and builtins and **no automations
and no logic**. (`deploypack` and `referencepack` do ship both, so this is a
choice the storefront packs made, not a rule of the platform.) A client's
process attaches from outside: `use <pack>.concepts.{...}` and
`@trigger(concept="v1:<pack>:<concept>")`.

## 8. Open questions

Each is filed as an issue that blocks what it names.

- **Q1 -- Rendering and routing (owner; reverses D3).** Static bundle bound at
  runtime, or server rendering that has no runtime (D3, G3). Carries three
  sub-questions that are one decision: does the catalog render in the browser
  from the bound store, so one bundle can be previewed and promoted; does the
  kind keep the `index.html` fallback or gain a per-site choice, so a
  multi-page bundle can still 404; and is a public PKCE client accepted for
  this store's Customer Account API (UNVERIFIED). *Blocks the product's catalog,
  cart and account work, and the engine's resolution task.*

  **The ENGINE half is decided and built (epic 1, memql#5535), and it does
  not pre-empt the product half.** The tail became a property of the SITE
  rather than of the kind: `v1:platform:site.resolutionTail`, ABSENT meaning
  the kind decides exactly as before, `fallback` and `not_found` naming the
  two tails for any kind. So a multi-page storefront bundle can 404 WITHOUT
  declaring `kind: static` and giving up the binding, the storefront block
  and the policy of G1 -- and a browser-rendered one needs no declaration at
  all. `TestSiteKindEnumIsExactlyThreeValues` stands. Whichever way the
  product answers Q1, and if it answers differently later, the engine serves
  it and neither answer is a release.
- **Q2 -- Who is a shopper to MemQL (owner).** A declared, narrowly scoped
  unauthenticated route per pack; or a Shopify customer as a verified,
  constrained principal; or the first for applying and the second for anything
  that reads a buyer's own data. *Blocks the packs foundation, the wholesale
  pack and reviews.*
- **Q3 -- How a client-agnostic pack ships.** Promote storefront packs out of
  `examples/` into the default build, governed by `packState`, and decide
  whether a storefront pack defaults to enabled or disabled. *Blocks both
  packs.*
- **Q4 -- D7 itself.** The owner's law, so the owner's reading of it. *Blocks the
  preview epic.*
- **Q5 -- The client's existing reviews.** If the store uses a reviews app
  today, that data is the app's and cannot be mirrored (the connector record,
  1.2). Import, run beside, or "not carried". *Blocks the product's reviews
  work; answered by the app inventory the checklist already requires.*
- **Q6 -- Deploy authority for product DSL.** Adding DSL to the package brings
  back `dsl_requires_authoring` and a cluster roll on every DSL change (2.6),
  which is why it was parked. `413db4e` names the alternative: the DSL in its
  own tree, "delivered as a bundle image pinned in the overlay". *Blocks every
  product task that writes DSL.*
- **Q7 -- The path has never run end to end.** The storefront kind was verified
  here at the level of its model and its document contract only, and G1 shows
  it could not have run. *ANSWERED (epic 1, memql#5536): a fixture bundle is
  published, served, and read in a browser. Two legs, because the cluster one
  skips without a cluster -- `test/clustere2e/storefront_serving_test.go`
  publishes it to a real cluster through the ordinary authorized path, and
  `component/edge/storefront_fixture_serve_test.go` serves the same directory
  through the real handler in the ordinary lane. Neither runs a browser, and
  both say so.*
- **SETTLED (epic 1, memql#5534).** The hosts a storefront needs admitted,
  against Shopify's own reference storefront rather than by inference: the
  bound store's origin from `binding.storeDomain`, plus the three fixed hosts
  Hydrogen's `defaultDirectives` names -- `cdn.shopify.com` (assets, and the
  client SDKs' own fetches), `shopify.com` (where the Customer Account API's
  token exchange and GraphQL endpoints resolve; the AUTHORIZE step is a
  navigation and needs no source), and `monorail-edge.shopifysvc.com` (the
  analytics beacon). They reach `connect-src`, and the asset hosts also reach
  `img-src` and a `media-src` that exists on no other kind. `script-src` is
  NOT widened, and there is no wildcard. The operator table is
  [the checklist's section 6](../../public/operate/shopify-storefront-checklist.md).

## 9. The seven epics

Five in the engine, two in the product. Grouped by what can ship and be
verified on its own, which is why the engine work is not the three epics first
proposed: G1 makes serving a problem of its own that precedes everything, and
Q2 with Q3 form a foundation both packs stand on, so reviews need not wait for
wholesale.

### Engine

**Epic 1 -- The storefront kind can serve a storefront (priority P21,
`epic:storefront-serving`).** The policy admits the bound store (G1); the
resolution tail is decided and built (the engine half of Q1); a fixture
storefront bundle proves the path in cluster-e2e (Q7); the checklist is
corrected. One PR. *Nothing in the product epic works before this.*

**Epic 2 -- The storefront deployable owns its store (priority P22,
`epic:storefront-owns-store`).** The binding references a store row instead of
copying it, with the authorization tiers reconciled (G4); a store, and a
development store, are attached through the storefront; the package manifest
binds by reference; the `app:stores/*` capabilities and seeds retire with the
OS change. Two PRs. *OS surface tracked separately.*

**Epic 3 -- Storefront preview (priority P23, `epic:storefront-preview`).** D7
confirmed and recorded (Q4); the candidate version and the preview binding on
the site row; the edge serves a candidate to an authorized operator, for a
draft site and for a live one (section 5, step 4); the go-live guard; what the
engine can observe of a preview, recorded for the OS to read. Two PRs. *OS
surface tracked separately.*

**Epic 4 -- What any storefront pack needs (priority P24,
`epic:storefront-packs-foundation`).** Q2 and Q3 decided; packs in the default
build under `packState`; the shopper write path, including a same-origin
endpoint that accepts a plain HTML form post and answers with a redirect; abuse
controls on every route it declares; reviews promoted, given the public read it
lacks and D9's `storeId`. Two PRs.

**Epic 5 -- The wholesale pack (priority P25, `epic:wholesale-pack`).** Section
7, built: the application and its states, the settings row, the transitions and
their invariants, the entitlement seam with the native-B2B adapter and the
plan-independent one, and the test of section 7 run as a test -- two fixture
clients with different processes over one unedited pack. Two PRs. *OS surface
tracked separately.*

### Product (`memql-fylo`)

**Epic 6 -- The storefront as a site (priority P02,
`epic:storefront-as-a-site`).** Q1 answered; the runtime document read; the
catalog, cart and checkout hand-off against the bound store; the app inventory
(Q5); the development store attached; published as a `shopify_storefront`
deployable; and the end-to-end walk of section 5 written down and run. Three
PRs.

**Epic 7 -- Commerce packs adoption (priority P03,
`epic:commerce-packs-adoption`).** Q6 answered; the product's DSL domain, held
to the edition gate; reviews displayed and submitted; the wholesale application
wired to the pack with the EIN in the body of a plain form post; the client's
own process over the pack's concepts; wholesale prices for an entitled buyer.
Three PRs.

## 10. Order and dependencies

```
Epic 1 serving  ------------------------------>  Epic 6 (catalog, cart)
   |                                                  |
Epic 2 owns-store  -->  Epic 3 preview  ----------->  Epic 6 (publish, the walk)
                                                      |
Epic 4 packs foundation  -->  Epic 5 wholesale  --->  Epic 7
        \------------------------------------------>  Epic 7 (reviews)
```

- The decision issues come first in their epics and block the tasks that
  depend on them: Q1 (Epic 6, mirrored in Epic 1), Q4 (Epic 3), Q2 and Q3
  (Epic 4), Q6 (Epic 7).
- Epic 6's catalog and cart need only Epic 1. Its publish step should follow
  Epic 2's model task, or the manifest is written twice. Its end-to-end walk
  needs Epic 3.
- Epic 3's guard wants to know a store is a development store, which is
  Epic 2's store row.
- Epic 7's reviews need Epic 4 and not Epic 5, which is the point of
  separating them.

## 11. Testing

- **Epic 1:** a policy test per kind that fails if a storefront's policy stops
  naming its bound store, and a twin proving `spa` and `static` policies are
  byte-identical to today's; a cluster-e2e leg serving a fixture storefront
  bundle against a stubbed Storefront endpoint.
- **Epic 2:** the binding resolves through the store row; a store row edit is
  visible to the site with no second write; the manifest round-trips; a
  capability sweep finds no `app:stores/*` reference left.
- **Epic 3:** an unauthenticated request to a draft site is still 404, and to a
  candidate is still the serving version; the guard refuses each of its cases;
  `TestNoEnvironmentBranchingInEngineCode` stays green with an empty exemption
  map, which is the assertion that D7 was honoured.
- **Epic 4:** every declared route is on the allowlist and rate limited; a form
  post with no JavaScript round-trips; a disabled pack is inert; a row written
  under one `storeId` is invisible under another.
- **Epic 5:** section 7's two-client fixture; each adapter against a development
  store on a plan that supports it.
- **Epics 6 and 7:** `scripts/dev/check-package.go` and
  `scripts/dev/edition-gate.sh` green; the walk of section 5, written as a
  runbook a second person can follow.

## 12. Issues

Filed 2026-09-20, all `claude`-labeled, each epic carrying its `epic:<slug>`
label and its program priority.

| Epic | Issue | Tasks |
|---|---|---|
| 1, serving | #5529 | #5534 the policy admits the bound store; #5535 the resolution tail decided and built; #5536 the fixture bundle in cluster-e2e; #5537 the checklist corrected |
| 2, owns its store | #5530 | #5538 one record of a store; #5539 attach through the storefront; #5540 the manifest binds by reference; #5541 the Stores capabilities retired; #5542 the operator docs follow |
| 3, preview | #5531 | #5543 preview is not an environment (decision); #5544 candidate version and preview binding; #5545 the edge serves a candidate; #5546 the go-live guard; #5547 what the engine observes; #5548 the cluster-e2e leg and docs |
| 4, packs foundation | #5532 | #5549 how a pack ships (decision); #5550 who a shopper is (decision); #5551 the shopper write path; #5552 storeId on shopper-written rows; #5553 reviews made consumable; #5554 the pack guide |
| 5, the wholesale pack | #5533 | #5555 the application and settings; #5556 transitions and invariants; #5557 the entitlement seam; #5558 the native-B2B adapter; #5559 the plan-independent adapter; #5560 the two-client test; #5561 the adoption guide |
| 6, the storefront as a site | memql-fylo#19 | memql-fylo#21 rendering and routing (decision); memql-fylo#22 the app inventory; memql-fylo#23 read the runtime document; memql-fylo#24 the catalog from the bound store; memql-fylo#25 cart and checkout hand-off; memql-fylo#26 the development store attached; memql-fylo#27 published as a storefront deployable; memql-fylo#28 the end-to-end walk |
| 7, commerce packs adoption | memql-fylo#20 | memql-fylo#29 deploy authority for product DSL (decision); memql-fylo#30 the product's DSL domain; memql-fylo#31 reviews displayed; memql-fylo#32 reviews submitted; memql-fylo#33 the wholesale application wired; memql-fylo#34 the client's own process; memql-fylo#35 wholesale prices for an entitled buyer; memql-fylo#36 the existing reviews |

Engine issues are in `znasllc-io/memql`; `memql-fylo#N` is `znasllc-io/memql-fylo`. The five
decision issues -- #5543, #5549, #5550, memql-fylo#21 and memql-fylo#29 -- open their epics and block the
tasks that depend on them.
