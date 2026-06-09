---
title: MemQL Function Language Specification
audience: public
status: stable
area: language
sinceVersion: 0.9.0
owner: znas
---

# MemQL Function Language Specification

> **Status:** Draft  
> **Last Updated:** December 7, 2025  
> **Purpose:** Specification for query, mutation, and automation functions in MemQL

---

## Overview

MemQL functions provide reusable, parameterized operations. This specification defines the following function types:

| Type | Purpose | Status |
|------|---------|--------|
| **Query** | Read data with optional filters | Implemented |
| **Mutation** | Insert data with parameters | Implemented |
| **Automation** | Multi-step workflows | Implemented |
| **Prompt** | SI prompt templates with input schemas | Implemented |
| **Provider** | SI provider configurations | Implemented |
| **Shape** | Reusable shape templates for data projection | Implemented |

## Naming Convention

Function names should use receiver prefixes:

- Query: `query*`
- Mutation: `mutation*`
- Spec: `spec*`
- Prompt: descriptive name (e.g., `agentReply`, `cognitionRouting`, `docSummary`)
- Provider: provider name (e.g., `chat54Mini`, `claudeSonnet`)
- Shape: descriptive name (e.g., `spaceCard`, `agentCard`)

The compiler emits naming diagnostics for mismatches.

## Builtin Functions (Registry-Driven)

Builtins are declared as JSON metadata files under `functions/v1/builtin/*.json` and loaded into the same function registry as user-defined functions. Each builtin JSON definition provides:

- `name` and optional `aliases` (call names)
- `executor` (Go handler key)
- `args` contract (parser-level argument profile/validation)

At runtime, parser resolution and executor dispatch are both registry-driven from this metadata rather than hardcoded builtin name branching.

---

## Syntax Principles

### Consistent Accessor Pattern

All data access uses the `functionName("parameter")` pattern:

```memql
args.fieldName           -- Function argument
var("VARIABLE_NAME")       -- Config variable  
node("path")               -- Node property (in shapes)
step("stepId")             -- Step result (in automations)
field(object, "key")       -- Field access on object
```

This avoids JavaScript-like `$var.NAME` or `${name}` interpolation.

### Operators

| Operator | Meaning | Example |
|----------|---------|---------|
| `?.` | Optional filter (skip if arg missing) | `?.payload.status==args.status` |
| `? :` | Ternary (choose between values) | `args.flag ? "yes" : "no"` |
| `==` | Equal | `payload.role=="admin"` |
| `!=` | Not equal | `payload.status!="archived"` |
| `>` | Greater than | `payload.count>10` |
| `>=` | Greater than or equal | `payload.count>=10` |
| `<` | Less than | `payload.count<10` |
| `<=` | Less than or equal | `payload.count<=10` |
| `in` | Value in list | `payload.status in ("a","b")` |
| `not in` | Value not in list | `payload.status not in ("x","y")` |
| `;` | AND (logical) | `concept==x;payload.y==z` |
| `,` | OR (logical) | `payload.a=="x",payload.a=="y"` |

---

## Query Functions (Implemented)

Query functions are read-only operations that return data.

### File Structure

```
dsl/v1/queries/v1/identity/activeUsers.memql   # struct-form query or procedural func + args { ... }
```

### Syntax

```memql
-- Description (becomes function description)
concept==v1:identity:user;
payload.active==true;
?.payload.authorizerId==args.authorizerId;
?.payload.role==args.role
```

### Optional Filters with `?.`

The `?.` prefix makes a filter conditional - it's only applied if the argument is provided:

```memql
-- Both filters are optional
concept==v1:cognition:participant;
?.payload.spaceId==args.spaceId;
?.payload.status==args.status
```

**Calling patterns:**
```memql
spaceParticipants()                              -- No filters
spaceParticipants({"spaceId": "s-1"})            -- Filter by spaceId only
spaceParticipants({"status": "active"})          -- Filter by status only
spaceParticipants({"spaceId": "s-1", "status": "active"})  -- Both filters
```

---

## Mutation Functions (Implemented)

Mutation functions execute insert operations with parameters.

### Execution Constraints

- Exactly **one** `insert(...)` per mutation function call.
- Mutation functions can only be invoked as a **top-level** expression: `myMutation({ ... })`.
- Mutation functions cannot be wrapped with directives like `shape()`, `paginate()`, `sort()`, `select()`, `asOf()`, or `withDepth()`.

### Syntax (struct form, canonical)

Each query / mutation binds its concept in the SIGNATURE:
`mutation <Concept> <name>` / `query <Concept> <name>`. Cross-file
constructs (other queries, mutations, shapes, traits, builtins,
logic blocks) are pulled into local scope via file-top `use
<module>.{ ... }` imports. The legacy per-construct `@useConcept`
annotation and the legacy single-binding `use <ns>.<concept>`
shape are both retired and rejected at parse time.

