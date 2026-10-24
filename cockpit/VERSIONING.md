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
  (`0.9.0`, no `v` prefix, no build suffix). It tracks the most
  recent tag and is bumped in the same commit that precedes the tag.
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

1. Land the release changes on `main` through PRs and update a clean main checkout.
2. From the monorepo root, run `make -C cockpit release` to inspect the recommendation, then
   `make -C cockpit release ARGS="--prepare"` to open the version PR. An explicit version
   uses `ARGS="--version=X.Y.Z --prepare"`. This stamps `cockpit/VERSION`,
   `cockpit/cmd/memql/main.go`, and `cockpit/internal/worker/connect.go` together.
3. After native checks pass and that PR merges, update main, record its full
   commit SHA, and create an immutable annotated tag at that exact commit:
   `git tag -a cockpit/vX.Y.Z <reviewed-sha> -m "cockpit/vX.Y.Z"` followed by
   `git push origin refs/tags/cockpit/vX.Y.Z`. Never move a published tag.
4. In MemQL Pipelines, open the connected MemQL monorepo source, choose **Run release**,
   and supply `cockpit/vX.Y.Z`. A tag push alone does not start a release build.
5. Wait for the complete native release run. Prepare its release candidate,
   review every platform asset and checksum, and publish through MemQL's
   approved release flow. The configured publication target uses this exact
   source SHA and version. Verify the completed receipts and installer downloads
   before marking the GitHub release ready for users.

The release helper opens a PR; it does not push main or publish a partially built
release. Native build steps receive no release publication credentials. Retrying
publication reuses the verified retained artifacts and durable receipts.
