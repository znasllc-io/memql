# MemQL DSL version one -- the language freeze, a six-epic program

- **Date:** 2026-09-13
- **Status:** agreed with the owner in the 2026-09-13 brainstorm. Every fork below was put
  to the owner as selectable options and answered (the expression shape, the
  logic-versus-automation boundary, loop protection, versioning after 1.0, the corpus
  and its CI standing, the execution approach); the derived decisions follow from those
  answers and were presented as eight sections, each approved. Issues are filed by the
  first epic's plan (`superpowers:writing-plans`), not by this record.
- **What it is:** the design for freezing the MemQL DSL at version one so that later
  changes are rare, safe and mechanical: one expression grammar with a named lambda
  parameter at every predicate position and one lowering to SQL checked at load; one
  body language shared by `logic` and `automation`; an attribute matrix with one
  registry, one gate and generated docs, with everything nothing reads retired; loop
  protection that refuses a cycle at load and caps depth at runtime; a declared language
  version per bundle with editions, a migrator and a deprecation window; and an
  executable conformance corpus with a completeness gate and a required differential
  lane between the two evaluators.
- **Why it is pivotal:** the DSL is the product surface. A client's repository holds its
  business logic, queries, mutations and automations in it; integrations are reached
  through it; and it is the vocabulary a model is given so that one construct call can
  stand in for a program. After release, a change to it is expensive for everyone who
  wrote against it. Today there are eight expression evaluators, four of them
  hand-rolled string scanners; `!` means three things; AND has three spellings; a
  lambda syntax exists and is used nowhere; six attributes are parsed into the AST that
  no allow-list can populate; the annotation registry and the loaders disagree; there
  is no golden-file corpus and no fuzz target; and three concrete automation loops run
  today with a 120-per-minute budget as the only ceiling.
- **Repositories:** `memql` (`component/language`, `component/memql`,
  `component/automations`, `component/work`, `component/database` for the concept
  parser's move, `cmd/memqlmigrate`, `cmd/memqllint`, `test/dslconformance`,
  `test/conformance`, `docs/public/language`, `dsl/`); every product bundle repository
  receives one migration PR per epic; `memql-cockpit` is untouched.
- **Predecessors:** the authoring rules (`docs/public/language/authoring-rules.md`),
  the grammar-versioning and codemod-per-epic rule already in force, the work spine
  (`2026-09-05-work-spine-design.md`), and the app-session recording and learning
  program (`2026-09-13-app-session-recording-and-learning-program-design.md`), whose
  construct-authoring epics schedule after this program's epics 2 and 3.

---

## 1. Problem

The owner's asks, made precise:

1. **Land a version one worth freezing.** Once people write bundles against it, a
   change is problematic; the language must be very good before release, and later
   changes must not be major.
2. **One way to declare a condition.** Query filters use one grammar, specs another,
   mutation values another, logic bodies another, automation conditions and trigger
   filters a runtime string grammar. Unify on the lambda form, everywhere.
3. **Logic must carry every kind of business logic a client needs**, within the
   boundary that MemQL is not a new general-purpose language, and the confusion between
   logic and automations must end: today calling one logic on an event means wrapping
   it in a one-step automation.
4. **The engine must recognise infinite loops** between automations reacting to each
   other's events, and refuse or bound them.
5. **Remove what no longer applies**, replace ugly call syntax with shorthand (as
   `coalesce()` became `??`), and make every construct easy to learn and read.
6. **A matrix of every construct and every attribute, with every possibility in a unit
   test that executes**, including the most complex constructs and automations.
7. **The DSL is an aid to intelligence:** the rules let the engine tell a model far
   more than a program would; humans and models use constructs the DSL defined.

## 2. What the tree already has

Verified on 2026-09-13 against `main` at `8a063ec3f` by three codebase lanes; file
references are the map a future session re-checks first.

### 2.1 Seventeen construct kinds, two dispatch paths

`concept`, `shape`, `provider`, `builtin`, `tool`, `prompt`, `policy`, `rule`,
`spec`, `trait`, `seed`, `action`, `capability` go through the parser's keyword table
(`component/language/parser/parser.go:603`); `query`, `mutate`, `logic`, `automation`
are rewritten to the internal receiver form before the lexer runs
(`parser/rewriter.go`). `mutate` declares and `mutation` invokes: two words for one
thing. Corpus counts under `dsl/`: shape 745, query 534, mutate 418, seed 376, concept
251, builtin 203, action 83, automation 62, logic 58, capability 56, tool 50, provider
43, trait 43, prompt 26, spec 13, rule 11, policy 10.

### 2.2 Eight expression evaluators

