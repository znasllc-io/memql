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
│   │  │ SI Runtime │  │   Event    │  │   Result   │  │   Schema   │              │  │
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
│   │  │    ┌──────────────┐  ┌──────────────┐  ┌──────────────┐                │  │  │
│   │  │    │ memory_nodes │  │  partitions  │  │   concepts   │                │  │  │
│   │  │    │ (hypertable) │  │  (metadata)  │  │              │                │  │  │
│   │  │    │ PK: partition│  │              │  │              │                │  │  │
│   │  │    │  + id + time │  │              │  │              │                │  │  │
│   │  │    └──────────────┘  └──────────────┘  └──────────────┘                │  │  │
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
- **Proto messages** -- 27 message types in the `InternalMessage` envelope (database, engine, integration, event, config, telemetry, lifecycle)
- **Correlation IDs** -- Every message carries a `correlation_id` for distributed tracing across channel hops

All components implement `Ready() <-chan struct{}` for parallel startup coordination,
and accept `SetWiring(*bus.Wiring)` to receive channel-based communication.

---

## Engine Components

### Component Registry

```
engine/
├── parser/                 # Lexer, Parser, AST definitions
│   ├── ast.go             # Abstract Syntax Tree node types
│   ├── lexer.go           # Tokenization
│   ├── parser.go          # Recursive descent parser
│   └── errors.go          # Error types with position info
│
├── compiler/              # AST to target format transformation
│   ├── compiler.go        # Main compiler interface
│   ├── api.go             # Public API functions
│   ├── automation_generator.go  # AST → JSON automation
│   └── function_generator.go    # AST → function definition
│
└── memql/                 # Query execution (existing)
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
| **SI Runtime** | Prompts | SI responses | LLM invocations |
| **Event Bus** | Events | Subscribers | Inter-component communication |

---

## Parser Engine

The Parser Engine (`engine/parser/`) transforms MemQL source text into an Abstract Syntax Tree (AST).

### Lexer Architecture

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                                   LEXER PIPELINE                                    │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                     │
│   Input: "concept==v1:crm:lead;payload.active==true"                                │
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
│   │ [IDENT:concept] [OP:==] [IDENT:v1:crm:lead] [SEMI] [IDENT:payload.active]   │   │
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
│  │   ├── TokenSemicolon       // ;  (AND operator)                             │
│  │   ├── TokenComma           // ,  (OR operator)                              │
│  │   └── TokenAt              // @  (timestamp suffix)                         │
│  │                                                                             │
│  ├── Values                                                                    │
│  │   ├── TokenIdentifier      // names, paths, concept refs                    │
│  │   ├── TokenNumber          // integers, floats                              │
│  │   └── TokenString          // "quoted strings"                              │
│  │                                                                             │
│  ├── Operators                                                                 │
│  │   ├── TokenOperator        // ==, !=, >, >=, <, <=, in, has, etc.           │
│  │   ├── TokenDefine          // :=                                            │
│  │   ├── TokenQuestion        // ?  (ternary)                                  │
│  │   └── TokenQuestionDot     // ?. (conditional filter)                       │
│  │                                                                             │
│  └── Keywords                                                                  │
│      ├── TokenKeywordQuery        // query                                     │
│      ├── TokenKeywordMutation     // mutation                                  │
│      ├── TokenKeywordAutomation   // automation                                │
│      ├── TokenKeywordWhen         // when                                      │
│      ├── TokenKeywordReturn       // return                                    │
│      ├── TokenKeywordSchedule     // schedule                                  │
│      ├── TokenKeywordEnabled      // enabled                                   │
│      ├── TokenKeywordOnComplete   // onComplete                                │
│      └── TokenKeywordOnError      // onError                                   │
│                                                                                │
└────────────────────────────────────────────────────────────────────────────────┘
```

### Parser Architecture

The parser uses recursive descent with the following grammar productions:

```
┌──────────────────────────────────────────────────────────────────────────────────────┐
│                              PARSER GRAMMAR (EBNF)                                   │
├──────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                      │
│  file            = { definition } ;                                                  │
│                                                                                      │
│  definition      = queryFunc | mutationFunc | automation ;                           │
│                                                                                      │
│  queryFunc       = "query" [identifier] argList "{" expression "}" ;                 │
│                                                                                      │
│  mutationFunc    = "mutation" [identifier] argList ["when" condition] "{" insert "}";│
│                                                                                      │
│  automation      = "automation" identifier argList "{" { automationStmt } "}" ;      │
│                                                                                      │
│  automationStmt  = schedule | enabled | step | returnStmt ;                          │
│                                                                                      │
│  step            = identifier ":" stepType ["when" condition] "{" stepBody "}" ;     │
│                                                                                      │
│  stepType        = "query" | "mutation" | "shape" | "webhook" | "event"              │
│                  | "forEach" | "parallel" | "switch" ;                               │
│                                                                                      │
│  expression      = logicalOr ;                                                       │
│  logicalOr       = logicalAnd { "," logicalAnd } ;                                   │
│  logicalAnd      = primary { ";" primary } ;                                         │
│  primary         = grouped | conditionalFilter | comparison | functionCall ;         │
│                                                                                      │
│  comparison      = fieldRef operator value ;                                         │
│  conditionalFilter = "?." comparison ;                                               │
│                                                                                      │
│  functionCall    = identifier "(" [ argList ] ")" ;                                  │
│  argList         = "(" [ arg { "," arg } ] ")" ;                                     │
│  arg             = identifier [ "=" value ] ;                                        │
│                                                                                      │
└──────────────────────────────────────────────────────────────────────────────────────┘
```

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
│  │   ├── PaginateExpr         // paginate(limit, offset)(...)                        │
│  │   ├── SelectExpr           // select(fields)(...)                                 │
│  │   ├── DepthExpr            // depth(n)(...)                                       │
│  │   ├── ShapeExpr            // shape(template)(...)                                │
│  │   ├── ConditionalFilterExpr// ?.field==value                                      │
│  │   ├── ArgRefExpr           // args.name                                         │
│  │   ├── LiteralExpr          // "string", 123, true                                 │
│  │   └── AIExpr               // ai("template", data)                                │
│  │                                                                                   │
│  ├── StatementNode (interface)                                                       │
│  │   ├── MutationStmt         // insert("concept", ...)                              │
│  │   └── QueryStmt            // expression as statement                             │
│  │                                                                                   │
│  ├── FunctionDef              // query/mutation/automation definition                │
│  │   ├── Name                 // function name                                       │
│  │   ├── Type                 // query | mutation | automation                       │
│  │   ├── Args                 // []FunctionArg                                       │
│  │   └── Body                 // Node (expression, mutation, or automation)          │
│  │                                                                                   │
│  ├── AutomationDef            // automation body                                     │
│  │   ├── Schedule             // cron expression                                     │
│  │   ├── Trigger              // event trigger                                       │
│  │   ├── Steps                // []StepDef                                           │
│  │   ├── OnComplete           // completion hook                                     │
│  │   └── OnError              // error hook                                          │
│  │                                                                                   │
│  ├── StepDef                  // automation step                                     │
│  │   ├── ID                   // step identifier                                     │
│  │   ├── Type                 // query | mutation | webhook | forEach | ...          │
│  │   ├── Condition            // "when" condition                                    │
│  │   └── Config               // step-specific configuration                         │
│  │                                                                                   │
│  └── File                     // parsed .memql file                                  │
│      └── Definitions          // []Node                                              │
│                                                                                      │
└──────────────────────────────────────────────────────────────────────────────────────┘
```

---

## Compiler Engine

The Compiler Engine (`engine/compiler/`) transforms AST nodes into target output formats, primarily JSON for automations.

### Compilation Pipeline

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                              COMPILATION PIPELINE                                    │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                      │
│   SOURCE (.memql)                                                                    │
│   ┌─────────────────────────────────────────────────────────────────────────────┐   │
│   │  automation leadProcessor() {                                                │   │
│   │      schedule "*/30 * * * *"                                                 │   │
│   │      fetchLeads: query {                                                     │   │
│   │          concept==v1:crm:lead;payload.active==true                           │   │
│   │      }                                                                       │   │
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
│   │          Schedule: "*/30 * * * *"                                            │   │
│   │          Steps: [                                                            │   │
│   │            StepDef { ID: "fetchLeads", Type: StepTypeQuery, ... }            │   │
│   │          ]                                                                   │   │
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
│   │    "schedule": "*/30 * * * *",                                               │   │
│   │    "steps": [                                                                │   │
│   │      {                                                                       │   │
│   │        "id": "fetchLeads",                                                   │   │
│   │        "type": "query",                                                      │   │
│   │        "query": {                                                            │   │
│   │          "query": "concept==v1:crm:lead;payload.active==true"                │   │
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

The Executor Engine (in `engine/memql/`) executes parsed queries against the database.

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
│  │ WebSocket│                                                                       │
│  │ gRPC     │                                                                       │
│  │ REST     │                                                                       │
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
│  │  │               Optional: SI Shape Functions                                │  │ │
│  │  │               ┌────────────────┐                                         │  │ │
│  │  │               │   SI Runtime   │                                         │  │ │
│  │  │               │ (LLM invocation│                                         │  │ │
│  │  │               │  for si() fn)  │                                         │  │ │
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
│                              │   (cmd/)     │                                       │
│                              └──────┬───────┘                                       │
│                                     │                                                │
│                     ┌───────────────┼───────────────┐                               │
│                     │               │               │                               │
│                     ▼               ▼               ▼                               │
│              ┌────────────┐  ┌────────────┐  ┌────────────┐                         │
│              │   server   │  │   grpc     │  │ automations│                         │
│              └──────┬─────┘  └──────┬─────┘  └──────┬─────┘                         │
│                     │               │               │                               │
│                     └───────────────┼───────────────┘                               │
│                                     │                                                │
│                                     ▼                                                │
│                           ┌──────────────────┐                                      │
│                           │  engine/memql    │  (MemoryEngine)                      │
│                           └────────┬─────────┘                                      │
│                                    │                                                 │
│              ┌─────────────────────┼─────────────────────┐                          │
│              │                     │                     │                          │
│              ▼                     ▼                     ▼                          │
│       ┌────────────┐       ┌────────────┐        ┌────────────┐                     │
│       │  engine/   │       │  engine/   │        │  database/ │                     │
│       │  parser    │       │  compiler  │        │  memory-   │                     │
│       │            │       │            │        │  nodes     │                     │
│       └────────────┘       └──────┬─────┘        └────────────┘                     │
│              │                    │                                                  │
│              │                    │                                                  │
│              │      ┌─────────────┘                                                  │
│              │      │                                                                │
│              ▼      ▼                                                                │
│       ┌────────────────┐                                                            │
│       │    engine/     │                                                            │
│       │    parser      │  (AST types shared)                                        │
│       └────────────────┘                                                            │
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
| `engine/parser` | Standard library only | `engine/compiler`, `engine/memql` |
| `engine/compiler` | `engine/parser` | `cmd/`, `automations/` |
| `engine/memql` | `engine/parser`, `database/`, `events/` | `server/`, `grpc/` |

---

## Extension Points

### Adding New Operators

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                           ADDING A NEW OPERATOR                                      │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                      │
│  1. LEXER (engine/parser/lexer.go)                                                  │
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
│  2. AST (engine/parser/ast.go)                                                      │
│     ┌─────────────────────────────────────────────────────────────────────────┐     │
│     │  const (                                                                │     │
│     │      OpContains ComparisonOperator = "=contains="  // NEW               │     │
│     │  )                                                                      │     │
│     └─────────────────────────────────────────────────────────────────────────┘     │
│                                                                                      │
│  3. EXECUTOR (engine/memql/executor.go)                                             │
│     ┌─────────────────────────────────────────────────────────────────────────┐     │
│     │  func buildFilterCondition(op ComparisonOperator, ...) {                │     │
│     │      case OpContains:                                                   │     │
│     │          return fmt.Sprintf("%s @> %s", field, value)  // JSONB op      │     │
│     │  }                                                                      │     │
│     └─────────────────────────────────────────────────────────────────────────┘     │
│                                                                                      │
│  4. DOCUMENTATION (docs/memql.md)                                                   │
│     ┌─────────────────────────────────────────────────────────────────────────┐     │
│     │  | `=contains=` | Array/object contains value | `tags=contains="urgent"`│     │
│     └─────────────────────────────────────────────────────────────────────────┘     │
│                                                                                      │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

### Adding New Step Types (Automations)

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                         ADDING A NEW AUTOMATION STEP TYPE                            │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                      │
│  1. AST (engine/parser/ast.go)                                                      │
│     ┌─────────────────────────────────────────────────────────────────────────┐     │
│     │  const (                                                                │     │
│     │      StepTypeSlack StepType = "slack"  // NEW                           │     │
│     │  )                                                                      │     │
│     │                                                                         │     │
│     │  type SlackStepConfig struct {                                          │     │
│     │      Channel  string                                                    │     │
│     │      Message  string                                                    │     │
│     │      Blocks   []any                                                     │     │
│     │  }                                                                      │     │
│     └─────────────────────────────────────────────────────────────────────────┘     │
│                                                                                      │
│  2. PARSER (engine/parser/parser.go)                                                │
│     ┌─────────────────────────────────────────────────────────────────────────┐     │
│     │  func (p *Parser) parseStep() {                                         │     │
│     │      case "slack":                                                      │     │
│     │          stepType = StepTypeSlack                                       │     │
│     │  }                                                                      │     │
│     └─────────────────────────────────────────────────────────────────────────┘     │
│                                                                                      │
│  3. COMPILER (engine/compiler/automation_generator.go)                              │
│     ┌─────────────────────────────────────────────────────────────────────────┐     │
│     │  func (c *Compiler) compileStep(step *StepDef) {                        │     │
│     │      case StepTypeSlack:                                                │     │
│     │          if cfg, ok := step.Config.(*SlackStepConfig); ok {             │     │
│     │              output["slack"] = map[string]any{                          │     │
│     │                  "channel": cfg.Channel,                                │     │
│     │                  "message": cfg.Message,                                │     │
│     │              }                                                          │     │
│     │          }                                                              │     │
│     │  }                                                                      │     │
│     └─────────────────────────────────────────────────────────────────────────┘     │
│                                                                                      │
│  4. SCHEDULER (automations/evaluator.go)                                            │
│     ┌─────────────────────────────────────────────────────────────────────────┐     │
│     │  func (e *Evaluator) executeStep(step Step) {                           │     │
│     │      case "slack":                                                      │     │
│     │          return e.executeSlackStep(step)                                │     │
│     │  }                                                                      │     │
│     └─────────────────────────────────────────────────────────────────────────┘     │
│                                                                                      │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

---

## Appendix

### Comparison: Old vs New Architecture

| Aspect | Before | After |
|--------|--------|-------|
| **Parser location** | Embedded in `engine/memql/parser.go` | Standalone `engine/parser/` package |
| **AST ownership** | Defined alongside engine | Dedicated `ast.go` with clean hierarchy |
| **Automation authoring** | JSON only | MemQL syntax + transpilation to JSON |
| **Testability** | Required full engine setup | Parser testable in isolation |
| **Reusability** | Tightly coupled | Parser usable by CLI tools, formatters |
| **Code generation** | N/A | `engine/compiler/` package |

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

memQL supports running as a distributed cluster where each node type specializes in
a subset of functionality. See [component/node/CLAUDE.md](../../../component/node/CLAUDE.md)
for full details.

**Node types:** bff (default), voice, cognition, agent, planner

Each node type compiles to a separate binary via Go build tags. See [build-tags.md](../build/build-tags.md).

**Key components:**
- `NodeService` gRPC bidirectional stream for inter-node communication
- `PeerManager` for mesh discovery
- `EventBridge` for distributed event propagation with dedup and TTL
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
