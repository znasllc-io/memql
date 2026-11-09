---
title: Assembling release candidates
audience: public
status: stable
area: operate
sinceVersion: 0.25.0
owner: znas
---

# Assembling release candidates

An owner can assemble an immutable release candidate from completed Workbench
pipeline runs. The installed `releaseAssembleCandidateWorkflow` resolves the
declared inputs and composes the candidate in MemQL. Preparation then verifies
the complete evidence, source versions, destinations and retained artifact
bytes. A ready candidate requires separate owner approval and publication.

To build before publishing a GitHub release, call
`pipelinesRunRelease(pipelineId: "…", tag: "v1.2.3")` for an existing tag of
your pipeline. Automatic delivery can stay disconnected for this explicit
request, so a bootstrap does not subscribe to unrelated changes. MemQL resolves and pins the tag's commit through your
source connection, runs the full release stages, and supplies the tag as
`MEMQL_VERSION`. The operation creates no tag or public release and grants no
publication approval. Retrying, or receiving a later release webhook, recovers
the same attempt. Use `pipelinesRerun` to repeat a completed attempt. A different
tag for a commit already recorded under another release version is refused.
In a monorepo, components released under separate tags therefore need distinct
verified commits. For example, publish the engine release first, then merge a
separate Cockpit version change and release its later commit. Keep each tag
pinned to its reviewed source; a rerun cannot change the recorded version.

## Reuse a release profile

An operator can opt in to `MEMQL_RELEASE_PROFILES` instead of adding a new
assembly and destination configuration for each version. This separate variable
does not alter `MEMQL_RELEASE_CANDIDATES` or its existing candidate identities.
A profile fixes the source repositories, version files, run aliases, publication
connections and credential references. Its installed DSL resolver selects the
artifacts and composes versioned names from native facts.

```json
{
  "formatVersion": 1,
  "profiles": [{
    "name": "engine",
    "resolver": "productReleaseProfile",
    "components": [{
      "component": "engine", "repository": "acme/engine",
      "path": "VERSION", "runs": ["release"]
    }],
    "versionFacts": [{
      "name": "sdk", "component": "engine",
      "path": "sdk/package.json", "jsonField": "version"
    }],
    "registries": [{
      "id": "engine-images", "component": "engine", "artifact": "bff",
      "origin": "https://registry.example.com", "repository": "acme/bff"
    }]
  }]
}
```

The resolver is an installed `@template` automation with `args { facts object! }`.
It has no native operations, network access or credential values. `facts`
contains `profileName`, source `components`, declared auxiliary `versions`, and
safe destination identities in `targets`. Each component has its exact
`repository`, `commit`, `version`, `releaseTag` and native `runs`. A run records
its `alias`, `workRunId`, `pipelineId`, `pipelineRunId`, `event`, `mode`, `status`,
`releaseTag` and `definitionDigest`. The engine reads completed successful
pipeline/work associations under their original owner; the product resolver
requires the intended event, full mode and component tag convention.

The resolver returns `components` with the ordinary artifact declarations below,
optional `compatibility`, and `publications` selecting configured `targetId`s.
For OCI, an optional `tag` must equal the exact component version, with or without
`v`; omission publishes by digest. For files, the profile's `releaseAssets` fix
the existing API/upload/download origins, repository, credential authority and
`tagPrefix`. The resolver supplies only `assetName` and `draft` metadata. Native
materialization binds the actual pipeline tag and source commit, requiring that
tag to equal `tagPrefix` plus the source version. It never accepts a resolver's
connection, credential, source SHA or release ID.

Call `releaseCandidateProfiles()` to review the profile's source inputs and
`configuration` digest. Then call
`releaseAssembleProfileCandidate(profileName: "engine", runs: {release: "…"},
expectedConfiguration: "sha256:…")`. The returned candidate follows the same
separate approval, publication and signed catalog process as a legacy assembly.
`releaseCandidateTargets(candidateId: "sha256:…")` returns its currently
authorized, candidate-bound destinations. No global configuration append is
needed for another version.

Before artifact retention, native storage records the exact owner, source facts,
installed resolver snapshot, selected artifact paths and fixed destination
bindings. Another replica resumes that selection; a retry cannot silently
rerun changed product policy. New effects still require the original owner and
unchanged current profile authority. Removing a profile or changing its source,
resolver, trust or credential reference requires fresh preparation and review.
Changing an unrelated profile does not change this candidate's identity.
Engine revisions remain part of preparation identity. Historical candidate
reads remain available when profile or catalog configuration is unavailable;
`catalog.state` reports `absent`, `sealed` or `unavailable` without claiming
unverified publication.

## Declare the assembly

Set the `MEMQL_RELEASE_CANDIDATES` global variable to a JSON object containing
version sources, publication destinations and optional named `assemblies`.
There is no default repository, destination or credential. For example:

```json
{
  "formatVersion": 1,
  "sources": [
    {"component": "engine", "repository": "acme/engine", "path": "VERSION"}
  ],
  "registries": [
    {
      "id": "engine-images", "component": "engine", "artifact": "bff",
      "origin": "https://registry.example.com", "repository": "acme/bff"
    }
  ],
  "assemblies": [
    {
      "name": "coordinated",
      "components": [
        {
          "name": "engine",
          "runs": ["engine_arm64"],
          "artifacts": [
            {
              "name": "bff", "run": "engine_arm64", "stepKey": "images.bff",
              "path": ".memql-image-build/image.oci.tar",
              "kind": "oci", "platform": "linux/arm64",
              "imageMetadataPath": ".memql-image-build/metadata.json"
            }
          ]
        }
      ],
      "compatibility": [],
      "targets": ["engine-images"]
    }
  ]
}
```

A component names one configured version source. Each run input is a unique
identifier of at most 64 characters: a letter followed by letters, digits or
underscores. Names cannot differ only by letter case. Multiple runs for one component must have exactly the same
repository and commit; they can cover different platforms or check suites.
Different components can refer to the same run through separate input names.

