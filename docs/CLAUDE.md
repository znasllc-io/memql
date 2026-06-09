# Documentation Directory

**Purpose:** memQL documentation, split public (drives memql.io) vs internal.
**Rules:** [DOCS_STANDARD.md](DOCS_STANDARD.md) — front-matter, layout, the
repo→site release-versioned pipeline. **Index:** [../GLOSSARY.md](../GLOSSARY.md).

---

## Layout

```
docs/
├── DOCS_STANDARD.md   The standard (read this first)
├── CLAUDE.md          This file
├── public/            Published to memql.io; areas mirror the site sidebar
│   ├── overview/      what it is, harness thesis, quickstart, tech stack
│   ├── concepts/      data model, events, identifiers, mental models
│   ├── language/      the MemQL DSL (reference, authoring, naming, specs)
│   ├── ai/            LLM cost control, operator capabilities
│   ├── build/         gRPC/audio, build tags; reference/_generated/ at release
│   ├── operate/       deploy, auth/, env, runbooks (public)
│   └── cockpit/       (cockpit product docs live in the memql-cockpit repo)
└── internal/          Never published
    ├── design/        ADRs / historical design rationale (status: historical)
    ├── planning/      active multi-phase plans (deleted when shipped)
    └── ops/           DR, CI, migrations, safety, provisioning runbooks
```

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
- No emojis (global convention).