| | Evaluator | Consumes |
|---|---|---|
| E1 | the AST converter: SQL pushdown plus in-process post-filter (`component/memql/ast_converter.go`) | query `filter`, spec and trait bodies |
| E2 | the same converter with collection methods (`collection_method.go`) | logic bodies, collection lambdas |
| E3 | the runtime STRING evaluator (`component/automations/evaluator.go`) | automation conditions, trigger `@filter`, preconditions, forEach `where` |
| E4 | the mutation-template evaluator, a string path and an AST path (`mutation_templates.go`) | `insert` and `update` values |
| E5 | the step-argument string resolver (`automations/steps/function.go`) | automation step arguments |
| E6 | Go text templates | prompt `.tmpl` files |
| E7 | textual `$args.` substitution (`tool_execution.go`) | tool `@handler(query=...)` |
| E8 | the LogicRunner's local short-circuits (`logic_runner.go`) | logic statements |

E1 and E2 share one lexer and parser; E3, E4's string half, E5 and E7 are four separate
hand-rolled scanners. The consequences are measurable: `in`, `startsWith` and
`when(...)` exist only in E1; `exists`, `len` and `!` exist only in E3; `!` parses
everywhere and is refused by E1 and E2 (`ast_converter.go:380`) while it works in E3
(`evaluator.go:1557`), so the one `!` in the corpus (`dsl/data/logic.memql:41`) is
legal only because an `if` condition inside a logic body is canonicalised to a string
and routed to E3, and the same text in the `return` one line below would refuse the
load. E1 has a published precedence; E3 has OR-then-AND and then probes the string for
`==`, `!=`, `>=`, `<=`, `>`, `<` in that order, so an operator inside a quoted literal
is found. E1's `!=` is null-safe (absent matches); E3's collapses nil and blank. Bare
names mean four things by position: a payload field, a spec reference, a step id, a
punned args field. The trigger `@filter` is documented as taking the query-filter
grammar; it does not.

### 2.3 Lambdas exist and are used nowhere

The lexer emits `=>`; the parser accepts `x => expr` and `(x, y) => expr` only as the
argument of one of twenty collection methods (`collection_method.go:20`); the engine
admits them only in logic bodies (`function_loader.go:706`) and refuses a mutation
inside one. `grep -rn "=>" dsl/` finds two comments in never-loaded reference
skeletons and the terse automation header `automation X @trigger(...) => logic Y`,
which is a regex rewrite that shares the glyph and nothing else.

### 2.4 The body languages

A logic body accepts `name := <call>`, `name := retry(n) <call>` (zero uses), `if`
with a single call (multi-statement bodies refuse), `if / else if / else` flattened
into steps stamped with the condition, `for item := range <expr>` with the loop
variable pinned to `item`, and a mandatory trailing `return`; no early return, no
`while`. The LogicRunner compiles the body to an automation and executes in the
compiler's topological order over the reference graph, not in source order, with ties
resolved arbitrarily. An automation body accepts `step` blocks of thirteen kinds, of
which `parallel`, `shape` and `webhook` have zero uses; a prior step is read in three
spellings (`steps.<id>.result`, bare `<id>.result`, bare `<id>` as an argument).

### 2.5 The attribute surface

The registry (`component/language/annotations/registry.go`) is the parse gate for
seven constructs and the load gate for the four function kinds; concepts are gated by
a different table in `component/database/memory-nodes/concept_parser.go`, outside the
language package; actions and capabilities skip the shared validator. Six function
attributes (`@deprecated`, `@timeout`, `@retry`, `@idempotent`, `@audit`, `@version`)
are parsed into `FunctionDef` fields, copied into the runtime function and rendered by
`help()`, and refused by every allow-list. `@unique` and `@immutable` on fields,
`@rateLimit` and `@scopes` on tools, `@latestMode`, `@namespace` on concepts and
`@enabled` are read by nothing that changes behaviour; `@default` on a field is
metadata never applied on insert. Two spellings exist for cache TTL, for no-cache, for
a schedule trigger, for the mutation keyword and for `@type` and `@default` across
constructs. Prompt fields silently tolerate unknown annotations; builtin fields drop
every annotation but `@required`. The docs' attribute matrix still lists three policy
attributes the parser refuses, and nothing pins that section.

### 2.6 Tests

`test/dslconformance` holds sixty-six text and structure gates over the corpus, none
of which executes a construct; `component/language` holds ninety-eight parser tests
with inline literals; there is no `testdata/` golden corpus and no `func Fuzz` in the
repository. One test boots the engine over the whole embedded tree; automations are
loaded only by their own package's tests. Bundles mounted at the runtime DSL path are
outside every conformance gate.

### 2.7 Loops

