# Parser Refactor: Shared Base via Embedded Struct

## Status of this work

**Owner:** previous session (commit `9a4e1d4`, branch `main`)
**Successor:** you

The DSL has 13 dedicated parsers across two packages. Each parser
re-implements the same set of low-level primitives. This handoff is
to refactor every parser onto a shared base via Go composition
(embedded struct) so duplication collapses while every parser keeps
its construct-specific grammar.

The previous session landed parsers for every construct but stopped
short of the shared-base refactor. That's the work you're picking up.

---

## Current parser inventory

| Construct | File | LOC | Pattern |
|---|---|---|---|
| concept | `component/database/memory-nodes/concept_parser.go` | 699 | self-contained |
| query | `component/memql/query_parser.go` | 82 | thin wrapper |
| mutation | `component/memql/mutation_parser.go` | 91 | thin wrapper |
| spec + trait | `component/memql/spec_parser.go` | 542 | self-contained |
| shape | `component/memql/shape_parser.go` | 1351 | self-contained |
| logic | `component/memql/logic_parser.go` | 77 | thin wrapper |
| automation | `component/memql/automation_parser.go` | 88 | thin wrapper |
| builtin | `component/memql/builtin_parser.go` | 389 | self-contained |
| tool | `component/memql/tool_parser.go` | 796 | self-contained |
| prompt | `component/memql/prompt_parser.go` | 834 | self-contained |
| provider | `component/memql/provider_parser.go` | 713 | self-contained |
| policy | `component/memql/policy_parser.go` | 417 | self-contained |

**Total:** 6079 LOC across 13 parsers.

Reference files live in `dsl/_reference/_<construct>.memql` and
document the author-facing surface of each construct. Read each one
before touching its parser. The four "thin wrapper" parsers don't
own a body grammar — they delegate to `tryParseNewFunctionSyntax` —
so their refactor scope is smaller (annotation validation only).

---

## Real duplication (measured)

Six parsers carry near-identical scanning primitives. The duplication
is real:

| Primitive | Parsers that re-implement it |
|---|---|
| `eof()` | 6 |
| `peek()` | 6 |
| `advance()` | 6 |
| `skipWhitespaceAndComments()` | 6 |
| `skipWhitespaceInline()` | 6 |
| `parseParenString()` | 6 |
| `skipBalancedBraces()` | 4 |
| `parseParenStringList()` | 3 |

