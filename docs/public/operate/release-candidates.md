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
target consumes a file and binds an existing draft, tag and source commit using
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