Every successful write, updates included, publishes `graph.node.created.<concept>`
because rows are append-only (`executor_mutation.go:152`). The guards that exist:
the journal is skipped for automations triggered by `v1:work:*` (protecting the journal
writer, not the automation); a per-process dedup and a cluster guard keyed on a hash of
the whole event payload, which carries `createdAt` and therefore changes on every write;
a storm counter that only warns; a process-global budget of 600 per minute and 120 per
automation per minute (`budget.go:38`), which is the only thing that bounds a true
loop, at 120 runs a minute forever; a one-level refusal for the `automation` step kind
against its immediate parent. Published events carry no causation id, no
correlation id and no depth. `work.Ceilings.MaxEvents` has no production caller.
`work.UnionFootprint`, which knows the concepts a call writes, has no caller outside
its test. Three loop shapes run unguarded today: an automation on `node.created` of a
concept whose step updates that concept (`dsl/forge/automations.memql:30`); two
automations writing each other's concepts, held open only by prose in
`dsl/library/automations.memql:250`; and a logic publishing the topic its automation
triggers on, through a LogicRunner that deliberately bypasses dedup, budget and
journal.

### 2.8 What the research found

- **Embedded, safe, pushed-down expression languages.** CEL's design goals are the
  ones MemQL needs: side-effect free, terminating, gradually typed, and, in its
  authors' words, designed so that translation to SQL unlocks offline checks and so
  that a launch subset keeps its guarantees while the language grows. Its macros take
  an explicit named variable, its precedence is published once, absence and error are
  distinct, and Kubernetes bounds its cost statically and at runtime. The implicit-
  receiver languages (JMESPath, jq, JSONata) read well at one level and become
  ambiguous when nested, which is the class of bug a named parameter removes. The
  object-relational mappers that compile lambdas to SQL (expression trees, Entity
  Framework) define the pushdown subset by refusing at compile time, and Entity
  Framework's own breaking-change record explains why: silent client evaluation pulled
  whole tables and "the warning proved too easy to ignore".
- **Loop protection.** Drools' `no-loop` and property reactivity; SQL Server's nested
  trigger limit of 32 and its distinction between direct and indirect recursion;
  Oracle's refusal of a mutating table; Salesforce's trigger depth of 16 and its
  preference for before-save fixes; Lambda's recursion detection at about 16 with an
  explicit per-function opt-in; EventBridge's guidance to fire on bad state rather than
  on every change; Datalog's stratified negation as the static algorithm; correlation
  and causation ids as the event-sourcing convention.
- **Versioning.** Go's compatibility promise and the `go` line that keys semantics per
  module; Rust editions (warning-free code on edition N compiles on N+1 with the same
  behaviour, one core compiler, thin front ends, `cargo fix --edition`); Dart's per-
  library language override for migrations; protobuf's breaking classifier with
  reserved numbers; Kubernetes' removal windows; Python's two-minor-release warning
  floor; Terraform's pre-1.0 per-version migrators.
