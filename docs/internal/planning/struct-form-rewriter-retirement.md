---
title: Struct-form rewriter retirement -- planning doc
audience: internal
status: draft
area: internal
sinceVersion: 0.9.0
owner: znas
---

# Struct-form rewriter retirement -- planning doc

**Status:** deferred. Not blocked on anything; deferred because the
work needs coordinated grammar + AST + evaluator changes that don't
fit in a single coherent commit.

**Last landed:** [`daaec10`](https://github.com/znasllc-io/memql/commit/daaec10)
consolidated the five rewriter files
(`query_rewrite.go` / `mutation_rewrite.go` / `logic_rewrite.go` /
`automation_rewrite.go` / `args_rewrite.go` plus `normalise_all.go`,
1306 LOC) into a single
[`component/language/parser/rewriter.go`](../../component/language/parser/rewriter.go)
at 936 LOC. The "five rewriters" stop existing as separate concerns
but the rewriter as a concept stays -- it still pre-processes
struct-form source into procedural source before tokenization.

This doc captures the actual retirement path: deleting `rewriter.go`
entirely by teaching the parser struct-form grammar natively.

---

## Why this is multi-day work

A failed attempt during the cleanup audit pass tried to embed the
rewriter into `parser.NewLexer` so callers wouldn't need to invoke
it explicitly. That broke four tests in
`component/memql/declared_usage_validator_test.go` plus
`TestParser_ConditionalFilterWithArgsFieldName`. The failure mode
showed why "just delete the rewriter" doesn't cleanly land in one
commit:

1. The rewriter has TWO outputs that downstream code depends on
   (procedural source AND a specific args/concept binding format).
2. Other source-level transformations
   (`translateConceptPathsToPayload`, `args.X -> ctx.X`) run AFTER
   the rewriter today. Those transformations expect the rewriter's
   normalized output. If the rewriter shifts position in the
   pipeline, they mangle un-rewritten input.
3. The parser already handles `args.X` natively as `ArgRefExpr`,
   but the rewriter translates `args.X -> ctx.X`, producing
   different AST shapes for the same author syntax. The engine
   evaluator has paths for both.

A native-grammar approach has to coordinate three changes; doing
them in isolation breaks tests.

---

## The three coupled changes

### 1. Native struct-form productions in `parser.go`

[`component/language/parser/parser.go:299-356`](../../component/language/parser/parser.go)
(`parseDefinition`) currently dispatches on `TokenKeywordFunc` and
the contextual `concept` identifier. Add three more contextual
dispatches:

- `query <NAME> { args, filter, shape, sort, paginate, asOf }` ->
  `*FunctionDef{Receiver: Query, Body: shape(concept==X;filter, "shape")}`
- `mutation <NAME> { args, insert <concept> {...} | update <concept> {...} }` ->
  `*FunctionDef{Receiver: Mutation, Body: insert(...) or update(...)}`
- `logic <NAME> { args, body { ... } }` ->
  `*FunctionDef{Receiver: Logic, Body: <body statements>}`
- `automation <NAME> { step <name> { ... } ... }` ->
  `*FunctionDef{Receiver: Automation, Body: <step assignments>}`

The lexer already treats lowercase `query`, `mutation`, `logic`,
`automation` as identifiers (capitalized variants are keywords --
see [`component/language/parser/lexer.go:572-577`](../../component/language/parser/lexer.go)).
No lexer change needed.

The hard part: each `parseStructX` needs to BUILD the Body
`ExpressionNode` AST without going through source roundtrip. The
existing expression parser uses tokens; the struct-form fields
(`filter`, `shape`) need to be tokenized and parsed inline. This
is doable -- the parser methods can call `parseExpression()` on
the next tokens until end-of-line -- but the line-vs-token
boundary is fiddly.

Estimated: ~500 LOC of new parser code in
`component/language/parser/parser.go` or a new sibling file
`struct_form_parser.go`.

### 2. AST-level concept-name resolution

[`component/memql/function_loader.go:118-141`](../../component/memql/function_loader.go)
(`tryParseNewFunctionSyntax`) runs
`translateConceptPathsToPayload(content, name)` on the SOURCE
string. This naive `\b<name>\.` regex replacement converts
`space.X` references inside the function body to `payload.X` so the
engine's existing `payload.X` evaluator path keeps working.

Two problems with leaving this as-is:

- It runs on raw source, so it mangles the `insert space {` block
  header in a struct-form mutation source unless the rewriter has
  already converted that to `insert(<fully-qualified-id>, ...)`.
  This is what broke during the lexer-embed attempt.
- It's a load-bearing source transformation that exists because the
  parser/AST converter doesn't know about `@useConcept(name)`
  bindings at AST construction time.

Replace with: when building the FunctionDef AST in #1, the
`@useConcept(<name>)` annotation is attached to the FunctionDef
struct. The AST converter (or an early pass before it) walks the
Body ExpressionNode tree and rewrites identifier references to
`payload.X` when their head matches the bound concept's bare name.

Estimated: ~150 LOC change. Lives in
`component/memql/concept_resolver.go` (already exists for similar
post-parse work).

### 3. Reconcile `args.X` handling

The parser has native `args.X -> ArgRefExpr` conversion at
[`component/language/parser/parser.go:3720-3732`](../../component/language/parser/parser.go).
The rewriter translates `args.X -> ctx.X` BEFORE lexing, which
means most struct-form-emitted source never hits the parser's
ArgRefExpr path. Procedural-form-authored sources that include an
`args { ... }` file-top block DO hit it (see
`TestParser_ConditionalFilterWithArgsFieldName`).

To eliminate the rewriter, pick ONE canonical form:

**Option A: parser-native ArgRefExpr everywhere.** Remove the
`args.X -> ctx.X` translation from the rewriter. The engine
evaluator already has an ArgRefExpr handler at
[`component/memql/executor_filter.go`](../../component/memql/executor_filter.go)
and a few other sites (search for `*ArgRefExpr`); audit them and
add coverage for the contexts that today rely on ctx.X form.

**Option B: never use ArgRefExpr.** Remove the parser's `args.X ->
ArgRefExpr` conversion. Force everything through ctx-form. This
loses the explicit author-surface distinction but simplifies the
runtime.

Option A is more in line with the existing parser design (ArgRefExpr
is a real AST node, not a placeholder). Estimated: ~200 LOC of
engine evaluator changes + ~150 LOC of test coverage to ensure no
silent semantic shifts.

### 4. Other call sites that disappear

Once #1-#3 land, these explicit rewriter calls go away:

- [`component/memql/function_loader.go`](../../component/memql/function_loader.go) --
  `languageParser.NormaliseAll` invocation in
  `tryParseNewFunctionSyntax`.
- [`component/language/compiler/api.go`](../../component/language/compiler/api.go) --
  `parser.NormaliseAll` in `CompileSource`, `ParseMemQL`,
  `ValidateMemQL`, `applyFullRewriteChain`.
- [`component/automations/loader.go`](../../component/automations/loader.go) --
  the `LooksLikeStructLogic`/`NormaliseLogicSource` +
  `LooksLikeStructAutomation`/`NormaliseAutomationSource` pair in
  `parseResolveCompile`. The `LooksLikeLegacyAutomation` rejection
  stays (it's a real product behavior) but the regex inlines.

Plus the test files in `component/language/parser/`:
- `automation_within_automation_test.go`
- `logic_automation_rewrite_test.go`
- `mutation_rewrite_test.go`

These need updating to either exercise the new native grammar path
or use lower-level `parseExpression`-style calls. If the parser
produces correct FunctionDefs, the test rewrite is straightforward.

---

## Recommended order

1. **Item 2 first (AST-level concept resolution).** It's the load-
   bearing piece blocking #1. Adding `@useConcept` resolution to the
   AST converter is mechanically straightforward and can ship as a
   standalone commit with tests verifying the existing rewriter
   output and the new AST-resolved output produce identical
   `Function` registrations.

2. **Item 3 (args.X reconciliation).** Pick Option A. Audit
   evaluator paths for ArgRefExpr handling; add coverage. Ship as a
   standalone commit. After this commit, the rewriter's
   `translateArgsRefsToCtx` becomes a no-op and can be deleted (no
   change to rewriter callers).

3. **Item 1 (native grammar).** Add the struct-form parser
   productions; verify against the same `Function` outputs as the
   rewriter. Delete `rewriter.go`. Update callers
   (function_loader, compiler/api.go, automations/loader.go) to
   stop invoking `NormaliseAll` -- the parser now handles
   struct-form natively.

Each commit independently revertable, each leaves the tree in a
green state.

---

## Estimated total cost

| Step | LOC | Risk |
|---|---|---|
| 1. Native grammar | +500 / -936 | Medium -- new AST construction paths |
| 2. AST concept resolution | +150 / -50 | Low -- well-bounded source-to-AST swap |
| 3. args.X reconciliation | +200 / -50 + test coverage | High -- engine semantic risk |
| Caller updates | -100 (cleanup) | Low |
| **Net** | **~-300 LOC** | **High overall** |

The work pays back ~300 LOC of removal plus eliminates the
preprocessor-style transformation layer entirely. The cost is real
multi-commit coordination with engine semantic verification.

---

## When to do this

When one of these things motivates the work:

- A new construct kind wants to land as struct-form, and adding a
  sixth rewriter feels worse than investing in native grammar.
- An engine semantic bug surfaces from the rewriter's source
  transformations interacting unexpectedly with downstream rewrites.
- The team has a stretch of focus to dedicate to a multi-commit
  refactor with cross-package test coverage.

Until one of these arrives, the 936-LOC `rewriter.go` is the
practical landing point. It's one cohesive file with a clear API
(`NormaliseAll` + per-construct helpers); no duplication; no
five-file footprint.
