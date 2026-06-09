# Versioning

memQL (the engine + node-type binaries in this repo) follows
[Semantic Versioning](https://semver.org/) with **git tags as the
single source of truth**.

## Current baseline: 0.9.0

The repo is on a clean `0.9.0` baseline as of the 2026-05-30
platform versioning reset (epic
[znasllc-io/memql#501](https://github.com/znasllc-io/memql/issues/501)).

`0.9.0` is an **honest pre-1.0** number:

- We have **not** committed a stable public API or wire contract yet.
  Pre-release rules apply (see "Pre-1.0 rules" below): a contract may
  change in any minor, and consumers are fixed in lockstep rather
  than carried with compatibility shims.
- It is deliberately **not** `0.1.0`. The engine is mature — a full
  DSL, gRPC surface, distributed node system, identity service, and
  cognition/voice/worker integrations all ship today. `0.9.0`
  reflects that maturity while staying honest that the API is not yet
  frozen.

The prior `2.3.0-<epoch>` string in the `VERSION` file and the
orphaned `v0.1.0` git tag are both retired. They were noise — an
internal working number with a manually-stamped epoch suffix, and a
tag from a different lineage.

## Git tag is the source of truth

The version of a memQL build is the **git tag** it was cut from
(`vX.Y.Z` on `main`), not a number embedded in a file.

- The in-repo `VERSION` file carries the **plain semver** the next
  release will be cut at (`0.9.0`). It is a convenience for tooling
  (`make version`, `make release` defaulting) — never a build stamp,
  never suffixed.
- **No `-<epoch>` suffixes.** Dev builds are identified by their git
  SHA (`scripts/release/release.sh` stamps
  `org.opencontainers.image.revision=<short-sha>` on every image, and
  flags a dirty tree). There is nothing to strip and nothing to
  reconcile.
- A release is cut by tagging `main` (`git tag vX.Y.Z`), then
  `make release VERSION=X.Y.Z ...` builds the immutable
  `memql:X.Y.Z` image from that commit. See
  [docs/public/operate/deployment-strategy.md](docs/public/operate/deployment-strategy.md#release--versioning-semver-tag---immutable-image).

## Pre-1.0 rules (today)

While we are below `1.0.0`:

- **Minor bumps may change contracts.** A wire/API change lands in
  memQL and its consumer (typically the CoPresent BFF carrier) at the
  same time. No backwards-compat shims, no deprecation windows — fix
  both ends and delete what is no longer needed (see the branch
  workflow notes in [CLAUDE.md](CLAUDE.md)).
- **Patch bumps** are bug fixes that keep the contract identical.
- memQL versions **independently** from the other platform repos. The
  engine may reach `0.14` while memql-cockpit is at `0.10`; that is
  expected. Coherence across repos is maintained by the pin chain, not
  by lockstep numbers — see [COMPATIBILITY.md](COMPATIBILITY.md).

## 1.0.0 at the beta

`1.0.0` is cut at the **invite-only beta (~Aug 2026)** — 1.0 means
"first real users," the point at which we commit to a stable public
surface and start honoring the usual semver compatibility guarantees
(breaking changes require a major bump).

The 1.0.0 cut is a **coordinated platform train**: at the beta, every
platform repo is tagged `1.0.0` together as a single coherent release.
After that train, repos resume independent semver. The train concept
is documented in [COMPATIBILITY.md](COMPATIBILITY.md).

## Documentation versioning

**Docs version == engine release.** There is no separate docs version
line. Public documentation lives in `docs/public/` (the source of truth;
see [docs/DOCS_STANDARD.md](docs/DOCS_STANDARD.md)) and is published to
memql.io per release:

- On each `releases/<X.Y.Z>.yaml` lockfile, the release pipeline builds a
  `docs-<X.Y.Z>.tgz` bundle (the `docs/public` markdown tree + generated
  reference + a `manifest.json`) and attaches it to the GitHub Release.
- memql.io consumes each bundle into a per-version snapshot and exposes a
  version dropdown. `latest` tracks `main`'s `docs/public`.
- Machine reference (DSL constructs, concept catalog, architecture
  diagrams) is generated at release time, so it can never drift from the
  engine the version was cut from.

This rides the same lockfile-as-source-of-truth model as the image
release flow above — a new engine version automatically yields a new docs
version.
