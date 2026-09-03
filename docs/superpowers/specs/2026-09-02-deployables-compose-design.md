# Deployables Compose -- Design

- **Date:** 2026-09-02
- **Status:** approved (in-session Q&A with the owner; every fork below
  records the choice that was made and why)
- **Scope:** `clients/os/src/apps/deployables/` (the app is rebuilt around
  one section and one page), `dsl/platform/` (one new concept, two probes,
  two credential capabilities, `placements` on `packageDeploy`, four refusal
  codes), `component/packages/` (the probe, credential resolution, the
  not-offered target scope, placements in the publish stage), and the
  operator docs. No new HTTP routes. The portal is untouched.
- **The wave this belongs to:** Epic 1 of four in
  [the Deployables program](2026-09-02-deployables-program-design.md).
  Logs, Build and Run follow, each with its own design round; this spec
  leaves their hooks out rather than half-designing them.

## Why

The owner's brief and the verified state are recorded in the program
design. In one sentence each: creating a deployable, giving it an address,
binding a domain and tying it to a client took three sections and two
mental models; a private repository could not be connected from the OS at
all; and the future kinds had no place to go. This epic is the one flow,
the credential it needs, and the model the future kinds will fill.

## Locked decisions

| # | Decision | Choice (owner-approved) |
|---|---|---|
| D1 | Sections | **Map, Deployables, Settings.** Actions, Sites and Packages retire. A saved default naming one of the three maps to Deployables |
| D2 | The list | **One row per thing that serves or will.** A package with two apps is two rows sharing a source, grouped under it. Search and facets behind Refine; archived stays a quiet flip below the list |
| D3 | The page | **The rail, top to bottom**: Source, What it is, Where it lives, Build, Live, then Every attempt. The Head's one primary action follows the state |
| D4 | Composition | **The rail is the form.** New deployable is the Head's primary action on Deployables and opens the same page in compose mode, inside the section. A parked run lives on its row |
| D5 | Clicks | **Two on a first deploy, one after.** Analyze creates the source record and parks a run at the existing confirm gate; Deploy confirms it with the placements. A redeploy is Deploy alone |
| D6 | Sources | **Three, chosen once**: a repository (probed), a zip in Files (the analysis decides whether it is a package tree or a built site), or the person's own CI pushing bundles (cluster owners; the stop shows the route and the mint command) |
| D7 | Going live | **Stays a deliberate click** at the end of the rail. A first deploy creates the site as a draft and publishes; Make it live is the Head's action afterwards. The publisher's reasoning stands: a first deploy that went straight to live would put a stranger's code on a hostname the moment it built |
| D8 | Placements | **`packageDeploy` takes `placements` per app**, `{hostname, accountId, ownDomain}`, replacing `hostnames`. The pipeline stamps the client and creates the domain binding after the site exists. No client-side follow-up writes |
| D9 | Targets | **A target has four parts**: the address stop's shape, the build surface, the live states, the row. Web is registered; ios, android and macos are written down as shapes. A known-but-unoffered kind is `deployable_target_not_offered`, scoped to the app and not fatal to the package |
| D10 | Credentials | **`v1:platform:sourceCredential`**, composite tier, sealed once server-side, resolved under the package owner's actor. `repoTokenRef` is deleted with no shim |
| D11 | The probe | **`sourceProbe` answers host, reachable, private, default branch and a typed reason**, storing nothing. `artifactProbe` answers whether a zip is a package tree or a built site |
| D12 | Migration | **Deletion.** Pre-release: `repoTokenRef` and `hostnames` go; a package row carrying a token name reads as "no credential" and asks for one |

## A. Information architecture

**Three sections.** Map is first and stays the window's default; Deployables
is the list and the page; Settings keeps the default-section and density
preferences and gains the Sources group (section D). The `settings.ts`
section list shrinks to three and its sanitiser maps `sites`, `packages` and
`actions` to `deployables`.

