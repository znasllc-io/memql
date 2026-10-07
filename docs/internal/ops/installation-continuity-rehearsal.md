---
title: Installation continuity rehearsal
audience: internal
status: draft
area: ops
sinceVersion: 0.25.0
owner: znas
---

# Installation continuity rehearsal

The [continuity test](../../../test/clustere2e/installation_continuity_test.go)
keeps an external client alive while the coordinated installation workflow
replaces the serving engine through ArgoCD. It does not initiate a rollout or
authorize completion of an installation journal record. Use it alongside the
native signed-image, workload, preservation and compatibility verifiers.

## Preconditions

Use the supported multi-node k3d + ArgoCD installation with at least two BFF
replicas and its normal TLS front door. Both baseline replicas must advertise
one known engine build. The replacement build must differ. The SDK checks the
replacement's advertised clean 12-character commit against the requested full
commit; the signed OCI verifier separately establishes full release identity.

Provide an existing user JWT valid for at least 21 more minutes. The test uses
that exact bearer on every reconnect, with no token refresh or new login. The
server verifies the token; the test's local expiry read only prevents starting
a rehearsal whose credential would predictably expire halfway through.

Choose at least one existing persisted site's stable public asset, ideally an
HTML document and an image or other binary asset. These must not be assets
whose content the release intentionally changes. The test uses HTTPS with
normal hostname verification, refuses redirects, and checks exact bytes and
content type before and after replacement. It never sends the engine bearer
to a site. Each asset is bounded to 4 MiB, and at most 16 URLs are accepted.

## Run

Export these values in the observer's environment; do not put tokens in a
command line or a checked-in file:

| Variable | Value |
|---|---|
| `MEMQL_E2E_ENDPOINT` | The normal `https://api.<installation-domain>` front door |
| `MEMQL_E2E_TOKEN` | The existing user's JWT |
| `MEMQL_E2E_ROOT_CA_FILE` | Optional PEM CA file; omission uses system roots |
| `MEMQL_E2E_INSTALLATION_EXPECT_COMMIT` | Full 40-character replacement engine commit |
| `MEMQL_E2E_INSTALLATION_ASSET_URLS` | JSON array of existing public HTTPS asset URLs, without credentials or query strings |
| `MEMQL_E2E_INSTALLATION_READY_FILE` | A new absolute path shared with the rollout operator |

Run the observer from the repository checkout:

```bash
go test -tags clustere2e -count=1 -timeout=25m \
  -run '^TestInstallationServingContinuityAcrossRollout$' \
  github.com/znasllc-io/memql/test/clustere2e -v
```

The observer reads the existing assets, connects ten authenticated SDK clients,
requires at least two distinct BFF identities, and opens structured graph
subscriptions. It writes and reads a uniquely named probe row and requires
delivery to every subscription before creating the readiness JSON file. The
rollout operator must wait for that file to contain valid JSON before starting
the authorized installation workflow. The observer has a 20-minute deadline.

Every original connection must reconnect, advertise the requested replacement
build and identify a new serving process. At least two replacement BFF
identities must be reached. The original user identity and access, the exact
persisted row and a new event on every original subscription must survive.
The observer then rereads every baseline site asset over a new TLS connection.

The result is written to `<ready-file>.result.json` with a `Passed` flag,
timestamps, serving identities, reconnect cycles and asset hashes. Both paths
are created exclusively with mode `0600`; stale files are refused. A readiness
file alone is never a pass. A process killed before it can write a result has
no passing evidence. Reports contain no bearer or asset contents.

## Interpretation and remaining qualification

This proves the tested session, rows, subscriptions and assets survived that
actual rollout. It does not prove gap-free event replay during the outage:
the test writes before and after replacement and validates subscription replay
and cross-replica delivery. It does not claim uninterrupted HTTP availability,
identity-token refresh, arbitrary user journeys, every site's assets, database
migration safety, or a signed native completion receipt.

The test writes two isolated `missingCapability` probe rows using the existing
cluster-test fixture; they do not invoke an AI provider. They remain in the
local rehearsal installation as evidence, matching the other cluster probes.
The observer changes no Kubernetes resources or release configuration. Run a
second observer across rollback with the restored engine commit as its target.

Without `MEMQL_E2E_INSTALLATION_EXPECT_COMMIT` this opt-in test skips. Once it
is explicitly requested, missing configuration, authentication, a single
replica, no actual reconnect, the wrong build, missing data or changed assets
are failures. Compilation or an unconfigured skip is not installed evidence.
