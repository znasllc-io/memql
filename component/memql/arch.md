# Engine Package Architecture

> **Last Updated:** 2026-09-14

This document describes the architecture of the MemQL processing engine,
which is split across two Go modules: `component/language/` (parsing +
compilation) and `component/memql/` (the query engine itself). There is
no single `engine/` directory -- the diagrams below use `engine/parser/`
etc. as a conceptual label for `component/language/parser/`, and so on
for the other pipeline stages.

## Package Overview

```
component/language/
├── parser/       # Lexical analysis and parsing
├── compiler/     # AST to target format transformation
├── ast/          # Shared AST node type definitions
├── annotations/  # Annotation (@foo(...)) parsing + validation
├── dslclause/    # Single source of truth for which keywords terminate a filter clause (memql#2815)
├── dslspec/      # Single source of truth for the DSL authoring surface: constructs / keywords / operators / field types
└── pagination/   # sort()/paginate() expression support

component/memql/
├── engine.go   # Base engine interface + MemQLEngine
├── executor.go # Query execution logic
├── ...         # 573 top-level files -- loaders, AI tool loop, dslgate callers, etc.
└── sense/      # MemQL Sense language intelligence (tokenize, complete, diagnose, hover, signature)
```

---

## Engine Separation

The engine package follows a **compiler pipeline architecture** with clear separation of concerns:

```
┌───────────────────────────────────────────────────────────────────────────────────┐
│                              ENGINE PIPELINE                                      │
├───────────────────────────────────────────────────────────────────────────────────┤
│                                                                                   │
│   .memql Source                                                                   │
│        │                                                                          │
│        ▼                                                                          │
│   ┌─────────────────────────────────────────────────────────────────────────┐     │
│   │                         PARSER ENGINE                                   │     │
│   │                         engine/parser/                                  │     │
│   │                                                                         │     │
│   │   ┌─────────┐      ┌─────────┐      ┌─────────┐                         │     │
│   │   │  Lexer  │─────▶  Parser ─────▶   AST                               │     │
│   │   └─────────┘      └─────────┘      └─────────┘                         │     │
│   │                                                                         │     │
│   │   Responsibility: Source → Tokens → Abstract Syntax Tree                │     │
│   └─────────────────────────────────────────────────────────────────────────┘     │
│                                      │                                            │
│                                      ▼                                            │
│   ┌─────────────────────────────────────────────────────────────────────────┐     │
│   │                        COMPILER ENGINE                                  │     │
│   │                        engine/compiler/                                 │     │
│   │                                                                         │     │
│   │   ┌───────────────────────┐    ┌───────────────────────┐                │     │
│   │   │ CheckBody+CompileBody │    │  FunctionGenerator    │                │     │
│   │   │                       │    │                       │                │     │
│   │   │ statements → steps    │    │   AST → definition    │                │     │
│   │   └───────────────────────┘    └───────────────────────┘                │     │
│   │                                                                         │     │
│   │   Responsibility: AST → the executor's steps and function definitions   │     │
│   └─────────────────────────────────────────────────────────────────────────┘     │
│                                      │                                            │
│                                      ▼                                            │
│   ┌────────────────────────────────────────────────────────────────────────┐      │
│   │                        MEMQL ENGINE                                    │      │
│   │                        engine/memql/                                   │      │
│   │                                                                        │      │
│   │   ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐               │      │
│   │   │ Executor │  │ Relations│  │   AI     │  │  Cache   │               │      │
│   │   │          │  │          │  │ Runtime  │  │          │               │      │
│   │   └──────────┘  └──────────┘  └──────────┘  └──────────┘               │      │
│   │                                                                        │      │
│   │   Responsibility: Query Execution, DB Operations, AI Invocation        │      │
│   └────────────────────────────────────────────────────────────────────────┘      │
│                                      │                                            │
│                                      ▼                                            │
│                               ExecuteResult                                       │
│                                                                                   │
└───────────────────────────────────────────────────────────────────────────────────┘
```

