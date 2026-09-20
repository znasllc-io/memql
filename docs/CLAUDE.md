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
repo→site release-versioned pipeline. **User entry:** [Documentation home](public/overview/index.md).
**Index:** [../GLOSSARY.md](../GLOSSARY.md).

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

## A bug is not fixed until its case is in the corpus

Borrowed from SQL Logic Test, and it is the rule that makes
`test/conformance/2026/` worth its size: a defect in the language — a filter
that lowers to the wrong SQL, an expression the two evaluators answer
differently, a refusal whose wording stopped naming the fix — is not fixed by
the commit that changes the code. It is fixed by the commit that adds the case
the old code fails and the new code passes.

The corpus is the only thing that remembers. A fix with no case survives
exactly until someone refactors the path it lives on, and the regression comes
back reading like a new bug.

Where the case goes, by what it is about:

| The defect | The case |
|---|---|
| A filter lowers wrongly, or the two evaluators disagree | `2026/expr/<position>/`, as a `lower` and an `evaluate` verdict over the rows that split them |
| A construct or annotation accepts what it should refuse | `2026/cells/<construct>/<attribute>/`, with the `code` and `message` the refusal carries |
| A refusal's wording lost the replacement it used to name | the same cell's `expect.json`; the wording is pinned, so a change to it is a deliberate edit and not a drift |
| A load refusal nothing covered | `2026/negative/<construct>/<fault>.memql` |
| The parser panicked on an input | `2026/fuzz/`, or the package's own `testdata/fuzz/<Target>/` |

**The differential lane is REQUIRED** (memql#5386). It runs in the
`mcp-conformance` job with `MEMQL_DIFFERENTIAL_REQUIRED=1`, and a disagreement
between the SQL lowering and the in-process evaluator fails it. The lane draws
its expressions from the corpus plus a deterministic grammar-driven generator,
so its seed is printed on the summary line: a red on a hosted runner is
reproducible locally from that one number.
