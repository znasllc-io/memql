# MemQL Grammar

**Status:** Phase 1 reference. This document describes the grammar
that the shared `component/language/parser` accepts today, after the
Phase 1 language-improvements pass (commit `4fe9fc6`).

It is NOT a consolidated spec yet -- the concept parser at
`component/database/memory-nodes/concept_parser.go` and the query
parser at `component/memql/parser.go` still run their own grammars.
Phase 2 of the plan consolidates them under the grammar below;
until then, treat this doc as authoritative for automations /
functions / mutations / specs and advisory for the other two.

Notation is an informal EBNF:

- `"literal"` -- literal text
- `UPPER` -- lexer token class
- `rule` -- grammar non-terminal
- `?` -- optional
- `*` -- zero or more
- `+` -- one or more
- `|` -- alternatives
- `(...)` -- grouping

---

## Lexical structure

### Whitespace

Spaces, tabs, and newlines are insignificant except inside string
literals. Line comments start with `//` and run to end of line.

### Identifiers

```
IDENT       = letter (letter | digit | "_")*
letter      = "a".."z" | "A".."Z" | "_"
digit       = "0".."9"
```

Keywords are not valid identifiers.

### Numbers

```
NUMBER      = digit+ ("." digit+)? (("e" | "E") ("+" | "-")? digit+)?
```

Parsed as float64 at value position.

### Strings

```
STRING      = '"' (escape | not-dquote-not-backslash)* '"'
escape      = "\" ( '"' | "\" | "n" | "t" | "r" | "u" hex4 )
```

### Operators and punctuation

| Token | Text | Notes |
|---|---|---|
| `TokenDefine` | `:=` | Step assignment. |
| `TokenAt` | `@` | Annotation prefix. |
| `TokenAmpAmp` | `&&` | Logical AND. |
| `TokenBang` | `!` | Boolean negation. |
| `TokenQuestion` | `?` | Ternary condition (`cond ? then : else`). |
| `TokenQuestionDot` | `?.` | Optional chaining -- DEPRECATED; removal tracked in Phase 4. |
| `TokenQuestionQuestion` | `??` | Null coalescing -- DEPRECATED; removal tracked in Phase 4. |
| `TokenColon`, `TokenSemicolon`, `TokenComma` | `:` `;` `,` | Standard. |
| `TokenParen*`, `TokenBrace*`, `TokenBracket*` | `( ) { } [ ]` | Standard. |

### Keywords

- Receiver-class keywords: `Query`, `Mutation`, `Automation`, `Spec`,
  `Tool`, `Builtin`.
- Control flow: `func`, `for`, `range`, `if`, `else`, `switch`,
  `case`, `default`, `continue`, `break`, `return`, `nil`, `retry`,
  `when`, `as`, `where`, `use`, `concept`.
- Membership: `in`, `has`, `not`.

Note: `if` is a STATEMENT keyword (control flow in automation
bodies). The conditional-value BUILTIN is `cond(pred, then, else)`,
never `if(...)`.

---

## Files

```
File            = UseDecl* FuncDecl*

UseDecl         = "use" IDENT ("." IDENT)* NL
```

`use a.b` brings concept `v1:a:b` (or similar) into the file's
namespace. Does not affect parsing of subsequent code; it is a
loader-side hint.

---

## Function declarations

```
FuncDecl        = Annotation* "func" "(" Receiver ")" IDENT "(" ArgList? ")" ReturnType? Body

Receiver        = "Query" | "Mutation" | "Automation" | "Spec" | "Tool" | "Builtin"

ArgList         = Arg ("," Arg)*
Arg             = IDENT Type?

ReturnType      = "(" TypeList ")"  | Type
TypeList        = Type ("," Type)*

Type            = IDENT                      # string, bool, int, float, datetime, object, any, error
                | "enum" "(" StringList ")"
                | "array" "(" Type ")"       # DEPRECATED; Phase 6 introduces `[]T`
                | "[" "]" Type               # slice (Phase 6)
                | "map" "[" Type "]" Type    # map (Phase 6)

Body            = "{" Statement* "}"
```

### Receiver semantics

| Receiver | Body shape | Runtime behavior |
|---|---|---|
| `Query` | Single query expression | Returns shape() result or graph bundle |
| `Mutation` | Single `insert(...)` call | Returns inserted node |
| `Automation` | Statement block with steps | Runs on trigger |
| `Spec` | Single filter expression | Predicate over a node |
| `Tool` | Field declarations (like concept body) | Declares tool input schema |
| `Builtin` | Field declarations | Declares builtin args; executor comes from @executor |

---

## Annotations

