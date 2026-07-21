# Doc-comments as the description source

**Status:** accepted · **Epic:** [#2601](https://github.com/znasllc-io/memql/issues/2601) · **Issue:** [#2632](https://github.com/znasllc-io/memql/issues/2632)
**Rulings 5 and 6 adopt the recommendations flagged in the #2632 proposal**
under the owner's end-to-end mandate for the open-issue sweep; both are
single-constant / single-story decisions and stay adjustable until the
conformance-gate story (#2636) lands.

A `///` doc-comment immediately above a declaration becomes its description.
The form is additive — `@description` stays valid everywhere it parses today —
and `///` wins when both are present. The epic exists because the corpus
states its prose twice: 2,645 `@description` occurrences as of this design's
merge (~470KB, a third of corpus bytes; the epic's 2,902 figure predates the
#2615 args-field strip), heavily duplicated by header comments, plus 82
"// Arguments:" and 58 "// Returns:" blocks restating `args{}`
field-for-field. One channel,
stated once, un-escaped, is the token win.

## Why

Descriptions are load-bearing: they feed agent discovery (`functions()` /
`tools`), MCP tool descriptors, promote-time catalog embedding, the SDK's
generated JSDoc/Go docs, sense hover, and the gRPC concept surface. Today the
canonical channel is a quoted, escaped annotation string, and the human
channel is a comment the loader throws away — so authors write both. Making
the comment the source collapses the duplication and removes the escaping tax.

## Rulings

### 1. Attachment rule

A `///` block attaches to the **immediately following declaration**. A blank
line breaks attachment. A detached `///` block (blank line before the next
declaration, or trailing at end of file) is an **ordinary comment — ignored,
never an error**. This matches the Go/Rust convention authors already expect,
and "ignored" lets the codemod convert incrementally with no flag day.

The attachment target set is **every describable construct kind** — the
full `@description`-valid set from the attribute matrix: `concept`, `query`,
`mutation`, `logic`, `automation`, `tool`, `shape`, `capability`, `prompt`,
`provider`, `policy`, `spec`, `trait`, `action` — plus `args{}` block
**fields**. (The #2633 story text enumerates only the first eight; its test
coverage must span this full set — the corpus carries live descriptions on
prompts, providers, policies, specs and actions, and the #2635 codemod and
#2636 gate would otherwise strand them.) The args-field half is the only channel for
arg descriptions: the grammar epoch retired args-field `@description`
(parser-discarded, corpus stripped in #2615), and this epic does **not**
resurrect that spelling.

Annotations between a `///` block and its declaration do not break
attachment: `///` above `@mcp` above `query x` documents `query x`. Only a
blank line or a non-comment, non-annotation line breaks the block.

### 2. Multi-line join

Strip the `///` prefix and **exactly one** following space from each line,
then join consecutive lines with a single **space** (descriptions are prose,
not code). A bare `///` line (no content) is a paragraph break and joins as a
**newline**. Stripping one space (not all leading whitespace) preserves an
author's deliberate sub-indentation (`///   - bullet` keeps two spaces).

### 3. Precedence

`///` **wins** over `@description` when both are present. Never concatenate.
(Owner ruling carried by the epic; recorded here for citation.)

### 4. Description-surface inventory

Every consumer that must switch sourcing in story 3 (#2634), verified on
main at the time of writing:

| Surface | Site | Mechanism |
|---|---|---|
| Promote-time catalog rows | `component/memql/authoring_catalog_match.go:25-27` | `catalogDescRe` regex over **raw source** |
| MCP tool descriptors | `component/memql/mcp_promote.go:27` | `fn.Description` (synthesized fallback when empty) |
| `functions()` / `tools` discovery | `component/memql/executor_builtin.go:382,439,497,586` | `fn.Description` / `tool.Description` |
| SDK generator | `sdk/gen/gen.go:84` (`descRe`), `:453-456` (`descriptionFor`, used at `:390`) → `sdk/gen/emit_ts.go`, `sdk/gen/emit_go.go` | its own regex over **raw source** |
| `shapes()` / shape-help builtins | `component/memql/executor_builtin.go:745,797` | `shape.Description` |
| sense hover | `component/memql/sense/hover.go:93,129,142,159` | `Description` fields (shape, concept, concept-field, function) |
| gRPC concept surface | `component/grpc/concepts_handlers.go:35` | `c.Description` |
| Language docs about `@description` | `docs/public/language/attribute-matrix.md`, `memql.md`, `authoring-rules.md` | prose — final story (#2636) updates |

The two **raw-source regex extractors** (catalog match, SDK generator) are
the hazard entries: they bypass the AST, so populating an AST slot does not
reach them. Story 3 must either point them at the parsed slot or teach the
regexes the `///` form — the former is the design intent (one extraction,
the parser's).

### 5. Editorial length policy

**Target: 200 characters. Channel: sense/lint HINT, not a hard gate.**
(Adopted per the #2632 proposal's recommendation; the number lives in one
lint constant and may be re-tuned before #2636 merges.)

Rationale: the corpus average is ~166 chars post-#2615 (178.4 at the epic's
filing), so typical descriptions pass untouched; the waste concentrates in the long tail. A hard gate would force
truncation of legitimately longer descriptions and could block the epic's own
migration; the hint nudges without breaking. If a hard gate is ever wanted, it
applies to NEW descriptions only, grandfathering the corpus.

### 6. GrammarVersion

**Bump once for the whole epic**: `2026.07-null-coalescing-operator` →
`2026.08-doc-comment-descriptions`, riding the sourcing-flip story (#2634 —
the first story after which the engine's read surface actually includes
comments). (Adopted per the #2632 proposal's recommendation.) Once comments
are load-bearing, a comment edit is a semantic change; GrammarVersion is the
identity of what the engine reads, and it now reads comments. `memqlmigrate`
participates as in the token-economy waves: the story-4 codemod (#2635) is a
named rewrite keyed on the bump.

### 7. Escaping contract for the codemod

A quoted `@description("...")` escapes `\"` and `\\`; a `///` line needs
neither. The story-4 codemod (#2635), when converting, **un-escapes**:
`\"` → `"`, `\\` → `\`, and unwraps the outer quotes. Multi-line output uses
the ruling-2 join in reverse: a description under the length target emits one
`///` line; longer prose wraps at the author's discretion (the codemod wraps
at ~100 columns); embedded newlines become bare-`///` paragraph breaks.

## Story obligations

- **#2633 (parser AST slot)** — capture-only: lexer keeps `///` blocks,
  parser attaches per rulings 1–2 into a new `DocComment` slot on
  declaration nodes and args-field nodes; `Description` fields and all
  consumers untouched. `BlankComments` continues blanking `///` for the
  header detectors; the struct-form rewriter must preserve `///` in emitted
  output (pinned by test).
- **#2634 (sourcing flip)** — every inventory row above switches to the
  parsed slot with ruling-3 precedence; GrammarVersion bumps here (ruling 6).
- **#2635 (dedup codemod)** — the named rewrite converting `@description` +
  duplicated header comments to `///` per ruling 7, and collapsing
  "// Arguments:"/"// Returns:" blocks that restate `args{}`.
- **#2636 (conformance gate + docs)** — the ruling-5 length hint, a gate that
  new corpus files carry `///` (not `@description`) where they describe, and
  the public-docs updates in the inventory's last row.
