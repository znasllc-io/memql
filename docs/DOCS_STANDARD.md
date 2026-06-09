# Documentation Standard

**Status:** stable · **Applies to:** `memql` (canonical), and — lighter — `copresent`, `memql-cockpit`, `memql-bff-copresent`.

This is the single rulebook for where documentation lives, how it is
tagged, and how it reaches **memql.io**. It exists because docs had
sprawled and the public site drifted from the engine. The contract:
**the repo is the source of truth; the site is generated from it,
versioned per memQL release.** (Epic: znasllc-io/memql#1167.)

---

## 1. Where docs live

```
docs/
├── DOCS_STANDARD.md          # this file
├── public/                   # the ONLY tree memql.io consumes
│   ├── overview/             # what it is, quickstart, install
│   ├── concepts/             # data model, events, identifiers, mental models
│   ├── language/             # MemQL DSL: constructs, authoring, reference
│   ├── ai/                   # SI providers, policies, integrations & tools
│   ├── operate/              # deploy, auth, env, runbooks that are public
│   ├── build/                # gRPC API, SDKs, building against memQL
│   ├── cockpit/              # the terminal IDE / ops console
│   └── reference/_generated/ # MACHINE-GENERATED at release time — do not hand-edit
└── internal/                 # never published to the site
    ├── design/               # ADRs / issue-tied design rationale (historical)
    ├── planning/             # active multi-phase plans (delete when shipped)
    └── ops/                  # runbooks, DR, CI, migrations
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
area: language          # overview | concepts | language | ai | operate | build | cockpit
sinceVersion: 0.9.0     # first release the doc's subject shipped in
owner: znas             # github handle responsible for keeping it current
---
```

- **`audience: public` is the gate.** The site build selects exactly
  `docs/public/**` where `audience: public`. A file under `docs/public/`
  marked `internal`/`ops` is excluded (use this to stage drafts).
- `sinceVersion` lets the site badge "new in X.Y.Z".
- `status: historical` marks a design doc that shipped or was superseded —
  kept for rationale, never silently rotting (see §4).

## 3. What is public vs internal

| Bucket | Goes to | Examples |
|---|---|---|
| Durable user/developer reference | `docs/public/<area>/` | concepts, DSL, API, SDK, public ops guides |
| Point-in-time design / ADR | `docs/internal/design/` | issue-tied design docs (`*-954.md`, `voice/4xx-*`) |
| Active multi-phase plan | `docs/internal/planning/` | in-flight feature plans |
| Ops runbook / DR / CI / migration | `docs/internal/ops/` | DR runbook, merge-queue, migrations |
| Repo governance | repo root | CONTRIBUTING, SECURITY, VERSIONING |

When unsure: if an outside developer building **against** memQL would
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

1. **Authoring:** prose is written/edited in `docs/public/**` via normal
   repo PRs. This is the only place public prose lives.
2. **Generated reference:** `cmd/docs-gen` produces the DSL construct
   reference, concept catalog, and architecture diagrams into
   `docs/public/reference/_generated/` from the engine itself (concept
   registry + `component/architecture`), so reference can never drift.
3. **Bundle:** `scripts/docs/build-docs-bundle.sh` selects the public set,
   runs the generator, and emits `docs-bundle/` = the markdown tree + a
   `manifest.json` (nav tree, section map, `version`, `engineVersion`).
4. **Release:** on a new `releases/<X.Y.Z>.yaml` lockfile, the
   `publish-releases` hook builds the bundle at that tag and publishes
   `docs-<X.Y.Z>.tgz` as a release asset.
5. **Site:** memql.io consumes each release's bundle into
   `versioned_docs/version-X.Y.Z/` and shows a version dropdown; `latest`
   tracks `main`'s `docs/public`.

**Docs version == engine release.** No separate docs version line.

## 6. Tone

Plain, technical, specific. **No emojis** (per the global style rule). Use
`SUCCESS:`/`WARNING:`/`ERROR:` and standard markdown. Show real commands
and real identifiers, not placeholders, wherever possible.
