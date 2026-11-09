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
| **Live** | not deployed, built, live, offline or archived, with the version history behind it |

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
[service-account-jwt.md](auth/service-account-jwt.md). Adding
`?target=candidate` publishes the build as the site's candidate version instead
of the serving one ([below](#publishing-the-candidate-version-instead)).

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
| Deploy | `packageDeploy(confirm: true, placements, deploymentId)` |

`packageDeploy` without `confirm` opens the deployment row, fetches, analyzes
and parks at `awaiting_confirm` with the report on the row. Nothing is built,
staged, rolled or published. That gate is always present -- a redeploy passes
it in one click, and it is on the ROW rather than in a browser, so somebody who
closed the window finds their run exactly where they left it.

**The second click ADVANCES the first click's run**, which is what
`deploymentId` names (memql#4954). Without it every call minted a run, the
confirmation included: the source kept a row at `awaiting_confirm` nobody would
ever answer, the list went on saying a deploy was waiting for a gate that had
been answered, and a Retry's report described bytes the click did not ship. A
resumed run also **re-reads the snapshot it already stored** rather than
fetching the source again, so the report a person read at the gate describes
what the click deploys.

Only a PARKED run of the same source resumes. A deployment id naming a run in
flight falls through to opening a new one, which is what leaves the append-only
rule refusing it -- the dedup two auto-deploy feeds noticing one push rely on.

On the hand-made path, Analyze is `createSite` (a draft holding the placeholder
bundle) plus, for a zip, `artifactProbe`; Deploy is `sitePublishFromArtifact`
for a zip and nothing at all for a CI-pushed source.

**A parked run is findable without opening anything.** A deployable whose
source has a run at `awaiting_confirm` is marked in the list -- "a deploy is
waiting for you" -- from a feed over parked runs alone
(`packageDeploymentsAwaitingConfirm`, owner or cluster owner, newest first).

### A run says which deployables it is for

`scopedTo` on the run row names them; **empty means the whole source**, which
is what a run that skips nothing is and what every run written before the field
existed was.

Runs are routinely scoped to one app -- a redeploy from a deployable's page
sends every sibling `skip: true`, and the compose gate does the same when
entered for one declared app -- and the row used to keep no trace of it
(memql#4953). So every reader assumed the source's newest run was about
whatever it was looking at: a serving `storefront` read itself as "Building"
while `web` deployed, drew its own Where-it-lives, Build and Live stops as
unreached, rendered the sibling's report and build failure as its own, and
offered a Cancel that stopped the other app's deploy.

Two things follow from a run knowing its own scope, and the second is not
merely cosmetic:

- **A deployable's page shows a run only when that run names its app**, and the
  list's compact rail stops repainting live siblings as unreached.
- **While any run of a source is in flight, that source's other apps offer no
  act that would start a second one**, and say why. The wrong reading was
  accidentally providing this -- every app of a busy source read "Building" and
  offered only Cancel -- and there is no per-source concurrency gate in the
  engine, while the roll rewrites one pointer and restarts the cluster onto it.

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
refused with `deployable_hostname_unchosen`; later deploys find the site through
`(packageId, packageDeployableName)` and never re-ask.

**The address starts as a generated name and is checked while it is typed.**
The Where-it-lives stop seeds each app with a two-word name rather than the
app's own -- every source declares `storefront` and `web`, so the first person
on a cluster took those and the second found them taken at the end of the flow
-- and asks `siteHostnameCheck` (and `customDomainCheck` for a client's own
domain) a beat after each pause. Both answer `{hostname, available, reason,
problem}` from the write guards' own shape and uniqueness rules and reserve
nothing: the line under the field reads "checking", then "free" or the
policy's own sentence, Generate draws again until a free name comes back, and
Deploy stays out of reach until every address going out has checked out. A
name free a moment ago can still be refused on the write, by the same guard.

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

### Publishing the candidate version instead

A placement may also carry **`target`** (memql#5601): `"serving"`, the default
and what every run did before the field existed, or `"candidate"`:

```
placements: {
  "web": { "target": "candidate" }
}
```

A candidate run builds and uploads exactly as any other, then writes the
version as the site's **`candidateRef`** through `setSiteCandidate` and leaves
`bundleRef` -- and so every visitor -- where it was. A preview grant is served
the candidate, on a draft deployable and on a live one, while the public goes on
getting the serving version; promoting it is `promoteSiteCandidate`, as for a
candidate set any other way. It is a placement rather than a manifest field for
`skip`'s reason: whether this run's build goes in front of the public is a
decision about the run, not about the software.

- **The part is `preview`, not `publish`.** The run needs `deploy` as ever, and
  writing the candidate needs `execute app:deployables/preview` on the
  deployable's organization -- the part whose definition is "publish a candidate
  version".
- **The outcome says which.** A candidate publish records `candidateRef` on the
  run's outcome, never `bundleRef`, so rolling back to that run re-points
  nothing for that app: the run left the serving version where it found it. A
  run that published only candidates does not record the source's
  `deployedVersion` either, so the source still reads as having an update to
  ship.
- **A candidate is a `spa` and `static` feature.** A `shopify_storefront`
  serves ONE build on both of its destinations, Testing and Production, each
  against its own store ([storefront-preview.md](storefront-preview.md)), so
  a storefront candidate would be served by nothing. A run asking for one is
  refused after the analysis and before anything is built, uploaded or bound,
  and the engine refuses, from any route, a storefront candidate that is not
  the version it already serves (`storefront_has_no_candidate`).
- **A candidate run carries no DSL change.** Staging a changed MemQL domain
  rolls every node onto it for every visitor, while a candidate leaves the
  public on the serving build -- so a run with any candidate whose DSL
  differs from what the cluster runs is refused before the build. Publish it
  to the serving version, or deploy the DSL first in a run with every app
  skipped and then publish the candidate. The run's report says in advance
  whether that applies: `report.dslChanges` is `true`, `false`, or absent
  when unknown ([packages.md](packages.md#the-order-and-why-it-never-changes)),
  and the confirm checks again.
- **A candidate run leaves the package's access gates as they were.** The list of
  DSL domains a package ships (`dslDomains` on the package) is what a
  deployable's access gates are checked against, and it is recorded by a run
  that serves visitors: one that publishes a serving version, or one with every
  app skipped that carries DSL. A run that published only candidates records
  nothing, so a candidate of a tree that dropped a domain, or that ships no DSL
  at all, cannot close the gates of the build the public is being served
  ([Access codes](site-access-codes.md#which-package-opens-a-gate)).
- **The target survives the confirm gate.** A run records its candidates when
  it opens (`candidates` on the run row). At the gate, a target the
  confirming call names wins -- choosing there, with the plan on screen, is
  the ordinary flow -- and a target it leaves out keeps the recorded one, so
  a run opened as a candidate never falls back to the serving version. A
  retry of a lost run keeps that run's candidates the same way.
- **The candidate is not the auto-deploy baseline.** An automatic run
  publishes to the serving version and confirms itself only against the last
  run that published to the public; a run that published only candidates is
  skipped, so a plan approved only as a candidate parks the next push.
- **Any other value is refused** before a run opens, and so is a `target`
  that is not a string (`null` reads as unset).

---

## What a deployable is called, and what you can do to it

One vocabulary, on every surface (the record is
`docs/superpowers/specs/2026-09-05-deployables-states-activation-and-source-archive-design.md`).
The enum values on `v1:platform:site.status` are unchanged; these are the
words a person reads:

| Word | Engine | Meaning |
|---|---|---|
| **Inactive** | declared, on the source's `disabledDeployables`, no site | skipped when the source was deployed; nothing built, no address |
| **Not deployed** | declared and never deployed, or `draft` with the placeholder bundle | nothing has landed here yet |
| **Built** | `draft` with real files | its files are in place at the address; not live until you say |
| **Live** | `live` | serving at the address |
| **Offline** | `disabled` | taken offline on purpose; visitors get "temporarily unavailable" rather than "no such site" |
| **Archived** | `archived` | filed away; answers nothing; the name is still held |
| **Deleting** | `deleted` with domains still coming down | releasing the address |

The verbs: **Analyze**; **Deploy** (and *Deploy the update*, *Redeploy*,
*Retry the deploy*), which builds the source and puts its files in place and
never changes whether the app is live; **Go live** / **Take offline**;
**Activate** / **Deactivate** for a source's app; **Archive** / **Restore**
for a source or a standalone; **Delete** (and *Discard* for a draft) for a
standalone; **Cancel** for a run; **Skip** at the gate.

**A source's app and a standalone are two ladders.** A standalone's bundle is
its only copy, so it climbs `Offline -> Archived -> Deleted`: Archive keeps the
name and is reversible, Delete releases it. A source's app is reproducible from
the source, so it has one destructive act from every state, live included:
**Deactivate** (`packageDeactivateDeployable`) releases its custom domains,
deletes its site so the address comes free, disarms the source's auto-deploy
when this was its last app, and puts the name on the source's off-list. The
confirmation is the app's manifest name. **Activate** is the inverse's first
half: the app comes off the off-list and the compose flow opens for it, scoped
to it, asking only where it should live.

**Skip is deactivate.** An app skipped at the gate lands on the off-list and
reads *inactive*. Clicking it -- in the list or on its source's page -- opens
the compose flow for that app with a notice saying it is inactive and Activate
on the bar; nothing is written until Activate is pressed.

**Archiving a source deactivates every app it produced, then archives the
source.** Live apps included: the confirmation names the live addresses and
says they go offline the moment it is confirmed. Restoring the source brings
its apps back inactive. See [packages.md](packages.md).

**A confirmation is the thing's name.** A standalone types its address label
(`storefront`, not the whole hostname; the server accepts either), a source's
app types its manifest name, a source types its own.

**A first deploy ends on Built**, with the files in place and the site not
live, deliberately -- a stranger's code should not go live the moment it
builds. The compose flow offers Go live beside Done, so the two-step is one
screen.

**Go live is offered only when the engine would accept it.** Both the
deployable's bar and the end of the compose flow ask `sitePreviewReadiness`,
which answers from the same rule the write guard refuses with. A storefront
with no connected store may go live for design review. Catalog, cart and checkout
become available after connection through the Store icon beside Traffic.
An amber marker identifies incomplete Shopify setup independently of Live status.
A bound development store or unreadable store still refuses publication.
GitHub accounts and organization access are managed in Settings; the repository
wizard selects that saved access, with an accent text action back to Settings.


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
`TestFleetOnlineWindowMatchesTheClients`, so neither can grow a value the other
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
| `lastUsedAt` | when a fetch, the poll or a pipeline last unsealed it, to within an hour: a use is stamped only when the stored value is more than an hour old, so a source polled every ten minutes moves it once an hour. A HEARTBEAT: displayed, never treated as news |
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

An **empty `ownerUserId` means cluster-owned.** The seeded OS site row keeps it
empty and lands cluster-owned on every boot, and so does the VS Code landing
page's row (`vscode.<domain>`, memql#5518) -- the second platform site, seeded
from the same file and behaving identically: managed by the cluster, no acts.

Consequences worth stating plainly, because they are surprising:

- A cluster owner cannot personally own a site. Theirs are cluster-owned.
- Handing a site over, or taking one back, is re-running `createSite` on the
  same id. There is no owner-assignment mutation: one writing the owner from an
  argument could not exist without failing the owner-stamp gate or lying about
  why it was exempt.

**The erasure, and its repair (memql#5292).** Between the arrival of
self-serve deployables and PR #5284, every developer deploy produced a site
whose owner was then blanked: `createSite` stamped the developer, and the
pipeline's next write (`recordSitePackageOrigin`, internal origin on the same
context) tripped the privileged undo above on a value it had not stamped, and
deleted it. The row read as cluster-owned, so the developer who deployed it
could neither see it in Deployables, flip it live, nor delete it. PR #5284
makes the undo fire only on a write whose delta named the owner. The rows it
already left are repaired by the versioned engine migration
`20260912000000_site_owner_restamp` (`component/database/`), which runs at boot
like every other: it selects `v1:platform:site` rows whose LATEST version has
an empty owner and a non-empty `packageId` (never `systemOwned`, never
`deleted`), takes the owner from the newest `packageDeployment` for that
package whose `deployables[]` names the site (its `requestedBy`), falls back
to the package row's own owner, and appends ONE new version with the owner
set and nothing else changed. A site neither source can name is left as it
is and logged by id; that one needs an operator to re-run `createSite` on its
id as the person who should own it. A second run selects nothing.

Writes need rank 200 and above (`{admin, developer, owner}` under the one
ladder). A client's own domain and a CI-pushed source are **cluster-owner
acts** and are offered to nobody else. A source that ships MemQL DSL needs an
actor who may author constructs -- an owner or a developer -- and is refused
with `dsl_requires_authoring` at the start of the run; see
[who may deploy what](packages.md#who-may-deploy-what).

---

## The hostname policy

A user's site is `<slug>.<domain>`, where `<domain>` is the one this cluster
serves (derived from `MEMQL_DOMAIN` through `component/frontdoor`, the same
derivation the OS host uses).

- The slug matches `[a-z0-9-]{3,40}`.
- It must be a **single label** -- one label, because the front door's
  `*.<domain>` Ingress wildcard matches exactly one.
- It must be unique across sites.
- It must not be reserved.

**The reserved set is derived, not listed.** It is `frontdoor.Roles()` (`api`,
`identity`, `mcp`) plus `frontdoor.PlatformSites()` (`os`, `vscode`), plus the
six held labels in `squatReservedSiteLabels`
(`component/memql/platform_site_hostname_policy.go`). Deriving it means a new
role or platform site can never become claimable by forgetting to add it here.
The held six are different in kind. `www`, `admin`, `mail` and `portal` are <!-- retired-vocabulary-ok: `portal` is a reserved site label in squatReservedSiteLabels -->
served by nothing, and that is precisely why they are held -- they are the
labels a person reads as the organization's rather than a tenant's, and `mail`
is where a mail host would land if one is ever added. `voice` and `turn` are
the LiveKit signaling and TURN hosts of [Ask and voice](ask-and-voice.md).

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
the console its config -- resolving the token from the named secret then, not at
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
grows with it. A system-owned deployable (MemQL OS, the VS Code landing page)
refuses the write whoever asks: those rows are re-seeded at every boot, so a
value set on one would be reverted and would look like it had worked until
then.

Enforced beside the engine's write path
(`component/memql/platform_site_settings_guard.go`), not in the mutation
body: a mutation sees a value and never an object's KEYS.

### Settings that belong to one store

A `shopify_storefront` deployable also carries **`storeSettings`**
(memql#5602): for each store, by its bare id, the values that belong to THAT
store rather than to the site -- the Customer Account API client id of one
store's Headless channel, the wholesale adapter configured for one store.

```
mutation updateSiteStoreSettings(siteId: "v1:platform:site:abc", storeSettings: {
  "acme":     {customerAccountClientId: "shp_live_client", wholesaleAdapter: "shopifyB2B"},
  "acme-dev": {customerAccountClientId: "shp_dev_client",  wholesaleAdapter: "customerTag"}
})
```

The edge merges the entry of the store the in-force binding names over
`settings`, into the same flat `settings` object of the runtime document, so a
bundle reads `config.settings.customerAccountClientId` as it always did.
Production is served its own store's values; the Testing destination, which
swaps in the Testing store, is served the Testing store's
([storefront-preview.md](storefront-preview.md)). Nothing branches on which
destination it is: the store the edge chose decides, so both destinations
bound to one store get that store's values.

- **A store's value overrides the site's for that store alone.** A key no
  store sets keeps the site's value, and an unbound destination gets the
  site's settings and no store's. A value that belongs to one store belongs in
  `storeSettings` only: left in `settings`, it reaches every store that does
  not set its own.
- **Keyed by store, not by destination.** Re-pointing a binding picks up the
  new store's entry; pointing it back restores the old one.
- **The same rules as `settings`, store by store** -- key form, the `Ref`
  refusal, plain strings, the per-deployable caps -- plus bare store ids
  (`acme-widgets`, never `v1:shopify:store:acme-widgets`), at most 16 stores,
  and one budget for every store's values together: no more characters than
  one full `settings` object can hold (`MEMQL_SITE_SETTINGS_MAX_KEYS` values
  of `MEMQL_SITE_SETTINGS_MAX_VALUE_LENGTH`). Public by construction, like
  every setting.
- **The write REPLACES**, for `updateSiteSettings`' reason, and has its
  authorization: the deployable's owner, or a cluster owner. It needs no
  store part -- which store a deployable is bound to stays
  `updateSiteStoreBinding`'s -- and an entry for a store the deployable is not
  bound to is never served.

---

## Statistics and health

How many people use a deployable, what they read, where they arrive from, and
whether it is healthy, all counted from the requests the edge recorded; nothing
has to be added to a site's build. In MemQL OS, a deployable's Statistics page
and the Websites section of the Deployables Overview draw these figures.

### What the edge records

The edge writes one row per request it answers for a deployable into its own
`edge_request` log: the deployable, the replica, when, the status, the bytes,
how long it took, what the edge did (an asset, an HTML document, the SPA
fallback, a proxied call, its own runtime-config document or scripts, a
page-change ping, or nothing at all, for a paused or draft deployable or a path
that matched nothing), and whether a bot asked. A **page view** also carries:

| Field | What it holds |
|---|---|
| path | The page's path with no query string or fragment, no trailing slash except at the root, at most 256 bytes. |
| source | Where a navigation arrived from (below). Empty for a navigation inside the site and for every ping. |
| country | An ISO 3166-1 alpha-2 code from the [country database](#the-country-database), or empty. |
| device, browser, OS | Members of closed sets read from the user agent (`component/edge/visit/agent.go`). |
| visitor | The day's visitor id (below), or empty when the replica held no secret for that day. |

Rows written before these fields existed read as *not measured* for everything
about visitors, and keep counting for health.

**Not recorded at all:** a hostname that resolves to no deployable; a
**system-owned** deployable (MemQL OS and the VS Code landing page, excluded by
the row field, so one added later is excluded too); the cluster's health probe
of a home page (`GET /` with the user agent `MemQL-Site-Health/1.0`); and the
version checks open pages make (`HEAD /runtime-config.json`).

### What is never stored

- **The IP address and the user agent.** The edge reads them on the serving
  path only to derive a page view's country, device, browser, operating system
  and visitor id, and keys the page-change ping's per-address budget on the
  address in memory only. Neither is a field of the record, so nothing the
  record reaches -- the writer, the table, a log line -- can hold them. This is
  what MemQL keeps: a CDN, a load balancer or the front door in front of the
  cluster may keep access logs of its own.
- **No query string, fragment or referring URL.** A path is normalized, and a
  source is a `utm_source` or a referring host.
- **No cookie.** Counting reads no cookie of the visitor's and stores nothing in
  the browser.

**The visitor id** is HMAC-SHA256 over the deployable's id, the visitor's
address (an IPv4 address whole, an IPv6 address by its /64) and their user
agent, under a random secret for one UTC day, truncated to 16 bytes and stored
as hex. The secret is a row of `edge_visitor_secret` that every edge replica
shares, so they hash one visitor alike. At each UTC midnight a replica makes the
new day's secret current and deletes every earlier day's, retrying the delete
hourly until it succeeds. Once a day's secret is gone, nothing stored can link
that day's ids back to an address, and because the deployable's id is hashed
in, one person on two deployables is two unrelated ids. A replica that holds no
secret for the day records page views with no visitor, so visitors read low
rather than one person being linked across two days.

### What counts

**A page view** is a person opening a page, by the rule `IsPageView` in
`component/edge/visit`: a `GET` answered 200 or 304, not a prefetch or
prerender, that the browser marks as a top-level navigation to a document
(`Sec-Fetch-Mode: navigate`, `Sec-Fetch-Dest: document`), or, from a browser
that sends no Fetch Metadata, one that accepts HTML and was answered with an
HTML document or the SPA fallback. The browser's word wins, so a navigation the
edge proxies (an identity page on the site's own origin, say) counts too. So
does an accepted [page-change ping](#the-page-change-ping).

**Never a page view**, though still recorded for health: a bot's request (an
empty user agent, or one on the closed list of crawlers, preview fetchers,
monitors, automation and HTTP clients in `component/edge/visit/agent.go`),
counted under health as a bot request; and a request served under a preview
grant -- an operator exercising a candidate version, or a storefront's testing
origin -- pings from such a page included. Bots that present a browser user
agent are counted as visitors.

**Sources** are recorded on a navigation: the landing URL's `utm_source`
(lowercased, at most 64 bytes); else the `Referer` host, without `www.` and with
a few names folded (`l.facebook.com` and `m.facebook.com` to `facebook.com`,
`t.co` to `x.com`, and `google.com`, `google.co.uk` and the like to `google`);
else `direct`. A `Referer` naming the host the request was made to, or the
deployable's own hostname, is a navigation inside the site and no source, and a
ping has none, so Sources counts arrivals rather than clicks between pages. A
`Referer` from another of the deployable's custom domains is a source.

**Countries** are the code of the visitor's address in the cluster's copy of
DB-IP's country database. A page view it cannot place, or one recorded while no
database was loaded, counts under `(unknown)`.

**Visits.** A visit is one visitor's page views with no gap over 30 minutes
(exactly 30 is the same visit), counted in the hour and the UTC day it started,
so hourly visits add up to the day's. A **bounce** is a one-page visit. **Views
per visit** is page views over visits and **bounce rate** bounces over visits,
both from a period's sums. In the top lists a visit counts under its entry page,
the first source among its page views, and its first page view's country,
device, browser and operating system.

**Visitors.** Days are UTC, and the visitor id changes at 00:00 UTC, so a person
seen either side of midnight is two visitors and their visit two visits. Over a
period longer than a day, visitors are each day's added up, so a person who
comes on three days is three visitors in a week; within `24h` they are counted
distinct from the request log, so one person seen in two hours is one. People
behind one address with the same browser are one visitor, and one person on two
networks is two. The address is the front door's: the right-most
`X-Forwarded-For` entry, else the connection's peer (`visit.ClientAddress`, the
rule the access-code limits use), so behind a CDN or another proxy in front of
the front door, everyone behind one of its addresses is one visitor, placed in
the proxy's country.

### Retention, tier by tier

| What | Where | Kept |
|---|---|---|
| Every request, with each page view's fields | `edge_request` | `MEMQL_EDGE_REQUEST_LOG_RETENTION_DAYS`: default 30 days, clamped to 1..365 |
| Per-minute and per-hour request counts | `edge_request_1m`, `edge_request_1h` | the same as the requests |
| Hourly statistics | `edge_stats_hourly` | 30 days |
| Daily statistics and each day's top lists | `edge_stats_daily`, `edge_stats_daily_top` | 400 days |
| Which days are final (below) | `edge_stats_final` | 400 days, as long as the day's figures |
| The visitor secret | `edge_visitor_secret` | its own UTC day |

An edge replica applies the request log's retention to the TimescaleDB policies
of the log and its two aggregates when its log starts; the rollup deletes
statistics rows past their retention on every run, so those tables keep it on a
database without TimescaleDB as well.

**Set `MEMQL_EDGE_REQUEST_LOG_RETENTION_DAYS` to the same value on every node,
not only on the edge.** The rollup reads it on whichever node holds the cron
lead, as how far back it recomputes a day that is not yet final, while the edge
applies it to the log. If the rollup's value is the wider, a day the log has
already dropped that is still open is recomputed from no rows, which erases its
figures; if it is the narrower, the older days of an outage stay open beyond
the rollup's reach.

### The rollup

`siteStatisticsRollupSweep` runs `builtin siteStatisticsRollup` every five
minutes on the cluster's cron leader, so while runs succeed a figure is at most
five minutes behind. A run recomputes today and every earlier day that is not
yet **final**. A day becomes final when a run recomputes it at or after 02:00
UTC the next day, and is not recomputed after that, so a request recorded for it
later is not counted. **An outage heals on the next run that succeeds**: the
days it left open are recomputed with every request that arrived meanwhile, as
far back as the request log's retention reaches. A day whose requests left the
log first keeps the figures last written for it.

### The page-change ping

A single-page app changes pages with `history.pushState`, which the edge never
sees, so the edge inserts a small script into every HTML document a live,
non-system-owned deployable serves from its own origin
([where it lands](site-hosting.md#counting-a-single-page-apps-pages)). When
`location.pathname` changes, by a `pushState` or the browser's back and
forward, the script sends the new path with `navigator.sendBeacon` to
`POST /_memql/view`, which the edge answers itself and never forwards. It never
reports the first load, which the navigation already counted, and it reads no
cookie and stores nothing in the browser. A ping is a page view only when:

- it is a `POST` from the site's own origin: its `Origin` names the host it was
  sent to, or, when `Origin` is absent or the `null` a page with a
  `no-referrer` policy sends, `Sec-Fetch-Site` says `same-origin`;
- its address has budget left: 120 pings a minute per address per deployable on
  each replica, a budget held in memory only;
- its body, at most 1 KiB, decodes to a path.

Every ping to a live deployable is answered `204` with no body, counted or not,
so the route says nothing about why one was refused; a refused ping is still
recorded for health, under the path class `view`.

### Reading the statistics

```
builtin siteStatistics(siteIds: ["abc"], period: "30d")
```

`siteStatistics` answers one `integration:siteTraffic:statistics` row. Name up
to 200 deployables in `siteIds`, as bare ids, or leave it out to read every
deployable the caller may read, added together, and keep that to some
organizations with `accountIds`.

- **Periods.** `24h` is the 24 hours ending with the hour under way, in hourly
  buckets; `7d`, `30d` and `90d` end with today, in daily buckets; `12m` is the
  current month and the twelve before it, in 13 monthly buckets, so this month
  stands beside the same month last year. The bucket under way is included.
  `period` is required; any other value is refused.
- **The previous period.** Every period but `12m` comes with the one of the same
  length just before it: `previous` holds its totals and `previousSeries` its
  buckets, aligned by offset with `series`. Twelve months have none
  (`hasPrevious` is false; `previous`, `previousSeries`, `prevStart` and
  `prevEnd` are `null`): the thirteen months before them are past the daily
  rows' 400 days.
- **What it answers.** `start`, `end` and `bucket`; `totals` (visitors, visits,
  bounces, page views, views per visit, bounce rate, requests, errors (5xx), not
  found (4xx), bytes, bot requests, and the median and 95th-percentile response
  time in milliseconds); `series`, one point per bucket (visitors, visits,
  bounces, page views, requests, errors, not found and the 95th-percentile
  response time); `top`, the 50 leading pages, sources, countries, devices,
  browsers and systems, each with its visitors, visits and page views, and an
  `(other)` remainder last; `sites`, each deployable with figures in the period
  and its own visitors, visits and page views, most visitors first; and
  `measuredSince`, the first day any of them has visitor figures.
- **A figure nothing measured is `null`, never 0**: the visitor figures before
  visitor counting began, every bucket before these deployables' first day of
  statistics, everything in a period with no rows at all, and a rate with no
  visit to divide by. Inside a period that has rows, a bucket with none is one
  nobody was served in, and reads 0.
- **Response times**: percentiles are accurate to about 10%; anything over about
  33 s lands in the last bucket.
- **Top lists** over days are summed from each day's hundred leading values per
  list, the rest of each day folded into its `(other)`, so a value that seldom
  makes a day's hundred reads low over a long period; for `24h` they are
  counted from the request log.

**Narrowed under the caller.** An id or an organization the caller cannot read
is dropped, not refused, so naming another client's deployable or organization
gets the answer a deployable with no traffic gets and confirms nothing.
System-owned deployables are never in an answer. More than 200 named
deployables are refused rather than answered short.

### Request counts for a list

The older read answers from the per-minute and per-hour request counts:

```
builtin siteTrafficInWindow(siteIds: ["abc"], bucket: "1h", windowStart: "2026-09-02T00:00:00Z", windowEnd: "2026-09-03T00:00:00Z")
```

It answers `v1:observability:siteTraffic` rows (requests, errors, client
errors, bytes, and when the deployable last served), one per bucket, or with
`summary: true` one per deployable for the whole window. `1m` buckets suit a
window of an hour and `1h` buckets a day or a week. `siteIds` is required, at
most 200, and refused past that rather than truncated; it is narrowed under the
caller's actor as above, and a deployable with no rows in the window gets no
row.

### The country database

Countries come from DB-IP's IP-to-Country Lite database, which the cluster
downloads itself rather than carrying one in its image, because DB-IP serves
only the current and the previous month's file. `geoCountryRefreshSweep` runs
`builtin geoCountryRefresh` daily at 04:10 UTC on the cron leader. While the
stored release is the current month it makes no request; otherwise it
downloads

```
https://download.db-ip.com/free/dbip-country-lite-YYYY-MM.mmdb.gz
```

for the current month, or for the previous month while DB-IP answers 404 for
the current one, downloading the previous month again each day until the
current one appears. A file over 32 MiB compressed or 64 MiB decompressed, or
one that does not open as an MMDB country database, is refused before anything
is replaced; a good one replaces the single row of `edge_geo_country`. This is
the only outbound call the statistics make. A failed download keeps the stored
database in use and the run still succeeds, so a cluster with no route to
`download.db-ip.com` does not fail every day. Every edge replica loads the
database when it starts and checks the stored release hourly, swapping a newer
one in without a restart.

**Before the first download there are no countries.** A new cluster downloads at
its first 04:10 UTC, and each edge replica loads the file within the hour; until
then page views are recorded with no country and stay under `(unknown)`. A
cluster that cannot reach `download.db-ip.com` records no country at all.

**Attribution.** The database is licensed under CC BY 4.0, which requires
attribution: show [IP Geolocation by DB-IP](https://db-ip.com) beside any
countries you present from it, in a page or a report. The repository's `NOTICE`
carries the same credit.

### A paragraph for a privacy policy

The paragraph below says what MemQL records for these statistics, for an
operator to adapt into a hosted site's privacy policy. If you changed
`MEMQL_EDGE_REQUEST_LOG_RETENTION_DAYS`, change how long the records of single
requests are kept to match, and add what anything in front of the cluster
records -- a CDN, a load balancer, the front door's own access logs -- which it
does not cover. Whether a site needs more than this, consent included, is for
that site's own legal review.

> **Website statistics.** This website keeps statistics about how it is used.
> When you open a page, we record the page's address without any query string,
> the website you arrived from (its name only, or a campaign tag in the link
> you followed), your country, which we look up from your IP address in DB-IP's
> IP-to-Country database, the type of device, browser and operating system your
> browser reports, and a visitor code. The visitor code is a one-way keyed hash
> of your IP address and your browser's identification string, made with a
> secret key that is replaced every day at 00:00 UTC and then deleted: it lets
> us count visitors within one day, and afterwards it cannot be linked back to
> you, to your visits on other days, or to your visits to other websites. Your
> IP address and your browser's identification string are not stored. We set
> no cookie and store nothing in your browser for these statistics; on pages
> that change without reloading, a small script reports the new page's address.
> For every request we also record its time, outcome, size and duration, to
> keep the website healthy. The records of single requests and page views are
> kept for 30 days, hourly totals for 30 days, and daily totals, with the day's
> most visited pages and most common sources, countries, devices, browsers and
> operating systems, for 400 days.

### Reading a figure

**Not measured is not zero.** Where nothing measured a figure, both reads say
so -- `siteTrafficInWindow` with no row, `siteStatistics` with `null` -- and
where something measured nothing, they say 0: one means nothing was
recording, the other that nobody came. A figure is not measured when:

- nothing at all was served in the period, which a read cannot tell apart from
  nothing recording;
- `MEMQL_EDGE_REQUEST_LOG_ENABLED` is `false` on the replica that served (a
  figure is then short by what that replica served);
- the deployable is system-owned, so never recorded;
- for the visitor figures alone, the period comes before visitor counting
  began, which `measuredSince` dates.

**Errors and not found are counted apart.** 5xx is the deployable failing; 4xx
is somebody asking for a page it does not have. Folding them together makes a
healthy site with a broken inbound link look unhealthy.

**The cost to a visitor** is bounded in-memory work -- string matching, one
HMAC, an in-memory country lookup and a hand-off to a batching writer that drops
and counts under pressure -- and never a wait on Postgres.

**When a figure looks low.** Drops are on `memql_site_traffic_dropped_total`
(labelled `queue_full` / `write_failed`) and writes on
`memql_site_traffic_written_total`: read them together, because the figure
itself cannot say what is missing from it. Three other causes leave their own
log lines:

- page views but few or no visitors: an edge replica could not read the day's
  visitor secret (`could not rotate the visitor secret; unique visitors are not
  counted until it succeeds`);
- every country `(unknown)`: no country database has been downloaded yet (the
  first run is at 04:10 UTC), or the download is failing (`Country database
  refresh failed; the stored release stays in use`, on the cron leader);
- figures that stop moving: rollup runs are failing (`Site statistics for a day
  were not written; the next run recomputes it`, one line per day, on the cron
  leader). A run that recomputed an older day or deleted rows past retention
  logs `Site statistics rolled up` at Info.

### What it is not

No cookie, and no visitor identity that outlives a UTC day; no live "visitors
now" figure; no custom date ranges, per-site time zones, goals or conversions;
no export or scheduled reports; no alerting and no uptime probing from outside
the cluster. Each is out of scope by decision rather than by omission.

---

## Access codes

A deployable is public: anybody who resolves its hostname is served its bundle.
**Access codes** let part of it open only for people an operator gives a code to,
with no sign-in, or to anyone, with guest access. The full account -- the wire, what the page must do, how a gate
is declared and the limits -- is [Access codes](site-access-codes.md). This
section is what belongs to the deployable.

**Where.** The lock after Traffic in a deployable's page head opens its Access
codes: a Guest access part, a list, a short Add form, and Regenerate, Change
expiry and Remove on each row. The control is *absent*, not disabled, for a person who does not hold the
part, on a system-owned deployable, and on one that was not deployed from a
package.

**The part.** `execute` on `app:deployables/access`, seeded on owner and
developer, and judged against the deployable's own organization as
`sitePreviewOpen` is. A code is minted for one gate of the deployable's own app,
optionally for one subject (one client), and **always expires**: more than a
minute away and at most 366 days, with presets of 10 minutes, 1 hour, 1 day,
7 days and 30 days.

**What a code opens is declared in the package.** A construct carries
`@accessGate(deployable="...", gate="...")` for the deployable's manifest name.
A deployable with none declared offers no gate to choose, and says how to add
one. Which DSL domains a package ships is recorded by the deploy that serves
visitors (`dslDomains`; see the candidate paragraph under
[Publishing the candidate version instead](#publishing-the-candidate-version-instead)),
so **a source deployed before access codes existed has no gates until it is
deployed again.**

**Refused**, with a sentence that says what to change:

- a deployable with **no owner**, and a **system-owned** one -- a call runs under
  its owner's authority, so a deployable without one has none to lend;
- a deployable with **no package deployable**, or a gate it does not declare;
- a name outside 1 to 80 characters, a subject over 120, and an expiry outside
  the bounds above.

**A person mints a code, never a run.** Add and Regenerate refuse inside a run,
because a run's journal is durable and readable by the cluster and the code must
exist in the reply and nowhere else. Every act needs a person behind it: a
synthetic actor and the operator credential are refused. A deployable that loses
its owner can only have its codes removed.

**Moving a deployable to another organization is refused while it has codes.** A
code copies its deployable's organization when it is minted, so the organization
cannot change while a code has not been removed -- expired ones included, since a
new expiry revives them. The refusal says to remove the codes first; add new ones
after the move.

**Guest access** is the same dialog's other half, for a deployable meant to be
explored in public: a **Guest access** part above the codes turns on a guest
pass, which lets anyone open one gate with no code until an end date the operator
sets (the same presets and 366-day cap). One pass at a time; Change end date and
Turn off are on it, and Turn off takes every guest out now. It shows no code,
because there is none, and a run may remove a pass but not create or re-time one.
All guests share one partition, so a product keeps anything personal out of a gate
it opens to them ([Guest access](site-access-codes.md#guest-access)).

**Regenerate is revocation.** It replaces the secret and keeps the id, name, gate
and subject, so rows a product stamped with the code's id stay attached, and
anyone using the old code is taken out now. **Remove is final.** Each act is
written to the audit log, never with the code.

---

## Where the surface is

**MemQL OS, the Deployables app**, in three sections: **Map** (what serves
where, and the window's default), **Deployables** (the list, and the page that
composes, deploys and manages one), and **Settings** (the app's own
preferences, plus the Sources group above). Composing is not a fourth section
and not a modal: New deployable opens the same page in compose mode, in place,
with a quiet Back to the list.

A package with two apps is **two rows sharing a source**, grouped under it --
one row per thing that serves or will. On a deployable's page, a lock after
Traffic in the head opens its Access codes, for a person who holds the part.

The Deployables app is the one surface, and `/sites` and
everything under it redirect to it, tail included, so an old bookmark or a
link in a runbook still lands.

---

## Related

- [Packages and deployables](packages.md) -- the source half: the manifest, the
  pipeline, DSL delivery, and the refusal catalogue.
- [The Library](library.md) -- where the zip artifact you deploy comes from.
- [Site hosting](site-hosting.md) -- the request path, the CI publish route and
  the per-site CSP.
- [Access codes](site-access-codes.md) -- keeping part of a deployable for people
  who hold a code: the codes, the gates, the wire and the limits.
- [Front door](front-door.md) -- hosts, certificates, custom domains and the
  two TLS regimes.
- [GitHub Connect](github-connect.md) -- connect GitHub once and pick a
  repository from a list; the pasted token stays behind "Use a token instead".
