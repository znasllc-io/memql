# component/language -- the MemQL front end

**Purpose:** Turn `.memql` source text into an AST, and the AST into the
JSON the automation scheduler consumes. Everything upstream of execution.
**Language:** Go
**Module:** `github.com/znasllc-io/memql/component/language` (its own
`go.mod`, plus three of its sub-packages -- see Module layout)

This is the most-edited tree in the repo and the one where a change is most
likely to be silently invisible to a test run. Read Testing before you
verify anything here.

---

## What lives here

| Package | Owns | Imported by |
|---|---|---|
| `parser/` | Lexer, parser, and the **struct-form rewriter**. Every `.memql` construct is authored in struct form; the rewriter translates it to the procedural form the grammar reads, before the parser proper sees it. | ~286 files |
| `ast/` | The AST node types, in their own module so consumers can depend on the shape without pulling in the lexer/parser. `parser` re-exports every symbol as a type alias, so old call sites still compile; **new code imports `ast/` directly**. | ~64 files |
| `compiler/` | AST -> output format (primarily the `.json` the automation scheduler consumes), plus the linter, the CQS validator, and the automation generator. | ~8 files |
| `annotations/` | The single physical source of truth for **which annotations each receiver kind accepts** (`ByReceiver`) and their one-line docs (`Docs`). A leaf package -- it imports nothing inside the repo -- so the engine load gate and the editor surface can both derive from it with no import cycle (memql#991). | ~12 files |
| `dslspec/` | The single machine-readable spec of the authoring surface: constructs, keywords, operators, field types, and the legal-next rules that drive completion. Derives annotations FROM `annotations/` rather than re-listing them, and exports as portable JSON (memql#2122-2125). | ~9 files |
| `dslclause/` | One answer to "which keywords terminate a filter clause", shared by the text-scanning gates so they cannot drift about it (memql#2815). Owns clause EXTRACTION only, not predicate decomposition -- the package comment says why that split is deliberate. | ~5 files |
| `pagination/` | The pure classifier behind the pagination authoring rule: a list-returning query must carry `paginate`, `sort`, `count`, or `@unbounded("reason")` (memql#1965). Operates on raw source text using line structure only. | ~3 files |
| `language.go` | The `Language` component: bundles the parser and compiler submodules under one lifecycle with their own env-configured loggers. Note that the *root* package is thin -- almost every consumer imports a sub-package directly, not this. | 2 files |

---

## The load-bearing thing to know: struct form is a rewrite, not a grammar

The author surface (`query NAME { args, filter, shape }`, `mutate`, `logic`,
`automation`, file-top `args { ... }`) is **not** what the grammar parses.
`parser.NormaliseAll(source)` runs a five-stage chain that rewrites each
construct into the older procedural form, and only then does the lexer run.
Each stage is a no-op when its detector does not match.

Two consequences that bite:

- **The rewriter is a line-oriented text pass, not a parse.** It scans
  `filter` / `return` bodies line by line, so a multi-line boolean
  expression it would accept on one line fails (memql#4123). Nothing in the
  shipped corpus hits this today; a future formatting-convention change
  would hit it immediately.
- **A parse error can come from the rewriter, not the parser**, and will
  point at rewritten source rather than what the author wrote. When an error
  message does not match the file you are looking at, check
  `parser/rewriter.go` first.

The retired author-side forms (`func (Query) NAME(ctx any)`, the `@use*`
annotation family, `@concepts(...)`, `@input { ... }`, `include` in a shape
body, `;`/`,` filter separators, `has`, `?.`) are refused at parse time with
a migration hint. They survive only in `dsl/_reference/*.memql` as
don't-do-this skeletons. Do not restore them when you see one in an old diff.

---

## Source-of-truth boundaries

These are deliberate and are what keep the editor, the load gate, and the
grammar from disagreeing. Adding a second copy of any of them is the
mistake this layout exists to prevent:

- **Annotations** live in `annotations/`. `dslspec` inverts that registry
  rather than re-listing it, so the spec cannot disagree with the gate.
  The per-construct decl parsers in `parser/` (`tool_decl.go`,
  `provider_decl.go`, ...) remain the authoritative *parse-time* gate for
  the declarative constructs; `annotations/` mirrors their accepted sets
  for the editor, kept in sync by review.
- **Constructs / keywords / operators / field types / legal-next** live in
  `dslspec/`. There was no pre-existing registry -- the truth was split
  between `parser/parser.go`'s top-level dispatch and `parser/rewriter.go`.
  A drift test introspects both and fails when the spec falls out of
  lockstep.
- **Filter-clause terminators** live in `dslclause/`.

---

## Module layout

`component/language` is its own Go module, and so are three of its
sub-packages -- `annotations/`, `ast/`, and `dslclause/` each carry a
`go.mod` and are wired in with `replace` directives. All four are listed in
`go.work`. The split is part of the module work in memql#3228; the leaf
packages are separate modules precisely so a consumer can depend on the
annotation registry or the AST types without dragging in the parser.

`compiler/`, `dslspec/`, `pagination/`, and `parser/` are **not** separate
modules -- they are packages inside `component/language`.

---

## Testing

**`go test ./...` does not compile this tree.** This is a multi-module
workspace, so a relative pattern resolves inside whichever module owns the
directory it is rooted at, and `component/language` is one of the modules
`./...` from the repo root misses entirely (memql#4032). The failure mode is
silent and confidence-increasing: you edit the parser, see `ok` across 64
packages, and report it verified.

Use the module path, which is prefix-matched across every workspace module:

```bash
make test                                     # the whole tree, the documented command
go test github.com/znasllc-io/memql/component/language/...   # just this tree
```

Offline, the engine's own gates run over a `.memql` corpus via
`cmd/memqllint`, which drives the same `MemQLEngine.Init` boot path the
runtime does -- useful when a change here could alter what the DSL gates
accept.

---

## See also

- [MemQL Language](../../docs/public/language/memql.md) -- the language reference
- [Authoring Rules & Gotchas](../../docs/public/language/authoring-rules.md) -- read before writing `.memql`
- [Functions](../../docs/public/language/functions.md)
- [component/CLAUDE.md](../CLAUDE.md) -- the component tree this sits in
- Root [CLAUDE.md](../../CLAUDE.md) -- DSL dependency tree, argument resolution, canonical filter syntax
