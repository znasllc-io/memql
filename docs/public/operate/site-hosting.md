---
title: Site hosting
audience: public
status: stable
area: operate
sinceVersion: 0.18.0
owner: znas
---

# Site hosting

A MemQL cluster hosts a site the same way it hosts everything else: as a
graph row. This page is the runbook for going from a built application to a
working, rollback-able site at its own hostname on the cluster's front
door -- what a build has to emit, how to publish and roll one back, and
where the real limits are.

Read [front-door.md](front-door.md) first if you have not -- it covers the
six host rules and the `*.<domain>` / apex routing every hosted site rides
on. This page does not restate it.

Related: [The cluster front door](front-door.md) ·
[Connected -- a site that stays where it is](connected-integration.md) ·
[MemQL Portal](portal.md) ·
[Service-account JWTs](auth/service-account-jwt.md)

---

## The contract: a build is a directory of static files

**A site build emits a directory of files with `index.html` at its root.
That is the whole interface.**

The edge (`component/edge`) serves those bytes; it never runs your site.
There is no Node.js in the edge image -- Node is a build-time dependency
(whatever tool produced the directory) and never a runtime one. This follows
directly from how the edge actually answers a request
(`component/edge/handler.go`): it resolves the request `Host` to a
`v1:platform:site` row, opens that row's bundle as a filesystem, and serves
a file from it. There is no process per site, no port per site, and nothing
to execute -- which is the concrete meaning behind decision D5 (a site is
data, not infrastructure) and D11 (dynamic data comes from the graph;
prerendering is a build concern) in
`docs/superpowers/specs/2026-08-13-cluster-front-door-design.md`.

**Works, and mixable per site** -- the edge does not know or care which tool
produced the directory, so two sites on one cluster can use different
stacks: React/Vite, Vue, Svelte, Astro, Next.js with `output: 'export'`,
Hugo, Eleventy, or plain HTML.

**Does not work** -- anything that needs a server at request time:

- Next.js with SSR, ISR, middleware, or API routes
- Remix
- Nuxt in SSR mode
- SvelteKit's `node` adapter

