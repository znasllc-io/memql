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
OCI artifacts require `linux/amd64` or `linux/arm64` and a separate metadata path
from the same successful producer. Its receipt-bound BuildKit JSON must contain
`containerimage.digest`; preparation independently verifies that digest against
the complete OCI archive. File artifacts use `kind: "file"`, an explicit
platform and no image metadata path.

Every target must already be configured and match the declared component,
artifact and kind. A registry target consumes an OCI artifact. A `releaseAssets`
target consumes a file and binds an existing draft or a reviewed draft-creation
intent, tag and source commit using
the [verified release asset contract](../build/verified-release-assets.md).
Assembly does not create a draft, tag or destination.

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

In MemQL OS, open **Cluster → Releases** and select **Prepare release**. Choose
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
changes visibility and records another verified readback. The tag, name, notes,
prerelease status, exact asset inventory and asset IDs must agree. The reviewed
`draft.latest` flag selects whether promotion should also make this
the latest release. It defaults to false, is incompatible with prereleases, and
when true requires the latest-release endpoint to read back this exact ID.
See the [GitHub release API contract](https://docs.github.com/en/rest/releases/releases).

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
effect is not yet visible. Uncertainty retains the candidate and artifacts and
prevents retirement. If readback cannot prove completion, resolve the remote
state explicitly; there is no automatic fence reset. A crash between claiming
and sending an effect is also uncertain.

Native row locks and pending publication receipts coordinate this system's
uploads and promotion across replicas. GitHub has no atomic operation binding
draft visibility, its tag and all assets together. Before/after checks detect
observed external drift; they cannot exclude concurrent remote administrators.
No delete or overwrite is performed. Latest selection follows only the reviewed
flag.

The candidate identity includes both lifecycle recipes whenever reviewed draft
metadata is configured. Changed engine or workflow identity requires fresh
review rather than executing changed code under an old approval.

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
and 256 KiB total. Unknown or duplicate JSON fields refuse. An explicit empty
`sources` array configures no publishers. Configuration read failures remain
errors rather than appearing as an empty list.

In **Cluster → Updates**, an identified developer, admin or owner can choose a
configured publisher, browse published releases, and inspect an exact release.
**Check again** requests a fresh read. Opening or reconnecting a visible page
also refreshes it; the screen does not continuously poll remote publishers.
Failures and disconnections keep the last displayed record without calling it
freshly verified. This screen has no install or publication effect.

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
