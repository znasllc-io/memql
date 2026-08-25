# Versioning

MemQL (the engine + node-type binaries in this repo) follows
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

The version of a MemQL build is the **git tag** it was cut from
(`vX.Y.Z` on `main`), not a number embedded in a file.

- The in-repo `VERSION` file carries the **plain semver** the next
  release will be cut at. It is a convenience for tooling (`make
  version`, `make release` defaulting) — never a build stamp, never
  suffixed, and since memql#3998 **never read by the binary**. It is
  deliberately **unprefixed**: it feeds the `memql:X.Y.Z` image tag,
  where a leading `v` does not belong. The `v` lives on the git tag
  only.
- **No `-<epoch>` suffixes.** Dev builds are identified by their git
  SHA (`scripts/release/release.sh` stamps
  `org.opencontainers.image.revision=<short-sha>` on every image, and
  flags a dirty tree). There is nothing to strip and nothing to
  reconcile.
- A release is cut by tagging `main` (`git tag vX.Y.Z`). `make release
  VERSION=X.Y.Z` builds an immutable `memql:X.Y.Z` image **locally** from
  that commit -- useful for inspecting what a release image contains, but
  it is not the cloud-release path: per CLAUDE.md's image-build rule, a
  deployable release image is built on the GitHub build server, not an
  operator machine. The cloud-release mechanism is the git tag plus a
  manual `workflow_dispatch` of
  [`.github/workflows/build-engine-images.yml`](.github/workflows/build-engine-images.yml)
  with the version as input; see
  [docs/public/operate/deploy-bundle-runbook.md](docs/public/operate/deploy-bundle-runbook.md)
  for the full deploy path (image build -> digest pin in the cloud overlay ->
  ArgoCD reconcile).