**The Deployables list.** One `LiveList` over the site feed, joined
client-side to the package feed by `packageId` for the source line and the
update chip. A package with several apps renders as a group: the source line
once, the rows beneath it. Each row: name, address, the standing five-dot
rail, the update chip when `updateAvailable`, the client chip, the arrival
cue. Refine holds search and the facets kind, status, client and source.
"Show archived" stays the quiet flip below the list that Packages defends in
`clients/os/README.md`; it re-baselines through `useLiveView`'s key.

**The Head's one primary action is New deployable** (rank at or above 200,
the deploy tier). It opens the page in compose mode in place, the way Files
opens a folder: the Head's title becomes "New deployable", the list is
replaced by the rail, and a quiet Back returns to the list. A person who
closes the window mid-compose finds their run on its row: the list marks a
row "a deploy is waiting for you" whenever a deployment at
`awaiting_confirm` exists for a package they own, and opening it lands on the
rail with the report in place. That mark comes from a FOURTH feed at the app
root, seeded by `packageDeploymentsAwaitingConfirm` and holding parked runs
only. It is a deliberate exception to the README's rule that a timeline is
retained by the page and never by the root: that rule guards against
subscribing a window to every deploy in the cluster, and a feed over parked
runs alone is a handful of rows that a person needs to see before they open
anything.

**The page.** Selecting a row, on the list or on the Map, opens the page
beneath it, as the detail panel does today. The Head carries the deployable's
name, one primary action that follows the state (Analyze, Deploy, Make it
live, Deploy the update, Retry), and the quiet Ask and Open. Below it the
rail, then Every attempt, the append-only runs with their own rails, roll
back on any succeeded one that is not the latest. Package-level acts sit on
the Source stop: archive this source and every app it produced, restore.

**The Map is untouched.** Its selection is the same selection the list holds.

**Roles as today.** Writes need rank 200 and above. A client's own domain
and a CI-pushed source are cluster-owner acts and render only for one. A
source that ships MemQL says on its What-it-is stop that deploying it is a
cluster owner's decision, the D9 gate of memql#4794 stated before the click.

## B. The target model

A target is what the rail needs to know about a kind of deployable. Every
stop renders from it, so the page has no branch on "which kind is this".

| Target | Address stop | Build surface | Live states | Row |
|---|---|---|---|---|
| web (`spa`, `static`, `shopify_storefront`) | hostname under the cluster domain; optional own domain | prebuilt output, or the workbench from the Build epic | draft, live, disabled, archived | `v1:platform:site` |
| ios | bundle id and App Store Connect app | a Mac in the person's Fleet | built, uploaded, in review, released | a new concept, never `site` |
| android | application id and Play listing | workbench with a JDK and SDK | built, uploaded, in review, released | the same new concept |
| macos | bundle id; notarized disk image or Mac App Store | a Mac in the person's Fleet | built, notarized, released | the same new concept |

**Only web is registered.** `targets.ts` holds one entry; the three others
are this table, not code. Nothing in the OS renders a control for them, and
the kind picker on a hand-made deployable offers the three web kinds with
the one sentence the create form carries today.

**The engine tells the truth about a kind it knows but does not offer.**
`analyzeDeployables` distinguishes three cases: an offered kind, a known
kind that is not offered (`ios`, `android`, `macos`), and a kind nobody has
heard of. The second is `deployable_target_not_offered`, scoped to the app,
fatal to that app and not to the package, exactly as `go_pack_not_deployable`
is reported today: the What-it-is stop shows the app with "iOS is not
offered on this cluster yet" and the rest deploys. The third stays
`deployable_kind_unknown`.

**One list, pinned twice.** `TestSiteKindEnumIsExactlyThreeValues` keeps
the enum; a new parity test holds the OS's offered kinds equal to it, the
way `TestFleetOnlineWindowMatchesPortal` holds the online window equal
across client and engine.

## C. The flow, stop by stop

### Source

Chosen once; a later deploy shows it as facts.

