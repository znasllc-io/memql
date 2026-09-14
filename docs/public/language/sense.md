---
title: MemQL Sense & the DSL Spec
audience: public
status: stable
area: language
sinceVersion: 0.9.0
owner: znas
---

# MemQL Sense & the DSL Spec

MemQL Sense is the language-intelligence service for `.memql` files:
tokenize (syntax highlighting), complete (context-aware
autocompletion), diagnose (errors and warnings), hover (symbol info),
signature help, and go-to-definition. It is what colors and assists the source you read
in the memQL [VS Code extension](vscode.md).

Sense has **one consumer** today and **two delivery paths** into it:

1. The **VS Code extension** (see [MemQL in VS Code](./vscode.md)), over an
   offline language server (`cmd/memql-lsp`) that embeds this package and reads
   `.memql` from disk with no cluster and no auth. This is the normal path.
2. The gRPC surface on `MemqlService.Stream`, which serves the same brain to a
   client holding a live cluster connection.

Both drive the same package -- a delivery mechanism is not a fork of the brain,
and neither path changed a wire contract.

> The Cockpit Editor was the second consumer until the Cockpit's TUI was
> removed (memql#4550). The gRPC surface stayed: it is the contract, not the
> caller, and removing it because its only caller went away would delete the
> path a future one would need.

This document covers two things and how they relate:

1. **The DSL spec (`dslspec`)** -- the single machine-readable source
   of truth for the MemQL authoring surface, from which Sense is driven
   and which any editor can fetch as portable JSON.
2. **MemQL Sense itself** -- the language operations, how their
   completion / hover / diagnose behaviour is projected from the spec,
   and the CI drift guard that keeps the spec honest against the live
   grammar.

---

## The DSL spec: one source of truth

`component/language/dslspec` is the single machine-readable source of
truth for the MemQL DSL authoring surface:

- the **top-level constructs** an author may write (`concept`, `query`,
  `mutate`, `logic`, `automation`, `action`, `capability`, `spec`,
  `trait`, `shape`, `tool`, `prompt`, `provider`, `builtin`, `policy`,
  `rule`, `seed`, `use`), each tagged with its category, its body
  sub-blocks, and whether its signature names a bound concept;
- the **keywords**, **operators**, and **field types** the grammar
  accepts;
- the **annotations** each construct allows;
- the **legal-next rules** that drive context-aware completion.

### Why it exists

Before `dslspec`, Sense carried its completion / hover / diagnose
tables as hand-maintained Go literals. They drifted from the grammar
with nothing to catch it -- they still listed the retired `has`
operator, the deprecated `array(T)` field type, and the
receiver-function model, while omitting live struct-form constructs
(`logic` / `trait` / `policy` / `seed`). `dslspec` is the durable fix:
ONE spec that Sense is generated and driven from, that a CI drift test
pins against the parser and annotation registry, and that exports as
portable JSON for the Cockpit and any future editor.

### Source-of-truth boundaries

The spec is deliberately split on where each fact already lives:

- **Annotations and their per-construct legality are DERIVED**, not
  re-listed. They come from `component/language/annotations` -- the
  registry that already backs the load-time gate. `dslspec` inverts
  that registry into a per-annotation view (name to doc plus the
  construct keywords it is legal on), so the spec cannot disagree with
  the engine's own enforcement.
- **Operators and the expression functions are projected** from the
  function catalog (`component/language/functions`): each operator row
  of `functions.Operators()` -- its form, what it does, its absence rule
  and its precedence level -- and each function and method of
  `functions.Catalog()`, with its signature and tier. Sense's hover
  cards and the generated docs read the same two tables, so an operator
  or a function is described once.
- **Constructs, keywords, field types, and legal-next rules** have no
  pre-existing registry -- the truth was split across the parser's
  top-level dispatch and the struct-form rewriter. `dslspec` is the
  source of truth for those, and a drift test introspects the parser and
  rewriter to assert the spec stays in lockstep.

The package is a near-leaf: it imports only the annotations registry
and the function catalog, so both Sense and the gRPC/SDK export layer
can consume it without an import cycle.

### The workspace graph (cross-reference resolution)

The spec answers "what is legal *here*" from the grammar; it cannot
answer "does `fylo.concepts` resolve" or "is `order` declared there".
Those are cross-reference questions about the whole `.memql` tree, and
Sense answers them from a second source: a **workspace graph** built
from the `dslimports` tree -- the same file/tree resolution `memqllint`
uses -- and handed to the service alongside the registry
(`sense.WorkspaceGraph`; adapter in `component/memql/workspace_graph.go`,
built by `BuildOfflineSense`). The registry is a *flat vocabulary*
(concept/function/shape names); the graph is *structured* (namespaces,
kinds, per-module declared ids) -- the input import and reference
diagnostics and segment-aware `use`-line completion consume.

Every graph answer is tri-state (`ResolvedYes` / `ResolvedNo` /
`ResolvedUnknown`). The third state is load-bearing: a product bundle
imports engine namespaces it does not carry (`common`, `platform`,
`identity`), and the edit-path registry can be stale between saves, so a
fact the workspace cannot *prove* is `Unknown` and callers stay silent
rather than emit a false squiggle -- mirroring the load-side verifier's
own conservatism (`dslimports.missingIsProvable`). The graph is built
from the file tree independently of engine boot, so it survives a
workspace with a broken reference -- exactly when reference resolution
is needed most.

---

## MemQL Sense: the language operations

Sense is exposed via gRPC on `MemqlService.Stream`. The core is pure
Go (`component/memql/sense/`, no gRPC dependency); the gRPC handlers
live in `component/grpc/sense_handlers.go`.

| Operation | What it returns |
|---|---|
| **Tokenize** | Semantic tokens for syntax highlighting -- keywords, identifiers, strings, annotations, concept ids, and the operators (`\|\|`, `.?` and `=>` each one token) |
| **Complete** | Context-aware autocompletion -- constructs, annotations, concepts, builtins, keywords; at an expression position, exactly what the tier manifest admits there (see [Expressions](#expressions-position-aware-completion-and-hover)) |
| **Diagnose** | Errors and warnings from the lexer, parser, and semantic validation |
| **Hover** | Symbol info at the cursor -- function docs, concept schemas, annotation docs, tool and prompt docs, and cards for operators, catalog functions and retired spellings that say where the item runs at the cursor's position. Resolves a BARE concept short name too (#2753): `candidate` in `shape candidate candidateFull` is ambient under rule 25, so it is matched by trailing segment against the registry. A collision across namespaces (`invocation` is both `v1:worker:invocation` and `v1:observability:invocation`) is broken by the document's own domain; where that cannot decide, hover returns nothing rather than the wrong concept. The domain comes from the document path, carried by `SenseHoverMsg.file_path` on the gRPC surface (#2760) and by the document URI over LSP |
| **SignatureHelp** | Parameter help inside call arguments, from the function catalog first, then the builtins and the registry's declared args |
| **Definition** | Go-to-definition (F12) -- resolves the construct reference under the cursor to the file and position that declares it (#2754). Backed by `dslimports.Index.DeclarationSites`, which finds the declaring file from the declaration index and recovers the line/column by re-lexing that file's raw source (the AST carries no positions). Colliding names are narrowed by the referencing file's own domain, and where that cannot decide it returns nothing rather than jumping to the wrong file. Exposed on both surfaces: `textDocument/definition` over LSP and `SenseDefinitionMsg` / `SenseDefinitionResult` over gRPC (#2760). The result carries a WORKSPACE-RELATIVE path, never a URI -- the LSP maps it to `file://` while the Cockpit addresses pack files as `(domain, path)`, so the wire stays neutral between them |

### Driven from the spec

Sense's completion no longer hardcodes its tables. At startup it builds
the spec once (`dslspec.Build()`) and projects it into the lookup
shapes completion needs:

- **Top-level construct completion** is the spec's construct list. Typing
  `mut` at the file top now offers `mutate`; the full struct-form set
  (`logic` / `trait` / `policy` / `seed`) is present because the spec
  carries it. This replaced the stale hand-coded `func / use / concept`
  set.
- **Annotation completion** filters by the receiver the cursor sits in,
  using the same registry projection the load-time gate enforces -- so
  Sense never offers an annotation the engine would reject.

Because both the keyword set and the import behaviour are read from the
spec rather than hardcoded, the drift test keeps them honest.

### Context-aware completion

Two behaviours are worth calling out because they come straight from
the spec's `ConceptInSignature` flag and `SuggestImportWhenMissing`
legal-next rule:

- **Concept-after-construct.** Concept-binding constructs name their
  bound concept in the signature: `mutate <Concept> <name>`,
  `query <Concept> <name>`, `seed <Concept> <name>`, and the `@row`
  form of `shape <Concept> <name>`. Right after the keyword, completion
  suggests a concept (filtered by the partial prefix), with
  registry-known concepts offered first.
- **Import suggestion.** When the spec's legal-next rule for that
  construct sets `SuggestImportWhenMissing`, any matching concept that
  is NOT already in file scope (no `use <domain>.concepts.{ Concept }`
  line in the source) ALSO gets an "import" completion whose insert
  text prepends the missing `use ...concepts.{ Concept }` line -- so
  authoring a fresh file pulls the concept into scope in one keystroke.
  Concepts already imported are suppressed from the import set (the
  bare concept suggestion stands).
- **Segment-aware `use` line (#2732).** An import is `use <ns>.<kind>.{ id,
  ... }`, and each segment now completes against the [workspace
  graph](#the-workspace-graph-cross-reference-resolution): after `use ` the
  namespaces; after `use <ns>.` the module kinds (`concepts` / `queries` /
  `mutations` / ...); inside the brace list the ids that module declares, minus
  the ids already listed. This replaces the old behaviour where typing `fylo.`
  fell through to the offer-everything body bucket and dumped the whole symbol
  table -- the classifier now claims the braced id list before it can be
  mistaken for a function body. Backed by the graph, so it needs no registry.
- **Kind-filtered invocation (#2733).** In a behavioral body an invocation is
  `<verb> <name>(...)` where the verb -- `query` / `mutation` / `logic` -- names
  the kind it can bind. Completing the name now offers only functions of that
  kind (a `query <name>` position lists queries, never mutations, logic, or
  tools), instead of dumping the whole registry. The verb requires body depth,
  so the top-level `query <Concept> <name>` declaration is unaffected.
- **Reference positions that name a construct (#2740).** A concept's
  `@relationship(... target=<cursor>)` offers concepts, in the form the slot
  uses: the common bare `target=user` binds the SHORT name, and the quoted
  `target="v1:ns:leaf"` binds the canonical id -- and only at the `target=`
  value, never at `type=` / `field=` / `direction=`. Both are matched from the
  raw text before tokenizing (the quoted form's unterminated string zeroes the
  lexer's tokens), confined to a single well-formed annotation call so an
  unclosed `@relationship(` elsewhere cannot poison a later `target=`.

---

## Expressions: position-aware completion and hover

Every expression in a `.memql` file sits in one of the twelve positions the
tier manifest names (`component/language/tiers`), and the position decides
what the expression may contain: a query filter is pushed down to SQL, a logic
body runs in process (see [Where each expression
runs](memql.md#where-each-expression-runs)). Sense works out the position
under the cursor, then completes and hovers from the manifest and the function
catalog (`component/language/functions`), so the editor offers what the load
accepts there and nothing it refuses.

### Finding the position

Sense reads the position from the source text, not from a parse
(`component/memql/sense/exprpos.go`): a buffer is mid-edit and unparseable
most of the time, and a detector that needed a parse would go quiet exactly
while an author types.

| Position | The cursor is in |
|---|---|
| `queryFilter` | a query's `filter row => ...` clause and its continuation lines, or the query of a tool's `@handler(query="...")` |
| `queryRefine` | a query's `refine row => ...` clause |
| `sort` | a query's `sort` clause |
| `specBody` | the body of `spec <bound> <name> = row => ...` or `trait <name> = row => ...` |
| `triggerFilter` | an automation's `@filter(row => ...)` |
| `rowAuthzArgument` | an argument of `@rowAuthz(...)` |
| `toolDefault` | a tool field's `@default(...)` |
| `promptInput` | a prompt field's `@default(...)` |
| `automationCondition` | an `if`, `else if`, `for`, `forEach ... where` or `switch` head, or a precondition's `check:` |
| `stepArgument` | the arguments of a kind-prefixed call in an automation (`mutation createTodo(title: ...)`), or a `name := ...` statement there |
| `logicBody` | a statement in a logic's `body { }` |
| `mutationValue` | a `key: <value>` line in a mutation's `insert`, `update` or `stamp` block |

A line continues a clause when a bracket opened above is still open, when it
opens with a binary operator (`&&`, `||`, `==`, `?`, `:`, ...), or when the
line above ended on one.

With the position come the lambda parameters in scope and the concept the
position's parameter reads. `filter row =>` and `refine row =>` bind `row` to
the query's concept; a spec binds the concept or shape in its signature, and
over an @actor shape its parameter is spelled `actor` and is the envelope;
`@filter(row =>` binds `row` to the concept named by the automation's
`@trigger(concept="...")` or by its `graph.node.*` event topic. The parameter
is whatever name the author wrote. A lambda opened inside the expression --
`row.tags.any(t => ...` -- puts its parameter in scope until the bracket it
sits in closes. The pre-v1 spellings of the pushdown positions (a filter with
no lambda header, a `filter { }` block, a `spec ... { return ... }` body) are
detected too, so a file that has not been migrated keeps its completion.

### Completion

At an expression position completion offers what the manifest admits there,
and nothing else. The three literal positions -- a sort key, a row-authz
argument, a tool default -- keep the annotation's or clause's own completion,
since each takes a literal. Everywhere else the list is:

- The lambda parameters in scope, innermost first, then the names that
  statements above the cursor bound in the same body
  (`rows := query activeUsers(status: "active")`), whose detail is `local`.
- The reserved roots the position evaluates with: `args`, `actor`, `now` and
  `config` in a query filter, a refine clause, a logic body and a mutation
  value; `now` in a spec body; the run's `event`, `steps`, `item`, `index`
  and `input` as well in an automation condition and a step argument. In a
  trigger filter `args` is the automation's declared args, bound from the
  triggering event's payload before the filter runs, and `args.` completes the
  args of the automation the annotation decorates -- declared below the
  cursor.
- The catalog functions the position admits. In a query filter or a spec body
  an in-process function is still offered, because
  `row.expiresAt < addDuration(now, "P1D")` is legal, but its detail reads
  "Computes before the query; cannot read the row" and it sorts last.
- Specs and traits, applied to the receiver they read: `isActiveRecord(row)`,
  `requiresOwner(actor)`. A row spec bound to a different concept than the
  position's is not offered.
- `nil`, `true` and `false`; the construct-call verbs (`query`, `mutation`,
  `logic`) only in a logic body and a step argument; the statement keywords
  (`if`, `for`, `return`, ...) only at the start of a statement in a logic
  body.

After a dot, completion offers members. `row.` offers the bound concept's
fields, each with its type, then the row intrinsics (`id`, `createdAt`, ...);
`actor.` in an actor spec offers the envelope. `row.tags.` offers the methods
of the field's type -- in a query filter or a spec body only the ones that
push down (`any`, `all`, `count` on a list), because an in-process method over
the row is a load refusal. `args.tags.` offers every method the position
admits, since an arg does not read the row.

The two clauses that open with a lambda header offer the header first. A
filter with no header yet offers `row => ...` beside the reserved heads and
the bound concept's fields of its pre-v1 spelling; a refine clause offers
`row => ...` and nothing else until the header is written.

No retired spelling is ever offered: `cond`, `concat` and `coalesce` do not
reach the list anywhere.

### Hover

Hover over an operator, a catalog function or method, or a retired spelling
shows a card with one shape, plain text in sentence case:

1. A `memql` code block with the signature (`lower(value string) string`), the
   operator in use (`a ?? b`), or -- for a retired spelling -- its
   replacement.
2. One sentence saying what it does: the first sentence of the catalog entry,
   or the operator's description followed by its absence rule.
3. Where it runs at the cursor's position: "Pushed down to SQL.", "Runs in
   process.", "Runs in process before the query, on values that do not read
   the row.", or, in a refine clause, "Runs in process, over the rows of the
   page the query read."
4. A legality line, only when the position refuses or restricts the item,
   naming the fix in one sentence: "Not allowed on the row in a query filter:
   `lower` runs in process. Compare the row against a computed value:
   `row.email == lower(args.email)`." A call whose arguments demonstrably do
   not read the row is a plan constant, legal where it stands, and gets no
   legality line.
5. For an entry that replaces retired spellings, the ones it replaces:
   "Replaces `len(x)` and `count(x)`."

Operator cards come from the one operator table, `functions.Operators()`. The
same glyphs outside an expression keep their own meaning and get no operator
card: `in` in a `forEach` head is the loop keyword, the terse automation's `=>`
is not a lambda, and a map key's `:` is not the conditional's.

A retired spelling's card reads the parser's own refusal table,
`parser.V1RetiredForms()`, by its stable rule ids, so the hover and the load
refusal cannot say different things: "`cond(p, a, b)` is retired in edition
2026. Write `p ? a : b` instead; `memqlmigrate --rewrite=expressions` rewrites
it." `when(`, `?.`, `;` as a connective, `has`, `not in`, `null`, `$args.x`,
`spec <name>` inside an expression, `.contains(...)` and every retired
function called by name are hovered this way. `contains` with one argument is
the live traversal and `count` alone on a line is the query's count clause, so
neither is flagged. `TestRetiredHoverIsTheParsersTable` fails when the parser retires a
form Sense neither hovers nor names as unhoverable; the comma connective is the
one it names, because a comma also separates arguments.

### Signature help

Inside a call's arguments signature help reads the catalog first: one
signature per function or method, including the relationship traversals, whose
optional leading `as` label is marked optional. The highlighted parameter
skips the label when the call leaves it out. Builtins the catalog does not
describe come next, then the registry's queries, mutations and logic, each
signature built from its declared `args { }`.

---

## Edit-time diagnostics

Diagnose runs parse errors plus a set of authoring rules over the
authored source. The load-mirroring rules carry stable code strings so
editors and agents can key on them:

| Code | Severity | Rule |
|---|---|---|
| `actor-unknown-property` | Error | `actor.<member>` names something outside the closed envelope (#2625) -- same tables as the load-time gate, pinned by a conformance test. |
| `actor-undeclared` | Error | A query/mutate/logic/automation body reads `actor.*` without `@actor` in the preamble -- the edit-time mirror of the engine's load rule (#2621), sharing the loader's own detection so squiggle and boot error cannot drift. |
| `unknown-import-module` | Warning | A Form-B import `use <ns>.<kind>.{ ... }` whose kind segment names no module in a workspace-owned namespace (`use fylo.concept.{...}` where the module is `concepts`). Resolved against the workspace graph (#2730). |
| `unknown-import-symbol` | Warning | An imported id that the resolved module does not declare (`use fylo.concepts.{ oder }`). |
| `signature-binds-wrong-kind` | Error | A `query`/`mutate`/`shape`/`seed` signature binds a name that IS declared, just not as a concept -- `shape todos ...` where `todos` is a query (#2762). The sibling rule below only asks whether the name exists at all, so a wrong-kind binding sailed through and surfaced as a boot failure instead. An explicit import does NOT suppress it: importing the query is exactly how the author got here. A name the workspace has never seen is left to `unknown-signature-concept`, since it may arrive at runtime via `MEMQL_DSL_PATH`. `spec` is out of scope by construction -- it binds a shape XOR concept, and the extractor covers only the four concept-binding keywords. Measured over `dsl/`: 680 signature bindings, zero flagged |
| `unknown-signature-concept` | Error | A `query`/`mutate`/`shape`/`seed <Concept> <name>` whose bound concept exists nowhere and is not imported (`mutate full ...` with no concept `full`). Error, because boot itself CrashLoops on an unresolvable signature concept. Extracted with the boot-pinned regex (`dslimports.SignatureConceptRefs`) and resolved with the load side's own `missingIsProvable` conservatism, so an external or unimported-but-global concept is never flagged (#2731). |
| `bare-row-intrinsic` | Warning | A filter still in its pre-v1 spelling (no lambda header) names a row intrinsic bare (`filter id == args.x`) instead of through the `row.` namespace (#2779). A v1 filter reads every field through its parameter (`filter row => row.id == args.x`), so it never trips the rule. Warning, not Error: the engine still resolves bare intrinsics correctly -- only the authoring gates retired the spelling -- so this never CrashLoops boot, though `test/dslconformance/conformance_test.go` does fail CI on it. Detection is the same `sense.ScanBareRowIntrinsics` the tree-wide gate calls, so squiggle and CI cannot disagree; it reads clause TEXT rather than parsed predicate structure, so `\|\|`-joined and parenthesized predicates are covered, and string-literal contents are excluded. |
| `bare-row-intrinsic-sort-key` | Warning | A sort key names a row intrinsic bare (`sort "createdAt", "desc"`) instead of through the `row.` namespace (#2786) -- the ordering half of the rule above, with the same ambiguity (`sort "id"` can name the row id or a payload property called `id`) and the same Warning rationale. Detection is the same `sense.ScanBareRowIntrinsicSortKeys` the tree-wide `TestSortKeysUseRowNamespace` calls. It is a SIBLING of the filter scanner, not a branch inside it: a filter names fields as code (so that scanner blanks string literals) while a sort names them as string literals, so this one reads literal contents. It opens a clause only on `sort` followed by a string literal, which keeps a construct field of the form `sort string @enum("createdAt", ...)` from being read as a sort clause; the whitespace skip is unicode-aware so it agrees with the rewriter's `TrimSpace`. It does NOT separate a provider `params` entry spelled `sort "createdAt"` from a directionless sort clause -- the two are byte-identical, and telling them apart needs enclosing-construct state the scanner does not carry. |
| `redundant-enabled` | Hint | `@enabled` restates the default (#2610). |
| `redundant-version` | Hint | `@version("1.0.0")` restates the default (#2613). |

There is no `discarded-args-description` rule any more (memql#3336). `@description` on an args field is now REJECTED by the parser, so a file carrying one never parses and the editor gets the load error itself through the `parse-error` path -- the same way every other rejected form surfaces (`@default` on an args field, `@useConcept`, `func (Query)`), none of which carries a bespoke rule either. The retired Hint said the annotation was "parsed and discarded", which stopped being true, and it lived in the semantic phase, which does not run when the parse fails.

The two import checks resolve against the [workspace graph](#the-workspace-graph-cross-reference-resolution) rather than the flat registry, and they run even in the registry-less fallback (a workspace whose engine boot tripped) because the graph needs no registry. They are deliberately conservative: a reference the workspace cannot *prove* wrong -- an id under an external engine namespace the bundle imports but does not carry, or anything when the graph is absent -- resolves inconclusively and is left silent, so a legitimate cross-namespace import never squiggles. That is why they are Warnings, not Errors.

### Snippet completions (#2629)

Completion items can carry LSP snippet syntax. `CompletionItem` has an
`IsSnippet` flag; the LSP layer maps it to
`InsertTextFormat=Snippet`, and every item now declares its format
explicitly -- snippet syntax sent WITHOUT the flag inserts literally,
dollar signs visible in the buffer, which is why the flag exists
rather than sniffing the text.

What ships as a snippet:

| Snippet | Where |
|---|---|
| `args { ... }`, `insert { ... }`, ... | Inside a construct body -- opens the block with the cursor inside, offered beside the plain block keyword. |
| `filter row => row.field == "value"` | Inside a query body. A v1 filter is a clause whose value is a lambda over the row, not a block, so its snippet writes the header. |
| `query` / `spec` / `trait` / `mutate` / `logic` / `automation` / `concept` skeletons | Top level -- full declarations with tabstops at the names, sorted BELOW the bare construct keyword so they never displace it. The `spec` and `trait` skeletons are `spec <Concept> <name> = row => ...` and `trait <name> = row => ...`. |
| `@filter(...)` | Where the construct takes `@filter` -- inserts `@filter(row => row.field == "value")`, a lambda over the triggering row. |
| The `use <domain>.concepts.{ X }` import | Now places the cursor after the bound name (it was multi-line plain text before -- correct, just cursor-less). |

Consumers without snippet support (the Cockpit gRPC path) call
`PlainInsertText()`, which collapses placeholders to their default
text and drops tabstops. Generated snippet text escapes literal `$`,
`}`, and `\` so it cannot be misread as a tabstop or a placeholder
terminator. The `IsSnippet` flag is mirrored in the proto envelope and
the Go SDK, with a wire round-trip test.

### Block-specific completion (#2628)

Inside a named block, completion offers THAT block's set. The block
labels have been declared in `nextrules.go` since the table was
written, but a label is not a detector -- a NextRule has zero effect
until the classifier computes its label, which is what this closes:

| Block | Offers |
|---|---|
| `args { }` | Field types (`string`, `bool`, `enum(...)`, ...). `enum` completes to the TYPE form (#2618); the `!` required sigil is documented on the item rather than offered as one. |
| `filter { }` (pre-v1) | The engine's reserved filter heads (`payload`, `actor`, `args`, `now`, `config`, `trace`, `meta`, `schema`, `partition`, `provenance`) plus the bound concept's fields. A v1 filter is a clause, and completes as an expression position (see [Expressions](#expressions-position-aware-completion-and-hover)). |
| `insert { }` / `update { }` | `accept` / `stamp` in the post-#2616 short form, plus the bound concept's fields. |
| `shape { }` | The bound concept's fields. |

The editor teaches the POST-epoch surface deliberately: offering the
superseded long forms (`@required`, `string @enum(...)`, `@cache(ttl=)`
-- the last of which still parses, but `@cache(N)` is the form the corpus
uses)
here would undo the grammar-epoch migration, and a conformance test
asserts no completion item's insert text carries one. A second gate
requires every declared NextRule label to be either computed by the
classifier or explicitly listed as not-yet-detected, so a rule added
to the table can never sit silently dead again.

### Receiver-filtered and construct-scoped completion (#2627)

With the enclosing construct resolved, completion is construct-aware
in the two places it matters:

- **Annotations.** `@` offers only the construct's legal annotations,
  mapped keyword -> `Construct.AnnotationReceiver` -> the registry's
  receiver list. Three edges: the CONCEPT construct's receiver key is
  `""` (a real key -- not "unresolved"), an unbacked construct (today
  exactly `use`) falls back to the union rather than offering nothing,
  and no detection at all (EOF) keeps the union. A registry-driven
  conformance test asserts that every annotation offered for every
  construct is legal for that receiver, so the never-offer-what-the-
  engine-rejects contract survives future registry edits.
- **Body blocks and statements.** A body offers ITS construct's
  `BodyBlocks` from the spec (query: args/filter/shape; mutate:
  args/insert/update; logic and automation: args/body) and only its own
  invocation verbs -- a logic body never offers `filter`, a shape body
  never offers `insert`.

### The context model

Every completion request resolves ONE enclosing-construct answer
(#2626): a backward walk over the token stream pairs each still-open
brace with the header that opened it, and the innermost frame whose
label is a dslspec construct keyword is the enclosure. It yields the
construct keyword, its annotations-registry receiver, the declared
name, and the chain of enclosing named blocks (`args`, `insert/stamp`,
`step`, `body`, ...). A top-level `@` is the one inverted case -- it
PRECEDES its construct, so the receiver comes from the next header
below the cursor, and at EOF there is none (the completer falls back
to the union of every receiver's annotations).

The lowercase construct keywords lex as plain identifiers (only
`concept` and `use` are lexer keywords), so headers are matched by
identifier literal against the spec's construct set -- a construct a
future grammar epic adds is recognized automatically.

Populating the receiver is what finally engages the per-receiver
annotation filter: `@` above a query no longer offers mutation-only
annotations like `@mergeFields`.

### Member completion (#2624)

Typing a dot after a known accessor root completes ONLY that root's
members -- a dot context never offers keywords, builtins, functions,
or concepts, and an unknown root offers nothing:

| Root | Members offered |
|---|---|
| `actor.` | The canonical auth envelope from the dslspec property table (#2623), aliases sorted after canonical members. |
| `event.` | The event envelope: `topic`, `kind`, `payload`, `actor`, `timestamp`. |
| `event.actor.` | `id` only -- the emitter's identity stamp (G4), a different object from the auth envelope. |
| `args.` | The enclosing construct's declared args fields (any function kind; the automation BARE-name completion shares the same declared-field scanner, so the two can never disagree). In a trigger filter, which is written above its automation, the fields are the args of the automation the annotation decorates. |
| `row.` (the position's lambda parameter) | The bound concept's fields, each with its declared type, then the row intrinsics. `actor.` in a spec over an @actor shape offers the envelope. |
| `payload.` | The enclosing construct's bound-concept fields (via the registry's concept projection) -- the pre-v1 spelling; a v1 expression reads `row.<field>`. |

Both lexer shapes are detected: a trailing dot (`actor.`) and a
mid-member position (`actor.us`, where the prefix filters).

## The CI drift guard

`dslspec` is only useful if it stays true to the live grammar. A CI
drift test (`component/language/dslspec/drift_test.go`) is that pin: it
FAILS when the live grammar (the parser's top-level dispatch plus the
struct-form rewriter) or the annotations registry moves ahead of the
spec.

The test imports the parser and the annotation registry; production
`dslspec` code imports the parser only for the two labels naming the
language (`Edition`, `GrammarVersion`, memql#5362), and the parser
imports nothing from `dslspec`, so neither creates an import cycle. It
compares the spec's construct set
against the parser's own authoritative, introspectable lists
(`parser.TopLevelDeclKeywords`, `parser.StructFormKeywords`) -- not a
hand-copied literal -- and names the exact drifted symbol on failure
(for example "add X to dslspec constructs()" / "remove Y"). Add a new
construct to the grammar without adding it to the spec, and this test
goes red.

The `SpecVersion` constant (`1.1.0`) versions the JSON envelope shape,
not its contents: a new construct or annotation does NOT move it. A
backward-incompatible change to the envelope moves the major, so a
consuming editor can detect a contract mismatch; an additive one moves
the minor (`1.1.0` added `edition` and `grammarVersion`).

### The syntax&lt;-&gt;Sense parity gate (#2734)

Most of Sense's vocabulary is projected from the spec at runtime, so it
cannot drift. But a few surfaces added for cross-reference resolution
(the invocation kind-filter, #2733) keep HAND-MAINTAINED tables that key
on construct keyword or invocation verb -- `behavioralConstruct` /
`nonBehavioralConstruct` / `invocationVerbKind` (`context.go`) and
`invocationKeywordsForConstruct` (`complete.go`). A conformance gate
(`component/memql/sense/parity_test.go`) pins them to the same SoT:

- Every dslspec construct must be classified EXACTLY once as behavioral
  (its body invokes other constructs) or non-behavioral -- a construct
  added to the grammar lands in neither set and fails, naming the file
  to update.
- The two invocation tables must agree (a construct is behavioral IFF
  `invocationKeywordsForConstruct` offers the call verbs).
- Every verb in `invocationVerbKind` must be a real invocation keyword
  per `parser.InvocationKindKeywords()`.

**This is how "keep MemQL Sense updated whenever the syntax or rules
change" is enforced:** adding a construct to the grammar, removing or
renaming an invocation keyword, or letting the two invocation tables
disagree fails this gate in CI until Sense is brought back in sync. (It
does not yet force Sense to handle a NEW invocation verb added to an
existing construct -- the verb pin is a one-directional subset check.)
Follows the #2628 detected/undetected-label conformance pattern.

---

## Portable JSON export (for editors)

The whole authoring surface serializes to portable, indented JSON --
the form a Monaco / CodeMirror grammar is generated from. Its top-level
shape is:

```json
{
  "version": "1.1.0",
  "edition": "2026",
  "grammarVersion": "<parser.GrammarVersion>",
  "constructs": [ ... ],
  "annotations": [ ... ],
  "keywords": [ ... ],
  "operators": [ ... ],
  "fieldTypes": [ ... ],
  "nextRules": [ ... ],
  "builtins": [ ... ]
}
```

`edition` and `grammarVersion` name the language the document describes
-- the same two facts `ServerHello` states on connect -- so an editor can
compare the grammar it was built from with the cluster's. A grammar
version is a label: two compare equal or different, never older or newer
(see [MemQL in VS Code](vscode.md) for how the extension orders them).

An editor fetches it over gRPC through the Go SDK:

```go
import "github.com/znasllc-io/memql/sdk/go/dslspec"

client := dslspec.NewClient(dispatcher)
spec, err := client.Fetch(ctx) // spec.JSON (portable JSON), spec.Version
```

`sdk/go/dslspec` is read-only by construction -- there is no write
path. The spec is assembled from the engine's own grammar tables and
annotation registry, so an editor that fetches it derives its language
intelligence from the EXACT same source of truth Sense is driven from.
The `Version` mirrors the embedded `SpecVersion` so a consumer can
check the contract version without parsing the document.

---

## Where this lives

| Piece | Location |
|---|---|
| The spec (source of truth) | `component/language/dslspec/` (`spec.go`, `constructs.go`, `lexicon.go`, `nextrules.go`, `punctuation.go`, `json.go`) |
| The drift guard | `component/language/dslspec/drift_test.go` |
| Sense core (pure Go) | `component/memql/sense/` |
| Sense gRPC handlers | `component/grpc/sense_handlers.go` |
| Spec fetch SDK | `sdk/go/dslspec/` |
| Sense SDK (Tokenize / Hover / ...) | `sdk/go/sense/` |

## See also

- [MemQL Authoring Rules & Gotchas](authoring-rules.md) -- the
  human-and-agent rule list Sense diagnostics surface at edit time.
- [MemQL Language](memql.md) -- the DSL reference.
- [Reserved Identifiers](reserved.md) -- the engine-reserved names.
</content>
