# DSL v1 expression language Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. This plan is deleted in the epic's merge.

**Goal:** One expression grammar with a named lambda parameter at every predicate position, one lowering to SQL checked at load, one in-process evaluator, and the four string evaluators deleted (epic memql#5363, tasks #5364-#5369).

**Architecture:** The parser gains a v1 expression grammar (CEL-shaped precedence, `!`, `.?`, `? :`, lambdas) that produces a small, uniform set of AST nodes. Authored positions parse in v1 mode; the engine's internal query form (the wire string an SDK sends to `Execute`, and the rewriter's generated wrapper) keeps its grammar. `component/language/tiers` and `component/language/functions` are data: which node kinds and functions each position admits, and one entry per function. `component/memql` holds the two evaluators: `Lower` (v1 AST to the executor's IR, then SQL, run at `Init` for every pushdown position) and `EvalExpr` (in process, every other position). The four string scanners' positions consume the parsed AST through `EvalExpr`, and the codemod `memqlmigrate --rewrite=expressions` migrates the whole tree in the same PR.

**Tech Stack:** Go 1.26 multi-module workspace; PostgreSQL 16 + TimescaleDB for db-gated tests (throwaway on port 15434); TypeScript for the VS Code extension.

**Spec:** `docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md`, section 4, epic 2; decisions D1, D6-D11, D23, D24.

## Global Constraints

- Branch `epic/dsl-v1-expressions`, based on epic 1's seam commit `fd035899b` (`origin/epic/dsl-v1-foundations`); one PR against `main`, merged after epic 1's PR.
- Verify with the MODULE PATH, never `go test ./...`: `go test github.com/znasllc-io/memql/component/language/...`, `make test`, and for db-gated trees `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=postgres://memql:memql_dev@localhost:15434/memql?sslmode=disable go test -count=1 ./component/memql/...` (and `./component/automations/...`).
- Stage files by explicit path; never `git add -A` / `git add .`.
- No backwards-compat shims: when a form is retired the parser refuses it naming `memqlmigrate --rewrite=expressions` and the replacement, and the tree is migrated in the same PR.
- The internal query form is a WIRE contract (SDK `conceptBrowser.ts`, `lookups.ts`, `sdk/go/client/concept_browser.go`, row-authz injected predicates, `staged_enforce.go`): its grammar does not change.
- No emojis anywhere. Documentation style per `docs/DOCS_STANDARD.md`.
- `GrammarVersion` moves; after epic 1 lands, the D25 parity gate requires the extension pin, version, CHANGELOG and `parser.EditorRelease` to move in the same PR.
- The arch model (`topology.model.json`) goes stale on new packages; regenerate it last (`make arch-model`), taking epic 1's side on conflict.
- Commit format: `Issue #<N>: <description>` with the attribution trailer.

---

## Decisions this plan makes (from the record, made concrete)

### The surface

| Position | v1 form |
|---|---|
| Query filter | `filter row => row.status == args.status && isActiveRecord(row)` |
| Spec (concept or @row shape) | `spec registration isRevoked = row => row.revoked == true` |
| Spec (@actor shape) | `spec actorEnvelope requiresAdmin = actor => actor.role == "admin"` |
| Trait | `trait isActiveRecord = row => row.active == true` |
| Predicate use | `isActiveRecord(row)`, `requiresOwner(actor)` -- a spec or trait is applied to its receiver |
| Trigger filter | `@filter(row => row.status == "archived")` -- `row` is the triggering row |
| Collection method | `row.tags.any(t => t == "urgent")`, `args.members.where(m => m.active)` |
| Conditional value | `p ? a : b` (was `cond(p, a, b)`) |
| String join | `a + b` (was `concat(a, b)`) |
| Presence | `x != nil` (was `exists(x)`, which also treated a blank string as absent: the codemod writes `(x != nil && x != "")` for those two call sites) |
| Length | `x.count()` (was `len(x)`, `count(x)`) |
| Substring test | `s.includes(sub)` (was `contains(s, sub)`; `contains(<lambda>)` stays the graph traversal) |
| Collection membership | `v in list` (the `.contains(v)` collection method is retired) |
| Optional argument guard | `args.x == nil \|\| row.f == args.x` under `&&`, `args.x != nil && row.f == args.x` under `\|\|` (was `when(args.x) { row.f == args.x }`) |
| Absent value | `nil` (was also `null`) |
| In-process over a page | `refine row => <in-process expression>` after `paginate N` |

A lambda parameter is any identifier that is not a reserved root, except that the parameter of a spec over an @actor shape is spelled `actor` and IS the actor envelope. The codemod always writes `row`, `actor`, and for nested lambdas a name derived from the collection (`t`, `m`, ...).

Bare names in a v1 expression resolve, in order: a lambda parameter in scope, a reserved root (`args`, `actor`, `now`, `config`, `event`, plus the automation roots `steps`, `item`, `index`, `input`), a body-local or step name, a predicate (spec or trait) when called, a catalog function when called. A payload field is never a bare name; it is always `<param>.field`.

### Precedence (D9), tightest first -- published in memql.md and pinned by `TestV1PrecedenceTableIsPublished`

| Level | Operators | Associativity |
|---|---|---|
| 1 | `.f` `.?f` `f(...)` `.m(...)` | left |
| 2 | `!` `-` (unary) | right |
| 3 | `*` `/` `%` | left |
| 4 | `+` `-` | left |
| 5 | `??` | left (folds n-ary) |
| 6 | `==` `!=` `<` `<=` `>` `>=` `in` `startsWith` | none (`a < b < c` refuses) |
| 7 | `&&` | left |
| 8 | `\|\|` | left |
| 9 | `c ? a : b` | right |
| 10 | `x => body`, `(x, y) => body` | right; the body extends as far as it can |

Retired and refused in v1 positions, each naming `memqlmigrate --rewrite=expressions` and its replacement: `;` and `,` as connectives, `and(...)`, `or(...)`, `not(...)`, `lt/gt/lte/gte(...)`, `when(args.x) { }`, `?.` (the conditional-filter prefix), `has`, `not in` (write `!(x in list)`), `cond(`, `concat(`, `coalesce(`, `exists(`, `len(`, `count(` (as a function), `contains(str, sub)`, `null`, a bare payload field, `spec ... { return ... }` bodies, `trait ... { return ... }` bodies, `@filter` without a lambda, `$args.` in a tool handler.

### The absence table (D8) -- one table, reproduced by both evaluators, pinned by the corpus and the differential lane

"Absent" means the key is missing OR its value is JSON null.

| Expression | `x` absent | Notes |
|---|---|---|
| `x == v` (v not nil, not `""`) | false | |
| `x == ""` | **true** | an absent string equals blank (completes rule 27; lowers to `COALESCE(x, '') = ''`) |
| `x != v` (v not nil, not `""`) | true | `IS DISTINCT FROM` |
| `x != ""` | false | `COALESCE(x, '') <> ''` |
| `x == nil` | true | `IS NULL` |
| `x != nil` | false | `IS NOT NULL` |
| `x < v`, `<=`, `>`, `>=` | false | |
| `x in list`, `v in x`, `x startsWith p`, `s.includes(t)` | false | |
| `!e` | negation of `e` | every lowered predicate is two-valued, so `!(x == v)` is exactly `x != v` |
| `a ?? b` | `b` | also when `a` is a blank or whitespace-only string (rule 30) |
| `x.f`, `x.?f` | absent | both propagate absence at run time; `.?` is REQUIRED at load when the object may be absent (an optional object field, an optional arg, an untyped value), and `.` is refused there with the `.?` spelling |

Typed equality: `1 == "1"` is false; numbers compare numerically across int and float. Strings order by byte (`COLLATE "C"` in SQL), so RFC3339 UTC timestamps order correctly. There is no truthiness: a condition must be boolean, statically where the type is known (refused at load, naming the type) and at run time otherwise (`condition_not_boolean`).

### Tiers (D11)

`P` positions push down to SQL: `queryFilter`, `sort`, `specBody`, `rowAuthzArgument`. `M` positions evaluate in process: `automationCondition`, `triggerFilter`, `logicBody`, `mutationValue`, `stepArgument`, `toolDefault`, `promptInput`.

In a `P` position, a subexpression that does not read the row parameter is a **plan constant**: it is evaluated once per call by `EvalExpr` before the query runs and bound as a parameter. So any `M` function is usable in a `P` position on plan-constant inputs (`row.expiresAt < addDuration(now, "P1D")`), and `args.x == nil || row.f == args.x` folds to `TRUE` or to `row.f = $1` before SQL. A row-dependent subexpression must be a `P` node kind or a `P` function; anything else refuses at load naming the node, the position and the nearest `P` spelling. A collection method in a `P` position needs a bounded source: a row array field (lowers to `jsonb_array_elements`), or a plan constant; a query result is unbounded and refuses.

`refine row => <M expression>` is the one named in-process construct: it requires `paginate`, runs after the SQL page is read, and may return fewer rows than the page size.

The `M` tier carries a static cost estimate (refused at load above `tiers.MaxStaticCost`, default 1,000,000) and a runtime step budget (`tiers.DefaultStepBudget`, default 1,000,000 node evaluations per call; exceeding it is `expression_budget_exceeded`).

### Behaviour changes the new evaluator makes (called out in the PR body)

- `dsl/identity/logic.memql` account-deletion reminders: E3 compared against the literal string `"now"`, so the gate was constant false; `now` is now the clock and the reminders fire.
- `dsl/data/logic.memql:41` `if !matchingConfirmed.empty()`: E3 read the call as truthy text, so the negation was constant false; conflicts are now emitted.
- Trigger `@filter` literals keep their quotes (E3 dropped them).
- `x == ""` matches an absent field (two filters: `dsl/work/queries.memql:141`, `dsl/identity/queries.memql:902`; both writers stamp the field, so rows are unchanged -- Task 6 verifies).
- Mutation payload string literals keep their type (`label: "123"` stays a string; E4 decoded it to int64).

---

## File Structure

| Path | Responsibility |
|---|---|
| `component/language/ast/v1.go` (new) | v1 expression nodes: `IdentExpr`, `MemberExpr`, `CallExpr`, `UnaryExpr`, `BinaryExpr`, `ListExpr`, `MapExpr`, `ParenExpr`; reuses `LambdaExpr`, `TernaryExpr`, `LiteralExpr`, `NilExpr`; `Span`; `Walk` |
| `component/language/ast/v1_format.go` (new) | `FormatExpr(node) string` canonical printer |
| `component/language/ast/v1_kind.go` (new) | `NodeKind` values and `KindOf(node)` |
| `component/language/parser/v1_expr.go` (new) | the v1 expression parser (`parseV1...`), `ParseV1Expression`, `ParseV1Lambda` |
| `component/language/parser/v1_refusals.go` (new) | the retired-form refusal table and messages |
| `component/language/parser/lexer.go` | `.?` token (`TokenDotQuestion`); `=>` stays an operator |
| `component/language/parser/parser.go`, `spec_decl.go`, `rewriter.go` | predicate positions: filter lambda emit, spec/trait `=` form, `@filter` lambda, `refine` clause; v1 mode for logic/automation/mutation expressions |
| `component/language/tiers/manifest.go` (new) | positions to tier, node kinds and functions; cost limits |
| `component/language/functions/catalog.go` (new) | one entry per function: name, signature, tier, doc, retired spellings |
| `component/memql/expr_eval.go` (new) | `EvalExpr`, `ExprScope`, the absence table in process, budget |
| `component/memql/expr_lower.go` (new) | `Lower`, plan constants, type check, refusals |
| `component/memql/expr_not.go` (new) | `NotExpression` IR node: SQL, post-filter, walkers |
| `component/memql/expr_collection_sql.go` (new) | `.any/.all/.count()` over row arrays |
| `component/memql/refine.go` (new) | the `refine` clause executor |
| `component/automations/run_scope.go` (new) | the automation run's `ExprScope` (what E3's `resolvePath` served) |
| `component/automations/evaluator.go` | DELETED |
| `component/automations/steps/function.go` | resolver DELETED; args evaluate through `EvalExpr` |
| `component/memql/mutation_templates.go` | string half DELETED |
| `component/memql/tool_execution.go` | `$args` substitution DELETED |
| `cmd/memqlmigrate/expressions.go` (new) | the `expressions` tree rewrite |
| `component/memql/sense/*` | position detection, tier-aware completion and hover |
| `test/conformance/2026/expr/<position>/` | cells for every position, node kind and function |
| `test/conformance/differential_db_test.go` (new) | the differential lane (advisory) |
| `component/language/parser/fuzz_test.go` (new), `component/memql/expr_lower_fuzz_test.go` (new) | fuzz targets |
| `docs/public/language/memql.md`, `functions.md`, `authoring-rules.md`, root `CLAUDE.md` | the language as shipped |

