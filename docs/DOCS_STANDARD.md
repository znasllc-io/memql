---
title: Documentation Standard
audience: internal
status: stable
area: ops
sinceVersion: 0.9.36
owner: znas
---

# Documentation Standard

**Status:** stable · **Applies to:** `memql` (canonical), and — lighter — `memql-cockpit` and the product repos (the frontend SPA and the product pack).

This is the single rulebook for where documentation lives, how it is
tagged, and how it reaches **memql.io**. It exists because docs had
sprawled and the public site drifted from the engine. The contract:
**the repo is the source of truth; the site is generated from it, one set
per MemQL release, and serves the newest release's set only.** (Epic:
znasllc-io/memql#1167; bundle contract v2: memql#5717.)

---

## 1. Where docs live

```
docs/
├── DOCS_STANDARD.md          # this file
├── public/                   # the ONLY tree memql.io consumes
│   ├── overview/             # what it is, quickstart, install, roadmap, proving
│   ├── concepts/             # data model, events, identifiers, mental models
│   ├── language/             # MemQL DSL: constructs, authoring, reference
│   ├── ai/                   # AI providers, policies, integrations & tools
│   ├── operate/              # deploy, auth, env, runbooks that are public
│   ├── build/                # gRPC API, SDKs, building against MemQL
│   ├── cockpit/              # the terminal IDE / ops console
│   └── reference/            # generated reference (the bundle renders the
│                             # concept catalog here as reference/concepts.md)
├── public_boundary_allowlist.toml  # links the boundary gate lets through
│                             # for now; it only shrinks (§5)
└── internal/                 # never published to the site
    ├── design/               # ADRs / issue-tied design rationale (historical)
    ├── planning/             # active multi-phase plans (delete when shipped)
    ├── ops/                  # runbooks, DR, CI, migrations
    └── program/              # historical program-level epic/plan docs
                               # (area: planning; kept for rationale rather
                               # than deleted, unlike a live planning doc)
```

- `docs/public/<area>/` subdirs **mirror the memql.io sidebar 1:1**, so the
  site nav maps directly onto the tree.
- Root governance files stay at the repo root and are **not** moved:
  `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`,
  `VERSIONING.md`, `COMPATIBILITY.md`.

## 2. Front-matter (required on every file under `docs/`)

```yaml
---
title: MemQL Language
audience: public        # public | internal | ops
status: stable          # stable | draft | historical
area: language          # overview | concepts | language | ai | operate | build |
                         # cockpit | reference | design | planning | ops
sinceVersion: 0.9.0     # first release the doc's subject shipped in
owner: znas             # github handle responsible for keeping it current
---
```

Two optional keys are read by the docs bundle (§5):

- `description` -- one sentence, the page's meta description and its
  `llms.txt` line. When it is absent the bundle derives one from the
  page's first paragraph of prose.
- `exposure` -- `engine` or `instance`. A page with no `exposure` reads as
  `engine` until the key becomes required. `exposure: instance` marks a
  page that belongs to an instance repository; a public page carrying it
  in this repository fails the bundle build.

- **Front matter is the gate.** The bundle publishes exactly the pages git
  tracks under `docs/public/` with `audience: public`, `status: stable`
  and `exposure: engine`. A file under `docs/public/` marked
  `internal`/`ops`, or `draft`/`historical`, is left out; so a link to it
  from a published page is refused, because the site would 404 on it.
- `sinceVersion` lets the site badge "new in X.Y.Z".
- `status: historical` marks a design doc that shipped or was superseded —
  kept for rationale, never silently rotting (see §4).

## 3. What is public vs internal

| Bucket | Goes to | Examples |
|---|---|---|
| Durable user/developer reference | `docs/public/<area>/` | concepts, DSL, API, SDK, public ops guides |
| Point-in-time design / ADR | `docs/internal/design/` | issue-tied design docs (`*-954.md`, `*-971.md`), ADRs (`*-adr.md`) |
| Active multi-phase plan | `docs/internal/planning/` | in-flight feature plans |
| Ops runbook / DR / CI / migration | `docs/internal/ops/` | DR runbook, merge-queue, migrations |
| Historical program-level epic/plan docs | `docs/internal/program/` | platformization program master plan + epics, kept for rationale (`area: planning`) |
| Repo governance | repo root | CONTRIBUTING, SECURITY, VERSIONING |

When unsure: if an outside developer building **against** MemQL would
want it, it's `public`; if it only helps someone changing **the engine**,
it's `internal`.

## 4. Lifecycle

- A design doc that ships flips to `status: historical`, gets a one-line
  banner (`> Historical: shipped in X.Y.Z; kept for rationale.`), and
  moves to `docs/internal/design/`. It is not deleted — the rationale is
  the value.
- A planning doc is deleted once fully shipped; any still-live follow-ups
  become GitHub issues first.
- Ephemera (handoff notes, scratch TODOs) do not belong in `docs/` — track
  in GitHub issues/Projects.
- A doc must not contradict the code. If a feature is retired, the doc is
  updated or moved to `historical` in the same change.

## 5. How docs reach memql.io (the pipeline)

memql.io serves **one documentation set: the newest release's.** A docs URL
carries no version, there is no version dropdown and no archive, and a page
lives at one URL for as long as it exists. Each release's set is a GitHub
Release asset; the site replaces its set with the newest one.

1. **Authoring.** Prose is written and edited in `docs/public/**` through
   ordinary pull requests. It is the only place public prose lives.
2. **Selection.** A page is published when git tracks it under
   `docs/public/` and its front matter says `audience: public`,
   `status: stable` and `exposure: engine` (a missing `exposure` reads as
   engine). A public page marked `exposure: instance` fails the build. So
   does a published page whose `area` is not a public area, or two pages
   served at one URL.
