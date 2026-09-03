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
changed is who may own one, where it may live, where its bytes may come from,
and -- with epic memql#4885 -- the fact that composing one, watching it deploy
and managing it afterwards are now **one flow** rather than three sections.

This page is the deployable: the flow, the address, the credential a private
source needs, and the target model. Its other half is
[Packages and deployables](packages.md), which is the SOURCE: the manifest, the
pipeline, the DSL delivery and the refusal catalogue.

Design records:
[the Deployables program](../../superpowers/specs/2026-09-02-deployables-program-design.md)
and [Compose](../../superpowers/specs/2026-09-02-deployables-compose-design.md).

---

## The one flow

Every deployable is composed, deployed and managed on one vertical device with
five stops, in this order:

| Stop | What it answers |
|---|---|
| **Source** | where the thing comes from -- a repository, a zip in Files, or your own CI |
| **What it is** | what the analysis found: each app with its kind, path and build plan, each DSL domain, any Go pack, and every problem |
| **Where it lives** | the address: a hostname under this cluster's domain, the client it is for, and -- for a cluster owner -- the client's own domain |
| **Build** | prebuilt output found and skipped, or the typed refusal saying what would have run |
| **Live** | draft, live, paused or archived, with the version history behind it |

The same five stops report progress while a deploy runs and standing status
afterwards, so there is no separate "progress screen" to learn. Beneath them
sits **Every attempt**: the append-only deployment rows, each with its own
six-stage rail, and roll back on any successful one that is not the latest.

**Chosen once.** A source is picked once and shown as facts afterwards; an
address is chosen once, at a deployable's first deploy, and remembered on its
site row. A later deploy of the same source keeps the same addresses and never
re-asks.

---

## The three sources

### A repository

An HTTPS `github.com` URL and, optionally, a branch, tag or commit. An empty
ref follows the repository's default branch, resolved at fetch time -- storing
the resolved default would freeze the package to whatever `main` meant on the
day it was added.

The URL is **probed** before anything is created (`sourceProbe`, below).
A reachable public repository reads "public, default branch main". One the
cluster cannot see reads "private, or not there", and a credential field
appears offering your active credentials for that host, or a new one.

Only `github.com` is fetched today. Any other host is refused with
`source_host_unsupported` -- "only github.com today, or upload a zip" -- and
the answer is the zip source below. Connecting a GitHub account instead of
pasting a token is the
[GitHub Connect](../../superpowers/specs/2026-09-03-github-connect-design.md)
epic, which lands separately; **what ships here is the pasted token**.

### A zip in Files

A zip artifact already in your [Library](library.md). Choosing one calls
`artifactProbe`, which decides which of two paths the zip takes:

- `memql-package.yaml` at the root -- it is a **package tree**, and the
  package path takes it from here ([packages.md](packages.md)).
- `index.html` at the root and no manifest -- it is a **built site**, and the
  hand-made path takes it and asks for the kind.
- Neither -- the zip is something else, commonly a folder wrapping the site one
  level down.

A manifest beside an `index.html` is a package: the manifest is the stronger
claim. Choosing a zip deploys nothing.

### Pushed by your CI

Cluster owners only. The deployable is created as a draft holding the
placeholder bundle `blob://sites/<siteId>/pending/` (the convention
[site-hosting.md](site-hosting.md) records), and its Source stop then shows the
site id, the publish route, and the command that mints the credential for it:

```
POST https://api.<domain>/sites/<siteId>/bundles
```

```bash
TOKEN="$(kubectl -n memql exec deploy/identity -- \
  memql service-account-token mint --label ci-publish --subject system:ci-publish)"
```

That route takes a `class="service_account"` JWT and nothing else -- a
signed-in person's session gets a `403`, not a `401`. The full worked example,
including why every file rides its own multipart part named for its path within
the bundle, is in [site-hosting.md](site-hosting.md); the credential itself is
[service-account-jwt.md](auth/service-account-jwt.md).

The Live stop then waits for the first push, which arrives as the `bundleRef`
flip the site row already broadcasts. Nothing polls.

---

## Two clicks on a first deploy, one after

**Analyze** creates the source record and parks a run at the confirm gate.
**Deploy** confirms it with the placements. A redeploy is Deploy alone.

On the package path:

| Click | Call |
|---|---|
| Analyze | `createPackage`, then `packageDeploy(confirm: false)` |
| Deploy | `packageDeploy(confirm: true, placements)` |

`packageDeploy` without `confirm` opens the deployment row, fetches, analyzes
and parks at `awaiting_confirm` with the report on the row. Nothing is built,
staged, rolled or published. That gate is always present -- a redeploy passes
it in one click, and it is on the ROW rather than in a browser, so somebody who
closed the window finds their run exactly where they left it.

On the hand-made path, Analyze is `createSite` (a draft holding the placeholder
bundle) plus, for a zip, `artifactProbe`; Deploy is `sitePublishFromArtifact`
for a zip and nothing at all for a CI-pushed source.

**A parked run is findable without opening anything.** A deployable whose
source has a run at `awaiting_confirm` is marked in the list -- "a deploy is
waiting for you" -- from a feed over parked runs alone
(`packageDeploymentsAwaitingConfirm`, owner or cluster owner, newest first).

---

## Placements: the address, the client, the domain

`packageDeploy` takes `placements`, an object keyed by deployable name:

```
placements: {
  "storefront": {
    "hostname":  "shop",
    "accountId": "v1:accounts:account:...",
    "ownDomain": "shop.acme.com"
  }
}
```

Read on a **first** deploy only. A never-deployed app with no `hostname` is
refused with `deployable_binding_missing`; later deploys find the site through
`(packageId, packageDeployableName)` and never re-ask.

The two optional halves are applied by the pipeline **after the site exists**,
as the same `updateSiteAccount` and `customDomainAdd` calls the page makes, run
**under the caller's actor**. The existing guards therefore decide exactly as
they do from the page, and the pipeline gains no bypass of either; it only
saves the person a second click.

**A refused half does not fail the publish.** The site is live at its cluster
address either way, and a hostname collision or a per-site cap is a fact about
the domain rather than about the deploy -- so it lands on that deployable's
outcome as `deployable_account_refused` or `deployable_domain_refused`, with
the guard's own sentence, for the Where-it-lives stop to render. What DID land
is recorded beside it as `accountId` and `ownDomain`.

**The deploy never waits on DNS.** The app goes live at its cluster address and
a bound domain stays "waiting on your DNS records" until both records check
out. The verification, the two records and the certificate are the custom-domain
flow ([front-door.md](front-door.md)).

---

## The target model

A **target** is what the flow needs to know about a kind of deployable: the
address stop's shape, the build surface, the live states, and the row it lands
on. Every stop renders from it, so the flow has no branch on "which kind is
this".

| Target | Address | Build surface | Live states | Row |
|---|---|---|---|---|
| **web** (`spa`, `static`, `shopify_storefront`) | a hostname under the cluster domain; optionally the client's own domain | prebuilt output in the source | draft, live, disabled, archived | `v1:platform:site` |
| ios | a bundle id and an App Store Connect app | a Mac in your Fleet | built, uploaded, in review, released | a new concept, never `site` |
| android | an application id and a Play listing | a workbench with a JDK and SDK | built, uploaded, in review, released | the same new concept |
| macos | a bundle id; a notarized disk image or the Mac App Store | a Mac in your Fleet | built, notarized, released | the same new concept |

**Only web is registered.** The three rows beneath it are a written-down shape,
not code: nothing renders a control for them, and `v1:platform:site.kind`
declares exactly the three web kinds
(`TestSiteKindEnumIsExactlyThreeValues`). A site is a hostname the edge
resolves, and a store listing is not one, so an `ios` site row would be a value
that never resolves.

**The engine tells the truth about a kind it knows and does not offer.** A
manifest declaring `ios`, `android` or `macos` is reported with
`deployable_target_not_offered` -- scoped to that app, **not fatal** to the
package, exactly as a Go pack is:

> iOS is not offered on this cluster yet. Its address will be a bundle id and
> an App Store Connect app, and nothing here serves one today; the rest of this
> package deploys around it.

The app is skipped with the build plan "skipped -- not offered on this cluster
yet" and recorded on the deployment row with that non-fatal refusal and no
site id -- recorded rather than omitted, because the row promises one entry per
manifest deployable and a missing entry reads as "nothing happened". A kind
nobody has heard of stays `deployable_kind_unknown`, which is fatal: the two
say opposite things to an author, "not yet" versus "not a thing".