- **A repository.** URL and an optional branch or tag (empty follows the
  default branch, resolved at fetch time as today). On blur the stop calls
  `sourceProbe`. Reachable and public: "public, default branch main". Not
  reachable: "private, or not there", and a credential field appears
  offering the person's active credentials for that host or a new one
  (label and token, the token field write-only and unmasked, the
  Integrations precedent). The probe runs again under the chosen
  credential; "this token cannot see it" is the reply when it still cannot.
  A non-GitHub host is `source_host_unsupported`: "only github.com today, or
  upload a zip". The stop states the token's shape: fine-grained, contents
  read on the repositories it should reach, nothing else.
- **A zip in Files.** The picker over the person's zip artifacts, retained
  only while open (`useZipArtifacts`). On choice the stop calls
  `artifactProbe`: a package tree (manifest at root) takes the package path;
  a built site (index.html at root, no manifest) takes the hand-made path
  and asks for the kind. Neither is deployed by choosing.
- **Pushed by your CI.** Cluster owners only. Name and kind. Analyze creates
  the site as a draft with the placeholder bundle
  (`blob://sites/<siteId>/pending/`, the convention `deployables.md`
  records), and the stop then shows the site id, the bundle route
  `POST /sites/{id}/bundles`, and the exact `memql service-account-token
  mint` command from `service-account-jwt.md`. The Live stop waits for the
  first push, which arrives as the `bundleRef` flip the row already
  broadcasts.

### What it is

Read-only, filled from the report the confirm gate already carries:

- each app: name, kind, path, build plan ("prebuilt, build skipped" or the
  command it will run and its output directory), a storefront's binding,
  and its problem if any, including a not-offered target;
- each DSL domain with construct counts and files, and when any exist the
  cluster-owner sentence;
- a Go pack, reported and deferred;
- for a built-site zip: file count, total bytes, and "index.html present".

While the analysis runs the stop shows the same ring the rail's current
stage shows; a fatal problem stops the rail here and renders its copy.

### Where it lives

Per app, on a first deploy: a slug with the hostname previewed at
keystroke rate and validated by the browser half of the policy
(`validateSlug`), the client it is for (the account picker), and for a
cluster owner the client's own domain, prefilled from that client's record
(`v1:accounts:account.domain`) and validated by `normalizeHostname`.
"Chosen once. A later deploy of this source keeps the same addresses." On a
redeploy the stop is facts: the address, the client chip, and each bound
domain's stepped rail with its two records and what the sweep last saw, the
Domains panel's content mounted as the stop.

The deploy never waits on DNS. The app goes live at its cluster address and
the domain stays "waiting on your DNS records" until both check out.

### Build

In this epic the stop has two readings. Prebuilt: skipped, with the reason
("its built output is in the source"). Needs a build: the typed refusal the
engine gives today, rendered in place with its copy, naming the command it
would have run and the two ways forward. The Build epic replaces the second
reading with progress on the surface that built it and the log; it changes
what the stop says, not where it is.

### Live

After a first publish: "Published to shop.znas.io. Not serving yet." and
Make it live as the Head's action (`updateSiteStatus` to `live`). Afterwards:
live since when, the version list walked from the row's own history with
roll back on each, pause and resume with the 503-versus-404 sentence,
archive with the typed confirmation the server verifies. A system-owned row
renders no controls, as today. For a package deployable, Every attempt
beneath the rail is where a whole-package rollback lives, restoring a prior
row's tuple.

### The Head's action, by state

| State | Action |
|---|---|
| composing, source incomplete | Analyze, disabled |
| composing, source complete | Analyze |
| run at `awaiting_confirm`, placements incomplete | Deploy, disabled |
| run at `awaiting_confirm`, placements complete | Deploy |
| run at a non-terminal stage | none; the rail is moving |
| site draft with a bundle | Make it live |
| live, source has a newer version | Deploy the update |
| run refused or failed | Retry |
| live, nothing newer | Redeploy, quiet |

### Wire