```
Annotation      = "@" IDENT ("(" AnnotationArgs? ")")?

AnnotationArgs  = STRING                     # single-arg form: @description("...")
                | NamedArgList                # multi-arg form: @relationship(type="parent", field="x")

NamedArgList    = NamedArg ("," NamedArg)*
NamedArg        = IDENT "=" AnnotationValue
AnnotationValue = STRING | NUMBER | IDENT
```

### Supported annotations (by context)

**Concept-level** (`applyConceptAnnotation`, `concept_parser.go:527`):

| Name | Arg shape | Effect |
|---|---|---|
| `@description` | `"text"` | Concept documentation. |
| `@type` | `"object" \| "collection" \| "reference"` | Node-type classifier. |
| `@cache` | `ttl=N` | Cache TTL seconds. |
| `@skipDeleted` | none | Auto-filter soft-deleted rows. |
| `@enforceRequired` | none | Validate required fields at write time. |
| `@defaultFilter` | `"query"` | Default filter applied to all reads. |
| `@scope` | `"partition" \| "global"` | Partition scope (see docs/core/memql-authoring-rules.md#3). |

**Property-level** (`applyPropertyAnnotation`, `concept_parser.go:567`):

| Name | Arg shape | Effect |
|---|---|---|
| `@required` | none | Mandatory on insert. |
| `@default` | `value` | Default on insert. |
| `@description` | `"text"` | Field documentation. |

(Phase 3 of the plan expands this set: `@unique`, `@pattern`,
`@minLength`, `@maxLength`, `@immutable`, `@secret`, `@variant`.)

**Function-level** (`function_loader.go:443`):

| Name | Arg shape | Effect |
|---|---|---|
| `@enabled` | none | Load this function. |
| `@description` | `"text"` | Function documentation. |
| `@deprecated` | `"reason"` | Soft warning. |
| `@trigger` | `event="..."` or `schedule="..."` | Automation trigger. |
| `@executor` | `"integration.name.capability"` | Bind builtin to Go handler. |
| `@handler` | `type="...", url="..."` | Tool handler binding. |
| `@template` | `{...}` | Shape template body. |
| `@concepts` | `"v1:...", "v1:..."` | Shape target concepts. |
| `@defaultProvider` | `"name"` | Default SI provider for prompt. |
| `@templateFile` | `"filename.tmpl"` | External prompt template. |

Note: arg schemas are declared via an `args { ... }` block, NOT an
annotation. See the "Args block" section below.

Relationship annotations (inside concept bodies):

```
@relationship(type="parent"|"child"|"contains"|"alias"|"owns"|"createdBy"|"interactsWith",
              field="<field>", target="<concept>", direction="outgoing"|"incoming"|"bidirectional")
```

### Args block

```
ArgsBlock       = "args" "{" ArgDecl* "}"
ArgDecl         = IDENT WS IDENT (WS Annotation)*    # <name> <type> [@required] [@enum(...)] [@default(...)] [@description("...")]
Annotation      = "@required"
                | "@enum" "(" StringList ")"
                | "@default" "(" Literal ")"
                | "@description" "(" StringLit ")"
```

Position by construct kind:
- **Struct query / mutation**: `args { ... }` is a body sub-block.
- **Procedural function / automation / policy**: `args { ... }` at
  file top, above the `func (...)` declaration.
- **Builtin / tool / prompt**: body fields directly (no `args`
  wrapper — the body IS the schema).

Example (struct query):
```
use identity.user

query queryByRole {
  args {
    role  string  @required  @enum("owner", "admin", "writer", "reader")
    name  string
  }
  filter  payload.role == args.role; ?.payload.name == args.name
  shape   userFull
}
```

---

## Statements (automation bodies)

```
Statement       = StepAssign
                | ForStatement
                | SwitchStatement
                | IfStatement
                | ParallelStatement
                | ReturnStatement
                | ExpressionStatement
```

### Step assignment

```
StepAssign      = IDENT (":=" | ":=" "retry" "(" NUMBER ")") StepRHS

StepRHS         = IfStepWrap
                | Expression

IfStepWrap      = "if" Condition "{" Expression "}"
```

As of Phase 1, any expression that normalises into a function call is
a valid StepRHS. The normaliser (`expressionToFunctionCall` in
parser.go) accepts these types:

- `FunctionCallExpr` -- `foo({...})`
- `CoalesceExpr` -- `coalesce(a, b, ...)`
- `CondExpr` -- `cond(pred, then, else)`
- `ConcatExpr` -- `concat(a, b, ...)`
- `HashExpr` -- `hash(value)`
- `FirstExpr`, `LastExpr` -- `first(x)`, `last(x)`
- `LowerExpr`, `UpperExpr`, `TrimExpr` -- string helpers
- `TimestampExpr` -- `timestamp()` / `now()`

Anything else (literal, arithmetic, ternary, bare identifier) is
rejected with a `got %T` error.

### For statement

```
ForStatement    = "for" IDENT ":=" "range" Expression ("where" Condition)? "{" Statement* "}"
                | "for" IDENT "," IDENT ":=" "range" Expression ("where" Condition)? "{" Statement* "}"
```

The second form binds index and value: `for i, item := range items`.

### Switch, if-statement, parallel, return

```
SwitchStatement = "switch" Expression "{" ( "case" StringList "{" Statement* "}" )+ ( "default" "{" Statement* "}" )? "}"

IfStatement     = "if" Condition "{" Statement* "}" ( "else" "{" Statement* "}" )?

ParallelStatement = "parallel" "{" "branches" ":=" "[" (BranchBlock ",")* "]" WaitAnnotation? FailFast? "}"

ReturnStatement = "return" Expression? NL
```

Return expressions inside automations support the `??` fallback
chain: `return emitCreated ?? newGrant ?? identityExists.first`.
Evaluates left-to-right, picks the first non-nil value. (Slated for
removal in Phase 4; `coalesce()` is the replacement.)

---

## Expressions

Precedence, tight to loose:

1. Primary: literal, identifier, function call, grouping, nil
2. Postfix member access: `.field`, `.method()`
3. Unary: `!`, `-` (lead), `?.` (deprecated)
4. Multiplicative: none today (no `*` / `/` at expression level;
   numeric work happens in runtime via `add`, `sub`, `lt`, `gt`)
5. Comparison: `==`, `!=`, `<`, `<=`, `>`, `>=`, `in`, `has`, `not in`
6. Logical AND: `&&` or comma-less sequence via `and(...)`
7. Logical OR: comma (inside expression groups) or `or(...)`
8. Null coalesce: `??` -- deprecated
9. Ternary: `? :`

```
Expression      = Ternary
Ternary         = LogicalOr ("?" Ternary ":" Ternary)?
LogicalOr       = NullCoalesce ("," NullCoalesce)*
NullCoalesce    = LogicalAnd ("??" LogicalAnd)*
LogicalAnd      = Equality ("&&" Equality)*
Equality        = Comparison (("==" | "!=") Comparison)*
Comparison      = Additive (("<"|">"|"<="|">=") Additive)*   # rarely used
Additive        = Unary
Unary           = ("!" | "?.") Unary | Postfix
Postfix         = Primary ("." IDENT ("(" ArgList? ")")?)*
Primary         = LITERAL | IDENT | FunctionCall | "(" Expression ")" | "nil"
```

### Object and array literals

```
ObjectLiteral   = "{" (ObjectEntry ("," ObjectEntry)* ","?)? "}"
ObjectEntry     = (IDENT | STRING) ":" Expression

ArrayLiteral    = "[" (Expression ("," Expression)* ","?)? "]"
```

Object literal keys accept both quoted strings (`{"k": v}`) and bare
identifiers (`{k: v}`) in the current parser. This is the "strict
vs lenient" folklore from `memql-authoring-rules.md` entry #2 --
both paths now accept both forms, but keep the documented convention
(quote keys when the key contains `-`, `.`, or starts with a digit).

### Function calls

```
FunctionCall    = IDENT "(" ArgList? ")"
ArgList         = (PositionalArg | NamedArg) ("," (PositionalArg | NamedArg))*
PositionalArg   = Expression
NamedArg        = IDENT "=" Expression
```

Named arguments are only accepted by specific builtin functions
(`insert`, `shape`, `createUser`, etc.) whose parser has explicit
named-arg support. For generic function calls, positional-only.

### Built-in accessors

Every accessor below returns an expression node the parser recognises
specifically (not a generic FunctionCallExpr). At step-RHS position
the `expressionToFunctionCall` normaliser converts these into
FunctionCallExpr-shaped step configs.

| Builtin | Shape | Purpose |
|---|---|---|
| `args.name` | scalar | Read function argument. |
| `var("ENV_VAR")` | scalar | Read environment variable. |
| `step("id")` / bare `id.result.x` | accessor | Read a previous step's result. |
| `field("payload.x")` | accessor | Read a nested payload field. |
| `input()` / `input("x")` | accessor | Read automation input. |
| `item()` / `item("x")` | accessor | Current forEach item. |
| `index()` | accessor | Current forEach index. |
| `event()` / `event.x` | accessor | Trigger event. |
| `error()` | accessor | Error from the previous step. |
| `timestamp()` / `now()` | scalar | Current UTC timestamp (RFC3339). |
| `memqlversion()` | scalar | Running engine version. |
| `concat(a, b, ...)` | value | String concatenation. |
| `coalesce(a, b, ...)` | value | First non-nil. |
| `cond(pred, a, b)` | value | Three-arg conditional (Phase 1). |
| `hash("text")` | scalar | SHA-256 hex digest. |
| `first(collection)` / `last(collection)` | scalar | Pick edge element. |
| `lower` / `upper` / `trim` | scalar | String helpers. |
| `and(...)` / `or(...)` / `not(...)` | boolean | Legacy logic builtins. |
| `eq`, `lt`, `gt`, `lte`, `gte` | boolean | Legacy comparison builtins. |

---

## MemQL query expressions

Used inside function bodies for `Query` and `Mutation` receivers and
in `Spec` bodies.

```
QueryExpr       = Filter (";" Filter)*
Filter          = ConceptFilter
                | PayloadFilter
                | RelationshipFilter
                | SpecReference

ConceptFilter   = "concept" ("==" | "!=") QualifiedName
PayloadFilter   = "payload" "." IDENT ("." IDENT)* Op Value
Op              = "==" | "!=" | "<" | "<=" | ">" | ">=" | "in" | "has" | "not in"
Value           = LITERAL | IDENT | FunctionCall | ArrayLiteral | ObjectLiteral

RelationshipFilter = ("parent" | "child" | "alias" | "owns" | "createdBy" | "contains"
                     | "interactsWith") "(" QualifiedName ")"

SpecReference   = IDENT                       # a spec name registered via func (Spec)
QualifiedName   = IDENT ":" IDENT (":" IDENT)*

Directive       = "shape" "(" QueryExpr "," STRING ")"
                | "sort" "(" QueryExpr "," STRING "," ("asc"|"desc") ")"
                | "paginate" "(" QueryExpr "," NUMBER ")"
                | "asOf" "(" QueryExpr "," (STRING | "latest") ")"
                | "select" "(" QueryExpr "," IDENT ("," IDENT)* ")"
                | "withDepth" "(" QueryExpr "," NUMBER ")"
```

Directives are **top-level only** -- they wrap a query expression
from the outside. A query function body that starts with `return
sort(concept==...)` works; one that nests a directive inside another
expression (e.g. as an argument to a function call) does not. See
`memql-authoring-rules.md` entry #1.

Phase 2 lifts this restriction by unifying the grammars so directives
can appear wherever a query expression is valid.

---

## Diagnostics

Parser errors now include human-readable token names
(Phase 1, commit `4fe9fc6`):

```
parse error at line 99, column 3: expected '{', got "newIdentity"
```

Previously:

```
parse error at line 99, column 3: expected 8, got "newIdentity"
```

Step-reference validation runs at compile time in the automation
compiler:

- Unknown reference: `automation "X": step "Y" references unknown step "Z"`
- Forward reference: `automation "X": step "Y" forward-references step "Z" (declared later in source)`

Mutation-time payload validation rejects reserved payload fields
with the concept name and a rename hint (Phase 1).

---

## Known limitations (tracked in the plan)

- **Multiple parsers.** The concept parser
  (`component/database/memory-nodes/concept_parser.go`) and the
  query-execution parser (`component/memql/parser.go`) run their own
  grammars. Phase 2 of the plan consolidates them under this parser
  as the shared frontend.
- **Directives only at top level.** `sort` / `paginate` / `asOf` /
  `select` / `withDepth` / `shape` cannot nest. Will lift once the
  shared frontend lands.
- **Forward step refs.** Runtime executes steps in source order, so
  forward references resolve to nil. Phase 2 introduces a
  topological-sort emit path, after which source order stops
  mattering.
- **No `map[K]V` / `[]T` type syntax.** Phase 6 adds these.
- **`?.` and `??`.** Deprecated; Phase 4 removes.

---

## Relationship to source files

| Source | What it does |
|---|---|
| `component/language/parser/lexer.go` | Token definitions + Tokenize() |
| `component/language/parser/ast.go` | AST node type definitions |
| `component/language/parser/parser.go` | Recursive-descent parser (function declarations, statements, expressions) |
| `component/language/parser/errors.go` | ParseError type + helpers |
| `component/language/compiler/automation_generator.go` | AST -> automation JSON |
| `component/language/compiler/function_generator.go` | AST -> function definition |
| `component/language/compiler/composition.go` | CQS validation |
| `component/database/memory-nodes/concept_parser.go` | Parallel parser for concept.memql files (Phase 2 target for consolidation) |
| `component/memql/parser.go` | Parallel parser for top-level query strings (Phase 2 target) |

Keep this document in lockstep with the parser. Any grammar change
should land with a corresponding edit here.