```memql
use <ns>.concepts.{ <conceptName> }

@enabled
@description("Creates a new record")
mutation <conceptName> mutationFunctionName {
  args { ... }
  insert {
    id: args.id
    ...
  }
}
```

### Example: createUser

```memql
use identity.concepts.{ user }

@enabled
@description("Creates a new user record")
mutation user mutationCreateUser {
  args {
    userId        string  @required
    authorizerId  string  @required
    email         string  @required
    displayName   string
  }
  insert {
    id:           args.userId
    authorizerId: args.authorizerId
    email:        args.email
    displayName:  coalesce(args.displayName, args.email)
    role:         var("MEMQL_DEFAULT_USER_ROLE")
    preferences: {
      theme:         var("MEMQL_DEFAULT_USER_THEME"),
      notifications: true
    }
    lastSeenAt:   timestamp()
    active:       true
  }
}
```

### Using Variables with `var()`

Variables are fetched from the `v1:platform:partitionVariable` concept:

```memql
"role": var("MEMQL_DEFAULT_USER_ROLE")
"theme": var("MEMQL_DEFAULT_USER_THEME")
"webhookUrl": var("DISCORD_WEBHOOK_URL")
```

### Ternary Operator

Use `condition ? valueIfTrue : valueIfFalse` for inline conditionals:

```memql
"displayName": args.displayName != "" ? args.displayName : args.email
"role": args.isAdmin ? "admin" : "member"
```

---

## Automation Functions (Proposed)

Automation functions define multi-step workflows with control flow.

### Syntax

```memql
automation functionName(arg1, arg2, ...) {
  stepId: stepType { ... }
  stepId: stepType when condition { ... }
  ...
  return expression
}
```

### Step Types

| Step Type | Description | Syntax |
|-----------|-------------|--------|
| `query` | Execute MemQL query | `step: query { expression }` |
| `mutation` | Execute insert | `step: mutation { insert(...) }` |
| `shape` | Transform data | `step: shape { source: ..., template: {...} }` |
| `webhook` | HTTP request | `step: webhook { url: ..., method: ..., body: {...} }` |
| `event` | Publish event | `step: event { topic: ..., payload: {...} }` |
| `forEach` | Iterate collection | `step: forEach source as item { ... }` |
| `parallel` | Concurrent execution | `step: parallel { branch1, branch2 }` |
| `switch` | Conditional branching | `step: switch expr { case "x": ... }` |

### Example: bootstrapUser

```memql
automation bootstrapUser(authorizerId, email, displayName) {
  -- Check if user already exists
  checkUser: query {
    activeUsers({"authorizerId": args.authorizerId})
  }
  
  -- Create user if not found (use .metadata.itemCount to check step result count)
  createUser: mutation when step("checkUser").metadata.itemCount == 0 {
    insert("v1:identity:user",
      id=concat("user-", args.authorizerId),
      payload={
        "authorizerId": args.authorizerId,
        "email": args.email,
        "displayName": args.displayName != "" ? args.displayName : args.email,
        "role": var("MEMQL_DEFAULT_USER_ROLE"),
        "preferences": {
          "theme": var("MEMQL_DEFAULT_USER_THEME"),
          "notifications": true
        },
        "lastSeenAt": timestamp(),
        "active": true
      }
    )
  }
  
  -- Return the user (created or existing)
  return coalesce(step("createUser"), first(step("checkUser")))
}
```

### Conditional Steps with `when`

Steps can have conditions that must be true for execution:

```memql
createUser: mutation when step("checkUser").metadata.itemCount == 0 {
  insert(...)
}

notify: webhook when step("createUser") != null {
  url: var("DISCORD_WEBHOOK_URL"),
  body: { "content": concat("New user: ", args.displayName) }
}
```

### Referencing Step Results with `step()`

```memql
step("checkUser")                        -- Full result
step("checkUser").metadata.itemCount     -- Count of results
step("checkUser").result                 -- Result data
first(step("checkUser"))                 -- First result
field(step("checkUser"), "id")           -- Field from result
```

### forEach Loops

```memql
persist: forEach step("classify") as lead {
  insert("v1:crm:lead",
    id=field(item(), "id"),
    payload={
      "name": field(item(), "name"),
      "classification": field(item(), "classification")
    }
  )
}

-- With filter
routeHot: forEach step("classify") where field(item(), "classification") == "hot" as lead {
  webhook {
    url: var("SALES_NOTIFY_URL"),
    body: { "leadId": field(item(), "id") }
  }
} onError: continue
```

### Scheduled Automations