- **Conformance corpora.** CEL's per-feature files with fully specified environment
  and expected value or error; the JSON Schema suite's `{schema, tests: [{data,
  valid}]}` shape; WebAssembly's distinct verdicts (return, trap, malformed, invalid);
  toml-test's one-fault-per-file negatives named after the fault and per-version
  manifests; SQLite's SQL Logic Test running millions of queries against four other
  engines and its rule that a bug is not fixed until its case is in the suite; native
  Go fuzzing; differential testing (McKeeman, 1998).
- **Readability for models.** Surveys find DSL syntax poorly represented in training
  data and hallucinated names the dominant failure; grammar-in-prompt and vocabulary-in-
  context recover most of the gap; constrained decoding removes syntax errors but can
  distort quality, so it is a net for syntax only; diagnostics that name the fix, with
  a re-check loop, repair most compile errors.

## 3. Decisions

The first six were put to the owner and answered; D7 to D24 follow from them and were
approved section by section; D25 records the editor-parity requirement the owner added
while the record was being written.

### D1 -- The receiver is a named lambda parameter, at every predicate position

`filter row => row.status == args.status && row.active`;
`spec registration isRevoked = row => row.revoked == true`;
`spec actorEnvelope requiresAdmin = actor => actor.role == "admin"`;
`trait isActiveRecord = row => row.active == true`; collections
`row.items.any(i => i.qty > 0)`. Intrinsics are already reserved field names, so
`row.id` and `row.status` never collide and the `row.` namespace exception disappears
with the bare-name rule. Chosen over the implicit receiver (two ways to name a field
survive, the `row.` exception stays) and over block lambdas with statements (the
boundary the owner named argues against them, and the pushdown subset becomes hard to
explain).

### D2 -- One body language, two keywords

`logic` is a callable body; `automation` is a trigger plus the same body. Steps become
statements, so the one-step wrapper is no longer needed. A logic never takes a trigger,
so there is exactly one kind of subscriber for the loop rules to govern. Chosen over one
keyword with an optional trigger (the OS, the docs and the work spine all use the noun
automation for the thing that fires) and over letting logic take a trigger (two
subscriber kinds, two body grammars, and the loop problem doubles).

### D3 -- Refuse cycles at load, annotate to permit, cap depth at runtime

Chosen over runtime-only (a loop still runs N times per event on every replica) and
over a static warning (keeps every current automation loading, and every current loop).

### D4 -- A bundle declares its language; editions, a migrator, a deprecation window

Chosen over a hard freeze with breaking changes only at 2.0 (the pressure to bend it
arrives with the first mistake) and over keeping no-shims for the language (every
release can break a bundle nobody re-ran the migrator on). The no-shims rule stays for
Go seams.

### D5 -- A data-driven corpus with a completeness gate and a required differential lane

Chosen over an advisory differential lane (the one place the language has two
implementations of one meaning would stay unproven on the commit that changes it) and
over hand-written Go tests alone (no matrix, no completeness, no file a model can read).

### D6 -- Edition-first, one epic per axis, everything migrated in the same PR

Chosen over a big-bang rewrite (a branch that size cannot clear the strict checks
against a main that moves daily) and over freezing what exists (it ships the eight
evaluators, the three spellings and the loops as version one).

### D7 -- One parser, one AST, two evaluators; the string evaluators are retired

The SQL lowering and the in-process evaluator consume the same AST and are held equal
by the differential lane. E3, the string half of E4, E5 and E7 are retired: automation
conditions, trigger filters, preconditions, step arguments, mutation values and tool
handler arguments become expressions parsed once. E6 (prompt templates) stays a
template language, because prose with holes is not a predicate, and its load gate
(every variable declared) is kept.

### D8 -- Booleans only, and absence is a value with one table

Every condition is boolean-typed at load against the concept's declared field types.
`??` keeps its blank-coalescing meaning and is documented as such; `row.?a.b` chains
through an absent object; comparisons against absent follow one published table (`!=`
is true on absent, as authoring rule 27 says today) that the lowering reproduces with
`IS DISTINCT FROM` and `COALESCE`. There is no truthiness: a string is never a
condition.

### D9 -- Operators, precedence and the retired connectives

CEL's order: member and call; unary `!` and `-`; `* / %`; `+ -`; `??` (kept tighter
than comparison, as today, so `args.stage ?? "" == "active"` groups the way it reads);
comparison with `in` and `startsWith`; `&&`; `||`; and a lowest, right-associative
`? :` that must be parenthesised when nested in the pushdown tier. `!` works
everywhere. `and`, `or`, `;` and `,` as connectives are gone. `when(args.x) { }` is
gone: `args.x == nil || row.f == args.x` says the same thing, lowers the same way, and
the lowering recognises the pattern.

### D10 -- One function catalog, one spelling each

`cond(p, a, b)` becomes `p ? a : b`; `concat(a, b)` becomes `a + b` on strings;
`exists(x)` becomes `x != nil`; `len(x)` and `count` become `.count()`; the string
`contains` and the graph traversal `contains` get different names; `addDuration` and
`daysBetween` join the catalog under one tier flag. Every function has one signature
in one catalog, with the tier it belongs to.

### D11 -- Tiers are data, lowering runs at load, and there is no silent fallback

A manifest maps every position (query filter, sort, spec body, row-authz argument,
automation condition, trigger filter, logic body, mutation value, step argument, tool
default, prompt input) to the node kinds and functions allowed there. Pushdown
positions get the P tier; in-process positions get the M tier, which is P plus time
arithmetic, formatting, regular-expression matching and the collection methods over
in-memory values. `Lower(ast)` runs in `MemQLEngine.Init` for every P-position
expression; a node that does not lower is a load refusal naming the node, the position
and the nearest P spelling. Evaluating an expression in process over a bounded page is
a named construct, never an automatic escape. A collection method in the P tier
requires a bounded source; the M tier carries a static cost estimate and a runtime
budget.

### D12 -- Statements execute in source order; a name is its value

`name := <expression or call>`, a bare call, `if / else if / else` with multi-statement
blocks, `for x in <expr> if <cond> { }` with the author's own loop name, `switch <expr>
{ case "a" { } default { } }`, `parallel { branch a { } branch b { } } wait all|any`,
`retry(n)` and `on error stop|continue` per statement, `publish <topic> { }` in
automations only, and `return <expr>` anywhere, which ends the body and, in an
automation, the run. A reference to a later name is refused at load. The silent
topological reordering ends. `rows := query activeUsers(status: "active")` then
`rows.count()` and `rows.first().email`; `steps.<id>.result`, the bare-id argument pun
and `.result.result` climbs are retired.

### D13 -- Calls keep their kind prefix; `mutate` and `mutation` become one word

`query`, `mutation`, `logic`, `builtin`, `automation` prefix a call, because the prefix
is what makes a call resolvable and readable; the declaration keyword becomes
`mutation` too.

### D14 -- What a logic may do, and journal parity

A logic reads and writes through queries and mutations, calls builtins and other logic,
and returns. It may not publish an event and it may not subscribe. A logic reached from
an automation counts toward that automation's write footprint. A logic invoked inside
a run is journaled as that run's steps; a logic invoked directly is journaled as its
own run only when it writes; a read-only logic leaves no run row.

### D15 -- Triggers: the filter is a lambda; the synonyms and the terse form go

`@trigger(event=..., concept=...)` or `@trigger(schedule=...)`, `@filter(row => ...)`
over the event's row, `args` bound from the payload as today. The `partition` kwarg
(already discarded at load), the `@schedule(cron=)` synonym and the terse
`automation X @trigger(...) => logic Y` form are retired; the last also because `=>`
now means lambda.

### D16 -- One registry, one gate, generated docs, unknown means refused

The annotation registry in `component/language` is the single source of truth for
every construct; the concept vocabulary moves out of the database package into it;
every construct parser calls the same validator; the construct list, the body-block
list and the field-annotation lists derive from the parser tables; the attribute matrix
in the docs is generated from the registry and pinned. Prompt fields and builtin fields
get the allow-list args fields have.

### D17 -- The retirements and the single spellings

Retired because nothing reads them: `@deprecated`, `@timeout`, `@retry`,
`@idempotent`, `@audit` and `@version` on functions; `@unique` and `@immutable` on
fields; `@rateLimit` and `@scopes` on tools; `@latestMode`; `@namespace` on concepts;
`@enabled`; the `import (...)` block; `?.` and the `;`/`,` connectives in the parse
cascade; `@default` on a field (never applied; `??` in the mutation is the mechanism).
One spelling each: `@cache(N)` with `@cache(0)` for no cache; `@trigger(schedule=)`;
`mutation`; provider `@type` becomes `@vendor`; tool `@allowedRoles` becomes
`@requiresRank` and `@requiresCapability`. `@displayCard` and `@composable` go unless
the OS reads them, which the epic verifies first. Every retired form refuses with the
migrator's name.

### D18 -- The static graph, stratification, and the permitting annotation

At boot the engine builds one directed graph over automations: an edge from A to B when
the concepts A writes, unioned transitively through every logic and mutation it
reaches, intersect the concept B triggers on, refined by event kind and by a `@filter`
that is statically decidable against A's writes. Strata are assigned the way Datalog
stratifies; a back edge is a cycle; the load refuses on any cycle no annotation covers
and prints the path. A self-edge is refused by default. `@loop(maxDepth=N, until=row
=> ...)` on the automation that closes a deliberate cycle permits it; the predicate
must appear in that automation's trigger filter. An edge the analysis cannot decide is
named in the refusal with the filter it could not read.

### D19 -- Causation, depth, budgets and modes at runtime

Every event an automation publishes or causes carries a causation id, the correlation
id of the chain and a chain depth, distinct from the mesh hop TTL. Depth is capped at a
value, 16 proposed, and a refusal is a run failure `loop_depth_exceeded` carrying the
chain. The chain-head dedup, the cluster guard and the per-window budget stay; the
dedup key stops hashing the whole payload; a per-(automation, row id) budget is added;
each automation declares `mode: single|queued|restart|parallel` with a `max`.

### D20 -- The in-write body

An automation that only adjusts fields on the row that triggered it may declare a
before-write body, so no second write and no second event exist.

### D21 -- Breaking is classified on three axes; deletion needs a reservation

Grammar (a form stops parsing), meaning (same text, different rows or decision),
defaults (an omitted annotation changes meaning). `memqlbreaking` diffs two corpora
with the categories parse, meaning and wire (generated SDK methods, shape keys, event
payloads). A construct or attribute name is deleted only with a reserved entry, so it
can never return with another meaning; the concept-field snapshot ledger is one row of
that matrix already.

### D22 -- The deprecation window and the reserved names

A public language form deprecates with a load-time warning naming its replacement for
at least two minor releases before it refuses; deprecated use is counted so removal is
evidence. Experimental attributes are opted into per bundle and carry no promise.
Annotations are engine-owned; a bundle may not declare one. A list of future keywords
is reserved now; payload fields get a raw-identifier escape; `now`, `actor`,
`partition`, `config`, `trace` and `event` stay reserved; underscore-prefixed names are
the experimental namespace.

### D23 -- The corpus, its verdicts and its policy

`test/conformance/<edition>/` with `cells/<construct>/<attribute>/`, `expr/<position>/`,
`scenarios/<domain>/`, `negative/<construct>/<fault>.memql`, `fuzz/` and a version
manifest. Verdicts: `load_ok`, `refuse_parse`, `refuse_load`, `lower`, `evaluate`,
with the refusal code and a message prefix as the contract. Two completeness gates, one
over registry cells and one over tier-table entries. The differential lane is in the
required db-tests set. A bug is not fixed until its case is in the corpus. The corpus
is the docs' example source and the models' few-shot source.

### D24 -- Readability: one form, a shipped grammar and vocabulary, refusals that name the fix

One canonical form per operation; symbols for `&&`, `||`, `!`, `==`, `??` and keywords
for the rare; the `///` doc comment as the only description channel, on every
construct, argument and field, gated. A BNF generated from the parser plus the tier
manifest ships with the engine and is served over the introspection builtins, for
grammar-in-prompt and syntax-only constrained decoding. A vocabulary dump lists every
construct, annotation, builtin and function with a description of three or four
sentences. Every refusal names the construct, the position, the rule id and the
replacement, and when the fix is mechanical, the rewritten line; the corpus pins the
wording. The Sense service reads the same registry and tier manifest as the loader.
Attributes are named verb-first, object named, never abbreviated.