Analyze on the package path is `createPackage` then
`packageDeploy(confirm: false)`, which opens the deployment row, fetches,
analyzes and parks, as today. Deploy is `packageDeploy(confirm: true,
placements)`. On the hand-made path Analyze is `createSite` (draft,
placeholder bundle) plus, for a zip, `artifactProbe`; Deploy is
`sitePublishFromArtifact` for a zip and nothing for a CI-pushed source.
Every write goes through the generated builders where they exist and
through `renderMemQLValue` everywhere else, the rule `packages/calls.ts`
already states.

## D. Personal source credentials

**Concept** `v1:platform:sourceCredential`, `@rowAuthz(owner="ownerUserId",
clusterOwner)`, broadcast created and updated:

| Field | Notes |
|---|---|
| `ownerUserId` | stamped from the actor by the writer; the row-authz key |
| `host` | `github.com` today; the probe and fetcher match on it |
| `label` | the person's name for it |
| `encryptedValue` | `@secret`; `secret.Encrypt` under the master key |
| `fingerprint` | the last four characters, for telling two apart |
| `status` | `active`, `revoked` |
| `lastUsedAt` | stamped by the fetcher and the poll; a heartbeat, never in an arrival-cue fingerprint |
| `revokedAt` | set by revoke; the row is never deleted |

**Shape** `sourceCredentialCard` projects everything but `encryptedValue`,
so no read can return the ciphertext. **Query** `sourceCredentialsMine`
lets the tier narrow. A cluster owner reads every row's metadata, which is
the oversight the composite tier exists for, and can decrypt none of it
from a browser because decryption happens only inside a fetch.

**Writes.** `sourceCredentialCreate({host, label, token})` is a capability:
it seals with `secret.Encrypt`, calls the `@serverOnly`
`createSourceCredential` under stamped internal origin with the caller's
user actor (the `auth.ContextWithUserActor` pattern the campaigns drain
uses, so the owner stamp is the caller and the origin is trusted), and
answers `{credentialId, fingerprint}`. The token is a function-local for
the length of one call and appears in no row, log or reply.
`sourceCredentialRevoke({credentialId})` flips status and stamps
`revokedAt` through an owned mutation.

**The package** loses `repoTokenRef` and gains `credentialId`, with
`@relationship(type="references", as="fetchesUnder", field="credentialId",
target=sourceCredential, direction="outgoing")`. `createPackage` and
`updatePackageSource` take `credentialId`.

**Resolution** in the fetcher and the polling feed: read the credential
under `auth.ContextWithUserActor(ctx, pkg.ownerUserId)` through the owned
query. Zero rows is `credential_not_found`; `status == "revoked"` is
`credential_revoked`; decrypt inside the fetch, hold the value in a local,
set the bearer, discard. A cluster owner deploying somebody's package
fetches under that package's own credential, which is correct: they are
deploying that package. No cluster-wide source credential exists any more.

**The probe.** `sourceProbe({repoUrl, credentialId})` parses the URL with
`parseGitHubRepo`, resolves the credential the same way when one is named,
asks `GET https://api.github.com/repos/{owner}/{repo}`, and answers
`{host, reachable, private, defaultBranch, reason}` where `reason` is one of
`ok`, `not_found_or_private`, `credential_cannot_see_it`,
`credential_not_found`, `credential_revoked`, `source_host_unsupported`,
`rate_limited`. It writes nothing and stamps nothing.

**Settings, the Sources group.** Every credential the person holds: host,
label, fingerprint, last used, and the sources fetching under it (a join on
`credentialId` over the package feed). Add, and revoke with the sentence
"sources fetching under it will refuse at their next fetch until you switch
them". Rotation is adding a new credential and switching a source to it on
its Source stop (`updatePackageSource`).

## E. Data model, capabilities, refusals

**Concepts.** `sourceCredential` new (section D). `package`: `repoTokenRef`
removed, `credentialId` added. `site` and `packageDeployment`: unchanged.
No row for a CI-pushed deployable: the site's `bundleRef` form and
`artifactId` are its Source stop's facts.