---

## Task 1: The v1 AST, its printer and its node kinds (#5364)

**Files:**
- Create: `component/language/ast/v1.go`, `component/language/ast/v1_format.go`, `component/language/ast/v1_kind.go`
- Test: `component/language/ast/v1_format_test.go`

**Interfaces (Produces):**

```go
// Span locates a node in the source it was parsed from (1-indexed line/column, half-open end).
type Span struct{ Line, Col, EndLine, EndCol int }

type IdentExpr  struct{ Name string; Span Span }                       // row, args, isActiveRecord, now
type MemberExpr struct{ Object ExpressionNode; Field string; Optional bool; Span Span } // x.f / x.?f
type CallExpr struct {
	Receiver ExpressionNode // non-nil for a method call x.m(...)
	Name     string         // function, method, predicate or construct name
	Kind     string         // construct-call prefix: query|mutation|logic|builtin|automation|action|capability; "" otherwise
	Args     []ExpressionNode
	Named    []NamedArg     // construct calls only
	Span     Span
}
type NamedArg   struct{ Name string; Value ExpressionNode }
type UnaryExpr  struct{ Op string; Operand ExpressionNode; Span Span }          // "!" | "-"
type BinaryExpr struct{ Op string; Left, Right ExpressionNode; Span Span }      // * / % + - ?? == != < <= > >= in startsWith && ||
type ListExpr   struct{ Elems []ExpressionNode; Span Span }
type MapExpr    struct{ Entries []MapEntry; Span Span }
type MapEntry   struct{ Key string; Value ExpressionNode }
type ParenExpr  struct{ Inner ExpressionNode; Span Span }
// Reused: LambdaExpr{Params, Body}, TernaryExpr{Condition, Then, Else}, LiteralExpr{Value}, NilExpr{}.

func FormatExpr(n ExpressionNode) string        // canonical v1 source
func WalkV1(n ExpressionNode, visit func(ExpressionNode) bool)
type NodeKind string                             // see v1_kind.go
func KindOf(n ExpressionNode) NodeKind
```