### D25 -- The editor speaks the cluster's language, and cannot silently lag it

The VS Code extension ships its own language server (`cmd/memql-lsp`), whose TextMate
grammar and language configuration are generated from the engine's `dslspec` tables,
and it learns only the engine version at connect. Three rules close the gap:

- **One source, generated.** The language server, the TextMate grammar, the language
  configuration and the snippets are generated from the registry, the tier manifest
  and the function catalog of D16, D11 and D10 at build time, never hand-edited; a gate
  fails when a generated asset is stale against the tables.
- **The extension declares what it was built from.** `package.json` carries
  `memql.edition` and `memql.grammarVersion`; a test in the engine repo compares them
  with `parser.GrammarVersion` and the edition and fails when the parser moved and the
  extension's pin, version and changelog did not move in the same PR, so a grammar
  change cannot merge without the extension release that carries it.
- **The cluster and the editor compare at connect.** The status the extension already
  reads gains the cluster's edition and grammar version; a cluster newer than the
  extension shows a notice naming both versions and the release to install, and the
  language server keeps working on the forms it knows; a cluster older than the
  extension warns that the editor may offer forms this cluster refuses. The Sense
  handlers on the engine answer the same question over the stream for any other
  client.

## 4. The six epics

Each names its change by path, its migration, its failure modes and its tests.

