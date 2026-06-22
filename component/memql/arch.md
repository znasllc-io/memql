# Engine Package Architecture

> **Last Updated:** 2025-12-08

This document describes the architecture of the `engine/` package, which contains the core MemQL processing engines.

## Package Overview

```
engine/
├── parser/          # Lexical analysis and parsing
├── compiler/        # AST to target format transformation
├── memql/           # Query execution engine
│   └── sense/       # MemQL Sense language intelligence
└── engine.go        # Base engine interface
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
│   │   │ AutomationGenerator   │    │  FunctionGenerator    │                │     │
│   │   │                       │    │                       │                │     │
│   │   │   AST → .json         │    │   AST → definition    │                │     │
│   │   └───────────────────────┘    └───────────────────────┘                │     │
│   │                                                                         │     │
│   │   Responsibility: AST → Target Formats (JSON, definitions)              │     │
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

## 1. Parser Engine (`engine/parser/`)

Transforms MemQL source text into an Abstract Syntax Tree.

### Files

| File | Purpose |
|------|---------|
| `ast.go` | AST node type definitions |
| `lexer.go` | Tokenization |
| `parser.go` | Recursive descent parser |
| `errors.go` | Error types with position info |

### Lexer Flow

```
Input String
     │
     ▼
┌──────────────────────────────────────────────────────────┐
│                        LEXER                             │
│                                                          │
│  "automation test() { step1: query { ... } }"            │
│                          │                               │
│                          ▼                               │
│  ┌────────────────────────────────────────────────────┐  │
│  │ Skip Whitespace / Comments                         │  │
│  └────────────────────────────────────────────────────┘  │
│                          │                               │
│                          ▼                               │
│  ┌────────────────────────────────────────────────────┐  │
│  │ Scan Token                                         │  │
│  │  ├── Keywords (automation, query, mutation, when)  │  │
│  │  ├── Operators (==, !=, >, >=, <, <=, in, not in, has, &&, ??, ?.)│  │
│  │  ├── Literals (strings, numbers)                   │  │
│  │  ├── Identifiers (names, concept refs)             │  │
│  │  └── Structural (, ; : { } [ ] ( ))                │  │
│  └────────────────────────────────────────────────────┘  │
│                          │                               │
│                          ▼                               │
│  Token Stream: [KEYWORD:automation] [IDENT:test] ...     │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

### Parser Grammar (Simplified)

```
file         = { definition }
definition   = queryFunc | mutationFunc | automation | expression

queryFunc    = "query" name? "(" args ")" "{" expression "}"
mutationFunc = "mutation" name? "(" args ")" ["when" cond] "{" insert "}"
automation   = "automation" name "(" args ")" "{" { statement } "}"

statement    = schedule | enabled | step | return
step         = id ":" stepType ["when" cond] "{" body "}"

expression   = logicalOr
logicalOr    = logicalAnd { "," logicalAnd }
logicalAnd   = primary { ";" primary }
primary      = comparison | functionCall | conditionalFilter | grouped
```

### AST Node Types

```
Node
├── ExpressionNode
│   ├── LogicalExpr           // a; b (AND) or a, b (OR)
│   ├── ComparisonExpr        // field==value
│   ├── FunctionCallExpr      // func(args)
│   ├── RelationshipExpr      // parentOf(...), childOf(...)
│   ├── ConditionalFilterExpr // ?.field==value
│   ├── SortExpr              // sort(fields)(...)
│   ├── PaginateExpr          // paginate(limit,offset)(...)
│   ├── DepthExpr             // depth(n)(...)
│   ├── LiteralExpr           // "string", 123, true
│   ├── TernaryExpr           // cond ? then : else
│   │
│   │   // Accessor Expressions (for automations/mutations)
│   ├── ArgRefExpr            // args.name
│   ├── VarRefExpr            // var("NAME")
│   ├── StepRefExpr           // step("id")
│   ├── InputRefExpr          // input()
│   ├── ItemRefExpr           // item()
│   ├── IndexRefExpr          // index()
│   ├── EventRefExpr          // event()
│   ├── ErrorRefExpr          // error()
│   ├── TimestampExprFunc     // timestamp(), now()
│   ├── FieldRefExpr          // field(obj, "key")
│   │
│   │   // Helper Function Expressions
│   ├── ConcatExpr            // concat(a, b, ...)
│   ├── CoalesceExpr          // coalesce(a, b, ...)
│   ├── IfExpr                // if(cond, then, else)
│   ├── FirstExpr             // first(collection)
│   ├── LastExpr              // last(collection)
│   ├── LowerExpr             // lower(str)
│   ├── UpperExpr             // upper(str)
│   ├── TrimExpr              // trim(str)
│   ├── HashExpr              // hash(str)
│   └── ContainsExpr          // contains(str, substr)
│
├── StatementNode
│   ├── MutationStmt        // insert(...)
│   └── QueryStmt           // expression as statement
│
├── FunctionDef             // query/mutation/automation definition
│   ├── Name
│   ├── Type                // query | mutation | automation
│   ├── Args[]
│   └── Body
│
├── AutomationDef           // automation body
│   ├── Schedule
│   ├── Steps[]
│   ├── OnComplete
│   └── OnError
│
├── StepDef                 // automation step
│   ├── ID
│   ├── Type                // query | mutation | webhook | forEach...
│   ├── Condition
│   └── Config
│
└── File                    // parsed .memql file
    └── Definitions[]
```