**One list, pinned twice.** The OS's offered-kind list and the site enum are
held equal by a parity test, in the tradition of
`TestFleetOnlineWindowMatchesPortal`, so neither can grow a value the other
does not have.

---

## Private sources: the credential

A private repository is fetched under a credential **you own**.

`v1:platform:sourceCredential` declares `@rowAuthz(owner="ownerUserId",
clusterOwner)`:

| Field | Notes |
|---|---|
| `ownerUserId` | stamped from the actor by the writer, never accepted as an argument; the row-authz key |
| `host` | `github.com` today, and the only value admitted. The probe and the fetcher match on it, so a credential can never be presented to a host it was not minted for |
| `label` | your own name for it. Display only |
| `encryptedValue` | the token, sealed by `secret.Encrypt` under `MEMQL_MASTER_KEY` on the node that received it. No client-readable shape projects it |
| `fingerprint` | the token's last four characters, prefixed with `...`, for telling two apart |
| `status` | `active` or `revoked` |
| `lastUsedAt` | when a fetch or the poll last unsealed it. A HEARTBEAT: displayed, never treated as news |
| `revokedAt` | when it was revoked. The row is never deleted |

**The token crosses the wire once.** `sourceCredentialCreate({host, label,
token})` seals it server-side and answers `{credentialId, fingerprint}`. It is
a function-local for the length of that one call and appears in no row, no log
line and no reply. It is a capability rather than a mutation for one reason: a
secret cannot be sealed in a browser, because `MEMQL_MASTER_KEY` exists on
nodes and must never exist on a laptop.

**A package NAMES a credential**, on `credentialId`. There is no token on the
package row, on a snapshot, or in a log.

### How it resolves, and why that is the whole security shape

The fetcher and the polling feed read the credential under the **package
owner's** actor, through a query whose only predicate is the owner term. So:

- A package naming **somebody else's** credential resolves zero rows and is
  refused `credential_not_found`, before any request leaves the cluster.
  "Does not exist" and "belongs to somebody else" are the same zero rows, and
  the sentence does not claim to know which.
- A credential whose `status` is `revoked` is refused `credential_revoked`.
- A **cluster owner** deploying somebody's package fetches under that
  package's own credential -- which is correct: they are deploying that
  package. There is no cluster-wide source credential any more.
- The polling feed **skips** a package whose credential will not resolve
  rather than polling it anonymously, so a private repository never reads as
  unchanged because the request answered 404.

Decryption happens only inside a fetch, or the probe that stands in for one.
No query and no capability returns plaintext; the one query that returns
ciphertext is `@serverOnly`.

> **This replaces the old named cluster secret outright, with no shim.** A
> package used to name a cluster-wide `v1:platform:globalSecret`, which had two
> defects that were one defect: nothing in the OS could create one, so a
> private repository could only be connected from the cockpit CLI; and any
> package owner could name any secret, so the fetcher could be made to fetch
> under an operator's token by whoever knew its name. A package row still
> carrying a secret NAME from before reads as "no credential" and is asked for
> one.

### Revoking and rotating

`sourceCredentialRevoke({credentialId})` flips `status` to `revoked` and
stamps `revokedAt` through an owned mutation, so the write guard admits the
row's owner (or a cluster owner) and nobody else. The row is never deleted --
it is the history of what fetched under it -- and every source fetching under
it refuses at its next fetch with `credential_revoked` until it is switched.

**Rotation is adding, then switching.** There is no in-place replace: add a
new credential and point the source at it on its Source stop
(`updatePackageSource`). A revoked credential is never reactivated.

The **Sources group**, in the Deployables app's Settings section, is where the
whole set lives: each credential's host, label, fingerprint and last use, plus
the sources fetching under it (a join on `credentialId`), with add and revoke.
Revoke says what it will cost: *sources fetching under it will refuse at their
next fetch until you switch them.*

---

## The two probes

Both are read-only questions the Source stop asks before a person commits to a
source. **Neither writes anything and neither stamps anything** -- not even the
`lastUsedAt` heartbeat a fetch records, because a probe is a question and a
question is not a use.

### `sourceProbe({repoUrl, credentialId})`

