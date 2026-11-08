# Versioning

MemQL Cockpit follows [Semantic Versioning 2.0.0](https://semver.org/).
Versions are `MAJOR.MINOR.PATCH`:

- **MAJOR** — incompatible CLI / config / wire changes.
- **MINOR** — backwards-compatible features.
- **PATCH** — backwards-compatible fixes.

## The git tag is the source of truth

A release **is** an annotated git tag of the form `cockpit/vMAJOR.MINOR.PATCH`
(e.g. `cockpit/v0.9.0`) on the monorepo’s `main`. Everything else mirrors that tag:

- The [`VERSION`](VERSION) file carries the plain semver string
  (`0.9.0`, no `v` prefix, no build suffix). It names the prepared release,
  or the most recent release when no newer one is being prepared. Update the
  three version stamps together before verifying the release source. Repairs
  and final preparation may retain that version while its tag is still absent.
  Once a tag exists, its source is immutable; another source needs a new version.
- The binary stamps its version at build time. `make` resolves the
  version via `git describe --tags --exact-match --match 'cockpit/v*'` (the exact tag when
  the checkout sits on one) and falls back to the `VERSION` file
  otherwise, then injects it through `-ldflags "-X main.version=..."`.
  The compiled-in `main.version` variable is the fallback
  for direct source builds in the monorepo workspace that don't go through the
  Makefile.

There is **no epoch / timestamp suffix**. Earlier builds stamped
`0.1.0-<unix-epoch>` via a `version-stamp` target; that target
has been removed. Release numbers are pinned to tags. Executables append semver build metadata
identifying their source revision, such as `0.16.0+g688c08f464fe`, with `.dirty`
for local changes. The Makefile stamps this metadata; direct Go builds
use Go's VCS build information when available. An unstamped build reports
`+unknown`. This metadata does not change semver precedence or archive names.

## How the Cockpit reports its version

```
$ memql --version
memql 0.16.0+g688c08f464fe (computeruse)
```

The same string is sent on the worker-protocol `Register` handshake
(`cockpitVersion()` in
`internal/worker/connect.go`) so the hub can see
which Cockpit a worker is running.

## Baseline: 0.9.0

`0.9.0` is the current baseline for the public beta runway. The first
stable release will be **1.0.0**, cut when the beta stabilizes. Until
then we move through the `0.9.x` / `0.x` range.

## Compatibility with memQL (compatibility, not lockstep)

Cockpit and the MemQL hub version **independently** — the Cockpit
is a client of the hub, not a piece released in lockstep with it.
Source builds use the engine from the same checkout; release artifacts stay
independent. What matters at runtime is the **minimum MemQL / worker-protocol version the
Cockpit speaks**, which the Cockpit declares as its compatibility
floor. A Cockpit works against any hub at or above that floor.

The cross-component compatibility matrix — which Cockpit versions
speak to which hub / protocol versions — lives in memQL's hub
document:

- [`COMPATIBILITY.md`](https://github.com/znasllc-io/memql/blob/main/COMPATIBILITY.md)
  in [`znasllc-io/memql`](https://github.com/znasllc-io/memql).

When the Cockpit's minimum supported memQL / protocol version
changes, update that matrix in the monorepo and reference it here.

## Releasing

1. Prepare the release changes and update `cockpit/VERSION`,
   `cockpit/cmd/memql/main.go`, and `cockpit/internal/worker/connect.go` together.
   `make -C cockpit release` can display a version recommendation. Its older
   `--prepare` PR workflow is not used while this repository has PRs disabled.
2. Test the changed paths, reconcile the latest `main`, and push as the owner,
   following the monorepo's [branch workflow](../CLAUDE.md#branch-workflow).
   Wait for the full `MemQL / memql / push` check to pass. Resolve failures before
   cutting a release.
3. In MemQL OS, open **Pipelines > Releases > Cut release**, select the Cockpit
   profile, and check the source. Verify that the saved commit includes the
   changes, the version is correct, and the tag is `cockpit/vX.Y.Z`. The native
   workflow confirms the immutable tag and starts the release build. Never move
   a published tag. A separate component release needs its own verified commit.
   If the engine claims the prepared source first, land a Cockpit-only
   preparation commit and wait for its full push check. An unpublished Cockpit
   version whose tag is still absent can keep its synchronized stamps.
4. Wait for the complete native release run. Prepare its release candidate,
   review every platform asset and checksum, and publish through MemQL's
   approved release flow. The configured publication target uses this exact
   source SHA and version. Verify the completed receipts and installer downloads
   before marking the GitHub release ready for users.

Follow [Cutting a release](../docs/public/operate/release-cutting.md) for source
verification, candidate approval and recovery. Native build steps receive no
release publication credentials. Retrying publication reuses the verified
retained artifacts and durable receipts. Publishing Cockpit does not change the
cluster's installed engine version.
