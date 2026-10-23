---
title: Disposable artifact publication qualification
audience: internal
status: stable
area: ops
sinceVersion: 0.30.9
owner: znas
---

# Disposable artifact publication qualification

The `delivery-artifacts.provider-qualification` step in
[`memql-package.yaml`](../../../memql-package.yaml) requires an explicitly
consented native Linux Docker worker. During the first delivery-engine bootstrap,
it runs its full fixed provider matrix on every ordinary pipeline run. This
preserves coverage while the installed compiler cannot read `packageRoots`.
The reviewed [follow-up](delivery-qualification.md#bootstrap-with-the-current-engine)
restores affected-component selection after the operator selects the new engine
and installs its policy; uncertainty continues to select full coverage.

Use the same capability locally, with the four digest pins declared by the
manifest:

```bash
bash scripts/ci/delivery-artifacts.sh \
  --database-image=ghcr.io/znasllc-io/ci-timescaledb@sha256:d8336a5c9cc49bdbf1e935234e8c87ab64b6d6d195fd2ecedff7b00185557631 \
  --blob-image=mcr.microsoft.com/azure-storage/azurite@sha256:647c63a91102a9d8e8000aab803436e1fc85fbb285e7ce830a82ee5d6661cf37 \
  --registry-image=docker.io/library/registry@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e \
  --artifact-image=docker.io/library/python@sha256:2d9aefe2fef018a7eb2c13064c89c71929800fd2e5dccdbf52ea5da5bb8d929a \
  --output=/tmp/memql-artifact-evidence
```

The host needs Docker, Go, Python 3.9 or later, Git, Bash and the normal engine
build prerequisites. The script uses the existing Go caches with two Go
execution threads and serial package compilation. Allow 30 minutes for cold
image pulls/builds, about 2.5 GiB of fixture memory, and enough disk for the
compiler cache and two copies of the input image. Images above 2 GiB are refused.
All ports bind only to loopback. There is no active-database, installation,
production credential, or deployment parameter.

To rehearse with an actual locally built engine image, pass its immutable Docker
repository digest as `artifact-image`. Pass a native TimescaleDB/pgvector image
digest as `database-image` when the CI database image does not support the host
architecture. The artifact must contain a `linux/arm64` image because the current
candidate fixture declares that platform. The capability exports its exact
manifest, config and layers from Docker's OCI export; it does not invent image
content or rebuild it. The platform image digest is preserved, and both image
and archive digests are recorded. The evidence also records the source checkout
commit and whether it contained uncommitted changes.

Each invocation starts its own Postgres/TimescaleDB/pgvector, official Azurite,
and official Distribution containers with random fixture-only credentials and
bounded temporary storage. It does not use an existing cluster or database.
Azurite uses the same `--skipApiVersionCheck` setting as the supported local
overlay because current SDK API-version headers can be newer than the emulator;
the blob operations, signatures, ETags and content are still actually verified.
The providers have unique names and an ownership label. Cleanup checks that label
before removal, includes anonymous volumes, and verifies disappearance on success,
test failure or cancellation. An unconfirmed cleanup makes the run fail and names
the remaining containers in its report. A host or daemon crash can prevent
cleanup; inspect the named containers and their matching
`io.memql.delivery-qualification` label before removing them. The capability does
not prune unrelated images, networks, caches, databases, or containers.

The fixed tests prove:

- Real blob stream verification, exact ETags, bounded 128 MiB transfers, lost
  response recovery, retirement fencing and late-writer refusal.
- Candidate publication to a real local OCI registry and a second-client retry.
- Actual native Library receipt storage in Postgres and blob storage, then
  candidate publication through a separate engine with no first-engine cache.
- Real account inventory before admission, fresh persisted owner identity and
  exactly one owner capacity charge after lost-response recovery/publication.

Every selected test must report a pass. Skips and absent expected test names fail
the capability, even when `go test` exits zero. Human progress goes to stderr;
stdout contains exactly one capability JSON envelope. Its evidence directory
contains the retained OCI archive, per-package Go JSON events, human test logs,
provider logs and `result.json`. The pipeline collects this directory as artifacts.

These tests use fixture Work receipts and source/version metadata. They qualify
the real provider and durable receipt paths, not release signing, provenance of
an official build, hosted GitHub permissions, or a completed installation rollout.
The installed Argo/receiver opt-in tests remain a separate controlled rehearsal;
this capability neither invokes them nor changes any running installation.

The fixture's own protocol test runs in the ordinary script test lane:

```bash
go test -p 1 ./scripts/ci -run '^TestDeliveryArtifactFixtureProtocol$' -count=1
go test -p 1 . -run '^TestDeliveryArtifactsManifest' -count=1
```