An artifact names its exact producing step and canonical relative path. Paths
cannot traverse directories, use backslashes or contain control characters.
OCI artifacts require `linux/amd64`, `linux/arm64` or `oci-index` and a separate metadata path
from the same successful producer. Its receipt-bound producer JSON must contain
`containerimage.digest`; preparation independently verifies that digest against
the complete OCI archive. File artifacts use `kind: "file"`, an explicit
platform and no image metadata path.

`oci-index` preserves a flat OCI index instead of choosing the publishing host's
architecture. Preparation verifies every referenced image, compressed layer,
root-filesystem digest and attached provenance document within the same archive
and byte limits. It rejects missing children, ambiguous platform matches,
external descriptors and unbound provenance subjects. Publication reads back
every child manifest and blob as well as the root index. This verifies content
identity and subject association, not an attestation's claims or signature.
When a producer deliberately selects a platform subset, its derived index has a
new digest; its metadata must distinguish that result from the upstream source.

Every target must already be configured and match the declared component,
artifact and kind. A registry target consumes an OCI artifact. A `releaseAssets`
target consumes a file and binds an existing draft or a reviewed draft-creation
intent, tag and source commit using
the [verified release asset contract](../build/verified-release-assets.md).
Assembly does not create a draft, tag or destination.

An `npmRegistries` target publishes a retained npm archive through the same
candidate approval and publication journal:

```json
{
  "id": "sdk-npm", "component": "sdk", "artifact": "archive",
  "origin": "https://npm.pkg.github.com",
  "packageName": "@acme/sdk", "version": "1.2.3", "distTag": "latest",
  "credentialSecret": "SDK_NPM_PUBLISH_TOKEN"
}
```

Declare `sdk` in `sources`, for example with `path: "sdk/ts/package.json"`
and `jsonField: "version"`. Its assembly artifact uses `kind: "file"` and
`platform: "npm"`. The configured package version must equal the component's
source version. The archive's own `package/package.json` must name that exact
package and version. Preparation and approval retain the normal complete-run
requirements; selecting a package does not narrow the qualification evidence.

Publication accepts at most 64 MiB compressed and 256 MiB expanded. It verifies
the original Library receipt, checks the archive structure, and publishes
without executing package scripts or consulting `.npmrc`. Registry credentials
stay in the native publisher. HTTPS is required; additional download origins,
when necessary, must be explicitly configured and never receive the registry
credential. GitHub's npm registry requires an appropriate publishing token;
the pipeline's GitHub App installation token is not substituted automatically.

The package name, version, registry, dist-tag, credential reference and download
trust are bound into approval. A durable version reservation prevents another
candidate from claiming the same package version across replicas. An existing
version succeeds only after its downloaded bytes match the approved digest;
recovery does not move dist-tags. A new publication selects the declared
dist-tag, then reads the archive back before recording completion. Conflicts
never overwrite or delete a version. An uncertain response retains its intent
and artifacts for explicit reconciliation on the same destination. Successful
publication can enter the signed catalog with an `npm` location naming the
exact package and version; no GitHub draft or promotion is involved.

Compatibility entries name two declared components and an inclusive minimum
and exclusive maximum version:

```json
{"component": "client", "requires": "engine", "minVersion": "0.25.0", "maxExclusive": "0.26.0"}
```

Configuration is bounded by 1 MiB and 64 plans. Each assembly permits at most
64 components, 64 run inputs per component, 256 inputs total, 128 artifacts per
component, 256 compatibility rules and 1,024 targets. Complete execution
evidence may contain at most 1,024 steps. Unknown or duplicate JSON fields and
inconsistent references refuse before preparation.

## Assemble and review

In MemQL OS, open **Pipelines → Releases** and select **Prepare release**. Choose
a configured plan, then one successful full-mode run for each declared input.
The picker walks older pages when needed and shows only runs from the declared
repository. Multiple inputs for one component must use distinct work runs at
the same commit. Review the inputs, artifacts and destinations, then prepare.
The candidate page shows the resolved versions and verification evidence for
separate approval. A changed plan requires choosing and reviewing it again.

Pass only the configured plan name and its run identities. A reusable recipe
can accept the run identity as an argument:

```memql
@template
automation prepareCoordinatedRelease {
  args {
    runId string!
  }
  builtin releaseAssembleCandidate(
    planName: "coordinated",
    runs: { engine_arm64: args.runId }
  )
}
```

`releaseCandidateConfiguration()` exposes declared plans, source rules and
target identities without credential values. `releaseAssembleCandidate` also
has generated Go and TypeScript SDK methods. Candidate operations require the
owner role, and execution evidence must belong to that owner.

The caller cannot supply versions, success flags, receipt IDs, digests or
download URLs. Native readers resolve them from the exact source commit,
complete execution journal and original artifact upload receipts. The DSL
orders these reads and constructs the candidate. Preparation requires that
projection to preserve every declared input and resolved fact.

The installed preparation policy accepts only full-mode runs. It includes
every declared step, including notifications and explicit empty selections;
callers cannot select just the successful checks. Unfinished, failed,
foreign-owned or incompatible-source runs refuse. Every evidence artifact is
pinned and verified, including reports and metadata not selected for publication.

### Reused steps