Answers `{host, reachable, private, defaultBranch, reason}`. `private` and
`defaultBranch` are meaningful only when `reachable` is true; a repository the
probe could not read has no known visibility, and `false` there is "not known",
never "public". `reason` is exactly one of:

| Reason | What it means |
|---|---|
| `ok` | reachable; `private` and `defaultBranch` are GitHub's answer |
| `not_found_or_private` | 404 with no credential. GitHub answers the two alike, and the stop offers a credential |
| `credential_cannot_see_it` | refused under the credential (401, 403 and 404 alike -- one repair for the three). Choose another, or widen its grant |
| `credential_not_found` | the caller cannot read the named credential |
| `credential_revoked` | it resolves, and it was revoked |
| `source_host_unsupported` | not `github.com`. Paste a github.com URL, or upload a zip |
| `rate_limited` | GitHub is rate-limiting; ask again later |

A GitHub this cluster cannot reach at all is an **error**, not a reason -- the
stop says so and stays editable. It never blocks Analyze on a public
repository: **the fetch is the authority and the probe is a courtesy.** The
credential resolves under the CALLER's own actor here (the person composing is
choosing among their own credentials), and a typed reason is answered before
any request leaves the cluster.

### `artifactProbe({artifactId})`

Answers `{isPackage, isBuiltSite, fileCount, totalBytes}` by opening the
caller's own zip through the same fetch a deploy uses -- expanded under the
packages limits, so a zip the deploy would refuse (too large, an entry escaping
the root, not a zip) is refused here, by the same code, before anybody commits
to it. The artifact stays exactly the Library row it was.

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

Writes need rank 200 and above (`{admin, developer, owner}` under the one
ladder). A client's own domain and a CI-pushed source are **cluster-owner
acts** and are offered to nobody else. A source that ships MemQL DSL says on
its What-it-is stop that deploying it is a cluster owner's decision, stated
before the click rather than refused after it.

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
**cluster-owner-only**, as before -- but no longer hand-certified. A client's
own domain is bound through the custom-domain flow, reachable both as the
`ownDomain` half of a placement and on the deployable's Where-it-lives stop
afterwards: it shows the two DNS records to create, the cluster verifies them
and says which one is still wrong, and the exact-host Ingress and Certificate
are provisioned once both check out (epic memql#4805,
[front-door.md](front-door.md)).

---

## Kinds

Three kinds are live, and the enum holds exactly these three:

| Kind | What it is |
|---|---|
| `spa` | A single-page app bundle. |
| `static` | A plain website. |
| `shopify_storefront` | A SPA bundle with a typed binding to a Shopify store. |

Android, iOS and macOS have **no schema at all**, and now have a written-down
target shape instead (above) rather than a client-side "coming soon" list. That
is deliberate: those are artifact *distribution* (stores, TestFlight,
notarisation), not hostname-resolved web surfaces, and an enum value that never
resolves would be the wrong kind of additive.

---

## Deploying a zip from the Library

`sitePublishFromArtifact(siteId, artifactId)` publishes a zip artifact from the
person's Library as the site's new bundle -- the hand-made path's Deploy, and
the Redeploy behind it afterwards. It runs entirely under the caller's actor:
the site, the artifact and the backing file are each read through owner-scoped
queries, so a cross-user call returns zero rows and is refused by name rather
than by a special case.

The bundle is validated before anything is written: it must be a zip, it must
carry `index.html` at the ROOT for `spa` and `shopify_storefront`, and it is
held to the same per-file (25 MB), total (500 MB) and file-count (20000) limits
the CI bundle route enforces. Path traversal is refused, not sanitised.

Atomicity is the publisher's: a new content-addressed version is written first,
and only then does the row's `bundleRef` flip. `artifactId` is stamped on the
row so a site can say which bundle it came from.

Every refusal carries a **stable machine-readable reason** rather than prose,
so the surface can explain it and an audit row can be searched on:
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
> for service accounts only. No user-facing bundle upload route was added; a
> browser deploys from bytes the system already holds.