`NodeKind` values (the tier manifest's vocabulary): `ident`, `member`, `optionalMember`, `call`, `methodCall`, `constructCall`, `not`, `negate`, `arithmetic`, `coalesce`, `comparison`, `in`, `startsWith`, `and`, `or`, `ternary`, `lambda`, `list`, `map`, `literal`, `nil`, `paren`.

`FormatExpr` rules: spaces around every binary operator and `? :`; `x => body`; `(a, b) => body`; no space inside parens; strings via `strconv.Quote`; a `ParenExpr` prints its parens; a child whose precedence is looser than its parent's slot gets parens even without a `ParenExpr` (so a hand-built AST prints to source that re-parses to the same tree).

- [ ] **Step 1: Write the failing printer tests** (`v1_format_test.go`): table of hand-built trees and the text each prints to, including `row.status == args.status && isActiveRecord(row)`, `(args.x == nil || row.f == args.x)`, `p ? a : b ? c : d` (right-assoc, no parens), `(p ? a : b) == v`, `"a" + (x ?? "b")`, `!(row.a == 1)`, `row.?lineage.planId`, `row.tags.any(t => t == "x")`, `query activeUsers(status: "active")`, `{a: 1, b: [x, y]}`, `-x * 2`.
- [ ] **Step 2: Run** `go test github.com/znasllc-io/memql/component/language/ast/ -run TestFormatExpr` -- expect compile failure.
- [ ] **Step 3: Implement** the node types (each with `node()` and `expressionNode()`), `FormatExpr` with a precedence function mirroring the table above, `WalkV1`, `KindOf`.
- [ ] **Step 4: Run** the test; expect PASS.
- [ ] **Step 5: Commit** `Issue #5364: the v1 expression AST, its canonical printer and node kinds`.

## Task 2: The v1 expression parser (#5364)

**Files:**
- Create: `component/language/parser/v1_expr.go`, `component/language/parser/v1_refusals.go`
- Modify: `component/language/parser/lexer.go` (add `TokenDotQuestion` for `.?`; `.` followed by `?` inside and after identifiers)
- Test: `component/language/parser/v1_expr_test.go`, `component/language/parser/v1_precedence_test.go`, `component/language/parser/v1_refusals_test.go`

**Interfaces:**
- Consumes: Task 1 nodes.
- Produces:

```go
// ParseV1Expression parses one v1 expression (the whole input). Errors carry line/column and, for a retired form, the replacement and the migrator name.
func ParseV1Expression(src string) (ast.ExpressionNode, error)
// ParseV1Lambda parses `x => body` / `(x, y) => body` and returns the lambda.
func ParseV1Lambda(src string) (*ast.LambdaExpr, error)
// (p *Parser) parseV1Expression() -- the entry the construct parsers call mid-stream; stops at a token that cannot continue an expression (`}` at depth 0, `)` closing an enclosing call, a statement keyword, EOF).
// V1PrecedenceTable returns the table above, for the docs pin.
func V1PrecedenceTable() []PrecedenceLevel
type PrecedenceLevel struct{ Level int; Operators []string; Assoc string }
// RetiredForm is one refused spelling. V1RetiredForms() lists them for Sense and the docs.
type RetiredForm struct{ Spelling, Replacement, Rule string }
func V1RetiredForms() []RetiredForm
```

Grammar (recursive descent, one function per level): `ternary -> or ('?' ternary ':' ternary)?`; `or -> and ('||' and)*`; `and -> relation ('&&' relation)*`; `relation -> coalesce (relop coalesce)?` and refuse a second relop; `coalesce -> additive ('??' additive)*` folded left into nested `BinaryExpr{Op:"??"}`; `additive`, `multiplicative`, `unary -> ('!'|'-') unary | postfix`; `postfix -> primary ('.' IDENT args? | '.?' IDENT)*`; `primary -> lambda | IDENT args? | kind IDENT '(' named ')' | literal | '(' expr ')' | '[' list ']' | '{' map '}' | nil | true | false | now`. A lambda is recognised by lookahead: `IDENT '=>'` or `'(' IDENT (',' IDENT)* ')' '=>'`. A fused dotted identifier (`row.items.any`) from the lexer is split into `IdentExpr` plus `MemberExpr`/method `CallExpr`. A colon-bearing identifier (`v1:crm:lead`) in a v1 position refuses: "write the id as a string: `\"v1:crm:lead\"`".

Refusal message shape (pinned by the corpus): `<spelling> is retired in edition 2026: write <replacement> (memqlmigrate --rewrite=expressions rewrites it)`, e.g. `when(args.x) { ... } is retired in edition 2026: write (args.x == nil || <predicate>) (memqlmigrate --rewrite=expressions rewrites it)`.