```memql
@enabled
@schedule(cron="0 0 * * *")
@description("Daily cleanup of temporary data")
automation dailyCleanup() {
  
  input: concept==v1:temp:data;payload.createdAt<timestamp()
  
  cleanup: forEach input() as item {
    insert("v1:temp:data", id=field(item(), "id"), payload={ "deleted": true })
  }
}
```

### Event-Triggered Automations

```memql
@enabled
@trigger(event="graph.node.created.v1:crm:lead", filter="payload.source == 'website'")
automation onLeadCreated() {
  
  classify: shape {
    source: event(),
    template: {
      "id": node("payload.nodeId"),
      "classification": si("classifyLead.v1", { "data": node("payload") })
    }
  }
}
```

### Attribute Reference

Automations are **disabled by default**. Use attributes to configure:

| Attribute | Arguments | Description |
|-----------|-----------|-------------|
| `@enabled` | none | Activates the automation |
| `@disabled` | none | Explicitly disables (default) |
| `@trigger` | `event="..."`, `filter="..."` | Event-based trigger |
| `@schedule` | `cron="..."` | Cron schedule |
| `@description` | `"..."` | Human-readable description |
| `@async` | none | Run asynchronously |
| `@timeout` | `"30s"` | Execution timeout |
| `@retry` | `count=3` | Retry on failure |

### Error Handling

```memql
-- Per-step error handling
riskyStep: webhook { ... } onError: continue
retryableStep: webhook { ... } onError: retry retries: 3

-- Automation-level hooks
onComplete: webhook {
  url: var("DISCORD_WEBHOOK_URL"),
  body: { "content": "Completed successfully" }
}

onError: webhook {
  url: var("DISCORD_WEBHOOK_URL"),
  body: { "content": concat("Failed: ", error()) }
}
```

---

## Helper Functions Reference

### Data Access

| Function | Description | Example |
|----------|-------------|---------|
| `args.name` | Function argument | `args.authorizerId` |
| `var("NAME")` | Config variable | `var("MEMQL_DEFAULT_USER_ROLE")` |
| `node("path")` | Node property (shapes) | `node("payload.name")` |
| `step("id")` | Step result | `step("checkUser")` |
| `input()` | Automation input | `input()` |
| `event()` | Trigger event | `event()` |
| `item()` | Current forEach item | `item()` |
| `index()` | Current forEach index | `index()` |
| `field(obj, "key")` | Field access | `field(item(), "name")` |

### Collection & Aggregation

| Function | Description | Example |
|----------|-------------|---------|
| `step("id").metadata.itemCount` | Count step results | `step("users").metadata.itemCount == 0` |
| `first(collection)` | First item | `first(step("users"))` |
| `last(collection)` | Last item | `last(step("users"))` |
| `sum(collection, "key")` | Sum values | `sum(step("orders"), "total")` |
| `avg(collection, "key")` | Average | `avg(step("scores"), "value")` |

### Logic

| Function | Description | Example |
|----------|-------------|---------|
| `coalesce(a, b, ...)` | First non-null | `coalesce(args.name, "default")` |
| `cond(pred, then, else)` | Conditional value | `cond(args.flag, "yes", "no")` |

### Strings

| Function | Description | Example |
|----------|-------------|---------|
| `concat(a, b, ...)` | Concatenate | `concat("user-", args.id)` |
| `lower(str)` | Lowercase | `lower(args.email)` |
| `upper(str)` | Uppercase | `upper(args.code)` |
| `trim(str)` | Remove whitespace | `trim(args.input)` |
| `contains(str, sub)` | Contains check | `contains(args.email, "@company.com")` |
| `hash(str)` | SHA256 hash | `hash(args.email)` |

### Time

| Function | Description | Example |
|----------|-------------|---------|
| `timestamp()` | Current ISO timestamp | `timestamp()` |
| `now()` | Alias for timestamp | `now()` |

### Error

| Function | Description | Example |
|----------|-------------|---------|
| `error()` | Current error message | `error()` |

---

## Implementation Roadmap

### Phase 1: Query Functions (Complete)
- [x] Directory-based function loading
- [x] `arg()` references
- [x] `?.` conditional filters
- [x] JSON Schema validation
- [x] Function composition

### Phase 2: Enhanced Syntax
- [ ] Parser support for `?.` (currently `?`)
- [ ] `var()` function for variable lookup
- [ ] Ternary operator `? :`
- [ ] `concat()` and string helpers

### Phase 3: Mutation Functions
- [x] `func (Mutation)` receiver syntax in parser
- [x] `insert()` within mutation functions
- [x] Argument substitution in mutation templates (`args.*`, `var()`, `timestamp()`, `concat()`, `hash()`, `coalesce()`, `if()`)

