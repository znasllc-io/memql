---
title: MemQL
audience: public
status: stable
area: language
sinceVersion: 0.9.0
owner: znas
---

# MemQL

> **Last Updated:** September 13, 2026

MemQL is the query and mutation language that powers the memory engine. It provides a deterministic, append-only interface for reading and writing concept-backed data stored in TimescaleDB. This document is the canonical reference for MemQL behavior. **Whenever the query language changes or new capabilities ship, update this guide alongside the code change.**

## When to Use MemQL

- Retrieving concept instances (agents, artifacts, plans, etc.) with filterable JSON payloads.
- Traversing graph-like relationships — parent/child hierarchies, containment, aliasing, ownership, and provenance edges (see [Relationships](#relationships)).
- Inserting new immutable records via mutations.

## What you write and what travels

MemQL is written in one place and carried in another, and this guide covers both:

1. **DSL constructs** — concepts, shapes, specs, traits, queries, mutations, logic, builtins, providers, prompts, tools, automations, and policies authored in `.memql` files under `dsl/<namespace>/`. These are loaded at engine startup and are the canonical way to define reusable behavior. Every expression in them is written in the one language [Expressions](#expressions) describes.
2. **The internal query form** — the string passed to `engine.Execute(ctx, query)` (or over gRPC / the WebSocket bridge / MCP): a call to a named query or mutation, a filter string, an `insert(...)` literal, or an introspection meta-command. It is a wire contract an SDK speaks, it keeps its own grammar, and it is not an authoring surface. See [The internal query form](#the-internal-query-form).

Anything that needs a projection (shape), a reusable predicate (spec), or AI involvement is defined in the DSL and *called* from the internal form — a client names a query and passes its arguments rather than composing a filter.

## The language line

Every tree of `.memql` files declares the language it is written in, in a two-line `memql.toml` beside its files:

```toml
memql = "1.0"
edition = "2026"
```

- `memql` is the **language line**, written `<major>.<minor>`. It is what later versions of the language key meaning on: when a behaviour changes between two lines, a tree is read with the meaning of the line it declares, the way a Go module is read under its `go` line.
- `edition` is the coarse label that names the parser front end the tree is written for. Every file is read through the front end of the edition its own domain declares before anything parses it, so domains written in two editions load in one engine. An edition never forks the parser: a front end is a thin source step in front of the one core, and the current edition's step changes nothing. An engine refuses an edition it has no front end for, and a file a front end refuses is refused at boot by path. A top-level statement that opens with a word no construct is spelled with -- a typo, or a keyword only another edition knows -- is refused too, naming the word and the construct keywords (`construct_unknown`).

The file is a strict subset of TOML: blank lines, `#` comments, and exactly those two `key = "value"` lines. A table, a third key, an unquoted value or a key written twice is refused with its line number.

### Where it lives: in every domain directory

**Every domain directory carries its own `<domain>/memql.toml`.** A domain directory is the only thing that reaches a node: a bundle image copies domain directories into `MEMQL_DSL_PATH`, a package deploy stages each domain on its own, and every mount reads domain directories and skips the files at its root. A declaration at the root of a bundle would never arrive, so there is no inheritance from one — a `memql.toml` at the root of `MEMQL_DSL_PATH`, of a bundle `memqllint` lints, or of a package's `dsl/` is never read. `memqllint` reports one as a diagnostic (`language_line_unread`), a package deploy reports it as a warning that does not stop the deploy (`dsl_language_line_unread`), and the `MEMQL_DSL_PATH` mount logs a warning; each says the line belongs inside each domain directory.

```
bundle/
├── storefront/
│   ├── memql.toml
│   ├── concepts.memql
│   └── queries.memql
└── orders/
    ├── memql.toml
    └── mutations.memql
```

The engine's own embedded tree is compiled in as one tree, so it declares its line once, at `dsl/memql.toml`, and every core domain speaks it.

### What the engine refuses

A domain the engine will not read refuses boot, and its concepts are dropped before any of them reaches the database. It is the same strict gate as a construct that fails to parse, so `MEMQL_DSL_ALLOW_SKIPS=1` is the operator break-glass. Each refusal names the file to fix and ends with a stable code:

| Code | When |
|---|---|
| `language_line_missing` | a domain that is not compiled into the engine has no `memql.toml` |
| `language_line_malformed` | the file does not read, lacks one of its two keys, or `memql` is not `<major>.<minor>` |
| `language_version_newer` | the domain declares a newer line than this engine speaks |
| `language_version_unsupported` | the domain declares an older line; this engine reads only `1.0` |
| `edition_unknown` | the engine has no front end for the declared edition |

A newer line reads, for example:

```text
domain "storefront" declares memql = "1.1", newer than the 1.0 this engine speaks: run an engine that speaks 1.1, or declare memql = "1.0" in storefront/memql.toml [language_version_newer]
```

A refused domain is read by no loader at all, so its author sees exactly one refusal per domain rather than everything that reading it under a line it did not declare would produce; fix the line, and the next boot reports what the domain itself holds. `memqllint` runs the same check over a bundle before it ships, and reports each refusal once. Pointed at one domain directory instead of at the bundle (`memqllint bundle/storefront`, or a file inside it), it checks that directory's own `memql.toml` when a mount would read the directory as a domain -- not a sub-namespace, not a `_` or `.` name, not a core domain: a malformed or newer file is refused with its code, and a missing one is a warning, because only the tree the directory is mounted in decides whether it is a domain. Linting the bundle checks every domain exactly as boot does.

### Adding the line

`memqlmigrate` writes the engine's line into every domain that has none, and leaves a domain that already declares one alone — moving a declared line is a decision about the tree, not a migration of it. It asks the loader's own questions, so it also writes nothing for a domain the engine compiles in: that domain speaks `dsl/memql.toml`, and every mount skips a directory named after it. Handed one directory rather than a bundle, it writes the line there only when a mount would read that directory as a domain -- the same rule `memqllint` asks. Run it over a bundle root (or over one domain directory), with `-w` to write in place:

```bash
memqlmigrate --rewrite=language-line -w bundle/
```

Later epics that change the language register their rewrites in the same registry, keyed by edition; the [versioning rule](authoring-rules.md#grammar-versioning-and-the-migration-channel) says when one is owed.

## Version one is frozen

Edition **2026** and language **1.0** are frozen (memql#5385). The freeze is not
a date. It is a set of gates that fail when the authoring surface moves without
saying so, which is what makes it safe to hold all of a product's logic in the
DSL.

What it means in practice:

- **Adding needs no ceremony.** A new construct, annotation, function or clause
  breaks nobody, and `memqlbreaking` never reports one. A command that reports
  every change is a command whose output nobody reads.
- **Removing is a window, then a reservation.** A public form deprecates with a
  load-time warning naming its replacement and the release it stops loading at,
  for at least two minor releases, and every use is counted so the removal rests
  on evidence rather than on a guess about who still writes it. Only then may it
  go -- and its name is reserved forever, so it can never come back meaning
  something else. A bundle still carrying the old spelling would otherwise load
  under the new meaning and quietly do the new thing.
- **The corpus is the language.** `test/conformance/2026/` holds a case per
  construct, attribute, expression position and refusal, and its manifest reads
  `"status": "frozen"`. A bug is not fixed until its case is there.

Three artifacts ship with the engine and are generated from the same tables the
parser reads, so none can describe a language the cluster does not accept:

| Artifact | Page | Served over |
|---|---|---|
| The grammar, in EBNF | [grammar.md](grammar.md) | `memqlGrammar()` |
| Every name and what it means | [vocabulary.md](vocabulary.md) | `memqlVocabulary()` |
| Which annotation is legal where | [attribute-matrix.md](attribute-matrix.md) | -- |

The first two exist to be given to a **model**: the grammar as grammar-in-prompt
or as the grammar a constrained decoder is held to, the vocabulary as what each
name means. A hand-maintained grammar does not fail when it drifts; it teaches a
form the parser refuses, and the failure reads as "the model is bad at MemQL".

Reserved names, which a payload field escapes with a raw identifier, are in
[reserved.md](reserved.md).

## Quick Start

### The DSL Tree

The DSL tree is flattened per construct: every namespace gets one directory under `dsl/<namespace>/`, and within it each construct kind is consolidated into a single `<construct>s.memql` file:

```
dsl/
├── library/
│   ├── concepts.memql      → v1:library:* concept schemas
│   ├── queries.memql       → named queries
│   ├── mutations.memql     → named mutations
│   ├── shapes.memql        → projections
│   ├── specs.memql         → boolean predicates
│   ├── logic.memql         → imperative procedures
│   ├── prompts.memql       → AI prompt templates
│   └── automations.memql   → event/schedule-triggered workflows
├── common/
│   ├── builtins.memql      → Go-backed builtin functions
│   ├── traits.memql        → cross-concept predicate scaffolds
│   └── shapes.memql
├── providers/
│   └── providers.memql     → AI provider records
└── policies/
    └── policies.memql      → AI provider-selection policies
```

Authoring reference skeletons live under `dsl/_reference/` (`_concept`, `_shape`, `_spec`, `_trait`, `_agent`); files whose path starts with `_` are never loaded.

When `MEMQL_DSL_PATH` is unset, the binary reads its baked-in embedded tree. Setting `MEMQL_DSL_PATH=/path/to/dsl-root` reads from disk instead, with per-namespace fallback to the embedded copy — useful for dev hacking, per-deploy patches, and test fixtures.

> **Retired layout.** The old versioned per-construct skeletons (`concepts/v1/...`, `specs/v1/...`, `functions/v1/...`, `shapes/v1/...`, `prompts/v1/...`, `providers/v1/...`, `automations/v1/...`) no longer exist. Constructs live in the flattened `dsl/<namespace>/<construct>s.memql` files described above.

### Basic Query

A query binds a concept in its signature, is declared once in the DSL, and is
called by name. Both live in the same domain directory (`dsl/examples/`), so the
query names the concept with no import:

<!-- corpus: 2026/examples/memql/quickstart/basic-query.memql -->
```memql
/// A world: the top-level container the examples domain is built around.
concept world {
  title   string!
  status  enum("active", "retired")!
}

/// Active worlds, newest first.
query world activeWorlds {
  filter   row => row.status == "active"
  sort     "row.createdAt", "desc"
  paginate 50
}
```

A client calls it in the internal query form, `activeWorlds()`, and gets every active world. MemQL responses use **omission semantics**—fields are only present when they contain data (see [Response Envelope](#response-envelope)):

```
{
  "result": {
    "bundle": {
      "nodes": [
        {
          "id": "v1:examples:world:world-aurora",
          "concept": "v1:examples:world",
          "payload": {
            "title": "Aurora Grid",
            "status": "active"
          }
        }
      ],
      "edges": [
        {
          "type": "contains",
          "fromId": "v1:examples:world:world-aurora",
          "toId": "v1:examples:module:module-foundations",
          "depth": 1
        }
      ],
      "rootIds": ["v1:examples:world:world-aurora"]
    }
  }
}
```

- `result.bundle.nodes` is a flat slice of every memory node touched during evaluation (matching records + relationship expansions).
- `result.bundle.edges` describes the relationships that were traversed. Each edge's `type` is one of the [edge labels](#edge-labels-on-the-wire) — that table is the complete set. Omitted when no edges exist.
- `result.bundle.rootIds` captures the IDs that directly satisfied the query before relationship expansion.
- `result.data` carries shaped output when the executed query carries a shape projection (i.e. a DSL-defined query with a `shape` directive). Omitted otherwise; when shaped, contains one element per root.
- `errors` is omitted on success; on failure, contains an array of structured issues (`code`, `message`, optional `metadata`).

## Response Envelope

MemQL uses **omission semantics**—fields are only included when they contain data:
- Present fields with data = included in response
- Absent/empty/not-applicable fields = omitted entirely

```
// Regular query (no shape) - has bundle, no data
{
  "result": {
    "bundle": {
      "nodes": [...],
      "edges": [...],      // omitted if empty
      "rootIds": [...]
    }
  }
}

// Shaped query (a DSL query with a shape projection) - has data
{
  "result": {
    "data": [...]
  }
}

// Error response
{
  "result": {...},         // may be partial
  "errors": [...]
}
```

**Field semantics:**

- `result.bundle` – Contains the graph structure (`nodes`, `edges`, `rootIds`). For regular queries, the bundle is present with matched nodes; shaped queries omit it.
- `result.bundle.nodes` – Array of matched memory nodes. Omitted when empty.
- `result.bundle.edges` – Array of relationship edges. Omitted when no edges exist.
- `result.bundle.rootIds` – Array of root node IDs. Omitted when empty.
- `result.data` – Array of shaped payloads. Omitted when the query has no shape projection.
- `errors` – Array of error objects when failures occur. Omitted on success.

Consumers should check for the presence of `errors` before operating on the result. This keeps backend services, clients, and AI agents aligned on the same contract.

## Concepts

Concepts are schemas for nodes (like tables in SQL). Each concept is declared in struct form in `dsl/<namespace>/concepts.memql`. The full concept id is derived from the **domain directory** plus the construct name: `dsl/library/` + `concept folder` → `v1:library:folder`. A directory may pin a namespace other than its own name with a one-line `namespace.pin` file beside its `.memql` files (`dsl/deployment/namespace.pin` holds `cluster`, so that domain's concepts are `v1:cluster:*`); nested namespaces are colon-delimited (`library:text` + `concept chunk` → `v1:library:text:chunk`). Each segment must be a single lowercase alphanumeric word; invalid names cause the loader to reject the concept.

> **`@namespace` on a concept is retired** (memql#5375, `annotation_retired`). It
> could only restate the domain directory or silently disagree with it, so it is
> refused at load and `memqlmigrate --rewrite=attributes` deletes it. Pin a
> deliberate divergence with `namespace.pin`.

Cross-domain references are imported with a file-top
`use <domain>.<construct>.{ names }` line. Constructs of the file's
OWN domain are ambient -- in scope with no import (#2617); the tree
gate keeps redundant same-domain imports out of the corpus.

<!-- corpus: 2026/examples/memql/concepts/retention-override.memql -->
```memql
use agents.concepts.{ agent }
use library.concepts.{ folder }

/// Per-(folderId, agentId) retention override.
concept retentionOverride {
  folderId  string  @required @description("v1:library:folder.id this override is scoped to.")
  agentId   string  @required @description("v1:agents:agent.id this override targets.")
  mode      enum("keep_forever", "keep_one_year", "inherit")  @required
  active    bool    @description("Soft-revoke flag.")

  @relationship(type="parent", field="folderId", target=folder, direction="outgoing")
  @relationship(type="references", field="agentId", target=agent, direction="outgoing")
}
```

Every concept a `@relationship` names is a `use ...concepts.{ }` import unless
it is this domain's own, `target=folder` included — the loader resolves the bare
name through the file's imports and refuses one it cannot find.

**Concept-level annotations:** `@version`, `@description` (required so humans and AI systems can reason about the dataset).

**Field annotations:** `@required`, `@description("...")`.

> **`@default` on a concept field is retired too** (memql#5375,
> `annotation_retired`, same rewrite). It was published as the JSON-Schema
> `default` keyword, which no validator applies, so a field carrying one never
> defaulted; the loader now drops the whole concept rather than register a
> schema that lies about it. Write the default in the mutation body,
> `args.<field> ?? <default>`. It **stays** on a `tool` / `prompt` / `builtin`
> field, where the body IS the schema a model reads.

**Field types are a closed set:** `string`, `bool`, `int`, `float`, `datetime`, `object`, `any`, `array`, plus the parameterised forms `[]<type>` (`[]string`, `[]object`), `map[string]<type>`, and inline `enum("a", "b", ...)` value sets. Map keys must be `string`.

Nothing else is accepted **in property position** — that includes properties nested inside a block, which are validated to the same standard. The failure is worth knowing about: an unrecognised type makes the concept's schema fail to build, and the loader drops **the whole concept**, not the offending field. Every query, mutation and shape bound to it then fails at runtime.

The JSON Schema spellings are the ones people reach for, because that is what the engine *emits* — but they are not what it *reads*:

| write this | not this |
|---|---|
| `bool` | `boolean` |
| `int` | `integer`, `int64`, `long` |
| `float` | `number`, `double`, `decimal` |
| `string` | `text`, `str`, `uuid` |
| `datetime` | `date`, `time`, `timestamp` |
| `array` | `list` |
| `object` | `json`, `dict` |

`memqllint` rejects each of these by name in property position, at any nesting depth, and points at the right spelling — so it is caught before boot rather than at it.

**Inside `[]<type>` and `map[string]<type>` type checking applies too**, at any nesting depth (memql#2951). An element type goes through the same builder a property type does, so `[]boolean` is rejected with the same `did you mean "bool"?` correction, `[]datetime` carries `format: date-time` on every entry, and `[][]string` / `map[string][]int` keep their inner type instead of collapsing to the outer one.

Annotations split by what they are *about*:

| kind | annotations | on `[]T` / `map[string]T` |
|---|---|---|
| **value constraints** | `@pattern`, `@minLength`, `@maxLength`, `@minimum`, `@maximum`, `@variant` | apply to each **element** |
| **field markers** | `@required` (and `!`), `@description`, `@unique`, `@immutable`, `@secret`, `@pii`, `@internal`, `@serverSet` | apply to the **field**, unchanged |

So `blocks []object @variant(discriminator="kind") { … }` validates every block against the union, and `tags []string @pattern("^[a-z]+$")` constrains every tag. `capabilities []string!` still means "the field is required", not "the elements are".

> **`@variant` without its branch block is REFUSED** (memql#3123), at every depth — `object`, `[]object`, `[][]object` and `map[string]object` alike. A discriminated union with no branches is not one: the discriminator never reached the schema, which asserted only `type: object`, so the row validated against nothing while the author believed a union was being enforced. Declare the branches, or drop the annotation if the field really is an arbitrary object.

> **The annotation table is ONE level deep, and going deeper is REJECTED**
> (memql#3049). A value constraint moves onto the immediate element, so that
> element has to be able to carry it. On a *composite* element -- `[][]T`,
> `[]map[string]T`, `map[string][]T` -- it would land on the inner array or map,
> where JSON Schema ignores it and where `@variant` is dropped entirely, so
> `[][]string @pattern(…)` would accept `[["ZZZ"]]` and `[][]object @variant(…)`
> would accept `[[{"nonsense":1}]]`. Rather than build a schema that contradicts
> the declaration, the loader refuses it and names the annotation, the
> declaration and the remedy. Single-wrap the field (`[]string @pattern(…)`,
> `[]object @variant(…)`), or move the constraint onto a named property inside an
> object. No *annotation* reaches the leaf of a composite element -- but a leaf
> *type* does, so `[][]enum("a","b")` and `[][]datetime` already constrain every
> leaf value, and are the spelling to reach for when that is what you meant.
>
> Only the annotation is refused, never the type: `[][]string`,
> `[]map[string]int` and `map[string][]string` all remain legal, keeping their
> inner type. Field markers are unaffected at any depth -- `capabilities
> [][]string!` is fine. Type checking is unaffected too: `[][]frobnicate` is
> still rejected at the inner level.

> **"The loader refuses" now covers BOTH ways an element can fail to carry a
> constraint** (memql#3124). The paragraph above is the *composite* case — the
> element is an array or a map, so the constraint would land one level too high.
> The other case is a perfectly ordinary element whose **type** cannot hold the
> keyword: `[]int @pattern("^[0-9]+$")`, `[]string @minimum(3)`,
> `[]object @minLength(3)`, `[]string @variant(…)`, and — with no wrapping
> involved at all — `object @minLength(3)`. Each emitted a keyword the validator
> ignores for that type, or in `@variant`'s case dropped the union outright. Both
> are refused at load now, at every depth, with the same diagnostic shape naming
> the annotation, the declaration and the type. Until this landed the sentence
> above over-promised, and the distinction was not one a reader was likely to
> draw unaided.
>
> The carrying types are exactly what the emitted schema says: `@pattern` /
> `@minLength` / `@maxLength` need a **string**, and `enum` and `datetime` count
> because both emit `type: string`; `@minimum` / `@maximum` need an **int** or
> **float**; `@variant` needs an **object**. `any` is exempt — it declares no
> type, so JSON Schema's own "applies to strings, ignored otherwise" rule is the
> declaration meaning exactly what it says rather than the engine contradicting
> it.

> `@minLength` / `@maxLength` remain a **character** count on the element, not an element count. There is still no way to bound the length of an array itself.

**Cross-concept references** are plain string fields holding the target's id (e.g. `folderId string`), optionally paired with an `@relationship` annotation so the engine can traverse the edge.

> **Retired.** The per-concept `concept.json` metadata file (with `type`, `skipDeleted`, `defaultFilter`, `cacheTTLSeconds`, `relationships` keys) is gone — concept schema, description, and relationships are all declared in the `.memql` construct shown above.

### Relationships

The `@relationship` annotation inside a concept body declares a graph edge:

<!-- corpus: 2026/examples/memql/concepts/retention-override.memql -->
```memql fragment
@relationship(type="parent", field="folderId", target=folder, direction="outgoing")
```

- `type` — how MemQL interprets the edge (see table below).
- `field` — the payload field used as the pointer. May be a dotted path when the field lives inside an object block (`field="lineage.originatingPlanId"`).
- `target` — the target concept (a short name resolved through the file-top `use ...concepts.{ ... }` import).
- `direction` — `outgoing` (this concept's field holds the target id) or `incoming` (the target's field holds this concept's id). There is no third value: an edge carried on both sides is two relationships, one declared from each concept.
- `as` — optional domain label naming what the edge *means* (see "The two axes" below).

| Type | Description | Use When |
|------|-------------|----------|
| `parent` | This node belongs to a parent node | The field stores a single ID pointing to the parent |
| `contains` | This node contains other nodes | The field stores an array of IDs of contained nodes |
| `owns` | This node owns other nodes | Similar to contains, but implies exclusive ownership |
| `alias` | This node is an alias for another | The field stores the ID of the aliased node |
| `equals` | This node is identity-equivalent to another | Reference concepts asserting two ids name the same thing |
| `createdBy` | This node was created by another | The field stores the creator's ID |
| `references` | This node interacts with another | Generic association — the default for a plain foreign key |

### The two axes: `type` and `as`

A relationship declares two independent things, and keeping them apart is the
point of the design.

**`type` is what the ENGINE does with the edge.** It is a closed set — the table
above — fixed by the engine. It decides id canonicalization, traversal, and the
node-type invariants.

**`as` is what the edge MEANS to your domain.** It is open. Any lowerCamelCase
identifier is valid, it is validated for *form* only and never checked against a
list, and it is optional.

<!-- corpus: 2026/examples/memql/concepts/assignment.memql -->
```memql
use agents.concepts.{ agent }
use identity.concepts.{ user }

/// Which agent answers for which person.
concept assignment {
  agentId    string
  forUserId  string

  @relationship(type="references", as="respondsAs", field="agentId",   target=agent, direction="outgoing")
  @relationship(type="references", as="actsFor",    field="forUserId", target=user,  direction="outgoing")
}
```

Without `as`, those two edges are structurally identical and there is no way to
say how they differ. `as` is what makes an edge self-describing.

**Why `as` is never validated against a list.** A closed vocabulary means a verb
the engine has not heard of is a boot refusal — and because this engine is
product-agnostic, a repo mounting its own DSL at `MEMQL_DSL_PATH` cannot patch
it. That is not hypothetical: `dependsOn` and `formedFrom` each cost an engine
release for exactly this reason before being retired to labels. `as` exists so a
new domain verb never requires one again.

Writing a domain verb in the `type` slot is the natural mistake, and the loader
says so directly:

```
relationship type "assignedTo" is invalid. Structural types are: alias, contains,
createdBy, equals, owns, parent, references. For a domain verb, use:
type="references", as="assignedTo"
```

`as` is optional everywhere, including on structural types
(`type="parent" as="filedUnder"` is legal). Two edges may share a label; a
label-scoped traversal returns their union.

A label is not write-only metadata: it is readable from a query (see
[Label-scoped traversal](#label-scoped-traversal)) and travels on the wire as
`result.bundle.edges[].as`.

**Common Mistake: Confusing `parent` vs `child`**

When a concept has a field that points TO another concept (like `folderId` pointing to a folder), use `type="parent"`. The relationship type describes the direction from the current node's perspective.

**Rule of thumb:**
- If concept A has a field storing concept B's ID → A declares `type="parent"` pointing to B
- If concept A has an array of concept B IDs → A declares `type="contains"` pointing to B
- The `child` type is not directly declared; child relationships are inferred by querying `childOf()`, which finds nodes that have a `parent` relationship to the target

#### Edge labels on the wire

`type` is what you **declare**. The label a traversal **emits** on
`result.bundle.edges[].type` is a separate, closed vocabulary, and the two are
not word-for-word the same. This is the complete emitted set:

| Edge label | Emitted when a traversal follows |
|---|---|
| `child` | a `parent` relationship, walked from the parent down to the child |
| `alias` | an `alias` relationship |
| `equals` | an `equals` relationship |
| `references` | an `references` relationship |
| `createdBy` | a `createdBy` relationship |
| `contains` | a `contains` relationship |
| `owns` | an `owns` relationship |

Two rules explain every difference between that list and the `type` table above:

- **An edge is written in the direction it was traversed.** `parent` is
  therefore declarable but never emitted — following it produces a `child`
  edge, pointing from the parent to the child it found.
- **A relationship type whose graph expansion is not wired contributes no
  edge.** Declaring it is still meaningful (it drives id canonicalization on
  the field), but no bundle edge appears for it.

Match on these exact strings. The engine emits them from one exported constant
set (`GraphEdgeLabels()` in `component/memql`), and a test fails if this table
and that set disagree, so what is written here is what arrives on the wire.

Alongside `type`, each edge carries `as` — the domain label of the relationship
it was traversed through, empty when that relationship declared none. The two
are independent: an edge is `child` because of what the engine did with it, and
`assignedTo` because of what its author said it means. `as` is additive, so a
client that ignores it sees exactly the previous contract.

## Expressions

A `.memql` file writes every expression in one language: a query's `filter` and
`refine`, a spec or trait body, a trigger `@filter`, a statement in a logic or
an automation body and its conditions, a mutation value, a call argument. Positions differ
in where the expression runs, and so in what it may contain; [Where each
expression runs](#where-each-expression-runs) lists them. The string a client
sends to `Execute` is not written in this language: see [The internal query
form](#the-internal-query-form).

<!-- corpus: 2026/examples/memql/expressions/actor-filter.memql -->
```memql
use todos.concepts.{ todo }

/// The caller's to-dos; `done` narrows to the open or the completed ones.
@actor
query todo myTodos {
  args {
    done  bool
  }
  filter   row => row.ownerUserId == actor.userId && (args.done == nil || row.done == args.done)
  sort     "row.createdAt", "desc"
  paginate 50
}
```

### The lambda parameter and bare names

A predicate position names the value it reads as a lambda parameter, and every
field is read through it:

- A payload field is `row.status`, never a bare `status`. So is a row intrinsic:
  `row.id`, `row.concept`, `row.type`, `row.createdAt`, `row.createdBy`,
  `row.provenance.<leaf>`.
- `row` is a convention, not a keyword: `filter t => t.status == "open"` is the
  same filter.
- A spec over an `@actor` shape reads the actor envelope, and its parameter is
  spelled `actor`: `spec actorEnvelope requiresOwner = actor => actor.role == "owner"`.
- A spec or trait is applied to the value it reads: `isActiveRecord(row)`,
  `requiresOwner(actor)`.
- A collection method names its element the same way:
  `row.tags.any(t => t == "urgent")`, `args.members.where(m => m.active)`. A
  lambda with two parameters is written `(acc, x) => acc + x`.

A bare name resolves, in order, to a lambda parameter in scope; a reserved root
(`args`, `actor`, `now`, `config`, `partition`, and in an automation `event`);
a loop variable; a name a statement above it bound
(`rows := query activeUsers()`); and, when it is called, a catalog function,
then a spec or trait. The catalog comes first, so a spec cannot shadow a
function by taking its name. Any other name is refused as unknown.

A filter may run over several lines. A line that opens with `&&`, `||` or `??`
continues the one above it, and so does the line after one that ends on an
operator or leaves a parenthesis open:

<!-- corpus: 2026/examples/memql/expressions/multiline-filter.memql -->
```memql fragment
filter  row => row.folderId == args.folderId
            && row.kind == "document"
            && isActiveRecord(row)
```

### Operators

<!-- BEGIN GENERATED: operators. Do not edit: go test github.com/znasllc-io/memql/component/language/functions -run Published -update-docs -->

| Operator | Form | What it does | Unset or absent operand |
|---|---|---|---|
| `.` | `row.status` | Reads a field of an object: `row.status` is the row's status field. | An absent object or field reads as absent. Where the object itself may be absent, the load requires `.?` instead. |
| `.?` | `row.?lineage.planId` | Reads a field of an object that may be absent: `row.?lineage.planId` is the plan id when lineage is present. | An absent object reads as absent instead of refusing, and the rest of the chain stays absent. The load requires `.?` wherever the object may be absent: an optional object field, an optional arg, an untyped value. |
| `!` | `!(x in list)` | Negates a boolean: `!(row.status in ["open", "held"])` is true when the status is neither. | Every predicate answers true or false, absent included, so `!(x == v)` is exactly `x != v`. |
| `-` | `-x` | Negates a number. |  |
| `*` | `a * b` | Multiplies two numbers. |  |
| `/` | `a / b` | Divides a by b; dividing by zero is an error. |  |
| `%` | `a % b` | Returns the remainder of dividing a by b; a zero divisor is an error. |  |
| `+` | `a + b` | Adds two numbers, or joins two strings: `"si-" + args.id`. A number joined to a string contributes its text. | In a join, an absent operand contributes the empty string. |
| `-` | `a - b` | Subtracts b from a. Write it with spaces, because `a-b` is one hyphenated name. |  |
| `??` | `a ?? b` | Returns a, or b when a is missing: `args.stage ?? "active"`. It binds tighter than comparison, so `a ?? "" == "x"` compares the coalesced value. | Falls through to b when a is absent or a blank or whitespace-only string; `false`, `0` and an empty list are kept. |
| `==` | `a == b` | Is true when two values are equal. Numbers compare numerically, and `1 == "1"` is false. | A missing field, JSON null, `nil` and `""` are one unset value: each equals the others and nothing else. |
| `!=` | `a != b` | Is true when two values differ. | The exact negation of `==`, so an unset field is not equal to `"x"`, and `row.f != nil` and `row.f != ""` are false for it. |
| `<` | `a < b` | Is true when a orders before b. Strings order by byte, so RFC 3339 timestamps order by time. | Any ordering against an absent field is false. |
| `<=` | `a <= b` | Is true when a orders before b or equals it. | Any ordering against an absent field is false. |
| `>` | `a > b` | Is true when a orders after b. | Any ordering against an absent field is false. |
| `>=` | `a >= b` | Is true when a orders after b or equals it. | Any ordering against an absent field is false. |
| `in` | `v in list` | Is true when v equals an element of the list: `row.status in ["open", "held"]`, `args.tag in row.tags`. | Membership is `==` against each element, so an unset v is in a list only when the list holds `""` or `nil`. Nothing is in an absent list. |
| `startsWith` | `s startsWith p` | Is true when the string s begins with the prefix p, or with any prefix in a list of them. | Is false when either side is absent; a blank prefix and an empty list match nothing. |
| `&&` | `a && b` | Is true when both sides are true, and skips the right side when the left is false. Both sides must be boolean: nothing else counts as true. |  |
| `\|\|` | `a \|\| b` | Is true when either side is true, and skips the right side when the left is true. Both sides must be boolean: nothing else counts as true. |  |
| `? :` | `p ? a : b` | Returns a when the boolean p is true, and b when it is false. It binds loosest of the value operators, so `x == 1 ? a : b` tests `x == 1`. |  |
| `=>` | `row => row.status == "open"` | Names the parameter a predicate or a projection reads: `row => ...` over a row, `t => t == "x"` over an element. Two parameters are written `(acc, x) => ...`. |  |

<!-- END GENERATED: operators -->

`!` works in every position: every predicate answers true or false, so `!e` is
its exact negation. There is no truthiness. `&&`, `||`, `!` and a condition take
a boolean, an absent value counts as false, a row field holding a value of
another type is not true, and anything else is refused with
`condition_not_boolean`: write `args.name != nil`, not `args.name`.

Equality is typed. `1 == "1"` is false, numbers compare numerically across
integers and floats, strings compare exactly and order by byte (so an RFC 3339
UTC timestamp orders as the instant it names).

### Operator precedence

Tightest first.

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

Three consequences are worth knowing:

- `??` binds tighter than a comparison, so `args.stage ?? "" == "active"`
  compares the coalesced value.
- `??` binds looser than `+`, so a coalesced operand of a join needs
  parentheses: `"P-" + (args.days ?? "90") + "D"`.
- A lambda's body runs as far right as it can: in `xs.any(x => x.a || x.b)`
  the body is `x.a || x.b`.

### Absent values

A field is *absent* when its key is missing or its value is JSON null. In `==`,
`!=` and `in`, an absent value, `nil` and the empty string are one value,
*unset*: each equals the others and nothing else. So `!=` is null-safe, and
`!= ""` and `!= nil` are the one "is set" test.

| Expression | `x` missing or null | `x` is `""` |
|---|---|---|
| `x == nil`, `x == ""` | true | true |
| `x != nil`, `x != ""` | false | false |
| `x == "open"` | false | false |
| `x != "open"` | true | true |
| `x < "m"`, and `<=`, `>`, `>=` | false | compared as a string |
| `x in ["open", ""]` | true | true |
| `x in ["open", "held"]` | false | false |
| `"open" in x` | false | refused: `in` needs a list |
| `x startsWith "o"` | false | false |
| `x.includes("o")` | false | false |
| `!(x == "open")` | true | true |
| `x ?? "none"` | `"none"` | `"none"` |
| `x.count()` | `0` | `0` |
| `x.?f` | absent | absent |

- An ordered comparison with a missing or null side is false either way round,
  so neither `row.dueAt < now` nor `row.dueAt >= now` matches a row with no due
  date. The empty string is a string, and orders before every other one.
- A blank prefix or needle matches nothing: `row.code startsWith ""` and
  `row.title.includes("  ")` are false for every row, so a caller who sends an
  empty search can never widen a selection to the whole table
  ([authoring rule 32](authoring-rules.md)).
- `??` also falls through on a whitespace-only string, and keeps `false`, `0`,
  `[]` and `{}` ([authoring rule 30](authoring-rules.md)).
- `.` and `.?` both read an absent object as absent at run time. They differ at
  load: where the object may be absent (an optional object field, an optional
  arg, an untyped value), `row.lineage.planId` is refused and
  `row.?lineage.planId` is the spelling.

It is one table for both evaluators: the SQL a filter compiles to and the
in-process evaluator give the same answer for every row.

### Membership and array fields

`in` tests membership in a list literal, an argument list or a row's string
array field:

<!-- corpus: 2026/examples/memql/expressions/membership.memql -->
```memql fragment
filter row => row.status in ["active", "pending"] && "filters" in row.topics
```

The first test matches a row whose `status` is "active" or "pending"; the second
a row whose `topics` array holds "filters", as a module with
`topics: ["filters", "limits", "sorting"]` does. Array membership is limited to
string arrays. Its negation is `!(v in list)`.

Over a row array field, `any`, `all` and `count()` push down too:
`row.tags.any(t => t startsWith "prio:")`, `row.assignees.count() > 1`. Every
other list method runs in process, so a filter can call it only on a list that
does not read the row, such as an argument.

### Where each expression runs

The pushdown positions -- a query's filter and sort key, a spec or trait body,
a row-authz argument -- are compiled to SQL, and the database evaluates them.
Every other position runs in process, in the engine.
The table is the tier manifest (`component/language/tiers`); a refusal at load
names the position by the name in its first column.

<!-- BEGIN GENERATED: where each expression runs. Do not edit: go test github.com/znasllc-io/memql/component/language/tiers -run TestWhereEachExpressionRunsIsPublished -update-docs -->

| Position | Written as | Runs | Takes | Applies a spec or trait |
|---|---|---|---|---|
| `beforeWriteValue` | a row.<field> = <expression> value in a before-write automation | In process | Every expression except a construct call | Yes |
| `queryFilter` | `filter row => ...` in a query, and the query a tool's `@handler(query=...)` runs | Pushed down to SQL | Every expression except a construct call; unary `-`, arithmetic, `??`, a map literal and an in-process function only on values that do not read the row | Yes |
| `sort` | `sort "row.createdAt", "desc"` | Pushed down to SQL | A literal, and nothing else | No |
| `specBody` | `spec c name = row => ...`, `trait name = row => ...` | Pushed down to SQL | Every expression except a construct call; unary `-`, arithmetic, `??`, a map literal and an in-process function only on values that do not read the row | Yes |
| `rowAuthzArgument` | `@rowAuthz(owner="ownerUserId")` | Pushed down to SQL | A literal, and nothing else | No |
| `automationCondition` | an automation's `if` and `else if` conditions, a `for` filter, a `switch` subject and a precondition's `check:` | In process | Every expression except a construct call | Yes |
| `triggerFilter` | `@filter(row => ...)` over the triggering row | In process | Every expression except a construct call | Yes |
| `logicBody` | a statement or a `return` in a logic body | In process | Every expression | Yes |
| `mutationValue` | `key: <value>` in `insert`, `update` or `stamp` | In process | Every expression except a construct call | Yes |
| `stepArgument` | an argument of a call statement in an automation | In process | Every expression | Yes |
| `toolDefault` | a tool field's `@default("...")` | In process | A literal, and nothing else | No |
| `promptInput` | a value bound into a prompt's input | In process | Every expression except a construct call | No |
| `queryRefine` | `refine row => ...` after `paginate` | In process | Every expression except a construct call | Yes |

<!-- END GENERATED: where each expression runs -->

Where a catalog function runs is in the [function catalog](functions.md#catalog).

### Plan constants

In a pushdown position, a subexpression that does not read the row is a plan
constant: it is computed once per call, in process, before the query runs, and
the query receives its value as a parameter. That is what lets an in-process
function, arithmetic, `??` or a map literal appear in a filter, on values that
do not read the row:

<!-- corpus: 2026/examples/memql/expressions/plan-constant.memql -->
```memql fragment
filter row => row.expiresAt < addDuration(now, "P1D")
```

It is also what makes an optional argument a plain predicate. `args.x == nil ||
row.f == args.x` folds before the SQL is written: to true when `args.x` is unset
(left out, null or `""`), and to `row.f = $1` otherwise. Under `||` the guard is
`args.x != nil && row.f == args.x`, as in the observability metrics query:

<!-- corpus: 2026/examples/memql/expressions/optional-argument.memql -->
```memql fragment
filter  row => row.bucket == args.bucket
            && row.windowStart >= args.windowStart
            && row.windowStart < args.windowEnd
            && ((args.codeReference != nil && row.codeReference == args.codeReference)
                || row.codeReference startsWith args.prefixes)
```

A subexpression that reads the row has to push down itself. The load refuses
one that cannot, and names the node, the position, the nearest pushdown
spelling and the rule's id: `lower(row.email) == args.email` is refused in a
filter, and `row.email == lower(args.email)` is the filter that runs. The id is
printed last, in brackets, and carried as a field on the load report and on an
authoring diagnostic; the message may be reworded, the id may not:

| Rule id | Refused |
|---|---|
| `lower_refused` | A node with no form at its position: an in-process function or arithmetic over the row, a construct call in a predicate, a node kind the position does not admit |
| `lower_unknown_name` | A name the position does not bind: a bare payload field (`status` for `row.status`), an undeclared argument, a predicate nothing registers |
| `lower_unknown_field` | A field the bound concept, shape or actor envelope does not declare |
| `lower_optional_hop` | A read through an optional object written with `.` instead of `.?` |
| `lower_context_spec_on_row` | A context spec applied to the row |
| `lower_row_predicate_on_actor` | A row spec or trait applied to the actor |
| `lower_not_boolean` | A condition whose type is known and is not boolean |
| `lower_cost_over_budget` | An in-process expression whose static cost estimate is over the bound below |
| `lower_actor_in_row_predicate` | A spec or trait over rows reading the `actor` root ([Specs](#specs)) |

An in-process expression is bounded twice. At load, a static estimate of how
many nodes it can evaluate is refused above 1,000,000; the estimate counts a
lambda over a collection it cannot size as 1,000 evaluations of the body, so one
scan loads and two nested scans do not. At run time each evaluation may visit
1,000,000 nodes, and one that needs more stops with
`expression_budget_exceeded`.

### The refine clause

`refine` is the one place a query evaluates an expression in process over its
rows. After `paginate` reads a page, `refine row => <expression>` keeps the rows
the expression holds for:

<!-- corpus: 2026/examples/memql/refine/refine.memql -->
```memql
use notes.concepts.{ note }

/// The caller's notes whose body mentions q, in any letter case.
@actor
query note notesMentioning {
  args {
    q  string!
  }
  filter   row => row.ownerUserId == actor.userId
  sort     "row.createdAt", "desc"
  paginate 50
  refine   row => lower(row.body).includes(lower(args.q))
}
```

- `refine` requires `paginate`: the page is what bounds the rows it reads.
- It runs after the page is read, so a page can hold fewer rows than `paginate`
  asked for, none included.
- It runs in process, so every in-process function and method is available on
  the row: here `lower(row.body)`, which a filter refuses.

A predicate that can push down belongs in `filter`, where the database narrows
the rows before any of them is read.

### Retired spellings

Edition 2026 retires the spellings below. The parser refuses each one wherever
a `.memql` file writes it, with one message shape:

```text
cond(p, a, b) is retired in edition 2026: write p ? a : b (memqlmigrate --rewrite=expressions rewrites it)
```

`memqlmigrate --rewrite=expressions` rewrites a tree onto the new forms, and
refuses, naming the clause, anything it cannot convert without changing what it
means. In VS Code the refusal is underlined as an error, and the language server
offers the same rewrite as a quick fix, **Rewrite to edition 2026**, for one
construct or the whole file ([Sense](sense.md#the-rewrite-quick-fix)). The table
is the parser's own (`parser.V1RetiredForms`), which Sense reads for its hover
too.

The string a client sends to `Execute` is not an authored expression:
[the internal query form](#the-internal-query-form) keeps its own grammar, which
this table does not change.

<!-- BEGIN GENERATED: retired spellings. Do not edit: go test github.com/znasllc-io/memql/component/language/parser -run TestV1RetiredFormsArePublished -update-docs -->

| Retired | Write instead |
|---|---|
| `when(args.x) { ... }` | `(args.x == nil \|\| <predicate>)` |
| `?.<predicate>` | `(args.x == nil \|\| <predicate>)` |
| `;` as a connective | `&&` |
| `,` as a connective | `\|\|` |
| `has` | `<value> in <list>` |
| `not in` | `!(<value> in <list>)` |
| `and(a, b)` | `a && b` |
| `or(a, b)` | `a \|\| b` |
| `not(a)` | `!a` |
| `lt(a, b)` | `a < b` |
| `gt(a, b)` | `a > b` |
| `lte(a, b)` | `a <= b` |
| `gte(a, b)` | `a >= b` |
| `cond(p, a, b)` | `p ? a : b` |
| `concat(a, b)` | `a + b` |
| `coalesce(a, b)` | `a ?? b` |
| `exists(x)` | `x != nil` |
| `len(x)` | `x.count()` |
| `count(x)` | `x.count()` |
| `contains(s, sub)` | `s.includes(sub)` |
| `mean(x)` | `x.avg()` |
| `first(x)` | `x.first()` |
| `last(x)` | `x.last()` |
| `timestamp()` | `now` |
| `now()` | `now` |
| `null` | `nil` |
| `$args.x` | `args.x` |
| `spec <name>` | `<name>(row)` |
| `trait <name>` | `<name>(row)` |
| `.contains(...)` | `v in <list>` for membership, `s.includes(sub)` for a substring |
| `{ args.x.y }` | `{ y: args.x.y }` |
| `filter <predicate>` | `filter row => <predicate>` |
| `spec <bound> <name> { return <predicate> }` | `spec <bound> <name> = row => <predicate>` |
| `trait <name> { return <predicate> }` | `trait <name> = row => <predicate>` |
| `@filter(<predicate>)` | `@filter(row => <predicate>)` |

<!-- END GENERATED: retired spellings -->

`contains` is retired only as the two-argument substring test: with a lambda it
is the graph traversal and keeps its name.

## Executing Queries

Every transport below carries [the internal query form](#the-internal-query-form).

### From Go

```go
tree, err := memEngine.Execute(ctx, `
	sort(
		paginate(
			concept==v1:examples:world && status=="active",
			50
		),
		"createdAt","desc"
	)
`)
```

### Via MCP `memql`

```json
{
  "query": "sort(paginate(contains(id==\"project-abc\") && status==\"open\",25),\"due_date\",\"asc\",\"createdAt\",\"desc\")"
}
```

### WebSocket Stream

Browser clients connect to `/memql/ws`, which upgrades to a long-lived WebSocket and forwards frames to the `MemqlService.Stream` gRPC method. The bearer credential travels as a WebSocket subprotocol (`new WebSocket(url, ["bearer", jwt])` -- the `Sec-WebSocket-Protocol` header, which stays out of request-line access logs); the `memql_auth` cookie is honored automatically, and the older `?bearer_token=` / `?token=` query params remain accepted but are deprecated.

Frames are JSON encodings of the existing protobuf envelopes. A typical request/response pair looks like:

```json
{
  "messageId": "req-123",
  "executeQuery": {
    "requestId": "req-123",
    "query": "concept==v1:examples:world && status==\"active\""
  }
}
```

```json
{
  "messageId": "resp-123",
  "queryResult": {
    "requestId": "req-123",
    "result": {
      "bundle": {
        "nodes": [
          { "id": "v1:examples:world:world-aurora", "concept": "v1:examples:world" }
        ],
        "rootIds": ["v1:examples:world:world-aurora"]
      }
    },
    "done": true
  }
}
```

The bridge enforces a small per-connection window (four concurrent queries and a 5 MiB frame limit). Clients should reuse a single socket and listen for `queryResult.done` or `queryError` payloads.

Configuration variables (prefixed with `MEMQL_WS_`) let you tune the gateway:

| Variable | Description | Default |
|----------|-------------|---------|
| `MEMQL_WS_DIAL_TIMEOUT_MS` | How long to wait when dialing the internal gRPC server. | `5000` |
| `MEMQL_WS_WRITE_TIMEOUT_MS` | Per-message write deadline applied to the WebSocket. | `10000` |
| `MEMQL_WS_MAX_CONCURRENT_REQUESTS` | Maximum in-flight `executeQuery` messages per WebSocket. | `4` |
| `MEMQL_WS_MAX_MESSAGE_BYTES` | Maximum accepted frame size from the browser. | `5242880` (5 MiB) |
| `MEMQL_WS_PING_INTERVAL_MS` | Interval for server-side WebSocket keepalive pings. Prevents idle connection timeouts on edge/proxy infrastructure. Set to `0` to disable. | `30000` (30s) |

## The internal query form

The string a client sends to `Execute` -- over gRPC, the WebSocket bridge or
the MCP `memql` tool -- is the internal query form: a call to a named query or
mutation (`artifactsInFolder(folderId: "f-1")`), a filter string, an `insert(...)`
literal, or an introspection meta-command. SDKs build it, row authorization
injects predicates into it, and the struct-form rewriter generates it from each
declared query. It is a wire contract, so its grammar does not follow the
authored language: it keeps bare payload fields, has no lambdas, and refuses
`!`. It is not an authoring surface. A `.memql` file declares a query in the
[expression language](#expressions), and a client calls it by name.

| Component            | Description                                                                                                     |
|----------------------|-----------------------------------------------------------------------------------------------------------------|
| Filters              | Comparison expressions joined by `&&` (AND) and `\|\|` (OR), with parentheses, using Go precedence. No `!` (NOT): it parses and is then refused by the AST converter (memql#3630); write the `!=` form. |
| Fields               | `concept`, `id`, `type`, `createdAt`, `createdBy`, or a bare payload property (`status`, `profile.name`).                                         |
| Operators            | `==`, `!=`, `>`, `>=`, `<`, `<=`, `==nil`, `!=nil`. See [Operators in the internal form](#operators-in-the-internal-form).  |
| Parentheses          | Group complex logic: `(concept==v1:assistant \|\| concept==v1:examples:persona) && active==true`.       |
| Limit                | Use `paginate(<expr>, limit)` to request an explicit page size; omitting both `paginate` and `sort` caps the read at `MEMQL_MEMORY_ENGINE_DEFAULT_LIST_CAP` (default 50, the unmarked-list backstop). Continuation is via keyset cursors, not an offset skip. |

The legacy parser of this form still reads `;` as AND and `,` as OR
(memql#3630), which is why a `,` inside parentheses was once an authorization
bypass (memql#3612). No authored expression can write either: the parser refuses
both in a `.memql` file, with every other [retired spelling](#retired-spellings).

IDs are persisted as `<concept>:<raw-id>`. MemQL supports both full IDs and short IDs (when concept context is provided):

**Full ID (always works):**
- `id=="v1:examples:world:world-aurora"` – exact match on full storage ID

**Short ID (requires concept context):**
- `concept==v1:examples:world && id=="world-aurora"` – short ID resolved using concept from query

**Important:** Short IDs without concept context will return an error:
```
// This will ERROR - no concept context to resolve short ID
id=="world-aurora"

// This works - concept provides context for ID resolution
concept==v1:examples:world && id=="world-aurora"

// This also works - full ID doesn't need context
id=="v1:examples:world:world-aurora"
```

This design ensures predictable, exact-match behavior and avoids ambiguous results.

### Filters

A filter in the internal form narrows the set of returned nodes:

```text
concept==v1:assistant && active==true
```

- Use `&&` for AND and `||` for OR, and group with parentheses:
  `(concept==A || concept==B) && active==true`. There is no `!`: write the
  `!=` form.
- Field paths support dot notation for nested JSON: `profile.name`.

An authored filter is written in the expression language instead, which adds
`!`, `in`, `startsWith`, `??`, the ternary and optional arguments as plain
predicates: see [Expressions](#expressions).

### Directives

Directives wrap filters and apply transformations or constraints. They must enclose the entire filter expression and form the outermost stack:

| Directive | Description | Example |
|-----------|-------------|---------|
| `asOf()` | Evaluate the expression at a specific timestamp | `asOf(concept==v1:assistant, "2025-01-01T00:00:00Z")` |
| `sort()` | Order results by field(s) | `sort(concept==v1:assistant, "createdAt", "desc")` |
| `paginate()` | Bound the result page size (LIMIT) | `paginate(concept==v1:assistant, 10)` |
| `withDepth()` | Limit traversal depth for relationships | `withDepth(parentOf(...), 2)` |

Directives can be nested: `sort(paginate(concept==v1:assistant, 10), "createdAt", "desc")` returns assistants sorted by creation date with pagination. Relationship functions (`parentOf()`, `childOf()`, `contains()`, ...) participate directly in the expression tree inside the directive stack.

> **Retired at the runtime surface (#250).** `shape(...)` and `select(...)` are no longer accepted in runtime query strings — declare the projection in the DSL instead (a `query` construct with a `shape` directive). The `@ "<RFC3339>"` / `@latest` timestamp suffix and inline spec definitions (`name := expr`) are likewise rejected in runtime strings; use `asOf(...)` and DSL-defined specs. The engine returns a typed `ErrUnsupportedQueryShape` with a migration hint for each of these.

### Dot-Path Field Access

MemQL supports deep dot-notation for accessing nested JSON fields:

```text
profile.settings.theme == "dark"
```

Path segments follow JSON keys, including arrays via numeric indices when
needed. An authored expression reads the same path through its parameter:
`row.profile.settings.theme == "dark"`.

### Operators in the internal form

| Operator          | Example                                                                 | Notes                                                                                     |
|-------------------|-------------------------------------------------------------------------|-------------------------------------------------------------------------------------------|
| `==` / `!=`       | `status=="open"`                                                | Direct equality / inequality.                                                             |
| `>` / `<`         | `score>0.85`                                                    | Numeric comparisons (strings use lexical ordering).                                       |
| `>=` / `<=`       | `attempts<=3`                                                   | Greater-than-or-equal / less-than-or-equal.                                               |
| `==nil`           | `metadata.notes==nil`                                           | Field absent or explicitly `null`. Apply to payload paths or intrinsic columns.           |
| `!=nil`           | `metadata.tags!=nil`                                            | Field present with a non-null value.                                                      |

A string sent to `Execute` cannot use `in` or `startsWith`: write membership as
a disjunction, `(x=="a" || x=="b")`. Both are operators of the authored
language, with the rest of the [operators](#operators).

Combined example covering several operators:

```text
concept==v1:lead &&
source!=nil &&
metadata.owner==nil &&
(status=="new" || status=="contacted")
```

The query above returns all leads in `new`/`contacted` state that already have a `source` value but still need an assigned `owner`.

### Sorting

Use `sort(<expr>, "<field>", "<direction>?", ...)` to order results. The function:

- Accepts any MemQL expression as the first argument.
- Requires at least one string literal field name; directions are optional (`"asc"` or `"desc"`, defaulting to `"desc"`).
- Allows multiple field/direction pairs for deterministic tie-breaking.
- Must wrap the entire query expression (i.e., `sort(...)` should be the outermost call).
- Supported fields: the row intrinsics `id`, `concept`, `createdAt`, `createdBy`, `type` -- each also addressable through the `row.` namespace (`"row.createdAt"`) -- and bare payload properties (`status`, `metadata.tags`).
- In an authored `.memql` sort clause the namespaced spelling is required for intrinsics and enforced by CI (memql#2786), because a bare key cannot be told apart from a payload property of the same name. This runtime form still accepts either spelling, so existing SDK and API callers are unaffected.
- Limits and offsets always apply **after** sorting. Sorting on payload properties may cause the engine to fetch up to `MEMQL_MEMORY_ENGINE_MAX_WINDOW` rows to guarantee correctness.

Example:

```
sort(
  paginate(childOf(concept==v1:examples:world && id=="v1:examples:world:world-aurora"), 100),
  "createdAt","desc",
  "id","asc"
)
```

### Pagination

`paginate(<expr>, limit)` bounds the result page size. The function:

- Requires a single integer argument (limit) greater than zero — the page size.
- Can be combined with other helpers (e.g., `sort(paginate(...), ...)`).
- **Continuation is keyset, not offset.** Offset pagination was removed
  (epic 5, 5.13 / memql#1993): it was O(offset) and drifted under
  concurrent inserts. To fetch the next page, pass the `nextCursor` from
  the prior response back as the query cursor; the engine pushes a
  `WHERE (createdAt, id) <keyset> (?, ?)` predicate and continues from the
  encoded position. The first page is bounded by a plain SQL `LIMIT`.

**Default-cap backstop (memql#1965).** A query that arrives with NO
explicit window — neither `paginate` nor `sort` — is treated as an
unmarked list read and capped at `MEMQL_MEMORY_ENGINE_DEFAULT_LIST_CAP`
(default **50**), not `MEMQL_MEMORY_ENGINE_MAX_RESULTS`. This bounds the blast
radius of an accidental full-table read. A query that paginates or sorts
states its own window (capped at `MEMQL_MEMORY_ENGINE_MAX_WINDOW`); a query
marked `@unbounded("reason")` is rewritten to an explicit wide paginate
and bypasses the 50-cap. See the [pagination authoring rule](authoring-rules.md#23-list-returning-queries-must-declare-their-bound).

```
paginate(concept==v1:examples:module && worldId=="v1:examples:world:world-aurora", 200)
```

### Temporal Snapshots

`asOf(<expr>, "<timestamp>")` evaluates a query using a consistent historical snapshot. Supply an RFC3339/RFC3339Nano timestamp string:

```
asOf(concept==v1:assistant && active==true, "2025-11-01T00:00:00Z")
```

In a **declared** query the clause also takes the caller's instant
(memql#2992) -- `args.<name> ?? latest`, where the fallback means an omitted
value is the latest version. The fallback is **required**, not optional: the
bare `asOf args.<name>` is rejected at parse, because omitting the argument is
the common path for this construct and without a fallback every such caller
fails at run time (memql#3028):

<!-- corpus: 2026/examples/memql/traversal/as-of-argument.memql -->
```memql
use deployment.concepts.{ deployment }

/// Deployment history for a cluster, at the caller's instant or the latest one.
query deployment deploymentsForClusterAt {
  args {
    clusterId  string!
    asOf       datetime
  }
  filter  row => row.clusterId == args.clusterId
  asOf    args.asOf ?? latest
}
```

Before this a declared query could offer only a fixed instant or `latest`, and
a caller-chosen point in time was reachable solely by hand-building a runtime
query string -- so a consumer that calls named queries could not reach it at
all. The value is validated as RFC3339 at call time, so a malformed instant is
an error rather than a silent fall back to `latest`.

### Depth Overrides

`withDepth(<expr>, depth)` customizes relationship traversal depth. Depth must be a positive integer:

```
withDepth(parentOf(concept==v1:examples:quest && id=="v1:examples:quest:quest-nodes"), 3)
```

## Relationship Functions

A traversal selects rows through the edges a concept declares (see
[Relationships](#relationships)). In an authored filter it takes a lambda that
selects the rows to start from, and it pushes down:

<!-- corpus: 2026/examples/memql/traversal/child-of.memql -->
```memql fragment
filter row => childOf(w => w.id == args.worldId) && row.tier == "silver"
```

That selects the silver-tier children of the world `args.worldId`. The internal
form writes the same traversal with a filter as its argument:
`childOf(concept==v1:examples:world && id=="v1:examples:world:world-aurora") && tier=="silver"`.

| Function        | Purpose                                                                                                  |
|-----------------|----------------------------------------------------------------------------------------------------------|
| `parentOf(match)`| Finds parents referenced by `parent` relationships.                                                     |
| `childOf(match)` | Retrieves children whose payload points to the parent ID.                                               |
| `contains(match)`| Expands collection membership arrays.                                                                   |
| `owns(match)`    | Resolves ownership links in both directions.                                                            |
| `aliasOf(match)` | Collects nodes sharing alias groups.                                                                    |
| `equals(match)`  | Follows equality relationships similar to alias.                                                        |
| `references(match)` | Traverses recorded interaction edges (e.g., the agent a run was assigned to).                        |
| `createdBy(match)` | Resolves creator nodes using payload or table-backed metadata.                                        |
| `ids(match)`     | Returns lightweight nodes (no payload/schema) useful for identifier lists.                              |

#### Label-scoped traversal

Every traversal except `ids` takes an optional leading string: the
[`as` label](#the-two-axes-type-and-as) to scope the traversal to.

<!-- corpus: 2026/examples/memql/traversal/label-scoped.memql -->
```memql fragment
filter row => references("respondsAs", a => a.id == args.agentId)
```

- Without the label a traversal follows every edge of its type.
- Where several edges on a concept share a label, the traversal follows their
  **union** — that is the useful reading of "every edge meaning *respondsAs*".
- A label matching no edge returns **empty, not an error**.
- `ids` does not take a label, and says so rather than ignoring it: `ids()`
  projects the rows it is given and follows no edge, so a label on it is always
  a mistake.
- In the internal form `contains` does not take a label either: there its
  two-argument slot is still the substring search `contains(text, substr)`. An
  authored expression writes the substring test as `s.includes(sub)`, which is
  what frees `contains("label", match)` to mean the labelled traversal.

**Note:** Relationship pointer fields are optional — if a node has a null or missing pointer field, it is silently skipped during traversal rather than causing an error. A pointer present but set to the empty string counts as missing, and is skipped the same way.

**A traversal that finds nothing returns an empty result, not an error** — uniformly across `parentOf`, `childOf`, `contains`, `owns`, `references`, `createdBy`, `aliasOf` and `equals`. "Who is this row's parent" asked about a root row is an ordinary question whose answer is "none". This holds whether the pointer key is absent, blank, or names a row that does not exist.

What *does* still error is a different question: asking for a traversal on a concept that declares no such relationship at all (`aliasOf` on a concept with no `alias` edge). That names an edge the schema does not have, so answering it "empty" would hide the typo.

## Result Shaping (Shapes)

Shapes are reusable field-projection templates, declared in struct form in `dsl/<namespace>/shapes.memql`. Queries reference them via the `shape <name>` directive; the engine projects each matched row through the template and returns the result in `result.data`.

Each shape declares its **kind** (where its fields come from) via `@row` and/or `@actor`. At least one is required (enforced at load since memql#3621); both is allowed (mixed shape). The body is a list of field paths — shapes have no inputs and no return.

**Row shapes** project a concept's payload + row intrinsics. The bound concept is named by the **signature** `shape <Concept> <name>` — this domain's own concept, as below, or one a file-top `use ...concepts.{ ... }` import brings in:

<!-- corpus: 2026/examples/memql/shapes/row-shape.memql -->
```memql
/// Per-(folderId, agentId) retention override.
concept retentionOverride {
  folderId  string!
  agentId   string!
  mode      enum("keep_forever", "keep_one_year", "inherit")!
  active    bool
}

@row
/// Per-(folder, agent) retention override projection
shape retentionOverride retentionOverrideFull {
  row.id
  folderId
  agentId
  mode
  active
  row.createdAt
}
```

Body path translations: a bare `name` → payload property; `row.X` → row intrinsic (`id`, `createdAt`, `createdBy`, etc.); `actor.X` → auth envelope (in function bodies, reading `actor.*` requires the `@actor` preamble annotation -- #2621). Each path becomes a template entry keyed by the path's terminal segment.

**Actor shapes** project the engine envelope (the authenticated actor + engine timestamp + allow-listed config). They carry no signature concept. Closed field set (#2623, the one canonical envelope): `actor.userId` / `actor.role` / `actor.identityId` / `actor.isClusterOwner` / `actor.primaryEmail` / `actor.now` (plus the legacy `isOwner` alias; `actor.config.<key>` is retired -- bare `config.<key>` is the config read):

<!-- corpus: 2026/examples/memql/shapes/actor-shape.memql -->
```memql
@actor
/// Caller envelope projection: authenticated actor, role, and now.
shape callerEnvelope {
  actor.userId
  actor.role
  actor.identityId
  actor.isClusterOwner
  actor.now
}
```

`actorEnvelope` in `dsl/common/shapes.memql` is the tree's own envelope shape,
and the one the specs below bind; a domain that wants its own declares it the
same way under a name of its own.

**Trait shapes** are `@row` shapes signature-bound to a generic trait concept — scaffolds for cross-concept predicates (`activeRowTrait`, `statusRowTrait`, `deletedRowTrait`, `archivedRowTrait`, etc. in `dsl/common/shapes.memql`).

**No composition.** `include` is not a shape verb. It was documented for a long time and never implemented (memql#3621): the parser reads a body as a path list, so `include spaceCard` parsed as two payload properties and projected two always-null keys. Zero shapes used it, so the promise was removed rather than built — `include` is now rejected at load. To share a projection, repeat the paths, or drop the body entirely and take the default projection over the bound concept (memql#2035).

**What the loader checks (memql#3621).** A bare payload property must be a declared field of the bound concept; the bound concept must resolve (an ambiguous bare name disambiguates through the shape's own domain); two paths may not collapse onto the same terminal key (every path is keyed by its LAST segment, so `row.id` + `id` used to yield one entry and lose the row id); and the declared kind must match the body — `actor.*` needs `@actor`, `row.*` / bare payload needs `@row`, and at least one kind is required.

> **Retired forms.** Receiver-function shapes (`func (Shape) ...`), the `@template` annotation, `node("...")`-wrapped template bodies, and the `@concepts("v1:...")` binding annotation are all retired and rejected at parse time. The concept binding lives in the signature; the body is a plain path list. Runtime `shape(<expr>, {...})` / `select(<expr>, ...)` query strings are retired too (#250) — wanting a projection means defining (or reusing) a DSL query with a `shape` directive.

To discover available shapes at runtime, use the `shapeTemplates()` and `shapeHelp("name")` introspection commands (see [Introspection](#introspection-functions)).

## AI: Providers, Levels, Policies, Rules, and Prompts

MemQL's AI integration is intentionally scoped: a language model can only influence the *output* of an explicitly AI-aware construct — a `prompt`, reached through a builtin that wraps the provider call; filters, sorts, pagination, and mutations remain deterministic.

### Providers

AI provider configurations (OpenAI and Anthropic — the only supported vendors) live in `dsl/providers/providers.memql`. Struct form, mirrors concepts / shapes / tools:

<!-- corpus: 2026/examples/memql/providers/model-provider.memql -->
```memql
@extends("openai")
@model("gpt-5.4-mini")
/// OpenAI GPT-5.4 Mini - balanced cost/latency chat (non-streaming)
provider chat54Mini {
  params {
    contextWindow        128000
    maxCompletionTokens  16384
  }
}
```

Base providers (vendor-level auth + type) use the same form:

<!-- corpus: 2026/examples/memql/providers/base-provider.memql -->
```memql
@base
@vendor("Anthropic")
provider anthropic {
  auth {
    federationRuleId   env("MEMQL_AI_ANTHROPIC_FEDERATION_RULE_ID")
    organizationId     env("MEMQL_AI_ANTHROPIC_ORGANIZATION_ID")
    serviceAccountId   env("MEMQL_AI_ANTHROPIC_SERVICE_ACCOUNT_ID")
    identityTokenFile  env("MEMQL_AI_ANTHROPIC_IDENTITY_TOKEN_FILE")
  }
}
```

The legacy `func (Provider) name { ... }` form is retired; the parser rejects it with a migration hint.

**Provider types** (`@type`, matched without regard to case; the clients are in `component/memql/ai_providers.go`) are `OpenAI` / `OpenAIChat` (chat completions), `OpenAITTS` (text-to-speech) and `OpenAIEmbedding` (embeddings) for OpenAI, and `Anthropic` / `AnthropicChat` (Claude chat / vision) for Anthropic. `Fleet` and `SubscriptionApp` are accepted on a `@base` provider only: their models are named from a policy (`fleet:<model>`, `app:<id>`) rather than declared as children. Streaming is a parameter (`streaming true` in `params`), not a type, and a child that `@extends` a base takes the base's type. Any other type leaves the provider registered but unavailable (`unsupported provider type`).

**Lifecycle annotation (`@disabled`).** Providers accept the same lifecycle flag as every other construct kind (the uniform ruling, #2604-#2608). `@enabled` was the explicit-on form and is **retired** (memql#5375, refused at parse as `annotation_retired`): constructs are on by default, so it was a no-op that read like a switch; `memqlmigrate --rewrite=attributes` deletes it. `@disabled` skips the provider at load — it is **not registered and no auth resolution is attempted** — while staying in the tree for a future re-enable. `@disabled` on a `@base` **propagates**: every child that `@extends` it is skipped too. Dependents degrade gracefully — a policy whose `@primary` is disabled routes via its `@fallback`; a prompt whose `@defaultProvider` is disabled falls back to the default.

> **Semantics of `@disabled`** (shared across every construct that takes it): the construct is **not loaded/active at runtime right now**. It does NOT mean deprecated, abandoned, or exempt from maintenance / refactors / conformance — it is a reversible on/off switch. ("Deprecated / abandoned" is a separate axis carried by `@deprecated`.)

### Levels

**A call declares how much intelligence it needs, never a model.** A model name at a call site is a release every time the fleet changes, so `@level` is what a `prompt` carries and what a Go call site names in its request.

The set is closed at four, and there will not be a fifth: an abstraction a person cannot hold in their head is not one.

| Level | What declares it |
|---|---|
| `fast` | Triage, intake, classification, summaries, suggestions, the safety classifier |
| `strong` | An agent's reply, a conductor turn, an authoring design pass |
| `reasoning` | Emitting or repairing a construct, re-planning a run |
| `embeddings` | Every embedding call |

<!-- corpus: 2026/examples/memql/routing/prompt-level.memql -->
```memql
@level("reasoning")
@templateFile("prompts/emitConstruct.tmpl")
/// Emit a construct from an approved design
prompt emitConstruct {
  design  object  @required  @description("The approved design to emit a construct from.")
}
```

`@level` is **required on every prompt**, in the embedded tree and in a bundle mounted at `MEMQL_DSL_PATH` alike; a prompt without one refuses to load and the message names all four values. **Modality is never declared** — whether a call is chat, streaming chat, tools, structured output, vision or an embedding is derived from the call itself and interface-checked by the router.

### Policies

The `policy` construct is an **ordered chain of places to look**: empty-bodied, annotated with `@primary` and repeatable `@fallback`, consolidated in `dsl/policies/policies.memql`.

<!-- corpus: 2026/examples/memql/routing/policy.memql -->
```memql
/// The default chain: local strongest, then a signed-in app, then the cheapest vendor.
@primary("fleet:strongest")
@fallback("app:*")
@fallback("federation:cheapest")
policy localFirst { }
```

A chain entry is one of a **closed grammar**, checked at load:

| Form | Resolves to |
|---|---|
| `streamClaudeSonnet` | one provider by name |
| `fleet:strongest` / `fleet:fastest` | the best / quickest local model that can serve this call |
| `fleet:<modelId>` | one local model by id (a model id may itself contain a colon) |
| `app:*` / `app:<id>` | any / one signed-in subscription app on the caller's machines |
| `federation:cheapest` / `federation:strongest` | the cheapest / strongest vendor record that qualifies |
| `federation:<providerName>` | one vendor record by name |
| `policy:<name>` | **another policy**, expanded at load |

`fleet:*` is **retired** and refuses to load with `fleet:strongest` named in the message: it said "any", which is not what it did.

`policy:<name>` is what makes policies compose. Chains are expanded at load, a cycle refuses to load and prints the loop, and the router only ever walks a chain with no `policy:` entry left in it.

`@maxLatencyMs`, `@maxTimeToFirstTokenMs` and `@preferredRole` are **removed from the grammar**. All three were parsed, stored, projected and consumed by no routing decision; an author who wrote one was telling the router something it would not act on, and silence there is worse than a refusal.

> **Decision-policy tier — RETIRED (#984).** The cross-cutting decision model (`func (Policy)` constructs, `@tier` / `@audited` annotations, `engine.EvaluatePolicy`) is fully removed. Caller-based boolean checks (admin / owner / permission) are authored as **context-specs** and applied in a filter to the actor, `requiresOwner(actor)`; the only live `policy` surface is provider selection.

### Rules

A **rule** maps a call's metadata to a policy. It is the half a policy cannot express: a chain says *where* to look and can never say *which calls* it is for. Declarative, empty-bodied, in `dsl/rules/rules.memql`.

<!-- corpus: 2026/examples/memql/routing/rule.memql -->
```memql
/// An operator's agent reply reasons.
@when(prompt="agentReply", role="operator")
@level("reasoning")
@policy("localFirst")
@precedence(60)
@onUnavailable("degrade")
rule operatorReplyReasons { }
```

**A rule name is unique across the whole corpus**, and a second declaration of
one is refused at load naming both files — a duplicate resolved by load order is
a routing decision nobody wrote and nobody can reproduce. (`operatorReasoning`
is the shipped rule this example is modelled on.)

`@when` takes a **closed key set**. Every key is optional and all present keys are ANDed; a rule with no keys at all matches every call.

| Key | Matches |
|---|---|
| `level` | the level the call declared |
| `modality` | the derived modality |
| `prompt` | the DSL prompt name |
| `role` | the **agent's** role slug |
| `actorRole` | the **calling person's** cluster role |
| `tag` | a call tag, such as `background` |
| `touches` | a concept-id prefix the call's footprint matches (`startsWith`) |

`role` and `actorRole` are separate keys on purpose: an operator watching a non-operator agent work is not an operator turn, and a rule that could not tell them apart would route on who is watching rather than on what is acting. A key written empty is a condition matching only an empty value; an **absent** key is no condition at all.

The remaining annotations:

- **`@policy`** — required; the chain this rule selects.
- **`@level`** — optional; overrides the level the call declared. Both the declared and the effective level land on the decision record.
- **`@precedence(N)`** — highest first. **A tie between two rules of the same locked-ness is a load error**, naming both: a tie is resolved by nothing, so the rule that wins would differ between replicas.
- **`@onUnavailable("degrade" | "park")`** — what happens when the chain is exhausted at the level. Unset means degrade. `degrade` walks the chain again one level down (`reasoning` → `strong` → `fast`) and records that it did; `park` returns the refusal with the door report. `fast` is the floor, and **`embeddings` never degrades** — a degraded embedder answers in a different vector space, so the vector does not belong in the index it is about to be written to.
- **`@exclude("fleet:<modelId>")`** — repeatable; removes one concrete model from this rule's resolution.
- **`@locked`** — accepted **only in the embedded tree**. A locked rule evaluates before every unlocked one regardless of precedence, is re-read from the embedded tree on every boot so nothing done to it at runtime survives a restart, and its name cannot be taken by a runtime-authored rule.

The first matching rule wins. **A call that matches no rule is impossible**: the shipped `default` rule states no conditions and cannot be removed, which is why nothing downstream has to handle that case.

Operator-facing detail — the six shipped rules, how to add your own, and how to read what the router decided — is in [AI routing](../operate/ai-routing.md).

### Prompts

AI prompt templates and their input schemas live in `dsl/<namespace>/prompts.memql`. Struct form — the body is a bare input-schema field list, and `@level` is how the prompt says how much intelligence its call needs:

<!-- corpus: 2026/examples/memql/routing/prompt-input.memql -->
```memql
@level("fast")
@templateFile("prompts/planStep.tmpl")
/// Choose the next step for an in-flight run
prompt planStep {
  run       object!   @description("The run row being advanced.")
  steps     []object! @description("Steps already recorded, newest first.")
  phase     string    @description("Phase off the run's own state machine.")
  // one entry per variable the template renders
}
```

Logic prompts (routing / suggest / classification) use the structured-output path (`ChatStructuredProvider.CallChatStructured`); prose prompts (agent replies to users) use regular chat.

**`@level` is required** (see [Levels](#levels) above): a prompt with no level refuses to load, because a guessed level is a routing decision nobody wrote. **`@defaultProvider` survives as an explicit PIN** — it rides the request's explicit-provider field and still wins over every rule — which is why the rule that it may not name a policy still holds. A pin is an override, not the ordinary way to choose: in this repository `TestNoPaidDefault` refuses a prompt pinned to a federated provider, and every concrete provider record shipped here is federated, so a pin naming one routes around the local-first rule the platform ships.

**The body must cover the template, and `@defaultProvider` must name a real provider** (memql#3616). The input schema compiles with `additionalProperties: false` and is validated **before** the template renders, so a variable the `.tmpl` reads but the body omits is a field no caller can ever supply — the load refuses rather than registering a schema that cannot serve its own template. Likewise `@defaultProvider` must name a declared `provider`, never a `policy` slug: a dangling name does not error at call time, it silently falls through to the default provider. A `@disabled` provider still counts as declared. See [authoring rule 28](authoring-rules.md).

Two legacy forms are retired (both rejected at parse time):
- `func (Prompt) name(ctx any) { ... }` — receiver-function wrapping.
- `@input { ... }` — body-level wrapper around the field list.

### Calling a prompt

**A prompt is not callable from a `.memql` body, and there is no bare inference
call either.** A statement's call names one of `query`, `mutation`, `logic`,
`builtin`, `automation` and `action`, and none of those names a prompt; the
only *bare* calls a body admits are catalog functions and the specs and traits
it can see. The parser builds no inference node and the catalog holds no
inference name, so `ai("<promptName>", <dataObject>)` — the blocking-LLM-call
spelling some comments in the tree still point at — is refused at load, with
`[body_call_unknown]` naming the call.

A prompt is rendered and sent from **Go**: an integration binds the values the
prompt's body declares — that body IS the input schema, validated before the
template renders — builds a `core/airoute` request carrying the prompt's
`@level`, and the router picks the provider ([Levels](#levels)). A prompt
declares no OUTPUT schema, so what the reply is read as belongs to the calling
integration, not to the construct. `MemQLEngine.InvokeAI` and the
structured-output path beside it are that seam, and it is where every prompt
this tree declares is used.

A body reaches that work through a **builtin that wraps it**. Two of the
builtins `dsl/agents/builtins.memql` declares invoke an agent, and like every
cross-namespace construct they come in through a file-top import:

<!-- corpus: 2026/examples/memql/prompts/agent-builtin.memql -->
```memql
use agents.builtins.{ agent }

/// Ask the assistant agent a question; the call returns as soon as the run is open.
logic askAssistant {
  args {
    question  string!
  }
  invoked := builtin agent(name: "assistant", prompt: args.question, partitionId: "system")
  return invoked
}
```

`agent(name:, prompt:, partitionId:)` is ASYNCHRONOUS: it opens a
`v1:work:goal` naming the `invokeAgent` template and returns `{goalId, runId}`
rather than the model's answer, and a run dispatcher claims the goal on an
agent node. `runAgentTurn(agentId:, prompt:)`, declared beside it, runs one
agent turn in line and returns its reply — and answers only on an agent node,
which it says rather than returning an empty reply.

Prompt templates are rendered with Go's `text/template` package. When embedding structured data in a template that expects JSON, serialize it first (pass JSON-encoded strings in the data object) rather than passing raw maps, which would render in Go's internal map format.

### AI Cache

- `MEMQL_SI_CACHE_DEFAULT_ENABLED` (`true`/`false`) toggles whether prompt calls cache their results when no explicit TTL is provided. The env var keeps the older `SI` spelling.
- `MEMQL_SI_CACHE_MAX_SECONDS` caps any AI cache entry (and doubles as the default TTL when caching is enabled). The engine clamps this to **≤ 300 seconds (5 minutes)**.

The AI cache hashes `{templateId, provider, renderedPrompt}` as the cache key. When caching is enabled, a successful provider response is reused until its TTL expires — preventing duplicate LLM calls for identical prompts.

## Mutations

MemQL follows an **append-only, immutable data model**. Records are never updated in place; instead, new versions are inserted. There are two write surfaces: the runtime `insert()` literal and DSL-defined named mutations.

### Runtime `insert()`

```
insert(
  "v1:examples:world",
  id="world-nebula",
  payload={
    "title":"Nebula Grid",
    "slug":"nebula-grid",
    "status":"active"
  }
)
```

Rules:

1. One `insert()` per statement; no mixing reads and writes.
2. Payload must match the concept schema (validated automatically).
3. Relationship hints (`parent`, `aliasOf`) rewrite the payload before persistence.
4. Inserts return the created node inside `result.bundle` (single node, empty edge list, and `rootIds` containing the inserted ID).
5. Stored identifiers always take the form `<concept>:<id>`; providing a bare `id` argument automatically applies the prefix.
6. The `id` argument must be a string literal or omitted — helper calls like `id=uuid()` are syntax errors. Pre-generate IDs and pass them as strings.
7. **The declared owner field is server-stamped.** When the target concept declares `@rowAuthz(owner="<field>")`, the engine sets `<field>` to the calling actor's user id, *overwriting* whatever the payload supplied. A raw `insert()` short-circuits the planner and never renders a mutation template, so the `accept { }` / `stamp { }` blocks that would otherwise set it never run — without this the raw surface could create a row owned by somebody else, and `@rowAuthz(owner=...)` would be an assertion the write path does not keep (memql#3059 / #3175). Two callers are exempt and write the owner they supply: the cluster owner, and trusted server-side Go stamping internal origin for that one write. A call carrying no resolved caller identity is refused rather than stamped with an empty owner. Named mutations are unaffected — their own `stamp { }` block is the author's stated answer.

### Content-Addressed IDs

When no `id` is provided, MemQL generates a **deterministic content-addressed ID** derived from the concept name and payload using SHA256. This provides:

- **Idempotent inserts**: The same payload always produces the same ID, preventing accidental duplicates
- **Reproducibility**: Given a payload, you can predict or verify its ID (see `contentId()` / `previewInsert()`)
- **Natural deduplication**: Identical content maps to the same record

```
-- No id specified: ID is derived from concept + payload
insert("v1:lead", payload={"name": "John", "email": "john@example.com"})
-- Returns: v1:lead:a3f8b2c1d4e5f6... (64-char hex hash)

-- Running the same insert again produces the same ID
-- This creates a new version of the same record, not a duplicate
```

The generated ID is a 64-character hexadecimal SHA256 hash. An optional server-side salt (configured via the content-ID salt env var, `*_CONTENTID_SALT`) can be added for deployment isolation. Explicit `id` parameters always take precedence over content-addressed derivation.

**Identical payloads create versions, not new records.** Inserting the same payload without an explicit ID creates a new *version* of the existing record:

| Goal | Solution |
|------|----------|
| Create multiple independent records | Use unique values in the payload (different names, UUIDs, etc.) |
| Update an existing record | Insert with the same payload/ID (this is the intended pattern) |
| Ensure uniqueness | Pass an explicit `id` parameter |

### Versioning via Insert (The "Update" Pattern)

There is no in-place update by design. To change a record's state:

1. **Insert a new version** with the same ID but updated payload fields
2. **Query to retrieve the most recent version** of each record (queries always return current state)
3. **Full history is preserved** and queryable via `asOf()`

```
-- Original lead (unclassified)
insert("v1:lead", id="lead-123", payload={"name": "John", "email": "john@example.com"})

-- "Update" by inserting a new version with the same ID
insert("v1:lead", id="lead-123", payload={"name": "John", "email": "john@example.com", "classification": "hot"})

-- Query current state (returns the classified version)
concept==v1:lead && id=="lead-123"

-- Query unclassified leads (current version missing classification field)
concept==v1:lead && classification==nil
```

**Soft deletes:** to "delete" a record, insert a version with `active: false`.

**Why append-only?**

| Benefit | Description |
|---------|-------------|
| **Audit trail** | Complete history of all changes with timestamps and actors |
| **Time travel** | Query data as it existed at any point: `asOf(expr, "2025-01-01T00:00:00Z")` |
| **No data loss** | Records are never destroyed; "deletes" are soft (set `active: false`) |
| **Determinism** | Same query + same timestamp = identical results, always |

### DSL Mutations (Struct Form)

Named mutations live in `dsl/<namespace>/mutations.memql`. The concept binding lives in the signature (`mutation <Concept> <name>`); the body carries an `args { ... }` block plus exactly one `insert { ... }` **or** `update { ... }` block (one write per body):

<!-- corpus: 2026/examples/memql/mutations/archive-folder.memql -->
```memql
use library.concepts.{ folder }

/// Insert a new version of a folder record (typically used to archive a folder).
@actor
mutation folder archiveFolder {
  args {
    folderId  string  @required
    payload   object  @required
  }
  insert {
    id: args.folderId
    ownerUserId: actor.userId
    args.payload
  }
}
```

- `insert { ... }` writes a new row of the signature concept; `update { id: ..., ... }` is the partial-update counterpart for read-merge-validate-write flows.
- The preferred body is the accept/stamp form: `accept { a, b }` lists caller-accepted public fields (each auto-binds its same-named declared arg -- load-validated) and `stamp { key: value }` carries the server-set fields. Never mix loose fields beside a nested accept/stamp -- the desugar rebuilds the body from the blocks alone and rejects the mix.
- Longhand: a bare `args.X` entry spreads the field under its own name; `name: <expr>` assigns explicitly.
- Engine-provided names are available in the body: `now` (RFC3339 timestamp captured at eval start), `actor.userId` / `actor.role` / `actor.identityId` / `actor.isClusterOwner`, `partition`, and allow-listed `config.X`.
- A value is any in-process expression: `"si-" + hash(args.agentId)`, `canonicalId(args.folderId, "folder")`, `args.title ?? "Untitled"`, `args.pinned ? "top" : "normal"`. The current time is the bare reserved `now` (no call parens).

## Specs

Specs are atomic boolean predicates, declared in `dsl/<namespace>/specs.memql`. A spec **binds exactly one shape XOR concept in its signature** (`spec <boundName> <name>`, resolved via the file-top `use` import), and its body is a lambda over what it binds: `= row => <predicate>`. The binding picks the evaluation strategy:

- **Row-specs** bind a concept or a `@row` shape. The body reads the row through its parameter (`row.archived`) and compiles into the SQL `WHERE` fragment of every query that applies it.
- **Context-specs** bind an `@actor` shape, the only gateway to the auth envelope. The parameter is spelled `actor` and is the envelope (`actor.role`); the body evaluates in process against the caller, and a query applies it to the actor: `requiresOwner(actor)`.

A spec reads only what its binding provides, through its parameter: the bound concept's fields and intrinsics, or the keys the bound shape projects. The `@shape("name")` annotation is **removed**; the binding moved to the signature.

A spec or trait over rows does not read the `actor` root. It is the same predicate for every caller -- applied in any query, cached, composed -- and the ownership test is what row-authz looks for in a query's own filter, where a spec would hide it. `spec note isCallersNote = row => row.ownerUserId == actor.userId` is refused at load (`lower_actor_in_row_predicate`): compare in the query filter (`row.ownerUserId == actor.userId`), or ask the actor question with a context-spec over an `@actor` shape.

Both kinds are declared in one file, and every `use` import sits at the file
top:

<!-- corpus: 2026/examples/memql/specs/specs.memql -->
```memql
use library.concepts.{ artifact }
use common.shapes.{ actorEnvelope }

/// Matches archived artifacts
spec artifact artifactIsArchived = row => row.archived == true

/// Actor is acting on their own behalf rather than through a delegation.
///
/// A ROLE comparison is deliberately not the example (epic memql#5166): a slug
/// comparison cannot see a role a cluster authored for itself, so a role
/// question is `@requiresRank("<slug>")` or
/// `@requiresCapability("<verb>", "<resource>")` rather than any spec.
spec actorEnvelope isSelfActing = actor => actor.identityId == actor.userId
```

### Traits

A `trait` is the one deliberately **unbound** row predicate — a cross-concept scaffold declared in `dsl/<namespace>/traits.memql` with no signature binding. Its body reads the row through its parameter, and the fields it reads are checked against the concept of each query that applies it:

<!-- corpus: 2026/examples/memql/specs/trait.memql -->
```memql
/// Matches records with archived==true field
trait isArchivedRecord = row => row.archived == true
```

When a trait covers a predicate (e.g. the shipped `isActiveRecord` for
`row.active == true`), **using the trait is mandatory** in authored query filters
— the inline comparison is rejected by the conformance test
(`test/dslconformance/conformance_test.go`). A trait name is resolved by bare
name everywhere it is applied, so a second declaration of a shipped one does not
shadow it politely: the name stops resolving, and every filter that applies it
is refused with `lower_unknown_name` — ~49 shipped queries, for
`isActiveRecord`.

### Using Specs and Traits in Filters

A spec or trait is applied, like a call, to the value it predicates over: a row predicate to the filter's row, a context-spec to `actor`:

<!-- corpus: 2026/examples/memql/specs/using-specs.memql -->
```memql fragment
filter  row => row.folderId == args.folderId && isGeneratedOutput(row) && isActiveRecord(row)
```

During load the engine resolves every application into the predicate's body over that value, so the resulting query plan behaves exactly as if the body were written inline. Spec dependencies are resolved at load; cycles and duplicates are rejected.

> **Retired forms.** The `spec <boundName> <name> { return <bool> }` and `trait <name> { return <bool> }` bodies, a spec named as a bare conjunct (`&& isActiveRecord`) or as `spec <name>`, the receiver-function spec (`func (Spec) name(ctx any) bool { ... }`), the `@shape("name")` pin, the JSON spec format (`specs/v1/*.json` documents with an `expression` string), and runtime inline spec definitions (`name := expr` inside a query string) are all retired and rejected. Declare `spec <boundName> <name> = row => <predicate>` and apply it: `<name>(row)`.

## Queries (Named Functions)

Named queries are reusable, parameterized reads, declared in struct form in `dsl/<namespace>/queries.memql`. The signature `query <Concept> <name>` binds the concept; cross-file dependencies (concepts, shapes, traits, specs) come in via file-top `use` imports:

<!-- corpus: 2026/examples/memql/queries/query-syntax.memql -->
```memql
use library.concepts.{ artifact }
use library.shapes.{ artifactFull }
use common.traits.{ isActiveRecord }

/// Get artifacts filed under a folder
query artifact artifactsInFolder {
  args {
    folderId  string
    lens      string  @enum("artifact", "record")
    kind      string  @enum("document", "note", "file")
  }
  filter  row => (args.folderId == nil || row.folderId == args.folderId)
              && (args.lens == nil || row.lens == args.lens)
              && (args.kind == nil || row.kind == args.kind)
              && isActiveRecord(row)
  shape   artifactFull
}
```

A long filter continues on the lines below it: a line that opens with `&&`,
`||` or `??` joins the one above it, as the example shows.

Body directives: `filter` (the predicate), `shape` (named projection), and optional `sort "field", "dir"` / `paginate N` / `refine row => ...` lines ([the refine clause](#the-refine-clause)):

<!-- corpus: 2026/examples/memql/queries/sort-paginate.memql -->
```memql
use work.concepts.{ run }
use work.shapes.{ workRunFull }

/// Returns the latest run recorded against a given goal
query run newestRunForGoal {
  args {
    goalId  string  @required
  }
  filter  row => row.goalId == args.goalId
  sort    "row.createdAt", "desc"
  paginate 1
  shape   workRunFull
}
```

### Temporal queries (`asOf`)

Time-travel is a **query-only** clause (alongside `filter` / `shape` / `sort` / `paginate`); it is rejected in logic / automation / spec bodies, which never time-travel directly. Two forms:

<!-- corpus: 2026/examples/memql/queries/as-of-forms.memql -->
```memql
use cluster.concepts.{ node }

/// The cluster's healthy nodes, as they stand right now.
query node liveNodes {
  asOf latest
  filter  row => row.health == "healthy"
  paginate 50
}

/// The nodes the cluster held at the start of 2026.
query node nodesAt {        // asOf <ts> -> deterministic, no marker
  asOf "2026-01-01T00:00:00Z"
  paginate 50
}
```

- `asOf latest` reads current (clock-dependent) state. The engine DERIVES time-dependence from the `asOf latest` clause itself, so the query needs no marker -- `@latestMode` restated it (and could contradict it) and was retired in epic memql#5375.
- `asOf <explicit timestamp>` reads immutable historical state — deterministic, so it needs no marker.
- **A caller-chosen instant is spelled `args.<name> ?? latest`** (memql#2992). A declared query can offer one; the fallback is required, so the bare `asOf args.at` is rejected at parse with a message naming the fix (memql#3028). See the caller-instant example above. *This bullet used to say the timestamp had to be a literal and that `args.X` was rejected outright — true before #2992, and stale since; it quoted an error string the parser no longer emits.*

  To read at a caller-supplied instant *without* declaring the argument, wrap the query in a **runtime query string** composed by the caller (Go/SDK/MCP) — not in `.memql`, where `asOf` is a query-only clause and a body using it is rejected:

  ```
  asOf(deploymentById(deploymentId:"d-abc"), "2026-07-28T12:00:00Z")
  ```

  **The wrapped query must declare no `asOf` clause of its own** — otherwise this fails with `multiple asOf() directives are not supported`. That rules out any query declaring `asOf` at all — `latest` or a literal — so both `liveNodes` and `nodesAt` above are unwrappable; `deploymentById` is the tree's worked example precisely because it declares none, and `TestDeploymentByIdRemainsWrappableInAsOf` guards that. Declaring a caller-chosen instant instead of wrapping is the `args.<name> ?? latest` form above (memql#2992, fallback required per memql#3028) — this sentence used to describe that as future work.

### Imports

Every construct another file pulls into local scope is declared via a dotted-path import at file top:

<!-- corpus: 2026/examples/memql/imports/use-imports.memql -->
```memql fragment
use library.concepts.{ artifact, folder }
use library.shapes.{ artifactFull }
use common.traits.{ isActiveRecord, isNotDeleted }
```

The dotted path maps to a file on disk (`library.concepts` → `dsl/library/concepts.memql`); the brace list names the constructs imported into local scope.

#### Aliasing an import (`as`)

An imported name can be bound to a different local name (memql#3802):

<!-- corpus: 2026/examples/memql/imports/alias.memql -->
```memql
use observability.concepts.{ invocation as codeInvocation }

/// This domain's own invocation record.
concept invocation {
  ownerUserId  string!
  toolName     string!
}

/// Slow code invocations -- the imported one.
query codeInvocation slowCodeInvocations {
  args {
    minDurationNs  int  @required
  }
  filter   row => row.durationNs >= args.minDurationNs
  paginate 50
}

/// The caller's own invocations -- `invocation` stays ambient.
@actor
query invocation invocationsForCaller {
  filter   row => row.ownerUserId == actor.userId
  paginate 50
}
```

This is what lets one file reference **two same-named concepts**. Four short
names are ambiguous across domains today — `account`, `call`, `invocation`
and `request` — and without an alias, importing one of them captures *every*
bare use of that name in the file, including the constructs that wanted their
own domain's.

Three rules, and the third is what makes the first two coherent:

1. **The alias names the imported concept.** `codeInvocation` above is
   `v1:observability:invocation`.
2. **A bare name stays ambient** — it continues to mean this domain's concept.
   Aliasing therefore fixes the capture structurally rather than by adding a
   check.
3. **An unaliased import that collides with this domain's own concept is
   refused at load**, naming the alias as the fix. Without that refusal the
   capture stays writable and still silently wins — and *silently* is the
   problem: both constructs compile, both bind the foreign concept, and nothing
   is reported.

An unaliased import that collides with nothing is unchanged and needs no alias;
that is the shape of every cross-domain import in the tree today.

`as` is defined on `use` generally rather than on concepts specifically, so it
already works the day another construct kind becomes namespaced. It is **inert
for the twelve flat kinds** (query, mutation, logic, spec, trait, shape, tool,
prompt, provider, builtin, policy, seed): their registries are keyed by bare
name and by nothing else, so there is no second key for an alias to bind and
nothing to alias between.

**What a second declaration of one of those names does is NOT uniform, and none
of the twelve refuses it outright.** A `provider`, `policy`, `builtin`, `tool`,
`shape` or `prompt` loads with no diagnostic at all, and one of the two simply
wins the key. A `query`, `mutation`, `logic`, `spec` or `trait` loads too, and
the damage lands on its CONSUMERS instead: every construct that named it by
bare name stops resolving. A `seed` has not been measured. (A `rule` IS refused
at load naming both files -- "a rule name is unique across the whole corpus" --
but a rule is not one of the twelve: its registry is keyed differently.) Declaring a second `trait isActiveRecord` refuses
~49 shipped queries with `lower_unknown_name`; declaring a second
`query todos` leaves the shipped `todosList` tool's handler naming a function
that is "not a registered function, query, mutation or builtin". Treat the
names as unique because the tree does, not because a gate makes them so.

> **Retired.** The `@use*` annotation family (`@useConcept`, `@useShape`, `@useQuery`, `@useMutation`, `@useLogic`, `@useBuiltin`, ...) is retired and rejected at parse time with a migration-pointing error. The concept binding lives in the construct signature; everything else comes in through `use` imports.
>
> The old **Form A** alias (`use library.folder as f`) is also retired: the
> alias goes inside the brace list now, `use library.concepts.{ folder as f }`.

### Doc Comments (`///`) -- the preferred description spelling

A `///` doc-comment block immediately above a declaration is captured by the
parser and attached to that declaration (#2633) -- for every describable
construct kind (`concept`, `query`, `mutation`, `logic`, `automation`,
`tool`, `shape`, `capability`, `prompt`, `provider`, `policy`, `spec`,
`trait`, `action`) and for `args {}` block fields. Capture semantics
(rulings 1-2, `docs/internal/design/doc-comments-description-source.md`):

- **Attachment**: the block attaches to the immediately following
  declaration. Annotations between the block and the declaration are
  transparent (`///` above `@mcp` above `query x` documents `query x`), as
  are ordinary `//` comments. A **blank line breaks attachment**; a detached
  block is an ordinary comment -- ignored, never an error. Exactly three
  slashes: `////` divider art is not a doc comment.
- **Join**: strip `///` plus exactly one following space per line, join
  consecutive lines with a single space; a bare `///` line is a paragraph
  break (newline). Extra indentation after the first space survives.

Sourcing (#2634, in force; gated #2636): the `///` doc comment IS the
description and the PREFERRED spelling -- the engine tree's conformance
gate rejects `@description` where `///` suffices (including a bare
`@description` shadowed by a `///` block), and downstream trees convert
with `memqlmigrate --rewrite=doc-comment-descriptions` at their repin.
Aim for ~200 characters (editorial target; sense emits a
hint-severity `description-length` diagnostic over the target, #2703). It
**wins** over `@description` whenever both are present (never
concatenated), and `@description` remains the fallback, so
annotation-only files behave exactly as before. This feeds every
description surface: `functions()`/`tools` discovery, MCP tool
descriptors and input schemas (args-field `///` docs appear as
`properties.<name>.description`), the promote-time catalog embedding,
SDK-generated Go/TS docs (construct and arg), and sense hover.

### Argument Declaration and Resolution

`args { ... }` field syntax: `<name> <type>[!] [@maxLength(N)] [@pattern("re")]`. The `!` sigil marks the field required (#2618; the `@required` annotation keeps parsing); omitting it makes the field optional. `enum("a", "b")` is a first-class type -- the self-contained spelling of the legacy `string @enum(...)` pair. Do not write `@description` on an args field -- the parser REJECTS it at load (memql#3336), because there is no AST slot for it; an arg description is a `///` doc comment on the line(s) immediately above the field (#2633). A `tool` / `prompt` / `builtin` field keeps its `@description` (those bodies ARE the schema), and the declaration-level `@description` on the construct itself is load-bearing.

> **`@default` is not valid on an args field** (rejected at load, #991), and since memql#5375 it is not valid on a **concept** field either (`annotation_retired`; see [Concepts](#concepts) above). Apply a default in the body with the `??` blank-coalescing operator (`args.X ?? <default>`; it falls through on a blank or whitespace-only string as well as on absent/null — see [authoring-rules.md §28](authoring-rules.md)). It **stays** on a `tool` / `prompt` / `builtin` field, where the body IS the schema a model reads.

How names resolve inside a body:

| Name pattern | Source | Available in |
|---|---|---|
| `args.X` | Caller-passed arg declared in `args { ... }` | every body |
| `actor.X` | Resolved auth context (`userId`, `role`, `identityId`, `isClusterOwner`) | every body |
| `now` | RFC3339 timestamp captured at eval start | every body |
| `partition` | Active partition for this call | every body |
| `config.X` | Allow-listed config | every body |
| `row.X` | Row payload property, through the lambda parameter | a filter, a refine clause, a spec or trait body, a trigger filter |
| `row.id`, `row.concept`, `row.type`, `row.createdAt`, `row.createdBy`, `row.provenance.<leaf>` | Row intrinsics, through the same parameter | the same positions |

A bare name is never a payload property: see [the lambda parameter and bare names](#the-lambda-parameter-and-bare-names). A shape body is the exception, because it is a list of paths rather than an expression: there a bare `name` is the payload property.

**Reserved engine names.** `now`, `actor`, `partition`, `config`, `trace` are reserved as top-level identifiers. An `args` field that collides with one of these names is rejected at load time.

> **Retired.** The `ctx` envelope is gone from the author surface — no `ctx.input.X`, no `ctx.X` shorthand, no `ctx.output =` assignment. Authors read caller args as `args.X` and return values directly.

### Calling Functions at Runtime

A client invokes a named query or mutation in the internal query form, as a call with named arguments; empty parentheses `()` mean no args:

```text
-- No args (returns all matching records)
activeFolders()

-- With filters
artifactsInFolder(folderId: "folder-456", lens: "artifact")

-- Combine with directives
sort(artifactsInFolder(folderId: "f-1"), "createdAt", "desc")
paginate(activeFolders(ownerId: "u-1"), 10)
```

Inside a `.memql` body a call keeps its kind prefix and names every argument: `rows := query activeFolders(ownerId: args.ownerId)`. The pun that let a bare name stand for an argument of the same name (`logic record(event)`) is retired; write `logic record(event: event)`.

### Argument Validation

Arguments are validated against the function's `args { ... }` schema at runtime:

- **Type validation**: Ensures argument types match (string, number, boolean, etc.)
- **Enum validation**: Rejects values not in the `@enum(...)` set
- **Required fields**: Returns error if `@required` arguments are missing
- **Additional properties**: Rejects unknown arguments

Example validation errors:

```json
{
  "error": "function 'activeFolders': argument validation failed: status: expected string"
}
```

```json
{
  "error": "function 'artifactsInFolder': argument validation failed: lens: value must be one of \"artifact\", \"record\""
}
```

### Function Rules

- One consolidated `queries.memql` / `mutations.memql` file per namespace. The declaration name carries **no kind prefix** (memql#2853) -- name it for what it does (`activeFolders`, `createFolder`); the `query` / `mutation` keyword already marks the kind. See [naming-conventions.md](naming-conventions.md).
- Functions can reference specs and traits (loaded after specs) and call other functions; circular dependencies are detected and rejected at load time.
- Comments use `//`; construct descriptions come from `@description("...")`.

### Procedural Form (internal post-rewrite shape)

The struct-form rewriter expands a query and a mutation to a `func (Receiver) NAME(ctx any) (any, error)` shape for the engine's parser; the `ctx` parameter name is a placeholder identifier only. **Don't author that form**: a query and a mutation are written in the struct form above, and a logic and an automation in the [body language](#bodies), which the parser reads as written. Receiver-function constructs in authored files are rejected at parse time with migration hints.

## Bodies

A `logic` and an `automation` are written in one body language: statements, which run in the order they are written. A logic is a name, an optional `args { }` block and statements, the last of them a `return`. An automation is a trigger (or `@template`), an optional `@filter`, an optional `args { }` block, optional `precondition` blocks, and the same statements. There is no `body { }` wrapper and no `step` block.

<!-- corpus: 2026/examples/memql/bodies/logic-ternary.memql -->
```memql
/// Route a submitted request by the submitter's role.
logic submittedRequestStatus {
  args {
    submitterRole any
  }
  role := args.submitterRole ?? ""
  return role == "owner" ? "queued" : role == "admin" || role == "writer" ? "needs_approval" : "needs_validation"
}
```

### Statements

| Statement | Form | Notes |
|---|---|---|
| bind | `name := <call>` or `name := <expression>` | `name` holds the value from the next statement on |
| call | `<kind> <name>(<named arguments>)` | the kind is one of `query`, `mutation`, `logic`, `builtin`, `automation`, `action` |
| if | `if <cond> { } else if <cond> { } else { }` | `else` goes on the line of the closing brace |
| for | `for <x> in <expression> [if <cond>] { }` | the author names the loop variable |
| switch | `switch <expression> { case <literal>[, <literal>] { } default { } }` | labels are literals, each used once |
| parallel | `parallel { branch <label> { } ... } [wait any]` | waits for every branch unless `wait any` is written |
| publish | `publish "<topic>" { <field>: <value>, ... }` | automations only; the payload is a map literal |
| return | `return [<expression or call>]` | ends the body, and in an automation the run |

One statement per line. An expression continues onto the next line when the line ends inside an open bracket or on an operator, or when the next line begins with one.

A bind and an `if` chain, inside an automation body:

<!-- corpus: 2026/statements/automation/else/else-if.memql -->
```memql fragment
  pending := query pendingBackorders()
  if pending.count() > 10 {
    mutation flagBacklog(level: "many")
  } else if pending.count() > 0 {
    mutation flagBacklog(level: "some")
  } else {
    mutation flagBacklog(level: "none")
  }
```

A `for` over a query's rows, with a filter clause and a nested bind:

<!-- corpus: 2026/statements/automation/for/for.memql -->
```memql fragment
  lines := query lineItems()
  for item in lines if item.quantity > 1 {
    total := logic lineTotal(quantity: item.quantity)
    mutation recordLine(sku: total > 4 ? item.sku : "small")
  }
```

**A construct call is a statement of its own**: the whole right-hand side of `:=`, the whole value of `return`, or a line by itself. A call nested inside an expression is refused, because a side effect that is not a statement is not journaled, previewed or retried. Arguments are always named (`logic triple(n: 14)`). A bare call inside an expression is a catalog function (`lower(x)`, `addDuration(now, "PT1H")`) or a spec or trait predicate; anything else is refused at load (`body_call_unknown`), since it could only fail when it runs.

### Trailing clauses

A statement can close with clauses, in this order when several are written:

| Clause | Written on | Means |
|---|---|---|
| `on surface("<name>")` | an `action` call | runs the action on that surface |
| `retry(<n>)` | a call | retries a failed call up to `n` more times |
| `on error continue` | a call, a `for`, a `parallel` | records the failure, leaves the name absent, and goes on |

Stopping the run on an error is the default and is never written.

<!-- corpus: 2026/statements/automation/retry/retry.memql -->
```memql fragment
  unpaid := query unpaidInvoices() retry(3)
  mutation markInvoiced(note: "counted") retry(2) on error continue
```

`on surface(...)` is the `action` clause, and goes first when several are
written:

<!-- corpus: 2026/statements/automation/onSurface/on-surface.memql -->
```memql fragment
  action deployRun(target: "cluster") on surface("ops")
```

### Names and scope

A bare name is a statement's name, a loop variable, a lambda parameter or a reserved root: `args`, `actor`, `now`, `config`, `partition`, and in an automation `event`. `trace` is reserved and nothing binds it, so reading it is refused. An argument is read `args.<name>` in both keywords; a bare argument name is refused.

- A name is bound once, and read only after the statement that binds it. Reading it earlier is refused, naming both lines (`body_forward_reference`).
- An `if`/`else` branch or a `switch` case runs at most once, and shares the scope around it. A name bound in a branch that did not run reads absent. The branches of one chain may each bind the same name; whichever runs binds it.
- A loop body and a parallel branch have their own scope. A name bound inside exists only there, and it may not shadow a name or a root outside.
- This remains true when `parallel` waits for all branches: waiting does not export their names. Keep dependent statements in the same branch, or run them sequentially when a later statement needs their results. `wait any` uses the same scope rule.
- An automation reading `actor` declares `@actor`; every `args.<name>` it reads must be declared in its argument schema. These rules are checked when its statements compile.
- `config.<key>` reads the configuration allow-list (`component/config`). A key the list does not hold is refused at load (`body_config_unknown`) rather than read as absent.

### What a logic may do, and what an automation may do

- A logic calls `query`, `mutation`, `logic` and `builtin`. `publish`, `automation` and `action` are an automation's, and a logic that uses one is refused (`body_publish_in_logic`, `body_call_not_in_logic`). A logic's last top-level statement is a `return` (`body_logic_return`).
- An automation makes every call, publishes, and may `return` to end its run early. The value it returns is recorded on the run.
- `return` is refused inside a parallel branch (`body_return_in_parallel`).

### What runs

A body compiles at load to a list of steps in the order written; nothing is reordered. Each call is one step: journaled on the run, previewed by a dry run, retried by `retry(n)` and by a resume. An `if` or a `switch` flattens into the steps of its branches, each carrying its branch's condition, so a switch compares with typed equality (`1 == "1"` is false). A logic called inside a run journals its statements as steps of that run.

A dry-run preview executes a builtin only when its executor is classified as a metadata read or a computation without side effects. Unclassified executors, including integration builtins, stop the preview with a refusal. This also applies to builtins called from a query or nested logic; a stopped preview does not claim successful execution.

A statement's value depends on its kind:

- a query's is its rows: each row reads `row.id` and the other intrinsics from the row, and every other name from its payload (`rows.first().email`);
- a mutation's is the written row;
- a logic's is its return value;
- a builtin's is its result;
- an action's is the result its capability produced.

### Retired forms

These forms are refused at parse, each refusal naming its replacement, and `memqlmigrate --rewrite=bodies` rewrites a tree that still holds them:

| Retired | Written now |
|---|---|
| `body { ... }` around a logic's statements | the statements, after `args { }` |
| `step n { <call> }` | `n := <call>` |
| `steps.n.result.f`, `n.result.f` | `n.f` |
| `automation a @trigger(...) => logic l` | an automation whose one statement is `logic l(event: event)` |
| a bare argument, or `event.payload.f` in an automation | `args.limit`, `args.f` |
| `for item := range x.nodes()`, `forEach x in s where f { }` | `for x in s if f { }` |
| `parallel { wait: "all", branches: [step a { }] }` | `parallel { branch a { } }` |
| `logic l(event)`, an argument named by its value | `logic l(event: event)` |
| `publishEvent(topic: "t", payload: { ... })` | `publish "t" { ... }` |
| `x := if c { <call> }` | `if c { x := <call> }` |
| a logic that publishes | its statements, moved by the rewrite into the one automation that calls it |
| `@trigger(..., partition="*")` | `@trigger(...)`: the kwarg is deleted |
| `@schedule(cron="<cron>")` | `@trigger(schedule="<cron>")` |
| `mutate <Concept> <name> { ... }` | `mutation <Concept> <name> { ... }` |

The step bodies' accessors are refused at parse too (`body_accessor_retired`), and the rewrite leaves them to the author: `step("n")` is `n`, `input()` is `args.<name>`, and `item()` and `index()` are the loop's own name, `for x in s` -- a loop has no index.

The old `error()` accessor is also refused: statement bodies have no onError context to read. Use the catalog function `error("message")` to raise an error; it requires no builtin import.

## Logic

A logic is a procedure an automation or another logic calls. It reads its arguments as `args.<name>`, runs its [statements](#bodies) in order, and ends with `return`:

<!-- corpus: 2026/examples/memql/logic/sweep.memql -->
```memql
/// Daily sweep: delete the archived folders whose retention window has elapsed.
logic purgeExpiredArchivedFolders {
  args {
    olderThan  datetime
  }
  expired := query expiredArchivedFolders(asOf: args.olderThan ?? now)
  for folder in expired {
    mutation deleteFolderNow(folderId: folder.id)
  }
  return expired.count()
}
```

Every field an `args { }` block declares has to be read somewhere in the body,
and every `args.<name>` a body reads has to be declared: both directions are
refused at load, so a declared-and-unread `event` is a load error rather than a
silently ignored line.

`now` above is the bare reserved current-timestamp primitive: it resolves to the clock in every body with no import and no call parens. The `now()` / `timestamp()` call forms are **retired** and rejected at parse time.

**Rows.** A query's value is a list of rows, which a `for` iterates and the collection library (below) filters, orders and aggregates: `expired.count()`, `expired.first()`, `expired.empty()`. `x.nodes()` returns the same rows as a list.

> **Retired.** The capitalized accessors `.First()` / `.Nodes()` / `.Len()` / `.Empty()` / `.Count()` / `.Last()` are retired in favor of the lowercase spellings above (`.Len()` becomes `.count()`).

### Collection / lambda library

A single method-chained collection library with **arrow lambdas** filters, projects and aggregates a list: a query statement's rows, an `args` list, a `select` projection, a row's array field. A chain is an expression, so it is the right-hand side of a bind rather than a statement of its own:

<!-- corpus: 2026/examples/memql/logic/collections.memql -->
```memql fragment
  // active admins among the passed members
  activeAdmins := args.members.where(m => m.role == "admin" && m.active).count()

  // newest active node
  newest := nodes.where(n => n.status == "active").orderByDesc(n => n.createdAt).first()
```

Arrow lambdas are `x => expr` (one param) and `(acc, x) => expr` (two, for `reduce`). The method surface, all over a list receiver:

| Group | Methods | Returns |
|---|---|---|
| Filter / project | `where(x => bool)`, `select(x => v)`, `distinct(x => k?)`, `take(n)`, `skip(n)` | list |
| Order | `orderBy(x => k)`, `orderByDesc(x => k)` | list |
| Pick one | `first()`, `last()`, `single()` | item |
| Test | `any(x => bool)`, `all(x => bool)`, `empty()` | bool |
| Aggregate | `count()`, `sum`/`min`/`max`/`avg(x => n)`, `groupBy(x => k)`, `reduce(seed, (acc, x) => v)` | number / groups / any |
| Rows | `nodes()` | list |

To take the first match, filter first: `xs.where(x => x.active).first()`. Membership is `v in list`: the `contains(v)` method is retired. Every method's signature and what it does with an absent list is in the [function catalog](functions.md#catalog).

Rules:

- **Pure lambda bodies.** A lambda may read `now`, `actor` and the names around it and call pure operators, but a mutation or action inside a lambda is a load error.
- **Where it runs.** In process everywhere, except that `any`, `all` and `count()` over a row's array field push down in a filter or spec body (`row.tags.any(t => t == "urgent")`). Every other method, in a filter, takes only a list that does not read the row, such as an argument ([plan constants](#plan-constants)). A lint also warns when a `where()` runs over an unfiltered full-concept fetch: that belongs in a query `filter`, not an in-memory scan.

This library replaces the retired function forms (`first(x)`, `last(x)`, `count(x)`, `len(x)`) and the retired capitalized result-set methods. (The `count` **query directive** used inside a `query { ... }` body for SQL is a separate surface and still exists.) The full construct matrix lives in the ADR: [core-builtins-and-collections-adr.md](../../internal/design/core-builtins-and-collections-adr.md).

### In-memory arithmetic

Binary `+` `-` `*` `/` `%` run in process. In a filter or spec body they are [plan constants](#plan-constants): `row.rank > args.floor * 2` pushes down, and `row.rank + 1 > 2` is refused (write `row.rank > 1`). Subtraction requires spacing — write `a - b`, not `a-b` — so hyphenated identifiers and ids (`bff-local`) are preserved. `+` also joins strings, which is what `concat(...)` used to do: `"si-" + hash(args.agentId)`. This enables aggregates like `reduce(0, (acc, n) => acc + n)` and ratios like `spent / budget`.

## Builtins

Builtins wrap Go integrations behind a declarative schema, so they look like regular DSL function calls. Struct form with an `@executor` annotation naming the Go-side capability; the body is the input-schema field list:

<!-- corpus: 2026/examples/memql/builtins/builtin.memql -->
```memql
@executor("integration.auth.checkPermission")
/// Check if the current authenticated user has a specific role. Returns boolean result.
builtin authCheckPermission {
  role  string  @required
}
```

The introspection commands (`concepts`, `memqlDocs`, `functions`, `help`, `contentId`, `previewInsert`, ...) are themselves builtins declared in `dsl/common/builtins.memql` with engine-internal executors. Builtins are callable from runtime query strings and from logic bodies.

## Tools

Tools are AI-callable tool definitions — the AI-facing surface of queries, mutations, and builtins. Struct form; the body is a list of input-schema fields with types and annotations (`@required`, `@default`, `@enum`, `@description`); `@handler` binds the tool to its backing operation and `@executionTime` hints latency:

<!-- corpus: 2026/examples/memql/builtins/tool-query.memql -->
```memql
/// Search for users
@handler(type="query", query="paginate(query searchUsers(active: args.active), args.limit)")
@executionTime("fast")
tool findUsers {
  active  boolean  @description("Filter by active status")
  limit   integer  @default("10") @description("Maximum number of results to return")
}
```

The tool loop binds tool-call args to handler args and forwards. A query handler is one construct call -- a query, mutation, logic, builtin or automation -- or that call inside `paginate(...)`, as above, and it is parsed when the tool loads: a handler that is not a call, such as a raw filter, refuses the load. It reads each tool argument as `args.<name>`, as in `@handler(type="query", query="query findEvents(title: args.title)")`, and the call is rendered from the arguments' values, so a caller's text is data whatever it contains; an argument the caller did not supply is left out of the call. The `$args.<name>` text substitution is retired: `$args.x`, bare or quoted as `"$args.x"`, refuses the load -- write `args.x` (`memqlmigrate --rewrite=expressions` rewrites both spellings). The legacy `func (Tool)` form is retired; the parser rejects it with a migration hint.

A webhook handler, `@handler(type="webhook", url=..., method=...)`, is written the same way. Its url is one expression over the tool's arguments: a fixed address is a quoted string, and a caller's value is joined with `+`, as in `url="\"https://api.example.com/items/\" + args.id"`. A bare address refuses the load, and the refusal shows it quoted. With no body template the request body is the tool's arguments as JSON; a body template (a tool registered from Go can carry one) is a map whose string leaves are expressions over `args`, fixed text quoted, and an argument the caller did not supply omits its key. `$args.` is refused in a url or a body leaf exactly as in a query handler, with the same replacement.

<!-- corpus: 2026/examples/memql/builtins/tool-webhook.memql -->
```memql
/// Tell the on-call channel
@handler(type="webhook", url="\"https://hooks.example.com/notify\"", method="post")
tool notifyOnCall {
  message  string  @required @description("What to tell the on-call channel")
}
```

## Automations

Automations are event- or schedule-triggered workflows declared in `dsl/<namespace>/automations.memql`, written in the [body language](#bodies):

<!-- corpus: 2026/examples/memql/automations/event-trigger.memql -->
```memql
@trigger(event="node.created", concept="v1:library:file")
/// Indexes a file into the Library the moment its row lands
automation indexLibraryFileOnCreate {
  logic artifactsForIndexedFile(event: event)
}
```

`artifactsForIndexedFile` is a logic of this automation's own domain, so it
needs no import; one in another domain comes in through a file-top
`use <domain>.logic.{ name }` line like any other construct.

### Triggers

| Form | Example | Fires |
|------|---------|-------|
| Node event | `@trigger(event="node.created", concept="v1:library:folder")` | When a node of the concept is created (`node.updated` / `node.deleted` likewise) |
| Custom topic | `@trigger(event="library.artifact.indexed")` | When the named application event is published |
| Lifecycle | `@trigger(event="system.startup")` / `@trigger(event="system.shutdown")` | At engine start/stop |
| Schedule | `@trigger(schedule="0 0 2 * * *")` | On a 6-field cron schedule (seconds first) |

The fields of the triggering event's payload are bound into the automation's `args` block, validated against it before the run starts: declare each field the body reads and read it as `args.<field>`. `event` is the event itself (`event.topic`, `event.kind`), and it is what an automation forwards when a logic wants the whole event: `logic l(event: event)`.

### `@filter` Annotation

`@filter(row => <predicate>)` decides whether a trigger fires the automation. `row` is the row whose event fired the trigger, and the automation runs only when the predicate holds:

<!-- corpus: 2026/examples/memql/automations/filter-annotation.memql -->
```memql
@trigger(event="node.created", concept="v1:data:record")
@filter(row => row.naturalKeyValue != nil)
/// Detects conflicts between new data records and existing confirmed records.
automation noticeRecordConflicts {
  logic recordsConflictingWith(event: event)
}
```

A trigger filter is written in the same expression language as a query filter, but it runs in process, against the one row that fired: every in-process function is available, and `args` holds the automation's declared args, bound from the event payload and validated before the filter runs.

### Loop protection

At load, MemQL builds a directed graph: an edge joins an automation to another
when its writes can fire the other's trigger. A trigger filter removes an edge
only when the written values prove that the filter cannot match. Unknown values
keep the edge. The graph includes mutation and publish calls through logic and
sub-automations; opaque builtins and actions are reported as coverage limits.
An uncovered cycle refuses the load with `[loop_cycle]`. `memqllint` applies the
same check in directory mode. Each graph stratum is the longest-path layer of
its condensed graph; members of a cycle share a layer.

Use a filter that excludes the state your own write produces. For a deliberate
converging cycle, declare `@loop(maxDepth=4, until=row => row.status == "done")`
and include `row.status != "done"` as a top-level conjunct of `@filter`.
Every cycle must pass through an annotated automation. The bound counts that
automation's runs within the chain and cannot exceed the global depth cap.
Reaching the converged state stops through the filter without recording an error.

Each event carries its causation, correlation and chain depth across the mesh.
A root starts at depth zero; its first automation runs at depth one. The default
cap is 16. A run beyond the cap is recorded as failed with
`loop_depth_exceeded` and its chain. Within a correlation, identical reads
suppress an in-flight echo; a new root cause can execute the same row state again.
For event-pure bodies, deduplication uses the declared and referenced payload
fields. Other bodies use the payload with the graph version clock removed.
A per-process budget also limits each (automation, row) to 30 executions per
60-second budget window, covering chains that cross an untracked boundary.

### Execution modes

Modes apply per automation, per process, including sub-automation calls.
Without `@mode`, execution is unbounded parallel subject to the executor's
existing concurrency and budget limits.

| Annotation | While another run is active |
|---|---|
| `@mode(single)` | Skip the new fire. |
| `@mode(queued, max=10)` | Wait in FIFO order; skip when ten fires already wait. Waiting holds no executor slot. |
| `@mode(restart)` | Cancel the previous run and start the new fire. |
| `@mode(parallel, max=3)` | Allow three concurrent runs; skip excess fires. |

`@mode(queued)` uses `MEMQL_AUTOMATION_QUEUED_MODE_DEFAULT_MAX` (10).
`max` is invalid on `single` and `restart`. A synchronous sub-automation call
cannot queue behind its own active ancestor and is skipped. Authored automations
with the same name have separate execution modes and deduplication per owner.

### Before-write adjustments

Use `@trigger(before="create", concept="v1:forge:request")` to adjust the incoming
row before its first version is stored. `before="update"` selects later versions;
`before="write"` selects both. The optional filter sees the merged incoming row.
Bodies run in automation-name order after read-merge and before validation.

Assignments such as `row.status = "queued"` modify a declared field directly.
The body may bind expressions, call read-only logic or queries, use `if`/`else`,
and `return`. It cannot mutate another row, publish, invoke an action, builtin,
or sub-automation, or assign an intrinsic or nested field. These restrictions
are checked through transitive calls. The adjusted row produces one stored
version and the normal write events, with no separate automation run row.

An activated authored before-write automation applies only to its author's own
writes. It runs with client authority and may adjust public payload fields;
server-set, ownership, account-scope and relationship fields are protected.
Activation and deactivation take effect without restarting the writing engine.

The **Cluster → Automations** section in MemQL OS shows loaded platform automations, their graph, filters,
bounds, execution modes and recent depth-related stops. Operators can inspect
the stopped chain to find the write that re-fired the automation.

### Parallel branches

`parallel` runs its branches concurrently and continues when every branch has finished, or, with `wait any`, when the first has succeeded:

<!-- corpus: 2026/statements/automation/parallel/parallel.memql -->
```memql fragment
@trigger(schedule="0 0 * * * *")
automation parallelStatement {
  parallel {
    branch counts {
      low := query lowStock()
      mutation restock(note: low.count() > 0 ? "some" : "none")
    }
    branch levels {
      level := logic restockLevel(n: 2)
      mutation restock(note: level > 11 ? "high" : "low")
    }
  }
}
```

Each branch has its own scope, and a branch label is used once per `parallel`. A failed branch stops the others and fails the `parallel`; closing it with `on error continue` lets the body go on past it.

> **Retired.** The receiver form `func (Automation) name() { ... }` is rejected at parse time, and JSON workflow definitions (`workflows/v1/**`) and the `$var.NAME` variable-substitution machinery they used are gone entirely. The `step` block, the terse `=> logic` header and the other retired body forms are listed under [Bodies](#retired-forms).

## Introspection Functions

MemQL exposes the documentation and concept catalog directly through the expression language so clients (human or AI) can bootstrap themselves dynamically.

These introspection calls are builtins declared in `dsl/common/builtins.memql`. Their names, aliases, and argument contracts are loaded into the function registry at startup; a meta-command dispatch shim recognizes them upfront (before either parser), so they work uniformly across all execution paths.

### `memqlDocs()`

Returns the embedded MemQL guide as a synthetic memory node:

```
memqlDocs()
```

Response payload (truncated for brevity):

```json
{
  "result": {
    "bundle": {
      "nodes": [
        {
          "concept": "memql:docs",
          "payload": {
            "format": "markdown",
            "content": "# MemQL Guide\n..."
          }
        }
      ],
      "edges": [],
      "rootIds": ["memql:docs:memql"]
    }
  }
}
```

Use this when an agent needs to refresh its understanding of the language without shipping the guide alongside every request.

### `concepts()` / `concepts("pattern")`

Lists the concepts (and their schemas) available in the current deployment. An optional pattern argument filters concepts by name (case-insensitive substring match):

```
// List all concepts
concepts()

// Filter concepts by pattern (e.g., all CRM-related concepts)
concepts("crm")

// Combine with paginate() to page through long lists
paginate(ids(concepts()), 5)
```

Each child node includes:

- `metadata`: normalized view of the concept declaration (name, description, relationships).
- `schemas`: JSON Schema objects derived from the concept's field declarations.

Because the result set is synthetic, wrap the call with `paginate()` whenever you expect many concepts.

### `validate()`

Validates a payload against a concept's schema without persisting anything. Useful for agents to check payloads before attempting an insert:

```
validate({"concept": "v1:crm:lead", "payload": {"email": "test@example.com", "name": "John"}})
```

Returns a validation result node with:

- `valid`: boolean indicating if validation passed
- `errors`: array of validation error objects (empty if valid)
- `required`: sorted array of required field names from the schema
- `provided`: sorted array of field names present in the payload
- `schema`: summary including `$id` and property names

Example response for an invalid payload (missing required field):

```json
{
  "payload": {
    "valid": false,
    "errors": [
      {
        "instanceLocation": "",
        "keywordLocation": "/required",
        "error": "missing properties: 'email'"
      }
    ],
    "required": ["email"],
    "provided": ["name"],
    "schema": { "$id": "v1.crm.lead", "properties": ["email", "name"] }
  }
}
```

### `functions()`

Returns a minimal list of every enabled registered function -- queries, mutations, logic, automations, and builtins alike, with `kind` as the discriminator (builtins joined the listing when their lifecycle flag became honest, #2608). Designed for agent discovery with minimal payload size:

```json
{
  "payload": {
    "functions": [
      {"name": "queryActiveSpaces", "description": "Returns active spaces", "kind": "query"},
      {"name": "createLibraryFolder", "description": "Creates a folder", "kind": "mutation"},
      {"name": "similarTo", "description": "Semantic similarity search", "kind": "builtin"}
    ],
    "count": 3
  }
}
```

Each entry includes only `name`, `description`, and `kind`. Use `help(name)` to get full details for a specific function.

### `tools()`

Returns MCP-compatible tool definitions for AI model integration. Each entry includes `name`, `description`, and `inputSchema`:

```json
{
  "payload": {
    "tools": [
      {
        "name": "findUsers",
        "description": "Search for users",
        "inputSchema": {
          "type": "object",
          "properties": {
            "active": {"type": "boolean"},
            "limit": {"type": "number"}
          }
        }
      }
    ],
    "count": 1
  }
}
```

### `help()`

Returns full details for a specific function or tool by name:

```
help("queryActiveSpaces")
help({"name": "findUsers"})
```

For functions, returns `type`, `name`, `description`, `kind`, `enabled`, and `argsSchema`; for tools, returns `inputSchema`, `handlerType`, and `annotations`. Returns an error if no function or tool matches the name.

### `shapeTemplates()`

Lists available shape templates for result projection. Optionally filter by concept:

```
shapeTemplates()                                   -- All shapes
shapeTemplates("v1:library:artifact")             -- Filter by concept (string shortcut)
shapeTemplates({"concept": "v1:library:artifact"})   -- Filter by concept (object)
```

Each entry includes only `name` and `description`. Use `shapeHelp(name)` to get full template details.

### `shapeHelp()`

Returns full details for a shape template by name, including the projected field structure and input schema:

```
shapeHelp("artifactFull")
shapeHelp({"name": "artifactFull"})
```

Agents can use `shapeHelp()` to understand the exact projection before calling a query that uses the shape.

### `contentId()`

Predicts the content-addressed ID that would be generated for a concept+payload combination, without actually inserting the data. Uses the same SHA256 algorithm as `insert()`:

```
contentId({"concept": "v1:crm:lead", "payload": {"name": "Ada", "email": "ada@example.com"}})
```

Returns:

```json
{
  "payload": {
    "valid": true,
    "id": "sha256:abc123...",
    "concept": "v1:crm:lead"
  }
}
```

Error cases return structured error objects:

```json
{"valid": false, "error": "MISSING_REQUIRED_FIELD", "target": "concept"}
{"valid": false, "error": "CONCEPT_NOT_FOUND", "target": "v1:unknown:concept"}
```

### `previewInsert()`

Performs complete preflight validation without inserting: validates payload against schema, predicts the content ID, and checks if a record with that ID already exists:

```
previewInsert({"concept": "v1:crm:lead", "payload": {"name": "Ada", "source": "website"}})
```

Success response:

```json
{
  "payload": {
    "valid": true,
    "id": "sha256:abc123...",
    "exists": false,
    "warnings": []
  }
}
```

Error codes:
- `MISSING_REQUIRED_FIELD` - Required argument (concept) not provided
- `CONCEPT_NOT_FOUND` - Concept does not exist
- `SCHEMA_ERROR` - Problem with concept schema
- `SCHEMA_VALIDATION_FAILED` - Payload doesn't match schema

Agents can use `previewInsert()` to validate payloads before inserting, predict assigned IDs, check existence for idempotent operations, and get detailed validation errors.

### `memqlVersion()`

Returns the engine's language/version metadata. (The legacy `concept==memql:version` compatibility query shape is retired — call `memqlVersion()` directly.)

## Cache Behavior

Query-result caching is **on by default**. A pure read that carries no
cache annotation is cached for **60 seconds** (memql#1970); it is not
opt-in, and it has not been since that epic landed. An author reaches
for an annotation to change that number, not to switch caching on.

| Form | Effect |
|---|---|
| *(no annotation)* | Cached for 60s -- the default backstop for any pure read |
| `@cache(N)` | Cached for `N` seconds. The preferred, positional form (memql#2618) |
| `@cache(ttl="N")` | The older keyword spelling. Still parses; not the form the corpus uses |
| `@nocache` | Never cached. The readable alias for `@cache(0)` |
| `@cache(0)` | Never cached |

Three guards bound what the default will do on its own:

- **A result whose dependencies cannot be named is never cached.** The
  engine caches only when it can name at least one concept the result
  reads, because an entry it cannot name is an entry it could never
  evict.
- **`v1:identity:` is denylisted from the default path.** Authentication
  and authorization state must read live: a revoked session or a
  downgraded role cannot be served from a 60-second-old answer. An
  explicit `@cache(N)` still wins -- the denylist governs what the engine
  will do *for* you, not what an author may choose.
- **`MEMQL_CACHE_MAX_TTL` clamps every resolved TTL** when it is set.

**Freshness is event-driven; the TTL is a backstop, not the mechanism.**
Every write publishes `cache.invalidate.<concept>`. On the writing node
the dependent entries are dropped synchronously, before the write's
response is observable, so a client that re-reads immediately after its
own write never sees the pre-write result. For the rest of the mesh a
single broadcast routing rule carries the event to every node, and each
one evicts through its own dependency index. The 60s TTL is what bounds
staleness if an invalidation is ever missed.

| Environment Variable | Description | Default |
|---------------------|-------------|---------|
| `MEMQL_CACHE_MAX_TTL` | Ceiling in seconds on any resolved result-cache TTL. **`0` means NO CLAMP** -- it does not disable caching, and a hint-free read still caches for 60s. Legacy name `CACHE_MAX_TTL` still accepted (memql#3831). | `0` |
| `MEMQL_SI_CACHE_DEFAULT_ENABLED` | Enable AI response caching | `false` |
| `MEMQL_SI_CACHE_MAX_SECONDS` | Maximum AI cache TTL | `300` |

To turn the default off for one read, annotate it `@nocache`. There is
no global switch that disables result caching, and `MEMQL_CACHE_MAX_TTL=0`
is not one -- setting it that way is the default and clamps nothing.

Cache keys are computed from the normalized query expression (cache hints fold into the canonical form, so two queries that differ only by hint value never share or overwrite cache entries).

> **Retired.** The concept-level cache default (`cacheTTLSeconds` in the old `concept.json`) is retired along with the concept metadata file — caching no longer falls back to a per-concept TTL. The `@cache(...)` / `@fields(...)` annotations on runtime query strings are rejected post-#250 (any `@`-suffix on a runtime string errors with a migration hint).

## Common Patterns

Each pattern is the `filter` line of a declared query.

### Finding Unprocessed Items

<!-- corpus: 2026/examples/memql/patterns/unprocessed.memql -->
```memql fragment
filter row => row.processed == nil
```

`== nil` matches a missing field, a JSON null and an empty string alike (see
[Absent values](#absent-values)).

### Filtering by Date Range

<!-- corpus: 2026/examples/memql/patterns/date-range.memql -->
```memql fragment
filter row => row.dueAt >= args.from && row.dueAt < args.to
```

Strings order by byte, so RFC 3339 UTC timestamps compare as the instants they
name. A row with no `dueAt` falls outside both bounds.

### Nested Field Queries

<!-- corpus: 2026/examples/memql/patterns/nested-field.memql -->
```memql fragment
filter row => row.settings.notifications.email == true
```

If `settings` is an optional object field, the load asks for
`row.?settings.notifications.email`, which reads as absent when `settings` is.

## Worked Examples

These are strings in the [internal query form](#the-internal-query-form), as a
client sends them to `Execute`.

### Basic Listing (historical snapshot)

```text
asOf(concept==v1:assistant && active==true, "2025-11-01T00:00:00Z")
```

List all assistants that were active at the start of November 2025.

### Paginated Worlds by Recency

```text
sort(
  paginate(concept==v1:examples:world && status=="active", 25),
  "createdAt","desc"
)
```

Returns up to 25 active worlds, sorted by recency. `paginate(<expr>, limit)`
takes exactly two arguments -- there is no offset-style third argument.
Continuation past the first page is via the response's keyset cursor, not an
offset skip: pass the cursor the previous page returned to fetch the next
one, rather than computing a page number.

### Combining Multiple Concepts

```text
concept==v1:user || concept==v1:admin
```

A declared query binds one concept in its signature, so a read across two is a
string in this form, or two queries.

### Graph Traversal

```text
childOf(concept==v1:examples:world && id=="v1:examples:world:world-aurora") && tier=="silver"
```

Fetch all silver-tier modules that belong to `world-aurora`. The same filter,
authored: `filter row => childOf(w => w.id == args.worldId) && row.tier == "silver"`.

### Mixed Relationships + Filters

```text
parentOf(
  contains(
    concept==v1:examples:module && (tier=="silver" || tier=="gold")
  )
) && status=="active"
```

This form has no `in`, so the membership is a disjunction.

### Insert Examples

- **Basic insert**

  ```
  insert(
    "v1:memql:backend:user",
    id="user-123",
    payload={"email":"user@example.com","role":"developer"}
  )
  ```

- **Insert with relationships**

  ```
  insert(
    "v1:examples:module",
    id="module-advanced",
    parent="v1:examples:world:world-aurora",
    payload={
      "worldId":"v1:examples:world:world-aurora",
      "name":"Advanced Patterns",
      "tier":"gold"
    }
  )
  ```

## Error Handling

### Structured Error Format

MemQL returns machine-actionable structured errors for AI agent consumption. Errors follow this JSON format:

```json
{
  "error": "ERROR_TYPE",
  "code": "SPECIFIC_CODE",
  "message": "Human-readable description",
  "details": {
    "concept": "v1:crm:lead",
    "field": "email"
  },
  "suggestion": {
    "description": "How to fix this error",
    "template": "concepts()"
  }
}
```

**Fields:**
- `error` – High-level error category (same as `code` for consistency)
- `code` – Specific error code from a fixed set (see below)
- `message` – Human-readable description
- `details` – Error-specific structured data (optional)
- `suggestion` – Recovery guidance with static template (optional)
- `position` – Character offset in query where error occurred (optional)
- `context` – Query fragment around error position (optional)

### Error Codes

| Code | Meaning | Common Cause |
|------|---------|--------------|
| `VALIDATION_FAILED` | Payload doesn't match schema | Schema validation error |
| `MISSING_REQUIRED_FIELDS` | Required fields absent | Missing fields in insert payload |
| `INVALID_FIELD_TYPE` | Field has wrong type | Type mismatch in payload |
| `UNKNOWN_CONCEPT` | Concept not registered | Typo in concept name or concept not loaded |
| `UNKNOWN_FUNCTION` | Function not found | Typo in function name |
| `SYNTAX_ERROR` | Query parse failure | Malformed MemQL expression |
| `INVALID_OPERATOR` | Unknown comparison operator | Unsupported operator for field type |
| `RELATIONSHIP_NOT_FOUND` | Relationship type not defined | Using relationship function on concept without that relationship |
| `INVALID_ARGUMENT` | Invalid argument provided | Wrong argument type or missing required argument |
| `NOT_FOUND` | Requested resource not found | ID doesn't exist |

This is a fixed, enumerated set. No dynamic error codes are generated.

### Suggestion Templates

Suggestions are static templates to help agents recover from errors. They never involve AI generation:

```
MISSING_REQUIRED_FIELDS → "Add the missing required fields: {fields}"
UNKNOWN_CONCEPT        → "Check available concepts with: concepts()"
UNKNOWN_FUNCTION       → "Check available functions with: functions()"
SYNTAX_ERROR           → "Check MemQL syntax with: memqlDocs()"
RELATIONSHIP_NOT_FOUND → "Check concept relationships with: help(\"conceptName\")"
```

### Common Errors

| Error | Cause | Solution |
|-------|-------|----------|
| `unknown concept` | Concept not defined | Check concept name spelling |
| `invalid query syntax` | Malformed expression | Review operator/parentheses usage |
| `unsupported query shape` | Runtime string uses a retired shape (`shape()`, `select()`, `in`, `@`-suffix, `:=`) | Follow the migration hint in the error; declare the construct in the DSL |
| `spec not found` | Referenced spec doesn't exist | Define the spec or check spelling |

### Debugging Tips

1. Start with simple queries and add complexity
2. Check concept definitions for required fields
3. Use `validate(concept, payload)` / `previewInsert(...)` to check payloads before insert
4. Use `functions()` to list available functions
5. Use `help(name)` to get detailed help on any function or tool
6. Use `shapeTemplates()` / `shapeHelp(name)` to inspect projections

## Query Execution

Queries can be executed via the gRPC `MemqlService.Stream` bidirectional RPC or through the WebSocket bridge at `/memql/ws`. Both paths share the same backend validation, so every expression follows the same rules described in this guide.

## Subscriptions & Events

MemQL provides a real-time event system that delivers notifications for graph mutations, query execution, AI completions, and session lifecycle events. Clients subscribe over the existing bidirectional gRPC stream (or WebSocket bridge) and receive `EventNotification` messages as changes occur.

The full subscription protocol -- `SubscribeMsg`, the `SubscriptionKind` enum
and its values, the structured graph-events subscribe surface vs. the
free-text filter surface for every other kind, event topic shapes,
`EventNotification` payloads, and unsubscribing -- lives in
[docs/public/concepts/events.md](../concepts/events.md), which is the
single source of truth for this protocol. This guide does not duplicate it
here: a second copy of an enum's numeric values drifts the moment either
side changes, which is exactly what happened to an earlier version of this
section (stale `SubscriptionKind` numbering, and event topics still showing
the partition segment epic #56 removed).

## MemQL Language Reference for AI Agents

This is a condensed syntax specification designed to fit within limited context windows. Use this for quick reference; for detailed explanations and examples, see the sections above.

### Authored expressions

One file holding every predicate position: a `trait`, a row `spec`, a context
`spec`, a query `filter` and `refine`, and an automation `@filter`.

<!-- corpus: 2026/examples/memql/cheatsheet/authored-expressions.memql -->
```memql
use library.concepts.{ artifact }
use common.shapes.{ actorEnvelope }

/// Matches records whose active field is true.
trait rowIsActive = row => row.active == true

/// Matches artifacts an agent produced rather than a person.
spec artifact artifactIsGenerated = row => row.source == "agent_generated"

/// The caller holds the cluster-owner role.
spec actorEnvelope callerIsOwner = actor => actor.role == "owner"

/// Artifacts at one lens, narrowed in process by a search term.
query artifact artifactsByLens {
  args {
    lens  string  @required
    q     string  @required
  }
  filter   row => row.lens == args.lens && rowIsActive(row)
  sort     "row.createdAt", "desc"
  paginate 50
  refine   row => lower(row.title).includes(lower(args.q))
}

@trigger(event="node.created", concept="v1:library:artifact")
@filter(row => row.kind == "file" && row.archived == true)
/// Notice an archived file artifact arriving.
automation noticeArchivedFileArtifact {
  args {
    id  any
  }
  publish "docs.archivedFileNoticed" { artifactId: args.id }
}
```

- A predicate names its row, and every field is read through it: `row.status`,
  `row.id`, `row.createdAt`. A spec over an `@actor` shape names it `actor`.
- A spec or trait is applied to its receiver: `rowIsActive(row)`,
  `callerIsOwner(actor)`.
- An optional argument is a plain predicate: `(args.x == nil || row.f == args.x)`.
- A condition is boolean. There is no truthiness: write `args.name != nil`.
- A filter and a spec body push down to SQL; an in-process function works there
  only on values that do not read the row. `refine` (after `paginate`) runs in
  process over the page.

### Operators

| Operator | Example | Meaning |
|----------|---------|---------|
| `==` / `!=` | `row.status != "archived"` | Typed equality. Missing, null, `nil` and `""` are one unset value, so `!=` is true on an unset field |
| `<` `<=` `>` `>=` | `row.dueAt < now` | False when either side is missing or null |
| `in` | `row.kind in ["a", "b"]`, `args.tag in row.tags` | Membership |
| `startsWith` | `row.codeReference startsWith ["integration.", "method:"]` | Prefix, ANY of a list; an empty list and a blank prefix match nothing |
| `&&` `\|\|` `!` | `!(row.status in ["open", "held"])` | Boolean connectives and negation |
| `??` | `args.stage ?? "active"` | The right side when the left is missing, null, `""` or whitespace-only |
| `c ? a : b` | `args.flag ? "yes" : "no"` | Conditional value |
| `+` `-` `*` `/` `%` | `"si-" + hash(args.id)` | Arithmetic; `+` also joins strings |
| `.?` | `row.?lineage.planId` | Member access through an object that may be absent |
| `=>` | `row.tags.any(t => t == "urgent")` | Lambda |

Precedence, tightest first: member and call; `!` and unary `-`; `* / %`;
`+ -`; `??`; comparisons, `in`, `startsWith`; `&&`; `||`; `? :`; `=>`.

### Retired spellings

`when(args.x) { p }` is `(args.x == nil || p)`; `cond(p, a, b)` is `p ? a : b`;
`concat(a, b)` is `a + b`; `coalesce(a, b)` is `a ?? b`; `exists(x)` is
`x != nil`; `len(x)` is `x.count()`; `contains(s, sub)` is `s.includes(sub)`;
`has` is `in`; `null` is `nil`; `;` and `,` are `&&` and `||`; a bare spec name
is `name(row)`. The full list is [Retired spellings](#retired-spellings).

### DSL Construct Cheat Sheet

<!-- corpus: 2026/examples/memql/cheatsheet/constructs.memql -->
```memql
use library.concepts.{ artifact, folder }
use library.shapes.{ artifactFull }
use common.traits.{ isActiveRecord }

query artifact artifactsFiledUnder {
  args { folderId string @required }
  filter row => row.folderId == args.folderId && isActiveRecord(row)
  shape artifactFull
}

@actor
mutation folder addLibraryFolder {
  args {
    folderId string @required
    name string @required
  }
  insert {
    id: args.folderId
    name: args.name
    ownerUserId: actor.userId
    createdAt: now
  }
}

spec artifact artifactArchived = row => row.archived == true

@row
shape artifact artifactCard { row.id  title }

@trigger(event="node.created", concept="v1:library:file")
automation noteFileArrival {
  args { id any }
  publish "docs.fileArrived" { fileId: args.id }
}

@primary("fleet:strongest")
@fallback("app:*")
@fallback("federation:cheapest")
policy localThenVendor { }

@when(level="reasoning")
@policy("federationStrongest")
@precedence(100)
@onUnavailable("park")
rule parkOnReasoning { }

@level("fast")
@templateFile("prompts/summariseDoc.tmpl")
prompt summariseDoc { title string  content string! }
```

### The internal query form

A client sends these strings to `Execute`. They keep the older grammar: bare
payload fields, no `!`, no `in`.

```text
concept==v1:user && active==true                   # a filter
activeFolders()                                    # call a declared query
artifactsInFolder(folderId: "folder-123", lens: "artifact")
sort(paginate(activeFolders(), 10), "createdAt", "desc")
asOf(expr, "2025-11-01T00:00:00Z")                 # historical read
withDepth(parentOf(expr), 2)                       # traversal depth
childOf(expr)   parentOf(expr)   contains(expr)   owns(expr)   ids(expr)
insert("concept", id="id", payload={...})          # append a version
```

Use this reference when constructing MemQL queries. Always validate syntax and concept paths against the engine's response.

## Parsing the internal query form

The internal query form consumed by `engine.Execute(ctx, query string)` — function invocations (`funcName(k: v, ...)`), filter expressions (`concept==X && Y==Z`), `insert(...)` literals, and introspection meta-commands — is parsed exclusively through the language parser's expression grammar (`langparser.ParseExpression` + `ASTConverter`, epic #218). Authored expressions have their own parser, `ParseV1Expression` in `component/language/parser/v1_expr.go`. The legacy in-package recursive-descent runtime parser was retired in #328 / #250 after the soak window; there is no fallback path.

A small set of legacy runtime shapes is rejected upfront with a typed `ErrUnsupportedQueryShape` carrying a shape-specific migration hint:

- `shape(...)` — use a DSL-defined query with a `shape` projection instead.
- `select(...)` — declare the projection in the DSL query.
- `<expr> in (...)` — rewrite as a disjunction (`x=="a" || x=="b" || ...`) in runtime strings (authored expressions keep `in`).
- `concept==memql:version` — call `memqlVersion()` directly.
- Trailing `@timestamp` / `@latest` suffix — pin the timestamp via `asOf(...)` or at the DSL definition site.
- Inline spec definition (`name := expr`) — declare the spec in the DSL and apply it in a declared query.
- Trailing comma in the query string.

Cross-parser equivalence for the supported shapes is guarded by `TestParseViaLangparser_Equivalence` in `component/memql/parser_langpath_test.go`. Add a row there if a new caller adopts a shape the corpus doesn't cover.

## Upcoming Features

These roadmap items are planned but not yet implemented. Update this section as features land or priorities change.

- **Streaming Responses** – add streaming execution so clients can start reading partial MemQL results before the query finishes (instead of waiting for a single HTTP response body).

## Keeping This Guide Up to Date

Any change to MemQL parsing, execution options, relationships, or mutations **must** be reflected here. Before merging query-language changes:

1. Update the relevant sections (syntax, operators, options, examples, roadmap).
2. Reference this document in pull requests so reviewers verify documentation parity.
