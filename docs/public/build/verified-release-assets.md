---
title: Verified release assets
audience: public
status: stable
area: build
sinceVersion: 0.25.0
owner: znas
---

# Verified release assets

The native `integrations/githubrelease` package verifies an immutable file and
publishes that exact asset to a bound GitHub draft release. Separate lifecycle
operations create tags and drafts, observe complete releases and promote them.
It supports
ordinary archives, extension packages and SDK files without interpreting or
executing their contents. It has no public DSL registration. A release
controller must first bind caller authority, a separately approved destination,
a durable publication intent and retained source bytes.

DSL chooses the release, version, source commit, assets, approval sequence and
eventual promotion. The native operation handles protocol requests, byte
verification and reconciliation of the same effect. Asset publication never
implicitly creates a tag or release or changes visibility. No operation deletes
or overwrites an asset.

## Input and ownership

`Verify(ctx, reader, Expected, Limits)` requires the authorized receipt's exact
`sha256:` checksum and byte length. It streams through EOF into a private
snapshot, verifies both values and returns an opaque `VerifiedFile`. The source
reader remains the caller's responsibility; blocked reads must respect its
context. Empty files are allowed. `Limits.FileBytes` may lower the native
ceiling of 2 GiB minus one byte. The handle's `Close` waits for an active
publication, then removes its private files.

`NewPublisher(Target)` validates operator configuration without network access:

- Exact API and upload origins, repository, draft `ReleaseID`, tag, approved
  source commit and asset name.
- Explicit credentials and optional root certificates. Configuration validation
  may omit the token; publication cannot. The approved destination binds the
  credential reference and trust settings; secret values stay outside DSL and
  execution records.
- An explicit list of HTTPS download origins for credential-free byte reads.
  Each must differ from the API and upload origins. Loopback HTTP for API and
  upload is available only through `AllowLoopbackHTTP`.

The adapter uses origin-root GitHub REST routes. It does not discover Enterprise
API prefixes. No environment token, proxy, netrc or Git credential helper is
used. Asset names use a stable ASCII subset: letters, digits, period, underscore
and hyphen, starting with a letter or digit and never ending with a period.
Tags are bounded ASCII Git references; source commits are exact 40-character
lowercase Git object IDs.

The explicit draft ID matters: GitHub's
[release-by-tag endpoint](https://docs.github.com/en/rest/releases/releases)
finds published releases. A workflow must select or create its draft separately
and bind that ID before upload. A candidate may approve an exact deferred draft
intent; native creation records the returned ID without changing the approved
destination. `target_commitish` is not proof of where an
existing tag points.

## Publication and recovery

`Publish(ctx, verifiedFile)` checks the exact release ID is still a mutable
draft, checks its tag and upload endpoint, and resolves the tag to the approved
commit. It follows at most eight annotated tag objects, rejecting cycles and
other object types. It reads the bounded asset inventory before deciding
whether this same asset already exists.

An existing same-name asset succeeds only when its uploaded state, size and
optional metadata digest agree and a complete binary download independently
matches the receipt. If absent, the adapter repeats the draft/tag check and
sends one raw upload. It then lists and downloads the asset again, even when
the upload response was lost. A final release/tag and asset identity check
must pass before it returns a receipt. A new host can reconcile with a new
verified handle and the same target; process-local state is not the receipt.

`ErrConflict` reports preexisting incompatible bytes or metadata. `ErrUncertain`
means a POST might have had an effect that readback could not verify. The caller
keeps its intent and source pins and reconciles the exact same destination.
Remote error bodies and signed URLs are not returned as error text. A successful
request or an asset's declared digest alone never constitutes publication proof.

GitHub's [asset API](https://docs.github.com/en/rest/releases/assets) can leave
a `starter` asset after an upstream failure and refuses duplicate filenames.
The adapter leaves both starter and conflicting assets untouched; cleanup or a
different release requires its own explicitly authorized workflow. It follows
at most three binary-download redirects to configured HTTPS origins without
credentials, cookies or referrers. API and upload redirects are refused.

Each publication has a 30-minute ceiling including handle admission, at most
96 protocol requests, 30-second metadata reads, bounded response headers and
2 MiB metadata bodies.
Pagination checks at most 1,000 assets plus an empty final page. The file and
inventory ceilings reflect GitHub's
[release limits](https://docs.github.com/en/repositories/releasing-projects-on-github/about-releases).

## Concurrency and evidence limits

GitHub provides no atomic conditional upload bound to draft state and tag
identity. Rechecking those facts before and after the effect detects observed
drift; it cannot prevent a concurrent administrator from promoting a draft or
moving its tag during the upload. The release controller must coordinate its
mutations. A verified receipt records what was observed, not a guarantee that
the remote asset remains present forever.

Tests use local HTTP and TLS fixtures, including concurrent publishers,
annotated tags, lost replies, conflicting/starter assets, malformed or excessive
metadata, credential confinement, corrupt/truncated byte streams, cancellation
and second-host reconciliation. They do not publish to GitHub. The candidate
controller additionally exercises native SQL authority, retained artifacts and
scoped DSL composition. Full installed qualification must cover these contracts
across the serving cluster too.

Related: [Assembling release candidates](../operate/release-candidates.md),
[Engine, integration and workflow boundaries](integration-boundary.md)
and [Verified OCI publication](verified-oci-publication.md).
