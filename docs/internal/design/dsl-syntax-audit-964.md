---
title: memQL DSL syntax audit + standardization reference (#964)
audience: internal
status: historical
area: internal
sinceVersion: 0.9.0
owner: znas
---

# memQL DSL syntax audit + standardization reference (#964)

Status: REVIEW ARTIFACT. Findings + a complete option reference for every
construct, to drive an owner markup pass ("keep / remove / change"). Precursor
to a standardization/cleanup epic and to the planner-authored-automations epic
#954 (an LLM author needs one consistent, documented syntax).

Method: read-only audit against the real grammar (`component/language/parser/`,
the per-construct converters/parsers) and all 156 live `dsl/**/*.memql` files
(`dsl/_reference/*` skeletons excluded from usage counts). Every claim below is
backed by a file:line in the source tree at the time of writing.

> Convention note: examples in this doc use `->` for "becomes / target form"
> and avoid emojis, per repo docs style.

---

## Part 1 — Consistency findings

### 1a. The two owner-flagged items

**insert/update restates the concept — 100% of the tree.** The concept is bound
in the signature and then named again in the body:

```
mutation participant mutationAddAgentToSpace {   // concept bound here
  ...
  insert participant { ... }                     // and restated here (redundant)
}
```

- `insert <concept> { ... }`: 131 occurrences.
- `update <concept> { ... }`: 59 occurrences.
- Bare `insert { ... }` / `update { ... }` (the form CLAUDE.md documents as
  canonical): **0 occurrences.** The canonical form was documented but never
  implemented/adopted.
- Related concept-string restatement: `canonicalId(args.x, "v1:cognition:space")`
  (35x, cognition only); `concat("v1:knowledge:document:", …)` (library/logic).

**`use` imports are clean.** 121 `use <ns>.<construct>.{ names }` statements,
uniform syntax, and zero legacy `@use*` annotations in any live file — every
`@useConcept`/`@useShape`/etc. is a commented line in `dsl/_reference/`
skeletons. The only real `@use*` cruft is in Go: dead allow-list entries in
`component/memql/function_annotation_allow_lists.go` that are simultaneously
hard-rejected by `baseparser` (`iface.go:39`). Two minor file stragglers bind a
concept without the sibling `use` import: `calendar/shapes.memql`,
`guide/shapes.memql`.

### 1b. Bigger structural problems surfaced by the audit

**(i) Three competing AND/OR syntaxes; two silently fail.**
- Filters use `;` for AND (RSQL-style separator) — `parser.go:4012`.
- Specs use `&&` for AND, but **`||` for OR does not tokenize** (the lexer has no
  `|`). Affected specs silently fail to load and never register — e.g.
  `spec requiresOwnerOrAdmin` (`dsl/deployment/specs.memql:18`), plus
  `_reference/_spec.memql:70,185`. Hidden behind a debug-level skip log
  (`unified_spec_loader.go:29`). LIVE BUG.
- Logic `if`-conditions mix Go `&&` with the English words `and`/`or`
  (`dsl/worker/logic.memql:35`, `dsl/identity/logic.memql:90,152`,
  `dsl/workbench/logic.memql:19`). The runtime normalizer rewrites only
  `&&`/`||` (`component/automations/evaluator.go:1059`), not the words, so
  word-form conditions fall through to a malformed single comparison and
  **silently misevaluate**. LIVE BUG.

**(ii) Docs are inverted from code.** CLAUDE.md / core docs state shapes REQUIRE
`@concepts("v1:…")` and specs REQUIRE `@shape(...)`. The code **hard-rejects
`@concepts` on shapes** (`shape_converter.go:71`) — concept binding is via the
signature `shape <Concept> <name>` — and only 3 specs tree-wide use `@shape`.
`@scope` is documented for concepts but **rejected** (`concept_parser.go:379`);
`@cache` is documented but the code uses `@cacheTTL`. Authoring from the docs
produces constructs that will not load.

**(iii) A documented policy tier has zero live constructs.** The cross-cutting
decision-policy surface (`@tier`/`@frontend_visible`/`@audited`/
`@traces_persisted`) is loaded by `loadPolicyFunctions`
(`engine_bootstrap.go:294`) which walks `dsl/v1/policies/core/...`
(`policy_function_loader.go:92`) — a path that does not exist in the flattened
tree. Every live policy (`dsl/policies/policies.memql`) is an empty-bodied
provider-selection stub (`policy balancedChat { }`). The "policies compose specs
and sub-policies" model is documentation-only today.

**(iv) Four construct kinds silently swallow unknown annotations.** Tool,
Provider, Builtin, Prompt parsers have no unknown-annotation rejection (every
other kind hard-rejects). A typo'd/stale annotation on those four is dropped
with no error — e.g. `@requires(...)` on `dsl/cognition/tools/recentChat.memql:30`
is a no-op.

**Root cause:** there is no single annotation registry. Recognition is split
across three tiers — an editor-only registry (`sense/builtins.go`), a
function-level gate (`function_annotation_allow_lists.go`), and per-construct
parsers — that disagree with each other and the docs.

---

## Part 2 — Operator standardization (consolidate on Go)

No FIQL/RSQL comparison operators (`=gt=`, `=lt=`, …) exist anywhere — nothing of
that family to deprecate. Comparisons are already Go-style. The non-Go surface is
the logical connectives plus a few legacy forms.

| Context | AND today | OR today | Other non-Go | Go-aligned target |
|---|---|---|---|---|
| filter (queries) | `;` | none (uses 2 queries) | `?.` optional-chain (deprecated, still used); `in`/`has` keywords | `&&` / `\|\|` |
| spec | `&&` | `\|\|` (BROKEN — no lexer token) | — | fix `\|\|` lexing |
| logic `if` | `&&` + word `and` | word `or` (BROKEN at runtime) | `!` ok | `&&` / `\|\|` only |
| comparisons (all) | `== != > >= < <=` already | — | dead prefix builtins `and()`/`or()`/`gt()`/`lt()`/… | remove dead builtins |
| `cond(pred,a,b)` | full expr grammar | (commas are arg delimiters) | only bare-truthiness used | keep; align operators |

Recognized operator tokens (`lexer.go` / `ast.go:65`): `== != < <= > >=`, `&&`,
`!`, `:=`, `in`, `has`, `not in` (`OpOut`), `== nil`/`!= nil`. Legacy
function-call builtins `and/or/not/eq/lt/gt/lte/gte` exist (`parser.go:4431`) but
have no in-tree call sites.

Consolidation in one line: **one AND (`&&`), one OR (`||`), everywhere** — make
`||` tokenize, delete the English `and`/`or` path and the dead prefix builtins,
and decide the fate of `?.` (already marked deprecated, still authored at
`dsl/agents/queries.memql:18`, `dsl/cognition/queries.memql:24,158,170,398,432`).
`in`/`has` are readable keyword operators — owner's call whether they stay.

---

## Part 3 — Annotation cleanup candidates

Flagged with rationale; keep/remove is the owner's call.

| Category | Annotations | Evidence |
|---|---|---|
| Recognized, ZERO real usage | `@deprecated` `@role` `@permission` `@cacheTTL` `@timeout` `@rateLimit` `@retry` `@audit` `@idempotent` `@destructive` `@requiresConfirmation` `@schedule` `@async` | `function_annotation_allow_lists.go`; 0 DSL hits |
| Dead / contradictory | `@use*` family (dead allow-list, hard-rejected elsewhere); `@assert` (RETIRED in one registry, "valid" in another) | `function_annotation_allow_lists.go`; `sense/builtins.go:271` |
| Doc says live, code rejects/ignores | `@scope` (concept), `@cache`, `@skipDeleted`, `@enforceRequired`, `@defaultFilter`, `@concepts`/`@caller` (shapes) | `concept_parser.go:379`; `shape_converter.go:71` |
| Overloaded name | `@type` (concept vs provider vs `@handler` arg); `@default` (honored on fields, discarded on args) | `args_block_parser.go:121` |
| No-op on some receivers | `@enabled`/`@disabled` (no effect on tools; no semantic effect on seeds) | `tool_decl.go:48`; `seed_parser.go:328` |
| Silent-tolerance gap (add rejection) | Tool / Provider / Builtin / Prompt accept any unknown annotation | `tool_decl.go`, `provider_decl.go`, `builtin_converter.go:91`, `prompt_converter.go:81` |

> This table is the #964 point-in-time snapshot, not current state. Since
> then: the "Recognized, ZERO real usage" set was removed from the
> allow-lists in #989 (load-rejected thereafter); `@internal` was retired
> (#2620 ruling / #2708); and `@role` (#2631 ruling / #2709) and
> `@permission` (#2713) were buried -- their AST/parser/registry plumbing
> deleted. See `attribute-matrix.md` for the live disposition.

---

## Part 4 — Kitchen-sink reference (all 13 constructs)

For each construct: the full option grammar, the richest real example
(file:line), and an annotated "all-options" synthesized form. Constructs marked
UNDEREXERCISED have thin real examples; their kitchen-sinks are synthesized from
the parser/skeletons and labeled.

### 1. concept (schema)

Options. Concept-level: `@version("M.M.P")` (req, semver), `@namespace("a:b:c")`
(req), `@description`, `@type("object"|"collection"|"reference")` (collection
requires a `contains` relationship; reference requires `alias`/`equals`),
`@displayCard(primary=,secondary=,tertiary=,status=)` (real, 23x). Field types:
`string bool int float datetime any`, `[]T`, `enum(...)`, `object`,
`map(string,T)`, nested `field { ... }`. Field annotations: `@required`
`@default` `@description` `@unique` `@pattern` `@minLength` `@maxLength`
`@minimum` `@maximum` `@immutable` `@secret` `@variant(discriminator="f")`.
Body decorator: `@relationship(type="parent"|"contains"|"alias"|"equals"|
"references", field=, target="v1:ns:name", direction="outgoing"|"incoming")`.
Reserved (never declare): `id createdAt createdBy concept partition payload
schema type`. Rejected: `@scope` (#56), `@visibility`.

Richest real: the `identity` concept (`dsl/identity/concepts.memql:157`) — only
`@variant` in the tree (7-branch discriminated union); `user` (`:256`) is the
deepest nested-object example.

```
@version("1.0.0")
@namespace("v1:crm:lead")
@description("A sales lead.")
@type("object")
@displayCard(primary="payload.name", secondary="payload.company", status="payload.stage")
concept lead {
  name      string    @required @minLength("1") @maxLength("120") @description("Full name")
  email     string    @unique @pattern("^[^@]+@[^@]+$")
  stage     enum("new","qualified","won","lost") @default("new")
  score     int       @minimum("0") @maximum("100") @default("0")
  apiKey    string    @secret
  createdRef string   @immutable
  channel   object {                              // nested object
    kind    enum("inbound","outbound") @required
    source  string
  }
  tags      []string
  contact   object @variant(discriminator="kind") // discriminated union
  @relationship(type="references", field="ownerUserId", target="v1:identity:user", direction="outgoing")
}
```

### 2. shape (projection)

Options. Signature: `shape <Concept> <name>` (concept-bound) or `shape <name>`
(trait/actor). Annotations: `@row`, `@actor`, `@description`; >=1 of `@row`/
`@actor` required; mixed allowed (none in tree). Body lines: `row.X` (intrinsic),
`payload.X`, `actor.X`, `include <shape>`. REJECTED: `@concepts` (#G.3.g),
`@caller` (#221), `func (Shape)`. NOTE the internal inconsistency: `@useConcept(bareName)`
is still SUPPORTED as a legacy binding form (0 live uses), and the converter's own
reject hint for `@concepts` points authors AT `@useConcept` — even though the modern
binding is the signature `shape <Concept> <name>`. Real-tree caveats: no shape uses
`include`; only 2 `@actor` shapes; no mixed shapes. LIVE REMNANT: 3 shapes in
`dsl/safety/shapes.memql` (6/30/53) still carry the rejected `@concepts(...)` on top
of an already-present `use` import + signature binding — a redundant third concept
statement that the converter rejects (load status of these 3 needs confirmation;
removing the `@concepts` lines is the fix).

Richest real: `workerInvocationFull` (`dsl/worker/shapes.memql:8`) — 22-path
`@row` projection.

```
@row
@description("Lead card projection")
shape lead leadCard {
  row.id
  row.createdAt
  payload.name
  payload.company
  payload.stage
  // include otherShape    // composition (supported; unused in tree today)
}

@actor
@description("Auth envelope projection")
shape actorEnvelope {
  actor.userId
  actor.role
  actor.isClusterOwner
}
```

### 3. spec (predicate)

Options. `spec <name> { <single bool expr> }`. Annotations: `@enabled`/
`@disabled`, `@description`, `@shape("name")` (binding; only 3 in tree).
Classification implicit: `payload.*`/intrinsics -> row-spec (SQL pushdown);
`actor.*` -> context-spec (in-process); mixed REJECTED. Operators: `== != >
>= < <=`, `&&`, `!`, `in (a,b)`, `field==null`, parens. NOTE: `||` does not
tokenize today (see Part 1) — use of OR currently breaks the spec.

Richest real: `requiresOwnerOrAdmin` (`dsl/deployment/specs.memql:14`) — the only
one combining `@enabled` + `@description` + `@shape` + a (broken) `||` body.

```
@enabled
@description("Active, qualified leads")
@shape("leadCard")
spec specQualifiedLead {
  payload.active == true && payload.stage == "qualified"   // row-spec -> SQL
}

@enabled
@description("Caller is owner or admin")
@shape("actorEnvelope")
spec requiresOwnerOrAdmin {
  actor.role == "admin" || actor.role == "owner"           // context-spec; '||' BROKEN today
}
```

### 4. trait (concept-agnostic predicate)

Options. `trait <name> { <single bool expr> }`. Annotations: `@enabled`/
`@disabled`, `@description` only. No concept/shape binding. Same operator surface
+ row/context classification as specs; mixed REJECTED.

Richest real: `traitIsChecked` (`dsl/common/traits.memql:41`).

```
@enabled
@description("Active records in checked validation state")
trait traitIsChecked {
  payload.active==true && payload.validationState=="checked"
}
```

### 5. mutation

Options. Signature `mutation <Concept> <name>`. Annotations: `@enabled`/
`@disabled`, `@description`, `@public`/`@internal` (authz markers). `args { <name>
<type> [@required] [@enum(...)] [@default(...)] [@description] }`. Exactly ONE
body: `insert <Concept> { ... }` OR `update <Concept> { ... }` (concept currently
restated — Part 1a). Body forms: explicit `key: <expr>`; payload shorthand
`args.field` (bare arg -> same-named field); `id: args.X`; engine values
`actor.userId`, `timestamp()`, `now`, `nil`; `coalesce(args.X, default)`; object
`{}` literals.

Richest real: `mutationCreateAuditEvent` (`dsl/identity/mutations.memql:336`,
17-arg insert); update `:84`.

```
use crm.concepts.{ lead }

@enabled
@description("Create a lead.")
@public                                        // authz marker (owned|granted|admin|public)
mutation lead mutationCreateLead {
  args {
    name    string @required @description("Full name")
    email   string @required
    stage   string @default("new") @enum("new","qualified","won","lost")
    detail  object
  }
  insert lead {                                // concept restated (target: bare `insert {`)
    id:        args.id
    name                                       // payload shorthand
    email
    stage:     coalesce(args.stage,"new")
    detail:    coalesce(args.detail,{})
    createdBy: actor.userId
    createdAt: now
  }
}

@description("Bump a timestamp (partial update).")
mutation lead mutationTouchLead {
  args { leadId string @required }
  update lead { id: args.leadId; lastTouchedAt: timestamp() }
}
```

### 6. query

Options. Signature `query <Concept> <name>`. Annotations: `@enabled`/
`@description`/`@public`. Struct directives (bare keywords): `filter <pred>;
<pred>; <traitName>` (`;`-AND; `payload.X`/intrinsics/`actor.X`/`args.X`/`?.`
chains/`in`/`has`/trait+spec names), `sort "field","asc"|"desc"`, `paginate <n>`,
`shape <shape>`. `asOf`/`withDepth`/`select` exist only in the procedural call
form, not struct queries. Procedural `func (Query)` form: 0 occurrences (all
queries are struct form).

Richest real: struct directives `dsl/cognition/queries.memql:37`; multi-filter
`dsl/calendar/queries.memql:26`.

```
use crm.concepts.{ lead }
use common.traits.{ traitIsNotDeleted }

@enabled
@description("Hot leads for the caller in a score window.")
@public
query lead queryHotLeads {
  args {
    minScore int    @required
    stage    string @default("qualified")
    groupId  string
  }
  filter  payload.ownerUserId==actor.userId;        // ';' = AND  (target '&&')
          payload.score>=args.minScore;
          payload.stage==args.stage;
          payload.channel in ["inbound","outbound"]; // 'in' keyword
          ?.payload.groupIds has args.groupId;        // '?.' DEPRECATED + 'has'
          traitIsNotDeleted                           // trait by bare name
  sort     "score", "desc"
  paginate 25
  shape    leadCard
}
```

### 7. logic (imperative procedure)

Options. `logic <name> { args { ... } body { ... } }`. Annotations: `@enabled`/
`@description`. File-top `use` imports. Body: intermediate steps `name := <call>`,
`for item := range <result>.Nodes()`, `if <cond> { ... }` as value, trailing
`return <expr>`. Seen calls/methods: `queryX({...})`, `mutationX({...})`,
`publishEvent({topic:,payload:})`, `.Nodes()`, `.First()`, `.Len()`,
`coalesce()`, `concat()`, `addDuration(ts,"P25D")`, `timestamp()`. No
`ctx.output`. NOTE: use `&&`/`||` in conditions — the English `and`/`or` forms
silently misevaluate (Part 1).

Richest real: `logicAccessRequestExpirySweep` (`dsl/identity/logic.memql:36`);
`publishEvent` example `:272`.

```
use crm.queries.{ queryHotLeads }
use crm.mutations.{ mutationTouchLead }

@enabled
@description("Touch every hot lead older than the cutoff, emit a summary event.")
logic logicSweepHotLeads {
  args { event object @required }
  body {
    hot := queryHotLeads({ minScore: 80 })
    for lead := range hot.Nodes() {
      if lead.payload.lastTouchedAt < addDuration(timestamp(), "-P7D") {   // '&&'/'||' only
        mutationTouchLead({ leadId: lead.id })
      }
    }
    publishEvent({ topic: "crm.leads.swept", payload: { count: hot.Len() } })
    return hot.Len()
  }
}
```

### 8. automation — UNDEREXERCISED

Options (as actually used). `automation <name> { step <id> { logic <Logic> {
event: event } } }`. Annotations: `@enabled`, `@trigger(event="...",
concept="v1:ns:name", partition="*")` OR `@trigger(schedule="0 30 2 * * *")`,
`@description`, `@filter`. Real automations are uniformly thin delegators — every
body is `step run { logic X { event: event } }`; the work lives in the logic.
The `if`/`parallel`/`foreach`/`switch`/`publishEvent`/`webhook`/sub-automation/
`si()` step types appear ONLY in the stale/"Proposed" `docs/public/language/functions.md`
— no real automation uses them.

Canonical real shape (`dsl/identity/automations.memql:236`):

```
@trigger(event="node.created", concept="v1:crm:lead", partition="*")
@description("On new lead, run the qualification sweep.")
automation onLeadCreated {
  step run {
    logic logicSweepHotLeads { event: event }
  }
}

// cron form:
@trigger(schedule="0 0 4 * * *")
@description("Nightly lead hygiene.")
automation nightlyLeadHygiene {
  step run { logic logicSweepHotLeads { event: event } }
}
```

### 9. policy — UNDEREXERCISED (model is doc-only)

Documented options: `@tier("core"|"bff")`, `@frontend_visible`, `@cacheable`,
`@audited`, `@traces_persisted`, `@description`; body `func (Policy) name(ctx any)
<ret> { if policy("sub",{...}) {...}; if spec("name") {...}; return ... }`.

Real tree: the ONLY policy file (`dsl/policies/policies.memql`) has 7 policies,
ALL empty-bodied with only `@description` + provider-selection annotations
(`@primary`/`@fallback`/`@maxLatencyMs`/`@preferredRole`). The decision-policy
loader reads a dead path (Part 1 iii), so the tier/audited/compose model has
ZERO live constructs.

```
// live form (provider selection):
@description("Default chat policy. Claude Sonnet primary, Haiku fallback.")
@primary("balancedChat")
@fallback("cheapestCapable")
@maxLatencyMs(8000)
policy balancedChat { }

// documented-but-not-live form (decision policy):
@tier("bff")
@frontend_visible
@audited
@description("May the caller run the deploy console?")
func (Policy) canUseDeployConsole(ctx any) bool {
  return spec("requiresOwnerOrAdmin")
}
```

### 10. provider

Options. Base: `@base`, `@type("OpenAI"|"Anthropic"|"OpenAIAudio"|...)`, body
`auth { apiKey env("VAR"); baseURL "url" }`. Derived: `@extends("base")`,
`@model("id")`, `@modality("audio")`, `@default`, `@description`, body `params {
contextWindow N; maxCompletionTokens N; inputCostPerMillion F;
outputCostPerMillion F; cachedInputCostPerMillion F; voice "alloy";
format "pcm16" }`.

Richest real: base `google` (`dsl/providers/providers.memql:267`); derived
`chat54Mini` (`:80`).

```
@base
@type("OpenAI")
provider openai {
  auth {
    apiKey  env("MEMQL_AI_OPENAI_API_KEY")
    baseURL "https://api.openai.com/v1"
  }
}

@extends("openai")
@model("gpt-5.4-mini")
@default
@description("Balanced cost/latency chat.")
provider chat54Mini {
  params {
    contextWindow             128000
    maxCompletionTokens       16384
    inputCostPerMillion       0.15
    outputCostPerMillion      0.60
    cachedInputCostPerMillion 0.075
  }
}
```

### 11. prompt

Options. `prompt <name> { <schema fields> }`. Annotations: `@defaultProvider`,
`@templateFile`, `@description`. Body IS the input schema: `<name> <type>
[@required] [@description]`; types `string`/`object`/`[]object`. No `@input`
wrapper, no `func (Prompt)`.

Richest real: `conductorTurn` (`dsl/cognition/prompts.memql:94`, 11 fields).

```
@defaultProvider("chat54Mini")
@templateFile("prompts/leadTriage.tmpl")
@description("Classify a lead and suggest next action.")
prompt leadTriage {
  lead        object   @required @description("The lead row")
  history     []object @description("Prior touches")
  hintContext object
}
```

### 12. builtin (Go-backed executor)

Options. `builtin <name> { <schema fields> }`. Annotations: `@enabled`/
`@disabled`, `@executor("integration.X.Y")` (req), `@args(profile="object")`,
`@sdk`, `@alias`, `@description`. Body fields: `<name> <type> [@required]
[@default] [@enum] [@description]`.

Richest real: `agentworkerRequestScope` (`dsl/worker/builtins.memql:70`).

```
@enabled
@executor("integration.crm.scoreLead")
@args(profile="object")
@description("Score a lead via the CRM model.")
builtin crmScoreLead {
  leadId  string @required
  model   string @default("default")
  factors []string
}
```

### 13. tool (AI-callable)

Options. `tool <name> { <schema fields> }`. Annotations: `@enabled`/`@disabled`,
`@handler(...)`, `@executionTime("fast"|"medium")`, `@allowedRoles`, `@scopes`,
`@clientExecution`, `@description`. Two `@handler` forms: `@handler(type="query",
query="queryX({\"a\":\"$args.a\"})")` (inline call + `$args.X` interpolation) and
`@handler(type="function", name="fnName")`. Body fields: `<name> <type>
[@required] [@default] [@enum] [@description]`; types `string`/`integer`/
`boolean`.

Richest real: `calendarCreate` (`dsl/calendar/tools.memql:36`).

```
@enabled
@handler(type="query", query="mutationCreateLead({\"name\":\"$args.name\",\"email\":\"$args.email\"})")
@executionTime("fast")
@allowedRoles("owner","admin","writer")
@description("Create a CRM lead for the caller.")
tool crmCreateLead {
  name   string  @required @description("Full name")
  email  string  @required @description("Email")
  stage  string  @default("new") @enum("new","qualified") @description("Initial stage")
}
```

---

## Cross-cutting observations for standardization

1. **One concept statement per construct.** Bind in the signature; drop the
   restatement in `insert`/`update` bodies and prefer signature/import resolution
   over `canonicalId(…, "v1:…")` / `concat("v1:…", …)`.
2. **One AND, one OR, one negation, Go comparisons — everywhere.** Pick `&&` /
   `||` / `!`; retire `;`-AND, `,`-OR, English `and`/`or`, `?.`, and the dead
   prefix builtins; fix `||` lexing.
3. **One annotation registry.** Collapse the three tiers into a single
   source-of-truth; make every construct kind reject unknown annotations; reconcile
   the docs to the code (or vice-versa) for the inverted items.
4. **Decide the policy story.** Either revive the decision-policy loader (fix the
   dead path) or formally retire the tier/audited/compose model and keep policies
   as provider-selection records.
5. **Fix the two live bugs** regardless of the broader standardization timing.

Owner markup pass: annotate keep / remove / change against the candidates in
Parts 2-3 and the option lists in Part 4; the result seeds the standardization +
cleanup epic.