---

## 2. Compiler Engine (`engine/compiler/`)

Transforms AST into target output formats.

### Files

| File | Purpose |
|------|---------|
| `compiler.go` | Main compiler interface |
| `api.go` | Public API functions |
| `automation_generator.go` | AST → JSON automation |
| `function_generator.go` | AST → function definition |

### Compilation Flow

```
┌──────────────────────────────────────────────────────────┐
│                    COMPILER                              │
│                                                          │
│   Input: AST (*File or *FunctionDef)                     │
│                                                          │
│   ┌───────────────────────────────────────────────────┐  │
│   │              Type Detection                       │  │
│   │                                                   │  │
│   │  FunctionDef.Type == ?                            │  │
│   │    ├── Automation → AutomationGenerator           │  │
│   │    ├── Query      → FunctionGenerator             │  │
│   │    └── Mutation   → FunctionGenerator             │  │
│   └───────────────────────────────────────────────────┘  │
│                          │                               │
│          ┌───────────────┴───────────────┐               │
│          ▼                               ▼               │
│   ┌──────────────────┐        ┌───────────────────┐      │
│   │ Automation       │        │ Function          │      │
│   │ Generator        │        │ Generator         │      │
│   │                  │        │                   │      │
│   │ Outputs:         │        │ Outputs:          │      │
│   │  - name.json     │        │  - function def   │      │
│   │                  │        │  - function.memql │      │
│   └──────────────────┘        └───────────────────┘      │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

### API Functions

```go
// High-level compilation
CompileSource(source string) (*CompileResult, error)
CompileFile(path string) (*CompileResult, error)
TranspileAutomation(source string) (string, error)

// Validation & inspection
ValidateMemQL(source string) error
DetectFileType(source string) (FileType, error)
GetAutomationName(source string) (string, error)
IsAutomationFile(source string) bool
ParseMemQL(source string) (Node, error)
```

### Transpilation Example

```
INPUT (.memql)                          OUTPUT (.json)
─────────────────                       ─────────────────
automation test() {                     {
  schedule "*/5 * * * *"          →       "name": "test",
  step1: query {                          "schedule": "*/5 * * * *",
    concept==v1:user                      "steps": [{
  }                                         "id": "step1",
}                                           "type": "query",
                                            "query": {
                                              "query": "concept==v1:user"
                                            }
                                          }],
                                          "enabled": true
                                        }