### Epic 1 -- Foundations: the language line, editions, the registry, the corpus scaffold

- `dsl/` and every bundle manifest: `memql = "1.0"`; the loader refuses a mounted
  bundle without one and one newer than the engine, naming both. `parser.GrammarVersion`
  stays the fine label; `Edition` is added as the coarse one, `2026`; the parser front
  end is selected by the declared edition.
- `cmd/memqlmigrate`: the framework gains `--edition` and a rewrite registry keyed by
  epic; every later epic registers its rewrite here.
- `component/language/annotations`: the concept vocabulary moves in from the database
  package; actions and capabilities call the shared validator; `dslspec` derives from
  the parser tables; `docs/public/language/attribute-matrix.md` is generated and
  pinned; the registry parity test covers every receiver, not four.
- `test/conformance/2026/`: the layout of D23, seeded from today's grammar so the two
  completeness gates are green on day one; the verdict runner; the version manifest.
- `editors/vscode/package.json`: `memql.edition` and `memql.grammarVersion`; the
  parity test of D25; the generated-asset staleness gate over `cmd/memql-lsp`'s
  grammar and language configuration.
- **Failure modes.** A bundle with no language line refuses with the line to add. A
  cell with no case refuses the build naming the cell.
- **Tests.** The gates themselves; a bundle-version refusal test; a generated-matrix
  drift test.

### Epic 2 -- The expression language

- `component/language/parser`: the lambda parameter at every predicate position; the
  precedence of D9; `!`, `in`, `startsWith`, `?.`-style optional chaining as `.?`,
  `? :` everywhere; `and`, `or`, `;`, `,`, `when` refused with the rewrite named.
- `component/memql/ast_converter.go` becomes the one lowering with the tier manifest
  (`component/language/tiers`); `Lower` runs at `Init` for every P position;
  `mutation_templates.go`'s string path, `automations/evaluator.go`,
  `steps/function.go`'s resolver and `tool_execution.go`'s substitution are deleted;
  their positions consume the AST.
- The function catalog of D10 in `component/language/functions`, one entry per function
  with signature and tier; the Sense service reads it.
- `cmd/memqlmigrate --rewrite=expressions`: filters, specs, traits, conditions, values
  and handler arguments rewritten across the tree; `when` guards rewritten to the
  `== nil ||` form.
- **Failure modes.** A P-position expression that does not lower refuses at load
  naming the nearest P spelling. A condition that is not boolean-typed refuses naming
  its type. The two evaluators disagreeing on a fixture is a differential-lane failure,
  never a runtime surprise.
- **Tests.** `expr/<position>/` cells for every node kind and function, positive and
  negative; the differential lane over awkward rows; fuzz targets for lexer, parser
  and lowering.

### Epic 3 -- The body language

- `component/language/parser` and `compiler`: statements of D12 for `logic` and
  `automation`; source-order execution with forward references refused; the terse
  header retired; `steps.<id>` spellings retired; loop variables author-named.
