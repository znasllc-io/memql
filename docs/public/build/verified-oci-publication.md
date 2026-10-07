---
title: Verified OCI publication
audience: public
status: stable
area: build
sinceVersion: 0.25.0
owner: znas
---

# Verified OCI publication

The native `integrations/ociregistry` package supplies a bounded registry effect.
It verifies an immutable artifact and publishes one exact image digest to one
operator-configured repository. It does not choose versions, select candidates,
approve releases, create mutable tags or deploy images. Those decisions belong
to the workflow and its authorization and execution records. The package has
no public DSL registration: a release controller must first bind its approved
candidate, destination, durable publication intent and artifact pins.

## Verification contract

`Verify` requires the receipt's archive SHA-256 and byte length, the reviewed
image digest, and `linux/amd64` or `linux/arm64`. It copies the reader into a
private bounded snapshot before interpreting any metadata. The caller supplies
an authorized, exact-version reader whose blocked reads respect cancellation.

The supported profile is one self-contained OCI image: SHA-256 blobs, ordinary
tar headers, OCI layout version 1.0.0, one image manifest, matching baseline
platform metadata, and plain or gzip layers. Each descriptor size and digest
must match its blob. Each uncompressed layer must match its root filesystem
DiffID. External or inline descriptors, auxiliary artifacts, specialized CPU
requirements, duplicate paths or metadata keys, case aliases, links, special
files, PAX/GNU extension headers and trailing nonzero archive data are refused.
Layer contents are hashed; the verifier does not execute or extract their
filesystem contents.

Native ceilings are 2 GiB per archive, 8 GiB across uncompressed layers,
4 MiB per JSON object and 8,192 archive entries. A caller may choose lower byte
limits. The private snapshot temporarily needs room for the archive and its
materialized blobs, at most twice the archive ceiling plus metadata overhead.
After validation only the private layout remains. The returned opaque handle
is the publication input; `Close` removes its files and waits for publication
using that handle to finish.

This proves content integrity and platform. It does not prove source
provenance, vulnerability clearance, approval or deployment health.

## Registry contract

`NewPublisher` accepts trusted operator configuration for an explicit registry
origin, repository and credential. There is no ambient keychain, credential
helper, proxy discovery or target selection from the archive. HTTPS is required;
an explicit setting permits HTTP only for a loopback registry fixture or
installation. An optional token exchange requires a configured exact token
endpoint and service. Requests for broader repository scopes are refused.

The protocol implementation uses pinned `go-containerregistry` v0.22.1. Every
request is checked against the configured origin, repository, verified digests
and supported upload paths before transport. Mutable manifest tags, cross-repo
mounts, unknown digests and arbitrary token realms are refused. Write, manifest
and token redirects are refused too.

Verified blob GETs may follow at most three redirects to exact HTTPS origins
in the operator's `BlobDownloadOrigins` list (at most 16). Bind the complete
sorted list into the approved target digest. Download origins are distinct from
registry and credential endpoints; no wildcard, scheme downgrade or implicit
port match is accepted. These GETs copy no authorization, cookie or referrer
headers and cannot exchange tokens or answer authentication challenges. Every
hop stays within the configured origins and every downloaded byte still has to
match the original descriptor's size and digest. Signed URLs are not included
in errors or passed through to registry response diagnostics. This supports
explicitly configured data endpoints, including the [dedicated data endpoints
used by Azure Container Registry](https://learn.microsoft.com/en-us/azure/container-registry/container-registry-dedicated-data-endpoints).

Response bodies, headers, request count,
concurrency and operation duration are bounded. Caller cancellation applies
throughout publication.

`Publish` first reads an existing image by digest. If absent, it uploads verified
content and the exact manifest, then reads back and hashes the manifest and
every referenced blob before returning a receipt. A duplicate call from a
different controller can reconcile through those registry bytes without the
original controller's local state. A lost final write reply may still produce
success when readback proves the complete image.

If a write may have happened but readback cannot prove completion, the result is
`ErrUncertain`. Keep the durable intent and artifact pins, and retry the same
candidate and target. Do not change the version, delete partially uploaded
content or mark publication successful. Registry error bodies and signed URLs
are not returned as error text because they may contain credentials.

## Local verification

The package tests include a disposable protocol registry, an independently
constructed second publisher, lost responses, incomplete uploads, corrupt
readback, credential diversion, invalid archives and cancellation. They compare
valid archives with the builder's independent Python OCI verifier. The optional
`TestDistributionRegistry` also runs against a separately started Distribution
registry using `MEMQL_OCI_TEST_REGISTRY=http://127.0.0.1:<port>`; it never loads
cloud credentials. Stop the disposable registry after testing.

Protocol references: [OCI image layout](https://github.com/opencontainers/image-spec/blob/main/image-layout.md),
[OCI Distribution specification](https://github.com/opencontainers/distribution-spec/blob/main/spec.md),
and [go-containerregistry v0.22.1](https://github.com/google/go-containerregistry/releases/tag/v0.22.1).
See also [engine, integration and workflow boundaries](integration-boundary.md)
and [streamed artifact storage](streamed-artifact-storage.md).