**Queries.** `packageDeploymentsAwaitingConfirm`, owned tier, newest first,
for the list's waiting mark.

**Capabilities.** `sourceProbe`, `artifactProbe` (read-only; the artifact
probe opens the zip through `OpenZip` under the packages limits and reports
`{isPackage, isBuiltSite, fileCount, totalBytes}`), `sourceCredentialCreate`,
`sourceCredentialRevoke`. `packageDeploy` takes `placements` and drops
`hostnames`.

**The publish stage** reads `placements[dep.Name]` where it read
`req.Hostnames[dep.Name]`. After `EnsureSite` and `bindSiteToPackage`, when
the placement names an account it runs `updateSiteAccount`, and when it
names an own domain it runs `customDomainAdd`, both under the caller's
actor so the guards `platform_custom_domain_policy.go` and the account
write already run decide exactly as they do from the page. A refused
domain is recorded on the outcome as `{name, siteId, hostname, ...,
domainRefusal}` and does not fail the publish: the site is live at its
cluster address and the Where-it-lives stop shows the refusal.

**Refusal codes**, stable, added to `component/packages/refusal.go` and to
the OS copy table: `credential_not_found`, `credential_revoked`,
`source_host_unsupported`, `deployable_target_not_offered`. Every code the
engine emits is held to a copy entry or an explicit "server sentence only"
listing by a coverage test.

**Routing rules.** `graph.node.created|updated.v1:platform:sourceCredential`
broadcast, beside the package rules in `component/node/routing.go`, so the
Sources group and a Source stop's credential chip are live.

**The fan-out** a new concept and new capabilities carry is expected work:
memqllint, SDK generation for both SDKs, the env registry, the architecture
model, the embed count, and the dslconformance classification of the new
concept (owned tier, composite form).

## F. The OS

`apps/deployables/` keeps `useSites`, `usePackages`, `rows.ts`,
`packages/rows.ts`, `hostname.ts`, `packages/refusals.ts`, the `map/`
tree, `domains.ts`, `DomainsPanel.tsx`'s content, `useCustomDomains`,
`useZipArtifacts` and `settings.ts`. It replaces `SitesSection`,
`PackagesSection`, `ActionsSection`, `CreateSite`, `NewPackage`,
`PackageDetail`, `SiteDetail`, `ConfirmGate` and `SiteLifecycle` with:

- `DeployablesSection.tsx`: the Head, Refine, the grouped LiveList, the
  archived flip, and the page or the compose view beneath.
- `page/DeployablePage.tsx`: the Head-with-state-action and the rail.
- `page/Rail.tsx` and `page/rail.ts`: the generalised StageRail. `railFor`
  becomes a reading over a stop set the target defines, with three modes,
  compose, deploy and standing; the skipped-stage rule and the
  dims-content-not-row rule survive unchanged, and the deploy mode reproduces
  today's `railFor` output exactly (its tests move, not change).
- `page/stops/Source.tsx`, `WhatItIs.tsx`, `WhereItLives.tsx`, `Build.tsx`,
  `Live.tsx`: one component per stop, each reading its target's stop
  definition and the row.
- `targets.ts`: the registry, web only, and the offered-kinds list.
- `sources/`: the probe hooks and the credential picker.
- `settings/SourcesGroup.tsx`: mounted by the app's Settings section.

**Copy vocabulary**, said once and kept: the noun is deployable; a package
is a source; the stops are Source, What it is, Where it lives, Build, Live;
the actions are Analyze, Deploy, Make it live, Deploy the update, Retry,
Redeploy, Roll back to this, Pause, Archive. "Published" is the word a
finished publish uses because Deploy is the button that produced it.

**House rules throughout**: one feed per concept retained at the app root;
arrival cues fingerprint change and never a heartbeat (`lastUsedAt`,
`lastCheckedAt` stay out); every refusal renders in place with the server's
sentence; no toasts, no dialogs; a section opens with the Head and at most
one primary action; forms use `Field`; nothing paints half a window of dead
space.

