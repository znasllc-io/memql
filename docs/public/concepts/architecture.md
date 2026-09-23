---
title: MemQL Engine Architecture
audience: public
status: stable
area: concepts
sinceVersion: 0.9.0
owner: znas
---

# MemQL Engine Architecture

## Overview

MemQL is a domain-specific query language for time-series memory graphs, built on TimescaleDB. This document describes the modular engine architecture that powers MemQL's parsing, compilation, and execution pipeline.

## Table of Contents

1. [High-Level Architecture](#high-level-architecture)
2. [Engine Components](#engine-components)
3. [Parser Engine](#parser-engine)
4. [Compiler Engine](#compiler-engine)
5. [Executor Engine](#executor-engine)
6. [Data Flow](#data-flow)
7. [Module Dependencies](#module-dependencies)
8. [Extension Points](#extension-points)

---

## High-Level Architecture

The MemQL system follows a classic compiler architecture with distinct phases for lexical analysis, parsing, compilation, and execution.

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                              MemQL SYSTEM ARCHITECTURE                              │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                     │
│   ┌─────────────┐    ┌─────────────┐    ┌─────────────┐    ┌─────────────┐          │
│   │   Source    │    │   Parser    │    │  Compiler   │    │  Executor   │          │
│   │   (.memql)  ───▶    Engine    ───▶    Engine   ───▶     Engine                │
│   └─────────────┘    └─────────────┘    └─────────────┘    └─────────────┘          │
│                             │                  │                  │                 │
│                             ▼                  ▼                  ▼                 │
│                      ┌───────────┐      ┌───────────┐      ┌───────────┐            │
│                      │    AST    │      │   JSON    │      │  Results  │            │
│                      │  (in-mem) │      │  Output   │      │  (Bundle) │            │
│                      └───────────┘      └───────────┘      └───────────┘            │
│                                                                                     │
│   ┌──────────────────────────────────────────────────────────────────────────────┐  │
│   │                           SUPPORTING SERVICES                                │  │
│   │  ┌────────────┐  ┌────────────┐  ┌────────────┐  ┌────────────┐              │  │
│   │  │ AI Runtime │  │   Event    │  │   Result   │  │   Schema   │              │  │
│   │  │            │  │    Bus     │  │   Cache    │  │  Registry  │              │  │
│   │  └────────────┘  └────────────┘  └────────────┘  └────────────┘              │  │
│   │  ┌────────────┐  ┌────────────┐  ┌────────────┐                              │  │
│   │  │ Component  │  │  Config    │  │ Telemetry  │  (channel-based bus layer)    │  │
│   │  │    Bus     │  │  Loader    │  │ Collector  │                              │  │
│   │  └────────────┘  └────────────┘  └────────────┘                              │  │
│   └──────────────────────────────────────────────────────────────────────────────┘  │
│                                                                                     │
│   ┌──────────────────────────────────────────────────────────────────────────────┐  │
│   │                            DATA LAYER                                        │  │
│   │  ┌────────────────────────────────────────────────────────────────────────┐  │  │
│   │  │                         TimescaleDB                                    │  │  │
│   │  │    ┌─────────────────────┐  ┌─────────────────────┐                    │  │  │
│   │  │    │     MemoryNodes     │  │  SecretMemoryNodes  │                    │  │  │
│   │  │    │    (hypertable)     │  │    (hypertable)     │                    │  │  │
│   │  │    │ PK: (id, createdAt) │  │                     │                    │  │  │
│   │  │    └─────────────────────┘  └─────────────────────┘                    │  │  │
│   │  └────────────────────────────────────────────────────────────────────────┘  │  │
│   └──────────────────────────────────────────────────────────────────────────────┘  │
│                                                                                     │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

### Component Bus Layer

Components communicate via typed Go channels carrying protobuf-defined messages
(`component/bus/bus.proto`). The bus provides:

- **Typed channels** -- `EngineRequests`, `IntegrationRequests`, `EventPublishCh`, `ConfigCh`, `TelemetryCh`, `ReadyCh`, `ShutdownCh`
- **ReplyTo pattern** -- Request-response over channels via embedded reply channel (buffered, size 1)
- **Backpressure** -- Buffered channels (default 64) with non-blocking send and drop counting
- **Telemetry** -- Channel fill-level sampling, message send/drop counters
- **Proto messages** -- dozens of typed messages in the `InternalMessage` envelope (database, engine, integration, event, config, telemetry, lifecycle); see `component/bus/bus.proto` for the current set
- **Correlation IDs** -- Every message carries a `correlation_id` for distributed tracing across channel hops

All components implement `Ready() <-chan struct{}` for parallel startup coordination,
and accept `SetWiring(*bus.Wiring)` to receive channel-based communication.

---

## Engine Components

### Component Registry

```
component/
├── language/
│   ├── parser/            # Lexer, Parser, AST definitions
│   │   ├── ast.go         # Abstract Syntax Tree node types
│   │   ├── lexer.go       # Tokenization
│   │   ├── parser.go      # Recursive descent parser
│   │   └── errors.go      # Error types with position info
│   │
│   └── compiler/          # AST to target format transformation
│       ├── compiler.go    # Main compiler interface
│       ├── api.go         # Public API functions
│       ├── automation_generator.go  # AST → JSON automation
│       └── function_generator.go    # AST → function definition
│
└── memql/                 # Query execution
    ├── engine.go          # Main memory engine
    ├── executor.go        # Query execution
    ├── relations.go       # Relationship traversal
    └── ...
```

### Responsibility Matrix

| Component | Input | Output | Responsibility |
|-----------|-------|--------|----------------|
| **Lexer** | Source string | Token stream | Tokenization, keyword recognition |
| **Parser** | Token stream | AST | Syntax analysis, tree construction |
| **Compiler** | AST | JSON/MemQL | Code generation, format conversion |
| **Executor** | AST/Query | Results | Database operations, filtering |
| **AI Runtime** | Prompts | AI responses | LLM invocations |
| **Event Bus** | Events | Subscribers | Inter-component communication |

---

## Parser Engine

The Parser Engine (`component/language/parser/`) transforms MemQL source text into an Abstract Syntax Tree (AST).

### Lexer Architecture

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                                   LEXER PIPELINE                                    │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                     │
│   Input: "concept==v1:crm:lead && payload.active==true"                             │
│                                                                                     │
│   ┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────┐                      │
│   │  Input   │    │  Rune    │    │  Token   │    │  Token   │                      │
│   │  String  ───▶   Stream  ───▶    Scanner ───▶   Stream                         │
│   └──────────┘    └──────────┘    └──────────┘    └──────────┘                      │
│                                         │                                           │
│                         ┌───────────────┼───────────────┐                           │
│                         ▼               ▼               ▼                           │
│                   ┌──────────┐   ┌──────────┐   ┌──────────┐                        │
│                   │ Keyword  │   │ Operator │   │ Literal  │                        │
│                   │ Matcher  │   │ Scanner  │   │ Scanner  │                        │
│                   └──────────┘   └──────────┘   └──────────┘                        │
│                                                                                     │
│   Output Tokens:                                                                    │
│   ┌─────────────────────────────────────────────────────────────────────────────┐   │
│   │ [IDENT:concept] [OP:==] [IDENT:v1:crm:lead] [OP:&&] [IDENT:payload.active]  │   │
│   │ [OP:==] [IDENT:true] [EOF]                                                  │   │
│   └─────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                     │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

### Token Types

```
┌────────────────────────────────────────────────────────────────────────────────┐
│                              TOKEN TYPE HIERARCHY                              │
├────────────────────────────────────────────────────────────────────────────────┤
│                                                                                │
│  TokenType                                                                     │
│  ├── Structural                                                                │
│  │   ├── TokenEOF             // End of file                                   │
│  │   ├── TokenParenOpen       // (                                             │
│  │   ├── TokenParenClose      // )                                             │
│  │   ├── TokenBraceOpen       // {                                             │
│  │   ├── TokenBraceClose      // }                                             │
│  │   ├── TokenBracketOpen     // [                                             │
│  │   ├── TokenBracketClose    // ]                                             │
│  │   ├── TokenColon           // :                                             │
│  │   ├── TokenSemicolon       // ;                                             │
│  │   ├── TokenComma           // ,  (argument / list separator)                │
│  │   └── TokenAt              // @  (annotation prefix)                        │
│  │                                                                             │
│  ├── Values                                                                    │
│  │   ├── TokenIdentifier      // names, paths, concept refs                    │
│  │   ├── TokenNumber          // integers, floats                              │
│  │   └── TokenString          // "quoted strings"                              │
│  │                                                                             │
│  ├── Operators                                                                 │
│  │   ├── TokenOperator        // ==, !=, >, >=, <, <=, in, etc.                │
│  │   ├── TokenAmpAmp          // && (logical AND)                              │
│  │   ├── TokenPipePipe        // || (logical OR)                               │
│  │   ├── TokenDefine          // :=                                            │
│  │   └── TokenQuestion        // ?  (ternary)                                  │
│  │                                                                             │
│  └── Keywords (a selection: the full set is TokenType in                       │
│      │        component/language/parser/lexer.go)                              │
│      ├── TokenKeywordQuery        // query                                     │
│      ├── TokenKeywordMutation     // mutation                                  │
│      ├── TokenKeywordAutomation   // automation                                │
│      ├── TokenKeywordUse          // use                                       │
│      ├── TokenKeywordIf / Else    // if, else                                  │
│      ├── TokenKeywordFor / In     // for, in                                   │
│      ├── TokenKeywordSwitch       // switch, case, default                     │
│      ├── TokenKeywordRetry        // retry                                     │
│      ├── TokenKeywordReturn       // return                                    │
│      └── TokenKeywordNil          // nil                                       │
│                                                                                │
└────────────────────────────────────────────────────────────────────────────────┘
```

### Parser Architecture

The parser uses recursive descent. The construct-level grammar, in EBNF (the
expression grammar is the one [memql.md](../language/memql.md#operators)
publishes, and the statement grammar is specified under
[Bodies](../language/memql.md#bodies)):

```
file         = { useDecl } { definition } ;
useDecl      = "use" module "." "{" name { "," name } "}" ;
definition   = { annotation } ( query | mutation | logic | automation | ... ) ;

query        = "query" concept identifier "{" [ argsBlock ] { queryClause } "}" ;
queryClause  = "filter" lambda | "sort" string "," string | "paginate" number
             | "refine" lambda | "count" | "asOf" expression | "shape" identifier ;
mutation     = "mutation" concept identifier "{" [ argsBlock ]
               ( "insert" | "update" ) "{" fieldList "}" "}" ;
logic        = "logic" identifier "{" [ argsBlock ] { statement } "}" ;
automation   = "automation" identifier "{" [ argsBlock ] { precondition }
               statement { statement } "}" ;

statement    = assign | call | if | for | switch | parallel | publish | return ;
assign       = identifier ":=" ( call | expression ) ;
call         = kind identifier "(" [ namedArg { "," namedArg } ] ")"
               [ "on" "surface" "(" string ")" ] [ "retry" "(" number ")" ]
               [ "on" "error" "continue" ] ;
kind         = "query" | "mutation" | "logic" | "builtin" | "automation" | "action" ;
namedArg     = identifier ":" expression ;
if           = "if" expression block { "else" "if" expression block } [ "else" block ] ;
for          = "for" identifier "in" expression [ "if" expression ] block
               [ "on" "error" "continue" ] ;
switch       = "switch" expression "{" { "case" literal { "," literal } block }
               [ "default" block ] "}" ;
parallel     = "parallel" "{" "branch" identifier block { "branch" identifier block } "}"
               [ "wait" "any" ] [ "on" "error" "continue" ] ;
publish      = "publish" string map ;
return       = "return" [ call | expression ] ;
block        = "{" { statement } "}" ;

argsBlock    = "args" "{" { argDecl } "}" ;
lambda       = identifier "=>" expression ;
```

One statement per line; `else` follows its closing brace on the same line. A
query and a mutation reach the parser through the struct-form rewriter; a
logic and an automation are read as written (`parser/v1_body.go`).

### AST Node Hierarchy

```
┌──────────────────────────────────────────────────────────────────────────────────────┐
│                              AST NODE HIERARCHY                                      │
├──────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                      │
│  Node (interface)                                                                    │
│  │                                                                                   │
│  ├── ExpressionNode (interface)                                                      │
│  │   ├── LogicalExpr          // AND/OR combinations                                 │
│  │   ├── ComparisonExpr       // field == value                                      │
│  │   ├── RelationshipExpr     // parentOf(), childOf(), etc.                         │
│  │   ├── FunctionCallExpr     // userFunc(args)                                      │
│  │   ├── SortExpr             // sort(fields)(...)                                   │
│  │   ├── PaginateExpr         // paginate(limit)(...)                                │
│  │   ├── SelectExpr           // select(fields)(...)                                 │
│  │   ├── DepthExpr            // depth(n)(...)                                       │
│  │   ├── ShapeExpr            // shape(template)(...)                                │
│  │   ├── ConditionalFilterExpr// when(args.x) { field==args.x }                      │
│  │   ├── ArgRefExpr           // args.name                                           │
│  │   ├── LiteralExpr          // "string", 123, true                                 │
│  │   └── AIExpr               // ai("template", data)                                │
│  │                                                                                   │
│  ├── StatementNode (interface)                                                       │
│  │   ├── MutationStmt         // insert { ... } / update { ... }                     │
│  │   └── QueryStmt            // expression as statement                             │
│  │                                                                                   │
│  ├── FunctionDef              // query/mutation/logic/automation definition          │
│  │   ├── Name                 // function name                                       │
│  │   ├── Type                 // query | mutation | logic | automation               │
│  │   ├── Args                 // []FunctionArg                                       │
│  │   └── Body                 // Node (expression, mutation, or AutomationDef)       │
│  │                                                                                   │
│  ├── AutomationDef            // a logic's or an automation's body                   │
│  │   ├── Schedule             // cron expression (@trigger(schedule=...))            │
│  │   ├── Trigger              // event trigger                                       │
│  │   └── Body                 // *Body: the statements, in the order written         │
│  │                                                                                   │
│  ├── Body                     // component/language/ast/body.go                      │
│  │   └── Statements           // assign, call, if, for, switch, parallel,            │
│  │                            // publish, return                                     │
│  │                                                                                   │
│  └── File                     // parsed .memql file                                  │
│      └── Definitions          // []Node                                              │
│                                                                                      │
└──────────────────────────────────────────────────────────────────────────────────────┘
```

The expression nodes above are what the engine's internal query form parses
to: the string an SDK sends to `Execute`, and the wrapper the struct-form
rewriter generates. An expression an author writes -- a filter, a spec body,
a condition, a logic statement -- parses to the smaller edition-2026 set in
`component/language/ast/v1.go`: `IdentExpr`, `MemberExpr`, `CallExpr`,
`UnaryExpr`, `BinaryExpr`, `ListExpr`, `MapExpr` and `ParenExpr`, plus the
`LambdaExpr`, `TernaryExpr`, `LiteralExpr` and `NilExpr` shared with the set
above. A payload field is a `MemberExpr` on the lambda parameter and applying
a spec is a `CallExpr`, so `ConditionalFilterExpr` (the retired
`when(args.x) { ... }` guard) has no edition-2026 counterpart: the guard is
written `args.x == nil || row.f == args.x`. See
[the internal query form](../language/memql.md#the-internal-query-form).

---

## Compiler Engine

The Compiler Engine (`component/language/compiler/`) transforms AST nodes into target output formats, primarily JSON for automations.

### Compilation Pipeline

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                              COMPILATION PIPELINE                                    │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                      │
│   SOURCE (.memql)                                                                    │
│   ┌─────────────────────────────────────────────────────────────────────────────┐   │
│   │  /// Process active leads every 30 minutes                                   │   │
│   │  @trigger(schedule="0 */30 * * * *")                                         │   │
│   │  automation leadProcessor {                                                  │   │
│   │      fetchLeads := query activeLeads()                                       │   │
│   │  }                                                                           │   │
│   └─────────────────────────────────────────────────────────────────────────────┘   │
│                                         │                                            │
│                                         ▼                                            │
│   ┌─────────────────────────────────────────────────────────────────────────────┐   │
│   │                              LEXER + PARSER                                  │   │
│   │                                                                              │   │
│   │  1. Tokenize source string                                                   │   │
│   │  2. Parse tokens into AST                                                    │   │
│   │  3. Validate syntax                                                          │   │
│   └─────────────────────────────────────────────────────────────────────────────┘   │
│                                         │                                            │
│                                         ▼                                            │
│   AST (*File)                                                                        │
│   ┌─────────────────────────────────────────────────────────────────────────────┐   │
│   │  File {                                                                      │   │
│   │    Definitions: [                                                            │   │
│   │      FunctionDef {                                                           │   │
│   │        Name: "leadProcessor"                                                 │   │
│   │        Type: FunctionTypeAutomation                                          │   │
│   │        Body: AutomationDef {                                                 │   │
│   │          Schedule: "0 */30 * * * *"                                          │   │
│   │          Body: Body { Statements: [                                          │   │
│   │            AssignStatement { Name: "fetchLeads",                             │   │
│   │              Call: ConstructCall { Kind: "query", Name: "activeLeads" } }    │   │
│   │          ] }                                                                 │   │
│   │        }                                                                     │   │
│   │      }                                                                       │   │
│   │    ]                                                                         │   │
│   │  }                                                                           │   │
│   └─────────────────────────────────────────────────────────────────────────────┘   │
│                                         │                                            │
│                                         ▼                                            │
│   ┌─────────────────────────────────────────────────────────────────────────────┐   │
│   │                           CODE GENERATORS                                    │   │
│   │                                                                              │   │
│   │  ┌──────────────────────┐    ┌──────────────────────┐                       │   │
│   │  │ AutomationGenerator  │    │  FunctionGenerator   │                       │   │
│   │  │                      │    │                      │                       │   │
│   │  │ AST → JSON           │    │ AST → function def   │                       │   │
│   │  │      automation      │    │      + function.memql│                       │   │
│   │  └──────────────────────┘    └──────────────────────┘                       │   │
│   └─────────────────────────────────────────────────────────────────────────────┘   │
│                                         │                                            │
│                                         ▼                                            │
│   OUTPUT (.json)                                                                     │
│   ┌─────────────────────────────────────────────────────────────────────────────┐   │
│   │  {                                                                           │   │
│   │    "name": "leadProcessor",                                                  │   │
│   │    "schedule": "0 */30 * * * *",                                             │   │
│   │    "steps": [                                                                │   │
│   │      {                                                                       │   │
│   │        "id": "fetchLeads",                                                   │   │
│   │        "type": "function",                                                   │   │
│   │        "binds": "fetchLeads",                                                │   │
│   │        "function": {                                                         │   │
│   │          "kind": "query",                                                    │   │
│   │          "name": "activeLeads"                                               │   │
│   │        }                                                                     │   │
│   │      }                                                                       │   │
│   │    ],                                                                        │   │
│   │    "enabled": true                                                           │   │
│   │  }                                                                           │   │
│   └─────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                      │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

### Compiler API

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                                 COMPILER API                                         │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                      │
│  // High-Level Functions                                                             │
│  ┌───────────────────────────────────────────────────────────────────────────────┐  │
│  │  CompileSource(source string) (*CompileResult, error)                         │  │
│  │      - Lexes, parses, and compiles MemQL source                               │  │
│  │      - Returns automations and functions                                       │  │
│  │                                                                                │  │
│  │  CompileFile(inputPath string) (*CompileResult, error)                        │  │
│  │      - Reads file and calls CompileSource                                     │  │
│  │                                                                                │  │
│  │  TranspileAutomation(source string) (string, error)                           │  │
│  │      - Quick conversion from .memql to JSON string                            │  │
│  │      - Primary entry point for automation transpilation                        │  │
│  │                                                                                │  │
│  │  CompileToDirectory(inputPath, outputDir string) error                        │  │
│  │      - Compiles and writes all outputs to directory                           │  │
│  └───────────────────────────────────────────────────────────────────────────────┘  │
│                                                                                      │
│  // Validation & Inspection                                                          │
│  ┌───────────────────────────────────────────────────────────────────────────────┐  │
│  │  ValidateMemQL(source string) error                                           │  │
│  │      - Syntax validation without compilation                                   │  │
│  │                                                                                │  │
│  │  DetectFileType(source string) (FileType, error)                              │  │
│  │      - Returns: FileTypeQuery | FileTypeMutation | FileTypeAutomation         │  │
│  │                                                                                │  │
│  │  GetAutomationName(source string) (string, error)                             │  │
│  │      - Extracts automation name without full parse                            │  │
│  │                                                                                │  │
│  │  IsAutomationFile(source string) bool                                         │  │
│  │      - Quick check for automation content                                     │  │
│  │                                                                                │  │
│  │  ParseMemQL(source string) (parser.Node, error)                               │  │
│  │      - Returns raw AST for inspection                                         │  │
│  └───────────────────────────────────────────────────────────────────────────────┘  │
│                                                                                      │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

---

## Executor Engine

The Executor Engine (in `component/memql/`) executes parsed queries against the database.

### Query Execution Flow

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                            QUERY EXECUTION FLOW                                      │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                      │
│   ┌─────────────────┐                                                               │
│   │  Query String   │                                                               │
│   │                 │                                                               │
│   │                 │                                                               │
│   │   concept==     │                                                               │
│   │   v1:crm:lead   │                                                               │
│   │                 │                                                               │
│   └────────┬────────┘                                                               │
│            │                                                                         │
│            ▼                                                                         │
│   ┌─────────────────┐                                                               │
│   │    Parse        │                                                               │
│   │    (Legacy)     │───────────────▶ QueryPlan                                     │
│   └────────┬────────┘                    │                                          │
│            │                             ├── Root: ExpressionNode                    │
│            │                             ├── Filters: []FilterNode                   │
│            │                             ├── Relationships: []RelationshipNode       │
│            │                             ├── Limit, Offset, Depth                    │
│            │                             ├── Sort: []SortField                       │
│            │                             └── ShapeTemplate                           │
│            ▼                                                                         │
│   ┌─────────────────┐                                                               │
│   │  Resolve Specs  │   Expand @spec references                                     │
│   └────────┬────────┘                                                               │
│            │                                                                         │
│            ▼                                                                         │
│   ┌─────────────────┐                                                               │
│   │  Resolve        │   Expand function() calls                                     │
│   │  Functions      │                                                               │
│   └────────┬────────┘                                                               │
│            │                                                                         │
│            ▼                                                                         │
│   ┌─────────────────┐      ┌─────────────────┐                                      │
│   │  Check Cache    │─────▶│   Cache Hit?    │                                      │
│   └────────┬────────┘      └────────┬────────┘                                      │
│            │                        │                                                │
│            │ miss                   │ hit                                            │
│            ▼                        ▼                                                │
│   ┌─────────────────┐      ┌─────────────────┐                                      │
│   │  Build SQL      │      │ Return Cached   │                                      │
│   │  + Execute      │      │ Result          │                                      │
│   └────────┬────────┘      └─────────────────┘                                      │
│            │                                                                         │
│            ▼                                                                         │
│   ┌─────────────────┐                                                               │
│   │  Traverse       │   Follow relationship edges                                   │
│   │  Relationships  │                                                               │
│   └────────┬────────┘                                                               │
│            │                                                                         │
│            ▼                                                                         │
│   ┌─────────────────┐                                                               │
│   │  Apply Sort     │                                                               │
│   │  + Pagination   │                                                               │
│   └────────┬────────┘                                                               │
│            │                                                                         │
│            ▼                                                                         │
│   ┌─────────────────┐                                                               │
│   │  Apply Shape    │   Transform output structure                                  │
│   │  Template       │                                                               │
│   └────────┬────────┘                                                               │
│            │                                                                         │
│            ▼                                                                         │
│   ┌─────────────────┐                                                               │
│   │  ExecuteResult  │                                                               │
│   │                 │                                                               │
│   │  ├─ Bundle      │  (GraphBundle with nodes + edges)                             │
│   │  ├─ Modules     │  (matched MemoryNodes)                                        │
│   │  └─ Shaped      │  (transformed output)                                         │
│   └─────────────────┘                                                               │
│                                                                                      │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

---

## Data Flow

### End-to-End Request Processing

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                         END-TO-END REQUEST PROCESSING                                │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                      │
│  Client                                                                              │
│  ┌──────────┐                                                                       │
│  │ gRPC     │  (MemqlService.Stream -- the primary surface)                        │
│  │ WebSocket│  (browser bridge -> the same gRPC stream, /memql/ws)                 │
│  └────┬─────┘                                                                       │
│       │                                                                              │
│       │  { "query": "concept==v1:user" }                                             │
│       ▼                                                                              │
│  ┌────────────────────────────────────────────────────────────────────────────────┐ │
│  │                              API LAYER                                          │ │
│  │  ┌─────────────┐    ┌─────────────┐    ┌─────────────┐                         │ │
│  │  │  Auth       │───▶│  Validate   │───▶│  Route      │                         │ │
│  │  │  (JWT)      │    │  Request    │    │  Handler    │                         │ │
│  │  └─────────────┘    └─────────────┘    └──────┬──────┘                         │ │
│  └───────────────────────────────────────────────┼────────────────────────────────┘ │
│                                                  │                                   │
│                                                  ▼                                   │
│  ┌────────────────────────────────────────────────────────────────────────────────┐ │
│  │                            MEMQL ENGINE                                         │ │
│  │                                                                                  │ │
│  │  ┌──────────────────────────────────────────────────────────────────────────┐  │ │
│  │  │                         Parse Phase                                       │  │ │
│  │  │                                                                           │  │ │
│  │  │  Query String ──▶ Lexer ──▶ Parser ──▶ QueryPlan                         │  │ │
│  │  └──────────────────────────────────────────────────────────────────────────┘  │ │
│  │                                    │                                            │ │
│  │                                    ▼                                            │ │
│  │  ┌──────────────────────────────────────────────────────────────────────────┐  │ │
│  │  │                       Execute Phase                                       │  │ │
│  │  │                                                                           │  │ │
│  │  │  QueryPlan ──▶ ResolveFunctions ──▶ BuildSQL ──▶ Execute                 │  │ │
│  │  │                       │                              │                    │  │ │
│  │  │                       ▼                              ▼                    │  │ │
│  │  │              ┌────────────────┐            ┌────────────────┐            │  │ │
│  │  │              │ FunctionReg.   │            │  TimescaleDB   │            │  │ │
│  │  │              │ (.memql files) │            │                │            │  │ │
│  │  │              └────────────────┘            └────────────────┘            │  │ │
│  │  └──────────────────────────────────────────────────────────────────────────┘  │ │
│  │                                    │                                            │ │
│  │                                    ▼                                            │ │
│  │  ┌──────────────────────────────────────────────────────────────────────────┐  │ │
│  │  │                       Post-Process Phase                                  │  │ │
│  │  │                                                                           │  │ │
│  │  │  Raw Results ──▶ Traverse Relations ──▶ Apply Shape ──▶ Format Output    │  │ │
│  │  │                                                                           │  │ │
│  │  │               Optional: AI post-processing                               │  │ │
│  │  │               ┌────────────────┐                                         │  │ │
│  │  │               │   AI Runtime   │                                         │  │ │
│  │  │               │ (a prompt run  │                                         │  │ │
│  │  │               │  from Go)      │                                         │  │ │
│  │  │               └────────────────┘                                         │  │ │
│  │  └──────────────────────────────────────────────────────────────────────────┘  │ │
│  │                                                                                  │ │
│  └──────────────────────────────────────────────────────────────────────────────────┘ │
│                                    │                                                 │
│                                    ▼                                                 │
│  ┌────────────────────────────────────────────────────────────────────────────────┐ │
│  │                            RESPONSE                                             │ │
│  │                                                                                  │ │
│  │  {                                                                               │ │
│  │    "modules": [...],        // Query results                                     │ │
│  │    "bundle": {...},         // Graph bundle (optional)                           │ │
│  │    "result": {...},         // Shaped output (if shape applied)                  │ │
│  │    "metadata": {                                                                 │ │
│  │      "duration": "12ms",                                                         │ │
│  │      "cached": false                                                             │ │
│  │    }                                                                             │ │
│  │  }                                                                               │ │
│  └────────────────────────────────────────────────────────────────────────────────┘ │
│                                                                                      │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

---

## Module Dependencies

### Package Dependency Graph

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                           PACKAGE DEPENDENCY GRAPH                                   │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                      │
│                              ┌──────────────┐                                       │
│                              │    main      │                                       │
│                              │ (main.go +   │                                       │
│                              │  app/)       │                                       │
│                              └──────┬───────┘                                       │
│                                     │                                                │
│                     ┌───────────────┼───────────────┐                               │
│                     │               │               │                               │
│                     ▼               ▼               ▼                               │
│              ┌────────────┐  ┌────────────┐  ┌────────────┐                         │
│              │ component/ │  │ component/ │  │ component/ │                         │
│              │   server   │  │   grpc     │  │ automations│                         │
│              └──────┬─────┘  └──────┬─────┘  └──────┬─────┘                         │
│                     │               │               │                               │
│                     └───────────────┼───────────────┘                               │
│                                     │                                                │
│                                     ▼                                                │
│                           ┌──────────────────┐                                      │
│                           │ component/memql  │  (MemQLEngine)                       │
│                           └────────┬─────────┘                                      │
│                                    │                                                 │
│              ┌─────────────────────┼─────────────────────┐                          │
│              │                     │                     │                          │
│              ▼                     ▼                     ▼                          │
│       ┌────────────┐       ┌────────────┐        ┌────────────┐                     │
│       │ component/ │       │ component/ │        │ component/ │                     │
│       │ language/  │       │ language/  │        │ database/  │                     │
│       │ parser     │       │ compiler   │        │ memory-    │                     │
│       └────────────┘       └──────┬─────┘        │ nodes      │                     │
│              ▲                    │              └────────────┘                     │
│              │                    │                                                  │
│              └────────────────────┘  (AST types shared)                              │
│                                                                                      │
│  ─────────────────────────────────────────────────────────────────────────────────  │
│                                                                                      │
│                              EXTERNAL DEPENDENCIES                                   │
│                                                                                      │
│       ┌────────────┐  ┌────────────┐  ┌────────────┐  ┌────────────┐               │
│       │  uptrace/  │  │  grpc-go   │  │   bun      │  │ timescale  │               │
│       │   bun      │  │            │  │            │  │            │               │
│       └────────────┘  └────────────┘  └────────────┘  └────────────┘               │
│                                                                                      │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

### Import Relationships

| Package | Imports | Imported By |
|---------|---------|-------------|
| `component/language/parser` | Standard library only | `component/language/compiler`, `component/memql` |
| `component/language/compiler` | `component/language/parser` | `component/memql`, `component/automations` |
| `component/memql` | `component/language/parser`, `component/database`, `component/events` | `component/server`, `component/grpc` |

---

## Extension Points

### Adding New Operators

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                           ADDING A NEW OPERATOR                                      │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                      │
│  1. LEXER (component/language/parser/lexer.go)                                      │
│     ┌─────────────────────────────────────────────────────────────────────────┐     │
│     │  func (l *Lexer) scanOperator(...) {                                    │     │
│     │      // Add new operator pattern                                        │     │
│     │      operators := []string{                                             │     │
│     │          "=contains=",  // NEW                                          │     │
│     │          ...                                                            │     │
│     │      }                                                                  │     │
│     │  }                                                                      │     │
│     └─────────────────────────────────────────────────────────────────────────┘     │
│                                                                                      │
│  2. AST (component/language/parser/ast.go)                                          │
│     ┌─────────────────────────────────────────────────────────────────────────┐     │
│     │  const (                                                                │     │
│     │      OpContains ComparisonOperator = "=contains="  // NEW               │     │
│     │  )                                                                      │     │
│     └─────────────────────────────────────────────────────────────────────────┘     │
│                                                                                      │
│  3. EXECUTOR (component/memql/executor.go)                                          │
│     ┌─────────────────────────────────────────────────────────────────────────┐     │
│     │  func buildFilterCondition(op ComparisonOperator, ...) {                │     │
│     │      case OpContains:                                                   │     │
│     │          return fmt.Sprintf("%s @> %s", field, value)  // JSONB op      │     │
│     │  }                                                                      │     │
│     └─────────────────────────────────────────────────────────────────────────┘     │
│                                                                                      │
│  4. DOCUMENTATION (docs/public/language/memql.md)                                   │
│     ┌─────────────────────────────────────────────────────────────────────────┐     │
│     │  | `=contains=` | Array/object contains value | `tags=contains="urgent"`│     │
│     └─────────────────────────────────────────────────────────────────────────┘     │
│                                                                                      │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

### Adding a New Capability to Automations

A new thing an automation can do is almost never a new statement. It is a
**builtin**: a Go integration registered with `memql.RegisterPlugin`, declared
in `dsl/<namespace>/builtins.memql` with `@executor("integration.<name>.<verb>")`,
and called from a body as `x := builtin <name>(<named args>)`. Work that
reaches the outside world through a script is an **action** over a declared
**capability**, called as `action <name>(...)`. Both are journaled, previewed
and retried like every other call.

The statement set itself is closed, and a new statement kind is a grammar
change touching every layer, each pinned by a test that fails until it is
done:

1. **AST** -- a statement type in `component/language/ast/body.go`.
2. **Parser** -- its production in `component/language/parser/v1_body.go`,
   its refusals in `v1_body_refusals.go`, and its entry in
   `parser.BodyStatementForms()`.
3. **Compiler** -- its scope rules in `compiler/body_scope.go` and its step in
   `compiler/body_compile.go` (`TestCompileBodyCoversEveryStatementKind`).
4. **Runtime** -- the step's executor in `component/automations/steps/`.
5. **Corpus** -- a cell per construct under
   `test/conformance/2026/statements/<construct>/<form>/`, which the
   completeness gate requires, and a `GrammarVersion` bump.

---

## Appendix

### Comparison: Old vs New Architecture

| Aspect | Before | After |
|--------|--------|-------|
| **Parser location** | Embedded in `component/memql/parser.go` | Standalone `component/language/parser/` package |
| **AST ownership** | Defined alongside engine | Dedicated `ast.go` with clean hierarchy |
| **Automation authoring** | JSON only | MemQL syntax + transpilation to JSON |
| **Testability** | Required full engine setup | Parser testable in isolation |
| **Reusability** | Tightly coupled | Parser usable by CLI tools, formatters |
| **Code generation** | N/A | `component/language/compiler/` package |

### Performance Characteristics

| Operation | Complexity | Notes |
|-----------|------------|-------|
| Tokenization | O(n) | Single pass, character-by-character |
| Parsing | O(n) | Recursive descent, single pass |
| AST to JSON | O(n) | Linear tree traversal |
| Query execution | O(n × m) | n=nodes, m=relationship depth |

### Error Handling Strategy

```
Parse Errors:
├── Lexer Errors (position-aware)
│   ├── Unterminated string
│   ├── Invalid escape sequence
│   └── Unexpected character
│
├── Parser Errors (position + token context)
│   ├── Unexpected token
│   ├── Missing expected token
│   └── Invalid syntax construct
│
└── Compile Errors
    ├── Unknown function reference
    ├── Type mismatch
    └── Invalid step configuration
```

---

## Distributed Node Architecture

MemQL supports running as a distributed cluster where each node type specializes in
a subset of functionality. See [component/node/CLAUDE.md](../../../component/node/CLAUDE.md)
for full details.

**Node types:** identity, bff (default), agent, planner, workbench, mcp, edge

Each node type compiles to a separate binary via Go build tags. See [build-tags.md](../build/build-tags.md).

**Key components:**
- `NodeService` gRPC bidirectional stream for inter-node communication
- `PeerManager` for mesh discovery
- `EventBridge` for distributed event propagation: both directions on every stream, dedup, and a sixteen-link hop budget ([mesh event delivery](../operate/mesh-event-delivery.md))
- Bootstrap strategy pattern selects components per node type
- `CapabilityRouter` routes function calls to nodes that own them

**Integration capabilities** (14 total across 8 integrations) are callable from the
MemQL DSL via `@executor("integration.X.Y")` decorators. This unifies the architecture
so domain operations flow through integrations, while protocol concerns stay in Go.

---

## Version History

| Version | Date | Changes |
|---------|------|---------|
| 1.0 | 2025-12 | Initial architecture with separate parser/compiler engines |
| 2.0 | 2026-03 | Distributed node architecture, integration capabilities pattern |

---

*Document generated for MemQL v2.x architecture. For implementation details, see source code in `component/memql/`, `component/node/`, and `integrations/`.*