---

## 1. Parser Engine (`component/language/parser/`)

Transforms MemQL source text into an Abstract Syntax Tree. The package's own
guide is [component/language/CLAUDE.md](../language/CLAUDE.md); the
construct-level grammar is in
[architecture.md](../../docs/public/concepts/architecture.md#parser-architecture),
and the language itself in
[memql.md](../../docs/public/language/memql.md).

### Files

| File | Purpose |
|------|---------|
| `lexer.go` | Tokenization |
| `rewriter.go` | The struct-form rewriter: a query and a mutation lowered to the internal procedural form |
| `parser.go` | Recursive descent parser: the top-level dispatch and every declaration |
| `v1_body.go` | The statement parser: a logic's and an automation's body, read as written |
| `v1_expr.go` | The edition-2026 expression grammar (`ParseV1Expression`, `ParseV1Lambda`) |
| `v1_refusals.go` / `v1_body_refusals.go` | The retired spellings and body forms, each refused by name |
| `errors.go` | Error types with position info |

### Two front halves

- A **query** and a **mutation** are rewritten first (`NormaliseAll`), then
  parsed; a refusal from the rewriter leads with `rewrite error at line L,
  column C:`.
- A **logic** and an **automation** are parsed as written into an
  `ast.Body` of statements (`component/language/ast/body.go`): assign, call,
  if, for, switch, parallel, publish and return.

Every expression an author writes -- a filter, a spec or trait body, a
trigger `@filter`, a statement, a condition, a mutation value -- is parsed by
the edition-2026 grammar into the node set of `component/language/ast/v1.go`
(`IdentExpr`, `MemberExpr`, `CallExpr`, `UnaryExpr`, `BinaryExpr`,
`TernaryExpr`, `LambdaExpr`, `ListExpr`, `MapExpr`, ...). The older node set
in `ast.go` (`LogicalExpr`, `ComparisonExpr`, `SortExpr`, ...) is the
engine's internal query form: the string an SDK sends to `Execute` and the
wrapper the rewriter generates.

`!` negates exactly in every position; there is no truthiness, so a
condition must be boolean.

---

## 2. Compiler Engine (`component/language/compiler/`)

Transforms the AST into what the engine loads.

### Files

| File | Purpose |
|------|---------|
| `compiler.go` / `api.go` | Main compiler interface and public API |
| `body_scope.go` | `CheckBody`: a body's scope and construct rules |
| `body_compile.go` | `CompileBody`: a body lowered to the executor's steps, in source order |
| `automation_generator.go` | An automation's trigger, args and steps as the executor's JSON |
| `function_generator.go` | AST → function definition |
| `composition.go` | The CQS file-composition rules |

### Compilation Flow

`CheckBody` refuses a name read before it is bound or outside the block that
binds it, a name bound twice, a bare argument read, and a construct call a
logic may not make; `CompileBody` then emits one step per statement in the
order written -- no sort -- flattening an `if` or a `switch` into the steps
of its branches, each carrying its branch's condition.

### Transpilation Example

```
INPUT (.memql)                          OUTPUT (steps)
─────────────────                       ─────────────────
@trigger(schedule="0 */5 * * * *")      {
automation test {                 →       "name": "test",
  users := query activeUsers()            "schedule": "0 */5 * * * *",
}                                         "steps": [{
                                            "id": "users",
                                            "type": "function",
                                            "binds": "users",
                                            "function": {
                                              "kind": "query",
                                              "name": "activeUsers"
                                            }
                                          }]
                                        }
```

A later statement reads `users` by its name; there is no `$`-expression
translation and no step accessor.

---

## 3. MemQL Engine (`engine/memql/`)

Executes queries against TimescaleDB and orchestrates supporting services.

### Core Components

```
┌─────────────────────────────────────────────────────────────────┐
│                       MEMQL ENGINE                              │
│                                                                 │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │                    MemoryEngine                           │  │
│  │                                                           │  │
│  │  Registries:                                              │  │
│  │  ├── concepts      (Concept definitions)                  │  │
│  │  ├── specs         (Query specifications)                 │  │
│  │  ├── functions     (Named functions)                      │  │
│  │  ├── tools         (Tool definitions)                     │  │
│  │  ├── prompts       (AI prompt templates)                  │  │
│  │  └── providers     (AI provider configs)                  │  │
│  │                                                           │  │
│  │  Services:                                                │  │
│  │  ├── cache         (Result caching)                       │  │
│  │  ├── siRuntime     (AI invocation)                        │  │
│  │  └── eventBus      (Event publishing)                     │  │
│  │                                                           │  │
│  │  Methods:                                                 │  │
│  │  ├── Execute()           (Run queries)                    │  │
│  │  ├── ResolveVariable()   (Fetch from v1:platform:partitionVariable)   │  │
│  │  └── InvokeAI()          (Execute AI prompts)             │  │
│  │                                                           │  │
│  └───────────────────────────────────────────────────────────┘  │
│                                                                 │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │                     Executor                              │  │
│  │                                                           │  │
│  │  ├── Query parsing (legacy parser)                        │  │
│  │  ├── Filter evaluation                                    │  │
│  │  ├── Relationship traversal                               │  │
│  │  ├── Sort / Pagination                                    │  │
│  │  ├── Shape template application                           │  │
│  │  └── Mutation execution (insert)                          │  │
│  └───────────────────────────────────────────────────────────┘  │
│                                                                 │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │                  In-process evaluator                     │  │
│  │                                                           │  │
│  │  EvalExpr (expr_eval.go) evaluates an edition-2026        │  │
│  │  expression over an ExprScope: a run's names and roots    │  │
│  │  (component/automations RunScope), a logic's bindings,    │  │
│  │  a mutation's args. Its functions are the catalog's       │  │
│  │  (component/language/functions), one spelling each.       │  │
│  └───────────────────────────────────────────────────────────┘  │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### Key Files

| File | Purpose |
|------|---------|
| `engine.go` | MemoryEngine struct, initialization, variable resolution |
| `executor.go` | Query execution logic |
| `expr_eval.go` | The in-process evaluator of edition-2026 expressions (`EvalExpr`) |
| `relations.go` | Relationship traversal |
| `shape_template.go` | Result shaping |
| `ai_runtime.go` | AI provider invocation |
| `result_cache.go` | Query result caching |
| `function_loader.go` | Load .memql functions |
| `spec_loader.go` | Load specifications |

### Variable Resolution

Variables are stored in the `v1:platform:partitionVariable` concept and resolved via:

```go
// Engine method
engine.ResolveVariable(ctx, "MEMQL_DEFAULT_USER_ROLE")

// In an expression, the catalog function var("NAME") reaches the same resolver
```

Query executed internally:
```memql
concept==v1:platform:partitionVariable;payload.name=="MEMQL_DEFAULT_USER_ROLE"
```

### Content-Addressed ID Generation

When `insert()` mutations are executed without an explicit `id` parameter, the engine generates a **deterministic content-addressed ID** from the concept name and payload.

#### ID Resolution Priority

The `Concept.Create()` method in `database/memory-nodes/concept.go` resolves IDs in this order:

```
1. Explicit id parameter     → params.ID (if provided)
2. ID from payload           → payload["id"] (if present)
3. Content-addressed hash    → SHA256(concept + payload + salt)
```

#### Content-Addressed ID Derivation

```go
// database/memory-nodes/concept.go
func (c *Concept) deriveContentId(payload map[string]any) string {
    input := map[string]any{
        "concept": c.Name,
        "payload": payload,
    }
    if c.contentIdSalt != "" {
        input["salt"] = c.contentIdSalt
    }
    return string(contentIdEngine.MustFromMap(input))
}
```

The `id` package (`core/id/`, imported here as `contentIdEngine = id.New()`) produces a 64-character hexadecimal SHA256 hash. The hash is deterministic: **same concept + payload + salt = same ID**.

#### Implications for Insert Behavior

```
┌─────────────────────────────────────────────────────────────────────────┐
│                 CONTENT-ADDRESSED ID BEHAVIOR                           │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│   insert("v1:space", payload={"name": "A"})                             │
│       │                                                                 │
│       ▼                                                                 │
│   ┌─────────────────────────────────────────────┐                       │
│   │  No explicit ID provided                    │                       │
│   │  → Derive ID from SHA256(concept + payload) │                       │
│   │  → ID = "v1:space:abc123..."                │                       │
│   └─────────────────────────────────────────────┘                       │
│       │                                                                 │
│       ▼                                                                 │
│   INSERT INTO memory_nodes (id, concept, payload, createdAt, ...)       │
│       │                                                                 │
│       ▼                                                                 │
│   Record created: id="v1:space:abc123...", createdAt=T1                 │
│                                                                         │
│   ─────────────────────────────────────────────────────────────         │
│                                                                         │
│   insert("v1:space", payload={"name": "A"})   ← SAME payload            │
│       │                                                                 │
│       ▼                                                                 │
│   ┌─────────────────────────────────────────────┐                       │
│   │  Derive ID → SAME hash = "v1:space:abc123"  │                       │
│   └─────────────────────────────────────────────┘                       │
│       │                                                                 │
│       ▼                                                                 │
│   INSERT INTO memory_nodes ...                                          │
│       │                                                                 │
│       ▼                                                                 │
│   NEW ROW created: id="v1:space:abc123...", createdAt=T2                │
│   (This is a NEW VERSION of the same record, not a duplicate)           │
│                                                                         │
│   Query returns most recent version (createdAt=T2)                      │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘
```

#### Key Design Principles

| Principle | Description |
|-----------|-------------|
| **Immutable time-series** | TimescaleDB stores all versions; queries return the latest |
| **Idempotent inserts** | Same payload = same ID = new version, not duplicate |
| **Replay safety** | Safe to retry or replay inserts without creating duplicates |
| **Explicit ID precedence** | `id="..."` parameter always overrides content-addressing |

#### Creating Unique Records

To create multiple independent records with the same payload structure:

```go
// Option 1: Explicit unique IDs
insert("v1:space", id="space-1", payload={"name": "A"})
insert("v1:space", id="space-2", payload={"name": "A"})

// Option 2: Unique payload content
insert("v1:space", payload={"name": "Space Alpha"})
insert("v1:space", payload={"name": "Space Beta"})

// Option 3: Include unique identifier in payload
insert("v1:space", payload={"name": "A", "uuid": "..."})
```

#### Server-Side Salt

A deployment-specific salt can be configured via `MEMORY_NODES_ZNASLLC_LAB_CONTENTID_SALT` to ensure IDs are unique across environments. The salt is set on concepts during registration via `Concept.SetContentIdSalt()`.

---

## Package Dependencies

```
                    ┌─────────────┐
                    │   (main)    │
                    │  cmd/, etc  │
                    └──────┬──────┘
                           │
              ┌────────────┼────────────┐
              │            │            │
              ▼            ▼            ▼
       ┌───────────┐ ┌───────────┐ ┌───────────┐
       │  server/  │ │   grpc/   │ │automations│
       └─────┬─────┘ └─────┬─────┘ └─────┬─────┘
             │             │             │
             └─────────────┼─────────────┘
                           │
                           ▼
                  ┌─────────────────┐
                  │  engine/memql   │
                  └────────┬────────┘
                           │
            ┌──────────────┼──────────────┐
            │              │              │
            ▼              ▼              ▼
     ┌───────────┐  ┌───────────┐  ┌───────────┐
     │  engine/  │  │  engine/  │  │ database/ │
     │  parser   │  │ compiler  │  │           │
     └───────────┘  └─────┬─────┘  └───────────┘
            ▲             │
            │             │
            └─────────────┘
          (compiler imports parser)
```

### Import Rules

| Package | Can Import | Cannot Import |
|---------|------------|---------------|
| `parser` | std lib only | compiler, memql |
| `compiler` | parser | memql |
| `memql` | parser, compiler, database | - |

---

## Extension Guide

### Adding a New Token Type

```go
// 1. lexer.go - Add token constant
const (
    TokenMyNew TokenType = iota + 100  // after existing tokens
)

// 2. lexer.go - Scan it
func (l *Lexer) NextToken() {
    case '@':
        if l.matchSequence('@', '@') {
            return makeToken(TokenMyNew, "@@")
        }
}
```

### Adding a New AST Node

```go
// 1. ast.go - Define the node
type MyNewExpr struct {
    Field string
    Value any
}

func (*MyNewExpr) node()           {}
func (*MyNewExpr) expressionNode() {}

// 2. parser.go - Parse it
func (p *Parser) parseMyNew() (*MyNewExpr, error) {
    // parsing logic
}
```

### Adding a Capability to Automations

A new thing an automation can do is a **builtin** (a Go integration behind
`@executor("integration.<name>.<verb>")`, called as `builtin <name>(...)`) or
an **action** over a declared capability -- not a new step type. The
statement set is closed; a new statement kind touches the AST
(`ast/body.go`), the statement parser (`parser/v1_body.go`), the compiler
(`body_scope.go`, `body_compile.go`), a step executor in
`component/automations/steps/`, and the statement cells under
`test/conformance/2026/statements/`, each pinned by a gate.

### Adding a New Function

A function an expression can call is one entry in the catalog,
`component/language/functions` (its spelling, signature, tier and the retired
spellings it replaces). Both evaluators, Sense and the generated docs read the
catalog, so there is no parser case or accessor node to add.

---

## Error Handling

```
┌─────────────────────────────────────────────────────────┐
│                    ERROR HIERARCHY                      │
│                                                         │
│  ParseError (engine/parser/errors.go)                   │
│  ├── Message      string                                │
│  ├── Pos          int      // character position        │
│  ├── Line         int      // line number               │
│  ├── Column       int      // column number             │
│  └── Token        *Token   // offending token           │
│                                                         │
│  Sentinel Errors:                                       │
│  ├── ErrEmptyInput                                      │
│  ├── ErrInvalidSyntax                                   │
│  ├── ErrUnexpectedToken                                 │
│  ├── ErrUnexpectedEOF                                   │
│  ├── ErrUnterminatedString                              │
│  └── ErrMissingArgument                                 │
│                                                         │
└─────────────────────────────────────────────────────────┘
```

---

## Testing Strategy

Run via `make test` (see root CLAUDE.md's Testing section -- a bare
`go test ./...` from the repo root misses this package, memql#4032). To
target one tree directly, use the module-path form, not a relative
`./engine/...` pattern (that directory does not exist):

| Package | Test Focus | Command |
|---------|------------|---------|
| `parser` | Lexer tokenization, parser grammar | `go test -count=1 github.com/znasllc-io/memql/component/language/parser/...` |
| `compiler` | AST to JSON, transpilation | `go test -count=1 github.com/znasllc-io/memql/component/language/compiler/...` |
| `memql` | Query execution, integration | `go test -count=1 github.com/znasllc-io/memql/component/memql/...` |

```bash
# Run all parser/compiler/memql tests
go test -count=1 github.com/znasllc-io/memql/component/language/... github.com/znasllc-io/memql/component/memql/...

# Run with verbose output
go test -count=1 github.com/znasllc-io/memql/component/memql/... -v

# Run a specific test
go test -count=1 github.com/znasllc-io/memql/component/language/parser/... -run TestLexer_LineCounterAdvancesThroughMultiLineString
```

---

*For system-wide architecture, see [`docs/public/concepts/architecture.md`](../../docs/public/concepts/architecture.md)*