```

### Expression Translation

The compiler translates accessor expressions to JSON `$` format:

| MemQL Syntax | JSON Format |
|--------------|-------------|
| `var("NAME")` | `$var.NAME` |
| `step("id")` | `$steps.id.result` |
| `step("id").metadata.itemCount` | `$steps.id.metadata.itemCount` |
| `input()` | `$input` |
| `item()` | `$item` |
| `index()` | `$index` |
| `event()` | `$event` |
| `error()` | `$error` |
| `timestamp()` | `$timestamp` |
| `field(item(), "name")` | `$item.name` |

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
│  │  └── InvokeSI()          (Execute AI prompts)             │  │
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
│  │                  RuntimeEvaluator                         │  │
│  │                                                           │  │
│  │  Evaluates accessor expressions during automation:        │  │
│  │  ├── EvaluateArg()       → args.name                    │  │
│  │  ├── EvaluateVar()       → var("NAME")                    │  │
│  │  ├── EvaluateStep()      → step("id")                     │  │
│  │  ├── EvaluateInput()     → input()                        │  │
│  │  ├── EvaluateItem()      → item()                         │  │
│  │  ├── EvaluateIndex()     → index()                        │  │
│  │  ├── EvaluateTimestamp() → timestamp()                    │  │
│  │  ├── EvaluateConcat()    → concat(a, b, ...)              │  │
│  │  ├── EvaluateCoalesce()  → coalesce(a, b, ...)            │  │
│  │  └── ... (count, first, last, lower, upper, etc.)         │  │
│  └───────────────────────────────────────────────────────────┘  │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### Key Files

| File | Purpose |
|------|---------|
| `engine.go` | MemoryEngine struct, initialization, variable resolution |
| `executor.go` | Query execution logic |
| `runtime_evaluator.go` | Accessor expression evaluation at runtime |
| `relations.go` | Relationship traversal |
| `shape_template.go` | Result shaping |
| `si_runtime.go` | AI provider invocation |
| `result_cache.go` | Query result caching |
| `function_loader.go` | Load .memql functions |
| `spec_loader.go` | Load specifications |

### RuntimeContext

The `RuntimeEvaluator` uses a `RuntimeContext` to resolve accessor expressions:

```go
type RuntimeContext struct {
    Engine *MemoryEngine       // For var() resolution
    Args   map[string]any      // For arg() resolution
    Steps  map[string]*StepResult // For step() resolution
    Input  any                 // For input() resolution
    Item   any                 // For item() in forEach
    Index  int                 // For index() in forEach
    Event  map[string]any      // For event() triggers
    Error  string              // For error() in onError
}
```

### Variable Resolution

Variables are stored in the `v1:platform:partitionVariable` concept and resolved via:

```go
// Engine method
engine.ResolveVariable(ctx, "MEMQL_DEFAULT_USER_ROLE")

// RuntimeEvaluator method (delegates to engine)
evaluator.EvaluateVar(ctx, "MEMQL_DEFAULT_USER_ROLE")
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

The `contentid` package (`database/contentid/`) produces a 64-character hexadecimal SHA256 hash. The hash is deterministic: **same concept + payload + salt = same ID**.

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

### Adding a New Automation Step Type

```go
// 1. ast.go - Add step type
const StepTypeMyStep StepType = "mystep"

type MyStepConfig struct {
    // config fields
}

// 2. parser.go - Parse it in parseStep()
case "mystep":
    stepType = StepTypeMyStep

// 3. automation_generator.go - Generate JSON
case StepTypeMyStep:
    // output generation
```

### Adding a New Accessor Function

```go
// 1. ast.go - Define the expression node
type MyAccessorExpr struct {
    Target ExpressionNode
}

func (*MyAccessorExpr) node()           {}
func (*MyAccessorExpr) expressionNode() {}

// 2. parser.go - Add to parseFunctionCall()
case "myaccessor":
    return p.parseMyAccessor()

func (p *Parser) parseMyAccessor() (ExpressionNode, error) {
    target, err := p.parseExpressionArg()
    // ...
    return &MyAccessorExpr{Target: target}, nil
}

// 3. automation_generator.go - Add to expressionToString()
case *parser.MyAccessorExpr:
    return fmt.Sprintf("myaccessor(%s)", c.expressionToString(e.Target))

// 4. automation_generator.go - Add to expressionToJSONExpr()
case *parser.MyAccessorExpr:
    return fmt.Sprintf("$myaccessor.%s", c.expressionToJSONExpr(e.Target))

// 5. runtime_evaluator.go - Add evaluation method
func (e *RuntimeEvaluator) EvaluateMyAccessor(target any) any {
    // evaluation logic
}
```

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

| Package | Test Focus | Command |
|---------|------------|---------|
| `parser` | Lexer tokenization, parser grammar | `go test ./engine/parser/...` |
| `compiler` | AST to JSON, transpilation | `go test ./engine/compiler/...` |
| `memql` | Query execution, integration | `go test ./engine/memql/...` |

```bash
# Run all engine tests
go test ./engine/...

# Run with verbose output
go test ./engine/... -v

# Run specific test
go test ./engine/parser -run TestLexer
```

---

*For system-wide architecture, see `/docs/arch.md`*