**A new deployable starts with a placeholder bundle.** `createSite` requires
`bundleRef` and the schema has no empty state, so a draft is created holding
`blob://sites/<siteId>/pending/` with `status: "draft"` -- the placeholder
convention [site-hosting.md](site-hosting.md) already describes. That is safe
because a draft site is refused *before* any file lookup happens, so the
placeholder is never dereferenced, and it is what lets a CI-pushed source be a
source rather than a special case. The first real publish replaces it, and a
draft still holding the placeholder is not a publish anybody made.

**Rollback** is unchanged: the Live stop walks the site's history with an
`asOf` read and sets `bundleRef` back. Old versions stay under their own
content-addressed prefixes, so restoring one serves its original content. For a
package deployable, a whole-package rollback lives under Every attempt and
restores a prior run's recorded tuple.

---

## The Shopify storefront

A `shopify_storefront` site is a SPA that MemQL hosts at `<slug>.<domain>`,
with a typed binding to a store. Checkout is Shopify's own hosted checkout.

The binding is `{storeDomain, storefrontTokenRef}`, where `storefrontTokenRef`
**names** a `v1:platform:globalSecret` row. The token itself is never stored on
the site row. This is the one named-cluster-secret reference a deployable still
carries, and it is a cluster-level integration credential rather than a
personal one -- which is exactly the distinction a source credential exists to
draw.

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

**The binding is read-only after creation.** There is no `updateSiteBinding`
mutation -- `binding` is written by `createSite`, and changed by re-running it
on the same id or by the package's manifest at the next deploy -- so the
surface renders the binding rather than offering a field that would silently do
nothing.

Orders, inventory and refunds are **not** this feature. They reach MemQL
through webhooks and reconciliation, so a sale from any channel lands, not only
one made through this storefront
([the Shopify connector](shopify-connector.md)).

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

## Runtime settings

A deployable carries **`settings`**: key-values its bundle reads when it
loads, served in the runtime-config document the edge already answers with
(`GET /runtime-config.json`). A bundle reads one as `config.settings.apiBase`.

The point is that **one bundle can serve two deployables against different
endpoints with no rebuild**. Publish the same bytes to `eu.<domain>` and
`us.<domain>`, give each its own `apiBase`, and each reads its own.

Set them on a deployable's Live stop in MemQL OS, or write them directly:

```
mutation updateSiteSettings(siteId: "v1:platform:site:abc", settings: {apiBase: "https://api.eu.example.com", region: "eu"})
```

**The write REPLACES rather than merges.** The whole object is the argument,
which is what makes removing a setting expressible -- a merge would make
every "delete this one" silently re-save the value already there. The OS
editor sends the map it shows for the same reason.

### Not a place for a secret

Every value here is served to **every visitor**, unauthenticated, in a
document their browser fetches. A value in `settings` is public by
construction.

A key ending in **`Ref` is refused**, and that refusal is the reason worth
knowing. `...Ref` is already this platform's convention for a value that must
NOT be public: the storefront binding's `storefrontTokenRef` NAMES a
`v1:platform:globalSecret` row that the edge resolves at serve time for
exactly one site kind. A settings key spelled that way would look like that
convention and be honoured by nothing -- the edge serves the string as typed
-- so the natural mistake would publish a secret's name, and the natural next
mistake would be teaching the edge to resolve it. Put a credential in the
cluster's secrets and name it from the deployable's `binding`.

### The limits

| Rule | Value |
|---|---|
| Key form | `[A-Za-z][A-Za-z0-9_]{0,63}` -- what a bundle can read as `config.settings.<key>` |
| Keys per deployable | `MEMQL_SITE_SETTINGS_MAX_KEYS`, default 64 |
| Characters per value | `MEMQL_SITE_SETTINGS_MAX_VALUE_LENGTH`, default 2048 |
| Value type | a plain string; anything else is refused |
| System-owned rows | refused, for a cluster owner too |

The caps are on the row because the document is served on every page load and
grows with it. A system-owned deployable (the portal, MemQL OS) refuses the
write whoever asks: those rows are re-seeded at every boot, so a value set on
one would be reverted and would look like it had worked until then.

Enforced beside the engine's write path
(`component/memql/platform_site_settings_guard.go`), not in the mutation
body: a mutation sees a value and never an object's KEYS.

---

## Traffic and health

Is anybody using this deployable, and is it healthy. Both are read from what
the edge actually served, never from a guess.