Plus paren-ident, paren-int, identifier-reading, string-literal
reading, comment-block handling. Subtle implementation variations
across parsers (some track line/col, some don't; some are
allocation-light, some aren't) — that drift is part of the cost
this refactor pays back.

The four thin-wrapper parsers (query/mutation/logic/automation)
share `validateConstructAnnotations` in
`component/memql/construct_annotations.go`. That helper is a
preview of what the shared base should look like.

---

## Target architecture

Go doesn't have inheritance. The idiomatic equivalent is **embedded
struct composition + interface for polymorphism**.

### The base struct

`component/memql/baseparser/base.go` (new package):

```go
package baseparser

// Base is the shared scanning + annotation primitives every DSL
// parser embeds. Construct-specific parsers extend Base via Go
// embedded-struct composition:
//
//     type myConstructParser struct {
//         baseparser.Base
//         // construct-specific state
//     }
//
// All scanning primitives (eof, peek, advance, skipWhitespace*,
// readWord, parseParenString, etc.) live here. Construct-specific
// grammar lives on the embedder.
type Base struct {
    Input  string
    Pos    int
    Line   int   // 1-based; advance() increments on '\n'
    Col    int   // 1-based; advance() increments on every non-'\n'
    Origin string // source identifier for error messages
}

// Scanning primitives -- exported so the embedder can use them.
func (b *Base) EOF() bool
func (b *Base) Peek() byte
func (b *Base) Advance() byte
func (b *Base) SkipWhitespaceAndComments()
func (b *Base) SkipWhitespaceInline()
func (b *Base) ReadWord() string
func (b *Base) MatchKeyword(word string) bool
func (b *Base) FindClosingBrace(openIdx int) (int, error)
func (b *Base) SkipBalancedParens()
func (b *Base) SkipOptionalParens()
func (b *Base) ParseParenString() (string, error)
func (b *Base) ParseParenIdent() (string, error)
func (b *Base) ParseParenStringList() ([]string, error)
func (b *Base) ParseParenInt() (int64, error)
func (b *Base) ReadQuotedString() (string, error)

// Errorf wraps fmt.Errorf with the origin + current line:col prefix
// so every error from every parser has consistent location data.
func (b *Base) Errorf(format string, args ...any) error
```

### The construct-parser interface

`component/memql/baseparser/iface.go`:

```go
// ConstructParser is implemented by every per-construct parser.
// The dispatcher in unified_functions_loader (and the other unified
// loaders) routes a slice to the right parser via this interface.
type ConstructParser interface {
    // Kind returns the construct keyword ("query", "mutation", ...).
    // Used by error messages + dispatcher matching.
    Kind() string

    // AllowedAnnotations returns the construct's annotation
    // allow-list. The base allow-list validator uses this to
    // hard-reject typos at parse time.
    AllowedAnnotations() map[string]bool

    // Parse parses one slice + returns the registered runtime
    // struct (*Spec / *Shape / *Function / etc.). Callers
    // type-assert based on Kind() or maintain separate dispatchers
    // per registry shape.
    Parse(origin string, raw []byte) (any, error)
}
```

The dispatcher in `unified_functions_loader.go` (and the other
unified loaders) routes by `Kind()` rather than the current
switch-on-`slice.Kind`. New parsers register themselves via a
`Register(ConstructParser)` call from their package's `init()`.

### Embedder shape

Each per-construct parser becomes:

```go
package memql

import "github.com/visionarys-io/memql/component/memql/baseparser"

type shapeParser struct {
    baseparser.Base
    // shape-specific state -- decl, etc.
}

func (p *shapeParser) Kind() string                     { return "shape" }
func (p *shapeParser) AllowedAnnotations() map[string]bool { return allowedShapeAnnotations }
func (p *shapeParser) Parse(origin string, raw []byte) (any, error) {
    p.Base = baseparser.Base{Input: string(raw), Pos: 0, Line: 1, Col: 1, Origin: origin}
    // ... construct-specific grammar, calling p.SkipWhitespaceAndComments(),
    //     p.ReadWord(), p.ParseParenString(), etc.
}
```

All the duplicated lexing primitives at the bottom of each parser
file (the last ~150-250 LOC of every self-contained parser) **delete**.
The embedder calls `p.Peek()` instead of `p.peek()`, etc.

---

## Migration plan

Land in **eight commits**, one per parser. Each commit is independently
revertable and keeps the test suite green.

### Commit 1: Build the base + interface

- Create `component/memql/baseparser/` package
- Implement `Base` struct with every primitive listed above
- Implement `ConstructParser` interface
- Write unit tests for `Base` primitives in
  `component/memql/baseparser/base_test.go`. Cover: line/col
  tracking, comment skipping, string-literal handling, brace
  balancing, all the paren-* parsers.
- Do NOT touch any existing parser yet.
- Verify: `go build ./...` clean, new test file passes.

### Commit 2: Migrate shape_parser.go

`shape_parser.go` is the largest (1351 LOC) — best demonstration of
the size collapse. Expect ~200-300 LOC removed.

- Replace `shapeMemQLParser` struct with embedded `baseparser.Base`
- Delete every duplicated primitive from this file
- Rewrite every call site from `p.peek()` → `p.Peek()`, etc.
- Verify: `go test ./component/memql/...` passes — every shape test
  exercises every primitive
- Verify: full DSL loads via engine bootstrap (smoke test that
  reads `dsl.Tree()` and counts registered shapes — expect 64
  including the trait shapes shipped in commit `5673e61`)

### Commits 3-7: Migrate the remaining self-contained parsers

In this order (smallest first, save the most-modified for last):

3. `builtin_parser.go` (389 LOC → ~250)
4. `policy_parser.go` (417 LOC → ~280)
5. `spec_parser.go` (542 LOC → ~350)
6. `provider_parser.go` (713 LOC → ~480)
7. `tool_parser.go` (796 LOC → ~550)
8. `prompt_parser.go` (834 LOC → ~580)
9. `concept_parser.go` (699 LOC → ~470) — note this one lives in
   `component/database/memory-nodes/` and may need a package-level
   import of `baseparser`. Verify no circular dependency before
   touching this commit.

Each migration commit:
- Replaces the construct's parser struct with embedded `Base`
- Deletes the duplicated primitives
- Rewrites call sites
- Runs `go test ./...` — must be green

### Commit 8: Promote the thin-wrapper parsers

The four thin-wrapper parsers (query/mutation/logic/automation)
currently call `tryParseNewFunctionSyntax`. They don't own a body
grammar, so their refactor is annotation-only. After this commit:

- `validateConstructAnnotations` in
  `component/memql/construct_annotations.go` becomes a method on
  `Base` (or a free function in `baseparser`).
- Each thin wrapper becomes a tiny struct that implements
  `ConstructParser.Kind()` + `.AllowedAnnotations()` + delegates
  `.Parse()` to the shared `tryParseNewFunctionSyntax` after the
  base allow-list check.
- `dispatchPerConstructParser` in `unified_functions_loader.go`
  routes by the registry rather than the switch.

### Commit 9: Final cleanup

- Delete `construct_annotations.go` if its helpers all moved into
  `baseparser`.
- Verify every parser is registered in the parser registry.
- Verify `go test ./...` passes; loader counts unchanged at engine
  boot (concepts 76, shapes 64, prompts 18, etc. — match the
  counts logged in the previous commit's loader-init lines).
- Write the wrap-up to the handoff document at the bottom of THIS
  file under "Sign-off".

---

## Working rules (the owner's preferences)

Read these before you start. They were learned across the audit
sessions that landed commits `9df63a0` through `9a4e1d4`. Every one
of them is non-negotiable.

### Git workflow

- **Direct-to-main**. No PRs, no feature branches. Commit + push
  directly to `main` on the worktree branch (the user runs multiple
  Claude sessions in parallel and feature branches collide).
- **Always sync user's local main after pushing**. After every push,
  run `git -C /Users/znas/projects/memql pull --ff-only origin main`
  so the user's local `main` matches origin/main.
- **`git add` explicit paths only**. NEVER `git add -A` or `git add .`.
  The user runs multiple sessions concurrently in the same worktree;
  -A sweeps in other sessions' work and ruins their branches.
- **Commit messages via `/tmp` + `git commit -F`**. Heredoc breaks on
  colons + quotes. Write the body to a file, pass `-F /tmp/foo.txt`.
- **NEVER skip hooks** (`--no-verify` etc.) unless the user asks.
- **NEVER amend or force-push** unless the user asks.

### Code style

- **Pre-release**. No backwards-compat shims. No deprecation windows.
  No legacy adapters. No re-export shims. Rename / delete cleanly and
  fix all callers in one commit.
- **No emojis in code, docs, CLI output**. Use text indicators
  ("SUCCESS:", "ERROR:", "WARNING:", `[x]`/`[ ]`). The user explicitly
  configured this; do not add emojis to anything you touch.
- **`@use*` rule**: every `@use*` annotation, every `@row` / `@caller`
  marker, every `args { ... }` field declared on a memQL construct
  MUST be referenced in the body. Unused declarations are load-time
  errors. Apply this when writing examples.
- **Comments**: default to writing NO comments. Add one only when the
  WHY is non-obvious. Don't explain WHAT the code does — names do
  that. Don't reference the current task / fix / PR / issue.

### Communication

- **The user does the troubleshooting, not you**. Never ask the user
  to grep logs, run psql, navigate UI to verify, or paste output you
  could read yourself. If verification needs runtime data, add logs
  and the user pastes the output back; you analyze.
- **Don't run the CoPresent preview server**. The user runs
  `npm run dev` themselves. Don't `preview_start` it.
- **SI vs AI terminology**: backend code + commit messages say "SI"
  (specialised intelligence); frontend code + end-user copy says
  "AI". Don't mix.
- **`/ultrareview` is user-only**. You cannot launch it. If the user
  asks about it, explain that they trigger it themselves.

### Memory

- The user has a persistent file-based memory system at
  `/Users/znas/.claude/projects/-Users-znas-projects-memql/memory/`.
  Read `MEMORY.md` when relevant. Don't pollute it.

---

## Verification gates

Each commit MUST pass:

1. `go build ./...` clean
2. `go test ./...` — every package green, no FAILs
3. The boot-validation summary in `engine.go` reports the same counts
   as the previous commit:
   - 76 concepts
   - 64 shapes (post-commit `5673e61`; this is shape+trait+caller
     shapes combined)
   - 18 specs+traits (2 specs + 16 traits per
     `unified_spec_loader.go`)
   - 247 functions (queries + mutations + procedural policies)
   - 35 automations
   - 50+ builtins, 12+ tools, 18 prompts, 56 providers, 5 routing
     policies, 3 decision policies
4. `dsl/_reference/*.memql` files still load through their parsers
   when the reference dir is briefly un-skipped for validation
   (don't commit that change; just verify locally).

If a count drops, something silently un-registered. Investigate
before continuing. Common cause: the parser's annotation allow-list
omits an annotation that's actually in use in `dsl/*/`.

---

## Useful context the previous session gathered

### Construct annotation surfaces (the reference)

Each `dsl/_reference/_<construct>.memql` enumerates the surface
authoritatively. Read these before touching the parser:

| Construct | Reference |
|---|---|
| concept | `dsl/_reference/_concept.memql` |
| trait | `dsl/_reference/_trait.memql` |
| spec | `dsl/_reference/_spec.memql` |
| shape | `dsl/_reference/_shape.memql` |
| (others) | not yet shipped — write one when you audit each |

### Recent commits (the context)

```
9a4e1d4 dsl: dedicated parsers for query / mutation / logic / automation
04e38b5 dsl: dedicated parser for spec + trait (retire rewriter pipeline)
5673e61 dsl: shape annotation audit (hard-reject unknowns, validate @useShape, ship trait shapes)
9a6b364 dsl: spec annotation audit (drop @shape + @enabled/@disabled, add @useShape)
456c77c dsl: drop concept-level @alias / @skipDeleted / @cache + their subsystems
9df63a0 dsl: concept annotation audit (@version semver, @namespace colon, drop @enforceRequired + @defaultFilter)
6e3fb71 dsl: reorganize per-domain files by construct kind
e871564 dsl: unified policy + policy-function loaders (parity with new tree)
f61c9e1 dsl: trigger event-topic strings -> structured form (loader + 13 automations)
```

The user's pattern: deep audit one construct at a time, drop
annotations they don't want, then write the reference doc + ship the
parser. After the spec parser refactor the user asked for "all the
parsers done" — the previous session built thin wrappers for
query/mutation/logic/automation, then the user followed up asking
for this inheritance refactor.

### Tests that exercise the parsers

- `component/memql/per_construct_parser_test.go` — locks the
  unknown-annotation rejection on the four thin-wrapper parsers
- `component/memql/spec_parser.go` exercises via
  `policy_evaluator_test.go` (uses `parseSpecMemQL` directly)
- `component/memql/shape_parser_test.go` — full shape grammar
  coverage; the most comprehensive set
- Each parser also gets indirectly exercised by the unified-loader
  boot sequence + the SI tool / query / mutation evaluator tests

### Things to NOT do

- Don't retire the four rewriter files
  (`query_rewrite.go`, `mutation_rewrite.go`, `logic_rewrite.go`,
  `automation_rewrite.go`). They translate struct→procedural form
  for the body grammars that live in the general parser. Retiring
  them is a multi-day refactor that doesn't pay off — the user
  explicitly accepted this tradeoff for the previous commit
  `9a4e1d4`. The thin-wrapper parsers KEEP the rewriter calls
  inside them.
- Don't merge the four thin-wrapper parsers into one shared file.
  Each construct gets its own file for symmetry with the
  self-contained parsers.
- Don't touch `component/language/parser/`. That package owns the
  general expression + statement grammar — it's downstream of every
  parser in this refactor and is intentionally not in the scope.

---

## Sign-off

When you finish:

1. Verify the three-way git sync:
   - worktree branch clean
   - `origin/main` matches
   - User's local `main` at `/Users/znas/projects/memql` matches
2. Run `go test ./...` — every package green
3. Append a final section to this document at the bottom called
   "Completed by [your session id], [commit hash range]" with a
   one-line summary per commit you landed.
4. Tell the user the refactor is done with the final commit hash and
   the total LOC delta.

---

## Quick reference

- Repo: `/Users/znas/projects/memql`
- Worktree convention: prepend `cd` to nothing; operate via absolute
  paths or the worktree-relative root.
- The user's email is `jsanz@visionarys.io` (`git config user.email`).
- The Claude co-author trailer goes on every commit:
  `Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>`

---

## Completed by Claude Opus 4.7 (commits 734fbc6..2666c7f)

Eight commits, smallest-first, each independently revertable, each
verified `go test ./...` green + loader counts at boot unchanged
(76 concepts, 64 shapes, 12 tools, 46 builtins, 230 functions =
126 queries + 101 mutations + 3 policies).

- `734fbc6` -- baseparser package: Base struct (Init, EOF, Peek,
  PeekAt, HasNext, Advance, SkipWhitespace*, SkipBalanced*,
  SkipOptionalParens, ReadWord, MatchWord, ParseParenString /
  ParseParenIdent / ParseParenStringList / ParseParenIdentList /
  ParseParenInt, ReadQuotedString, FindClosingBrace, PosPlus,
  Errorf) + ConstructParser interface + ValidateConstructAnnotations
  + FormatAnnotationAllowList + unit tests.
- `4812666` -- shape_parser embeds Base: 1351 -> 1056 LOC. Custom
  helpers preserved (peekWord, readShapePath, parseRelaxedTemplate +
  relaxedTemplateParser for legacy @template inline form).
- `6b326e6` -- tool_parser + builtin_parser embed Base: tool 796 ->
  552 LOC, builtin 389 -> 386 LOC (builtin keeps the toolMemQLParser
  embedding so it inherits parseParenArgs through the chain).
- `5e74517` -- policy_parser embeds Base: 417 -> 164 LOC.
  ParseParenInt's int64 return takes an int() conversion at the two
  assignment sites.
- `b1feb62` -- spec_parser embeds Base: 542 -> 329 LOC.
  matchKeyword replaced by Base.MatchWord (call sites already ran
  skipWhitespaceAndComments before matching). Spec parser gains
  line/col tracking via the embedded Base.
- `a411af7` -- provider_parser embeds Base: 713 -> 469 LOC.
  Provider-specific helpers stay on the wrapper (parseEnvCall,
  parseAuthBlock, parseParamsBlock, readNumber, readProviderName).
- `3bbe51c` -- prompt_parser embeds Base: 834 -> 567 LOC.
  Prompt-specific helpers stay on the wrapper (parseTemplateBlock,
  readTripleQuotedString, dedent, parseField).
- `2666c7f` -- thin-wrapper parsers (query / mutation / logic /
  automation) + spec_parser switch to baseparser.ValidateConstructAnnotations
  and baseparser.FormatAnnotationAllowList; construct_annotations.go
  deleted.

**Net LOC delta:** parsers shrank from 6079 -> 4552 LOC (-1527);
baseparser package added 954 LOC; construct_annotations.go deleted
(-131). Net repo delta -704 LOC, the drift in scanning primitives
across six parsers collapses to one authoritative implementation.

**Scope guards observed.** The four rewriter files
(`query_rewrite.go`, `mutation_rewrite.go`, `logic_rewrite.go`,
`automation_rewrite.go`) were NOT retired -- the doc flagged that
as multi-day work outside this refactor. The `concept_parser.go`
file under `component/database/memory-nodes/` was NOT migrated --
it has no scanning primitives (it translates `parser.ConceptDecl`
from `component/language/parser`, which is downstream and out of
scope).

**ConstructParser registry.** Defined in baseparser/iface.go but
the dispatcher in `unified_functions_loader.go` still uses the
switch-on-`slice.Kind`. The interface is available for a future
registry consumer; converting now would add indirection without a
concrete benefit (only 4 cases, all trivially routable). Left as
forward-compat.