- [ ] **Step 1: Write failing parse tests** (`v1_expr_test.go`), each comparing `ast.FormatExpr(ParseV1Expression(src))` against a fully parenthesised expectation printed by a test helper `fullyParen(node)`: `a || b && c` -> `(a || (b && c))`; `a ?? b == c` -> `((a ?? b) == c)`; `!a == b` -> `((!a) == b)`; `a + b * c` -> `(a + (b * c))`; `p ? a : q ? b : c` -> `(p ? a : (q ? b : c))`; `-a.b` -> `(-(a.b))`; `row.?a.b` parses to Member{Member{Ident row, a, Optional}, b}; `row => row.x == 1` parses to a LambdaExpr; `(a, b) => a + b`; `x in [1, 2]`; `row.s startsWith args.p`; `query activeUsers(status: "active")` is a CallExpr with Kind query and one named arg; `row.tags.any(t => t == "x")` is a method CallExpr whose arg is a lambda.
- [ ] **Step 2: Write failing refusal tests** (`v1_refusals_test.go`): each retired spelling from the list above refuses and the message contains both the replacement and `memqlmigrate --rewrite=expressions`; `a < b < c` refuses ("comparisons do not chain"); a trailing operator refuses with a line/column.
- [ ] **Step 3: Write the failing precedence-publication test** (`v1_precedence_test.go`): reads `docs/public/language/memql.md`, finds the table under the heading `### Operator precedence`, and asserts each row equals `V1PrecedenceTable()` (the docs half is written in Task 14; until then the test is skipped with `t.Skip("docs table lands in Task 14")` and Task 14 removes the skip).
- [ ] **Step 4: Run** `go test github.com/znasllc-io/memql/component/language/parser/ -run 'TestV1'` -- expect failures.
- [ ] **Step 5: Implement** the lexer token and `v1_expr.go` / `v1_refusals.go`.
- [ ] **Step 6: Run** the tests; expect PASS. Run the whole parser package: `go test github.com/znasllc-io/memql/component/language/...` -- every existing test still passes (the legacy grammar is untouched).
- [ ] **Step 7: Commit** `Issue #5364: the v1 expression grammar, its precedence and the retired forms refused`.

## Task 3: Predicate positions and in-process positions parse in v1 (#5364)

**Files:**
- Modify: `component/language/parser/rewriter.go` (`parseStructQueryBody`, `buildStructQueryExpr`: emit `concept==<id> && (<lambda>)`, no `;`; add the `refine` clause; the filter value must open with a lambda header, else refuse), `component/language/parser/parser.go` (`parsePrimary` legacy path: a lambda in a predicate operand parses the body with `parseV1Expression`; `parseAttribute` for `@filter`: `@filter(<lambda>)`; `parseConditionExpression`, `parseIfStatement`, `parseForRangeStep`, switch subject, step args, `parseObject` values in mutation bodies: v1 mode), `component/language/parser/spec_decl.go` (`spec <bound> <name> = <lambda>`, `trait <name> = <lambda>`; the brace/`return` form refuses)
- Modify: `component/language/ast/ast.go` (`SpecDecl.Lambda *LambdaExpr`; `TriggerDef.FilterLambda`; `StepDef.ConditionExpr ExpressionNode` beside the canonical string)
- Test: `component/language/parser/v1_positions_test.go`

**Interfaces (Produces):** `SpecDecl.Lambda`, `TriggerDef.Filter` holds `ast.FormatExpr(lambda)`, `StepDef.Condition` holds `ast.FormatExpr(conditionExpr)` (canonical v1 source; the compiler writes it to JSON unchanged, and the runtime parses it once at load with `ParseV1Expression`).

- [ ] **Step 1: Failing tests**: a struct query with `filter row => row.a == args.a && isX(row)` normalises and parses; its return expression is `LogicalExpr{AND, concept==..., LambdaExpr}`; a filter without a lambda header refuses naming the migrator; a multi-line filter whose continuation lines start with `&&`, `||`, `?`, `:` joins; `spec c isY = row => row.a == 1` parses to a `SpecDecl` with `Lambda`; `spec c isY { return a == 1 }` refuses; `@filter(row => row.status == "x")` stores the canonical text `row => row.status == "x"`; `@filter(payload.status == "x")` refuses; an automation `if steps.x.result == true && event.payload.y != nil {` yields a `StepDef.Condition` of `steps.x.result == true && event.payload.y != nil`; `refine row => row.a.includes("x")` without `paginate` refuses.
- [ ] **Step 2: Run** -- fail.
- [ ] **Step 3: Implement.** The rewriter's continuation tables gain `?`, `:`, `=>` (leading) and `?` (trailing). `buildStructQueryExpr` joins with ` && ` and parenthesises the lambda. `refine` emits the internal directive `refine(<base>, <lambda>)` outermost-inside-shape, and requires a `paginate` clause.
- [ ] **Step 4: Run** -- pass; run the parser package.
- [ ] **Step 5: Commit** `Issue #5364: predicate and in-process positions parse the v1 grammar`.

## Task 4: The tier manifest and the function catalog (#5365)

**Files:**
- Create: `component/language/functions/catalog.go`, `component/language/functions/catalog_test.go`, `component/language/tiers/manifest.go`, `component/language/tiers/manifest_test.go`
- Modify: `component/language/dslspec/builtins.go` (the expression builtins project from the catalog), `component/language/dslspec/lexicon.go` (operators from `parser.V1PrecedenceTable`), dslspec drift tests

**Interfaces (Produces):**

```go
package functions
type Tier string
const (TierP Tier = "P"; TierM Tier = "M")
type Param struct{ Name, Type string; Variadic bool }  // Type: string|number|bool|datetime|duration|list|map|any|lambda
type Function struct {
	Name, Doc string
	Receiver  string   // "" for a function; "string"|"list"|"any" for a method
	Params    []Param
	Returns   string
	Tier      Tier     // P functions lower to SQL; M functions evaluate in process (and in a P position only on plan constants)
	Retired   []string // spellings this entry replaces, for refusals and Sense
}
func Catalog() []Function          // sorted by (Receiver, Name)
func Lookup(name string) (Function, bool)
func Method(receiver, name string) (Function, bool)

package tiers
type Tier = functions.Tier
func TierOf(p Position) Tier
func Allows(p Position, k ast.NodeKind) bool
func AllowsFunction(p Position, name string) bool
func NodeKinds(p Position) []ast.NodeKind
const MaxStaticCost = 1_000_000
const DefaultStepBudget = 1_000_000
```