- **`PUSH=1` is break-glass, and the script now says so** (memql#4116).
  `make release VERSION=X.Y.Z ACR=acrmemql PUSH=1` runs a real `docker push`
  to the shared registry -- precisely the path CLAUDE.md's image-build rule
  forbids. It was reachable with no flag, check, or warning marking it, so the
  rule held only for someone who had read the rule. It is now refused (exit 3)
  unless the caller adds `CONFIRM=push-from-an-operator-machine`.

  The capability was gated rather than deleted because a genuine break-glass
  need exists -- the build server is unavailable and an image has to be cut --
  and a removed capability gets worked around outside the script, where nothing
  stamps the revision label or refuses to overwrite an existing tag. The phrase
  spells out what the operator is asserting, and it lands in shell history.

  What a break-glass push forfeits, and why it is not the default: the build
  server's image is reproducible, natively `linux/amd64`, and carries
  provenance. A locally-pushed image has none of those and is indistinguishable
  from a build-server one once it is in the registry. Local builds without
  `PUSH=1` are unaffected -- that is the normal use of this target.

## How a running binary states its release (memql#3998)

**The release is linked into the binary, and that is the only way it
gets there.** The image build passes the release it is cutting as the
`MEMQL_RELEASE` docker build arg, and the Dockerfile turns it into a
linker flag:

```
go build -ldflags "-s -w -X github.com/znasllc-io/memql/core/buildinfo.release=${MEMQL_RELEASE}" .
```

`core/buildinfo.Version()` is then the single answer for the whole
process: `resolveServiceVersion` in `main.go` returns it (so it reaches
`app.RunConfig.Version` and the engine's `memqlVersion()` builtin), and
`component/grpc` reads it for the `ServerHello.engine_version` field a
client sees on connect.

A build that was **not** cut from a release — `go build .`, `make dev`,
any branch image — leaves the release stamp empty and reports
`dev+<12 hex>`: the word, plus the revision it was built from, plus
`-dirty` when that tree had uncommitted changes. It reports the bare
`dev` only when it cannot establish a revision at all.

Both forms are deliberate and both are safe for the same reason: a
release parser wants `vX.Y.Z` before it will look at anything else, so
neither parses as a release tag and a client comparing versions lands on
"cannot compare" instead of a confident wrong answer.

**Why the revision rides the string here and not for a release**
(memql#4575). A cluster built from a checkout used to answer the bare
word, which is honest and useless: a developer who rebuilt an hour ago
and one who installed last week read the same thing. The revision is the
answer, and the version string is where it reaches every surface at once
— the portal's rail footer, the editor's cluster row, the boot log,
`memqlVersion()` — because they all already render this one value. The
editor in particular has nowhere else to put it: its recorded version
lives in the operator's `clusters.yaml`, and the cockpit rewrites that
file from its own struct and drops every key it does not model.

A **release** keeps its bare tag. That value is compared, sorted and
matched against an image tag in a dozen places, and gluing a second fact
onto it hands every one of them something to remember to ignore. A
release states its revision through `ServerHello.engine_commit` instead
— which is the case that needs it most, because a tag's image pins are
written before that tag's own images exist, so an instance declaring
`ENGINE_REF=v0.19.6` legitimately runs 0.19.5 binaries and only the
revision can say which source is executing.

**The revision has to be passed in, even locally.** `.dockerignore`
excludes `.git`, so the Go toolchain's own `vcs.revision` stamping —
which covers a plain `go build .` — cannot fire inside an image build.
The `MEMQL_COMMIT` build arg is the only source there, and until
memql#4574 the shared local mapping (`scripts/lib/engine_build_args.sh`,
used by `make dev` and by the editor's rebuild) passed nothing, so every
locally built image carried no revision at all.

There is no environment variable and no file. Both existed and both were
removed rather than demoted:

- `resolveServiceVersion` read the `VERSION` **file**, which had said
  `0.15.0` at every tag from `v0.16.1` onward, and the image build
  rewrote it as `0.15.0-<epoch>` — the suffix form forbidden two
  sections up. Every released node therefore reported a number that was
  release-*shaped*, unrelated to its release, and re-stamped on each
  build so it looked intentional.
- A `VERSION` **env var** could override all of it at deploy time. A
  version a running process can be *told* is not a version; the reason
  the engine could not state its release honestly is that it had two
  ways to be told what to say and no way to know.

`TestEveryEngineImageLinksTheReleaseIntoTheBinary` and the guards beside
it (`release_stamp_test.go`), together with the revision's own half in
`commit_stamp_test.go`, hold this in place, because `-X` is
**silently ignored** when its target does not resolve: a renamed
variable or a typo'd import path breaks nothing visible and quietly
turns every subsequent release into `dev`.

**This only helps clusters cut after it ships.** It cannot teach
`v0.18.0` — or anything older — to introduce itself, because the binary
that cannot say its version is the one already installed. That is why
version awareness in the VS Code plugin does not rest on this alone: the
plugin records a cluster's version in `clusters.yaml` at install time
and refreshes it from every trustworthy source (the install receipt, the
ArgoCD `targetRevision`, deploy-control status, `memqlVersion()`). This
change makes the engine the most trustworthy of those sources once it
exists; it does not make it the only one.

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

**Precondition satisfied.** `v0.15.0` was cut on 2026-08-07 and the
proxy answers with it:

```
$ curl -s https://proxy.golang.org/github.com/znasllc-io/memql/@latest
{"Version":"v0.15.0","Time":"2026-08-07T17:34:36Z", ...}
```

Submodule tags may be cut from here. How, and for which modules, is the
next section.

## Submodule versions (memql#3228 / memql#3245)

MemQL is **48 Go modules**, one per tier directory, so its dependency
direction is enforced by the compiler rather than by convention (see
[docs/internal/ops/ci-design.md](docs/internal/ops/ci-design.md) §D3). Each of those modules has
its own tag namespace.

### Tag form

```
vX.Y.Z                          the root module
<module-dir>/vX.Y.Z             a nested module
```

So `github.com/znasllc-io/memql/component/grpc/gen@v0.15.0` resolves
**only** if the tag `component/grpc/gen/v0.15.0` exists. The root tag
does not publish nested modules; there is no inheritance. This is Go's
rule for a repository with nested modules, not a MemQL convention.

### Two independent lines. Everything else is lockstep.

| line | modules | version |
|---|---|---|
| `wire` | `component/{grpc,node,bus}/gen` | **independent** |
| `engine` | `component/{language,database,harness,actions,memql}`, `dsl` | **independent** |
| lockstep | **every other module** | `== the root release` |

**Read this before adding a third line.** 48 modules with 48 version
lines is 48 things to keep in step, and the coordination cost is paid
on every release forever. Two modules version independently because
they have consumers that need them to — `wire` because it is the wire
contract clients pin directly, `engine` because it is the platform.
Nothing else has that consumer. A lockstep tag carries the number the
root release already chose, so cutting it is mechanical and there is
nothing to decide.

The lockstep set is **computed as the complement**, not listed. A
module added tomorrow is lockstep by default — the answer that needs no
thought — and `scripts/release/submodule_lines_test.go` asserts the
partition stays total, that both explicit lists name real directories,
and that no module lands on two lines.

### Cutting them

```bash
make tag-submodules VERSION=X.Y.Z                    # all three lines
make tag-submodules VERSION=X.Y.Z LINE=wire          # one line
make tag-submodules VERSION=X.Y.Z DRY_RUN=1          # print the plan
```

The script ([`scripts/release/tag-submodules.sh`](scripts/release/tag-submodules.sh))
tags the commit the root `vX.Y.Z` tag names, so the module line and the
release line cannot disagree. It refuses on a dirty tree, on a tag that
already exists (module tags are write-once, like the image tags), and
on the failure below.

### The `v0.0.0` placeholder, and why the release commit must rewrite it

Nested modules on `main` require each other at the epoch-zero
placeholder plus a relative `replace`:

```
require github.com/znasllc-io/memql/component/actions v0.0.0
replace github.com/znasllc-io/memql/component/actions => ../actions
```

That is correct for local builds and for the `GOWORK=off` lane, and
**wrong the moment it is published**: a consumer ignores a
*dependency's* replace directives — only the main module's apply — so
the published go.mod sends every consumer looking for
`component/actions@v0.0.0`, which does not exist. On an immutable tag.

So **the release commit rewrites internal requires from `v0.0.0` to the
release version**, keeping the `replace` alongside (locally the replace
wins; for a consumer the require resolves against the tag that is about
to exist). `tag-submodules.sh` refuses to tag a module that still
carries a placeholder, and names the module and the require.

The `wire` modules are exempt by construction: they are L0 — no
internal requires at all — which is why `wire` is the line that could
be published first.

### `wire` is published, and resolvable — demonstrated

**Point-in-time demonstration, not current-state narrative.** The walkthrough
below is exactly what was run and observed on 2026-08-07, at root tag
`v0.15.0`, to prove the mechanism works once. It has not been re-run since.
As of this writing the root line has advanced to `v0.19.1` (six releases past
`v0.15.0`); `wire`'s three tags are still only cut at `v0.15.0` (versioning
independently means no later cut was required, not that one is owed), and
the `engine` line remains unopened -- zero `component/{language,database,
harness,actions,memql}/vX.Y.Z` or `dsl/vX.Y.Z` tags exist -- despite its
stated precondition (memql#3228 landing) having been met in the interim.
Read the proxy responses and git-tag counts below as "true on 2026-08-07,"
not as "true today" -- re-run the same `curl`/`git tag` commands against the
current tags for the live state.

The three wire tags were cut at `v0.15.0` on 2026-08-07:

```
component/grpc/gen/v0.15.0
component/node/gen/v0.15.0
component/bus/gen/v0.15.0
```

An external module — outside this repository, with **no `replace`
directive of any kind** — resolves and compiles against it:

```
$ cat go.mod
module example.com/wireconsumer
go 1.26.1
require github.com/znasllc-io/memql/component/grpc/gen v0.15.0
...

$ GOWORK=off go run .
wire module resolved from the proxy; round-tripped id="demo-1" (8 bytes)
```

and the proxy agrees about what it published:

```
$ curl -s https://proxy.golang.org/github.com/znasllc-io/memql/component/grpc/gen/@v/v0.15.0.info
{"Version":"v0.15.0", ..., "Subdir":"component/grpc/gen",
 "Hash":"d48b30b52be06c8768c86276817f72a7dd9bcece"}
```

`Subdir` and `Hash` are the point: the proxy serves the wire tier from
the same commit the `v0.15.0` release was cut from. The module line and
the release line agree.

`engine` could not be published at `v0.15.0` — its modules did not exist at
that commit. Its line was designed to open at the first release cut after
[memql#3228](https://github.com/znasllc-io/memql/issues/3228) lands, with
`make tag-submodules VERSION=X.Y.Z LINE=engine`, and only once the
release commit has rewritten the placeholders described above. As of this
writing memql#3228 has landed and the root line has advanced six releases
past `v0.15.0`, but the `engine` line has still not been opened -- `git tag`
carries no `component/{language,database,harness,actions,memql}/vX.Y.Z` or
`dsl/vX.Y.Z` tag. Cutting one is a `make tag-submodules ... LINE=engine`
away at the next release; nothing besides operator action is blocking it.

### Consumers: what this means for the per-module `replace` set

[`memql-cockpit`](https://github.com/znasllc-io/memql-cockpit) consumes
21 MemQL packages spanning every tier, and today resolves them through a
per-module `replace` set against a **pinned sibling checkout**
(memql#3238). Its go.mod records why in detail.

**That set can be dropped in favour of version pins, and the condition
is stated rather than assumed:** every module cockpit consumes must be
published at a resolvable version — which under the lockstep rule means
one `<dir>/vX.Y.Z` tag per module at each release, cut by the command
above.

What it should *not* do is drop the sibling `replace` for **local
development**. That replace is how the cockpit dev loop edits MemQL and
its client together, and pinning to a published version would trade a
one-command inner loop for a publish round-trip. The change is to the
CI/consumer path: with tags published, cockpit's workflows pin versions
instead of checking out a sibling at `.github/memql-pin`, and the
per-tier pin-bump PR the split required stops being necessary — the
failure rows in that go.mod's table only occur *while* modules are
landing, and after this epic nothing is landing.

## Pre-1.0 rules (today)

While we are below `1.0.0`:

- **Minor bumps may change contracts.** A wire/API change lands in
  MemQL and its consumer (typically the product's BFF carrier) at the
  same time. No backwards-compat shims, no deprecation windows — fix
  both ends and delete what is no longer needed (see the branch
  workflow notes in [CLAUDE.md](CLAUDE.md)).
- **Patch bumps** are bug fixes that keep the contract identical.
- MemQL versions **independently** from the other platform repos. The
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