- `component/automations`: the executor runs statements in order; `parallel`, `retry`
  and `on error` are statement forms; `publish` is refused inside a logic; the journal
  records statements as steps exactly as today.
- `component/memql/logic_runner.go`: the LogicRunner's short-circuits go, since the
  in-process evaluator is now the one M-tier evaluator; direct logic calls that write
  open a run.
- `cmd/memqlmigrate --rewrite=bodies`: step blocks rewritten to statements, terse
  automations expanded, step references rewritten, loop variables renamed.
- **Failure modes.** A forward reference refuses naming both statements. A logic that
  publishes refuses naming the automation form. A body whose topological order today
  differs from its source order is flagged by the rewrite, which reorders and leaves a
  comment naming the move, so the migration is readable.
- **Tests.** `cells/logic/*` and `cells/automation/*` for every statement form and
  attribute; scenario suites for the decide-and-apply sweeps, the forge state machine,
  the deployment pipeline and the campaigns engine; dry-run and resume over the new
  compiled form.

### Epic 4 -- The attribute cleanup

- Retirements and single spellings of D17 in the registry, the parsers and the
  loaders; dead `FunctionDef` fields removed; `help()` no longer renders them.
- `cmd/memqlmigrate --rewrite=attributes`.
- **Failure modes.** A retired attribute refuses naming the rewrite. A bundle carrying
  `@displayCard` after the OS check refuses the same way.
- **Tests.** One negative cell per retired form; the registry parity test; the
  generated matrix.

### Epic 5 -- Loop protection

- `component/work/footprint.go` gains the callers it never had: `component/automations`
  builds the graph of D18 at load, in the loader, with the stratification and the path
  in the refusal; `@loop` and `mode` in the registry.
- `component/events`: causation id, correlation id and depth on the envelope; the
  executor stamps them; the depth cap of D19; the per-row budget; the dedup key
  narrowed to the fields the automation reads.
- The in-write body of D20 as a statement position.
- The architecture model renders the graph; the counter of loops stopped by reason.
- **Failure modes.** A cycle the analysis cannot decide refuses naming the edge and
  the filter. A deliberate cycle without `until` in its filter refuses naming the
  predicate. Depth exceeded is a run failure with the chain.
- **Tests.** The three shapes of section 2.7 as scenarios: each refuses at load, each
  is permitted by `@loop` and then stops at its predicate, and none reaches the depth
  cap; a runtime test that a chain of 17 is stopped at 16 with the chain recorded.

### Epic 6 -- The freeze

- The differential lane joins the required db-tests set; the fuzz seed corpus is
  committed; the BNF and the vocabulary dump are generated and served; every refusal's
  wording is pinned; `memqlbreaking` runs in CI against the previous edition's corpus;
  the version manifest flips to edition 2026 and language 1.0; the docs are regenerated
  from the corpus.
- The extension release: the language server, grammar, configuration and snippets
  regenerated for edition 2026, the connect-time edition comparison of D25, and the
  extension version and changelog moved in the same PR as the version manifest.
- **Tests.** The lane itself; a `memqlbreaking` self-test over a deliberately breaking
  fixture; the refusal-wording pins.

## 5. Cross-cutting rules

- **Every epic migrates everything in the same PR:** the in-repo tree and every known
  product bundle, each product repository getting its own PR, with the template
  repository's lint against the latest engine as the cross-repo gate.
- **Mechanics before meaning.** Each epic lands its codemod and its corpus cells first,
  then flips the parser, so a migration is a mechanical diff a reviewer can read.
- **Nothing in this program changes concept storage**, row authorization, the work
  spine's concepts or the wire protocol; where a generated SDK method or a shape key
  changes name, `memqlbreaking` says so and the SDKs regenerate in the same PR.
- **The language package owns the language.** `component/language` holds the parser,
  the registry, the tiers, the functions and the generated grammar; `component/memql`
  lowers and evaluates; `component/automations` executes. No vocabulary lives outside
  the first.
- **The editor is part of every grammar change.** A PR that moves the grammar version
  or the edition carries the regenerated extension assets and its version bump, or the
  parity gate refuses it (D25).
- **Values, not constants.** The depth cap, the budgets, the deprecation window, the
  cost budget and the retention of deprecated-use counters are manifest values with
  the defaults this record names.

## 6. Failure modes of the program

- **A migration that changes meaning silently.** The differential lane and the
  `evaluate` verdicts in the corpus catch a filter that lowers differently after a
  rewrite; the `bodies` rewrite leaves a comment at every reordered statement.
- **Static loop analysis that cries wolf.** Conservative edges are named with the
  filter that could not be read; `@loop` is first-class; the epic measures the false
  positive count over the in-repo tree before flipping the refusal on.