Naming these explicitly matters more than the abstract rule: someone will
reach for one of them, and finding out at deploy time -- a build that
"succeeds" and then serves broken routes, or an adapter that expects a Node
process nothing here provides -- is worse than reading it here first. (A
site built with one of these is not locked out of MemQL entirely; see
[The escape hatch](#the-escape-hatch-a-site-that-genuinely-needs-ssr) below.)

### You are not losing a server. The server is MemQL.

The instinct is that "static only" means giving up a backend. It does not --
it means the backend moves. A Next.js API route becomes a MemQL integration
plus DSL: a language change, not a capability loss (see "Extension Points"
in the top-level `CLAUDE.md`). Data a server-rendered route would have
fetched and stamped into HTML instead lands in the graph -- via
[`POST /inbound/{source}`](inbound-delivery.md) for third-party push, or a
scheduled automation for pull -- and the browser reads it same-origin
through `/_memql/*` ([below](#apiproxy-same-origin-access-to-your-data)).

The concrete win this buys: because the browser talks to your OWN cluster
for data rather than to a third-party API directly, no third-party
credential (a Shopify Storefront token, a CMS API key) ever has to ship to
the browser at all -- a conventional headless setup calling the vendor
directly cannot make that claim.

---

## Freshness has three layers

A build's HTML is a snapshot. What keeps a snapshot from going stale in
front of a real visitor is what happens after the page loads, and there are
three layers, each answering a different question:

1. **Prerendered HTML** -- what the build wrote at build time. This is what
   a crawler sees and what paints first for a human. It is exactly as fresh
   as the last build, no fresher.
2. **Hydrate from the graph** -- once the bundle's own JS runs, it reads
   current values same-origin through `/_memql/*` and replaces whatever the
   build guessed. This is MemQL's own data: price, stock, whatever your
   inbound automations keep current in the graph.
3. **Live vendor calls from the browser** -- for the parts that must stay
   the vendor's, not MemQL's (a Shopify cart, a hosted checkout redirect),
   the browser talks to the vendor directly, exactly as it would from any
   other frontend.

This is decision D11: *"Prerendered HTML is a snapshot for crawlers and
first paint; live values come from the graph on hydrate."* Skip layer 2 and
a price change is wrong on every page until the next build.

---

## SEO is not a compromise

Say it once, plainly, because "SSR is non-negotiable for SEO" is common
enough that silence here would read as agreement: **client-side-only
rendering is what that sentence actually rules out, not prerendering.**
Googlebot needs populated HTML at crawl time; it does not care when that
HTML was populated. A route this platform prerendered at build time and a
route some other platform server-rendered on request are, to a crawler, the
same artifact -- HTML with the content already in it. Static satisfies the
SEO requirement identically to SSR, provided the routes that need to rank
are actually prerendered -- which is the next section's whole subject.

---

## The commerce case: why "headless" doesn't need a server

"Headless commerce" is usually assumed to need a framework this platform
cannot serve. It is worth checking that assumption against what a
storefront actually needs a server FOR. Using Shopify as the concrete case
researched for this page:

| Capability | Needs a server? | Why |
|---|---|---|
| Product / collection reads | No | The public Storefront API token is designed for browsers; it scales by buyer IP |
| Cart create / add / update | No | The Cart API is client-side |
| **Checkout** | **No** | `cart.checkoutUrl` redirects to Shopify's own HOSTED checkout page; the old Checkout API was shut off 2025-04-01 |
| Customer accounts | No | The Customer Account API supports public OAuth PKCE clients -- built for SPAs |
| Inbound events reaching MemQL (orders, inventory) | Yes | `POST /inbound/{source}` -- see [inbound-delivery.md](inbound-delivery.md) |
| Admin / private-token operations | Yes | A MemQL integration; the private token never reaches a browser |

**Payments and PCI are Shopify's**, not this platform's and not the site's --
that is the fact that makes static-only viable for commerce at all. A
storefront that never touches a card number does not need the server that
would handle one.

### Freshness, honestly

Even server-rendered / ISR storefronts serve stale prices during their
rebuild window (production reports put ISR's window around 90 seconds). The
axis was never fresh-versus-stale -- it is *how* stale, and the genuinely
fresh answer is the same regardless of hosting model: **read price and
stock client-side, at render**, which is exactly layer 2 above. Static with
hydration and SSR with hydration converge on the same freshness once you
account for what each actually guarantees at first paint; the difference is
only in what shows up before the JavaScript runs.

---

## The prerender budget

Do not read the sections above as "prerender everything." Static generation
at build time breaks down well before it sounds like it should: reports
from real deployments put build-time and API-rate-limit exhaustion setting
in well before 50,000 SKUs, and an architecture that works cleanly at 500
SKUs can already mean 45-minute build windows.

**Prerender what earns it: collections, best-sellers, landing pages.** Page
40 of a paginated catalog has no SEO value -- nobody searches for it by
position -- and prerendering it anyway spends build time and vendor API
calls for nothing. Let the long tail render client-side, the same as any
page whose data changes too fast to bake in.

Size this before committing to it, not after a build starts timing out in
CI. The failure mode is build time and vendor rate limits, not a runtime
error, so it will not show up until someone runs the numbers or a build
starts failing on its own.

---

## Recommended stack: Astro

Not a taste preference -- the islands pattern **is** this edge's model, with
no adaptation required. A build emits static HTML for everything (product
pages, landing pages) and hydrates only the pieces that need to be
interactive (cart, variant selector, search) as independent islands. That
is exactly the split this page has been describing: static by default,
dynamic where it earns it. Off-the-shelf integrations already ship this
shape for commerce specifically (`thomasKn/astro-shopify`, Hermes
Commerce) -- product pages as zero-JS static HTML, cart and search
hydrating as islands.

---

## Bake vs upload: choosing a `bundleRef` scheme

A site's `bundleRef` (`v1:platform:site`, `dsl/platform/concepts.memql`) is
a URI, and its scheme decides how a deploy actually happens:

| Scheme | Example | Deploy is | Rollback is | Use for |
|---|---|---|---|---|
| `file://` | `file:///app/sites/shop` | An image rebuild + rollout | An image rollback | Something that ships WITH the cluster -- baked into the engine image at build time, or a working-tree path for the dev inner loop |
| `blob://` | `blob://sites/shop/v3f9a2.../` | An upload plus a row write, in seconds | One more row write | Something that changes on its OWN clock |

The guidance is about cadence, not mechanism: **bake what ships with the
cluster, upload what changes on its own clock.** A typo fix on a landing
page must not roll the node serving every other site on the cluster, and
marketing copy diverges from engine release cadence immediately -- a
content team publishing a sale banner should never be blocked on, or
coupled to, an engine deploy.

The portal itself is the worked example of the baked case: its row's
`bundleRef` is `file:///app/portal` (`dsl/platform/seeds.memql`), because
the platform's own console has to resolve the moment the cluster boots and
rolls out exactly on the engine's own cadence. A customer's storefront is
the opposite case -- it will change far more often than the engine does, so
`blob://` is almost always the right choice for one.

---

## Deploying a site, end to end

**The portal's Deployables screen is where most of this happens.**
`/deployables` (`clients/portal/src/deployables/`, memql#4346 -- the Sites
screen from memql#3717, renamed and widened; `/sites` redirects there) lists
the caller's own sites, or every site in the cluster for a cluster owner, with
a form to create one and a detail screen per site (`/deployables/:siteId`) to
deploy, roll back, change status and delete. What follows is the underlying
graph writes those controls perform; reach for the portal first, and use the
raw mutation calls below when scripting a step (CI, an install wizard, a
one-off fix) instead of clicking through it.

**One thing the portal does NOT do, which is why step 2 below still exists as
an ops task: register the hostname.** Getting bundle bytes into storage used
to be the second one; it is not any more. A person uploads a zip to their
Library and the portal deploys it with `sitePublishFromArtifact`, which reads
the bytes from object storage server-side -- see
[Deployables](deployables.md#deploying-from-the-library).

### 1. Create the site row

`createSite` (`dsl/platform/mutations.memql`) is the write behind the
portal's "New deployable" form -- it is the only way a site starts existing. It
requires a `bundleRef` up front -- there is no "empty" state in the
schema -- so for a brand-new uploaded site, pass a placeholder prefix and
leave `status` at its default:

```
mutation createSite(
  siteId: "shop",
  hostname: "shop.memql.localhost",
  bundleRef: "blob://sites/shop/pending/",
  apiProxy: true,
  title: "Shop"
)
```

Three things worth knowing before you run it:

- **`status` defaults to `"draft"`**, which is unconditionally unreachable
  (see [Status](#status-draft-live-disabled) below) -- so a placeholder
  `bundleRef` pointing at nothing yet is harmless. Nobody can request the
  site until you flip it live.
- **`site` is `@rowAuthz(owner="ownerUserId", clusterOwner)`**
  (`dsl/platform/concepts.memql`) -- the COMPOSITE tier: the row's owner,
  or a cluster owner. **This supersedes decision D6**, which recorded a
  plain `clusterOwner` tier and the reason for it: an owner-tier
  alternative makes "list every site" and cluster-wide hostname uniqueness
  unenforceable, because `enforceRowAuthzOnPlan` ANDs the owned predicate
  with no cluster-owner escape. That reason still holds, and the composite
  is what satisfies it while also making deployables self-serve
  (memql#4344, design D3): it injects
  `(ownerUserId==actor.userId)||(actor.isClusterOwner==true)`, so a user
  reads and writes exactly their own sites and an operator reads every row
  in the cluster. `admin` is still not among them -- `AccessContext.
  IsClusterOwner()` checks `Role == RoleOwner` exactly
  (`component/auth/access_context.go`).
- **A user creating a site owns it; a cluster owner creating one creates
  the deployment's.** `createSite` stamps `ownerUserId` from the actor, and
  a write made as the deployment (a cluster owner, a system actor, or
  trusted server-side Go) has that stamp UNDONE, leaving the row
  cluster-owned -- an empty `ownerUserId`, which is what the seeded portal
  carries. See `component/memql/platform_site_hostname_policy.go`.
- **A user's hostname must be `<slug>.<domain>`** -- slug `[a-z0-9-]{3,40}`,
  cluster-unique, and not one of `api`, `identity`, `mcp`, `portal`, `www`,
  `admin`, `mail` or the apex, under the domain the cluster serves (derived
  through `component/frontdoor`, so it cannot disagree with the front door's
  own hosts). Any other hostname stays cluster-owner-only and hand-certified,
  for the reason [Limits](#limits) gives. Hostname UNIQUENESS binds every
  caller including a cluster owner: the edge resolves a request Host to one
  row, so a second live row on the same hostname makes which site answers
  depend on row order.
- **A caller who owns no sites is shown an empty list, not an error.**
  `sitesAll` and `siteById` (`dsl/platform/queries.memql`) carry the
  composite predicate as an explicit filter conjunct, so a read comes back
  with the caller's own rows and nothing else -- not an error, just fewer
  rows, which reads exactly like "there are no sites" if nothing else says
  otherwise. If you are scripting against the raw queries, keep that in
  mind: "zero rows" and "you cannot see the others" are different facts,
  and only one of them is a
  bug.

### 2. Add the hostname

**In the cloud, routing needs nothing; TLS depends on which issuer the overlay
declares.** The wildcard DNS record and the `*.<domain>` Ingress rule
(committed once at install -- decision D2) route any `<label>.<domain>`
hostname to the edge the moment a live site row names it, so the bytes are
served end to end.

**With a DNS-01 issuer, the certificate follows for free** (memql#4347). Both
cloud overlays ship a `letsencrypt-dns01` ClusterIssuer whose Azure DNS solver
authenticates as a managed identity, and one wildcard `Certificate`
(`memql-wildcard-tls`) for `*.<domain>` plus the apex; the edge Ingress's
wildcard rule carries it under `tls`. A freshly deployed site is live over TLS
with no operator step -- which is what makes self-serve deployables real.
Install-time prerequisites (an Azure DNS zone for the domain, the identity's
`DNS Zone Contributor` role on that zone, the federated credential) are in
[azure-entry-install.md](azure-entry-install.md).

**Without one, the pre-#4347 rule still stands** (memql#4224). The generated
front-door certificate names exact hosts only -- `api.`, `identity.`, `mcp.`,
`portal.` and the apex -- because HTTP-01 cannot issue a wildcard and a single
wildcard SAN fails the whole ACME order. A site routed by the wildcard then
terminates TLS with the ingress controller's self-signed default until it has
a cert-manager `Certificate` for its hostname and an exact-host Ingress
pointing at `svc/edge:8085` -- the same shape as the generated
`portal-front-door`. That is one Kubernetes object pair per site, an explicit
exception to "a site is data" forced by HTTP-01.

The render gate decides which regime applies by reading the issuer's SOLVER,
not its name, so an issuer created out of band counts as HTTP-01 and #4224's
exact-host assertion holds by default rather than needing to be re-chosen.
Locally the mkcert pair IS a wildcard, so a site that works over https on the
local cluster is still no evidence that it has a certificate in the cloud.

**Locally, there is no wildcard hosts entry**, so the new hostname has to
be added to the managed block `scripts/install/hosts-entries.sh` owns
(front-door.md covers why: a developer machine has no wildcard DNS).

> **WARNING: `--action=add` REPLACES the managed block; it does not merge
> into it.** Passing only your new hostname drops `api.`, `identity.`,
> `mcp.`, `portal.` and the apex from `/etc/hosts` along with it --
> `render()`'s `upsert` mode discards every line of the existing managed
> block before writing the new one
> (`scripts/install/hosts-entries.sh`). Pass the **complete** set you want
> resolvable, not just the addition:

```bash
sudo scripts/install/hosts-entries.sh --action=add \
  --hostnames=api.memql.localhost,identity.memql.localhost,mcp.memql.localhost,portal.memql.localhost,memql.localhost,shop.memql.localhost \
  --confirm=add-memql-hosts
```

A hostname outside the cluster's own domain -- a genuine custom domain -- is
a different, install-time change; see [Limits](#limits) below.

### 3. Publish the first bundle from CI

`POST https://api.<domain>/sites/<siteId>/bundles` is the atomic publish
endpoint (`component/server/site_bundle_handler.go`, mounted on the bff;
CLAUDE.md's endpoint-protocol exception table, memql#3713). It is HTTP, not
gRPC, for the same documented reason `/spaces/{id}/attachments` is: a CI
job hands over an arbitrary, variable-shaped file tree -- unknown paths,
unknown count, mixed content types -- which is exactly what multipart
form-data is for and exactly what a fixed protobuf schema is not.

**This is a CI/automation credential, not something a signed-in human can
use.** The handler requires a `class="service_account"` identity-issued JWT
and checks it itself -- an ordinary signed-in operator's session carries
`class="user"` (or no class claim at all, which resolves the same way) and
gets a `403`, not a `401`: authenticated, just the wrong kind of
credential. This is a SEPARATE authorization decision from the
`service_account` gRPC interceptor documented in
[service-account-jwt.md](auth/service-account-jwt.md) -- that interceptor
pins `MemqlService.Stream` traffic to reads plus one agent-turn message
type; this is a plain HTTP handler on the bff checking the same `class`
claim on its own, independently.

**What a signed-in person CAN do in the portal is the other half of
this.** `/deployables/:siteId`'s "Point at a bundle reference" control
(`DeployableDetailPage.tsx`) calls `updateSiteBundle` directly with a
`bundleRef` VALUE and flips the row -- exactly the same write
[Rollback](#rollback) below uses, in the other direction. It does not touch
bytes and cannot:
`updateSiteBundle` has no way to know whether the prefix it is pointing at
has anything in it. The two halves are complementary, not redundant -- CI
puts bytes at a `blob://` prefix (this section) and gets back the
resulting `bundleRef`; the portal is where a human then points a site's
row at a `bundleRef` that already exists, whichever process produced it.
A reader who has just learned a browser session cannot upload bytes here
is usually asking what it CAN do -- this is the answer.

Mint a token the same way the deploy gate does:

```bash
TOKEN="$(kubectl -n memql exec deploy/identity -- \
  memql service-account-token mint --label ci-publish --subject system:ci-publish)"
```

It is short-lived by design (`DefaultServiceAccountTokenTTLSeconds`, 1
hour, no refresh path) -- mint one per CI run rather than storing it as a
long-lived secret. There is no `v1:identity:identity` row to individually
revoke a service-account token, so expiry and identity signing-key
rotation are the only ways a leaked one stops working.

Every file in the bundle rides its own multipart part, named for its path
**within** the bundle via the form field NAME, not the filename -- the
handler's own doc comment explains why: `multipart`'s stdlib parser runs
every `filename` through `filepath.Base`, which would silently collapse
`assets/app.js` and `css/app.js` onto the same name.

```bash
args=()
while IFS= read -r -d '' f; do
  rel="${f#./}"
  args+=(-F "${rel}=@dist/${rel}")
done < <(cd dist && find . -type f -print0)

curl -sS -X POST "https://api.memql.localhost/sites/shop/bundles" \
  -H "Authorization: Bearer ${TOKEN}" \
  "${args[@]}"
```

A successful publish answers `201` with the version it produced:

```json
{ "version": "v3f9a2b1c7d4", "bundleRef": "blob://sites/shop/v3f9a2b1c7d4/" }
```

**Log that response.** It is your rollback record -- see
[Rollback](#rollback) below.

The endpoint refuses a bundle with no `index.html` before uploading
anything, and bounds both the site id and every file path against
traversal (`fs.ValidPath`, the same boundary the read side enforces) --
the request path and the multipart field names arrive from the internet
even though the row the bundle lands against came from an operator.

> **This is why step 1 has to happen first.** The publish endpoint's
> underlying write, `updateSiteBundle`, is an `update()` mutation, and
> `update()`'s contract is that the row must already exist
> (`component/memql/executor_mutation.go`: *"update()'s only extra
> contract is that the row MUST already exist ...; a missing row is an
> error pointing the caller at insert()"*). Publishing against a site id
> nobody has created yet fails.

### 4. Go live

Flip status once you have confirmed the real bundle is in place:

```
mutation updateSiteStatus(siteId: "shop", status: "live")
```

---

## Status: draft, live, disabled

- **`draft`** -- resolves for nobody. `404`, identical to an unknown
  hostname, because as far as the internet is concerned neither exists.
- **`live`** -- serves.
- **`disabled`** -- `503`, deliberately different from `draft` or an
  unknown host. A site somebody paused on purpose and a hostname somebody
  mistyped are different situations, and whoever is debugging one needs to
  be able to tell which from the status code alone
  (`component/edge/handler.go`: *"STATUS BEFORE ANY FILE LOOKUP ... a
  DISABLED site is 503, deliberately"*).

Only a `live` site reaches anything past that check -- including
`GET /runtime-config.json` ([below](#apiproxy-same-origin-access-to-your-data)),
which is dispatched after the status switch, not before it.

**`deleted` is a separate flag, not a fourth status value** -- worth
saying plainly, because it is the state a reader will otherwise try to
infer from the three above. `deleteSite` (`dsl/platform/mutations.memql`,
memql#3717) soft-deletes by stamping `deleted: true`; the row survives,
time-series history intact, but `isNotDeleted` is now an explicit
conjunct on `siteByHostname`, `sitesAll` AND `siteById`
(`dsl/platform/queries.memql`), so a deleted site simply stops matching
`siteByHostname` -- the edge resolves it exactly like an unknown
hostname, `404`, the same bucket as `draft`, not a status code of its
own.

**Deleting a `systemOwned` row is refused server-side, not just hidden
behind a disabled button.** `component/memql/platform_site_delete_guard.go`
runs on every write that would set `deleted: true`, reads the PRIOR row's
`systemOwned` flag, and refuses (naming the hostname in the error) unless
the caller is a system actor. That is what actually protects the portal's
own row -- `systemOwned`'s doc comment has said "blocks deletion" since
before this guard existed, and nothing enforced it until this landed. The
portal's own detail screen (`DeployableDetailPage.tsx`) disables the delete
control for a `systemOwned` row as a courtesy; the real gate is this
write-path check, reachable and effective against a raw mutation call
too.

---

## Resolution order: `kind`, spa versus static

`kind` (`spa` or `static`) governs only the LAST rung of resolution -- what
happens when nothing else matched. The full order
(`component/edge/handler.go`'s `resolveAsset`, decision D11):

1. The exact file at the request path.
2. `<path>/index.html`.
3. `<path>.html` -- the rung that makes prerendering worth doing at all.
   Without it, an `spa` site's fallback would serve `index.html` for every
   route regardless of what the build actually emitted, and a crawler
   would see one page no matter how many routes you prerendered.
4. **`spa`**: falls back to `index.html` (client-side routing takes over).
   **`static`**: `404` -- a mistyped path in a multi-page site should be
   visible, not silently rendered as the home page.

`index.html` is never cached (`Cache-Control: no-cache`) so a deploy
reaches a returning visitor; everything else is served
`immutable, max-age=31536000` because it is content-addressed by the
build.

---

## `apiProxy`: same-origin access to your data

A site's `apiProxy` flag mounts `/_memql/*` on the site's own origin,
reverse-proxied to the bff (`component/edge/proxy.go`). Turn it on for any
site whose bundle calls back into MemQL -- which, per
[the contract](#the-contract-a-build-is-a-directory-of-static-files) above,
is essentially every site hosting more than pure marketing copy.

What it removes is worth stating alongside what it adds: the bff's own
auth cookie is `SameSite=Lax`, which browsers do not send cross-site at
all, so a site calling `api.<domain>` directly would need CORS AND a token
held in memory instead of the cookie, plus the cluster domain compiled
into the bundle. Same-origin through `/_memql/*` needs none of that; see
[connected-integration.md](connected-integration.md) for the full
cross-origin story a site does NOT have to deal with once this is on.

Two things to know before flipping it on:

- **It needs `MEMQL_EDGE_API_TARGET` set on the edge node** -- the bff's
  plain-HTTP address, e.g. `http://bff-http:8085`, never the gRPC one.
  Unset, the proxy refuses `/_memql/*` for every site regardless of any
  individual site's `apiProxy` value, and the edge logs a warning once at
  boot rather than staying silent about it.
- **A site with signed-in users needs more than the proxy.** Not because
  the token exchange is cross-origin -- it is not. `POST /oauth/token`,
  `/auth/refresh`, `/auth/logout` and `GET /.well-known/jwks.json` are
  `fetch()` calls that stay on the SITE's own origin: `runtime-config.json`
  publishes an empty `identityApiBaseUrl`, and the edge forwards those four
  exact paths to the identity service (`component/edge/identity_proxy.go`).
  That is a separate forwarder from `/_memql/*`, which only ever targets the
  bff, and it is not gated on `apiProxy`. What IS cross-origin is the
  top-level **`/authorize` navigation**, which still goes to
  `identity.<domain>` -- so the site's own `https://<hostname>/auth/callback`
  has to be registered in `MEMQL_IDENTITY_REGISTERED_CLIENTS` before sign-in
  can return anything, and no proxy setting substitutes for that. The edge's
  CSP names the cluster's identity origin in every site's `connect-src`
  regardless (`component/edge/csp.go`), so a client still calling identity
  cross-origin is not additionally broken by a policy violation. The symptom
  of that entry being missing for such a client is worth knowing: a
  navigation is not governed by `connect-src`, so sign-in visibly proceeds
  and then fails silently at the very last step, with nothing in identity's
  own logs to explain why.

Every LIVE hosted site also gets `GET /runtime-config.json` for free -- not
an opt-in, unlike the proxy -- carrying the cluster's identity URL, its
configured `domain` (`MEMQL_DOMAIN`, so a client that has to NAME this
cluster somewhere else need not reverse-engineer it out of the issuer), and
(matched by the requesting hostname against the cluster's registered OAuth
clients) the site's own client id. An unregistered site still gets a `200`
with an empty client id rather than an error: the honest answer for "this
site has no client to present."

---

## Live data in a hosted site

The reactive path is not hosted-only, and it is not a new mechanism to
learn: MemQL carries structured CDC subscriptions on the same wire every
query and mutation rides.

The TS SDK exposes it as `subscriptions.subscribeGraph(handler, { concept,
actions })` -- filtered by concept type id and by verb (`created` /
`updated` / `deleted`); the server composes the bus topic, so no client
ever writes a topic string. Rather than a fresh example, read the
platform's own working one: `clients/portal/src/cluster/useConceptRows.ts`
queries a page once, opens a subscription, and applies live events on top
of it -- the shape any hosted site's list view wants. With `apiProxy: true`
this connection is same-origin through `/_memql/*`: no CORS, no second
credential, no separate realtime vendor to operate.

This belongs directly after the "static only" constraint because it is
most of the answer to it: **SSR optimizes the first paint and does nothing
for every update after that.** Any live view opens a persistent connection
regardless of how its first byte was produced, so the moment a page has
hydrated, the static and SSR paths are identical -- the more reactive an
application is, the less SSR actually buys it.

**Reads are AUTHENTICATED by default, and that is what you promise a
client.** A logged-in view gets live updates for free. A page nobody has
signed into is a different promise, and it takes two deliberate steps.

- **Logged-in views** -- effortless, the default, no extra design needed.
- **Anonymous public pages** (a stock counter a logged-out storefront
  visitor is looking at) -- the public tier, below. Two decisions, one by
  the operator and one by the author, and neither falls out of turning
  `apiProxy` on.

### Serving a page to a visitor who has not signed in

Two independent steps, in this order. Doing only the first publishes
nothing; doing only the second serves nothing.

**1. The operator opts the cluster in.** Set
`MEMQL_PUBLIC_READS_ENABLED=true`. Default is false, which is why every
existing cluster is unaffected: with it off, a WebSocket dial carrying no
credential is refused exactly as it always has been.

Turning it on does not publish anything by itself. It opens a door into a
graph where no concept declares the public tier, so every read through it
refuses until step 2. The node logs a WARN at boot saying so.

**2. The author declares the tier on the concepts that hold the content.**

```memql fragment
@rowAuthz(public)
concept productPage { ... }
```

Declare it in your PRODUCT BUNDLE, on your own content concepts. Nothing
in the engine tree declares it and a conformance gate keeps it that way --
so an operator who enables the flag to serve one bundle's content is not
silently publishing anything else.

**What an anonymous session can and cannot do.** The rules are enforced,
not conventions:

| | |
|---|---|
| Read a concept declaring `@rowAuthz(public)` | yes |
| Subscribe to its graph events | yes, admitted per row by the same function the read uses |
| Read a concept declaring any other tier | refused |
| Read a concept declaring NOTHING | **refused** |
| Write anything, anywhere | refused -- there is no anonymous write |
| Reach the AI, tool, identity or admin messages | refused at the stream |

The undeclared row is the one worth reading twice. Most concepts in a
tree declare no tier at all, and for a signed-in caller those admit
everyone. An anonymous caller does **not** inherit that: undeclared means
nobody has classified the concept, which is not the same as deciding it is
publishable. So the tier is something you opt a concept INTO, never
something a concept falls into by omission.

`@rowAuthz(public)` is a READ tier. It declares who may see a row and says
nothing about who may create one; an anonymous write is refused at the
engine's write chokepoint regardless of what the concept declares.

**Two things this is not.** It is not a way to let visitors submit
anything -- that is `POST /inbound/{source}`
([inbound-delivery.md](inbound-delivery.md)) or the guest-token path, both
of which authenticate. And it is not per-visitor: every anonymous reader is
the same actor, which is deliberate -- it is what lets one cached result
serve every visitor, and it means no filter can branch on which stranger is
asking.

**Caching.** Anonymous reads carry no caller dimension, so they are the
best-cached data in the system: one computation, served to everyone. That
is a reason to prefer the public tier over a per-visitor guest token for
genuinely public content, not merely a side effect.

---

## Rollback

`updateSiteBundle` is deploy AND rollback -- the same write, aimed at a
different version. Bundles live under versioned prefixes rather than being
overwritten in place specifically so this works: `component/edge/publish.go`'s
`version()` derives the prefix from the bundle's own content hash, so a
version id can be verified against the bytes it names, and republishing
identical bytes is a no-op rather than a new version accumulating storage
forever.

```
mutation updateSiteBundle(siteId: "shop", bundleRef: "blob://sites/shop/v_previous.../")
```

**The practical rollback record is your own CI log.** Every publish
returns `{version, bundleRef}` -- keep that output wherever your deploy
history already lives, and rollback is pasting a value back in.

**If you did not log it**, there is no single query that lists every
version a site has had. `dsl/platform/queries.memql` carries
`siteByHostname` and `sitesAll`, both current-state reads, and no such
capability exists as either a query directive or a builtin. What DOES
exist is enough to reconstruct it by hand: every query result collapses to
one row per id **at the instant asked for**, so wrapping a read in
`asOf(<query>, <timestamp>)` and walking backward through each answer's
own `createdAt` reconstructs the row's history using nothing but the
ordinary query surface -- proven against a live engine in
`component/memql/asof_reconstructability_1872_db_test.go`. Be honest with
whoever is asking: finding a prior version this way is a walk, not a
lookup.

**The portal does exactly this walk for you.** `/deployables/:siteId`'s
"Version history" band (`clients/portal/src/deployables/calls.ts`,
`useDeployables.ts`) re-issues `siteById` under successive `asOf`
timestamps, each one set just before the previous result's `createdAt`,
bounded to the last `MAX_HISTORY_VERSIONS` (5) versions -- the mechanism
above, already built, with a "Roll back to this" button on each entry that
calls `updateSiteBundle` with that version's `bundleRef`. `siteById`
(`dsl/platform/queries.memql`, memql#3717) is a query added specifically
for this: it deliberately carries no `asOf latest` clause of its own,
because a query that declares one refuses to be wrapped in a caller's own
`asOf` a second time -- which is exactly the caller-chosen-instant
capability the walk needs.

---

## Two different 404s, and why one of them is a 503

`draft` and an unknown hostname both answer `404` from the edge's own
handler, once a request reaches it. `disabled` answers `503`
([above](#status-draft-live-disabled)).

**But a request does not always reach the edge's handler at all.**
Measured on a live cluster: traefik drops the ENTIRE router when the
backend Service is absent --

```
ERR Cannot create service error="service not found" ingress=edge-front-door serviceName=edge servicePort=8085
```

-- so a hostname with no site row, and a hostname whose `svc/edge` is
missing, scaled to zero, or crashlooping, come back as the **identical**
404. That second one is traefik's own 404, not the edge's -- the edge's
careful `draft` / `disabled` / unknown-host distinction never gets a
chance to run.

**`kubectl -n memql get svc edge` is the discriminator.** It is the first
thing to check when a hosted-site hostname 404s and the site row looks
right: if the Service is not there (or not Ready), the rule is fine and
the node is not, which is a different fix from anything on the site row.
Full detail: [front-door.md](front-door.md).

---

## The escape hatch: a site that genuinely needs SSR

The edge does not have to serve every site, and treating "hosted, static"
as the only option is what turns a real constraint into a resented one.

A site that genuinely needs server-side rendering -- true per-request
personalization, something with no static shape at all -- can be hosted
somewhere that runs Node (Vercel, Cloudflare, anywhere) and still use
MemQL as its data plane, dialing `api.<domain>` exactly as any other
cross-origin client does. See
[Connected -- a site that stays where it is](connected-integration.md) for
the full setup: a typed client generated from the cluster's DSL, an OAuth
client for the site's origin, and the CORS + CSP work that same-origin
hosting would otherwise have handled for you.

What you give up moving to Connected is same-origin: a CORS grant, a token
that has to live in memory instead of a cookie, and the cluster domain
compiled into the bundle. What you keep is everything else -- the same
generated client, the same queries, the same subscriptions, the same auth
flow. Nobody has to unpick a Connected integration to host it here later,
or the reverse.

---

## Caching: what the edge does, and putting a CDN in front of it

Three layers cache a hosted site's bytes, and they compose because each one
answers a different question.

### 1. The browser

| Response | `Cache-Control` | Validator |
|---|---|---|
| Hashed assets (`assets/app.abc123.js`) | `public, max-age=31536000, immutable` | strong `ETag` |
| `index.html` and any `.html` fallback | `no-cache, no-store, must-revalidate` | strong `ETag` |
| `runtime-config.json` | `no-store` | none |
| A 404 | `no-cache, no-store, must-revalidate` | none |

`no-cache` does not mean "do not store" -- it means "revalidate before
use". So a returning visitor asks about `index.html` on every load, and the
`ETag` is what turns that from a full re-transfer into a 304. That single
request is the most common one a live site serves.

### 2. The edge's own memory

Two caches, deliberately separate:

- **Host to site row** (`MEMQL_EDGE_SITE_CACHE_TTL_SECONDS`, default 30s).
  Time-bounded because a site row is mutable -- an operator can flip a
  status or roll a bundle back -- and the TTL is the backstop behind the
  change-feed invalidation that normally beats it. It caches MISSES too, so
  a scanner walking random hostnames against the wildcard cannot turn each
  request into a database query.
- **Bundle bytes** (`MEMQL_EDGE_BUNDLE_CACHE_MB`, default 64, `blob://`
  only). Size-bounded LRU keyed by (content-addressed version prefix,
  path), with concurrent cold requests for one asset collapsed into a
  single download. **It has no TTL and no invalidation, by construction**: a
  republish lands under a NEW prefix, so it is a new key and old entries
  simply age out. Set to `0` to disable it. The `file://` path is not
  cached -- that is the tree the image shipped, on local disk, and the cost
  being avoided here is a network round-trip to object storage.

### 3. A CDN in front of the edge

**Fronting the edge with a CDN is safe, and needs no purge integration.**
That is a property of the layout rather than a promise:

- Every cacheable asset is **immutable and content-addressed**. A build
  emits `app.abc123.js`; a rebuild emits a different name. A CDN holding
  the old one forever is correct, because the old one is still the correct
  answer for the old name.
- The three mutable things are **never cacheable**: `index.html` and any
  `.html` fallback are `no-cache`, `runtime-config.json` is `no-store`, and
  site resolution happens inside the edge on every request. So there is
  nothing a CDN can hold that a deploy needs it to forget.

**Key on the request URI as usual, and add nothing.** The edge sets no
`Vary`, and that is a decision rather than an omission: the two things that
change a response are the request's host (which selects the site) and its
scheme, both of which are already components of every cache key. A blanket
`Vary` added "to be safe" fragments entries that would otherwise be shared
and quietly destroys the hit ratio the immutable policy exists to earn.

**Do not put the identity service behind the same CDN.** Its JWKS endpoint
sends `Cache-Control: public, max-age=300`, so a cache in that path serves
the pre-rotation keyset for up to five minutes after a signing-key swap --
which presents as roughly half of all sign-ins failing, for five minutes,
with every manifest correct. See the rotation section in
[identity-service.md](auth/identity-service.md).

**What is NOT cached, so nobody is surprised:** a bundle-path MISS. The
resolution ladder tries up to three names per request (the exact file,
`<path>/index.html`, `<path>.html`), and an SPA deep link legitimately
misses all three before falling back to `index.html`. Negative caching was
considered and left out: the key space is attacker-chosen (every path under
every hostname the wildcard routes), and near-zero-byte entries would never
be evicted by a byte cap, so it needs its own entry cap and is a separate
decision. Prerendering the routes that matter removes the cost entirely --
see [The prerender budget](#the-prerender-budget).

---

## Limits

**One domain per cluster.** `*.<domain>` plus the apex, one wildcard
certificate, issued once at install (decision D2). Every hosted site's
hostname is a label under that one domain, or the apex itself -- nothing
in the `site` concept restricts what string you put in `hostname`, but
nothing routes to a hostname outside the cluster's own domain either: no
DNS record points at it and no certificate covers it, so a row naming one
is simply unreachable.

**Custom domains are an install-time change, not a per-site feature.**
Per-site custom domains with runtime ACME were designed in full during
this epic's brainstorm and deliberately shelved rather than built -- see
§11 of `docs/superpowers/specs/2026-08-13-cluster-front-door-design.md`
for the shape it would take (HTTP-01 only, a pending/verifying state
machine on the site row, and either cert-manager objects per domain or SNI
passthrough in the edge) and why it was not needed under this design's
core decision: a customer's cluster is already on the customer's own
domain (D1 -- one MemQL cluster per customer). If a cluster ever needs a
second domain, that is a cluster configuration change at install -- the
same lever `MEMQL_DOMAIN` already is -- not a row a site's owner sets.

**Exact-host-versus-wildcard precedence** -- whether `api.<domain>` really
does keep its own backend rather than falling to the wildcard `*.<domain>`
edge rule -- is covered in full in [front-door.md](front-door.md). This
page does not restate it; the short version is that it is a load-bearing
assumption of the six-host design, DECLARED on the wildcard locally (it
names itself lowest, memql#3810) and resolved by `server_name` specificity
on ingress-nginx in the cloud.

**The edge Deployment itself is landing separately** (memql#3714, in
progress alongside this page). Everything above the front-door routing --
the site concept, the resolution logic, the publish endpoint, the CSP and
runtime-config generation -- is built and merged; what is not yet true on
`main` as of this writing is a running `svc/edge` reconciled by the
committed local-overlay manifests. Check `kubectl -n memql get svc edge`
rather than trusting this paragraph's age.

---

## Reference

- Design: `docs/superpowers/specs/2026-08-13-cluster-front-door-design.md`
  (epic [memql#3700](https://github.com/znasllc-io/memql/issues/3700)) --
  decisions D5 (a site is data), D6 (the site concept's row-authz tier),
  D8 (the portal is site #1), D9 (the same-origin proxy), D10 (publish is
  an atomic version flip), D11 (prerendering is a build concern, and the
  resolution order), D2 / §11 (one domain per cluster, custom domains
  shelved).
- Concept + DSL surface: `dsl/platform/concepts.memql`,
  `dsl/platform/mutations.memql`, `dsl/platform/queries.memql`,
  `dsl/platform/seeds.memql`.
- Serving path: `component/edge/handler.go`, `resolve.go`, `csp.go`,
  `runtimeconfig.go`, `proxy.go`, `bundle.go`, `blob.go`.
- Publish path: `component/edge/publish.go`,
  `component/server/site_bundle_handler.go`, `app/transport_sites.go`.
- Node wiring: `app/transport_edge.go`, `app/build_edge.go`.
- Soft-delete guard: `component/memql/platform_site_delete_guard.go`.
- Portal Deployables screen (memql#4346, replacing the Sites screen of
  memql#3717): `clients/portal/src/deployables/` --
  `DeployablesPage.tsx` (list + create), `DeployableDetailPage.tsx` (deploy
  from the Library, point at a bundle ref, roll back, status, delete),
  `useDeployables.ts` (all four hooks), `calls.ts` (the `asOf` version walk),
  `hostname.ts` (the client-side mirror of the hostname policy),
  `publishRefusal.ts` (the refusal-reason table). There is no non-owner
  refusal screen: the composite tier gives an ordinary caller sites of their
  own, so there is nothing to refuse.
