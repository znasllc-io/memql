# Native fixture refresh

The TimescaleDB test fixture is a public upstream OCI index, pinned in
`timescaledb.json`. Its component version is independent of the engine:
`16.0.1` names PostgreSQL major 16 and our first fixture revision. Change the
source digest and increment that version together in a reviewed PR.

The native `fixtures.timescaledb` step runs only on manual pipeline runs. It
downloads the explicitly selected native platforms anonymously, verifies every child image,
layer and attached provenance document, and retains an OCI archive plus
metadata. It does not publish, use Docker credentials or select the runner's
CPU architecture. The committed selection preserves both Linux AMD64 and
ARM64 plus their attached provenance. These are the two container architectures
the native runner accepts. The existing upstream index also includes 386 and
ARM v6/v7 and exceeds the 2 GiB artifact ceiling; those unsupported architectures
are excluded explicitly, without raising the ceiling. The derived index has its
own digest, and metadata records the original upstream index and selected
platforms. The existing five-platform mirror remains at its old immutable digest. Verification establishes exact bytes and
subject association; it does not certify an upstream attestation's claims.

Configure a `fixture-timescaledb` release component with version source
`deploy/ci-fixtures/timescaledb.json`, JSON field `version`. Its assembly selects
the successful manual run's `fixtures.timescaledb` step, archive
`.memql-fixture/image.oci.tar`, metadata `.memql-fixture/metadata.json`, kind
`oci` and platform `oci-index`. The separately approved registry target is
`ghcr.io/znasllc-io/ci-timescaledb`, using the exact component version as its
immutable tag. Credentials stay in the native publication service.

Publication reads back the root index, every child manifest and every blob.
An existing tag containing different bytes refuses. A later host can reconcile
the same immutable candidate after an uncertain response. Record the successful
native publication receipt before removing the legacy mirror workflow.

Only after successful publication and anonymous pull verification, change the
service digest in `memql-package.yaml` through a normal PR and its database
checks. Publishing a fixture does not change the image used by current runs.