- **A matrix that rots.** It is generated and gated; a hand-maintained table is the
  thing being removed.
- **A corpus that becomes a burden.** Cells are small files with one verdict each; the
  fuzz corpus grows only from found failures; scenarios are the automations the
  product already ships.
- **An edition front end that forks the core.** One AST and one compiler; the edition
  selects a parser table, nothing below it.

## 7. Testing

Every epic's tests are named in its section. Program-wide: the corpus runs in
`make test`; the differential lane runs in db-tests and is required; the
completeness gates, the generated-matrix pin, the refusal-wording pins and
`memqlbreaking` run in the required set; every product bundle's CI runs the corpus'
loader over its own tree.

## 8. Delivery

| # | Epic | Depends on |
|---|---|---|
| 1 | Foundations: the language line, editions, the registry, the corpus scaffold | nothing |
| 2 | The expression language | 1 |
| 3 | The body language | 2 |
| 4 | The attribute cleanup | 1; may run beside 2 and 3 |
| 5 | Loop protection | 3 |
| 6 | The freeze | all |

One PR per epic; the first epic's plan files the issues under `claude` and
`epic:<name>`, one epic issue and its task sub-issues each, and records their numbers
here. Plans are written by the session that picks the epic up and deleted in the
epic's merge. The recording-and-learning program's construct-authoring epics schedule
after epics 2 and 3 land.

## 9. Out of scope

- New construct kinds.
- Concept storage, row authorization tiers and the wire protocol.
- The OS beyond what the Sense service and the generated docs need.
- Product-specific bundle content.
- The prompt template language, which stays a template language with its load gate.

## 10. Facts to re-verify before starting

- That `work.UnionFootprint` and `work.CheckCeilings` still have no production caller.
- That `=>` is used nowhere in `dsl/` except the terse automation header.
- The exact set of attributes each allow-list accepts, against the registry, before
  the retirement rewrite is written.
- Whether the OS reads `@displayCard` or `@composable`.
- Which product bundles exist and their CI lint against the engine.
- The eight evaluators' entry points, since a peer may have consolidated one.

## 11. Sources

Cited by title and identifier; the research lanes' full reports are in the brainstorm
session that produced this record.

- Google. Common Expression Language: language definition, overview, and the 2024
  design note on subsetting and translation to SQL; the CEL conformance suite.
- Kubernetes documentation: CEL cost limits; API deprecation policy.
- JMESPath specification; jq manual; JSONata documentation.
- Open Policy Agent: Rego language and the `default` keyword.
- CUE documentation; Dhall safety guarantees.
- Shute et al. SQL has problems, we can fix them: pipe syntax in SQL. VLDB 2024.
- GraphQL specification, October 2021: input coercion.
- Firebase Security Rules language reference.
- Microsoft Learn: expression trees; Entity Framework Core client evaluation and the
  3.x breaking-change record; SQL Server nested triggers.
- Drools reference: `no-loop`, `lock-on-active`, agenda groups, property reactivity.
- PostgreSQL trigger recursion and `pg_trigger_depth`; Oracle mutating-table error.
- Salesforce architect decision guide for record-triggered automation; Apex trigger
  depth limit.
- AWS Lambda recursive-invocation detection; EventBridge troubleshooting guidance.
- Azure Durable Functions eternal orchestrations; Confluent Kafka Streams
  architecture; Home Assistant automation modes.
- Arkency; Rails Event Store: correlation and causation ids.
- Go 1 compatibility promise; the Go toolchain and `go` directive; Go 1.22 release
  notes; the `go fix` proposal.
- Rust RFC 2052 (editions); the Rust 2024 edition guide on reserved keywords.
- Dart language evolution and per-library version override; Swift 6 language modes.
- Buf breaking-change rules.
- Terraform v1 compatibility promises and pre-1.0 upgrade guides.
- PEP 387: backwards compatibility policy.
- JSON Schema test suite; graphql-cats; SQLite testing overview; the Go `test/`
  directory driver; the WebAssembly spec interpreter README; toml-test; yaml-test-suite.
- McKeeman. Differential testing for software. Digital Technical Journal, 1998.
- Go native fuzzing; Hypothesis grammar-based generation; SQLancer.
- Wang et al. Grammar prompting for domain-specific language generation. NeurIPS 2023.
  arXiv:2305.19234.
- Ugare et al. SynCode. 2024. arXiv:2403.01632. Park et al. Grammar-aligned decoding.
  2024. arXiv:2405.21047.
- A survey of large language models for domain-specific languages. TOSEM 2025.
  arXiv:2410.03981. Plan with Code. 2024. arXiv:2408.08335. Anka. 2025.
  arXiv:2512.23214.
- Anthropic: defining tools; writing tools for agents. OpenAI: function calling guide.
- Not the silver bullet: LLM-enhanced programming error messages. 2024.
  arXiv:2409.18661. RustAssistant. ICSE 2025.