A release may have skipped an artifact-free check because the run that tested
the same git tree passed it ([What a release reuses](release-cutting.md#what-a-release-reuses)).
Its evidence stays the release's own step, a `pipeline_passed_earlier` skip, with
the source recorded beside it:

```json
{
  "name": "engine-release-tests.go-tests-1-…",
  "stepKey": "tests.go-tests#1",
  "reusedFrom": {
    "pipelineRunId": "…", "workRunId": "…", "attempt": 1, "event": "pull_request",
    "commit": "…", "treeHash": "…", "receiptDigest": "sha256:…"
  }
}
```

The caller cannot assert this either. Coverage re-derives it from the journal on
every read, at preparation, publication and overlay alike, and refuses a candidate
whose record is missing, changed or claimed for a step that ran. It requires:

- the release ran in full mode;
- the source is a completed, successful run of the same owner, pipeline and
  repository, was not asked to stop and is not a scheduled scan;
- the source finished no more than 24 hours before the attempt that reused the step
  started;
- the source's journal names that run, its commit and the same tree;
- the source's step is a command that ran, exit zero, with no artifact intents,
  inside a work run that succeeded;
- both rows declare the same `treeFingerprint` and packages.

A failed-only re-run that carried a reused step is proven the same way: the walk
back through its attempts stops at the attempt that reused it, and that attempt's
start bounds the age. Every artifact in the candidate still comes from the run it
names, and a reused step can name none.

A live candidate keeps three kinds of run past
`MEMQL_PIPELINES_RUN_RETENTION_DAYS`, so it stays provable when an overlay is built
from it later:

- **Every run its evidence names**, whatever started it. A separate check suite on a
  push has no artifact to keep it, and coverage reads it on every read. Only the runs the
  candidate names are kept: another push of the same commit leaves on its usual window.
- **The runs it reused a step from.** With a shorter window, prepare the candidate
  before the source's rows leave, or preparation refuses it.
- **Every attempt of its release.** A candidate from a failed-only re-run names one
  attempt, and coverage reads every earlier one on every read, including an attempt
  refused before it opened any work. Retention keeps every release run at the repository
  and commit of a component the candidate names, which can include another pipeline's
  release of the same commit.

A retired candidate holds nothing, and neither does an archived one: archival gives up
its build inputs, so its own run's rows leave on their usual window.

The holds on named runs and on attempts read only what a candidate's manifest has always
named, so a candidate saved before them needs no change and is held from the next nightly
pass. They cannot bring back rows that have already left. A candidate that lost a run that
way is still refused, naming the check or attempt it could not read or saying "release run
is unavailable", so a new overlay needs a candidate from a later release.

**Rolling back to an engine from before tree hashes:**

- A candidate with `reusedFrom` is refused, because the older engine drops the field
  and the candidate's identity no longer matches.
- A candidate that reused nothing still verifies, because a receipt's digest leaves
  the input's tree out.
- A `v1:pipelines:run` row that holds a `treeHash` cannot be written by the older
  engine, so a run in flight at the rollback stops being driven. Re-run it.

A new candidate is `ready`, with no approval ID or publication effect. Repeating
the same inputs recovers the existing candidate and its current review state.
`releaseGetCandidate` and `releaseCandidates` expose its durable review record.
`releasePrepareCandidate` remains available for callers that already hold a
complete candidate manifest.

Preparation binds the installed assembly, preparation and publication recipes,
all configured assembly plans, selected source rules and the engine revision
into the candidate identity. Changed assembly configuration requires a new
review even if product bytes are identical. Another engine can assemble the
same inputs and recover the same durable candidate.

Approve with `releaseApproveCandidate` after review, then publish an exact
approved destination with `releasePublishCandidate`. Each repeats verification.
`releaseCandidatePublications` reports persisted pending or complete effects;
uncertain publication must reconcile its existing intent and retained bytes.
Assembly does not install an update, promote a draft or retire GitHub Actions.

An OCI registry target may declare `tag`, for example `"v0.29.7"`. It must
equal the resolved component version, optionally prefixed with `v`; floating
names such as `latest` refuse. The tag is part of the destination approval and
is permanently reserved for that publication intent before a registry write.
Another candidate cannot claim the same registry/repository/tag, even from
another replica. A conflicting existing registry tag refuses, and completion
requires both full digest readback and exact tag readback. Omit `tag` for
digest-only publication. Keep competing publisher credentials disabled or use
registry tag immutability; deployment consumers should continue pinning digests.

Related: [Engine, integration and workflow boundaries](../build/integration-boundary.md),
[Pipelines](pipelines.md) and [Verified OCI publication](../build/verified-oci-publication.md).

## Create a draft and publish its complete contents

The Releases screen keeps these actions separate: approve the candidate,
create or verify its draft, upload each file, and publish the release. The
review includes the release name, notes, tag source, prerelease flag and latest
selection. Draft creation also creates the tag, which may trigger repository
automation. Public publication is offered only after every candidate
destination has a complete receipt. Existing draft targets without reviewed
metadata retain their upload action but cannot be promoted from the screen.

After a lost response, reopen the candidate and use its explicit reconciliation
action. Opening the page never retries a write. If current history or
configuration cannot be read, actions wait while the last record remains
visible. Recorded completion describes the earlier verified outcome, not a
fresh remote availability check.

A file target may declare `releaseId: 0` together with exact `draft` metadata.
Approval then covers the immutable source, tag, notes, connection settings and
artifact set before a remote release exists. An entry in `releaseAssets`:

```json
{
  "id": "cockpit-linux", "component": "cockpit", "artifact": "linux",
  "apiOrigin": "https://api.github.com", "uploadOrigin": "https://uploads.github.com",
  "downloadOrigins": ["https://release-assets.githubusercontent.com"],
  "repository": "acme/cockpit", "tag": "v0.25.0",
  "sourceCommit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "releaseId": 0, "assetName": "cockpit-linux.tar.gz",
  "credentialSecret": "COCKPIT_RELEASE_TOKEN",
  "draft": {"name": "Cockpit 0.25.0", "body": "Reviewed release notes.", "prerelease": false, "latest": false}
}
```

The source commit is illustrative; use the exact commit proved by the completed
runs. Targets sharing one release must agree on source, tag, metadata, trust
and credential reference, and name distinct assets. One release permits 1–64
assets totaling at most 8 GiB; each file remains smaller than 2 GiB. Preparation
rejects inconsistent groups before approval.

After reviewing and approving the candidate, invoke
`releaseCreateCandidateDraft(candidateId, approvalId, targetId)` with named
arguments. The target identifies its entire release group. The installed
`releaseCreateDraftWorkflow` ensures the immutable tag, observes the marked
draft, claims the create attempt and records verified readback. The returned
ID stays in the native journal; operators do not copy it into global
configuration. Draft notes gain one hidden intent comment for recovery.

Upload each destination with `releasePublishCandidate`. Once every destination
in the candidate has a verified complete publication receipt, separately invoke
`releasePromoteCandidateDraft(candidateId, approvalId, targetId)`. Its recipe
checks completion, observes every expected asset's full bytes, claims promotion,
changes visibility and records another verified readback of every byte. The tag,
name, notes, prerelease status, exact asset inventory and asset IDs must agree.
Between the claim and the change of visibility it reads no bytes: it reads the
inventory, the release and its tag, and each asset must still have the ID whose
bytes it read. The reviewed `draft.latest` flag selects whether promotion should
also make this the latest release. It defaults to false, is incompatible with
prereleases, and when true requires the latest-release endpoint to read back this
exact ID.
See the [GitHub release API contract](https://docs.github.com/en/rest/releases/releases).
A saved release does all of this in one call: see
[Publish a whole release in one call](#publish-a-whole-release-in-one-call).

An existing positive `releaseId` still supports uploads. To promote that draft
through this API, declare its exact `draft` metadata before candidate review.
Its body has no MemQL creation marker added. Neither operation changes metadata
to make a conflicting existing release fit the candidate.

`releaseCandidateDrafts(candidateId)` reads durable history without credentials,
remote effects or current configuration:

- `prepared`: no draft POST was claimed.
- `creating`: a creation attempt has been fenced and may have run.
- `ready`: includes the verified release ID.
- `promoting`: publication may have run.
- `published`: includes the historical verified receipt.

An entry does not guarantee that a remote administrator has left the release
unchanged. Retrying the same explicit action on another node reconciles its
durable identity. Creation uses bounded authenticated release inventory and an
exact intent marker; promotion uses the bound release ID and full asset set.
Neither repeats a POST/PATCH merely because the prior response was lost or the
effect is not yet visible. Creation re-reads the inventory for up to 15 seconds
after its one POST before calling the outcome uncertain, because a new draft can
be missing from the list for a moment. Uncertainty retains the candidate and
artifacts and prevents retirement. If readback cannot prove completion, resolve the remote
state explicitly; there is no automatic fence reset. A crash between claiming
and sending an effect is also uncertain.

An owner can call `releaseArchiveCandidate(candidateId)` to release a published
candidate's duplicate build inputs. Every destination must have a complete
native publication receipt and the candidate must have a signed catalog. The
operation freshly reads and verifies the canonical registry images, release
assets or npm archive before recording a permanent `archived` state. Missing
or changed bytes, unfinished publication and an inconsistent catalog refuse
archival. Historical GitHub releases need not remain selected as latest, and a
release's name and notes may be edited once it is public, as a deployment hold
does: its identity, tag and bytes are what archival reads.

Archival is not a new effect on the release, so a candidate from a release profile
does not need the engine revision or resolver that prepared it. It could not have
them: the deployment a release exists for replaces that engine, normally well before
the 48 hours below. It still reads the destinations the candidate was approved with,
so it refuses when the profile is gone or a destination no longer matches its reviewed
digest, and the inputs stay retained.

Archival releases only that candidate's Library retention references. Other
candidate references and deliberately kept Library files still protect shared
inputs. Ordinary pipeline retention removes eligible duplicate files later;
published downloads, tags, images, approvals, signed catalogs and compact
publication receipts remain. An interrupted reference release resumes on a
different node, but an archived candidate can never prepare or publish again.
Archival does not prune canonical release versions or select which versions
an installation needs for rollback.

Owners can enable automatic duplicate-input archival with
`releaseSetInputArchiveSchedule(enabled: true)`. The installed
`releasePublishedInputRetentionPolicy` waits 48 hours after catalog sealing.
An agent schedule admits at most one candidate per pass, using a durable lease
and cursor shared by all replicas. It includes existing and future sealed
candidates belonging to the enrolled owner. An interrupted pass resumes, and
completed input releases leave the scan without deleting publication history.

Enrollment captures the caller's authority ceiling, never a credential. Each
pass resolves the owner's current role and preserves restricted-grant expiry;
revocation or expiry leaves the inputs retained. Disable with
`releaseSetInputArchiveSchedule(enabled: false)` to prevent another pass. An
already admitted pass may finish. This enrollment authorizes duplicate-input
archival only; it cannot remove canonical release downloads or registry images.

An upload lease and pending publication receipts coordinate this system's uploads
and promotion across replicas. GitHub has no atomic operation binding draft
visibility, its tag and all assets together. Before/after checks detect observed
external drift; they cannot exclude concurrent remote administrators. Promotion reads an
asset's bytes before its claim and after its PATCH, and in between the listing stands for
them: the asset's ID and the digest GitHub reports. GitHub's
[asset API](https://docs.github.com/en/rest/releases/assets) can rename an asset but
offers no way to change its bytes, and replacing one means deleting it and uploading
another. A replacement the listing does not show yet, or bytes changed under an unchanged
ID and digest, would show only in the read after the PATCH, with the release already
public. The only delete is of an asset GitHub reports as incomplete, under an approved
name, on the bound draft. Latest selection follows only the reviewed flag.

The candidate identity includes both lifecycle recipes whenever reviewed draft
metadata is configured. Changed engine or workflow identity requires fresh
review rather than executing changed code under an old approval.

Since 0.29.36, an approved file-only release can resume an existing ready draft
after an engine or workflow upgrade. Prepare and approve a new candidate, then
create or verify its draft. All sources, versions,
artifacts, evidence and destinations must remain identical. The operation keeps
the original remote release ID and recovery marker, carries forward completed
publication receipts, and permanently fences the previous candidate from new
effects. Both approvals and the original receipts remain in the native journal.
Promotion still verifies every remote asset again before publishing.

Renewal refuses pending uploads, uncertain creation or promotion, already
published releases, and candidates containing registry publications. Reconcile
unfinished effects under their original authority before renewing. Uploading or
promoting alone never transfers authority to a different candidate.

### Archive a candidate that has no catalog

Archival needs a signed catalog, and some approved candidates can never have
one: a candidate a renewal replaced (its draft moved to the successor, and a
superseded candidate cannot be sealed), one whose publication stopped before it
completed, and one published before catalogs were configured. Retirement refuses
them too, because they have publication intents. Their build inputs would stay
retained for good, so an owner can call
`releaseArchiveWithoutCatalog(candidateId)`, which archives one when three
things hold:

- **A catalogued release supersedes it.** Every component the candidate contains
  has another candidate of the same owner, at the same or a higher version, with
  a catalog that verifies under the configured publisher trust. A renewal's
  successor counts. The newest release of a component never qualifies, so this
  cannot stand in for sealing a current release.
- **What it published is read back first.** Every destination whose publication
  completed is verified as archival verifies it: registry images by their content,
  npm archives by digest and size, and release assets by their bytes if their
  release was published (on the successor's release for a renewed candidate). The
  destinations are resolved through the candidate's release profile or the
  current configuration only when there is such a copy to read. Then, as in
  archival, a profile or configuration that is gone refuses, and so does a
  destination read that no longer matches its approved digest; a profile must
  also still authorize every destination it selected, read or not. A candidate
  with nothing to read needs neither.
- **Nothing it began is still running.** No upload is in progress, and no pending
  publication or draft creation or promotion has changed in the last two hours,
  longer than any publication operation runs. Resuming a publication restarts
  that period, and for a renewed candidate the successor's release, which holds
  its files, is held to the same rule.

Every declared destination is recorded with its outcome:

| Outcome | Meaning |
|---|---|
| `verified` | The publication completed and its published copy was read back. |
| `uploaded` | A file was uploaded to a release that was never published; nothing was read. |
| `pending` | The publication began and never completed. |
| `notBegun` | Nothing was attempted for this destination. |

The candidate is then permanently `archived`. Its archive evidence is these
outcomes, the verified copies and the catalogued release that superseded each
component, in place of a catalog digest. It can never prepare, publish or be
sealed again, and a publication that never completed stays pending in its
history. Nothing remote changes: registry tags and their reservations, GitHub
drafts and releases, approvals, publication receipts and catalogs stay as they
are. Only this candidate's own retention references are released, so files that
another live candidate also pins stay until that one is terminal too. A
candidate that has a catalog is refused and belongs to `releaseArchiveCandidate`.
An interrupted run leaves the archived state in place; running it again on any
replica finishes releasing the references without reading a provider. Only the
owner can call it; the 48-hour schedule never does.

### Publish a whole release in one call

**Publish release** (`releasePublishBuild`) runs the saved recipe once and finishes
the release, however many files it has: the draft, every upload, the promotion and
the signed catalog record. No per-file calls are needed. A recipe may call each
effect once per target, and the cost stays proportional to the release:

- The whole candidate is verified before the call's first effect, and uploads to the
  private draft reuse that verification. Each upload still checks the lease, the
  owner's stored role and the release profile, the journal still checks the exact
  approval, and the file's own bytes are verified again immediately before it is sent.
- Anything public verifies afresh: the promotion, and each registry or npm write.
- Once a release's draft is `ready`, draft creation for its other files answers
  from the journal. Once it is `published`, so does promotion.

Direct calls to `releaseCreateCandidateDraft`, `releasePublishCandidate` and
`releasePromoteCandidateDraft` keep their own full verification.

The call has no time limit as a whole, because no fixed limit suits both ten files
and 8 GiB. One upload may take 30 minutes and any one recipe operation 70, and a
stuck operation ends the call. The call continues if the page closes or the
connection drops; read its progress with `releaseGetBuild`. While it runs the
release reads `publishing` and does not offer **Publish release**.

It stops before its next operation if the owner loses the owner role or the release
profile changes, and a new engine version replaces the node running it. Nothing else
stops it: a request to cancel the call is acknowledged, and the publication carries
on.

Uploads to one release run one at a time. An upload from any other caller is refused
while one is running, and so are promotion and draft renewal, until it finishes or
for up to 3 minutes after the node running it died.

If a call stops, press **Publish release** again, on any replica. Completed uploads
keep their receipts and are not sent again, and neither the draft POST nor the
promotion PATCH repeats. The release reads `uncertain` with the reason. If its node
died, it reads `publishing` for up to 6 minutes, then `uncertain` with the reason
`publication_interrupted`, and offers the action again.

An upload cut off part way can leave an asset GitHub reports as `starter` or `open`.
GitHub then refuses every later upload under that name, and
[documents](https://docs.github.com/en/rest/releases/assets) that a `starter` asset
can be safely deleted. The next publish removes exactly that asset and uploads the
file again, logging the asset ID it replaced. It first checks that the release is
still the bound draft at the approved tag, and reads the asset once more in case it
finished. GitHub has no conditional delete, so that last read narrows the window
without closing it. A call never does this to its own upload, because that upload may
still be finishing.

Two stops need more than pressing again:

- **`release_asset_conflict`** names a file that is finished but is not the approved
  file, or whose state is unknown. It is left alone, because nothing can tell which
  copy is right. Put the approved file in its place, then publish again. If the file
  has no complete publication yet (`releaseCandidatePublications`), removing it is
  enough, since the next publish sends it. Once it has one, nothing sends it again, so
  removing it would leave the release unable to go public. A read that fails is not a
  conflict: it ends the call as `operation_unconfirmed`, and pressing again reads again.
  Found once promotion has started, the reason says the release may already be public.
- **`promotion_unconfirmed`** means making the release public was claimed and could
  not be confirmed. A claimed promotion never repeats its PATCH on a guess, including
  when the read just before the PATCH fails, so pressing again cannot get past it while
  the release is still a draft. If it is still a draft on GitHub, check that its tag, notes
  and files are still the approved ones, publish it there, then press **Publish release**
  again to record it. The press verifies all of that and refuses a release that differs.

A claim with no PATCH behind it is the case only the owner can clear, so little happens
between the two, and it does not grow with the release. The credential is fetched before
the claim, and a failure there leaves the draft `ready`, so pressing again works. After
the claim the call checks the lease, the owner and the release profile, then reads the
inventory, the release and its tag, usually three requests and no bytes. A read that gets
no answer repeats for about 15 seconds, longer if a request hangs. A read that shows a
change stops the promotion at once. What still leaves a claim with no PATCH sent:

- those reads getting no answer after the repeats, or showing that an asset, the tag or
  the release changed since the bytes were read. A file that differs reads
  `release_asset_conflict`, and once it is back the next press reads
  `promotion_unconfirmed`. Anything else reads `promotion_unconfirmed` directly;
- the lease, the owner's role or the release profile no longer holding;
- the node dying between the claim and the PATCH, which first reads
  `publication_interrupted`.

A PATCH GitHub refused or lost reads the same way: nothing can tell whether it landed
until the release is seen to be public.

## Publish portable catalog evidence

After publication, the owner can call
`releaseSealPublishedCandidate(candidateId, approvalId)`. This local operation
requires complete native receipts for every approved destination. Each file
release must also have a verified `published` lifecycle record; uploading to a
draft is insufficient. Every artifact in the candidate must have a published
location. Sealing performs no remote writes.

The record includes component names, versions, exact source commits,
compatibility requirements, artifact sizes and digests, and frozen publication
locations. OCI locations name a registry and repository, used with the image
digest. File locations name the API origin, repository, release ID and asset
ID, plus the reviewed tag and asset name. A consumer still verifies downloaded
bytes and applies its own connection and credential policy. Locations do not
grant permission to forward credentials or follow arbitrary redirects.

Publication freezes the approved target configuration in the private native
journal. Catalog creation reads that snapshot rather than current settings.
The exported projection omits the owner, work runs, Library references,
credential references and transport trust. An opaque candidate identity and
provenance digest bind the private evidence; the release signature attests to
its native verification. The private evidence remains available to the owner.

Configure `MEMQL_RELEASE_CATALOG` as a global variable:

```json
{
  "formatVersion": 1,
  "publisher": "my-publisher",
  "keyId": "release-2026",
  "signingKeySecret": "RELEASE_SIGNING_KEY",
  "publicKeys": {
    "release-2026": "BASE64_ED25519_PUBLIC_KEY"
  }
}
```

The referenced global secret contains a base64-encoded 32-byte Ed25519 seed.
Use a dedicated release key, separate from identity signing. `keyId` and
`signingKeySecret` may both be omitted on a reader. Public keys are base64
32-byte Ed25519 keys. Retain trusted old keys while their release records must
remain readable; removing a key refuses its signatures. An existing catalog
record is immutable and is not silently signed again during key rotation.

Identified developers, admins and owners can call
`releaseListPublishedCandidates(cursor, limit)` (limit 1–20) and
`releaseGetPublishedCandidate(candidateId)`. The latter returns the verified
public projection, its catalog digest and a portable signed envelope. These
reads need no publication credentials, retained artifact store or access to
private candidate records. They do not grant publication authority.

The envelope uses [DSSE](https://github.com/secure-systems-lab/dsse/blob/master/protocol.md)
with Ed25519 and payload type
`application/vnd.memql.published-release.v1+json`. A receiving installation
must pin its accepted publisher and public keys independently; keys delivered
with an envelope cannot establish trust. The verifier authenticates exact
payload bytes, rejects unknown versions, ambiguous JSON and oversized inputs,
and returns an immutable verified value. The Go contract is
`pipelines.VerifyPublishedRelease`; native same-installation readers can use
`release.PublishedCatalog` with the same trust policy.

This is a detached record produced after publication, outside the candidate's
own artifact set, so its digest cannot create a self-reference. It is a
historical attestation, not a claim that a release is currently newest or that
remote assets remain available. Update selection, rollback authorization and
fresh artifact/installation checks remain separate actions. This operation
does not automatically host the envelope on an external service.

## Discover releases from another installation

On the receiving installation, set the `MEMQL_RELEASE_SOURCES` global variable:

```json
{
  "formatVersion": 1,
  "sources": [
    {
      "id": "upstream",
      "publisher": "my-publisher",
      "endpoint": "https://api.publisher.example.com",
      "credentialSecret": "PUBLISHER_READ_TOKEN",
      "publicKeys": {"release-2026": "BASE64_ED25519_PUBLIC_KEY"}
    }
  ]
}
```

`endpoint` is the publisher's authenticated gRPC TLS front door. It has no
catalog path, query or embedded credentials. The referenced global secret
contains a bearer token for an identified developer, admin or owner on the
publisher. Expired credentials must be replaced by the operator. An optional
`rootCaPem` supplies the CA certificates trusted for that endpoint; omitting it
uses system roots. Certificate hostname verification remains enabled. The
publisher's release signing keys are separate from its TLS trust and credentials.
Obtain those keys through a trusted operator channel, independently of the
catalog response.

Source and publisher names, and signing key IDs, are lowercase identifiers of
at most 128 characters using letters, digits, dots, underscores and hyphens;
the first character is a letter. Secret names use uppercase letters, digits
and underscores. Configuration permits at most 16 sources, 32 keys per source
and 256 KiB total. Unknown or duplicate JSON fields refuse. An unset variable or
an explicit empty `sources` array configures no publishers. Other read failures remain
errors rather than appearing as an empty list.

In **Cluster → Updates → Published releases**, an identified developer, admin or owner can choose a
configured publisher, browse published releases, and inspect an exact release.
**Check again** requests a fresh read. Opening or reconnecting a visible page
also refreshes it; the screen does not continuously poll remote publishers.
Failures and disconnections keep the last displayed record without calling it
freshly verified. Discovery has no installation or publication effect; installation
selection lives in the separate **This installation** tab.

The same reads are available as DSL builtins and generated Go/TypeScript SDK
methods:

| Construct | Result |
|---|---|
| `releaseSources()` | Configured source IDs and publisher names, without endpoints or credentials |
| `releaseDiscoverPublishedCandidates(sourceId, cursor, limit)` | One page of independently verified release summaries, with an opaque next cursor |
| `releaseReadDiscoveredCandidate(sourceId, candidateId, catalogDigest)` | Reverified public projection of that exact selection |

The discovery limit is 1–10, default 10. A call lasts at most 30 seconds and
reads only one page. The unsigned remote list supplies candidate locators;
every displayed component and version comes from the signed record verified
against the receiving installation's configured publisher and keys. Malformed
or unverifiable entries fail the page without returning partial results. No
credential values, remote private diagnostics or publisher configuration are
returned to the caller.

Each call resolves current configuration and credentials. Selecting a release
does not cache trust or authorize installation. A native installation preparer
must independently call `releasecatalog.Reader.Get` with the source ID,
candidate ID and catalog digest; it receives an opaque
`pipelines.VerifiedPublishedRelease`. A serialized projection or `verified`
flag from a client cannot create that value. Another receiving replica can
reverify the selection without the discovering replica's local state.

Discovery verifies the publisher's attestation. It does not prove that a
release is newest, that remote artifacts are still available, or that the
release is compatible with the receiving cluster. DSL policy owns update
selection, cadence, retries and the installation workflow; native preparation
owns fresh artifact and cluster evidence, effect fencing and rollback authority.
This boundary also applies when an automation calls the discovery builtins.

Installation preparation tolerates routine Argo Application status refreshes
while retaining checks for real configuration changes, including an edit later
restored to its original value. The receiver keeps the checked history with the
preparation so another replica can resume it. If required Kubernetes history is
no longer available after a pause, cancel the unfinished preparation and prepare
again. A started installation must be reconciled from its saved intent; missing
history never authorizes replacing an uncertain operation.

The receiving configuration an update saves at preparation records what each
input means, not when it was last written, so routine cluster activity between
preparation, Apply, **Check progress** and **Restore previous release** no
longer stops an update:

- Argo CD's repository server restarting (with `kubectl rollout restart` or
  Argo CD's restart action), being rescheduled or briefly becoming unready.
  Every step instead checks that each serving pod belongs to the configured
  Deployment's current revision, runs its template, image, service account and
  network settings with no debugging container attached, and is ready. A
  debugging container stays in its pod until the pod is replaced, so restart
  the repository server after debugging it.
- Its TLS certificate renewed under the same certificate authority, before or
  after the repository server restarts to load it, when the repository server
  reads that certificate's Secret only as its serving certificate, as Argo
  CD's standard manifests do. A server that also mounts or reads that Secret
  another way has every key of it checked, so a renewal stops the update.
- A Kubernetes patch release, a different API server address, or a custom
  resource definition installed or upgraded for something else, as long as
  every API group's discovery still answers: one that does not still stops each
  step until it does.
- A write that changes only the labels or annotations of a ConfigMap or Secret
  the update reads, such as a credential's expiry annotation. The one exception
  is the `app.kubernetes.io/part-of` label of Argo CD's `argocd-cm`, which Argo
  CD needs in order to read it.

A real change still refuses the update: a rotated password or token, edited
configuration data, a repository server running another template or image, or
restarted by a tool that changes its template, a certificate from another
authority, a Kubernetes minor upgrade, or a removed or rescoped API that the
installation reads or that the release applies.

Every step also checks that the certificate authority it trusts is
self-signed: the Secret's `ca.crt`, or the last certificate of its `tls.crt`
when it has no `ca.crt`, must be a root certificate authority or a self-signed
certificate. An intermediate there stops every step until the Secret names its
root.

A self-signed repository server certificate is its own authority, so **a
self-signed certificate whose key changes at renewal is a new authority and
still stops every update started before the renewal**; such an update must be
closed. cert-manager changes the key at each renewal by default. A self-signed
certificate survives renewal only when cert-manager keeps its private key
(`privateKey.rotationPolicy: Never`), and even then a step that renders refuses
until the repository server restarts to load the renewed certificate. Issue it
from a CA issuer to renew it freely.

A release that would change the installation's own update configuration, the
ConfigMap that holds `installation.json`, is refused at preparation: every later
step is checked against that configuration, so the update could never be
verified. The preparation still holds the installation, so cancel it, deploy
that change by hand, then prepare the update again. Changing only that
ConfigMap's labels or annotations is allowed.

An update prepared by engine 0.31.28 or earlier keeps that engine's stricter
checks. If one of the routine changes above happens while it is started, it can
no longer be verified, and its operator ends it with **Close without
verification**. Engines released before this change cannot verify an update
prepared with these checks, so an update that installs such an engine is closed
rather than verified.

Protected-resource evidence likewise tolerates routine controller updates to
the owner of a protected Secret, such as an External Secrets refresh rewriting
its ExternalSecret's status every hour. An owner's fingerprint covers its
identity and UID, what it declares, its owner references and finalizers, and
its Argo prune and comparison options, but not its status, `resourceVersion`,
`managedFields`, generation, labels or other annotations. A change to any
fingerprinted field between preparation and Apply, completion or restoration
still refuses the plan.

A started update whose saved protected-resource evidence can no longer match
can be neither verified nor restored. **Check progress** and **Restore previous
release** both compare fresh evidence with the evidence saved at preparation,
and cancellation applies only to an update that has not started. That happens
when a controller updates a protected owner after an earlier engine, which
fingerprinted whole owners, prepared the update, or when a fingerprinted field
of a protected owner changes outside the installation after the update starts.
The operator who started such an update ends it with **Close without
verification**, described in
[Close an update that cannot be verified](#close-an-update-that-cannot-be-verified).


## Restore a selected installation

In **Cluster → Updates → This installation**, **Restore previous release**
selects the exact release recorded before the update. The original operator
must confirm this action separately. It is available for a started update, or
the latest completed update while no later preparation or plan has advanced
this installation. Another operator cannot take over the selection by holding
an installation role.

Restoration runs the plan's retained rollback recipe with fresh native checks.
It refuses pending or foreign controller operations, changed migrations,
unverifiable original access, conflicting application ownership, and missing
artifact or preservation evidence. It never downgrades the database. The
original forward completion receipt remains in history.

Restoring a completed update makes every one of those checks before it takes
the installation back. When one refuses, the update stays completed and
verified, the installation stays free for another preparation, and **Restore
previous release** can be tried again once the cause is fixed. Each
application's change is then preceded by a recorded start, taken only after
that application's own checks pass. A restoration refused before its first
recorded start also returns the update to its completed state, and the refusal
says so. Once a restoration has recorded a start for any application it holds
the installation until it is resumed and verified.

Routine events no longer stop an update or a restoration:

- Syncing an application again by hand at the same revision after the
  update's or restoration's own sync succeeded. Argo CD keeps only its latest
  sync, so MemQL records each success it sees: at **Check progress**, and
  whenever **Resume restoration** reads the applications. A sync to another
  revision, or a change to the application, still stops verification. For an
  update being applied, a sync made before MemQL saw the update's own success
  leaves that success unproven; close the update. A restoration in that
  position is verified from Argo CD's deployment history instead: the
  application's newest deployment must be the restored revision and source,
  deployed no earlier than the restoration's start.
- Argo CD refreshing an application's status between a restoration's planning
  and its change. Any other change to the application in that time still
  refuses the restoration.
- Resuming a restoration after Kubernetes stopped keeping the application's
  change history from when it was planned, usually a few minutes on a busy
  cluster. An application the restoration has not yet changed continues, and is
  planned again from its current state when Argo CD has written to it since,
  after every check runs again. The same applies to a restoration started by
  engine 0.31.28 or earlier whose change was refused because Argo CD refreshed
  the application's status first: MemQL plans that change again once the
  application shows it can never be applied.

**Resume restoration** continues an interrupted recorded reversal. **Check
progress** observes it and verifies completion; it cannot apply missing steps.
A lost reply does not trigger another write automatically. Read the saved plan
before explicitly continuing. An installation with several applications shows
their individual progress and reports differing revisions without inventing a
shared installed version. Controller synchronization alone is not completion.

The screen says **The previous release is restored and verified** only after a
native `rolled_back` receipt and matching current revision. That receipt checks
the previous workloads and images, unchanged migrations, original access,
storage and protected resources. Other release choices become selectable again
after the active operation finishes.

The same operation is available through the existing MemQL stream and generated
Go/TypeScript SDKs:

| Builtin | Effect |
|---|---|
| `releaseInstallationStatus()` | Read current member revisions, saved preparation or plan, and current `canRollback` eligibility |
| `releaseRollbackInstallation(installationId, planId)` | Explicitly start or resume restoration of that exact plan's previous release |
| `releaseObserveInstallation(installationId, planId)` | Observe the saved direction and record completion only after native verification |

`canRollback` means the original operator may request fresh verification. It
is not a promise that restoration is safe, and cannot authorize an effect.
An in-progress restoration is reported as `state: "applying"` with
`direction: "rollback"`; completion is `state: "rolled_back"`. Callers must
not interpret the original candidate's identity as the restored release.

## Close an update that cannot be verified

In **Cluster → Updates → This installation**, **Close without verification**
ends a started update that can no longer finish, such as one whose saved
protected-resource evidence can no longer match. Only the operator who started
the update can close it, only while it is being applied, and they must give a
reason.

Closing changes no application and restores nothing. MemQL records who closed
the update, when and why, and releases the installation so another release can
be prepared. It writes no completion receipt: the screen says **This update was
closed without verification**, and the update can no longer be restored from
there. Confirm the installation's workloads by other means before relying on the
release the update applied.

The close is refused while the update's own Argo CD sync is still queued or
running, including a restoration's reverse sync, because that sync would then
finish with nothing tracking it. The applications need not still be at the
update's revision: a hand deploy may have moved them on, or the update's change
may never have reached them. An application that cannot be read refuses the
close.

A restoration of an update that had completed can be closed only while it has
recorded no start for any application, so nothing of it can have reached the
cluster. Its operator then sees **Stop restoration** instead: stopping it
records who stopped the restoration, when and why, and returns the update to
its completed and verified state, so the installation is free and the update
can be restored again later. Once the restoration has recorded a start, it
cannot be closed; resume it instead.

| Builtin | Effect |
|---|---|
| `releaseCloseInstallation(installationId, planId, reason)` | Close that exact started plan without completion proof, or stop a restoration of a completed update that has recorded no start. The reason is one line of at most 500 characters |

`canClose` in `releaseInstallationStatus()` means the original operator may
request a close: of an update being applied, or of a restoration
(`direction: "rollback"`) that has recorded no start. The request still
refuses while the update's own sync runs. A closed plan reads
`state: "closed"` with `closedAt` and `closeReason`, and is reported as
`latest` until a newer update finishes; a stopped restoration reads
`state: "complete"` again.

## Propose a complete installation update

An owner can bind `MEMQL_RELEASE_INSTALLATION_OVERLAY` to an installed proposal
recipe and an exact set of repository files. A proposal creates a reviewable
branch and pull request. It does not merge the change or select a production
release. Each receiving installation retains its own repository grant and
release trust; a foreign host cannot acquire an upstream write grant through
this configuration.

Formats 1 and 2 retain their existing image, Kustomize source and environment
codecs. Format 3 additionally permits two explicit data codecs:

- `jsonScalars` replaces existing scalar slots selected by absolute JSON key
  pointers. The only facts are `componentVersion`, `componentTag` (the verified
  stable version prefixed with `v`), `componentCommit`, `companionSHA256` (bare
  hexadecimal) and `companionSize` (bytes). Unknown facts, duplicate keys,
  overlapping pointers, missing slots and type mismatches refuse. Unbound
  JSON values remain unchanged; arbitrary replacement data is not accepted.
- `nativeManifest` creates one canonical evidence file at a configured path
  containing exactly one `{version}` placeholder. The approved immutable Git
  tree must prove that path is absent. Existing manifests are never replaced.

Format 3 also declares `nativeEvidence`: the selected `component`, its
`companionComponent` and `companionArtifact`, and explicit `images` bindings
(`name`, `component`, `artifact`, `repository`). The image manifest records a
single complete producing run, artifact receipts and completed publication
receipts. It does not invent execution or remote availability claims. Its file
companion must be a signed, published GitHub release asset for the same source
repository, stable version and commit.

The recipe composes `releaseOverlayRead`, `releaseOverlayNativeEvidence`,
`releaseOverlayBindPublication`, `releaseOverlayBuild`, `releaseOverlayReserve`
and `releaseOverlayPropose`. Native evidence returns bounded run facts and
eligible companion identities. The installed DSL chooses the acceptable run
modes and companion; the binding operation accepts only an exact identity and
catalog digest returned in that scope. No serialized approval or caller-supplied
fact can create authority.

This extension requires the publisher's original same-owner private journals,
including approvals and complete execution coverage. Public signatures alone
cannot reconstruct those private facts on another installation. If they are
unavailable, use a separately reviewed proposal; do not fabricate a manifest.
The chosen companion, catalog and approval identities, source tuples, exact
replacement bytes and expected absence state share one durable proposal intent.
Another replica revalidates that exact selection, never a newer companion.
Existing format 1 and 2 proposals keep their original identity and recovery
semantics. Install the matching engine capability before opting into a format 3
recipe, then qualify the repository's ordinary checks before merging its output.