3. **Bundle.** `cmd/docs-gen bundle`, run by the capability script
   `scripts/docs/build-docs-bundle.sh` (`make docs-bundle`), builds the set:
   - The concept catalog is rendered from the DSL in process, as
     `reference/concepts.md` (area `reference`). It is never written into
     `docs/public`.
   - Every relative link to a published page becomes that page's absolute
     route, with its `#fragment` kept. `overview/index.md` is the docs home,
     `/docs/`; any other `<dir>/index.md` is `/docs/<dir>/`; every other page
     is `/docs/<path without .md>/`. The site serves trailing slashes.
   - Every other relative link is refused: one that leaves `docs/public`
     (into `docs/internal`, `docs/superpowers` or the source tree), one to a
     page the bundle does not publish, one to a path that is not a page.
   - HTML comments outside code are stripped. They are gate markers, and the
     site prints raw HTML as text.
   - The set is the page tree plus five root files: `manifest.json`,
     `memql-docs-version` (the bare version, one line), `llms.txt`,
     `llms-full.txt` and `sitemap-docs.xml`. It is packed as
     `docs-<X.Y.Z>.tgz` at the repository root, byte-identical across builds
     of one commit.
   - `--version` must be `X.Y.Z` and equal `VERSION`, which equals the
     release tag (VERSIONING.md). Each page's `lastUpdated` is the committer
     date of its last commit, so a build refuses a shallow clone.
4. **The boundary gate.** `docs_public_boundary_test.go` runs the bundle's
   own check on every pull request, and `docs_bundle_build_test.go` builds
   the bundle; the release lane runs the same check (`make docs-bundle-check`)
   before it builds, so a release cannot ship a link the pull request would
   have refused. The links that break the boundary today are listed in
   `docs/public_boundary_allowlist.toml`; an allowlisted link ships as its
   plain text, never as a link into the repository. An entry that matches no
   link fails the gate, so the list only shrinks. Fix a failure by linking
   the public page that covers the subject, or by saying it in prose; do not
   add an entry for a new link. The docs-safety epic adds its rules
   (hostnames, customer names, money) to the same check.
5. **Release.** When a GitHub Release is published for an `X.Y.Z` or
   `vX.Y.Z` tag, `.github/workflows/publish-docs-bundle.yml` checks out the
   tag with full history, runs the check, builds the set **at that tag** and
   attaches `docs-<X.Y.Z>.tgz` to the release. `workflow_dispatch` rebuilds
   one release's set, by either tag spelling; a rebuild picks up no later
   docs fix.
6. **Site.** This step belongs to the instance repository that hosts
   memql.io; what follows is its side of the contract. Its docs sync
   (memql#5716) replaces the instance's docs set with the newest release's
   asset, on a schedule and, once the token for it is in place, on an
   `engine-docs-released` repository dispatch from the publish workflow.
   Its site renders the manifest's version in the docs chrome and as
   `<meta name="memql-docs-version" content="X.Y.Z">` on every docs page
   (memql#5715), and follows the absolute routes the bundle's links carry
   (memql#5718).
   `scripts/docs/current-check.sh` fails weekly when the newest release tag
   has no asset or the site's version differs.

   **Merge order.** A contract v2 page links other pages by absolute route
   (`/docs/<slug>/`). The site's reader before memql#5718 resolves only
   relative `.md` links and renders every other in-docs link as plain text,
   so the first release built with contract v2 would unlink every
   cross-page link on memql.io. memql#5718 (or, at the least, a reader that
   follows `/docs/` routes) merges before the first engine release cut
   after contract v2; the transitional `nav` below keeps only the sidebar
   working.

Two generated facts are committed rather than bundled: `make arch-model`
writes the code's architecture model and `make platform-graph` the platform
graph (roles, deployment, gRPC services, front door, routing, concepts,
automations, OS navigation) into `component/architecture/embedded/`, each
held to the tree by a drift gate. The diagrams the docs are drawn from read
them.

### The manifest (schema 2)

`manifest.json` is the set's table of contents. The site builds its
navigation, meta tags and sitemap from it.

| Field | Meaning |
|---|---|
| `schema` | `2`. Schema 1 was the Python bundler's `{version, engineVersion, pageCount, areas, nav}`. |
| `version` | The bare release version, the only version field. |
| `tag` | The release tag at the built commit, either spelling, or `null` for a build at an untagged commit. |
| `commit`, `releasedAt` | The built commit and its committer date. |
| `siteUrl` | The site the absolute URLs in `llms.txt` and the sitemap point at. |
| `home` | The path of the page served at `/docs/`: `overview/index.md`. |
| `pageCount` | The number of published pages. |
| `areas` | `{id, title, order}` for each area with a page, from the one area table in `cmd/docs-gen/bundle`. |
| `pages` | One entry per page, in sidebar order: `path`, `slug`, `url`, `title`, `area`, `order` (within its area), `description`, `descriptionDerived`, `exposure`, `sinceVersion`, `lastUpdated`, `generated`. |
| `nav` | **Transitional**: schema 1's `{area, pages: [{path, title, sinceVersion}]}` tree, derived from `pages`, kept because the site's current reader builds its sidebar from it. It goes when the site reads `pages` (docs-current PR 6, memql#5718). It keeps the sidebar only: in-page links need the reader that follows routes (§5, step 6, merge order). |
| `positioning`, `nodeTypes`, `providers`, `apps`, `diagrams` | Reserved, `null` until the change that fills each lands. `null` means "not produced by this release", never "empty". |

**Docs version == engine release.** No separate docs version line.

## 6. Tone

Plain, technical, specific. **No emojis** (per the global style rule). Use
`SUCCESS:`/`WARNING:`/`ERROR:` and standard markdown. Show real commands
and real identifiers, not placeholders, wherever possible.
