# Versioning

memQL (the engine + node-type binaries in this repo) follows
[Semantic Versioning](https://semver.org/) with **git tags as the
single source of truth**.

## The 0.9.0 baseline

The line starts at `0.9.0`, set by the 2026-05-30 platform versioning
reset (epic
[znasllc-io/memql#501](https://github.com/znasllc-io/memql/issues/501)).
It has advanced normally since — see the `VERSION` file for where the
line stands today, and "Tag form" below for the one irregularity in
how those releases were tagged.

`0.9.0` was an **honest pre-1.0** number, and the reasoning still
governs the line:

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
  release will be cut at (`0.14.0` today). It is a convenience for
  tooling (`make version`, `make release` defaulting) — never a build
  stamp, never suffixed. It is deliberately **unprefixed**: it feeds
  the `memql:X.Y.Z` image tag, where a leading `v` does not belong.
  The `v` lives on the git tag only.
- **No `-<epoch>` suffixes.** Dev builds are identified by their git
  SHA (`scripts/release/release.sh` stamps
  `org.opencontainers.image.revision=<short-sha>` on every image, and
  flags a dirty tree). There is nothing to strip and nothing to
  reconcile.
- A release is cut by tagging `main` (`git tag vX.Y.Z`), then
  `make release VERSION=X.Y.Z ...` builds the immutable
  `memql:X.Y.Z` image from that commit. See
  docs/public/operate/deployment-strategy.md (see the product pack repo's docs/operate/deployment-strategy.md).

## Tag form: `vX.Y.Z`, and the unprefixed stretch

**The tag form is `vX.Y.Z`.** The next release is cut as `v0.15.0`.

Between `v0.9.6` (2026-06-01) and `0.9.9` (2026-06-02) the `v` was
dropped, and roughly eighty releases through `0.14.0` (2026-07-16)
were cut unprefixed. That was not a decision; it was drift, and this
document went on specifying `vX.Y.Z` throughout. Resolved in
[memql#3214](https://github.com/znasllc-io/memql/issues/3214) in
favour of the document.

**The two forms are one continuous line, not two lineages.** `0.9.9`
is the successor of `v0.9.6`; nothing was renumbered, nothing forked,
and **no number appears in both forms** — every tag is either
`v`-prefixed or bare, never both. So resuming needs no renumbering
and creates no ambiguity: `v0.15.0` simply follows `0.14.0`.

(`0.9.7` and `0.9.8` were never cut in either form. Gaps in the patch
sequence are normal here — a release number is consumed when a build
is attempted, not when one succeeds.)

The genuinely orphaned tag is `v0.1.0`, from a different lineage
before the `0.9.0` baseline reset. It is retired, as noted above, and
is unrelated to the prefix question.

### Consequence: Go module resolution

This is the reason the prefix matters rather than being cosmetic.

**An unprefixed tag is not a valid Go module version.** The Go proxy
ignores `0.14.0` entirely, so `github.com/znasllc-io/memql@latest`
still resolves to **`v0.9.6`, from 2026-06-02** — the last v-prefixed
tag, and five-plus release cycles stale:

```
$ curl -s https://proxy.golang.org/github.com/znasllc-io/memql/@latest
{"Version":"v0.9.6","Time":"2026-06-02T02:01:53Z", ...}
```

Consumers that need current code therefore pin a pseudo-version
against a commit rather than a release (`memql-cockpit` does exactly
this today). That works, but it means the module line and the release
line disagree, and nothing reconciles them.

Cutting `v0.15.0` unfreezes the module line.

**This is a precondition for publishing submodules.** The module-
boundary work ([memql#3228](https://github.com/znasllc-io/memql/issues/3228))
publishes `wire` and `engine` as independently versioned modules,
whose tags take the form `component/grpc/gen/vX.Y.Z`. Cutting those on
top of a release line the proxy already disagrees with is how
consumers get an unrecoverable `ambiguous import`. Submodule tags must
not be cut until the root line is v-prefixed and current.

**There is no automated guard on this.** Nothing in the repository
creates tags — they are cut by hand — so there is no seam to hold the
line, and a test reading `git tag` would fail open under CI's shallow
clones, which is worse than no test. The check is this document plus
review at release time.

## Pre-1.0 rules (today)

While we are below `1.0.0`:

- **Minor bumps may change contracts.** A wire/API change lands in
  memQL and its consumer (typically the product's BFF carrier) at the
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
