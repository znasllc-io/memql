# DSL Import-Model Refactor — design doc

> **Status:** Design locked. Commit 1 machinery COMPLETE (12 sub-commits).
> Commit 2 (file migration) UNDERWAY — concepts done (76 files,
> byte-equality verified). Queries / mutations / specs / shapes /
> tools / prompts / providers / builtins / automations / logic /
> policies migration is the next chunk. See "Implementation status"
> section for the precise punch list.
> **Scope:** Replace directory-as-namespace + `@useConcept` annotations
> with explicit Go-style `import (...)` blocks. Generalize across every
> DSL construct (concepts, queries, mutations, automations, logic,
> shapes, specs, tools, prompts, providers, builtins, policies).
> **File count:** 654 `.memql` files at design time.
> **Phasing:** 3 commits on `main`, mirroring Phase A → E shape.

---

## Motivation

Today the engine loads `.memql` files by walking
`dsl/v1/<type>/v1/<namespace>/<name>.memql`. The directory structure
IS the namespace; concept IDs are derived from path; cross-construct
references resolve via a flat global registry plus per-file
`@useConcept(<bareName>)` annotations.

That model has three real costs that compound as the tree grows:

1. **Layout is load-bearing.** Renaming a directory rewrites concept
   IDs. Moving a query to a different namespace breaks every caller.
2. **Flat global registry causes accidental coupling.** Two
   constructs that happen to share a bare name collide; a function
   becomes reachable from anywhere by name alone, making refactors
   surface in surprising places.
3. **One file = one construct.** Authors can't co-locate an
   automation with the logic blocks it composes from, or a concept
   with the queries that immediately operate on it. Cohesion is
   sacrificed to the loader's directory walker.

The new model:

- **File is the unit of cohesion.** A `.memql` file can declare any
  combination of constructs (concept + queries + mutations +
  automations + logic + ...). Layout serves authors, not the engine.
- **Imports are explicit.** Cross-file references go through
  `import (...)` blocks at the top of each file, naming the imported
  file (Go-style) and binding it to a local alias.
- **Concept IDs are structural, declared on the concept block.**
  `@version(N)` + `@namespace("...")` + `concept <name>` →
  assembled ID `v<N>:<namespace>:<name>` (byte-identical to today's).
- **Identity for non-concept constructs is `(file_path, name)`.**
  Two files can declare `query foo`; callers reach them via
  `a.foo` / `b.foo`.

---

## The 15 locked decisions

### 1. Imports name files, not symbols (Variant A)

```memql
import (
    "./cognition/participant" as cog
    "./common/space"                    // default alias from basename
)
query foo {
    concept cog.participant
    ...
}
```

The file is the package. Everything declared at the top level of
the imported file is reachable as `<alias>.<name>`. Variant B
(import-by-symbol-name) would have created a flat-namespace problem
we're explicitly moving away from.

### 2. Unified `import (...)` block — no per-kind buckets

A single import block at the top of the file lists every imported
file path. The construct's kind comes from its declaration site
(`concept X`, `query Y`); the importing side doesn't need to repeat
it. Mixed-content imports stay one line, not three.

### 3. Walk + import-graph discovery (no manifest)

At startup the engine walks the DSL root (embedded FS by default,
`MEMQL_DSL_PATH` override), parses every `.memql` file, extracts
each file's `import (...)` block, builds the import graph,
topo-sorts, and compiles in order. No top-level `main.memql`.

A stray `.memql` is loaded by virtue of existing; soft-disable
convention is `_filename.memql` (Go-style) or `_disabled/` subdir.

### 4. Symbol references everywhere on the authoring surface

Magic concept-ID strings disappear from the authoring surface.
Wherever today's code writes `v1:cognition:participant` (filter
clauses, `@concepts(...)`, `@handler(query="...")`, etc.) the new
form is a symbol reference: `cog.participant`.

The engine threads the actual ID string through internally during
resolution. Row data and event topics are unaffected — they still
carry the same `v1:<ns>:<name>` bytes.

### 5. `(file_path, name)` is internal identity for non-concept constructs

Two files can declare `query foo` and that's fine — callers reach
them via `a.foo` / `b.foo`. Today's flat global query/mutation
registries go away; per-file registries keyed by
`(file_path, name)` replace them. Cross-file dispatch (tool
handlers, automation triggers, prompt references) is symbol-refed.