Catalog entries (one spelling each): functions `lower`, `upper`, `trim`, `hash`, `shortId`, `canonicalId(v, "concept")`, `toString`, `addDuration(ts, dur)`, `daysBetween(a, b)`, `error(msg)`; the relationship traversals `parentOf`, `childOf`, `aliasOf`, `equals`, `references`, `contains`, `owns`, `createdBy`, `ids` (P; optional leading label string, then a lambda); string methods `includes`, `startsWith` (operator, listed for docs), `count` (length); list methods `count`, `any`, `all`, `where`, `select`, `first`, `last`, `empty`, `sum`, `min`, `max`, `avg`, `orderBy`, `orderByDesc`, `groupBy`, `distinct`, `take`, `skip`, `nodes` (the method list is exactly `component/memql/collection_method.go`'s table minus `contains`). P: the traversals, `count`, `any`, `all` on a row array, `includes`. Everything else M.

Position rules: every position admits `literal`, `nil`, `list`, `paren`, `ident`, `member`, `optionalMember`, `and`, `or`, `not`, `comparison`, `in`, `startsWith`, `coalesce`, `ternary`. `P` positions add `call` (P functions and predicates), `methodCall` (P methods), `lambda` (as a traversal or method argument). `M` positions add `arithmetic`, `negate`, `map`, `call`, `methodCall`, `lambda`; `logicBody` and `stepArgument` add `constructCall`. `sort` and `rowAuthzArgument` admit only `literal` (string) -- their syntax does not change. `toolDefault` admits only `literal`.

- [ ] **Step 1: Failing tests**: `TestEveryNodeKindIsInATier` (every `ast.NodeKind` constant is allowed by at least one position -- the acceptance test "a test fails when a node kind or function is in no tier"); `TestEveryFunctionIsInATier` (every catalog function is admitted by `AllowsFunction` somewhere); `TestCatalogMatchesTheEvaluators` (every catalog function has an implementation in `component/memql`'s `EvalExpr` dispatch -- this test lives in `component/memql` and is written in Task 5); `TestCatalogOneSpellingEach` (no name appears twice for one receiver; every `Retired` spelling is refused by the parser's `V1RetiredForms`).
- [ ] **Step 2: Run** `go test github.com/znasllc-io/memql/component/language/tiers/ github.com/znasllc-io/memql/component/language/functions/` -- fail.
- [ ] **Step 3: Implement** both packages as data plus lookups; repoint dslspec's builtins and operators; update its drift tests.
- [ ] **Step 4: Run** -- pass, and `go test github.com/znasllc-io/memql/component/language/...`.
- [ ] **Step 5: Commit** `Issue #5365: the tier manifest and the function catalog`.

## Task 5: The in-process evaluator `EvalExpr` (#5366, #5367)

**Files:**
- Create: `component/memql/expr_eval.go`, `component/memql/expr_eval_test.go`, `component/memql/expr_row.go`
- Modify: `component/memql/runtime_evaluator.go` (helpers reused, not duplicated)

**Interfaces (Produces):**

```go
// ExprScope resolves the bare names an expression reads. ok=false is an unknown name.
type ExprScope interface{ Lookup(name string) (any, bool) }
// MapScope is the simple scope: names to values.
type MapScope map[string]any
// ExprRow is a row as a v1 expression reads it: row.id etc. are intrinsics, row.<f> reads Payload.
type ExprRow struct {
	ID, Concept, Type, CreatedBy string
	CreatedAt time.Time
	Provenance map[string]any
	Payload map[string]any
}
type EvalOptions struct {
	Now        time.Time
	Budget     int                                  // 0 -> tiers.DefaultStepBudget
	Predicates func(name string) (param string, body ast.ExpressionNode, ok bool) // spec/trait bodies (v1)
	CanonicalID func(ctx context.Context, value any, concept string) (string, error)
	Calls      func(ctx context.Context, call *ast.CallExpr, args []any, named map[string]any) (any, error) // construct calls, logic bodies only
}
func EvalExpr(ctx context.Context, n ast.ExpressionNode, scope ExprScope, opts EvalOptions) (any, error)
func EvalCondition(ctx context.Context, n ast.ExpressionNode, scope ExprScope, opts EvalOptions) (bool, error) // refuses a non-boolean: condition_not_boolean
// Absent is the in-process absence marker (key missing); nil is JSON null; both are "absent" to the table.
var Absent absentValue
```

Semantics: exactly the absence table; `??` via the existing `coalesceSelect` blank rule (a single implementation, pinned by `coalesce_array_missing_3627_test.go`); `+` on two strings concatenates, on two numbers adds, a string with a number concatenates the number's canonical text (the `concat` meaning, so the migration is exact), absent contributes `""` to a concatenation; ordering on strings is byte order; `now` is `opts.Now` formatted RFC3339Nano UTC; methods per the catalog (reuse `evalCollectionMethod`'s bodies by adapting them to call `EvalExpr` for lambda bodies); predicate application evaluates the predicate's v1 body with its parameter bound; every node evaluation decrements the budget.

- [ ] **Step 1: Failing table tests** (`expr_eval_test.go`): the absence table, row by row, for absent-key, JSON-null, `""`, `" "`, `0`, `false`, unicode (`"é"` vs `"e"`), nested (`row.a.b` with `a` absent); precedence cases evaluated to values; `+` cases; `??` cases (rule 30 table); `.?` propagation; ternary; methods (`count`, `any`, `all`, `where`, `first`, `includes`); predicate application; `EvalCondition` refusing `"yes"`; budget exhaustion on `args.xs.where(x => args.xs.any(y => y == x))` with a 2,000-element list and budget 10,000 returns `expression_budget_exceeded`.
- [ ] **Step 2: Run** `go test github.com/znasllc-io/memql/component/memql/ -run 'TestEvalExpr|TestEvalCondition'` -- fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** -- pass. Add `TestCatalogMatchesTheEvaluators` (Task 4) here and run it.
- [ ] **Step 5: Commit** `Issue #5367: EvalExpr, the one in-process evaluator`.

## Task 6: `Lower`, the one lowering, at `Init` (#5366)

**Files:**
- Create: `component/memql/expr_lower.go`, `component/memql/expr_lower_test.go`, `component/memql/expr_not.go`, `component/memql/expr_collection_sql.go`, `component/memql/expr_plan_const.go`, `component/memql/refine.go`
- Modify: `component/memql/ast_converter.go` (a `LambdaExpr` in a predicate operand routes to `Lower`), `component/memql/function_loader.go` (queries), `component/memql/spec_converter.go` + `spec_binding_resolver.go` (spec bodies: the lambda lowers against the binding), `component/memql/executor_filter.go` (`NotExpression` in `tryCompileCombinedFilter` and `nodeMatchesExpression`; `== ""` absent rule; `COLLATE "C"` on text ordering), every IR walker that switches on node types (`expression_helpers.go`, `function_validator.go` `expandExpressionWithArgs` for `PlanConstExpression`, `construct_reference_resolver.go`, `collection_scan_lint.go`, `function_loader.go` walkers, `executor.go` resolvers, `rowauthz_*`), `component/memql/engine_bootstrap.go` (the `Init` pass)

**Interfaces:**
- Consumes: Tasks 1, 4, 5.
- Produces:

```go
type LowerEnv struct {
	Position  tiers.Position
	Param     string                  // the lambda's parameter
	Concept   *memoryNodes.Concept    // nil for a trait (unbound) -- fields are then unchecked
	ShapeKeys map[string]string       // for a spec over a shape: key -> stored path
	Args      map[string]ArgType      // declared args and their types
	Predicate func(name string) (*Spec, bool)
}
// Lower turns a v1 predicate body into the executor's IR. Refusals carry the node, the position and the nearest P spelling.
func Lower(body ast.ExpressionNode, env LowerEnv) (ExpressionNode, error)
// NotExpression negates a two-valued predicate. SQL: NOT COALESCE((<target>), FALSE).
type NotExpression struct{ Target ExpressionNode }
// PlanConstExpression is a row-independent v1 subexpression, evaluated by EvalExpr during argument expansion and replaced by a literal (or a constant boolean).
type PlanConstExpression struct{ Expr ast.ExpressionNode }
// ArrayPredicateExpression is .any/.all/.count() over a row array field.
type ArrayPredicateExpression struct{ Field FieldReference; Method string; Param string; Body ExpressionNode; Compare *ComparisonOperator; Value any }
```

