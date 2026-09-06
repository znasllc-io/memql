---
title: Documentation Directory
audience: internal
status: stable
area: ops
sinceVersion: 0.1.0
owner: znas
---

# Documentation Directory

**Purpose:** MemQL documentation, split public (drives memql.io) vs internal.
**Rules:** [DOCS_STANDARD.md](DOCS_STANDARD.md) — front-matter, layout, the
repo→site release-versioned pipeline. **Index:** [../GLOSSARY.md](../GLOSSARY.md).

---

## Layout

```
docs/
├── DOCS_STANDARD.md   The standard (read this first)
├── CLAUDE.md          This file
├── public/            Published to memql.io; areas mirror the site sidebar
│   ├── overview/      what it is, the harness, quickstart, tech stack, roadmap, proving
│   ├── concepts/      data model, events, identifiers, mental models
│   ├── language/      the MemQL DSL (reference, authoring, naming, specs)
│   ├── ai/            LLM cost control, operator capabilities
│   ├── build/         gRPC/audio, build tags; reference/_generated/ at release
│   ├── operate/       deploy, auth/, env, runbooks (public)
│   └── cockpit/       engine-backed Cockpit surfaces (Editor); the rest live in the memql-cockpit repo
├── internal/          Never published
│   ├── design/        ADRs / historical design rationale (status: historical)
│   ├── planning/      active multi-phase plans (deleted when shipped)
│   ├── program/       epic/planning documents (e.g. 00-master-plan.md)
│   └── ops/           DR, CI, migrations, safety, provisioning runbooks
└── superpowers/       Brainstorm and plan output. EXEMPT from the
    ├── specs/         front-matter and relative-link gates, so its
    └── plans/         lifecycle is a convention and nothing enforces it
```

**`superpowers/` is the one tree no gate watches**, which is why its rule is
written out below rather than left to the standard. It was absent from this
layout until 2026-09-04, and 748 KB of spent implementation plans had
accumulated in it by then.

## Conventions

- Every file carries front-matter (`audience`/`status`/`area`/`sinceVersion`/`owner`);
  the site selects `public/**` where `audience: public`. See DOCS_STANDARD.
- `public/language/memql.md` is also **embedded into the binary** via
  `docs/embed.go` (the `memqlGuide` builtin) — if you move it, update the
  `//go:embed` directive.
- Lowercase-with-hyphens filenames. Cross-reference with relative paths.
- When a feature ships, update the affected `public/` reference and either
  delete the `internal/planning/` doc or flip an `internal/design/` doc to
  `status: historical` in the same commit. No stale "deprecated" stubs.
- **A PLAN is spent when it ships; a RECORD is not.**
  `superpowers/plans/` holds implementation plans, and an executed plan is the
  largest stale artifact this repo produces — **delete it in the epic's own
  merge**, the way an `internal/planning/` doc is deleted.
  `superpowers/specs/` holds design records, which are kept and cited by
  CLAUDE.md files and READMEs across the tree. Delete a spec only when all
  three hold: its work shipped, **nothing in the repo cites it** (grep the
  filename — citations from `clients/**` and `editors/**` are outside every
  docs gate and break silently), and it names a tree that has since been
  restructured, so it can no longer be read as an accurate account of
  anything. Otherwise leave it: a stale-but-cited record wants its content
  repointed, not the file removed.
- No emojis (global convention).