Concepts are the exception: their durable ID lives in DB rows, so
they get an explicit ID via `@version` + `@namespace` (see #11).
Every other construct's identity is purely runtime.

### 6. Structured `@trigger` with concept symbol-ref

Today:

```memql
@trigger(event="graph.node.created.*.v1:cognition:participant")
```

Tomorrow:

```memql
@trigger(event="node.created", concept=cog.participant, partition="*")
```

The engine assembles the actual subscription topic at registration
time. Concept-less events omit the `concept` field. Closes the
abstraction #4 opened — concept IDs no longer leak into event-topic
strings.

### 7. Relative-only paths, root cap, symlink-resolve

Import paths are relative (`./...` or `../...`). They resolve
against the importing file's directory. The resolved canonical
path must have the configured DSL root as a prefix; otherwise the
import errors at parse time. Symlinks are resolved before the
prefix check.

`MEMQL_DSL_PATH` continues to override the embedded-FS root. No
"implicit CWD" mode.

### 8. Basename default alias, `as` override on collision

`import "./participant"` defaults to alias `participant`. If two
imports default to the same name, the parser errors and the author
adds `as <name>`. Basename must be a legal identifier (regex
`^[a-z][a-zA-Z0-9_]*$`); files with non-identifier basenames
require an explicit `as`.

### 9. No transitive imports — explicit only

If A imports B and B imports C, A doesn't see C's symbols. A must
add C to its own import block. Reading any file in isolation, its
`import (...)` block is the complete dependency contract.

### 10. Forbid cyclic imports

Topo-sort fails → engine reports `import cycle: ./a → ./b → ./a`.
Author fixes by extracting the shared bit into a third file or by
co-locating the cycle's two ends in one file. No lazy resolution,
no two-pass compilation.

### 11. Structural concept ID — `@version(N)` + `@namespace("...")` + name from declaration

```memql
@version(1)
@namespace("cognition")
@description("A human or SI participant instance within a space.")
concept participant {
    ...
}
// assembled row-ID prefix: v1:cognition:participant
```

The literal ID string lives nowhere in author code. Engine
generates it deterministically.

**Concept names are lowercase-first** (camelCase for multi-word
concepts: `participant`, `canvasState`, `auditEvent`,
`partitionAccess`, `nodeType`). Validated at parse time via
`^[a-z][a-zA-Z0-9]*$`.

**Migration discipline:** every concept's assembled
`v<N>:<ns>:<name>` must equal the bytes currently stamped on rows
in production DBs. The Commit-2 migration script audits every
concept against the existing concept registry.

### 12. Magic strings stay strings for cross-boundary refs

Symbol refs replace magic strings only when the referenced thing
lives in a `.memql` file. Things that cross a boundary the
language doesn't own stay as strings:

- `@executor("integration.email.sendEmail")` — Go-side registration
  key, not a `.memql` symbol. Format regex enforced at parse time.
- `@templateFile("./agentReply.tmpl")` — sibling text file, not a
  language entity. Path-scope rules same as imports.
- Enum-shaped values: `@scope("global")`, `@type("OpenAI")`,
  `@model("gpt-5.4-mini")`, `@enum("a","b")` — these are values,
  not references. Per-attribute enum-set validation.

### 13. Small rules

- **Reserved-alias collisions reject:** `import "./foo" as actor`
  errors. Reserved: `actor`, `now`, `partition`, `config`, `trace`.
- **Auto-append `.memql`:** `import "./foo"` resolves to
  `./foo.memql`. Explicit suffix is tolerated.
- **Unused imports error at parse time.** Same rule as args,
  `@useConcept`, shape fields today.
- **Concept name = lowercase-first identifier** (see #11).

### 14. `engine.Validate(target)` API + `memql-cockpit lint <path>` CLI

Single entry point. `target` is auto-detected: file → validate the
file + its transitive imports as one compilation unit; directory
→ validate the whole tree as a topo-sorted unit. `--json` flag on
the CLI flips human output to machine output.

Pipeline (fail-fast at each layer):

1. Lex + parse each file.
2. Resolve `import (...)` — path scope, root cap, symlink resolve,
   `.memql` auto-append.
3. Build import graph, detect cycles.
4. Topo-sort, compile in order, populate per-file registries.
5. Resolve cross-file symbol refs (every dangling ref is an error).
6. Run per-construct validators (`ValidateMemQL`, declared-must-be-used,
   policy lint).
7. Assemble + uniqueness-check concept IDs.

Engine runs the whole-tree validator at startup before accepting
traffic; any error-level diagnostic fails the process loud.

### 15. 3-commit big-bang on main

- **Commit 1 — Add machinery.** Parser surface, loader, validator.
  Internal: parser accepts both old and new file shapes (the
  transitional state). All 654 files still use old syntax and
  still load. New machinery is exercised by tests + a handful of
  proof-of-concept new-syntax files.
- **Commit 2 — Migrate all files.** Mechanical sweep. Every file
  edited to new syntax. Per-concept byte-equality audit on
  assembled IDs. Test suite passes; engine boots; row data
  untouched.
- **Commit 3 — Reject legacy form.** Loader errors on old syntax.
  Dead code from Commit 1's transitional state deleted. CLAUDE.md
  files + docs/core/memql-authoring-rules.md updated. The two
  stale PascalCase `concept Node` examples in
  `dsl/v1/automations/CLAUDE.md` (flagged at session start) get
  fixed here too.

Each commit is a holistically-correct state of the tree. Between
commits, the engine boots, tests pass, and the user can pause and
inspect.

---

## Migration mechanics — Commit 2 in detail

The 654-file sweep is mechanical but the byte-equality discipline
on concept IDs is the one place a typo breaks production rows. The
sweep script produces `concept_id_audit.txt`:

```
v1:cognition:participant     [registry: 12483 rows]     [migrated: v1:cognition:participant]   OK
v1:cognition:space           [registry:  2074 rows]     [migrated: v1:cognition:space]         OK
v1:copresent:canvasState     [registry:   801 rows]     [migrated: v1:copresent:canvasState]   OK
...
```

Any line where `[registry]` and `[migrated]` differ in bytes is a
hard error. The commit doesn't land until the audit is clean.

For non-concept constructs (queries, mutations, etc.) the migration
is purely syntactic:

- `@useConcept(participant)` → add `import (...)` block + replace
  body references `participant.X` with `<alias>.participant.X`.
- `@trigger(on=participant.created)` → `@trigger(event="node.created", concept=<alias>.participant)`.
- `@trigger(event="graph.node.created.*.v1:cognition:participant")` → same as above.
- `concept==v1:cognition:participant` in filters → `concept <alias>.participant`.
- `@handler(type="query", query="concept==v1:foo:bar")` → symbol ref.

The migration script lives at `scripts/migrate-import-model.go`
(invokable via `go run`). It walks every `.memql` file, applies
transforms, and writes the audit file alongside the diff.

---

## Implementation status

**Commit 1 (additive machinery, transitional state) — COMPLETE.**
Twelve sub-commits shipped to `main`:

| # | Commit | What landed |
|---|---|---|
| 1 | design doc | This file. |
| 2 | parser + AST for `import (...)` | Lexer keyword, AST node, block parser, single-line shorthand, 7 tests. |
| 3 | path resolver + alias rules | ResolveImport, DefaultAlias, ValidateAlias. 11 tests. |
| 4 | import graph | NewImportGraph, AddEdge, OutEdges, DetectCycle, Topo (DFS post-order, stable alpha tie-break). 10 tests. |
| 5 | FS walker + soft-disable | WalkMemqlFiles + `_`-prefix file/dir skip rule. 7 tests. |
| 6 | integrated build pipeline | BuildImportGraph: RawImport → graph + per-file alias tables. 9 tests. |
| 7 | dslimports.Load | Wires walker + parser + build pipeline into one entry point. *LoadError aggregates per-file diagnostics. 7 tests. |
| 8 | lint CLI + ParseFileSource | `memql-cockpit lint <path>` (human + JSON). compiler.ParseFileSource runs the full rewriter chain. Diagnostics flatten through multi-error wrappers. |
| 9 | imports-only fallback parser | parser.ExtractImports for files the generic parser can't handle (shape/provider/builtin/prompt/tool struct forms). 8 tests. Lint now reports 0 diagnostics on the full `dsl/v1/` tree. |
| 10 | @version + @namespace + ID assembly | Parser accepts `@name(<integer>)`. AST package gets AssembleConceptId / ValidateAssemblyInputs / ExtractVersionAttribute / ExtractNamespaceAttribute / AssembleConceptIdFromDecl. 14 tests. |
| 11 | structured @trigger | ast.BuildTriggerTopic + ParseStructuredTriggerArgs. Allowed event kinds: node.created / node.updated / node.deleted. 9 tests. |
| 12 | cross-file symbol resolver | Tree.ResolveSymbol returns typed handles (Kind / File / Name / ConceptId) for `alias.name` references. VerifyAllSymbolReferences walks @trigger concept= / @handler query= attribute args. 7 tests. |

**Commit 2 (file migration sweep) — UNDERWAY.**

| # | Commit | What landed |
|---|---|---|
| 1 | concepts migration | 76 concept.memql files stamped with @version(1) + @namespace("..."). Concept declaration names lowercased (PascalCase → camelCase first letter). Byte-equality audit: 76/76 match the runtime ID. scripts/migrate-concept-headers idempotent + supports --dry-run. concept_parser.go updated to accept the new annotations. Row data unaffected. |

**What works today:**

- `import (...)` blocks parse correctly, including single-line
  shorthand and `as` aliases.
- Path resolution: relative-only, root-capped, symlink-safe.
- Cycle detection on the import graph; topo-sort emits
  depends-first order.
- Walker discovers every `.memql` under a root and applies the
  `_`-prefix soft-disable rule.
- End-to-end `memql-cockpit lint <path>` validates a single file
  or a whole tree. Reports `OK: 644 file(s) loaded, no
  diagnostics` against the full live tree.
- Structural concept IDs assemble correctly for flat (76) and
  nested (8) concepts. Byte-equality audit on the concepts
  migration: 76/76 match the runtime-derived ID.
- All 76 concept declarations now carry `@version(1)` +
  `@namespace("...")` and use lowercase-first declaration names.
- Cross-file symbol references (`alias.name`) resolve through
  the per-file alias tables to typed Resolution handles.

**Remaining for Commit 2 — file migration sweep:**

| Item | Files touched | Notes |
|---|---|---|
| Migrate `@useConcept(X)` annotations to `import (...)` blocks on every query / mutation / spec / shape / automation / logic / policy file | ~570 | Mechanical sweep. Each file gets its `@useConcept(name)` replaced with `import "./path/to/name" as <alias>` and any body references to `name.X` updated to `alias.name.X`. The concept-file path needs the deriveConceptName mapping reversed. |
| Migrate `@trigger(event="graph.node.created.*.v1:X:Y")` → structured form on every automation | 35 | Convert to `@trigger(event="node.created", concept=<sym>, partition="*")`. Requires the concept to be imported first. |
| Migrate `@trigger(on=name.created)` → structured form | (subset of above) | Convert to `@trigger(event="name.created", concept=<sym>, partition="*")`. |
| Migrate `concept==v1:X:Y` filter clauses to `concept <sym>` | depends on file; not a separate sweep | Done as part of the per-file @useConcept migration. |
| Migrate `@handler(type="query", query="concept==v1:X:Y")` → `@handler(type="query", query=<sym>)` on tools | 13 | Tool handlers' query strings become symbol refs. |

These can ship as separate commits per construct kind (one for
queries, one for mutations, ...) so each is reviewable
independently and the lint pipeline catches drift after every
sub-commit.

**After Commit 2: Commit 3** rejects the legacy form
(`@useConcept`, `use` directive, raw concept-ID strings in
attribute values), deletes the transitional shim code in
concept_parser.go's @version / @namespace acceptance, and updates
the CLAUDE.md surface to document the new model as the only
model.

---

## File-restructure (Commit 2 sub-phase) — locked design

Once import resolution lives in the parser (Commit 1 ✓), the
directory-as-namespace tree (`dsl/v1/<kind>/v1/<domain>/<name>.memql`)
is no longer load-bearing. The restructure collapses 654 files
into ~65 by reorganizing **domain-first, kind-second** — every
file is "about" an entity or feature, not a construct kind.

### 11 locked decisions

| # | Decision | Recommendation |
|---|---|---|
| 1 | Automation file granularity | A-mostly-with-B-for-big — small automations bundle into `<domain>/automations.memql`; automations with multiple logic blocks or >80 lines get their own `<domain>/<name>.memql`. |
| 2 | Top-level layout | Flat. `dsl/cognition/`, `dsl/copresent/`, ..., `dsl/common/`, `dsl/providers/`, `dsl/policies/`. No `v1/`, no `domains/` vs `infra/` split. |
| 3 | Per-entity file granularity within a domain | Cluster only on signal 1 (parent/child sub-concept like `space/context`) or signal 2 (parallel shape + shared actor + shared lifecycle like `audio/video/mic Override`). Else one concept per file. |
| 4 | Prompts | `<domain>/prompts/` subdirectory with `.memql` + `.tmpl` pairs co-located. |
| 5 | Providers | One file per vendor: `providers/{openai,anthropic,google,groq,mistral,xai}.memql` — base + every model in one file. |
| 6 | Policies | One file per tier: `policies/routing.memql` + `policies/bff.memql` (+ `policies/core.memql` when populated). Per-area subdirs collapse to section comments. |
| 7 | Builtins | `<domain>/builtins.memql` for domain-scoped; `common/builtins.memql` for cross-cutters (embedding, similarity, files, gcs, email, auth). |
| 8 | Tools | `<domain>/tools.memql` for domain-scoped tools. Sub-area groupings (canvas, worker, claw) become section comments. |
| 9 | Shapes | Concept-bound shapes inline with their entity file. Cross-domain shapes (traits + caller) in `common/shapes.memql` with section comments. |
| 10 | Common content | Move `agent` → `copresent/`, `documentChunk` → `knowledge/document.memql`, `knowledgeDomain` + `knowledgeBridge` → `knowledge/domain.memql`. `common/` keeps ONLY `shapes.memql`, `specs.memql`, `builtins.memql`. |
| 11 | Logic block placement | Inline with their automation in the same file. Shared-within-domain in `automations.memql` top section under "shared logic" heading. Shared-across-domains is a refactor smell — promote to builtin or duplicate. |

### Resulting tree shape

```
dsl/
├── cognition/
│   ├── space.memql            # space + space/context cluster
│   ├── participant.memql      # participant + participant/presence cluster
│   ├── session.memql
│   ├── utterance.memql
│   ├── privateUtterance.memql
│   ├── turn.memql             # turn/state
│   ├── presence.memql         # audioOverride + videoOverride + micState cluster (signal 2)
│   ├── feedback.memql
│   ├── clientTools.memql      # client/tool/request + response cluster (signal 1)
│   ├── textChunk.memql        # text/chunk
│   ├── automations.memql      # 5 cognition automations + their logic
│   ├── builtins.memql         # @executor("integration.cognition.*") wrappers
│   ├── tools.memql            # if any
│   └── prompts/
│       ├── conductorReply.memql / .tmpl
│       └── ...
│
├── copresent/                 # ~10-12 entity files + automations / builtins / tools / prompts/
├── identity/                  # ~10 entity files (incl. relocated agent — wait, agent is copresent)
├── cluster/                   # node, topology (cluster+database+identityProvider cluster)
├── platform/                  # partition, secrets (global+partition cluster), variables, policyTrace
├── knowledge/                 # document (+ documentChunk relocated), domain (+ knowledgeBridge relocated), items, prompts/
├── data/                      # record, log, policy + automations
├── worker/                    # invocation, registration + automations + tools
├── router/                    # budget, call, modelcatalog, policycatalog
├── memql/                     # checkpoint
├── common/
│   ├── shapes.memql           # trait shapes + caller shapes (cross-domain)
│   ├── specs.memql            # cross-concept specs (those binding to trait shapes)
│   └── builtins.memql         # cross-cutting integration wrappers
├── providers/
│   ├── openai.memql           # base + every OpenAI model
│   ├── anthropic.memql
│   ├── google.memql, groq.memql, mistral.memql, xai.memql
└── policies/
    ├── routing.memql          # 5 SI-router policies
    └── bff.memql              # bff/copresent + bff/voice policies
```

### Pass 2 status -- concepts unblocked; queries/mutations/etc. still blocked

Pass 2 (engine loader cutover) is partially landed:

**Concepts: full coverage via the new tree.** The unified concept
loader (`component/memql/unified_loader.go`) is called from
`app/database.go` alongside the legacy loader. It uses a
**concepts-only extractor** (`concepts_only_extractor.go`) that
scans each consolidated file's source text for
`concept ... { }` blocks, slices each with its preamble of
@-attributes, and parses each slice in isolation via
`parser.ParseFile`. This bypasses the struct-form rewriter chain
entirely, so multi-concept files load cleanly. All 76 concepts
register from the new tree; concept IDs assemble byte-identically
to the legacy paths, so `MergeAll` is a safe overlay during the
transitional state.

**Queries / mutations / specs / shapes / automations / logic /
prompts / providers / policies / tools: still loaded from legacy
tree.** The struct-form rewriters (mutation_rewrite,
query_rewrite, spec_rewrite, automation_rewrite, logic_rewrite)
assume single-concept-per-file binding. The consolidated files
break that assumption, so `compiler.ParseFileSource` errors out
on most of them with messages like:

    mutation rewrite: struct-form mutation "mutationArchiveSpace":
    insert target "space" does not match the concept binding
    "context" -- write it as `insert context { ... }`

When `ParseFileSource` errors, `dslimports.Load` falls back to
`ExtractImports`, which strips all `Definitions`. So a unified
loader for any of these construct kinds gets nothing useful from
the new tree until the rewriters are taught to bind each
construct to its OWN `@useConcept(<name>)` (or import alias)
annotation rather than a single file-level binding.

**Next session work:**

1. Teach `mutation_rewrite` and `query_rewrite` to consult each
   construct's local annotations when determining concept binding,
   instead of file-level. This unlocks unified loading for
   queries + mutations + specs + shapes from the new tree.
2. Build per-kind unified loaders (`LoadUnifiedQueries`,
   `LoadUnifiedMutations`, ...) following the same pattern as
   `LoadUnifiedConcepts` -- walk the new tree, extract relevant
   declarations, register via the existing per-kind registration
   helpers.
3. Once all kinds load from the new tree, retire the legacy
   per-kind embed packages and delete `dsl/v1/`.

### Migration strategy

Three-pass big-bang inside Commit 2:

1. **Pass 1 — additive emit**. A migration script reads the old
   tree, computes the new consolidated files, writes them
   alongside the old structure (no deletion yet). Engine still
   loads from old paths; nothing breaks. Byte-equality audit
   ensures concept IDs stay identical across the move.

2. **Pass 2 — engine cutover**. Update concept/query/mutation/
   shape/spec/prompt/provider/tool/automation/policy loaders to
   read from the new tree via `dslimports.Load`. Old tree
   remains on disk but is no longer authoritative. Tests +
   `memql-cockpit lint` validate the new tree is complete.

3. **Pass 3 — delete old**. Remove the entire `dsl/v1/<kind>/v1/`
   structure. Delete the per-kind embed declarations. Tree is
   clean.

Each pass ships as a separate commit. The tree is in a working
state at every commit boundary.

---

## Open items deferred past Commit 3

These didn't ship with the refactor and are tracked as follow-ups:

- **MemQL Sense (LSP) import awareness.** Tokenize/complete/hover/
  signature in `component/memql/sense/` need to understand imports
  for editor go-to-definition + cross-file autocomplete. Same code
  paths consume the new tree; work is largely surface-level.
- **F.1 from the prior handoff** (split big single-step automations
  into multi-step recipes). Becomes easier under the new model
  because automation + its logic blocks can live in one file.
- **F.5 description quality on logic blocks.** Still applies.
- **Test-coverage gap on logic-to-logic composition** (F.4).
  Migrate the existing rewrite tests to the new syntax once
  Commit 3 lands.

---

## Reference files

| Path | Role |
|---|---|
| `component/language/parser/parser.go` | Top-level parser. Adds `import` block parsing. |
| `component/language/parser/ast.go` | AST node types. Adds `Import` + per-file symbol table. |
| `component/memql/concept_resolver.go` | Today's `@useConcept` resolver. Generalized to symbol-ref resolver. |
| `component/memql/function_loader.go` | Walks embedded/disk FS. Becomes the import-graph driver. |
| `component/memql/dslfs/dslfs.go` | Embedded-FS / disk-override abstraction. Stays unchanged. |
| `component/automations/loader.go` | Per-construct loader. Adopts per-file `(file_path, name)` registry. |
| `component/language/compiler/api.go` | `CompileSource` / `ValidateMemQL`. Wrapped by the new `engine.Validate(target)`. |
| `cmd/memql-cockpit/` | Adds `lint <path>` subcommand. |
| `scripts/migrate-import-model.go` | New. Drives Commit 2's mechanical sweep + audit. |
| `dsl/v1/concepts/v1/<ns>/<name>/concept.memql` | Existing concept files. Get `@version` + `@namespace` annotations during Commit 2. |
| `dsl/v1/automations/v1/<ns>/<name>/automation.memql` | Existing automations. Trigger annotations and step bodies rewritten. |

---

## Ground rules (durable)

- Commit + push directly to `main`. No feature branches.
- No backwards-compat shims past Commit 3.
- `git add <explicit-path>` only — never `-A` or `.`.
- Commit messages via `/tmp` + `git commit -F`.
- No emojis (code, commits, docs).
- Backend says SI; frontend says AI.

---

**Living doc.** Updated as implementation surfaces new constraints
or simplifies decisions in flight.