Mapping: `param.<intrinsic>` -> `FieldReference{Parts: [canonical]}`; `param.<field>[.<more>]` -> `FieldReference{Parts: ["payload", field, ...]}` (checked against `Concept.DeclaredFields()` when bound; a shape binding maps through `ShapeKeys`); `actor.<f>` -> `ActorReference` operand / actor comparison; `args.<f>` -> `ArgRefExpr`; a comparison with the row on the right is flipped; `v in param.<arrayField>` -> `OpHas`; `param.<f> in <list or args>` -> `OpIn`; `== nil` / `!= nil` -> `OpMissing` / `OpNotMissing`; `== ""` -> new `OpBlank` (SQL `COALESCE(x, '') = ''`); `!e` -> `NotExpression`; `isX(param)` / `isX(actor)` -> `SpecReferenceExpression` after checking the spec's kind matches the argument; a traversal `childOf(p => ...)` -> `RelationshipExpression` with the lambda lowered against an unbound row; a row-independent subtree -> `PlanConstExpression`; `? :` with a plan-constant condition folds at expansion, a boolean ternary `c ? p : q` over the row lowers to `(c && p) || (!c && q)`, a value ternary over the row refuses ("write the boolean form").

The `Init` pass (`lowerAllPushdownPositions`) walks every registered query, spec, trait and query-handler tool, lowers, then dry-compiles each comparison with a typed placeholder per arg (`compileComparisonExpressionWithContext`), and records failures on the `LoadReport` (strict boot refuses).

- [ ] **Step 1: Failing lowering tests** (`expr_lower_test.go`, engine-free where possible): each v1 body above maps to its IR (compared with `canonicalExpression`); refusals: `lower(row.email) == "x"` in `queryFilter` refuses naming `lower`, the position `queryFilter` and the spelling `row.email == lower(args.email)`; `row.a + 1 > 2` refuses naming arithmetic and `row.a > 1`; `query x().any(...)` in a P position refuses ("unbounded source"); `row.title && row.a == 1` refuses ("row.title is a string; a condition must be boolean"); `row.lineage.planId` refuses on an optional object field with the `row.?lineage.planId` spelling.
- [ ] **Step 2: Failing SQL/in-process agreement tests** (`expr_not_test.go`, db-gated with `dbtest`): for each absence-table row, the SQL the executor runs and `nodeMatchesExpression` agree on a fixture row, including under `!`.
- [ ] **Step 3: Failing `Init` test**: an engine over a tree with a query whose filter uses an M-only function on the row refuses boot with the three-part message.
- [ ] **Step 4: Run** the new tests -- fail.
- [ ] **Step 5: Implement.** Then run `MEMQL_REQUIRE_DB=1 ... go test -count=1 ./component/memql/...` (db) and `go test github.com/znasllc-io/memql/component/memql/...`.
- [ ] **Step 6: Verify the two `== ""` filters** (`dsl/work/queries.memql:141`, `dsl/identity/queries.memql:902`): grep their writers for `decision:` / `revokedAt:` stamps and record in the commit body that every writer stamps the field.
- [ ] **Step 7: Commit** `Issue #5366: Lower, the one lowering, run at Init for every pushdown position`.

## Task 7: Retire E3, E5, E4-string and E7 (#5367)

**Files:**
- Create: `component/automations/run_scope.go`, `component/automations/run_scope_test.go`
- Modify: `component/language/compiler/automation_generator.go` (conditions, forEach filter/source, switch subject, trigger filter, step args and value leaves compile to canonical v1 source; a leaf that is an expression is written `{"$expr": "<source>"}` so a literal string can never be mistaken for a reference), `component/automations/loader.go` (parse every expression once at load; a parse failure refuses the automation), `component/automations/executor.go`, `resume.go`, `scheduler.go` (`evaluateTriggerFilter`), `run_relay.go`, `precondition.go`, `logic_runner.go`, `logic_logical.go`, `logic_arithmetic.go`, `collection_chain.go`, `steps/*.go` (every `EvaluateCondition`/`EvaluateValue`/`EvaluateString`/`EvaluateFilterValue`/`EvaluateStepReference` call site from the E3 map moves to `EvalExpr` / `EvalCondition` with a `RunScope`)
- Delete: `component/automations/evaluator.go`, the resolver in `component/automations/steps/function.go` (`resolveArgValueRef`, `evaluateArgConcat`, `looksLikeNestedBuiltin`, `isRuntimeReference` and its string heuristics), the string half of `component/memql/mutation_templates.go` (`evalString` and the payload text parsers; payload values parse as a v1 map literal at load), the `$args` substitution in `component/memql/tool_execution.go` (a query handler parses once at load with `args.x` as an arg reference and executes with bound args; webhook url/body templates are v1 expressions)

**Interfaces:**
- Consumes: Tasks 2, 3, 5.
- Produces: `type RunScope struct{...}` implementing `ExprScope` with the roots `event`, `steps` (with the accessors `result`, `status`, `error`, `metadata`, `count`, `nodes`, `empty`, `first`, `last`), `item`, `index`, `input`, `args`, `actor`, `now`, `config`, `automation`, the step ids and the G2 bare-args tiers exactly as `resolvePath` served them (epic 3 retires the step spellings, not this epic); `func NewRunScope(run *RunContext) *RunScope`.

- [ ] **Step 1: Failing tests**: `TestNoStringOperatorProbing` (walks `component/automations/**.go` non-test files and fails on `strings.Contains`/`strings.Index`/`SplitN` applied to operator literals `"=="`, `"!="`, `">="`, `"<="`, `"&&"`, `"||"` -- the acceptance grep as a test); `TestTriggerFilterStartsWithLoadsAndFires` (an automation `@filter(row => row.code startsWith "abc")` loads and fires on a matching row, not on another); `TestStepArgumentIsAnExpression` (a step argument `hash(args.a + "-" + args.b)` resolves through `EvalExpr`, pinned by asserting the loaded step carries a parsed AST, not a string); the per-caller data-model tests from the E3 map (executor, resume, onError, trigger filter, logic runner, forEach clone) each resolve `event.payload.x`, `steps.s.result`, `item.x`, `index`, `args.x`, `actor.userId`, `now`; the two behaviour fixes (reminders, data conflicts) as regression tests.
- [ ] **Step 2: Run** `MEMQL_REQUIRE_DB=1 ... go test -count=1 ./component/automations/...` -- fail.
- [ ] **Step 3: Implement**, deleting the four scanners.
- [ ] **Step 4: Run** the automations tree, `./component/memql/...` (mutation templates, tools), and `go test github.com/znasllc-io/memql/component/language/...` -- pass.
- [ ] **Step 5: Commit** `Issue #5367: retire the four string evaluators; every position consumes the parsed AST`.

## Task 8: `memqlmigrate --rewrite=expressions` (#5368)

