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
and signature help. It is what colors and assists the source you read
in the Cockpit [Editor](../cockpit/editor.md).

Sense has **two consumers** today, both driving the same brain:

1. The Cockpit [Editor](../cockpit/editor.md), over gRPC
   (`MemqlService.Stream`).
2. The **VS Code extension** (see [MemQL in VS Code](./vscode.md)), over an
   offline language server (`cmd/memql-lsp`) that embeds this package and reads
   `.memql` from disk with no cluster and no auth.

Adding VS Code changed no wire contract -- it is a new delivery mechanism on
top of the existing Sense package, not a fork of the brain.

This document covers two things and how they relate:

1. **The DSL spec (`dslspec`)** -- the single machine-readable source
   of truth for the memQL authoring surface, from which Sense is driven
   and which any editor can fetch as portable JSON.
2. **MemQL Sense itself** -- the five language operations, how their
   completion / hover / diagnose behaviour is projected from the spec,
   and the CI drift guard that keeps the spec honest against the live
   grammar.

---

## The DSL spec: one source of truth

`component/language/dslspec` is the single machine-readable source of
truth for the memQL DSL authoring surface:

- the **top-level constructs** an author may write (`concept`, `query`,
  `mutation`, `logic`, `automation`, `spec`, `trait`, `shape`, `tool`,
  `prompt`, `provider`, `builtin`, `policy`, `seed`, `use`), each tagged
  with its category, its body sub-blocks, and whether its signature
  names a bound concept;
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
- **Constructs, keywords, operators, field types, and legal-next
  rules** have no pre-existing registry -- the truth was split across
  the parser's top-level dispatch and the struct-form rewriter.
  `dslspec` is the source of truth for those, and a drift test
  introspects the parser and rewriter to assert the spec stays in
  lockstep.

The package is a near-leaf: it imports only the annotations registry,
so both Sense and the gRPC/SDK export layer can consume it without an
import cycle.

---

## MemQL Sense: the five operations

Sense is exposed via gRPC on `MemqlService.Stream`. The core is pure
Go (`component/memql/sense/`, no gRPC dependency); the gRPC handlers
live in `component/grpc/sense_handlers.go`.

| Operation | What it returns |
|---|---|
| **Tokenize** | Semantic tokens for syntax highlighting -- keywords, identifiers, strings, annotations, concept ids |
| **Complete** | Context-aware autocompletion -- constructs, annotations, concepts, builtins, keywords |
| **Diagnose** | Errors and warnings from the lexer, parser, and semantic validation |
| **Hover** | Symbol info at the cursor -- function docs, concept schemas, annotation docs |
| **SignatureHelp** | Parameter help inside call arguments |

### Driven from the spec

Sense's completion no longer hardcodes its tables. At startup it builds
the spec once (`dslspec.Build()`) and projects it into the lookup
shapes completion needs:

- **Top-level construct completion** is the spec's construct list. Typing
  `mut` at the file top now offers `mutation`; the full struct-form set
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
  bound concept in the signature: `mutation <Concept> <name>`,
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

---

## Edit-time diagnostics

Diagnose runs parse errors plus a set of authoring rules over the
authored source. The load-mirroring rules carry stable code strings so
editors and agents can key on them:

| Code | Severity | Rule |
|---|---|---|
| `actor-undeclared` | Error | A query/mutate/logic/automation body reads `actor.*` without `@actor` in the preamble -- the edit-time mirror of the engine's load rule (#2621), sharing the loader's own detection so squiggle and boot error cannot drift. |
| `redundant-enabled` | Hint | `@enabled` restates the default (#2610). |
| `redundant-version` | Hint | `@version("1.0.0")` restates the default (#2613). |
| `discarded-args-description` | Hint | `@description` on an args field is parsed and discarded (#2615). |

### Member completion (#2624)

Typing a dot after a known accessor root completes ONLY that root's
members -- a dot context never offers keywords, builtins, functions,
or concepts, and an unknown root offers nothing:

| Root | Members offered |
|---|---|
| `actor.` | The canonical auth envelope from the dslspec property table (#2623), aliases sorted after canonical members. |
| `event.` | The event envelope: `topic`, `kind`, `payload`, `actor`, `timestamp`. |
| `event.actor.` | `id` only -- the emitter's identity stamp (G4), a different object from the auth envelope. |
| `args.` | The enclosing construct's declared args fields (any function kind; the automation BARE-name completion shares the same declared-field scanner, so the two can never disagree). |
| `payload.` | The enclosing construct's bound-concept fields (via the registry's concept projection). |

Both lexer shapes are detected: a trailing dot (`actor.`) and a
mid-member position (`actor.us`, where the prefix filters).

## The CI drift guard

`dslspec` is only useful if it stays true to the live grammar. A CI
drift test (`component/language/dslspec/drift_test.go`) is that pin: it
FAILS when the live grammar (the parser's top-level dispatch plus the
struct-form rewriter) or the annotations registry moves ahead of the
spec.

The test is allowed to import the parser and annotation registry that
production `dslspec` code must not, because a `_test.go` file does not
create a production import cycle. It compares the spec's construct set
against the parser's own authoritative, introspectable lists
(`parser.TopLevelDeclKeywords`, `parser.StructFormKeywords`) -- not a
hand-copied literal -- and names the exact drifted symbol on failure
(for example "add X to dslspec constructs()" / "remove Y"). Add a new
construct to the grammar without adding it to the spec, and this test
goes red.

The `SpecVersion` constant (`1.0.0`) versions the JSON envelope shape,
not its contents: a new construct or annotation does NOT bump it; only
a backward-incompatible change to the JSON shape does, so a consuming
editor can detect a contract mismatch.

---

## Portable JSON export (for editors)

The whole authoring surface serializes to portable, indented JSON --
the form a Monaco / CodeMirror grammar is generated from. Its top-level
shape is:

```json
{
  "version": "1.0.0",
  "constructs": [ ... ],
  "annotations": [ ... ],
  "keywords": [ ... ],
  "operators": [ ... ],
  "fieldTypes": [ ... ],
  "nextRules": [ ... ]
}
```

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
| The spec (source of truth) | `component/language/dslspec/` (`spec.go`, `constructs.go`, `lexicon.go`, `nextrules.go`, `json.go`) |
| The drift guard | `component/language/dslspec/drift_test.go` |
| Sense core (pure Go) | `component/memql/sense/` |
| Sense gRPC handlers | `component/grpc/sense_handlers.go` |
| Spec fetch SDK | `sdk/go/dslspec/` |
| Sense SDK (Tokenize / Hover / ...) | `sdk/go/sense/` |

## See also

- [The Cockpit Editor](../cockpit/editor.md) -- the read-only pack
  browser that Sense colors.
- [MemQL Authoring Rules & Gotchas](authoring-rules.md) -- the
  human-and-agent rule list Sense diagnostics surface at edit time.
- [MemQL Language](memql.md) -- the DSL reference.
- [Reserved Identifiers](reserved.md) -- the engine-reserved names.
</content>