**What is measured.** The edge writes one row per served request into its own
`edge_request` log -- the deployable it resolved to, the status it answered,
the bytes, how long it took, and what it did with the request (an asset, an
HTML document, the SPA fallback, a proxied call, the runtime-config document,
or nothing at all for a deployable that is paused or in draft). TimescaleDB
folds those rows into per-minute and per-hour aggregates. The figure and the
raw log cannot disagree, because the figure IS the log, folded.

**What is not measured.** Nothing that identifies a visitor: no address, no
user agent, no path, no referrer. The question this exists to answer needs
counts and outcomes and nothing about who.

**The buckets.** A window of an hour reads minute buckets; a day or a week
reads hour buckets, because a week of minute buckets is ten thousand rows to
draw one line. Both are kept for `MEMQL_EDGE_REQUEST_LOG_RETENTION_DAYS`
(default 30, clamped 1..365) -- the raw rows and both aggregates on the same
schedule, so "unmeasured" means the same thing at every horizon.

**Unmeasured is not zero.** A window with no rows answers *unmeasured* and
says so in words; a window with requests and no errors answers zero errors.
Read them as different facts: one means nobody visited, the other means
nothing was recording, and they send you to different places. Three things
make a window unmeasured:

- nobody visited;
- `MEMQL_EDGE_REQUEST_LOG_ENABLED` is `false` on the replica that served
  (the aggregate is then short by that replica's share);
- the deployable is **system-owned**. The portal and MemQL OS are excluded by
  construction, so they are always unmeasured -- measuring the console
  somebody reads a figure in would be measuring the act of looking.

**Errors and not-found are counted apart.** 5xx is the deployable failing;
4xx is somebody asking for a page it does not have. Folding them together
makes a healthy site with a broken inbound link look unhealthy.

**Who can read it.** Whoever can read the deployable: the read resolves each
id through `siteById` / `sitesAll` under the caller's own actor, so a
deployable you cannot read contributes no rows -- the same answer one with no
traffic gives, which is what keeps the call from telling you whether somebody
else's id exists.

**The cost to a visitor.** None that is measurable. The write is a
non-blocking hand-off to a batching writer that drops and counts under
pressure rather than making anybody wait on Postgres; serving a bundle asset
measured 3139-3202 ns/op without the log and 3000-3188 ns/op with it, at one
extra 32-byte allocation. Drops are on `memql_site_traffic_dropped_total`
(labelled `queue_full` / `write_failed`) and writes on
`memql_site_traffic_written_total` -- read them together when a figure looks
low, because the figure itself cannot say what is missing from it.

Read it on a deployable's Live stop in MemQL OS, or directly:

```
builtin siteTrafficInWindow(siteIds: ["v1:platform:site:abc"], bucket: "1h", windowStart: "2026-09-02T00:00:00Z", windowEnd: "2026-09-03T00:00:00Z")
```

`summary: true` folds the window into one row per deployable instead of one
per bucket -- what a list needs to say "last served" for twenty of them
without pulling twenty series. At most 200 deployables per call, refused past
it rather than truncated: a silently short answer would read as "those have
no traffic".

### What it is not

No alerting, no uptime probing from outside the cluster, no per-path
analytics and no geographic breakdown. Each is out of scope by decision
rather than by omission.

---

## Where the surface is

**MemQL OS, the Deployables app**, in three sections: **Map** (what serves
where, and the window's default), **Deployables** (the list, and the page that
composes, deploys and manages one), and **Settings** (the app's own
preferences, plus the Sources group above). Composing is not a fourth section
and not a modal: New deployable opens the same page in compose mode, in place,
with a quiet Back to the list.

A package with two apps is **two rows sharing a source**, grouped under it --
one row per thing that serves or will.

The portal keeps a maintenance-only `/deployables` page, and `/sites` and
everything under it redirect to it, tail included, so an old bookmark or a
link in a runbook still lands. No new portal work is done here.

---

## Related

- [Packages and deployables](packages.md) -- the source half: the manifest, the
  pipeline, DSL delivery, and the refusal catalogue.
- [The Library](library.md) -- where the zip artifact you deploy comes from.
- [Site hosting](site-hosting.md) -- the request path, the CI publish route and
  the per-site CSP.
- [Front door](front-door.md) -- hosts, certificates, custom domains and the
  two TLS regimes.