## G. Security

- Both new capabilities run under the caller's actor; the credential writer
  stamps the owner from it and refuses an empty actor.
- The token crosses the wire once, inside `sourceCredentialCreate`, and is
  never in a query result, a broadcast payload, a log line or a reply.
- Resolution is owner-scoped by construction: the fetcher reads under the
  package owner's actor and the owned tier admits only that owner's rows.
- The probe carries no more authority than a fetch: it resolves a credential
  the same way and answers a typed reason, never the API's own body.
- `placements` runs the account and domain writes under the caller's actor,
  so the existing guards decide; the pipeline gains no bypass.
- `deployable_target_not_offered` is scoped and non-fatal, so an unoffered
  app cannot block a package and cannot be smuggled through as an offered
  kind.

## H. Error handling

Typed reason at every refusal, rendered at the stop it belongs to with the
OS's copy above and the server's sentence beneath, verbatim. A refusal with
no known code renders under a neutral heading with the server's sentence
alone. The rail marks the stop stopped and every later stop unreached.
Failures before publish leave every site serving what it was serving, as
today. A probe that cannot reach GitHub says so and leaves the stop
editable; it never blocks Analyze on a public repository, because the fetch
is the authority and the probe is a courtesy.

## I. Testing

- **`rail.test.ts`** grows the compose and standing readings: a private
  repository with no credential parks the Source stop; a not-offered target
  renders its sentence and the rest deploys; a prebuilt app skips Build with
  the reason; a first deploy ends on "not serving yet"; a redeploy is one
  click; the deploy mode reproduces every existing case.
- **Parity**: the offered kinds equal the site enum; every refusal code has
  copy or an explicit listing; the section list and the settings picker
  offer the same set.
- **Surfaces** (`deployables.test.tsx`): the Head's action by state; the
  grouped list; "a deploy is waiting for you"; the retained-feed contract per
  `clients/os/README.md`; the Sources group; vitest per-file isolation.
- **Engine** (`component/packages`): the probe against a fake GitHub for
  public, private without a credential, private with one, revoked,
  rate-limited and a non-GitHub host; the fetcher refusing a credential the
  package owner cannot read; the poll feed under the same rule;
  `sourceCredentialCreate` never returning or logging the token (a test
  greps the reply and the captured log); `placements` stamping the account
  and creating the domain binding under the existing guards, and recording
  a refused domain without failing the publish; the analyzer's not-offered
  scope non-fatal to the package; the render-parse test for every rendered
  call.
- **Db-gated cases** run against the throwaway Postgres on 15434 with
  `MEMQL_REQUIRE_DB=1`, never the k3d decoy on 5432.
- **Acceptance for the surface** is rendered screenshots at real size, both
  modes, empty and populated, per `clients/os/DESIGN.md`.

## J. Migration and docs

Pre-release, so deletion: `repoTokenRef` on the concept, the mutations, the
shapes, the SDKs and the OS; `hostnames` on `packageDeploy`. A package row
still carrying a token name reads as "no credential" on its Source stop and
asks for one. Old default-section preferences map to Deployables.
`docs/public/operate/deployables.md` and `packages.md` are rewritten around
the one flow, the credential and the target model; the OS README's
Deployables and Packages passages are replaced by one that records the
rail-as-form rule and the three modes of `railFor`; GLOSSARY entries follow.
The portal stays maintenance-only and is not touched.

## K. Out of scope, and neighbours

Out of scope for this epic: the log store and the Logs app (Epic 2);
workbench builds, Fleet routing, abandoned-run detection and the auto-deploy
switch (Epic 3); runtime settings and traffic and health (Epic 4); a GitHub
App; non-GitHub hosts; any mobile schema; a bundle upload from the browser.

Neighbours: the Accounts app owns the account picker this epic mounts on
the address stop; the custom-domains sweep and scripts are consumed
unchanged; the Files app's zip picker is reused, not copied.