**Files:**
- Create: `cmd/memqlmigrate/expressions.go`, `cmd/memqlmigrate/expressions_test.go`
- Modify: `cmd/memqlmigrate/rewrites.go` (register `{name: "expressions", edition: "2026", epic: "dsl-v1-expressions", tree: rewriteExpressions}`), the package doc in `main.go`

**Interfaces:** `func rewriteExpressions(root string, files map[string][]byte) (map[string][]byte, error)`.

Per file: (1) predicate names = every `spec`/`trait` declared anywhere in `files` (a flat registry, as the engine's), with each spec's binding kind (concept, @row shape, @actor shape) read from the shapes in `files`; (2) query `filter` clauses (the rewriter's clause extraction, continuation-aware): parse with the LEGACY `ParseExpression`, transform the legacy AST to v1 (bare field -> `row.field`; `row.X` stays; a predicate name -> `name(row)` or `name(actor)`; `when(args.x){e}` -> `(args.x == nil || e')` under `&&`/top level, `(args.x != nil && e')` under `||`; `args.v in field` -> `args.v in row.field`; `field in list` -> `row.field in list`; `null` -> `nil`; an unquoted canonical id -> a string), print with `FormatExpr`, splice `row => <text>`; (3) spec/trait bodies: `{ return e }` -> `= <param> => e'` with `param` = `actor` for an @actor-shape binding else `row`; (4) `@filter(e)` -> `@filter(row => e')` with `payload.X` -> `row.X`; (5) in-process positions (logic, automation, mutation, step args): the byte-scanner model of `null_coalesce_migrate.go` for `cond(p, a, b)` -> `(p ? a : b)` (parens dropped when the call is a whole statement right-hand side), `concat(a, b, c)` -> `a + b + c` (an argument containing `??` or `? :` is parenthesised), `exists(x)` -> `(x != nil && x != "")`, `payload.X` in an automation condition -> `event.payload.X`, `id` -> `event.payload.id`... (the two condition sites); (6) tool handlers: `\"$args.x\"` and `$args.x` -> `args.x`.

Verification inside the rewrite (the accept-stamp model): for every P position, lower the OLD clause through the legacy converter and the NEW clause through `Lower`, compare `canonicalExpression` of both, and return the original file bytes plus an error naming the clause on any difference; for M positions, the rewritten file must parse with the v1 parser. Idempotent: a v1 clause (opens with a lambda header) is left alone.

- [ ] **Step 1: Failing tests**: before/after pairs for every row of the explorer inventory (the 25 conjunct shapes, the `||`-guard at `dsl/observability/queries.memql:53`, the multi-line `dsl/router/queries.memql:64-68`, specs over concept/@row/@actor, traits, the six `@filter`s, `cond` nested four deep, `concat` with `??` arguments, the two `exists`, the tool handlers); `TestExpressionsRewriteIsIdempotent`; `TestExpressionsRewriteRefusesAMeaningChange` (a hand-built clause whose lowering would differ returns the original bytes and an error).
- [ ] **Step 2: Run** `go test github.com/znasllc-io/memql/cmd/memqlmigrate/ -run Expressions` -- fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** -- pass.
- [ ] **Step 5: Commit** `Issue #5368: memqlmigrate --rewrite=expressions`.

## Task 9: Migrate the tree and flip the parser (#5368, #5364)

**Files:** `dsl/**`, `examples/**/dsl/**`, `deploy/fleet/dsl/**`, root `*.memql`, `test/**/*.memql`, the corpus cells, `cmd/shopifyschema/emit_dsl.go` (and regenerate `dsl/shopify/generated/`), `component/emailrules/generate.go`, `component/routingrules/*` (rendered constructs), `dsl/authoring/prompts/authoring{Emit,Repair,Design}.tmpl`, Go test fixtures that embed struct-form filters (about 76 files), `component/language/parser/grammar_surface_drift_test.go` corpus and `grammar_version.go`.

- [ ] **Step 1:** `go run ./cmd/memqlmigrate --rewrite=expressions -w dsl examples deploy/fleet/dsl test` and review the diff (`git diff --stat`, spot-check every file kind).
- [ ] **Step 2:** Run it again; `-check` must report nothing (idempotent).
- [ ] **Step 3:** Update the generators and regenerate their output; update the LLM authoring templates to teach the v1 grammar; migrate Go fixtures (the codemod's library entry point `migrateExpressionSource` over string literals, then hand review).
- [ ] **Step 4: Flip:** in authored positions the parser now refuses every retired form; `ParseFile` of a legacy filter, spec body, `@filter`, `cond(` etc. refuses naming the migrator. The internal query form (`ParseExpression`, `Execute`) is unchanged.
- [ ] **Step 5:** Bump `GrammarVersion` (`2026.09-dsl-v1-expressions-<digest>`), add the grammar-surface corpus entries for the v1 forms and flip the legacy accept entries to refusals, record the narrowings in the header.
- [ ] **Step 6: Verify** `TestUnifiedTreeLoadsClean`, `go run ./cmd/memqllint dsl` (zero refusals), `make test`.
- [ ] **Step 7: Commit** `Issue #5368: the tree migrated to the v1 expression grammar; the parser refuses the retired forms`.

## Task 10: Port the gates that read expression text (#5368)

Every gate listed in the migrator explorer's section 2 that would fail loudly or go silently blind under the v1 grammar: `dslgate` (retired operators, row intrinsic, user-scope selection, admin-gate composition, `ClauseGuarantees`, cross-namespace import), `test/dslconformance` (spec-declaration regexes in `admin_gate_test.go`, `caller_arg_selection_test.go`, `pii_projection_test.go`; `row_id_mirror_test.go`; `cross_domain_use_test.go`; `TestFilterSyntaxCanonical`; `TestNoShortIdConceptPrefix`; `no_named_writes_test.go`; `per_part_hashed_id_test.go`; `naming_conventions_test.go` declarations), `component/language/pagination/checker.go` (the `when` stripping becomes the `args.x == nil ||` pattern), `component/memql/dslimports/integrity.go` (filter-field lane reads `row.<field>`; spec-body lane reads `<param>.<field>`), `component/memql/callgraph/callgraph.go` (`@filter` regex, `concat` rule), `cmd/memqlmigrate/rowauthz_infer.go`, `component/memql/rowauthz_shadow.go` `topLevelPayloadField`, `keyword_slices.go` and `sense/runnable.go` (brace-less spec/trait slicing), `authoring_catalog.go` (spec/trait `constructName`).

Rule: a gate that matched text now parses the clause with `ParseV1Lambda` and inspects the AST, and every gate asserts a reachable positive (it found at least N constructs of the kind it checks) so it cannot go blind silently.

- [ ] **Step 1:** For each gate, a failing test with a v1 fixture that the gate must still catch (e.g. `filter row => row.ownerUserId == args.userId` on a person-scoped concept is still flagged).
- [ ] **Step 2:** Port each gate.
- [ ] **Step 3:** Run `go test -count=1 github.com/znasllc-io/memql/test/dslconformance/... github.com/znasllc-io/memql/component/memql/dslgate/... github.com/znasllc-io/memql/component/language/...` and `make test`.
- [ ] **Step 4: Commit** `Issue #5368: the gates read the v1 grammar`.

## Task 11: Sense, dslspec, the language server and the extension (#5365)

**Files:** `component/memql/sense/{context.go,enclosing.go,complete.go,hover.go,signature.go,tokenize.go,spec.go,snippets.go,builtins.go}`, `component/language/dslspec/*`, `cmd/memql-lsp/internal/grammar/grammar.go`, `editors/vscode/syntaxes/memql.tmLanguage.json` (regenerated), `editors/vscode/package.json` (version bump), `editors/vscode/CHANGELOG.md`

- [ ] **Step 1: Failing tests**: `TestCompletionOffersTheTierOfThePosition` (cursor in `filter row => row.` offers the concept's fields and intrinsics; cursor in `filter row => ` offers P functions, predicates applied to `row`, `args.`, `actor.`, and NOT `addDuration` on the row; cursor in a logic `return` offers M functions); `TestHoverShowsSignatureTierAndLegality` (hover on `lower` inside a filter shows the signature, "In-process (M)", and "Allowed here on plan constants only"); `TestHoverOnOperators` (`??`, `.?`, `=>`, `!` have hover); `TestTokenizePipePipe` (`||` is an operator token); `TestFilterClauseDetectedOnOneLine` (the braceless clause is detected).
- [ ] **Step 2:** Implement position detection (`CursorContext.Position tiers.Position`), catalog-driven completion and hover (use the frontend-design skill for the hover card and completion detail copy), the operator hover step, fix `mapTokenType`, update snippets to the v1 forms.
- [ ] **Step 3:** `make vscode-grammar` (regenerate), bump the extension version, CHANGELOG entry.
- [ ] **Step 4:** Run `go test github.com/znasllc-io/memql/component/memql/sense/... github.com/znasllc-io/memql/cmd/memql-lsp/...` and the extension's `npm test` in `editors/vscode`.
- [ ] **Step 5: Commit** `Issue #5365: Sense reads the tier manifest and the function catalog`.

## Task 12: The corpus, the fuzz targets and the differential lane (#5369)

**Files:** `test/conformance/2026/expr/<position>/{expect.json,fixture.memql,*.memql}` for all eleven positions, `test/conformance/engine_adapter_test.go` (bodies swapped to `Lower` + SQL and `EvalExpr`), `test/conformance/corpus_test.go` (the tier completeness gate over `(position, node kind or function)`), `component/language/parser/fuzz_test.go` (`FuzzLexer`, `FuzzParseV1Expression`), `component/memql/expr_lower_fuzz_test.go` (`FuzzLower`: any parsed expression either lowers or refuses, never panics), `test/conformance/differential_db_test.go`, `test/conformance/2026/fuzz/` seeds.

The completeness gate computes coverage from the cells themselves: it parses every `load_ok`/`lower`/`evaluate` case at its position, collects `ast.KindOf` and every called function name, and fails naming each `(position, kind)` that `tiers.Allows` admits but no positive case uses, and each position with no refused case. The lane: for every `lower` case (and a generated matrix over the absence table) it inserts fixture rows (absent key, JSON null, `""`, `" "`, `0`, `false`, unicode, nested) into a scratch concept, runs the lowered SQL, runs `EvalExpr` on the same rows, and reports the first disagreement by case file; it runs in the `mcp-conformance` job and, until epic 6, a disagreement logs `DIFFERENTIAL:` lines and fails only when `MEMQL_DIFFERENTIAL_REQUIRED=1`.

- [ ] **Step 1:** Write the cells (positive and negative per position; every node kind and every function positively somewhere and negatively somewhere); flip `filter-with-bang.memql` to `load_ok` with a v1 body.
- [ ] **Step 2:** Swap the adapter; implement the gate; run `go test -count=1 -run TestCorpusVerdicts ./test/conformance/`.
- [ ] **Step 3:** Fuzz targets; run each for 60s locally (`go test -fuzz=FuzzParseV1Expression -fuzztime=60s ...`) and commit any found failures as seeds plus fixes.
- [ ] **Step 4:** The lane; run it against the throwaway Postgres; fix every disagreement it reports (the fix is in `Lower` or `EvalExpr`, and the case stays).
- [ ] **Step 5: Commit** `Issue #5369: expression cells for every position, fuzz targets and the differential lane`.

## Task 13: Documentation (#5364, #5365, #5366)

**Files:** `docs/public/language/memql.md` (`### Operator precedence` table -- remove the skip in `TestV1PrecedenceTableIsPublished`; `### Absent values`; the lambda forms in Filters, Specs, Traits, `@filter`, Collection methods; `### Where each expression runs` generated from the tier manifest and pinned; `refine`), `docs/public/language/functions.md` (the catalog, generated and pinned), `docs/public/language/authoring-rules.md` (rules 11b, 11c, 21, 22, 27, 30, 32 rewritten for v1; a new rule for `.?` and the plan-constant rule), root `CLAUDE.md` (the "Canonical filter-clause syntax" and Specs/Queries examples; verify with `go test -count=1 .`), `component/language/CLAUDE.md`, every ```memql fence under `docs/public` and README (`TestDocsMemqlSnippets`).

- [ ] **Step 1:** Write the docs; generate the two tables.
- [ ] **Step 2:** Run `go test -count=1 . ./docs/...` and `go test github.com/znasllc-io/memql/component/language/parser/ -run TestV1Precedence`.
- [ ] **Step 3: Commit** `Issue #5364: the v1 expression language documented`.

## Task 14: Integration, verification and delivery

- [ ] **Step 1:** Rebase on the latest `origin/epic/dsl-v1-foundations` (and on `main` once epic 1 merges); resolve conflicts by taking epic 1's side for its files and rerunning the codemod for `dsl/**`; regenerate `topology.model.json`.
- [ ] **Step 2:** Full verification: `make test`; `MEMQL_REQUIRE_DB=1 ... go test -count=1` over `scripts/ci/db-gated-packages.sh --trees`; `go test -count=1 ./test/conformance/...` with the DB; the seven node-tag builds (`go build -tags <t> .` for identity, agent, planner, workbench, mcp, edge, and the default); `make frontdoor-paths-check`; `go run ./cmd/memqllint dsl`; `editors/vscode` `npm test`.
- [ ] **Step 3:** Delete this plan file.
- [ ] **Step 4:** Push, open the PR (one PR, `Closes #5363` and each task issue on its own line), watch CI, fix, enqueue with `gh pr merge <n> --repo znasllc-io/memql`.
- [ ] **Step 5:** After merge: close the epic and task issues if not auto-closed, delete the local branch and worktree, prune stale refs.