### Phase 4: Automation Functions
- [ ] `automation` keyword in parser
- [ ] Step parsing and execution
- [ ] `when` conditions
- [ ] `step()` references
- [ ] `forEach` and iteration
- [ ] `webhook`, `event` step types
- [ ] Error handling (`onError`, `onComplete`)
- [ ] Scheduled triggers
- [ ] Event triggers

---

## Migration from JSON Automations

Current automations are defined in JSON. The new syntax is equivalent:

**JSON (current):**
```json
{
  "name": "leadClassification",
  "schedule": "*/30 * * * * *",
  "steps": [
    {
      "id": "classify",
      "type": "shape",
      "shape": {
        "source": "$input",
        "template": { "name": "$node.payload.name" }
      }
    }
  ]
}
```

**MemQL (proposed):**
```memql
automation leadClassification {
  schedule: "*/30 * * * * *"
  
  classify: shape {
    source: input(),
    template: { "name": node("payload.name") }
  }
}
```

Both formats will be supported during transition.

---

## Prompt Functions (Implemented)

Prompt functions define SI prompt templates with typed input schemas and default provider configuration.

### File Structure

```
prompts/v1/<domain>/<promptName>.memql
```

### Syntax

```memql
@description("Generate an SI assistant reply")
@defaultProvider("chat54Mini")
@templateFile("agentReply.tmpl")
func (Prompt) agentReply(args any) {
  @input {
    trigger              string  @required
    assistant            object  @required
    space                object  @required
    participants         array(object)
    history              array(object)  @required
    conversationContext  object
  }
}
```

### Attributes

| Attribute | Description |
|-----------|-------------|
| `@description` | Human-readable description of the prompt |
| `@defaultProvider` | Default SI provider name to use |
| `@templateFile` | Go text/template file for the prompt |
| `@input` | Input schema block declaring expected arguments |

### Input Field Types

| Type | Description |
|------|-------------|
| `string` | String value |
| `object` | JSON object |
| `array(object)` | Array of JSON objects |
| `@required` | Field modifier marking the field as required |

---

## Provider Functions (Implemented)

Provider functions define SI provider configurations using MemQL syntax instead of JSON files.

### File Structure

```
providers/v1/<vendor>/<providerName>.memql
```

### Syntax

```memql
@extends("openai")
@model("gpt-5.4-mini")
func (Provider) chat54Mini {
  params {
    maxCompletionTokens  16384
  }
}
```

### Attributes

| Attribute | Description |
|-----------|-------------|
| `@type` | Provider type (`OpenAI`, `OpenAIStream`, `OpenAITTS`, `Anthropic`, `AnthropicStream`). Optional when `@extends` is used. |
| `@model` | Model identifier |
| `@default` | Marks this provider as the default fallback |
| `@extends` | Inherits `auth` and `@type` from a named base provider (e.g., `@extends("openai")`) |
| `@base` | Marks a file as a base provider definition (uses `provider` keyword instead of `func`) |

### Blocks

| Block | Description |
|-------|-------------|
| `auth` | Authentication credentials using `env()` for environment variable references. Inherited from base when using `@extends`. |
| `params` | Provider-specific parameters (temperature, maxCompletionTokens, etc.) |

---

## Shape Functions (Implemented)

Shape functions define reusable data projection templates. Each concept has one comprehensive shape
(e.g., `participantFull`, `agentFull`) that includes all fields. Queries reference shapes by name
instead of defining inline templates.

### File Structure

```
shapes/v1/
├── common/         agentFull, configFull, invitationFull, attachmentFull, mediaFull
├── cognition/      participantFull, utteranceFull, spaceFull, spaceContextFull, sessionFull
├── identity/      userFull, identityFull, partitionAccessFull
├── data/           recordFull, policyFull, logFull
└── memql/          variableFull
```

### Definition Syntax

```memql
@description("Comprehensive participant projection with all fields")
@concepts("v1:cognition:participant")
func (Shape) participantFull {
  @template({
    id: node("id"),
    spaceId: node("payload.spaceId"),
    displayName: node("payload.displayName"),
    participantType: node("payload.participantType"),
    status: node("payload.status"),
    createdAt: node("createdAt")
  })
}
```

### Usage in Queries

Queries reference shapes by name as the second argument to `shape()`:

```memql
func (Query) spaceParticipants(args any) (any, error) {
  return shape(
    concept==v1:cognition:participant;
    ?.payload.spaceId==args.spaceId,
    "participantFull"
  ), nil
}
```

### Attributes

| Attribute | Description |
|-----------|-------------|
| `@description` | Human-readable description |
| `@concepts` | Concept(s) this shape applies to |
| `@template` | Shape template using `node()` accessors |

